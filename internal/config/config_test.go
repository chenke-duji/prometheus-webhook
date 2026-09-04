package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	path := writeTempConfig(t, `
webhook:
  listenAddr: "0.0.0.0:9093"
  authToken: "test-token"
  source: "alertmanager"
  domainId: "prod"
  maxBodyBytes: 1048576
  metricsPath: /metrics
  healthPath: /healthz
cepEngine:
  baseUrl: "http://localhost:8080"
  batchPath: /api/v1/events/batch
  timeoutMs: 5000
  retryMax: 3
  retryBaseMs: 200
forward:
  batchSize: 50
  batchFlushIntervalMs: 200
  workers: 4
  queueCapacity: 10000
  queueFullPolicy: drop
logging:
  level: info
  maxSizeMB: 100
  maxBackups: 5
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Webhook.ListenAddr != "0.0.0.0:9093" {
		t.Errorf("listenAddr: got %q", cfg.Webhook.ListenAddr)
	}
	if cfg.Webhook.AuthToken != "test-token" {
		t.Errorf("authToken: got %q", cfg.Webhook.AuthToken)
	}
	if cfg.Webhook.DomainID != "prod" {
		t.Errorf("domainId: got %q, want 'prod'", cfg.Webhook.DomainID)
	}
	if cfg.CEPEngine.BaseURL != "http://localhost:8080" {
		t.Errorf("baseUrl: got %q", cfg.CEPEngine.BaseURL)
	}
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTempConfig(t, `
webhook:
  authToken: "tok"
cepEngine:
  baseUrl: "http://localhost:8080"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Webhook.ListenAddr != "0.0.0.0:9093" {
		t.Errorf("default listenAddr: got %q", cfg.Webhook.ListenAddr)
	}
	if cfg.Webhook.Source != "alertmanager" {
		t.Errorf("default source: got %q", cfg.Webhook.Source)
	}
	if cfg.Webhook.Path != "/webhook" {
		t.Errorf("default path: got %q", cfg.Webhook.Path)
	}
	if cfg.Webhook.MaxBodyBytes != 1048576 {
		t.Errorf("default maxBodyBytes: got %d", cfg.Webhook.MaxBodyBytes)
	}
	if cfg.Forward.BatchSize != 50 {
		t.Errorf("default batchSize: got %d", cfg.Forward.BatchSize)
	}
}

func TestLoad_MissingListenAddr(t *testing.T) {
	path := writeTempConfig(t, `
webhook:
  listenAddr: ""
cepEngine:
  baseUrl: "http://localhost:8080"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty listenAddr")
	}
}

func TestLoad_MissingBaseURL(t *testing.T) {
	path := writeTempConfig(t, `
webhook:
  listenAddr: "0.0.0.0:9093"
  authToken: "tok"
cepEngine:
  baseUrl: ""
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty baseUrl")
	}
}

func TestLoad_InvalidQueueFullPolicy(t *testing.T) {
	path := writeTempConfig(t, `
webhook:
  listenAddr: "0.0.0.0:9093"
  authToken: "tok"
cepEngine:
  baseUrl: "http://localhost:8080"
forward:
  queueFullPolicy: "bogus"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid queueFullPolicy")
	}
}

func TestApplyEnv_Overrides(t *testing.T) {
	path := writeTempConfig(t, `
webhook:
  listenAddr: "0.0.0.0:9093"
  authToken: "original"
cepEngine:
  baseUrl: "http://localhost:8080"
`)

	t.Setenv("AMWH_WEBHOOK_AUTH_TOKEN", "env-token")
	t.Setenv("AMWH_WEBHOOK_SOURCE", "alertmanager_prod")
	t.Setenv("AMWH_WEBHOOK_DOMAIN_ID", "staging")
	t.Setenv("AMWH_CEPENGINE_BASEURL", "http://override:9090")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Webhook.AuthToken != "env-token" {
		t.Errorf("authToken override: got %q", cfg.Webhook.AuthToken)
	}
	if cfg.Webhook.Source != "alertmanager_prod" {
		t.Errorf("source override: got %q", cfg.Webhook.Source)
	}
	if cfg.Webhook.DomainID != "staging" {
		t.Errorf("domainId override: got %q", cfg.Webhook.DomainID)
	}
	if cfg.CEPEngine.BaseURL != "http://override:9090" {
		t.Errorf("baseUrl override: got %q", cfg.CEPEngine.BaseURL)
	}
}

func TestApplyEnv_CIDROverrides(t *testing.T) {
	path := writeTempConfig(t, `
webhook:
  listenAddr: "0.0.0.0:9093"
  authToken: "tok"
cepEngine:
  baseUrl: "http://localhost:8080"
`)
	t.Setenv("AMWH_WEBHOOK_ALLOWED_CIDRS", "10.0.0.0/8, 192.168.0.0/16")
	t.Setenv("AMWH_WEBHOOK_TRUSTED_PROXIES", "127.0.0.1/32, 10.0.0.1/32")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Webhook.AllowedCIDRs) != 2 {
		t.Errorf("allowedCIDRs: got %d, want 2", len(cfg.Webhook.AllowedCIDRs))
	}
	if cfg.Webhook.AllowedCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("allowedCIDRs[0]: got %q", cfg.Webhook.AllowedCIDRs[0])
	}
	if len(cfg.Webhook.TrustedProxies) != 2 {
		t.Errorf("trustedProxies: got %d, want 2", len(cfg.Webhook.TrustedProxies))
	}
}

func TestSplitCommaList(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"a,b,c", 3},
		{"  a , b , c  ", 3},
		{",,", 0},
		{"", 0},
		{"single", 1},
	}
	for _, tc := range cases {
		got := splitCommaList(tc.input)
		if len(got) != tc.want {
			t.Errorf("splitCommaList(%q): got %d items, want %d", tc.input, len(got), tc.want)
		}
	}
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
