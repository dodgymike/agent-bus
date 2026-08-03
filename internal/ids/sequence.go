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
//
// This sentinel now has NO reachable producer through the exported API of
// EITHER allocator. On both Sequence and NameSuffixes the seal gate pre-empts
// it: issuing anything requires a Seal, and a sealed RaiseFloor returns
// ErrFloorSealed first (see Sequence.RaiseFloor and NameSuffixes.RaiseFloor).
//
// Both branches survive as defence-in-depth, and the sentinel is kept rather
// than deleted: the whole one-way state machine rests on a single bool staying
// one-way, and if that ever changes, these are the checks that stop a stale
// floor landing on top of numbers already issued. The reasoning recorded on
// each branch is retained for the same reason — it is what the check would
// catch.
var ErrFloorBelowIssued = errors.New("ids: refusing to set a sequence floor at or below a number already issued")

// ErrFloorUnproven is returned by Sequence.Next, and by
// NameSuffixes.NextSuffix, while that allocator is still UNSEALED — nobody has
// yet asserted that its floor is the true high-water mark. It is the fail-closed
// half of invariant 1: an allocator that cannot be shown a proven floor refuses
// to mint rather than minting from a guess.
//
// The MESSAGE TEXT below is written for the message-sequence case (one floor,
// derived from the highest sequence ever written). NameSuffixes.NextSuffix
// therefore does not return this value bare: it returns an error WRAPPING it
// that carries the per-name suffix guidance instead, since floors there are per
// name and derived from a different thing. Match on this sentinel with
// errors.Is; do not read its text as advice for the suffix case.
//
// The fix is to PROVE the floor, then Seal. It is NEVER to call Seal until the
// error stops appearing.
var ErrFloorUnproven = errors.New("ids: refusing to issue a sequence number from an unproven floor: derive the floor from the highest sequence number EVER WRITTEN TO DISK — every prepare, committed, aborted and dangling alike, NOT a record count and NOT the highest COMMITTED sequence — hand it to Resume/RaiseFloor and then call Seal(); calling Seal() merely to silence this error resumes below the high-water mark and silently reissues message ids, which nothing downstream can detect")

// ErrFloorSealed is returned by Sequence.RaiseFloor and by
// NameSuffixes.RaiseFloor once that allocator has been sealed, and by a second
// call to either Seal. Sealing is one-way and happens exactly once per
// allocator: after it, floor assembly is over, and any further claim about the
// high-water mark is by definition too late.
var ErrFloorSealed = errors.New("ids: the sequence floor is sealed: floor assembly ended at Seal() and the floor can no longer be changed")

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
//	if err := seq.Seal(); err != nil { … } // ONCE, at startup: "floor proven"
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
// # The floor must be SEALED before anything is issued
//
// The paragraph above — "a caller that cannot PROVE its floor … MUST refuse to
// start rather than guess" — used to be advice with nothing behind it. Seal is
// the mechanism that now enforces it. An allocator has two states and moves
// between them once, in one direction:
//
//   - UNSEALED. The floor is still being assembled. RaiseFloor is legal; Next is
//     not, and returns (0, ErrFloorUnproven) without allocating anything.
//   - SEALED, after Seal. Next is legal; RaiseFloor is not, and returns
//     ErrFloorSealed without changing anything. A second Seal is likewise an
//     error: the floor is claimed by exactly one code path, and two callers each
//     believing they own the derivation is the bug, not a duplicate no-op.
//
// BOTH constructors are born unsealed, including NewSequence. That is the point.
// A fresh bus and a bus whose recovery scan returned 0 because it FAILED are
// both "floor 0", and the type cannot tell them apart — so neither is allowed to
// issue until somebody says out loud that the floor is proven. Requiring the
// seal even on the empty case is what forces the caller to SAY which of the two
// it has, at a line a reviewer can find, instead of letting "floor 0 because
// there is nothing on disk" quietly become the default that absorbs "floor 0
// because we could not read the disk". It does not, and cannot, tell them apart
// on the caller's behalf — a failed scan that returns 0 seals exactly as
// cleanly as an empty one, which is why the derivation must return an ERROR
// rather than 0 when it fails.
//
// The seal also leaves the assembly window intact: the floor may still be raised
// from several sources — the WAL high-water mark, the audit high-water mark, a
// peer — arriving in any order, because sealing, not the first Next, is what
// closes that window. A PEER-supplied claim is untrusted input (invariant 1: a
// client-supplied id is input to be validated, never an identity to be trusted).
// RaiseFloor applies no upper bound, so a peer claiming math.MaxUint64 exhausts
// this bus's id space at once — and permanently, once a near-max number reaches
// disk and the next restart derives its floor from it. Validate and bound a
// peer's claim BEFORE it reaches RaiseFloor.
//
// The value of Seal is that it is a single, visible, greppable line at the exact
// point where a reviewer must check the derivation. Be clear about its limit,
// though: it proves only that the caller made a CLAIM, not that the claim is
// TRUE. This package holds no durable state, so it still cannot tell a correct
// floor from one computed off a record count or off committed history. Sealing a
// wrong floor reissues ids just as silently as before. What the seal removes is
// the possibility of never making the claim at all.
//
// The zero value is not usable; construct with NewSequence or Resume. It does at
// least fail closed rather than mint: an unsealed Sequence reached by accident
// refuses Next instead of handing out 1 from a floor nobody derived.
type Sequence struct {
	// mu guards every field. A mutex, not a CAS loop: Next sits behind an
	// fsync on the real write path, so lock cost is irrelevant, and a mutex
	// makes RaiseFloor's read-compare-write trivially atomic against a
	// concurrent Next — which a CAS loop over two words would not be
	// (invariant 8).
	mu sync.Mutex

	// floor is the highest number that is BURNED: Next issues floor+1.
	floor uint64

	// last is the highest number issued BY THIS ALLOCATOR, 0 if none. It is
	// tracked separately from floor because a resumed allocator starts with a
	// non-zero floor and nothing issued, and Last() answers "what did I hand
	// out", not "what is burned". (It also feeds RaiseFloor's
	// ErrFloorBelowIssued branch, but the seal pre-empts that branch — see
	// RaiseFloor — so Last()'s own contract is what keeps this field.)
	last uint64

	// sealed reports whether floor assembly has ended. False until Seal;
	// one-way thereafter. It gates Next (unsealed: refuse) and RaiseFloor
	// (sealed: refuse), so the zero value of Sequence is closed, not open.
	sealed bool
}

