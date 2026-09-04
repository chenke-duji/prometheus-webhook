// Command webhookd receives Alertmanager webhook notifications and forwards
// them to a cep-engine instance as RawEvents. It uses a bounded, multi-worker
// batching queue with backpressure for high-throughput, and exposes
// Prometheus metrics on the same port as the webhook endpoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"prometheus-webhook/internal/config"
	"prometheus-webhook/internal/forward"
	"prometheus-webhook/internal/metrics"
	"prometheus-webhook/internal/webhook"
)

// build info (overridable at link time via -ldflags "-X main.buildVersion=... -X main.buildDate=...").
var (
	buildVersion = "dev"
	buildDate    = "unknown"
)

func main() {
	var configPath string
	var showVersion bool
	flag.StringVar(&configPath, "config", "", "path to YAML config file")
	flag.BoolVar(&showVersion, "v", false, "print build/version info and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("prometheus-webhook %s (%s)\n", buildVersion, buildDate)
		return
	}

	if err := run(configPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// run wires up the daemon and blocks until a termination signal is received.
func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("config load failed: %w", err)
	}

	logger, logCleanup := newLogger(cfg.Logging)
	defer logCleanup()

	if cfg.Webhook.AuthToken == "" {
		logger.Error("authToken is empty — server will reject ALL requests (fail-closed)")
	}

	// --- Forwarder (HTTP to cep-engine) ---
	httpFwd, err := forward.NewHTTPForwarder(&forward.HTTPConfig{
		BaseURL:    cfg.CEPEngine.BaseURL,
		BatchPath:  cfg.CEPEngine.BatchPath,
		SinglePath: cfg.CEPEngine.SinglePath,
		AuthToken:  cfg.CEPEngine.AuthToken,
		Timeout:    cfg.CEPEngine.Timeout,
		RetryMax:   cfg.CEPEngine.RetryMax,
		RetryBase:  cfg.CEPEngine.RetryBase,
	}, logger)
	if err != nil {
		return fmt.Errorf("forwarder init failed: %w", err)
	}
	defer func() { _ = httpFwd.Close() }()

	// --- Metrics + Batch queue (single shared instance) ---
	// A closure captures `queue` by reference so that metrics.Handler()
	// reports the correct queue depth. The closure is only invoked after
	// the server starts, by which point `queue` is non-nil.
	var queue *forward.BatchQueue
	m := metrics.New(func() int {
		if queue != nil {
			return queue.QueueDepth()
		}
		return 0
	})
	queue = forward.NewBatchQueue(cfg.Forward, httpFwd, m, logger)
	queue.Start()
	defer queue.Close()

	// --- Webhook HTTP server ---
	srv, err := webhook.New(cfg, queue, m, logger)
	if err != nil {
		return fmt.Errorf("webhook server init failed: %w", err)
	}

	// Start HTTP server in goroutine
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	logger.Info("prometheus-webhook started",
		"listen", cfg.Webhook.ListenAddr,
		"source", cfg.Webhook.Source,
		"cepEngine", cfg.CEPEngine.BaseURL,
		"queueCapacity", cfg.Forward.QueueCapacity,
		"tls", cfg.TLS.Enabled,
		"ipAllowlist", len(cfg.Webhook.AllowedCIDRs),
		"trustedProxies", len(cfg.Webhook.TrustedProxies),
	)

	// --- Graceful shutdown ---
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-stop:
		logger.Info("shutting down...")
	case err := <-srvErr:
		return fmt.Errorf("webhook server failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	return nil
}

// newLogger builds a structured logger with optional file rotation.
func newLogger(lc config.LoggingConfig) (*slog.Logger, func()) {
	var w io.Writer = os.Stdout
	cleanup := func() {}
	if lc.File != "" {
		rw := newRotatingWriter(lc.File, lc.MaxSizeMB, lc.MaxBackups)
		w = rw
		cleanup = func() { _ = rw.Close() }
	}
	level := parseLevel(lc.Level)
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(handler), cleanup
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// rotatingWriter appends to a file and rolls it over when it exceeds a size
// limit, keeping up to maxBackups rotated files.
type rotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int
	file       *os.File
}

func newRotatingWriter(path string, maxSizeMB, maxBackups int) *rotatingWriter {
	return &rotatingWriter{
		path:       path,
		maxSize:    int64(maxSizeMB) * 1024 * 1024,
		maxBackups: maxBackups,
	}
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Ensure file is open.
	if w.file == nil {
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		w.file = f
	}

	// Rotate if this write would exceed the size limit.
	if info, err := w.file.Stat(); err == nil && info.Size()+int64(len(p)) > w.maxSize {
		w.rotate()
		// rotate closes and nulls w.file; reopen for the new write.
		if w.file == nil {
			f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return 0, err
			}
			w.file = f
		}
	}

	return w.file.Write(p)
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

func (w *rotatingWriter) rotate() {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	ts := time.Now().Format("20060102-150405")
	rotated := fmt.Sprintf("%s.%s", w.path, ts)
	_ = os.Rename(w.path, rotated)
	if w.maxBackups > 0 {
		matches, _ := filepath.Glob(w.path + ".*")
		for len(matches) > w.maxBackups {
			oldest := matches[0]
			for _, m := range matches {
				if m < oldest {
					oldest = m
				}
			}
			_ = os.Remove(oldest)
			matches = removeStr(matches, oldest)
		}
	}
}

func removeStr(s []string, v string) []string {
	for i, x := range s {
		if x == v {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}
