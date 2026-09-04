// Package webhook implements the HTTP server that receives Alertmanager
// webhook notifications, applies security checks (Bearer token + IP/CIDR
// allowlist + trustedProxies XFF extraction + optional mTLS), splits the
// alerts array into individual RawEvents, and enqueues them for forwarding.
package webhook

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"prometheus-webhook/internal/config"
	"prometheus-webhook/internal/forward"
	"prometheus-webhook/internal/metrics"
	"prometheus-webhook/internal/model"
)

// Server is the webhook HTTP server. It serves three endpoints on a single
// port: /webhook (Alertmanager receiver), /metrics (Prometheus self-monitor),
// /healthz (health check).
type Server struct {
	cfg              *config.Config
	queue            *forward.BatchQueue
	metrics          *metrics.Metrics
	log              *slog.Logger
	allowedIPs       []net.IPNet
	trustedProxyNets []net.IPNet
	httpSrv          *http.Server
}

// New creates a new webhook Server.
func New(cfg *config.Config, queue *forward.BatchQueue, m *metrics.Metrics, log *slog.Logger) (*Server, error) {
	s := &Server{
		cfg:     cfg,
		queue:   queue,
		metrics: m,
		log:     log,
	}

	// Parse allowed CIDRs.
	for _, cidr := range cfg.Webhook.AllowedCIDRs {
		ipNet, err := parseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("webhook: invalid allowedCIDR %q: %w", cidr, err)
		}
		s.allowedIPs = append(s.allowedIPs, *ipNet)
	}

	// Parse trusted proxy CIDRs.
	for _, cidr := range cfg.Webhook.TrustedProxies {
		ipNet, err := parseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("webhook: invalid trustedProxy %q: %w", cidr, err)
		}
		s.trustedProxyNets = append(s.trustedProxyNets, *ipNet)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(cfg.Webhook.Path, s.handleWebhook)
	mux.Handle(cfg.Webhook.MetricsPath, m.Handler())
	mux.HandleFunc(cfg.Webhook.HealthPath, s.handleHealth)

	s.httpSrv = &http.Server{
		Addr:              cfg.Webhook.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}

	return s, nil
}

// Start begins listening. If TLS is enabled, starts a TLS server (optionally
// with mTLS client verification).
func (s *Server) Start() error {
	if s.cfg.TLS.Enabled {
		return s.startTLS()
	}
	s.log.Info("webhook server starting (HTTP)", "addr", s.cfg.Webhook.ListenAddr)
	return s.httpSrv.ListenAndServe()
}

