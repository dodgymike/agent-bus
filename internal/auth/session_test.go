package auth_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
)

// TestEnrollThenSessionRoundTrip is the whole documented exchange, end to end,
// with a real keypair: enrol -> BeginSession -> sign SessionSigningContext +
// token -> CompleteSession -> Authenticate.
//
// The direction of the signature is the point. The SERVER chooses the token, so
// the client never picks the bytes it signs and cannot pre-compute; the CLIENT
// signs, so the server holds no material that could forge the agent's calls.
func TestEnrollThenSessionRoundTrip(t *testing.T) {
	svc, clock := newService(t, auth.Options{})
	agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

	ch, err := svc.BeginSession(agentID)
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	if ch.AgentID != agentID {
		t.Errorf("challenge agent id = %q, want %q", ch.AgentID, agentID)
	}
	if ch.Token == "" {
		t.Fatal("BeginSession returned an empty token")
	}
	if want := epoch.Add(auth.ChallengeTTL); !ch.ChallengeExpiresAt.Equal(want) {
		t.Errorf("challenge expires at %s, want %s", ch.ChallengeExpiresAt, want)
	}

	t.Run("a pending token is not yet a credential", func(t *testing.T) {
		if _, err := svc.Authenticate(ch.Token); !errors.Is(err, auth.ErrUnknownSession) {
			t.Fatalf("Authenticate on a PENDING token err = %v, want one wrapping ErrUnknownSession: a token that was never signed is not a credential", err)
		}
	})

	sess, err := svc.CompleteSession(ch.Token, signToken(priv, ch.Token))
	if err != nil {
		t.Fatalf("CompleteSession with a correct signature: %v", err)
	}
	if sess.AgentID != agentID {
		t.Errorf("session agent id = %q, want %q", sess.AgentID, agentID)
	}
	if sess.State != auth.SessionActive {
		t.Errorf("session state = %v, want %v", sess.State, auth.SessionActive)
	}
	if want := epoch.Add(auth.SessionLifetime); !sess.ExpiresAt.Equal(want) {
		t.Errorf("session expires at %s, want %s", sess.ExpiresAt, want)
	}

	t.Run("the session is keyed by the token's hash, never by the token", func(t *testing.T) {
		sum := sha256.Sum256([]byte(ch.Token))
		if want := hex.EncodeToString(sum[:]); sess.TokenHash != want {
			t.Errorf("TokenHash = %q, want the hex SHA-256 of the token %q", sess.TokenHash, want)
		}
	})

	t.Run("Authenticate resolves the token to the enrolled agent", func(t *testing.T) {
		clock.Advance(30 * time.Minute)
		p, err := svc.Authenticate(ch.Token)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if p.AgentID != agentID {
			t.Errorf("principal agent id = %q, want %q", p.AgentID, agentID)
		}
		if !p.ExpiresAt.Equal(sess.ExpiresAt) {
			t.Errorf("principal expires at %s, want the session's %s", p.ExpiresAt, sess.ExpiresAt)
		}
	})

	t.Run("an unrelated random token authenticates nothing", func(t *testing.T) {
		for _, token := range []string{"", "not-a-token", ch.Token + "x"} {
			if _, err := svc.Authenticate(token); !errors.Is(err, auth.ErrUnknownSession) {
				t.Errorf("Authenticate(%q) err = %v, want one wrapping ErrUnknownSession", token, err)
			}
		}
	})
}

