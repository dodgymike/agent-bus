// IDEM-11's ACCEPTANCE EVIDENCE: real kill -9 crash injection proving the
// applied-key table is durable, WAL-recovered state and not an in-memory cache.
//
// "The code is written" is not evidence for a durability claim, and neither is a
// graceful restart. idem_test.go's TestAppliedKeyStoreSurvivesRestart closes the
// log politely first, so every deferred Close, buffer flush and runtime shutdown
// in the process gets to run. A SIGKILL is the only thing that proves none of
// them was load-bearing: the parent opens the exact bytes the dying process had
// put on disk, with nobody having tidied the tail on the way out.
//
// The three crash points here are the three windows that matter:
//
//   - AFTER the commit fsync, BEFORE the client is acknowledged — invariant 10's
//     reason for existing. The effect is durable; the ack was lost in flight.
//   - BETWEEN the prepare fsync and the commit fsync — invariant 5's
//     prefix-consistency case. The key must behave as NEVER SEEN, or a real
//     first attempt is silently swallowed.
//   - AFTER the commit fsync of a PRE-IDEM-11 entry (no Entry.Idem at all) —
//     the back-compat path, without which the first restart after this change
//     drops every applied key an existing on-disk log holds.
//
// The derivation guards IDEM-11 also requires — idem.ParkedPollMax ==
// hub.MaxPollTimeout and idem.MaxEntries == hub.MaxIdempotencyEntries — already
// exist as TestParkedPollMaxMatchesHub and TestMaxIdempotencyEntriesMatchesIdem
// in idem_test.go and are deliberately NOT duplicated here.
package hub_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	// envIdemCrashPoint selects where the child kills itself. Unset means "not a
	// crash child", which is what makes TestIdemCrashChild a no-op skip in a
	// normal run of the suite.
	envIdemCrashPoint = "HUB_IDEM_CRASH_POINT"
	// envIdemCrashDir is the data directory the child writes into: a t.TempDir()
	// belonging to the parent, so no test ever shares a data directory with
	// another and the tracked data/ dir is never touched.
	envIdemCrashDir = "HUB_IDEM_CRASH_DIR"

	// idemCrashPostCommit: the child's send is committed and fsynced, and the
	// process dies before publish can apply it to memory, remember the key, wake
	// a waiter or return. Nothing was acknowledged.
	idemCrashPostCommit = "post-commit-pre-ack"

	// idemCrashDanglingPrepare: the child's PREPARE — carrying the applied-key
	// record built by the real publish path — is fsynced, and the process dies
	// with no COMMIT record ever written.
	idemCrashDanglingPrepare = "dangling-prepare"

	// idemCrashLegacyEntry: the child commits a PRE-IDEM-11 shaped entry (a
	// message record carrying an idempotency key, with no Entry.Idem at all) and
	// dies the instant it is durable.
	idemCrashLegacyEntry = "legacy-entry-post-commit"
)

// The fixture the child and the parent must agree on byte-for-byte: the retry
// the parent sends has to be the SAME payload under the SAME key, or the parent
// would be testing the violation path by accident.
const (
	idemCrashKey       = "k-crash-post-commit"
	idemCrashSender    = "alpha"
	idemCrashRecipient = "beta"
)

var (
	idemCrashBody      = []byte("the acknowledgement this client never received")
	idemCrashOtherBody = []byte("a DIFFERENT payload reusing the same key")
)

// ---------------------------------------------------------------------------
// The child
// ---------------------------------------------------------------------------

