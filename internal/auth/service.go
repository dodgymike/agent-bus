package auth

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
)

// MaxIdempotencyKeyLen bounds a client-supplied idempotency key. The key is
// echoed into the server log, so it is bounded and character-restricted for the
// same reason httpapi.SanitizeRequestID bounds a request id.
const MaxIdempotencyKeyLen = 128

// Defaults for the Options bounds. Every one of them is a FAIL-CLOSED limit on
// an UNAUTHENTICATED surface: enrolment and session establishment are the calls
// that ISSUE the credential (invariant 3), so they cannot be protected by one,
// and admission control is all that stands between an anonymous caller and
// unbounded server memory.
//
// The numbers are deliberately generous for a bus that federates tens to
// thousands of agents and deliberately finite. They are configurable through
// Options; a value of 0 means "use the default", and there is no "unlimited".
const (
	// DefaultMaxRosterEntries bounds how many agents may be enrolled.
	DefaultMaxRosterEntries = 4096

	// DefaultMaxIdempotencyEntries bounds the remembered idempotency keys.
	DefaultMaxIdempotencyEntries = 16384

	// DefaultMaxSessions bounds the session table, pending and active together.
	DefaultMaxSessions = 16384
)

// Options configures NewService.
type Options struct {
	// Minter mints the fully-qualified agent id for an enrolment. It is
	// REQUIRED and has NO DEFAULT.
	//
	// A defaulted minter would be built on a fresh ids.NameSuffixes, which
	// starts every name at suffix 1 with nothing on disk backing it, and would
	// therefore re-mint agent ids that already exist the moment it ran on a bus
	// with history — the exact failure invariant 1 forbids, arrived at by a
	// convenience default. ids.NewAgentIDMinter rejects a nil allocator for
	// precisely this reason and this is the same rejection one layer up.
	Minter *ids.AgentIDMinter

	// Roster records enrolled agents. Defaults to NewMemoryRoster(), which is
	// NOT durable — see the Roster doc.
	Roster Roster

	// Now is the clock. Defaults to time.Now. Server-side expiry is
	// authoritative and is always measured against this clock.
	Now func() time.Time

	// MaxRosterEntries bounds enrolments; 0 means DefaultMaxRosterEntries.
	MaxRosterEntries int

	// MaxIdempotencyEntries bounds remembered idempotency keys; 0 means
	// DefaultMaxIdempotencyEntries.
	MaxIdempotencyEntries int

	// MaxSessions bounds the session table; 0 means DefaultMaxSessions.
	//
	// It is, with ChallengeTTL, the ONLY bound on the session table. There is
	// deliberately no per-agent cap to go with it — see BeginSession.
	MaxSessions int
}

// Service is the enrolment and session authority. It is safe for concurrent
// use.
//
// The zero value is not usable; construct with NewService.
type Service struct {
	minter *ids.AgentIDMinter
	roster Roster
	now    func() time.Time

	maxRosterEntries      int
	maxIdempotencyEntries int
	maxSessions           int

	// enrolMu guards idem and serialises the whole of Enrol. sessMu guards
	// sessions.
	//
	// # Why these are two mutexes and not one
	//
	// They are split to keep AUTH-3's fsync OFF the AUTH-2 hot path. Enrol holds
	// its lock across the capacity check, the mint and the roster write so that
	// admission control cannot be raced past by N concurrent callers all
	// observing Len() == max-1 — and once AUTH-3 makes that roster write durable,
	// the lock is held across an fsync, for milliseconds. Authenticate is a
	// single map lookup that AUTH-2 will make on EVERY authenticated request. One
	// shared mutex would propagate the enrolment fsync straight into it: measured
	// on a roster whose Put slept 300ms, a concurrent Authenticate took 250ms.
	// The session lock is separately worth keeping narrow, since it also covers
	// ed25519.Verify and the O(n) sweep/count/oldest scans.
	//
	// # Lock order
	//
	// NO path takes both. Each is one-deep over the independently synchronised
	// Roster and *ids.AgentIDMinter (enrolMu -> Roster.mu/minter.mu in Enrol,
	// sessMu -> Roster.mu in CompleteSession), so there is no ordering between
	// them to invert. They share no mutable field: idem is touched only under
	// enrolMu, sessions only under sessMu. If a future path ever needs both,
	// that is a design change to think about, not a nesting to add.
	enrolMu sync.Mutex
	sessMu  sync.Mutex

	// idem remembers applied idempotency keys with the payload they were
	// applied for and the result they produced. Memory only and lost on
	// restart; see recordIdempotent.
	idem map[string]idempotentEnrol

	// sessions holds pending and active sessions keyed by the HEX SHA-256 OF
	// THE TOKEN, never by the token itself — see tokenHash.
	//
	// There is deliberately NO per-agent index. Revoking every session of one
	// agent (AUTH-4) is an O(n) scan over this map, which at these bounds is
	// microseconds and needs no second data structure to keep in step. The
	// affordance is intentional: AUTH-4 is not missing an index, it is expected
	// to scan (invariant 8).
	sessions map[string]*Session
}

