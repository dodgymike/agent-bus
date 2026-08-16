package auth

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Operator sessions and THE AUTHORIZATION CHECK (AUTH-10).
//
// This file is the operator-plane twin of session.go, and it is a twin rather
// than a reuse. Every difference below is deliberate and each one is justified
// where it appears; none of them is an accident of writing the file second.
//
//	                       AGENT (session.go)          OPERATOR (this file)
//	principal type         Principal                   OperatorPrincipal  (a DISTINCT Go type)
//	session table          Service.sessions            OperatorService.sessions (SEPARATE)
//	signing context        SessionSigningContext       OperatorSessionSigningContext
//	begin                  unauthenticated             REQUIRES the client certificate
//	cert cross-check       a second call the caller    a REQUIRED PARAMETER of Authenticate
//	                       can forget
//
// What it shares is everything where sharing is the safer answer: SessionLifetime,
// SessionRefreshFraction, ChallengeTTL, TokenRandBytes, tokenHash and newToken
// are USED, not copied, so the one-hour ceiling and the token entropy cannot
// drift between the two planes.

const (
	// OperatorSessionSigningContext is the DOMAIN SEPARATION prefix for an
	// operator's challenge signature: the exact byte string the client signs is
	// OperatorSessionSigningContext + token.
	//
	// IT IS DISTINCT FROM SessionSigningContext, AND THAT IS LOAD-BEARING.
	// Without the distinction, a signature produced over an agent challenge
	// would be a valid signature over an operator challenge with the same token
	// bytes, and any party able to obtain one signature could present it on the
	// other plane. With it, an operator's signature can NEVER be replayed as an
	// agent's and vice versa — the two contexts are different messages, and
	// Ed25519 verification over one says nothing about the other.
	//
	// "v1" is the EXISTING HTTP API version already carried by this server's
	// /v1/ routes, exactly as in SessionSigningContext. No new version number is
	// minted here.
	//
	// The client must PIN this constant. It is deliberately NOT sent on the
	// wire: a client that learned the context from the server would sign
	// whatever a man in the middle chose to prefix.
	OperatorSessionSigningContext = "agent-bus:operator-session-token:v1:"

	// DefaultMaxOperatorSessions bounds the operator session table, pending and
	// active together.
	//
	// 64, which is four orders of magnitude below DefaultMaxSessions, because
	// the populations are not comparable: a bus federating thousands of agents
	// has a handful of operators. A small table is also the point — see
	// OperatorService.sessions for why this table is separate from the agent one
	// and why its size is what makes that separation worth anything.
	DefaultMaxOperatorSessions = 64

	// DefaultMaxActiveOperatorSessionsPerOperator bounds how many ACTIVE
	// sessions one PROVEN operator identity may hold at once.
	//
	// 8. The steady state for a well-behaved client is TWO concurrent sessions
	// (it refreshes at SessionRefreshFraction, so old and new overlap for the
	// final quarter of the lifetime); 8 leaves room for an operator working from
	// two machines and re-handshaking after losing a token, while bounding one
	// identity to 8/64 of the table.
	//
	// Like DefaultMaxActiveSessionsPerAgent this cap is safe ONLY because it is
	// enforced where the key is a PROVEN identity — in CompleteSession, after a
	// signature verified — and never on the begin path. See the comment on the
	// check itself.
	DefaultMaxActiveOperatorSessionsPerOperator = 8

	// DefaultMaxPendingOperatorSessionsPerOperator bounds how many PENDING
	// (unsigned) challenges one PROVEN operator identity may hold at once.
	//
	// 8, the same number as the active cap and for the same shape of reason: a
	// well-behaved client holds ONE outstanding challenge, 8 leaves room for an
	// operator retrying from two machines after losing a token, and it bounds one
	// identity to 8/64 of the table.
	//
	// # WITHOUT IT, ONE OPERATOR LOCKS OUT EVERY OTHER OPERATOR
	//
	// DefaultMaxOperatorSessions is GLOBAL, so one certificate holder parking 64
	// pending challenges fills the whole table and no other operator can begin a
	// session — and the admin plane is exactly the plane whose job is to fix that
	// kind of incident, so denying it is the expensive failure (it is AUTH-7's
	// shape again: an identity locks everyone out and the only remedy is
	// restarting the bus).
	//
	// # WHY A PER-OPERATOR KEY IS SAFE HERE AND IS NOT ON Service.BeginSession
	//
	// This is the distinction AUTH-1-FU-PENDINGCAP turns on and it must not be
	// collapsed. On the AGENT plane agentID is an ATTACKER-SUPPLIED VICTIM
	// IDENTIFIER — anyone can name anyone — so any bucket keyed on it is itself a
	// lockout primitive, which is why Service.BeginSession has no per-agent cap.
	// Here the caller has ALREADY proved possession of the named operator's
	// certificate private key through the TLS handshake before this check is
	// reached (BeginSession resolves certFP to an operator FIRST and refuses
	// unless it IS operatorID), so the key is a PROVEN identity and a flooder can
	// only ever fill its OWN bucket.
	DefaultMaxPendingOperatorSessionsPerOperator = 8
)