// TestIdemCrashChild is the child half of every crash test below. It does
// NOTHING in a normal run: without envIdemCrashPoint it skips immediately.
func TestIdemCrashChild(t *testing.T) {
	point := os.Getenv(envIdemCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envIdemCrashPoint + " is unset")
	}
	dir := os.Getenv(envIdemCrashDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envIdemCrashPoint, point, envIdemCrashDir)
	}

	// closeOnCleanup is false throughout: a crash child must not have a deferred
	// Close registered, because a Close that ran would be exactly the graceful
	// shutdown these tests exist to rule out.
	lg := openTestLog(t, dir, false)
	sender := agentID(t, testBusID, idemCrashSender)
	recipient := agentID(t, testBusID, idemCrashRecipient)

	switch point {
	case idemCrashPostCommit:
		h := newHubOverDurable(t, &killAfterCommit{l: lg}, lg, testBusID, idemCrashSender, idemCrashRecipient)
		res, err := mintedSend(t, h, hub.SendRequest{
			Sender:         sender,
			To:             recipient,
			Body:           idemCrashBody,
			IdempotencyKey: idemCrashKey,
		})
		t.Fatalf("child: Send returned (%+v, %v) but the durable log kills this process the instant the commit is fsynced; the crash was never injected", res, err)

	case idemCrashDanglingPrepare:
		h := newHubOverDurable(t, &killAfterPrepare{l: lg}, lg, testBusID, idemCrashSender, idemCrashRecipient)
		res, err := mintedSend(t, h, hub.SendRequest{
			Sender:         sender,
			To:             recipient,
			Body:           idemCrashBody,
			IdempotencyKey: idemCrashKey,
		})
		t.Fatalf("child: Send returned (%+v, %v) but the durable log kills this process the instant the prepare is fsynced; the crash was never injected", res, err)

	case idemCrashLegacyEntry:
		// Written the PRE-IDEM-11 way, deliberately NOT through publish: a
		// wal.Entry with no Idem field at all, exactly as a binary built before
		// this change would have written it. Seq 1 is what publish would have
		// minted on an empty log.
		m, err := store.NewMessage(testBusID, sender, false, []string{recipient}, 1, time.Now().UTC(), idemCrashBody, idemCrashKey, fixtureTimestampMs, fixtureSignature())
		if err != nil {
			t.Fatalf("child: store.NewMessage: %v", err)
		}
		payload, err := m.Encode()
		if err != nil {
			t.Fatalf("child: Encode: %v", err)
		}
		// The real two-phase path: this returns only once the commit is fsynced.
		if _, err := lg.Write(wal.Entry{Kind: store.RecordKind, Body: payload}); err != nil {
			t.Fatalf("child: Write: %v", err)
		}
		// No Close, no Sync, no defer: the next statement is the kill.
		killSelf()

	default:
		t.Fatalf("child: unknown crash point %q", point)
	}
	t.Fatalf("child: still running after SIGKILL")
}

// killAfterCommit is the honest post-commit, pre-ack kill.
//
// It delegates to the REAL *wal.Log.Write — the whole prepare, commit and fsync
// cycle — and kills the process before returning, so publish never reaches
// store.Append, never reaches idem.Remember, never notifies a waiter and never
// returns a Result. The message is on stable storage and nothing in this process
// or any client knows it.
type killAfterCommit struct{ l *wal.Log }

func (k *killAfterCommit) Write(e wal.Entry) (wal.Committed, error) {
	if passThroughSeqFloor(e) {
		return k.l.Write(e)
	}
	// Asserted here rather than in the parent because this is the only place the
	// entry publish built can be seen BEFORE it is written: the applied-key
	// record must travel in the SAME transaction as the effect, not in a second
	// write ordered after it.
	if len(e.Idem) == 0 {
		return wal.Committed{}, fmt.Errorf("child: the entry publish handed to the durable log carries NO applied-key record; the record must ride in the same two-phase transaction as the effect (IDEM-11)")
	}
	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	killSelf()
	return c, nil
}

// killAfterPrepare stops the two-phase write between its two fsyncs: the PREPARE
// record — carrying the applied-key record publish built — is durable and
// complete, and no COMMIT record is ever written.
//
// The Txn is deliberately never resolved and the transaction lock is never
// released. Nothing runs after the kill, so there is nothing to release it for.
type killAfterPrepare struct{ l *wal.Log }

