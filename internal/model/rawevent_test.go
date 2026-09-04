package model

import (
	"strings"
	"testing"
	"time"
)

func TestNewFromAlert_RawEventDeterministic(t *testing.T) {
	alert := &Alert{
		Status: "firing",
		Labels: map[string]string{
			"alertname": "HighCPU",
			"instance":  "node1.example.com",
			"severity":  "critical",
			"job":       "node_exporter",
		},
		Annotations:  map[string]string{"summary": "CPU usage > 80%"},
		StartsAt:     "2026-09-04T10:00:00.000Z",
		EndsAt:       "2026-09-04T10:05:00.000Z",
		GeneratorURL: "http://prometheus:9090/graph?g0.expr=cpu_usage",
		Fingerprint:  "e3b0c44298fc1c14",
	}
	payload := &AlertPayload{
		Version:      "4",
		GroupKey:     "alertname=HighCPU,instance=node1",
		Status:       "firing",
		Receiver:     "cep-webhook",
		Alerts:       []Alert{*alert},
		ExternalURL:  "http://alertmanager:9093",
		CommonLabels: map[string]string{"alertname": "HighCPU", "severity": "critical"},
		GroupLabels:  map[string]string{"alertname": "HighCPU"},
	}

	ev1 := NewFromAlert(alert, payload, "alertmanager", "10.0.0.5", "default", time.Now())
	ev2 := NewFromAlert(alert, payload, "alertmanager", "10.0.0.5", "default", time.Now().Add(time.Second))

	if ev1.RawEvent != ev2.RawEvent {
		t.Errorf("rawEvent must be deterministic across instances:\n  ev1=%q\n  ev2=%q", ev1.RawEvent, ev2.RawEvent)
	}
	if ev1.OriginTimestamp != ev2.OriginTimestamp {
		t.Errorf("originTimestamp must be deterministic: %d != %d", ev1.OriginTimestamp, ev2.OriginTimestamp)
	}
}

func TestNewFromAlert_RawEventFormat(t *testing.T) {
	alert := &Alert{
		Status: "firing",
		Labels: map[string]string{
			"alertname": "HighCPU",
			"instance":  "node1.example.com",
			"severity":  "critical",
			"job":       "node_exporter",
		},
		StartsAt:     "2026-09-04T10:00:00.000Z",
		GeneratorURL: "http://prometheus:9090/graph",
		Fingerprint:  "abc123",
	}
	payload := &AlertPayload{Version: "4", Receiver: "cep-webhook"}

	ev := NewFromAlert(alert, payload, "alertmanager", "10.0.0.1", "default", time.Now())

	expected := "alertmanager|firing|HighCPU|node1.example.com|critical|node_exporter|abc123|2026-09-04T10:00:00.000Z|http://prometheus:9090/graph"
	if ev.RawEvent != expected {
		t.Errorf("rawEvent mismatch:\n  got:  %q\n  want: %q", ev.RawEvent, expected)
	}
}

func TestNewFromAlert_MissingLabelsRenderEmpty(t *testing.T) {
	alert := &Alert{
		Status:      "resolved",
		Labels:      map[string]string{"alertname": "DiskFull"},
		StartsAt:    "2026-09-04T11:00:00.000Z",
		Fingerprint: "def456",
	}
	payload := &AlertPayload{Version: "4"}

	ev := NewFromAlert(alert, payload, "alertmanager", "192.168.1.1", "default", time.Now())

	parts := strings.Split(ev.RawEvent, "|")
	if len(parts) != 9 {
		t.Fatalf("expected 9 pipe-separated fields, got %d", len(parts))
	}
	// instance, severity, job should be empty
	if parts[3] != "" || parts[4] != "" || parts[5] != "" {
		t.Errorf("missing labels should render empty: instance=%q severity=%q job=%q", parts[3], parts[4], parts[5])
	}
	if parts[0] != "alertmanager" {
		t.Errorf("source mismatch: got %q", parts[0])
	}
	if parts[1] != "resolved" {
		t.Errorf("status mismatch: got %q", parts[1])
	}
	if parts[2] != "DiskFull" {
		t.Errorf("alertname mismatch: got %q", parts[2])
	}
	if parts[6] != "def456" {
		t.Errorf("fingerprint mismatch: got %q", parts[6])
	}
}

func TestNewFromAlert_MetadataFields(t *testing.T) {
	alert := &Alert{
		Status:       "firing",
		Labels:       map[string]string{"alertname": "HighCPU", "severity": "critical"},
		Annotations:  map[string]string{"summary": "CPU high"},
		StartsAt:     "2026-09-04T10:00:00.000Z",
		EndsAt:       "2026-09-04T10:05:00.000Z",
		GeneratorURL: "http://prometheus:9090/graph",
		Fingerprint:  "abc123",
	}
	payload := &AlertPayload{
		Version:      "4",
		GroupKey:     "alertname=HighCPU",
		Receiver:     "cep-webhook",
		ExternalURL:  "http://am:9093",
		CommonLabels: map[string]string{"alertname": "HighCPU"},
		GroupLabels:  map[string]string{"alertname": "HighCPU"},
	}

	ev := NewFromAlert(alert, payload, "alertmanager", "10.0.0.1", "default", time.Now())

	checks := map[string]string{
		"status":       "firing",
		"fingerprint":  "abc123",
		"startsAt":     "2026-09-04T10:00:00.000Z",
		"endsAt":       "2026-09-04T10:05:00.000Z",
		"generatorURL": "http://prometheus:9090/graph",
		"groupKey":     "alertname=HighCPU",
		"receiver":     "cep-webhook",
		"externalURL":  "http://am:9093",
		"domainId":     "default",
	}
	for key, want := range checks {
		got, ok := ev.Metadata[key]
		if !ok {
			t.Errorf("metadata missing key %q", key)
			continue
		}
		if got != want {
			t.Errorf("metadata[%q] = %v, want %q", key, got, want)
		}
	}

	// labels and annotations should be maps
	if _, ok := ev.Metadata["labels"].(map[string]string); !ok {
		t.Errorf("metadata[labels] should be map[string]string")
	}
	if _, ok := ev.Metadata["annotations"].(map[string]string); !ok {
		t.Errorf("metadata[annotations] should be map[string]string")
	}
}

