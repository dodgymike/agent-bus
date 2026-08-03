package client

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests in this file cover CLI-3 (watch) at the CLIENT-PACKAGE level: the
// long-poll loop, and above all the CURSOR contract, which is where a mistake
// silently loses messages.

// fastRetry is a retry policy whose every delay is a millisecond, so a test
// that must exercise the backoff path does not have to wait for it. It does not
// change WHICH failures are retried — only how long the loop pauses.
var fastRetry = RetryPolicy{Attempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}

// TestCLIWatchTimeoutIsNotAnError checks a `timed_out: true` 200 keeps the
// watch running.
//
// On a quiet bus this is the STEADY STATE, not an anomaly: the bus answers a
// poll that found nothing with 200, an empty list and the cursor unchanged. A
// watch that treated it as a failure would log an error every 30 seconds for a
// bus that is working perfectly.
func TestCLIWatchTimeoutIsNotAnError(t *testing.T) {
	var polls int32
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&polls, 1)
		if n <= 2 {
			// Cursor echoed back UNCHANGED, exactly as the bus does.
			stubWriteJSON(w, http.StatusOK, Batch{
				Messages: []Message{},
				Cursor:   r.URL.Query().Get("cursor"),
				TimedOut: true,
			})
			return
		}
		stubWriteJSON(w, http.StatusOK, Batch{
			Messages: []Message{stubMessage(1, "bus-x.other-1", "finally")},
			Cursor:   "cursor-1",
		})
	})
	c := bus.client(t, nil)

	var got []Message
	stats, err := c.Watch(context.Background(), WatchOptions{
		PollTimeout: time.Second,
		Max:         1,
		Persist:     true,
	}, func(m Message) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatalf("Watch across timed-out polls = %v, want nil — a timed-out poll is a 200 and the steady state", err)
	}
	if stats.Delivered != 1 || len(got) != 1 {
		t.Fatalf("Delivered = %d (handler saw %d), want 1", stats.Delivered, len(got))
	}
	if stats.Polls < 3 {
		t.Fatalf("Polls = %d, want at least 3 — the two timed-out polls must be counted, not treated as failures", stats.Polls)
	}
	if stats.Cursor != "cursor-1" {
		t.Fatalf("Cursor = %q, want %q", stats.Cursor, "cursor-1")
	}
}

// TestCLIWatchDoesNotAdvanceCursorWhenHandlerFails is the crash-semantics test,
// and the most important one in this file.
//
// The contract is: hand EVERY message in a batch to the handler, and only if
// every one returned nil adopt the batch's cursor. A handler that fails
// part-way therefore leaves the cursor exactly where it was, and the WHOLE
// batch — including the messages the handler had already accepted — is
// delivered again next time.
//
// That direction is the only safe one. Delivery is AT-LEAST-ONCE; advancing the
// cursor before the caller has the messages would convert it into at-most-once
// and silently DROP messages on any crash. Re-deliver, never skip.
func TestCLIWatchDoesNotAdvanceCursorWhenHandlerFails(t *testing.T) {
	batch := []Message{
		stubMessage(1, "bus-x.other-1", "one"),
		stubMessage(2, "bus-x.other-1", "two"),
		stubMessage(3, "bus-x.other-1", "three"),
	}
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			stubWriteJSON(w, http.StatusOK, Batch{Messages: batch, Cursor: "cursor-3"})
			return
		}
		// Anything past the batch is quiet.
		stubWriteJSON(w, http.StatusOK, Batch{
			Messages: []Message{},
			Cursor:   r.URL.Query().Get("cursor"),
			TimedOut: true,
		})
	})
	c := bus.client(t, nil)

	base, err := c.resolveBusURL()
	if err != nil {
		t.Fatalf("resolveBusURL: %v", err)
	}
	busURL := base.String()

	before, err := c.Store().Cursor(bus.AgentID, busURL)
	if err != nil {
		t.Fatalf("Cursor before: %v", err)
	}
	if before != "" {
		t.Fatalf("stored cursor before the first watch = %q, want empty", before)
	}

	boom := errors.New("handler refused message 3")
	var firstRun []string
	_, werr := c.Watch(context.Background(), WatchOptions{
		PollTimeout: time.Second,
		Persist:     true,
	}, func(m Message) error {
		firstRun = append(firstRun, m.MessageID)
		if m.Seq == 3 {
			return boom
		}
		return nil
	})
	if !errors.Is(werr, boom) {
		t.Fatalf("Watch = %v, want the handler's own error returned verbatim", werr)
	}
	if len(firstRun) != 3 {
		t.Fatalf("the handler saw %v, want all three messages up to and including the failing one", firstRun)
	}

	after, err := c.Store().Cursor(bus.AgentID, busURL)
	if err != nil {
		t.Fatalf("Cursor after: %v", err)
	}
	if after != "" {
		t.Fatalf("stored cursor after a failed handler = %q, want it UNCHANGED (%q) — the batch was not fully accepted", after, before)
	}

	// A second watch, on the same identity and store, must be handed the WHOLE
	// batch again — including messages 1 and 2, which the first handler
	// accepted. Re-deliver, never skip.
	c2 := bus.client(t, nil)
	var secondRun []string
	stats, err := c2.Watch(context.Background(), WatchOptions{
		PollTimeout: time.Second,
		Persist:     true,
		Max:         3,
	}, func(m Message) error {
		secondRun = append(secondRun, m.MessageID)
		return nil
	})
	if err != nil {
		t.Fatalf("second Watch: %v", err)
	}
	if len(secondRun) != 3 {
		t.Fatalf("the second watch saw %v, want all three messages RE-DELIVERED", secondRun)
	}
	for i := range secondRun {
		if secondRun[i] != firstRun[i] {
			t.Fatalf("second watch message %d = %q, want %q — the whole batch is re-delivered, not the tail of it", i, secondRun[i], firstRun[i])
		}
	}
	if stats.Cursor != "cursor-3" {
		t.Fatalf("second watch Cursor = %q, want %q once the whole batch was accepted", stats.Cursor, "cursor-3")
	}
}

