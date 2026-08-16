//go:build linux || darwin

// ACK-2's ACCEPTANCE EVIDENCE: real kill -9 crash injection proving the
// sender-visible delivery lifecycle row is durable, WAL-recovered state and not
// an in-memory cache.
//
// CLAUDE.md is explicit that "the code looks right" is not evidence for a
// durability claim, and neither is a graceful restart: a polite Close lets every
// deferred flush, buffer and runtime shutdown in the process run, so it proves
// only that the happy path tidies up. A SIGKILL is the only thing that proves
// none of that was load-bearing — the parent opens the exact bytes the dying
// process had put on disk, with nobody having tidied the tail on the way out.
//
// The three crash points are the three windows that matter, and each asserts a
// DIFFERENT half of the property:
//
//		NOTHING ACKNOWLEDGED IS EVER LOST       (invariant 4)
//		NOTHING UNACKNOWLEDGED IS EVER VISIBLE  (invariant 5: recovery yields a
//		                                         PREFIX of accepted history)
//
//	  - ackCrashPostCommit     — the lifecycle row's own COMMIT is fsynced and the
//	    process dies before publish can wake a waiter or return. The row MUST
//	    survive, and it must sit AFTER the message it describes.
//	  - ackCrashDanglingPrepare — the row's PREPARE is fsynced and no COMMIT is
//	    ever written. The row MUST behave as never seen; a recovered `accepted`
//	    here would be a durability claim about a transaction that never committed.
//	  - ackCrashBeforeRow      — the MESSAGE is committed and the process dies
//	    before the row is written at all. The message MUST survive and the row
//	    MUST be absent, which is the honest, documented degradation: the sender is
//	    later told `unknown` rather than `accepted` (ACK-CONTRACT.md §11.3, §14 D1).
//	    This case exists to prove the window Hub.recordAcceptance names is the
//	    window that actually exists, rather than a comment nobody checked.
package hub_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	// envAckCrashPoint selects where the child kills itself. Unset means "not a
	// crash child", which is what makes TestAckCrashChild a no-op skip in a
	// normal run of the suite.
	envAckCrashPoint = "HUB_ACK_CRASH_POINT"
	// envAckCrashDir is the data directory the child writes into: a t.TempDir()
	// belonging to the parent, so no test ever shares a data directory with
	// another and the tracked data/ dir is never touched.
	envAckCrashDir = "HUB_ACK_CRASH_DIR"

	ackCrashPostCommit      = "ack-post-commit"
	ackCrashDanglingPrepare = "ack-dangling-prepare"
	ackCrashBeforeRow       = "ack-before-row"
)

// The fixture the child and the parent must agree on.
const (
	ackCrashKey       = "k-ack-crash"
	ackCrashSender    = "alpha"
	ackCrashRecipient = "beta"
)

var ackCrashBody = []byte("the message whose acceptance row this test is about")

// ---------------------------------------------------------------------------
// The child
// ---------------------------------------------------------------------------

// TestAckCrashChild is the child half of every crash test below. It does
// NOTHING in a normal run: without envAckCrashPoint it skips immediately.
func TestAckCrashChild(t *testing.T) {
	point := os.Getenv(envAckCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envAckCrashPoint + " is unset")
	}
	dir := os.Getenv(envAckCrashDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envAckCrashPoint, point, envAckCrashDir)
	}

	// closeOnCleanup is false throughout: a crash child must not have a deferred
	// Close registered, because a Close that ran would be exactly the graceful
	// shutdown these tests exist to rule out.
	lg := openTestLog(t, dir, false)

	var killAt string
	killKind := ack.RecordKind
	switch point {
	case ackCrashPostCommit:
		killAt = killAtCommit
	case ackCrashDanglingPrepare:
		killAt = killAtPrepare
	case ackCrashBeforeRow:
		killAt = killAtBeforeWrite
	default:
		t.Fatalf("child: unknown crash point %q", point)
	}
	durable := &killOnKind{l: lg, kind: killKind, at: killAt}

	// The lifecycle table records through the SAME wrapper the hub writes
	// through, which is what puts its write inside the crash window at all. In
	// production both are the one *wal.Log.
	acks := ack.NewStore(ack.Options{})
	if err := acks.Attach(durable); err != nil {
		t.Fatalf("child: attaching the lifecycle table: %v", err)
	}

	h := openAckHub(t, durable, lg, acks, ackCrashSender, ackCrashRecipient)
	res, err := mintedSend(t, h, hub.SendRequest{
		Sender:         agentID(t, testBusID, ackCrashSender),
		To:             agentID(t, testBusID, ackCrashRecipient),
		Body:           ackCrashBody,
		IdempotencyKey: ackCrashKey,
	})
	t.Fatalf("child: Send returned (%+v, %v) but the durable log kills this process at %q; the crash was never injected", res, err, point)
}

