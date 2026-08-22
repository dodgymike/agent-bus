// These tests cover IDEM-11's wiring of internal/idem into the hub, through
// the EXPORTED surface only — the same posture hub_test.go takes.
package hub_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// TestParkedPollMaxMatchesHub is the anti-drift check retention.go promises.
//
// internal/idem is a LEAF package: it has no internal dependencies, and
// importing internal/hub — which imports it — would cycle. So it RESTATES
// hub.MaxPollTimeout as idem.ParkedPollMax, a term in the derivation of the
// retention window. This test lives here, in the package that may import both,
// and fails the moment either value moves without the other. Without it, the
// restatement would be a comment claiming a relationship nothing enforces.
func TestParkedPollMaxMatchesHub(t *testing.T) {
	if idem.ParkedPollMax != hub.MaxPollTimeout {
		t.Fatalf("idem.ParkedPollMax = %v but hub.MaxPollTimeout = %v; the retention window is DERIVED from the parked-poll ceiling (internal/idem/retention.go), so the two must not drift",
			idem.ParkedPollMax, hub.MaxPollTimeout)
	}
}

// TestMaxIdempotencyEntriesMatchesIdem pins the number CONTRACTS-HTTP.md
// documents (65536) to its single definition. hub.MaxIdempotencyEntries is the
// name the HTTP contract uses; idem.MaxEntries is where it is derived. They are
// the same constant, and this fails if that ever stops being true.
func TestMaxIdempotencyEntriesMatchesIdem(t *testing.T) {
	if hub.MaxIdempotencyEntries != idem.MaxEntries {
		t.Fatalf("hub.MaxIdempotencyEntries = %d but idem.MaxEntries = %d", hub.MaxIdempotencyEntries, idem.MaxEntries)
	}
	if hub.MaxIdempotencyEntries != 65536 {
		t.Fatalf("hub.MaxIdempotencyEntries = %d, want 65536 (the value CONTRACTS-HTTP.md documents)", hub.MaxIdempotencyEntries)
	}
}

// TestAppliedKeyStoreSurvivesRestart is the durability claim IDEM-11 exists to
// make: the applied-key table is RECOVERED STATE, not an in-memory cache. A
// client that retries a send across a restart must get its ORIGINAL result
// back, not a second message.
//
// It also proves the applied-key record travelled in the SAME two-phase
// transaction as the message: the second hub is rebuilt purely by replaying the
// WAL, so a key it knows about is a key that was on disk beside its effect.
func TestAppliedKeyStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, false)
	h := newHubOver(t, lg, testBusID, "alpha", "beta")
	a := agentID(t, testBusID, "alpha")
	b := agentID(t, testBusID, "beta")

	first, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: []byte("hello"), IdempotencyKey: "k-restart"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	bc, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("all hands"), IdempotencyKey: "k-restart-bc"})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if got := h.IdempotencyStats().Count; got != 2 {
		t.Fatalf("IdempotencyStats().Count = %d before the restart, want 2", got)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// --- RESTART: a fresh log and a fresh hub over the same directory. ---
	lg2 := openTestLog(t, dir, true)
	h2 := newHubOver(t, lg2, testBusID, "alpha", "beta")
	st := h2.IdempotencyStats()
	if st.Count != 2 {
		t.Fatalf("after the restart IdempotencyStats().Count = %d, want 2 — the applied-key table must be RECOVERED state, not an in-memory cache", st.Count)
	}
	if st.Window != idem.RetentionWindow || st.MaxEntries != hub.MaxIdempotencyEntries {
		t.Fatalf("recovered bounds = %v / %d, want %v / %d", st.Window, st.MaxEntries, idem.RetentionWindow, hub.MaxIdempotencyEntries)
	}

	again, err := mintedSend(t, h2, hub.SendRequest{Sender: a, To: b, Body: []byte("hello"), IdempotencyKey: "k-restart"})
	if err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if !again.Replayed || again.MessageID != first.MessageID || again.Seq != first.Seq {
		t.Fatalf("retry after restart returned %+v, want the original %+v with Replayed set", again, first)
	}
	// Sender and Broadcast are NOT stored in the result — they are rebuilt from
	// the record's own scope (Agent and Op). This is what proves that.
	if again.Broadcast || again.Sender != a {
		t.Fatalf("the replayed result lost its scope-derived fields: %+v", again)
	}
	bcAgain, err := mintedBroadcast(t, h2, hub.BroadcastRequest{Sender: a, Body: []byte("all hands"), IdempotencyKey: "k-restart-bc"})
	if err != nil {
		t.Fatalf("broadcast retry after restart: %v", err)
	}
	if !bcAgain.Replayed || bcAgain.MessageID != bc.MessageID || !bcAgain.Broadcast {
		t.Fatalf("broadcast retry after restart returned %+v, want the original %+v", bcAgain, bc)
	}

	// A key reused for DIFFERENT content across the restart is still a
	// violation, so the fingerprint survived the round trip too.
	if _, err := mintedSend(t, h2, hub.SendRequest{Sender: a, To: b, Body: []byte("different"), IdempotencyKey: "k-restart"}); !errors.Is(err, hub.ErrIdempotencyKeyReused) {
		t.Fatalf("a key reused for different content after a restart gave err = %v, want ErrIdempotencyKeyReused", err)
	}
}

