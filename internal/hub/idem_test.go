// These tests cover IDEM-11's wiring of internal/idem into the hub, through
// the EXPORTED surface only — the same posture hub_test.go takes.
package hub_test

import (
	"errors"
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
