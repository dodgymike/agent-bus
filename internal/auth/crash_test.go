package auth_test

// AUTH-3, part 4: CRASH INJECTION.
//
// The claim under test, in one line: AN ENROLMENT IS ON THE ROSTER AFTER A
// RESTART IF AND ONLY IF ITS RECORD DURABLY COMMITTED.
//
// "The code looks right" is not evidence for a durability claim, so each point
// in the write path is exercised by a real process that is really SIGKILLed:
//
//	A  after the PREPARE fsync, before COMMIT   -> absent from the roster,
//	                                               PRESENT in the suffix floors
//	B  after the COMMIT fsync                   -> present, every field intact
//	C  a TORN COMMIT frame                      -> repaired tail, absent
//
// Point A carries the single most important assertion in this file: agent 2's
// number is BURNED even though agent 2 is not enrolled. That pairing — the
// enrolment is lost, the id is never re-issued — is what makes the whole design
// correct, and it is exactly the case a committed-state-only derivation cannot
// see.
//
// Every child is proven to have died on SIGKILL. Without that check a child
// that failed its own assertions and exited non-zero would silently turn the
// parent into a test of nothing.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	envAuthCrashPoint = "AUTH_CRASH_POINT"
	envAuthCrashDir   = "AUTH_CRASH_DIR"

	// crashAfterPrepare: agent A committed through the whole path, agent B's
	// PREPARE fsynced, then death with the transaction open.
	crashAfterPrepare = "after-prepare"

	// crashAfterCommit: agent A's Put RETURNED — prepare fsynced, commit
	// fsynced, applied — and then death with no Close, no Sync and no defer.
	crashAfterCommit = "after-commit"

	// crashTornCommit: agent B's prepare is fsynced and a PARTIAL commit frame
	// sits on the end of the file.
	crashTornCommit = "torn-commit"
)

// crashTime is the fixed instant the crash children stamp their entries with,
// so the parent can assert on exact values across a process boundary.
var crashTime = time.Date(2026, time.August, 7, 9, 0, 0, 0, time.UTC)

// crashAgentA is the enrolment that must SURVIVE. Every reserved field is
// populated, including a retired and a live certificate binding, so "present
// with every field intact" is a claim with content.
func crashAgentA(t *testing.T) auth.RosterEntry {
	t.Helper()
	retired := crashTime.Add(30 * time.Minute)
	return auth.RosterEntry{
		AgentID:            mustAgentID(t, "worker", 1),
		Name:               "worker",
		AuthPublicKey:      fixedKey(0xA1),
		MessagingPublicKey: fixedKey(0xA2),
		InviteID:           "invite-crash-a",
		Epoch:              crashTime,
		EnrolledAt:         crashTime,
		CertBindings: []auth.CertBinding{
			{Fingerprint: fixedFingerprint(0xA3), BoundAt: crashTime, RetiredAt: &retired},
			{Fingerprint: fixedFingerprint(0xA4), BoundAt: retired},
		},
	}
}

// crashAgentB is the enrolment that must NOT survive. Its suffix is 7 rather
// than 2 so that "the floor rose because of B" cannot be confused with "the
// floor rose because of A".
func crashAgentB(t *testing.T) auth.RosterEntry {
	t.Helper()
	return auth.RosterEntry{
		AgentID:       mustAgentID(t, "worker", 7),
		Name:          "worker",
		AuthPublicKey: fixedKey(0xB1),
		Epoch:         crashTime,
		EnrolledAt:    crashTime,
	}
}

// authSuicide SIGKILLs this process. Written here rather than imported from
// internal/wal's tests: a test helper is not an API, and a crash test that
// depended on another package's unexported test scaffolding would break the
// moment that scaffolding moved.
func authSuicide() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	// Unreachable: the signal is delivered before Kill returns. Panicking
	// rather than looping means a platform where that is somehow untrue fails
	// loudly instead of hanging the suite.
	panic("auth crash test: SIGKILL to self did not kill the process")
}