// NewSequence returns an allocator for a FRESH bus: floor 0, so the first
// Next returns 1. Zero is never issued — see MessageID, which rejects it,
// because a zero sequence is indistinguishable from an unset field.
//
// The allocator is returned UNSEALED and will refuse Next with
// ErrFloorUnproven until Seal is called. That applies here too, even though the
// floor is trivially 0: "fresh bus" is a claim about the disk, and the caller
// makes it by sealing.
func NewSequence() *Sequence { return &Sequence{} }

// Resume returns an allocator that resumes strictly ABOVE highestOnDisk, so
// the first Next returns highestOnDisk+1.
//
// highestOnDisk is the highest sequence number EVER WRITTEN TO DISK — prepared
// or committed, delivered or discarded. 0 means "nothing on disk" and is
// exactly equivalent to NewSequence. Read the Sequence doc before choosing this
// value: getting it too low is the one way this package can be made to break
// invariant 1, and it cannot detect that it has.
//
// The allocator is returned UNSEALED: the floor may still be raised from other
// sources, and Next refuses with ErrFloorUnproven until Seal is called.
func Resume(highestOnDisk uint64) *Sequence { return &Sequence{floor: highestOnDisk} }

// Seal ends floor assembly: after it, Next may issue and RaiseFloor may not.
// It is the caller asserting "the floor now held by this allocator is greater
// than or equal to every sequence number ever written to disk". Nothing here can
// verify that assertion — see the Sequence doc — so Seal is a promise, and the
// reason it exists as an explicit call is that a promise has to be WRITTEN
// somewhere a reviewer can find it.
//
// Sealing twice returns an error wrapping ErrFloorSealed and changes nothing.
// That is not pedantry about idempotency: the floor is derived by exactly one
// startup path, so a second Seal means two paths each think they own the
// derivation, and the floor in force is then whichever of them ran first — a
// race over the one value invariant 1 depends on.
//
// Safe for concurrent use, though in practice it is called once, from startup,
// before anything else can reach the allocator.
func (s *Sequence) Seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sealed {
		return fmt.Errorf("%w: Seal called a second time (floor is %d); the floor is derived and sealed by exactly ONE startup path, so a second seal means two callers each believe they own that derivation and the floor in force is whichever won the race",
			ErrFloorSealed, s.floor)
	}
	s.sealed = true
	return nil
}

