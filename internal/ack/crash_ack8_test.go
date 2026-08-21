//go:build linux || darwin

// ACK-8's CRASH-INJECTION EVIDENCE: a real SIGKILL inside the two-phase write
// path of a delivery-lifecycle SETTLE, and what recovery then yields.
//
// # WHY A SIGKILL, AND WHY THE SETTLE TRANSITION SPECIFICALLY
//
// CLAUDE.md requires durability and recovery code to have crash-injection tests
// — "a test that writes, kills at a chosen point in the write path, and asserts
// what recovery yields" — because "the code looks right" is not evidence for a
// durability claim. A graceful Close is not evidence either: it runs every
// deferred flush and buffer in the process, so it proves only that the happy
// path tidies up. Only a SIGKILL leaves the file in the state a power cut would.
//
// internal/hub/ack_crash_test.go already does this for E1, the `accepted`
// transition, at three crash points. It is the ONLY crash coverage the ack plane
// had. This file covers what that one does not: the SETTLE transition — E5
// `delivered`, E6 `refused`, E4 `undeliverable` — which is the transition that
// writes a TERMINAL row, and therefore the transition where a crash can produce
// the two failures that matter most and are exact opposites of one another:
//
//	A TERMINAL OUTCOME THAT WAS ACKNOWLEDGED AND THEN LOST     (invariant 4)
//	A TERMINAL OUTCOME THAT WAS NEVER COMMITTED AND IS VISIBLE (invariant 5)
//
// The second is the subtler one and it is why the dangling-prepare case exists.
// A recovered `delivered` row that reached only PREPARE would tell a sender its
// message was accepted by the recipient's application on the strength of a
// transaction that never committed. That is not a lost message; it is a false
// statement about one, which is worse, because the sender stops retrying.
//
// # THE THREE CRASH POINTS
//
//	ack8CrashBeforeSettle  killed before any settle byte reaches the file.
//	                       Recovery must yield `accepted` — the settle simply
//	                       never happened.
//	ack8CrashSettlePrepare killed after the PREPARE record is fsynced and before
//	                       any COMMIT. Recovery must yield `accepted`, must NOT
//	                       yield the terminal, and must never re-use the index the
//	                       discarded prepare burned (invariant 1).
//	ack8CrashSettleCommit  killed after the COMMIT record is fsynced, before
//	                       Settle returns and before foldIn runs. Recovery MUST
//	                       yield the terminal: it is durable, so it is history,
//	                       whether or not any caller was ever told.
//
// The last one also proves something about the implementation that is easy to
// get wrong in the other direction: `foldIn` is a SERVING-COPY update, not part
// of the durability contract. Recovery must produce the row from the log alone,
// with the process that wrote it having died before folding anything in.
package ack_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	// envAck8CrashPoint selects where the child kills itself. Unset means "not a
	// crash child", which is what makes the child test a no-op skip in an
	// ordinary run of this package.
	envAck8CrashPoint = "ACK8_CRASH_POINT"
	// envAck8CrashDir is the data directory the child writes into: a t.TempDir()
	// belonging to the parent. No test ever shares a data directory with another
	// and the tracked data/ dir is never touched.
	envAck8CrashDir = "ACK8_CRASH_DIR"

	ack8CrashBeforeSettle  = "before-settle"
	ack8CrashSettlePrepare = "settle-prepare"
	ack8CrashSettleCommit  = "settle-commit"
)

// The fixture the child and every parent must agree on.
const ack8CrashKey = "testbus-4242"

// ---------------------------------------------------------------------------
// The child
// ---------------------------------------------------------------------------

