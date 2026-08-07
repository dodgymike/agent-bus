//go:build linux || darwin

package wal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// CRASH INJECTION FOR THE DURABLE INDEX FLOOR.
//
// CLAUDE.md: "Durability and recovery code must have crash-injection tests -- a
// test that writes, kills at a chosen point in the write path, and asserts what
// recovery yields. 'The code looks right' is not evidence for a durability
// claim." The floor's whole purpose is a property ACROSS a process death, so a
// same-process simulation would be evidence of nothing: it still runs every
// defer, every Close and every buffer flush on the way out -- including the
// floor's own seal, which is exactly the step a crash is defined by NOT running.
//
// So a child process is re-exec'd, writes through the real Log, and SIGKILLs
// itself. SIGKILL cannot be caught, blocked or handled.
//
// THREE THINGS ARE DELIBERATELY NOT TRUSTED, each because it has produced a
// vacuous pass somewhere before:
//
//   - The child's EXIT CODE. `go test -run NameThatDoesNotExist` prints "no
//     tests to run" and EXITS 0, so an exit code alone cannot tell "the child
//     did the work" from "the child did nothing". Every child therefore writes
//     a REPORT FILE listing every index it was handed, fsynced before the kill,
//     and the parent asserts on THAT. No report means the child never ran, and
//     that is a failure -- see internal/dirlock/crossproc_test.go, which states
//     the same rule for the same reason.
//   - "err != nil" from the child. A child that failed its own assertions also
//     exits non-zero. The parent asserts on the WAIT STATUS: Signaled() and
//     Signal() == SIGKILL.
//   - "recovery returned no error". A quarantine case that never quarantined
//     would pass every index assertion trivially, so the quarantine itself is
//     asserted (Recovered.Repaired.Quarantined != "").
//
// Every directory is a t.TempDir() belonging to the parent. The tracked ./data
// directory is never touched.
// ---------------------------------------------------------------------------

const (
	// envIFChild selects what the index-floor child does. Unset means "not a
	// child", which makes TestWALIndexFloorChild a no-op skip in a normal run.
	envIFChild = "WAL_INDEXFLOOR_CHILD"
	// envIFDir is the data directory the child writes into: a parent t.TempDir().
	envIFDir = "WAL_INDEXFLOOR_DIR"
	// envIFReport is where the child records every index it was handed, so the
	// parent checks EVIDENCE rather than an exit status a no-op also produces.
	envIFReport = "WAL_INDEXFLOOR_REPORT"
	// envIFTxns is how many complete transactions the child writes before it
	// leaves one unresolved and dies.
	envIFTxns = "WAL_INDEXFLOOR_TXNS"

	// ifWriteAndDie: Open, write envIFTxns transactions, leave one PREPARE
	// unresolved, report, and SIGKILL.
	ifWriteAndDie = "write-and-die"

	// ifChildWait bounds a child, so a wedge fails this test in a minute rather
	// than hanging the suite until the package timeout.
	ifChildWait = 90 * time.Second
)