func (s *Server) startTLS() error {
	cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if err != nil {
		return fmt.Errorf("webhook: load TLS keypair: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if s.cfg.TLS.ClientCAFile != "" {
		caData, err := os.ReadFile(s.cfg.TLS.ClientCAFile)
		if err != nil {
			return fmt.Errorf("webhook: read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caData) {
			return fmt.Errorf("webhook: client CA file %s contains no valid certificates", s.cfg.TLS.ClientCAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	s.httpSrv.TLSConfig = tlsCfg
	s.log.Info("webhook server starting (mTLS)", "addr", s.cfg.Webhook.ListenAddr, "clientCA", s.cfg.TLS.ClientCAFile)
	return s.httpSrv.ListenAndServeTLS("", "")
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

// handleWebhook processes Alertmanager webhook notifications.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// 1. Extract realClientIP (trustedProxies-aware XFF)
	realClientIP := s.extractClientIP(r)

	// 2. IP allowlist check (before token to fail fast)
	if len(s.allowedIPs) > 0 {
		if !s.isIPAllowed(realClientIP) {
			s.metrics.IncAuthRejected()
			s.metrics.IncWebhookRequest(403)
			writeJSON(w, 403, map[string]string{"status": "error", "reason": "ip not allowed"})
			s.log.Warn("webhook: IP rejected", "realClientIP", realClientIP, "remoteAddr", r.RemoteAddr)
			return
		}
	}

	// 3. Bearer token check (fail-closed when token is empty)
	if s.cfg.Webhook.AuthToken == "" {
		s.metrics.IncAuthRejected()
		s.metrics.IncWebhookRequest(401)
		writeJSON(w, 401, map[string]string{"status": "error", "reason": "unauthorized"})
		// Startup warning is logged once in main.go; no per-request log to avoid flooding.
		return
	}
	if !checkBearerToken(r, s.cfg.Webhook.AuthToken) {
		s.metrics.IncAuthRejected()
		s.metrics.IncWebhookRequest(401)
		writeJSON(w, 401, map[string]string{"status": "error", "reason": "unauthorized"})
		s.log.Warn("webhook: token rejected", "realClientIP", realClientIP)
		return
	}

	// 4. Method check
	if r.Method != http.MethodPost {
		s.metrics.IncWebhookRequest(405)
		writeJSON(w, 405, map[string]string{"status": "error", "reason": "method not allowed"})
		return
	}

	// 5. Body size limit
	if s.cfg.Webhook.MaxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Webhook.MaxBodyBytes)
	}

	// 6. Read and parse body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.metrics.IncWebhookRequest(413)
		writeJSON(w, 413, map[string]string{"status": "error", "reason": "body too large"})
		s.log.Warn("webhook: body too large", "realClientIP", realClientIP, "err", err)
		return
	}

	var payload model.AlertPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		s.metrics.IncWebhookRequest(400)
		writeJSON(w, 400, map[string]string{"status": "error", "reason": "invalid JSON: " + err.Error()})
		s.log.Warn("webhook: JSON parse failed", "realClientIP", realClientIP, "err", err)
		return
	}

	// 7. Split alerts and enqueue
	domainID := s.cfg.Webhook.DomainID
	if domainID == "" {
		domainID = "default"
	}
	alerts := payload.Alerts
	enqueued := 0
	dropped := 0
	for i := range alerts {
		alert := &alerts[i]
		s.metrics.IncAlertByStatus(alert.Status)
		ev := model.NewFromAlert(alert, &payload, s.cfg.Webhook.Source, realClientIP, domainID, time.Now())
		if s.queue.Enqueue(ev) {
			enqueued++
		} else {
			dropped++
		}
	}

	// 8. Response
	if dropped > 0 {
		s.metrics.IncWebhookRequest(202)
		writeJSON(w, 202, map[string]interface{}{
			"status":  "accepted",
			"count":   enqueued,
			"dropped": dropped,
		})
	} else {
		s.metrics.IncWebhookRequest(200)
		writeJSON(w, 200, map[string]interface{}{
			"status": "accepted",
			"count":  enqueued,
		})
	}
}

// handleHealth returns a simple health check response.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// extractClientIP determines the real client IP.
// If RemoteAddr is in trustedProxies, extracts from X-Forwarded-For (leftmost).
// Otherwise uses RemoteAddr directly.
func (s *Server) extractClientIP(r *http.Request) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}

	// If no trusted proxies configured, don't trust any XFF header.
	if len(s.trustedProxyNets) == 0 {
		return remoteIP
	}

	// Check if the direct connection is from a trusted proxy.
	remoteAddr := net.ParseIP(remoteIP)
	if remoteAddr == nil {
		return remoteIP
	}
	isTrusted := false
	for _, n := range s.trustedProxyNets {
		if n.Contains(remoteAddr) {
			isTrusted = true
			break
		}
	}
	if !isTrusted {
		return remoteIP
	}

	// Trusted proxy: walk X-Forwarded-For right-to-left, skipping trusted
	// proxy IPs, to find the first non-trusted IP (the real client).
	// For single-proxy setups this yields the same result as leftmost.
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remoteIP
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		parsed := net.ParseIP(ip)
		if parsed == nil {
			continue
		}
		isTrusted := false
		for _, n := range s.trustedProxyNets {
			if n.Contains(parsed) {
				isTrusted = true
				break
			}
		}
		if !isTrusted {
			return ip
		}
	}
	// All XFF entries were trusted proxies; fall back to leftmost.
	if len(parts) > 0 {
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip
		}
	}
	return remoteIP
}

// isIPAllowed checks if an IP is in the allowed CIDR list.
func (s *Server) isIPAllowed(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range s.allowedIPs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// checkBearerToken validates the Authorization header against the expected token
// using constant-time comparison to prevent timing attacks.
func checkBearerToken(r *http.Request, expected string) bool {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return false
	}
	token := auth[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, code int, body interface{}) {
	data, err := json.Marshal(body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","reason":"internal marshal error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}

// parseCIDR parses a CIDR string, handling both "1.2.3.0/24" and "1.2.3.4/32"
// forms. A bare IP without /prefix is treated as /32 (IPv4) or /128 (IPv6).
func parseCIDR(s string) (*net.IPNet, error) {
	if !strings.Contains(s, "/") {
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP: %s", s)
		}
		if ip.To4() != nil {
			return &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}, nil
		}
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
	}
	_, ipNet, err := net.ParseCIDR(s)
	return ipNet, err
}
