package main

// Unit tests for MSG-FU-SUFFIXFLOOR's startup half: the WAL derivation
// (walAgentIDFloors) and the allocator construction (openSuffixAllocator).
//
// The BEHAVIOURAL proof -- a real server, restarted, minting a strictly greater
// suffix for a re-enrolled name -- lives in suffixrestart_test.go. These tests
// cover the paths a running server cannot easily be driven into: a corrupt
// floors file, a seal that cannot write, and a WAL whose records this scan must
// or must not fold.

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// testBusID is a syntactically valid bus id used where the test needs to know
// the id before anything mints one.
const testBusID = "bus-testsuffixfloor"

// idsImportPath is the package TestNoFreshSuffixCounterInCmd resolves import
// names against, so an alias or a dot-import cannot slip the guard.
const idsImportPath = "github.com/dodgymike/agent-bus/internal/ids"

// quietLogger is a logger that discards everything, for the unit tests that
// exercise openSuffixAllocator without wanting its output.
func quietLogger(t *testing.T) *logging.Logger {
	t.Helper()
	return logging.New(io.Discard, logging.LevelError)
}

// logBuf is a logger sink the test can inspect, so a log line that is part of
// the CONTRACT (the graded seal line, the repaired-log exposure) is asserted --
// at its LEVEL, by giving the logger a minimum level and requiring output --
// rather than assumed.
type logBuf struct{ b strings.Builder }

func (l *logBuf) Write(p []byte) (int, error) { return l.b.Write(p) }
func (l *logBuf) String() string              { return l.b.String() }

