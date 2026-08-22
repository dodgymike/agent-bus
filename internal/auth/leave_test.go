package auth_test

// AUTH-4: DURABLE LEAVE / REVOCATION.
//
// The claim, in one line: AFTER AN AGENT LEAVES, IT STAYS GONE ACROSS A CRASH
// AND A RESTART, ITS ID IS NEVER RE-ISSUED, AND LEAVING TWICE IS A CLEAN RETRY.
//
// This is the durable half AUTH-5 could not cover: AUTH-5's own header records
// that "DURABLE AGENT revocation does not exist yet — that is AUTH-4 ... and
// WALRoster has no remove path to exercise", filing this as AUTH-5-FU-REVOCATION.
// AUTH-4 adds Roster.Remove (a tombstone through the two-phase write path) and
// Service.Leave, and this file exercises both across a real SIGKILL.
//
// Invariants exercised:
//
//	1  ids are never reused, INCLUDING after leave — a re-enrolment of the same
//	   NAME after a departure mints a STRICTLY HIGHER suffix, because the per-name
//	   suffix floor is not reclaimed on leave (the leave record itself preserves it
//	   in EnrolmentSuffixesInWAL).
//	4  nothing acknowledged before durable — a Remove that RETURNED before the kill
//	   is on the platter, so the agent is absent after recovery.
//	5  memory is the serving copy, disk is the truth — recovery replays enrol then
//	   leave and reaches "absent".
//	6  the removal is an APPEND (a tombstone), never a log rewrite or truncation.
//	3  the roster is the authoritative identity set the token path checks; a
//	   departed agent cannot even obtain a challenge (BeginSession refuses), and its
//	   live sessions are dropped at once.
//	10 leaving twice is a legitimate retry — the second Remove writes nothing,
//	   returns removed=false and NO error, and does not disconnect.

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	envLeaveCrashDir = "AUTH_LEAVE_CRASH_DIR"

	// leaveCrashAfterCommit: agentA's Put and then its Remove both RETURNED —
	// each is a prepare fsync, a commit fsync and an Apply — and then death with
	// no Close, no Sync and no defer. Invariant 4's claim is that this is already
	// enough for the departure to survive.
	leaveCrashAfterCommit = "after-leave-commit"
)

// leaveTime is the fixed instant the crash child stamps its records with, so the
// parent can assert exact values across a process boundary.
var leaveTime = time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)

// leaveAgent is the enrolment that is created and then LEFT. worker-1: a
// re-enrolment of "worker" after the departure must mint worker-2, never
// worker-1 (invariant 1).
func leaveAgent(t *testing.T) auth.RosterEntry {
	t.Helper()
	return auth.RosterEntry{
		AgentID:       mustAgentID(t, "worker", 1),
		Name:          "worker",
		AuthPublicKey: fixedKey(0x71),
		Epoch:         leaveTime,
		EnrolledAt:    leaveTime,
	}
}

// leaveSuicide SIGKILLs this process — the same self-kill crash_test.go uses,
// written here rather than shared because a test helper is not an API.
func leaveSuicide() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	panic("auth leave crash test: SIGKILL to self did not kill the process")
}

// TestLeaveCrashChild is the child half of the crash test. In a normal run it
// SKIPS immediately, so its presence costs the suite nothing.
func TestLeaveCrashChild(t *testing.T) {
	dir := os.Getenv(envLeaveCrashDir)
	if dir == "" {
		t.Skip("not a leave crash child: " + envLeaveCrashDir + " is unset")
	}

	r := auth.NewWALRoster(nil)
	l, err := wal.Open(wal.LogOptions{Dir: dir, Applier: r})
	if err != nil {
		t.Fatalf("child: wal.Open: %v", err)
	}
	if err := r.Attach(l); err != nil {
		t.Fatalf("child: Attach: %v", err)
	}

	// Enrol worker-1 all the way through the durable path.
	if err := r.Put(leaveAgent(t)); err != nil {
		t.Fatalf("child: Put worker-1: %v", err)
	}
	// It LEAVES, durably. When Remove returns, the departure is acknowledged
	// history.
	removed, err := r.Remove(mustAgentID(t, "worker", 1), leaveTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("child: Remove worker-1: %v", err)
	}
	if !removed {
		t.Fatalf("child: Remove reported the agent was already absent; it had just been enrolled")
	}

	// No Close, no Sync, no defer, no runtime shutdown. The next statement is the
	// kill.
	leaveSuicide()
	t.Fatalf("child: still running after SIGKILL")
}

