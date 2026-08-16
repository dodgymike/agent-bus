// RELAY-24-BLOCKER-HUBINGEST's CRASH-INJECTION EVIDENCE: real kill -9 proving
// that (*hub.Hub).IngestRelayed — a NEW ENTRY into the two-phase write path —
// recovers the way the local send path does, and that a peer's retry after a
// lost acknowledgement resolves against WAL-RECOVERED applied-key state rather
// than against an in-memory cache that a SIGKILL takes with it.
//
// # WHY THIS SUITE EXISTS SEPARATELY FROM idem_crash_test.go
//
// idem_crash_test.go proves the property for a LOCAL SEND (idem.OpSend, a
// client-held mint reservation, store.LocalBusPath provenance). A relayed
// ingest differs on all three: the operation is idem.OpRelay, the sequence is
// minted INTERNALLY because a peer bus holds no reservation, and the record
// carries a multi-hop bus path. None of those is exercised by the local-send
// crash tests, and each is a way for recovery to be subtly wrong for relayed
// traffic while every existing crash test stays green.
//
// # THE TWO WINDOWS, AND WHAT EACH ONE IS ABOUT
//
//   - AFTER the commit fsync, BEFORE IngestRelayed returns. The message is
//     durable and the PEER WAS TOLD NOTHING — the relay handler's 200 never
//     went out. A peer retrying a lost ack is the NORMAL steady state of a
//     relay, so the retry must resolve as idem.OutcomeRetry against the
//     original local id. This is the load-bearing case: idem.Outcome's ZERO
//     VALUE is idem.OutcomeNew, and OutcomeNew is the answer relay.Acceptor
//     RE-FORWARDS on, so a duplicate reported as new is an amplification loop
//     across the bus path rather than a cosmetic wrong answer.
//   - BETWEEN the prepare fsync and the commit fsync. The applied-key record
//     reached stable storage inside a transaction that never committed, so it is
//     not part of accepted history and neither is the key (invariant 5: recover
//     to a PREFIX of accepted history). Treating it as seen would SILENTLY
//     SWALLOW the peer's genuine first delivery of that message.
//
// Invariants read in full before writing this: 1 (server-authoritative,
// never-reused ids), 4 (nothing acknowledged before it is durable), 5 (disk is
// the truth; recovery yields a prefix), 6 (append-only metadata log; loud
// discard), 10 (idempotency's three uncollapsed outcomes).
package hub_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	// envRelayCrashPoint selects where the child kills itself. Unset means "not
	// a crash child", which is what makes TestRelayIngestCrashChild a no-op skip
	// in a normal run of the suite.
	//
	// It is a DIFFERENT variable from idem_crash_test.go's HUB_IDEM_CRASH_POINT,
	// and the child below is a different test function, for a mechanical reason:
	// that switch lives in a file this task may not edit, so a case cannot be
	// added to it. The re-exec runner further down is a deliberate copy of
	// runIdemCrashChild for the same reason — it must name THIS child's test
	// function and THESE environment variables.
	envRelayCrashPoint = "HUB_RELAY_INGEST_CRASH_POINT"
	// envRelayCrashDir is the data directory the child writes into: a
	// t.TempDir() belonging to the parent, so no test shares a data directory
	// with another and the tracked data/ dir is never touched.
	envRelayCrashDir = "HUB_RELAY_INGEST_CRASH_DIR"

	// relayCrashPostCommit: the relayed message's commit is fsynced and the
	// process dies before IngestRelayed can apply it to memory, remember the
	// key, wake a waiter or return. NOTHING WAS ACKNOWLEDGED TO THE PEER.
	relayCrashPostCommit = "relay-post-commit-pre-ack"

	// relayCrashDanglingPrepare: the PREPARE — carrying the applied-key record
	// the real publish path built — is fsynced, and the process dies with no
	// COMMIT record ever written.
	relayCrashDanglingPrepare = "relay-dangling-prepare"

	// relayCrashPeerBuckets commits two relayed messages whose origin bus differs
	// only by case, then dies immediately after the second commit fsync. Recovery
	// must rebuild one canonical foreign-peer bucket from those durable records.
	relayCrashPeerBuckets = "relay-peer-buckets-post-second-commit"
)

