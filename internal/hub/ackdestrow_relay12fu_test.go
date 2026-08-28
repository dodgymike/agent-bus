package hub_test

// ACK-12-FU-DESTINATION-ROW — a relayed message gets a DESTINATION lifecycle row
// per recipient at INGEST, keyed (origin message id, recipient) and charged to
// the origin sender, and that row — not the retained message body — is the
// primary authorisation for a transit ack.
//
// hub.recordAcceptance no longer early-returns for `relayed` (it still does for
// `broadcast`, a deliberate non-goal). hub.AcknowledgeDelivery discriminates on
// the correlation key's bus half BEFORE Settle: a RELAYED key (bus half != this
// bus) goes to transitAck and returns Transit:true, never settling locally.
// transitAck authorises off the destination row first (settler.Lookup), with
// store.RelayProvenanceByOriginMessageID as a fallback — so the ack window is
// bounded by ack.Retention (24h), independent of message-body pruning.
//
// What this file proves:
//
//   - TestRelayedIngestWritesDestinationRow          — the row exists, one per
//     recipient, keyed and charged correctly, left `accepted`, and DURABLE.
//   - TestTransitAckResolvesAfterMessageBodyPruned    — the ack still resolves as
//     transit after the message BODY is pruned (the acceptance property): only
//     the destination row can carry it, since the message-provenance fallback is
//     dead once the body is gone.
//   - TestDestinationRowSurvivesRestart               — the row and its
//     authorisation survive a restart (rebuilt from the WAL alone), and still
//     authorise a transit ack after the body is pruned post-restart.
//   - TestDuplicateRelayedIngestOpensNoSecondRow      — a duplicate relayed ingest
//     opens NO second row and disconnects nobody (invariant 10); a same-key
//     different-payload ingest is refused and opens no row either.
//   - TestBroadcastGetsNoDestinationRow               — the documented non-goal:
//     a broadcast gets no row.
//
// Fixtures come from ack_boundary_test.go (newAckBoundaryHub, ackBoundaryOptions,
// deliveredAck, newTestClock), acktransit_test.go (atRelayedTo),
// relayingest_relay24blocker_test.go (riIngest, riOriginMessageID, riOriginBus),
// ack_seam_test.go (ackEntriesIn) and hub_test.go (agentID, testBusID, enrolAll,
// openTestLog). Nothing is re-invented.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// TestRelayedIngestWritesDestinationRow is the shape ACK-12-FU-DESTINATION-ROW
// adds: one accepted lifecycle row per recipient, keyed on the ORIGIN message id
// and charged to the ORIGIN sender, and DURABLE rather than only in the serving
// copy.
func TestRelayedIngestWritesDestinationRow(t *testing.T) {
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "bob", "carol")
	bob := agentID(t, testBusID, "bob")
	carol := agentID(t, testBusID, "carol")
	key := riOriginMessageID(t, 311)

	if _, err := h.IngestRelayed(context.Background(), riIngest(t, key, bob, carol)); err != nil {
		t.Fatalf("IngestRelayed: %v", err)
	}

	// ONE ROW PER RECIPIENT.
	if n := acks.Len(); n != 2 {
		t.Fatalf("a relayed ingest to two recipients opened %d rows, want 2 (one per recipient)", n)
	}

	// The sender the ROW is charged to is the origin agent, fully qualified
	// (invariant 2). agentID is deterministic (suffix 1), so this matches what
	// riIngest stamped.
	wantSender := agentID(t, riOriginBus, "alpha")
	for _, rcpt := range []string{bob, carol} {
		r, ok := acks.Lookup(key, rcpt)
		if !ok {
			t.Fatalf("no destination row for recipient %s under origin key %q", rcpt, key)
		}
		if r.State != ack.StateAccepted {
			t.Errorf("the row for %s is %s, want accepted; a relayed ingest leaves it non-terminal", rcpt, r.State)
		}
		if r.CorrelationKey != key {
			t.Errorf("the row's correlation key is %q, want the ORIGIN message id %q (invariant 1 — never re-minted here)", r.CorrelationKey, key)
		}
		if r.Sender != wantSender {
			t.Errorf("the row's sender is %q, want the origin agent %q (invariant 2)", r.Sender, wantSender)
		}
		if r.Recipient != rcpt {
			t.Errorf("the row's recipient is %q, want %q", r.Recipient, rcpt)
		}
		if r.Class != "" || r.AttestedBy != "" || !r.SettledAt.IsZero() {
			t.Errorf("an accepted row carries class/attestation/settled_at, which are set IFF terminal: %+v", r)
		}
	}

	// DURABLE, not just in memory: two lifecycle records on the log.
	if got := len(ackEntriesIn(t, dir)); got != 2 {
		t.Fatalf("the durable log holds %d lifecycle records, want 2 (one per recipient)", got)
	}
}

