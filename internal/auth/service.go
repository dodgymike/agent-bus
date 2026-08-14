package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
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

	// DefaultMaxActiveSessionsPerAgent bounds how many ACTIVE sessions one
	// PROVEN agent identity may hold at once. Enforced in CompleteSession, which
	// is the only place the key is a proven identity rather than an
	// attacker-supplied one — see the comment on the check there
	// (AUTH-1-FU-ACTIVECAP) for why a per-agent key is safe there and is not on
	// the unauthenticated BeginSession route.
	//
	// # Why 32
	//
	// The steady state for a well-behaved agent is TWO concurrent sessions: a
	// client establishes its next one at SessionRefreshFraction (75%) of
	// SessionLifetime, so the old and the new overlap for the final quarter. 32
	// is about sixteen times that — generous room for one agent id driven from
	// several processes or hosts, and for a client that loses its token and
	// re-handshakes rather than waiting out the old session — while still
	// bounding one proven identity to 32/16384 = 0.2% of the session table.
	//
	// The ergonomic hazard is real and is why the value is generous and
	// tunable: a refusal is not transient the way a global-capacity refusal is.
	// An agent that has genuinely reached its cap stays refused until its OWN
	// oldest session expires, which is up to SessionLifetime away, because
	// nothing here evicts.
	DefaultMaxActiveSessionsPerAgent = 32
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
	// It is, with ChallengeTTL, the GLOBAL bound on the session table. The
	// per-agent bounds either side of it are deliberately asymmetric: there is
	// NO per-agent cap on PENDING sessions (BeginSession, where agentID is
	// attacker-supplied and any bucket is a lockout primitive), and there IS one
	// on ACTIVE sessions (CompleteSession, where the key is a proven identity)
	// — see MaxActiveSessionsPerAgent and the comments on both methods.
	MaxSessions int

	// MaxActiveSessionsPerAgent bounds the ACTIVE sessions one agent may hold
	// at once; 0 means DefaultMaxActiveSessionsPerAgent. Enforced in
	// CompleteSession.
	MaxActiveSessionsPerAgent int
}

// Service is the enrolment and session authority. It is safe for concurrent
// use.
//
// The zero value is not usable; construct with NewService.
type Service struct {
	minter *ids.AgentIDMinter
	roster Roster
	now    func() time.Time

	maxRosterEntries          int
	maxIdempotencyEntries     int
	maxSessions               int
	maxActiveSessionsPerAgent int

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
	// messagingKey is part of the REMEMBERED PAYLOAD, not decoration. Invariant
	// 10 defines a legitimate retry as "same key + same payload", and the
	// messaging key is now part of what an enrolment asserts — so a second call
	// under one key carrying a DIFFERENT messaging key is a key reused for
	// different content and is refused. Leaving it out of the comparison would
	// be worse than a missing check: the replay would return the ORIGINAL result
	// and apply nothing, so the roster would keep the FIRST messaging key while
	// the client walked away believing the second one was registered — every
	// message it signs then fails to verify, everywhere, with nothing pointing
	// at the cause.
	messagingKey []byte
	// inviteID is part of the REMEMBERED PAYLOAD for exactly the reason
	// messagingKey is. An enrolment presented with an invite ASSERTS that invite,
	// so the same key re-presented with a DIFFERENT invite id is a key reused for
	// different content — ErrIdempotencyKeyReused — and not a retry. Leaving it
	// out would be worse than a missing check: the replay would return the
	// ORIGINAL result and apply nothing, so the second invite would be left
	// UNSPENT while the caller walked away believing it had been redeemed.
	// An UN-INVITED enrolment remembers the empty string, so a retry that adds an
	// invite to a previously un-invited key is caught too.
	inviteID string
	result   EnrolResult
}

