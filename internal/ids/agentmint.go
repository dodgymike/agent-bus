package ids

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

// ErrSuffixExhausted is returned by NextSuffix when a name has had
// math.MaxUint64 suffixes issued and there is none left to hand out. It is a
// sentinel so a caller can distinguish "this NAME is finished" from any other
// failure — note it is per-name and distinct from ErrSequenceExhausted, which
// is about message sequence numbers for the whole bus.
var ErrSuffixExhausted = errors.New("ids: agent name suffixes exhausted: math.MaxUint64 has been issued for this name and a suffix is never reused")

// errSuffixFloorUnproven is what NextSuffix returns while the allocator is
// UNSEALED. It exists because ErrFloorUnproven's own message TEXT is written for
// the message-sequence case — it tells the reader to derive "the highest
// sequence number ever written to disk" and warns about reissuing MESSAGE ids —
// and NameSuffixes sits on the LIVE ENROLMENT path, where that advice would send
// an operator to the wrong counter and the wrong derivation entirely. The
// failure here is about per-name AGENT ID suffixes, whose floors are derived
// differently and whose reuse is worse.
//
// The SUFFIX-SPECIFIC guidance therefore comes FIRST, and ErrFloorUnproven is
// appended at the END, explicitly labelled as the underlying sentinel whose
// wording is for the message-sequence case. The order is load-bearing, not
// cosmetic: this string is what an operator sees on a failed ENROLMENT, and a
// rendering that leads with the sequence-number derivation advice sends them to
// the wrong counter before they ever reach the correction.
//
// It still WRAPS ErrFloorUnproven — %w may appear anywhere in the format string,
// so every caller matching errors.Is(err, ids.ErrFloorUnproven) is unaffected by
// the reordering; it stays unexported precisely so callers keep matching the
// shared sentinel rather than this. It is a package-level var, built once: the
// unproven-floor condition is whole-allocator, not per name, so there is nothing
// name-specific to interpolate and nothing to allocate per call.
var errSuffixFloorUnproven = fmt.Errorf("ids: refusing to issue an AGENT ID SUFFIX from an unproven floor: NameSuffixes will not mint because its floors are unsealed, and floors here are PER NAME: each name's floor is the highest suffix EVER WRITTEN TO DISK for that name — prepared or committed, still enrolled or long departed — and NOT the highest suffix in the committed roster, which is the obvious wiring and is WRONG, because the roster misses a suffix burned by a dangling prepare and misses every agent that has since departed; the name SET is as load-bearing as the per-name maxima, so a derivation that cannot complete must return an ERROR, never a partial map; hand the result to ResumeNameSuffixes/RaiseFloor and then call Seal() ONCE; calling Seal() merely to silence this error re-mints agent ids, handing a NEW agent holding a DIFFERENT keypair a previous agent's routing and authorization identity, which nothing downstream can detect. This is the AGENT ID SUFFIX case; the underlying sentinel follows, and ITS wording is written for the MESSAGE-SEQUENCE case — read it for the shared shape of the failure, not for the derivation to perform here: %w", ErrFloorUnproven)

// SuffixAllocator is the narrow seam through which the minter obtains the
// per-name suffix. It exists so AgentIDMinter can be handed a durably-backed
// allocator later (AUTH-3) without the id format knowing anything about the
// store, and so tests can inject a counter that fails or that is already
// exhausted.
//
// An implementation MUST be safe for concurrent use, MUST return a strictly
// increasing value per name, and MUST NOT return 0 — AgentID rejects 0, so a
// zero return is an allocation bug that will surface as a mint failure rather
// than as a bad id.
type SuffixAllocator interface {
	NextSuffix(name string) (uint64, error)
}

