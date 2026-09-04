package webhook

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"
	"prometheus-webhook/internal/config"
	"prometheus-webhook/internal/forward"
	"prometheus-webhook/internal/metrics"
	"prometheus-webhook/internal/model"
)

type noopForwarder struct{}

func (n *noopForwarder) ForwardBatch(_ context.Context, _ []model.RawEvent) error { return nil }
func (n *noopForwarder) ForwardSingle(_ context.Context, _ *model.RawEvent) error { return nil }
func (n *noopForwarder) Close() error                                             { return nil }

func TestBearerToken_Missing(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:  "127.0.0.1:0",
			Path:        "/webhook",
			Source:      "alertmanager",
			AuthToken:   "secret-token",
			MetricsPath: "/metrics",
			HealthPath:  "/healthz",
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader("{}"))
	req.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.handleWebhook(w, req)
	if w.Code != 401 {
		t.Errorf("missing token: got %d, want 401", w.Code)
	}
}

func TestBearerToken_Wrong(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:  "127.0.0.1:0",
			Path:        "/webhook",
			Source:      "alertmanager",
			AuthToken:   "secret-token",
			MetricsPath: "/metrics",
			HealthPath:  "/healthz",
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.handleWebhook(w, req)
	if w.Code != 401 {
		t.Errorf("wrong token: got %d, want 401", w.Code)
	}
}

func TestBearerToken_Correct(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:   "127.0.0.1:0",
			Path:         "/webhook",
			Source:       "alertmanager",
			AuthToken:    "secret-token",
			MetricsPath:  "/metrics",
			HealthPath:   "/healthz",
			MaxBodyBytes: 1048576,
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)

	body := `{"version":"4","status":"firing","receiver":"test","alerts":[]}`
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.handleWebhook(w, req)
	if w.Code != 200 {
		t.Errorf("correct token: got %d, want 200", w.Code)
	}
}

func TestAuthTokenEmpty_RejectsAll(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:  "127.0.0.1:0",
			Path:        "/webhook",
			Source:      "alertmanager",
			AuthToken:   "", // fail-closed
			MetricsPath: "/metrics",
			HealthPath:  "/healthz",
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer any-token")
	req.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.handleWebhook(w, req)
	if w.Code != 401 {
		t.Errorf("empty authToken should reject all: got %d, want 401", w.Code)
	}
}

func TestIPAllowlist_Allowed(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:   "127.0.0.1:0",
			Path:         "/webhook",
			Source:       "alertmanager",
			AuthToken:    "token",
			AllowedCIDRs: []string{"10.0.0.0/8"},
			MetricsPath:  "/metrics",
			HealthPath:   "/healthz",
			MaxBodyBytes: 1048576,
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)
	body := `{"version":"4","status":"firing","receiver":"test","alerts":[]}`
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.RemoteAddr = "10.0.0.5:1234"
	w := httptest.NewRecorder()
	srv.handleWebhook(w, req)
	if w.Code != 200 {
		t.Errorf("allowed IP: got %d, want 200", w.Code)
	}
}

func TestIPAllowlist_Blocked(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:   "127.0.0.1:0",
			Path:         "/webhook",
			Source:       "alertmanager",
			AuthToken:    "token",
			AllowedCIDRs: []string{"10.0.0.0/8"},
			MetricsPath:  "/metrics",
			HealthPath:   "/healthz",
			MaxBodyBytes: 1048576,
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer token")
	req.RemoteAddr = "192.168.1.1:1234"
	w := httptest.NewRecorder()
	srv.handleWebhook(w, req)
	if w.Code != 403 {
		t.Errorf("blocked IP: got %d, want 403", w.Code)
	}
}

func TestXFF_TrustedProxy(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:     "127.0.0.1:0",
			Path:           "/webhook",
			Source:         "alertmanager",
			AuthToken:      "token",
			AllowedCIDRs:   []string{"10.0.0.0/8"},
			TrustedProxies: []string{"127.0.0.1/32"},
			MetricsPath:    "/metrics",
			HealthPath:     "/healthz",
			MaxBodyBytes:   1048576,
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)

	body := `{"version":"4","status":"firing","receiver":"test","alerts":[]}`
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Forwarded-For", "10.0.0.50, 127.0.0.1")
	req.RemoteAddr = "127.0.0.1:54321" // trusted proxy

	w := httptest.NewRecorder()
	srv.handleWebhook(w, req)
	if w.Code != 200 {
		t.Errorf("XFF trusted proxy: got %d, want 200 (real IP 10.0.0.50 should be allowed)", w.Code)
	}
}