// errOperatorBeginRefused is THE ONE refusal BeginSession returns to a caller
// whose client certificate does not resolve to the operator it named.
//
// It is a single package-level value rather than a fmt.Errorf per call site
// precisely so it CANNOT drift into three subtly different messages again: one
// sentinel (ErrOperatorCertMismatch), one sentence, no operator id, no
// revocation instant, no reason, no count. Live, revoked, unknown and malformed
// are byte-identical to an unauthenticated caller, which is the whole property —
// and it is the same technique the agent plane uses for
// "no-matching-reservation" (invariant 10's deliberately indistinguishable
// arm): where two cases must not be told apart, they must share one error VALUE,
// not two errors that happen to read alike today.
//
// It deliberately does NOT say which of the two possible causes applies, and it
// deliberately does not suggest that the operator might exist.
var errOperatorBeginRefused = fmt.Errorf("%w: this connection's client certificate is not the live binding of the operator named, so no challenge is issued; this refusal is IDENTICAL whether that operator is live, revoked, unknown or malformed, and reports nothing about which", ErrOperatorCertMismatch)

// OperatorPrincipal is the authenticated identity behind a live OPERATOR
// session. It is what an admin route authorises against.
//
// # IT IS A DISTINCT GO TYPE FROM Principal, AND THAT IS THE PRIMARY ENFORCEMENT
//
// If an admin route reused AGENT authentication, an AGENT credential would
// authorise minting the credentials that CREATE AGENTS: any enrolled agent could
// mint itself an unlimited supply of new identities, which collapses invariant 3
// completely. So the principal is distinct in KIND, not merely in permission —
// and the sharpest expression of that is the Go type system. A handler whose
// signature requires an OperatorPrincipal CANNOT BE SATISFIED BY AN
// auth.Principal AT COMPILE TIME. No runtime check can be forgotten, no boolean
// can be defaulted wrong, and no reviewer has to notice.
//
// This is the same structural technique internal/httpapi/peerprincipal.go uses
// on the peer plane with noAgentPrincipal: a type that is deliberately NOT the
// agent principal, put where an agent principal would otherwise be assumed, so
// that any code path expecting an agent fails to compile rather than silently
// accepting a peer. Two planes, one technique, on purpose.
//
// Do not add a conversion between the two types. There is no correct one: an
// operator is not an agent, an agent is not an operator, and a function that
// mapped either onto the other would be the single line that undoes all of it.
type OperatorPrincipal struct {
	// OperatorID is the fully-qualified "op:<bus-id>.<name>-<suffix>". It is the
	// AUTHORIZATION subject — and, unlike Principal.AgentID, it is never a
	// ROUTING subject: no message is addressed to it and no relay path contains
	// it.
	OperatorID string

	// Name is the operator's short name, carried so an audit line can be written
	// without re-parsing the id.
	Name string

	// ExpiresAt is when this session stops authenticating.
	ExpiresAt time.Time
}

// OperatorChallenge is what BeginSession hands back: the token the operator must
// sign, and the deadline by which it must do so.
type OperatorChallenge struct {
	// OperatorID is the operator this challenge was minted for.
	OperatorID string

	// Token is the opaque session token. It is a LIVE CREDENTIAL once the
	// challenge is completed, so it must never be logged, echoed into an error,
	// or stored anywhere but the operator's own token file.
	Token string

	// ChallengeExpiresAt is when an uncompleted challenge stops being
	// completable.
	ChallengeExpiresAt time.Time
}

// OperatorSession is one entry in the operator session table.
//
// Sessions are IN MEMORY ONLY and are lost on restart, deliberately — the same
// choice invariant 3 makes for agent sessions, and for the same reason: a
// session is a short-lived credential with a one-hour ceiling, losing one costs
// a single challenge/response round trip, and persisting live credentials would
// put replayable material on disk for no benefit.
type OperatorSession struct {
	// OperatorID is the authenticated subject.
	OperatorID string

	// State is SessionPending or SessionActive — the same two states, and the
	// same type, as an agent session. Shared because the STATE MACHINE is
	// genuinely identical; what is not shared is the table it lives in.
	State SessionState

	// CertFingerprint is the client certificate this session was established
	// over. It is recorded at BeginSession, re-checked at CompleteSession, and
	// re-checked again on every Authenticate.
	CertFingerprint [32]byte

	// CreatedAt is when the challenge was issued.
	CreatedAt time.Time

	// ChallengeExpiresAt bounds the pending state. It is not consulted once the
	// session is active.
	ChallengeExpiresAt time.Time

	// ExpiresAt bounds the active state. It is set ONCE, when the challenge is
	// first completed successfully, and is NEVER extended.
	ExpiresAt time.Time

	// TokenHash is the hex SHA-256 of the token — the key this session is stored
	// under. The token itself is NOT held here; see tokenHash.
	TokenHash string
}

