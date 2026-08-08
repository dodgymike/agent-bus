package wal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// DUR-5 -- the append-only message audit log.
//
// Every test here is named TestAuditLog... on purpose: the task's registered
// proof command is `go test -race -run TestAuditLog ./internal/wal`, and a proof
// that matches nothing prints "ok ... [no tests to run]" and exits 0. The naming
// is what makes that proof non-vacuous.
//
// The properties under test, in the order a reviewer should check them:
//
//	1. every message entry produces exactly one audit record, and nothing else does
//	2. the record NEVER contains the message body (invariant 6)
//	3. an audit record that would be unusable fails the write with NOTHING written
//	4. the trail is a SUPERSET of committed history, proved with a real SIGKILL
//	5. damage in the audit log is discarded, LOGGED, and the bus still starts
//	6. a newer binary's extra fields do not make a record unreadable
// ---------------------------------------------------------------------------

// auditTestTime is a fixed clock so fixtures are byte-reproducible.
var auditTestTime = time.Date(2026, 8, 7, 11, 22, 33, 456789000, time.UTC)

// sampleAudit builds a valid directed-message audit record.
func sampleAudit(seq uint64) *AuditRecord {
	return &AuditRecord{
		MessageID:     fmt.Sprintf("bus-alpha-%d", seq),
		Seq:           seq,
		Sender:        "bus-alpha.worker-1",
		Recipients:    []string{"bus-alpha.worker-2", "bus-beta.worker-9"},
		BusPath:       []string{"bus-alpha"},
		SentAt:        auditTestTime.Add(time.Duration(seq) * time.Second),
		Size:          int64(10 + seq),
		ContentSHA256: strings.Repeat("ab", 32),
	}
}

// sampleBroadcastAudit builds a valid broadcast audit record: no recipients, and
// a bus path with a relay hop in it.
func sampleBroadcastAudit(seq uint64) *AuditRecord {
	a := sampleAudit(seq)
	a.Broadcast = true
	a.Recipients = nil
	a.BusPath = []string{"bus-alpha", "bus-beta"}
	return a
}

// auditEntry is a message entry that requests an audit record.
func auditEntry(seq uint64, body string) Entry {
	return Entry{Kind: "message", Body: json.RawMessage(body), Audit: sampleAudit(seq)}
}

// readAuditLog scans the audit file and decodes every record. It is the reader
// an operator or an fsck would use, so the tests exercise the same path.
func readAuditLog(t *testing.T, path string) ([]AuditRecord, []uint64) {
	t.Helper()
	recs, _, err := ScanAll(path, KindAudit)
	if err != nil {
		t.Fatalf("ScanAll(%s, audit): %v", path, err)
	}
	var got []AuditRecord
	var prepares []uint64
	for _, rec := range recs {
		a, prepareIndex, err := DecodeAudit(path, rec)
		if err != nil {
			t.Fatalf("DecodeAudit(record %d): %v", rec.Index, err)
		}
		got = append(got, a)
		prepares = append(prepares, prepareIndex)
	}
	return got, prepares
}

// auditMessageIDs is the trail reduced to the thing it is joined on.
func auditMessageIDs(recs []AuditRecord) []string {
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.MessageID
	}
	return ids
}