// TestAppliedKeyRecoveryFromPreIdemLog is the BACK-COMPAT path, and it is
// mandatory rather than nice-to-have: a WAL written before IDEM-11 carries no
// applied-key record in its prepare payload, only the message's own idempotency
// key. Without the reconstruction path, every applied key in an existing
// on-disk log would be lost on the first restart after the change — a
// durability REGRESSION delivered by a durability improvement, exactly once, at
// the upgrade.
func TestAppliedKeyRecoveryFromPreIdemLog(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, false)
	a := agentID(t, testBusID, "alpha")
	b := agentID(t, testBusID, "beta")
	sentAt := time.Now().UTC().Add(-time.Minute)

	// Written the PRE-IDEM-11 way: wal.Entry with no Idem field at all.
	m, err := store.NewMessage(testBusID, a, false, []string{b}, 1, sentAt, []byte("legacy"), "k-legacy", fixtureTimestampMs, fixtureSignature())
	if err != nil {
		t.Fatalf("store.NewMessage: %v", err)
	}
	payload, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := lg.Write(wal.Entry{Kind: store.RecordKind, Body: payload}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lg2 := openTestLog(t, dir, true)
	h := newHubOver(t, lg2, testBusID, "alpha", "beta")
	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("IdempotencyStats().Count = %d after replaying a pre-IDEM-11 log, want 1 — the applied key must be rebuilt from the message record", got)
	}
	again, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: []byte("legacy"), IdempotencyKey: "k-legacy"})
	if err != nil {
		t.Fatalf("retry of a pre-IDEM-11 key: %v", err)
	}
	if !again.Replayed || again.MessageID != m.ID {
		t.Fatalf("retry of a pre-IDEM-11 key returned %+v, want the original %s with Replayed set", again, m.ID)
	}
	// And the RECOMPUTED fingerprint must be the same one publish computes, or
	// a legitimate retry would look like a key-reuse violation.
	if _, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: []byte("changed"), IdempotencyKey: "k-legacy"}); !errors.Is(err, hub.ErrIdempotencyKeyReused) {
		t.Fatalf("a pre-IDEM-11 key reused for different content gave err = %v, want ErrIdempotencyKeyReused", err)
	}
}