// OperatorOptions configures an OperatorService.
type OperatorOptions struct {
	// Now is the clock. Defaults to time.Now. Server-side expiry is
	// authoritative and is always measured against this clock — injectable for
	// the reason auth.Options.Now is: an expiry test that sleeps is a slow test
	// that is also flaky.
	Now func() time.Time

	// MaxSessions bounds the operator session table; 0 means
	// DefaultMaxOperatorSessions.
	MaxSessions int

	// MaxActiveSessionsPerOperator bounds the ACTIVE sessions one operator may
	// hold at once; 0 means DefaultMaxActiveOperatorSessionsPerOperator.
	MaxActiveSessionsPerOperator int

	// MaxPendingSessionsPerOperator bounds the PENDING challenges one operator
	// may hold at once; 0 means DefaultMaxPendingOperatorSessionsPerOperator.
	MaxPendingSessionsPerOperator int
}

// OperatorService issues and resolves OPERATOR sessions over an
// OperatorRegistry.
//
// The zero value is not usable; construct with NewOperatorService. It is safe
// for concurrent use.
type OperatorService struct {
	reg *OperatorRegistry
	now func() time.Time

	maxSessions                   int
	maxActiveSessionsPerOperator  int
	maxPendingSessionsPerOperator int

	// mu guards sessions.
	//
	// # THE TABLE IS SEPARATE FROM THE AGENT SESSION TABLE, AND THAT IS THE POINT
	//
	// It is not a `kind` flag on Service.sessions, and the reason is an incident
	// rather than a preference. The AGENT session table is FILLABLE BY AN
	// UNAUTHENTICATED FLOOD: BeginSession is unauthenticated by construction
	// (invariant 3 — it is how a credential is obtained), its own comment says
	// so at length, and the mitigation is an open task
	// (AUTH-1-FU-RATELIMIT). If operators shared that table, an operator could
	// not obtain a session DURING a flood — and an operator who cannot get a
	// session during a flood cannot fix the flood. That is precisely the class
	// of incident AUTH-7 was filed off: an agent locked itself out and the only
	// remedy was restarting the bus, punishing all six agents to unstick one.
	//
	// Separate tables give the admin plane its own, much smaller, budget that no
	// volume of anonymous agent-plane traffic can consume. It is not a defence
	// against a flood of OPERATOR begins — but those are not anonymous: this
	// plane's BeginSession refuses anyone who does not already present a bound
	// operator's client certificate, so filling THIS table requires a
	// certificate this bus has already been configured to trust.
	mu       sync.Mutex
	sessions map[string]*OperatorSession
}

