package store

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
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

	// Logger receives the ONE event this package now reports: an Append whose
	// delivery POSITION is not above every position already appended
	// (SIGN-1-FU-REORDER-WATERMARK). That cannot happen without a server bug —
	// positions are WAL commit indices minted under the hub's write lock — so it
	// is logged at ERROR, and the message is retained anyway.
	//
	// It defaults to a logger on os.Stderr rather than to the discarding logger
	// the rest of the repo defaults to, and that difference is deliberate:
	// invariant 6 makes SILENT loss the defect, and the only production caller
	// (internal/hub) builds Options without this field. A discarding default
	// would therefore make the real bus silent, which is the failure mode the
	// invariant names.
	Logger *logging.Logger
}

// Store is the in-memory serving copy of the message stream (invariant 5:
// memory is the serving copy, disk is the truth). It is safe for concurrent
// use.
//
// # Held in DELIVERY ORDER (Message.Pos), which is not sequence order
//
// Messages are held in one slice in ascending Message.Pos — the WAL commit
// index, i.e. the order this bus took responsibility for them. They are NOT
// held in sequence order, and the difference is the whole subject of
// SIGN-1-FU-REORDER-WATERMARK.
//
// # IDENTITY (Seq) IS SEPARATE FROM DELIVERY ORDER (Pos), and that CLOSED the gap
//
// Since SIGN-1 a sequence is minted, and durably burned, BEFORE the client
// signs and sends: hub.Mint hands the number out so the SENDER can sign it, and
// the send follows up to hub.MintTTL later. Two agents holding reservations at
// once therefore spend them in whatever order they please. Seq is a
// PRE-ASSIGNED IDENTITY; it says nothing about when the message committed.
//
// This type used to keep the slice in Seq order and let a cursor be a Seq, and
// that combination lost mail. A message committed, fsynced and ACKNOWLEDGED at
// a sequence below a reader's cursor was inserted BELOW that cursor, Since
// binary-searched strictly after the cursor and never reached it, and
// hub.notify skipped the parked waiter for the same reason — so an actively
// long-polling reader missed it permanently and was never even woken. The
// sender chose when to spend, which made it a targeted suppression and
// false-ack primitive rather than a race.
//
// The fix is the split, not a watermark. A watermark ("no sequence <= W can
// still arrive") was REFUTED by execution: one unspent reservation withheld 200
// already-acknowledged messages from every reader on the bus, and with
// hub.DefaultBatchLimit at 64 a reader clamped behind it cannot drain what it
// is finally released. So:
//
//   - Seq is unchanged: server-minted, client-signed, never reused, never
//     rewound (invariant 1 is NOT narrowed by this).
//   - Pos is the delivery position and is the wal.Committed.CommitIndex of the
//     record that made the message durable. Cursors, Since, HasVisibleAfter and
//     hub.notify all read it.
//
// A late arrival with a LOW Seq therefore gets a HIGH Pos, lands ABOVE every
// cursor, is served to every reader and wakes every waiter. Nothing is
// suppressed and nothing is stalled.
//
// # WHY THE POSITION MUST BE THE WAL INDEX and not a counter of this type's own
//
// It has to survive a restart, and it has to survive it IDENTICALLY. Replay
// folds the log in commit order, so a recovered message is handed the same
// index it committed under; a counter incremented inside Append would restart at
// 1 and renumber the whole retained window, so every stored client cursor would
// silently point somewhere else. The index is also already monotone, already
// durable, and already never reused — invariant 1 is what guarantees that, and
// internal/wal's index floor is what enforces it across restarts. See Append for
// the ordering the monotonicity actually rests on.
//
// There is no per-agent queue and deliberately so: a broadcast would have to be copied
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
	log      *logging.Logger

	// msgs is ascending by Pos — DELIVERY ORDER — and holds no gaps of its own
	// making. Gaps in both counters are normal and must not be compacted away: a
	// gap in the SEQUENCE means the bus burned a number (a discarded prepare, an
	// expired reservation), and a gap in the POSITION means the log holds
	// records that are not messages (floor records, enrolment, aborts). Neither
	// is damage, and invariant 1 never rewinds either counter.
	msgs []Message

	// bySeq maps a retained message's sequence to its id. It is the ONLY
	// duplicate-sequence detector left: the slice is no longer sequence-sorted,
	// so the insertion search cannot double as the duplicate check.
	//
	// It covers the RETAINED window only, and that is a real narrowing rather
	// than an oversight — a sequence that was served and has since been pruned
	// leaves no evidence here, so a double-apply of it is not DETECTED. What
	// the split does buy is that a re-arrival cannot be served from a slot
	// behind the window either: it gets a fresh, higher position and appears at
	// the tail like any other late arrival. See Append.
	bySeq map[uint64]string

	// bytes is the sum of Size() over msgs.
	bytes int64

	// head is the highest SEQUENCE ever APPENDED, which is not the same as the
	// highest retained: it survives pruning. It feeds nothing on the read path —
	// cursors are positions — and exists for the operator statistics and for the
	// invariant-1 assertion that it never rewinds.
	head uint64

	// posHead is the highest POSITION ever appended, 0 for an empty store. It is
	// what makes the ordinary append O(1): a position above it goes at the end.
	// It only ever grows, and it survives pruning for the same reason head does —
	// a cursor at the head must stay at the head after the window moves.
	posHead uint64

	// prunedPos is the HIGHEST position retention has ever dropped, 0 if nothing
	// has been dropped. It only ever grows.
	//
	// NOTHING IS REFUSED ON IT, and reintroducing such a refusal would reopen
	// the suppression this design closes — under delivery ordering a late
	// arrival is genuinely deliverable however low its sequence, because it
	// lands at the TAIL. It is kept as the operator-facing measure of how far
	// behind the window a non-monotone position landed.
	prunedPos uint64

	// dropped counts messages retention has removed, for the operator-facing
	// statistics. It only ever grows.
	dropped uint64

	// nonMonotonicPos counts appends whose position was NOT above posHead. It is
	// a fault counter for a case that cannot be reached without a server bug
	// (see Append) and is exposed by NonMonotonicPositions so the condition is
	// observable rather than log-only.
	nonMonotonicPos uint64
}

