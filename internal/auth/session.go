package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
)

const (
	// SessionLifetime is how long an ACTIVE session is valid, measured from the
	// instant its challenge was successfully completed.
	//
	// One hour is a CEILING, not a starting point. It may be shortened; it must
	// never be configurable ABOVE this value, because the whole reason a
	// short-lived session is acceptable in place of per-request signing is that
	// a stolen token stops working soon. Raise this and that argument
	// evaporates.
	SessionLifetime = time.Hour

	// SessionRefreshFraction is where in a session's life a well-behaved client
	// is expected to establish its next one: at 75% of SessionLifetime, so a
	// refresh has a quarter of the lifetime of slack to survive a failure, a
	// retry and a restart before anything expires. See RefreshAfter.
	SessionRefreshFraction = 0.75

	// ChallengeTTL is how long an UNSIGNED token stays completable. It bounds
	// the window in which an intercepted challenge is worth anything, and it
	// bounds how long unauthenticated callers can hold table space.
	ChallengeTTL = 2 * time.Minute

	// TokenRandBytes is the entropy in a session token, in bytes, from
	// crypto/rand. The token is an opaque handle whose only security property
	// is unguessability, so this is deliberately far above the point where
	// guessing stops being a strategy.
	TokenRandBytes = 32

	// SessionSigningContext is the DOMAIN SEPARATION prefix. The exact byte
	// string the client signs is SessionSigningContext + token, so a signature
	// produced for this protocol can never be replayed as a signature over
	// anything else the same AUTH keypair signs, and vice versa — a keypair
	// that signs bare, unprefixed server-chosen bytes is a signing oracle.
	//
	// "v1" here is the EXISTING HTTP API version already carried by this
	// server's /v1/ routes. No new version number is minted for this string,
	// and it moves only when that API version moves.
	//
	// The client must PIN this constant. It is deliberately NOT sent on the
	// wire: a client that learned the context from the server would sign
	// whatever a man in the middle chose to prefix.
	SessionSigningContext = "agent-bus:session-token:v1:"
)

// RefreshAfter reports how long after a session becomes active a client should
// establish its next one: SessionRefreshFraction of SessionLifetime.
//
// It is advice to the client and nothing more. Expiry is enforced server-side
// against the server's own clock, with no skew grace — see Authenticate.
func RefreshAfter() time.Duration {
	return time.Duration(float64(SessionLifetime) * SessionRefreshFraction)
}

// SessionState is where a session is in the challenge/response exchange.
type SessionState uint8

const (
	// SessionPending is a token that has been issued but not yet signed. It is
	// NOT a credential: Authenticate rejects it.
	SessionPending SessionState = iota + 1

	// SessionActive is a token whose signature verified. It is the credential.
	SessionActive
)

// String implements fmt.Stringer.
func (s SessionState) String() string {
	switch s {
	case SessionPending:
		return "pending"
	case SessionActive:
		return "active"
	default:
		return "unknown"
	}
}

// Challenge is what BeginSession hands back: the token the client must sign,
// and the deadline by which it must do so.
type Challenge struct {
	// AgentID is the agent this challenge was minted for.
	AgentID string

	// Token is the opaque session token. It is a LIVE CREDENTIAL once the
	// challenge is completed, so it must never be logged, echoed into an error,
	// or stored anywhere but the client's own token file.
	Token string

	// ChallengeExpiresAt is when an uncompleted challenge stops being
	// completable.
	ChallengeExpiresAt time.Time
}

// Session is one entry in the session table.
//
// Sessions are IN MEMORY ONLY and are lost on restart, deliberately and
// permanently — see the package doc. Losing one costs an agent a single
// challenge/response round trip.
type Session struct {
	// AgentID is the authenticated subject.
	AgentID string

	// State is SessionPending or SessionActive.
	State SessionState

	// CreatedAt is when the challenge was issued.
	CreatedAt time.Time

	// ChallengeExpiresAt bounds the pending state. It is not consulted once the
	// session is active.
	ChallengeExpiresAt time.Time

	// ExpiresAt bounds the active state. It is set ONCE, when the challenge is
	// first completed successfully, and is never extended afterwards.
	ExpiresAt time.Time

	// TokenHash is the hex SHA-256 of the token — the key this session is
	// stored under. The token itself is NOT held here: see tokenHash.
	TokenHash string
}

// Principal is the authenticated identity behind a live session. It is what
// AUTH-2's middleware will attach to a request context.
type Principal struct {
	// AgentID is the fully-qualified agent id (invariant 2). It is the routing
	// and authorization subject.
	AgentID string

	// ExpiresAt is when this session stops authenticating.
	ExpiresAt time.Time
}

