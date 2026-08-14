package hub

// ---------------------------------------------------------------------------
// RELAY-11 -- the bus path a message travelled reaches the audit trail.
//
// Invariant 6 names "the bus path traversed" as part of the audit record, and
// that field is the entire reason a relay hop is auditable. Until this task the
// path was UNWRITABLE beyond one hop: wal.AuditRecord.BusPath existed and was
// validated, but store's only constructor hard-coded []string{busID} and no hub
// API accepted a path, so A->B->C could never be evidenced from C's trail.
//
// # Why these tests are in package hub and not package hub_test
//
// The capability lands at hub.publish, which is UNEXPORTED and has no exported
// caller yet: the relay ingest route that will supply a path is a later task.
// Testing it through a route that does not exist is not possible, and inventing
// an exported entry point so a test could reach it would ship an untested public
// surface to make a test convenient -- worse, one a CLIENT might reach, which is
// exactly the forgery the path must never be open to. So this file is an
// internal test and calls publish directly.
//
// It therefore cannot use the fixtures in hub_test.go (a different package) and
// carries its own, prefixed bp* so the two can never be confused.
//
// # The pair of tests is the point
//
// TestAuditRecordsMultiHopBusPath proves the NEW capability. Its twin,
// TestLocalSendAuditPayloadIsUnchanged, proves the capability cost the existing
// trail NOTHING -- a byte-for-byte comparison against the exact audit payload
// this code wrote before the change. A new field that quietly reshapes every
// local send's record would break the continuity of an APPEND-ONLY trail that
// cannot be rewritten afterwards, and no assertion about the new path would
// notice.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// bpBusID is the bus under test: the bus that ACCEPTS, and therefore the hop
// that must be last in every path it records.
const bpBusID = "testbus"

// bpOriginBusID and bpMiddleBusID are the two upstream hops of the A->B->C line
// the FEDERATION epic exists to deliver: this bus is C.
const (
	bpOriginBusID = "busa"
	bpMiddleBusID = "busb"
)

// bpTimestampMs is the SENDER's clock on every fixture message, and bpClock is
// THIS BUS's clock. They are different facts (store.Message.TimestampUnixMilli)
// and are deliberately different values here so a test that confused them would
// fail rather than coincide.
const bpTimestampMs int64 = 1754130896789

var bpClock = time.Date(2026, 8, 2, 12, 34, 56, 789000000, time.UTC)

// bpEnrolledAt is when every fixture agent joined: BEFORE the fixture traffic,
// or Message.VisibleTo would filter every message out of every read and each
// assertion below would pass on an empty batch.
var bpEnrolledAt = bpClock.Add(-time.Hour)

// bpSignature is a well-formed 64-byte placeholder. The bus enforces the LENGTH
// and never verifies -- it does not hold the sender's messaging key -- so a
// constant keeps the durable fixture reproducible byte for byte, which the
// golden comparison below depends on.
func bpSignature() []byte { return bytes.Repeat([]byte{0xAB}, signing.SignatureSize) }

// bpHub opens a real WAL in t.TempDir() and a Hub over it with alice and bob
// enrolled, on a FROZEN clock.
//
// The clock is frozen because one of these tests compares the audit payload byte
// for byte, and sent_at is in it. It returns the log so a test can Close it
// before reading the trail -- an assertion about what is DURABLE must not race
// the writer's buffers -- and the directory the trail lives in.
func bpHub(t *testing.T) (*Hub, *wal.Log, string) {
	t.Helper()
	dir := t.TempDir()
	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = lg.Close() })

	roster := NewStaticRoster()
	h, err := Open(Options{
		BusID:     bpBusID,
		DataDir:   dir,
		Durable:   lg,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(lg.Path(), fn) },
		NextIndex: lg.Recovered().NextIndex,
		Roster:    roster,
		Now:       func() time.Time { return bpClock },
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	for _, name := range []string{"alice", "bob"} {
		// Enrolled BEFORE the fixture traffic: a roster whose EnrolledAt sat after
		// it would make every read return nothing (Message.VisibleTo).
		roster.Add(Agent{AgentID: bpAgent(t, name), Name: name, EnrolledAt: bpEnrolledAt})
	}
	return h, lg, dir
}

