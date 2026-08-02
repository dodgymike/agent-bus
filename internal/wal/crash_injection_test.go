//go:build linux || darwin

package wal

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// DUR-6 -- the crash-injection suite for the two-phase write path.
//
// Everything here asserts ONE property, stated two ways because the two halves
// fail in opposite directions:
//
//	NOTHING ACKNOWLEDGED IS EVER LOST  (invariant 4: a send returns success only
//	                                    after the commit record is fsynced)
//	NOTHING UNACKNOWLEDGED IS EVER VISIBLE
//
// Together: whatever recovery yields must be a valid PREFIX of accepted history
// (invariant 5), and the cut between "present" and "absent" must sit exactly at
// the last commit record that completed its fsync.
//
// WHERE EACH STAGE OF THE WRITE PATH IS COVERED. The path is
//
//	Begin  -> encode prepare -> WriteAt -> fsync -> return       (phase one)
//	Commit -> encode commit  -> WriteAt -> fsync -> Apply -> ret (phase two)
//
//	stage 1  before the prepare fsync   TestWALCrashTornFrameTailIsRepaired
//	                                    (replay_crash_test.go: real SIGKILL, torn
//	                                    PREPARE frame on the tail)
//	                                    + TestCrashInjectionTruncationPrefixSweep
//	stage 2  between prepare and commit TestWALReplayCrashBetweenPrepareAndCommit
//	                                    (replay_crash_test.go: real SIGKILL)
//	                                    + TestCrashInjectionTruncationPrefixSweep
//	stage 3  mid commit-record write    TestCrashInjectionMidCommitWriteKill (below,
//	                                    real SIGKILL, torn COMMIT frame)
//	                                    + TestCrashInjectionTruncationPrefixSweep
//	stage 4  after the commit fsync     TestWALReplayCrashInsideApply
//	                                    (replay_crash_test.go: real SIGKILL inside
//	                                    Apply -- durable, never acknowledged)
//
// The two SWEEPS below are what makes this a suite rather than four anecdotes:
// instead of picking plausible crash points by hand, they cut and corrupt the
// file at EVERY BYTE OFFSET and assert the property universally. A hand-picked
// offset proves the offsets someone thought of; a sweep proves the ones nobody
// did.
// ---------------------------------------------------------------------------

// crashInjectionEntries is the accepted history every case here starts from.
// The bodies are deliberately different lengths so that frames do not all begin
// at the same offset modulo anything, and a byte-by-byte sweep therefore lands
// on every part of every frame: length field, index, type, checksum, payload.
var crashInjectionEntries = []Entry{
	{Kind: "message", Body: json.RawMessage(`{"to":"bus-1.agent-a","seq":1}`)},
	{Kind: "agent", Body: nil},
	{Kind: "message", Body: json.RawMessage(`{"to":"bus-1.agent-b","seq":2,"pad":"xxxxxxxxxxxxxxxxxxxx"}`)},
	{Kind: "message", Body: json.RawMessage(`{"to":"bus-1.agent-c","seq":3}`)},
}

// crashInjectionClock is a fixed clock, so the bytes a fixture produces are
// reproducible and a sweep is not silently testing a different file each run.
func crashInjectionClock() func() time.Time {
	n := 0
	return func() time.Time {
		n++
		return time.Date(2026, 8, 2, 9, 0, n, 0, time.UTC)
	}
}

// acceptedHistory writes crashInjectionEntries through the REAL two-phase path
// and reports, for each entry, the file offset at which it became ACKNOWLEDGED:
// the offset just past its COMMIT frame, which is where Append's fsync returned
// and therefore the first moment Write could have told a caller "yes".
//
// That offset is the whole point. Every assertion below is phrased against it
// rather than against a record count, because "acknowledged" is a fact about
// when fsync returned, not about how many records someone counted.
func acceptedHistory(t *testing.T) (path string, acked []Committed, ackedAt []int64) {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir, Now: crashInjectionClock()})
	if err != nil {
		t.Fatalf("building the accepted history: Open: %v", err)
	}
	for i, e := range crashInjectionEntries {
		c, err := l.Write(e)
		if err != nil {
			t.Fatalf("building the accepted history: Write %d: %v", i, err)
		}
		// Write has returned, so the COMMIT record is fsynced: this entry is
		// acknowledged, and the file is exactly this long at that instant.
		acked = append(acked, c)
		ackedAt = append(ackedAt, l.w.Size())
	}
	if err := l.Close(); err != nil {
		t.Fatalf("building the accepted history: Close: %v", err)
	}
	return filepath.Join(dir, WALFileName), acked, ackedAt
}

// crashFixture copies bytes into a fresh directory and returns that directory
// and the WAL path inside it. Each sweep iteration gets its own directory: a
// sweep that damaged one shared file in place would be testing the accumulation
// of its own damage, not the damage it meant to inject.
func crashFixture(t *testing.T, parent string, n int, b []byte) (dir, path string) {
	t.Helper()
	dir = filepath.Join(parent, fmt.Sprintf("case-%04d", n))
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatalf("crashFixture: mkdir: %v", err)
	}
	path = filepath.Join(dir, WALFileName)
	if err := os.WriteFile(path, b, fileMode); err != nil {
		t.Fatalf("crashFixture: write: %v", err)
	}
	return dir, path
}

// recoverFixture starts a Log on dir exactly the way the server does and reports
// what came back to memory.
func recoverFixture(t *testing.T, dir string) ([]Committed, Recovered, error) {
	t.Helper()
	app := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: app})
	if err != nil {
		return nil, Recovered{}, err
	}
	rec := l.Recovered()
	got := make([]Committed, app.count())
	for i := range got {
		got[i] = app.at(i)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("recoverFixture: Close: %v", err)
	}
	return got, rec, nil
}

// visibleAt reports how many entries of the accepted history had completed their
// commit fsync by the time the file was `size` bytes long -- that is, how many
// had been acknowledged. It is the EXACT expected recovery result for a file cut
// at that offset: every entry at or below it was acknowledged and must survive,
// and every entry above it was not and must not appear.
func visibleAt(ackedAt []int64, size int64) int {
	n := 0
	for _, at := range ackedAt {
		if at <= size {
			n++
		}
	}
	return n
}

// TestCrashInjectionTruncationPrefixSweep is the load-bearing evidence for
// invariants 4 and 5.
//
// It takes a real, fsynced accepted history and cuts it at EVERY BYTE OFFSET
// from 0 to the end of the file -- every point inside every length field, index,
// type, checksum and payload of both phases -- and asserts, for each one, the
// exact recovery result:
//
//	recovered == acked[:visibleAt(cut)]
//
// Both directions of that equality are a durability guarantee. Fewer entries
// than that means an ACKNOWLEDGED write was lost. More means an entry that was
// never acknowledged -- because its commit fsync had not returned when the file
// ended -- became visible.
//
// A cut at an arbitrary offset is precisely what a power loss during any append
// can leave, which is why "every offset" is the right quantifier: it covers a
// crash in phase one and a crash in phase two without having to decide in
// advance which offsets are interesting.
func TestCrashInjectionTruncationPrefixSweep(t *testing.T) {
	pristine, acked, ackedAt := acceptedHistory(t)
	full := readFile(t, pristine)
	parent := t.TempDir()

	for cut := 0; cut <= len(full); cut++ {
		cut := int64(cut)
		dir, path := crashFixture(t, parent, int(cut), full)
		truncate(t, path, cut)

		got, rec, err := recoverFixture(t, dir)

		// A cut inside the 16-byte FILE header leaves a file whose layout cannot
		// be established at all. Refusing to start is the only safe answer and is
		// an accepted outcome; so is treating it as an empty log, since no entry
		// can have been acknowledged by then either way. Everything from the file
		// header onwards MUST start, because every such cut leaves a torn tail and
		// nothing else, which is exactly what RepairTail exists to handle.
		if err != nil {
			if cut >= FileHeaderSize {
				t.Fatalf("cut at offset %d of %d: Open failed: %v\n"+
					"a truncation at or past the file header leaves a torn TAIL and nothing else, "+
					"so recovery must repair it and start, not refuse", cut, len(full), err)
			}
			continue
		}

		want := acked[:visibleAt(ackedAt, cut)]
		if !sameCommitted(got, want) {
			t.Fatalf("cut at offset %d of %d: recovery yielded %s, want %s\n"+
				"the file ended at %d, so exactly %d entries had completed their commit fsync; "+
				"fewer means an ACKNOWLEDGED write was lost, more means an UNACKNOWLEDGED write became visible\n"+
				"recovered: %+v",
				cut, len(full), showCommitted(got), showCommitted(want), cut, len(want), rec)
		}
		// An entry that was cut away must never come back under a reissued index
		// that a survivor already holds.
		if n := uint64(2 * len(want)); rec.NextIndex <= n {
			t.Fatalf("cut at offset %d: NextIndex = %d, but %d records survived: the next append would overwrite an index that is already in the file",
				cut, rec.NextIndex, n)
		}
	}
}

