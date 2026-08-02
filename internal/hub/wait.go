package hub

import (
	"context"
	"fmt"
	"time"

	"github.com/dodgymike/agent-bus/internal/store"
)

// Batch is one page of a read: history and long-poll return the same shape, on
// purpose. An agent that catches up with GET /v1/messages and then parks on GET
// /v1/wait uses one cursor and one parser for both.
type Batch struct {
	// Messages are in ascending sequence order and are already filtered to
	// what the requesting agent may see.
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

	// after is the cursor position the waiter parked at.
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
func (h *Hub) notify(m store.Message) {
	h.waitMu.Lock()
	defer h.waitMu.Unlock()

	for w := range h.waiters {
		if m.Seq <= w.after || !m.VisibleTo(w.agentID, w.enrolledAt) {
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
	// The PER-AGENT PARK CAP. It is not primarily about memory — a waiter is a
	// few words and no goroutine of its own — it is about notify: that loop
	// runs under writeMu, on the critical path of every send, and is O(parked
	// waiters). Without a cap, one agent parking thousands of polls slows down
	// every OTHER agent's durable write, which is attacker-controlled
	// amplification rather than self-harm.
	//
	// Keyed on the agent id, which is safe HERE for exactly the reason
	// auth.MaxActiveSessionsPerAgent is safe and the pending-session cap was
	// not: this is an AUTHENTICATED route, so the key is a proven identity and
	// a flooder can only fill its own bucket. It fails closed and evicts
	// nothing — evicting would let one connection of an agent kill another's.
	if h.waitersByAgent[agentID] >= MaxWaitersPerAgent {
		h.waitMu.Unlock()
		return Batch{Cursor: after}, fmt.Errorf("%w: agent %q already has %d parked long polls, the per-agent limit", ErrCapacity, agentID, MaxWaitersPerAgent)
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
