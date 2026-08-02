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

// TestSessionBeginNoVictimLockout is the ADVERSARIAL test for the per-agent
// pending cap. The invariant: an UNAUTHENTICATED flooder that calls
// BeginSession with a VICTIM's agent_id must never be able to stop that victim
// from completing an authentication.
//
// The attack it guards against: /v1/session/begin is unauthenticated and
// agent_id is attacker-supplied, so a flooder's challenges land in the VICTIM's
// bucket and eviction under a cap keyed on agent_id drops the victim's own
// correctly-issued challenge. The cap this replaced defaulted to 8, so NINE
// anonymous requests per round were enough to lock a named agent out
// permanently. Refusing at the cap instead is the same lockout by the other
// route. The cap was therefore removed outright (AUTH-1-FU-PENDINGCAP) rather
// than retuned, and this test is what stops one being reintroduced.
//
// What this test does NOT claim: it does not claim the unauthenticated surface
// is flood-proof. A flooder can still consume the GLOBAL session table, which
// is per-source rate limiting's problem (task AUTH-1-FU-RATELIMIT, public_id
// 42670f8b-ab58-491d-a8cf-04a6e92185f1), not this one. The property here is
// narrower and behavioural: no amount of traffic naming the victim may destroy
// the victim's ability to authenticate.
func TestSessionBeginNoVictimLockout(t *testing.T) {
	// floodSize is deliberately hard-coded rather than derived from a cap
	// constant: this test must keep meaning after the per-agent cap is removed.
	// 32 is far past the old cap of 8 (and past the 2*8 the property needs), so
	// any bucket keyed on the victim would have turned over several times.
	const floodSize = 32

	// MaxSessions is set far above the traffic this test generates so the GLOBAL
	// cap is deliberately NOT the binding constraint — an ErrCapacity refusal
	// here would be measuring the wrong limit. The global cap is a separate,
	// non-amplified bound and is covered by
	// TestEnrollThenSessionGlobalCapFailsClosed.
	opts := auth.Options{MaxSessions: 4096}

	// flood is the attacker: unauthenticated BeginSession calls naming someone
	// else's agent id. It holds no key and never completes anything; its only
	// input is the victim's public, well-known id.
	flood := func(t *testing.T, svc *auth.Service, clock *fakeClock, targetID string) {
		t.Helper()
		for i := 0; i < floodSize; i++ {
			if _, err := svc.BeginSession(targetID); err != nil {
				t.Fatalf("flood BeginSession(%q) #%d: %v (the global cap must not be what this test measures)", targetID, i, err)
			}
			// A tick per call so the flood's challenges have strictly ordered
			// CreatedAt values and eviction is deterministic.
			clock.Advance(time.Millisecond)
		}
	}

	t.Run("the victim's outstanding challenge survives a flood naming it", func(t *testing.T) {
		svc, clock := newService(t, opts)
		victim, victimPriv := enrolAgent(t, svc, "victim", "idem-victim")

		// The victim asks for its challenge and is now signing it — the window
		// between begin and complete is exactly one client round trip.
		ch, err := svc.BeginSession(victim)
		if err != nil {
			t.Fatalf("victim BeginSession: %v", err)
		}

		flood(t, svc, clock, victim)

		if _, err := svc.CompleteSession(ch.Token, signToken(victimPriv, ch.Token)); err != nil {
			t.Fatalf("the victim could not complete its own challenge after %d unauthenticated begins naming it: %v; an unauthenticated caller must never be able to destroy a named agent's pending challenge", floodSize, err)
		}
		if _, err := svc.Authenticate(ch.Token); err != nil {
			t.Fatalf("the victim's completed session does not authenticate: %v", err)
		}
	})

	t.Run("a sustained flood over many rounds never locks the victim out", func(t *testing.T) {
		svc, clock := newService(t, opts)
		victim, victimPriv := enrolAgent(t, svc, "victim", "idem-victim")

		const rounds = 4
		for round := 0; round < rounds; round++ {
			// Flooding before the victim's begin AND during its signing round
			// trip: a real flood is continuous, not a single burst.
			flood(t, svc, clock, victim)

			ch, err := svc.BeginSession(victim)
			if err != nil {
				t.Fatalf("round %d: victim BeginSession: %v", round, err)
			}
			clock.Advance(time.Second)

			flood(t, svc, clock, victim)

			if _, err := svc.CompleteSession(ch.Token, signToken(victimPriv, ch.Token)); err != nil {
				t.Fatalf("round %d: victim CompleteSession: %v; %d anonymous begins per round must not be a lockout primitive", round, err, floodSize)
			}
			p, err := svc.Authenticate(ch.Token)
			if err != nil {
				t.Fatalf("round %d: victim Authenticate: %v", round, err)
			}
			if p.AgentID != victim {
				t.Fatalf("round %d: principal agent id = %q, want the victim %q", round, p.AgentID, victim)
			}
			clock.Advance(time.Second)
		}
	})

	t.Run("no third party can destroy an already-issued challenge", func(t *testing.T) {
		// Guards a future regression that keys the cap globally-but-wrongly: a
		// flood naming a DIFFERENT enrolled agent must also leave the victim's
		// pending challenge intact.
		svc, clock := newService(t, opts)
		victim, victimPriv := enrolAgent(t, svc, "victim", "idem-victim")
		other, otherPriv := enrolAgent(t, svc, "other", "idem-other")

		ch, err := svc.BeginSession(victim)
		if err != nil {
			t.Fatalf("victim BeginSession: %v", err)
		}
		otherCh, err := svc.BeginSession(other)
		if err != nil {
			t.Fatalf("other BeginSession: %v", err)
		}

		flood(t, svc, clock, other)
		flood(t, svc, clock, victim)

		if _, err := svc.CompleteSession(ch.Token, signToken(victimPriv, ch.Token)); err != nil {
			t.Fatalf("the victim's challenge was collateral damage from floods naming it and another agent: %v", err)
		}
		if _, err := svc.CompleteSession(otherCh.Token, signToken(otherPriv, otherCh.Token)); err != nil {
			t.Fatalf("the second agent's challenge did not survive either: %v", err)
		}
	})

	// The two subtests below extend the same adversarial property to the
	// per-agent ACTIVE-session cap added by AUTH-1-FU-ACTIVECAP. A per-agent
	// bucket is exactly the shape of thing this test exists to forbid, so the
	// new one has to earn its place behaviourally: the bucket must be keyed on a
	// PROVEN identity, which means a flooder can fill its OWN and nobody else's.
	//
	// activeCap is small on purpose. The flood is floodSize (32) begins per
	// round, eight times the cap, so a bucket that counted ANY of the flooder's
	// unauthenticated calls against the victim would have overflowed many times
	// over before the victim's first handshake completed.
	const activeCap = 4

	// As above, MaxSessions is far beyond this test's traffic so the GLOBAL cap
	// is deliberately NOT the binding constraint — an ErrCapacity here must be
	// the per-agent cap or the test is measuring the wrong limit.
	capOpts := auth.Options{MaxSessions: opts.MaxSessions, MaxActiveSessionsPerAgent: activeCap}

	t.Run("a flood naming the victim cannot consume the victim's ACTIVE bucket", func(t *testing.T) {
		svc, clock := newService(t, capOpts)
		victim, victimPriv := enrolAgent(t, svc, "victim", "idem-victim")

		established := 0
		for i := 0; i < activeCap; i++ {
			// A fresh burst before EVERY one of the victim's handshakes: a real
			// flood is continuous. The flooder holds no private key, so not one
			// of its pending entries can ever become an ACTIVE entry — and the
			// victim must therefore still get its FULL cap, not one slot less.
			flood(t, svc, clock, victim)

			ch, err := svc.BeginSession(victim)
			if err != nil {
				t.Fatalf("victim BeginSession #%d: %v", i+1, err)
			}
			if _, err := svc.CompleteSession(ch.Token, signToken(victimPriv, ch.Token)); err != nil {
				t.Fatalf("the victim could not establish active session %d of %d after %d anonymous begins naming it per round: %v; an unauthenticated flooder must never be able to spend a slot of the victim's ACTIVE bucket, because it cannot produce the signature that puts one there", i+1, activeCap, floodSize, err)
			}
			if _, err := svc.Authenticate(ch.Token); err != nil {
				t.Fatalf("victim session %d does not authenticate: %v", i+1, err)
			}
			established++
		}
		if established == 0 {
			t.Fatal("the victim established NO sessions at all, so this test probed nothing about its bucket")
		}
		if established != activeCap {
			t.Fatalf("the victim established %d active sessions, want its full cap of %d despite %d anonymous begins per round", established, activeCap, floodSize)
		}
	})

	t.Run("a flooder that fills its OWN active bucket cannot stop the victim", func(t *testing.T) {
		// Containment, asserted in one place: the attacker's refusal AND the
		// victim's success on the SAME service. Asserting only the refusal would
		// prove the cap counts; asserting both is what proves it is
		// self-inflicted.
		svc, _ := newService(t, capOpts)
		victim, victimPriv := enrolAgent(t, svc, "victim", "idem-victim")
		attacker, attackerPriv := enrolAgent(t, svc, "attacker", "idem-attacker")

		establishActive(t, svc, attacker, attackerPriv, activeCap)

		ch, err := svc.BeginSession(attacker)
		if err != nil {
			t.Fatalf("attacker BeginSession past its cap: %v", err)
		}
		if _, err := svc.CompleteSession(ch.Token, signToken(attackerPriv, ch.Token)); !errors.Is(err, auth.ErrCapacity) {
			t.Fatalf("the attacker's completion number %d err = %v, want one wrapping ErrCapacity: one enrolled identity must not be able to take an unbounded share of the session table", activeCap+1, err)
		}

		// And now the whole point: the victim, on the same service, is entirely
		// unaffected and still gets its OWN full cap.
		held := establishActive(t, svc, victim, victimPriv, activeCap)
		authenticated := 0
		for i, hs := range held {
			p, err := svc.Authenticate(hs.token)
			if err != nil {
				t.Fatalf("victim session %d of %d does not authenticate while the attacker sits at its cap: %v; the cap must contain the agent that hit it and nobody else", i+1, len(held), err)
			}
			if p.AgentID != victim {
				t.Fatalf("victim session %d resolved to %q, want the victim %q", i+1, p.AgentID, victim)
			}
			authenticated++
		}
		if authenticated == 0 {
			t.Fatal("no victim session was authenticated, so containment was never actually probed")
		}
	})
}

