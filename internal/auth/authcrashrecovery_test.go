package auth_test

// AUTH-5: END-TO-END AUTH CRASH / RECOVERY.
//
// The claim, in one line: AFTER A CRASH, AN AGENT CAN OBTAIN AND USE A SESSION
// TOKEN IF AND ONLY IF ITS ENROLMENT DURABLY COMMITTED, AND A SESSION THAT
// EXISTED BEFORE THE CRASH DOES NOT SURVIVE IT.
//
// This is deliberately NOT a duplicate of crash_test.go. crash_test.go injects
// the SAME SIGKILLs but asserts only against the recovered roster MAP
// (WALRoster.Get / Len / the suffix floors). AUTH-5 asserts one level up, on the
// thing a client actually holds — the SESSION TOKEN — by driving the real
// auth.Service challenge/response (BeginSession -> sign -> CompleteSession ->
// Authenticate) over the RECOVERED roster. A roster that came back in name only,
// or came back with the wrong auth key, passes every crash_test.go assertion and
// fails every agent trying to authenticate; nothing until here caught that
// across a crash. TestAUTH3Acceptance... drives the token path across a restart
// but through a GRACEFUL l.Close(), not a SIGKILL, so it proves nothing about
// invariant 4's "no clean shutdown required" clause.
//
// Invariants exercised:
//
//	4  nothing acknowledged before durable — a Put that RETURNED before the kill
//	   is on the platter, so its agent authenticates after recovery; a prepare
//	   that fsynced but never committed was never acknowledged, so its agent is
//	   absent and cannot even obtain a challenge.
//	5  memory is the serving copy, disk is the truth — recovery reconstructs the
//	   serving roster from the log, and the token path reads only that.
//	3  the roster is the authoritative identity set the token path checks, and
//	   SESSIONS ARE MEMORY-ONLY: a token minted before the crash is rejected after
//	   it, and the agent must re-establish (which succeeds, because the enrolment
//	   is durable).
//
// # Deterministic keypairs
//
// The child dies, so the parent cannot be handed the child's random private key.
// Both sides derive the SAME Ed25519 keypair from a fixed seed instead, so the
// parent can sign the post-crash challenge with the private half the "enrolled"
// agent used. Real keys through the real signature path — a hand-built byte slice
// would prove nothing about verification.
//
// # What AUTH-5 does NOT cover here, and why
//
// The task description also asks to "revoke an agent, crash, restart, assert the
// token stays rejected". DURABLE AGENT revocation does not exist yet — that is
// AUTH-4 (leave / revocation), still open — and WALRoster has no remove path to
// exercise. The realizable "token stays rejected after a crash" today is the
// invariant-3 property proven in the third sub-test: a session established before
// the crash is dead after it. The agent-revocation-durability variant is filed as
// a follow-up (AUTH-5-FU-REVOCATION) blocked on AUTH-4; the OPERATOR plane, which
// DOES have durable revocation, carries its own revocation-recovery coverage in
// operatorsession_test / operator_test.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	envRecoveryPoint = "AUTH_RECOVERY_CRASH_POINT"
	envRecoveryDir   = "AUTH_RECOVERY_CRASH_DIR"

	// recoveryAfterCommit: the durable agent's Put RETURNED — prepare fsync,
	// commit fsync, Apply all done — and then death with no Close, no Sync, no
	// defer. Its enrolment is acknowledged history (invariant 4).
	recoveryAfterCommit = "after-commit"

	// recoveryAfterPrepare: the durable agent committed fully; a SECOND agent's
	// PREPARE fsynced and then death with that transaction still open. The second
	// enrolment was never acknowledged — Put never returned — so it must be
	// invisible after recovery.
	recoveryAfterPrepare = "after-prepare"

	// recoveryAfterSession: the durable agent committed, a full session handshake
	// completed, the live token was captured to a file (standing in for a token an
	// attacker observed in flight), and then death. The token must NOT authenticate
	// after recovery — sessions are memory-only (invariant 3).
	recoveryAfterSession = "after-session"

	// capturedTokenFile is where the after-session child drops the live token it
	// held at the instant of the crash, so the parent can prove that exact token
	// is rejected after recovery.
	capturedTokenFile = "captured-token"
)