// TestEnrollThenSessionRejectsSignaturesThatProveNothing pins what a signature
// has to be over. The context prefix case is the one that matters most: if
// CompleteSession accepted a signature over the BARE token, domain separation
// would not be applied at all and the agent's AUTH keypair would be a signing
// oracle for server-chosen bytes.
func TestEnrollThenSessionRejectsSignaturesThatProveNothing(t *testing.T) {
	svc, _ := newService(t, auth.Options{})
	agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")
	_, otherPriv := newKeypair(t)

	cases := []struct {
		name    string
		sign    func(token string) []byte
		because string
	}{
		{
			name:    "over the bare token, without the signing context",
			sign:    func(token string) []byte { return ed25519.Sign(priv, []byte(token)) },
			because: "the exact byte string signed is SessionSigningContext + token; accepting the bare token makes the keypair a signing oracle",
		},
		{
			name: "over the context with a DIFFERENT token",
			sign: func(token string) []byte {
				return ed25519.Sign(priv, []byte(auth.SessionSigningContext+token+"-tampered"))
			},
			because: "the server chose the token; a signature over anything else proves possession of nothing this challenge asked for",
		},
		{
			name:    "by the wrong key",
			sign:    func(token string) []byte { return ed25519.Sign(otherPriv, []byte(auth.SessionSigningContext+token)) },
			because: "only the enrolment private key may activate the session (invariant 3)",
		},
		{
			name:    "context signed with a lowercase-different prefix",
			sign:    func(token string) []byte { return ed25519.Sign(priv, []byte("AGENT-BUS:SESSION-TOKEN:V1:"+token)) },
			because: "the context is an exact byte string, not a case-insensitive label",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ch, err := svc.BeginSession(agentID)
			if err != nil {
				t.Fatalf("BeginSession: %v", err)
			}
			var res auth.Session
			mustNotPanic(t, "CompleteSession "+tc.name, func() {
				res, err = svc.CompleteSession(ch.Token, tc.sign(ch.Token))
			})
			if !errors.Is(err, auth.ErrBadSignature) {
				t.Fatalf("err = %v, want one wrapping ErrBadSignature (%s)", err, tc.because)
			}
			if res.State != 0 {
				t.Errorf("a rejected completion returned session %+v, want the zero value", res)
			}
			if _, err := svc.Authenticate(ch.Token); !errors.Is(err, auth.ErrUnknownSession) {
				t.Errorf("the token authenticates after a rejected signature: err = %v, want one wrapping ErrUnknownSession", err)
			}
		})
	}
}

// TestEnrollThenSessionRejectsWrongLengthSignaturesCleanly checks the other
// half of the ed25519 trap. A wrong-size SIGNATURE makes Verify return false
// rather than panic, which is precisely why the public-key panic is so easy to
// miss — but the length is still checked explicitly so the log can tell a
// mis-encoded signature from a wrong key, and it must never panic either.
func TestEnrollThenSessionRejectsWrongLengthSignaturesCleanly(t *testing.T) {
	svc, _ := newService(t, auth.Options{})
	agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

	cases := []struct {
		name string
		sig  []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"one byte", []byte{0x01}},
		{"one byte short", make([]byte, ed25519.SignatureSize-1)},
		{"one byte long", make([]byte, ed25519.SignatureSize+1)},
		{"a correct signature with a byte appended", append(signToken(priv, "x"), 0x00)},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ch, err := svc.BeginSession(agentID)
			if err != nil {
				t.Fatalf("BeginSession: %v", err)
			}
			mustNotPanic(t, "CompleteSession with a "+tc.name+" signature", func() {
				_, err = svc.CompleteSession(ch.Token, tc.sig)
			})
			if !errors.Is(err, auth.ErrBadSignature) {
				t.Fatalf("err = %v, want one wrapping ErrBadSignature", err)
			}
		})
	}
}