// killAt* name the three points in the two-phase cycle this file cuts at.
const (
	killAtBeforeWrite = "before-write"
	killAtPrepare     = "prepare"
	killAtCommit      = "commit"
)

// killOnKind kills the process at a chosen point in the write path, for entries
// of ONE wal.Entry.Kind only.
//
// Every other kind is written FOR REAL and passed through untouched, which is
// not a convenience: a send is three durable transactions since SIGN-1's
// reserve-then-send (the sequence floor, the message, and now the lifecycle
// row), and killing on the wrong one would crash the process at a window none of
// these tests is about while still looking like an injected crash.
type killOnKind struct {
	l    *wal.Log
	kind string
	at   string
}

func (k *killOnKind) Write(e wal.Entry) (wal.Committed, error) {
	if e.Kind != k.kind {
		return k.l.Write(e)
	}
	switch k.at {
	case killAtBeforeWrite:
		// Nothing of this kind ever reaches the file.
		killSelf()
	case killAtPrepare:
		if _, err := k.l.Begin(e); err != nil {
			return wal.Committed{}, err
		}
		// Begin returns only after the PREPARE record is fsynced. The Txn is
		// deliberately never resolved and the transaction lock is never
		// released: nothing runs after the kill, so there is nothing to release
		// it for.
		killSelf()
	}
	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	killSelf()
	return c, nil
}

// runAckCrashChild re-execs this test binary at the given crash point and
// asserts the child really DIED ON SIGKILL rather than failing its own
// assertions — without that check a broken child would silently turn the parent
// into a test of an empty directory.
func runAckCrashChild(t *testing.T, point, dir string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestAckCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), envAckCrashPoint+"="+point, envAckCrashDir+"="+dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child %q did not finish in time: %v\n--- child output ---\n%s", point, ctx.Err(), out.String())
	}
	// A child that failed its OWN assertions also exits non-zero, so "err != nil"
	// is not the assertion. The wait status is.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child %q: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s", point, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child %q: wait status is %T, want syscall.WaitStatus", point, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s", point, ws.ExitStatus(), out.String())
	}
}