// TestWALIndexFloorChild is the child half. It does NOTHING in a normal run.
func TestWALIndexFloorChild(t *testing.T) {
	mode := os.Getenv(envIFChild)
	if mode == "" {
		t.Skip("not an index-floor child: " + envIFChild + " is unset")
	}
	if mode != ifWriteAndDie {
		t.Fatalf("child: unknown mode %q", mode)
	}
	dir := os.Getenv(envIFDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envIFChild, mode, envIFDir)
	}
	report := os.Getenv(envIFReport)
	if report == "" {
		t.Fatalf("child: %s is empty; the parent would have no evidence this child ran", envIFReport)
	}
	txns, err := strconv.Atoi(os.Getenv(envIFTxns))
	if err != nil {
		t.Fatalf("child: %s=%q is not a number: %v", envIFTxns, os.Getenv(envIFTxns), err)
	}

	l, err := Open(LogOptions{Dir: dir, Now: crashInjectionClock()})
	if err != nil {
		t.Fatalf("child: Open: %v", err)
	}

	lines := []string{"start=" + strconv.FormatUint(l.Recovered().NextIndex, 10)}
	for i := 0; i < txns; i++ {
		c, werr := l.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
		if werr != nil {
			t.Fatalf("child: Write %d: %v", i, werr)
		}
		lines = append(lines,
			"index="+strconv.FormatUint(c.PrepareIndex, 10),
			"index="+strconv.FormatUint(c.CommitIndex, 10))
	}

	// One transaction left UNRESOLVED, so the kill lands genuinely mid-write: the
	// PREPARE record is fsynced and durable, and its COMMIT will never exist. Its
	// index was handed out and must never come back.
	//
	// (txns == 0 is the "died before touching the log" case, which is a distinct
	// and load-bearing shape: Open reserved a block and fsynced it, and NOTHING
	// was ever issued. It must not burn that block.)
	if txns > 0 {
		txn, berr := l.Begin(Entry{Kind: "message", Body: json.RawMessage(`{"unresolved":true}`)})
		if berr != nil {
			t.Fatalf("child: Begin: %v", berr)
		}
		lines = append(lines, "index="+strconv.FormatUint(txn.PrepareIndex(), 10))
	}

	// The report is fsynced BEFORE the kill, or the parent would have nothing to
	// assert against and this would degrade into a test of an exit code.
	f, err := os.OpenFile(report, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("child: creating the report %s: %v", report, err)
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatalf("child: writing the report: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("child: fsyncing the report: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("child: closing the report: %v", err)
	}

	// No Close, no Sync, no defer, and above all NO SEAL of the index floor: the
	// next statement is the kill.
	suicide()
	t.Fatalf("child: still running after SIGKILL")
}

// ifChildReport is what one killed child was handed.
type ifChildReport struct {
	start   uint64          // the index Open gave it
	indices []uint64        // every index it was handed, in order
	highest uint64          // the largest of them, or start-1 when it wrote nothing
	set     map[uint64]bool // membership, for the "never twice" assertion
}

