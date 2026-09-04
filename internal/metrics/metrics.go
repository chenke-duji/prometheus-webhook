// Package metrics implements the self-monitoring counters exposed to
// Prometheus. It records receive/forward/failed/dropped/auth-rejected totals
// plus a sliding 5-minute throughput gauge, and renders the Prometheus text
// format over HTTP without pulling in a heavy client library.
package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// slidingWindowSeconds is the length of the throughput window (5 minutes).
const slidingWindowSeconds = 300

// Metrics implements forward.Recorder and serves a Prometheus /metrics text
// endpoint. All counters are thread-safe.
type Metrics struct {
	startTime time.Time

	received  atomic.Uint64
	forwarded atomic.Uint64
	failed    atomic.Uint64
	dropped   atomic.Uint64

	// Auth/security counters
	authRejected atomic.Uint64

	// Webhook request counters by HTTP status code
	webhookRequests sync.Map // map[int]*atomic.Uint64

	// Alert counts by status (firing/resolved)
	alertsByStatus sync.Map // map[string]*atomic.Uint64

	// Sliding window for throughput: one slot per wall-clock second.
	winMu    sync.Mutex
	winStamp []int64  // unix second per slot
	winCount []uint64 // received count per slot

	queueDepth func() int
}

// New creates a Metrics instance. queueDepth is called when rendering the
// metrics text to report the current forwarding queue depth.
func New(queueDepth func() int) *Metrics {
	return &Metrics{
		startTime:  time.Now(),
		winStamp:   make([]int64, slidingWindowSeconds),
		winCount:   make([]uint64, slidingWindowSeconds),
		queueDepth: queueDepth,
	}
}

// IncReceived records one received alert.
func (m *Metrics) IncReceived() {
	m.received.Add(1)
	now := time.Now().Unix()
	idx := now % slidingWindowSeconds
	m.winMu.Lock()
	if m.winStamp[idx] != now {
		m.winStamp[idx] = now
		m.winCount[idx] = 1
	} else {
		m.winCount[idx]++
	}
	m.winMu.Unlock()
}

// IncForwarded records n successfully forwarded events.
func (m *Metrics) IncForwarded(n uint64) { m.forwarded.Add(n) }

// IncForwardFailed records n failed forward attempts.
func (m *Metrics) IncForwardFailed(n uint64) { m.failed.Add(n) }

// IncDropped records n dropped events.
func (m *Metrics) IncDropped(n uint64) { m.dropped.Add(n) }

// IncAuthRejected records one auth/security rejection (401 or 403).
func (m *Metrics) IncAuthRejected() { m.authRejected.Add(1) }

// IncWebhookRequest records a webhook HTTP request by status code.
func (m *Metrics) IncWebhookRequest(statusCode int) {
	v, _ := m.webhookRequests.LoadOrStore(statusCode, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

// IncAlertByStatus records an alert by its status (firing/resolved).
func (m *Metrics) IncAlertByStatus(status string) {
	v, _ := m.alertsByStatus.LoadOrStore(status, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

// through5m computes the average received events/sec over the last 5 minutes.
func (m *Metrics) through5m() float64 {
	now := time.Now().Unix()
	floor := now - slidingWindowSeconds + 1
	var sum uint64
	var slots int64
	m.winMu.Lock()
	for i := 0; i < slidingWindowSeconds; i++ {
		if m.winStamp[i] >= floor && m.winStamp[i] <= now {
			sum += m.winCount[i]
			slots++
		}
	}
	m.winMu.Unlock()
	if slots == 0 {
		return 0
	}
	return float64(sum) / float64(slidingWindowSeconds)
}

// Handler returns an http.Handler that renders the metrics in Prometheus text
// format (content-type: text/plain; version=0.0.4).
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		depth := 0
		if m.queueDepth != nil {
			depth = m.queueDepth()
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		lines := []string{
			"# HELP alertmanager_received_total Total alerts received from Alertmanager.",
			"# TYPE alertmanager_received_total counter",
			fmt.Sprintf("alertmanager_received_total %d", m.received.Load()),
			"# HELP alertmanager_forwarded_total Total events successfully forwarded.",
			"# TYPE alertmanager_forwarded_total counter",
			fmt.Sprintf("alertmanager_forwarded_total %d", m.forwarded.Load()),
			"# HELP alertmanager_forward_failed_total Total events that failed to forward.",
			"# TYPE alertmanager_forward_failed_total counter",
			fmt.Sprintf("alertmanager_forward_failed_total %d", m.failed.Load()),
			"# HELP alertmanager_dropped_total Total events dropped (queue full).",
			"# TYPE alertmanager_dropped_total counter",
			fmt.Sprintf("alertmanager_dropped_total %d", m.dropped.Load()),
			"# HELP alertmanager_auth_rejected_total Total requests rejected by security layer (401/403).",
			"# TYPE alertmanager_auth_rejected_total counter",
			fmt.Sprintf("alertmanager_auth_rejected_total %d", m.authRejected.Load()),
			"# HELP alertmanager_queue_depth Current forwarding queue depth.",
			"# TYPE alertmanager_queue_depth gauge",
			fmt.Sprintf("alertmanager_queue_depth %d", depth),
			"# HELP alertmanagerd_start_time_seconds Unix time when the daemon started.",
			"# TYPE alertmanagerd_start_time_seconds gauge",
			fmt.Sprintf("alertmanagerd_start_time_seconds %d", m.startTime.Unix()),
			"# HELP alertmanager_throughput_5m Average received alerts/sec over the last 5 minutes.",
			"# TYPE alertmanager_throughput_5m gauge",
			fmt.Sprintf("alertmanager_throughput_5m %f", m.through5m()),
		}
		// Webhook requests by status code
		lines = append(lines,
			"# HELP alertmanager_webhook_requests_total Total webhook HTTP requests by status code.",
			"# TYPE alertmanager_webhook_requests_total counter",
		)
		m.webhookRequests.Range(func(key, val any) bool {
			lines = append(lines, fmt.Sprintf(`alertmanager_webhook_requests_total{status="%d"} %d`,
				key.(int), val.(*atomic.Uint64).Load()))
			return true
		})
		// Alerts by status
		lines = append(lines,
			"# HELP alertmanager_alerts_by_status_total Total alerts by status (firing/resolved).",
			"# TYPE alertmanager_alerts_by_status_total counter",
		)
		m.alertsByStatus.Range(func(key, val any) bool {
			lines = append(lines, fmt.Sprintf(`alertmanager_alerts_by_status_total{status="%s"} %d`,
				key.(string), val.(*atomic.Uint64).Load()))
			return true
		})
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	})
}
