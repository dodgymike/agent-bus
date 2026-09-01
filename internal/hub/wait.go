package hub

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/dodgymike/agent-bus/internal/store"
)

// Batch is one page of a read: history and long-poll return the same shape, on
// purpose. An agent that catches up with GET /v1/messages and then parks on GET
// /v1/wait uses one cursor and one parser for both.
type Batch struct {
	// Messages are in DELIVERY ORDER — ascending store.Message.Pos, the order
	// this bus committed them — and are already filtered to what the requesting
	// agent may see.
	//
	// That is NOT ascending sequence order, and the difference is deliberate
	// (SIGN-1-FU-REORDER-WATERMARK): a sequence is minted before the client
	// signs, so a message minted early and spent late carries a low sequence and
	// a high position. Serving in sequence order would put it below cursors that
	// have already passed it and lose it for those readers permanently.
	Messages []store.Message

	// Cursor is the position to resume from. On an EMPTY batch it is the
	// cursor that was passed in, unchanged — a long poll that times out hands
	// back exactly what it was given (POLL-1).
	Cursor uint64

	// More reports that the batch was cut short by the limit and another call
	// will return immediately.
	More bool

	// TimedOut reports that a long poll parked and reached its deadline
	// without a message. It is FALSE for history reads, which never park.
	// It is not an error: the response is a 200 with an empty batch.
	TimedOut bool
}

// waiter is one parked long-poll.
//
// There is deliberately NO goroutine behind it. Wait parks the CALLER's
// goroutine — the one net/http already dedicated to this request — on a select,
// so the only thing to release when a client vanishes is a map entry and a
// timer. A registry that spawned its own goroutine per waiter would be a second
// thing to leak, and POLL-3 asserts on exactly this.
type waiter struct {
	// agentID is the AUTHENTICATED principal this waiter belongs to. It is
	// what notify filters visibility against, so a wake can never be caused by
	// a message the waiter is not entitled to see.
	agentID string

	// after is the cursor the waiter parked at: a DELIVERY POSITION
	// (store.Message.Pos), never a sequence. notify compares it against the same
	// counter store.Since binary-searches, and the two must not drift apart — a
	// wake filter on one counter and a read on the other is precisely how a
	// parked poll ends up never woken for a message its own read would return.
	after uint64

	// enrolledAt is the waiter's enrolment epoch, pinned at registration. It is
	// carried on the waiter, not looked up in notify, so the wake filter and
	// the read filter can never disagree about the same request.
	enrolledAt time.Time

	// ch is buffered with capacity 1 and is written to with a NON-BLOCKING
	// send. Both matter:
	//
	//   - buffered, so a publisher never blocks on a waiter that has not
	//     reached its select yet (the publisher holds writeMu, so blocking
	//     there would stall the whole write path);
	//   - capacity 1 with a non-blocking send, so N messages arriving before
	//     the waiter runs COALESCE into one wake. The waiter then re-reads the
	//     store and gets all N in a single batch. That is what "wakes every
	//     eligible waiter exactly once, no duplicates, no misses" means in
	//     practice: the wake is an edge, the store is the truth.
	ch chan struct{}
}

// notify wakes every parked waiter that is entitled to see m and is positioned
// behind it.
//
// The caller must hold writeMu, and must call this ONLY after m is durable AND
// applied to the serving copy. That ordering is POLL-2's whole content: a
// waiter woken before the commit is durable can observe a message a crash then
// un-observes, which violates invariants 4 and 5.
//
// The visibility test is the same store.Message.VisibleTo the read path uses,
// so a waiter is never woken for a message its own next read would filter out —
// which is what keeps "one broadcast, one wake per eligible waiter" true rather
// than approximately true.
//
// # The position test is the same counter store.Since searches, and must stay so
//
// This compared m.Seq against the waiter's cursor until
// SIGN-1-FU-REORDER-WATERMARK. That was the second half of the suppression: a
// message minted early and spent late carries a LOW sequence, so every parked
// waiter at the head was skipped here AND the message sat below their cursor in
// Since — lost once for the wake and once for the read. Moving Since onto
// positions without moving this line would leave the message served on the next
// poll but the parked poll never woken, which is the same bug wearing a longer
// timeout.
func (h *Hub) notify(m store.Message) {
	h.waitMu.Lock()
	defer h.waitMu.Unlock()

	for w := range h.waiters {
		if m.Pos <= w.after || !m.VisibleTo(w.agentID, w.enrolledAt) {
			continue
		}
		select {
		case w.ch <- struct{}{}:
		default:
			// Already signalled and not yet drained. The waiter will re-read
			// the store and see this message too.
		}
	}
}