// bpAgent builds the fully-qualified "<bus-id>.<name>-1" (invariant 2).
func bpAgent(t *testing.T, name string) string {
	t.Helper()
	id, err := ids.AgentID(bpBusID, name, 1)
	if err != nil {
		t.Fatalf("ids.AgentID(%q, %q, 1): %v", bpBusID, name, err)
	}
	return id
}

// bpMint reserves an assignment and dresses it as the SignedMint a client
// presents back. Since SIGN-1 a publish without one is ErrUnknownMint, so every
// publish here goes through the mint a real client would.
func bpMint(t *testing.T, h *Hub, sender, op, key string) SignedMint {
	t.Helper()
	m, err := h.Mint(MintRequest{Sender: sender, Op: op, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("Mint(%s, %s, %q): %v", sender, op, key, err)
	}
	return SignedMint{
		MessageID:          m.MessageID,
		Seq:                m.Seq,
		TimestampUnixMilli: bpTimestampMs,
		Signature:          bpSignature(),
	}
}

// bpAuditRecords closes the log and reads every record out of bus.audit through
// the SAME decoder an fsck would use.
//
// Going through wal.DecodeAudit rather than reading the JSON is load-bearing for
// the multi-hop test: DecodeAudit RE-APPLIES the writer's own validate() on the
// way out, so a three-hop path that this package was willing to write but that
// wal.AuditRecord.validate would refuse fails HERE rather than being discovered
// by an operator's fsck one day.
func bpAuditRecords(t *testing.T, lg *wal.Log, dir string) []wal.AuditRecord {
	t.Helper()
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the durable log: %v", err)
	}
	path := filepath.Join(dir, wal.AuditFileName)
	recs, _, err := wal.ScanAll(path, wal.KindAudit)
	if err != nil {
		t.Fatalf("scanning %s: %v", path, err)
	}
	out := make([]wal.AuditRecord, 0, len(recs))
	for _, r := range recs {
		a, _, err := wal.DecodeAudit(path, r)
		if err != nil {
			t.Fatalf("decoding audit record %d: %v", r.Index, err)
		}
		out = append(out, a)
	}
	return out
}