// ---------------------------------------------------------------------------
// IDEM-12 — an idempotent send. THE HAPPY PATH ONLY.
//
// Same key + SAME payload is a LEGITIMATE RETRY: the ack was probably lost in
// flight. The bus must return the ORIGINAL message id and sequence verbatim,
// allocate NO new sequence (invariant 1 — sequences are server-minted and are
// never duplicated for one logical operation), write NO second record to the
// append-only audit log (invariant 6 — a retry must not create a phantom second
// entry for what is, from the trail's point of view, ONE logical send), return
// NO error and disconnect NOBODY (invariant 10 — punishing a well-behaved
// retrying client breaks exactly the client doing the right thing).
//
// THE CARVE-OUT THAT MUST NOT BE COLLAPSED: only same key + DIFFERENT payload is
// a violation, and that path is IDEM-14's. Nothing here asserts anything about
// it, deliberately.
//
// # What is NOT here, and why
//
// BROADCAST. /v1/broadcast answers 501 today and hub.Broadcast fails closed,
// because signing format v1 has not defined a canonical broadcast audience, so a
// broadcast test in this package routes through
// skipIfBroadcastHasNoSigningDigest and SKIPS. A suite whose leaves all skip
// proves nothing — scripts/proof-check.sh judges it VACUOUS — so a broadcast
// retry test here would actively poison this task's proof rather than extend it.
// It is filed as a follow-up gated on SIGN-3; see that helper for the standing
// operator ruling on why these are skipped and not rewritten to assert the
// refusal.
//
// SIGN-6's interaction — a message refused for a missing or invalid signature
// was NEVER applied, so its key must not be burned — is proved end to end by
// internal/httpapi.TestSignRejectionIsTerminalForItsIdempotencyKey, where the
// signature check actually lives. It is not re-asserted here.
//
// The crash/restart shape of a retry is proved by
// TestIdemCrashPostCommitRetryReplaysTheOriginal, and the HTTP end-to-end shape
// by internal/httpapi.TestClientSendRetryIsOneMessageEndToEnd.
// ---------------------------------------------------------------------------

// auditRecordCount reads the append-only audit log off DISK and counts what is
// in it, through the same strict decoder an fsck would use.
//
// It reads the LIVE file without closing the log, and that is safe rather than
// lucky: wal.Writer.Append is a single WriteAt followed by an fsync, with no
// buffering, and invariant 4 means nothing is acknowledged before that fsync
// returns. So by the time a Send has returned to this goroutine every audit
// record it caused is whole on disk, and no partial frame can be observed. That
// is what allows the count to be taken BETWEEN two sends in one hub's lifetime,
// which is exactly what "the retry added no second record" needs and what a
// close-then-scan helper cannot do.
func auditRecordCount(t *testing.T, lg *wal.Log) int {
	t.Helper()
	path := lg.AuditPath()
	if path == "" {
		t.Fatal("the durable log has no audit log open; invariant 6 says every message is written to the trail, so a count of it can never be 'not applicable'")
	}
	recs, _, err := wal.ScanAll(path, wal.KindAudit)
	if err != nil {
		t.Fatalf("scanning the append-only audit log %s: %v", path, err)
	}
	return len(recs)
}

// durableMessageCount replays the WAL and counts COMMITTED message records.
//
// It asks the FILE, not the hub: "the serving copy holds one message" and "one
// message was written" are different claims, and a retry that re-wrote the
// durable record while the serving copy deduplicated it would satisfy the first
// and violate the second. wal.Replay opens the path read-only and creates
// nothing, so it is safe to run beside the live log.
func durableMessageCount(t *testing.T, lg *wal.Log) int {
	t.Helper()
	n := 0
	if _, err := wal.Replay(lg.Path(), func(c wal.Committed) error {
		if c.Entry.Kind == store.RecordKind {
			n++
		}
		return nil
	}); err != nil {
		t.Fatalf("replaying %s to count durable message records: %v", lg.Path(), err)
	}
	return n
}

// deliveredCount is how many messages the recipient can actually read. It is the
// third independent witness — durable record, audit record, delivered message —
// because a double-apply can show up in any one of the three alone.
func deliveredCount(t *testing.T, h *hub.Hub, agent string) int {
	t.Helper()
	batch, err := h.History(agent, 0, 100)
	if err != nil {
		t.Fatalf("History(%s): %v", agent, err)
	}
	return len(batch.Messages)
}