// runLeaveCrashChild re-execs this test binary and PROVES the child died on
// SIGKILL rather than failing its own assertions — without which a child that
// t.Fatalf'd on its first line would exit 1 and every "the agent is absent"
// assertion below would pass for the wrong reason.
func runLeaveCrashChild(t *testing.T, dir string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	cmd := exec.Command(self, "-test.run=^TestLeaveCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), envLeaveCrashDir+"="+dir)
	out, err := cmd.CombinedOutput()

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("leave crash child: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s", err, out)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("leave crash child: wait status is %T, want syscall.WaitStatus", ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("leave crash child exited with status %d instead of dying on SIGKILL; the crash was never injected, so nothing below is being tested\n--- child output ---\n%s", ws.ExitStatus(), out)
	}
}

// TestLeaveRevocation is AUTH-4's proof (proof_cmd:
// go test -race -run TestLeaveRevocation ./internal/auth).
func TestLeaveRevocation(t *testing.T) {
	t.Run("durable_leave_survives_crash", func(t *testing.T) {
		dir := t.TempDir()
		runLeaveCrashChild(t, dir)

		r, l := openRoster(t, dir)
		defer l.Close()

		// (1) The agent that left is ABSENT after recovery. Its Put AND its Remove
		// both returned before the kill, so the departure is acknowledged history
		// (invariant 4) and recovery replays enrol-then-leave to "absent"
		// (invariant 5).
		id := mustAgentID(t, "worker", 1)
		if _, ok := r.Get(id); ok {
			t.Fatalf("agent %q is STILL on the roster after a crash that followed its durable leave; a left agent must stay gone (invariants 4, 5)", id)
		}
		if got := r.Len(); got != 0 {
			t.Fatalf("the roster holds %d agents after the only one left, want 0", got)
		}

		// (2) Its id is NOT re-issued (invariant 1). The suffix floor survives —
		// the leave record itself carries worker-1, so the fold sees it even if the
		// enrol record were gone — so the next "worker" enrolment is worker-2.
		floors, err := auth.EnrolmentSuffixesInWAL(l.Path(), testBusID)
		if err != nil {
			t.Fatalf("EnrolmentSuffixesInWAL: %v", err)
		}
		if got := floors["worker"]; got != 1 {
			t.Fatalf(`the floor for "worker" is %d, want 1; a departed agent's suffix must stay burned so its id is never re-issued (invariant 1)`, got)
		}
		if got, want := mintOverFloors(t, floors, "worker"), mustAgentID(t, "worker", 2); got != want {
			t.Fatalf("the next enrolment of worker minted %q, want %q; a re-enrolment must never receive the departed id", got, want)
		}
	})

	t.Run("leave_is_idempotent", func(t *testing.T) {
		dir := t.TempDir()
		r, l := openRoster(t, dir)
		defer l.Close()

		if err := r.Put(leaveAgent(t)); err != nil {
			t.Fatalf("Put worker-1: %v", err)
		}
		id := mustAgentID(t, "worker", 1)

		// First leave: removes the agent durably.
		removed, err := r.Remove(id, leaveTime)
		if err != nil {
			t.Fatalf("first Remove: %v", err)
		}
		if !removed {
			t.Fatalf("first Remove reported already-absent for an enrolled agent")
		}
		if _, ok := r.Get(id); ok {
			t.Fatalf("agent %q still present after its first leave", id)
		}

		// Second leave: the idempotent retry (invariant 10). It WRITES NOTHING,
		// returns removed=false and NO error, and leaves the agent absent. There is
		// no disconnect here to test — Remove has no connection — but the
		// no-error/no-reapply contract is exactly invariant 10's retry rule.
		removed, err = r.Remove(id, leaveTime.Add(time.Hour))
		if err != nil {
			t.Fatalf("second Remove (idempotent retry) returned an error: %v; leaving twice is a legitimate retry, not a failure (invariant 10)", err)
		}
		if removed {
			t.Fatalf("second Remove reported it removed the agent again; the agent was already gone, so the retry must report removed=false and write nothing")
		}
		if _, ok := r.Get(id); ok {
			t.Fatalf("agent %q reappeared after an idempotent second leave", id)
		}
	})

	t.Run("departed_agent_cannot_authenticate", func(t *testing.T) {
		svc, _ := newService(t, auth.Options{Roster: auth.NewMemoryRoster()})

		agentID, priv := enrolAgent(t, svc, "worker", "leave-auth-1")

		// Establish a live session and confirm it authenticates.
		ch, err := svc.BeginSession(agentID)
		if err != nil {
			t.Fatalf("BeginSession before leave: %v", err)
		}
		if _, err := svc.CompleteSession(ch.Token, signToken(priv, ch.Token)); err != nil {
			t.Fatalf("CompleteSession before leave: %v", err)
		}
		if _, err := svc.Authenticate(ch.Token); err != nil {
			t.Fatalf("Authenticate before leave: %v; the session should be live", err)
		}

		// LEAVE. The live session must be dropped, and the agent must be gone.
		res, err := svc.Leave(agentID)
		if err != nil {
			t.Fatalf("Leave: %v", err)
		}
		if res.AlreadyLeft {
			t.Fatalf("a fresh leave reported AlreadyLeft for an enrolled agent")
		}
		if res.SessionsDropped < 1 {
			t.Fatalf("Leave dropped %d sessions, want at least the 1 live session it held; a departed agent's opaque handles must be dropped (invariant 3)", res.SessionsDropped)
		}

		// The pre-leave token no longer authenticates.
		if _, err := svc.Authenticate(ch.Token); !errors.Is(err, auth.ErrUnknownSession) {
			t.Fatalf("Authenticate after leave = %v, want auth.ErrUnknownSession; the departed agent's session must stop authenticating at once", err)
		}
		// And it cannot obtain a NEW challenge either: it is gone from the roster.
		if _, err := svc.BeginSession(agentID); !errors.Is(err, auth.ErrUnknownAgent) {
			t.Fatalf("BeginSession after leave = %v, want auth.ErrUnknownAgent; a departed agent is not enrolled (invariant 3)", err)
		}

		// Leaving again is the idempotent retry at the Service layer.
		again, err := svc.Leave(agentID)
		if err != nil {
			t.Fatalf("second Leave returned an error: %v; it is a legitimate retry (invariant 10)", err)
		}
		if !again.AlreadyLeft {
			t.Fatalf("second Leave did not report AlreadyLeft; the agent was already gone")
		}
		if again.SessionsDropped != 0 {
			t.Fatalf("second Leave dropped %d sessions, want 0; there were none left", again.SessionsDropped)
		}
	})
}