// openTestWAL opens a WAL in dir for the test to write fixture records into.
func openTestWAL(t *testing.T, dir string) *wal.Log {
	t.Helper()
	lg := quietLogger(t)
	l, err := wal.Open(wal.LogOptions{Dir: dir, Logger: lg})
	if err != nil {
		t.Fatalf("wal.Open(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// writeMessageRecord commits one store message record naming sender and
// recipients -- the shape a legacy data dir is full of.
func writeMessageRecord(t *testing.T, l *wal.Log, busID, sender string, recipients []string, seq uint64) {
	t.Helper()
	broadcast := len(recipients) == 0
	m, err := store.NewMessage(busID, sender, broadcast, recipients, seq, time.Unix(0, 0).UTC(), []byte("hello"), fmt.Sprintf("idem-%d", seq))
	if err != nil {
		t.Fatalf("store.NewMessage: %v", err)
	}
	body, err := m.Encode()
	if err != nil {
		t.Fatalf("encoding message: %v", err)
	}
	if _, err := l.Write(wal.Entry{Kind: store.RecordKind, Body: body}); err != nil {
		t.Fatalf("writing message record: %v", err)
	}
}

// writeRawRecord commits an entry with an arbitrary kind and body, for the
// cases a validated constructor cannot produce.
func writeRawRecord(t *testing.T, l *wal.Log, kind string, body string) {
	t.Helper()
	if _, err := l.Write(wal.Entry{Kind: kind, Body: json.RawMessage(body)}); err != nil {
		t.Fatalf("writing %s record: %v", kind, err)
	}
}

func TestWALAgentIDFloors(t *testing.T) {
	t.Run("a missing log is not a failure", func(t *testing.T) {
		// A fresh bus's first start MUST NOT be a refusal to boot. Note this
		// also guards the errors.Is(err, os.ErrNotExist) vs os.IsNotExist trap:
		// wal.ScanAll wraps its open error with %w and the legacy predicate does
		// not unwrap.
		floors, err := walAgentIDFloors(filepath.Join(t.TempDir(), "bus.wal"), testBusID)
		if err != nil {
			t.Fatalf("walAgentIDFloors on a missing log = %v, want nil", err)
		}
		if floors == nil {
			t.Fatal("walAgentIDFloors returned a nil map for a missing log; the caller must be able to tell 'derived, empty' from 'not derived'")
		}
		if len(floors) != 0 {
			t.Fatalf("floors = %v, want empty", floors)
		}
	})

	t.Run("an empty log derives nothing", func(t *testing.T) {
		dir := t.TempDir()
		l := openTestWAL(t, dir)
		floors, err := walAgentIDFloors(l.Path(), testBusID)
		if err != nil {
			t.Fatalf("walAgentIDFloors: %v", err)
		}
		if len(floors) != 0 {
			t.Fatalf("floors = %v, want empty", floors)
		}
	})

	// Table-driven over the record shapes that matter.
	tests := []struct {
		name    string
		write   func(t *testing.T, l *wal.Log)
		want    map[string]uint64
		wantErr string // substring; empty means success expected
	}{
		{
			name: "the sender raises its name's floor",
			write: func(t *testing.T, l *wal.Log) {
				writeMessageRecord(t, l, testBusID, testBusID+".alpha-7", []string{testBusID + ".beta-2"}, 1)
			},
			want: map[string]uint64{"alpha": 7, "beta": 2},
		},
		{
			name: "recipients raise their names' floors too",
			write: func(t *testing.T, l *wal.Log) {
				writeMessageRecord(t, l, testBusID, testBusID+".alpha-1", []string{testBusID + ".beta-9", testBusID + ".gamma-3"}, 1)
			},
			want: map[string]uint64{"alpha": 1, "beta": 9, "gamma": 3},
		},
		{
			name: "the maximum wins, in either record order",
			write: func(t *testing.T, l *wal.Log) {
				writeMessageRecord(t, l, testBusID, testBusID+".alpha-5", []string{testBusID + ".beta-1"}, 1)
				writeMessageRecord(t, l, testBusID, testBusID+".alpha-2", []string{testBusID + ".beta-1"}, 2)
				writeMessageRecord(t, l, testBusID, testBusID+".alpha-3", []string{testBusID + ".beta-1"}, 3)
			},
			want: map[string]uint64{"alpha": 5, "beta": 1},
		},
		{
			name: "a broadcast still raises its SENDER's floor",
			write: func(t *testing.T, l *wal.Log) {
				writeMessageRecord(t, l, testBusID, testBusID+".alpha-4", nil, 1)
			},
			want: map[string]uint64{"alpha": 4},
		},
		{
			name: "a foreign bus id burns no local suffix",
			write: func(t *testing.T, l *wal.Log) {
				writeMessageRecord(t, l, testBusID, "bus-elsewhere.alpha-9000", []string{testBusID + ".beta-2"}, 1)
			},
			want: map[string]uint64{"beta": 2},
		},
		{
			name: "records of another kind are skipped",
			write: func(t *testing.T, l *wal.Log) {
				writeRawRecord(t, l, "agent", `{"agent_id":"`+testBusID+`.alpha-99"}`)
				writeMessageRecord(t, l, testBusID, testBusID+".alpha-1", []string{testBusID + ".beta-1"}, 1)
			},
			// The enrolment record is NOT folded here: this scan's population is
			// the message records. Its suffix is covered by the floors FILE from
			// the first start of this binary onward.
			want: map[string]uint64{"alpha": 1, "beta": 1},
		},
		{
			name: "an ABORTED prepare still counts: the suffix reached disk",
			write: func(t *testing.T, l *wal.Log) {
				m, err := store.NewMessage(testBusID, testBusID+".alpha-6", true, nil, 1, time.Unix(0, 0).UTC(), []byte("x"), "idem-abort")
				if err != nil {
					t.Fatalf("store.NewMessage: %v", err)
				}
				body, err := m.Encode()
				if err != nil {
					t.Fatalf("encoding: %v", err)
				}
				tx, err := l.Begin(wal.Entry{Kind: store.RecordKind, Body: body})
				if err != nil {
					t.Fatalf("Begin: %v", err)
				}
				if err := tx.Abort("test"); err != nil {
					t.Fatalf("Abort: %v", err)
				}
			},
			want: map[string]uint64{"alpha": 6},
		},
		{
			name: "a message record with no sender is a TOTAL failure",
			write: func(t *testing.T, l *wal.Log) {
				writeRawRecord(t, l, store.RecordKind, `{"recipients":["`+testBusID+`.beta-2"]}`)
			},
			wantErr: "carries no sender",
		},
		{
			name: "an unparseable agent id is a TOTAL failure",
			write: func(t *testing.T, l *wal.Log) {
				writeRawRecord(t, l, store.RecordKind, `{"sender":"not an agent id"}`)
			},
			wantErr: "unparseable agent id",
		},
		{
			name: "a record whose CONTENT HASH is wrong still raises the floor",
			write: func(t *testing.T, l *wal.Log) {
				// store.Decode would reject this; the floor derivation must not.
				// The suffix reached disk either way.
				writeRawRecord(t, l, store.RecordKind, `{"v":1,"sender":"`+testBusID+`.alpha-8","broadcast":true,"content_sha256":"deadbeef","size":99,"body":"aGk="}`)
			},
			want: map[string]uint64{"alpha": 8},
		},
		{
			name: "a record from a NEWER build with unknown fields still raises the floor",
			write: func(t *testing.T, l *wal.Log) {
				writeRawRecord(t, l, store.RecordKind, `{"v":42,"sender":"`+testBusID+`.alpha-11","envelope":{"scheme":"future"}}`)
			},
			want: map[string]uint64{"alpha": 11},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := openTestWAL(t, dir)
			tc.write(t, l)

			got, err := walAgentIDFloors(l.Path(), testBusID)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("walAgentIDFloors = %v, nil; want an error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				if got != nil {
					t.Fatalf("a FAILED derivation returned a partial map %v; failure must be total, because a missed name mints from 1 over ids already on disk", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("walAgentIDFloors: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("floors = %v, want %v", got, tc.want)
			}
			for name, n := range tc.want {
				if got[name] != n {
					t.Fatalf("floors[%q] = %d, want %d (full map %v)", name, got[name], n, got)
				}
			}
		})
	}
}

// TestOpenSuffixAllocatorBackfillsALegacyDataDir is the legacy-dir proof at the
// unit level: a WAL holding agent ids and NO agent-suffixes file must not
// re-mint those ids.
func TestOpenSuffixAllocatorBackfillsALegacyDataDir(t *testing.T) {
	dir := t.TempDir()
	busID, err := ids.LoadOrCreateBusID(dir, testBusID)
	if err != nil {
		t.Fatalf("LoadOrCreateBusID: %v", err)
	}
	l := openTestWAL(t, dir)
	writeMessageRecord(t, l, busID, busID+".alpha-5", []string{busID + ".beta-2"}, 1)

	// Precondition: this is a LEGACY dir -- ids on disk, no floors file.
	if _, err := os.Stat(filepath.Join(dir, "agent-suffixes")); !os.IsNotExist(err) {
		t.Fatalf("agent-suffixes must not exist before the first openSuffixAllocator; stat err = %v", err)
	}

	alloc, err := openSuffixAllocator(dir, l, busID, false, quietLogger(t))
	if err != nil {
		t.Fatalf("openSuffixAllocator on a legacy dir: %v", err)
	}
	for name, wantAbove := range map[string]uint64{"alpha": 5, "beta": 2} {
		n, err := alloc.NextSuffix(name)
		if err != nil {
			t.Fatalf("NextSuffix(%q): %v", name, err)
		}
		if n <= wantAbove {
			t.Fatalf("NextSuffix(%q) = %d, want strictly greater than %d: the id %s.%s-%d is already durable in the log and re-minting it hands a new keypair a previous agent's identity", name, n, wantAbove, busID, name, wantAbove)
		}
	}
	// A name with no history still mints from 1: the seal asserted that names
	// absent from the map were never written.
	if n, err := alloc.NextSuffix("delta"); err != nil || n != 1 {
		t.Fatalf("NextSuffix(\"delta\") = %d, %v; want 1, nil", n, err)
	}
	// And the backfill was PERSISTED, not just held in memory.
	if _, err := os.Stat(filepath.Join(dir, "agent-suffixes")); err != nil {
		t.Fatalf("agent-suffixes must exist after Seal: %v", err)
	}
}

// TestOpenSuffixAllocatorShoutsWhenTheFloorsFileIsMissingOnADirWithHistory is
// the security gate's M2, made into a regression guard.
//
// Delete ONLY agent-suffixes from a live data dir and the bus resumes every name
// whose suffix no durable record happens to name -- the floors file was the only
// thing that knew. It cannot be a refusal to boot (a legacy dir looks identical,
// so refusing would block the migration and hand anyone with data-dir write
// access a permanent boot-denial primitive), so the requirement is that it is
// LOUD: ERROR when the dir has history, WARN when it does not. It went out at a
// bland INFO before this test existed.
func TestOpenSuffixAllocatorShoutsWhenTheFloorsFileIsMissingOnADirWithHistory(t *testing.T) {
	t.Run("a dir WITH history logs at ERROR", func(t *testing.T) {
		dir := t.TempDir()
		busID, err := ids.LoadOrCreateBusID(dir, testBusID)
		if err != nil {
			t.Fatalf("LoadOrCreateBusID: %v", err)
		}
		l := openTestWAL(t, dir)
		writeMessageRecord(t, l, busID, busID+".alpha-2", nil, 1)
		// REOPENED, deliberately: wal.Log.Recovered() describes what replay saw
		// at Open, so a record written after Open is not in it. Production always
		// opens a log that already holds its history; a test that did not reopen
		// would be asserting against a Recovered() that is empty for a reason
		// that never occurs in production.
		if err := l.Close(); err != nil {
			t.Fatalf("closing the fixture WAL: %v", err)
		}
		l = openTestWAL(t, dir)
		if l.Recovered().Records == 0 {
			t.Fatal("the reopened log reports no records; the fixture proves nothing")
		}

		// First start writes a correct floors file.
		if _, err := openSuffixAllocator(dir, l, busID, false, quietLogger(t)); err != nil {
			t.Fatalf("first openSuffixAllocator: %v", err)
		}
		if err := os.Remove(filepath.Join(dir, "agent-suffixes")); err != nil {
			t.Fatalf("removing the floors file: %v", err)
		}

		// LevelError, so a line that is merely INFO or WARN produces NOTHING here
		// and the assertion below fails -- which is the whole point.
		var buf logBuf
		alloc, err := openSuffixAllocator(dir, l, busID, false, logging.New(&buf, logging.LevelError))
		if err != nil {
			t.Fatalf("openSuffixAllocator with a missing floors file: %v", err)
		}
		if !strings.Contains(buf.String(), "WITHOUT a persisted floors file") {
			t.Fatalf("a missing floors file on a dir WITH history must be logged at ERROR; the log at level=error was:\n%q", buf.String())
		}
		// What the log DOES name is still recovered, so the loss is bounded.
		if n, err := alloc.NextSuffix("alpha"); err != nil || n <= 2 {
			t.Fatalf("NextSuffix(\"alpha\") = %d, %v; want strictly greater than 2: %s.alpha-2 is still durable in the log", n, err, busID)
		}
	})

	t.Run("a fresh dir logs at WARN, not ERROR", func(t *testing.T) {
		dir := t.TempDir()
		l := openTestWAL(t, dir)

		var errBuf logBuf
		if _, err := openSuffixAllocator(dir, l, testBusID, true, logging.New(&errBuf, logging.LevelError)); err != nil {
			t.Fatalf("openSuffixAllocator on a fresh dir: %v", err)
		}
		if errBuf.String() != "" {
			t.Fatalf("a genuinely fresh data dir must not log at ERROR; got:\n%s", errBuf.String())
		}

		dir2 := t.TempDir()
		l2 := openTestWAL(t, dir2)
		var warnBuf logBuf
		if _, err := openSuffixAllocator(dir2, l2, testBusID, true, logging.New(&warnBuf, logging.LevelWarn)); err != nil {
			t.Fatalf("openSuffixAllocator on a fresh dir: %v", err)
		}
		if !strings.Contains(warnBuf.String(), "EMPTY at startup and had no persisted floors file") {
			t.Fatalf("a first start with no floors file must still say so at WARN; got:\n%s", warnBuf.String())
		}
	})

	t.Run("the steady state is quiet", func(t *testing.T) {
		dir := t.TempDir()
		l := openTestWAL(t, dir)
		if _, err := openSuffixAllocator(dir, l, testBusID, false, quietLogger(t)); err != nil {
			t.Fatalf("first openSuffixAllocator: %v", err)
		}
		var buf logBuf
		if _, err := openSuffixAllocator(dir, l, testBusID, false, logging.New(&buf, logging.LevelWarn)); err != nil {
			t.Fatalf("second openSuffixAllocator: %v", err)
		}
		if buf.String() != "" {
			t.Fatalf("an ordinary start with a present floors file must be quiet above INFO, or the loud lines above become noise nobody reads; got:\n%s", buf.String())
		}
	})
}

// TestOpenSuffixAllocatorDoesNotReadTheLogWhenFloorsExist is the DIRECT guard on
// the !Existed() gate, and it is direct on purpose.
//
// The gate exists because wal.ScanAll accumulates every record INCLUDING FULL
// PAYLOADS and the WAL never compacts, so scanning on every start puts peak
// startup memory in proportion to the whole log -- forever, with no compaction to
// recover with. Every OTHER test here would still pass if the gate were removed:
// the floors would be identical, only the memory profile would change, and a
// memory profile is exactly what a functional test does not see. So this test
// makes the log UNREADABLE and requires startup to succeed anyway. If anything
// ever reads the WAL on an ordinary start, this goes red immediately.
func TestOpenSuffixAllocatorDoesNotReadTheLogWhenFloorsExist(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 does not stop the read")
	}
	dir := t.TempDir()
	busID, err := ids.LoadOrCreateBusID(dir, testBusID)
	if err != nil {
		t.Fatalf("LoadOrCreateBusID: %v", err)
	}
	l := openTestWAL(t, dir)
	writeMessageRecord(t, l, busID, busID+".alpha-3", nil, 1)

	// First start: derives from the log and writes the floors file.
	if _, err := openSuffixAllocator(dir, l, busID, false, quietLogger(t)); err != nil {
		t.Fatalf("first openSuffixAllocator: %v", err)
	}

	walPath := l.Path()
	if err := os.Chmod(walPath, 0o000); err != nil {
		t.Fatalf("chmod %q: %v", walPath, err)
	}
	t.Cleanup(func() { _ = os.Chmod(walPath, 0o600) })
	// Sanity: the fixture really does make the file unreadable, so a pass below
	// cannot come from a chmod that did nothing.
	if _, err := walAgentIDFloors(walPath, busID); err == nil {
		t.Fatal("the WAL is still readable after chmod 000; this test would prove nothing")
	}

	alloc, err := openSuffixAllocator(dir, l, busID, false, quietLogger(t))
	if err != nil {
		t.Fatalf("openSuffixAllocator with a present floors file and an UNREADABLE log = %v; an ordinary start must not read the log at all -- see the gate on !alloc.Existed()", err)
	}
	// And the floors it sealed are the persisted ones, not an empty map.
	if n, err := alloc.NextSuffix("alpha"); err != nil || n <= 3 {
		t.Fatalf("NextSuffix(\"alpha\") = %d, %v; want strictly greater than 3 from the PERSISTED floors", n, err)
	}
}

// TestOpenSuffixAllocatorBootsButShoutsWhenTheLogWasRepaired covers the
// ordering hazard internal/auth/floors.go documents and this task had to resolve
// explicitly: the backfill scan runs AFTER wal.Open, so anything recovery
// removed is invisible to it and a floor can come out too low.
//
// The resolution (DECISIONS.md 2026-08-07) is BOOT -- the bus always restarts --
// but say what is exposed, at ERROR. A silent start here would be the defect:
// the operator would have no way to know the floors are weaker than usual on
// exactly the dir where they are being derived rather than read.
func TestOpenSuffixAllocatorBootsButShoutsWhenTheLogWasRepaired(t *testing.T) {
	dir := t.TempDir()
	busID, err := ids.LoadOrCreateBusID(dir, testBusID)
	if err != nil {
		t.Fatalf("LoadOrCreateBusID: %v", err)
	}

	// One intact record, then a torn tail.
	l := openTestWAL(t, dir)
	writeMessageRecord(t, l, busID, busID+".alpha-4", nil, 1)
	walPath := l.Path()
	if err := l.Close(); err != nil {
		t.Fatalf("closing the fixture WAL: %v", err)
	}
	f, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("opening %q to append a torn tail: %v", walPath, err)
	}
	if _, err := f.Write([]byte(strings.Repeat("X", 96))); err != nil {
		t.Fatalf("appending a torn tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing %q: %v", walPath, err)
	}

	var buf logBuf
	lg := logging.New(&buf, logging.LevelError)
	repaired, err := wal.Open(wal.LogOptions{Dir: dir, Logger: lg})
	if err != nil {
		t.Fatalf("wal.Open on a torn log: %v", err)
	}
	defer repaired.Close()
	if !suffixBackfillExposure(repaired.Recovered()) {
		t.Fatalf("the fixture did not actually make recovery remove anything; the test would prove nothing. Repair = %+v", repaired.Recovered().Repaired)
	}

	alloc, err := openSuffixAllocator(dir, repaired, busID, false, lg)
	if err != nil {
		t.Fatalf("openSuffixAllocator after a repaired log = %v; the bus must still START (DECISIONS.md: availability over retention)", err)
	}
	if !strings.Contains(buf.String(), "recovery had already REPAIRED") {
		t.Fatalf("a backfill off a repaired log must name the exposure at ERROR; log was:\n%s", buf.String())
	}
	// The surviving record's id is still honoured.
	if n, err := alloc.NextSuffix("alpha"); err != nil || n <= 4 {
		t.Fatalf("NextSuffix(\"alpha\") = %d, %v; want strictly greater than 4", n, err)
	}
}

// TestOpenSuffixAllocatorFailsClosed is the no-fallback proof. Each case is a
// failure this startup path could plausibly be "helped past" by falling back to
// a fresh counter, and each must instead be FATAL -- because a fresh counter
// mints -1 for every name over ids that are already durable.
func TestOpenSuffixAllocatorFailsClosed(t *testing.T) {
	t.Run("a corrupt floors file refuses to start", func(t *testing.T) {
		dir := t.TempDir()
		l := openTestWAL(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "agent-suffixes"), []byte("not an agent-bus floors file\n"), 0o600); err != nil {
			t.Fatalf("writing the corrupt floors file: %v", err)
		}
		alloc, err := openSuffixAllocator(dir, l, testBusID, false, quietLogger(t))
		if err == nil {
			t.Fatalf("openSuffixAllocator returned %v and no error on a CORRUPT floors file; it must refuse, never regenerate, because regenerating resumes every name from 1", alloc)
		}
		if !strings.Contains(err.Error(), "suffix floors") {
			t.Fatalf("error = %v, want it to name the suffix floors", err)
		}
	})

	t.Run("a floors file the seal cannot write refuses to start", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: a read-only directory does not stop the write")
		}
		dir := t.TempDir()
		l := openTestWAL(t, dir)
		// The WAL exists; now make the directory unwritable so Seal's
		// temp-file-plus-rename fails.
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		alloc, err := openSuffixAllocator(dir, l, testBusID, false, quietLogger(t))
		if err == nil {
			t.Fatalf("openSuffixAllocator returned %v and no error when Seal could not write; an unsealed allocator refuses every enrolment, so startup must fail loudly instead", alloc)
		}
		if !strings.Contains(err.Error(), "sealing") {
			t.Fatalf("error = %v, want it to name the failed seal", err)
		}
	})

	t.Run("an underivable log on a LEGACY dir refuses to start", func(t *testing.T) {
		dir := t.TempDir()
		l := openTestWAL(t, dir)
		// A message record naming an id nothing can parse: the derivation cannot
		// complete, there is no floors file to fall back on, and a partial map
		// must never be sealed.
		writeRawRecord(t, l, store.RecordKind, `{"sender":"?????"}`)

		alloc, err := openSuffixAllocator(dir, l, testBusID, false, quietLogger(t))
		if err == nil {
			t.Fatalf("openSuffixAllocator returned %v and no error when the backfill derivation failed on a dir with no floors file", alloc)
		}
		if !strings.Contains(err.Error(), "derivation FAILED") {
			t.Fatalf("error = %v, want it to say the derivation failed", err)
		}
	})
}