// The fixture the child and the parent must agree on BYTE FOR BYTE. The parent
// re-ingests the same relayed message the child died writing, so any drift here
// would silently turn the retry assertion into a test of the violation path (a
// different body under the same key) or of a different key entirely.
const (
	relayCrashOriginBus = "busorigin"
	relayCrashMiddleBus = "busmiddle"
	// The ORIGIN agent's short name. Fully qualified as
	// "busorigin.alpha-1" — a sender on a FOREIGN bus, which is the whole point
	// of IngestRelayed: every other exported write path here refuses it.
	relayCrashOriginAgent = "alpha"
	// The LOCAL recipient, enrolled on testBusID in both processes.
	relayCrashLocalAgent = "bob"
	// The ORIGIN sequence, which makes the origin message id "busorigin-7". A
	// relayed message's idempotency key IS the origin's message id, and it must
	// parse as one whose bus half is both the sender's bus and BusPath[0].
	relayCrashOriginSeq = 7
)

// relayCrashBody is the payload; a single package-level value so the two
// processes cannot disagree about the bytes the fingerprint is computed over.
var relayCrashBody = []byte("a relayed message whose acknowledgement to the peer was lost in the crash")

// relayCrashRecordedPath is the provenance the DURABLE record must carry: the
// hops as received, then this bus appended. A restart must not rewrite it to a
// single local hop — that would be a durable claim that the message originated
// here, indistinguishable afterwards from a genuine local send (invariant 6).
var relayCrashRecordedPath = []string{relayCrashOriginBus, relayCrashMiddleBus, testBusID}

// relayCrashRequest builds the arriving message. Called identically in the
// child and in the parent.
func relayCrashRequest(t *testing.T) hub.RelayedIngestRequest {
	t.Helper()
	origin, err := ids.MessageID(relayCrashOriginBus, relayCrashOriginSeq)
	if err != nil {
		t.Fatalf("ids.MessageID(%q, %d): %v", relayCrashOriginBus, relayCrashOriginSeq, err)
	}
	sender := agentID(t, relayCrashOriginBus, relayCrashOriginAgent)
	return hub.RelayedIngestRequest{
		Sender:            sender,
		Recipients:        []string{agentID(t, testBusID, relayCrashLocalAgent)},
		Body:              relayCrashBody,
		OriginMessageID:   origin,
		OriginAttestation: fixtureOriginAttestation(sender),
		// The path AS RECEIVED — NOT including this bus. IngestRelayed appends
		// our own hop.
		BusPath:            []string{relayCrashOriginBus, relayCrashMiddleBus},
		TimestampUnixMilli: fixtureTimestampMs,
		Signature:          fixtureSignature(),
	}
}

func relayCrashPeerBucketRequest(t *testing.T, originBus, agent string, seq uint64) hub.RelayedIngestRequest {
	t.Helper()
	origin, err := ids.MessageID(originBus, seq)
	if err != nil {
		t.Fatalf("ids.MessageID(%q, %d): %v", originBus, seq, err)
	}
	sender := agentID(t, originBus, agent)
	return hub.RelayedIngestRequest{
		Sender:             sender,
		Recipients:         []string{agentID(t, testBusID, relayCrashLocalAgent)},
		Body:               []byte("peer bucket crash fixture " + originBus + agent),
		OriginMessageID:    origin,
		OriginAttestation:  fixtureOriginAttestation(sender),
		BusPath:            []string{originBus, relayCrashMiddleBus},
		TimestampUnixMilli: fixtureTimestampMs,
		Signature:          fixtureSignature(),
	}
}

// ---------------------------------------------------------------------------
// The child
// ---------------------------------------------------------------------------