// NewOperatorService builds the operator session service over reg.
//
// It calls reg.AttachSessions(itself), so a service built this way is ALWAYS
// wired for synchronous revocation and there is no ordering for a caller to get
// wrong: the moment an OperatorService exists, OperatorRegistry.Revoke can drop
// its sessions. A registry that already has a session table is an ERROR — one
// registry backs exactly one table, and a second would leave the first holding
// live sessions for operators the registry has revoked.
func NewOperatorService(reg *OperatorRegistry, opts OperatorOptions) (*OperatorService, error) {
	if reg == nil {
		return nil, errors.New("auth: creating the operator service: an operator registry is required and is never defaulted; a service over no registry would have nothing to check revocation against, and revocation is what makes an opaque session handle worth choosing (invariant 3)")
	}
	s := &OperatorService{
		reg:                           reg,
		now:                           opts.Now,
		maxSessions:                   opts.MaxSessions,
		maxActiveSessionsPerOperator:  opts.MaxActiveSessionsPerOperator,
		maxPendingSessionsPerOperator: opts.MaxPendingSessionsPerOperator,
		sessions:                      make(map[string]*OperatorSession),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxSessions <= 0 {
		s.maxSessions = DefaultMaxOperatorSessions
	}
	if s.maxActiveSessionsPerOperator <= 0 {
		s.maxActiveSessionsPerOperator = DefaultMaxActiveOperatorSessionsPerOperator
	}
	if s.maxPendingSessionsPerOperator <= 0 {
		s.maxPendingSessionsPerOperator = DefaultMaxPendingOperatorSessionsPerOperator
	}
	if err := reg.AttachSessions(s); err != nil {
		return nil, fmt.Errorf("auth: creating the operator service: %w", err)
	}
	return s, nil
}

// BeginSession issues a challenge for operatorID: a fresh opaque token, recorded
// PENDING, that the operator must sign with its private session-signing key and
// present to CompleteSession.
//
// # IT REQUIRES THE CLIENT CERTIFICATE UP FRONT, AND THE AGENT PATH DOES NOT
//
// certFP is the fingerprint of the certificate on the connection this call
// arrived over — a TRANSPORT FACT taken from r.TLS, never a request field
// (EnrolRequest.ClientCertFingerprint's rule: a client-supplied fingerprint is a
// claim anyone can make about anyone's certificate). This call REFUSES unless it
// matches the named operator's LIVE binding.
//
// That is strictly stronger than Service.BeginSession, which is unauthenticated,
// and it is what makes this surface neither of the two things that one has to
// live with:
//
//   - NOT AN ENUMERATION ORACLE — BUT ONLY BECAUSE OF THE ORDER BELOW, WHICH IS
//     THEREFORE LOAD-BEARING AND MUST NOT BE REARRANGED. An earlier version of
//     this method resolved the NAME first (parse, registry lookup, revocation
//     check) and the CERTIFICATE last, and this comment claimed the property
//     anyway. It did not hold: an unauthenticated caller got three
//     distinguishable refusals — ErrOperatorCertMismatch for a live operator,
//     ErrOperatorRevoked (naming the exact revocation instant) for a revoked one
//     and ErrUnknownOperator for one that does not exist — which is a roster
//     enumeration oracle plus a timestamp leak, reachable by anyone who can
//     complete a TLS handshake. NOW: the presented CERTIFICATE is resolved
//     first, and if it does not resolve to operatorID the refusal is ONE
//     sentinel with ONE fixed sentence that echoes no id, no instant and no
//     reason, identical for live, revoked, unknown and malformed. Only AFTER the
//     certificate has proved the caller IS that operator may a specific error be
//     returned, and at that point the caller already knows everything it is
//     told.
//   - NOT A LOCKOUT PRIMITIVE. Service.BeginSession must have NO per-agent cap,
//     because there agentID is an ATTACKER-SUPPLIED VICTIM IDENTIFIER and any
//     bucket keyed on it is a lockout (AUTH-1-FU-PENDINGCAP). Here the caller
//     has already proved possession of the named operator's certificate private
//     key through the handshake, so it cannot make its allocations land in
//     anybody else's bucket — which is what makes the per-operator PENDING cap
//     below safe on this plane and unsafe on that one.
//
// A REVOKED operator is refused here: a revoked operator's fingerprint is not a
// live binding, so it cannot resolve at step 2 at all, and it receives the same
// uniform refusal as everybody else. That is a deliberate loss of a helpful
// signal — the remedy is `agent-bus operator list -all` on the bus host, which
// is an operator action rather than an unauthenticated query.
func (s *OperatorService) BeginSession(operatorID string, certFP [32]byte) (OperatorChallenge, error) {
	// 1. THE ZERO FINGERPRINT, FIRST AND SEPARATELY. It is the value a caller
	// holds when there was NO certificate at all, and saying so distinguishes
	// nothing about the registry — the caller is being told a fact about its own
	// connection.
	if certFP == ([32]byte{}) {
		return OperatorChallenge{}, fmt.Errorf("%w: no client certificate was presented; the zero fingerprint is the ABSENCE of a certificate and names nobody, and an operator challenge is only ever issued over the certificate the operator is bound to (invariant 11)", ErrOperatorCertMismatch)
	}

	// 2. THE CERTIFICATE RESOLVES BEFORE THE NAME IS LOOKED AT. Everything that
	// fails here shares ONE sentinel and ONE sentence: an unresolvable
	// certificate, a certificate belonging to a different operator, a revoked
	// operator (whose binding is not live), an operator id that is not
	// registered, and an id that is not even well-formed — including every AGENT
	// id, which cannot resolve because no operator record ever holds an agent's
	// id. A caller that does not hold the operator's certificate private key
	// learns exactly one bit ("no"), and that bit was already knowable.
	holder, resolveErr := s.reg.LiveOperatorForCertFingerprint(certFP)
	if resolveErr != nil || holder != operatorID {
		return OperatorChallenge{}, errOperatorBeginRefused
	}

	// --- FROM HERE THE CALLER HAS PROVED IT IS operatorID ---------------------
	// It holds the private key of the certificate bound to this live operator, so
	// a specific error tells it nothing it does not already know.

	// Kept as defence in depth rather than as the discriminating check it used to
	// be: holder came out of the registry, so it necessarily parses, and this can
	// only fire if a malformed id reached the serving copy off disk.
	if _, _, _, err := ParseOperatorID(operatorID); err != nil {
		return OperatorChallenge{}, fmt.Errorf("%w: %v", ErrUnknownOperator, err)
	}
	o, ok := s.reg.Get(operatorID)
	if !ok {
		// Racing a revocation between the resolve above and here. Refused rather
		// than assumed.
		return OperatorChallenge{}, fmt.Errorf("%w: %q is not registered on this bus", ErrUnknownOperator, operatorID)
	}
	if o.Revoked() {
		// Unreachable through step 2 (a revoked binding is not live) except by
		// racing a concurrent revocation; kept because it fails closed and because
		// this method must not depend on the read side to stay revocation-aware.
		return OperatorChallenge{}, fmt.Errorf("%w: %q was revoked at %s", ErrOperatorRevoked, operatorID, o.RevokedAt.UTC().Format(time.RFC3339Nano))
	}
	if err := s.checkCertBinding(o, certFP); err != nil {
		return OperatorChallenge{}, err
	}

	token, err := newToken()
	if err != nil {
		return OperatorChallenge{}, err
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Lazy sweep, exactly as Service.BeginSession does it: this is the only path
	// that grows the table, so it is the only path that needs to shrink it, and
	// a sweep here keeps the map bounded by what is LIVE rather than by
	// everything ever issued.
	s.sweepLocked(now)

	// The GLOBAL cap, FAILING CLOSED, leaving the table exactly as it found it.
	// There is no eviction: evicting would let one certificate holder destroy
	// another operator's issued challenge on demand.
	if len(s.sessions) >= s.maxSessions {
		return OperatorChallenge{}, fmt.Errorf("%w: the operator session table holds %d entries, the limit", ErrCapacity, s.maxSessions)
	}

	// THE PER-OPERATOR PENDING CAP, also FAILING CLOSED and also leaving the
	// table exactly as it found it. Without it the global cap above is a lockout
	// primitive between operators: one certificate holder parks maxSessions
	// pending challenges and no other operator can begin a session, on the one
	// plane whose job is to fix that kind of incident. See
	// DefaultMaxPendingOperatorSessionsPerOperator for why keying this on the
	// operator id is safe HERE and is not on Service.BeginSession
	// (AUTH-1-FU-PENDINGCAP): step 2 above has already proved the caller holds
	// this operator's certificate private key, so it can only fill its OWN
	// bucket. Nothing is evicted — evicting would let a certificate holder
	// destroy its own outstanding challenge and, worse, would be the shape of
	// code somebody later re-keys onto an unproven identity.
	pending := 0
	for _, other := range s.sessions {
		if other.State == SessionPending && other.OperatorID == operatorID {
			pending++
		}
	}
	if pending >= s.maxPendingSessionsPerOperator {
		return OperatorChallenge{}, fmt.Errorf("%w: operator %q holds %d pending challenges, at the per-operator limit of %d; complete or let one expire before asking for another, and note that no other operator's challenges are affected", ErrCapacity, operatorID, pending, s.maxPendingSessionsPerOperator)
	}

	sess := &OperatorSession{
		OperatorID:         operatorID,
		State:              SessionPending,
		CertFingerprint:    certFP,
		CreatedAt:          now,
		ChallengeExpiresAt: now.Add(ChallengeTTL),
		TokenHash:          tokenHash(token),
	}
	s.sessions[sess.TokenHash] = sess

	return OperatorChallenge{
		OperatorID:         operatorID,
		Token:              token,
		ChallengeExpiresAt: sess.ChallengeExpiresAt,
	}, nil
}

// CompleteSession verifies the operator's Ed25519 signature over
// OperatorSessionSigningContext + token and activates the session.
//
// It re-checks the certificate binding and re-checks revocation, both against
// the registry as it is NOW rather than as it was at BeginSession: an operator
// revoked in between must not be able to complete a challenge it already holds.
//
// # Why this needs no idempotency key, and why ExpiresAt is NEVER extended
//
// Completion is idempotent by construction (Service.CompleteSession's rule,
// unchanged). Re-completing an ALREADY ACTIVE session re-verifies the signature
// and returns the same session with the SAME ExpiresAt — the expiry is set
// exactly once, at the first successful completion. If a repeat completion
// refreshed it, an operator could hold one session open indefinitely off a
// single signature and the one-hour ceiling would be fiction.
//
// # Single attempt per PENDING challenge
//
// A failed verification of a pending session DELETES it: the client asks for
// another challenge, while an attacker gets no fixed target to grind signatures
// against. An ACTIVE session is deliberately NOT deleted on a failed
// verification — that would hand anyone who learned a token an instant way to
// destroy a live session.
func (s *OperatorService) CompleteSession(token string, signature []byte, certFP [32]byte) (OperatorSession, error) {
	hash := tokenHash(token)
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepLocked(now)

	sess, ok := s.sessions[hash]
	if !ok {
		// Covers "never existed", "already swept" and "deleted by a previous
		// failed attempt" identically, on purpose.
		return OperatorSession{}, fmt.Errorf("%w: no operator session for the presented token", ErrUnknownSession)
	}
	if operatorSessionExpired(sess, now) {
		delete(s.sessions, hash)
		return OperatorSession{}, fmt.Errorf("%w: the operator session for the presented token has expired", ErrUnknownSession)
	}

	o, ok := s.reg.Get(sess.OperatorID)
	if !ok {
		// The operator vanished between BeginSession and here. It cannot happen
		// with the shipped registry, which never removes — but treat it as
		// authoritative rather than assume: drop the session and report the
		// operator as unknown rather than authenticating a departed principal.
		delete(s.sessions, hash)
		return OperatorSession{}, fmt.Errorf("%w: %q is no longer registered", ErrUnknownOperator, sess.OperatorID)
	}
	if o.Revoked() {
		// Revoked between begin and complete. The pending session is dropped:
		// there is nothing it could ever become.
		delete(s.sessions, hash)
		return OperatorSession{}, fmt.Errorf("%w: %q was revoked at %s", ErrOperatorRevoked, sess.OperatorID, o.RevokedAt.UTC().Format(time.RFC3339Nano))
	}
	// RE-CHECKED, not trusted from BeginSession: the connection presenting the
	// signature is not necessarily the connection that took the challenge, and
	// invariant 11's cross-check is about THIS connection.
	if err := s.checkCertBinding(o, certFP); err != nil {
		return OperatorSession{}, err
	}
	if certFP != sess.CertFingerprint {
		return OperatorSession{}, fmt.Errorf("%w: the challenge for operator %q was issued over a different client certificate", ErrOperatorCertMismatch, sess.OperatorID)
	}

	// The length check that must precede EVERY ed25519.Verify: Verify PANICS on
	// a public key that is not exactly ed25519.PublicKeySize, while it merely
	// returns false for a bad signature. validateOperator already refuses a
	// wrong-size key on the way in AND on the way off disk, so this is defence
	// in depth — and it is not merely defensive, because the registry is rebuilt
	// FROM DISK, where a truncated record could otherwise turn into a panic.
	if len(o.AuthPublicKey) != ed25519.PublicKeySize {
		return OperatorSession{}, fmt.Errorf("%w: the registry holds a %d-byte public key for operator %q, want exactly %d", ErrInvalidPublicKey, len(o.AuthPublicKey), sess.OperatorID, ed25519.PublicKeySize)
	}
	// Checked explicitly rather than left to Verify: Verify would return false,
	// which is the right outcome, but naming the reason keeps the server log
	// able to tell a mis-encoded signature from a wrong key.
	if len(signature) != ed25519.SignatureSize {
		if sess.State == SessionPending {
			delete(s.sessions, hash)
		}
		return OperatorSession{}, fmt.Errorf("%w: got a %d-byte signature, want exactly %d", ErrBadSignature, len(signature), ed25519.SignatureSize)
	}

	if !ed25519.Verify(o.AuthPublicKey, []byte(OperatorSessionSigningContext+token), signature) {
		if sess.State == SessionPending {
			delete(s.sessions, hash)
		}
		return OperatorSession{}, fmt.Errorf("%w: for operator %q", ErrBadSignature, sess.OperatorID)
	}

	if sess.State == SessionActive {
		// Verified, and already live. Returned UNCHANGED — in particular with
		// its original ExpiresAt.
		//
		// This return must stay ABOVE the per-operator cap below, for
		// Service.CompleteSession's reason: re-completing an already-active
		// session creates NO new entry and is already counted in its bucket, so
		// refusing it would turn a safe retry into a failure (invariant 10).
		return *sess, nil
	}

	// THE PER-OPERATOR ACTIVE-SESSION CAP. It fails CLOSED and NEVER evicts, and
	// a refusal leaves the table exactly as it found it: the pending session is
	// NOT deleted (the single-attempt rule burns a challenge on a FAILED
	// VERIFICATION, and this signature verified).
	//
	// An operator-id key is safe HERE and would not be on the begin path, for
	// AUTH-1-FU-ACTIVECAP's reason: an entry can only be counted into a bucket
	// by someone who produced a valid Ed25519 signature with that operator's
	// PRIVATE key, so a flooder can only fill its OWN bucket.
	active := 0
	for _, other := range s.sessions {
		if other.State == SessionActive && other.OperatorID == sess.OperatorID {
			active++
		}
	}
	if active >= s.maxActiveSessionsPerOperator {
		return OperatorSession{}, fmt.Errorf("%w: operator %q holds %d active sessions, at the per-operator limit of %d; one of its OWN sessions must expire before another can be established, and none is evicted to make room", ErrCapacity, sess.OperatorID, active, s.maxActiveSessionsPerOperator)
	}

	sess.State = SessionActive
	sess.ExpiresAt = now.Add(SessionLifetime)
	return *sess, nil
}

// Authenticate resolves an opaque operator session token to the operator behind
// it. THIS IS THE AUTHORIZATION CHECK.
//
// # THE CERTIFICATE FINGERPRINT IS A REQUIRED PARAMETER, NOT A FOLLOW-UP CALL
//
// On the AGENT plane the invariant 11 cross-check lives in a SECOND function
// (httpapi.enforceCertBinding, over Service.AgentIDForClientCertificate), which
// means a caller can perform the token check and simply forget the other half —
// and a forgotten cross-check fails OPEN and passes every positive test. Here it
// is impossible to call the check without the fingerprint, because there is no
// signature that omits it. That is the whole reason this method takes six
// arguments' worth of work instead of returning a principal from a token.
//
// certFP is a TRANSPORT FACT from r.TLS, never a request field.
//
// EVERY STEP FAILS CLOSED, in this order:
//
//  1. certFP must be non-zero. The zero value is what a caller holds when there
//     was NO certificate, and the idiomatic slip `fp, _ := FromContext(ctx)`
//     produces it silently.
//  2. the token must resolve to a session that is ACTIVE and unexpired, against
//     the SERVER'S OWN CLOCK with NO SKEW GRACE. A grace window is just a longer
//     lifetime with a less honest name.
//  3. the operator must still be in the registry.
//  4. the operator must NOT be revoked — RE-READ FROM THE REGISTRY ON EVERY
//     CALL, with NO CACHE. This is what makes revocation take effect at the very
//     next request, and it is the property invariant 3 chose opaque server-side
//     handles for in the first place. A cached "is revoked" flag on the session
//     would reintroduce exactly the signed-claim behaviour that decision
//     rejected.
//  5. certFP must equal the operator's LIVE binding — invariant 11's
//     cross-check, applied UNNARROWED.
//  6. certFP must not resolve to a DIFFERENT live operator. Ambiguity refuses
//     rather than picks (LiveOperatorForCertFingerprint).
//
// A PENDING (unsigned) session is not a credential and is rejected exactly like
// an unknown one.
func (s *OperatorService) Authenticate(token string, certFP [32]byte) (OperatorPrincipal, error) {
	// 1. Before the token is even hashed: a caller with no certificate is not on
	// this plane at all.
	if certFP == ([32]byte{}) {
		return OperatorPrincipal{}, fmt.Errorf("%w: no client certificate was presented; the zero fingerprint is the ABSENCE of a certificate and names nobody, and an operator session is only ever valid over the certificate it is bound to (invariant 11)", ErrOperatorCertMismatch)
	}

	hash := tokenHash(token)
	now := s.now()

	s.mu.Lock()
	// 2. Token -> session, active and unexpired.
	sess, ok := s.sessions[hash]
	if !ok {
		s.mu.Unlock()
		return OperatorPrincipal{}, fmt.Errorf("%w: no operator session for the presented token", ErrUnknownSession)
	}
	if sess.State != SessionActive {
		s.mu.Unlock()
		return OperatorPrincipal{}, fmt.Errorf("%w: the operator session for the presented token has not completed its challenge", ErrUnknownSession)
	}
	if !now.Before(sess.ExpiresAt) {
		delete(s.sessions, hash)
		s.mu.Unlock()
		return OperatorPrincipal{}, fmt.Errorf("%w: the operator session for the presented token expired at %s", ErrUnknownSession, sess.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}
	operatorID := sess.OperatorID
	boundFP := sess.CertFingerprint
	expiresAt := sess.ExpiresAt
	s.mu.Unlock()

	// The registry is consulted OUTSIDE s.mu: the two locks are kept disjoint so
	// there is no ordering between them to invert, and OperatorRegistry.Revoke
	// calls back into RevokeSessions (which takes s.mu) from under its own
	// writeMu.

	// 3. The operator must still exist.
	o, ok := s.reg.Get(operatorID)
	if !ok {
		s.drop(hash)
		return OperatorPrincipal{}, fmt.Errorf("%w: %q is no longer registered", ErrUnknownOperator, operatorID)
	}
	// 4. And must not be revoked. Re-read every call, no cache.
	if o.Revoked() {
		s.drop(hash)
		return OperatorPrincipal{}, fmt.Errorf("%w: %q was revoked at %s", ErrOperatorRevoked, operatorID, o.RevokedAt.UTC().Format(time.RFC3339Nano))
	}

	// 5. The cross-check, unnarrowed: this connection's certificate must be THIS
	// operator's live binding.
	if err := s.checkCertBinding(o, certFP); err != nil {
		return OperatorPrincipal{}, err
	}
	if certFP != boundFP {
		// Redundant while an operator record carries exactly ONE fingerprint —
		// step 5 has already compared against the same value — and deliberately
		// kept, because it is the check that stays correct if an operator ever
		// carries a rotation pair. It costs one array comparison.
		return OperatorPrincipal{}, fmt.Errorf("%w: this session was established over a different client certificate of operator %q", ErrOperatorCertMismatch, operatorID)
	}

	// 6. And must not ALSO name somebody else. Ambiguity refuses rather than
	// picks: a certificate held live by two operators resolves to nobody.
	holder, err := s.reg.LiveOperatorForCertFingerprint(certFP)
	if err != nil {
		return OperatorPrincipal{}, fmt.Errorf("refusing an operator session over an unresolvable client certificate: %w", err)
	}
	if holder != operatorID {
		return OperatorPrincipal{}, fmt.Errorf("%w: the presented client certificate names operator %q, but the presented session token names %q", ErrOperatorCertMismatch, holder, operatorID)
	}

	return OperatorPrincipal{OperatorID: operatorID, Name: o.Name, ExpiresAt: expiresAt}, nil
}

// RevokeSessions drops every session belonging to operatorID — pending and
// active alike — and reports how many were dropped.
//
// It is what makes revocation FAST rather than merely eventual: an already
// issued token stops working the instant OperatorRegistry.Revoke commits, not at
// its natural expiry up to SessionLifetime later. It is also the reason
// invariant 3's "opaque server-side handles, not signed claims" is worth its
// cost — a signed claim could not be dropped from anywhere.
//
// Dropping PENDING sessions too is deliberate: a challenge issued to an operator
// that is now revoked must never be completable.
//
// Deleting from a map during range is defined behaviour in Go: an entry removed
// before it is reached is simply not produced.
func (s *OperatorService) RevokeSessions(operatorID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for hash, sess := range s.sessions {
		if sess.OperatorID == operatorID {
			delete(s.sessions, hash)
			n++
		}
	}
	return n
}

// SessionCount reports how many operator sessions are held, pending and active
// together. It exists for operators and tests; it is not part of the
// authentication path.
func (s *OperatorService) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// checkCertBinding is the ONE comparison behind invariant 11's cross-check on
// this plane: does this connection's certificate fingerprint equal the LIVE
// binding of THIS operator.
//
// It is a method with one implementation for the reason certFingerprintOwner is
// a free function: a fail-closed rule written out at three call sites (begin,
// complete, authenticate) is three chances for one of them to stop failing
// closed. The zero fingerprint is refused here as well as at Authenticate's
// door, because BeginSession and CompleteSession reach this without that check.
func (s *OperatorService) checkCertBinding(o Operator, certFP [32]byte) error {
	if certFP == ([32]byte{}) {
		return fmt.Errorf("%w: no client certificate was presented for operator %q; the zero fingerprint is the ABSENCE of a certificate and names nobody", ErrOperatorCertMismatch, o.OperatorID)
	}
	if certFP != o.CertFingerprint {
		// The mismatch does NOT name the expected fingerprint. A caller that
		// does not already hold the operator's certificate has no business
		// learning its digest from an error message.
		return fmt.Errorf("%w: the presented client certificate is not the one bound to operator %q", ErrOperatorCertMismatch, o.OperatorID)
	}
	return nil
}

// drop deletes one session by token hash. It exists so Authenticate can discard
// a session whose principal has gone or been revoked without holding s.mu across
// the registry read.
func (s *OperatorService) drop(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, hash)
}

// operatorSessionExpired reports whether sess is past the deadline that applies
// to its state: the challenge deadline while pending, the session deadline once
// active.
//
// The boundary instant counts as expired (!now.Before(deadline)), matching
// Authenticate and the agent-plane expired(): at exactly ExpiresAt the session
// is over.
func operatorSessionExpired(sess *OperatorSession, now time.Time) bool {
	if sess.State == SessionActive {
		return !now.Before(sess.ExpiresAt)
	}
	return !now.Before(sess.ChallengeExpiresAt)
}

// sweepLocked deletes every expired operator session. The caller must hold s.mu.
func (s *OperatorService) sweepLocked(now time.Time) {
	for hash, sess := range s.sessions {
		if operatorSessionExpired(sess, now) {
			delete(s.sessions, hash)
		}
	}
}