// TestCrashInjectionTruncationIsResumable proves the other half of "a valid
// prefix": the recovered log is not merely readable, it is a log a server can go
// on writing to. For every cut it restarts, appends a fresh entry, and re-reads
// the file from scratch -- the survivors must still be there, the new entry must
// follow them, and no index may appear twice.
//
// This is separate from the sweep above because it is the assertion that would
// catch a recovery that produced the right in-memory answer while leaving the
// writer positioned at the wrong offset -- which would silently overwrite
// acknowledged history on the very next write.
func TestCrashInjectionTruncationIsResumable(t *testing.T) {
	pristine, acked, ackedAt := acceptedHistory(t)
	full := readFile(t, pristine)
	parent := t.TempDir()

	// Every frame boundary, plus one byte either side of it, plus the midpoint of
	// each frame: the shapes that a torn write can leave, without paying for a
	// full byte-by-byte restart-and-append sweep.
	recs, _, err := ScanAll(pristine, KindWAL)
	if err != nil {
		t.Fatalf("the fixture is not well framed: %v", err)
	}
	var cuts []int64
	for _, r := range recs {
		mid := r.Offset + FrameHeaderSize + int64(len(r.Payload))/2
		cuts = append(cuts, r.Offset-1, r.Offset, r.Offset+1, r.Offset+FrameHeaderSize, mid)
	}
	cuts = append(cuts, int64(len(full)))

	for i, cut := range cuts {
		if cut < FileHeaderSize || cut > int64(len(full)) {
			continue
		}
		dir, path := crashFixture(t, parent, i, full)
		truncate(t, path, cut)

		want := acked[:visibleAt(ackedAt, cut)]

		app := &testApplier{}
		l, err := Open(LogOptions{Dir: dir, Applier: app, Now: crashInjectionClock()})
		if err != nil {
			t.Fatalf("cut at offset %d: Open: %v", cut, err)
		}
		got := make([]Committed, app.count())
		for j := range got {
			got[j] = app.at(j)
		}
		if !sameCommitted(got, want) {
			l.Close()
			t.Fatalf("cut at offset %d: recovery yielded %s, want %s", cut, showCommitted(got), showCommitted(want))
		}

		// Carry on writing, the way a restarted server does.
		fresh := Entry{Kind: "message", Body: json.RawMessage(`{"after":"recovery"}`)}
		c, err := l.Write(fresh)
		if err != nil {
			l.Close()
			t.Fatalf("cut at offset %d: Write after recovery: %v", cut, err)
		}
		if err := l.Close(); err != nil {
			t.Fatalf("cut at offset %d: Close: %v", cut, err)
		}

		// Re-read the file from nothing: the survivors, then the new entry, in
		// that order and nothing else.
		var coll collector
		if _, err := Replay(path, coll.fn); err != nil {
			t.Fatalf("cut at offset %d: Replay of the resumed log: %v", cut, err)
		}
		wantAll := append(append([]Committed{}, want...), c)
		if !sameCommitted(coll.got, wantAll) {
			t.Fatalf("cut at offset %d: the resumed log holds %s, want %s: recovery must produce a log that can be appended to",
				cut, showCommitted(coll.got), showCommitted(wantAll))
		}
		assertIndicesUnique(t, path)
	}
}

// sameEntry compares one delivered entry with one expected entry.
func sameEntry(a, b Committed) bool { return sameCommitted([]Committed{a}, []Committed{b}) }

// isSubsequence reports whether every element of a appears in b, in order.
//
// SUBSEQUENCE, not prefix, is the shape recovery produces after the 2026-08-02
// policy change: a damaged record in the middle of a log is discarded and the
// intact records BEHIND it are kept, so what comes back is accepted history with
// HOLES in it rather than a truncation of it. What must still be impossible is an
// entry that was never acknowledged appearing, an entry appearing twice, or the
// order changing -- all three of which this catches.
func isSubsequence(a, b []Committed) bool {
	j := 0
	for _, x := range a {
		for j < len(b) && !sameEntry(b[j], x) {
			j++
		}
		if j == len(b) {
			return false
		}
		j++
	}
	return true
}