// idempotentEnrol is one remembered enrolment: enough of the request to tell a
// legitimate RETRY from a key REUSED for different content, plus the original
// result to replay.
type idempotentEnrol struct {
	name      string
	publicKey []byte
	result    EnrolResult
}

// NewService builds a Service from opts.
func NewService(opts Options) (*Service, error) {
	if opts.Minter == nil {
		return nil, errors.New("auth: creating service: an agent id minter is required and is never defaulted; a minter invented here would start every name at suffix 1 with nothing on disk behind it and would re-mint live agent ids (invariant 1)")
	}

	s := &Service{
		minter:                opts.Minter,
		roster:                opts.Roster,
		now:                   opts.Now,
		maxRosterEntries:      opts.MaxRosterEntries,
		maxIdempotencyEntries: opts.MaxIdempotencyEntries,
		maxSessions:           opts.MaxSessions,
		idem:                  make(map[string]idempotentEnrol),
		sessions:              make(map[string]*Session),
	}
	if s.roster == nil {
		s.roster = NewMemoryRoster()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxRosterEntries <= 0 {
		s.maxRosterEntries = DefaultMaxRosterEntries
	}
	if s.maxIdempotencyEntries <= 0 {
		s.maxIdempotencyEntries = DefaultMaxIdempotencyEntries
	}
	if s.maxSessions <= 0 {
		s.maxSessions = DefaultMaxSessions
	}
	return s, nil
}

// EnrolRequest is one enrolment attempt. EVERY field is untrusted client input.
type EnrolRequest struct {
	// Name is the short name the client asks for. The server decides the id.
	Name string

	// PublicKey is the PUBLIC half of the client's Ed25519 AUTH keypair. It may
	// be nil, empty or the wrong length — that is a clean validation error, and
	// never a panic (see ErrInvalidPublicKey).
	PublicKey ed25519.PublicKey

	// IdempotencyKey makes the enrolment safe to retry (invariant 10). It is
	// REQUIRED: without one, a retry after a lost acknowledgement mints a
	// second agent id for the same agent, and the client has no way to tell.
	IdempotencyKey string
}

// EnrolResult is what an accepted enrolment produced.
type EnrolResult struct {
	// AgentID is the fully-qualified, server-minted id (invariants 1 and 2).
	AgentID string

	// BusID is this bus's id, the qualifying half of AgentID.
	BusID string

	// Name is the short name as requested, the other half of AgentID.
	Name string

	// EnrolledAt is when the ORIGINAL enrolment was accepted. A replay carries
	// the original instant, not the instant of the replay: the result of an
	// idempotent retry is the original result, byte for byte.
	EnrolledAt time.Time

	// Replayed reports that this result came from the idempotency table rather
	// than from a fresh enrolment — the client retried and nothing was
	// re-applied.
	//
	// It is NOT part of the enrolment: it describes THIS call, so it is false
	// in the stored record and set only on the returned copy. The HTTP layer
	// surfaces it as a response header, leaving the body identical between the
	// original and the replay.
	Replayed bool
}

// Enrol validates an enrolment, mints an agent id and records the agent.
//
// It is idempotent per invariant 10: the same key with the same payload returns
// the ORIGINAL result and applies nothing; the same key with a DIFFERENT
// payload is a protocol violation and returns ErrIdempotencyKeyReused.
//
// # Nothing here is durable
//
// This records the agent in memory and returns. It does NOT write a WAL prepare
// or commit, so an acknowledged enrolment does NOT survive a crash. That is a
// known, deliberately-scoped gap that AUTH-3 closes by making this method write
// through the two-phase path before it returns — see point 1 of the
// ids.NameSuffixes doc for the ordering that will be required. Until then no
// caller may present enrolment as durable.
//
// # The suffix is BURNED by Mint, and that is correct
//
// If the roster write fails after the id is minted, the suffix the minter
// allocated is spent and is NOT handed back. Gaps in a name's suffix sequence
// are correct and must not be compacted (point 4 of the ids.NameSuffixes doc):
// re-issuing a number after a failure is how a later agent inherits an earlier
// agent's identity. A failed enrolment costs one number out of 2^64 per name.
func (s *Service) Enrol(req EnrolRequest) (EnrolResult, error) {
	// Validation first, and OUTSIDE the lock: it touches no shared state, and
	// keeping it out means a flood of malformed requests cannot serialise
	// behind the enrolments that are actually doing work.
	if err := validateIdempotencyKey(req.IdempotencyKey); err != nil {
		return EnrolResult{}, err
	}
	if err := ids.ValidateAgentName(req.Name); err != nil {
		return EnrolResult{}, fmt.Errorf("%w: %s", ErrInvalidName, err)
	}
	// BEFORE anything stores this key, and long before anything verifies with
	// it: ed25519.Verify PANICS on a public key of the wrong size, so an
	// unchecked key here is a remote denial of service reachable without
	// credentials. A nil or empty key lands here too and is a plain 400.
	if len(req.PublicKey) != ed25519.PublicKeySize {
		return EnrolResult{}, fmt.Errorf("%w: got %d bytes, want exactly %d", ErrInvalidPublicKey, len(req.PublicKey), ed25519.PublicKeySize)
	}

	s.enrolMu.Lock()
	defer s.enrolMu.Unlock()

	// Idempotency BEFORE admission control, deliberately. A retry of an
	// enrolment this server already accepted must keep succeeding even once the
	// roster is full: the agent is already in that roster, so replaying its
	// result admits nobody new.
	if prev, ok := s.idem[req.IdempotencyKey]; ok {
		if prev.name != req.Name || !bytes.Equal(prev.publicKey, req.PublicKey) {
			// Same key, different content. Not a retry — a protocol violation.
			// The payload is NOT echoed into the error: the caller already
			// knows what it sent, and the two public keys have no business in a
			// log line.
			return EnrolResult{}, fmt.Errorf("%w: key %q was applied for agent %q", ErrIdempotencyKeyReused, req.IdempotencyKey, prev.result.AgentID)
		}
		out := prev.result
		out.Replayed = true
		return out, nil
	}

	// Admission control, failing CLOSED. Enrolment is unauthenticated by
	// design, so this bound is the only thing stopping an anonymous caller from
	// growing the roster — and the per-name suffix counters behind it, which
	// are never reclaimed (point 8 of the ids.NameSuffixes doc) — without
	// limit.
	if s.roster.Len() >= s.maxRosterEntries {
		return EnrolResult{}, fmt.Errorf("%w: the roster holds %d agents, the limit", ErrCapacity, s.maxRosterEntries)
	}
	// Checked before the mint so a full idempotency table cannot burn suffixes.
	if len(s.idem) >= s.maxIdempotencyEntries {
		return EnrolResult{}, fmt.Errorf("%w: %d idempotency keys are remembered, the limit", ErrCapacity, s.maxIdempotencyEntries)
	}

	agentID, err := s.minter.Mint(req.Name)
	if err != nil {
		return EnrolResult{}, fmt.Errorf("auth: enrolling %q: %w", req.Name, err)
	}

	now := s.now()
	entry := RosterEntry{
		AgentID:    agentID,
		Name:       req.Name,
		PublicKey:  req.PublicKey,
		EnrolledAt: now,
	}
	if err := s.roster.Put(entry); err != nil {
		// The suffix inside agentID is spent. See the doc comment: it is not
		// reused, and the gap it leaves is correct.
		return EnrolResult{}, fmt.Errorf("auth: recording enrolment of %q: %w", agentID, err)
	}

	result := EnrolResult{
		AgentID:    agentID,
		BusID:      s.minter.BusID(),
		Name:       req.Name,
		EnrolledAt: now,
	}
	s.recordIdempotent(req, result)
	return result, nil
}

// recordIdempotent remembers that key was applied, with the payload it was
// applied for and the result to replay. The caller must hold s.enrolMu.
//
// # This table is memory-only and unbounded in TIME, not in size
//
// Two gaps are known and deliberately left to IDEM-11, which owns the
// cross-cutting idempotency layer:
//
//   - DURABILITY. Invariant 10 requires that the memory of applied keys survive
//     restart — it is part of recovered state, not a cache. This map is neither,
//     so today a retry that straddles a restart re-applies. AUTH-3/IDEM-11 fix
//     that by recording the key in the same durable record as the enrolment.
//   - RETENTION. There is no expiry here: a key is remembered until the process
//     ends. The right answer is a bounded retention WINDOW (remember every key
//     for long enough to cover any plausible client retry, then forget), and
//     choosing that window is IDEM-11's call, not this task's.
//
// What this does NOT do is evict under pressure. The table FAILS CLOSED at
// maxIdempotencyEntries (the check is in Enrol, before anything is applied),
// because evicting a remembered key silently converts the next replay of it
// into a fresh application — a second agent id for one agent, which is exactly
// the double-apply invariant 10 forbids. A refused enrolment is recoverable; a
// silently duplicated one is not.
func (s *Service) recordIdempotent(req EnrolRequest, result EnrolResult) {
	s.idem[req.IdempotencyKey] = idempotentEnrol{
		name: req.Name,
		// Copied for the same reason MemoryRoster.Put copies: the caller may
		// still hold this slice, and a mutated copy here would make a genuine
		// retry look like a key reused with different content, or worse.
		publicKey: append([]byte(nil), req.PublicKey...),
		result:    result,
	}
}

// validateIdempotencyKey enforces the shape of a client-supplied idempotency
// key: non-empty, at most MaxIdempotencyKeyLen bytes, and [A-Za-z0-9._-] only.
//
// This is the same posture as httpapi.SanitizeRequestID, and for the same
// reason: the key is reflected into the server log, so anything outside a short
// run of safe bytes is REJECTED outright rather than escaped and kept. The
// check is spelled out here rather than imported because internal/httpapi
// imports this package, and depending back on it would be a cycle.
func validateIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: an idempotency key is required on every mutating call so a retry after a lost acknowledgement cannot be applied twice (invariant 10)", ErrInvalidIdempotencyKey)
	}
	if len(key) > MaxIdempotencyKeyLen {
		// Not echoed: it is oversized, and an attacker choosing the input must
		// not choose a multiple of it back out of a log line.
		return fmt.Errorf("%w: %d bytes, but an idempotency key is at most %d; the key is not echoed here because it is oversized", ErrInvalidIdempotencyKey, len(key), MaxIdempotencyKeyLen)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '-':
		default:
			// The offending BYTE is quoted, never the whole key: the key is
			// untrusted and about to be written to a log.
			return fmt.Errorf("%w: byte %d is %q, but an idempotency key must contain only [A-Za-z0-9._-]", ErrInvalidIdempotencyKey, i, key[i:i+1])
		}
	}
	return nil
}