// Next allocates the next sequence number. It is safe for concurrent use.
//
// An UNSEALED allocator issues nothing: it returns (0, ErrFloorUnproven) and
// leaves floor and last untouched, so a caller that ignores the error gets a
// zero sequence — which MessageID rejects — rather than a number minted from a
// floor nobody proved. This check runs FIRST, ahead of the exhaustion check
// below: an allocator with no proven floor cannot honestly claim to be exhausted
// either, since "exhausted" is a statement about how far the floor has been
// carried, and refusing for want of a proven floor is the more fundamental
// answer. Returning an error never mutates the allocator.
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
	if !s.sealed {
		return 0, ErrFloorUnproven
	}
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
//   - UNSEALED and atLeast <= floor: NO-OP, success. (The predicate is the
//     SEAL, not Last() == 0: on a Sequence the two now coincide — nothing can
//     have been issued before the seal — but it is the seal that decides, and
//     reading this as "nothing issued yet" is how the next reader gets it
//     wrong.) During assembly the floor is only a lower bound, and it is
//     legitimately assembled from several sources — the WAL high-water mark, the
//     audit high-water mark, a peer — which may arrive in any order. Taking the
//     maximum of a set does not care about order, and nothing has been handed
//     out that a lower claim could collide with, so this is harmless.
//
//   - SEALED: ERROR (errors.Is(err, ErrFloorSealed)), for EVERY atLeast —
//     assembly is over, and the sentinel does not depend on how atLeast relates
//     to the floor or to Last(). That uniformity is the reason the sealed check
//     runs first rather than after the one below.
//
//   - Something has been issued and atLeast <= Last(): ERROR
//     (errors.Is(err, ErrFloorBelowIssued)) — but on a Sequence this case can no
//     longer be reached, because the seal pre-empts it. Last() != 0 requires a
//     successful Next, Next requires the allocator to be sealed, and the sealed
//     check above returns ErrFloorSealed before this one is ever consulted. The
//     branch is kept as defence-in-depth: the whole one-way state machine rests
//     on a single bool, and if that ever changes this is the check that stops a
//     stale floor from landing on top of issued numbers.
//
//     NameSuffixes.RaiseFloor now carries the same branch, under the same seal,
//     equally unreachable and equally kept — so ErrFloorBelowIssued has no
//     reachable producer anywhere in this package's exported API.
//
//     The reasoning is still worth reading, because it is exactly what these
//     checks would catch if the one-way seal ever stopped being one-way: a
//     caller asserting that atLeast is the high-water mark while a number at or
//     above it has already been handed out is asserting something FALSE, and it
//     is the same wrong belief that, computed one restart later and fed to
//     Resume, silently reissues ids and breaks invariant 1. The equality case is
//     included deliberately: a caller whose
//     view merely matches ours has learned nothing new, and treating a stale
//     derivation as success is precisely the silent no-op that hides the
//     off-by-one this check exists to catch. The allocator is left unchanged —
//     the error reports a broken caller, it does not damage the sequence.
//
// # When it may be called
//
// While UNSEALED, and only while unsealed. That window is the assembly phase:
// startup, before Seal, before anything is issued. Once Seal has run, RaiseFloor
// returns an error wrapping ErrFloorSealed and changes nothing — checked FIRST,
// ahead of the ErrFloorBelowIssued case below, because the seal is the stronger
// statement. "Floor assembly is over" holds whether or not this allocator has
// issued anything yet, and a sealed allocator that has issued nothing would slip
// straight past the last != 0 guard.
//
// RaiseFloor is not a mid-life "confirm this number is burned" call. After the
// seal it is refused outright; before the seal, confirming a number this
// allocator issued cannot arise, because an unsealed allocator has issued none.
//
// Be precise about what the seal did and did not buy. It did NOT make the
// returned error load-bearing — a bare `s.RaiseFloor(x)` is still go vet-clean
// (the unusedresult analyzer does not flag discarded errors), so a caller can
// still drop it on the floor. What changed is that dropping it no longer matters
// as much: the floor is now gated by Seal, an affirmative call the caller cannot
// forget without Next refusing to issue at all. The mitigation is the gate, not
// the error. Still treat a non-nil return as FATAL at startup rather than
// logging past it. Note precisely what such a rejection now means on a
// Sequence: it can ONLY be ErrFloorSealed, i.e. "called after assembly closed",
// which may be a pure ordering bug rather than a disagreement about the
// high-water mark. Within its legal window — unsealed — RaiseFloor can no longer
// return non-nil at all. A rejection is therefore always a startup-sequencing
// defect, and shipping past it means the floor claim it carried was silently
// dropped.
//
// What remains undefended: the seal makes the floor claim explicit, greppable
// and reviewable, but the allocator still cannot tell whether the sealed floor
// is CORRECT. It holds no durable state, so a floor computed from a record count
// or from committed history seals just as cleanly as a right one. Proving the
// floor is, as it always was, the caller's obligation.
//
// Raising to math.MaxUint64 succeeds while the allocator is UNSEALED, and leaves
// the allocator exhausted: the next Next — which, like any Next, requires a Seal
// in between to get that far — returns ErrSequenceExhausted rather than
// wrapping. Once math.MaxUint64 has itself been ISSUED, raising to it returns
// ErrFloorSealed, NOT ErrFloorBelowIssued: for MaxUint64 to have been issued the
// allocator must already be sealed, and the seal check runs first.
func (s *Sequence) RaiseFloor(atLeast uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sealed {
		return fmt.Errorf("%w: asked to raise the floor to %d after Seal (floor is %d); the floor is assembled once, before anything is issued, so a later claim about the high-water mark is either a stale derivation or a caller still recomputing floors while serving — if the sealed floor is genuinely wrong the bus must be restarted with a correctly derived one, never patched at runtime",
			ErrFloorSealed, atLeast, s.floor)
	}
	if s.last != 0 && atLeast <= s.last {
		return fmt.Errorf("%w: asked to raise the floor to %d but %d has already been issued by this allocator; a sequence number is never reused, including across restarts (invariant 1), so this floor is stale — recompute it from the highest sequence EVER written to disk, not from a record count and not from a post-truncation high-water mark",
			ErrFloorBelowIssued, atLeast, s.last)
	}
	if atLeast > s.floor {
		s.floor = atLeast
	}
	return nil
}