// WaiterCount reports how many long-polls are parked. For operators and for the
// concurrency tests, which assert it returns to zero.
func (h *Hub) WaiterCount() int {
	h.waitMu.Lock()
	defer h.waitMu.Unlock()
	return len(h.waiters)
}

// waiterParkedHook is a TEST-ONLY observation point, nil in production.
//
// # Why it has to exist in this file rather than in a test
//
// "The waiter is parked" cannot be observed from outside. WaiterCount() goes to
// one as soon as Wait registers in the map, but registration is NOT the park:
// Wait then does a SECOND store.Since read to close the registration race (see
// Wait's doc comment). A test that publishes as soon as the count rises lands
// inside that gap, so the message is returned by that READ — which consults
// Message.Pos whatever notify compares — and notify is never exercised at all.
//
// That is not a theoretical gap. It was measured on SIGN-1-FU-REORDER-WATERMARK:
// with the fix's `m.Pos <= w.after` mutated back to the defective
// `m.Seq <= w.after`, BOTH parked-poll tests still passed, because both were
// really testing the second read. Waking the parked poll IS this task's P0, so a
// proof that never reaches notify is not a proof.
//
// # Cost in production
//
// One atomic load and a nil check per PARKED poll — not per message, not per
// send, and not on the fast path, which returns before this point. The hook is
// never set outside tests.
//
// It is an atomic rather than a plain var because the reader is the request
// goroutine and the writer is a test goroutine, and this package's tests run
// with -race.
var waiterParkedHook atomic.Pointer[func()]

// SetWaiterParkedHook installs fn, which Wait calls once it has registered its
// waiter AND completed the registration-race read — the instant from which the
// ONLY way that call can still return a message is a notify wake. It returns a
// function that restores the previous hook.
//
// TEST-ONLY. Nothing in cmd/ or internal/httpapi calls it, and production leaves
// the hook nil. It is exported only because the tests that need it live in
// package hub_test; see waiterParkedHook for why an external observation point
// cannot do this job.
func SetWaiterParkedHook(fn func()) (restore func()) {
	prev := waiterParkedHook.Swap(&fn)
	return func() { waiterParkedHook.Store(prev) }
}

// clampLimit bounds a client-requested batch size.
func clampLimit(limit int) int {
	if limit <= 0 {
		return DefaultBatchLimit
	}
	if limit > MaxBatchLimit {
		return MaxBatchLimit
	}
	return limit
}

// History returns the messages visible to agentID after the given cursor
// position. It never parks.
//
// An agent that is not on this bus's roster gets ErrUnknownSender and NOT an
// empty batch. Failing closed matters here: the alternative is to read with a
// zero enrolment epoch, which disables the epoch filter (see
// store.Message.VisibleTo) — an empty roster would then serve everything to
// anyone rather than nothing to nobody.
func (h *Hub) History(agentID string, after uint64, limit int) (Batch, error) {
	epoch, ok := h.enrolmentEpoch(agentID)
	if !ok {
		return Batch{Cursor: after}, fmt.Errorf("%w: %q", ErrUnknownSender, agentID)
	}
	msgs, next, more := h.store.Since(agentID, epoch, after, clampLimit(limit))
	return Batch{Messages: msgs, Cursor: next, More: more}, nil
}

