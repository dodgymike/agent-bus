package auth_test

// AUTH-3, part 3: SUFFIX FLOORS.
//
// The point of EnrolmentSuffixesInWAL is that it sees suffixes NO REPLAY CAN SHOW IT: a
// prepare that was fsynced and never committed burned its number, and wal.Replay
// exposes committed entries only. A floors test that observes only COMMITTED
// prepares therefore proves nothing at all — it would pass just as happily
// against the wrong derivation (folding the replayed roster), which is the exact
// mistake the ids.NameSuffixes doc warns about and the reason this function
// exists.
//
// So the headline test here leaves a prepare DANGLING and asserts the floor
// rose anyway, and then carries the map through ResumeNameSuffixes -> Seal ->
// Mint to prove end to end that the burned number is never re-issued.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// mintOverFloors is a MEASURING INSTRUMENT, not a model of the startup wiring.
//
// It answers exactly one question: "if these numbers were a floor, what would
// the next mint of name be?" — which is how the tests below express "the suffix
// was seen" and "the suffix was missed" in the units that actually matter
// (an agent id), instead of asserting on a bare integer.
//
// IT MUST NOT BE READ AS THE PRODUCTION WIRING, and an earlier version of this
// comment ("performs the startup wiring the floors exist for") said exactly the
// wrong thing. ResumeNameSuffixes + Seal over the output of
// EnrolmentSuffixesInWAL is the combination floors.go explicitly forbids — that
// map is a strict SUBSET of the agent ids on disk, so sealing it re-mints live
// ids. Production builds its allocator with ids.OpenNameSuffixes, which writes
// each floor AHEAD instead of deriving anything. Nothing outside this test file
// may copy this function.
func mintOverFloors(t *testing.T, floors map[string]uint64, name string) string {
	t.Helper()
	alloc := ids.ResumeNameSuffixes(floors)
	if err := alloc.Seal(); err != nil {
		t.Fatalf("sealing the resumed allocator: %v", err)
	}
	m, err := ids.NewAgentIDMinter(testBusID, alloc)
	if err != nil {
		t.Fatalf("building the minter: %v", err)
	}
	id, err := m.Mint(name)
	if err != nil {
		t.Fatalf("minting %q: %v", name, err)
	}
	return id
}

// putEncoded writes one enrolment record straight through the log, bypassing
// WALRoster.Put. It is how a test constructs records Put would refuse to make
// (a foreign bus id, a malformed body, a duplicate).
func putEncoded(t *testing.T, l *wal.Log, body json.RawMessage) {
	t.Helper()
	if _, err := l.Write(wal.Entry{Kind: auth.RecordKind, Body: body}); err != nil {
		t.Fatalf("wal.Write: %v", err)
	}
}

// TestSuffixFloorsSeesADanglingPrepare is the test that actually matters.
//
// worker-1 is committed through the real durable path. worker-9's PREPARE is
// fsynced and then LEFT UNRESOLVED — the exact shape a crash between prepare and
// commit leaves behind, and the shape no replay can show. The floor must be 9,
// and the next mint must be 10.
//
// Note the log is deliberately not Closed while the transaction is open: Begin
// holds the Log's transaction lock and Close waits for it, so closing here would
// wedge rather than leave a dangling prepare. The prepare is already fsynced, so
// the bytes EnrolmentSuffixesInWAL reads are on the platter regardless.
func TestSuffixFloorsSeesADanglingPrepare(t *testing.T) {
	dir := t.TempDir()
	r, l := openRoster(t, dir)

	if err := r.Put(baseEntry(t, "worker", 1)); err != nil {
		t.Fatalf("Put worker-1: %v", err)
	}

	dangling, err := auth.Encode(baseEntry(t, "worker", 9))
	if err != nil {
		t.Fatalf("Encode worker-9: %v", err)
	}
	txn, err := l.Begin(wal.Entry{Kind: auth.RecordKind, Body: dangling})
	if err != nil {
		t.Fatalf("Begin the dangling prepare: %v", err)
	}
	t.Cleanup(func() {
		// Resolved only AFTER the assertions, so the log can be closed without
		// deadlocking on the open transaction.
		_ = txn.Abort("test cleanup")
		_ = l.Close()
	})

	// The prepare must be invisible to replay — otherwise this test is not
	// exercising the case it claims to.
	roster2 := auth.NewWALRoster(nil)
	if _, err := wal.Replay(l.Path(), roster2.Apply); err != nil {
		t.Fatalf("Replay of the log with a dangling prepare: %v", err)
	}
	if _, ok := roster2.Get(mustAgentID(t, "worker", 9)); ok {
		t.Fatalf("worker-9 is visible to REPLAY; the fixture did not leave a dangling prepare, so nothing below is being tested")
	}

	floors, err := auth.EnrolmentSuffixesInWAL(l.Path(), testBusID)
	if err != nil {
		t.Fatalf("EnrolmentSuffixesInWAL: %v", err)
	}
	if got := floors["worker"]; got != 9 {
		t.Fatalf("EnrolmentSuffixesInWAL gave the name %q a floor of %d, want 9.\nThe DANGLING prepare burned suffix 9: the number is on disk and in the audit trail, and no committed roster entry mentions it. A floor of 1 here means the derivation is folding COMMITTED state, which re-mints a live agent id (invariant 1).", "worker", got)
	}

	// End to end: the burned number is never re-issued.
	if got, want := mintOverFloors(t, floors, "worker"), mustAgentID(t, "worker", 10); got != want {
		t.Fatalf("the next enrolment of %q minted %q, want %q; suffix 2 would hand a NEW agent holding a DIFFERENT keypair a number this bus has already written to disk", "worker", got, want)
	}
}