func TestXFF_UntrustedProxy(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:     "127.0.0.1:0",
			Path:           "/webhook",
			Source:         "alertmanager",
			AuthToken:      "token",
			AllowedCIDRs:   []string{"10.0.0.0/8"},
			TrustedProxies: []string{"127.0.0.1/32"},
			MetricsPath:    "/metrics",
			HealthPath:     "/healthz",
			MaxBodyBytes:   1048576,
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)

	req := httptest.NewRequest("POST", "/webhook", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Forwarded-For", "10.0.0.50") // spoofed
	req.RemoteAddr = "192.168.1.1:54321"           // NOT a trusted proxy

	w := httptest.NewRecorder()
	srv.handleWebhook(w, req)
	if w.Code != 403 {
		t.Errorf("untrusted proxy: got %d, want 403 (RemoteAddr 192.168.1.1 not in allowedCIDRs)", w.Code)
	}
}

func TestXFF_NoTrustedProxies_IgnoresHeader(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:     "127.0.0.1:0",
			Path:           "/webhook",
			Source:         "alertmanager",
			AuthToken:      "token",
			AllowedCIDRs:   []string{"10.0.0.0/8"},
			TrustedProxies: []string{}, // empty = no XFF trust
			MetricsPath:    "/metrics",
			HealthPath:     "/healthz",
			MaxBodyBytes:   1048576,
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)

	req := httptest.NewRequest("POST", "/webhook", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Forwarded-For", "10.0.0.50") // spoofed, should be ignored
	req.RemoteAddr = "192.168.1.1:54321"

	w := httptest.NewRecorder()
	srv.handleWebhook(w, req)
	if w.Code != 403 {
		t.Errorf("no trustedProxies: should ignore XFF, got %d, want 403", w.Code)
	}
}

func TestAlertPayloadParsing(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:   "127.0.0.1:0",
			Path:         "/webhook",
			Source:       "alertmanager",
			AuthToken:    "token",
			MetricsPath:  "/metrics",
			HealthPath:   "/healthz",
			MaxBodyBytes: 1048576,
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)

	body := `{
		"version": "4",
		"status": "firing",
		"receiver": "cep-webhook",
		"groupKey": "alertname=HighCPU",
		"externalURL": "http://am:9093",
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU", "instance": "node1", "severity": "critical"},
				"annotations": {"summary": "CPU > 80%"},
				"startsAt": "2026-09-04T10:00:00.000Z",
				"endsAt": "2026-09-04T10:05:00.000Z",
				"generatorURL": "http://prom:9090/graph",
				"fingerprint": "abc123"
			},
			{
				"status": "resolved",
				"labels": {"alertname": "DiskFull", "instance": "node2", "severity": "warning"},
				"annotations": {"summary": "Disk > 90%"},
				"startsAt": "2026-09-04T09:00:00.000Z",
				"endsAt": "2026-09-04T10:00:00.000Z",
				"generatorURL": "http://prom:9090/graph2",
				"fingerprint": "def456"
			}
		]
	}`

	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:1234"

	w := httptest.NewRecorder()
	srv.handleWebhook(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["count"].(float64) != 2 {
		t.Errorf("expected count=2, got %v", resp["count"])
	}
}

func TestHealthEndpoint(t *testing.T) {
	cfg := &config.Config{
		Webhook: config.WebhookConfig{
			ListenAddr:  "127.0.0.1:0",
			Path:        "/webhook",
			Source:      "alertmanager",
			MetricsPath: "/metrics",
			HealthPath:  "/healthz",
		},
		CEPEngine: config.CEPEngineConfig{BaseURL: "http://localhost:8080"},
		Forward:   forward.DefaultForwardConfig,
	}
	srv := mustNewServer(t, cfg)

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	srv.handleHealth(w, req)
	if w.Code != 200 {
		t.Errorf("health: got %d, want 200", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Errorf("health response: got %q, want 'ok'", resp["status"])
	}
}

// mustNewServer creates a Server for testing, failing on error.
// The metrics instance is wired into the queue (same closure pattern as
// main.go) so that queue->metrics counters are correctly linked.
func mustNewServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	fwd := &noopForwarder{}
	fwdCfg := forward.ForwardConfig{
		BatchSize:          10,
		BatchFlushInterval: 100,
		Workers:            1,
		QueueCapacity:      100,
		QueueFullPolicy:    "drop",
	}
	var queue *forward.BatchQueue
	m := metrics.New(func() int {
		if queue != nil {
			return queue.QueueDepth()
		}
		return 0
	})
	queue = forward.NewBatchQueue(fwdCfg, fwd, m, slog.Default())
	srv, err := New(cfg, queue, m, slog.Default())
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	return srv
}