// runIndexFloorChild re-execs the test binary, proves the child really died on
// SIGKILL, and returns the report it fsynced before dying.
func runIndexFloorChild(t *testing.T, dir string, txns int) ifChildReport {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v (cannot re-exec the test binary, so the crash claim cannot be proved)", err)
	}
	report := filepath.Join(t.TempDir(), "indices")

	ctx, cancel := context.WithTimeout(context.Background(), ifChildWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestWALIndexFloorChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		envIFChild+"="+ifWriteAndDie,
		envIFDir+"="+dir,
		envIFReport+"="+report,
		envIFTxns+"="+strconv.Itoa(txns),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	runErr := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("the index-floor child did not finish in time: %v\n--- child output ---\n%s", ctx.Err(), out.String())
	}
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("the index-floor child: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s",
			runErr, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("the index-floor child: wait status is %T, want syscall.WaitStatus", ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("the index-floor child exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s",
			ws.ExitStatus(), out.String())
	}

	// THE REPORT IS THE EVIDENCE. A child that skipped, or that never reached its
	// writes, also dies -- so its absence is a failure, not a detail.
	body, rerr := os.ReadFile(report)
	if rerr != nil {
		t.Fatalf("the child died on SIGKILL but fsynced no report (%v): it never got as far as writing, so this test would be proving nothing"+
			"\n--- child output ---\n%s", rerr, out.String())
	}

	rep := ifChildReport{set: map[uint64]bool{}}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		switch {
		case strings.HasPrefix(line, "start="):
			n, perr := strconv.ParseUint(strings.TrimPrefix(line, "start="), 10, 64)
			if perr != nil {
				t.Fatalf("the child report has a bad start line %q: %v", line, perr)
			}
			rep.start = n
		case strings.HasPrefix(line, "index="):
			n, perr := strconv.ParseUint(strings.TrimPrefix(line, "index="), 10, 64)
			if perr != nil {
				t.Fatalf("the child report has a bad index line %q: %v", line, perr)
			}
			rep.indices = append(rep.indices, n)
			rep.set[n] = true
			if n > rep.highest {
				rep.highest = n
			}
		default:
			t.Fatalf("the child report has an unrecognised line %q", line)
		}
	}
	if rep.start == 0 {
		t.Fatalf("the child report has no start line:\n%s", body)
	}
	if want := 2*txns + boolToInt(txns > 0); len(rep.indices) != want {
		t.Fatalf("the child reported %d indices, want %d for %d transactions plus the unresolved prepare: the kill landed somewhere other than where this test believes",
			len(rep.indices), want, txns)
	}
	if rep.highest == 0 {
		rep.highest = rep.start - 1
	}
	return rep
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestWALIndexFloorCrashNeverReissuesAnIndex is the test that matters most: a
// real SIGKILL, then damage, then a restart that MUST come up and MUST resume
// above every index the dead process was handed.
//
// The two defects this closes were both reported against exactly these shapes:
//
//	e120153b -- a discarded TAIL record's index handed straight back out;
//	db350e39 -- a QUARANTINE resetting the index to 1, reissuing the whole
//	            history, because the file's own answer for an empty log is 1.
//
// Both halves are asserted in every row, because either alone is worthless:
//
//	INVARIANT 6 -- Open returns a usable *Log. There is NO refuse-to-start
//	  behaviour here and none is wanted; a quarantine still starts a fresh log
//	  and the bus still boots.
//	INVARIANT 1 -- the next index is STRICTLY GREATER than every index the child
//	  reported, and the indices the restarted log then hands out intersect the
//	  child's set nowhere.
//
// And the skip is LOUD. Skipping index space silently is the same failure as
// discarding a record silently, applied to the id space instead of the message
// space, so the row says which line must appear.
func TestWALIndexFloorCrashNeverReissuesAnIndex(t *testing.T) {
	const (
		damageNone       = "none"
		damageTornTail   = "torn-tail"
		damageQuarantine = "quarantine"
	)

	cases := []struct {
		name string
		// txns is how many complete transactions the child writes before leaving
		// one prepare unresolved and dying. 0 means it dies having appended
		// nothing at all.
		txns   int
		damage string
		// wantQuarantine requires the log to have actually been moved aside, so
		// the row cannot pass by never reaching the path it is named for.
		wantQuarantine bool
		// wantLoud is the operator-log message the skip must carry, or "" when
		// nothing was skipped and the log must therefore stay quiet.
		wantLoud      string
		wantLoudLevel string
		// wantNext, when non-zero, is the EXACT index the restart must resume at.
		// The relational assertions below (strictly above the child's highest)
		// are the safety property; this pins the number so a change of policy has
		// to be made deliberately rather than absorbed.
		wantNext uint64
		// crossesBlock requires the child to have written PAST indexReserveBlock,
		// so reserve() genuinely touched the disk mid-run. It is asserted rather
		// than assumed: if the fixture ever shrinks below the boundary, the whole
		// amortisation path silently stops being tested and every row here still
		// passes.
		crossesBlock bool
		note         string
	}{
		{
			// A CRASH BURNS THE RESERVATION, and this row is where that policy is
			// most visible: Open fsynced a reservation for a whole block before
			// the child could append, the child died without using any of it, and
			// the next start still resumes past the whole block.
			//
			// THIS ROW EXPECTED 1 UNTIL 2026-08-07, on the argument that a
			// reservation is not an issue. The argument is sound; the PREMISE it
			// needed is not. To resume at 1 safely, recovery must know that no
			// frame in 1..64 was ever written -- and after a crash it cannot know
			// that, because a truncation at a clean frame boundary, a rewrite, or
			// a deleted bus.wal all leave a file that is byte-indistinguishable
			// from one that never held those records. A reviewer's probe reissued
			// an index at 25 of 2289 truncation offsets on exactly that reasoning.
			// The bit that IS knowable is "did the previous run close cleanly",
			// and a killed child leaves sealed 0.
			name: "died before appending anything, log intact",
			txns: 0, damage: damageNone,
			wantNext:      indexReserveBlock + 1,
			wantLoud:      "wal resumed the record index above what the log file alone would have given",
			wantLoudLevel: "WARN",
			note:          "a crashed run leaves sealed 0, so its reservation is burned: the alternative is trusting a file that cannot testify",
		},
		{
			// The ordinary crash: whole frames on disk (a SIGKILL cannot tear a
			// write -- the page cache outlives the process), one unresolved
			// prepare, no seal. The log is READABLE -- and that is precisely the
			// case that used to reissue, because "readable" and "complete" are not
			// the same claim and only the second one would justify trusting it.
			name: "died mid-transaction, log intact",
			txns: 8, damage: damageNone,
			wantNext:      indexReserveBlock + 1,
			wantLoud:      "wal resumed the record index above what the log file alone would have given",
			wantLoudLevel: "WARN",
			note:          "an intact-LOOKING log after a crash proves nothing about what was deleted before this start; sealed 0 is the only honest reading",
		},
		{
			// e120153b: the tail record is discarded, and its index must be
			// stepped over rather than handed back.
			name: "died mid-transaction, tail torn",
			txns: 8, damage: damageTornTail,
			wantNext:      indexReserveBlock + 1,
			wantLoud:      "wal resumed the record index above what the log file alone would have given",
			wantLoudLevel: "WARN",
			note:          "a discarded tail record's index is burned, never reissued",
		},
		{
			// db350e39: the log is gone and the fresh one that replaces it says
			// "index 1". Only the floor -- which lives OUTSIDE the log -- can say
			// otherwise.
			name: "died mid-transaction, whole log quarantined",
			txns: 8, damage: damageQuarantine,
			wantQuarantine: true,
			wantNext:       indexReserveBlock + 1,
			wantLoud:       "wal resumed the record index above the durable floor after a quarantine",
			wantLoudLevel:  "ERROR",
			note:           "a quarantine must not reissue the bus's entire history",
		},
		{
			// THE BLOCK BOUNDARY, crossed for real. 130 transactions is 260
			// records plus an unresolved prepare, so reserve() genuinely touched
			// the disk at index 257 -- the amortisation path is otherwise never
			// exercised by a crash test, and an off-by-one there would only ever
			// show up as a reissued id in production.
			name: "died past the block boundary, whole log quarantined",
			txns: 130, damage: damageQuarantine,
			wantQuarantine: true,
			wantLoud:       "wal resumed the record index above the durable floor after a quarantine",
			wantLoudLevel:  "ERROR",
			crossesBlock:   true,
			note:           "the ceiling must have tracked the reservations made past the first block",
		},
		{
			name: "died past the block boundary, tail torn",
			txns: 130, damage: damageTornTail,
			wantLoud:      "wal resumed the record index above what the log file alone would have given",
			wantLoudLevel: "WARN",
			crossesBlock:  true,
			note:          "a torn tail past the boundary resumes above the second block, not inside it",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, WALFileName)

			// (1) A REAL PROCESS DIES ON SIGKILL, having been handed real indices.
			child := runIndexFloorChild(t, dir, tc.txns)

			// The row is what it says it is. Without this, a fixture that drifted
			// below the block boundary would stop exercising the amortisation path
			// entirely while every assertion below still passed.
			if tc.crossesBlock && child.highest <= indexReserveBlock {
				t.Fatalf("this row claims to cross the block boundary but the child's highest index was %d, at or below indexReserveBlock (%d): reserve() never touched the disk, so the amortisation path is untested here",
					child.highest, indexReserveBlock)
			}
			if !tc.crossesBlock && child.highest > indexReserveBlock {
				t.Fatalf("this row is meant to stay inside the first reserved block but the child reached index %d", child.highest)
			}

			// (2) THE DAMAGE. Applied by the parent, after the child is dead, so
			// the bytes recovery sees are the dying process's plus exactly this.
			switch tc.damage {
			case damageNone:
			case damageTornTail:
				recs, _, err := ScanAll(path, KindWAL)
				if err != nil {
					t.Fatalf("ScanAll on the crashed log: %v (a SIGKILL leaves whole frames, so it must scan clean)", err)
				}
				if len(recs) == 0 {
					t.Fatalf("the crashed log holds no records, so there is no tail to tear")
				}
				last := recs[len(recs)-1]
				truncate(t, path, last.Offset+FrameHeaderSize+2)
				if _, err := Replay(path, nil); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("Replay of the torn log = %v, want ErrCorrupt: the tail is not actually torn, so this row proves nothing", err)
				}
			case damageQuarantine:
				// Nine bytes: too short even for the file header, so the layout
				// cannot be established and not one record can be salvaged.
				truncate(t, path, 9)
			default:
				t.Fatalf("unknown damage %q", tc.damage)
			}

			// (3) THE RESTART. It must SUCCEED -- invariant 6, no refusal.
			var buf bytes.Buffer
			l, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
			if err != nil {
				t.Fatalf("Open after a kill -9 and %s damage: %v\nrecovery must ALWAYS reach a running server; there is no refuse-to-start behaviour here and none is wanted (invariant 6)",
					tc.damage, err)
			}
			defer l.Close()
			out := buf.String()
			rec := l.Recovered()

			// The path was really taken.
			if tc.wantQuarantine && rec.Repaired.Quarantined == "" {
				t.Fatalf("Recovered().Repaired.Quarantined is empty: this row is about the QUARANTINE path and never reached it, so every index assertion below would pass vacuously. Repair: %+v", rec.Repaired)
			}
			if !tc.wantQuarantine && rec.Repaired.Quarantined != "" {
				t.Fatalf("the log was quarantined (%s) in a row that expected it to survive: %+v", rec.Repaired.Quarantined, rec.Repaired)
			}

			// (4) INVARIANT 1. Strictly greater than EVERY index the dead process
			// was handed -- not "greater than what survives in the file", which is
			// the arithmetic both defects did.
			if rec.NextIndex <= child.highest {
				t.Fatalf("Recovered().NextIndex = %d, but the killed process was handed index %d: recovery is REISSUING an index this data directory already authorised (invariant 1, reaffirmed WITHOUT narrowing on 2026-08-02). %s.\nchild indices: %v\nrepair: %+v",
					rec.NextIndex, child.highest, tc.note, child.indices, rec.Repaired)
			}
			if tc.wantNext != 0 && rec.NextIndex != tc.wantNext {
				t.Errorf("Recovered().NextIndex = %d, want exactly %d: %s", rec.NextIndex, tc.wantNext, tc.note)
			}

			// (5) AND THE INDICES IT ACTUALLY HANDS OUT ARE ALL NEW.
			for i := 0; i < 3; i++ {
				c, werr := l.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"after":%d}`, i))})
				if werr != nil {
					t.Fatalf("Write %d after recovery: %v: a repaired log must be one a server can go on writing to", i, werr)
				}
				for _, idx := range []uint64{c.PrepareIndex, c.CommitIndex} {
					if child.set[idx] {
						t.Fatalf("the write after recovery got index %d, which the killed process had already been handed: no index may EVER be handed out twice (invariant 1). %s", idx, tc.note)
					}
					if idx <= child.highest {
						t.Fatalf("the write after recovery got index %d, at or below the killed process's highest index %d", idx, child.highest)
					}
				}
			}
			assertIndicesUnique(t, path)

			// (6) THE SKIP IS LOUD, or there was no skip and the log stays quiet.
			// Recovery must never step over index space silently: that is the
			// silent-discard defect applied to the id space.
			if tc.wantLoud != "" {
				assertLogged(t, out, tc.wantLoudLevel, tc.wantLoud,
					"indices_skipped=", "index_floor="+filepath.Join(dir, IndexFloorFileName),
					"invariant 1")
			} else {
				assertNotLogged(t, out, "wal resumed the record index above what the log file alone would have given")
				assertNotLogged(t, out, "wal resumed the record index above the durable floor after a quarantine")
			}
			// NO FALSE ALARM ON THIS START, in every row. The burned block sits
			// ABOVE everything in the file, so it is not an interior hole and
			// replay must not count it as one -- a loss channel that cries wolf is
			// the mirror image of a silent discard. (The hole becomes interior on
			// the NEXT start, once records exist above it; it is reported at WARN
			// there, and its Reason says the range may be a burned reservation
			// rather than a loss.)
			if rec.MissingRecords != 0 {
				t.Errorf("MissingRecords = %d on the start that made the jump, want 0: nothing is missing from between the records this file holds. Discards: %+v",
					rec.MissingRecords, rec.Discarded)
			}

			// (7) The floor on disk covers everything the dead process had.
			reserved, written, _ := readFloorFile(t, dir)
			if written < child.highest {
				t.Errorf("the durable floor says %d indices are burned, but the killed process was handed up to %d: the burn must be recorded before the Log is returned, or the NEXT crash forgets it",
					written, child.highest)
			}
			if reserved < written {
				t.Errorf("the floor claims %d burned but only %d reserved", written, reserved)
			}
			if tc.crossesBlock && reserved <= indexReserveBlock {
				t.Errorf("the durable ceiling is %d after the child wrote past index %d: reserve() must have PERSISTED a new ceiling when the index crossed the block, or the indices past it were stamped into frames while nothing on stable storage had authorised them",
					reserved, indexReserveBlock)
			}
		})
	}
}

// TestWALIndexFloorWriteAheadOrderingLeavesNoReissue walks the crash window
// AROUND a floor update, point by point.
//
// HONEST LABELLING, because the difference matters: the test above performs a
// GENUINE kill -9. This one does NOT. Two of the four points below sit INSIDE
// Writer.Append -- between the floor's fsync and the frame's -- and there is no
// way to stop a real process there without adding a test hook to production
// code, which is not a change this task licenses. So each point is reproduced by
// MANIPULATING THE TWO FILES DIRECTLY into exactly the state that crash would
// have left, and the comment on each says which state that is. No crash is
// claimed that was not performed.
//
// The ordering being pinned is the whole guarantee: THE FLOOR IS WRITTEN AHEAD
// OF THE INDEX IT AUTHORISES, never after. A crash between the two must leave an
// index BURNED-BUT-UNUSED (a hole, which is legal and permanent) and never
// USED-BUT-UNAUTHORISED (a reissue, which is the defect).
func TestWALIndexFloorWriteAheadOrderingLeavesNoReissue(t *testing.T) {
	// writeTxns lays down n transactions through the real write path and returns
	// the data directory and every index handed out.
	writeTxns := func(t *testing.T, n int) (string, map[uint64]bool, uint64) {
		t.Helper()
		dir := t.TempDir()
		l, err := Open(LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		handed := map[uint64]bool{}
		var highest uint64
		for i := 0; i < n; i++ {
			c, werr := l.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
			if werr != nil {
				t.Fatalf("Write %d: %v", i, werr)
			}
			handed[c.PrepareIndex], handed[c.CommitIndex] = true, true
			highest = c.CommitIndex
		}
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return dir, handed, highest
	}

	cases := []struct {
		name string
		// txns is how many transactions exist in the log before the simulated
		// crash state is imposed.
		txns int
		// floor is the (reserved, written, sealed) triple the crash would have
		// left in <data-dir>/wal-index-floor. It is written OVER whatever is
		// there. sealed FALSE is the crash shape -- begin fsyncs sealed 0 before
		// a byte can be appended and only a clean Writer.Close ever sets it -- so
		// every row that models a crash leaves it false.
		reserved, written uint64
		sealed            bool
		// quarantine truncates the log to nine bytes, so the file's own answer
		// for "what index comes next" is 1.
		quarantine bool
		// wantNext is the index the restart must hand out.
		wantNext uint64
		why      string
	}{
		{
			// POINT 1: a crash BEFORE THE FIRST APPEND. Open's begin fsynced
			// reserved=indexReserveBlock, written=0, sealed=0, and the process
			// died.
			//
			// THIS ROW EXPECTED 1 UNTIL 2026-08-07, on the argument that "a
			// reservation is not an issue, so burning it costs a hole for
			// nothing". The reviewer's probe killed that argument: the trigger for
			// consulting the ceiling used to be "did this recovery find damage",
			// and a truncation at a clean frame boundary is byte-indistinguishable
			// from a shorter log, so the ceiling was skipped in exactly the cases
			// that needed it and 25 of 2289 truncation offsets reissued an index.
			// The trigger is now "did the previous run close cleanly", which is
			// knowable -- and a crashed run leaves sealed 0, so the block IS
			// burned. That is the price, and it is why the block came down from
			// 256 to 64.
			name: "before the first append",
			txns: 0, reserved: indexReserveBlock, written: 0, sealed: false,
			wantNext: indexReserveBlock + 1,
			why:      "a crashed run leaves sealed 0, so the next start takes the ceiling: it is >= every index that run could have authorised, because nothing is stamped into a frame before its reservation is fsynced",
		},
		{
			// POINT 1b: THE SAME FLOOR, CLEANLY SEALED. This is the row that
			// proves the seal bit is doing work rather than the ceiling simply
			// having become unconditional: identical numbers, sealed 1, and the
			// reservation is NOT burned.
			name: "before the first append, but the run closed cleanly",
			txns: 0, reserved: indexReserveBlock, written: 0, sealed: true,
			wantNext: 1,
			why:      "sealed 1 means written is EXACT, so written+1 already dominates every index ever written and the ceiling would only burn a hole",
		},
		{
			// POINT 2: a crash BETWEEN THE FLOOR WRITE AND THE FRAME WRITE.
			// reserve() persisted a new ceiling for an index that no frame ever
			// carried.
			//
			// This expected 41 until 2026-08-07 ("an authorised-but-unissued index
			// is free"). It is free only if recovery can PROVE no frame carried
			// it, and after a crash it cannot: the log may have been truncated at
			// a frame boundary, rewritten, or deleted, and none of those leave
			// evidence. sealed 0 therefore means the ceiling, not the file.
			name: "between the floor write and the frame write",
			txns: 20, reserved: 512, written: 40, sealed: false,
			wantNext: 513,
			why:      "after a crash the file's high-water mark is a lower bound, not the answer; only the ceiling bounds it from above",
		},
		{
			// POINT 3: a crash AFTER A FRAME'S FSYNC BUT BEFORE THE NEXT FLOOR
			// UPDATE. The floor is amortised, so for 63 out of every 64 appends it
			// is deliberately BEHIND the log.
			//
			// The maximum is still taken -- the file's 41 is used when it is the
			// larger -- but a crashed run also takes the ceiling, and here the
			// ceiling (64) is above the file (41), so the block is burned.
			name: "after a frame fsync, before the next floor update",
			txns: 20, reserved: indexReserveBlock, written: 0, sealed: false,
			wantNext: indexReserveBlock + 1,
			why:      "the floor lags the log by design, so the maximum of the two is taken -- and a crashed run adds the ceiling to that maximum",
		},
		{
			// POINT 3b: the same state after a CLEAN close, where `written` is
			// exact at 40 and the file agrees. No hole at all: this is the
			// ordinary restart, and it is the property the seal bit buys back.
			name: "a clean close with the floor exactly at the log's high-water mark",
			txns: 20, reserved: indexReserveBlock, written: 40, sealed: true,
			wantNext: 41,
			why:      "a clean cycle must leave the index sequence dense; burning a block on every ordinary restart would put a permanent hole in every bus's log",
		},
		{
			// POINT 4: THE INDUCTION HOLE, and the subtlest state in the whole
			// design. A previous run JUMPED the index (a quarantine forced it to),
			// then wrote NOTHING at all, then crashed. No record anywhere carries
			// the jump. If the floor did not record written = start-1 at every
			// Open, this restart would see an empty, undamaged log, find no damage
			// to justify a jump, and cheerfully resume at 1 -- reissuing every
			// index the jumped run had authorised.
			name: "a run that jumped, wrote nothing, and crashed",
			txns: 0, reserved: 512, written: 512, sealed: false,
			wantNext: 513,
			why:      "the jump is durable even though no record carries it; without it, an empty undamaged log after a jumped run resumes at 1",
		},
		{
			// POINT 5: the floor was pushed past the block boundary and the log
			// was then lost ENTIRELY. The file says 1; only the ceiling can say
			// otherwise, and it must be believed.
			name: "the floor crossed a block boundary and the log was then quarantined",
			txns: 20, reserved: 512, written: 40, sealed: false, quarantine: true,
			wantNext: 513,
			why:      "a quarantine takes the log's answer away, so the ceiling is the only bound left",
		},
		{
			// POINT 5b: THE SAME QUARANTINE AFTER A CLEAN CLOSE. `written` is
			// exact, so the bus resumes one past it rather than one past a
			// reservation -- the quarantine still cannot rewind the index, but it
			// no longer costs a block either.
			name: "a quarantine after a clean close",
			txns: 20, reserved: 512, written: 40, sealed: true, quarantine: true,
			wantNext: 41,
			why:      "written is exact after a clean close, so a quarantine costs no burned indices at all -- and still cannot rewind below 41",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir, handed, highest := writeTxns(t, tc.txns)

			// The simulated crash state, imposed directly on the two files. This
			// is the step that is NOT a crash, and it is why the doc comment above
			// says so out loud.
			writeFloorFile(t, dir, "", encodeFloorBody(tc.reserved, tc.written, tc.sealed))
			if tc.quarantine {
				truncate(t, filepath.Join(dir, WALFileName), 9)
			}

			l, err := Open(LogOptions{Dir: dir})
			if err != nil {
				t.Fatalf("Open: %v: recovery must always reach a running server", err)
			}
			defer l.Close()

			if got := l.Recovered().NextIndex; got != tc.wantNext {
				t.Fatalf("Recovered().NextIndex = %d, want %d: %s", got, tc.wantNext, tc.why)
			}
			if tc.wantNext <= highest {
				t.Fatalf("this case is mis-specified: wantNext %d is not above the highest index actually issued (%d)", tc.wantNext, highest)
			}

			// No index handed out before the simulated crash may come back.
			c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"crash"}`)})
			if err != nil {
				t.Fatalf("Write after the simulated crash: %v", err)
			}
			for _, idx := range []uint64{c.PrepareIndex, c.CommitIndex} {
				if handed[idx] {
					t.Fatalf("index %d was handed out a second time: %s", idx, tc.why)
				}
			}
		})
	}
}