// sameRecipients compares two recipient lists element by element.
// reflect.DeepEqual would also do it, but it calls a nil slice and an empty one
// different, which is not a distinction any assertion here is making. (The
// identically-shaped equalStrings in buspath_test.go is unreachable from here:
// that file is in package hub, this one is in package hub_test.)
func sameRecipients(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIdempotentSend(t *testing.T) {
	t.Run("RetryReplaysTheOriginalAndAddsNoSecondAuditRecord", testIdempotentSendRetryReplaysOriginal)
	t.Run("ConcurrentInFlightRetryIsSingleFlighted", testIdempotentSendConcurrentInFlight)
}

// testIdempotentSendRetryReplaysOriginal is the sequential retry: the ack was
// lost, so the client re-presents THE SAME REQUEST — same key, same body, and
// the same reservation it has already signed.
//
// Re-presenting the SAME SignedMint is what a client that never saw the ack
// actually does, and it is the only shape that can prove "allocate NO new
// sequence": a retry that re-minted would burn a number inside Hub.Mint before
// publish ever saw it, and the sequence this test watches would move for a
// reason that has nothing to do with the send path.
//
// Almost every check is t.Errorf rather than t.Fatalf. The result value stays
// safe to read after a failure (it is the zero Result at worst), and a run that
// reports EVERY property that broke is worth far more than one that stops at the
// first — particularly here, where a single defect breaks the id, the trail and
// the delivery at once.
func testIdempotentSendRetryReplaysOriginal(t *testing.T) {
	h, lg, _ := newTestHub(t, "alpha", "beta")
	a := agentID(t, testBusID, "alpha")
	b := agentID(t, testBusID, "beta")
	body := []byte("the acknowledgement was lost in flight, so this was sent twice")
	const key = "k-idem12-retry"

	minted := mintFor(t, h, a, "send", key)
	if minted.MessageID == "" {
		t.Fatal("Mint returned no reservation; there is nothing to retry and every assertion below would be vacuous")
	}
	// ONE request value, spent twice, so the two calls cannot drift apart in any
	// field. A builder that rebuilt the second request could differ by a byte and
	// this would silently become IDEM-14's test instead of IDEM-12's.
	req := hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: key, SignedMint: minted}

	first, err := h.Send(req)
	if err != nil {
		t.Fatalf("the first send: %v", err)
	}
	if first.Replayed {
		t.Error("the FIRST send reports replayed; the flag exists to tell a replayed ack from a fresh one, and a fresh one that claims to be replayed makes it useless for exactly that")
	}
	if n := auditRecordCount(t, lg); n != 1 {
		t.Errorf("the audit log holds %d records after ONE send, want exactly 1 (invariant 6: every message is written to the trail, and only messages are)", n)
	}
	if n := durableMessageCount(t, lg); n != 1 {
		t.Errorf("the WAL holds %d committed message records after ONE send, want exactly 1", n)
	}

	// --- THE RETRY: byte-identical request, same reservation ---------------
	retry, err := h.Send(req)
	if err != nil {
		t.Errorf("the retry under the same key was REFUSED: %v; same key + same payload is a legitimate retry and must not be punished (invariant 10)", err)
	}
	if !retry.Replayed {
		t.Error("the retry does not report replayed; a caller must be able to tell a replayed ack from a fresh one, and the bus must not have re-applied it")
	}
	if retry.MessageID != first.MessageID || retry.Seq != first.Seq {
		t.Errorf("the retry returned %q/%d, want the ORIGINAL %q/%d verbatim — a different id or sequence is a SECOND message, not a retry",
			retry.MessageID, retry.Seq, first.MessageID, first.Seq)
	}
	// Replayed is the ONLY field allowed to differ from the original ack.
	if retry.Sender != first.Sender || retry.Broadcast != first.Broadcast || !sameRecipients(retry.Recipients, first.Recipients) || !retry.SentAt.Equal(first.SentAt) {
		t.Errorf("the replayed ack differs from the original outside the Replayed flag:\n  got  %+v\n  want %+v", retry, first)
	}

	// --- NO SECOND RECORD, on any of the three witnesses -------------------
	if n := auditRecordCount(t, lg); n != 1 {
		t.Errorf("the append-only audit log holds %d records after one send and one retry, want exactly 1: a retry must not create a phantom second entry for what is ONE logical send (invariant 6)", n)
	}
	if n := durableMessageCount(t, lg); n != 1 {
		t.Errorf("the WAL holds %d committed message records after one send and one retry, want exactly 1", n)
	}
	if n := deliveredCount(t, h, b); n != 1 {
		t.Errorf("the recipient can read %d messages after one send and one retry, want exactly 1", n)
	}
	// Asked HERE, above the sequence probe, and not at the end of the function:
	// a poisoned hub refuses to mint, so the probe below would abort the test
	// before this check ran and the ONE fact that explains every failure above
	// would go unreported.
	if err := h.Poisoned(); err != nil {
		t.Errorf("the hub is poisoned after a legitimate retry: %v", err)
	}

	// --- NO NEW SEQUENCE ---------------------------------------------------
	//
	// Asked of the bus's own id authority rather than of the retry's answer: an
	// echo of the original sequence looks identical whether or not a number was
	// ALSO burned behind it. The next reservation this bus issues is the only
	// thing that can tell the two apart.
	probe := mintFor(t, h, a, "send", "k-idem12-retry-probe")
	if probe.MessageID == "" {
		t.Fatal("the probe mint was refused, so the sequence assertion below cannot be made")
	}
	if probe.Seq != first.Seq+1 {
		t.Errorf("the next sequence this bus mints is %d and the send took %d, so %d sequence(s) were consumed in between; a retry must allocate NONE (invariant 1)",
			probe.Seq, first.Seq, probe.Seq-first.Seq-1)
	}
}