// recoveryEpoch is the fixed instant the crash children stamp their entries with.
var recoveryEpoch = time.Date(2026, time.August, 8, 9, 0, 0, 0, time.UTC)

// recoveryKeypair derives a deterministic Ed25519 keypair from a one-byte seed,
// so the parent and the (dead) child agree on the private key without the child
// having to hand anything back.
func recoveryKeypair(seed byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	return priv.Public().(ed25519.PublicKey), priv
}

// recoveryDurableSeed is the enrolled agent that must SURVIVE every crash point.
// recoveryEphemeralSeed is the agent whose prepare is fsynced but never
// committed. recoveryImpostorSeed is a key that never enrolled.
const (
	recoveryDurableSeed   = 0xD1
	recoveryEphemeralSeed = 0xE2
	recoveryImpostorSeed  = 0x99
)

// recoveryEntry builds a minimal storable RosterEntry carrying a REAL auth
// public key, so the recovered record can verify a real signature.
func recoveryEntry(t *testing.T, name string, n uint64, pub ed25519.PublicKey) auth.RosterEntry {
	t.Helper()
	return auth.RosterEntry{
		AgentID:       mustAgentID(t, name, n),
		Name:          name,
		AuthPublicKey: pub,
		Epoch:         recoveryEpoch,
		EnrolledAt:    recoveryEpoch,
	}
}

// TestAuthCrashRecoveryChild is the child half of the AUTH-5 crash tests. In a
// normal run it SKIPS immediately, so its presence costs the suite nothing.
func TestAuthCrashRecoveryChild(t *testing.T) {
	point := os.Getenv(envRecoveryPoint)
	if point == "" {
		t.Skip("not a crash child: " + envRecoveryPoint + " is unset")
	}
	dir := os.Getenv(envRecoveryDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envRecoveryPoint, point, envRecoveryDir)
	}

	r := auth.NewWALRoster(nil)
	l, err := wal.Open(wal.LogOptions{Dir: dir, Applier: r})
	if err != nil {
		t.Fatalf("child: wal.Open: %v", err)
	}
	if err := r.Attach(l); err != nil {
		t.Fatalf("child: Attach: %v", err)
	}

	// The durable enrolment goes all the way through the real durable path:
	// prepare fsync, commit fsync, Apply. When Put returns, it is acknowledged
	// history.
	durablePub, durablePriv := recoveryKeypair(recoveryDurableSeed)
	if err := r.Put(recoveryEntry(t, "worker", 1, durablePub)); err != nil {
		t.Fatalf("child: Put durable agent: %v", err)
	}

	switch point {
	case recoveryAfterCommit:
		// No Close, no Sync, no defer, no runtime shutdown. The enrolment is
		// durable and must authenticate after recovery.
		authSuicide()

	case recoveryAfterSession:
		// Establish a full session, capture the LIVE token, then die. The token
		// must be dead after recovery (invariant 3).
		svc, err := auth.NewService(auth.Options{
			Minter: newMinter(t),
			Roster: r,
			Now:    func() time.Time { return recoveryEpoch },
		})
		if err != nil {
			t.Fatalf("child: NewService: %v", err)
		}
		id := mustAgentID(t, "worker", 1)
		ch, err := svc.BeginSession(id)
		if err != nil {
			t.Fatalf("child: BeginSession: %v", err)
		}
		sess, err := svc.CompleteSession(ch.Token, signToken(durablePriv, ch.Token))
		if err != nil {
			t.Fatalf("child: CompleteSession: %v", err)
		}
		if sess.State != auth.SessionActive {
			t.Fatalf("child: the completed session is %v, want active", sess.State)
		}
		// Drop the live token where the parent can read it — this stands in for a
		// token an attacker captured off the wire moments before the crash. The
		// bytes reach the page cache before the kill and outlive the process.
		if err := os.WriteFile(filepath.Join(dir, capturedTokenFile), []byte(ch.Token), 0600); err != nil {
			t.Fatalf("child: writing the captured token: %v", err)
		}
		authSuicide()

	case recoveryAfterPrepare:
		// A SECOND agent's PREPARE is fsynced, then death with the transaction
		// open. This enrolment is NOT acknowledged (Begin's caller never got a
		// Committed back), so recovery must not surface it.
		ephemeralPub, _ := recoveryKeypair(recoveryEphemeralSeed)
		body, err := auth.Encode(recoveryEntry(t, "worker", 2, ephemeralPub))
		if err != nil {
			t.Fatalf("child: Encode ephemeral agent: %v", err)
		}
		if _, err := l.Begin(wal.Entry{Kind: auth.RecordKind, Body: body}); err != nil {
			t.Fatalf("child: Begin ephemeral agent: %v", err)
		}
		authSuicide()

	default:
		t.Fatalf("child: unknown crash point %q", point)
	}
	t.Fatalf("child: still running after SIGKILL at point %q", point)
}

