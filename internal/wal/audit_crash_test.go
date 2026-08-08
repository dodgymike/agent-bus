//go:build linux || darwin

package wal

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------------------
// DUR-5 crash injection.
//
// CLAUDE.md: durability and recovery code must have crash-injection tests -- a
// test that writes, kills at a chosen point in the write path, and asserts what
// recovery yields. "The code looks right" is not evidence for a durability
// claim, and neither is a simulation that still runs every defer on the way out.
//
// Both tests here re-exec the test binary and SIGKILL the child (see
// runCrashChild, which inspects the WAIT STATUS rather than the exit code, so a
// child that merely failed its own assertions cannot masquerade as a crash).
//
// Between them they pin the ONE property that makes the audit trail worth
// having, which is an asymmetry rather than an equality:
//
//	AUDIT TRAIL  ⊇  COMMITTED HISTORY
//
//	TestAuditLogCrashInsideApply         nothing that committed is MISSING from it
//	TestAuditLogCrashBetweenAuditAndCommit it may hold a record for a message that
//	                                       never committed -- and that is correct
//
// The asymmetry is the whole reason the audit append sits BETWEEN the prepare
// fsync and the commit fsync. Move it after the commit fsync and the first test
// still passes while the guarantee is gone: a crash in the new window leaves an
// acknowledged message with no trace in the trail.
// ---------------------------------------------------------------------------

// crashAuditEntries are the three audited messages a crash child writes.
var crashAuditEntries = []Entry{
	{Kind: "message", Body: json.RawMessage(`{"to":"bus-alpha.worker-2","n":1}`), Audit: sampleAudit(1)},
	{Kind: "message", Body: json.RawMessage(`{"to":"bus-alpha.worker-2","n":2,"pad":"xxxxxxxxxxxx"}`), Audit: sampleAudit(2)},
	{Kind: "message", Body: json.RawMessage(`{"to":"bus-alpha.worker-2","n":3}`), Audit: sampleAudit(3)},
}

// TestAuditLogCrashInsideApply kills the process inside the third Apply, at
// which point the third message's prepare, audit record and commit record have
// all been fsynced and the caller has been told nothing.
//
// Recovery must therefore find all three messages in the WAL -- an entry whose
// commit record is durable IS accepted history, acknowledged or not (invariant
// 4) -- and all three in the audit trail. A trail that is missing the last one
// would mean an acknowledged message could exist with no provenance record,
// which is exactly what invariant 6 exists to prevent.
func TestAuditLogCrashInsideApply(t *testing.T) {
	dir := t.TempDir()
	runCrashChild(t, crashAuditInsideApply, dir)

	got, r := replayDir(t, dir)
	if len(got) != len(crashAuditEntries) {
		t.Fatalf("replay delivered %d entries, want %d: a commit record that completed its fsync is accepted history even though the process died before Apply",
			len(got), len(crashAuditEntries))
	}
	if r.Applied != uint64(len(crashAuditEntries)) || len(r.Dangling) != 0 {
		t.Fatalf("Applied = %d, Dangling = %v, want %d and none", r.Applied, r.Dangling, len(crashAuditEntries))
	}
	assertIndicesUnique(t, filepath.Join(dir, WALFileName))

	audit, prepares := readAuditLog(t, filepath.Join(dir, AuditFileName))
	want := []string{"bus-alpha-1", "bus-alpha-2", "bus-alpha-3"}
	if ids := auditMessageIDs(audit); !reflect.DeepEqual(ids, want) {
		t.Fatalf("audit trail = %v, want %v: every message that reached commit must be in the trail", ids, want)
	}
	// The trail names the WAL transaction each message was written in, and the
	// prepare indices it names are the ones replay actually paired.
	for i := range got {
		if prepares[i] != got[i].PrepareIndex {
			t.Errorf("audit record %d names prepare %d, but the WAL committed it as prepare %d",
				i, prepares[i], got[i].PrepareIndex)
		}
	}
}

// TestAuditLogCrashBetweenAuditAndCommit is the superset direction.
//
// The child dies with the third message's PREPARE and AUDIT records fsynced and
// its COMMIT record never written. Recovery must:
//
//   - DISCARD the third message from the WAL. It was never acknowledged, so it
//     is not accepted history, and making it visible would be invariant 4's
//     failure in the other direction.
//   - KEEP the third audit record. The trail is allowed to over-report.
//   - BURN the prepare's index either way (invariant 1).
//
// What this does NOT prove: that a signal can be delivered inside the few
// statements between the two fsyncs. The child reproduces that byte state with
// the real writers and the real encoder (see crashAuditBeforeCommit). What the
// kill does prove is that nothing tidied the files up on the way out.
func TestAuditLogCrashBetweenAuditAndCommit(t *testing.T) {
	dir := t.TempDir()
	runCrashChild(t, crashAuditBeforeCommit, dir)

	got, r := replayDir(t, dir)
	if len(got) != 2 {
		t.Fatalf("replay delivered %d entries, want 2: the third message never committed and must not be visible", len(got))
	}
	// Records 1-4 are the two committed pairs; 5 is the orphaned prepare.
	const orphan = 5
	if !reflect.DeepEqual(r.Dangling, []uint64{orphan}) {
		t.Fatalf("Dangling = %v, want [%d]: the crash left exactly one unresolved prepare", r.Dangling, orphan)
	}
	if r.NextIndex != orphan+1 {
		t.Fatalf("NextIndex = %d, want %d: a discarded prepare still burns its index", r.NextIndex, orphan+1)
	}

	audit, prepares := readAuditLog(t, filepath.Join(dir, AuditFileName))
	want := []string{"bus-alpha-1", "bus-alpha-2", "bus-alpha-3"}
	if ids := auditMessageIDs(audit); !reflect.DeepEqual(ids, want) {
		t.Fatalf("audit trail = %v, want %v: the audit log is a SUPERSET of committed history -- the record for the message that never committed stays, and losing it would mean the trail can under-report",
			ids, want)
	}
	// And the trail says which WAL transaction it belonged to, which is what
	// lets an fsck report "audit record 3 names prepare 5, which never
	// committed" rather than leaving an operator to guess.
	if prepares[2] != orphan {
		t.Fatalf("the third audit record names prepare %d, want %d (the orphaned prepare)", prepares[2], orphan)
	}

	// The bus starts and keeps going, and the audit trail continues to append.
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open after the crash: %v", err)
	}
	if _, err := l.Write(auditEntry(4, `{"after":"recovery"}`)); err != nil {
		t.Fatalf("Write after the crash: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	audit, _ = readAuditLog(t, filepath.Join(dir, AuditFileName))
	if ids := auditMessageIDs(audit); !reflect.DeepEqual(ids, append(want, "bus-alpha-4")) {
		t.Fatalf("audit trail after recovery = %v, want %v", ids, append(want, "bus-alpha-4"))
	}
}