// NameSuffixes is a strictly monotonic, concurrency-safe allocator of per-name
// agent id suffixes. For each distinct name it never issues the same number
// twice, and — given a correct resume floor — never issues a number a previous
// run of this bus issued for that name either. The suffix is the last component
// of a fully-qualified agent id ("<bus-id>.<name>-<n>", see AgentID), so a
// reissued suffix is a reissued AGENT ID, which invariant 1 forbids outright.
//
// It is the per-name analogue of Sequence and follows its design exactly; read
// that type's doc as well, because the reasoning about floors carries over
// unchanged and is only made sharper here by what an agent id is used for.
//
// NameSuffixes does NOT validate names. It never interprets a name; it only
// keys on it. The caller must pass a name that has already passed
// ValidateAgentName, and in production AgentIDMinter is the only path that
// reaches this type — it validates first, precisely so no unvalidated byte
// string can ever become a durable counter key.
//
// # What a DURABLE implementation must guarantee
//
// This section is the contract AUTH-3 has to satisfy when it restores these
// counters from the WAL. Every point below is a way to break invariant 1 that
// this type cannot detect for itself.
//
// 1. This allocator holds NO durable state, and NextSuffix is NOT the
// durability point. NameSuffixes is memory only: it writes nothing, reads
// nothing, fsyncs nothing. (DurableNameSuffixes in suffixstore.go is the
// wrapper that does — it persists floor[name] = n and fsyncs it BEFORE it lets
// n out, so with it the durability point moves in front of the allocation. The
// caller's obligations below are unchanged either way: the floors file records
// that a suffix was BURNED, never that an enrolment was accepted.) A suffix
// becomes durable because the CALLER writes it into a WAL PREPARE frame and
// fsyncs that frame before enrolment is acknowledged. The order the caller owns
// is:
//
//	n, err := alloc.NextSuffix(name)   // allocate
//	id, err := AgentID(busID, name, n) // format the fully-qualified id
//	write (name, n, id) into the PREPARE
//	fsync the WAL prepare              // the suffix has reached disk: it is BURNED
//	fsync the audit record
//	fsync the WAL commit               // the enrolment is now accepted history
//	only now return id to the client
//
// Acknowledging before the commit fsync violates invariant 4 no matter what
// this type does, and no amount of care here compensates for it.
//
// 2. A crash BEFORE the prepare fsync may safely reissue a suffix. The rule is
// about suffixes that reached DISK, not suffixes that were merely allocated in
// memory. If a crash lands between NextSuffix and the prepare fsync, the number
// is in no record anywhere: no agent bears that id, no audit line mentions it,
// no client was ever told it. Handing it out again after restart is therefore
// not a reuse — nothing exists to collide with. This is why NameSuffixes needs
// no durable state of its own.
//
// 3. RECOVERY MUST RESTORE, PER NAME, A FLOOR >= THE HIGHEST SUFFIX EVER
// WRITTEN TO DISK FOR THAT NAME — prepared or committed, still enrolled or long
// departed. This is the paragraph to re-read before wiring AUTH-3, because the
// OBVIOUS wiring is wrong. Replay hands you COMMITTED state, so folding the
// committed roster gives you the highest suffix among agents that are CURRENTLY
// ENROLLED — and that is wrong twice over. It misses a suffix burned by a
// dangling prepare (allocated, prepare fsynced, crash before commit: the number
// is on disk and in the audit log, but no committed roster entry mentions it).
// And it misses every agent that has since LEFT (AUTH-4 removes the roster
// entry; the suffix it burned is still in the audit log forever).
//
// The consequence is strictly worse than a duplicate message id. The
// fully-qualified agent id is the ROUTING and AUTHORIZATION subject (invariants
// 2 and 3). Re-minting one hands a NEW agent, holding a DIFFERENT keypair, the
// exact identity a previous agent used: every DM addressed to the old id now
// routes to the new holder, every ACL naming it now names someone else, and the
// append-only audit log attributes two different principals' traffic to a
// single id — destroying it as evidence, which is the whole point of invariant
// 6. "A reused name never collides with a previous holder" is this type's
// entire reason to exist, and a floor derived from live roster state silently
// removes it.
//
// 4. Gaps in a name's suffix sequence are CORRECT — do not compact them. A
// failed, aborted or crashed enrolment burns a suffix, so a name's ids may run
// -1, -2, -5. Closing that gap means reissuing a number that is on disk.
// Consumers must treat suffixes as strictly increasing per name, never as
// dense; anything that requires contiguity (an agent count, a "next free slot")
// is reading the wrong counter.
//
// 5. A name's counter is NEVER deleted or reset — INCLUDING on leave or
// revocation (AUTH-4). When an agent departs, the ROSTER entry goes away; the
// COUNTER entry must not. Deleting it is exactly the same bug as point 3, just
// arrived at deliberately: the next agent to ask for that name would be minted
// "-1" again and would inherit a departed agent's identity. The durable
// counter map is therefore not a cache of the roster and must not be rebuilt
// from it.
//
// 6. Keys are EXACT BYTES. No case folding, no trimming, no Unicode
// normalization at the durable layer. The key must equal, byte for byte, the
// name half of the ids already on disk — see ValidateAgentName for why the
// server refuses uppercase instead of folding it, which is what makes this
// property free.
//
// 7. A name ABSENT from the restored floors is treated as floor 0, so its first
// mint is "-1". That is only sound if "absent" provably means "never written".
// A corrupt-tail truncation may lower a floor (or drop a name entirely) ONLY
// across bytes that provably never completed an fsync — the same rule
// wal.RepairTail operates under, and the same rule the Sequence doc states.
// Across anything else, lowering a floor is forbidden, and a server that cannot
// PROVE its floors are at or above every suffix ever written MUST REFUSE TO
// START rather than guess. A loud, recoverable outage beats silent identity
// reuse. Seal — see "The floors must be SEALED before anything is issued"
// below — is the single line at which that proof obligation is finally
// discharged out loud.
//
// 8. Memory growth is monotonic, and that is deliberate. There is one map entry
// per distinct name EVER enrolled, and it is never reclaimed (see point 5), so
// the map is bounded by the number of distinct names the bus has ever seen and
// not by the number of agents currently connected. Enrolment is unauthenticated
// by design — it is the call that ISSUES the credential (invariant 3) — so
// bounding how many distinct names an unauthenticated caller may create is
// AUTH-1's job (rate limiting, admission control). This type cannot bound it:
// forgetting a name is exactly the reset point 5 forbids.
//
// # The floors must be SEALED before anything is issued
//
// Point 7 above states a proof obligation — "a server that cannot PROVE its
// floors are at or above every suffix ever written MUST REFUSE TO START" — and
// for a while nothing enforced it. Seal is the mechanism that now does. An
// allocator has two states and moves between them once, in one direction:
//
//   - UNSEALED. The floors are still being assembled. RaiseFloor is legal;
//     NextSuffix is not, for ANY name, and returns (0, ErrFloorUnproven)
//     without allocating anything.
//   - SEALED, after Seal. NextSuffix is legal; RaiseFloor is not, and returns
//     ErrFloorSealed without changing anything. A second Seal is likewise an
//     error: the floors are claimed by exactly one code path, and two callers
//     each believing they own the derivation is the bug, not a duplicate no-op.
//
// # The seal is GLOBAL, not per-name — and that is the whole point
//
// It would look natural to seal each name's floor as that name's derivation
// finishes. It is wrong, because of how the floors are actually derived: ONE
// replay pass over the whole log, and that same pass is also what DISCOVERS
// which names exist. A per-name seal cannot express "the derivation is
// complete"; it can only express "this name's floor was set". It would
// therefore have to let a name that was UNKNOWN at seal time mint from an
// unproven floor of 0 — which is exactly the collapse of "proven to be zero"
// into "never proven" that this gate exists to prevent.
//
// So the gate is whole-allocator: while UNSEALED, NO name may issue, whatever
// its floor; after Seal, EVERY name may issue, INCLUDING names absent from the
// map.
//
// # What Seal asserts
//
// Not merely "each floor I set is right". Seal asserts: this map now holds, for
// every name, a floor greater than or equal to every suffix EVER WRITTEN TO
// DISK for that name — INCLUDING the names absent from it, whose floor is
// hereby asserted to be zero. That absent-name half is precisely the claim
// point 7 demands ("'absent' provably means 'never written'"), and Seal is the
// line where it is finally made out loud rather than assumed by omission.
//
// A consequence, not an exception: a name FIRST SEEN AFTER sealing — a
// genuinely new agent enrolling on a running bus — still mints from 1, and that
// is SOUND, because the seal already asserted that names absent from the map
// were never written. Enrolment of new names must and does keep working; the
// gate is about the derivation window, not about the name set.
//
// # The limit of the seal, stated honestly
//
// Seal proves that a CLAIM was made, not that the claim is TRUE. This type
// holds no durable state, so it cannot tell a correct set of floors from one
// folded off the committed roster (point 3) or off a name-count. Sealing wrong
// floors re-mints agent ids just as silently as before. What the seal removes is
// the possibility of never making the claim at all.
//
// There is a failure mode UNIQUE to the per-name case, and it is worth naming
// because it has no analogue on Sequence: a derivation that got every floor it
// SAW right but MISSED A NAME ENTIRELY — a partial scan, a truncated replay, a
// replay that stopped at the first decode error — seals exactly as cleanly as a
// complete one, and every missed name then mints from 1 onto suffixes that are
// already on disk. The NAME SET is as load-bearing as the per-name maxima, and
// nothing in this package can check either. That is why the derivation must
// return an ERROR when it cannot complete, never a partial map.
//
// # Where the durable floors actually come from
//
// Points 3 and 7 state the obligation and leave it with the caller. It now has
// an implementation: DurableNameSuffixes (suffixstore.go) persists each name's
// floor to its own atomically-replaced, fsynced file in the data dir, WRITTEN
// AHEAD of the suffix it authorises, and composes this type for the arithmetic
// and the seal gate. That is the SuffixAllocator production should be built on;
// this type on its own is the memory half of it. Read that type's doc for what
// it does and does not guarantee on a data dir that predates it.
//
// # The zero value
//
// The zero value is not usable; construct with NewNameSuffixes,
// ResumeNameSuffixes, or — in production — OpenNameSuffixes. The seal gate does
// NOT make the zero value usable and does not pretend to: it is UNSEALED, so
// NextSuffix refuses with (0, an error satisfying errors.Is(err,
// ErrFloorUnproven)) before touching a map, and that is all the gate buys.
//
// It no longer PANICS, which it previously did in two places this doc used to
// list as known traps: a zero value that was sealed panicked on the next
// NextSuffix with "assignment to entry in nil map", and RaiseFloor on an
// unsealed zero value panicked for any atLeast >= 1. Both maps are now created
// lazily at the point of first write, so a misconstructed allocator behaves as
// an empty one rather than crashing the process — which matters because
// RaiseFloor's panic landed during STARTUP FLOOR ASSEMBLY, the one window where
// the floors are being proven. That is a robustness fix, not a licence: an
// allocator built as a struct literal has floors nobody derived, so construct
// properly.
type NameSuffixes struct {
	// mu guards both maps. A mutex, not a sync.Map or a per-name lock:
	// NextSuffix sits behind an fsync on the real enrolment path, so lock cost
	// is irrelevant, and a single mutex makes RaiseFloor's read-compare-write
	// trivially atomic against a concurrent NextSuffix for the same name
	// (invariant 8).
	mu sync.Mutex

	// floor holds, per name, the highest suffix that is BURNED: NextSuffix
	// issues floor+1. A name absent from the map has floor 0 — see point 7
	// above for when that is sound.
	floor map[string]uint64

	// last holds, per name, the highest suffix issued BY THIS ALLOCATOR, with
	// absent meaning none. It is tracked separately from floor because a
	// resumed allocator starts with non-zero floors and nothing issued, and
	// RaiseFloor treats those two states differently.
	last map[string]uint64

	// sealed reports whether floor assembly has ended. False until Seal;
	// one-way thereafter, and never cleared. It is GLOBAL, not per name — see
	// "The seal is GLOBAL, not per-name" on the type — and gates NextSuffix
	// (unsealed: refuse, for every name) and RaiseFloor (sealed: refuse, for
	// every name), so the zero value of NameSuffixes is closed, not open.
	sealed bool
}

