package hub_test

// SIGN-1-FU-OUTOFORDER-POISON — THE REPRODUCTION.
//
// # The defect, in one paragraph
//
// SIGN-1 made a send a TWO-STEP: Hub.Mint allocates and DURABLY BURNS a sequence
// number so the client can sign it, and only then does the client send. The
// reservation lives for hub.MintTTL — fifteen minutes — so two agents holding
// reservations at the same time and spending them in the OTHER order is not an
// exotic race, it is the ordinary shape of the protocol. The TTL sets the size
// of the window, not whether the hole exists: shortening it narrows the
// exposure and closes nothing. But store.Append still requires a
// strictly increasing sequence, and hub.publish calls it AFTER the two-phase
// durable write has already committed and fsynced. So the lower-numbered send
// lands on disk, is refused by the serving copy, and the hub sets h.poisoned —
// permanently. Every later Mint and Send on that hub answers ErrPoisoned until
// the process restarts, and on restart Hub.Apply cannot append the record
// either, so it logs a DISCARD on EVERY start, for ever.
//
// Any enrolled agent can do this at will with two mints and two sends. That is
// why the task is P0 and why the security gate ruled that relay ingest must not
// be wired up until it is fixed.
//
// # Two local agents. NO relay.
//
// The task description also records a relay-ingest shape of the same defect (one
// local agent holding an outstanding mint while a relayed message arrives). It
// is deliberately NOT tested here: hub.IngestRelayed does not exist at this
// commit, so a test for it could not compile in a clean overlay of HEAD. The
// two-local-agent shape below needs no relay, no peer and no federation to
// reproduce, which makes it the stronger reproduction anyway.
//
// # What must stay true when this is fixed
//
// Invariant 1: the sequence never rewinds and a number is never reused — a fix
// that let the store rewind its head, or accept the same sequence twice, would
// trade this P0 for a worse one. Invariant 4: the record is durable before the
// ack, and that ordering is not touched. Invariant 5: memory is the serving copy
// and disk is the truth, so recovery must rebuild EXACTLY what was served.

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/hub"
)

// discardOnApplyLine is the recovery log line the orphaned record produces. It is
// matched as a SUBSTRING of the operator-facing message because that message is
// the observable consequence: invariant 6 requires a discard to be loud, so the
// only way to see "recovery threw this record away" from outside the package is
// to read what the operator is told.
const discardOnApplyLine = "DISCARDING a message record that could not be applied during recovery"

// signedMintFrom dresses a reservation as the SignedMint a client presents back.
//
// It is distinct from hub_test.go's mintFor, which mints and dresses in one step:
// these tests must hold TWO reservations open at once and spend them in the
// wrong order, which is exactly what a helper that does both cannot express.
func signedMintFrom(m hub.Mint) hub.SignedMint {
	return hub.SignedMint{
		MessageID:          m.MessageID,
		Seq:                m.Seq,
		TimestampUnixMilli: fixtureTimestampMs,
		Signature:          fixtureSignature(),
	}
}

// TestOutOfOrderMintSpendDoesNotPoison is the end-to-end reproduction: two local
// agents mint, then spend in the opposite order, and the bus must survive it.
func TestOutOfOrderMintSpendDoesNotPoison(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	h, _ := openMintHub(t, dir, lg, nil, "", "alice", "bob", "carol")

	alice := agentID(t, testBusID, "alice")
	bob := agentID(t, testBusID, "bob")
	carol := agentID(t, testBusID, "carol")

	// BOTH reservations are taken before EITHER is spent. That is the whole
	// setup: nothing here is concurrent or timing-dependent, because the mint
	// table holds a reservation for hub.MintTTL.
	aliceMint := mustMint(t, h, alice, "send", "k-alice")
	bobMint := mustMint(t, h, bob, "send", "k-bob")

	// Guard the premise. If mints ever stopped being handed out in call order
	// this test would still pass while testing nothing.
	if aliceMint.Seq >= bobMint.Seq {
		t.Fatalf("PREMISE BROKEN: alice minted sequence %d and bob minted %d; this test needs alice to hold "+
			"the LOWER reservation so that spending bob's first is an out-of-order spend",
			aliceMint.Seq, bobMint.Seq)
	}

	// The HIGHER reservation is spent first. This succeeds at HEAD.
	bobRes, err := h.Send(hub.SendRequest{
		Sender: bob, To: carol, Body: []byte("bob to carol"),
		IdempotencyKey: "k-bob", SignedMint: signedMintFrom(bobMint),
	})
	if err != nil {
		t.Fatalf("bob spending the HIGHER reservation (sequence %d) first: %v", bobMint.Seq, err)
	}

	// THE LOWER RESERVATION IS SPENT SECOND. THIS IS THE ASSERTION THAT IS RED
	// AT HEAD.
	aliceRes, err := h.Send(hub.SendRequest{
		Sender: alice, To: carol, Body: []byte("alice to carol"),
		IdempotencyKey: "k-alice", SignedMint: signedMintFrom(aliceMint),
	})
	if err != nil {
		if errors.Is(err, hub.ErrPoisoned) {
			t.Fatalf("SPENDING A RESERVATION OUT OF ORDER POISONED THE BUS: alice spent sequence %d after "+
				"bob had spent %d, and the hub is now refusing ALL writes until the process restarts. The "+
				"message was already committed and fsynced before the serving copy refused it, so it is "+
				"orphaned on disk as well (SIGN-1-FU-OUTOFORDER-POISON): %v",
				aliceMint.Seq, bobMint.Seq, err)
		}
		t.Fatalf("alice spending the LOWER reservation (sequence %d) second was refused: %v", aliceMint.Seq, err)
	}
	if errors.Is(err, hub.ErrPoisoned) {
		t.Fatalf("the out-of-order send returned a poison error alongside a nil-checked success: %v", err)
	}
	if p := h.Poisoned(); p != nil {
		t.Fatalf("the hub reports itself POISONED after an out-of-order spend that returned no error: %v", p)
	}

	// THE BUS IS NOT STOPPED. A poisoned hub refuses every subsequent mint AND
	// every subsequent send, so a third agent completing the two-step is the
	// direct check that any enrolled agent has not just halted the bus.
	carolMint := mustMint(t, h, carol, "send", "k-carol")
	if _, err := h.Send(hub.SendRequest{
		Sender: carol, To: alice, Body: []byte("carol to alice"),
		IdempotencyKey: "k-carol", SignedMint: signedMintFrom(carolMint),
	}); err != nil {
		t.Fatalf("a THIRD agent's mint+send after the out-of-order spend was refused, so the bus is stopped: %v", err)
	}

	// BOTH messages are readable, in ASCENDING SEQUENCE ORDER. carol is the
	// recipient of both and the sender of neither, so History is the read path a
	// real client would use. Ordering matters as much as presence: the serving
	// copy is binary-searched by cursor, so a late message parked at the end of
	// the slice would be invisible to every reader already past it.
	if got, want := historyIDs(t, h, carol), []string{aliceRes.MessageID, bobRes.MessageID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("carol reads %v, want %v — both messages, in ascending sequence order (alice minted %d, bob minted %d)",
			got, want, aliceMint.Seq, bobMint.Seq)
	}
}