// notCovered returns the entries of want that do not appear in got, in order. It
// is the anti-cascade measurement: want is what the damage did NOT touch, so
// anything it names is acknowledged history recovery deleted for no reason.
func notCovered(want, got []Committed) []Committed {
	var missing []Committed
	j := 0
	for _, w := range want {
		found := false
		for ; j < len(got); j++ {
			if sameEntry(got[j], w) {
				found = true
				j++
				break
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	return missing
}

// patchedCopy returns b with the low bit of the byte at off flipped, matching
// what flipByte does to the file. It is how a refusal-to-start case checks that
// the bytes on disk were left exactly as the damage left them.
func patchedCopy(b []byte, off int64) []byte {
	out := append([]byte(nil), b...)
	out[off] ^= 0x01
	return out
}

// TestCrashInjectionSingleBitCorruptionSweep is the "corrupt the file at a chosen
// byte offset" half of DUR-6, quantified over every offset in the file.
//
// It flips ONE BIT at each offset in turn -- the shape of a bit-rot or a torn
// sector -- and asserts the two rules that survive the availability decision.
//
// WHAT IT USED TO ASSERT: recovery may discard a torn tail and NOTHING else, so
// the only acceptable answers to mid-file damage were "recover everything anyway"
// or "REFUSE TO START". The user reversed that (DECISIONS.md, 2026-08-02,
// "Availability over retention"): refusing is no longer an available answer, and
// the damaged record is discarded. So the recovered history is now accepted
// history MINUS the damaged record, which is a SUBSEQUENCE and not a prefix.
//
// The two rules, and they are what the whole file is for:
//
//	(1) NOTHING UNACKNOWLEDGED APPEARS, nothing appears twice, nothing is
//	    reordered -- recovery is a subsequence of accepted history.
//	(2) DAMAGE DOES NOT CASCADE. Every acknowledged entry whose PREPARE frame and
//	    COMMIT frame both lie OUTSIDE the flipped byte's frame must survive. Those
//	    bytes are untouched; losing one is silent, permanent loss of acknowledged
//	    history, and it is what one flipped bit in a length field used to do to
//	    eight committed records.
func TestCrashInjectionSingleBitCorruptionSweep(t *testing.T) {
	pristine, acked, _ := acceptedHistory(t)
	full := readFile(t, pristine)
	recs, _, err := ScanAll(pristine, KindWAL)
	if err != nil {
		t.Fatalf("the fixture is not well framed: %v", err)
	}
	parent := t.TempDir()

	// frameIndexAt reports the record INDEX of the frame containing an offset, or
	// 0 when the offset is in the file header.
	frameIndexAt := func(off int64) uint64 {
		for _, r := range recs {
			if off >= r.Offset && off < r.Offset+r.frameSize() {
				return r.Index
			}
		}
		return 0
	}

	type loss struct {
		off     int64
		damaged uint64
		got     []Committed
		missing []Committed
		rec     Recovered
	}
	var losses []loss

	for off := int64(0); off < int64(len(full)); off++ {
		dir, path := crashFixture(t, parent, int(off), full)
		flipByte(t, path, off)

		got, rec, err := recoverFixture(t, dir)
		if err != nil {
			// STILL FATAL is now a very short list, and exactly ONE region of
			// this file is in it: the 4-byte FORMAT VERSION at [8,12). A version
			// number under our own magic is not damage -- it says the file was
			// written by a layout this binary does not implement -- and guessing
			// at that layout is how a downgrade eats a log. Everything else,
			// including the magic and the header checksum on either side of it,
			// is damage and must be repaired into a running server.
			if off >= 8 && off < 12 && strings.Contains(err.Error(), "this binary does not implement that layout") {
				if after := readFile(t, path); !bytes.Equal(after, patchedCopy(full, off)) {
					t.Fatalf("bit flip at offset %d: a refusal to start CHANGED the file; the bytes are evidence", off)
				}
				continue
			}
			t.Fatalf("bit flip at offset %d of %d: Open failed: %v\n"+
				"damage is never fatal (DECISIONS.md 2026-08-02): the record is discarded, logged, and the server starts",
				off, len(full), err)
		}

		// (1) Whatever it serves is a subsequence of accepted history.
		if !isSubsequence(got, acked) {
			t.Fatalf("bit flip at offset %d: recovery served %s, which is not a subsequence of the accepted history %s: "+
				"an entry that was never acknowledged became visible, or entries were reordered or duplicated",
				off, showCommitted(got), showCommitted(acked))
		}

		// (2) The anti-cascade rule. One flipped bit damages ONE frame; every
		// entry built out of other frames is untouched and must come back.
		damaged := frameIndexAt(off)
		var mustSurvive []Committed
		for _, c := range acked {
			if c.PrepareIndex != damaged && c.CommitIndex != damaged {
				mustSurvive = append(mustSurvive, c)
			}
		}
		if lost := notCovered(mustSurvive, got); len(lost) > 0 {
			losses = append(losses, loss{off: off, damaged: damaged, got: got, missing: lost, rec: rec})
		}
	}

	if len(losses) > 0 {
		first := losses[0]
		offs := make([]int64, len(losses))
		for i, l := range losses {
			offs[i] = l.off
		}
		t.Fatalf(`ACKNOWLEDGED HISTORY WAS SILENTLY DELETED by recovery.

%d of the %d single-bit corruptions in this %d-byte log caused recovery to start
successfully while LOSING an entry that the flipped bit never touched. Those
records were not damaged: they sat in frames the flip is not even inside, whole
and checksum-valid, and recovery deleted them anyway.

First offence: bit flip at offset %d, inside the frame carrying record index %d.
  recovery served %s
  entries lost that the damage did not touch: %s
  Recovered = %+v

Discarding the DAMAGED record is sanctioned (DECISIONS.md 2026-08-02).
Deleting the intact records behind it is the cascade resyncFrom exists to stop,
and it is a violation of invariant 4 (nothing acknowledged is ever lost).

All offending offsets: %v`,
			len(losses), len(full), len(full),
			first.off, first.damaged, showCommitted(first.got), showCommitted(first.missing), first.rec,
			offs)
	}
}

// ---------------------------------------------------------------------------
// Stage 3: a REAL process kill with a half-written COMMIT record.
//
// replay_crash_test.go kills a process with a torn PREPARE frame on the tail.
// This is the other phase, and it is the one that matters most: the PREPARE is
// fsynced, so the entry exists on disk in full, and only the COMMIT record --
// the thing that makes it accepted history -- was cut in half. Recovery must
// therefore make it INVISIBLE. Getting this backwards would surface an entry no
// caller was ever told about.
// ---------------------------------------------------------------------------

const (
	envCIPoint = "WAL_CI_CRASH_POINT"
	envCIDir   = "WAL_CI_CRASH_DIR"

	// ciMidCommitWrite: two entries acknowledged, a third prepared and fsynced,
	// and a PARTIAL commit frame on the end of the file.
	ciMidCommitWrite = "mid-commit-write"
)

// TestCrashInjectionChild is the child half of the kill below. Without
// envCIPoint it does nothing, so it costs a normal run nothing but a skip.
func TestCrashInjectionChild(t *testing.T) {
	point := os.Getenv(envCIPoint)
	if point == "" {
		t.Skip("not a crash child: " + envCIPoint + " is unset")
	}
	dir := os.Getenv(envCIDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envCIPoint, point, envCIDir)
	}
	if point != ciMidCommitWrite {
		t.Fatalf("child: unknown crash point %q", point)
	}

	l, err := Open(LogOptions{Dir: dir, Now: crashInjectionClock()})
	if err != nil {
		t.Fatalf("child: Open: %v", err)
	}
	// Two entries all the way through the real path: acknowledged history.
	for i, e := range crashInjectionEntries[:2] {
		if _, err := l.Write(e); err != nil {
			t.Fatalf("child: Write %d: %v", i, err)
		}
	}
	// Phase one of a third. Begin returns only after the PREPARE record is
	// fsynced, so record 5 is durable and complete.
	txn, err := l.Begin(crashInjectionEntries[2])
	if err != nil {
		t.Fatalf("child: Begin: %v", err)
	}

	// Phase two, interrupted. See the long note at crashMidFrameWrite in
	// replay_crash_test.go for why the torn bytes are written deliberately: a
	// SIGKILL cannot tear a write on its own, because the page cache outlives the
	// process. The bytes below are the exact bytes Commit was about to put on the
	// platter -- same encoder, same index -- stopped part way through the payload.
	// What the kill contributes, and it cannot be faked, is that nothing graceful
	// runs afterwards: no Close, no Sync, no defer, no runtime shutdown.
	payload, err := encodeCommit(txn.PrepareIndex())
	if err != nil {
		t.Fatalf("child: encodeCommit: %v", err)
	}
	frame := encodeFrame(txn.PrepareIndex()+1, TypeCommit, payload)
	partial := FrameHeaderSize + len(payload)/2
	if partial <= FrameHeaderSize || partial >= len(frame) {
		t.Fatalf("child: a %d-byte cut of a %d-byte frame is not a torn payload", partial, len(frame))
	}
	f, err := os.OpenFile(filepath.Join(dir, WALFileName), os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		t.Fatalf("child: OpenFile to append the torn commit: %v", err)
	}
	if _, err := f.Write(frame[:partial]); err != nil {
		t.Fatalf("child: writing the torn commit frame: %v", err)
	}
	// No Close, no Sync, no defer: the next statement is the kill.
	suicide()
	t.Fatalf("child: still running after SIGKILL")
}

// runCrashInjectionChild re-execs this test binary at the given crash point and
// proves the child really died on SIGKILL rather than failing its own
// assertions -- without that check a broken child would silently turn the parent
// into a test of nothing.
func runCrashInjectionChild(t *testing.T, point, dir string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestCrashInjectionChild$", "-test.v")
	cmd.Env = append(os.Environ(), envCIPoint+"="+point, envCIDir+"="+dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child %q did not finish in time: %v\n--- child output ---\n%s", point, ctx.Err(), out.String())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child %q: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s",
			point, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child %q: wait status is %T, want syscall.WaitStatus", point, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s",
			point, ws.ExitStatus(), out.String())
	}
}

// TestCrashInjectionMidCommitWriteKill covers stage 3 of the write path with a
// genuinely killed process: the commit record was half written when the machine
// died.
//
// The prepared entry is on disk, whole and fsynced. Its commit is not. Write
// never returned, so no caller, peer or relay was ever told this entry existed.
// Recovery must repair the torn tail, keep the two acknowledged entries exactly,
// and leave the prepared one UNRESOLVED AND INVISIBLE.
func TestCrashInjectionMidCommitWriteKill(t *testing.T) {
	dir := t.TempDir()
	runCrashInjectionChild(t, ciMidCommitWrite, dir)
	path := filepath.Join(dir, WALFileName)

	// (1) The tail really is torn. Without this the rest would pass just as
	// happily against a healthy file and would prove nothing.
	if _, err := Replay(path, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Replay of the crashed log = %v, want ErrCorrupt: the child did not leave a torn commit frame", err)
	}
	tornSize := fileSize(t, path)

	got, rec, err := recoverFixture(t, dir)
	if err != nil {
		t.Fatalf("Open after a crash mid-commit: %v, want a repaired start", err)
	}

	want := []Committed{
		{PrepareIndex: 1, CommitIndex: 2, Entry: Entry{Kind: crashInjectionEntries[0].Kind, Body: crashInjectionEntries[0].Body}},
		{PrepareIndex: 3, CommitIndex: 4, Entry: Entry{Kind: crashInjectionEntries[1].Kind, Body: crashInjectionEntries[1].Body}},
	}
	if !sameCommitted(got, want) {
		t.Fatalf("recovery after a crash mid-commit served %s, want %s: the entry whose COMMIT record was torn was never acknowledged and must not be visible",
			showCommitted(got), showCommitted(want))
	}

	// (2) The repair took the torn commit frame and nothing else.
	rep := rec.Repaired
	if !rep.Truncated {
		t.Fatalf("Recovered().Repaired = %+v, want Truncated true: the log ends in a half-written commit frame", rep)
	}
	if rep.Removed <= FrameHeaderSize {
		t.Errorf("Repaired.Removed = %d, want more than the %d-byte frame header: the commit frame was torn inside its PAYLOAD",
			rep.Removed, FrameHeaderSize)
	}
	if rep.At+rep.Removed != tornSize {
		t.Errorf("Repaired.At+Removed = %d, want the pre-repair size %d", rep.At+rep.Removed, tornSize)
	}
	if got := fileSize(t, path); got != rep.At {
		t.Errorf("the file is %d bytes after the repair, want exactly At = %d", got, rep.At)
	}

	// (3) The PREPARE survives as an unresolved transaction -- record 5, still on
	// disk, deliberately not applied. It is reported so an operator can see that
	// a client was left without an answer.
	if len(rec.Dangling) != 1 || rec.Dangling[0] != 5 {
		t.Fatalf("Dangling = %v, want [5]: the prepare whose commit was torn away is unresolved, not applied and not discarded from the file",
			rec.Dangling)
	}
	if rec.Applied != 2 || rec.Aborted != 0 {
		t.Errorf("Recovered = %+v, want 2 applied and 0 aborted", rec)
	}

	// (4) The prepared entry's index is BURNED. The prepare frame completed its
	// fsync, so that index is on stable storage and must never be handed to a
	// different entry (invariant 1). The torn commit frame's index 6 may be
	// reissued -- its fsync never returned, so nothing can have observed it.
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open to resume: %v", err)
	}
	defer l.Close()
	c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"crash"}`)})
	if err != nil {
		t.Fatalf("Write after the crash: %v", err)
	}
	if c.PrepareIndex != 6 {
		t.Fatalf("the first write after the crash got prepare index %d, want 6: index 5 was fsynced as a PREPARE and must never be reissued",
			c.PrepareIndex)
	}
	assertIndicesUnique(t, path)
}

// ---------------------------------------------------------------------------
// DUR-11 -- crash injection for the DISCARD PATHS.
//
// The 2026-08-02 policy change (DECISIONS.md, "Availability over retention":
// "always be able to restart, prefer to discard messages and/or corruption, with
// logging") turned a family of REFUSALS into a family of DISCARDS. Every one of
// those is a new way for the bus to lose data on purpose, so every one of them
// needs evidence, and "recovery returned no error" is not evidence of anything.
//
// Each case below asserts all three halves, and a case that could pass on two of
// them is not written:
//
//	(a) RECOVERY RUNS -- Open returns a usable *Log and the log can be WRITTEN TO
//	    afterwards. A repair that produces a file the writer cannot append to is
//	    a repair that has broken the server in a different way.
//	(b) EXACTLY WHAT WENT -- which record indices survive (re-scanned from the
//	    file, not taken from the recovery's own report), what the Applier saw,
//	    and the counters: NextIndex, Applied, DiscardCount, MissingRecords.
//	(c) THE DISCARD WAS LOGGED -- the specific message, at the right LEVEL, with
//	    the record's index and type in it. Deleting the logging fails these; that
//	    is deliberate, and it is the whole reason discarding is allowed at all.
// ---------------------------------------------------------------------------

// logExpect is one line the operator log must carry. level "" accepts any.
type logExpect struct {
	level  string
	msg    string
	fields []string
}

// sixTxnOps is three complete transactions: records 1..6, with records 5 and 6
// (a prepare/commit pair) as acknowledged history sitting behind any mid-file
// damage.
func sixTxnOps() []walOp {
	return []walOp{
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opPrepare("message", `{"n":2}`), // 3
		opCommit(3),                     // 4
		opPrepare("message", `{"n":3}`), // 5
		opCommit(5),                     // 6
	}
}

// scanIndices reports the record indices in a log, in file order. It is how the
// survivor assertions are made against THE FILE rather than against recovery's
// own account of what it did.
func scanIndices(t *testing.T, path string, kind Kind) []uint64 {
	t.Helper()
	recs, _, err := ScanAll(path, kind)
	if err != nil {
		t.Fatalf("ScanAll after recovery: %v: a recovered log must scan clean end to end", err)
	}
	out := make([]uint64, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Index)
	}
	return out
}

func sameIndices(a, b []uint64) bool {
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

// TestCrashInjectionDiscardPathsRecoverAndLog is the table over every
// newly-sanctioned discard.
func TestCrashInjectionDiscardPathsRecoverAndLog(t *testing.T) {
	// The two transactions a six-record fixture recovers when nothing before
	// record 5 is lost.
	txn1 := wantC(1, 2, "message", `{"n":1}`)
	txn2 := wantC(3, 4, "message", `{"n":2}`)
	txn3 := wantC(5, 6, "message", `{"n":3}`)

	cases := []struct {
		name string
		// build lays the log down in dir and returns its pristine records.
		build func(t *testing.T, dir string) []Record
		// damage mutates the file.
		damage func(t *testing.T, path string, recs []Record)
		// applier, when non-nil, replaces the plain recording Applier.
		applier func() *testApplier

		wantSurvivors  []uint64 // record indices in the file after recovery
		wantApplied    []Committed
		wantNextIndex  uint64
		wantDiscards   int    // REPLAY-stage discards (Recovered.DiscardCount)
		wantFraming    int    // FRAMING-stage discards (Recovered.Repaired.DiscardCount)
		wantMissing    uint64 // holes in the index sequence
		wantQuarantine bool
		wantLog        []logExpect
		wantNote       string
	}{
		{
			// PATH 1: a torn tail -- the ordinary crash artefact, a frame that
			// was half written when the machine died.
			name: "a torn tail",
			build: func(t *testing.T, dir string) []Record {
				_, path, _, _ := buildWALIn(t, dir, sixTxnOps()...)
				recs, _, err := ScanAll(path, KindWAL)
				if err != nil {
					t.Fatalf("fixture: %v", err)
				}
				return recs
			},
			damage: func(t *testing.T, path string, recs []Record) {
				truncate(t, path, recs[5].Offset+FrameHeaderSize+2)
			},
			wantSurvivors: []uint64{1, 2, 3, 4, 5},
			wantApplied:   []Committed{txn1, txn2},
			wantNextIndex: 6,
			wantFraming:   1,
			wantLog: []logExpect{
				{"ERROR", "wal discarded a damaged record", []string{"stage=framing", "record_index=6", "record_type=commit", "discarded as a torn tail"}},
				{"WARN", "wal truncated damage at the end of the log", []string{"at=", "removed="}},
			},
			wantNote: "the prepare whose commit was torn away is unresolved and invisible; the two acknowledged entries survive",
		},
		{
			// PATH 2: a COMPLETE final record whose checksum fails. Not a torn
			// write -- the file is full length -- so this record may have been
			// fsynced and ACKNOWLEDGED. It goes anyway, at ERROR, because the bus
			// will not be held hostage to damaged media.
			name: "a checksum-failing last record",
			build: func(t *testing.T, dir string) []Record {
				_, path, _, _ := buildWALIn(t, dir, sixTxnOps()...)
				recs, _, err := ScanAll(path, KindWAL)
				if err != nil {
					t.Fatalf("fixture: %v", err)
				}
				return recs
			},
			damage: func(t *testing.T, path string, recs []Record) {
				flipByte(t, path, recs[5].Offset+FrameHeaderSize+1)
			},
			wantSurvivors: []uint64{1, 2, 3, 4, 5},
			wantApplied:   []Committed{txn1, txn2},
			wantNextIndex: 6,
			wantFraming:   1,
			wantLog: []logExpect{
				{"ERROR", "wal discarded a damaged record", []string{"stage=framing", "record_index=6", "record_type=commit", "checksum"}},
				{"WARN", "wal truncated damage at the end of the log", nil},
			},
			wantNote: "an acknowledged commit lost to bit rot must be discarded LOUDLY, and its index must not be reissued to a survivor",
		},
		{
			// PATH 3: THE CASCADE CASE. Damage in the middle with intact,
			// committed records behind it. Exactly the damaged record goes; the
			// file is REWRITTEN to remove it; the survivors keep their original
			// indices, so the log gains a permanent HOLE at index 2 -- which is
			// counted and reported on this start and on every later one.
			name: "a mid-file damaged record with intact records after it",
			build: func(t *testing.T, dir string) []Record {
				_, path, _, _ := buildWALIn(t, dir, sixTxnOps()...)
				recs, _, err := ScanAll(path, KindWAL)
				if err != nil {
					t.Fatalf("fixture: %v", err)
				}
				return recs
			},
			damage: func(t *testing.T, path string, recs []Record) {
				flipByte(t, path, recs[1].Offset+FrameHeaderSize+1)
			},
			wantSurvivors: []uint64{1, 3, 4, 5, 6},
			wantApplied:   []Committed{txn2, txn3},
			wantNextIndex: 7,
			wantFraming:   1,
			wantDiscards:  1, // the hole, reported by Replay
			wantMissing:   1,
			wantLog: []logExpect{
				{"ERROR", "wal discarded a damaged record", []string{"stage=framing", "record_index=2", "record_type=commit", "the next intact record was found at offset"}},
				{"WARN", "wal rewrote a damaged log, keeping every intact record", []string{"kept=5"}},
				{"ERROR", "wal discarded a damaged record", []string{"stage=replay", "are missing from the index sequence"}},
			},
			wantNote: "one flipped bit must cost one record, not the four committed records behind it",
		},
		{
			// PATH 4: a damaged FILE HEADER with salvageable records behind it.
			// There is nothing in a header to recover, only 16 constant bytes to
			// rewrite, so refusing to start over a flipped bit in a fixed preamble
			// would throw away an entire readable log.
			name: "a damaged file header with records behind it",
			build: func(t *testing.T, dir string) []Record {
				_, path, _, _ := buildWALIn(t, dir, sixTxnOps()...)
				recs, _, err := ScanAll(path, KindWAL)
				if err != nil {
					t.Fatalf("fixture: %v", err)
				}
				return recs
			},
			damage: func(t *testing.T, path string, recs []Record) {
				patch(t, path, 0, []byte("XGNTBUSW"))
			},
			wantSurvivors: []uint64{1, 2, 3, 4, 5, 6},
			wantApplied:   []Committed{txn1, txn2, txn3},
			wantNextIndex: 7,
			wantLog: []logExpect{
				{"ERROR", "wal rebuilding a damaged file header", []string{"kind=wal", "records_salvaged=6"}},
				{"WARN", "wal rewrote a damaged log, keeping every intact record", []string{"kept=6"}},
			},
			wantNote: "every record behind the header is intact, so nothing may be lost to a damaged preamble",
		},
		{
			// PATH 6: an undecodable PREPARE payload, and the COMMIT that named
			// it. Two losses of different severity from one fault, which is why
			// the levels are asserted separately: the prepare acknowledged
			// nothing (WARN), the commit acknowledged a write to a client
			// (ERROR).
			name: "an undecodable prepare payload",
			build: func(t *testing.T, dir string) []Record {
				_, path, _, _ := buildWALIn(t, dir,
					opRaw(TypePrepare, `this is not a prepare payload`), // 1
					opCommit(1),                     // 2
					opPrepare("message", `{"n":2}`), // 3
					opCommit(3),                     // 4
				)
				recs, _, err := ScanAll(path, KindWAL)
				if err != nil {
					t.Fatalf("fixture: %v", err)
				}
				return recs
			},
			damage:        func(t *testing.T, path string, recs []Record) {},
			wantSurvivors: []uint64{1, 2, 3, 4},
			wantApplied:   []Committed{txn2},
			wantNextIndex: 5,
			wantDiscards:  2,
			wantLog: []logExpect{
				{"WARN", "wal discarded a damaged record", []string{"stage=replay", "record_index=1", "record_type=prepare", "what this record reserved cannot be known"}},
				{"ERROR", "wal discarded a damaged record", []string{"stage=replay", "record_index=2", "record_type=commit", "an acknowledged write is lost here"}},
			},
			wantNote: "the FILE is untouched -- a payload that will not decode says nothing about the framing",
		},
		{
			// PATH 7: a COMMIT naming no open prepare. The transaction behind it
			// must survive: one unusable record costs one record.
			name: "a commit naming no open prepare",
			build: func(t *testing.T, dir string) []Record {
				_, path, _, _ := buildWALIn(t, dir,
					opPrepare("message", `{"n":1}`), // 1
					opCommit(1),                     // 2
					opCommit(1),                     // 3 -- already committed
					opPrepare("message", `{"n":2}`), // 4
					opCommit(4),                     // 5
				)
				recs, _, err := ScanAll(path, KindWAL)
				if err != nil {
					t.Fatalf("fixture: %v", err)
				}
				return recs
			},
			damage:        func(t *testing.T, path string, recs []Record) {},
			wantSurvivors: []uint64{1, 2, 3, 4, 5},
			wantApplied:   []Committed{txn1, wantC(4, 5, "message", `{"n":2}`)},
			wantNextIndex: 6,
			wantDiscards:  1,
			wantLog: []logExpect{
				{"ERROR", "wal discarded a damaged record", []string{"stage=replay", "record_index=3", "record_type=commit", "not an open prepare"}},
			},
			wantNote: "a duplicate commit must never re-apply an entry, and must never cost the transaction after it",
		},
		{
			// PATH 8: an unknown record type in a WAL. scanFrom accepts it (its
			// checksum proves a writer meant those bytes); replay cannot know what
			// it did to accepted history, so it is discarded -- and its INDEX IS
			// STILL BURNED, because an id on stable storage is never reissued.
			name: "an unknown record type in a WAL",
			build: func(t *testing.T, dir string) []Record {
				_, path, _, _ := buildWALIn(t, dir,
					opPrepare("message", `{"n":1}`), // 1
					opCommit(1),                     // 2
				)
				appendRawFrame(t, path, 3, Type(4242), []byte(`{}`))
				recs, _, err := ScanAll(path, KindWAL)
				if err != nil {
					t.Fatalf("fixture: %v", err)
				}
				return recs
			},
			damage:        func(t *testing.T, path string, recs []Record) {},
			wantSurvivors: []uint64{1, 2, 3},
			wantApplied:   []Committed{txn1},
			wantNextIndex: 4,
			wantDiscards:  1,
			wantLog: []logExpect{
				{"WARN", "wal discarded a damaged record", []string{"stage=replay", "record_index=3", "record_type=unknown(4242)", "have no meaning in a write-ahead log"}},
			},
			wantNote: "a record type this binary cannot interpret must not stop the start, and must not have its index reissued",
		},
		{
			// PATH 9: the APPLIER rejects a committed entry. The sharpest edge of
			// the availability decision: the entry is durable on disk and absent
			// from memory, which is a real divergence -- so the whole weight rests
			// on the ERROR line naming it.
			name: "an applier that rejects a committed entry",
			build: func(t *testing.T, dir string) []Record {
				_, path, _, _ := buildWALIn(t, dir, sixTxnOps()...)
				recs, _, err := ScanAll(path, KindWAL)
				if err != nil {
					t.Fatalf("fixture: %v", err)
				}
				return recs
			},
			damage: func(t *testing.T, path string, recs []Record) {},
			applier: func() *testApplier {
				return &testApplier{check: func(c Committed) error {
					if c.PrepareIndex == 3 {
						return errors.New("the roster rejected a replayed entry")
					}
					return nil
				}}
			},
			wantSurvivors: []uint64{1, 2, 3, 4, 5, 6},
			// The rejected entry is offered and refused; the ones on either side
			// of it are applied. wantApplied is compared against the OFFERS, so
			// all three appear -- what matters is that the entry after the
			// rejection was offered at all.
			wantApplied:   []Committed{txn1, txn2, txn3},
			wantNextIndex: 7,
			wantDiscards:  1,
			wantLog: []logExpect{
				{"ERROR", "wal discarded a damaged record", []string{"stage=replay", "record_index=4", "record_type=commit",
					"the applier rejected this committed entry", "durable on disk but absent from the rebuilt memory state"}},
			},
			wantNote: "a caller-side rejection must not stop the start and must not stop the entries behind it being applied",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, WALFileName)
			recs := tc.build(t, dir)
			tc.damage(t, path, recs)

			app := &testApplier{}
			if tc.applier != nil {
				app = tc.applier()
			}
			var buf bytes.Buffer

			// (a) RECOVERY RUNS.
			l, err := Open(LogOptions{Dir: dir, Applier: app, Logger: logging.New(&buf, logging.LevelDebug)})
			if err != nil {
				t.Fatalf("Open: %v\ndamage is never fatal (DECISIONS.md 2026-08-02): %s", err, tc.wantNote)
			}
			rec := l.Recovered()
			out := buf.String()

			// (b) EXACTLY WHAT WENT.
			got := make([]Committed, app.count())
			for i := range got {
				got[i] = app.at(i)
			}
			if !sameCommitted(got, tc.wantApplied) {
				_ = l.Close()
				t.Fatalf("recovery applied %s, want %s: %s", showCommitted(got), showCommitted(tc.wantApplied), tc.wantNote)
			}
			if rec.NextIndex != tc.wantNextIndex {
				t.Errorf("Recovered().NextIndex = %d, want %d", rec.NextIndex, tc.wantNextIndex)
			}
			if rec.DiscardCount != tc.wantDiscards {
				t.Errorf("Recovered().DiscardCount = %d, want %d (replay stage): %+v", rec.DiscardCount, tc.wantDiscards, rec.Discarded)
			}
			if rec.Repaired.DiscardCount != tc.wantFraming {
				t.Errorf("Recovered().Repaired.DiscardCount = %d, want %d (framing stage): %+v",
					rec.Repaired.DiscardCount, tc.wantFraming, rec.Repaired.Discards)
			}
			if rec.MissingRecords != tc.wantMissing {
				t.Errorf("Recovered().MissingRecords = %d, want %d: a hole in the index sequence is reported on EVERY start, "+
					"not only the one that made it", rec.MissingRecords, tc.wantMissing)
			}

			// (a) again: the recovered log is one a server can go on writing to.
			fresh, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"recovery"}`)})
			if err != nil {
				_ = l.Close()
				t.Fatalf("Write after recovery: %v: a repaired log must be appendable", err)
			}
			if fresh.PrepareIndex != tc.wantNextIndex {
				t.Errorf("the write after recovery got prepare index %d, want %d: the next append starts one past the "+
					"highest index that SURVIVED, and never reuses a survivor's", fresh.PrepareIndex, tc.wantNextIndex)
			}
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// (b) again, measured against the FILE rather than against recovery's
			// own account of itself.
			wantIdx := append(append([]uint64{}, tc.wantSurvivors...), tc.wantNextIndex, tc.wantNextIndex+1)
			if gotIdx := scanIndices(t, path, KindWAL); !sameIndices(gotIdx, wantIdx) {
				t.Fatalf("the recovered log holds records %v, want %v (survivors keep their ORIGINAL indices, so a repaired "+
					"log has HOLES, and the two new records follow them): %s", gotIdx, wantIdx, tc.wantNote)
			}
			assertIndicesUnique(t, path)

			// (c) THE DISCARD WAS LOGGED.
			for _, want := range tc.wantLog {
				assertLogged(t, out, want.level, want.msg, want.fields...)
			}
		})
	}
}