// TestAuditRecordsMultiHopBusPath is RELAY-11's proof: a message INGESTED FROM A
// PEER, carrying the two hops it has already travelled plus this bus, leaves an
// audit record naming all three IN ORDER.
//
// It is the smallest honest model of the epic's deliverable -- A -> B -> C with
// the path visible at C -- that can exist before the relay ingest route does.
// The sender here is a LOCAL agent because publish still requires an enrolled
// sender; who a relayed message's sender may be, and how that is attested, is the
// ingest task's question and not this one's. What is under test is the PATH.
func TestAuditRecordsMultiHopBusPath(t *testing.T) {
	h, lg, dir := bpHub(t)
	sender, to := bpAgent(t, "alice"), bpAgent(t, "bob")
	body := []byte("relayed from busa via busb")

	// [origin, middle, this bus]: append-only and origin-first, with the
	// ingesting bus as the final hop.
	wantPath := []string{bpOriginBusID, bpMiddleBusID, bpBusID}

	// The caller's own slice, handed over and then MUTATED, to prove the message
	// took a copy: a peer-supplied slice the relay still holds must not be able
	// to rewrite the provenance of a message that is already durable.
	given := append([]string(nil), wantPath...)

	// The idempotency OUTCOME is not what this test is about; publish returns it
	// uncollapsed for the relay ingest (see Hub.IngestRelayed).
	res, _, err := h.publish(publishRequest{
		sender:     sender,
		broadcast:  false,
		recipients: []string{to},
		body:       body,
		key:        "k-multihop",
		signedMint: bpMint(t, h, sender, "send", "k-multihop"),
		busPath:    given,
	})
	if err != nil {
		// Before RELAY-11 there was no busPath field to set and no constructor to
		// carry it; a multi-hop path could not be recorded at all.
		t.Fatalf("publishing a relayed message carrying the path %v failed: %v", wantPath, err)
	}
	given[0] = "tampered"

	// --- the serving copy ------------------------------------------------
	msgs, _, _ := h.Store().Since(to, bpEnrolledAt, 0, 10)
	if len(msgs) != 1 {
		t.Fatalf("the serving copy holds %d messages after one publish, want 1", len(msgs))
	}
	if got := msgs[0].BusPath; !equalStrings(got, wantPath) {
		t.Errorf("the served message's bus path = %v, want %v (a caller's later mutation must not reach an accepted message)", got, wantPath)
	}

	// --- the durable message record --------------------------------------
	//
	// Read back off disk rather than out of memory: the path has to SURVIVE, or a
	// restart silently rewrites the provenance of every relayed message to one
	// hop.
	var replayed []store.Message
	if _, err := wal.Replay(lg.Path(), func(c wal.Committed) error {
		if c.Entry.Kind != store.RecordKind {
			return nil
		}
		m, derr := store.Decode(c.Entry.Body)
		if derr != nil {
			return derr
		}
		replayed = append(replayed, m)
		return nil
	}); err != nil {
		t.Fatalf("replaying the durable log: %v", err)
	}
	if len(replayed) != 1 {
		t.Fatalf("the durable log holds %d message records, want 1", len(replayed))
	}
	if got := replayed[0].BusPath; !equalStrings(got, wantPath) {
		t.Errorf("the DURABLE message record's bus path = %v, want %v", got, wantPath)
	}

	// --- THE AUDIT RECORD, which is what invariant 6 is about -------------
	audits := bpAuditRecords(t, lg, dir)
	if len(audits) != 1 {
		t.Fatalf("the audit trail holds %d records after one publish, want 1", len(audits))
	}
	got := audits[0]
	if got.MessageID != res.MessageID {
		t.Fatalf("the audit record names message %q, the publish returned %q", got.MessageID, res.MessageID)
	}
	if !equalStrings(got.BusPath, wantPath) {
		// THE HEADLINE ASSERTION. Before RELAY-11 this reads [testbus]: the path
		// was hard-coded to the accepting bus, so a relay hop left no trace and
		// the epic's whole deliverable -- proving A->B->C from the trail -- was
		// impossible.
		t.Fatalf("the audit record's bus path = %v, want %v: invariant 6 records the path TRAVERSED, which is the only thing that makes a relay hop auditable", got.BusPath, wantPath)
	}
	// Stated separately so a regression that dropped the upstream hops but kept
	// the shape says WHICH hop went missing.
	if len(got.BusPath) < 2 {
		t.Fatalf("the audit record carries %d hop(s); a relayed message that shows only the accepting bus proves nothing about where it came from", len(got.BusPath))
	}

	// --- and still NO BODY, ever -----------------------------------------
	//
	// Checked against the raw file, because a body could only reach it through a
	// field the decoded struct does not have. RELAY-11 adds a field to the record
	// the trail carries, which is exactly when this needs restating.
	raw, err := os.ReadFile(filepath.Join(dir, wal.AuditFileName))
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	if bytes.Contains(raw, body) {
		t.Fatal("the audit log contains the message BODY; invariant 6 keeps bodies out of the trail so it stays writable once payloads are end-to-end encrypted")
	}
}

// bpGoldenLocalAuditPayload is the EXACT audit payload this code wrote for a
// local send BEFORE RELAY-11, captured from the unmodified build on 2026-08-08.
//
// It is a whole-record golden and not a bus_path assertion on purpose: the
// property is that a local send's trail entry is BYTE-IDENTICAL, and a change
// that reshaped any other field -- a reordered key, a renamed one, a differently
// formatted timestamp -- would break an append-only trail that already holds
// records nobody can rewrite.
//
// If this fails, do not update the constant until you can say what changed and
// why the trail may be discontinuous. prepare_index is part of it, so a change to
// how many WAL records a send costs will fail here too; that is a real change to
// the trail and is worth stopping for.
const bpGoldenLocalAuditPayload = `{"message_id":"testbus-1","seq":1,"sender":"testbus.alice-1",` +
	`"broadcast":false,"recipients":["testbus.bob-1"],"bus_path":["testbus"],` +
	`"sent_at":"2026-08-02T12:34:56.789Z","size":6,` +
	`"content_sha256":"71fa9d78a7860178b61b8035c7b03d5fc091ce8478df18c0749c38a88699624a",` +
	`"prepare_index":3}`