// New returns an empty Store.
func New(opts Options) *Store {
	s := &Store{
		maxAge:   opts.MaxAge,
		maxBytes: opts.MaxBytes,
		now:      opts.Now,
		log:      opts.Logger,
		bySeq:    make(map[uint64]string),
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
	if s.log == nil {
		// os.Stderr, NOT io.Discard — see Options.Logger. ERROR is the only level
		// this package emits (the non-monotone-position fault in Append), and a
		// LevelWarn threshold is below it, so the threshold cannot silence the
		// one line invariant 6 requires.
		s.log = logging.New(os.Stderr, logging.LevelWarn)
	}
	return s
}

// Append adds m to the serving copy.
//
// It MUST be called only after m is durable (invariant 4). This type has no way
// to check that and does not try to: the ordering lives in the hub's publish
// path, which writes through the two-phase log and only then calls here.
//
// # The sequence need NOT be greater than the head (SIGN-1-FU-OUTOFORDER-POISON)
//
// It used to have to be, and the rule's doc comment claimed it "catches a
// replay applying an entry twice, or a write path that reused a number". Those
// are the real properties, and neither of them needs "strictly greater than the
// head" — the old rule only implied them because, before SIGN-1, the sequence
// was allocated under the hub's write lock immediately before this call, so
// commit order equalled allocation order by construction.
//
// SIGN-1 broke that coupling on purpose: hub.Mint allocates and durably burns a
// number so the CLIENT can sign it, and the send follows. Reservations live for
// hub.MintTTL, so two agents holding numbers at once and spending them in the
// other order is the ordinary shape of the protocol, not a race. Refusing the
// late one here was a P0: the hub has already completed the two-phase durable
// write and fsynced (invariant 4) before it calls Append, so the refusal
// orphaned a committed record on disk and poisoned the hub permanently — two
// mints and two sends from any enrolled agent stopped the bus.
//
// What is enforced instead is exactly the two load-bearing properties:
//
//	P1  No sequence is ever SERVED twice (invariant 1: ids are never reused).
//	    Within the RETAINED window this is exact, and a violation is DETECTED
//	    and reported as ErrDuplicateSequence, out of bySeq. Across the region
//	    retention has already dropped, the detection is GONE — a pruned sequence
//	    leaves no evidence — and that narrowing is deliberate and recorded on
//	    the Spec Server task SIGN-1-FU-OUTOFORDER-POISON.
//	P2  msgs stays sorted ascending by Pos, because Since binary-searches it
//	    (and HasVisibleAfter, which shares the search).
//
// # THE AT-OR-BELOW-prunedHead REFUSAL IS GONE, and must not come back
//
// A sequence arriving after retention had dropped that sequence's slot used to
// be accepted-but-NOT-retained. Under delivery ordering that would be pure
// suppression: the message is not going into a slot behind the window, it is
// going to the TAIL of the delivery order at a position above every live
// cursor, so it is genuinely deliverable to everyone. Refusing to retain it
// would discard an acknowledged message for no property gained
// (SIGN-1-FU-REORDER-WATERMARK).
//
// # THE POSITION MUST BE MONOTONE, and NOTHING IN THIS PACKAGE CAN ENFORCE THAT
//
// This is the load-bearing precondition and it is invisible from here.
// Positions are monotone only because hub.publish holds writeMu ACROSS
// durable.Write → store.Append, so the WAL hands out commit indices and this
// type consumes them in the same order. Recovery gets it for free: replay is
// single-threaded and in commit order.
//
// A THIRD CALLER APPENDING OFF THAT LOCK SILENTLY RESTORES THE DEFECT THIS
// DESIGN CLOSED — two writers interleaved between Write and Append would let a
// higher position be applied first, and the lower one would then land below
// cursors that have already passed it. There is no check here that would catch
// it: the branch below RETAINS the message (see the reasoning there), so the
// only signal is the ERROR line and NonMonotonicPositions. If you are adding a
// call site, hold writeMu across both halves or do not add it.
//
// The head still NEVER rewinds (invariant 1). It is the highest sequence ever
// appended, and a late lower number must not drag it back: the next number the
// bus hands out would then be one it has already used.
func (s *Store) Append(m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if m.Seq == 0 {
		return fmt.Errorf("%w: sequence 0 is never allocated", ErrInvalidMessage)
	}
	if m.Pos == 0 {
		// Position 0 is the RESERVED "I have seen nothing" cursor value, so a
		// message stamped 0 would sit below every cursor in existence and be
		// re-served on every poll — a message that replays a reader's whole
		// retention window for ever. It is not a degraded delivery, it is a
		// caller that forgot to stamp the WAL commit index, and it is refused
		// with the same shape as sequence 0 so the two read alike.
		return fmt.Errorf("%w: delivery position 0 is never assigned; the position is the WAL commit index and 0 is the reserved \"seen nothing\" cursor (message %s, sequence %d)", ErrInvalidMessage, m.ID, m.Seq)
	}

	if prev, dup := s.bySeq[m.Seq]; dup {
		// P1. A genuine double-apply, and it stays LOUD: the hub poisons itself
		// on this, and that is the correct response to the server having handed
		// the same id out twice.
		return fmt.Errorf("%w: sequence %d is already applied (appending message %s, retained message %s)", ErrDuplicateSequence, m.Seq, m.ID, prev)
	}

	if m.Pos > s.posHead {
		// THE ORDINARY CASE, and it is O(1). Positions are monotone (see the doc
		// comment), so the tail of the slice is the insertion point and there is
		// no search to do. A late LOW SEQUENCE takes this branch like any other
		// message — that is the whole point of the split.
		s.msgs = append(s.msgs, m)
	} else {
		// A NON-MONOTONE OR DUPLICATE POSITION. It is RETAINED, in order, and the
		// call RETURNS NIL. All three halves of that are deliberate, and the
		// resolution is recorded on SIGN-1-FU-REORDER-WATERMARK:
		//
		//   - It is NOT client-reachable. Positions are WAL commit indices minted
		//     by the server under the hub's write lock, so a merely BUGGY (or
		//     hostile) client cannot steer a message here — invariant 10's first
		//     question. It is therefore not a denial-of-service vector whichever
		//     answer is chosen, and the choice can be made on damage alone.
		//   - RETURNING AN ERROR WOULD POISON THE HUB. By the time this runs the
		//     record is committed and fsynced (invariant 4), so a refusal orphans
		//     it on disk and stops the bus — exactly the P0 that
		//     SIGN-1-FU-OUTOFORDER-POISON fixed. A distinct sentinel does not
		//     help: the caller's only response to any error here is to poison.
		//   - RETURNING NIL WITHOUT RETAINING would be silent suppression, which
		//     is the defect this whole task exists to remove.
		//
		// So: retain, stay up, and be LOUD (invariant 6 — the discard is never
		// the defect, the SILENCE is). It is counted as well as logged, because a
		// log line is not queryable and this is the one signal that the write
		// path's locking assumption has stopped holding.
		s.nonMonotonicPos++
		s.log.Error("message applied with a delivery position that is NOT above the highest position already applied. Positions are WAL commit indices and must be monotone; this means the durable write and the serving-copy append are no longer serialised by the hub's write lock, and readers whose cursor is already past this position will NOT be handed this message. The message is durable, and it is RETAINED rather than discarded — refusing it would orphan a committed record and halt the bus (SIGN-1-FU-REORDER-WATERMARK)",
			"pos", m.Pos,
			"pos_head", s.posHead,
			"pruned_pos", s.prunedPos,
			"seq", m.Seq,
			"message_id", m.ID,
			"sender", m.Sender,
			"non_monotonic_total", s.nonMonotonicPos,
		)
		// P2: insert in POSITION order, not at the end. A message appended past
		// the tail of an ordered slice would break the binary search in Since and
		// be invisible to every reader — a worse outcome than the one being
		// reported.
		i := sort.Search(len(s.msgs), func(k int) bool { return s.msgs[k].Pos >= m.Pos })
		s.msgs = append(s.msgs, Message{})
		copy(s.msgs[i+1:], s.msgs[i:])
		s.msgs[i] = m
	}

	s.bySeq[m.Seq] = m.ID
	s.bytes += int64(m.Size())
	// Both high-water marks only ever GROW (invariant 1 — neither rewinds).
	if m.Seq > s.head {
		s.head = m.Seq
	}
	if m.Pos > s.posHead {
		s.posHead = m.Pos
	}
	s.pruneLocked()
	return nil
}

// NonMonotonicPositions reports how many appends have arrived with a delivery
// position that was not above every position already applied.
//
// It is ZERO on a correct server and can only become non-zero through a bug in
// the write path's locking (see Append). It is exposed separately from Stats,
// which reports the RETAINED state an operator reads routinely, because this is
// a fault counter: an alert watches it for any value at all rather than for a
// trend.
func (s *Store) NonMonotonicPositions() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nonMonotonicPos
}