// TestEnrollThenSessionFailedCompletionBurnsTheChallenge pins the single-attempt
// rule: one failed verification destroys the PENDING challenge, so an attacker
// gets no fixed target to grind signatures against. The honest client simply
// asks for another challenge, which is a cheap unauthenticated call.
func TestEnrollThenSessionFailedCompletionBurnsTheChallenge(t *testing.T) {
	svc, _ := newService(t, auth.Options{})
	agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

	ch, err := svc.BeginSession(agentID)
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	if _, err := svc.CompleteSession(ch.Token, signToken(priv, "some other token")); !errors.Is(err, auth.ErrBadSignature) {
		t.Fatalf("first attempt err = %v, want one wrapping ErrBadSignature", err)
	}

	// The CORRECT signature, on the same token, must now find nothing.
	if _, err := svc.CompleteSession(ch.Token, signToken(priv, ch.Token)); !errors.Is(err, auth.ErrUnknownSession) {
		t.Fatalf("second attempt err = %v, want one wrapping ErrUnknownSession: a failed verification deletes the pending challenge", err)
	}
	if n := svc.SessionCount(); n != 0 {
		t.Errorf("session table holds %d entries after a burned challenge, want 0: failed attempts must not hold table space", n)
	}

	t.Run("a fresh challenge still works", func(t *testing.T) {
		ch2, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession: %v", err)
		}
		if _, err := svc.CompleteSession(ch2.Token, signToken(priv, ch2.Token)); err != nil {
			t.Fatalf("completing a fresh challenge after a burned one: %v", err)
		}
	})

	t.Run("an ACTIVE session survives a bad signature", func(t *testing.T) {
		// Deliberately NOT symmetric with the pending case: deleting an active
		// session on a failed verification would hand anyone who learned a
		// token an instant way to destroy a live session.
		ch3, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession: %v", err)
		}
		if _, err := svc.CompleteSession(ch3.Token, signToken(priv, ch3.Token)); err != nil {
			t.Fatalf("activating: %v", err)
		}
		if _, err := svc.CompleteSession(ch3.Token, signToken(priv, "junk")); !errors.Is(err, auth.ErrBadSignature) {
			t.Fatalf("err = %v, want one wrapping ErrBadSignature", err)
		}
		if _, err := svc.Authenticate(ch3.Token); err != nil {
			t.Fatalf("the live session was destroyed by a bad signature: %v", err)
		}
	})
}

// TestEnrollThenSessionExpiryIsServerAuthoritative is the test that stops one
// signature being leveraged into an unbounded session.
//
// Two properties, both enforced against the server's OWN clock: an active
// session dies at its deadline with no skew grace, and re-completing an already
// active session returns the ORIGINAL expiry rather than a fresh one.
func TestEnrollThenSessionExpiryIsServerAuthoritative(t *testing.T) {
	t.Run("expiry is never extended by re-completing", func(t *testing.T) {
		svc, clock := newService(t, auth.Options{})
		agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

		ch, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession: %v", err)
		}
		sig := signToken(priv, ch.Token)

		first, err := svc.CompleteSession(ch.Token, sig)
		if err != nil {
			t.Fatalf("first completion: %v", err)
		}

		// Half an hour later the client presents the SAME signature again.
		clock.Advance(30 * time.Minute)
		second, err := svc.CompleteSession(ch.Token, sig)
		if err != nil {
			t.Fatalf("re-completing an active session must succeed: %v", err)
		}
		if !second.ExpiresAt.Equal(first.ExpiresAt) {
			t.Fatalf("re-completion moved the expiry from %s to %s; one signature would hold a session open forever and the one-hour ceiling would be fiction", first.ExpiresAt, second.ExpiresAt)
		}

		// And again, closer to the deadline.
		clock.Advance(29 * time.Minute)
		third, err := svc.CompleteSession(ch.Token, sig)
		if err != nil {
			t.Fatalf("third completion: %v", err)
		}
		if !third.ExpiresAt.Equal(first.ExpiresAt) {
			t.Fatalf("re-completion moved the expiry to %s, want the original %s", third.ExpiresAt, first.ExpiresAt)
		}
	})

	t.Run("the deadline instant itself is expired, with no grace", func(t *testing.T) {
		svc, clock := newService(t, auth.Options{})
		agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

		ch, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession: %v", err)
		}
		sess, err := svc.CompleteSession(ch.Token, signToken(priv, ch.Token))
		if err != nil {
			t.Fatalf("completing: %v", err)
		}

		// One nanosecond before: still live.
		clock.Advance(auth.SessionLifetime - time.Nanosecond)
		if _, err := svc.Authenticate(ch.Token); err != nil {
			t.Fatalf("session rejected 1ns before its deadline %s: %v", sess.ExpiresAt, err)
		}

		// Exactly at the deadline: over. A grace window is just a longer
		// lifetime with a less honest name.
		clock.Advance(time.Nanosecond)
		if _, err := svc.Authenticate(ch.Token); !errors.Is(err, auth.ErrUnknownSession) {
			t.Fatalf("err = %v at exactly ExpiresAt, want one wrapping ErrUnknownSession", err)
		}
		if n := svc.SessionCount(); n != 0 {
			t.Errorf("session table holds %d entries after expiry, want 0", n)
		}
	})

	t.Run("an expired session cannot be revived by re-presenting the signature", func(t *testing.T) {
		svc, clock := newService(t, auth.Options{})
		agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

		ch, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession: %v", err)
		}
		sig := signToken(priv, ch.Token)
		if _, err := svc.CompleteSession(ch.Token, sig); err != nil {
			t.Fatalf("completing: %v", err)
		}

		clock.Advance(auth.SessionLifetime)
		if _, err := svc.CompleteSession(ch.Token, sig); !errors.Is(err, auth.ErrUnknownSession) {
			t.Fatalf("err = %v, want one wrapping ErrUnknownSession: an expired session is gone, and re-signing does not resurrect it", err)
		}
	})
}

