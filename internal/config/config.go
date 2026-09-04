// Package config loads and validates prometheus-webhook configuration from a
// YAML file with optional environment-variable overrides.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"prometheus-webhook/internal/forward"
)

// WebhookConfig holds the HTTP webhook receiver settings.
type WebhookConfig struct {
	ListenAddr     string   `yaml:"listenAddr"`     // HTTP listen address
	Path           string   `yaml:"path"`           // webhook receive path, default /webhook
	Source         string   `yaml:"source"`         // RawEvent.source value, default "alertmanager"
	MaxBodyBytes   int64    `yaml:"maxBodyBytes"`   // request body size limit
	AllowedCIDRs   []string `yaml:"allowedCIDRs"`   // Alertmanager IP/CIDR allowlist; empty = token-only
	TrustedProxies []string `yaml:"trustedProxies"` // reverse proxy IPs; empty = no XFF trust
	AuthToken      string   `yaml:"authToken"`      // Bearer token; empty = reject all (fail-closed)
	DomainID       string   `yaml:"domainId"`       // CEP domain ID; empty = "default"
	MetricsPath    string   `yaml:"metricsPath"`    // Prometheus metrics path, default /metrics
	HealthPath     string   `yaml:"healthPath"`     // health check path, default /healthz
}

// TLSConfig holds optional mTLS settings.
type TLSConfig struct {
	Enabled      bool   `yaml:"enabled"`
	CertFile     string `yaml:"certFile"`
	KeyFile      string `yaml:"keyFile"`
	ClientCAFile string `yaml:"clientCAFile"`
}

// CEPEngineConfig describes the downstream cep-engine REST endpoint.
type CEPEngineConfig struct {
	BaseURL    string `yaml:"baseUrl"`
	BatchPath  string `yaml:"batchPath"`
	SinglePath string `yaml:"singlePath"`
	AuthToken  string `yaml:"authToken"`
	Timeout    int    `yaml:"timeoutMs"`
	RetryMax   int    `yaml:"retryMax"`
	RetryBase  int    `yaml:"retryBaseMs"`
}

// LoggingConfig controls log level and output file rotation settings.
type LoggingConfig struct {
	Level      string `yaml:"level"` // debug | info | warn | error
	File       string `yaml:"file"`  // empty -> stdout
	MaxSizeMB  int    `yaml:"maxSizeMB"`
	MaxBackups int    `yaml:"maxBackups"`
}

// Config is the root configuration.
type Config struct {
	Webhook   WebhookConfig         `yaml:"webhook"`
	TLS       TLSConfig             `yaml:"tls"`
	CEPEngine CEPEngineConfig       `yaml:"cepEngine"`
	Forward   forward.ForwardConfig `yaml:"forward"`
	Logging   LoggingConfig         `yaml:"logging"`
}