// TestCLIWatchPersistsCursorAcrossRuns checks a watch that stops cleanly
// records where it got to, and that a FRESH Client on the same identity
// directory resumes AFTER it rather than replaying.
func TestCLIWatchPersistsCursorAcrossRuns(t *testing.T) {
	first := []Message{
		stubMessage(1, "bus-x.other-1", "one"),
		stubMessage(2, "bus-x.other-1", "two"),
	}
	third := stubMessage(3, "bus-x.other-1", "three")

	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			stubWriteJSON(w, http.StatusOK, Batch{Messages: first, Cursor: "cursor-2"})
		case "cursor-2":
			stubWriteJSON(w, http.StatusOK, Batch{Messages: []Message{third}, Cursor: "cursor-3"})
		default:
			stubWriteJSON(w, http.StatusOK, Batch{
				Messages: []Message{},
				Cursor:   r.URL.Query().Get("cursor"),
				TimedOut: true,
			})
		}
	})

	c1 := bus.client(t, nil)
	var run1 []uint64
	stats1, err := c1.Watch(context.Background(), WatchOptions{
		PollTimeout: time.Second,
		Persist:     true,
		Max:         2,
	}, func(m Message) error {
		run1 = append(run1, m.Seq)
		return nil
	})
	if err != nil {
		t.Fatalf("first Watch: %v", err)
	}
	if len(run1) != 2 || run1[0] != 1 || run1[1] != 2 {
		t.Fatalf("first watch delivered %v, want [1 2]", run1)
	}
	if stats1.Cursor != "cursor-2" {
		t.Fatalf("first watch Cursor = %q, want %q", stats1.Cursor, "cursor-2")
	}

	// A FRESH Client — nothing carried over in memory; the resume must come off
	// disk.
	c2 := bus.client(t, nil)
	var run2 []uint64
	stats2, err := c2.Watch(context.Background(), WatchOptions{
		PollTimeout: time.Second,
		Persist:     true,
		Max:         1,
	}, func(m Message) error {
		run2 = append(run2, m.Seq)
		return nil
	})
	if err != nil {
		t.Fatalf("second Watch: %v", err)
	}
	if len(run2) != 1 || run2[0] != 3 {
		t.Fatalf("second watch delivered %v, want [3] — it must resume AFTER the persisted cursor, not replay", run2)
	}
	if stats2.Cursor != "cursor-3" {
		t.Fatalf("second watch Cursor = %q, want %q", stats2.Cursor, "cursor-3")
	}

	// The resume must have gone out on the wire, not merely been computed.
	var sawResume bool
	for _, call := range bus.calls(routeWait) {
		if call.Query.Get("cursor") == "cursor-2" {
			sawResume = true
		}
	}
	if !sawResume {
		t.Fatalf("no poll carried cursor=%q; the second run did not resume from the persisted position", "cursor-2")
	}
}