// TestLocalSendAuditPayloadIsUnchanged is the twin of the test above: the new
// capability must cost the existing trail nothing.
//
// A plain local Send -- the ordinary path every client takes, which supplies no
// bus path at all -- must still produce the single-hop record it produced before
// publish could accept one, down to the byte.
func TestLocalSendAuditPayloadIsUnchanged(t *testing.T) {
	h, lg, dir := bpHub(t)
	sender, to := bpAgent(t, "alice"), bpAgent(t, "bob")

	if _, err := h.Send(SendRequest{
		Sender:         sender,
		To:             to,
		Body:           []byte("golden"),
		IdempotencyKey: "k-golden",
		SignedMint:     bpMint(t, h, sender, "send", "k-golden"),
	}); err != nil {
		t.Fatalf("a plain local send failed: %v", err)
	}

	if err := lg.Close(); err != nil {
		t.Fatalf("closing the durable log: %v", err)
	}
	path := filepath.Join(dir, wal.AuditFileName)
	recs, _, err := wal.ScanAll(path, wal.KindAudit)
	if err != nil {
		t.Fatalf("scanning %s: %v", path, err)
	}
	if len(recs) != 1 {
		t.Fatalf("the audit trail holds %d records after one send, want 1", len(recs))
	}
	if got := string(recs[0].Payload); got != bpGoldenLocalAuditPayload {
		t.Fatalf("a local send's audit payload changed.\n got: %s\nwant: %s\nThe trail is APPEND-ONLY: records already written cannot be brought into line with a new shape, so a local send must keep producing the record it produced before RELAY-11", got, bpGoldenLocalAuditPayload)
	}
}

// TestPublishRefusesABusPathThatDoesNotEndAtThisBus pins the one rule that keeps
// the trail honest when a path IS supplied: the ingesting bus appends itself, so
// a path that does not end here is refused rather than written.
//
// The refusal must cost NOTHING durable. A path this bus will not vouch for must
// not leave a half-record behind, and the message must not be served.
func TestPublishRefusesABusPathThatDoesNotEndAtThisBus(t *testing.T) {
	h, lg, dir := bpHub(t)
	sender, to := bpAgent(t, "alice"), bpAgent(t, "bob")

	cases := []struct {
		name string
		path []string
		why  string
	}{
		{
			name: "ends at the sending peer",
			path: []string{bpOriginBusID, bpMiddleBusID},
			why:  "this is the path AS RECEIVED ON THE WIRE; the ingesting bus must append itself before the record is built, or the trail at this bus never names this bus",
		},
		{
			name: "this bus is present but not last",
			path: []string{bpBusID, bpOriginBusID},
			why:  "a path is append-only and origin-first, so the final hop is always the bus writing the record",
		},
		{
			name: "a malformed hop",
			path: []string{"bus.a", bpBusID},
			why:  "every hop is untrusted peer input and is validated with the same rule as a hop read off disk; '.' is the invariant-2 separator and is never part of a bus id",
		},
	}
	for i, c := range cases {
		// A key an idempotency key may actually be: [A-Za-z0-9._-] only, so the
		// case name (which has spaces) cannot be used as one.
		key := fmt.Sprintf("k-refused-%d", i)
		_, _, err := h.publish(publishRequest{
			sender:     sender,
			broadcast:  false,
			recipients: []string{to},
			body:       []byte("should not be accepted"),
			key:        key,
			signedMint: bpMint(t, h, sender, "send", key),
			busPath:    c.path,
		})
		if err == nil {
			t.Fatalf("case %d (%s): publish ACCEPTED bus path %v; %s", i, c.name, c.path, c.why)
		}
		if !errors.Is(err, store.ErrInvalidMessage) {
			t.Errorf("case %d (%s): publish refused with %v, want a store.ErrInvalidMessage; the refusal is a malformed message, and a caller that cannot classify it cannot answer the peer correctly", i, c.name, err)
		}
	}

	served, _, _ := h.Store().Since(to, bpEnrolledAt, 0, 10)
	if n := len(served); n != 0 {
		t.Errorf("the serving copy holds %d messages after only refused publishes, want 0", n)
	}
	if audits := bpAuditRecords(t, lg, dir); len(audits) != 0 {
		t.Errorf("the audit trail holds %d records after only refused publishes, want 0: a path this bus will not vouch for must leave nothing behind", len(audits))
	}
}

// equalStrings compares two string slices element by element. reflect.DeepEqual
// would also do it, but it treats a nil slice and an empty one as different,
// which is not the distinction any assertion here is making.
func equalStrings(a, b []string) bool {
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