// TestSuffixFloorsCountsAnAbortedPrepare: an ABORT says the enrolment will never
// commit. It does NOT say the suffix was never written.
func TestSuffixFloorsCountsAnAbortedPrepare(t *testing.T) {
	dir := t.TempDir()
	r, l := openRoster(t, dir)

	if err := r.Put(baseEntry(t, "worker", 1)); err != nil {
		t.Fatalf("Put worker-1: %v", err)
	}
	body, err := auth.Encode(baseEntry(t, "worker", 5))
	if err != nil {
		t.Fatalf("Encode worker-5: %v", err)
	}
	txn, err := l.Begin(wal.Entry{Kind: auth.RecordKind, Body: body})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := txn.Abort("the enrolment failed after the prepare"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	floors, err := auth.EnrolmentSuffixesInWAL(walPath(dir), testBusID)
	if err != nil {
		t.Fatalf("EnrolmentSuffixesInWAL: %v", err)
	}
	if got := floors["worker"]; got != 5 {
		t.Fatalf("an ABORTED prepare left the floor for %q at %d, want 5; the abort resolves the transaction, it does not un-write the suffix", "worker", got)
	}
	if got, want := mintOverFloors(t, floors, "worker"), mustAgentID(t, "worker", 6); got != want {
		t.Fatalf("the next enrolment minted %q, want %q", got, want)
	}
}

// TestSuffixFloorsMissingWALFileIsNotAFailure: a fresh bus's FIRST BOOT must not
// be refused. "Does not exist" is the one error distinguished from every other,
// precisely so a permission denial can never be mistaken for an empty bus.
func TestSuffixFloorsMissingWALFileIsNotAFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), wal.WALFileName)

	floors, err := auth.EnrolmentSuffixesInWAL(path, testBusID)
	if err != nil {
		t.Fatalf("EnrolmentSuffixesInWAL on a bus with no log = %v, want a nil error: this is a fresh bus's first boot", err)
	}
	if floors == nil {
		t.Fatalf("EnrolmentSuffixesInWAL returned a NIL map for a missing log, want an empty non-nil one; the caller must be able to tell \"derived, and it is empty\" from \"not derived\"")
	}
	if len(floors) != 0 {
		t.Fatalf("EnrolmentSuffixesInWAL on a missing log returned %d floors, want 0", len(floors))
	}
	// An empty derivation is legitimately sealable, and the first agent is 1.
	if got, want := mintOverFloors(t, floors, "worker"), mustAgentID(t, "worker", 1); got != want {
		t.Fatalf("a fresh bus minted %q, want %q", got, want)
	}
}