func (k *killAfterPrepare) Write(e wal.Entry) (wal.Committed, error) {
	if passThroughSeqFloor(e) {
		return k.l.Write(e)
	}
	if len(e.Idem) == 0 {
		return wal.Committed{}, fmt.Errorf("child: the entry publish handed to the durable log carries NO applied-key record; this crash point exists to prove an UNCOMMITTED one is not recovered, so it must be present to begin with")
	}
	if _, err := k.l.Begin(e); err != nil {
		return wal.Committed{}, err
	}
	// Begin returns only after the PREPARE record is fsynced.
	killSelf()
	return wal.Committed{}, nil
}

// passThroughSeqFloor reports that e is a SEQUENCE-FLOOR record and must be
// written for real, without the assertion or the kill.
//
// Since SIGN-1's reserve-then-send, a send is TWO durable writes and only the
// second one is the message: Hub.Mint burns a batch of sequence numbers ahead
// (hub.SeqFloorRecordKind) before it hands a number to the client, and that
// record legitimately carries NO applied-key record — it is not an operation a
// client performed, so there is no key to remember.
//
// Both crash points below therefore have to let it through, for two separate
// reasons, and dropping either check would be a silent way to stop injecting a
// crash at all:
//
//   - the "no applied-key record" assertion would fire on it and turn the child
//     into a failed mint, so the send would be refused with ErrUnknownMint and
//     the crash would never happen;
//   - killing on it would crash the process at the MINT rather than at the
//     message write, which is not the window either test is about.
func passThroughSeqFloor(e wal.Entry) bool { return e.Kind == hub.SeqFloorRecordKind }

// killSelf kills this process with SIGKILL. SIGKILL cannot be caught, blocked or
// ignored, so nothing deferred, buffered or graceful runs afterwards — which is
// the entire evidentiary value of these tests over a polite Close.
func killSelf() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	// Unreachable: the signal is delivered before Kill returns. Panicking rather
	// than looping means a platform where that is somehow untrue fails loudly
	// instead of hanging the suite.
	panic("hub idem crash test: SIGKILL to self did not kill the process")
}

// runIdemCrashChild re-execs this test binary at the given crash point and
// asserts the child really DIED ON SIGKILL rather than failing its own
// assertions — without that check a broken child would silently turn the parent
// into a test of an empty directory.
func runIdemCrashChild(t *testing.T, point, dir string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}

	// Bounded, so a wedged child fails this test in a minute rather than hanging
	// the suite until the package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestIdemCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), envIdemCrashPoint+"="+point, envIdemCrashDir+"="+dir)
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
		t.Fatalf("crash child %q: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s",
			point, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child %q: wait status is %T, want syscall.WaitStatus", point, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s",
			point, ws.ExitStatus(), out.String())
	}
}

// replayCommitted reads the committed history straight off the crashed log,
// read-only, without opening a writer on it. It is how the parent learns what
// the dying child actually got onto stable storage.
func replayCommitted(t *testing.T, dir string) []wal.Committed {
	t.Helper()
	var got []wal.Committed
	if _, err := wal.Replay(filepath.Join(dir, wal.WALFileName), func(c wal.Committed) error {
		got = append(got, c)
		return nil
	}); err != nil {
		t.Fatalf("replaying the crashed log in %s: %v", dir, err)
	}
	return got
}