// TestWALIndexFloorCrashedRunThatJumpedIsRemembered is point 4 above, end to
// end and with a REAL kill this time: a run whose start was forced upwards, that
// then died having written nothing, must not have the jump forgotten.
//
// This is the induction step of the whole argument. The ceiling branch is
// CONDITIONAL -- it fires only when this recovery found damage it could not
// enumerate -- so without the floor recording written = start-1 on EVERY Open,
// the run after a jumped-but-silent one sees a clean file and resumes below it.
func TestWALIndexFloorCrashedRunThatJumpedIsRemembered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, WALFileName)

	// (1) A real process is handed real indices and is SIGKILLed.
	child := runIndexFloorChild(t, dir, 4)

	// (2) Its log is destroyed outright, so the next start is FORCED to jump.
	truncate(t, path, 9)

	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open after the quarantine: %v", err)
	}
	if l.Recovered().Repaired.Quarantined == "" {
		_ = l.Close()
		t.Fatalf("the log was not quarantined, so no jump was forced and this test proves nothing")
	}
	jumped := l.Recovered().NextIndex
	if jumped <= child.highest {
		_ = l.Close()
		t.Fatalf("the forced start %d is not above the child's highest index %d", jumped, child.highest)
	}
	// (3) THIS RUN WRITES NOTHING AT ALL and closes. Its log is empty apart from
	// a file header, so NO RECORD ANYWHERE CARRIES THE JUMP.
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if recs, _, serr := ScanAll(path, KindWAL); serr != nil || len(recs) != 0 {
		t.Fatalf("the fresh log holds %d records (%v), want an empty one: the point of this test is that the file cannot possibly remember the jump", len(recs), serr)
	}

	// (4) THE NEXT START sees an empty, undamaged log. There is no damage to
	// justify a jump and no record to infer one from -- only the floor.
	var buf bytes.Buffer
	l2, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer l2.Close()
	if got := l2.Recovered().Repaired.Quarantined; got != "" {
		t.Fatalf("the second start quarantined something (%s); it was meant to find a clean, empty log", got)
	}
	if got := l2.Recovered().NextIndex; got < jumped {
		t.Fatalf("the run after a jumped-but-silent one resumed at %d, below the %d the jumped run had already authorised: the jump was FORGOTTEN, which is the induction hole the floor's `written` field exists to close",
			got, jumped)
	}
	c, err := l2.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"the jump"}`)})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if child.set[c.PrepareIndex] || child.set[c.CommitIndex] {
		t.Fatalf("the write after two restarts got {prepare:%d commit:%d}, one of which the original killed process had already been handed",
			c.PrepareIndex, c.CommitIndex)
	}
}
