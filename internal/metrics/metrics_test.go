package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIncReceived(t *testing.T) {
	m := New(nil)
	m.IncReceived()
	m.IncReceived()
	m.IncReceived()
	body := renderMetrics(m)
	if !strings.Contains(body, "alertmanager_received_total 3") {
		t.Errorf("expected received_total 3, got:\n%s", body)
	}
}

func TestIncForwarded(t *testing.T) {
	m := New(nil)
	m.IncForwarded(5)
	m.IncForwarded(3)
	body := renderMetrics(m)
	if !strings.Contains(body, "alertmanager_forwarded_total 8") {
		t.Errorf("expected forwarded_total 8, got:\n%s", body)
	}
}

func TestIncForwardFailed(t *testing.T) {
	m := New(nil)
	m.IncForwardFailed(2)
	body := renderMetrics(m)
	if !strings.Contains(body, "alertmanager_forward_failed_total 2") {
		t.Errorf("expected forward_failed_total 2, got:\n%s", body)
	}
}

func TestIncDropped(t *testing.T) {
	m := New(nil)
	m.IncDropped(4)
	body := renderMetrics(m)
	if !strings.Contains(body, "alertmanager_dropped_total 4") {
		t.Errorf("expected dropped_total 4, got:\n%s", body)
	}
}

func TestIncAuthRejected(t *testing.T) {
	m := New(nil)
	m.IncAuthRejected()
	m.IncAuthRejected()
	body := renderMetrics(m)
	if !strings.Contains(body, "alertmanager_auth_rejected_total 2") {
		t.Errorf("expected auth_rejected_total 2, got:\n%s", body)
	}
}

func TestIncWebhookRequest(t *testing.T) {
	m := New(nil)
	m.IncWebhookRequest(200)
	m.IncWebhookRequest(200)
	m.IncWebhookRequest(401)
	m.IncWebhookRequest(500)
	body := renderMetrics(m)
	if !strings.Contains(body, `alertmanager_webhook_requests_total{status="200"} 2`) {
		t.Errorf("expected 2 requests with status 200, got:\n%s", body)
	}
	if !strings.Contains(body, `alertmanager_webhook_requests_total{status="401"} 1`) {
		t.Errorf("expected 1 request with status 401, got:\n%s", body)
	}
	if !strings.Contains(body, `alertmanager_webhook_requests_total{status="500"} 1`) {
		t.Errorf("expected 1 request with status 500, got:\n%s", body)
	}
}

func TestIncAlertByStatus(t *testing.T) {
	m := New(nil)
	m.IncAlertByStatus("firing")
	m.IncAlertByStatus("firing")
	m.IncAlertByStatus("resolved")
	body := renderMetrics(m)
	if !strings.Contains(body, `alertmanager_alerts_by_status_total{status="firing"} 2`) {
		t.Errorf("expected 2 firing alerts, got:\n%s", body)
	}
	if !strings.Contains(body, `alertmanager_alerts_by_status_total{status="resolved"} 1`) {
		t.Errorf("expected 1 resolved alert, got:\n%s", body)
	}
}

func TestHandler_QueueDepth(t *testing.T) {
	depth := 0
	m := New(func() int { return depth })

	depth = 42
	body := renderMetrics(m)
	if !strings.Contains(body, "alertmanager_queue_depth 42") {
		t.Errorf("expected queue_depth 42, got:\n%s", body)
	}

	depth = 0
	body = renderMetrics(m)
	if !strings.Contains(body, "alertmanager_queue_depth 0") {
		t.Errorf("expected queue_depth 0, got:\n%s", body)
	}
}

func TestThrough5m_NoData(t *testing.T) {
	m := New(nil)
	body := renderMetrics(m)
	// With no data, throughput should be 0.
	if !strings.Contains(body, "alertmanager_throughput_5m 0.000000") {
		t.Errorf("expected throughput_5m 0.000000, got:\n%s", body)
	}
}

func TestHandler_PrometheusFormat(t *testing.T) {
	m := New(nil)
	m.IncReceived()
	body := renderMetrics(m)

	// Verify essential Prometheus exposition format markers.
	required := []string{
		"# HELP alertmanager_received_total",
		"# TYPE alertmanager_received_total counter",
		"# HELP alertmanager_forwarded_total",
		"# TYPE alertmanager_forwarded_total counter",
		"# HELP alertmanager_queue_depth",
		"# TYPE alertmanager_queue_depth gauge",
		"# HELP alertmanagerd_start_time_seconds",
		"# TYPE alertmanagerd_start_time_seconds gauge",
	}
	for _, marker := range required {
		if !strings.Contains(body, marker) {
			t.Errorf("missing marker %q in output:\n%s", marker, body)
		}
	}
}

// renderMetrics invokes the Metrics Handler and returns the body.
func renderMetrics(m *Metrics) string {
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}