// PosHead reports the highest DELIVERY POSITION ever appended, or 0 for an empty
// store. It is the cursor value a reader reaches when it has seen everything,
// and — unlike Head, which reports a sequence — it is the counter cursors are
// expressed in.
func (s *Store) PosHead() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posHead
}

// Head reports the highest SEQUENCE ever appended, or 0 for an empty store.
//
// It is NOT a cursor value and must not be used as one: cursors are delivery
// positions (Message.Pos), and PosHead is their high-water mark. The two
// counters are unrelated — a message's sequence is minted before it is signed,
// its position when it commits.
func (s *Store) Head() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head
}

// Stats reports the retained state, for operators and tests.
//
// oldest and head are both SEQUENCES — the sequence of the oldest retained
// message and the highest ever appended. Neither is a cursor value; see PosHead
// for the delivery-position high-water mark, and NonMonotonicPositions for the
// fault counter that is deliberately not folded in here.
func (s *Store) Stats() (count int, bytes int64, oldest uint64, head uint64, dropped uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) > 0 {
		oldest = s.msgs[0].Seq
	}
	return len(s.msgs), s.bytes, oldest, s.head, s.dropped
}

// Since returns up to limit messages with a DELIVERY POSITION strictly greater
// than after that are VISIBLE TO agentID, in delivery order.
//
// after is a POSITION (Message.Pos), not a sequence. That is what closes
// SIGN-1-FU-REORDER-WATERMARK: a message minted early and spent late carries a
// low sequence but a HIGH position, so it lands above every live cursor and is
// served to every reader rather than being invisible to the ones that had
// already passed its sequence.
//
// The returned next is the cursor position the caller should resume from:
//   - the POSITION of the last message in the batch, when the batch is
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
	i := sort.Search(len(s.msgs), func(k int) bool { return s.msgs[k].Pos > after })
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
		next = m.Pos
	}
	return batch, next, false
}