// NewNameSuffixes returns an allocator for a FRESH bus: every name has floor 0,
// so the first NextSuffix for any name returns 1. Zero is never issued — see
// AgentID, which rejects it, because a zero suffix is indistinguishable from an
// unset field.
//
// It is returned already SEALED, and this is a DELIBERATE DEVIATION from
// Sequence, where NewSequence is born unsealed so that even the empty case has
// to say out loud that it is empty. The deviation exists because NameSuffixes,
// unlike Sequence, has a LIVE PRODUCTION CALLER: cmd/agent-bus/main.go builds
// ids.NewNameSuffixes() on every start and every enrolment mints through it.
// Making this constructor unsealed would refuse EVERY enrolment on a running
// bus, and that caller sits outside this package.
//
// So NewNameSuffixes IS the fresh-bus constructor, and calling it IS the
// empty-disk claim — the claim is carried by the constructor's NAME instead of
// by a separate Seal call. If you are deriving floors from anything on disk,
// this is the wrong constructor: use ResumeNameSuffixes, which is born unsealed,
// or better OpenNameSuffixes, which derives them from the data dir itself.
//
// # This constructor is now the WRONG one for production, and is on its way out
//
// DurableNameSuffixes (suffixstore.go) supersedes it: it persists each name's
// floor ahead of the suffix it authorises, so a restart resumes above every
// suffix ever issued without any derivation at all. Once cmd/agent-bus/main.go
// constructs through OpenNameSuffixes, this constructor should be made
// born-unsealed for parity with NewSequence, or deleted. That wiring is the
// remaining half of the P0 and is tracked separately; until it lands, a
// restarting bus still builds a FRESH counter here and still re-mints agent ids,
// which is the whole reason the fix is only half-shipped.
//
// The residual weakness, stated rather than papered over: a caller whose
// per-name derivation FAILED and which then falls back to NewNameSuffixes()
// mints every name from 1, silently, exactly as before the seal existed. That
// is the one hole the seal does not close on this type, and closing it means
// main.go constructing through the resume path.
//
// The compensating property is real but NARROW, and covers exactly one of the
// two shapes: a sealed-at-birth allocator cannot SILENTLY ABSORB a later floor
// derivation. If a startup path derives floors and calls RaiseFloor on an
// allocator built here, it gets ErrFloorSealed — which a caller that checks its
// errors will see — rather than a floor claim that quietly did nothing. Note the
// limits of that. The loudness is the CALLER's, not this type's: a dropped
// RaiseFloor error is still go vet-clean (see RaiseFloor's doc), so nothing here
// forces the caller to look. And the FALLBACK shape above is not covered at all,
// because a caller that gives up on a failed derivation calls neither RaiseFloor
// nor Seal — there is no call for the error to fire on. That shape stays
// UNCOVERED until main.go constructs through the resume path. Nor is it
// hypothetical: the current production caller performs NO derivation whatsoever
// — cmd/agent-bus/main.go just calls NewNameSuffixes() — so the uncovered case
// is the DEFAULT TODAY, not an edge reached only by a derivation that failed.
func NewNameSuffixes() *NameSuffixes {
	return &NameSuffixes{
		floor:  make(map[string]uint64),
		last:   make(map[string]uint64),
		sealed: true,
	}
}