// TestTransitAckResolvesAfterMessageBodyPruned is the ACCEPTANCE test: a transit
// ack resolves AFTER the relayed message body has been pruned, which only the
// destination row can make true. The message-provenance FALLBACK is dead once
// the body is gone, so a green result here is the row doing its job.
//
// RED-BEFORE was demonstrated by stashing the two production files and re-running
// this test: without the change the ack answered Transit:false / ErrAckNotRetained
// (see the task's kind=report note for the verbatim output). GREEN-after is this
// test passing against the shipped change.
func TestTransitAckResolvesAfterMessageBodyPruned(t *testing.T) {
	clock := newTestClock()
	const msgMaxAge = time.Minute
	// A SMALL message MaxAge against a 24h ack retention, so the body ages out
	// while the row is comfortably live. That gap is the whole fixture.
	h, acks, _ := newAckBoundaryHub(t, ackBoundaryOptions{clock: clock, msgMaxAge: msgMaxAge, ackRetention: 24 * time.Hour}, "bob")
	bob := agentID(t, testBusID, "bob")
	key := atRelayedTo(t, h, 301, bob)

	// The destination row exists and is accepted (written at ingest).
	if r, ok := acks.Lookup(key, bob); !ok || r.State != ack.StateAccepted {
		t.Fatalf("no accepted destination row after relayed ingest: (%+v, %v)", r, ok)
	}
	// The message body is present, so the prune below is a REAL transition and not
	// a fixture that never had the body.
	if _, ok := h.Store().RelayProvenanceByOriginMessageID(key); !ok {
		t.Fatalf("the relayed message body is not present before the prune; the fixture proves nothing")
	}

	// PRUNE THE BODY: advance past msgMaxAge but below ack.Retention, then read the
	// provenance, which prunes retention-first. The message-provenance FALLBACK is
	// now dead, so ONLY the destination row can authorise the ack.
	clock.Advance(msgMaxAge + time.Minute)
	if _, ok := h.Store().RelayProvenanceByOriginMessageID(key); ok {
		t.Fatalf("the relayed message body survived past its %s MaxAge; the fallback is still alive and this test would pass without needing the destination row — FIX THE FIXTURE, not the assertion", msgMaxAge)
	}

	// THE ACK STILL RESOLVES AS TRANSIT, authorised by the destination row whose
	// lifetime is ack.Retention (24h), independent of message-body pruning.
	res, err := h.AcknowledgeDelivery(deliveredAck(key, bob))
	if err != nil {
		t.Fatalf("AcknowledgeDelivery after the body was pruned = %v, want nil; the destination row must authorise the transit ack once the message body is gone", err)
	}
	if !res.Transit {
		t.Fatalf("Transit = false after the body was pruned; authorisation fell back to the ABSENT message instead of the destination row, which is the exact gap ACK-12-FU-DESTINATION-ROW closes")
	}
	// The row is unchanged (still accepted): the transit ack settles nothing.
	if r, ok := acks.Lookup(key, bob); !ok || r.State != ack.StateAccepted {
		t.Fatalf("the destination row is (%+v, %v) after the transit ack, want STILL accepted (a transit ack settles nothing locally)", r, ok)
	}
}