// activeHandshake is one completed begin+complete round trip, retained so a test
// can re-present the EXACT token and signature a real client would retry with.
// A client holds nothing else, so neither does this.
type activeHandshake struct {
	token string
	sig   []byte
	sess  auth.Session
}

// establishActive drives n full handshakes for agentID and requires every one of
// them to succeed.
//
// It is the fixture for the per-agent ACTIVE-session cap, and it fails loudly
// rather than returning short: filling a bucket is the precondition of nearly
// every assertion about that cap, so a fixture that quietly established FEWER
// sessions than asked would leave the refusal that follows measuring nothing at
// all.
func establishActive(t *testing.T, svc *auth.Service, agentID string, priv ed25519.PrivateKey, n int) []activeHandshake {
	t.Helper()
	if n <= 0 {
		t.Fatalf("establishActive(%q, %d): a fixture that establishes nothing makes every assertion built on it vacuous", agentID, n)
	}
	out := make([]activeHandshake, 0, n)
	for i := 0; i < n; i++ {
		ch, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession #%d for %q: %v", i+1, agentID, err)
		}
		sig := signToken(priv, ch.Token)
		sess, err := svc.CompleteSession(ch.Token, sig)
		if err != nil {
			t.Fatalf("CompleteSession #%d of %d for %q: %v; the agent is still under its per-agent cap here, so this must succeed", i+1, n, agentID, err)
		}
		if sess.State != auth.SessionActive {
			t.Fatalf("CompleteSession #%d for %q returned state %v, want %v", i+1, agentID, sess.State, auth.SessionActive)
		}
		out = append(out, activeHandshake{token: ch.Token, sig: sig, sess: sess})
	}
	if len(out) != n {
		t.Fatalf("establishActive built %d handshakes for %q, want %d", len(out), agentID, n)
	}
	return out
}