// ResumeNameSuffixes returns an allocator that resumes strictly ABOVE the given
// per-name floors, so the first NextSuffix for a name returns its floor+1.
//
// highestOnDisk maps each name to the highest suffix EVER WRITTEN TO DISK for
// it — prepared or committed, still enrolled or long departed. A nil or empty
// map is legal and means "nothing on disk"; a name absent from the map is
// treated as floor 0. Read the "What a DURABLE implementation must guarantee"
// section on NameSuffixes before choosing these values: deriving them from the
// committed roster is the obvious wiring and is WRONG, and this package cannot
// detect that it has been handed a floor that is too low.
//
// A nil or empty map is NO LONGER "exactly equivalent to NewNameSuffixes", as
// this doc used to claim. The two now differ in the one way that matters: an
// allocator from here is born UNSEALED and will refuse NextSuffix with
// ErrFloorUnproven until Seal is called, whereas NewNameSuffixes is born SEALED
// because it is the fresh-bus constructor. That is the whole distinction — "I
// derived floor 0 from an empty disk" (seal it and say so) versus "I am a fresh
// bus" (the constructor's name is the claim) — and collapsing them is what let a
// FAILED derivation returning an empty map mint from 1 without anyone noticing.
//
// The map is COPIED. The caller keeps ownership of the map it passed and cannot
// mutate this allocator's floors after construction — a floor that could be
// lowered from outside would be a floor with no guarantee at all.
func ResumeNameSuffixes(highestOnDisk map[string]uint64) *NameSuffixes {
	s := &NameSuffixes{
		floor: make(map[string]uint64, len(highestOnDisk)),
		last:  make(map[string]uint64),
	}
	for name, n := range highestOnDisk {
		s.floor[name] = n
	}
	return s
}