// testIdempotentSendConcurrentInFlight is the case implementations usually
// break: the client retried BEFORE the first ack landed, so both requests are in
// flight at once and there is no stored result yet when the second one arrives.
// A naive check-then-act double-applies here.
//
// The design answer (IDEM-12 item (a)) is a single flight taken in the SAME
// critical section that consumes the reservation: hub.publish holds writeMu
// across BOTH the applied-key lookup and the durable write, so the second caller
// BLOCKS and then reads the first's result out of the table rather than
// receiving a retriable "in progress". This test pins that choice — both callers
// return, both return the SAME assignment, and exactly one reports it replayed.
//
// Both goroutines spend the SAME SignedMint, because that is what a real client
// retry does: it holds one signed assignment and has seen no ack. It also
// sharpens the property. The send that commits DELETES the reservation, so the
// loser can only be answered out of the applied-key table — answered from the
// mint table, or re-applied, it would fail with ErrUnknownMint instead.
func testIdempotentSendConcurrentInFlight(t *testing.T) {
	h, lg, _ := newTestHub(t, "alpha", "beta")
	a := agentID(t, testBusID, "alpha")
	b := agentID(t, testBusID, "beta")
	body := []byte("two requests, one logical send")
	const key = "k-idem12-inflight"

	minted := mintFor(t, h, a, "send", key)
	if minted.MessageID == "" {
		t.Fatal("Mint returned no reservation; there is nothing for the two callers to spend")
	}
	req := hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: key, SignedMint: minted}

	const callers = 2
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results [callers]hub.Result
		errs    [callers]error
	)
	for i := 0; i < callers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The barrier is what makes this a RACE rather than two sequential
			// calls dressed up as one: both goroutines are parked and released
			// together, so whichever reaches writeMu first genuinely arrives with
			// the other already running.
			<-start
			results[i], errs[i] = h.Send(req)
		}()
	}
	close(start)
	wg.Wait()

	replayed := 0
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d was REFUSED: %v; both concurrent requests carry the same key and the same payload, so both must be answered (invariant 10)", i, err)
			continue
		}
		got := results[i]
		if got.MessageID != minted.MessageID || got.Seq != minted.Seq {
			t.Errorf("caller %d got %q/%d, want the single minted assignment %q/%d — two concurrent requests under one key must not resolve to two messages",
				i, got.MessageID, got.Seq, minted.MessageID, minted.Seq)
		}
		if got.Replayed {
			replayed++
		}
	}
	// EXACTLY ONE, in both directions. Zero means the loser was applied a second
	// time (or the flag was dropped); two means neither caller did the work and
	// an ack nobody earned is being echoed.
	if replayed != 1 {
		t.Errorf("%d of %d concurrent callers reported a replayed ack, want exactly 1: one request does the work, the other is answered from the applied-key table", replayed, callers)
	}

	if n := auditRecordCount(t, lg); n != 1 {
		t.Errorf("the append-only audit log holds %d records after two concurrent same-key sends, want exactly 1 (invariant 6)", n)
	}
	if n := durableMessageCount(t, lg); n != 1 {
		t.Errorf("the WAL holds %d committed message records after two concurrent same-key sends, want exactly 1", n)
	}
	if n := deliveredCount(t, h, b); n != 1 {
		t.Errorf("the recipient can read %d messages after two concurrent same-key sends, want exactly 1", n)
	}
	if err := h.Poisoned(); err != nil {
		t.Errorf("the hub is poisoned after two concurrent same-key sends: %v", err)
	}
}
