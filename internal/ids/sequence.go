package ids

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

// ErrSequenceExhausted is returned by Next when the allocator has issued
// math.MaxUint64 and has no number left to hand out. It is a sentinel so a
// caller can distinguish "this bus is finished" from any other failure.
var ErrSequenceExhausted = errors.New("ids: sequence exhausted: math.MaxUint64 has been issued and a sequence number is never reused")

// ErrFloorBelowIssued is returned by RaiseFloor when the caller asks to raise
// the floor to a value this allocator has ALREADY issued. See RaiseFloor for
// why that is an error rather than a no-op.
var ErrFloorBelowIssued = errors.New("ids: refusing to set a sequence floor at or below a number already issued")

// Sequence is a strictly monotonic, concurrency-safe allocator of message
// sequence numbers. It never issues the same number twice, and — given a
// correct resume floor — never issues a number a previous run of this bus
// issued either. Sequence numbers are the second half of a message id
// ("<bus-id>-<seq>", see MessageID), so a reissued number is a reissued
// message id, which invariant 1 forbids outright.
//
// # The allocator holds NO durable state of its own
//
// Sequence is memory only. It writes nothing, reads nothing, and fsyncs
// nothing. A number becomes durable because the CALLER writes it into a WAL
// PREPARE frame and fsyncs that frame before the send is acknowledged. Sequence
// cannot enforce that ordering and does not try; the caller's contract is:
//
//	n, err := seq.Next()     // allocate
//	write n into the PREPARE // the number is now part of the record
//	fsync the WAL prepare    // the number has reached disk: it is BURNED
//	fsync the audit record
//	fsync the WAL commit     // the message is now accepted history
//	only now acknowledge "<bus-id>-<n>" to the client
//
// Acknowledging before the commit fsync violates invariant 4 no matter what
// this type does, and no amount of care here compensates for it.
//
// # A crash BEFORE the prepare fsync may safely reissue a number
//
// The rule is about numbers that reached DISK, not numbers that were merely
// allocated in memory. If a crash lands between Next and the prepare fsync, the
// number is in no record anywhere: no message bears that id, no audit line
// mentions it, no client was ever told it. Handing it out again after restart
// is therefore not a reuse — nothing exists to collide with. This is why
// Sequence needs no durable state: the only numbers that matter are the ones
// the caller already made durable, and the caller can read those back off disk.
//
// # Gaps in the committed stream are CORRECT — do not "fix" them
//
// Resume takes the highest sequence number EVER WRITTEN TO DISK, whether that
// record reached commit or was only a prepare that recovery later discarded.
// Under normal operation every prepare commits and the committed stream is
// contiguous. A crash between the prepare fsync and the commit fsync BURNS one
// number: the prepare is durable, so the number reached disk, but the message
// is not accepted history and will never be delivered. Recovery discards the
// prepare (see wal.Recovered.Dangling) and the committed stream is left with a
// hole.
//
// That hole is expected and correct. It is tempting to close it — to resume
// from the highest COMMITTED sequence instead, so the delivered stream has no
// gaps — and that is wrong. The discarded prepare is on disk and is in the
// append-only audit log, which is a superset of committed history; reissuing
// its number would put two different messages under one "<bus-id>-<seq>", and
// the audit trail would then hold both under that single id, destroying its
// value as evidence and breaking dedup (invariant 10) for anyone keyed on
// message id. Invariant 1 — "ids are never reused, including across restarts" —
// BEATS gap-freeness. Consumers must treat the sequence as strictly increasing,
// never as dense; anything that requires contiguity is reading the wrong
// counter.
//
// # The resume floor is the caller's responsibility, and it is the one place
// invariant 1 can be broken
//
// Resuming from a floor LOWER than a number a past run issued silently reissues
// ids, and this package CANNOT detect it: it keeps nothing on disk, so the only
// history it has is the floor it was handed. Everything else here is safe by
// construction; the floor is the whole attack surface. Two known ways to get it
// wrong:
//
//   - Deriving the floor from a record COUNT rather than from the highest
//     sequence number ever written. A count equals the high-water mark only in a
//     log with no gaps and no non-message records, which is exactly the property
//     a burned number destroys — so the first crash makes the count too low, and
//     the next run reissues.
//   - Lowering the floor after a corrupt-tail truncation, without first proving
//     the truncation may lower it. Truncation discards records and so lowers the
//     APPARENT high-water mark. Whether that is safe turns on ONE question: did
//     the discarded bytes ever complete an fsync? A frame that is torn on disk
//     is a frame whose append never returned, so nothing in it was ever
//     acknowledged and no number it carried was ever observed — reissuing that
//     number cannot make two OBSERVED messages share an id, which is the
//     property invariant 1 actually protects. That is why wal.RepairTail may cut
//     the last frame and reuse its index. The rule does NOT generalise: any
//     COMPLETE, fsynced record is durable and its sequence stays burned forever,
//     including a dangling PREPARE, which recovery discards as an ENTRY while
//     still counting its number in the high-water mark. So a floor may follow a
//     truncation down only across provably-never-durable bytes. Across anything
//     else, lowering it is simply forbidden — and note that nothing records the
//     sequence inside a frame that was cut, so "take the mark from before the
//     cut" is not an option a caller actually has.
//   - Deriving the floor from COMMITTED history. This is the one that will
//     actually happen, because it is what the obvious wiring produces:
//     wal.Replay hands its callback committed entries ONLY, and wal.Recovered
//     carries no message-sequence high-water mark at all. Fold that stream and
//     you get the highest COMMITTED sequence — which is exactly the value the
//     section above forbids. Allocate 100, fsync the prepare, crash before the
//     commit: 100 is burned and is in the audit log, the replay never sees it,
//     the floor comes back 99, and the next send is minted as "<bus-id>-100".
//     Two different messages, one id, both in the append-only audit trail. The
//     floor must come from EVERY prepare ever written — committed, aborted and
//     dangling alike — not from the committed projection of them.
//
// A caller that cannot PROVE its floor is greater than or equal to every
// sequence number ever written MUST refuse to start rather than guess. Refusing
// to start is a loud, recoverable outage; guessing low is a silent, permanent
// corruption of the id space.
//
// Note that wal.Recovered.NextIndex is the WAL RECORD index — a related but
// DISTINCT counter, incremented by commits and aborts as well as prepares, and
// shared by every record type in the log. It is not the message sequence and
// the two are not interchangeable. How the message-sequence floor is derived
// from recovery is the wiring task's decision, not an assumption to make here.
//
// The zero value is not usable; construct with NewSequence or Resume.
type Sequence struct {
	// mu guards both fields. A mutex, not a CAS loop: Next sits behind an
	// fsync on the real write path, so lock cost is irrelevant, and a mutex
	// makes RaiseFloor's read-compare-write trivially atomic against a
	// concurrent Next — which a CAS loop over two words would not be
	// (invariant 8).
	mu sync.Mutex

	// floor is the highest number that is BURNED: Next issues floor+1.
	floor uint64

	// last is the highest number issued BY THIS ALLOCATOR, 0 if none. It is
	// tracked separately from floor because a resumed allocator starts with a
	// non-zero floor and nothing issued, and RaiseFloor treats those two
	// states differently.
	last uint64
}