// TestCrashInjectionQuarantinesAnUninterpretableFile is the discard path that
// cannot be a table row, because the file it recovers from is GONE from its old
// name afterwards.
//
// A file whose header does not verify and from which not one record can be
// salvaged is not a log this code can make anything of. Refusing to start on it
// for ever is not an option under the availability decision, and deleting it is
// not an option either -- a file THIS code cannot read is not necessarily a file
// NOBODY can read, and an operator with a hex editor is owed the bytes. So it is
// RENAMED aside and startup continues with a fresh log.
//
// The assertion that matters most here is the one about the original bytes: a
// quarantine that "tidied up" by unlinking would pass every other check in this
// file.
func TestCrashInjectionQuarantinesAnUninterpretableFile(t *testing.T) {
	dir := t.TempDir()
	_, path, _, _ := buildWALIn(t, dir, sixTxnOps()...)

	// Nine bytes: too short even for the 16-byte file header, so the layout
	// cannot be established and there is no record anywhere behind it.
	truncate(t, path, 9)
	original := readFile(t, path)

	app := &testApplier{}
	var buf bytes.Buffer
	l, err := Open(LogOptions{Dir: dir, Applier: app, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open on an uninterpretable log: %v: startup must continue with a fresh one", err)
	}
	rec := l.Recovered()
	out := buf.String()

	if app.count() != 0 {
		_ = l.Close()
		t.Fatalf("recovery applied %d entries from a file it could not interpret, want 0", app.count())
	}
	if rec.NextIndex != 1 || rec.Records != 0 {
		t.Errorf("Recovered() = {NextIndex:%d Records:%d}, want a fresh log {1 0}", rec.NextIndex, rec.Records)
	}
	dest := rec.Repaired.Quarantined
	if dest == "" {
		_ = l.Close()
		t.Fatalf("Recovered().Repaired = %+v, want the file moved aside", rec.Repaired)
	}

	// RENAMED, NEVER DELETED. This is the assertion a "tidying" implementation
	// would fail and every other one here would not.
	kept, err := os.ReadFile(dest)
	if err != nil {
		_ = l.Close()
		t.Fatalf("the quarantined file %s cannot be read back: %v: quarantine RENAMES, it never deletes", dest, err)
	}
	if !bytes.Equal(kept, original) {
		_ = l.Close()
		t.Fatalf("the quarantined file holds %q, want the original %q verbatim: an operator is owed the bytes even when "+
			"this code can make nothing of them", kept, original)
	}
	if !strings.HasPrefix(filepath.Base(dest), WALFileName+".corrupt-") {
		t.Errorf("the quarantined file is named %q, want %q with a timestamp: repeated quarantines must not overwrite each other",
			filepath.Base(dest), WALFileName+".corrupt-")
	}

	// (a) The fresh log is usable.
	fresh, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"quarantine"}`)})
	if err != nil {
		_ = l.Close()
		t.Fatalf("Write after a quarantine: %v", err)
	}
	if fresh.PrepareIndex != 1 || fresh.CommitIndex != 2 {
		t.Errorf("the first write after a quarantine got {prepare:%d commit:%d}, want {1 2}: the fresh log starts at 1",
			fresh.PrepareIndex, fresh.CommitIndex)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if gotIdx := scanIndices(t, path, KindWAL); !sameIndices(gotIdx, []uint64{1, 2}) {
		t.Errorf("the fresh log holds records %v, want [1 2]", gotIdx)
	}

	// (c) LOGGED, at ERROR, naming both paths -- an operator cannot go looking
	// for bytes whose new name was never written down.
	assertLogged(t, out, "ERROR", "wal quarantined an unreadable log and started a fresh one",
		"path="+path, "moved_to="+dest, "bytes=9")
}

// TestCrashInjectionEvictsTooManyUnresolvedPrepares covers the one discard that
// is a defence rather than a repair.
//
// Replay retains every prepare it has not paired with a commit or an abort, and
// it builds that set from a file it has no reason to trust yet. Hitting the bound
// used to REFUSE the start, on the reasoning that a boot-time OOM would survive
// every restart. Under "always be able to restart" that answer is gone -- and so
// is allocating -- so the OLDEST unresolved prepares are EVICTED.
//
// Why that is safe, stated because the test depends on it: AN UNRESOLVED PREPARE
// NEVER COMMITTED, so nothing about it was ever acknowledged and evicting one
// loses nothing a client was promised. What is NOT given up is the index: every
// evicted prepare still burns its own, so an id that reached stable storage is
// never handed to a second record.
func TestCrashInjectionEvictsTooManyUnresolvedPrepares(t *testing.T) {
	// Bodies just under MaxPayloadSize, so the BYTE bound trips after a few dozen
	// records rather than needing 1024 fsynced appends.
	body := `"` + strings.Repeat("a", MaxPayloadSize-512) + `"`
	const kind = "m"
	perRecord := int64(len(kind) + len(body))
	n := int(maxOpenPrepareBytes/perRecord) + 2

	dir := t.TempDir()
	ops := make([]walOp, 0, n)
	for i := 0; i < n; i++ {
		ops = append(ops, opPrepare(kind, body))
	}
	_, path, _, _ := buildWALIn(t, dir, ops...)

	app := &testApplier{}
	var buf bytes.Buffer
	l, err := Open(LogOptions{Dir: dir, Applier: app, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open on %d unresolved prepares: %v: a file this server did not write must not stop it starting", n, err)
	}
	rec := l.Recovered()
	out := buf.String()

	// (b) Nothing became visible -- not one of these prepares ever committed --
	// and the eviction is reported.
	if app.count() != 0 {
		_ = l.Close()
		t.Fatalf("recovery applied %d entries, want 0: an unresolved prepare is never accepted history", app.count())
	}
	if rec.DiscardCount == 0 {
		_ = l.Close()
		t.Fatalf("Recovered() = %+v, want the evictions reported: a bound that evicts silently turns a hostile file into "+
			"quiet data loss", rec)
	}
	d := rec.Discarded[0]
	if d.Stage != "replay" || d.Type != TypePrepare || d.Index != 1 {
		t.Errorf("Recovered().Discarded[0] = %+v, want the eviction of PREPARE record 1 -- the OLDEST, which is the one "+
			"furthest from ever being resolved", d)
	}
	// EVERY index is still burned. Eviction frees memory, never ids.
	if rec.NextIndex != uint64(n)+1 {
		t.Errorf("Recovered().NextIndex = %d, want %d: an evicted prepare still burned its index", rec.NextIndex, n+1)
	}
	// The file is UNTOUCHED: this is a memory bound, not a repair.
	if rec.Repaired.Truncated || rec.Repaired.Rewritten || rec.Repaired.Quarantined != "" {
		t.Errorf("Recovered().Repaired = %+v, want no repair: the framing of this file is perfect", rec.Repaired)
	}

	// (a) It is still a log a server can write to, and the new entry follows
	// every burned index.
	fresh, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"eviction"}`)})
	if err != nil {
		_ = l.Close()
		t.Fatalf("Write after an eviction: %v", err)
	}
	if fresh.PrepareIndex != uint64(n)+1 {
		t.Errorf("the write after recovery got prepare index %d, want %d", fresh.PrepareIndex, n+1)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertIndicesUnique(t, path)

	// (c) LOGGED. An eviction is WARN: nothing here was ever acknowledged.
	assertLogged(t, out, "WARN", "wal discarded a damaged record",
		"stage=replay", "record_type=prepare", "evicted the oldest unresolved prepare")
}

