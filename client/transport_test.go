package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestRetrySucceedsAfterTransientFailures checks the client actually retries a
// retryable request (a 503) and eventually succeeds, rather than giving up
// after one attempt or looping forever.
func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			// Retry-After is what makes a 503 a CAPACITY refusal rather than
			// the bus reporting it cannot durably accept writes at all; the
			// real bus sends it on every capacity 503 (CONTRACTS-HTTP.md), and
			// without it statusError classifies this as fatal and correctly
			// refuses to retry. See the 503 split in statusError.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "busy"})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID:    "bus-x.a-1",
			BusID:      "bus-x",
			Name:       "a",
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	c, err := New(Config{
		BusURL:      srv.URL,
		IdentityDir: t.TempDir(),
		Retry:       RetryPolicy{Attempts: 5, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := c.Enrol(context.Background(), EnrolOptions{Name: "a", Save: true})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if res.AgentID == "" {
		t.Fatalf("Enrol succeeded with no agent id")
	}
	if got := atomic.LoadInt32(&count); got != 3 {
		t.Fatalf("server saw %d requests, want exactly 3 (two failures then a success)", got)
	}
}

// TestRetryDoesNotRetryBadRequest checks a 400 — which the bus will answer
// identically forever — is tried exactly once, not burned through the retry
// budget.
func TestRetryDoesNotRetryBadRequest(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid agent name"})
	}))
	defer srv.Close()

	c, err := New(Config{
		BusURL:      srv.URL,
		IdentityDir: t.TempDir(),
		Retry:       RetryPolicy{Attempts: 5, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Enrol(context.Background(), EnrolOptions{Name: "a", Save: true})
	if err == nil {
		t.Fatalf("Enrol against a 400 response = nil error, want one")
	}
	if got := KindOf(err); got != KindRejected {
		t.Fatalf("KindOf(err) = %q, want %q", got, KindRejected)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("server saw %d requests, want exactly 1 (a 400 must not be retried)", got)
	}
}