// activeCapMaxSessions is the GLOBAL session cap used throughout
// TestSessionActiveCap: far above anything these tests generate, so the global
// bound is provably not what any refusal below is measuring. An ErrCapacity from
// the wrong limit would make the whole test a lie, so the tests assert the table
// is short of this value before they assert a refusal.
const activeCapMaxSessions = 4096

// TestSessionActiveCap pins the per-agent ACTIVE-session cap
// (AUTH-1-FU-ACTIVECAP): one agent may hold at most MaxActiveSessionsPerAgent
// active sessions at once, the completion that would exceed that is REFUSED
// rather than served by evicting anything, and the refusal costs the agent
// nothing it already held.
//
// What breaks if this regresses: an active session is reclaimed only after
// SessionLifetime (an hour), where a pending challenge goes after ChallengeTTL
// (two minutes). With no per-agent bound, a SINGLE enrolment can complete
// MaxSessions handshakes and hold the entire session table for an hour at a few
// requests per second, denying session establishment to every other agent long
// after the flood itself stops. The cap raises the cost of that from one
// enrolment to ceil(MaxSessions/cap) distinct ones.
//
// Why an agent-id key is legitimate here when AUTH-1-FU-PENDINGCAP removed one
// from BeginSession: an entry enters an agent's bucket only after an Ed25519
// signature over SessionSigningContext+token verified against that agent's
// enrolment key, so the key is a PROVEN identity and a flooder can fill its OWN
// bucket and nobody else's. The adversarial half of that claim lives in
// TestSessionBeginNoVictimLockout; this test pins the mechanics.
func TestSessionActiveCap(t *testing.T) {
	t.Run("an agent gets exactly its cap of active sessions and not one more", func(t *testing.T) {
		cases := []struct {
			name  string
			limit int
		}{
			{"a cap of one", 1},
			{"a cap of two", 2},
			{"a cap of five", 5},
		}

		probed := 0
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				svc, _ := newService(t, auth.Options{MaxSessions: activeCapMaxSessions, MaxActiveSessionsPerAgent: tc.limit})
				agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

				held := establishActive(t, svc, agentID, priv, tc.limit)

				// The global cap is nowhere near binding, so the ErrCapacity
				// asserted below can only be the per-agent one.
				if n := svc.SessionCount(); n >= activeCapMaxSessions {
					t.Fatalf("the session table holds %d of a global %d entries, so an ErrCapacity below could be the GLOBAL cap rather than the per-agent one this test claims to measure", n, activeCapMaxSessions)
				}

				ch, err := svc.BeginSession(agentID)
				if err != nil {
					t.Fatalf("BeginSession past the cap: %v; the cap bounds ACTIVE sessions, and issuing a challenge is not one", err)
				}
				beforeCompletion := svc.SessionCount()

				res, err := svc.CompleteSession(ch.Token, signToken(priv, ch.Token))
				if !errors.Is(err, auth.ErrCapacity) {
					t.Fatalf("completion number %d for an agent capped at %d: err = %v, want one wrapping ErrCapacity; an unbounded per-agent share of the session table is what lets one enrolment deny session establishment to everyone else", tc.limit+1, tc.limit, err)
				}
				if res.State != 0 {
					t.Errorf("a refused completion returned session %+v, want the zero value", res)
				}
				if n := svc.SessionCount(); n != beforeCompletion {
					t.Errorf("the session table went from %d to %d entries across a REFUSED completion, want it unchanged: the refusal mutates nothing, and in particular does not burn the pending challenge (that rule is for a FAILED verification, and this signature verified)", beforeCompletion, n)
				}

				// It NEVER evicts. Every session the agent already held is
				// still a credential.
				// live counts sessions that ACTUALLY still authenticate, not
				// merely ones that were looked at: a counter incremented on
				// the error path too would report full coverage while every
				// single session had been evicted.
				live := 0
				for i, hs := range held {
					if _, err := svc.Authenticate(hs.token); err != nil {
						t.Errorf("session %d of %d stopped authenticating after a refused completion: %v; the cap refuses NEW sessions and must never make room by destroying established ones", i+1, len(held), err)
						continue
					}
					live++
				}
				if live != len(held) {
					t.Fatalf("%d of %d established sessions survived a refused completion, want all of them: the cap must never evict", live, len(held))
				}
			})
			probed++
		}
		if probed == 0 {
			t.Fatal("the cap table produced no cases, so this test asserted nothing")
		}
	})

	// A refusal must leave the caller exactly as it found it. This is the
	// property a future refactor is most likely to break by folding the cap check
	// in with the single-attempt rule that DELETES a pending challenge — that
	// rule exists for a FAILED VERIFICATION, and a completion refused at the cap
	// verified fine.
	t.Run("a refusal at the cap destroys nothing", func(t *testing.T) {
		// The token used here is still completable AFTERWARDS because the
		// scenario is deliberately positioned inside ChallengeTTL of the moment
		// the agent's own oldest session expires. That is the ONLY arrangement
		// in which the same pending challenge can outlive the refusal: a
		// challenge lives 2 minutes and an active session an hour, so a client
		// refused early in its sessions' lives necessarily needs a fresh
		// challenge — which is the second subtest below.
		t.Run("the same pending challenge completes once the agent's own session expires", func(t *testing.T) {
			const capN = 2
			svc, clock := newService(t, auth.Options{MaxSessions: activeCapMaxSessions, MaxActiveSessionsPerAgent: capN})
			agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

			held := establishActive(t, svc, agentID, priv, capN)

			// One minute short of the agent's own sessions expiring, so the
			// challenge issued now (ChallengeTTL = 2 minutes) outlives them.
			clock.Advance(auth.SessionLifetime - time.Minute)

			ch, err := svc.BeginSession(agentID)
			if err != nil {
				t.Fatalf("BeginSession: %v", err)
			}
			sig := signToken(priv, ch.Token)
			beforeCompletion := svc.SessionCount()

			if _, err := svc.CompleteSession(ch.Token, sig); !errors.Is(err, auth.ErrCapacity) {
				t.Fatalf("err = %v, want one wrapping ErrCapacity at %d active sessions with a cap of %d", err, capN, capN)
			}
			if n := svc.SessionCount(); n != beforeCompletion {
				t.Fatalf("session table = %d entries after the refusal, want %d unchanged: the refused pending challenge must still be there", n, beforeCompletion)
			}

			// The agent's own sessions reach their deadline. Nothing evicted
			// them; they simply expired, which is the only way the bucket ever
			// drains.
			clock.Advance(time.Minute)
			sess, err := svc.CompleteSession(ch.Token, sig)
			if err != nil {
				t.Fatalf("the SAME token and signature would not complete after the agent's own sessions expired: %v; a refusal at the cap is transient and must not have consumed the challenge", err)
			}
			if want := epoch.Add(auth.SessionLifetime).Add(auth.SessionLifetime); !sess.ExpiresAt.Equal(want) {
				t.Errorf("the newly active session expires at %s, want %s (a full lifetime from the completion instant)", sess.ExpiresAt, want)
			}
			if _, err := svc.Authenticate(ch.Token); err != nil {
				t.Fatalf("the newly completed session does not authenticate: %v", err)
			}
			// The expired ones are gone, and gone by expiry rather than by
			// anything the refusal did.
			for i, hs := range held {
				if _, err := svc.Authenticate(hs.token); !errors.Is(err, auth.ErrUnknownSession) {
					t.Errorf("old session %d err = %v, want one wrapping ErrUnknownSession an hour after it was established", i+1, err)
				}
			}
		})

		t.Run("a challenge refused far from expiry needs a fresh one, and gets it", func(t *testing.T) {
			// The honest other half: the agent is refused at the START of its
			// sessions' hour, so by the time a slot frees the pending challenge
			// has itself expired (ChallengeTTL) and the correct answer is
			// ErrUnknownSession, not a completion. What must still hold is that
			// a FRESH challenge works — the refusal cost the agent nothing
			// beyond the round trip.
			const capN = 1
			svc, clock := newService(t, auth.Options{MaxSessions: activeCapMaxSessions, MaxActiveSessionsPerAgent: capN})
			agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

			establishActive(t, svc, agentID, priv, capN)

			ch, err := svc.BeginSession(agentID)
			if err != nil {
				t.Fatalf("BeginSession: %v", err)
			}
			sig := signToken(priv, ch.Token)
			beforeCompletion := svc.SessionCount()

			if _, err := svc.CompleteSession(ch.Token, sig); !errors.Is(err, auth.ErrCapacity) {
				t.Fatalf("err = %v, want one wrapping ErrCapacity", err)
			}
			if n := svc.SessionCount(); n != beforeCompletion {
				t.Fatalf("session table = %d entries after the refusal, want %d unchanged", n, beforeCompletion)
			}

			clock.Advance(auth.SessionLifetime)
			if _, err := svc.CompleteSession(ch.Token, sig); !errors.Is(err, auth.ErrUnknownSession) {
				t.Fatalf("err = %v, want one wrapping ErrUnknownSession: the challenge expired at ChallengeTTL long before the slot freed, and an expired challenge is gone whatever refused it", err)
			}
			if _, err := establishActiveOne(t, svc, agentID, priv); err != nil {
				t.Fatalf("a FRESH handshake after the slot freed: %v; the earlier refusal must not leave the agent permanently unable to authenticate", err)
			}
		})
	})

	// Invariant 10, and the reason the cap check sits BELOW the SessionActive
	// early return in CompleteSession. Same token, same signature is a legitimate
	// retry of a call whose ack was probably lost; it creates no new entry and is
	// already counted in the bucket, so the cap must never see it. Moving the
	// check one line up would turn every retry by a busy agent into a failure —
	// punishing exactly the clients doing the right thing.
	t.Run("re-completing an already-active session is never refused, even at the cap", func(t *testing.T) {
		const capN = 3
		svc, clock := newService(t, auth.Options{MaxSessions: activeCapMaxSessions, MaxActiveSessionsPerAgent: capN})
		agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

		held := establishActive(t, svc, agentID, priv, capN)

		// Half an hour later — a lost ack retried late, still inside the
		// session's lifetime.
		clock.Advance(30 * time.Minute)

		retried := 0
		for i, hs := range held {
			again, err := svc.CompleteSession(hs.token, hs.sig)
			if err != nil {
				t.Fatalf("re-completing active session %d of %d at the cap: %v; a retry of an ALREADY-ACTIVE session creates no new entry and must never be refused (invariant 10)", i+1, len(held), err)
			}
			if !again.ExpiresAt.Equal(hs.sess.ExpiresAt) {
				t.Fatalf("re-completion moved session %d's expiry from %s to %s; the expiry is set once and never extended", i+1, hs.sess.ExpiresAt, again.ExpiresAt)
			}
			if again.AgentID != agentID {
				t.Errorf("re-completion returned agent id %q, want %q", again.AgentID, agentID)
			}
			retried++
		}
		if retried == 0 {
			t.Fatal("no session was re-completed, so the idempotency-at-the-cap property was never probed")
		}
		if n := svc.SessionCount(); n != capN {
			t.Errorf("session table holds %d entries after %d retries, want %d: a retry must not allocate", n, retried, capN)
		}
	})

	// The bucket drains by EXPIRY and by nothing else — the cap never evicts, so
	// expiry is the only thing that can ever unblock a capped agent. If it did
	// not free the slot, the first agent to reach its cap would be locked out
	// permanently.
	t.Run("expiry frees the bucket", func(t *testing.T) {
		const capN = 2
		svc, clock := newService(t, auth.Options{MaxSessions: activeCapMaxSessions, MaxActiveSessionsPerAgent: capN})
		agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

		establishActive(t, svc, agentID, priv, capN)

		ch, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession at the cap: %v", err)
		}
		if _, err := svc.CompleteSession(ch.Token, signToken(priv, ch.Token)); !errors.Is(err, auth.ErrCapacity) {
			t.Fatalf("err = %v, want one wrapping ErrCapacity", err)
		}

		clock.Advance(auth.SessionLifetime)
		sess, err := establishActiveOne(t, svc, agentID, priv)
		if err != nil {
			t.Fatalf("a fresh handshake after the agent's whole bucket expired: %v; expiry is the ONLY thing that frees a slot, so if it does not, a capped agent is locked out forever", err)
		}
		if want := epoch.Add(auth.SessionLifetime).Add(auth.SessionLifetime); !sess.ExpiresAt.Equal(want) {
			t.Errorf("the fresh session expires at %s, want %s", sess.ExpiresAt, want)
		}

		t.Run("only the expired session's slot is freed", func(t *testing.T) {
			// Staggered establishment: the bucket is not all-or-nothing, and a
			// cap that freed the whole bucket on any expiry would let a capped
			// agent burst straight back to twice its cap.
			svc, clock := newService(t, auth.Options{MaxSessions: activeCapMaxSessions, MaxActiveSessionsPerAgent: capN})
			agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

			establishActive(t, svc, agentID, priv, 1) // expires at epoch+1h
			clock.Advance(10 * time.Minute)
			establishActive(t, svc, agentID, priv, 1) // expires at epoch+1h10m

			clock.Advance(auth.SessionLifetime - 10*time.Minute) // now epoch+1h
			if _, err := establishActiveOne(t, svc, agentID, priv); err != nil {
				t.Fatalf("the slot freed by the FIRST session's expiry was not usable: %v", err)
			}
			// And that is the only slot freed: the second session is still live,
			// so the agent is back at its cap immediately.
			ch, err := svc.BeginSession(agentID)
			if err != nil {
				t.Fatalf("BeginSession: %v", err)
			}
			if _, err := svc.CompleteSession(ch.Token, signToken(priv, ch.Token)); !errors.Is(err, auth.ErrCapacity) {
				t.Fatalf("err = %v, want one wrapping ErrCapacity: one expiry frees exactly one slot, never the whole bucket", err)
			}
		})
	})

	// PENDING entries are not ACTIVE entries. An agent may legitimately hold many
	// outstanding challenges — every unauthenticated caller naming it creates
	// one, which is precisely why AUTH-1-FU-PENDINGCAP removed the pending cap —
	// so counting them here would hand a flooder the victim lockout back through
	// the other door.
	t.Run("pending challenges do not count toward the active cap", func(t *testing.T) {
		const (
			capN     = 2
			pendings = 5
		)
		svc, _ := newService(t, auth.Options{MaxSessions: activeCapMaxSessions, MaxActiveSessionsPerAgent: capN})
		agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

		tokens := make([]string, 0, pendings)
		for i := 0; i < pendings; i++ {
			ch, err := svc.BeginSession(agentID)
			if err != nil {
				t.Fatalf("BeginSession #%d: %v", i+1, err)
			}
			tokens = append(tokens, ch.Token)
		}
		if len(tokens) < capN+1 {
			t.Fatalf("the fixture issued %d challenges, want more than the cap of %d or the assertions below probe nothing", len(tokens), capN)
		}

		completed := 0
		for i, token := range tokens {
			_, err := svc.CompleteSession(token, signToken(priv, token))
			if i < capN {
				if err != nil {
					t.Fatalf("completion %d of %d failed while %d challenges were outstanding: %v; PENDING entries must not consume the ACTIVE bucket, or a flooder naming the victim would deny it a session through the other door", i+1, capN, pendings, err)
				}
				completed++
				continue
			}
			if !errors.Is(err, auth.ErrCapacity) {
				t.Fatalf("completion %d err = %v, want one wrapping ErrCapacity: the agent is at its ACTIVE cap of %d", i+1, err, capN)
			}
			completed++
		}
		if completed == 0 {
			t.Fatal("no challenge was completed, so nothing about pending-versus-active was probed")
		}
		if completed != len(tokens) {
			t.Fatalf("probed %d of %d challenges", completed, len(tokens))
		}
	})

	// The zero value means "use the default", exactly as every other Options
	// bound does. A zero that meant "unlimited" would silently disable the cap
	// for every caller that did not set it — including the server's own startup
	// path — which is the failure mode this subtest exists to catch.
	t.Run("a zero MaxActiveSessionsPerAgent means the documented default", func(t *testing.T) {
		if auth.DefaultMaxActiveSessionsPerAgent <= 0 {
			t.Fatalf("DefaultMaxActiveSessionsPerAgent = %d, must be positive: a non-positive default is either 'unlimited' (no cap at all) or 'refuse everything' (no agent can ever authenticate)", auth.DefaultMaxActiveSessionsPerAgent)
		}
		if auth.DefaultMaxActiveSessionsPerAgent < 2 {
			t.Errorf("DefaultMaxActiveSessionsPerAgent = %d, want at least 2: a well-behaved client establishes its next session at %.0f%% of the lifetime, so the old and the new overlap and TWO concurrent sessions are the compliant steady state", auth.DefaultMaxActiveSessionsPerAgent, auth.SessionRefreshFraction*100)
		}
		if auth.DefaultMaxActiveSessionsPerAgent >= auth.DefaultMaxSessions {
			t.Errorf("DefaultMaxActiveSessionsPerAgent = %d and DefaultMaxSessions = %d; a per-agent cap at or above the whole table bounds nothing, because one agent could still fill it", auth.DefaultMaxActiveSessionsPerAgent, auth.DefaultMaxSessions)
		}

		// Behavioural half: a service built with the zero value does not refuse
		// well before the default. Three handshakes is deliberately cheap — the
		// exact boundary is pinned by the table above, which sets the option
		// explicitly.
		svc, _ := newService(t, auth.Options{})
		agentID, priv := enrolAgent(t, svc, "alpha", "idem-1")

		const under = 3
		if under > auth.DefaultMaxActiveSessionsPerAgent {
			t.Fatalf("this subtest assumes the default (%d) is at least %d", auth.DefaultMaxActiveSessionsPerAgent, under)
		}
		established := 0
		for i := 0; i < under; i++ {
			if _, err := establishActiveOne(t, svc, agentID, priv); err != nil {
				t.Fatalf("handshake %d of %d on a service built with MaxActiveSessionsPerAgent: 0 was refused: %v; the zero value must mean the default of %d, never a cap of zero or one", i+1, under, err, auth.DefaultMaxActiveSessionsPerAgent)
			}
			established++
		}
		if established == 0 {
			t.Fatal("no handshake was attempted against the default service, so the default was never probed")
		}
	})
}