// Seal ends floor assembly for the WHOLE allocator: after it, NextSuffix may
// issue — for every name, including names absent from the floor map — and
// RaiseFloor may not. It is the caller asserting "the floors now held by this
// allocator are, for EVERY name, greater than or equal to every suffix ever
// written to disk for that name, and the names absent from them were never
// written at all". Nothing here can verify that assertion — see "What Seal
// asserts" and "The limit of the seal" on NameSuffixes — so Seal is a promise,
// and the reason it exists as an explicit call is that a promise has to be
// WRITTEN somewhere a reviewer can find it.
//
// Sealing twice returns an error wrapping ErrFloorSealed and changes nothing.
// That is not pedantry about idempotency: the floors are derived and sealed by
// exactly ONE startup path, so a second Seal means two callers each believe they
// own that derivation, and the floors in force are then whichever of them won
// the race — a race over the very values invariant 1 depends on.
//
// Note that an allocator from NewNameSuffixes is born SEALED, so Seal on one of
// those ALWAYS returns ErrFloorSealed. That is not a spurious failure: it means
// the caller derived floors and handed them to the fresh-bus constructor, whose
// name already claimed the disk was empty. Build with ResumeNameSuffixes
// instead.
//
// Safe for concurrent use, though in practice it is called once, from startup,
// before anything else can reach the allocator.
func (s *NameSuffixes) Seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sealed {
		return fmt.Errorf("%w: Seal called on an already-sealed allocator holding %d name floor(s); the floors are derived and sealed by exactly ONE startup path, so a second Seal means two callers each believe they own that derivation and the floors in force are whichever won the race — and note that NewNameSuffixes returns an allocator that is ALREADY sealed (it is the fresh-bus constructor and its name IS the empty-disk claim), so if you are deriving floors, construct with ResumeNameSuffixes",
			ErrFloorSealed, len(s.floor))
	}
	s.sealed = true
	return nil
}

// NextSuffix allocates the next suffix for name. It is safe for concurrent use,
// including for the same name from several goroutines.
//
// name is not validated (see the type doc): it is used only as a map key, and
// the caller is responsible for having passed it through ValidateAgentName
// first. Distinct names have entirely independent counters, so two names never
// interact.
//
// An UNSEALED allocator issues nothing, for ANY name: it returns an error
// satisfying errors.Is(err, ErrFloorUnproven) and leaves both maps untouched, so
// a caller that ignores the error gets a zero suffix — which AgentID rejects —
// rather than a suffix minted from floors nobody proved. What is returned is the
// single package-level errSuffixFloorUnproven, which WRAPS ErrFloorUnproven and
// carries the per-name SUFFIX derivation guidance: the shared sentinel's own text
// is written for message sequence numbers and would send an operator hitting an
// ENROLMENT failure off to derive the wrong counter. The name is still NOT
// interpolated, and that part of the reasoning is unchanged: an unproven floor is
// a whole-allocator condition and not a fact about that name — nothing has been
// discovered about name, and dressing the error up per name would invite a caller
// to retry with a different one (and would allocate on a path that has nothing
// name-specific to say).
//
// This check runs FIRST, ahead of the exhaustion check below, for the reason
// Sequence.Next gives: an allocator with no proven floor cannot honestly claim a
// name is exhausted either. Reporting exhaustion would tell an operator "this
// name is finished" — unrecoverable, an identity permanently unusable — when the
// truth is "your derivation is broken", which is recoverable by fixing startup.
// Returning an error never mutates the allocator.
//
// Because the check precedes every map access, it is also what makes the ZERO
// VALUE fail closed: an unsealed NameSuffixes reached by accident refuses here
// instead of panicking on its nil maps, which is a real improvement on the
// previous behaviour.
//
// Overflow is an ERROR, never a wrap: at math.MaxUint64 it returns 0 and
// ErrSuffixExhausted and that name stays exhausted for the life of the process.
// Wrapping to 0 would reissue every agent id this bus has ever minted for the
// name, which is the single worst outcome available here — far worse than a
// name that can no longer be enrolled. Reaching it requires ~1.8e19 enrolments
// of one name, so in practice this branch exists to make the wrap impossible,
// not because it is expected.
func (s *NameSuffixes) NextSuffix(name string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextLocked(name, true)
}

