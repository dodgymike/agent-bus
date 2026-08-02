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
// so what it allocates is bounded, by the GLOBAL session cap and by nothing
// else. Entries leave the table only by EXPIRING, and the two states expire on
// very different scales: a pending challenge after ChallengeTTL, an active
// session only after SessionLifetime. See the cap check below for what that
// bound does and, just as importantly, does not protect against.
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

	// The GLOBAL session cap. It is the ONLY bound on how far an unauthenticated
	// caller can grow this table; expiry (ChallengeTTL while pending,
	// SessionLifetime once active) is what drains it. It fails CLOSED — a session
	// table that grew without limit is a memory exhaustion reachable without
	// credentials — and a refusal leaves the table exactly as it found it, so
	// this error path has no side effect on anybody's state.
	//
	// # There is deliberately NO per-agent cap
	//
	// A per-agent bucket could only be keyed on agentID, and agentID on this
	// unauthenticated route is an ATTACKER-SUPPLIED VICTIM IDENTIFIER: anyone who
	// knows a real agent's id makes its BeginSession calls land in THAT agent's
	// bucket. Whichever way such a bucket behaves at its limit, the result is a
	// lockout of the victim — EVICTING destroys the victim's own correctly-issued
	// challenge, REFUSING denies the victim its next one — so on the order of
	// cap+1 anonymous requests per round permanently stop a named agent
	// authenticating. There is no ordering of a victim-keyed bucket that is not a
	// lockout primitive, so the cap was removed outright rather than retuned
	// (AUTH-1-FU-PENDINGCAP).
	//
	// # What this does NOT solve, and what removing the cap made CHEAPER
	//
	// A flooder can still fill this table to maxSessions and deny session
	// establishment to EVERYONE until entries expire. That is not fixed here and
	// must not be read as fixed. Two honest caveats:
	//
	//   - Removing the cap made that flood CHEAPER, not merely no worse. Pending
	//     entries used to be bounded by cap × roster size, so exhausting the table
	//     first meant enrolling enough distinct ids; it is now directly reachable
	//     with maxSessions begins naming ONE known agent. The trade is still
	//     clearly worth it — roughly maxSessions/ChallengeTTL sustained requests
	//     per second to hold an UNTARGETED, unamplified, self-healing outage,
	//     against nine requests per round for a TARGETED, permanent, stealthy one
	//     — but it does raise the priority of the mitigation below.
	//   - ChallengeTTL is NOT the recovery bound in the worst case. It drains
	//     pending entries in two minutes, but nothing here caps ACTIVE sessions
	//     per agent, and enrolment is itself unauthenticated: an attacker that
	//     enrols its own agent can complete handshakes and fill the table with
	//     ACTIVE entries, which are reclaimed only after SessionLifetime. That
	//     costs far less traffic to hold and outlives the flood by an hour. It is
	//     a PRE-EXISTING gap — the removed cap counted only pending sessions and
	//     never protected against it — and is filed as AUTH-1-FU-ACTIVECAP. A cap
	//     keyed on agent id is SAFE there, and only there: an active session can
	//     exist only if someone proved possession of that agent's private key, so
	//     the key is a proven identity rather than an attacker-supplied one.
	//
	// The mitigation for the flood itself is per-SOURCE rate limiting — the only
	// thing that can charge the flooder rather than the victim — task
	// AUTH-1-FU-RATELIMIT.
	//
	// # What IS guaranteed
	//
	// Nothing an unauthenticated caller does can DESTROY a challenge already
	// issued to another agent. A challenge leaves this table by exactly three
	// routes, and the third requires the token: it expires (ChallengeTTL), it is
	// completed, or a completion attempt against it FAILS verification (see
	// CompleteSession's single-attempt rule). The token is 32 bytes of
	// crypto/rand and the table is keyed on its hash, so that third route is
	// reachable only by whoever holds the token — the agent itself, or someone
	// who observed it in flight. That unguessability is load-bearing now that no
	// other per-agent bound exists.
	if len(s.sessions) >= s.maxSessions {
		return Challenge{}, fmt.Errorf("%w: the session table holds %d entries, the limit", ErrCapacity, s.maxSessions)
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