// assertFloorsRefused is the security-critical assertion behind every
// unreadable-log case: an error, NOT the empty-map fresh-bus answer, and NOT
// os.ErrNotExist — "does not exist" is the ONLY error a caller may read as an
// empty bus and seal, and the map must be nil because a partial derivation
// seals exactly as cleanly as a complete one.
func assertFloorsRefused(t *testing.T, path string) {
	t.Helper()
	assertNotAPermissionTest(t, path)
	floors, err := auth.EnrolmentSuffixesInWAL(path, testBusID)
	if err == nil {
		t.Fatalf("EnrolmentSuffixesInWAL(%s) returned %v with NO error; a log this bus cannot read reported as an empty bus mints from 1 onto suffixes that are already on disk", path, floors)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("EnrolmentSuffixesInWAL(%s) reported an unreadable log as os.ErrNotExist (%v); \"does not exist\" is the ONLY error that may be read as an empty bus, and it is the one the caller is allowed to Seal", path, err)
	}
	if floors != nil {
		t.Fatalf("EnrolmentSuffixesInWAL(%s) returned a %d-entry map alongside its error, want nil; failure is TOTAL and there is no such thing as a usefully incomplete answer", path, len(floors))
	}
}

// assertNotAPermissionTest is what keeps TestSuffixFloorsUnreadableLogIsAnErrorForEveryUID
// honest about its own name.
//
// Every case there must fail for a STRUCTURAL reason — the object is a
// directory, a path component is not a directory, the header bytes are not a
// header — never because of the file mode. A mode-based refusal is precisely
// what root ignores (CAP_DAC_OVERRIDE), so if someone later "simplifies" one of
// these cases into a chmod the test would go back to being a no-op in the
// container, and it would do so silently. This assertion makes that edit fail
// instead: every path here that exists at all is readable by its owner, so the
// refusal cannot be coming from permissions.
func assertNotAPermissionTest(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		// The path may legitimately not resolve — that IS the case for the
		// parent-is-a-regular-file fixture, whose whole point is ENOTDIR.
		return
	}
	if info.Mode().Perm()&0o400 == 0 {
		t.Fatalf("%s has mode %v, which is unreadable by its owner; this test claims to hold for EVERY uid, and a refusal that comes from the file mode is exactly the one root (the Docker Compose default) ignores", path, info.Mode())
	}
}

// TestSuffixFloorsUnreadableLogIsAnErrorForEveryUID is the ROOT-SAFE form of
// the check below, and it is the one that matters.
//
// TestSuffixFloorsUnreadableFileIsAnError makes the log unreadable with
// chmod 0000, which root ignores — so it SKIPS at euid 0, and the deployment
// target for this project is Docker Compose, where the server runs as root by
// default. The single most security-relevant assertion in this file therefore
// never ran anywhere it mattered. None of the mechanisms here depend on the
// file mode, so all of them fail for root exactly as they do for anyone else.
func TestSuffixFloorsUnreadableLogIsAnErrorForEveryUID(t *testing.T) {
	t.Run("a DIRECTORY where the log file belongs", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), wal.WALFileName)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		assertFloorsRefused(t, path)
	})

	t.Run("a path whose PARENT component is a regular file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), wal.WALFileName)
		if err := os.WriteFile(parent, []byte("a regular file, not a directory"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", parent, err)
		}
		assertFloorsRefused(t, filepath.Join(parent, wal.WALFileName))
	})

	t.Run("a REAL log with enrolments whose header is then destroyed", func(t *testing.T) {
		// The closest root-safe analogue of the chmod case: the enrolment
		// bytes really are on the platter and really are unreadable, so a
		// silent empty map here would re-mint an id this bus has already
		// handed out.
		dir := t.TempDir()
		r, l := openRoster(t, dir)
		if err := r.Put(baseEntry(t, "worker", 3)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		path := l.Path()
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Sanity: intact, the derivation sees the suffix. Without this the
		// case below could pass against a log that was empty all along.
		floors, err := auth.EnrolmentSuffixesInWAL(path, testBusID)
		if err != nil || floors["worker"] != 3 {
			t.Fatalf("before the damage EnrolmentSuffixesInWAL = %v, %v; want a floor of 3 and no error", floors, err)
		}

		f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("opening %s to damage it: %v", path, err)
		}
		if _, err := f.WriteAt(bytes.Repeat([]byte{0xFF}, 16), 0); err != nil {
			f.Close()
			t.Fatalf("overwriting the file header: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("closing %s: %v", path, err)
		}

		assertFloorsRefused(t, path)
	})
}