// committedMessages narrows a replayed history to the MESSAGE transactions.
//
// Since SIGN-1's reserve-then-send a send costs TWO durable transactions, not
// one: Hub.Mint commits a sequence-floor record (hub.SeqFloorRecordKind) burning
// a batch of numbers ahead, and only then does the message's own transaction
// follow. A raw count of committed entries therefore no longer answers the
// question these tests ask — "did the child's MESSAGE reach stable storage?" —
// and counting the floor record as an effect would let a crash at the wrong
// moment look like a successful send.
//
// The floor records are not ignored: they are asserted for separately, because
// their presence BEFORE the message is the whole of the guarantee mint.go
// makes.
func committedMessages(committed []wal.Committed) []wal.Committed {
	var out []wal.Committed
	for _, c := range committed {
		if c.Entry.Kind == store.RecordKind {
			out = append(out, c)
		}
	}
	return out
}

// committedSeqFloors is committedMessages' counterpart: the sequence-floor
// records a mint wrote.
func committedSeqFloors(committed []wal.Committed) []wal.Committed {
	var out []wal.Committed
	for _, c := range committed {
		if c.Entry.Kind == hub.SeqFloorRecordKind {
			out = append(out, c)
		}
	}
	return out
}

// storedMessages counts what the serving copy holds and where its head sits.
func storedMessages(t *testing.T, h *hub.Hub) (int, uint64) {
	t.Helper()
	count, _, _, head, _ := h.Store().Stats()
	return count, head
}

// ---------------------------------------------------------------------------
// Case 1 — killed AFTER the commit fsync, BEFORE the client was acknowledged
// ---------------------------------------------------------------------------

