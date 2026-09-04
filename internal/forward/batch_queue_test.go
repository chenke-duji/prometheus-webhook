package forward

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"prometheus-webhook/internal/model"
)

// recordingForwarder records all batch/single calls for assertion.
type recordingForwarder struct {
	mu        sync.Mutex
	batches   [][]model.RawEvent
	singles   []*model.RawEvent
	batchErr  error
	singleErr error
}

func (f *recordingForwarder) ForwardBatch(_ context.Context, events []model.RawEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, events)
	return f.batchErr
}

func (f *recordingForwarder) ForwardSingle(_ context.Context, event *model.RawEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.singles = append(f.singles, event)
	return f.singleErr
}

func (f *recordingForwarder) Close() error { return nil }

func (f *recordingForwarder) totalForwarded() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.batches {
		n += len(b)
	}
	return n
}

func (f *recordingForwarder) singleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.singles)
}

// testRec implements Recorder for assertion in tests.
type testRec struct {
	received  atomic.Uint64
	forwarded atomic.Uint64
	failed    atomic.Uint64
	dropped   atomic.Uint64
}

func (r *testRec) IncReceived()              { r.received.Add(1) }
func (r *testRec) IncForwarded(n uint64)     { r.forwarded.Add(n) }
func (r *testRec) IncForwardFailed(n uint64) { r.failed.Add(n) }
func (r *testRec) IncDropped(n uint64)       { r.dropped.Add(n) }

func testEvent(s string) *model.RawEvent {
	return &model.RawEvent{Source: "alertmanager", RawEvent: s}
}

func TestEnqueue_AcceptsEvent(t *testing.T) {
	fwd := &recordingForwarder{}
	cfg := Config{BatchSize: 10, BatchFlushInterval: 100, Workers: 0, QueueCapacity: 10, QueueFullPolicy: "drop"}
	q := NewBatchQueue(cfg, fwd, nil, slog.Default())
	if !q.Enqueue(testEvent("e1")) {
		t.Error("enqueue should succeed on non-full queue")
	}
}

func TestEnqueue_Drop_WhenFull(t *testing.T) {
	fwd := &recordingForwarder{}
	rec := &testRec{}
	cfg := Config{BatchSize: 10, BatchFlushInterval: 100, Workers: 0, QueueCapacity: 2, QueueFullPolicy: "drop"}
	q := NewBatchQueue(cfg, fwd, rec, slog.Default())

	q.Enqueue(testEvent("e1"))
	q.Enqueue(testEvent("e2"))
	if q.Enqueue(testEvent("e3")) {
		t.Error("enqueue on full queue with drop policy should return false")
	}
	if rec.dropped.Load() != 1 {
		t.Errorf("expected 1 dropped, got %d", rec.dropped.Load())
	}
	if rec.received.Load() != 3 {
		t.Errorf("expected 3 received, got %d", rec.received.Load())
	}
}

func TestEnqueue_Single_CallsForwardSingle(t *testing.T) {
	fwd := &recordingForwarder{}
	cfg := Config{BatchSize: 10, BatchFlushInterval: 100, Workers: 0, QueueCapacity: 1, QueueFullPolicy: "single"}
	q := NewBatchQueue(cfg, fwd, nil, slog.Default())

	// First enqueue fills the 1-capacity channel (Workers=0, no draining).
	q.Enqueue(testEvent("e1"))
	// Second enqueue triggers PolicySingle (channel is full).
	if !q.Enqueue(testEvent("e2")) {
		t.Error("single policy should return true")
	}
	if fwd.singleCount() != 1 {
		t.Errorf("expected 1 single send, got %d", fwd.singleCount())
	}
}

func TestEnqueue_RecordsReceived(t *testing.T) {
	fwd := &recordingForwarder{}
	rec := &testRec{}
	cfg := Config{BatchSize: 10, BatchFlushInterval: 100, Workers: 0, QueueCapacity: 10, QueueFullPolicy: "drop"}
	q := NewBatchQueue(cfg, fwd, rec, slog.Default())

	q.Enqueue(testEvent("e1"))
	q.Enqueue(testEvent("e2"))
	q.Enqueue(testEvent("e3"))
	if rec.received.Load() != 3 {
		t.Errorf("expected 3 received, got %d", rec.received.Load())
	}
}

func TestWorker_BatchForwarding(t *testing.T) {
	fwd := &recordingForwarder{}
	cfg := Config{BatchSize: 3, BatchFlushInterval: 500, Workers: 1, QueueCapacity: 100, QueueFullPolicy: "drop"}
	q := NewBatchQueue(cfg, fwd, nil, slog.Default())
	q.Start()
	defer q.Close()

	for i := 0; i < 3; i++ {
		q.Enqueue(testEvent("e"))
	}

	// Wait for worker to process the batch.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout: forwarded %d events", fwd.totalForwarded())
		default:
		}
		if fwd.totalForwarded() >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorker_FlushOnTimer(t *testing.T) {
	fwd := &recordingForwarder{}
	cfg := Config{BatchSize: 100, BatchFlushInterval: 50, Workers: 1, QueueCapacity: 100, QueueFullPolicy: "drop"}
	q := NewBatchQueue(cfg, fwd, nil, slog.Default())
	q.Start()
	defer q.Close()

	q.Enqueue(testEvent("e1"))

	// With batchSize=100 and only 1 event, the timer should flush it.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout: forwarded %d events", fwd.totalForwarded())
		default:
		}
		if fwd.totalForwarded() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestClose_DrainsRemaining(t *testing.T) {
	fwd := &recordingForwarder{}
	cfg := Config{BatchSize: 100, BatchFlushInterval: 10000, Workers: 1, QueueCapacity: 100, QueueFullPolicy: "drop"}
	q := NewBatchQueue(cfg, fwd, nil, slog.Default())
	q.Start()

	for i := 0; i < 5; i++ {
		q.Enqueue(testEvent("e"))
	}

	// Close should drain remaining buffered events.
	q.Close()

	if fwd.totalForwarded() != 5 {
		t.Errorf("expected 5 forwarded after close, got %d", fwd.totalForwarded())
	}
}