// TestDestinationRowSurvivesRestart is the DURABILITY test: the destination row
// and its authorisation ride a restart, rebuilt from the WAL ALONE, and still
// authorise a transit ack after the message body is pruned post-restart.
//
// This is a clean replay-after-close restart (not a kill -9 crash injection): the
// log is closed and reopened, and the ack table is rebuilt from the durable log
// with a fresh, empty ack.Store, so any row it holds can only have come off disk
// (invariant 5). The kill -9 durability of the acceptance write itself is already
// covered by ack_crash_test.go's TestAckLocalAcceptanceDurable.
func TestDestinationRowSurvivesRestart(t *testing.T) {
	clock := newTestClock()
	dir := t.TempDir()
	bob := agentID(t, testBusID, "bob")
	key := riOriginMessageID(t, 401)
	const msgMaxAge = time.Minute
	const ackRetention = 24 * time.Hour

	// --- SESSION 1: ingest the relayed message, confirm the row, close cleanly.
	{
		lg, err := wal.Open(wal.LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("wal.Open (session 1): %v", err)
		}
		acks := ack.NewStore(ack.Options{Now: clock.Now, Retention: ackRetention})
		if err := acks.Attach(lg); err != nil {
			t.Fatalf("Attach (session 1): %v", err)
		}
		h := openRelayDestHub(t, dir, lg, acks, clock, msgMaxAge, "bob")
		if _, err := h.IngestRelayed(context.Background(), riIngest(t, key, bob)); err != nil {
			t.Fatalf("IngestRelayed (session 1): %v", err)
		}
		if r, ok := acks.Lookup(key, bob); !ok || r.State != ack.StateAccepted {
			t.Fatalf("before restart the destination row is (%+v, %v), want accepted", r, ok)
		}
		if err := lg.Close(); err != nil {
			t.Fatalf("Close (session 1): %v", err)
		}
	}

	// --- SESSION 2: RESTART from the SAME wal. Rebuild the ack table from the log
	// alone with a FRESH, EMPTY store, so a recovered row can only have come off
	// disk.
	lg2, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open (restart): %v", err)
	}
	t.Cleanup(func() { _ = lg2.Close() })
	acks2 := ack.NewStore(ack.Options{Now: clock.Now, Retention: ackRetention})
	if _, err := wal.Replay(filepath.Join(dir, wal.WALFileName), acks2.Apply); err != nil {
		t.Fatalf("replaying the ack table from the durable log: %v", err)
	}
	if err := acks2.Attach(lg2); err != nil {
		t.Fatalf("Attach (restart): %v", err)
	}
	h2 := openRelayDestHub(t, dir, lg2, acks2, clock, msgMaxAge, "bob")

	// The row is present after the restart, rebuilt from disk (invariant 5).
	if r, ok := acks2.Lookup(key, bob); !ok || r.State != ack.StateAccepted {
		t.Fatalf("after restart the destination row is (%+v, %v), want accepted rebuilt from the log alone", r, ok)
	}
	// The message body was recovered too, so the prune below is a real transition.
	if _, ok := h2.Store().RelayProvenanceByOriginMessageID(key); !ok {
		t.Fatalf("the relayed message body was not recovered after restart; the prune assertion below would pass vacuously")
	}

	// PRUNE THE BODY post-restart: advance past msgMaxAge (below ackRetention).
	clock.Advance(msgMaxAge + time.Minute)
	if _, ok := h2.Store().RelayProvenanceByOriginMessageID(key); ok {
		t.Fatalf("the relayed message body survived %s of its %s MaxAge after restart; the fixture never reaches the state it exists to test", msgMaxAge+time.Minute, msgMaxAge)
	}

	// THE ACK STILL RESOLVES AS TRANSIT — authorised by the REPLAYED durable row,
	// not by the now-absent message. This is the property the task is about: the
	// authorisation rode the restart on the row, not on the body.
	res, err := h2.AcknowledgeDelivery(deliveredAck(key, bob))
	if err != nil {
		t.Fatalf("AcknowledgeDelivery after restart+prune = %v, want nil; the recovered destination row must authorise the transit ack independently of the message body", err)
	}
	if !res.Transit {
		t.Fatalf("Transit = false after restart+prune; the recovered destination row did not authorise the ack, so the authorisation did NOT survive the restart")
	}
}