// Wait returns the messages visible to agentID after the given cursor position,
// PARKING until one arrives, the timeout elapses, or ctx is done.
//
// # Timeout is not an error
//
// Reaching the deadline returns a Batch with no messages, TimedOut set, and the
// SAME cursor, and a nil error. The HTTP layer answers 200. A long poll that
// found nothing is the normal, expected outcome of a quiet bus; making it a
// non-2xx would force every client to treat its steady state as a failure.
//
// # ctx is an error
//
// A cancelled context means the client hung up or the server is shutting down.
// That returns ctx.Err(), and the HTTP layer writes nothing — there is nobody
// to write to. It is distinct from a timeout precisely so the two are not
// confused in a log.
//
// # The registration race, and why the second read closes it
//
// Between the first store read and registering in the waiter map, a publisher
// could commit a message: it would not see this waiter, and the waiter would
// then park on a message that already exists and sleep until its deadline. So
// the store is read AGAIN after registration. Publishing is ordered
// append-then-notify, so for any message either the append precedes this second
// read (the read sees it) or the notify follows this registration (the wake
// finds it). There is no third case, and no lock is held across the store read.
func (h *Hub) Wait(ctx context.Context, agentID string, after uint64, limit int, timeout time.Duration) (Batch, error) {
	limit = clampLimit(limit)
	if timeout <= 0 {
		timeout = h.pollTimeout
	}
	if timeout > MaxPollTimeout {
		timeout = MaxPollTimeout
	}

	epoch, ok := h.enrolmentEpoch(agentID)
	if !ok {
		// Fails closed, for the reason spelled out on History.
		return Batch{Cursor: after}, fmt.Errorf("%w: %q", ErrUnknownSender, agentID)
	}

	// Fast path: never park when there is already something to return.
	if msgs, next, more := h.store.Since(agentID, epoch, after, limit); len(msgs) > 0 {
		return Batch{Messages: msgs, Cursor: next, More: more}, nil
	}
	// Cheap pre-check for a context that is already done, so a cancelled
	// request does not register a waiter it will immediately drop.
	if err := ctx.Err(); err != nil {
		return Batch{Cursor: after}, err
	}

	w := &waiter{agentID: agentID, after: after, enrolledAt: epoch, ch: make(chan struct{}, 1)}

	h.waitMu.Lock()
	// SINGLE-ACTIVE per agent id (MaxWaitersPerAgent == 1). The FIRST poll for
	// an agent holds the delivery slot; a SECOND concurrent poll for the SAME
	// authenticated principal is REFUSED here, before it registers.
	//
	// Why refuse rather than park a second waiter: notify SPLITS a message among
	// the waiters entitled to see it, so two parked polls on one identity divide
	// that agent's inbox non-deterministically — a DM meant for an interactive
	// session can be delivered to a background monitor polling the same id and
	// never reach the session. Refusing the second poll keeps delivery whole.
	//
	// Keyed on the agent id, which is safe HERE for exactly the reason
	// auth.MaxActiveSessionsPerAgent is safe and the pending-session cap was
	// not: this is an AUTHENTICATED route, so the key is a PROVEN, fully-
	// qualified identity (invariants 2, 3) and one identity can only refuse
	// itself. A same-name agent on a DIFFERENT bus has a different qualified id
	// and its own slot. It fails closed and EVICTS NOTHING — evicting the
	// holding poll would let one connection of an agent kill another's.
	//
	// This is a REFUSE-AND-RESPOND, not a disconnect: the caller is a buggy
	// client running two pollers on one identity, which invariant 10 says must
	// not be dropped. ErrPollActive is a DISTINCT sentinel from ErrCapacity so
	// the HTTP layer can answer 409 (a clean, non-retryable refusal) instead of
	// the 503 + Retry-After a capacity limit gets, which would loop the refused
	// poller forever.
	if h.waitersByAgent[agentID] >= MaxWaitersPerAgent {
		h.waitMu.Unlock()
		return Batch{Cursor: after}, fmt.Errorf("%w: agent %q already has an active long poll; only one may be active at a time", ErrPollActive, agentID)
	}
	h.waiters[w] = struct{}{}
	h.waitersByAgent[agentID]++
	h.waitMu.Unlock()

	// Deregistration is deferred so it runs on EVERY exit — timeout, wake,
	// cancel, and any panic in between. This, and the absence of a per-waiter
	// goroutine, is the whole of "a client that vanishes mid-wait releases
	// promptly" (POLL-3).
	defer func() {
		h.waitMu.Lock()
		delete(h.waiters, w)
		// The per-agent counter is decremented in the SAME critical section as
		// the map delete, and the entry is removed at zero so the map does not
		// accumulate one entry per agent that ever polled.
		if n := h.waitersByAgent[agentID] - 1; n > 0 {
			h.waitersByAgent[agentID] = n
		} else {
			delete(h.waitersByAgent, agentID)
		}
		h.waitMu.Unlock()
	}()

	// See the doc comment: this second read is what closes the registration
	// race.
	if msgs, next, more := h.store.Since(agentID, epoch, after, limit); len(msgs) > 0 {
		return Batch{Messages: msgs, Cursor: next, More: more}, nil
	}

	// TEST-ONLY, nil in production: the waiter is registered and the
	// registration-race read is behind us, so from here notify is the only thing
	// that can still hand this call a message. See waiterParkedHook.
	//
	// It is safe for the wake to arrive before the select below is reached: w.ch
	// is buffered with capacity 1 and notify's send is non-blocking, so the edge
	// is retained rather than missed.
	if hook := waiterParkedHook.Load(); hook != nil && *hook != nil {
		(*hook)()
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return Batch{Cursor: after}, ctx.Err()

	case <-timer.C:
		// 200 with an empty batch and the cursor the caller gave us.
		return Batch{Cursor: after, TimedOut: true}, nil

	case <-w.ch:
		// The wake is an edge; the store is the truth. Re-read rather than
		// carrying a message on the channel, so a burst that coalesced into one
		// signal still comes back as one complete batch.
		msgs, next, more := h.store.Since(agentID, epoch, after, limit)
		return Batch{Messages: msgs, Cursor: next, More: more}, nil
	}
}