// TestAuthCrashChild is the child half of the crash tests. In a normal run it
// SKIPS immediately, so its presence costs the suite nothing.
func TestAuthCrashChild(t *testing.T) {
	point := os.Getenv(envAuthCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envAuthCrashPoint + " is unset")
	}
	dir := os.Getenv(envAuthCrashDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envAuthCrashPoint, point, envAuthCrashDir)
	}

	r := auth.NewWALRoster(nil)
	l, err := wal.Open(wal.LogOptions{Dir: dir, Applier: r})
	if err != nil {
		t.Fatalf("child: wal.Open: %v", err)
	}
	if err := r.Attach(l); err != nil {
		t.Fatalf("child: Attach: %v", err)
	}

	// Agent A goes all the way through the real durable path: prepare fsync,
	// commit fsync, Apply. When Put returns, it is acknowledged history.
	if err := r.Put(crashAgentA(t)); err != nil {
		t.Fatalf("child: Put agent A: %v", err)
	}

	if point == crashAfterCommit {
		// No Close, no Sync, no defer, no runtime shutdown. Invariant 4's
		// actual claim is that this is already enough.
		authSuicide()
	}

	body, err := auth.Encode(crashAgentB(t))
	if err != nil {
		t.Fatalf("child: Encode agent B: %v", err)
	}
	txn, err := l.Begin(wal.Entry{Kind: auth.RecordKind, Body: body})
	if err != nil {
		t.Fatalf("child: Begin agent B: %v", err)
	}

	switch point {
	case crashAfterPrepare:
		// Agent B's PREPARE is fsynced and its transaction is open. Die.
		authSuicide()

	case crashTornCommit:
		// -------------------------------------------------------------------
		// HONEST ACCOUNT OF WHAT THIS INJECTS.
		//
		// A SIGKILL cannot by itself tear a write: os.File.Write is one
		// syscall, the bytes land in the PAGE CACHE, and the page cache
		// outlives the process. Killing between two appends therefore leaves
		// WHOLE frames — which is point A above, not this one. The torn bytes
		// have to be written deliberately; what the kill contributes, and it
		// cannot be faked, is that nothing graceful runs afterwards.
		//
		// The bytes below are a COMMIT frame for agent B's prepare, built to
		// the documented version 2 layout
		//
		//	payloadLen[4] ++ index[8] ++ type[2] ++ reserved[2] ++ mac[32] ++ payload
		//
		// with the exact payload internal/wal writes for a commit
		// ({"prepare_index":N}) and the exact index the commit would have taken
		// (prepare index + 1), then cut short in the MIDDLE OF THE PAYLOAD.
		//
		// ONE THING IS NOT AUTHENTIC AND IS SAID SO OUT LOUD: the 32 MAC bytes
		// are zeros, because the frame tag is keyed per file and the keying is
		// unexported — internal/auth cannot compute it. That difference is
		// UNOBSERVABLE on this path: the frame's length header promises more
		// payload bytes than the file holds, so the reader hits EOF mid-payload
		// and never reaches the tag verification at all. The tear is genuine;
		// only the bytes that the tear makes unreadable are fake.
		// -------------------------------------------------------------------
		payload := []byte(fmt.Sprintf(`{"prepare_index":%d}`, txn.PrepareIndex()))
		frame := make([]byte, wal.FrameHeaderSize+len(payload))
		binary.BigEndian.PutUint32(frame[0:4], uint32(len(payload)))
		binary.BigEndian.PutUint64(frame[4:12], txn.PrepareIndex()+1)
		binary.BigEndian.PutUint16(frame[12:14], uint16(wal.TypeCommit))
		binary.BigEndian.PutUint16(frame[14:16], 0) // reserved
		// frame[16:48] is the MAC and is left zero -- see above.
		copy(frame[wal.FrameHeaderSize:], payload)

		partial := wal.FrameHeaderSize + len(payload)/2
		if partial <= wal.FrameHeaderSize || partial >= len(frame) {
			t.Fatalf("child: a %d-byte cut of a %d-byte frame is not a torn payload", partial, len(frame))
		}
		f, err := os.OpenFile(l.Path(), os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatalf("child: OpenFile to append the torn commit: %v", err)
		}
		if _, err := f.Write(frame[:partial]); err != nil {
			t.Fatalf("child: writing the torn commit frame: %v", err)
		}
		// No Close, no Sync, no defer: the next statement is the kill.
		authSuicide()

	default:
		t.Fatalf("child: unknown crash point %q", point)
	}
	t.Fatalf("child: still running after SIGKILL")
}

// runAuthCrashChild re-execs this test binary at the given crash point and
// PROVES the child died on SIGKILL rather than failing its own assertions.
//
// This check is not ceremony. Without it, a child that t.Fatalf'd on its first
// line would exit 1, leave an empty log behind, and every "the agent is not on
// the roster" assertion in the parent would pass for the wrong reason.
func runAuthCrashChild(t *testing.T, point, dir string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestAuthCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), envAuthCrashPoint+"="+point, envAuthCrashDir+"="+dir)
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

