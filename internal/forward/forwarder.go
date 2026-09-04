// Package forward transports RawEvents from prometheus-webhook to cep-engine.
// The primary transport is REST HTTP (batch POST). All forwarders implement
// the Forwarder interface so the batch queue can be transport-agnostic.
package forward

import (
	"context"

	"prometheus-webhook/internal/model"
)

// Forwarder sends RawEvents to a downstream target.
type Forwarder interface {
	// ForwardBatch posts a batch of events to the target.
	ForwardBatch(ctx context.Context, events []model.RawEvent) error
	// ForwardSingle posts a single event (used by the degraded path when the
	// queue is full under "single" policy).
	ForwardSingle(ctx context.Context, event *model.RawEvent) error
	// Close releases any resources held by the forwarder.
	Close() error
}

// QueueFullPolicy controls behaviour when the bounded batch queue is full.
type QueueFullPolicy string

const (
	// PolicyDrop discards the event, logs a drop record, and counts it.
	PolicyDrop QueueFullPolicy = "drop"
	// PolicyBlock blocks the producer until a slot frees up.
	PolicyBlock QueueFullPolicy = "block"
	// PolicySingle sends the event synchronously via ForwardSingle instead of
	// enqueuing it (best effort, avoids HTTP stall while still attempting).
	PolicySingle QueueFullPolicy = "single"
)

// DefaultConfig carries default values applied when a config omits them.
var DefaultConfig = Config{
	BatchSize:          50,
	BatchFlushInterval: 200, // milliseconds
	Workers:            4,
	QueueCapacity:      10000,
	QueueFullPolicy:    string(PolicyDrop),
	DropLogEnabled:     true,
}

// Config holds queue and batching parameters.
type Config struct {
	BatchSize          int    `yaml:"batchSize"`
	BatchFlushInterval int    `yaml:"batchFlushIntervalMs"`
	Workers            int    `yaml:"workers"`
	QueueCapacity      int    `yaml:"queueCapacity"`
	QueueFullPolicy    string `yaml:"queueFullPolicy"`
	DropLogEnabled     bool   `yaml:"dropLogEnabled"`
}