// peekNext reports the suffix NextSuffix would issue for name WITHOUT issuing
// it: it runs the same seal and exhaustion gates, in the same order, and mutates
// nothing on any path.
//
// It exists for DurableNameSuffixes, which must know the number BEFORE it can
// persist the floor that authorises it — the write-ahead ordering that type's
// doc describes. It is unexported on purpose: a peeked number is only meaningful
// while the caller holds a lock that keeps anyone else from taking it, and
// DurableNameSuffixes owns its NameSuffixes exclusively and holds its own mutex
// across the peek, the fsync and the commit. Exporting it would invite a
// peek-then-issue race whose symptom is two agents on one id.
func (s *NameSuffixes) peekNext(name string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextLocked(name, false)
}

// nextLocked is the one implementation of "what comes next for this name". With
// commit true it issues the number; with commit false it only reports it. The
// caller must hold s.mu.
//
// The gates run in this order for the reason NextSuffix's doc gives: an
// allocator with no proven floor cannot honestly claim a name is exhausted
// either. Neither refusal mutates anything.
func (s *NameSuffixes) nextLocked(name string, commit bool) (uint64, error) {
	if !s.sealed {
		return 0, errSuffixFloorUnproven
	}
	f := s.floor[name]
	if f == math.MaxUint64 {
		return 0, fmt.Errorf("%w: name %q", ErrSuffixExhausted, name)
	}
	f++
	if !commit {
		return f, nil
	}
	// Lazily create the maps. Reading a nil map is fine — it yields 0, the
	// correct floor for an unknown name — but WRITING to one panics, and a
	// NameSuffixes reached as a zero value is sealed only if someone sealed it,
	// which is legal today. Panicking on the live enrolment path because a
	// counter was constructed with a struct literal instead of a constructor is
	// a denial of service dressed up as a programming error; failing safe here
	// costs one nil check per allocation.
	if s.floor == nil {
		s.floor = make(map[string]uint64)
	}
	if s.last == nil {
		s.last = make(map[string]uint64)
	}
	s.floor[name] = f
	s.last[name] = f
	return f, nil
}

// isSealed reports whether floor assembly has ended. Unexported: the seal is not
// a thing callers branch on — they call Seal once and check the error — and an
// exported predicate would invite exactly the check-then-act race the one-way
// state machine exists to remove. DurableNameSuffixes uses it to decide whether
// a Seal needs a disk write before it delegates.
func (s *NameSuffixes) isSealed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealed
}

// floorSnapshot returns a copy of the per-name floors. Unexported for the same
// reason as peekNext: it is a durable-store implementation detail, and a floor
// map handed out mid-life would tempt a caller into recomputing floors while the
// allocator is serving.
func (s *NameSuffixes) floorSnapshot() map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]uint64, len(s.floor))
	for name, n := range s.floor {
		out[name] = n
	}
	return out
}

// LastSuffix reports the highest suffix this allocator has ISSUED for name, or
// 0 if it has issued none. A resumed allocator reports 0 for a name until its
// first NextSuffix for that name, even though the name's floor is non-zero:
// LastSuffix answers "what did I hand out", not "what is burned".
func (s *NameSuffixes) LastSuffix(name string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[name]
}