// TestOutOfOrderMintSpendSurvivesRestart is the DURABILITY half.
//
// The out-of-order record is written to disk BEFORE the serving copy rejects it,
// so the damage outlives the process: Hub.Apply cannot append it either, and
// logs a discard on every start for ever. Worse, that path returns before
// recoverIdemRecord, so the message's applied-key record is never rebuilt — a
// client retrying that idempotency key after a restart would get a SECOND
// message, which is the double-apply invariant 10 forbids.
//
// The send failure is reported with t.Errorf rather than t.Fatalf on purpose:
// at HEAD the record IS on disk, and the restart assertions below are the
// evidence for the durable half of the defect, so the test must reach them.
func TestOutOfOrderMintSpendSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, false)
	h, _ := openMintHub(t, dir, lg, nil, "", "alice", "bob", "carol")

	alice := agentID(t, testBusID, "alice")
	bob := agentID(t, testBusID, "bob")
	carol := agentID(t, testBusID, "carol")

	aliceMint := mustMint(t, h, alice, "send", "k-alice")
	bobMint := mustMint(t, h, bob, "send", "k-bob")
	if aliceMint.Seq >= bobMint.Seq {
		t.Fatalf("PREMISE BROKEN: alice minted sequence %d and bob minted %d; alice must hold the LOWER one",
			aliceMint.Seq, bobMint.Seq)
	}

	if _, err := h.Send(hub.SendRequest{
		Sender: bob, To: carol, Body: []byte("bob to carol"),
		IdempotencyKey: "k-bob", SignedMint: signedMintFrom(bobMint),
	}); err != nil {
		t.Fatalf("bob spending the HIGHER reservation (sequence %d) first: %v", bobMint.Seq, err)
	}
	if _, err := h.Send(hub.SendRequest{
		Sender: alice, To: carol, Body: []byte("alice to carol"),
		IdempotencyKey: "k-alice", SignedMint: signedMintFrom(aliceMint),
	}); err != nil {
		t.Errorf("alice spending the LOWER reservation (sequence %d) second was refused: %v", aliceMint.Seq, err)
	}

	// A CLEAN restart: close the log, reopen it, rebuild the hub over the SAME
	// data directory. No damage is injected — everything below is the cost of
	// the orphaned record alone.
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}
	lg2 := openTestLog(t, dir, true)
	h2, buf := openMintHub(t, dir, lg2, nil, "", "alice", "bob", "carol")

	if got := buf.String(); strings.Contains(got, discardOnApplyLine) {
		t.Errorf("RECOVERY DISCARDED A COMMITTED MESSAGE: the record alice's out-of-order send committed "+
			"(sequence %d) cannot be applied by Hub.Apply either, so this discard repeats on EVERY restart "+
			"and the message's applied-key record is never rebuilt. Recovery log was:\n%s",
			aliceMint.Seq, got)
	}
	if got, want := historyIDs(t, h2, carol), []string{aliceMint.MessageID, bobMint.MessageID}; !reflect.DeepEqual(got, want) {
		t.Errorf("after a restart carol reads %v, want %v — recovery must rebuild EXACTLY what was served "+
			"(invariant 5: memory is the serving copy, disk is the truth)", got, want)
	}
	if p := h2.Poisoned(); p != nil {
		t.Errorf("the REOPENED hub is poisoned: %v", p)
	}
}