// TestCLIWatchStopsOnFatalUnavailable checks a 503 with NO Retry-After stops
// the watch immediately and is NOT retried.
//
// That 503 is the bus saying its write path is not durable (hub.ErrNotDurable /
// hub.ErrPoisoned) — the header's ABSENCE is the signal, because every capacity
// refusal carries it. Backing off forever on it would convert an
// operator-visible fault into a silent one.
func TestCLIWatchStopsOnFatalUnavailable(t *testing.T) {
	var polls int32
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&polls, 1)
		// NO Retry-After, deliberately.
		stubWriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the hub cannot durably accept messages"})
	})
	// The DEFAULT retry policy on purpose: the point is that the fatal bit
	// defeats the retry loop, not that the loop was configured away.
	c := bus.client(t, nil)

	stats, err := c.Watch(context.Background(), WatchOptions{PollTimeout: time.Second}, func(Message) error {
		t.Errorf("handler called, but the bus never delivered anything")
		return nil
	})
	if err == nil {
		t.Fatalf("Watch against a fatally unavailable bus = nil error, want one")
	}
	if !IsFatalUnavailable(err) {
		t.Fatalf("IsFatalUnavailable(err) = false, want true for a 503 with no Retry-After: %v", err)
	}
	if KindOf(err) != KindServer {
		t.Fatalf("KindOf(err) = %q, want %q (fatal is one extra bit, not a new Kind)", KindOf(err), KindServer)
	}
	if stats.Delivered != 0 {
		t.Fatalf("Delivered = %d, want 0", stats.Delivered)
	}
	if got := atomic.LoadInt32(&polls); got != 1 {
		t.Fatalf("the bus saw %d polls, want exactly 1 — a fatal 503 must not be retried at any layer", got)
	}
}

// TestCLIWatchRetriesTransientFailure is the CONTRAST to the test above: a 503
// that DOES carry Retry-After is a live capacity bound, transient by
// construction. It is backed off, reported through OnRetry, and the watch
// continues.
func TestCLIWatchRetriesTransientFailure(t *testing.T) {
	var polls int32
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&polls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			stubWriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "too many waiters for this agent"})
			return
		}
		stubWriteJSON(w, http.StatusOK, Batch{
			Messages: []Message{stubMessage(1, "bus-x.other-1", "after the blip")},
			Cursor:   "cursor-1",
		})
	})
	// Attempts:1 so the transport does not absorb the failure itself and the
	// WATCH loop is the thing under test; millisecond delays so the backoff
	// (which would otherwise honour the 1s hint) does not slow the suite.
	c := bus.client(t, func(cfg *Config) { cfg.Retry = fastRetry })

	var (
		mu      sync.Mutex
		retries []error
		delays  []time.Duration
	)
	stats, err := c.Watch(context.Background(), WatchOptions{
		PollTimeout: time.Second,
		Max:         1,
		OnRetry: func(err error, d time.Duration) {
			mu.Lock()
			retries = append(retries, err)
			delays = append(delays, d)
			mu.Unlock()
		},
	}, func(Message) error { return nil })
	if err != nil {
		t.Fatalf("Watch across a transient 503 = %v, want nil — a capacity refusal is retried, not fatal", err)
	}
	if stats.Delivered != 1 {
		t.Fatalf("Delivered = %d, want 1", stats.Delivered)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(retries) != 1 {
		t.Fatalf("OnRetry fired %d times, want 1 — a transient failure must be reported so an outage is not silent", len(retries))
	}
	if IsFatalUnavailable(retries[0]) {
		t.Fatalf("the retried error reports IsFatalUnavailable: a 503 WITH Retry-After is transient")
	}
	if KindOf(retries[0]) != KindServer {
		t.Fatalf("retried error Kind = %q, want %q", KindOf(retries[0]), KindServer)
	}
	if delays[0] < 0 {
		t.Fatalf("OnRetry delay = %s, want a non-negative backoff", delays[0])
	}
}

// TestCLIWatchStopsCleanlyOnContextCancel checks that cancelling the context —
// what Ctrl-C does — returns a NIL error and the stats gathered so far. An
// interrupted tail is a FINISHED tail, not a failure.
func TestCLIWatchStopsCleanlyOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var polls int32
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&polls, 1) == 1 {
			stubWriteJSON(w, http.StatusOK, Batch{
				Messages: []Message{stubMessage(1, "bus-x.other-1", "one")},
				Cursor:   "cursor-1",
			})
			return
		}
		// Park until the client hangs up, which is what a real long poll does.
		<-r.Context().Done()
	})
	c := bus.client(t, func(cfg *Config) { cfg.Retry = fastRetry })

	stats, err := c.Watch(ctx, WatchOptions{PollTimeout: 5 * time.Second}, func(m Message) error {
		// Cancel from a timer rather than inline, so the cancellation lands
		// while a poll is genuinely IN FLIGHT rather than between iterations.
		time.AfterFunc(50*time.Millisecond, cancel)
		return nil
	})
	if err != nil {
		t.Fatalf("Watch stopped by ctx cancellation = %v, want nil — a Ctrl-C is the successful end of a tail", err)
	}
	if stats.Delivered != 1 {
		t.Fatalf("Delivered = %d, want 1 — the stats gathered before the cancellation must survive it", stats.Delivered)
	}
	if stats.Cursor != "cursor-1" {
		t.Fatalf("Cursor = %q, want %q", stats.Cursor, "cursor-1")
	}
}