// TestAuthCrashInjectionAfterPrepare is POINT A: the process died between the
// prepare fsync and the commit.
//
// Agent B was never acknowledged — Put never returned — so it must be invisible.
// Its NUMBER, though, reached the platter, and the floor derivation must see it.
// A floor of 1 here would hand the next "worker" enrolment agent B's id, giving
// a NEW agent holding a DIFFERENT keypair an identity this bus has already
// written down.
func TestAuthCrashInjectionAfterPrepare(t *testing.T) {
	dir := t.TempDir()
	runAuthCrashChild(t, crashAfterPrepare, dir)

	r, l := openRoster(t, dir)
	defer l.Close()

	wantA := normaliseEntry(crashAgentA(t))
	gotA, ok := r.Get(wantA.AgentID)
	if !ok {
		t.Fatalf("agent %q is NOT on the roster after the crash, but its Put returned before the kill: an acknowledged enrolment was lost (invariant 4)", wantA.AgentID)
	}
	if !reflect.DeepEqual(gotA, wantA) {
		t.Fatalf("agent %q came back changed by recovery.\n  got  %+v\n  want %+v", wantA.AgentID, gotA, wantA)
	}

	idB := mustAgentID(t, "worker", 7)
	if _, ok := r.Get(idB); ok {
		t.Fatalf("agent %q IS on the roster, but its COMMIT never reached disk; recovery surfaced an enrolment no caller was ever told about", idB)
	}
	if got := r.Len(); got != 1 {
		t.Fatalf("the roster holds %d agents, want exactly 1", got)
	}

	// The pairing that makes the design correct.
	floors, err := auth.EnrolmentSuffixesInWAL(l.Path(), testBusID)
	if err != nil {
		t.Fatalf("EnrolmentSuffixesInWAL: %v", err)
	}
	if got := floors["worker"]; got != 7 {
		t.Fatalf(`the floor for "worker" is %d, want 7.

Agent worker-7's PREPARE was fsynced before the kill, so its number reached the
platter. It is NOT enrolled and never will be -- and it must still never be
re-issued. A floor of %d hands the next "worker" enrolment an id this bus has
already written down (invariant 1).`, got, got)
	}
	if got, want := mintOverFloors(t, floors, "worker"), mustAgentID(t, "worker", 8); got != want {
		t.Fatalf("the next enrolment minted %q, want %q", got, want)
	}
}

// TestAuthCrashInjectionAfterCommit is POINT B and is invariant 4's actual
// claim: acknowledged means durable, with NO graceful shutdown involved
// anywhere. The child's Put returned and the next statement was the kill — no
// Close, no Sync, no defer, no runtime shutdown.
func TestAuthCrashInjectionAfterCommit(t *testing.T) {
	dir := t.TempDir()
	runAuthCrashChild(t, crashAfterCommit, dir)

	r, l := openRoster(t, dir)
	defer l.Close()

	want := normaliseEntry(crashAgentA(t))
	got, ok := r.Get(want.AgentID)
	if !ok {
		t.Fatalf(`agent %q is NOT on the roster.

Its Put RETURNED before the process was killed, which means the prepare and the
commit were both fsynced. Nothing is acknowledged before it is durable
(invariant 4), and nothing about that guarantee may depend on a clean shutdown.`,
			want.AgentID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agent %q survived the crash with different contents.\n  got  %+v\n  want %+v", want.AgentID, got, want)
	}
	if n := r.Len(); n != 1 {
		t.Fatalf("the roster holds %d agents, want 1", n)
	}
	// Specifically the reserved fields, which travelled the real write path.
	if got.InviteID != "invite-crash-a" {
		t.Errorf("the invite id did not survive the crash: got %q", got.InviteID)
	}
	if len(got.CertBindings) != 2 || got.CertBindings[0].RetiredAt == nil || got.CertBindings[1].RetiredAt != nil {
		t.Errorf("the certificate history did not survive the crash intact: %+v", got.CertBindings)
	}
}

// TestAuthCrashInjectionTornCommit is POINT C: the commit frame was half written
// when the machine died.
//
// Agent B's prepare is on disk, whole and fsynced. Its commit is not. Write
// never returned, so no caller, peer or relay was ever told agent B existed —
// recovery must repair the torn tail and leave agent B INVISIBLE. Getting this
// backwards would surface an enrolment nobody was ever promised.
func TestAuthCrashInjectionTornCommit(t *testing.T) {
	dir := t.TempDir()
	runAuthCrashChild(t, crashTornCommit, dir)
	path := walPath(dir)

	// (1) The tail really IS torn. Without this the rest would pass just as
	// happily against a healthy file and would prove nothing.
	if _, err := wal.Replay(path, nil); !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("Replay of the crashed log = %v, want ErrCorrupt: the child did not leave a torn frame, so this test is not exercising a torn commit", err)
	}

	// (2) Recovery repairs it and the bus starts.
	r, l := openRoster(t, dir)
	defer l.Close()

	wantA := normaliseEntry(crashAgentA(t))
	gotA, ok := r.Get(wantA.AgentID)
	if !ok {
		t.Fatalf("agent %q is NOT on the roster after the torn tail was repaired; the damage was BEHIND it and must not cascade", wantA.AgentID)
	}
	if !reflect.DeepEqual(gotA, wantA) {
		t.Fatalf("agent %q came back changed.\n  got  %+v\n  want %+v", wantA.AgentID, gotA, wantA)
	}

	idB := mustAgentID(t, "worker", 7)
	if _, ok := r.Get(idB); ok {
		t.Fatalf("agent %q IS on the roster off the back of a TORN commit frame; a half-written commit is not accepted history", idB)
	}
	if got := r.Len(); got != 1 {
		t.Fatalf("the roster holds %d agents, want exactly 1", got)
	}

	// (3) And, as at point A, agent B's number is still burned.
	floors, err := auth.EnrolmentSuffixesInWAL(path, testBusID)
	if err != nil {
		t.Fatalf("EnrolmentSuffixesInWAL: %v", err)
	}
	if got := floors["worker"]; got != 7 {
		t.Fatalf(`the floor for "worker" is %d, want 7: worker-7's PREPARE survived the repair, so its number is burned even though its commit was torn`, got)
	}
}
