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

	// Logger receives the ONE event this package discards: a message whose
	// sequence arrived after retention had already dropped that position
	// (Append, SIGN-1-FU-OUTOFORDER-POISON).
	//
	// It defaults to a logger on os.Stderr rather than to the discarding logger
	// the rest of the repo defaults to, and that difference is deliberate:
	// invariant 6 sanctions the discard but makes SILENT discard the defect, and
	// the only production caller (internal/hub) builds Options without this
	// field. A discarding default would therefore make the real bus silent,
	// which is the failure mode the invariant names.
	Logger *logging.Logger
}

// Store is the in-memory serving copy of the message stream (invariant 5:
// memory is the serving copy, disk is the truth). It is safe for concurrent
// use.
//
// # Held in sequence order, which is NOT arrival order
//
// Messages are held in one slice in strictly ascending sequence order. They do
// not ARRIVE in that order: since SIGN-1 the sequence is minted (and durably
// burned) before the client signs and sends, so two agents holding concurrent
// reservations spend them in whatever order they please, and Append inserts a
// late lower number into position rather than refusing it. See Append.
//
// # KNOWN GAP: a late arrival is not DELIVERED to a reader that has passed it
//
// Filed as SIGN-1-FU-REORDER-WATERMARK, Spec Server task
// c829af9a-4418-437a-a0f8-34ef2f5d15d0. LOOK IT UP BY THE UUID: that task's
// `key` field is null and the server has no way to set one after creation, so
// fetching it by the readable name 404s. Deliberately NOT closed here — this
// type cannot close it (see the watermark paragraph below).
//
// A cursor is a sequence. If seq 2 is delivered and a reader's cursor advances
// to 2, a seq 1 that lands afterwards is inserted BELOW that cursor, and Since
// — which binary-searches for the first message strictly after the cursor —
// will never hand it to that reader. The message is durable, it is in the audit
// trail, and it is served to every cursor still below it; what it is not is
// delivered to a reader that has already passed that position.
//
// # DO NOT read this as "rare" — for an actively waiting agent it is the COMMON case
//
// The obvious reading is that only a reader who happened to poll between the
// two spends loses the message. That reading is wrong, and it was measured: a
// reader LONG-POLLING AT THE HEAD CURSOR misses it permanently AND ITS POLL
// NEVER WAKES. The mechanism is in the hub, not here: hub.notify skips any
// waiter whose cursor is at or above the message's sequence, and every wake
// point then re-reads through Since with that same cursor — so a late arrival
// lands below it twice over, once for the wake and once for the read.
// Long-poll-at-head is the primary mode of every agent on this bus, so for an
// actively waiting recipient this is the ordinary outcome, not a narrow race.
//
// This is a pre-existing consequence of SIGN-1, not of the fix. The poison
// MASKED it: before this fix the bus stopped dead instead of skipping a
// message, so the gap could not be observed. Trading a whole-bus halt that any
// enrolled agent could trigger at will for a missed delivery is the right way
// round, and it is emphatically not the end state.
//
// Closing it needs a REORDER WATERMARK — "no sequence <= W can still arrive" —
// and this package cannot compute one: the answer lives in the hub's table of
// outstanding mints (a reservation is outstanding until it is spent or its TTL
// expires), which the store cannot see and must not be given a back channel to.
// So the fix belongs above this type, and there is deliberately NO watermark
// API here for nothing to call.
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

	// prunedHead is the HIGHEST sequence retention has ever dropped, 0 if
	// nothing has been dropped. It only ever grows.
	//
	// It stops a sequence that has already been served and then pruned from
	// sailing back in and being served a SECOND time from a slot behind the
	// window. The old strictly-increasing rule got that for free from
	// monotonicity: a number below the head could not be re-admitted because no
	// number below the head could be admitted at all. Ordered insertion loses it,
	// because the duplicate check can only see what is still RETAINED.
	//
	// NOTE WHAT IT DOES NOT DO, and do not let the P1 wording in Append drift
	// back into claiming it: this is a HIGH-WATER MARK, not a set, so it PREVENTS
	// the re-serve without DETECTING the reissue. Once a position is pruned, a
	// genuine double-apply of that sequence and a merely very late arrival are
	// indistinguishable here, and both take the same branch. See that branch for
	// why it must not be an error.
	prunedHead uint64

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
		log:      opts.Logger,
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
		// os.Stderr, NOT io.Discard — see Options.Logger. WARN is the only level
		// this package emits, so the threshold costs nothing and cannot silence
		// the one line invariant 6 requires.
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
//	    and reported as ErrDuplicateSequence. Across the region retention has
//	    already dropped it is enforced only in the weaker sense that prunedHead
//	    refuses to retain the message a second time: the reissue is PREVENTED
//	    from being served but is NOT detected, because a high-water mark cannot
//	    distinguish a double-apply from a merely very late first arrival. That
//	    is a deliberate narrowing of an enforcement point the old rule covered —
//	    see the branch below, and the Spec Server task
//	    SIGN-1-FU-OUTOFORDER-POISON, which carries the reasoning and both gate
//	    findings in full.
//	P2  msgs stays sorted ascending by Seq, because Since binary-searches it
//	    (and HasVisibleAfter, which shares the search). A late message appended
//	    at the END rather than into
//	    position would be invisible to every reader, which is a worse bug than
//	    the one being fixed.
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

	// The insertion point, and the duplicate check, in one search: i is the
	// first retained message whose sequence is >= m.Seq.
	i := sort.Search(len(s.msgs), func(k int) bool { return s.msgs[k].Seq >= m.Seq })
	if i < len(s.msgs) && s.msgs[i].Seq == m.Seq {
		// P1. A genuine double-apply, and it stays LOUD: the hub poisons itself
		// on this, and that is the correct response to the server having handed
		// the same id out twice.
		return fmt.Errorf("%w: sequence %d is already applied (appending message %s, retained message %s)", ErrDuplicateSequence, m.Seq, m.ID, s.msgs[i].ID)
	}

	if m.Seq <= s.prunedHead {
		// ACCEPTED BUT NOT RETAINED, which is precisely what retention means.
		// The window has already moved past this position, so keeping the
		// message would resurrect a slot BEHIND the window.
		//
		// THIS BRANCH IS AMBIGUOUS AND THE AMBIGUITY IS NOT RESOLVABLE HERE. It
		// is taken by two different events that this type cannot tell apart:
		//
		//  1. a legitimate first arrival that is simply later than the window —
		//     harmless, and nothing is owed to it beyond the log line;
		//  2. a DOUBLE-APPLY of a sequence that was already served and has since
		//     been pruned — an invariant 1 breach, which the old
		//     strictly-increasing rule DID catch and this one does not.
		//
		// prunedHead is a high-water mark, not a set, so once the position is
		// gone there is no evidence left to separate them. The log line therefore
		// names BOTH readings rather than reporting the benign one as fact.
		//
		// It must not be an ERROR, and that is the reason the ambiguity is
		// tolerated rather than resolved by failing closed. The record is already
		// committed and fsynced by the time it reaches here, so an error re-opens
		// a narrower version of the very DoS this change closes: an agent holding
		// a reservation across a byte-pressure prune would poison the bus. Case 2
		// is also, by construction, unable to serve the sequence twice — which is
		// the harm invariant 1 exists to prevent — so what is lost is DETECTION,
		// not the property. The narrowing is deliberate and is recorded on the
		// Spec Server task SIGN-1-FU-OUTOFORDER-POISON.
		s.log.Warn("message NOT applied to the serving copy: its sequence is at or below the highest sequence retention has already dropped, so the window has moved past that position. This is EITHER a legitimate arrival later than the retention window OR a double-apply of a sequence that was already served and pruned (invariant 1), and the store cannot tell them apart once the position is gone. The message is durable and in the audit trail; it will NOT be served",
			"seq", m.Seq,
			"message_id", m.ID,
			"sender", m.Sender,
			"pruned_head", s.prunedHead,
		)
		return nil
	}

	// A LATE INSERT IS LOGGED, and this line is not decoration.
	//
	// The message IS retained and IS served to every cursor still below it, so
	// this is not a discard in the store's own terms. But a reader that has
	// already passed this position never receives it and is never woken — see
	// the "KNOWN GAP" section on Store — and FROM THAT RECIPIENT'S POINT OF VIEW
	// the message was silently dropped. Invariant 6 rates a silent discard the
	// defect, so the ordinary late insert must not be the one event on this path
	// that says nothing.
	//
	// The volume is bounded by the reservation system, not by this line: on the
	// LIVE path a late insert requires an outstanding mint, mints are capped at
	// hub.MaxOutstandingMintsPerAgent per agent and hub.MaxOutstandingMints
	// bus-wide per hub.MintTTL, and every one of them costs the sender a fsynced
	// durable send.
	//
	// THE RECOVERY PATH IS NOT COVERED BY THAT BOUND, and an earlier draft of
	// this comment claimed it was bounded by the retained set. THAT WAS FALSE and
	// the security gate refuted it with a running test: with MaxBytes tuned so
	// only two messages are retained, replaying twenty out-of-order records
	// emitted TWENTY lines. Replay calls Append for every record in the log, and
	// the log is never compacted (internal/wal), so the real bound is the number
	// of out-of-order records EVER WRITTEN — and it is re-paid in full on every
	// single start, with no further cost to whoever produced them.
	//
	// So this line is UNCAPPED on the recovery path, unlike the analogous replay
	// line one layer up, which hub caps one-shot and then reports a total. That
	// is a known defect, not a considered trade: at volume these lines can drown
	// the capped invariant-6 DISCARD errors they interleave with. Filed with the
	// logger pass-through fix, which shares the same call site.
	//
	// It is emitted under s.mu, like the branch above. That serialises readers
	// against a blocking write to the log sink for the duration of one line.
	// Accepted: the alternative is collecting the event and logging it after the
	// unlock, which lets two concurrent appends report in an order that does not
	// match the order they were applied — and on this branch, whose entire
	// subject is a confusing order, that is the worse trade.
	if m.Seq < s.head {
		s.log.Warn("message applied BELOW the head of the serving copy: it was minted before, and spent after, a higher sequence. It is durable, retained and served to every cursor still behind it, but a reader whose cursor has ALREADY passed this position will never be handed it and will not be woken. Tracked as SIGN-1-FU-REORDER-WATERMARK, Spec Server task c829af9a-4418-437a-a0f8-34ef2f5d15d0 (look it up by that UUID, not by the name)",
			"seq", m.Seq,
			"message_id", m.ID,
			"sender", m.Sender,
			"head", s.head,
		)
	}

	// P2: insert in sequence order, not at the end.
	s.msgs = append(s.msgs, Message{})
	copy(s.msgs[i+1:], s.msgs[i:])
	s.msgs[i] = m

	s.bytes += int64(m.Size())
	// The head only ever GROWS (invariant 1 — it never rewinds).
	if m.Seq > s.head {
		s.head = m.Seq
	}
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
// A cursor is a SEQUENCE, so a message that lands below a cursor that has
// already passed it is never handed to that reader — and for a reader parked on
// a long poll at the head cursor, that is the ordinary outcome rather than a
// rare one. Read the "KNOWN GAP" section on Store before treating it as a bug
// to fix here: it is filed as SIGN-1-FU-REORDER-WATERMARK (Spec Server task
// c829af9a-4418-437a-a0f8-34ef2f5d15d0) and it cannot be closed in this package.
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
//
// # The age bound is SOFT by at most hub.MintTTL (SIGN-1-FU-OUTOFORDER-POISON)
//
// The age loop stops at the first message that has NOT expired, which assumes
// msgs is ordered by SentAt. Since Append began inserting late arrivals into
// sequence position, that assumption no longer holds exactly: msgs is ordered
// by Seq, and a message minted EARLY but spent LATE carries a NEWER SentAt than
// the higher-sequence neighbours BEHIND it, which were minted later and spent
// sooner. (Stated in that direction deliberately — the first draft of this
// paragraph had it backwards, and the inverted version makes the conclusion
// below look like under-retention when it is the opposite.)
//
// So the loop can stop EARLY, on a young message sitting at a low sequence
// position, leaving genuinely expired messages behind it retained. That is
// OVER-retention, and it is the harmless direction: the loop only ever drops a
// message it has individually tested as expired, so nothing is dropped before
// its time.
//
// It is accepted, not overlooked, because the disorder is BOUNDED. For any two
// retained messages k before j in the slice, seq_k < seq_j implies k was minted
// no later than j; a sequence cannot be spent more than hub.MintTTL after it was
// minted (the reservation expires); so SentAt_k − SentAt_j ≤ hub.MintTTL. A
// message is therefore retained at most MintTTL past its expiry.
//
// The BYTE bound — the memory-safety one — is unaffected: it drops from the
// front unconditionally, without consulting a timestamp.
//
// What would break the bound is growing hub.MintTTL substantially. Do NOT
// "fix" this by sorting on SentAt: that destroys P2 (see Append), and Since —
// the read path every client reaches — binary-searches this slice by sequence.
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
	// msgs is ascending by Seq and drops come off the front, so the last
	// dropped message carries the highest sequence retention has just removed.
	// Guarded so prunedHead only ever grows — it is the floor Append refuses
	// below, and a floor that could fall would let a pruned sequence back in.
	if last := s.msgs[drop-1].Seq; last > s.prunedHead {
		s.prunedHead = last
	}

	// Re-slice into a FRESH backing array rather than s.msgs[drop:]. Keeping
	// the old array would pin every dropped message's body in memory for as
	// long as the slice lives, which is exactly the growth retention exists to
	// stop.
	kept := make([]Message, len(s.msgs)-drop)
	copy(kept, s.msgs[drop:])
	s.msgs = kept
}