func TestNewFromAlert_OriginTimestampFromStartsAt(t *testing.T) {
	alert := &Alert{
		Status:      "firing",
		Labels:      map[string]string{"alertname": "Test"},
		StartsAt:    "2026-09-04T10:00:00.000Z",
		Fingerprint: "fp",
	}
	payload := &AlertPayload{}

	ev := NewFromAlert(alert, payload, "alertmanager", "1.2.3.4", "default", time.Now())

	// 2026-09-04T10:00:00.000Z = epoch millis
	expected := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC).UnixMilli()
	if ev.OriginTimestamp != expected {
		t.Errorf("originTimestamp = %d, want %d", ev.OriginTimestamp, expected)
	}
}

func TestNewFromAlert_OriginTimestampFallback(t *testing.T) {
	alert := &Alert{
		Status:      "firing",
		Labels:      map[string]string{"alertname": "Test"},
		StartsAt:    "not-a-date",
		Fingerprint: "fp",
	}
	payload := &AlertPayload{}

	ev := NewFromAlert(alert, payload, "alertmanager", "1.2.3.4", "default", time.Now())

	if ev.OriginTimestamp <= 0 {
		t.Errorf("originTimestamp should fall back to deterministic hash, got %d", ev.OriginTimestamp)
	}

	// Same input should produce same fallback
	ev2 := NewFromAlert(alert, payload, "alertmanager", "1.2.3.4", "default", time.Now())
	if ev.OriginTimestamp != ev2.OriginTimestamp {
		t.Errorf("fallback originTimestamp must be deterministic: %d != %d", ev.OriginTimestamp, ev2.OriginTimestamp)
	}
}

func TestNewFromAlert_DifferentAlertsDifferentRawEvent(t *testing.T) {
	alert1 := &Alert{
		Status:      "firing",
		Labels:      map[string]string{"alertname": "A", "instance": "node1"},
		StartsAt:    "2026-09-04T10:00:00.000Z",
		Fingerprint: "fp1",
	}
	alert2 := &Alert{
		Status:      "firing",
		Labels:      map[string]string{"alertname": "B", "instance": "node2"},
		StartsAt:    "2026-09-04T10:00:00.000Z", // same startsAt!
		Fingerprint: "fp2",
	}
	payload := &AlertPayload{}

	ev1 := NewFromAlert(alert1, payload, "alertmanager", "1.2.3.4", "default", time.Now())
	ev2 := NewFromAlert(alert2, payload, "alertmanager", "1.2.3.4", "default", time.Now())

	if ev1.RawEvent == ev2.RawEvent {
		t.Errorf("different alerts must produce different rawEvent:\n  ev1=%q\n  ev2=%q", ev1.RawEvent, ev2.RawEvent)
	}
}

func TestNewFromAlert_FiringResolvedDifferentRawEvent(t *testing.T) {
	// Same alert, different status -> different rawEvent (for dedup)
	base := &Alert{
		Labels:      map[string]string{"alertname": "A", "instance": "node1"},
		StartsAt:    "2026-09-04T10:00:00.000Z",
		Fingerprint: "fp1",
	}
	payload := &AlertPayload{}

	firing := &Alert{Status: "firing", Labels: base.Labels, StartsAt: base.StartsAt, Fingerprint: base.Fingerprint}
	resolved := &Alert{Status: "resolved", Labels: base.Labels, StartsAt: base.StartsAt, Fingerprint: base.Fingerprint}

	ev1 := NewFromAlert(firing, payload, "alertmanager", "1.2.3.4", "default", time.Now())
	ev2 := NewFromAlert(resolved, payload, "alertmanager", "1.2.3.4", "default", time.Now())

	if ev1.RawEvent == ev2.RawEvent {
		t.Errorf("firing and resolved must produce different rawEvent for same alert")
	}
}

func TestNewFromAlert_SourceInjectable(t *testing.T) {
	alert := &Alert{Status: "firing", Labels: map[string]string{"alertname": "A"}, StartsAt: "2026-09-04T10:00:00.000Z", Fingerprint: "fp"}
	payload := &AlertPayload{}

	ev := NewFromAlert(alert, payload, "alertmanager_prod", "1.2.3.4", "default", time.Now())
	if ev.Source != "alertmanager_prod" {
		t.Errorf("source = %q, want %q", ev.Source, "alertmanager_prod")
	}
	if !strings.HasPrefix(ev.RawEvent, "alertmanager_prod|") {
		t.Errorf("rawEvent should start with source: %q", ev.RawEvent)
	}
}