// HasVisibleAfter reports whether any retained message strictly after the given
// DELIVERY POSITION is visible to agentID. It is the cheap predicate the
// long-poll registration path uses to decide whether to park, without
// materialising a batch.
//
// It shares Since's search and must keep sharing its counter: a predicate that
// asked about sequences while Since served positions would park a poll on a
// message the very next read would not return.
func (s *Store) HasVisibleAfter(agentID string, enrolledAt time.Time, after uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := sort.Search(len(s.msgs), func(k int) bool { return s.msgs[k].Pos > after })
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
//
// # The age bound is EXACT again (SIGN-1-FU-REORDER-WATERMARK)
//
// The age loop stops at the first message that has NOT expired, which assumes
// msgs is ordered by SentAt. It is: msgs is ordered by Message.Pos, and COMMIT
// ORDER IS SentAt ORDER. On the live path hub.publish stamps SentAt and takes
// the WAL commit index under the same writeMu, so the two advance together; on
// the recovery path replay folds records in commit order carrying their
// original SentAt, so the same holds after a restart.
//
// This paragraph previously documented a SOFT bound — over-retention by up to
// hub.MintTTL — because the slice was then ordered by SEQUENCE, and a message
// minted early but spent late sat at a low sequence position carrying a newer
// SentAt than its neighbours. Ordering by position removed the disorder rather
// than bounding it. DO NOT restore the soft-bound wording: it would assert a
// disorder that no longer exists and invite a "fix" for it.
//
// The one residual is the non-monotone-position branch of Append, which cannot
// be reached without a server bug, is logged at ERROR and counted by
// NonMonotonicPositions. In that case the loop can stop early and OVER-retain,
// which is the harmless direction: nothing is dropped that has not been
// individually tested as expired.
//
// The BYTE bound — the memory-safety one — is unaffected either way: it drops
// from the front unconditionally, without consulting a timestamp.
//
// Do NOT "fix" anything here by sorting on SentAt: that destroys P2 (see
// Append), and Since — the read path every client reaches — binary-searches this
// slice by position.
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
	// The duplicate-sequence index covers the RETAINED window, so a dropped
	// message's sequence leaves it with the message. This is where P1's
	// narrowing physically happens: past this point a re-arrival of that
	// sequence is indistinguishable from a first one. It is not a leak to
	// "fix" by keeping the whole history — an unbounded set keyed on every
	// sequence the bus ever issued is exactly the growth retention exists to
	// stop.
	for k := 0; k < drop; k++ {
		delete(s.bySeq, s.msgs[k].Seq)
	}
	// msgs is ascending by Pos and drops come off the front, so the last dropped
	// message carries the highest position retention has just removed. Guarded so
	// prunedPos only ever grows. NOTHING IS REFUSED ON IT — see the field.
	if last := s.msgs[drop-1].Pos; last > s.prunedPos {
		s.prunedPos = last
	}

	// Re-slice into a FRESH backing array rather than s.msgs[drop:]. Keeping
	// the old array would pin every dropped message's body in memory for as
	// long as the slice lives, which is exactly the growth retention exists to
	// stop.
	kept := make([]Message, len(s.msgs)-drop)
	copy(kept, s.msgs[drop:])
	s.msgs = kept
}