// Load reads a YAML config file and applies defaults and env overrides.
func Load(path string) (*Config, error) {
	cfg := defaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	applyEnv(cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// defaultConfig returns a Config populated with sensible defaults.
func defaultConfig() *Config {
	return &Config{
		Webhook: WebhookConfig{
			ListenAddr:   "0.0.0.0:9093",
			Path:         "/webhook",
			Source:       "alertmanager",
			MaxBodyBytes: 1048576, // 1 MiB
			MetricsPath:  "/metrics",
			HealthPath:   "/healthz",
		},
		CEPEngine: CEPEngineConfig{
			BatchPath:  "/api/v1/events/batch",
			SinglePath: "/api/v1/events",
			Timeout:    5000,
			RetryMax:   3,
			RetryBase:  200,
		},
		Forward: forward.DefaultForwardConfig,
		Logging: LoggingConfig{
			Level:      "info",
			MaxSizeMB:  100,
			MaxBackups: 5,
		},
	}
}

// applyEnv applies AMWH_* environment overrides.
func applyEnv(cfg *Config) {
	setStr := func(env string, dst *string) {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	setBool := func(env string, dst *bool) {
		if v := os.Getenv(env); v != "" {
			*dst = strings.EqualFold(v, "true") || v == "1"
		}
	}
	setInt := func(env string, dst *int) {
		if v := os.Getenv(env); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}

	setStr("AMWH_WEBHOOK_LISTENADDR", &cfg.Webhook.ListenAddr)
	setStr("AMWH_WEBHOOK_PATH", &cfg.Webhook.Path)
	setStr("AMWH_WEBHOOK_SOURCE", &cfg.Webhook.Source)
	setStr("AMWH_WEBHOOK_AUTH_TOKEN", &cfg.Webhook.AuthToken)
	setStr("AMWH_WEBHOOK_DOMAIN_ID", &cfg.Webhook.DomainID)
	setStr("AMWH_WEBHOOK_METRICS_PATH", &cfg.Webhook.MetricsPath)
	setStr("AMWH_WEBHOOK_HEALTH_PATH", &cfg.Webhook.HealthPath)
	setStr("AMWH_CEPENGINE_BASEURL", &cfg.CEPEngine.BaseURL)
	setStr("AMWH_CEPENGINE_AUTHTOKEN", &cfg.CEPEngine.AuthToken)
	setStr("AMWH_LOGGING_LEVEL", &cfg.Logging.Level)
	setStr("AMWH_LOGGING_FILE", &cfg.Logging.File)

	setStr("AMWH_TLS_CERT_FILE", &cfg.TLS.CertFile)
	setStr("AMWH_TLS_KEY_FILE", &cfg.TLS.KeyFile)
	setStr("AMWH_TLS_CLIENT_CA_FILE", &cfg.TLS.ClientCAFile)
	setBool("AMWH_TLS_ENABLED", &cfg.TLS.Enabled)

	setInt("AMWH_CEPENGINE_TIMEOUT", &cfg.CEPEngine.Timeout)
	setInt("AMWH_CEPENGINE_RETRY_MAX", &cfg.CEPEngine.RetryMax)
	setInt("AMWH_CEPENGINE_RETRY_BASE", &cfg.CEPEngine.RetryBase)

	if v := os.Getenv("AMWH_WEBHOOK_ALLOWED_CIDRS"); v != "" {
		cfg.Webhook.AllowedCIDRs = splitCommaList(v)
	}
	if v := os.Getenv("AMWH_WEBHOOK_TRUSTED_PROXIES"); v != "" {
		cfg.Webhook.TrustedProxies = splitCommaList(v)
	}
}

// validate checks required settings and normalizes values.
func validate(cfg *Config) error {
	if cfg.Webhook.ListenAddr == "" {
		return fmt.Errorf("config: webhook.listenAddr is required")
	}
	if cfg.Webhook.Path == "" {
		cfg.Webhook.Path = "/webhook"
	}
	if cfg.Webhook.Source == "" {
		cfg.Webhook.Source = "alertmanager"
	}
	if cfg.Webhook.MetricsPath == "" {
		cfg.Webhook.MetricsPath = "/metrics"
	}
	if cfg.Webhook.HealthPath == "" {
		cfg.Webhook.HealthPath = "/healthz"
	}
	if cfg.CEPEngine.BaseURL == "" {
		return fmt.Errorf("config: cepEngine.baseUrl is required")
	}
	// authToken empty is intentional (fail-closed); startup warning is
	// logged in main.go. No validation error here.
	if cfg.Forward.QueueFullPolicy == "" {
		cfg.Forward.QueueFullPolicy = string(forward.PolicyDrop)
	}
	switch forward.QueueFullPolicy(cfg.Forward.QueueFullPolicy) {
	case forward.PolicyDrop, forward.PolicyBlock, forward.PolicySingle:
	default:
		return fmt.Errorf("config: unsupported forward.queueFullPolicy %q", cfg.Forward.QueueFullPolicy)
	}
	return nil
}

// splitCommaList splits a comma-separated string, trimming whitespace and
// dropping empty entries.
func splitCommaList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