// TestRelayIngestCrashChild is the child half of both crash tests below. It
// does NOTHING in a normal run: without envRelayCrashPoint it skips
// immediately.
func TestRelayIngestCrashChild(t *testing.T) {
	point := os.Getenv(envRelayCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envRelayCrashPoint + " is unset")
	}
	dir := os.Getenv(envRelayCrashDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envRelayCrashPoint, point, envRelayCrashDir)
	}

	// closeOnCleanup is false throughout: a crash child must not have a deferred
	// Close registered, because a Close that ran would be exactly the graceful
	// shutdown these tests exist to rule out.
	lg := openTestLog(t, dir, false)
	req := relayCrashRequest(t)

	switch point {
	case relayCrashPostCommit:
		// killAfterCommit (idem_crash_test.go) delegates to the REAL
		// *wal.Log.Write — the whole prepare, commit and fsync cycle — and kills
		// the process before returning. IT ALSO ASSERTS, ITSELF, that the entry
		// publish handed the durable log carries a non-empty Entry.Idem: the
		// applied-key record must ride in the SAME two-phase transaction as the
		// effect, not in a second write ordered after it. That assertion is made
		// here, in the child, because this is the only place the entry can be
		// seen BEFORE it is written; the parent re-asserts it against the bytes
		// on disk.
		h := newHubOverDurable(t, &killAfterCommit{l: lg}, lg, testBusID, relayCrashLocalAgent)
		res, err := h.IngestRelayed(context.Background(), req)
		t.Fatalf("child: IngestRelayed returned (%+v, %v) but the durable log kills this process the instant the commit is fsynced; the crash was never injected", res, err)

	case relayCrashDanglingPrepare:
		// killAfterPrepare stops the two-phase write between its two fsyncs: the
		// PREPARE record is durable and complete, and no COMMIT is ever written.
		h := newHubOverDurable(t, &killAfterPrepare{l: lg}, lg, testBusID, relayCrashLocalAgent)
		res, err := h.IngestRelayed(context.Background(), req)
		t.Fatalf("child: IngestRelayed returned (%+v, %v) but the durable log kills this process the instant the prepare is fsynced; the crash was never injected", res, err)

	case relayCrashPeerBuckets:
		h := newHubOverDurable(t, &killAfterMessageCount{l: lg, remaining: 2}, lg, testBusID, relayCrashLocalAgent)
		for _, peerReq := range []hub.RelayedIngestRequest{
			relayCrashPeerBucketRequest(t, "BUSPEER", "first", 11),
			relayCrashPeerBucketRequest(t, "buspeer", "second", 12),
		} {
			if _, err := h.IngestRelayed(context.Background(), peerReq); err != nil {
				t.Fatalf("child: IngestRelayed before injected kill: %v", err)
			}
		}
		t.Fatal("child: both relayed ingests returned; second committed message did not SIGKILL")

	default:
		t.Fatalf("child: unknown crash point %q", point)
	}
}

// killAfterMessageCount delegates every write to the real WAL and SIGKILLs
// only after remaining committed message transactions. Sequence-floor writes
// pass through without consuming the count.
type killAfterMessageCount struct {
	l         *wal.Log
	remaining int
}

func (k *killAfterMessageCount) Write(e wal.Entry) (wal.Committed, error) {
	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	if passThroughSeqFloor(e) {
		return c, nil
	}
	if len(e.Idem) == 0 {
		return wal.Committed{}, errors.New("child: committed peer-bucket fixture has no applied-key record")
	}
	k.remaining--
	if k.remaining == 0 {
		killSelf()
	}
	return c, nil
}

// runRelayIngestCrashChild re-execs this test binary at the given crash point
// and asserts the child really DIED ON SIGKILL rather than failing its own
// assertions — without that check a broken child would silently turn the parent
// into a test of an empty directory.
//
// IT IS A DELIBERATE COPY of runIdemCrashChild, not a call to it. That runner
// hard-codes `-test.run=^TestIdemCrashChild$` and idem_crash_test.go's two
// environment variables, and this task's boundary forbids editing that file to
// parameterise it. Copying ~40 lines is the correct trade against reaching
// across the boundary; if the two ever need to diverge, they already can.
func runRelayIngestCrashChild(t *testing.T, point, dir string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}

	// Bounded, so a wedged child fails this test in a minute rather than hanging
	// the suite until the package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestRelayIngestCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), envRelayCrashPoint+"="+point, envRelayCrashDir+"="+dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("relay crash child %q did not finish in time: %v\n--- child output ---\n%s", point, ctx.Err(), out.String())
	}
	// A child that failed its OWN assertions also exits non-zero, so "err != nil"
	// is not the assertion. The wait status is.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("relay crash child %q: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s",
			point, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("relay crash child %q: wait status is %T, want syscall.WaitStatus", point, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("relay crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s",
			point, ws.ExitStatus(), out.String())
	}
}

