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
//	D  a TORN PREPARE frame                     -> repaired tail, absent, and
//	                                               the burned suffix is
//	                                               UNREADABLE FROM THE LOG
//
// Point A carries the single most important assertion in this file: agent 2's
// number is BURNED even though agent 2 is not enrolled. That pairing — the
// enrolment is lost, the id is never re-issued — is what makes the whole design
// correct, and it is exactly the case a committed-state-only derivation cannot
// see.
//
// Point D is point A's counterexample and the reason floors.go shouts. A prepare
// that was WRITTEN but only half reached the platter is a suffix this bus handed
// out and that no scan of the log can ever name again. Everything derived from
// the log — including EnrolmentSuffixesInWAL, which A leans on — is blind to it.
// The number stays burned only because ids.OpenNameSuffixes wrote the floor
// AHEAD of issuing it, in a file the tear cannot reach. D is what makes "the log
// is not a floor" an executable assertion rather than a comment.
//
// Every child is proven to have died on SIGKILL. Without that check a child
// that failed its own assertions and exited non-zero would silently turn the
// parent into a test of nothing.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
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

	// crashTornPrepare: agent B never got a whole PREPARE. A partial prepare
	// frame — the one carrying agent B's enrolment record itself — sits on the
	// end of the file, so the bytes naming agent B's suffix are the bytes that
	// did not survive.
	crashTornPrepare = "torn-prepare"
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

	if point == crashTornPrepare {
		// -------------------------------------------------------------------
		// POINT D. Agent B never gets a whole PREPARE.
		//
		// The same honest account as point C below applies: a SIGKILL cannot
		// tear a write by itself, so the partial frame is written deliberately
		// and the 32 MAC bytes are zeros because the frame tag is keyed per
		// file and the keying is unexported. Both are UNOBSERVABLE here — the
		// length header promises more payload than the file holds, so the
		// reader hits EOF mid-payload and never reaches the tag (see
		// internal/wal/reader.go's truncated-payload branch, which returns
		// before verifyTag). What the kill contributes, and what cannot be
		// faked, is that nothing graceful runs afterwards.
		//
		// WHAT IS DIFFERENT FROM POINT C, and the whole reason this point
		// exists: the torn frame is the PREPARE, so the bytes that did not
		// survive are the enrolment record itself — the only place on disk
		// that ever said the words "worker-7".
		//
		// The index is agent A's Put plus two. A Put is exactly one prepare
		// and one commit through wal.Log.Write, and this child opened a fresh
		// directory, so the next index is deterministic. The parent asserts it
		// (the repair report names the discarded index), which is what stops a
		// wrong guess here from quietly turning point D into a test of a frame
		// nobody was looking at.
		next := l.Recovered().NextIndex + 2
		payload, err := json.Marshal(struct {
			Kind string          `json:"kind"`
			TS   string          `json:"ts"`
			Body json.RawMessage `json:"body"`
		}{Kind: auth.RecordKind, TS: crashTime.Format(time.RFC3339Nano), Body: body})
		if err != nil {
			t.Fatalf("child: marshalling the prepare payload: %v", err)
		}
		frame := make([]byte, wal.FrameHeaderSize+len(payload))
		binary.BigEndian.PutUint32(frame[0:4], uint32(len(payload)))
		binary.BigEndian.PutUint64(frame[4:12], next)
		binary.BigEndian.PutUint16(frame[12:14], uint16(wal.TypePrepare))
		binary.BigEndian.PutUint16(frame[14:16], 0) // reserved
		// frame[16:48] is the MAC and is left zero -- see above.
		copy(frame[wal.FrameHeaderSize:], payload)

		partial := wal.FrameHeaderSize + len(payload)/2
		if partial <= wal.FrameHeaderSize || partial >= len(frame) {
			t.Fatalf("child: a %d-byte cut of a %d-byte frame is not a torn payload", partial, len(frame))
		}
		f, err := os.OpenFile(l.Path(), os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatalf("child: OpenFile to append the torn prepare: %v", err)
		}
		if _, err := f.Write(frame[:partial]); err != nil {
			t.Fatalf("child: writing the torn prepare frame: %v", err)
		}
		// No Close, no Sync, no defer: the next statement is the kill.
		authSuicide()
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

// TestAuthCrashInjectionTornPrepare is POINT D: the machine died while the
// ENROLMENT RECORD ITSELF was being written.
//
// Points A and C both leave agent B's prepare WHOLE on disk, so the log still
// carries the string "worker-7" and a scan can find it. This point removes that
// last copy: the torn frame IS the prepare, so the only bytes that ever named
// agent B's suffix are the bytes that did not reach the platter.
//
// # What this test pins, in order of importance
//
//  1. The bus STARTS (invariant 6). A half-written enrolment at the tail is
//     repaired and recovery reaches a running server; it never refuses to boot.
//  2. The discard is SPECIFIC, not silent — the repair report names the offset,
//     the index and the record TYPE. Silent discard is the P0, not discard.
//  3. Damage at the tail does not cascade BACKWARDS: agent A, committed before
//     it, comes back byte-for-byte.
//  4. Agent B is absent. Its Put never returned, so nothing was ever promised.
//  5. The index it consumed is NOT reissued (invariant 1, reaffirmed without
//     narrowing): recovery advances past the hole rather than rewinding into it.
//  6. AND THE ONE THIS POINT EXISTS FOR: EnrolmentSuffixesInWAL comes back with
//     NO TRACE of suffix 7. Read that as a LIMITATION being pinned, never as a
//     correctness property. It is the crash-level proof of floors.go's contract
//     — the log is an audit scan and NOT a floor — because here the log is
//     provably incapable of naming a suffix this bus handed out. What keeps
//     worker-7 burned is ids.OpenNameSuffixes, which persists each name's floor
//     BEFORE issuing it, in a file this tear cannot reach — and to be exact
//     about what is and is not demonstrated here, this test builds no allocator
//     and its data directory holds no agent-suffixes file at all. It pins the
//     HOLE; internal/ids' own suite owns the mitigation. A build that ever
//     sealed this map into an allocator would resume at worker-2 and walk back
//     over worker-7 — a suffix this bus has already issued — and no test that
//     only tears a COMMIT can see that.
func TestAuthCrashInjectionTornPrepare(t *testing.T) {
	dir := t.TempDir()
	runAuthCrashChild(t, crashTornPrepare, dir)
	path := walPath(dir)

	// (1) The tail really IS torn, and torn in the PAYLOAD rather than merely
	// short. Without this the rest would pass just as happily against a healthy
	// file and would prove nothing.
	_, err := wal.Replay(path, nil)
	if !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("Replay of the crashed log = %v, want ErrCorrupt: the child did not leave a torn frame, so this test is not exercising a torn prepare", err)
	}
	if !strings.Contains(err.Error(), "truncated payload") {
		t.Fatalf("Replay reported %v; want a TRUNCATED PAYLOAD. Any other damage means the frame header itself did not survive, which is a different failure from the one this point injects", err)
	}

	// (2) Before the repair, the derivation FAILS TOTALLY rather than handing
	// back the half of the log it could read. A partial map here would be the
	// worst possible answer: it looks like an answer.
	if floors, err := auth.EnrolmentSuffixesInWAL(path, testBusID); err == nil {
		t.Fatalf("EnrolmentSuffixesInWAL over the UNREPAIRED log returned %v with a nil error; a log whose tail is torn is not a log that can be reported on, and a caller cannot tell an incomplete scan from a complete one", floors)
	} else if len(floors) != 0 {
		t.Fatalf("EnrolmentSuffixesInWAL returned a %d-entry map alongside its error, want none; failure is TOTAL", len(floors))
	}

	// (3) Recovery repairs it and the bus starts.
	r, l := openRoster(t, dir)
	defer l.Close()

	rec := l.Recovered()
	if !rec.Repaired.Truncated {
		t.Fatalf("recovery did not report truncating anything: %+v\nThe child left a torn frame on the end of the file, so the tail must have been repaired", rec.Repaired)
	}
	// (4) The discard is specific. Invariant 6's absolute requirement is that
	// every discard is logged SPECIFICALLY, and "specifically" means the report
	// can say what was lost -- here, that it was a PREPARE and which index.
	if n := len(rec.Repaired.Discards); n != 1 {
		t.Fatalf("recovery recorded %d discards, want exactly 1: %+v", n, rec.Repaired.Discards)
	}
	d := rec.Repaired.Discards[0]
	if !d.TypeKnown || d.Type != wal.TypePrepare {
		t.Fatalf("the discard is reported as type %v (known=%v), want a PREPARE.\nIf the header did not survive, the child tore the frame in the wrong place and points 5 and 6 below are testing something else: %+v", d.Type, d.TypeKnown, d)
	}
	if d.Index != 3 {
		t.Fatalf("the discarded record carried index %d, want 3 (agent A's Put is one prepare and one commit, so agent B's prepare is the third record). The child's arithmetic and this assertion must agree, or the tear landed on a frame nobody is looking at: %+v", d.Index, d)
	}
	if d.Reason == "" {
		t.Fatalf("the discard carries no reason: %+v\nA discard an operator cannot act on is the silent-discard defect wearing a struct", d)
	}
	if d.Stage != "framing" {
		t.Fatalf("the discard was decided at stage %q, want \"framing\": a torn frame is caught by the salvage pass over the raw file, not by replay. If this says \"replay\" the frame was whole and the damage is in its CONTENT, which is a different injection: %+v", d.Stage, d)
	}
	// The loss is BOUNDED: recovery can say exactly what it removed.
	//
	// READ Discard.Severe FOR WHAT IT IS. It is the explicit OVERRIDE flag, and
	// internal/wal/salvage.go sets it in exactly one place (the `exhausted`
	// case, salvage.go:476): records dropped WITHOUT proof they were
	// unreadable, because the search for the next intact record ran out of its
	// work budget. That is the "I do not know what I just deleted" case, and it
	// is the one an operator must never see for an ordinary power loss.
	//
	// IT IS NOT THE PREPARE-VERSUS-COMMIT DISTINCTION, and a reviewer suggested
	// it was. Measured, because the difference matters: the computed severity
	// (a lost COMMIT is ERROR, a type-known PREPARE is WARN) lives in the
	// UNEXPORTED method Discard.severe(), which this package cannot call, and
	// the exported field is false for a torn commit too — asserting it here
	// caught nothing when point D's frame was retyped as a COMMIT. So this
	// assertion pins bounded-versus-unbounded loss, which is real, and claims
	// nothing about record type. Point C's severity is point C's business.
	if d.Severe {
		t.Fatalf("the discarded frame is flagged SEVERE: %+v\nSevere is set only when recovery dropped bytes it could NOT prove were unreadable. A single torn frame at the tail is fully bounded -- recovery knows the offset, the length and the type -- so seeing it here means the salvage search gave up, and the loss is larger and less identified than this test believes", d)
	}

	// (5) Damage at the tail does not cascade backwards.
	wantA := normaliseEntry(crashAgentA(t))
	gotA, ok := r.Get(wantA.AgentID)
	if !ok {
		t.Fatalf("agent %q is NOT on the roster after the torn prepare was repaired; its own commit fsynced long before the damage, which is BEHIND it and must not cascade", wantA.AgentID)
	}
	if !reflect.DeepEqual(gotA, wantA) {
		t.Fatalf("agent %q came back changed.\n  got  %+v\n  want %+v", wantA.AgentID, gotA, wantA)
	}

	// (6) Agent B is absent: its Put never returned, so nobody was ever told.
	idB := mustAgentID(t, "worker", 7)
	if _, ok := r.Get(idB); ok {
		t.Fatalf("agent %q IS on the roster off the back of a TORN PREPARE; half an enrolment record is not an enrolment", idB)
	}
	if got := r.Len(); got != 1 {
		t.Fatalf("the roster holds %d agents, want exactly 1", got)
	}

	// (7) The index the torn record consumed is never handed out again.
	if next := rec.NextIndex; next <= d.Index {
		t.Fatalf("recovery resumes at index %d, but index %d was already written to this file and then discarded. An index this data directory has authorised is never authorised again -- when recovery discards a record the sequence advances PAST the hole, it never rewinds into it (invariant 1)", next, d.Index)
	}

	// (8) THE POINT. See the doc comment: this asserts a LIMITATION.
	floors, err := auth.EnrolmentSuffixesInWAL(path, testBusID)
	if err != nil {
		t.Fatalf("EnrolmentSuffixesInWAL over the REPAIRED log: %v", err)
	}
	if got := floors["worker"]; got != 1 {
		t.Fatalf(`the floor this scan reports for "worker" is %d, want 1.

Want 1 -- NOT 7 -- and that is not an aspiration, it is the limitation this
point exists to pin. worker-7's suffix was handed out by this bus and the only
record of it went down with the torn prepare; nothing left in the log can name
it. If this ever reports 7, the scan has started reading bytes the repair
discarded and the whole file's corruption story needs re-reading.

What keeps worker-7 burned is NOT this map. It is ids.OpenNameSuffixes, which
writes each name's floor before issuing it, in a file the tear never touched. On
a real bus that file would still say 7; this test does NOT build an allocator,
so it pins the LIMITATION and leaves the mitigation to internal/ids' own suite.
Sealing THIS map into an allocator would resume at
worker-2 and walk straight back over worker-7, re-issuing a suffix this bus has
already handed out (invariant 1). Note worker-2 itself was never issued and is
not on disk anywhere; the collision is the walk, not the first number. That is
exactly what floors.go forbids in capitals.`, got)
	}
}