// NewSequence returns an allocator for a FRESH bus: floor 0, so the first
// Next returns 1. Zero is never issued — see MessageID, which rejects it,
// because a zero sequence is indistinguishable from an unset field.
func NewSequence() *Sequence { return &Sequence{} }

// Resume returns an allocator that resumes strictly ABOVE highestOnDisk, so
// the first Next returns highestOnDisk+1.
//
// highestOnDisk is the highest sequence number EVER WRITTEN TO DISK — prepared
// or committed, delivered or discarded. 0 means "nothing on disk" and is
// exactly equivalent to NewSequence. Read the Sequence doc before choosing this
// value: getting it too low is the one way this package can be made to break
// invariant 1, and it cannot detect that it has.
func Resume(highestOnDisk uint64) *Sequence { return &Sequence{floor: highestOnDisk} }

// Next allocates the next sequence number. It is safe for concurrent use.
//
// Overflow is an ERROR, never a wrap: at math.MaxUint64 it returns 0 and
// ErrSequenceExhausted and the allocator stays exhausted. Wrapping to 0 would
// reissue every id this bus has ever minted, which is the single worst outcome
// available here — far worse than a bus that stops accepting sends. Reaching it
// requires ~1.8e19 messages, so in practice this branch exists to make the
// wrap impossible, not because it is expected.
func (s *Sequence) Next() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.floor == math.MaxUint64 {
		return 0, ErrSequenceExhausted
	}
	s.floor++
	s.last = s.floor
	return s.floor, nil
}