// TestIdemCrashPostCommitRetryReplaysTheOriginal is the scenario invariant 10
// exists for, injected for real: the message is committed and fsynced, the
// process dies before the caller can be told, and the client — which saw a
// dropped connection, not a result — retries.
//
// A retry of the same key with the same payload must return the ORIGINAL result
// and re-apply NOTHING. A retry of the same key with a DIFFERENT payload must
// still be the protocol violation that disconnects, ACROSS THE RESTART BOUNDARY.
func TestIdemCrashPostCommitRetryReplaysTheOriginal(t *testing.T) {
	dir := t.TempDir()
	runIdemCrashChild(t, idemCrashPostCommit, dir)

	// (0) What the dying process actually left on stable storage. Without this
	// the rest could pass just as happily against a directory where nothing was
	// ever written, and would prove nothing.
	all := replayCommitted(t, dir)
	// The reservation's floor record must be durable too, and it must be durable
	// FIRST. That ordering is the whole of mint.go's guarantee — a number handed
	// to a client before it is durably burned is a number a restart can hand out
	// again — and a SIGKILL is the only way to observe it without taking the
	// process's word for it.
	if len(committedSeqFloors(all)) == 0 {
		t.Fatalf("the crashed log holds no sequence-floor record among its %d committed entries: the child's mint handed out a sequence without first durably burning it, so a restart could reissue that message id (invariant 1)", len(all))
	}
	if all[0].Entry.Kind != hub.SeqFloorRecordKind {
		t.Fatalf("the first committed entry is %q, want %q: the floor record must reach stable storage BEFORE the message whose sequence it burns", all[0].Entry.Kind, hub.SeqFloorRecordKind)
	}
	committed := committedMessages(all)
	if len(committed) != 1 {
		t.Fatalf("the crashed log holds %d committed MESSAGE entries, want exactly 1: the child died before its send was durable, so there is no post-commit crash to recover from", len(committed))
	}
	if len(committed[0].Entry.Idem) == 0 {
		t.Fatalf("the committed transaction carries NO applied-key record (Entry.Idem is empty): IDEM-11's load-bearing requirement is that the record commits in the SAME two-phase transaction as the effect, not in a second write ordered after it")
	}
	original, err := store.Decode(committed[0].Entry.Body)
	if err != nil {
		t.Fatalf("decoding the committed message: %v", err)
	}

	// --- RESTART on the SAME directory. ---
	lg := openTestLog(t, dir, true)
	h := newHubOver(t, lg, testBusID, idemCrashSender, idemCrashRecipient)
	sender := agentID(t, testBusID, idemCrashSender)
	recipient := agentID(t, testBusID, idemCrashRecipient)

	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("after recovering from a SIGKILL the applied-key table holds %d records, want 1: the table must be REBUILT FROM THE DURABLE LOG, not held in memory — a process killed with -9 flushes nothing", got)
	}
	count, head := storedMessages(t, h)
	if count != 1 || head != original.Seq {
		t.Fatalf("the recovered store holds %d messages with head %d, want 1 message at seq %d", count, head, original.Seq)
	}

	// (a) THE RETRY: same key, same payload. The original result, no error, and
	// NO DISCONNECT.
	again, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             recipient,
		Body:           idemCrashBody,
		IdempotencyKey: idemCrashKey,
	})
	if err != nil {
		t.Fatalf("retrying the same key with the same payload after the crash returned err = %v, want the ORIGINAL result and no error: this is a legitimate retry of a lost acknowledgement, so it must NOT error and must NOT disconnect the client (invariant 10's legitimate-retry carve-out)", err)
	}
	if !again.Replayed {
		t.Errorf("the retry returned Replayed = false, want true: the caller must be able to tell that NOTHING was re-applied")
	}
	if again.MessageID != original.ID || again.Seq != original.Seq {
		t.Errorf("the retry returned message %s / seq %d, want the original %s / seq %d",
			again.MessageID, again.Seq, original.ID, original.Seq)
	}
	if !again.SentAt.Equal(original.SentAt) {
		t.Errorf("the retry returned sent_at %s, want the original %s: the stored result must be returned verbatim, not re-minted",
			again.SentAt.Format(time.RFC3339Nano), original.SentAt.Format(time.RFC3339Nano))
	}
	if again.Sender != sender || again.Broadcast {
		t.Errorf("the retry returned sender %q / broadcast %v, want %q / false — the scope-derived fields did not survive recovery", again.Sender, again.Broadcast, sender)
	}

	// (b) EXACTLY ONE EFFECT. The serving copy, the sequence head and the durable
	// log all have to agree that the retry produced nothing.
	count, head = storedMessages(t, h)
	if count != 1 || head != original.Seq {
		t.Fatalf("after the retry the store holds %d messages with head %d, want 1 message still at seq %d: the retry was RE-APPLIED, which is the double-apply invariant 10 forbids", count, head, original.Seq)
	}
	if after := committedMessages(replayCommitted(t, dir)); len(after) != 1 {
		t.Fatalf("after the retry the durable log holds %d committed MESSAGE entries, want 1: the retry wrote a SECOND effect to disk", len(after))
	}
	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("after the retry the applied-key table holds %d records, want 1", got)
	}

	// (c) THE VIOLATION, ACROSS THE RESTART BOUNDARY: the same key with a
	// DIFFERENT payload is a protocol violation, and the fingerprint that detects
	// it has to have survived the crash on disk.
	if _, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             recipient,
		Body:           idemCrashOtherBody,
		IdempotencyKey: idemCrashKey,
	}); !errors.Is(err, hub.ErrIdempotencyKeyReused) {
		t.Fatalf("reusing the key for DIFFERENT content after the crash gave err = %v, want ErrIdempotencyKeyReused: the payload fingerprint must survive the crash, or a key reused for new content is silently answered with somebody else's result", err)
	}
	count, _ = storedMessages(t, h)
	if count != 1 {
		t.Fatalf("the refused key-reuse left %d messages in the store, want 1: a rejected violation must write nothing", count)
	}
}

// ---------------------------------------------------------------------------
// Case 2 — killed BETWEEN the prepare fsync and the commit fsync
// ---------------------------------------------------------------------------