// TestAck8SettleCrashChild is the child half of every crash test in this file.
// Without envAck8CrashPoint it skips immediately, so it costs nothing in a
// normal run.
func TestAck8SettleCrashChild(t *testing.T) {
	point := os.Getenv(envAck8CrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envAck8CrashPoint + " is unset")
	}
	dir := os.Getenv(envAck8CrashDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envAck8CrashPoint, point, envAck8CrashDir)
	}

	// NO defer Close and NO t.Cleanup that closes: a Close that ran would be
	// exactly the graceful shutdown this file exists to rule out.
	st := ack.NewStore(ack.Options{})
	l, err := wal.Open(wal.LogOptions{Dir: dir, Applier: st})
	if err != nil {
		t.Fatalf("child: wal.Open: %v", err)
	}

	var at string
	switch point {
	case ack8CrashBeforeSettle:
		at = ack8KillBeforeWrite
	case ack8CrashSettlePrepare:
		at = ack8KillAtPrepare
	case ack8CrashSettleCommit:
		at = ack8KillAtCommit
	default:
		t.Fatalf("child: unknown crash point %q", point)
	}

	// nth = 2: the FIRST ack write is the Accept below, which must complete for
	// real — the row has to exist before there is a settle to crash. The second
	// is the Settle, and that is the one cut into.
	durable := &ack8KillOnNth{l: l, nth: 2, at: at}
	if err := st.Attach(durable); err != nil {
		t.Fatalf("child: Attach: %v", err)
	}

	if err := st.Accept(ack8CrashKey, ack8Sender, ack8Recipient); err != nil {
		t.Fatalf("child: Accept must succeed before the settle can be crashed: %v", err)
	}
	err = st.Settle(ack8CrashKey, ack8Recipient, ack.StateDelivered, "", ack.AttestedByRecipientSignatureUnverified)
	t.Fatalf("child: Settle returned %v but the durable log kills this process at %q; THE CRASH WAS NEVER INJECTED and any parent assertion built on it would be meaningless", err, point)
}

// ack8Kill* name the three points in the two-phase cycle this file cuts at.
const (
	ack8KillBeforeWrite = "before-write"
	ack8KillAtPrepare   = "prepare"
	ack8KillAtCommit    = "commit"
)

// ack8KillOnNth kills the process at a chosen point in the write path, on the
// nth write of ack's kind.
//
// Counting is not a convenience. The child performs two ack writes and would
// perform more if the store grew a transition; killing on the first one that
// arrives would cut the ACCEPT rather than the SETTLE, and the test would still
// look like an injected crash while asserting something else entirely. Every
// other kind is passed through untouched for the same reason.
type ack8KillOnNth struct {
	l   *wal.Log
	nth int
	at  string
	n   int
}

func (k *ack8KillOnNth) Write(e wal.Entry) (wal.Committed, error) {
	if e.Kind != ack.RecordKind {
		return k.l.Write(e)
	}
	k.n++
	if k.n != k.nth {
		return k.l.Write(e)
	}
	switch k.at {
	case ack8KillBeforeWrite:
		// Not one byte of this transaction ever reaches the file.
		ack8KillSelf()
	case ack8KillAtPrepare:
		if _, err := k.l.Begin(e); err != nil {
			return wal.Committed{}, err
		}
		// Begin returns only once the PREPARE record is FSYNCED. The Txn is
		// deliberately never resolved and the transaction lock is never
		// released: nothing runs after the kill, so there is nothing to release
		// it for.
		ack8KillSelf()
	}
	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	// Write returns only once the COMMIT record is fsynced. The entry is
	// accepted history; the caller is about to not be told.
	ack8KillSelf()
	return c, nil
}

// ack8KillSelf is a real SIGKILL to this process. Not os.Exit: os.Exit still
// runs the runtime's exit path, and the point is that NOTHING runs.
func ack8KillSelf() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {} // unreachable; the signal is not catchable
}

// ---------------------------------------------------------------------------
// The parent
// ---------------------------------------------------------------------------