// RaiseFloor raises name's floor to atLeast, so that every subsequent
// NextSuffix for that name returns a number strictly greater than atLeast. It
// NEVER lowers a floor, and it touches only the one name.
//
// # Why this exists
//
// AUTH-3 restores the floors by folding a REPLAY STREAM of enrolment records,
// which arrives one record at a time and in whatever order recovery produces.
// Taking a per-name maximum incrementally — RaiseFloor(name, n) for every
// record seen — is order-independent and cannot be got wrong by arithmetic. The
// alternative, "build the map yourself and then ResumeNameSuffixes", puts the
// caller in charge of deriving each maximum, which is the ONE way this package
// can be made to break invariant 1 (see point 3 of the type doc). Prefer this
// call; it exists so the caller does not have to be careful.
//
// # Where the line between "no-op" and "error" sits
//
// Exactly as Sequence.RaiseFloor, and on the same predicate — the SEAL, which
// is global here, not per name:
//
//   - UNSEALED and atLeast <= the name's floor: NO-OP, success. During assembly
//     a floor is only a lower bound, legitimately assembled from several
//     sources — the WAL high-water mark, the audit high-water mark, a peer —
//     which may arrive in any order. Taking the maximum of a set does not care
//     about order, and nothing has been handed out that a lower claim could
//     collide with, because an unsealed allocator has issued nothing for any
//     name.
//
//     A PEER-supplied claim is UNTRUSTED INPUT (invariant 1: a client-supplied
//     id is input to be validated, never an identity to be trusted). RaiseFloor
//     applies NO upper bound, so a peer claiming math.MaxUint64 for a name
//     exhausts that name's id space at once — and permanently, once a near-max
//     suffix reaches disk and the next restart derives its floor from it. That
//     is a denial of one agent NAME, forever, from a remote party. Validate and
//     BOUND a peer's claim BEFORE it reaches RaiseFloor.
//
//   - SEALED: ERROR (errors.Is(err, ErrFloorSealed)), for EVERY name and EVERY
//     atLeast — assembly is over, and the sentinel does not depend on how
//     atLeast relates to that name's floor or to LastSuffix(name). That
//     uniformity is why the sealed check runs FIRST, ahead of the one below.
//
//   - Something has been issued for that name and atLeast <= LastSuffix(name):
//     ERROR (errors.Is(err, ErrFloorBelowIssued) — the SAME sentinel
//     Sequence.RaiseFloor returns, because it is the same caller bug about a
//     different counter). Like Sequence's, this case can no longer be REACHED,
//     because the seal pre-empts it: last[name] != 0 requires a successful
//     NextSuffix, NextSuffix requires the allocator to be sealed, and the sealed
//     check above returns ErrFloorSealed before this one is ever consulted. With
//     Sequence's branch equally unreachable, ErrFloorBelowIssued now has no
//     reachable producer anywhere in this package's exported API.
//
//     The branch is kept as defence-in-depth, exactly as Sequence keeps its
//     own: the whole one-way state machine rests on a single bool, and if that
//     ever changes this is the check that stops a stale floor landing on top of
//     issued suffixes. The reasoning is retained because it is what the check
//     would catch. The caller would be asserting that atLeast is the high-water
//     mark while this process has already handed out a suffix at or above it for
//     that name. That assertion is FALSE, and it is the same wrong belief that,
//     computed one restart later and fed to ResumeNameSuffixes, silently
//     re-mints a live agent id. The equality case is included deliberately: a
//     caller whose view merely matches ours has learned nothing new, and
//     treating a stale derivation as success is precisely the silent no-op that
//     hides the off-by-one this check exists to catch. The allocator is left
//     unchanged — the error reports a broken caller, it does not damage the
//     counter.
//
// # When it may be called
//
// While UNSEALED, and only while unsealed. That window is the assembly phase:
// startup, before Seal, before anything is issued for any name. Once Seal has
// run, RaiseFloor returns an error wrapping ErrFloorSealed and changes nothing —
// checked FIRST, because the seal is the stronger statement: "floor assembly is
// over" holds whether or not this allocator has issued anything for the name in
// question, and a sealed allocator that has issued nothing for that name would
// slip straight past the last != 0 guard.
//
// Note that an allocator from NewNameSuffixes is born SEALED, so RaiseFloor on
// one of those always fails. That is deliberate and is the compensating property
// of that constructor: a derivation cannot be silently absorbed by the fresh-bus
// allocator. If you are deriving floors from replay, construct with
// ResumeNameSuffixes (born unsealed), not NewNameSuffixes (born sealed).
//
// RaiseFloor is not a mid-life "confirm this suffix is burned" call. After the
// seal it is refused outright; before the seal, confirming a suffix this
// allocator issued cannot arise, because an unsealed allocator has issued none.
//
// Be precise about what the seal did and did not buy. The old guard here — "it
// fires only once something has been issued for that name" — was INERT during
// exactly the window in which floors are derived: at startup nothing has been
// issued, so every value was accepted, including one far too low. That guard is
// no longer the mitigation; the SEAL is. The error is still not load-bearing:
// a bare `s.RaiseFloor(n, x)` remains go vet-clean (the unusedresult analyzer
// does not flag discarded errors, and its funcs flag does not change that), so
// a caller can still drop it. What changed is that dropping it no longer lets an
// unproven floor SERVE, because nothing issues for any name until Seal, an
// affirmative call the caller cannot forget without every NextSuffix refusing.
//
// Still treat a non-nil return as FATAL at startup rather than logging past it,
// and note precisely what one now means: within its legal window — unsealed —
// RaiseFloor can no longer return non-nil at all on this type either. A
// rejection is therefore always a startup-SEQUENCING defect (called after Seal,
// or called on a NewNameSuffixes allocator), and shipping past it means the
// floor claim it carried was silently dropped.
//
// What remains undefended: the seal makes the floor claims explicit, greppable
// and reviewable, but the allocator still cannot tell whether the sealed floors
// are CORRECT, nor whether the name SET is complete. Proving them is, as it
// always was, the caller's obligation (point 7 of the type doc).
//
// Raising a name to math.MaxUint64 succeeds while the allocator is UNSEALED, and
// leaves that name exhausted: its next NextSuffix — which, like any NextSuffix,
// requires a Seal in between to get that far — returns ErrSuffixExhausted rather
// than wrapping. Once math.MaxUint64 has itself been ISSUED for a name, raising
// that name to it returns ErrFloorSealed, NOT ErrFloorBelowIssued: for it to
// have been issued the allocator must already be sealed, and the seal check runs
// first.
func (s *NameSuffixes) RaiseFloor(name string, atLeast uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sealed {
		return fmt.Errorf("%w: asked to raise the floor for agent name %q to %d after the floors were sealed (%d name floor(s) held); the floors are assembled once, before anything is issued, so a later claim about a name's high-water mark is either a stale derivation or a caller still recomputing floors while serving — if you are deriving floors from replay, construct with ResumeNameSuffixes (born unsealed), NOT NewNameSuffixes (born sealed, because it is the fresh-bus constructor); if a sealed floor is genuinely wrong the bus must be restarted with a correctly derived one, never patched at runtime",
			ErrFloorSealed, name, atLeast, len(s.floor))
	}
	// UNREACHABLE through the exported API, and kept as defence-in-depth: see
	// the doc above. last[name] != 0 requires a successful NextSuffix, which
	// requires s.sealed, and the sealed check above has already returned.
	if last := s.last[name]; last != 0 && atLeast <= last {
		return fmt.Errorf("%w: asked to raise the floor for agent name %q to %d but %d has already been issued for that name by this allocator; an agent id is never reused, including across restarts (invariant 1), so this floor is stale — recompute it from the highest suffix EVER written to disk for that name (prepared or committed, enrolled or departed), not from the committed roster",
			ErrFloorBelowIssued, name, atLeast, last)
	}
	if atLeast > s.floor[name] {
		// Lazily create the map — see nextLocked for why. This is the call that
		// used to PANIC on a zero-value NameSuffixes for any atLeast >= 1, which
		// the type doc named as a known trap; a floor-assembly path that dies
		// with "assignment to entry in nil map" is the worst possible way to
		// learn a counter was built with a struct literal, because it happens at
		// startup, in the one window where the floors are being proven.
		if s.floor == nil {
			s.floor = make(map[string]uint64)
		}
		s.floor[name] = atLeast
	}
	return nil
}

