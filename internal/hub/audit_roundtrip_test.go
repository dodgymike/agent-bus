package hub_test

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// The PRODUCER-SIDE round trip for the audit log (DUR-5, invariant 6).
//
// # Why this test exists, stated plainly, because its absence is what shipped
//
// internal/wal's audit tests exercise that package IN ISOLATION: they build an
// AuditRecord by hand, write it, read it back and assert it survived. Every one
// of them stayed GREEN while internal/hub passed a zero-valued
// wal.AuditRecord{} at the only call site in the product that writes one -- so
// the enforcement landed, the producer did not satisfy it, and POST /v1/send
// returned 500 on every message with "audit record is invalid: message_id is
// empty".
//
// No test in internal/wal could have caught that. The hub's call site is not
// visible from there. So the check has to live HERE, on the far side of the
// boundary, and it has to run the REAL write path rather than a hand-built
// record: the failure mode is precisely a producer that never populates.
// ---------------------------------------------------------------------------

// TestSendWritesItsAuditRecord is the round trip: a plain directed send on a
// PRISTINE data directory succeeds with the audit path compiled in, and the
// record it leaves in bus.audit is the message that was actually minted.
//
// A pristine directory is part of the property, not incidental setup. The
// regression this pins broke the FIRST send against a fresh bus -- a freshly
// enrolled agent's very first message -- so a fixture that pre-seeded a log
// would be testing a state no new bus is ever in.
func TestSendWritesItsAuditRecord(t *testing.T) {
	h, lg, dir := newTestHub(t, "alice", "bob")
	sender := agentID(t, testBusID, "alice")
	to := agentID(t, testBusID, "bob")
	body := []byte("the audit trail records that this was sent, never what it said")

	// The mint is taken explicitly rather than through mintedSend, because the
	// assertion below is that the audit record carries THE ID THE MINT RETURNED.
	// Reading that id back out of the send's own Result would compare the write
	// path against itself and would still pass if both halves drifted together.
	minted := mintFor(t, h, sender, "send", "k-audit-roundtrip")
	if minted.MessageID == "" {
		t.Fatal("Mint returned no message id; there is nothing for the audit record to be compared against")
	}

	res, err := h.Send(hub.SendRequest{
		Sender:         sender,
		To:             to,
		Body:           body,
		IdempotencyKey: "k-audit-roundtrip",
		SignedMint:     minted,
	})
	if err != nil {
		// THE HEADLINE ASSERTION. Before the fix this is
		// "wal: audit record is invalid: message_id is empty" -- and note that
		// the message id was never empty, the RECORD was.
		t.Fatalf("a first send on a pristine data directory failed: %v", err)
	}
	if res.MessageID != minted.MessageID {
		t.Fatalf("Send returned message id %q but the mint issued %q", res.MessageID, minted.MessageID)
	}

	// Read the audit log back off disk through the SAME decoder an fsck would
	// use, so a record that only this process can interpret fails here.
	//
	// The log is closed first: the assertion is about what is DURABLE, and a
	// reader that raced the writer's buffers would be testing memory.
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the durable log: %v", err)
	}
	auditPath := filepath.Join(dir, wal.AuditFileName)
	recs, _, err := wal.ScanAll(auditPath, wal.KindAudit)
	if err != nil {
		t.Fatalf("scanning %s: %v", auditPath, err)
	}
	if len(recs) != 1 {
		t.Fatalf("the audit log holds %d records after ONE send, want exactly 1: invariant 6 says every message is written to the trail, and only messages are", len(recs))
	}
	got, prepareIndex, err := wal.DecodeAudit(auditPath, recs[0])
	if err != nil {
		t.Fatalf("decoding the audit record: %v", err)
	}

	// --- the identity join: the record is THIS message -------------------
	if got.MessageID != minted.MessageID {
		t.Fatalf("the audit record names message %q but the mint issued %q; an audit row nobody can join on is not a trail", got.MessageID, minted.MessageID)
	}
	if got.Seq != minted.Seq {
		t.Fatalf("the audit record carries sequence %d, the mint issued %d", got.Seq, minted.Seq)
	}
	if prepareIndex == 0 {
		t.Fatal("the audit record carries prepare index 0; wal stamps it from the transaction so an fsck can pair the two files, and 0 means it was never set")
	}

	// --- every field invariant 6 names is POPULATED ----------------------
	//
	// Checked one by one rather than with a struct compare, because the defect
	// this test exists for was a record that was structurally fine and entirely
	// EMPTY. A zero value in any single field here is the same class of bug.
	if got.Sender != sender {
		t.Errorf("audit sender = %q, want %q", got.Sender, sender)
	}
	if got.Broadcast {
		t.Error("the audit record for a directed send is marked broadcast")
	}
	if len(got.Recipients) != 1 || got.Recipients[0] != to {
		t.Errorf("audit recipients = %v, want [%s]", got.Recipients, to)
	}
	if len(got.BusPath) != 1 || got.BusPath[0] != testBusID {
		t.Errorf("audit bus path = %v, want [%s]: the accepting bus is always the first hop", got.BusPath, testBusID)
	}
	if got.SentAt.IsZero() {
		t.Error("audit sent_at is the zero time")
	}
	if got.Size != int64(len(body)) {
		t.Errorf("audit size = %d, want %d", got.Size, len(body))
	}

	// --- THE CONTENT HASH, and the trap it is pinning --------------------
	//
	// PROTOCOL.md 8.6 binds this field to signing.CanonicalDigest -- SHA-256
	// over the canonical SIGNING bytes. The wrong answer, store.ContentHash of
	// the bare body, is ALSO 64 lowercase hex characters, so nothing in wal can
	// reject it and no assertion on shape will ever notice. This is the only
	// place the two are told apart.
	//
	// The expected value is rebuilt from the message's parts INDEPENDENTLY here,
	// rather than by calling the hub's own helper, so that a helper which
	// changed what it hashes would fail this test instead of moving it.
	want, err := signing.CanonicalDigest(signing.Message{
		MessageID:          minted.MessageID,
		Sequence:           minted.Seq,
		Sender:             sender,
		Recipients:         []string{to},
		TimestampUnixMilli: fixtureTimestampMs,
		Body:               body,
	})
	if err != nil {
		t.Fatalf("building the expected canonical digest: %v", err)
	}
	wantHex := hex.EncodeToString(want[:])
	if got.ContentSHA256 != wantHex {
		t.Errorf("audit content_sha256 = %q, want the CANONICAL digest %q (PROTOCOL.md 8.6)", got.ContentSHA256, wantHex)
	}
	// Stated as its own assertion so the failure message says WHICH wrong hash
	// was used. If the two ever coincide the canonical format has stopped
	// covering anything but the body, and that is worth failing on too.
	if bare := store.ContentHash(body); got.ContentSHA256 == bare {
		t.Errorf("audit content_sha256 is store.ContentHash(body) = %q -- the BARE BODY hash. PROTOCOL.md 8.6 requires SHA-256 over the canonical signing bytes; the bare-body hash fingerprints content while proving nothing about who sent it, to whom, or in what order", bare)
	}

	// --- and NO BODY, ever -----------------------------------------------
	//
	// Asserted against the raw file rather than the decoded record: a body could
	// only reach disk through a field this struct does not have, so decoding
	// first would look in the wrong place. The producer is what could leak it,
	// which is why this check belongs on this side of the boundary as well as in
	// internal/wal.
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("reading %s: %v", auditPath, err)
	}
	if bytes.Contains(raw, body) {
		t.Fatal("the audit log contains the message BODY; invariant 6 keeps bodies out of the trail so it stays writable once payloads are end-to-end encrypted")
	}
}