// TestIdemCrashDanglingPrepareLeavesTheKeyUnseen is invariant 5's
// prefix-consistency case. The applied-key record reached stable storage inside
// a PREPARE that never committed, so the effect is not part of accepted history
// — and neither is the key.
//
// If a key whose effect never committed came back as "already applied", the
// client's REAL first attempt would be answered with a result that names a
// message nobody ever received. The operation would be silently swallowed.
func TestIdemCrashDanglingPrepareLeavesTheKeyUnseen(t *testing.T) {
	dir := t.TempDir()
	runIdemCrashChild(t, idemCrashDanglingPrepare, dir)

	// (0) No MESSAGE committed — the crash really did land between the two
	// fsyncs of the message's own transaction.
	//
	// The mint's sequence-floor record IS committed, and must be: it was written
	// and fsynced one step earlier, before the number was handed out. That is the
	// correct outcome and not a leak — the number stays burned, the client
	// re-mints under the same key and gets a fresh one, and the gap is what
	// internal/ids/sequence.go documents as correct.
	all := replayCommitted(t, dir)
	if got := committedMessages(all); len(got) != 0 {
		t.Fatalf("the crashed log holds %d committed MESSAGE entries, want 0: the child was supposed to die with its prepare unresolved", len(got))
	}
	if len(committedSeqFloors(all)) == 0 {
		t.Fatalf("the crashed log holds no sequence-floor record: the child's mint handed out a sequence without first durably burning it (invariant 1)")
	}

	lg := openTestLog(t, dir, true)
	rec := lg.Recovered()
	if len(rec.Dangling) == 0 {
		t.Fatalf("Recovered().Dangling = %v, want a discarded prepare: without one there is no unresolved transaction and this test proves nothing", rec.Dangling)
	}
	// The ONLY transactions applied are the mint's floor records. Expressed as a
	// count of those rather than as a literal 0, so it still fails the moment the
	// dangling prepare is applied — a literal that was simply bumped to 1 would
	// pass just as happily if the prepare were the thing recovered and the floor
	// record the thing dropped.
	if want := uint64(len(committedSeqFloors(all))); rec.Applied != want {
		t.Fatalf("Recovered().Applied = %d, want %d (the mint's sequence-floor record(s) and nothing else): an uncommitted prepare must not be applied", rec.Applied, want)
	}

	h := newHubOver(t, lg, testBusID, idemCrashSender, idemCrashRecipient)
	sender := agentID(t, testBusID, idemCrashSender)
	recipient := agentID(t, testBusID, idemCrashRecipient)

	if st := h.IdempotencyStats(); st.Count != 0 {
		t.Fatalf("the applied-key table holds %d records after recovering a DANGLING prepare, want 0: the key's effect never committed, so it is not part of accepted history and must behave as NEVER SEEN (invariant 5)", st.Count)
	}
	if count, _ := storedMessages(t, h); count != 0 {
		t.Fatalf("the recovered store holds %d messages, want 0: an uncommitted prepare is not history", count)
	}

	// The client's REAL first attempt. It must be applied as new.
	res, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             recipient,
		Body:           idemCrashBody,
		IdempotencyKey: idemCrashKey,
	})
	if err != nil {
		t.Fatalf("the first attempt after a dangling prepare returned err = %v, want success: nothing was ever committed under this key", err)
	}
	if res.Replayed {
		t.Fatalf("the first attempt after a dangling prepare returned Replayed = true with message %s: a key whose effect never committed was remembered as applied, so a genuine first attempt was SILENTLY SWALLOWED and answered with a message that was never delivered", res.MessageID)
	}
	count, head := storedMessages(t, h)
	if count != 1 || head != res.Seq {
		t.Fatalf("after the first attempt the store holds %d messages with head %d, want exactly 1 at seq %d", count, head, res.Seq)
	}
	if got := committedMessages(replayCommitted(t, dir)); len(got) != 1 {
		t.Fatalf("the durable log holds %d committed MESSAGE entries after the first attempt, want exactly 1", len(got))
	}
	if st := h.IdempotencyStats(); st.Count != 1 {
		t.Fatalf("the applied-key table holds %d records after the first attempt succeeded, want 1", st.Count)
	}
	// And from here on it is an ordinary applied key: a retry replays.
	retry, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             recipient,
		Body:           idemCrashBody,
		IdempotencyKey: idemCrashKey,
	})
	if err != nil || !retry.Replayed || retry.MessageID != res.MessageID {
		t.Fatalf("retrying the newly-applied key returned (%+v, %v), want the result of %s replayed", retry, err, res.MessageID)
	}
}