// relayCrashSamePath reports whether a recorded bus path is the one the fixture
// must produce. Written here rather than reused so this file's assertions do
// not depend on a helper in a file it does not own.
func relayCrashSamePath(got, want []string) bool {
	return strings.Join(got, ">") == strings.Join(want, ">")
}

// ---------------------------------------------------------------------------
// Case 1 — killed AFTER the commit fsync, BEFORE the peer was acknowledged
// ---------------------------------------------------------------------------

// TestRelayIngestCrashPostCommitRetryReportsOutcomeRetry is the scenario the
// relay lives in, injected for real: the message is committed and fsynced, the
// process dies before IngestRelayed can return, the peer sees a dropped
// connection rather than a 200 — and retries.
//
// THE ASSERTION THAT MATTERS is idem.OutcomeRetry. relay.Acceptor re-forwards on
// idem.OutcomeNew ALONE, and OutcomeNew is idem.Outcome's zero value, so a
// duplicate answered "new" after a restart is not a wrong-looking field: it is
// the same message forwarded onto the mesh again, by a bus that has already
// forwarded it, every time the peer retries.
//
// It can only pass if the applied-key record for an idem.OpRelay operation was
// REBUILT FROM THE DURABLE LOG. A process killed with -9 flushes nothing, runs
// no deferred Close and hands nothing to a successor, so there is no in-memory
// cache for the second attempt to be answered from.
func TestRelayIngestCrashPostCommitRetryReportsOutcomeRetry(t *testing.T) {
	dir := t.TempDir()
	runRelayIngestCrashChild(t, relayCrashPostCommit, dir)

	req := relayCrashRequest(t)
	bob := agentID(t, testBusID, relayCrashLocalAgent)

	// (0) WHAT THE DYING PROCESS ACTUALLY LEFT ON STABLE STORAGE. Without this
	// the rest could pass just as happily against a directory where nothing was
	// ever written, and would prove nothing.
	all := replayCommitted(t, dir)
	// A relayed ingest costs TWO durable transactions on a fresh log exactly
	// like a minted send does: mintRelayedSeqLocked burns a batch of sequence
	// numbers (hub.SeqFloorRecordKind) BEFORE it hands one out, and only then
	// does the message's own transaction follow. A number handed out before it
	// is durably burned is a number a restart can hand out again (invariant 1),
	// and a SIGKILL is the only way to observe the ordering without taking the
	// process's word for it.
	if len(committedSeqFloors(all)) == 0 {
		t.Fatalf("the crashed log holds no sequence-floor record among its %d committed entries: the relayed ingest minted a local sequence without first durably burning it, so a restart could reissue that message id (invariant 1)", len(all))
	}
	if all[0].Entry.Kind != hub.SeqFloorRecordKind {
		t.Fatalf("the first committed entry is %q, want %q: the floor record must reach stable storage BEFORE the message whose sequence it burns", all[0].Entry.Kind, hub.SeqFloorRecordKind)
	}
	committed := committedMessages(all)
	if len(committed) != 1 {
		t.Fatalf("the crashed log holds %d committed MESSAGE entries, want exactly 1: the child died before the relayed message was durable, so there is no post-commit crash to recover from", len(committed))
	}
	// Re-asserted against the BYTES ON DISK. killAfterCommit already refuses to
	// write an entry with an empty Entry.Idem — see its body — but that check
	// lives in the child, in the process that died; this one is made by the
	// parent against what actually survived, which is the thing recovery reads.
	if len(committed[0].Entry.Idem) == 0 {
		t.Fatalf("the committed transaction carries NO applied-key record (Entry.Idem is empty): the record must commit in the SAME two-phase transaction as the effect, not in a second write ordered after it — a second write is a second answer to \"have I applied this?\", and two answers is how a duplicate becomes a second message")
	}
	rec, err := idem.DecodeRecord(committed[0].Entry.Idem)
	if err != nil {
		t.Fatalf("decoding the durable applied-key record: %v", err)
	}
	// The scope, on disk: (origin agent, OpRelay, origin message id). OpRelay is
	// DOMAIN SEPARATION rather than labelling — a relayed message must never
	// share a scope with, or be mistaken for a retry of, a local send under the
	// same key.
	if rec.Op != idem.OpRelay {
		t.Errorf("the durable applied-key record's op = %q, want %q: the relay path must record its own operation, or a relayed message and a local send collide in one scope", rec.Op, idem.OpRelay)
	}
	if rec.Agent != req.Sender {
		t.Errorf("the durable applied-key record's agent = %q, want the ORIGIN sender %q", rec.Agent, req.Sender)
	}
	if rec.Key != req.OriginMessageID {
		t.Errorf("the durable applied-key record's key = %q, want the ORIGIN message id %q: that id IS the idempotency key, which is what makes one message arriving by two routes land on ONE scope", rec.Key, req.OriginMessageID)
	}

	original, err := store.Decode(committed[0].Entry.Body)
	if err != nil {
		t.Fatalf("decoding the committed message: %v", err)
	}
	if original.ID == req.OriginMessageID {
		t.Fatalf("the durable record adopted the ORIGIN's id %q: this bus mints its own (invariant 1)", original.ID)
	}
	if !relayCrashSamePath(original.BusPath, relayCrashRecordedPath) {
		t.Fatalf("the committed record's bus path = %v, want %v (the hops as received, then this bus)", original.BusPath, relayCrashRecordedPath)
	}

	// --- RESTART on the SAME directory. ---
	lg := openTestLog(t, dir, true)
	h := newHubOver(t, lg, testBusID, relayCrashLocalAgent)

	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("after recovering from a SIGKILL the applied-key table holds %d records, want 1: the record for an %s operation must be REBUILT FROM THE DURABLE LOG, not held in memory — a process killed with -9 flushes nothing", got, idem.OpRelay)
	}
	count, head := storedMessages(t, h)
	if count != 1 || head != original.Seq {
		t.Fatalf("the recovered store holds %d messages with head %d, want 1 message at seq %d", count, head, original.Seq)
	}
	// THE PROVENANCE SURVIVED THE RESTART. The serving copy is rebuilt from the
	// log, so a recovery path that dropped or defaulted the path would show up
	// here as a single local hop — a message that crossed two buses, durably
	// claiming it was sent on this one.
	served, _, _ := h.Store().Since(bob, fixtureEnrolledAt, 0, 10)
	if len(served) != 1 {
		t.Fatalf("the recovered serving copy holds %d messages for %s, want 1", len(served), bob)
	}
	if !relayCrashSamePath(served[0].BusPath, relayCrashRecordedPath) {
		t.Fatalf("the RECOVERED message's bus path = %v, want %v: a restart must not rewrite a relayed message's provenance", served[0].BusPath, relayCrashRecordedPath)
	}
	if served[0].Sender != req.Sender {
		t.Errorf("the recovered message's sender = %q, want the ORIGIN's %q", served[0].Sender, req.Sender)
	}

	// (a) THE PEER RETRIES. Same origin message id, same body, same recipients —
	// a peer legitimately re-delivering after a lost ack, which is the NORMAL
	// steady state of a relay and must not be an error.
	again, err := h.IngestRelayed(context.Background(), relayCrashRequest(t))
	if err != nil {
		t.Fatalf("the peer's retry after the crash returned err = %v, want the ORIGINAL result and no error: the ack was lost in flight, not refused (invariant 10's legitimate-retry carve-out)", err)
	}
	if again.Outcome != idem.OutcomeRetry {
		t.Fatalf("the retry after the crash reported Outcome = %s, want %s. %s is idem.Outcome's ZERO VALUE and is the answer relay.Acceptor RE-FORWARDS on, so a duplicate reported as new is an amplification loop across the bus path — and this can only be answered correctly from WAL-recovered state",
			again.Outcome, idem.OutcomeRetry, idem.OutcomeNew)
	}
	if again.MessageID != original.ID || again.Seq != original.Seq {
		t.Errorf("the retry returned message %s / seq %d, want the ORIGINAL %s / seq %d replayed verbatim",
			again.MessageID, again.Seq, original.ID, original.Seq)
	}

	// (b) EXACTLY ONE EFFECT. The serving copy, the sequence head and the
	// durable log all have to agree that the retry produced nothing.
	count, head = storedMessages(t, h)
	if count != 1 || head != original.Seq {
		t.Fatalf("after the retry the store holds %d messages with head %d, want 1 message still at seq %d: the peer's retry was RE-APPLIED, which is the double-apply invariant 10 forbids", count, head, original.Seq)
	}
	after := committedMessages(replayCommitted(t, dir))
	if len(after) != 1 {
		t.Fatalf("after the retry the durable log holds %d committed MESSAGE entries, want 1: the retry wrote a SECOND effect to disk", len(after))
	}
	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("after the retry the applied-key table holds %d records, want 1", got)
	}
	// And the record on disk is still the one the child wrote, path intact: a
	// retry replays an answer, it does not rewrite history.
	replayedAgain, err := store.Decode(after[0].Entry.Body)
	if err != nil {
		t.Fatalf("decoding the message record after the retry: %v", err)
	}
	if replayedAgain.ID != original.ID || !relayCrashSamePath(replayedAgain.BusPath, relayCrashRecordedPath) {
		t.Fatalf("after the retry the durable record is %s with path %v, want the original %s with path %v",
			replayedAgain.ID, replayedAgain.BusPath, original.ID, relayCrashRecordedPath)
	}
}