// AgentIDMinter is the one production path from a client's requested name to a
// fully-qualified agent id. It binds this bus's id (invariant 2) to a suffix
// allocator, validates the untrusted half, and formats the result.
//
// It is safe for concurrent use if its allocator is; NameSuffixes is.
//
// The zero value is not usable; construct with NewAgentIDMinter.
type AgentIDMinter struct {
	busID string
	alloc SuffixAllocator
}

// NewAgentIDMinter returns a minter for busID drawing suffixes from alloc.
//
// busID is validated: it comes from LoadOrCreateBusID, and a minter built on a
// malformed bus id would fail on every Mint instead of at start-up.
//
// A nil alloc is an ERROR, not a silently-defaulted fresh counter. A minter
// that invented its own allocator would start every name at 1 with nothing on
// disk backing it, and would therefore re-mint ids that already exist the
// moment it is used on a bus with history — the exact failure invariant 1
// forbids, arrived at by a convenience default.
func NewAgentIDMinter(busID string, alloc SuffixAllocator) (*AgentIDMinter, error) {
	if err := ValidateBusID(busID); err != nil {
		return nil, fmt.Errorf("creating agent id minter: %w", err)
	}
	if alloc == nil {
		return nil, errors.New("creating agent id minter: suffix allocator must not be nil; a minter never invents its own counter, because one that started every name at 1 would re-mint agent ids that are already on disk (invariant 1)")
	}
	return &AgentIDMinter{busID: busID, alloc: alloc}, nil
}

// BusID reports the bus id this minter qualifies every agent id with.
func (m *AgentIDMinter) BusID() string { return m.busID }

// Mint allocates a suffix for requestedName and returns the fully-qualified
// agent id "<bus-id>.<requestedName>-<n>".
//
// requestedName is UNTRUSTED CLIENT INPUT — it is the one part of an agent id
// the client gets a say in — and is validated before it is allowed anywhere
// near the allocator, so no unvalidated byte string can become a counter key
// or reach the durable roster. The client never chooses the suffix or the bus
// prefix: those are minted here (invariant 1).
//
// The returned id is NOT durable. The caller must write it through the
// two-phase path and fsync the commit before acknowledging it to the client —
// see point 1 of the NameSuffixes doc, which holds whatever allocator is
// plugged in.
func (m *AgentIDMinter) Mint(requestedName string) (string, error) {
	if err := ValidateAgentName(requestedName); err != nil {
		return "", fmt.Errorf("minting agent id on bus %q: %w", m.busID, err)
	}

	n, err := m.alloc.NextSuffix(requestedName)
	if err != nil {
		return "", fmt.Errorf("minting agent id for %q on bus %q: %w", requestedName, m.busID, err)
	}

	id, err := AgentID(m.busID, requestedName, n)
	if err != nil {
		return "", fmt.Errorf("minting agent id for %q on bus %q: %w", requestedName, m.busID, err)
	}

	// Defensive re-validation, mirroring GenerateBusID: parse the id we just
	// built and confirm it round-trips to the same three components. It should
	// never fail — AgentID validated every input and formatted the suffix with
	// strconv — but an agent id that fails its own invariant must never reach a
	// caller, because it is the routing and authorization subject (invariants 2
	// and 3) and the suffix behind it is about to be burned on disk.
	gotBus, gotName, gotN, perr := ParseAgentID(id)
	if perr != nil {
		return "", fmt.Errorf("minted agent id %q failed its own validation: %w", id, perr)
	}
	if gotBus != m.busID || gotName != requestedName || gotN != n {
		return "", fmt.Errorf("minted agent id %q does not round-trip: parsed as bus %q, name %q, suffix %d, but was built from bus %q, name %q, suffix %d", id, gotBus, gotName, gotN, m.busID, requestedName, n)
	}
	return id, nil
}