// TestCrashInjectionMidFileDamageDoesNotCascade is the regression test for the
// exact fault a reviewer's probe found, reproduced rather than described.
//
// THE BUG. One bit flipped in the LENGTH field of a mid-file record makes that
// frame declare an extent running past the end of the file, which is
// byte-for-byte the shape of a torn tail. Add ONE JUNK BYTE at the end -- and a
// torn tail is not a rare independent second fault, it is the NORMAL state of
// every file recovery is called on -- and the old "a following record ends
// exactly at EOF" anchor found nothing to stop the cut. Recovery deleted EIGHT
// COMMITTED RECORDS and dropped NextIndex from 41 to 33, with no error at all:
// silent, permanent loss of acknowledged history.
//
// THE FIX being pinned: the forward search is anchored on the RECORD INDEX, not
// on the end of the file (resyncFrom), so it finds the next intact record whether
// or not the file ends cleanly.
//
// Two variants, because they exercise different halves of the answer:
//
//   - the length field ALONE is damaged. The record's own checksum, recomputed
//     over the bytes actually present, proves it complete, so it is REBUILT and
//     nothing at all is lost but the junk byte.
//   - the length field AND the payload are damaged (a double fault). No checksum
//     can rescue that record, so EXACTLY ONE record is discarded -- and the
//     seven behind it still survive.
func TestCrashInjectionMidFileDamageDoesNotCascade(t *testing.T) {
	const txns = 20          // 40 records: 20 prepare/commit pairs
	const damagedIndex = 33  // a mid-file record with 8 records behind it
	const finalIndex = 40    // the highest index the fixture writes
	const wantNextIndex = 41 // one past it: the reviewer's probe saw 33 here

	buildFixture := func(t *testing.T) (dir, path string, recs []Record, acked []Committed) {
		t.Helper()
		dir = t.TempDir()
		l, err := Open(LogOptions{Dir: dir, Now: crashInjectionClock()})
		if err != nil {
			t.Fatalf("fixture Open: %v", err)
		}
		for i := 0; i < txns; i++ {
			c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
			if err != nil {
				t.Fatalf("fixture Write %d: %v", i, err)
			}
			acked = append(acked, c)
		}
		if err := l.Close(); err != nil {
			t.Fatalf("fixture Close: %v", err)
		}
		path = filepath.Join(dir, WALFileName)
		recs, _, err = ScanAll(path, KindWAL)
		if err != nil {
			t.Fatalf("the fixture is not well framed: %v", err)
		}
		if len(recs) != 2*txns || recs[len(recs)-1].Index != finalIndex {
			t.Fatalf("the fixture has %d records ending at index %d, want %d ending at %d",
				len(recs), recs[len(recs)-1].Index, 2*txns, finalIndex)
		}
		return dir, path, recs, acked
	}

	// flipLengthBit sets bit 16 of the record's payload length, the way a bad
	// sector or a cosmic ray delivers it: ~65 KiB, comfortably legal and
	// comfortably past the end of a small log.
	flipLengthBit := func(t *testing.T, path string, rec Record, size int64) {
		t.Helper()
		flipped := uint32(len(rec.Payload)) ^ 0x00010000
		if flipped > MaxPayloadSize {
			t.Fatalf("the flipped length %d is over MaxPayloadSize; this case must reach the forward search", flipped)
		}
		if int64(flipped) <= size-rec.Offset-FrameHeaderSize {
			t.Fatalf("the flipped length %d does not overshoot the %d-byte file; this case would not look like a torn tail",
				flipped, size)
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], flipped)
		patch(t, path, rec.Offset, b[:])
	}

	t.Run("the length field alone: the record is rebuilt, nothing is lost", func(t *testing.T) {
		dir, path, recs, acked := buildFixture(t)
		bad := recs[damagedIndex-1]
		if bad.Index != damagedIndex {
			t.Fatalf("record %d has index %d", damagedIndex, bad.Index)
		}
		flipLengthBit(t, path, bad, fileSize(t, path))
		// ... and the second half of the fault: the file no longer ends on a
		// record boundary, which is what defeated the original fix.
		appendBytes(t, path, []byte{0x7b})

		got, rec, out, err := openCapturing(t, dir)
		if err != nil {
			t.Fatalf("Open: %v: damage is never fatal", err)
		}

		// THE ASSERTION THE PROBE FAILED. NextIndex must still be 41; the probe
		// saw 33 because eight committed records had been deleted.
		if rec.NextIndex != wantNextIndex {
			t.Fatalf("NextIndex = %d, want %d: %d committed records were deleted by ONE flipped bit in a length field",
				rec.NextIndex, wantNextIndex, (int64(wantNextIndex)-int64(rec.NextIndex))/2)
		}
		// Only the length was wrong, so the record's own checksum proves it
		// complete and it is RECOVERED: the whole accepted history comes back.
		if !sameCommitted(got, acked) {
			t.Fatalf("recovery served %d of the %d acknowledged entries: only a LENGTH field was damaged, and the record's "+
				"own checksum over the bytes present proves every one of them is still there",
				len(got), len(acked))
		}
		if rec.Repaired.Rebuilt != 1 {
			t.Errorf("Repaired.Rebuilt = %d, want 1: the damaged record must be restored, not discarded", rec.Repaired.Rebuilt)
		}
		if rec.MissingRecords != 0 {
			t.Errorf("MissingRecords = %d, want 0: nothing was discarded, so the index sequence has no hole", rec.MissingRecords)
		}
		// Every record is still in the file, in order, with no hole.
		want := make([]uint64, 0, 2*txns)
		for i := uint64(1); i <= finalIndex; i++ {
			want = append(want, i)
		}
		if gotIdx := scanIndices(t, path, KindWAL); !sameIndices(gotIdx, want) {
			t.Fatalf("the repaired log holds records %v, want all of 1..%d", gotIdx, finalIndex)
		}
		// The only thing thrown away is the junk byte, and it is logged as the
		// unidentifiable region it is.
		if rec.Repaired.DiscardCount != 1 {
			t.Errorf("Repaired.DiscardCount = %d, want exactly 1 (the junk byte): %+v", rec.Repaired.DiscardCount, rec.Repaired.Discards)
		}
		assertLogged(t, out, "ERROR", "wal discarded a damaged record",
			"stage=framing", "bytes=1", "record_type=unreadable", "record_index=unknown")
		assertLogged(t, out, "WARN", "wal restored records whose length field was corrupt but whose checksum proved them complete",
			"records=1")
	})

	t.Run("length and payload both damaged: exactly one record goes", func(t *testing.T) {
		dir, path, recs, acked := buildFixture(t)
		bad := recs[damagedIndex-1]
		// The DOUBLE FAULT: the payload is damaged too, so no recomputed checksum
		// can prove the record complete and it genuinely cannot be recovered.
		flipByte(t, path, bad.Offset+FrameHeaderSize+1)
		flipLengthBit(t, path, bad, fileSize(t, path))
		appendBytes(t, path, []byte{0x7b})

		got, rec, out, err := openCapturing(t, dir)
		if err != nil {
			t.Fatalf("Open: %v: damage is never fatal", err)
		}

		// Still 41. The record that was really damaged is gone; the SEVEN intact
		// records behind it are not, and neither is the high-water mark.
		if rec.NextIndex != wantNextIndex {
			t.Fatalf("NextIndex = %d, want %d: discarding the damaged record must not move the high-water mark backwards, "+
				"because the records behind it survive", rec.NextIndex, wantNextIndex)
		}
		want := make([]uint64, 0, 2*txns)
		for i := uint64(1); i <= finalIndex; i++ {
			if i == damagedIndex {
				continue // the permanent HOLE: survivors are never renumbered
			}
			want = append(want, i)
		}
		if gotIdx := scanIndices(t, path, KindWAL); !sameIndices(gotIdx, want) {
			t.Fatalf("the repaired log holds records %v, want 1..%d WITHOUT %d: exactly the damaged record goes, and the "+
				"survivors keep their original indices", gotIdx, finalIndex, damagedIndex)
		}
		if rec.Repaired.DiscardCount != 2 {
			t.Errorf("Repaired.DiscardCount = %d, want 2 (the damaged record and the junk byte): %+v",
				rec.Repaired.DiscardCount, rec.Repaired.Discards)
		}
		if rec.Repaired.Rebuilt != 0 {
			t.Errorf("Repaired.Rebuilt = %d, want 0: a record whose PAYLOAD is damaged cannot be proved complete", rec.Repaired.Rebuilt)
		}
		if rec.MissingRecords != 1 {
			t.Errorf("MissingRecords = %d, want 1: the hole at index %d is reported on every start", rec.MissingRecords, damagedIndex)
		}

		// Record 33 is a PREPARE (odd indices are prepares), so the transaction
		// it belonged to is lost and its COMMIT (record 34) is a dangling
		// reference -- an acknowledged write, reported at ERROR. Every OTHER
		// transaction survives.
		var wantEntries []Committed
		for _, c := range acked {
			if c.PrepareIndex == damagedIndex || c.CommitIndex == damagedIndex {
				continue
			}
			wantEntries = append(wantEntries, c)
		}
		if !sameCommitted(got, wantEntries) {
			t.Fatalf("recovery served %s\nwant %s: exactly the transaction built on the damaged record is lost",
				showCommitted(got), showCommitted(wantEntries))
		}
		assertLogged(t, out, "WARN", "wal discarded a damaged record",
			"stage=framing", "record_index=33", "record_type=prepare", "the next intact record was found at offset")
		assertLogged(t, out, "ERROR", "wal discarded a damaged record",
			"stage=replay", "record_index=34", "record_type=commit", "an acknowledged write is lost here")
		assertLogged(t, out, "WARN", "wal rewrote a damaged log, keeping every intact record", "kept=39")
	})
}