// runAck8CrashChild runs the child at `point` against dir and REQUIRES that it
// died of SIGKILL.
//
// The requirement is the load-bearing part. A child that exited normally — or
// failed to build, or skipped — would leave the parent asserting against a
// directory nothing crashed in, and every assertion below would pass for the
// wrong reason. That is the stale-fixture failure mode, and this is the check
// that makes it impossible.
func runAck8CrashChild(t *testing.T, dir, point string) {
	t.Helper()

	// A DEADLINE, matching internal/hub/ack_crash_test.go. A child that hangs
	// instead of dying must fail this test in seconds; without a deadline it
	// would hang until the PACKAGE timeout and report as an unrelated panic,
	// which is a much harder failure to read.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAck8SettleCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		envAck8CrashPoint+"="+point,
		envAck8CrashDir+"="+dir,
	)
	out, err := cmd.CombinedOutput()

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child at %q returned err=%v, want an ExitError from SIGKILL.\nIf it exited cleanly the crash was never injected and nothing below is evidence.\nchild output:\n%s", point, err, out)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child at %q did not die of SIGKILL (waitstatus %v).\nchild output:\n%s", point, ee.Sys(), out)
	}
	// A child that SKIPPED is a child that never ran the write path.
	if strings.Contains(string(out), "--- SKIP") {
		t.Fatalf("crash child at %q SKIPPED; the crash window was never entered.\nchild output:\n%s", point, out)
	}
}

// TestAckSettleCrashedAfterCommitIsRecoveredAsTerminal — invariant 4.
//
// The COMMIT record is fsynced and the process dies before Settle returns and
// before foldIn touches the serving copy. Nobody was told, and memory never
// learned. The record is nonetheless on stable storage, so it is accepted
// history and recovery must produce it.
//
// This is the direction people find easy to believe and hard to prove, because
// the in-memory table of a process that survived would show the same thing. Here
// the process that wrote it is gone.
func TestAckSettleCrashedAfterCommitIsRecoveredAsTerminal(t *testing.T) {
	dir := t.TempDir()
	runAck8CrashChild(t, dir, ack8CrashSettleCommit)

	st, l, _, _ := openAck8(t, dir)
	defer l.Close()

	r, ok := st.Lookup(ack8CrashKey, ack8Recipient)
	if !ok {
		t.Fatalf("no row at all after a crash that had already fsynced the settle COMMIT record. Invariant 4: nothing is acknowledged before it is durable, and the converse — what IS durable is history — is what makes the guarantee worth anything.")
	}
	if r.State != ack.StateDelivered {
		t.Errorf("recovered state = %s, want delivered. The terminal transition's COMMIT was fsynced before the kill, so it is accepted history; recovering the row as %s loses a settled outcome that the disk plainly records.", r.State, r.State)
	}
	if r.AttestedBy != ack.AttestedByRecipientSignatureUnverified {
		t.Errorf("recovered attested_by = %q, want %q: the terminal record carries its provenance and a restart must not drop it", r.AttestedBy, ack.AttestedByRecipientSignatureUnverified)
	}
	if r.SettledAt.IsZero() {
		t.Errorf("recovered settled_at is zero on a terminal row; it is the terminal retention anchor, so a zero one retires the row on the very next sweep")
	}
	if r.AcceptedAt.IsZero() {
		t.Errorf("recovered accepted_at is zero; Settle carries it forward from the accepted row precisely so a terminal row still records when the message was ACCEPTED")
	}
}

