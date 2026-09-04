package forward

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"prometheus-webhook/internal/model"
)

// Recorder receives operational counters from the batch queue. It is
// implemented by the metrics package; defined here as an interface to avoid an
// import cycle (forward must not import metrics).
type Recorder interface {
	IncReceived()
	IncForwarded(n uint64)
	IncForwardFailed(n uint64)
	IncDropped(n uint64)
}

// noopRecorder is the default recorder when metrics are disabled.
type noopRecorder struct{}

func (noopRecorder) IncReceived()            {}
func (noopRecorder) IncForwarded(uint64)     {}
func (noopRecorder) IncForwardFailed(uint64) {}
func (noopRecorder) IncDropped(uint64)       {}

// BatchQueue is a bounded, multi-worker forwarding queue with backpressure.
// Producers call Enqueue; workers batch events and push them through a
// Forwarder. When the bounded channel is full the queueFullPolicy applies.
type BatchQueue struct {
	cfg       Config
	forwarder Forwarder
	recorder  Recorder
	log       *slog.Logger
	ctx       context.Context
	cancel    context.CancelFunc

	ch chan *model.RawEvent
	wg sync.WaitGroup
}

// NewBatchQueue creates a batch queue with the given config and forwarder.
func NewBatchQueue(cfg Config, f Forwarder, rec Recorder, log *slog.Logger) *BatchQueue {
	if rec == nil {
		rec = noopRecorder{}
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultConfig.BatchSize
	}
	if cfg.BatchFlushInterval <= 0 {
		cfg.BatchFlushInterval = DefaultConfig.BatchFlushInterval
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultConfig.Workers
	}
	if cfg.QueueCapacity <= 0 {
		cfg.QueueCapacity = DefaultConfig.QueueCapacity
	}
	if cfg.QueueFullPolicy == "" {
		cfg.QueueFullPolicy = DefaultConfig.QueueFullPolicy
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &BatchQueue{
		cfg:       cfg,
		forwarder: f,
		recorder:  rec,
		log:       log,
		ctx:       ctx,
		cancel:    cancel,
		ch:        make(chan *model.RawEvent, cfg.QueueCapacity),
	}
}

// Start launches the worker pool. It is idempotent-safe to call once.
func (q *BatchQueue) Start() {
	for i := 0; i < q.cfg.Workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
}

// QueueDepth returns the current number of buffered events.
func (q *BatchQueue) QueueDepth() int {
	return len(q.ch)
}

// Enqueue adds an event to the queue. It never blocks the HTTP handler except
// under PolicyBlock. Returns true if the event was accepted (enqueued or sent),
// false if it was dropped under PolicyDrop.
func (q *BatchQueue) Enqueue(event *model.RawEvent) bool {
	// Increment receive counter regardless of outcome.
	q.recorder.IncReceived()

	select {
	case q.ch <- event:
		return true
	default:
		// Channel full: apply policy.
		switch q.cfg.QueueFullPolicy {
		case string(PolicyBlock):
			// Block until a slot frees, the queue shuts down, or a 30s
			// timeout expires (prevents indefinite goroutine blocking).
			select {
			case q.ch <- event:
				return true
			case <-q.ctx.Done():
				q.drop(event)
				return false
			case <-time.After(30 * time.Second):
				q.drop(event)
				return false
			}
		case string(PolicySingle):
			// Synchronous best-effort single send.
			if err := q.forwarder.ForwardSingle(q.ctx, event); err != nil {
				q.recorder.IncForwardFailed(1)
				q.log.Error("forward: single send failed", "sourceIp", event.SourceIP, "fingerprint", metaString(event.Metadata, "fingerprint"), "err", err)
			} else {
				q.recorder.IncForwarded(1)
			}
			return true
		default: // PolicyDrop
			q.drop(event)
			return false
		}
	}
}

// drop logs a drop record (per requirement) and counts it.
func (q *BatchQueue) drop(event *model.RawEvent) {
	q.recorder.IncDropped(1)
	if q.cfg.DropLogEnabled {
		q.log.Error("forward: event dropped (queue full)",
			"sourceIp", event.SourceIP,
			"fingerprint", metaString(event.Metadata, "fingerprint"),
			"reason", "queue full",
			"queueDepth", q.QueueDepth(),
			"summary", event.RawEvent,
		)
	}
}

// worker drains the channel, batches events, and forwards them.
func (q *BatchQueue) worker() {
	defer q.wg.Done()
	for {
		event, ok := <-q.ch
		if !ok {
			return
		}
		batch := []model.RawEvent{*event}
		flushInterval := time.Duration(q.cfg.BatchFlushInterval) * time.Millisecond
		timer := time.NewTimer(flushInterval)
		flushed := false
		for !flushed && len(batch) < q.cfg.BatchSize {
			select {
			case e, ok := <-q.ch:
				if !ok {
					timer.Stop()
					q.flush(batch)
					return
				}
				batch = append(batch, *e)
			case <-timer.C:
				q.flush(batch)
				flushed = true
			}
		}
		if !flushed {
			timer.Stop()
			q.flush(batch)
		}
	}
}

// flush forwards a batch and records success/failure.
func (q *BatchQueue) flush(batch []model.RawEvent) {
	if len(batch) == 0 {
		return
	}
	if err := q.forwarder.ForwardBatch(q.ctx, batch); err != nil {
		q.recorder.IncForwardFailed(uint64(len(batch)))
		q.log.Error("forward: batch send failed", "count", len(batch), "err", err)
		return
	}
	q.recorder.IncForwarded(uint64(len(batch)))
}

// Close stops the queue, flushes any remaining buffered events, and waits for
// workers to finish. The channel is closed first so workers drain remaining
// events; cancel is called after workers exit to release any blocking selects.
func (q *BatchQueue) Close() {
	close(q.ch)
	q.wg.Wait()
	q.cancel()
}

// metaString extracts a string value from the metadata map for logging.
func metaString(meta map[string]interface{}, key string) string {
	if v, ok := meta[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