// NewService builds a Service from opts.
func NewService(opts Options) (*Service, error) {
	if opts.Minter == nil {
		return nil, errors.New("auth: creating service: an agent id minter is required and is never defaulted; a minter invented here would start every name at suffix 1 with nothing on disk behind it and would re-mint live agent ids (invariant 1)")
	}

	s := &Service{
		minter:                    opts.Minter,
		roster:                    opts.Roster,
		now:                       opts.Now,
		maxRosterEntries:          opts.MaxRosterEntries,
		maxIdempotencyEntries:     opts.MaxIdempotencyEntries,
		maxSessions:               opts.MaxSessions,
		maxActiveSessionsPerAgent: opts.MaxActiveSessionsPerAgent,
		idem:                      make(map[string]idempotentEnrol),
		sessions:                  make(map[string]*Session),
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
	if s.maxActiveSessionsPerAgent <= 0 {
		s.maxActiveSessionsPerAgent = DefaultMaxActiveSessionsPerAgent
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

	// MessagingPublicKey is the PUBLIC half of the client's Ed25519 MESSAGING
	// keypair — a DIFFERENT key from PublicKey, and the reason RosterEntry keeps
	// the two apart (see RosterEntry.MessagingPublicKey). It is client-supplied
	// MATERIAL, never an identity: nothing here derives, checks or influences the
	// minted agent id from it (invariant 1). The server only records the public
	// half, exactly as it does for the auth key.
	//
	// It is what a peer bus is later handed to verify this agent's signed
	// messages, and it is what RELAY-17's per-agent attestation signs over — a
	// bus cannot attest a key it never received, which is why the field is
	// carried on the enrolment rather than registered by a later call.
	//
	// # OPTIONAL TODAY, and this is a STAGING state, not the end state
	//
	// Empty is accepted and stored as the reserved/unpopulated state, for exactly
	// the reason RosterEntry documents: every agent enrolled before this field
	// existed has none, and a roster that refused them would brick every current
	// identity on the bus. New enrolments are INTENDED to be refused without one
	// (an agent that cannot be attested cannot participate in relay), and that
	// flip is deliberately NOT made here — see the RELAY-13 follow-up note in
	// Enrol.
	//
	// When present it must be exactly ed25519.PublicKeySize bytes — a
	// wrong-length key reaching ed25519.Verify is a PANIC, not a false — and it
	// must NOT equal PublicKey. Both checks are in Enrol.
	MessagingPublicKey ed25519.PublicKey

	// IdempotencyKey makes the enrolment safe to retry (invariant 10). It is
	// REQUIRED: without one, a retry after a lost acknowledgement mints a
	// second agent id for the same agent, and the client has no way to tell.
	IdempotencyKey string

	// Invite is an in-flight invite redemption this enrolment must be ATOMIC
	// with. It is supplied by the caller (internal/httpapi), which has already
	// verified the presented secret.
	//
	// NIL MEANS AN UN-INVITED ENROLMENT, AND THAT IS STILL FULLY SUPPORTED. This
	// build REDEEMS an invite when one is presented; it does not REQUIRE one.
	// Invariant 3's end state is invite-only enrolment, and making that flip is a
	// SEPARATE task — nothing here may be read as "enrolment is gated". The flip
	// cannot be made here in any case: the compiled CLI cannot yet present an
	// invite, so requiring one would lock every enrolled agent's peers out of the
	// bus.
	//
	// When it is non-nil the roster MUST support the composite write
	// (PutWithInvite). A roster that does not — MemoryRoster is one — makes the
	// enrolment fail with ErrInviteNotAtomic rather than proceed: see Enrol.
	Invite InviteRedemption
}

// InviteRedemption is one in-flight invite redemption the enrolment it
// authorises must be ATOMIC with. *invite.Redemption is adapted to it by the
// CALLER (internal/httpapi), so this package composes the transaction without
// importing internal/invite.
//
// The lifecycle mirrors invite.Redemption exactly: Consume builds the durable
// consumption record, the caller writes it in the SAME wal.Entry as the
// enrolment, and Commit folds it into the serving copy afterwards — or Abort
// releases the reservation when nothing became durable.
//
// # Commit and Abort return NOTHING, on purpose
//
// The only error invite.Redemption.Commit returns is caller MISUSE (commit
// without consume), which cannot happen on this path because Enrol calls them in
// exactly one order. And by the time Commit runs, the enrolment is DURABLE: a
// durable enrolment must never be reported to the client as failed over a
// bookkeeping error in the serving copy. The ADAPTER logs it.
type InviteRedemption interface {
	// InviteID is the id of the invite being redeemed. It is recorded on the
	// roster entry as provenance.
	InviteID() string

	// RiderKind is the wal.Entry.Kind the consumption record replays as
	// (invite.RecordKind).
	RiderKind() string

	// Consume builds the durable consumption record for this redemption, given
	// the result the enrolment produced. It writes nothing.
	Consume(EnrolResult) (json.RawMessage, error)

	// Commit folds the consumption into the serving copy. Called ONLY after the
	// composite entry is durable.
	Commit()

	// Abort releases the reservation. Called ONLY when the write demonstrably
	// did not commit. It is a documented no-op after a successful Commit and on
	// a replay outcome.
	Abort()
}

// ErrInviteNotAtomic reports an INVITED enrolment against a roster that cannot
// write the invite consumption and the enrolment as one transaction.
//
// It is a REFUSAL, not a downgrade to two writes. Splitting them reopens exactly
// the window the participant API exists to close: a crash between the two leaves
// an agent enrolled against an invite that is still open (redeemable a second
// time), or an invite spent on an enrolment that never happened. Failing closed
// costs one refused enrolment; the alternative costs single use, which is the
// property the whole invite mechanism rests on.
var ErrInviteNotAtomic = errors.New("auth: this roster cannot record an enrolment and its invite consumption in one transaction")

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
// # What here is durable, and what is not
//
// The ROSTER WRITE is durable WHEN — and only when — a WALRoster was injected
// through Options.Roster: Roster.Put then writes a WAL prepare, fsyncs it,
// writes and fsyncs the commit, and applies to memory, all before this method
// returns, which is the ordering point 1 of the ids.NameSuffixes doc requires.
// With the default MemoryRoster it writes nothing.
//
// Nothing else here is durable. The idempotency table (s.idem) and the session
// table are still MEMORY ONLY, so a retry that straddles a restart re-applies
// and mints a second agent id for one agent — invariant 10's durability half is
// not yet met on this route (IDEM-11 owns it).
//
// And be precise about what is actually running: cmd/agent-bus/main.go still
// injects the MEMORY roster. Until the wiring task lands, the durable path
// exists and is tested but is not the one a deployed bus takes, so no caller
// may present enrolment on the shipped binary as durable.
//
// # The messaging key rides the SAME durable record (RELAY-13)
//
// req.MessagingPublicKey is written into the roster entry, so with a WALRoster
// it is encoded into the one enrolment record (auth.Encode's "msg_pub") that is
// fsynced before this method returns, and is rebuilt by replay at the next
// start. There is no second write, no second file and no in-memory-only half:
// a field that were written but not replayed would be WORSE than an absent one,
// because it would look present until a restart.
//
// It is still OPTIONAL on the way in. Refusing an enrolment that carries no
// messaging key is the intended end state — an agent that cannot be attested
// cannot participate in relay — but making that flip RED-LINES every existing
// caller and test that enrols with an auth key alone, in files this task does
// not own. It is therefore reported as a follow-up rather than half-made here;
// NO TASK HAS BEEN FILED for it yet, so name one here when one exists. Read this
// method as "records it when offered", never as "requires it".
//
// # An invite is REDEEMED when one is presented — and is NOT required
//
// When req.Invite is non-nil the enrolment record and the invite CONSUMPTION
// record are written as ONE composite wal.Entry (EnrolInviteRecordKind, through
// WALRoster.PutWithInvite), so a crash can never leave an agent enrolled
// against an invite that is still open, or an invite spent on an enrolment that
// never happened. A roster that cannot do that write refuses the enrolment
// outright (ErrInviteNotAtomic) rather than splitting it into two transactions.
//
// req.Invite == nil is an UN-INVITED enrolment and is STILL ACCEPTED. This
// build redeems an invite; it does not require one. Invite-only enrolment
// (invariant 3) is a separate task and nothing here may be read as claiming the
// gate is on.
//
// # The suffix is BURNED by Mint, and that is correct
//
// If the roster write fails after the id is minted, the suffix the minter
// allocated is spent and is NOT handed back. Gaps in a name's suffix sequence
// are correct and must not be compacted (point 4 of the ids.NameSuffixes doc):
// re-issuing a number after a failure is how a later agent inherits an earlier
// agent's identity. A failed enrolment costs one number out of 2^64 per name.
func (s *Service) Enrol(req EnrolRequest) (EnrolResult, error) {
	// THE RESOLVE GUARD, AND IT IS THE FIRST STATEMENT IN THE METHOD ON PURPOSE.
	// Every path that leaves Enrol WITHOUT a durable composite entry releases the
	// invite reservation the caller handed in, so a failed enrolment never
	// strands an invite in the in-flight table until the ReservationTTL sweep
	// reaps it.
	//
	// THE PLACEMENT IS THE GUARANTEE, not the comment. The reservation is already
	// held when Enrol is entered — internal/httpapi/auth.go takes it before
	// calling — so ANY return statement above this defer is an uncovered leak.
	// It sat below the input validation until 2026-08-14, and four validation
	// refusals (bad idempotency key, bad name, bad auth key, bad or duplicated
	// messaging key) were reachable from the wire with a valid invite presented
	// and leaked the reservation for the TTL. Registering the defer before the
	// first `if` is what makes "every exit releases it" a property of control
	// flow rather than a claim to be re-audited on every edit; if you add a
	// return, it is covered without your doing anything. Nothing above it may
	// return, and nothing may be moved above it.
	//
	// A BLANKET GUARD IS SAFE BECAUSE ABORT CANNOT UN-SPEND ANYTHING.
	// invite.Redemption.Abort is a documented no-op after a successful Commit,
	// and a no-op on an OutcomeReplay redemption (it returns before touching the
	// table in both cases), so covering the replay and success exits costs
	// nothing and risks nothing. The ONE case where an abort WOULD be wrong — a
	// write that failed AFTER its commit record was fsynced, where the invite is
	// durably spent — never reaches Abort either, because the durable branch of
	// PutWithInvite's error path sets resolved below.
	//
	// It is also safe this early because the guard touches no shared state and
	// takes no lock: it only reads req.Invite, which is the caller's.
	resolved := false
	defer func() {
		if req.Invite != nil && !resolved {
			req.Invite.Abort()
		}
	}()

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
	// The messaging key is OPTIONAL today and validated only when present — see
	// EnrolRequest.MessagingPublicKey for why empty must stay acceptable, and
	// why a present-but-wrong-length key must not.
	//
	// The length rule is ALSO enforced by validateRosterEntryKeys, which every
	// Roster.Put runs, so this is not the only guard against handing a wrong-size
	// key to ed25519.Verify. WHAT THIS ONE ADDS IS ORDERING: it runs before
	// minter.Mint below, so a malformed key is refused without BURNING AN AGENT
	// ID SUFFIX. A number spent on a rejected enrolment is never handed back
	// (invariant 1 — ids are never reused, and gaps are correct), so validating
	// after the mint would let an anonymous caller burn one suffix per malformed
	// request.
	if len(req.MessagingPublicKey) != 0 && len(req.MessagingPublicKey) != ed25519.PublicKeySize {
		return EnrolResult{}, fmt.Errorf("%w: messaging public key is %d bytes, want exactly %d", ErrInvalidPublicKey, len(req.MessagingPublicKey), ed25519.PublicKeySize)
	}
	// ONE KEY MAY NOT SERVE BOTH ROLES. Documented as impossible is not the same
	// as enforced, and this is the only point at which both keys are in hand.
	//
	// The hazard: the session handshake has the SERVER choose the bytes the auth
	// key signs (invariant 3), so an agent reusing its auth key for messaging
	// would be putting a server-chosen input under the key its PEERS verify.
	//
	// BE ACCURATE ABOUT WHAT THIS CHECK IS AND IS NOT. It is NOT what stops a
	// session signature being replayed as a message signature — DOMAIN
	// SEPARATION does that, and already does: a session challenge starts with the
	// 'a' of SessionSigningContext, a canonical message with the 0x00 of a uint32
	// length, so the two byte languages cannot be confused (internal/signing's
	// canonical.go makes this argument for exactly this key pair). This check
	// makes the separation STRUCTURAL instead of contingent on every future
	// signing domain staying disjoint, and it bounds one compromised key to one
	// role. It is also per-request: it cannot stop an enroller registering some
	// OTHER agent's public key as its messaging key, because NO PROOF OF
	// POSSESSION of the messaging private key is taken at enrolment. That gap is
	// real and is NOT covered by AUTH-1-FU-POPKEY, which is about the AUTH key —
	// no task covers this one yet, so do not read it as scheduled.
	if len(req.MessagingPublicKey) != 0 && bytes.Equal(req.MessagingPublicKey, req.PublicKey) {
		return EnrolResult{}, fmt.Errorf("%w: the messaging public key is the same key as the auth public key; they are separate keys with separate jobs and one key may not serve both", ErrInvalidPublicKey)
	}

	// NOTE: with a WALRoster injected, enrolMu is now GENUINELY held across an
	// fsync — two of them — for the duration of Roster.Put below. That is the
	// cost the lock split above anticipated in so many words ("once AUTH-3 makes
	// that roster write durable, the lock is held across an fsync, for
	// milliseconds"), and it is why Authenticate takes sessMu and not this one.
	// Nothing about the locking changes here; the comment records that the
	// anticipated condition has arrived.
	s.enrolMu.Lock()
	defer s.enrolMu.Unlock()

	// Idempotency BEFORE admission control, deliberately. A retry of an
	// enrolment this server already accepted must keep succeeding even once the
	// roster is full: the agent is already in that roster, so replaying its
	// result admits nobody new.
	if prev, ok := s.idem[req.IdempotencyKey]; ok {
		if prev.name != req.Name ||
			!bytes.Equal(prev.publicKey, req.PublicKey) ||
			!bytes.Equal(prev.messagingKey, req.MessagingPublicKey) ||
			prev.inviteID != inviteIDOf(req.Invite) {
			// Same key, different content. Not a retry — a protocol violation.
			// The payload is NOT echoed into the error: the caller already
			// knows what it sent, and the two public keys have no business in a
			// log line.
			return EnrolResult{}, fmt.Errorf("%w: key %q was applied for agent %q", ErrIdempotencyKeyReused, req.IdempotencyKey, prev.result.AgentID)
		}
		// A REPLAY APPLIES NOTHING, so the guard above aborts the (fresh)
		// reservation this call took, and that is exactly right: this enrolment
		// spends no invite, and the reservation must go back so the invite is not
		// locked out by a retry. Note the invite is NOT re-spent either — the
		// original enrolment already spent whichever invite it carried.
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
		AgentID: agentID,
		Name:    req.Name,
		// Both keys are recorded as the CLIENT presented them. Neither is
		// consulted by the mint above, which has already happened: the id is the
		// server's (invariant 1) and no part of it is derived from key material.
		AuthPublicKey: req.PublicKey,
		// Empty when the client sent none, which is the reserved state Decode and
		// validateRosterEntryKeys already accept — see
		// EnrolRequest.MessagingPublicKey. Roster.Put DEEP-COPIES it, so the
		// caller's slice is not the stored credential.
		MessagingPublicKey: req.MessagingPublicKey,
		// Epoch equals EnrolledAt today: this IS the enrolment, so the
		// credential's epoch begins here. It is set explicitly rather than left
		// zero because the durable record carries it (Decode refuses a zero
		// epoch), and it is a separate field so a future re-key can bump it
		// without rewriting EnrolledAt. See RosterEntry.Epoch.
		Epoch:      now,
		EnrolledAt: now,
		// CertBindings is left ZERO and remains reserved (see RosterEntry):
		// MTLS-BIND populates it, and nothing on this path may invent a value for
		// it. MessagingPublicKey is no longer among the reserved fields —
		// RELAY-13 populates it above, from the request — and neither is InviteID:
		// INVITE-GATE populates it just below, from the redemption, so an invited
		// enrolment records WHICH invite admitted it.
	}
	if req.Invite != nil {
		entry.InviteID = req.Invite.InviteID()
	}

	// Built BEFORE the write because Consume needs it: the consumption record
	// stores the agent id the redemption created and the response a legitimate
	// retry will be replayed verbatim.
	result := EnrolResult{
		AgentID:    agentID,
		BusID:      s.minter.BusID(),
		Name:       req.Name,
		EnrolledAt: now,
	}

	if req.Invite == nil {
		// THE UN-INVITED PATH, unchanged. An enrolment carrying no invite is
		// still accepted; see EnrolRequest.Invite.
		if err := s.roster.Put(entry); err != nil {
			// The suffix inside agentID is spent. See the doc comment: it is not
			// reused, and the gap it leaves is correct.
			return EnrolResult{}, fmt.Errorf("auth: recording enrolment of %q: %w", agentID, err)
		}
	} else {
		// THE INVITED PATH: the enrolment record and the invite consumption
		// record ride in ONE wal.Entry.
		ir, ok := s.roster.(interface {
			PutWithInvite(RosterEntry, InviteRider) (bool, error)
		})
		if !ok {
			// FAIL CLOSED. See ErrInviteNotAtomic: an invited enrolment whose
			// consumption cannot share the transaction must not proceed at all,
			// because the two-write alternative can spend an invite on an
			// enrolment that never happened, or enrol an agent against an invite
			// that stays redeemable.
			return EnrolResult{}, fmt.Errorf("%w: refusing the invited enrolment of %q; the injected roster (%T) has no PutWithInvite, so the consumption record could only be written as a SEPARATE transaction, which is exactly the window the composite record exists to close", ErrInviteNotAtomic, agentID, s.roster)
		}
		body, err := req.Invite.Consume(result)
		if err != nil {
			// Nothing is durable and the guard aborts the reservation. The agent
			// id SUFFIX is already burned, and that is CORRECT and must not be
			// "fixed": invariant 1 — ids are never reused and gaps are correct.
			return EnrolResult{}, fmt.Errorf("auth: consuming the invite for the enrolment of %q: %w", agentID, err)
		}
		durable, err := ir.PutWithInvite(entry, InviteRider{Kind: req.Invite.RiderKind(), Body: body})
		if err != nil {
			if durable {
				// The composite entry — INCLUDING the invite consumption record —
				// is on stable storage. DO NOT abort: releasing the reservation
				// would admit a SECOND redemption of an invite the log already
				// says is spent (internal/invite/store.go's Redeem states this
				// rule and PutWithInvite inherits it verbatim). Leaving it
				// unresolved is fail-closed: the invite stays locked until a
				// restart rebuilds the table from the durable log.
				resolved = true
			}
			return EnrolResult{}, fmt.Errorf("auth: recording enrolment of %q: %w", agentID, err)
		}
		// Durable. Resolve BEFORE committing so the guard cannot abort a
		// redemption that is already spent on disk, whatever Commit does.
		resolved = true
		req.Invite.Commit()
	}

	s.recordIdempotent(req, result)
	return result, nil
}

// inviteIDOf is the invite id an enrolment asserts, or "" for an un-invited
// one. It is what the idempotency replay check compares, so a nil Invite and an
// invite id are never confused.
func inviteIDOf(r InviteRedemption) string {
	if r == nil {
		return ""
	}
	return r.InviteID()
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
		// Copied for the same reason, and note append(nil, empty...) yields nil:
		// an absent messaging key is remembered as nil, and bytes.Equal treats
		// nil and empty alike, so a client that sends "" and a client that omits
		// the field are the same payload rather than a false conflict.
		messagingKey: append([]byte(nil), req.MessagingPublicKey...),
		// "" for an un-invited enrolment; see idempotentEnrol.inviteID.
		inviteID: inviteIDOf(req.Invite),
		result:   result,
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