// TestAckSettleCrashedAtPrepareIsNotRecovered — invariant 5, and invariant 1.
//
// The PREPARE record is fsynced; no COMMIT is ever written. The transaction did
// not happen, and recovery must yield a PREFIX of accepted history: the row is
// still `accepted`, and the terminal it was about to become is nowhere.
//
// It then asserts the invariant-1 half, which is the one with live precedent
// elsewhere in this repo: the prepare BURNED an index. That index must never be
// handed out again, even though the record carrying it was discarded. A
// recovered bus that re-used it would write a NEW record at an index a discarded
// one already carried — the same shape as the sibling finding where a bus
// re-issued sequence 257 over a record already written at 1000.
func TestAckSettleCrashedAtPrepareIsNotRecovered(t *testing.T) {
	dir := t.TempDir()
	runAck8CrashChild(t, dir, ack8CrashSettlePrepare)

	st, l, spy, _ := openAck8(t, dir)
	defer l.Close()

	r, ok := st.Lookup(ack8CrashKey, ack8Recipient)
	if !ok {
		t.Fatalf("the ACCEPTED row is missing. Its own write completed before the crash window opened, so losing it means recovery dropped a record it had already acknowledged (invariant 4).")
	}
	if r.State != ack.StateAccepted {
		t.Errorf("recovered state = %s, want accepted. The terminal transition reached PREPARE and never COMMIT, so it is not accepted history. Serving it would tell the sender its message was acknowledged by the recipient's application on the strength of a transaction that never committed — not a lost message, but a FALSE STATEMENT about one, which is worse because the sender stops retrying (invariant 5).", r.State)
	}
	if !r.SettledAt.IsZero() {
		t.Errorf("recovered settled_at = %s on a non-terminal row; the settle never committed and nothing may carry its timestamp", r.SettledAt.UTC())
	}

	// INVARIANT 1: the burned index is never handed out again.
	rec := l.Recovered()
	if len(rec.Dangling) == 0 {
		t.Fatalf("recovery reports NO dangling prepare, but the child was killed between the prepare fsync and the commit. Either the crash was not injected where this test believes, or the discarded prepare left no trace — and a discard that leaves no trace is invariant 6's actual defect.")
	}
	burned := rec.Dangling[len(rec.Dangling)-1]
	if rec.NextIndex <= burned {
		t.Errorf("Recovered().NextIndex = %d but index %d was burned by the discarded prepare. Invariant 1: when recovery discards a record the sequence ADVANCES PAST THE HOLE; it never rewinds.", rec.NextIndex, burned)
	}

	// Prove it with an actual append, not only with the reported number.
	if err := st.Settle(ack8CrashKey, ack8Recipient, ack.StateDelivered, "", ack.AttestedByRecipientSignatureUnverified); err != nil {
		t.Fatalf("settling the recovered row: %v", err)
	}
	got := spy.lastCommitted()
	if got.PrepareIndex <= burned {
		t.Errorf("the first write after recovery took prepare index %d, but the discarded prepare had already burned %d. A salvage path that re-uses the index of a damaged record is a DEFECT to fix, not a licence to narrow invariant 1.", got.PrepareIndex, burned)
	}
}

// TestAckSettleCrashedBeforeAnyWriteLeavesTheRowAccepted is the control.
//
// It kills before a single settle byte reaches the file, so recovery must yield
// exactly the accepted row and nothing else. Without it, the prepare case above
// could pass for the wrong reason — a recovery that discarded EVERY settle,
// committed or not, would satisfy it — and this pins the boundary from the other
// side by proving the difference between the three points is the crash point
// itself.
func TestAckSettleCrashedBeforeAnyWriteLeavesTheRowAccepted(t *testing.T) {
	dir := t.TempDir()
	runAck8CrashChild(t, dir, ack8CrashBeforeSettle)

	st, l, _, _ := openAck8(t, dir)
	defer l.Close()

	r, ok := st.Lookup(ack8CrashKey, ack8Recipient)
	if !ok {
		t.Fatalf("the accepted row is missing; its write completed before the crash window opened")
	}
	if r.State != ack.StateAccepted {
		t.Errorf("recovered state = %s, want accepted: no settle byte was ever written", r.State)
	}
	if n := st.Len(); n != 1 {
		t.Errorf("Len = %d, want exactly 1 row", n)
	}
	// And recovery reports no dangling prepare, because none was written. This
	// is what distinguishes this case from the prepare case on the disk itself
	// rather than only in the assertion.
	if d := l.Recovered().Dangling; len(d) != 0 {
		t.Errorf("recovery reports dangling prepares %v, want none: the child died before Write was called at all", d)
	}
}