// TestSuffixFloorsUnreadableFileIsAnError: an I/O failure is NOT an empty bus. A
// derivation that cannot read the log must fail loudly, because a floor map that
// missed a name seals exactly as cleanly as a complete one and then mints from 1
// onto suffixes already on disk.
//
// It is kept alongside TestSuffixFloorsUnreadableLogIsAnErrorForEveryUID because
// a permission denial is the realistic operational shape of this failure — but
// it is NOT the coverage, because it cannot run as root. See that test.
func TestSuffixFloorsUnreadableFileIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: root ignores the file mode, so chmod 0000 cannot make the log unreadable. The root-safe coverage is TestSuffixFloorsUnreadableLogIsAnErrorForEveryUID")
	}
	dir := t.TempDir()
	r, l := openRoster(t, dir)
	if err := r.Put(baseEntry(t, "worker", 3)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := l.Path()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatalf("chmod 0000 %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })

	floors, err := auth.EnrolmentSuffixesInWAL(path, testBusID)
	if err == nil {
		t.Fatalf("EnrolmentSuffixesInWAL on an unreadable log returned %v with NO error; a permission denial reported as an empty bus mints from 1 onto suffixes that are already on disk", floors)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("EnrolmentSuffixesInWAL reported an unreadable log as os.ErrNotExist (%v); \"does not exist\" is the ONLY error that may be read as an empty bus", err)
	}
	if floors != nil {
		t.Fatalf("EnrolmentSuffixesInWAL returned a %d-entry map alongside its error, want nil; failure is TOTAL and there is no such thing as a usefully incomplete answer", len(floors))
	}
}

// TestSuffixFloorsSkipsForeignBusIDs: the suffix space is per bus per name. A
// peer-roster record legitimately carries a foreign id, and folding it in would
// inflate a LOCAL floor from a REMOTE bus's history.
func TestSuffixFloorsSkipsForeignBusIDs(t *testing.T) {
	dir := t.TempDir()
	l := openPlainLog(t, dir)

	local := baseEntry(t, "worker", 2)
	localBody, err := auth.Encode(local)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	foreign := local
	foreignID, err := ids.AgentID("some-other-bus", "worker", 900)
	if err != nil {
		t.Fatalf("building the foreign id: %v", err)
	}
	foreign.AgentID = foreignID
	foreignBody, err := auth.Encode(foreign)
	if err != nil {
		t.Fatalf("Encode foreign: %v", err)
	}

	putEncoded(t, l, localBody)
	putEncoded(t, l, foreignBody)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	floors, err := auth.EnrolmentSuffixesInWAL(walPath(dir), testBusID)
	if err != nil {
		t.Fatalf("EnrolmentSuffixesInWAL: %v", err)
	}
	if got := floors["worker"]; got != 2 {
		t.Fatalf("the floor for %q is %d, want 2; a record from bus %q inflated a LOCAL floor", "worker", got, "some-other-bus")
	}
	if len(floors) != 1 {
		t.Fatalf("EnrolmentSuffixesInWAL returned %d names (%v), want just the local one", len(floors), floors)
	}
}

// TestSuffixFloorsRaisesTheFloorForAMalformedRecord pins the LENIENT-SCAN
// contract, which will look like a bug to a future reader: a record strict
// Decode REFUSES still raises the floor.
//
// It has to. Validation protects the ROSTER; the floor is a claim about BYTES
// THAT REACHED DISK. A record whose schema version this build does not
// understand, or which carries a field it has never heard of, STILL BURNED A
// SUFFIX, and refusing to see it is precisely how a floor ends up too low.
func TestSuffixFloorsRaisesTheFloorForAMalformedRecord(t *testing.T) {
	tests := []struct {
		name string
		body json.RawMessage
		want uint64
	}{
		{
			name: "a schema version this build refuses",
			body: json.RawMessage(`{"v":999,"agent_id":"` + testBusID + `.worker-7"}`),
			want: 7,
		},
		{
			name: "a field this build has never heard of",
			body: json.RawMessage(`{"v":1,"agent_id":"` + testBusID + `.worker-8","revoked":true}`),
			want: 8,
		},
		{
			name: "a cert_bindings field of a type this build cannot read",
			body: json.RawMessage(`{"v":1,"agent_id":"` + testBusID + `.worker-11","cert_bindings":"not even an array"}`),
			want: 11,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// The fixture must genuinely be a record strict Decode refuses,
			// otherwise this proves nothing about leniency.
			if _, err := auth.Decode(tc.body); !errors.Is(err, auth.ErrInvalidRecord) {
				t.Fatalf("the fixture decodes cleanly (%v); it is not the malformed record this test is about", err)
			}

			dir := t.TempDir()
			l := openPlainLog(t, dir)
			putEncoded(t, l, tc.body)
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			floors, err := auth.EnrolmentSuffixesInWAL(walPath(dir), testBusID)
			if err != nil {
				t.Fatalf("EnrolmentSuffixesInWAL over a malformed-but-readable record = %v, want the floor raised;\nthe floor scan is DELIBERATELY LENIENT because a record too damaged to enter the roster still burned its suffix", err)
			}
			if got := floors["worker"]; got != tc.want {
				t.Fatalf("the floor for %q is %d, want %d", "worker", got, tc.want)
			}

			// And the record is genuinely NOT on the roster: the enrolment is
			// lost, the id is not. That pairing is the whole design.
			r, l2 := openRoster(t, dir)
			defer l2.Close()
			if r.Len() != 0 {
				t.Fatalf("the roster holds %d agents, want 0: the record is undecodable and must be discarded", r.Len())
			}
		})
	}
}