// establishActiveOne is one begin+complete round trip that RETURNS its error
// instead of failing the test, for the cases whose whole point is what the error
// is. It still fails on a BeginSession error: that call is never the thing under
// test in this file's cap assertions.
func establishActiveOne(t *testing.T, svc *auth.Service, agentID string, priv ed25519.PrivateKey) (auth.Session, error) {
	t.Helper()
	ch, err := svc.BeginSession(agentID)
	if err != nil {
		t.Fatalf("BeginSession for %q: %v", agentID, err)
	}
	return svc.CompleteSession(ch.Token, signToken(priv, ch.Token))
}

// TestEnrollThenSessionGlobalCapFailsClosed pins that the session table refuses
// rather than growing without limit: it is memory an UNAUTHENTICATED caller can
// allocate.
func TestEnrollThenSessionGlobalCapFailsClosed(t *testing.T) {
	svc, _ := newService(t, auth.Options{MaxSessions: 2})

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

	// A REFUSED call must leave the table exactly as it found it: a failed call
	// with a side effect on the caller's own state is the worst kind. The
	// scenario is the one a client actually hits — alpha already holds a pending
	// challenge, the table fills, and alpha's NEXT begin is refused. That refusal
	// must not cost alpha the challenge it is already part-way through signing.
	t.Run("a refusal at the global cap destroys no pending challenge", func(t *testing.T) {
		svc, _ := newService(t, auth.Options{MaxSessions: 2})
		alpha, alphaPriv := enrolAgent(t, svc, "alpha", "idem-1")
		beta, _ := enrolAgent(t, svc, "beta", "idem-2")

		ch, err := svc.BeginSession(alpha)
		if err != nil {
			t.Fatalf("BeginSession(alpha): %v", err)
		}
		if _, err := svc.BeginSession(beta); err != nil {
			t.Fatalf("BeginSession(beta): %v", err)
		}

		// The table is now at the global cap, so alpha's next begin is refused.
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
	svc, _ := newService(t, auth.Options{MaxSessions: 1024})
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
