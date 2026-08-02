package store

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Retention defaults, decided by the user on 2026-08-02: ONE DAY or ONE
// GIGABYTE, WHICHEVER COMES FIRST.
//
// Both bounds are enforced together and the tighter one wins. Age bounds how
// far back an agent that was offline can catch up; bytes bounds what a busy bus
// can cost in memory regardless of how recent the traffic is.
const (
	// DefaultMaxAge is how long a message stays readable.
	DefaultMaxAge = 24 * time.Hour

	// DefaultMaxBytes is the ceiling on retained message BODIES. It accounts
	// bodies only, not the Go overhead of the surrounding structs, because the
	// body is the part a client controls and therefore the part a bound has to
	// be stated against.
	//
	// The per-message overhead is bounded too, but INDIRECTLY and by another
	// package, which is worth writing down because it would break silently:
	// every message carries a mandatory idempotency key, the hub remembers one
	// entry per message, and it forgets an entry only when that message ages
	// out of here — so the retained COUNT is capped by hub.MaxIdempotencyEntries
	// (65536), and the struct overhead by roughly 200 bytes times that. Raising
	// that cap, or making the idempotency key optional, removes the count bound
	// and leaves only this byte bound, which a flood of empty bodies does not
	// touch.
	DefaultMaxBytes int64 = 1 << 30

	// MaxBatchBytes bounds ONE batch returned by Since, in body bytes.
	//
	// The batch limit alone bounds a response by COUNT, and count is the wrong
	// unit: 256 messages of store.MaxBodyBytes is 16 MiB of body, which the
	// caller then base64-encodes and marshals, so one authenticated request
	// costs about 45 MiB of live allocation and a hundred of them cost several
	// gigabytes. Bounding bytes as well makes a response cost independent of
	// how large the messages happen to be.
	//
	// At least ONE message is always returned even if it alone exceeds this,
	// so a large message can never become permanently undeliverable — a batch
	// that returns nothing while `more` is true is an infinite loop for a
	// client that pages politely.
	MaxBatchBytes int64 = 1 << 20
)

// Options configures New. The zero value yields the documented defaults.
type Options struct {
	// MaxAge is the retention window; 0 means DefaultMaxAge.
	MaxAge time.Duration

	// MaxBytes is the retained-body ceiling; 0 means DefaultMaxBytes.
	MaxBytes int64

	// Now is the clock, overridable so retention is testable without sleeping.
	// Defaults to time.Now.
	Now func() time.Time
}

// Store is the in-memory serving copy of the message stream (invariant 5:
// memory is the serving copy, disk is the truth). It is safe for concurrent
// use.
//
// # Append-only, in sequence order
//
// Messages are held in one slice in strictly ascending sequence order. There is
// no per-agent queue and deliberately so: a broadcast would have to be copied
// into every queue, an agent that enrols later could not read back through the
// retention window, and the "exactly once per waiter" property POLL-3 demands
// would then depend on N queues agreeing rather than on one order everybody
// reads with their own cursor. One stream plus a per-agent cursor is the
// simpler construction (invariant 8) and it is what makes at-least-once
// delivery mean something precise.
//
// # Retention drops from the FRONT only
//
// Nothing is ever edited or removed from the middle. Pruning takes whole
// messages off the oldest end, which is the only shape that keeps the remaining
// slice a contiguous suffix of the accepted history — the same prefix property
// invariant 5 asks of recovery, read from the other end.
type Store struct {
	// mu guards every field. A plain Mutex rather than an RWMutex because both
	// Append and Since prune, so both need write access; an RWMutex whose read
	// path takes the write lock anyway is a more complicated way to spell this.
	mu sync.Mutex

	maxAge   time.Duration
	maxBytes int64
	now      func() time.Time

	// msgs is ascending by Seq and holds no gaps of its own making: a gap in
	// the sequence means the bus burned a number (a discarded prepare, a failed
	// write), which is correct and must not be compacted away — invariant 1
	// never rewinds a sequence.
	msgs []Message

	// bytes is the sum of Size() over msgs.
	bytes int64

	// head is the highest sequence ever APPENDED, which is not the same as the
	// highest retained: it survives pruning, so a cursor at the head stays at
	// the head even after everything behind it has aged out.
	head uint64

	// dropped counts messages retention has removed, for the operator-facing
	// statistics. It only ever grows.
	dropped uint64
}

// New returns an empty Store.
func New(opts Options) *Store {
	s := &Store{
		maxAge:   opts.MaxAge,
		maxBytes: opts.MaxBytes,
		now:      opts.Now,
	}
	if s.maxAge <= 0 {
		s.maxAge = DefaultMaxAge
	}
	if s.maxBytes <= 0 {
		s.maxBytes = DefaultMaxBytes
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// Append adds m to the serving copy.
//
// It MUST be called only after m is durable (invariant 4). This type has no way
// to check that and does not try to: the ordering lives in the hub's publish
// path, which writes through the two-phase log and only then calls here.
//
// The sequence must be strictly greater than the head. That is not a
// convenience for the binary search below — it is the check that catches a
// replay applying an entry twice, or a write path that reused a number, at the
// one moment such a bug is still cheap to see.
func (s *Store) Append(m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if m.Seq == 0 {
		return fmt.Errorf("%w: sequence 0 is never allocated", ErrInvalidMessage)
	}
	if m.Seq <= s.head {
		return fmt.Errorf("%w: appending sequence %d behind head %d (message %s)", ErrOutOfOrder, m.Seq, s.head, m.ID)
	}
	s.msgs = append(s.msgs, m)
	s.bytes += int64(m.Size())
	s.head = m.Seq
	s.pruneLocked()
	return nil
}

// Head reports the highest sequence ever appended, or 0 for an empty store. It
// is the position a cursor reaches when it has seen everything.
func (s *Store) Head() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head
}

// Stats reports the retained state, for operators and tests.
func (s *Store) Stats() (count int, bytes int64, oldest uint64, head uint64, dropped uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) > 0 {
		oldest = s.msgs[0].Seq
	}
	return len(s.msgs), s.bytes, oldest, s.head, s.dropped
}