// TestAuditLogWritesOneRecordPerMessage is the core of invariant 6: every
// message reaches the audit trail, with the metadata and routing fields the
// invariant names, and every entry that is NOT a message reaches it not at all.
func TestAuditLogWritesOneRecordPerMessage(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, want := l.AuditPath(), filepath.Join(dir, AuditFileName); got != want {
		t.Fatalf("AuditPath() = %q, want %q", got, want)
	}

	// Two messages, a broadcast, and two entries that are NOT messages -- a
	// roster record and an invite record share this WAL and must leave no trace
	// in the message trail.
	var prepareOf []uint64
	for _, e := range []Entry{
		auditEntry(1, `{"to":"bus-alpha.worker-2"}`),
		{Kind: "agent", Body: json.RawMessage(`{"agent_id":"bus-alpha.worker-3"}`)},
		auditEntry(2, `{"to":"bus-alpha.worker-2"}`),
		{Kind: "invite", Body: nil},
		{Kind: "message", Body: json.RawMessage(`{"broadcast":true}`), Audit: sampleBroadcastAudit(3)},
	} {
		c, err := l.Write(e)
		if err != nil {
			t.Fatalf("Write(%s): %v", e.Kind, err)
		}
		if e.Audit != nil {
			prepareOf = append(prepareOf, c.PrepareIndex)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, prepares := readAuditLog(t, filepath.Join(dir, AuditFileName))
	if len(got) != 3 {
		t.Fatalf("the audit log holds %d records (%v), want 3: exactly the three messages, and neither the roster nor the invite record",
			len(got), auditMessageIDs(got))
	}
	if !reflect.DeepEqual(prepares, prepareOf) {
		t.Errorf("audit records carry prepare indices %v, want %v: the record must name the WAL transaction it was written in",
			prepares, prepareOf)
	}

	// Every field invariant 6 names survives the round trip.
	want := []*AuditRecord{sampleAudit(1), sampleAudit(2), sampleBroadcastAudit(3)}
	for i := range want {
		if !reflect.DeepEqual(got[i], *want[i]) {
			t.Errorf("audit record %d = %+v, want %+v", i, got[i], *want[i])
		}
	}
	// And the third is the broadcast, which is the case whose routing shape
	// differs: no recipients, two buses in the path.
	if !got[2].Broadcast || len(got[2].Recipients) != 0 {
		t.Errorf("the broadcast record is %+v, want Broadcast=true with no recipients", got[2])
	}
	if !reflect.DeepEqual(got[2].BusPath, []string{"bus-alpha", "bus-beta"}) {
		t.Errorf("bus_path = %v, want the traversed path [bus-alpha bus-beta]", got[2].BusPath)
	}
}

// TestAuditLogNeverRecordsTheMessageBody is the invariant-6 exclusion, asserted
// against the BYTES ON DISK rather than against the struct.
//
// It is deliberately blunt: it puts a distinctive string in the WAL entry body,
// confirms the WAL really did record it -- so that the test cannot pass because
// nothing was written at all -- and then requires that string to be absent from
// the audit file. Any future change that "helpfully" carries the body across
// fails here.
func TestAuditLogNeverRecordsTheMessageBody(t *testing.T) {
	const secret = "PLAINTEXT-THAT-MUST-NEVER-REACH-THE-AUDIT-TRAIL"

	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e := auditEntry(1, `{"body":"`+secret+`"}`)
	if _, err := l.Write(e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	walBytes, err := os.ReadFile(filepath.Join(dir, WALFileName))
	if err != nil {
		t.Fatalf("read the WAL: %v", err)
	}
	if !bytes.Contains(walBytes, []byte(secret)) {
		t.Fatalf("the WAL does not contain the body: this test proves nothing unless the body was really written somewhere")
	}
	auditBytes, err := os.ReadFile(filepath.Join(dir, AuditFileName))
	if err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	if bytes.Contains(auditBytes, []byte(secret)) {
		t.Fatalf("the audit log contains the message body. Invariant 6 (corrected 2026-08-02) says the audit trail records METADATA AND ROUTING INFO ONLY: a trail holding plaintext becomes unwritable the moment forward-secret payloads land, and one holding ciphertext the bus can never decrypt is dead weight")
	}
	if bytes.Contains(auditBytes, []byte(`"body"`)) {
		t.Fatalf("the audit record has a body field. There must not be one, ever")
	}
	// The content hash IS there: that is what preserves the ability to prove
	// WHAT was sent without retaining it.
	if !bytes.Contains(auditBytes, []byte(strings.Repeat("ab", 32))) {
		t.Fatalf("the audit log does not carry the content hash, which is the whole compromise that makes excluding the body acceptable")
	}
}

// TestAuditLogRejectsAnUnusableRecordAndWritesNothing covers the fail-closed
// rule: a message that cannot be audited is not accepted, and the rejection
// leaves BOTH files byte-for-byte unchanged so a caller may safely retry.
func TestAuditLogRejectsAnUnusableRecordAndWritesNothing(t *testing.T) {
	good := sampleAudit(1)
	mutate := func(f func(*AuditRecord)) *AuditRecord {
		a := *good
		a.Recipients = append([]string(nil), good.Recipients...)
		a.BusPath = append([]string(nil), good.BusPath...)
		f(&a)
		return &a
	}
	longID := strings.Repeat("z", maxAuditIDBytes+1)

	cases := []struct {
		name string
		rec  *AuditRecord
		want string
	}{
		{"NoMessageID", mutate(func(a *AuditRecord) { a.MessageID = "" }), "message_id is empty"},
		{"OversizedMessageID", mutate(func(a *AuditRecord) { a.MessageID = longID }), "message_id is"},
		{"ZeroSeq", mutate(func(a *AuditRecord) { a.Seq = 0 }), "seq is 0"},
		{"NoSender", mutate(func(a *AuditRecord) { a.Sender = "" }), "sender is empty"},
		{"DirectedWithNoRecipients", mutate(func(a *AuditRecord) { a.Recipients = nil }), "no recipients"},
		{"EmptyRecipient", mutate(func(a *AuditRecord) { a.Recipients = []string{""} }), "recipients[0] is empty"},
		{"BroadcastWithRecipients", mutate(func(a *AuditRecord) { a.Broadcast = true }), "addressed to the whole bus"},
		{"NoBusPath", mutate(func(a *AuditRecord) { a.BusPath = nil }), "bus_path is empty"},
		{"EmptyBusInPath", mutate(func(a *AuditRecord) { a.BusPath = []string{"bus-alpha", ""} }), "bus_path[1] is empty"},
		{"ZeroTime", mutate(func(a *AuditRecord) { a.SentAt = time.Time{} }), "sent_at is the zero time"},
		{"NegativeSize", mutate(func(a *AuditRecord) { a.Size = -1 }), "size is -1"},
		{"ShortHash", mutate(func(a *AuditRecord) { a.ContentSHA256 = "abcd" }), "lowercase hex"},
		{"UppercaseHash", mutate(func(a *AuditRecord) { a.ContentSHA256 = strings.ToUpper(strings.Repeat("ab", 32)) }), "lowercase hex"},
		{"NonHexHash", mutate(func(a *AuditRecord) { a.ContentSHA256 = strings.Repeat("zz", 32) }), "lowercase hex"},
		{
			"TooManyRecipients",
			mutate(func(a *AuditRecord) {
				a.Recipients = make([]string, maxAuditRecipients+1)
				for i := range a.Recipients {
					a.Recipients[i] = "bus-alpha.worker-x"
				}
			}),
			"the limit is",
		},
		{
			"TooLongBusPath",
			mutate(func(a *AuditRecord) {
				a.BusPath = make([]string, maxAuditBusPath+1)
				for i := range a.BusPath {
					a.BusPath[i] = "bus-x"
				}
			}),
			"the limit is",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			// One good message first, so the assertion below is about THIS
			// write rather than about an empty pair of files.
			if _, err := l.Write(auditEntry(1, `{"ok":true}`)); err != nil {
				t.Fatalf("Write(good): %v", err)
			}
			before := snapshotDataDir(t, dir)

			_, err = l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"x":1}`), Audit: tc.rec})
			if err == nil {
				t.Fatalf("Write with an invalid audit record succeeded; it must fail closed")
			}
			if !errors.Is(err, ErrInvalidAudit) {
				t.Fatalf("err = %v, want one matching ErrInvalidAudit", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %q", err, tc.want)
			}

			after := snapshotDataDir(t, dir)
			for name, want := range before {
				if !bytes.Equal(after[name], want) {
					t.Errorf("%s changed: a rejected write must leave every file byte-for-byte unchanged (%d bytes before, %d after)",
						name, len(want), len(after[name]))
				}
			}

			// The log is still usable: rejection is not poisoning.
			if _, err := l.Write(auditEntry(2, `{"ok":true}`)); err != nil {
				t.Fatalf("Write after a rejected one: %v", err)
			}
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			got, _ := readAuditLog(t, filepath.Join(dir, AuditFileName))
			if want := []string{"bus-alpha-1", "bus-alpha-2"}; !reflect.DeepEqual(auditMessageIDs(got), want) {
				t.Fatalf("audit trail = %v, want %v", auditMessageIDs(got), want)
			}
		})
	}
}

// TestAuditLogSnapshotsTheRecordAtBegin proves that "validated in Begin" is a
// claim about the bytes that actually get written.
//
// Begin validates and Commit encodes, and in between the caller owns its memory.
// Without a deep copy, a caller that reused its AuditRecord -- or just its
// Recipients slice -- would put an UNVALIDATED value in the audit trail: here,
// a different message id, a recipient the message never went to, and a content
// hash that would have been rejected outright.
func TestAuditLogSnapshotsTheRecordAtBegin(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	a := sampleAudit(1)
	want := *a.clone()

	txn, err := l.Begin(Entry{Kind: "message", Body: json.RawMessage(`{"x":1}`), Audit: a})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// The window: validated, prepare fsynced, audit record not yet encoded.
	a.MessageID = "bus-alpha-TAMPERED"
	a.Seq = 999
	a.Recipients[0] = "bus-evil.attacker"
	a.BusPath[0] = "bus-evil"
	a.ContentSHA256 = "" // would not have validated at all
	if _, err := txn.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, _ := readAuditLog(t, filepath.Join(dir, AuditFileName))
	if len(got) != 1 {
		t.Fatalf("the audit log holds %d records, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("audit record = %+v, want %+v: the record written must be the one that was validated, not whatever the caller's memory held by the time Commit ran",
			got[0], want)
	}
}

// TestAuditLogRejectsAnOversizedRecord covers the TOTAL field budget, which the
// per-field limits do not imply: JSON escaping expands a control or invalid-UTF-8
// byte sixfold, so limits on individual fields alone allow a payload several
// times MaxPayloadSize -- and it would be discovered in Commit, with a durable
// prepare already on disk, instead of in Begin with nothing written.
func TestAuditLogRejectsAnOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	a := sampleAudit(1)
	// Each id is inside maxAuditIDBytes and the count is inside
	// maxAuditRecipients; only the SUM is over budget.
	const each, n = 500, 300
	a.Recipients = make([]string, n)
	for i := range a.Recipients {
		a.Recipients[i] = strings.Repeat("r", each)
	}
	if each > maxAuditIDBytes || n > maxAuditRecipients {
		t.Fatalf("the fixture trips a per-field limit (%d bytes x %d), so it would not test the total budget", each, n)
	}
	if each*n <= maxAuditFieldBytes {
		t.Fatalf("the fixture totals %d bytes, which is inside the %d-byte budget", each*n, maxAuditFieldBytes)
	}

	before := snapshotDataDir(t, dir)
	_, err = l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"x":1}`), Audit: a})
	if err == nil {
		t.Fatalf("Write with an over-budget audit record succeeded")
	}
	if !errors.Is(err, ErrInvalidAudit) {
		t.Fatalf("err = %v, want one matching ErrInvalidAudit", err)
	}
	if !strings.Contains(err.Error(), "variable-length fields total") {
		t.Errorf("err = %v, want it to name the total budget", err)
	}
	after := snapshotDataDir(t, dir)
	for name, wantBytes := range before {
		if !bytes.Equal(after[name], wantBytes) {
			t.Errorf("%s changed: the rejection must happen before anything is written", name)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestAuditLogSurvivesARestart proves the trail is genuinely append-only across
// process lifetimes: a restart continues the file rather than starting a new
// one, and every earlier record is still readable under the directory's key.
func TestAuditLogSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	for run := 0; run < 3; run++ {
		l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
		if err != nil {
			t.Fatalf("run %d: Open: %v", run, err)
		}
		if _, err := l.Write(auditEntry(uint64(run+1), `{"run":1}`)); err != nil {
			t.Fatalf("run %d: Write: %v", run, err)
		}
		if err := l.Close(); err != nil {
			t.Fatalf("run %d: Close: %v", run, err)
		}
	}
	path := filepath.Join(dir, AuditFileName)
	got, _ := readAuditLog(t, path)
	want := []string{"bus-alpha-1", "bus-alpha-2", "bus-alpha-3"}
	if !reflect.DeepEqual(auditMessageIDs(got), want) {
		t.Fatalf("audit trail after three restarts = %v, want %v", auditMessageIDs(got), want)
	}
	recs, _, err := ScanAll(path, KindAudit)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	for i, rec := range recs {
		if rec.Index != uint64(i+1) {
			t.Fatalf("audit record %d has index %d, want %d: a restart must continue the file's index sequence, not restart it",
				i, rec.Index, i+1)
		}
		if rec.Type != TypeAuditMessage {
			t.Fatalf("audit record %d has type %s, want %s", i, rec.Type, TypeAuditMessage)
		}
	}
}

// TestAuditLogToleratesUnknownFields is the forward-compatibility requirement
// DUR-5 names explicitly: the CRYPTO epic must be able to add an
// encrypted-envelope descriptor without an on-disk format break.
//
// It is asserted in both directions, because only the pair is evidence: a record
// carrying a field this binary has never heard of still decodes, AND a record
// with damage that is not merely an unknown field still does not.
func TestAuditLogToleratesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AuditFileName)
	w, err := OpenWriter(path, KindAudit)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	future := []byte(`{"message_id":"bus-alpha-1","seq":1,"sender":"bus-alpha.worker-1",` +
		`"broadcast":false,"recipients":["bus-alpha.worker-2"],"bus_path":["bus-alpha"],` +
		`"sent_at":"2026-08-07T11:22:33.456789Z","size":11,"content_sha256":"` + strings.Repeat("ab", 32) + `",` +
		`"prepare_index":7,"envelope":{"scheme":"x3dh","epoch":4},"some_future_scalar":9}`)
	if _, err := w.Append(TypeAuditMessage, future); err != nil {
		t.Fatalf("Append: %v", err)
	}
	garbled := []byte(`{"message_id":"bus-alpha-2","seq":"not-a-number"}`)
	if _, err := w.Append(TypeAuditMessage, garbled); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recs, _, err := ScanAll(path, KindAudit)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	a, prepareIndex, err := DecodeAudit(path, recs[0])
	if err != nil {
		t.Fatalf("DecodeAudit on a record written by a newer binary: %v; unknown fields must be IGNORED, not fatal -- the audit log is never replayed into serving state, so refusing to read a newer trail is the worse failure", err)
	}
	if a.MessageID != "bus-alpha-1" || a.Seq != 1 || prepareIndex != 7 {
		t.Fatalf("decoded %+v (prepare %d), want the known fields intact", a, prepareIndex)
	}
	if _, _, err := DecodeAudit(path, recs[1]); err == nil {
		t.Fatalf("DecodeAudit accepted a record whose known fields are the wrong TYPE; tolerance is for fields we do not know, not for fields we do")
	}
}

// TestAuditLogRejectsARecordItWouldNotHaveWritten closes the loop on validation:
// the reader applies the same rules as the writer, so a record that was crafted
// or damaged into an unusable shape is reported as corruption rather than handed
// back as a row.
func TestAuditLogRejectsARecordItWouldNotHaveWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AuditFileName)
	w, err := OpenWriter(path, KindAudit)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	// Structurally valid JSON, correctly framed and correctly MAC'd -- and
	// still not a record this package would ever have written: no sender, no
	// bus path, no content hash.
	if _, err := w.Append(TypeAuditMessage, []byte(`{"message_id":"bus-alpha-1","seq":1,"sent_at":"2026-08-07T11:22:33Z"}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	recs, _, err := ScanAll(path, KindAudit)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	_, _, err = DecodeAudit(path, recs[0])
	if err == nil {
		t.Fatalf("DecodeAudit accepted a record with no sender, no bus path and no content hash")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want one matching ErrCorrupt", err)
	}
}

// TestAuditLogRefusesAWALInItsPlace is the "damage is not fatal, but being
// unable to read the file IS" line, drawn on the audit file.
//
// A WAL sitting where the audit log should be is not corruption to repair: it is
// a data directory that is not what we think it is, and reinterpreting one log
// as the other would destroy a file that is perfectly intact.
func TestAuditLogRefusesAWALInItsPlace(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.Write(auditEntry(1, `{"x":1}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	walBytes, err := os.ReadFile(filepath.Join(dir, WALFileName))
	if err != nil {
		t.Fatalf("read the WAL: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, AuditFileName), walBytes, fileMode); err != nil {
		t.Fatalf("plant the WAL at the audit path: %v", err)
	}
	_, err = Open(LogOptions{Dir: dir})
	if err == nil {
		t.Fatalf("Open succeeded with a WAL file at %s; recovery must not reinterpret one log as the other", AuditFileName)
	}
	if !strings.Contains(err.Error(), "wal file") || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("err = %v, want it to say the file is a wal file where an audit file was expected", err)
	}
}

// TestAuditLogRepairsDamageAndStillStarts is invariant 6's availability half
// applied to the audit trail: damaged records are discarded, the discard is
// LOGGED loudly and specifically, and the bus starts.
//
// Silent discard is the defect (rated P0), not discard itself -- so the log
// assertion here is as load-bearing as the "Open succeeded" one.
func TestAuditLogRepairsDamageAndStillStarts(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := uint64(1); i <= 3; i++ {
		if _, err := l.Write(auditEntry(i, `{"x":1}`)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(dir, AuditFileName)
	recs, end, err := ScanAll(path, KindAudit)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	last := recs[len(recs)-1]
	// Cut INSIDE the last frame: a cut on a frame boundary is byte-identical to
	// a shorter file and is not damage at all.
	cut := last.Offset + FrameHeaderSize + (end-last.Offset-FrameHeaderSize)/2
	if cut <= last.Offset+FrameHeaderSize || cut >= end {
		t.Fatalf("the last audit frame is too small to cut inside its payload (offset %d, end %d)", last.Offset, end)
	}
	truncate(t, path, cut)

	var buf bytes.Buffer
	l2, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open after damaging the audit log: %v\n--- log ---\n%s\nrecovery must ALWAYS reach a running server: a bus held hostage by one bad sector of its audit trail is worse than one that lost a record and said so", err, buf.String())
	}
	rec := l2.Recovered()
	if !rec.AuditRepaired.Truncated {
		t.Errorf("Recovered().AuditRepaired = %+v, want Truncated: the torn tail of the audit log was cut away", rec.AuditRepaired)
	}
	if rec.Repaired.Truncated || rec.Repaired.DiscardCount != 0 {
		t.Errorf("Recovered().Repaired = %+v, want zero: the WAL was not damaged and must not be reported as if it were", rec.Repaired)
	}
	logText := buf.String()
	if !strings.Contains(logText, "message audit log") {
		t.Errorf("the operator log does not say the AUDIT trail lost records. Silent discard is the defect, and a line that does not name which trail was damaged is nearly as bad.\n--- log ---\n%s", logText)
	}
	if !strings.Contains(logText, AuditFileName) {
		t.Errorf("the operator log does not name %s.\n--- log ---\n%s", AuditFileName, logText)
	}

	// The bus is usable and the trail continues, without reissuing an index.
	if _, err := l2.Write(auditEntry(4, `{"x":1}`)); err != nil {
		t.Fatalf("Write after recovery: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, _ := readAuditLog(t, path)
	want := []string{"bus-alpha-1", "bus-alpha-2", "bus-alpha-4"}
	if !reflect.DeepEqual(auditMessageIDs(got), want) {
		t.Fatalf("audit trail = %v, want %v: the damaged record is gone, the intact ones behind it are not", auditMessageIDs(got), want)
	}
}

// TestAuditLogNeverLaundersAForgedVersion1File is the security gate's P1,
// pinned.
//
// THE ATTACK: a version 1 frame is authenticated by CRC32C, which is UNKEYED --
// anyone can compute one. So an attacker with write access to the data directory
// but NO wal-mac.key writes a version 1 bus.audit full of records they made up.
// If recovery upgraded that file the way it upgrades a version 1 WAL, every
// forged record would be RE-SIGNED under the server's real key and would from
// then on verify. The gate demonstrated it end to end.
//
// THE FIX IS TO NOT HAVE THE UPGRADE: audit records have only ever been written
// at format version 2, so a version 1 bus.audit is never a real bus's file. It
// is quarantined like anything else this code cannot interpret -- renamed aside,
// not deleted -- and the bus starts.
func TestAuditLogNeverLaundersAForgedVersion1File(t *testing.T) {
	const forged = "FORGED-BY-AN-ATTACKER-WITH-NO-KEY"

	dir := t.TempDir()
	// A real bus first, so the directory has a genuine MAC key the attacker
	// does not know and cannot compute a version 2 tag with.
	l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.Write(auditEntry(1, `{"x":1}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The attacker replaces the trail with a version 1 file of their own,
	// carrying a record that names them. writeV1Log builds it with the unkeyed
	// CRC32C layout -- no key required, which is the whole point.
	path := filepath.Join(dir, AuditFileName)
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the real trail: %v", err)
	}
	payload := `{"message_id":"` + forged + `","seq":1,"sender":"bus-evil.attacker",` +
		`"broadcast":false,"recipients":["bus-alpha.worker-2"],"bus_path":["bus-evil"],` +
		`"sent_at":"2026-08-07T11:22:33Z","size":1,"content_sha256":"` + strings.Repeat("ab", 32) + `","prepare_index":1}`
	writeV1Log(t, path, KindAudit, v1Record{Index: 1, Type: TypeAuditMessage, Payload: []byte(payload)})

	var buf bytes.Buffer
	l2, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open with a forged version 1 audit file: %v\n--- log ---\n%s\nrecovery must still reach a running server", err, buf.String())
	}
	// What recovery REPORTS must match what it did. A quarantine that comes back
	// as "discarded_records=0" reads to an operator as "nothing was lost".
	ar := l2.Recovered().AuditRepaired
	if ar.Quarantined == "" || !ar.LostUnidentified || ar.NextIndex != 1 || ar.DiscardCount != 1 || ar.DiscardedBytes == 0 {
		t.Errorf("AuditRepaired = %+v, want a quarantine reported in the same shape recovery uses everywhere else: Quarantined set, LostUnidentified, NextIndex 1, DiscardCount 1, DiscardedBytes non-zero",
			ar)
	}
	if _, err := l2.Write(auditEntry(2, `{"x":2}`)); err != nil {
		t.Fatalf("Write after the forgery was rejected: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, _ := readAuditLog(t, path)
	for _, a := range got {
		if strings.Contains(a.MessageID, "FORGED") || strings.Contains(a.Sender, "attacker") {
			t.Fatalf("the audit trail holds %+v: a record an attacker wrote WITHOUT the MAC key must never end up signed with it. A version 1 frame is CRC32C-authenticated, which is to say authenticated by nobody",
				a)
		}
	}
	// Quarantined, not deleted: the operator is owed the bytes even when this
	// code will make nothing of them.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	quarantined := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), AuditFileName+".") {
			quarantined = true
		}
	}
	if !quarantined {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the forged file was not renamed aside; the directory holds %v. Recovery renames, it does not delete", names)
	}
	if !strings.Contains(buf.String(), "quarantined") {
		t.Errorf("the operator log does not mention the quarantine.\n--- log ---\n%s", buf.String())
	}
}

// TestAuditLogAnnouncesAMissingTrail covers the one loss that is not a discard
// and therefore fired nothing: bus.audit deleted outright.
//
// The security gate found recovery entirely silent -- nothing to repair, a
// zero-valued Repair, and the trail restarting at index 1 as though it had never
// existed. Silent loss is the defect invariant 6 rates P0, so it is announced.
func TestAuditLogAnnouncesAMissingTrail(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := uint64(1); i <= 3; i++ {
		if _, err := l.Write(auditEntry(i, `{"x":1}`)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, AuditFileName)); err != nil {
		t.Fatalf("removing the trail: %v", err)
	}

	var buf bytes.Buffer
	l2, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open with the trail deleted: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	logText := buf.String()
	if !strings.Contains(logText, "no append-only message audit log") {
		t.Fatalf("recovery said nothing about the missing audit trail. A file that is gone is not a 'discard', so nothing else in recovery reports it -- and silence is exactly the failure invariant 6 rates P0.\n--- log ---\n%s", logText)
	}
	if !strings.Contains(logText, AuditFileName) {
		t.Errorf("the line does not name %s.\n--- log ---\n%s", AuditFileName, logText)
	}

	// A FRESH data directory must NOT produce the line: a bus starting for the
	// first time has no WAL records and has lost nothing.
	fresh := t.TempDir()
	var freshBuf bytes.Buffer
	l3, err := Open(LogOptions{Dir: fresh, Logger: logging.New(&freshBuf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open on a fresh directory: %v", err)
	}
	if err := l3.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Contains(freshBuf.String(), "no append-only message audit log") {
		t.Fatalf("a first-ever start reported a missing audit trail; a channel that cries wolf is one operators learn to ignore.\n--- log ---\n%s", freshBuf.String())
	}
}

// TestAuditLogRefusesEarlyWhenTheTrailIsPoisoned covers the amplification the
// security gate measured: once the audit writer's poison has latched, every
// later message failed in Commit, costing a PREPARE and an ABORT -- two fsynced
// WAL appends and an ERROR line -- per attempt. Twenty retries grew the WAL by
// 4714 bytes and 40 fsyncs while never giving a different answer.
//
// A client that retries is a client doing the right thing, so the answer has to
// be cheap: the refusal now happens in Begin, before anything is written.
func TestAuditLogRefusesEarlyWhenTheTrailIsPoisoned(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir, Now: func() time.Time { return auditTestTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.Write(auditEntry(1, `{"x":1}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Poison the audit writer the way a real I/O failure does: close the
	// descriptor out from under it, then let one append fail.
	if err := l.audit.f.Close(); err != nil {
		t.Fatalf("closing the audit descriptor: %v", err)
	}
	if _, err := l.Write(auditEntry(2, `{"x":2}`)); err == nil {
		t.Fatalf("a write to a broken audit log succeeded")
	}
	if l.audit.poisonErr() == nil {
		t.Fatalf("the audit writer did not poison after a failed append")
	}

	walSize := l.w.Size()
	const retries = 20
	for i := 0; i < retries; i++ {
		_, err := l.Write(auditEntry(3, `{"x":3}`))
		if err == nil {
			t.Fatalf("retry %d succeeded against a poisoned audit log", i)
		}
		if !errors.Is(err, ErrPoisoned) {
			t.Fatalf("retry %d: err = %v, want one matching ErrPoisoned", i, err)
		}
	}
	if grew := l.w.Size() - walSize; grew != 0 {
		t.Fatalf("%d retries against a poisoned audit log grew the WAL by %d bytes; a refusal that is known in advance must cost nothing on disk", retries, grew)
	}

	// A NON-message entry still gets through: the audit log is unwritable, not
	// the WAL, and taking the roster down with the trail would lose work that has
	// nothing to do with it.
	if _, err := l.Write(Entry{Kind: "agent", Body: json.RawMessage(`{"agent_id":"bus-alpha.worker-9"}`)}); err != nil {
		t.Fatalf("a non-message entry was refused because the AUDIT log is poisoned: %v", err)
	}
}

// TestAuditLogTruncationSweepAlwaysStarts cuts the audit file at EVERY byte
// offset and asserts two things at each one: the bus STARTS, and what survives
// is a PREFIX of what was written.
//
// A hand-picked offset proves the offsets someone thought of. This proves the
// ones nobody did -- every point inside every length field, index, type, MAC and
// payload, plus the file header and the empty file.
func TestAuditLogTruncationSweepAlwaysStarts(t *testing.T) {
	pristine := t.TempDir()
	l, err := Open(LogOptions{Dir: pristine, Now: func() time.Time { return auditTestTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const n = 4
	for i := uint64(1); i <= n; i++ {
		// Bodies of different lengths so frames do not all start at the same
		// offset modulo anything.
		if _, err := l.Write(auditEntry(i, `{"pad":"`+strings.Repeat("y", int(i)*7)+`"}`)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	snap := snapshotDataDir(t, pristine)
	full := snap[AuditFileName]
	allIDs := []string{"bus-alpha-1", "bus-alpha-2", "bus-alpha-3", "bus-alpha-4"}

	// Every offset gets its own data directory -- a sweep that damaged one
	// shared file in place would be testing the accumulation of its own damage
	// -- and the offsets are worked through a bounded pool, because each one
	// costs several fsyncs and a serial sweep doubles the package's test time
	// for no extra coverage. This is the same shape as sweepTruncations in
	// indexfloor_auth_test.go; assertions happen back on the test goroutine,
	// since t.Fatalf from a worker is not allowed.
	type outcome struct {
		ids []string
		err string
	}
	offsets := len(full) + 1
	results := make([]outcome, offsets)
	base := t.TempDir()

	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}
	next := int64(-1)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				cut := int(atomic.AddInt64(&next, 1))
				if cut >= offsets {
					return
				}
				dir := filepath.Join(base, strconv.Itoa(cut))
				cutSnap := map[string][]byte{}
				for name, b := range snap {
					if name == AuditFileName {
						b = b[:cut]
					}
					cutSnap[name] = b
				}
				if err := restoreDataDir(cutSnap, dir); err != nil {
					results[cut] = outcome{err: "restoring the fixture: " + err.Error()}
					continue
				}
				l2, err := Open(LogOptions{Dir: dir})
				if err != nil {
					results[cut] = outcome{err: "Open: " + err.Error() +
						"\nrecovery must ALWAYS reach a running server, at every offset"}
					continue
				}
				if cerr := l2.Close(); cerr != nil {
					results[cut] = outcome{err: "Close: " + cerr.Error()}
					continue
				}
				ids, derr := auditIDsOnDisk(filepath.Join(dir, AuditFileName))
				if derr != nil {
					results[cut] = outcome{err: "reading the recovered audit log: " + derr.Error()}
					continue
				}
				results[cut] = outcome{ids: ids}
				_ = os.RemoveAll(dir)
			}
		}()
	}
	wg.Wait()

	for cut, r := range results {
		if r.err != "" {
			t.Fatalf("cut %d: %s", cut, r.err)
		}
		if len(r.ids) > n {
			t.Fatalf("cut %d: recovered %v, which is more than was ever written", cut, r.ids)
		}
		if !reflect.DeepEqual(r.ids, allIDs[:len(r.ids)]) {
			t.Fatalf("cut %d: recovered %v, want a PREFIX of %v: recovery may lose the tail, never reorder or resurrect", cut, r.ids, allIDs)
		}
	}
}

// auditIDsOnDisk is readAuditLog without a *testing.T, for use from the sweep's
// worker goroutines. It returns an error rather than failing the test, because
// t.Fatalf outside the test goroutine is not allowed.
func auditIDsOnDisk(path string) ([]string, error) {
	recs, _, err := ScanAll(path, KindAudit)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(recs))
	for _, rec := range recs {
		a, _, err := DecodeAudit(path, rec)
		if err != nil {
			return nil, err
		}
		ids = append(ids, a.MessageID)
	}
	return ids, nil
}
