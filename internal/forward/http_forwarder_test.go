package forward

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"prometheus-webhook/internal/model"
)

func TestForwardBatch_Success(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	fwd, err := NewHTTPForwarder(&HTTPConfig{
		BaseURL:   srv.URL,
		BatchPath: "/api/v1/events/batch",
		Timeout:   2000,
		RetryMax:  0,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewHTTPForwarder: %v", err)
	}
	defer fwd.Close()

	events := []model.RawEvent{{Source: "alertmanager", RawEvent: "test"}}
	if err := fwd.ForwardBatch(context.Background(), events); err != nil {
		t.Fatalf("ForwardBatch: %v", err)
	}
	if gotPath != "/api/v1/events/batch" {
		t.Errorf("path: got %q, want /api/v1/events/batch", gotPath)
	}
	if len(gotBody) == 0 {
		t.Error("expected body to be received")
	}
}

func TestForwardBatch_RetryOn500(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	fwd, err := NewHTTPForwarder(&HTTPConfig{
		BaseURL:   srv.URL,
		BatchPath: "/batch",
		Timeout:   2000,
		RetryMax:  2,
		RetryBase: 10,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewHTTPForwarder: %v", err)
	}
	defer fwd.Close()

	err = fwd.ForwardBatch(context.Background(), []model.RawEvent{{Source: "test"}})
	if err == nil {
		t.Fatal("expected error after retries")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("expected 3 attempts (1+2 retries), got %d", got)
	}
}

func TestForwardBatch_NoRetryOn400(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(400)
	}))
	defer srv.Close()

	fwd, err := NewHTTPForwarder(&HTTPConfig{
		BaseURL:   srv.URL,
		BatchPath: "/batch",
		Timeout:   2000,
		RetryMax:  3,
		RetryBase: 10,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewHTTPForwarder: %v", err)
	}
	defer fwd.Close()

	err = fwd.ForwardBatch(context.Background(), []model.RawEvent{{Source: "test"}})
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected 1 attempt (no retry on 4xx), got %d", got)
	}
}

func TestForwardBatch_RetryOn429(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(429)
	}))
	defer srv.Close()

	fwd, err := NewHTTPForwarder(&HTTPConfig{
		BaseURL:   srv.URL,
		BatchPath: "/batch",
		Timeout:   2000,
		RetryMax:  2,
		RetryBase: 10,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewHTTPForwarder: %v", err)
	}
	defer fwd.Close()

	_ = fwd.ForwardBatch(context.Background(), []model.RawEvent{{Source: "test"}})
	if got := attempts.Load(); got != 3 {
		t.Errorf("expected 3 attempts (429 is retriable), got %d", got)
	}
}

func TestForwardBatch_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	fwd, err := NewHTTPForwarder(&HTTPConfig{
		BaseURL:   srv.URL,
		BatchPath: "/batch",
		Timeout:   2000,
		RetryMax:  5,
		RetryBase: 50,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewHTTPForwarder: %v", err)
	}
	defer fwd.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err = fwd.ForwardBatch(ctx, []model.RawEvent{{Source: "test"}})
	if err == nil {
		t.Fatal("expected error with canceled context")
	}
}

func TestForwardSingle_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	fwd, err := NewHTTPForwarder(&HTTPConfig{
		BaseURL:    srv.URL,
		SinglePath: "/api/v1/events",
		Timeout:    2000,
		RetryMax:   0,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewHTTPForwarder: %v", err)
	}
	defer fwd.Close()

	ev := &model.RawEvent{Source: "alertmanager", RawEvent: "single"}
	if err := fwd.ForwardSingle(context.Background(), ev); err != nil {
		t.Fatalf("ForwardSingle: %v", err)
	}
}

func TestIsRetriableStatus(t *testing.T) {
	cases := []struct {
		code   int
		expect bool
	}{
		{200, false},
		{301, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
	}
	for _, tc := range cases {
		if got := isRetriableStatus(tc.code); got != tc.expect {
			t.Errorf("isRetriableStatus(%d) = %v, want %v", tc.code, got, tc.expect)
		}
	}
}