// Since returns up to limit messages with a sequence strictly greater than
// after that are VISIBLE TO agentID, in sequence order.
//
// The returned next is the cursor position the caller should resume from:
//   - the sequence of the last message in the batch, when the batch is
//     non-empty;
//   - after, UNCHANGED, when the batch is empty.
//
// Leaving the cursor untouched on an empty batch is what POLL-1 requires (a
// long poll that times out returns the same cursor), and it is also the safe
// direction: advancing a cursor past messages the caller has not been handed is
// how an at-least-once system quietly becomes an at-most-once one.
//
// more reports that the batch was cut short — by limit or by MaxBatchBytes —
// and another call will return immediately. It is NOT "the batch was full": a
// batch that exactly fills the limit with nothing visible behind it reports
// more == false, because a further call would return nothing.
//
// The filter is applied with agentID and enrolledAt, which the caller takes
// from the AUTHENTICATED principal and this bus's roster. See
// Message.VisibleTo, and read the enrolment-epoch section there before
// changing what enrolledAt is passed.
func (s *Store) Since(agentID string, enrolledAt time.Time, after uint64, limit int) (batch []Message, next uint64, more bool) {
	if limit <= 0 {
		return nil, after, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Retention is enforced here as well as on Append so that an IDLE bus does
	// not hold a day-old stream open for ever. Append alone would mean the
	// clock only advances when someone sends.
	s.pruneLocked()

	next = after
	// Binary search for the first retained message strictly after the cursor.
	// A cursor that has fallen behind the retention window lands at 0 and the
	// caller resumes at the oldest retained message — messages in between are
	// GONE, which is what a retention window means. CONTRACTS-HTTP.md states
	// it; it is not a delivery bug to be hidden.
	var bytes int64
	i := sort.Search(len(s.msgs), func(k int) bool { return s.msgs[k].Seq > after })
	for ; i < len(s.msgs); i++ {
		m := s.msgs[i]
		if !m.VisibleTo(agentID, enrolledAt) {
			continue
		}
		// Both cut-short tests run BEFORE the message is added, and both are
		// skipped for an empty batch: a caller must always make progress, or a
		// polite pager that stops at `more == false` spins for ever on a
		// message too large for the byte budget.
		if len(batch) > 0 && (len(batch) == limit || bytes+int64(m.Size()) > MaxBatchBytes) {
			return batch, next, true
		}
		batch = append(batch, copyMessage(m))
		bytes += int64(m.Size())
		next = m.Seq
	}
	return batch, next, false
}

// HasVisibleAfter reports whether any retained message strictly after
// is visible to agentID. It is the cheap predicate the long-poll registration
// path uses to decide whether to park, without materialising a batch.
func (s *Store) HasVisibleAfter(agentID string, enrolledAt time.Time, after uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := sort.Search(len(s.msgs), func(k int) bool { return s.msgs[k].Seq > after })
	for ; i < len(s.msgs); i++ {
		if s.msgs[i].VisibleTo(agentID, enrolledAt) {
			return true
		}
	}
	return false
}

// copyMessage deep-copies the two slices a Message carries out of the store.
//
// NewMessage copies carefully on the way IN, for the reason given there; a
// caller that could reach into the stored slices on the way OUT would defeat
// that entirely. A handler renders these into a response and a test inspects
// them, so the aliasing would be silent right up to the moment something
// mutated a body the store still believes it holds.
func copyMessage(m Message) Message {
	out := m
	out.Body = append([]byte(nil), m.Body...)
	out.Recipients = append([]string(nil), m.Recipients...)
	out.BusPath = append([]string(nil), m.BusPath...)
	return out
}

// pruneLocked drops messages off the front until both retention bounds hold.
// The caller must hold s.mu.
//
// Age is evaluated against SentAt, the instant this bus ACCEPTED the message,
// not against an insertion timestamp: on the recovery path a replayed message
// keeps its original SentAt, so a restart does not silently reset the retention
// clock and resurrect a day of history that had already aged out.
func (s *Store) pruneLocked() {
	if len(s.msgs) == 0 {
		return
	}
	cutoff := s.now().Add(-s.maxAge)

	drop := 0
	for drop < len(s.msgs) && s.msgs[drop].SentAt.Before(cutoff) {
		s.bytes -= int64(s.msgs[drop].Size())
		drop++
	}
	for drop < len(s.msgs) && s.bytes > s.maxBytes {
		s.bytes -= int64(s.msgs[drop].Size())
		drop++
	}
	if drop == 0 {
		return
	}
	s.dropped += uint64(drop)

	// Re-slice into a FRESH backing array rather than s.msgs[drop:]. Keeping
	// the old array would pin every dropped message's body in memory for as
	// long as the slice lives, which is exactly the growth retention exists to
	// stop.
	kept := make([]Message, len(s.msgs)-drop)
	copy(kept, s.msgs[drop:])
	s.msgs = kept
}