// ---------------------------------------------------------------------------
// Case 3 — the back-compat replay path, crashed
// ---------------------------------------------------------------------------

// TestIdemCrashPreIdemLogRebuildsAppliedKey covers the upgrade case under a real
// kill: a log written BEFORE IDEM-11 has no Entry.Idem, only the message's own
// idempotency key.
//
// Without the reconstruction path every applied key in an existing on-disk log
// would be lost on the FIRST restart after this change — a durability REGRESSION
// delivered by a durability improvement, exactly once, at the upgrade, where it
// is hardest to notice. idem_test.go covers the graceful-restart shape of this;
// here the process is killed the instant the legacy entry is durable, which is
// the shape an upgrade actually takes when the old binary is stopped hard.
func TestIdemCrashPreIdemLogRebuildsAppliedKey(t *testing.T) {
	dir := t.TempDir()
	runIdemCrashChild(t, idemCrashLegacyEntry, dir)

	committed := replayCommitted(t, dir)
	if len(committed) != 1 {
		t.Fatalf("the crashed log holds %d committed entries, want exactly 1", len(committed))
	}
	if committed[0].Entry.Idem != nil {
		t.Fatalf("the fixture entry carries an applied-key record (%s); this test must model a PRE-IDEM-11 log, which has none", committed[0].Entry.Idem)
	}
	original, err := store.Decode(committed[0].Entry.Body)
	if err != nil {
		t.Fatalf("decoding the committed message: %v", err)
	}
	if original.IdempotencyKey != idemCrashKey {
		t.Fatalf("the legacy message carries idempotency key %q, want %q", original.IdempotencyKey, idemCrashKey)
	}

	lg := openTestLog(t, dir, true)
	h := newHubOver(t, lg, testBusID, idemCrashSender, idemCrashRecipient)
	sender := agentID(t, testBusID, idemCrashSender)
	recipient := agentID(t, testBusID, idemCrashRecipient)

	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("the applied-key table holds %d records after replaying a PRE-IDEM-11 log, want 1: the key must be rebuilt from the message record, or every applied key in an existing log is dropped at the upgrade", got)
	}

	again, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             recipient,
		Body:           idemCrashBody,
		IdempotencyKey: idemCrashKey,
	})
	if err != nil {
		t.Fatalf("retrying a pre-IDEM-11 key returned err = %v, want the original result", err)
	}
	if !again.Replayed || again.MessageID != original.ID || again.Seq != original.Seq {
		t.Fatalf("retrying a pre-IDEM-11 key returned %+v, want the original %s / seq %d replayed", again, original.ID, original.Seq)
	}
	if count, head := storedMessages(t, h); count != 1 || head != original.Seq {
		t.Fatalf("after the retry the store holds %d messages with head %d, want 1 at seq %d: the retry of a legacy key was RE-APPLIED", count, head, original.Seq)
	}

	// The fingerprint is RECOMPUTED on this path rather than read off disk, so it
	// has to be the same digest publish computes — otherwise a legitimate retry
	// would be reported as a key-reuse violation, and a genuine violation would
	// be answered with somebody else's result.
	if _, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             recipient,
		Body:           idemCrashOtherBody,
		IdempotencyKey: idemCrashKey,
	}); !errors.Is(err, hub.ErrIdempotencyKeyReused) {
		t.Fatalf("reusing a pre-IDEM-11 key for DIFFERENT content gave err = %v, want ErrIdempotencyKeyReused", err)
	}
}