// TestSuffixFloorsAgentRecordWithNoAgentIDFailsTotally: an enrolment record
// (its Kind said so) that names no id leaves the scan unable to say WHICH name's
// floor it should have raised. That is a MISSED NAME, the one failure the
// derivation may never paper over — a partial map seals exactly as cleanly as a
// complete one, and every missed name then mints from 1 onto suffixes already on
// disk.
//
// The assertion that the map is NIL is the load-bearing one: a partial map
// returned alongside an error is a map some caller will use.
func TestSuffixFloorsAgentRecordWithNoAgentIDFailsTotally(t *testing.T) {
	tests := []struct {
		name string
		body json.RawMessage
	}{
		{name: "no agent_id key at all", body: json.RawMessage(`{"v":1}`)},
		{name: "an empty agent_id", body: json.RawMessage(`{"v":1,"agent_id":""}`)},
		{name: "an unparseable agent_id", body: json.RawMessage(`{"v":1,"agent_id":"not-an-id"}`)},
		{name: "an agent_id that is not a string", body: json.RawMessage(`{"v":1,"agent_id":42}`)},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := openPlainLog(t, dir)
			// A perfectly good record FIRST, so a partial map would be non-empty
			// and the nil check below is a real check.
			good, err := auth.Encode(baseEntry(t, "worker", 3))
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			putEncoded(t, l, good)
			putEncoded(t, l, tc.body)
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			floors, err := auth.EnrolmentSuffixesInWAL(walPath(dir), testBusID)
			if err == nil {
				t.Fatalf("EnrolmentSuffixesInWAL returned %v with no error over an enrolment record that names no agent id", floors)
			}
			if floors != nil {
				t.Fatalf("EnrolmentSuffixesInWAL returned a PARTIAL map %v alongside its error, want nil; a partial derivation seals as cleanly as a complete one and then re-mints live ids", floors)
			}
		})
	}
}

// TestSuffixFloorsIgnoresRecordsOfAnotherKind pins the SCOPE of
// EnrolmentSuffixesInWAL: it reports enrolment records and nothing else.
//
// READ THE ASSERTION AS A LIMITATION, NOT AS A CORRECTNESS PROPERTY. An earlier
// version of this comment claimed records of another kind "burned no agent name
// suffix". That is FALSE and it is the exact claim the security gate blocked
// on: a store message record names its sender and its recipients, and those are
// agent ids whose suffixes are absolutely burned. The 4096 below is deliberately
// far above the enrolment record's 4 so that folding it in would be unmissable —
// and this function does not fold it in.
//
// That is why the result is not a floor and must never be sealed (see
// floors.go). It is also why deleting this function would be wrong: production's
// derivation in cmd/agent-bus/suffixfloors.go folds ONLY message records, so the
// two cover complementary halves and the union is what a correct floor needs.
// This test is what stops someone "fixing" the omission here in isolation and
// believing they had made the map sealable.
func TestSuffixFloorsIgnoresRecordsOfAnotherKind(t *testing.T) {
	dir := t.TempDir()
	l := openPlainLog(t, dir)

	good, err := auth.Encode(baseEntry(t, "worker", 4))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	putEncoded(t, l, good)
	if _, err := l.Write(wal.Entry{Kind: "message", Body: json.RawMessage(`{"agent_id":"` + testBusID + `.worker-4096"}`)}); err != nil {
		t.Fatalf("wal.Write of a message record: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	floors, err := auth.EnrolmentSuffixesInWAL(walPath(dir), testBusID)
	if err != nil {
		t.Fatalf("EnrolmentSuffixesInWAL: %v", err)
	}
	if got := floors["worker"]; got != 4 {
		t.Fatalf("the floor for %q is %d, want 4; a record of another kind was folded into an agent name's suffix space", "worker", got)
	}
}