// TestNoFreshSuffixCounterInCmd is the regression guard the happy path cannot
// give: ids.NewNameSuffixes() must not appear anywhere in package main, on any
// path, including an "only for a fresh dir" fallback. OpenNameSuffixes already
// handles a fresh dir, so a fallback buys nothing and silently restores the
// defect while every other test stays green.
// It parses rather than greps, so the comment in main.go that NAMES the
// forbidden constructor -- deliberately, to tell the next reader why it is gone
// -- does not trip it. A comment is documentation; a CallExpr is the defect.
func TestNoFreshSuffixCounterInCmd(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		checked++

		// Resolve what THIS FILE calls the ids package, rather than assuming
		// "ids". An aliased import (`import x "…/ids"`) or a dot-import would
		// otherwise walk straight past the guard -- a brittleness the reviewer
		// gate named, and the kind that makes a guard look green while the
		// defect is back.
		idsNames := map[string]bool{}
		dotImported := false
		for _, imp := range file.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil || path != idsImportPath {
				continue
			}
			switch {
			case imp.Name == nil:
				idsNames["ids"] = true
			case imp.Name.Name == ".":
				dotImported = true
			case imp.Name.Name == "_":
				// Blank import: nothing can be called through it.
			default:
				idsNames[imp.Name.Name] = true
			}
		}

		report := func(pos token.Pos) {
			t.Errorf("%s calls ids.NewNameSuffixes(): that is a FRESH counter starting every name at suffix 1, and agent ids are already durable in store message records. Construct through ids.OpenNameSuffixes (openSuffixAllocator) instead -- it handles a fresh data dir on its own, so there is no case that needs this fallback", fset.Position(pos))
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				pkg, ok := fun.X.(*ast.Ident)
				if ok && idsNames[pkg.Name] && fun.Sel.Name == "NewNameSuffixes" {
					report(call.Pos())
					return false
				}
			case *ast.Ident:
				// Only reachable through a dot-import of the ids package.
				if dotImported && fun.Name == "NewNameSuffixes" {
					report(call.Pos())
					return false
				}
			}
			return true
		})
	}
	// A guard that checked nothing would pass silently -- exactly the vacuous
	// proof CLAUDE.md warns about.
	if checked == 0 {
		t.Fatal("no non-test .go files were parsed; this guard proved nothing")
	}
}