// TestEnrollThenSessionChallengeTTL bounds how long an UNSIGNED token stays
// completable — the window in which an intercepted challenge is worth anything,
// and how long an unauthenticated caller can hold table space.
func TestEnrollThenSessionChallengeTTL(t *testing.T) {
	t.Run("just inside the TTL the challenge still completes", func(t *testing.T) {
		svc, clock := newService(t, auth.Options{})
		agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

		ch, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession: %v", err)
		}
		clock.Advance(auth.ChallengeTTL - time.Nanosecond)
		if _, err := svc.CompleteSession(ch.Token, signToken(priv, ch.Token)); err != nil {
			t.Fatalf("completing 1ns before the challenge deadline: %v", err)
		}
	})

	t.Run("at the TTL the challenge is gone", func(t *testing.T) {
		svc, clock := newService(t, auth.Options{})
		agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

		ch, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession: %v", err)
		}
		clock.Advance(auth.ChallengeTTL)
		if _, err := svc.CompleteSession(ch.Token, signToken(priv, ch.Token)); !errors.Is(err, auth.ErrUnknownSession) {
			t.Fatalf("err = %v, want one wrapping ErrUnknownSession", err)
		}
		if n := svc.SessionCount(); n != 0 {
			t.Errorf("session table holds %d entries after the challenge expired, want 0", n)
		}
	})
}

// TestEnrollThenSessionBeginRejectsUnknownAgents pins that a malformed id and
// an unenrolled one are indistinguishable from the outside: the difference
// tells a caller probing for valid agents something it should not learn.
func TestEnrollThenSessionBeginRejectsUnknownAgents(t *testing.T) {
	svc, _ := newService(t, auth.Options{})
	enrolAgent(t, svc, "alpha", "idem-1")

	cases := []struct {
		name    string
		agentID string
	}{
		{"empty", ""},
		{"not qualified with a bus", "alpha-1"},
		{"no minted suffix", testBusID + ".alpha"},
		{"zero suffix", testBusID + ".alpha-0"},
		{"a second dot in the name", testBusID + ".al.pha-1"},
		{"well formed but not enrolled", testBusID + ".ghost-1"},
		{"right name, wrong suffix", testBusID + ".alpha-2"},
		{"right agent on a different bus", "some-other-bus.alpha-1"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var err error
			mustNotPanic(t, "BeginSession("+tc.name+")", func() {
				_, err = svc.BeginSession(tc.agentID)
			})
			if !errors.Is(err, auth.ErrUnknownAgent) {
				t.Fatalf("err = %v, want one wrapping ErrUnknownAgent", err)
			}
		})
	}

	if n := svc.SessionCount(); n != 0 {
		t.Errorf("session table holds %d entries after only rejected begins, want 0", n)
	}
}