// committedOfKind narrows a replayed history to one wal.Entry.Kind.
func committedOfKind(committed []wal.Committed, kind string) []wal.Committed {
	var out []wal.Committed
	for _, c := range committed {
		if c.Entry.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// replayAckStore rebuilds a lifecycle table from the durable log ALONE, exactly
// as wal.Open does in production by handing every committed entry to the
// registered applier.
//
// The store it returns is UNATTACHED, deliberately: nothing here may write, so a
// row it holds can only have come off disk. That is what makes this a test of
// invariant 5 (memory is the serving copy, disk is the truth) and not a test of
// a store that was handed its answer.
func replayAckStore(t *testing.T, dir string) *ack.Store {
	t.Helper()
	s := ack.NewStore(ack.Options{})
	if _, err := wal.Replay(filepath.Join(dir, wal.WALFileName), func(c wal.Committed) error {
		return s.Apply(c)
	}); err != nil {
		t.Fatalf("replaying the crashed log in %s: %v", dir, err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Case 1 — killed AFTER the lifecycle row's commit fsync
// ---------------------------------------------------------------------------

// TestAckLocalAcceptanceDurable is ACK-2's headline claim, injected for real:
// a local send's sender-visible ACCEPTANCE row is on stable storage before
// anything in the process — or any client — knows it, and a restart rebuilds it
// from the log alone.
//
// It asserts five separate things, because four of them would each pass on a
// build that got the fifth wrong:
//
//  1. the row's transaction COMMITTED (it is in the replayed history at all);
//  2. it sits AFTER the message it describes — `accepted` means "this bus has
//     committed and fsynced the message", so a row ordered before its own
//     message would be claiming a durability the message did not yet have;
//  3. its CONTENT is the contract's: state `accepted`, the correlation key read
//     through store.Message.OriginID(), the fully-qualified sender and
//     recipient, and NO class, NO attestation and NO settled_at — the fields
//     that are forbidden until something terminal happens;
//  4. a table rebuilt from the log ALONE holds exactly that row (invariant 5);
//  5. nothing else was written — one row per recipient, not two.
func TestAckLocalAcceptanceDurable(t *testing.T) {
	dir := t.TempDir()
	runAckCrashChild(t, ackCrashPostCommit, dir)

	all := replayCommitted(t, dir)
	msgs := committedOfKind(all, store.RecordKind)
	if len(msgs) != 1 {
		t.Fatalf("the crashed log holds %d committed MESSAGE entries among %d, want exactly 1: the child died before its send was durable, so there is no acceptance row to recover", len(msgs), len(all))
	}
	m, err := store.Decode(msgs[0].Entry.Body)
	if err != nil {
		t.Fatalf("decoding the committed message: %v", err)
	}

	rows := committedOfKind(all, ack.RecordKind)
	if len(rows) != 1 {
		t.Fatalf("the crashed log holds %d committed %q entries among %d, want exactly 1: the sender-visible acceptance row did not reach stable storage, so a restart would report `unknown` for a message this bus demonstrably accepted", len(rows), ack.RecordKind, len(all))
	}
	// (2) ORDER. Both are committed; the message must be the earlier one.
	if rows[0].CommitIndex <= msgs[0].CommitIndex {
		t.Fatalf("the acceptance row committed at index %d, at or BEFORE the message at %d: `accepted` asserts that this bus has committed and fsynced the message, so a row that reaches disk first claims a durability the message does not yet have (invariant 4)",
			rows[0].CommitIndex, msgs[0].CommitIndex)
	}

	// (3) CONTENT, decoded through the package's own strict decoder rather than
	// by matching bytes, so the assertion is about the record and not about a
	// spelling.
	r, err := ack.DecodeRecord(rows[0].Entry.Body)
	if err != nil {
		t.Fatalf("decoding the committed acceptance row: %v\n--- raw ---\n%s", err, rows[0].Entry.Body)
	}
	if r.State != ack.StateAccepted {
		t.Errorf("the recovered row is %s, want accepted: local acceptance is plane A and nothing in this build may advance it further", r.State)
	}
	if r.CorrelationKey != m.OriginID() {
		t.Errorf("the row's correlation key is %q, want %q (store.Message.OriginID()): the key is the ORIGIN bus's server-minted message id and is never a fourth identifier", r.CorrelationKey, m.OriginID())
	}
	if want := agentID(t, testBusID, ackCrashSender); r.Sender != want {
		t.Errorf("the row's sender is %q, want the fully-qualified %q (invariant 2)", r.Sender, want)
	}
	if want := agentID(t, testBusID, ackCrashRecipient); r.Recipient != want {
		t.Errorf("the row's recipient is %q, want the fully-qualified %q (invariant 2)", r.Recipient, want)
	}
	if r.Class != "" {
		t.Errorf("the row carries class %q; a class is set IFF the state is a NEGATIVE terminal, and an accepted row has nothing to explain", r.Class)
	}
	if r.AttestedBy != "" {
		t.Errorf("the row carries attestation %q; attestation is set IFF the state is terminal", r.AttestedBy)
	}
	if !r.SettledAt.IsZero() {
		t.Errorf("the row carries settled_at %s; it is set IFF the state is terminal, and it is what the retention sweep reads", r.SettledAt.Format(time.RFC3339Nano))
	}
	if r.AcceptedAt.IsZero() {
		t.Error("the row carries no accepted_at; it is the retention anchor for a non-terminal row, so a row without one could never be swept")
	}
	// The BODY is not in the record and must never be. Invariant 6: the durable
	// trail is metadata and routing only.
	if bytes.Contains(rows[0].Entry.Body, ackCrashBody) {
		t.Errorf("the durable acceptance row contains the MESSAGE BODY; the trail records metadata and routing ONLY (invariant 6)\n--- raw ---\n%s", rows[0].Entry.Body)
	}

	// (4) The table rebuilt from disk alone.
	s := replayAckStore(t, dir)
	got, ok := s.Lookup(m.OriginID(), agentID(t, testBusID, ackCrashRecipient))
	if !ok {
		t.Fatalf("a lifecycle table rebuilt from the crashed log holds NO row for %s -> %s: the table must be REBUILT FROM THE DURABLE LOG, not held in memory — a process killed with -9 flushes nothing (invariant 5)",
			m.OriginID(), agentID(t, testBusID, ackCrashRecipient))
	}
	if got.State != ack.StateAccepted {
		t.Errorf("the rebuilt row is %s, want accepted", got.State)
	}
	// (5) One row per recipient, and this send had one recipient.
	if n := s.Len(); n != 1 {
		t.Errorf("the rebuilt table holds %d rows, want exactly 1: a directed send produces one row PER RECIPIENT and this send named one", n)
	}
}

// ---------------------------------------------------------------------------
// Case 2 — killed BETWEEN the row's prepare fsync and its commit fsync
// ---------------------------------------------------------------------------

// TestAckDanglingPrepareIsNotRecovered is invariant 5's half of the property:
// NOTHING UNACKNOWLEDGED IS EVER VISIBLE.
//
// The row's PREPARE record is complete and fsynced on disk. No COMMIT record was
// ever written. Recovery must therefore behave as though the row was never seen
// — a recovered `accepted` here would be a durability claim about a transaction
// that never completed, which is exactly the torn-write case the two-phase path
// exists to make impossible.
//
// The MESSAGE, which committed before it, must be untouched: a dangling prepare
// on a later transaction may not take an earlier committed one down with it.
func TestAckDanglingPrepareIsNotRecovered(t *testing.T) {
	dir := t.TempDir()
	runAckCrashChild(t, ackCrashDanglingPrepare, dir)

	all := replayCommitted(t, dir)
	if n := len(committedOfKind(all, store.RecordKind)); n != 1 {
		t.Fatalf("the crashed log holds %d committed MESSAGE entries, want 1: a dangling PREPARE on the lifecycle row must not cost the message that already committed", n)
	}
	if rows := committedOfKind(all, ack.RecordKind); len(rows) != 0 {
		t.Fatalf("the crashed log holds %d COMMITTED %q entries, want 0: the child died between the prepare fsync and the commit fsync, so nothing was acknowledged and nothing may be visible (invariant 5)\n--- first row ---\n%s",
			len(rows), ack.RecordKind, rows[0].Entry.Body)
	}
	if n := replayAckStore(t, dir).Len(); n != 0 {
		t.Fatalf("a lifecycle table rebuilt from the crashed log holds %d rows, want 0: an uncommitted prepare must not become a recovered acceptance", n)
	}
}

// ---------------------------------------------------------------------------
// Case 3 — killed AFTER the message commit and BEFORE the row is written
// ---------------------------------------------------------------------------

// TestAckCrashBeforeRowLeavesMessageDurableAndStatusUnknown pins the window
// Hub.recordAcceptance names in its doc comment, so that comment is a checked
// claim rather than a remembered one.
//
// The row is written in a SECOND wal transaction after the message's own commit.
// That is a TRADE and not a limitation — a composite Entry.Kind could carry both
// in one transaction, exactly as auth.EnrolInviteRecordKind ("agent+invite")
// already does, and Hub.recordAcceptance says why it was not taken. A crash in
// between therefore leaves the message DURABLE
// with NO lifecycle row, and the sender is later told `unknown` rather than
// `accepted`.
//
// That is a bounded loss of OBSERVATION and never of the message — the same
// degradation the capacity refusal makes by design (ACK-CONTRACT.md §11.3) — and
// closing it is ACK-8's (§14 D1). This test asserts the loss is exactly that
// shape: the message survives, the row is absent, and NOTHING half-written is
// recovered in its place.
func TestAckCrashBeforeRowLeavesMessageDurableAndStatusUnknown(t *testing.T) {
	dir := t.TempDir()
	runAckCrashChild(t, ackCrashBeforeRow, dir)

	all := replayCommitted(t, dir)
	msgs := committedOfKind(all, store.RecordKind)
	if len(msgs) != 1 {
		t.Fatalf("the crashed log holds %d committed MESSAGE entries, want 1: the message must be durable before the lifecycle row is even attempted (invariant 4)", len(msgs))
	}
	if rows := committedOfKind(all, ack.RecordKind); len(rows) != 0 {
		t.Fatalf("the crashed log holds %d %q entries, want 0: this crash point kills the process before the row is written at all", len(rows), ack.RecordKind)
	}
	m, err := store.Decode(msgs[0].Entry.Body)
	if err != nil {
		t.Fatalf("decoding the committed message: %v", err)
	}
	s := replayAckStore(t, dir)
	if _, ok := s.Lookup(m.OriginID(), agentID(t, testBusID, ackCrashRecipient)); ok {
		t.Fatalf("a lifecycle table rebuilt from the crashed log holds a row for %s: no row was ever written, so recovering one would mean the table invented a durability claim", m.OriginID())
	}
	// The honest consequence, asserted rather than described: a caller looking
	// this key up finds nothing, which the status API must render as `unknown`.
	// It must NOT be rendered as any durable state, and `unknown` must never be
	// written back.
	if n := s.Len(); n != 0 {
		t.Fatalf("the rebuilt table holds %d rows, want 0", n)
	}
}