// TestRelayIngestCrashRecoversCanonicalPeerBucket proves the peer-bucket
// denominator is rebuilt from real WAL bytes after an uncatchable process kill.
func TestRelayIngestCrashRecoversCanonicalPeerBucket(t *testing.T) {
	dir := t.TempDir()
	runRelayIngestCrashChild(t, relayCrashPeerBuckets, dir)

	lg := openTestLog(t, dir, true)
	h := newHubOverDurable(t, lg, lg, testBusID, relayCrashLocalAgent)
	st := h.IdempotencyStats()
	if st.Count != 2 || st.Agents != 1 {
		t.Fatalf("after SIGKILL recovery Stats=%+v, want two durable relayed keys charged to one case-folded peer-bus denominator bucket", st)
	}
}

// ---------------------------------------------------------------------------
// Case 2 — killed BETWEEN the prepare fsync and the commit fsync
// ---------------------------------------------------------------------------

// TestRelayIngestCrashDanglingPrepareLeavesTheRelayKeyUnseen is invariant 5's
// prefix-consistency case on the relay path. The applied-key record reached
// stable storage inside a PREPARE that never committed, so the effect is not
// part of accepted history — and neither is the key.
//
// If a key whose effect never committed came back as "already applied", the
// peer's REAL first delivery of that message would be answered with
// idem.OutcomeRetry naming a message this bus never wrote and never delivered:
// the message would be silently swallowed, the local recipient would never see
// it, and — because relay forwards on idem.OutcomeNew alone — it would not
// travel onward either.
func TestRelayIngestCrashDanglingPrepareLeavesTheRelayKeyUnseen(t *testing.T) {
	dir := t.TempDir()
	runRelayIngestCrashChild(t, relayCrashDanglingPrepare, dir)

	bob := agentID(t, testBusID, relayCrashLocalAgent)

	// (0) NO MESSAGE COMMITTED — the crash really did land between the two
	// fsyncs of the message's own transaction.
	//
	// The sequence-floor record IS committed, and must be: it was written and
	// fsynced one step earlier, before the number was allocated. That is the
	// correct outcome and not a leak — the number stays burned, the peer
	// re-delivers, this bus mints a FRESH number, and the gap is what
	// internal/ids/sequence.go documents as correct. A reissue would be the
	// defect; a gap is the safe direction (invariant 1).
	all := replayCommitted(t, dir)
	if got := committedMessages(all); len(got) != 0 {
		t.Fatalf("the crashed log holds %d committed MESSAGE entries, want 0: the child was supposed to die with its prepare unresolved", len(got))
	}
	if len(committedSeqFloors(all)) == 0 {
		t.Fatalf("the crashed log holds no sequence-floor record: the relayed ingest allocated a local sequence without first durably burning it (invariant 1)")
	}

	lg := openTestLog(t, dir, true)
	recovered := lg.Recovered()
	if len(recovered.Dangling) == 0 {
		t.Fatalf("Recovered().Dangling = %v, want a discarded prepare: without one there is no unresolved transaction and this test proves nothing", recovered.Dangling)
	}
	// The ONLY transactions applied are the floor records. Expressed as a count
	// of those rather than as a literal, so it still fails the moment the
	// dangling prepare is applied — a literal that was simply bumped would pass
	// just as happily if the prepare were the thing recovered and the floor
	// record the thing dropped.
	if want := uint64(len(committedSeqFloors(all))); recovered.Applied != want {
		t.Fatalf("Recovered().Applied = %d, want %d (the sequence-floor record(s) and nothing else): an uncommitted prepare must not be applied", recovered.Applied, want)
	}

	h := newHubOver(t, lg, testBusID, relayCrashLocalAgent)
	if st := h.IdempotencyStats(); st.Count != 0 {
		t.Fatalf("the applied-key table holds %d records after recovering a DANGLING prepare, want 0: the key's effect never committed, so it is not part of accepted history and must behave as NEVER SEEN (invariant 5)", st.Count)
	}
	if count, _ := storedMessages(t, h); count != 0 {
		t.Fatalf("the recovered store holds %d messages, want 0: an uncommitted prepare is not history", count)
	}

	// THE PEER'S REAL FIRST DELIVERY. It must be applied as new.
	res, err := h.IngestRelayed(context.Background(), relayCrashRequest(t))
	if err != nil {
		t.Fatalf("the first arrival after a dangling prepare returned err = %v, want success: nothing was ever committed under this key", err)
	}
	if res.Outcome != idem.OutcomeNew {
		t.Fatalf("the first arrival after a dangling prepare reported Outcome = %s with message %s, want %s: a key whose effect never committed was remembered as applied, so the peer's genuine first delivery was SILENTLY SWALLOWED — answered with a message nobody ever received, and not forwarded onward either",
			res.Outcome, res.MessageID, idem.OutcomeNew)
	}
	if res.MessageID == "" || res.Seq == 0 {
		t.Fatalf("the first arrival returned message %q / seq %d, want a freshly minted local assignment", res.MessageID, res.Seq)
	}

	// EXACTLY ONE MESSAGE EXISTS AFTERWARDS: the store, the durable log and the
	// serving copy agree, and the uncommitted prepare contributed nothing.
	count, head := storedMessages(t, h)
	if count != 1 || head != res.Seq {
		t.Fatalf("after the first arrival the store holds %d messages with head %d, want exactly 1 at seq %d", count, head, res.Seq)
	}
	written := committedMessages(replayCommitted(t, dir))
	if len(written) != 1 {
		t.Fatalf("the durable log holds %d committed MESSAGE entries after the first arrival, want exactly 1", len(written))
	}
	m, err := store.Decode(written[0].Entry.Body)
	if err != nil {
		t.Fatalf("decoding the message written after recovery: %v", err)
	}
	if m.ID != res.MessageID {
		t.Errorf("the durable record names message %q, IngestRelayed returned %q", m.ID, res.MessageID)
	}
	// The provenance is recorded in full on the message written AFTER recovery
	// too: a bus that recovered from a torn write must not record the next
	// relayed message as a local one.
	if !relayCrashSamePath(m.BusPath, relayCrashRecordedPath) {
		t.Fatalf("the durable record's bus path = %v, want %v: the hops as received, then this bus", m.BusPath, relayCrashRecordedPath)
	}
	served, _, _ := h.Store().Since(bob, fixtureEnrolledAt, 0, 10)
	if len(served) != 1 {
		t.Fatalf("the serving copy holds %d messages for %s, want 1", len(served), bob)
	}
	if st := h.IdempotencyStats(); st.Count != 1 {
		t.Fatalf("the applied-key table holds %d records after the first arrival succeeded, want 1", st.Count)
	}

	// And from here on it is an ordinary applied key: the peer's next retry
	// replays, it does not write a second message.
	retry, err := h.IngestRelayed(context.Background(), relayCrashRequest(t))
	if err != nil {
		t.Fatalf("retrying the newly-applied relay key returned err = %v, want the original result replayed", err)
	}
	if retry.Outcome != idem.OutcomeRetry || retry.MessageID != res.MessageID || retry.Seq != res.Seq {
		t.Fatalf("retrying the newly-applied relay key returned %+v, want %s / seq %d replayed as %s", retry, res.MessageID, res.Seq, idem.OutcomeRetry)
	}
	if got := committedMessages(replayCommitted(t, dir)); len(got) != 1 {
		t.Fatalf("the durable log holds %d committed MESSAGE entries after the retry, want 1", len(got))
	}
}