// TestEnrollThenSessionPendingCapEvictsTheOldest pins the per-agent cap's
// choice: it EVICTS the oldest pending challenge rather than refusing the new
// one.
//
// It pins the BEHAVIOUR only, and claims no security property for it. The cap
// is keyed on agentID, which an unauthenticated caller supplies, so eviction
// drops whatever is oldest in the VICTIM's bucket — including the victim's own
// correctly-issued challenge. Eviction is chosen because it bounds memory
// without failing a client that legitimately retries, not because it defends
// against a flood; see the long comment on the cap in BeginSession for the
// trade-off and the mitigations that would actually address it.
func TestEnrollThenSessionPendingCapEvictsTheOldest(t *testing.T) {
	svc, clock := newService(t, auth.Options{MaxPendingPerAgent: 2})
	agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

	begin := func() auth.Challenge {
		t.Helper()
		ch, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession: %v", err)
		}
		return ch
	}

	first := begin()
	clock.Advance(time.Second)
	second := begin()
	clock.Advance(time.Second)
	third := begin()

	if n := svc.SessionCount(); n != 2 {
		t.Fatalf("session table holds %d entries, want 2 (the per-agent cap)", n)
	}

	// The OLDEST went, and it is the one that cannot complete.
	if _, err := svc.CompleteSession(first.Token, signToken(priv, first.Token)); !errors.Is(err, auth.ErrUnknownSession) {
		t.Fatalf("the oldest challenge err = %v, want one wrapping ErrUnknownSession: it should have been evicted", err)
	}

	// The two newest both still complete — the cap must not have taken them.
	if _, err := svc.CompleteSession(second.Token, signToken(priv, second.Token)); err != nil {
		t.Fatalf("the second-oldest challenge was evicted too: %v", err)
	}
	if _, err := svc.CompleteSession(third.Token, signToken(priv, third.Token)); err != nil {
		t.Fatalf("the newest challenge could not be completed: %v", err)
	}

	t.Run("one agent's flood does not evict another agent's challenge", func(t *testing.T) {
		otherID, otherPriv := enrolAgent(t, svc, "beta", "idem-2")
		otherCh, err := svc.BeginSession(otherID)
		if err != nil {
			t.Fatalf("BeginSession for beta: %v", err)
		}
		for i := 0; i < 5; i++ {
			clock.Advance(time.Second)
			begin()
		}
		if _, err := svc.CompleteSession(otherCh.Token, signToken(otherPriv, otherCh.Token)); err != nil {
			t.Fatalf("beta's challenge was collateral damage from alpha's flood: %v", err)
		}
	})
}

