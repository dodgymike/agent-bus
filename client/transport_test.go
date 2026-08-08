package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestSend404UnknownRecipientIsRejectedNotVersionSkew pins security gate
// finding F1 on 52930611: a 404 on /v1/send is a GENUINE per-resource refusal
// (hub.ErrUnknownRecipient, "unknown recipient" — CONTRACTS-HTTP.md), not a
// missing route, and must stay KindRejected/exit 7. Before this fix EVERY 404
// — including this one — was reclassified KindVersionSkew/exit 9, with a
// remedy that told the caller to point --bus at a DIFFERENT bus: actively
// wrong advice for an addressing mistake, and a nudge toward pinned-
// fingerprint churn under invariant 11. TestHTTPStatusMapsToKindAndExitCode
// (errors_test.go) proves the OTHER side of this fix (an unrelated route's
// 404 IS version skew); this test proves send's specific carve-out.
func TestSend404UnknownRecipientIsRejectedNotVersionSkew(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case routeSend:
			stubWriteJSON(w, http.StatusNotFound, map[string]string{"error": "unknown recipient"})
		default:
			t.Errorf("stub bus: unexpected route %s", r.URL.Path)
		}
	})
	c := bus.client(t, nil)

	_, err := c.Send(context.Background(), SendOptions{
		To:   "bus-x.nobody-1",
		Body: []byte("hello"),
	})
	if err == nil {
		t.Fatalf("Send to an unknown recipient = nil error, want a 404")
	}
	if got := KindOf(err); got != KindRejected {
		t.Fatalf("KindOf(err) = %q, want %q — an unknown recipient is a resource-level refusal, not a missing route", got, KindRejected)
	}
	if got := ExitCode(err); got != ExitRejected {
		t.Fatalf("ExitCode(err) = %d, want %d", got, ExitRejected)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not a *client.Error: %v", err)
	}
	if strings.Contains(e.Remedy, "upgrade the bus") || strings.Contains(e.Remedy, "point --bus at one built") {
		t.Fatalf("Remedy = %q wrongly advises a bus upgrade / a DIFFERENT bus for what is actually an addressing mistake", e.Remedy)
	}
}

// TestVersionSkewOnAgentsRouteIsDistinguishedFromRejected is 52930611's core
// positive case, driven end to end through a real Client (not a hand-built
// *Error): a 404 on a route this client depends on but the bus does not
// serve — the field-observed shape was an old bus with no /v1/mint route,
// answering every OTHER call normally and only failing send — is
// KindVersionSkew/ExitVersionSkew, named by route, with a remedy that says
// "upgrade the bus" rather than the old exit-7 "the bus understood and
// refused it" wording.
//
// It exercises routeAgents rather than routeMint: newStubBus (stubbus_test.go)
// serves /v1/mint itself as part of the authenticated handshake fixture and
// does not let a test override that response, while /v1/agents is routed
// straight to the test's own handler. Both are fixed, static, authenticated
// paths with no per-resource lookup behind them — the property this
// classification actually depends on, not which specific route is used — so
// the substitution proves the same thing. TestSend404UnknownRecipientIs-
// RejectedNotVersionSkew (above) proves the one route that is DIFFERENT
// (routeSend, which has a genuine per-resource 404) is correctly carved out
// of this classification.
func TestVersionSkewOnAgentsRouteIsDistinguishedFromRejected(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case routeAgents:
			stubWriteJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		default:
			t.Errorf("stub bus: unexpected route %s", r.URL.Path)
		}
	})
	c := bus.client(t, nil)

	_, err := c.Agents(context.Background())
	if err == nil {
		t.Fatalf("Agents against a 404 = nil error, want one")
	}
	if got := KindOf(err); got != KindVersionSkew {
		t.Fatalf("KindOf(err) = %q, want %q — a 404 on a fixed route this client depends on means the bus does not know it, not that it refused the request", got, KindVersionSkew)
	}
	if got := ExitCode(err); got != ExitVersionSkew {
		t.Fatalf("ExitCode(err) = %d, want %d", got, ExitVersionSkew)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not a *client.Error: %v", err)
	}
	if !strings.Contains(e.Remedy, "upgrade the bus") {
		t.Fatalf("Remedy = %q, want it to name the actual fix: upgrade the bus", e.Remedy)
	}
	if !strings.Contains(e.Message, routeAgents) {
		t.Fatalf("Message = %q, want it to name the missing route %q", e.Message, routeAgents)
	}
}