// Last reports the highest number this allocator has ISSUED, or 0 if it has
// issued none. A resumed allocator reports 0 until its first Next, even though
// its floor is non-zero: Last answers "what did I hand out", not "what is
// burned".
func (s *Sequence) Last() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// RaiseFloor raises the floor to atLeast, so that every subsequent Next returns
// a number strictly greater than atLeast. It NEVER lowers the floor.
//
// # Where the line between "no-op" and "error" sits
//
// The two cases differ in whether a reissue is possible at all, not in whether
// the floor moves:
//
//   - Nothing issued yet (Last() == 0) and atLeast <= floor: NO-OP, success.
//     Before the first allocation the floor is only a lower bound, and it is
//     legitimately assembled from several sources — the WAL high-water mark, the
//     audit high-water mark, a peer — which may arrive in any order. Taking the
//     maximum of a set does not care about order, and nothing has been handed
//     out that a lower claim could collide with, so this is harmless.
//
//   - Something has been issued and atLeast <= Last(): ERROR
//     (errors.Is(err, ErrFloorBelowIssued)). The caller is asserting that
//     atLeast is the high-water mark while this process has already handed out a
//     number at or above it. That assertion is FALSE, and it is the same wrong
//     belief that, computed one restart later and fed to Resume, silently
//     reissues ids and breaks invariant 1. Here it is still visible, so it is
//     reported. Note the equality case is included deliberately: a caller whose
//     view merely matches ours has learned nothing new, and treating a stale
//     derivation as success is precisely the silent no-op that hides the
//     off-by-one this check exists to catch. The allocator is left unchanged —
//     the error reports a broken caller, it does not damage the sequence.
//
// # When it may be called
//
// STARTUP ONLY, while the floor is still being assembled and before the first
// Next. It is not a mid-life "confirm this number is burned" call: confirming a
// number this allocator just issued IS the equality case above and ALWAYS
// returns ErrFloorBelowIssued.
//
// Note what the guard does and does not buy. It fires only once something has
// been issued, so during the window where the floor is actually derived —
// startup, Last() == 0 — every value is accepted, including one far too low.
// RaiseFloor is therefore a check on a caller that keeps computing floors after
// it has started serving; it is NOT a defence against a wrong initial floor,
// and nothing in this package is. That remains the caller's proof obligation.
//
// The returned error is the entire mitigation, and a bare `s.RaiseFloor(x)` is
// go vet-clean — so treat a non-nil return as FATAL at startup rather than
// logging past it.
//
// Raising to math.MaxUint64 succeeds while nothing has been issued, and leaves
// the allocator exhausted: the next Next returns ErrSequenceExhausted rather
// than wrapping. Once math.MaxUint64 has itself been ISSUED it is the equality
// case, and raising to it returns ErrFloorBelowIssued like any other.
func (s *Sequence) RaiseFloor(atLeast uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.last != 0 && atLeast <= s.last {
		return fmt.Errorf("%w: asked to raise the floor to %d but %d has already been issued by this allocator; a sequence number is never reused, including across restarts (invariant 1), so this floor is stale — recompute it from the highest sequence EVER written to disk, not from a record count and not from a post-truncation high-water mark",
			ErrFloorBelowIssued, atLeast, s.last)
	}
	if atLeast > s.floor {
		s.floor = atLeast
	}
	return nil
}
