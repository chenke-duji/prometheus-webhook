// Package model defines the RawEvent JSON structure that prometheus-webhook
// builds and forwards to cep-engine. Field names and structure are strictly
// aligned with cep-engine's com.dujitech.cep.model.RawEvent (deserialized
// with Gson, so JSON keys must match the Java field names exactly).
package model

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// RawEvent is the payload posted to cep-engine.
type RawEvent struct {
	Source          string                 `json:"source"`
	SourceIP        string                 `json:"sourceIp"`
	ReceivedAt      int64                  `json:"receivedAt"`
	OriginTimestamp int64                  `json:"originTimestamp"`
	RawEvent        string                 `json:"rawEvent"`
	Metadata        map[string]interface{} `json:"metadata"`
}

// Alert represents a single alert from an Alertmanager webhook payload.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// AlertPayload is the top-level Alertmanager webhook JSON structure.
type AlertPayload struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

// NewFromAlert builds a RawEvent from a single Alertmanager alert.
//
// Metadata carries the structured fields consumed by cep-engine's
// alertmanager_parser.groovy:
//
//	status (string), fingerprint (string), labels (map), annotations (map),
//	startsAt (RFC3339), endsAt (RFC3339), generatorURL, groupKey, receiver,
//	externalURL, commonLabels, groupLabels, domainId.
//
// originTimestamp uses the alert's startsAt parsed to epoch millis (UTC).
// If parsing fails, a deterministic hash of startsAt is used so multiple
// Active-Active daemon instances still produce an identical value.
//
// rawEvent is a deterministic pipe-separated rendering (no receivedAt/endsAt)
// used by cep-engine's TransportDeduplicator fingerprint.
func NewFromAlert(alert *Alert, payload *AlertPayload, source, realClientIP string, domainID string, receivedAt time.Time) *RawEvent {
	metadata := make(map[string]interface{}, 14)

	metadata["status"] = alert.Status
	metadata["fingerprint"] = alert.Fingerprint
	metadata["labels"] = alert.Labels
	metadata["annotations"] = alert.Annotations
	metadata["startsAt"] = alert.StartsAt
	metadata["endsAt"] = alert.EndsAt
	metadata["generatorURL"] = alert.GeneratorURL
	metadata["groupKey"] = payload.GroupKey
	metadata["receiver"] = payload.Receiver
	metadata["externalURL"] = payload.ExternalURL
	metadata["commonLabels"] = payload.CommonLabels
	metadata["groupLabels"] = payload.GroupLabels
	metadata["domainId"] = domainID

	rawText := renderRawEvent(source, alert)

	origin := parseRFC3339Millis(alert.StartsAt)
	if origin <= 0 {
		origin = deterministicHash(alert.StartsAt)
	}

	return &RawEvent{
		Source:          source,
		SourceIP:        realClientIP,
		ReceivedAt:      receivedAt.UnixMilli(),
		OriginTimestamp: origin,
		RawEvent:        rawText,
		Metadata:        metadata,
	}
}

// renderRawEvent produces a deterministic pipe-separated string for the
// TransportDeduplicator fingerprint. Format:
//
//	source|status|alertname|instance|severity|job|fingerprint|startsAt|generatorURL
//
// Missing fields render as empty strings; pipe positions are preserved.
// Excludes receivedAt and endsAt for cross-instance determinism.
func renderRawEvent(source string, alert *Alert) string {
	labels := alert.Labels
	get := func(k string) string {
		if v, ok := labels[k]; ok {
			return v
		}
		return ""
	}
	parts := []string{
		source,
		alert.Status,
		get("alertname"),
		get("instance"),
		get("severity"),
		get("job"),
		alert.Fingerprint,
		alert.StartsAt,
		alert.GeneratorURL,
	}
	return strings.Join(parts, "|")
}

// parseRFC3339Millis parses an RFC3339 timestamp and returns epoch millis.
// Returns 0 on failure.
func parseRFC3339Millis(s string) int64 {
	if s == "" {
		return 0
	}
	// Alertmanager uses RFC3339 with a 'Z' or offset suffix.
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// Try without sub-seconds.
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return 0
		}
	}
	return t.UTC().UnixMilli()
}

// deterministicHash returns a stable int64 derived from a string.
// Uses the first 8 bytes of SHA-256 as an unsigned 63-bit value.
func deterministicHash(s string) int64 {
	sum := sha256.Sum256([]byte(s))
	var u uint64
	for i := 0; i < 8; i++ {
		u = u<<8 | uint64(sum[i])
	}
	v := int64(u &^ (1 << 63)) //#nosec G115 -- masked to <= 2^63-1, conversion is safe
	if v == 0 {
		v = 1
	}
	return v
}

// String returns a human-readable summary for logging.
func (e *RawEvent) String() string {
	return fmt.Sprintf("source=%s sourceIp=%s originTs=%d rawLen=%d",
		e.Source, e.SourceIP, e.OriginTimestamp, len(e.RawEvent))
}
