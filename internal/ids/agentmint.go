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
// nothing, fsyncs nothing. A suffix becomes durable because the CALLER writes
// it into a WAL PREPARE frame and fsyncs that frame before enrolment is
// acknowledged. The order the caller owns is:
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
// reuse.
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
// The zero value is not usable; construct with NewNameSuffixes or
// ResumeNameSuffixes.
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
}

// NewNameSuffixes returns an allocator for a FRESH bus: every name has floor 0,
// so the first NextSuffix for any name returns 1. Zero is never issued — see
// AgentID, which rejects it, because a zero suffix is indistinguishable from an
// unset field.
func NewNameSuffixes() *NameSuffixes {
	return &NameSuffixes{
		floor: make(map[string]uint64),
		last:  make(map[string]uint64),
	}
}

// ResumeNameSuffixes returns an allocator that resumes strictly ABOVE the given
// per-name floors, so the first NextSuffix for a name returns its floor+1.
//
// highestOnDisk maps each name to the highest suffix EVER WRITTEN TO DISK for
// it — prepared or committed, still enrolled or long departed. A nil or empty
// map is legal and means "nothing on disk", exactly equivalent to
// NewNameSuffixes; a name absent from the map is treated as floor 0. Read the
// "What a DURABLE implementation must guarantee" section on NameSuffixes before
// choosing these values: deriving them from the committed roster is the obvious
// wiring and is WRONG, and this package cannot detect that it has been handed a
// floor that is too low.
//
// The map is COPIED. The caller keeps ownership of the map it passed and cannot
// mutate this allocator's floors after construction — a floor that could be
// lowered from outside would be a floor with no guarantee at all.
func ResumeNameSuffixes(highestOnDisk map[string]uint64) *NameSuffixes {
	s := NewNameSuffixes()
	for name, n := range highestOnDisk {
		s.floor[name] = n
	}
	return s
}

// NextSuffix allocates the next suffix for name. It is safe for concurrent use,
// including for the same name from several goroutines.
//
// name is not validated (see the type doc): it is used only as a map key, and
// the caller is responsible for having passed it through ValidateAgentName
// first. Distinct names have entirely independent counters, so two names never
// interact.
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

	f := s.floor[name]
	if f == math.MaxUint64 {
		return 0, fmt.Errorf("%w: name %q", ErrSuffixExhausted, name)
	}
	f++
	s.floor[name] = f
	s.last[name] = f
	return f, nil
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
// Exactly as Sequence.RaiseFloor, per name:
//
//   - Nothing issued yet for that name (LastSuffix(name) == 0) and atLeast <=
//     its floor: NO-OP, success. Before the first allocation the floor is only a
//     lower bound, legitimately assembled from several sources — the WAL
//     high-water mark, the audit high-water mark, a peer — which may arrive in
//     any order. Taking the maximum of a set does not care about order, and
//     nothing has been handed out that a lower claim could collide with.
//
//   - Something has been issued for that name and atLeast <= LastSuffix(name):
//     ERROR (errors.Is(err, ErrFloorBelowIssued) — the SAME sentinel
//     Sequence.RaiseFloor returns, because it is the same caller bug about a
//     different counter). The caller is asserting that atLeast is the
//     high-water mark while this process has already handed out a suffix at or
//     above it for that name. That assertion is FALSE, and it is the same wrong
//     belief that, computed one restart later and fed to ResumeNameSuffixes,
//     silently re-mints a live agent id. The equality case is included
//     deliberately: a caller whose view merely matches ours has learned nothing
//     new, and treating a stale derivation as success is precisely the silent
//     no-op that hides the off-by-one this check exists to catch. The allocator
//     is left unchanged — the error reports a broken caller, it does not damage
//     the counter.
//
// # When it may be called
//
// STARTUP ONLY, while the floors are still being assembled and before the first
// NextSuffix. It is not a mid-life "confirm this suffix is burned" call:
// confirming a suffix this allocator just issued IS the equality case above and
// ALWAYS returns ErrFloorBelowIssued.
//
// Note what the guard does and does not buy. It fires only once something has
// been issued for that name, so during the window where the floors are actually
// derived — startup, nothing issued — every value is accepted, including one
// far too low. RaiseFloor is therefore a check on a caller that keeps computing
// floors after it has started serving; it is NOT a defence against wrong
// initial floors, and nothing in this package is. That remains the caller's
// proof obligation (point 7 of the type doc).
//
// The returned error is the entire mitigation, and a bare `s.RaiseFloor(n, x)`
// is go vet-clean — so treat a non-nil return as FATAL at startup rather than
// logging past it.
//
// Raising a name to math.MaxUint64 succeeds while nothing has been issued for
// it, and leaves that name exhausted: its next NextSuffix returns
// ErrSuffixExhausted rather than wrapping.
func (s *NameSuffixes) RaiseFloor(name string, atLeast uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if last := s.last[name]; last != 0 && atLeast <= last {
		return fmt.Errorf("%w: asked to raise the floor for agent name %q to %d but %d has already been issued for that name by this allocator; an agent id is never reused, including across restarts (invariant 1), so this floor is stale — recompute it from the highest suffix EVER written to disk for that name (prepared or committed, enrolled or departed), not from the committed roster",
			ErrFloorBelowIssued, name, atLeast, last)
	}
	if atLeast > s.floor[name] {
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