// tokenHash is the session table's key: the hex SHA-256 of the token.
//
// The table is keyed by the hash and NOT by the token so that the server's own
// memory holds no directly usable live credential. A heap dump, a core file or
// a debug endpoint that leaked the map keys would leak hashes, and a hash
// cannot be presented as a token — the holder would have to invert SHA-256.
// This costs one hash per authenticated request and is worth it.
//
// It is NOT a password hash and does not need to be a slow one: the token is 32
// bytes of crypto/rand, so there is no low-entropy input for an attacker who
// obtained the hashes to guess at.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newToken mints an opaque session token: TokenRandBytes from crypto/rand,
// base64.RawURLEncoding so it is safe in a header, a URL and a shell variable
// without escaping.
//
// A crypto/rand failure is a HARD ERROR. There is deliberately no fallback to a
// weaker source: a predictable token is a forgeable credential, and failing the
// request loudly is strictly better than issuing one that looks fine and is
// guessable. (Contrast httpapi.NewRequestID, which does fall back to a counter
// — a request id is a correlation aid and grants nothing.)
func newToken() (string, error) {
	b := make([]byte, TokenRandBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: reading %d bytes from crypto/rand for a session token: %w; there is no weaker fallback, because a predictable token is a forgeable credential", TokenRandBytes, err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// BeginSession issues a challenge for agentID: a fresh opaque token, recorded
// PENDING, that the agent must sign with its enrolment private key and present
// to CompleteSession.
//
// agentID is untrusted input. It must be a well-formed fully-qualified id AND
// be in the roster; a malformed id and an unknown one both return
// ErrUnknownAgent, because the difference tells a caller probing for valid
// agents something it should not learn.
//
// This route is UNAUTHENTICATED — it is part of how a credential is obtained —
// so everything it allocates is bounded; see the caps below.
func (s *Service) BeginSession(agentID string) (Challenge, error) {
	// Parsed before anything else touches it, and outside the lock. Parsing
	// establishes only that the string is a well-formed id; it grants nothing,
	// and the roster check below is what decides the agent exists.
	if _, _, _, err := ids.ParseAgentID(agentID); err != nil {
		return Challenge{}, fmt.Errorf("%w: %s", ErrUnknownAgent, err)
	}
	if _, ok := s.roster.Get(agentID); !ok {
		return Challenge{}, fmt.Errorf("%w: %q is not enrolled on this bus", ErrUnknownAgent, agentID)
	}

	token, err := newToken()
	if err != nil {
		return Challenge{}, err
	}

	now := s.now()

	s.sessMu.Lock()
	defer s.sessMu.Unlock()

	// Lazy sweep. There is no background goroutine to reap expired sessions:
	// this is the only path that grows the table, so it is the only path that
	// needs to shrink it, and a sweep here keeps the map bounded by what is
	// LIVE rather than by everything ever issued. O(n) over a capped table.
	s.sweepLocked(now)

	// Global cap, checked BEFORE the per-agent eviction below. Fails CLOSED: a
	// session table that grew without limit is a memory exhaustion reachable by
	// an unauthenticated caller.
	//
	// The order matters for a reason that is not about capacity. If this ran
	// after the eviction, a call that ends up REFUSED would still have destroyed
	// the caller's earlier pending challenge on its way out — a failed call with
	// a side effect on the caller's own state, which is the worst kind. A
	// refusal now leaves the table exactly as it found it. The cost is that a
	// per-agent reclaim can no longer rescue a call made at the global cap;
	// that call is refused with ErrCapacity and the client retries, which is the
	// documented behaviour of every other cap here.
	if len(s.sessions) >= s.maxSessions {
		return Challenge{}, fmt.Errorf("%w: the session table holds %d entries, the limit", ErrCapacity, s.maxSessions)
	}

	// Per-agent pending cap. When an agent is over it we EVICT ITS OLDEST
	// PENDING CHALLENGE rather than refusing the new one.
	//
	// # This cap is NOT a defence against a flooder, and must not be read as one
	//
	// The cap is keyed on agentID, which is an ATTACKER-SUPPLIED VICTIM
	// IDENTIFIER: this route is unauthenticated, so anyone who knows a real
	// agent's id makes its BeginSession calls land in THAT agent's bucket.
	// Eviction therefore drops the VICTIM's correctly-issued challenge, not the
	// flooder's — MaxPendingPerAgent+1 anonymous calls per round are enough to
	// keep a named agent from ever completing an authentication. Refusing at the
	// cap instead is exactly the same lockout by the other route: the victim's
	// own BeginSession is what gets refused. There is no ordering of a bucket
	// keyed on the victim that is not a lockout primitive.
	//
	// Eviction is chosen only because it bounds memory WITHOUT also penalising
	// the common honest case — a client that legitimately retries BeginSession
	// keeps working, where refusal would start failing it at the cap. That is
	// the entire justification; it buys nothing against an attacker.
	//
	// The real mitigations, neither of them implemented yet and both filed as
	// follow-ups on AUTH-1, are: per-SOURCE rate limiting on the three
	// unauthenticated routes (the only thing that can charge the flooder rather
	// than the victim), or dropping this per-agent cap entirely and letting the
	// global cap above plus ChallengeTTL bound the memory on their own.
	for s.countPendingLocked(agentID) >= s.maxPendingPerAgent {
		oldest := s.oldestPendingLocked(agentID)
		if oldest == "" {
			// Cannot happen while the count is at the cap; break rather than
			// spin if it ever does.
			break
		}
		delete(s.sessions, oldest)
	}

	sess := &Session{
		AgentID:            agentID,
		State:              SessionPending,
		CreatedAt:          now,
		ChallengeExpiresAt: now.Add(ChallengeTTL),
		TokenHash:          tokenHash(token),
	}
	s.sessions[sess.TokenHash] = sess

	return Challenge{
		AgentID:            agentID,
		Token:              token,
		ChallengeExpiresAt: sess.ChallengeExpiresAt,
	}, nil
}

// CompleteSession verifies the client's signature over
// SessionSigningContext + token and activates the session.
//
// # Why this needs no idempotency key
//
// Completion is idempotent by construction. Re-completing an ALREADY ACTIVE
// session re-verifies the signature and returns the same session with the SAME
// ExpiresAt — the expiry is set exactly once, at the first successful
// completion, and is NEVER extended. If a repeat completion refreshed it, a
// client could hold one session open indefinitely off a single signature, and
// the one-hour ceiling that justifies bearer tokens at all would be fiction.
//
// # Single attempt per PENDING challenge
//
// A failed verification of a pending session DELETES that session. The client
// simply asks for another challenge, which is a cheap unauthenticated call,
// while an attacker gets no fixed target to grind signatures against and no way
// to hold table space with repeated failures. An ACTIVE session is deliberately
// NOT deleted on a failed verification: doing so would hand anyone who learned
// a token an instant way to destroy a live session.
func (s *Service) CompleteSession(token string, signature []byte) (Session, error) {
	hash := tokenHash(token)
	now := s.now()

	s.sessMu.Lock()
	defer s.sessMu.Unlock()

	s.sweepLocked(now)

	sess, ok := s.sessions[hash]
	if !ok {
		// Covers "never existed", "already swept" and "deleted by a previous
		// failed attempt" identically, on purpose.
		return Session{}, fmt.Errorf("%w: no session for the presented token", ErrUnknownSession)
	}
	if expired(sess, now) {
		delete(s.sessions, hash)
		return Session{}, fmt.Errorf("%w: the session for the presented token has expired", ErrUnknownSession)
	}

	entry, ok := s.roster.Get(sess.AgentID)
	if !ok {
		// The agent vanished between BeginSession and here. It cannot happen
		// with MemoryRoster, which never removes, but AUTH-4 introduces leave
		// and revocation, so treat it as authoritative: drop the session and
		// report the agent as unknown rather than authenticating a departed id.
		delete(s.sessions, hash)
		return Session{}, fmt.Errorf("%w: %q is no longer enrolled", ErrUnknownAgent, sess.AgentID)
	}

	// The length check that must precede EVERY ed25519.Verify: Verify PANICS on
	// a public key that is not exactly ed25519.PublicKeySize, while it merely
	// returns false for a bad signature. Enrol already rejects a wrong-size key
	// at the door, so this is defence in depth — and it stops being merely
	// defensive the moment AUTH-3 reloads this roster FROM DISK, where a
	// truncated or corrupt record could otherwise turn into a panic on an
	// unauthenticated route.
	if len(entry.PublicKey) != ed25519.PublicKeySize {
		return Session{}, fmt.Errorf("%w: the roster holds a %d-byte public key for %q, want exactly %d", ErrInvalidPublicKey, len(entry.PublicKey), sess.AgentID, ed25519.PublicKeySize)
	}
	// Checked explicitly rather than left to Verify: Verify would return false,
	// which is the right outcome, but naming the reason keeps the server log
	// able to tell a mis-encoded signature from a wrong key.
	if len(signature) != ed25519.SignatureSize {
		if sess.State == SessionPending {
			delete(s.sessions, hash)
		}
		return Session{}, fmt.Errorf("%w: got a %d-byte signature, want exactly %d", ErrBadSignature, len(signature), ed25519.SignatureSize)
	}

	if !ed25519.Verify(entry.PublicKey, []byte(SessionSigningContext+token), signature) {
		if sess.State == SessionPending {
			delete(s.sessions, hash)
		}
		return Session{}, fmt.Errorf("%w: for agent %q", ErrBadSignature, sess.AgentID)
	}

	if sess.State == SessionActive {
		// Verified, and already live. Return it UNCHANGED — in particular with
		// its original ExpiresAt. See the doc comment.
		return *sess, nil
	}

	sess.State = SessionActive
	sess.ExpiresAt = now.Add(SessionLifetime)
	return *sess, nil
}

// Authenticate resolves an opaque session token to the agent behind it.
//
// THIS IS THE SEAM AUTH-2 CONSUMES. It is the single place a bearer token
// becomes an identity, and AUTH-2's middleware wraps exactly this call. This
// task deliberately does NOT wire it into any middleware and enforces the token
// on NO route: exposing the check and enforcing it are separate changes, and
// keeping them separate is what makes the enforcement change reviewable on its
// own.
//
// Expiry is checked against the service's own clock on EVERY call, with NO SKEW
// GRACE. Server-side expiry is authoritative; a client's opinion of the time
// does not enter into it, and a grace window is just a longer lifetime with a
// less honest name.
//
// A pending (unsigned) session is not a credential and is rejected exactly like
// an unknown one.
func (s *Service) Authenticate(token string) (Principal, error) {
	hash := tokenHash(token)
	now := s.now()

	s.sessMu.Lock()
	defer s.sessMu.Unlock()

	sess, ok := s.sessions[hash]
	if !ok {
		return Principal{}, fmt.Errorf("%w: no session for the presented token", ErrUnknownSession)
	}
	if sess.State != SessionActive {
		return Principal{}, fmt.Errorf("%w: the session for the presented token has not completed its challenge", ErrUnknownSession)
	}
	if !now.Before(sess.ExpiresAt) {
		delete(s.sessions, hash)
		return Principal{}, fmt.Errorf("%w: the session for the presented token expired at %s", ErrUnknownSession, sess.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}
	return Principal{AgentID: sess.AgentID, ExpiresAt: sess.ExpiresAt}, nil
}

// SessionCount reports how many sessions are held, pending and active
// together. It exists for operators and tests; it is not part of the
// authentication path.
func (s *Service) SessionCount() int {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	return len(s.sessions)
}

// expired reports whether sess is past the deadline that applies to its state:
// the challenge deadline while pending, the session deadline once active.
//
// The boundary instant counts as expired (!now.Before(deadline)), matching
// Authenticate: at exactly ExpiresAt the session is over.
func expired(sess *Session, now time.Time) bool {
	if sess.State == SessionActive {
		return !now.Before(sess.ExpiresAt)
	}
	return !now.Before(sess.ChallengeExpiresAt)
}

// sweepLocked deletes every expired session. The caller must hold s.sessMu.
//
// Deleting from a map during range is defined behaviour in Go: an entry removed
// before it is reached is simply not produced.
func (s *Service) sweepLocked(now time.Time) {
	for hash, sess := range s.sessions {
		if expired(sess, now) {
			delete(s.sessions, hash)
		}
	}
}

// countPendingLocked counts agentID's outstanding challenges. The caller must
// hold s.sessMu, and should have swept first so expired challenges do not count
// against a live agent.
func (s *Service) countPendingLocked(agentID string) int {
	n := 0
	for _, sess := range s.sessions {
		if sess.State == SessionPending && sess.AgentID == agentID {
			n++
		}
	}
	return n
}

// oldestPendingLocked returns the table key of agentID's oldest outstanding
// challenge, or "" if it has none. The caller must hold s.sessMu.
//
// Ties are broken by the table key so the choice is deterministic rather than
// dependent on Go's randomised map iteration order — two challenges minted in
// the same clock tick would otherwise make eviction unpredictable, which is
// exactly the sort of thing that makes a test flake once a week.
func (s *Service) oldestPendingLocked(agentID string) string {
	var (
		oldestHash string
		oldestAt   time.Time
	)
	for hash, sess := range s.sessions {
		if sess.State != SessionPending || sess.AgentID != agentID {
			continue
		}
		if oldestHash == "" || sess.CreatedAt.Before(oldestAt) ||
			(sess.CreatedAt.Equal(oldestAt) && hash < oldestHash) {
			oldestHash, oldestAt = hash, sess.CreatedAt
		}
	}
	return oldestHash
}