// TestDuplicateRelayedIngestOpensNoSecondRow is invariant 10 on the destination
// row: a duplicate relayed ingest (same origin id + same content) is absorbed as
// a retry, opens NO second row, and disconnects nobody; a same-key
// different-payload ingest is the invariant-10 violation — refused and logged,
// not disconnected, and opens no row either.
func TestDuplicateRelayedIngestOpensNoSecondRow(t *testing.T) {
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "bob")
	bob := agentID(t, testBusID, "bob")
	key := riOriginMessageID(t, 351)

	first, err := h.IngestRelayed(context.Background(), riIngest(t, key, bob))
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if first.Outcome != idem.OutcomeNew {
		t.Fatalf("first arrival Outcome = %s, want %s", first.Outcome, idem.OutcomeNew)
	}
	if n := acks.Len(); n != 1 {
		t.Fatalf("the first ingest opened %d rows, want 1", n)
	}
	if r, ok := acks.Lookup(key, bob); !ok || r.State != ack.StateAccepted {
		t.Fatalf("the first ingest's row is (%+v, %v), want accepted", r, ok)
	}
	walAfterFirst := len(ackEntriesIn(t, dir))

	// DUPLICATE: same origin id + same content. Absorbed as a retry, NO second row,
	// no new durable record, no error (invariant 10's first case).
	second, err := h.IngestRelayed(context.Background(), riIngest(t, key, bob))
	if err != nil {
		t.Fatalf("the duplicate ingest errored: %v — a legitimate retry must return the original result, not error (invariant 10)", err)
	}
	if second.Outcome != idem.OutcomeRetry {
		t.Fatalf("the duplicate Outcome = %s, want %s", second.Outcome, idem.OutcomeRetry)
	}
	if second.MessageID != first.MessageID {
		t.Fatalf("the duplicate returned message id %q, want the ORIGINAL %q", second.MessageID, first.MessageID)
	}
	if n := acks.Len(); n != 1 {
		t.Fatalf("the duplicate opened a SECOND row (table now %d), want exactly 1 per (origin id, recipient)", n)
	}
	if got := len(ackEntriesIn(t, dir)); got != walAfterFirst {
		t.Fatalf("the duplicate appended %d lifecycle records, want 0: a retry re-applies nothing", got-walAfterFirst)
	}

	// NEGATIVE: same origin id + DIFFERENT content is invariant 10's third case.
	// Refused and logged, NOBODY disconnected, and NO row created.
	bad := riIngest(t, key, bob)
	bad.Body = []byte("a DIFFERENT payload presented under the same origin message id")
	third, err := h.IngestRelayed(context.Background(), bad)
	if !errors.Is(err, hub.ErrIdempotencyKeyReused) {
		t.Fatalf("same key + different payload = %v, want ErrIdempotencyKeyReused (invariant 10's third case)", err)
	}
	if third.Outcome != idem.OutcomeViolation {
		t.Fatalf("the violation Outcome = %s, want %s", third.Outcome, idem.OutcomeViolation)
	}
	if n := acks.Len(); n != 1 {
		t.Fatalf("the violation opened a row (table now %d), want the original 1", n)
	}
	// The bus is still usable — the violation cost only the offending call. A
	// transit ack for the ORIGINAL row still resolves.
	if res, err := h.AcknowledgeDelivery(deliveredAck(key, bob)); err != nil || !res.Transit {
		t.Fatalf("after a violation the bus refused a well-formed transit ack: (%+v, %v); invariant 10 was narrowed so a violation costs the offending call and nothing else", res, err)
	}
}

// TestBroadcastGetsNoDestinationRow is the documented NON-GOAL: a broadcast gets
// no destination row. recordAcceptance keeps its deliberate broadcast
// early-return; ACK-12-FU-DESTINATION-ROW removed only the `relayed` arm.
//
// This SKIPS today because hub.Broadcast fails closed under signing format v1
// (skipIfBroadcastHasNoSigningDigest, hub_test.go) — a broadcast has no canonical
// audience, so there is no (message, recipient) pair to key a row on. The day
// SIGN-3 lands and broadcasts start succeeding, this test runs on its own and
// asserts the non-goal, self-healing exactly like the rest of the broadcast suite.
func TestBroadcastGetsNoDestinationRow(t *testing.T) {
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "alpha", "beta")
	alpha := agentID(t, testBusID, "alpha")

	if _, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: alpha, Body: []byte("to everyone"), IdempotencyKey: "k-bcast-norow"}); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if n := acks.Len(); n != 0 {
		t.Fatalf("a broadcast opened %d destination lifecycle rows, want 0: a broadcast has no canonical audience to key a row on (the documented non-goal)", n)
	}
	if rows := ackEntriesIn(t, dir); len(rows) != 0 {
		t.Fatalf("a broadcast wrote %d lifecycle records to the durable log, want 0", len(rows))
	}
}

// openRelayDestHub opens a Hub over an already-open log with a controllable clock
// and a small message MaxAge, wiring the lifecycle recorder. It is the
// restart-test twin of openAckHub (ack_seam_test.go), which takes no clock or
// MaxAge — the restart test needs both, to age the message body out while the ack
// row stays live.
func openRelayDestHub(t *testing.T, dir string, lg *wal.Log, acks hub.AckRecorder, clock *testClock, msgMaxAge time.Duration, agents ...string) *hub.Hub {
	t.Helper()
	path := lg.Path()
	roster := hub.NewStaticRoster()
	h, err := hub.Open(hub.Options{
		BusID:     testBusID,
		DataDir:   dir,
		Durable:   lg,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: lg.Recovered().NextIndex,
		Roster:    roster,
		Acks:      acks,
		Now:       clock.Now,
		MaxAge:    msgMaxAge,
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	enrolAll(t, roster, testBusID, agents...)
	return h
}