// TestEnrollThenSessionGlobalCapFailsClosed pins that the session table refuses
// rather than growing without limit: it is memory an UNAUTHENTICATED caller can
// allocate.
func TestEnrollThenSessionGlobalCapFailsClosed(t *testing.T) {
	svc, _ := newService(t, auth.Options{MaxSessions: 2, MaxPendingPerAgent: 8})

	a, _ := enrolAgent(t, svc, "alpha", "idem-1")
	b, _ := enrolAgent(t, svc, "beta", "idem-2")
	c, _ := enrolAgent(t, svc, "gamma", "idem-3")

	for _, id := range []string{a, b} {
		if _, err := svc.BeginSession(id); err != nil {
			t.Fatalf("BeginSession(%q): %v", id, err)
		}
	}
	if _, err := svc.BeginSession(c); !errors.Is(err, auth.ErrCapacity) {
		t.Fatalf("err = %v, want one wrapping ErrCapacity", err)
	}
	if n := svc.SessionCount(); n != 2 {
		t.Errorf("session table holds %d entries, want 2", n)
	}

	// A REFUSED call must leave the table exactly as it found it. The global cap
	// is therefore checked BEFORE the per-agent eviction: with the checks the
	// other way round, a caller already at its per-agent cap would have its
	// oldest challenge destroyed on the way to being told the server is full —
	// a failed call with a side effect on the caller's own state.
	t.Run("a refusal at the global cap destroys no pending challenge", func(t *testing.T) {
		svc, _ := newService(t, auth.Options{MaxSessions: 2, MaxPendingPerAgent: 1})
		alpha, alphaPriv := enrolAgent(t, svc, "alpha", "idem-1")
		beta, _ := enrolAgent(t, svc, "beta", "idem-2")

		ch, err := svc.BeginSession(alpha)
		if err != nil {
			t.Fatalf("BeginSession(alpha): %v", err)
		}
		if _, err := svc.BeginSession(beta); err != nil {
			t.Fatalf("BeginSession(beta): %v", err)
		}

		// alpha is at its per-agent cap AND the table is at the global cap.
		if _, err := svc.BeginSession(alpha); !errors.Is(err, auth.ErrCapacity) {
			t.Fatalf("err = %v, want one wrapping ErrCapacity", err)
		}
		if n := svc.SessionCount(); n != 2 {
			t.Errorf("session table holds %d entries after a refused call, want 2 unchanged", n)
		}
		if _, err := svc.CompleteSession(ch.Token, signToken(alphaPriv, ch.Token)); err != nil {
			t.Fatalf("alpha's pending challenge was destroyed by its own REFUSED BeginSession: %v", err)
		}
	})
}

// TestEnrollThenSessionLifetimeCeiling pins the constants themselves, so a
// later edit that raises the ceiling fails a test rather than passing review.
//
// One hour is a CEILING. The whole argument for accepting a bearer token in
// place of per-request signing is that a stolen one stops working soon; raise
// this and that argument evaporates.
func TestEnrollThenSessionLifetimeCeiling(t *testing.T) {
	if auth.SessionLifetime <= 0 {
		t.Fatalf("SessionLifetime = %s, must be positive", auth.SessionLifetime)
	}
	if auth.SessionLifetime > time.Hour {
		t.Fatalf("SessionLifetime = %s, but ONE HOUR IS A CEILING: a session may be shortened, never lengthened past an hour", auth.SessionLifetime)
	}
	if want := time.Duration(float64(auth.SessionLifetime) * 0.75); auth.RefreshAfter() != want {
		t.Errorf("RefreshAfter() = %s, want %s (75%% of the lifetime, leaving a quarter of it as slack for a failed refresh)", auth.RefreshAfter(), want)
	}
	if auth.RefreshAfter() >= auth.SessionLifetime {
		t.Error("RefreshAfter() is not strictly inside the lifetime, so a client following the advice would refresh too late")
	}
	if auth.ChallengeTTL <= 0 || auth.ChallengeTTL >= auth.SessionLifetime {
		t.Errorf("ChallengeTTL = %s, want a positive value well inside the session lifetime", auth.ChallengeTTL)
	}
	if auth.TokenRandBytes < 16 {
		t.Errorf("TokenRandBytes = %d; a token's only security property is unguessability", auth.TokenRandBytes)
	}
	if auth.SessionSigningContext == "" {
		t.Error("SessionSigningContext is empty, so signatures carry no domain separation at all")
	}
}

// TestEnrollThenSessionTokensAreUnique guards the one property an opaque handle
// has to have. Two challenges must never collide, or one agent's session would
// authenticate as another.
func TestEnrollThenSessionTokensAreUnique(t *testing.T) {
	svc, _ := newService(t, auth.Options{MaxPendingPerAgent: 256, MaxSessions: 1024})
	agentID, _ := enrolAgent(t, svc, "alpha", "idem-1")

	const n = 128
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		ch, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession %d: %v", i, err)
		}
		if seen[ch.Token] {
			t.Fatalf("token %q was issued twice", ch.Token)
		}
		seen[ch.Token] = true
	}
}