// runAuthRecoveryChild re-execs this test binary at the given crash point and
// PROVES the child died on SIGKILL rather than failing its own assertions.
//
// Without this check a child that t.Fatalf'd on its first line would exit 1,
// leave a log the crash never touched, and every "the agent authenticates" /
// "the agent is absent" assertion in the parent would pass for the wrong reason.
func runAuthRecoveryChild(t *testing.T, point, dir string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestAuthCrashRecoveryChild$", "-test.v")
	cmd.Env = append(os.Environ(), envRecoveryPoint+"="+point, envRecoveryDir+"="+dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child %q did not finish in time: %v\n--- child output ---\n%s", point, ctx.Err(), out.String())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child %q: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s",
			point, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child %q: wait status is %T, want syscall.WaitStatus", point, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected, so nothing below is being tested\n--- child output ---\n%s",
			point, ws.ExitStatus(), out.String())
	}
}

// TestAuthCrashRecovery is AUTH-5. Each sub-test injects a real SIGKILL at a
// chosen point in the auth write path, restarts a BRAND-NEW roster and service
// from the log alone, and asserts the token outcome.
func TestAuthCrashRecovery(t *testing.T) {
	durableID := mustAgentID(t, "worker", 1)
	_, durablePriv := recoveryKeypair(recoveryDurableSeed)

	t.Run("a durably committed enrolment yields a valid token after a crash", func(t *testing.T) {
		dir := t.TempDir()
		runAuthRecoveryChild(t, recoveryAfterCommit, dir)

		// --- the restart: a fresh roster and a fresh service from the log alone ---
		r, l := openRoster(t, dir)
		defer l.Close()
		svc, _ := newService(t, auth.Options{Roster: r})

		if _, ok := r.Get(durableID); !ok {
			t.Fatalf("agent %q is NOT on the roster after the crash, but its Put returned before the kill: an acknowledged enrolment was lost (invariant 4)", durableID)
		}

		// The full challenge/response WITHOUT re-enrolling — the token path over the
		// recovered roster.
		ch, err := svc.BeginSession(durableID)
		if err != nil {
			t.Fatalf("BeginSession for %q after the crash = %v; the agent was acknowledged as enrolled and never left, so the recovered roster must know it", durableID, err)
		}
		sess, err := svc.CompleteSession(ch.Token, signToken(durablePriv, ch.Token))
		if err != nil {
			t.Fatalf("CompleteSession after the crash = %v; the agent signed with the SAME private key it enrolled with, so the recovered auth key must verify it. A failure here means the durable roster survived the crash in name only", err)
		}
		if sess.State != auth.SessionActive {
			t.Fatalf("the completed session is %v, want active", sess.State)
		}
		princ, err := svc.Authenticate(ch.Token)
		if err != nil {
			t.Fatalf("Authenticate with the post-crash token = %v, want a principal", err)
		}
		if princ.AgentID != durableID {
			t.Fatalf("Authenticate returned principal %q, want %q", princ.AgentID, durableID)
		}

		// A DIFFERENT keypair is still refused. Without this the success above only
		// shows SOME key verified, not that the recovered key is the ENROLLED one.
		_, impostor := recoveryKeypair(recoveryImpostorSeed)
		ch2, err := svc.BeginSession(durableID)
		if err != nil {
			t.Fatalf("BeginSession for the impostor attempt: %v", err)
		}
		if _, err := svc.CompleteSession(ch2.Token, signToken(impostor, ch2.Token)); !errors.Is(err, auth.ErrBadSignature) {
			t.Fatalf("CompleteSession with a DIFFERENT keypair after the crash = %v, want ErrBadSignature; the recovered key must be the ENROLLED one, not merely a key", err)
		}
	})

	t.Run("an enrolment that never committed yields NO token after a crash", func(t *testing.T) {
		dir := t.TempDir()
		runAuthRecoveryChild(t, recoveryAfterPrepare, dir)

		r, l := openRoster(t, dir)
		defer l.Close()
		svc, _ := newService(t, auth.Options{Roster: r})

		// The durably committed agent is unaffected: damage at the tail must not
		// cascade backwards, and it must still authenticate.
		if _, ok := r.Get(durableID); !ok {
			t.Fatalf("agent %q is NOT on the roster; its own commit fsynced before the ephemeral prepare that was torn off the tail, so it must survive", durableID)
		}
		ch, err := svc.BeginSession(durableID)
		if err != nil {
			t.Fatalf("BeginSession for the durable agent %q = %v; it committed before the crash and must authenticate", durableID, err)
		}
		if _, err := svc.CompleteSession(ch.Token, signToken(durablePriv, ch.Token)); err != nil {
			t.Fatalf("CompleteSession for the durable agent = %v; it enrolled durably and signs with its enrolled key", err)
		}

		// The non-durable enrolment is invisible AND unauthenticatable. Its PREPARE
		// fsynced, so an id-level scan can still see its number burned (crash_test.go
		// proves that); but it was never acknowledged, so the roster must not carry it
		// and the token path must refuse to even issue it a challenge.
		ephemeralID := mustAgentID(t, "worker", 2)
		if _, ok := r.Get(ephemeralID); ok {
			t.Fatalf("agent %q IS on the roster, but its COMMIT never reached disk; recovery resurrected an enrolment no caller was ever told about (invariants 4, 5)", ephemeralID)
		}
		if _, err := svc.BeginSession(ephemeralID); !errors.Is(err, auth.ErrUnknownAgent) {
			t.Fatalf("BeginSession for %q = %v, want ErrUnknownAgent; a prepare that fsynced but never committed must not become an authenticatable identity after recovery", ephemeralID, err)
		}
	})

	t.Run("a session established before a crash does not survive it", func(t *testing.T) {
		dir := t.TempDir()
		runAuthRecoveryChild(t, recoveryAfterSession, dir)

		// The token the child held, active, at the instant of the crash.
		captured, err := os.ReadFile(filepath.Join(dir, capturedTokenFile))
		if err != nil {
			t.Fatalf("reading the captured token: %v; the child must have written it before the kill", err)
		}
		if len(captured) == 0 {
			t.Fatalf("the captured token is empty; the child recorded no live session, so this sub-test proves nothing")
		}

		r, l := openRoster(t, dir)
		defer l.Close()
		svc, _ := newService(t, auth.Options{Roster: r})

		// The enrolment is durable, so the agent is back on the roster...
		if _, ok := r.Get(durableID); !ok {
			t.Fatalf("agent %q is NOT on the roster after the crash, but its enrolment was acknowledged (invariant 4)", durableID)
		}
		// ...but the session it had is GONE. Sessions are memory-only; a token that
		// outlived the process would be an unrevokable credential (invariant 3).
		if n := svc.SessionCount(); n != 0 {
			t.Fatalf("a service built over the recovered roster holds %d sessions, want 0; sessions do NOT survive a crash", n)
		}
		if _, err := svc.Authenticate(string(captured)); !errors.Is(err, auth.ErrUnknownSession) {
			t.Fatalf("Authenticate with the pre-crash token = %v, want ErrUnknownSession; a session token that survived the crash would be a bearer credential nobody could revoke", err)
		}

		// And the agent can re-establish, WITHOUT re-enrolling — the durable half is
		// intact even though the ephemeral half is not.
		ch, err := svc.BeginSession(durableID)
		if err != nil {
			t.Fatalf("BeginSession to re-establish after the crash = %v; the enrolment is durable, so a fresh challenge must be issuable", err)
		}
		if _, err := svc.CompleteSession(ch.Token, signToken(durablePriv, ch.Token)); err != nil {
			t.Fatalf("CompleteSession to re-establish after the crash = %v; the recovered auth key must verify the enrolled agent's signature", err)
		}
	})
}
