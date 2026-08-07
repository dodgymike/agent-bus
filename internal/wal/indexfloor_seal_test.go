package wal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// THE `sealed` BIT: the two P0 regressions, and the property it buys back.
//
// A reviewer returned the durable index-floor work CHANGES-REQUESTED with two
// P0s, both PROVED WITH AN EXECUTABLE PROBE rather than argued. The defect they
// share is one line of policy: the durable CEILING was consulted only when
// recovery could PROVE the log had lost something (Repair.LostUnidentified).
//
//	P0-A  Crash with no Close, then lose bus.wal. Replay reports an empty log,
//	      nothing proves damage, and the start index is 1 -- while `reserved`
//	      sits on disk, unread, naming indices a client was already given.
//	P0-B  Truncate the log at a CLEAN FRAME BOUNDARY. That is
//	      byte-indistinguishable from a log that was simply shorter, so salvage
//	      reports no damage at all and the indices past the cut are handed out a
//	      second time. The reviewer measured 25 of 2289 truncation offsets over a
//	      12-message log doing exactly this, and they were precisely the frame
//	      boundaries.
//
// WHY THE TRIGGER HAD TO CHANGE RATHER THAN BE TIGHTENED: "did this recovery
// find damage" is NOT KNOWABLE -- the whole point of P0-B is that a legitimate
// short log and a truncated one are the same bytes. "Did the previous run close
// cleanly" IS knowable, because this process controls when it is written: begin
// fsyncs sealed 0 before the Writer may append anything, and only a clean
// Writer.Close ever sets sealed 1.
//
// Every directory here is a t.TempDir(). The tracked ./data directory is never
// touched.
// ---------------------------------------------------------------------------

// crashedFixture builds a data directory in EXACTLY the on-disk state a crash
// leaves: n transactions written through the real write path, every one fsynced
// by Append, and NO Close -- so the index floor is never sealed.
//
// HONEST LABELLING, in the same voice as the crash-injection tests: this is not
// a kill -9, and it does not claim to be. It does not need to be. The two things
// a SIGKILL contributes are (a) that no Close, Sync or defer runs, and (b) that
// whatever is in the file got there because Append fsynced it -- and both hold
// here by construction, because the Log is simply never closed. The bytes on
// disk are byte-for-byte what a SIGKILL at this point would have left.
// (TestWALIndexFloorCrashNeverReissuesAnIndex performs the real kill; this
// helper exists so a 2289-iteration sweep does not fork 2289 processes.)
//
// The Log is deliberately not closed and not returned: closing it would seal the
// floor, which is the one thing this fixture must not do.
func crashedFixture(t *testing.T, dir string, n int) (handed map[uint64]bool, highest uint64) {
	t.Helper()
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("fixture Open: %v", err)
	}
	handed = map[uint64]bool{}
	for i := 0; i < n; i++ {
		c, werr := l.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
		if werr != nil {
			t.Fatalf("fixture Write %d: %v", i, werr)
		}
		handed[c.PrepareIndex], handed[c.CommitIndex] = true, true
		highest = c.CommitIndex
	}
	// NO l.Close(): a Close would seal the floor and destroy the very state
	// under test. The descriptor is released when the test binary exits.
	if _, _, sealed := readFloorFile(t, dir); sealed {
		t.Fatalf("the fixture floor is already sealed; this helper must leave the crash state, not a clean one")
	}
	return handed, highest
}

// snapshotDataDir reads every regular file in dir into memory, so one built
// fixture can be replayed under thousands of different damages without being
// rebuilt or re-read. It takes ALL of them, not just bus.wal: the MAC key and
// the index floor are as much a part of the data directory's state as the log.
func snapshotDataDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the fixture directory %s: %v", dir, err)
	}
	snap := map[string][]byte{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("reading %s: %v", e.Name(), rerr)
		}
		snap[e.Name()] = b
	}
	return snap
}

// restoreDataDir writes a snapshot into a fresh directory. It returns an error
// rather than failing the test, because the sweep calls it from worker
// goroutines and t.Fatalf outside the test goroutine is not allowed.
func restoreDataDir(snap map[string][]byte, dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	for name, b := range snap {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// TestWALIndexFloorP0ALostLogAfterCrashDoesNotRestartAtOne is P0-A.
//
// THE SHAPE: a run is handed real indices, dies without closing, and its bus.wal
// is then LOST -- deleted, on a volume that did not come back, restored from a
// backup that predates it. Recovery sees no file at all, which is not damage and
// cannot be distinguished from a fresh data directory by looking at the log.
//
// Under the old rule that was fatal to invariant 1: replay reported an empty log,
// repair reported nothing, LostUnidentified was false, `written` was 0 because
// nothing had sealed it, and the bus restarted at INDEX 1 -- reissuing every id
// it had ever minted -- with the ceiling sitting on disk, unread, in the file
// whose entire purpose is to prevent that.
//
// The floor file is the ONLY surviving evidence here. If this test ever fails,
// nothing else in the package can catch the defect: there is no log left to
// cross-check against.
func TestWALIndexFloorP0ALostLogAfterCrashDoesNotRestartAtOne(t *testing.T) {
	dir := t.TempDir()
	handed, highest := crashedFixture(t, dir, 3) // indices 1..6

	if highest == 0 || len(handed) == 0 {
		t.Fatalf("the fixture handed out no indices, so this test would prove nothing")
	}
	reserved, written, _ := readFloorFile(t, dir)
	if written != 0 {
		t.Fatalf("the fixture floor claims %d indices burned; this test is about the state where `written` is 0 and only `reserved` remembers anything", written)
	}
	if reserved < highest {
		t.Fatalf("the fixture floor reserved only %d but index %d was handed out: the reservation must cover every issued index, or nothing here could work", reserved, highest)
	}

	// THE LOG IS GONE. Not damaged -- gone. Nothing about the remaining bytes
	// says a record ever existed.
	path := filepath.Join(dir, WALFileName)
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the log: %v", err)
	}

	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open on a data directory whose log is gone: %v: recovery must ALWAYS reach a running server (invariant 6)", err)
	}
	defer l.Close()
	rec := l.Recovered()

	if rec.NextIndex == 1 {
		t.Fatalf("Recovered().NextIndex = 1 after a crashed run's log was lost: the bus is about to reissue its ENTIRE index space, and the durable floor (reserved %d) was sitting on disk unread. This is P0-A", reserved)
	}
	if rec.NextIndex <= highest {
		t.Fatalf("Recovered().NextIndex = %d, but index %d had already been handed out: an index this data directory has authorised is never authorised again (invariant 1)",
			rec.NextIndex, highest)
	}
	if want := reserved + 1; rec.NextIndex != want {
		t.Errorf("Recovered().NextIndex = %d, want %d (one past the durable ceiling): with no log at all, the ceiling is the ONLY bound there is",
			rec.NextIndex, want)
	}

	// And the indices it actually hands out are all new.
	for i := 0; i < 3; i++ {
		c, werr := l.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"after":%d}`, i))})
		if werr != nil {
			t.Fatalf("Write %d after recovery: %v", i, werr)
		}
		for _, idx := range []uint64{c.PrepareIndex, c.CommitIndex} {
			if handed[idx] {
				t.Fatalf("the write after recovery got index %d, which the crashed run had already been handed", idx)
			}
		}
	}
}

// TestWALIndexFloorP0BTruncationSweepNeverReissues is P0-B, and it is the
// reviewer's probe turned into a permanent regression test.
//
// IT SWEEPS EVERY BYTE OFFSET of a 12-message WAL -- every possible place a
// truncated write, a short read, a partial restore or a filesystem could have
// cut the file -- and for each one asserts that the restarted bus resumes
// STRICTLY ABOVE every index the original run was handed.
//
// The 25 offsets that used to fail were EXACTLY THE CLEAN FRAME BOUNDARIES, and
// that is the whole lesson: at a frame boundary the shortened file is a
// perfectly valid log, salvage finds nothing to report, LostUnidentified stays
// false, and a rule keyed on "did recovery find damage" therefore skips the
// ceiling in precisely the cases that needed it. A sweep is the right shape of
// test here because a hand-picked offset would almost certainly have missed
// them -- 25 in 2289 is about one percent.
//
// One fixture is built and COPIED per offset, so the whole sweep is a few
// seconds rather than a few thousand fsyncs of setup.
func TestWALIndexFloorP0BTruncationSweepNeverReissues(t *testing.T) {
	const messages = 12

	src := t.TempDir()
	_, highest := crashedFixture(t, src, messages)
	path := filepath.Join(src, WALFileName)
	size := fileSize(t, path)
	if highest != 2*messages {
		t.Fatalf("the fixture was handed up to index %d, want %d (a prepare and a commit per message)", highest, 2*messages)
	}
	if size < 1000 {
		t.Fatalf("the fixture log is only %d bytes; the sweep needs a log with many frame boundaries in it to be worth running", size)
	}
	snap := snapshotDataDir(t, src)

	// One outcome per offset. The sweep runs in a small worker pool -- each
	// offset is completely independent, and serially it is a few thousand fsyncs
	// and takes about twenty seconds, which is too slow for a check that should
	// be runnable in seconds. Workers never touch testing.T: t.Fatalf outside the
	// test goroutine is undefined, so every outcome comes back as data and is
	// judged below.
	type outcome struct {
		next  uint64
		clean bool   // the truncated log showed NO detectable damage
		err   string // non-empty means the offset could not be evaluated at all
	}
	offsets := int(size) + 1 // 0 (file gone) through size (untouched)
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
				off := atomic.AddInt64(&next, 1)
				if off >= int64(offsets) {
					return
				}
				dir := filepath.Join(base, strconv.FormatInt(off, 10))
				if err := restoreDataDir(snap, dir); err != nil {
					results[off] = outcome{err: "restoring the fixture: " + err.Error()}
					continue
				}
				if err := os.Truncate(filepath.Join(dir, WALFileName), off); err != nil {
					results[off] = outcome{err: "truncating: " + err.Error()}
					continue
				}
				l, err := Open(LogOptions{Dir: dir})
				if err != nil {
					results[off] = outcome{err: "Open: " + err.Error()}
					continue
				}
				rec := l.Recovered()
				res := outcome{
					next: rec.NextIndex,
					// A cut at a clean frame boundary leaves a file that scans end
					// to end: no damage is detectable, which is exactly why this
					// offset class was the one that reissued.
					clean: rec.Repaired.DiscardCount == 0 && rec.Repaired.Quarantined == "",
				}
				if cerr := l.Close(); cerr != nil {
					res.err = "Close: " + cerr.Error()
				}
				results[off] = res
				// Reclaim as we go: 2289 copies of the fixture is a lot of inodes
				// to hold until the test's own cleanup runs.
				_ = os.RemoveAll(dir)
			}
		}()
	}
	wg.Wait()

	var reissued []int
	cleanOffsets := 0
	shown := 0
	for off, r := range results {
		if r.err != "" {
			t.Fatalf("offset %d: %s: recovery must ALWAYS reach a running server, whatever the log looks like (invariant 6)", off, r.err)
		}
		if r.clean {
			cleanOffsets++
		}
		if r.next <= highest {
			reissued = append(reissued, off)
			if shown < 8 {
				shown++
				t.Errorf("truncating to offset %d resumes at index %d, but index %d was already handed out: this offset REISSUES an index (invariant 1). A cut at a clean frame boundary is byte-indistinguishable from a shorter log, so nothing in the file can prove the loss -- only the durable ceiling, which is consulted whenever the previous run did not close cleanly",
					off, r.next, highest)
			}
		}
	}

	if len(reissued) != 0 {
		t.Fatalf("%d of %d truncation offsets reissue an already-issued index: %v (first few). This is P0-B",
			len(reissued), offsets, reissued[:minInt(len(reissued), 16)])
	}
	// The sweep must actually have covered the dangerous class. If every offset
	// produced detectable damage, the test would be passing without ever reaching
	// the case it exists for.
	if cleanOffsets < 10 {
		t.Fatalf("only %d of %d offsets left a log with no detectable damage; the clean-frame-boundary class is what reissued, and this sweep is not reaching it",
			cleanOffsets, offsets)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestWALIndexFloorSealedCleanCycleBurnsNothing is the property the seal bit
// BUYS BACK, and it is exactly as important as the two P0s above.
//
// Making the ceiling unconditional would close both P0s and would also put a
// hole of up to indexReserveBlock-1 in every bus's log on EVERY restart, for
// ever. That is not a cost worth paying and it is not necessary: a run that
// closed cleanly has told recovery precisely what it wrote, so `written+1`
// already dominates every index that ever reached a frame.
//
// So the trigger is the seal bit and not "always". This test is what stops
// anyone simplifying it back to "always".
func TestWALIndexFloorSealedCleanCycleBurnsNothing(t *testing.T) {
	dir := t.TempDir()

	const txns = 5
	handed := map[uint64]bool{}
	var highest uint64
	for cycle := 0; cycle < 3; cycle++ {
		l, err := Open(LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("cycle %d: Open: %v", cycle, err)
		}
		rec := l.Recovered()
		if rec.MissingRecords != 0 {
			t.Errorf("cycle %d: MissingRecords = %d, want 0: a clean close/reopen cycle must leave the index sequence DENSE. Burning a reserved block on an ordinary restart puts a permanent hole in every bus's log. Discards: %+v",
				cycle, rec.MissingRecords, rec.Discarded)
		}
		if want := highest + 1; rec.NextIndex != want {
			t.Errorf("cycle %d: Recovered().NextIndex = %d, want %d: one past the last index written, with nothing burned in between",
				cycle, rec.NextIndex, want)
		}

		// WHILE THE RUN IS LIVE the floor says sealed 0, and that is what makes a
		// crash safe: the bit is cleared and fsynced by begin BEFORE the Writer
		// can append, so there is no instant at which a running process has left
		// a clean-close claim on disk.
		if _, _, sealed := readFloorFile(t, dir); sealed {
			t.Fatalf("cycle %d: the floor says sealed 1 while the Log is OPEN: begin must clear the bit before anything can be appended, or a crash inherits the previous run's claim", cycle)
		}

		for i := 0; i < txns; i++ {
			c, werr := l.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"c":%d,"n":%d}`, cycle, i))})
			if werr != nil {
				t.Fatalf("cycle %d: Write %d: %v", cycle, i, werr)
			}
			for _, idx := range []uint64{c.PrepareIndex, c.CommitIndex} {
				if handed[idx] {
					t.Fatalf("cycle %d: index %d was handed out twice", cycle, idx)
				}
				handed[idx] = true
			}
			highest = c.CommitIndex
		}

		if err := l.Close(); err != nil {
			t.Fatalf("cycle %d: Close: %v", cycle, err)
		}
		// AND THE CLEAN CLOSE IS RECORDED, exactly, so the next start can trust it.
		reserved, written, sealed := readFloorFile(t, dir)
		if !sealed {
			t.Fatalf("cycle %d: the floor says sealed 0 after a clean Close: without the bit every restart burns a block", cycle)
		}
		if written != highest {
			t.Errorf("cycle %d: the floor says %d indices burned after a clean close, want exactly %d: sealed 1 claims `written` is EXACT, so it must be",
				cycle, written, highest)
		}
		if reserved < written {
			t.Errorf("cycle %d: the floor claims %d burned but only %d reserved", cycle, written, reserved)
		}
	}

	// The log is contiguous 1..2*txns*3 -- no hole anywhere, which is the whole
	// claim. scanIndices reads the FILE, not recovery's report of it.
	got := scanIndices(t, filepath.Join(dir, WALFileName), KindWAL)
	if len(got) != 2*txns*3 {
		t.Fatalf("the log holds %d records, want %d", len(got), 2*txns*3)
	}
	for i, idx := range got {
		if idx != uint64(i+1) {
			t.Fatalf("the log holds indices %v, want a contiguous 1..%d: three clean cycles must not burn a single index", got, 2*txns*3)
		}
	}
}

// TestWALIndexFloorExistedAtOpenIsNotFlippedByAWrite pins the P2 the reviewer
// found alongside the P0s: existedAtOpen()'s doc said "present when this data
// directory was OPENED" and persistLocked then set the field true, so the
// accessor answered a different question -- "has anyone, including me a moment
// ago, ever written it".
//
// It matters because log.go's MIGRATION WARNING is guarded by it. With the field
// flipped by the first write, the guard is always false and the warning is dead
// code no test can see, which is precisely how it was shipped once already.
func TestWALIndexFloorExistedAtOpenIsNotFlippedByAWrite(t *testing.T) {
	dir := t.TempDir()

	f := mustOpenFloor(t, dir)
	if f.existedAtOpen() {
		t.Fatalf("existedAtOpen() = true for a fresh data directory")
	}
	if err := f.begin(1); err != nil {
		t.Fatalf("begin(1): %v", err)
	}
	if f.existedAtOpen() {
		t.Error("existedAtOpen() = true after this process created the file: the accessor answers \"was it there when the directory was OPENED\", and writing it now is not an answer to that question. Flipping it here makes log.go's migration warning unreachable")
	}
	if err := f.seal(1); err != nil {
		t.Fatalf("seal(1): %v", err)
	}
	if f.existedAtOpen() {
		t.Error("existedAtOpen() = true after seal(): no write may change it")
	}

	// A FRESH open of the same directory does see it, which is the other half of
	// the property: the field is about the open, not about the file being absent
	// for ever.
	if !mustOpenFloor(t, dir).existedAtOpen() {
		t.Error("existedAtOpen() = false for a directory whose floor file is on disk at open time")
	}
}

// TestWALIndexFloorReapsStaleTempFiles is the other P2: a crash between
// os.CreateTemp and os.Rename inside atomicReplaceFile leaves a
// `.wal-index-floor-*` temp file, and before this nothing ever removed one, so a
// data directory accumulated one per crash-during-a-floor-write for ever.
//
// The reap is safe ONLY because the data directory is held under an exclusive
// flock for the life of the server (internal/dirlock): one process at a time
// owns the directory, so a temp file present at open belongs to a process that
// is already gone. If that lock ever goes, this reap must go with it.
func TestWALIndexFloorReapsStaleTempFiles(t *testing.T) {
	dir := t.TempDir()

	// Two leftovers, as a crash during a floor write would leave, plus files that
	// must SURVIVE: a matcher that is too greedy would delete the log or the MAC
	// key, and losing the MAC key is fatal to the whole data directory.
	stale := []string{".wal-index-floor-123456", ".wal-index-floor-987654321"}
	for _, name := range stale {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("partial"), 0o600); err != nil {
			t.Fatalf("writing the stale temp %s: %v", name, err)
		}
	}
	key := floorKey(t, dir) // creates a REAL wal-mac.key, which must also survive
	writeFloorFile(t, dir, "", encodeFloorBody(10, 5, true))
	keep := []string{IndexFloorFileName, WALFileName, MACKeyFileName, "wal-index-floor-not-a-temp"}
	for _, name := range keep {
		if name == IndexFloorFileName || name == MACKeyFileName {
			continue // both already written, and neither may be clobbered
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep me"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	f, err := openIndexFloor(dir, key, true)
	if err != nil {
		t.Fatalf("openIndexFloor: %v", err)
	}
	// The floor itself was still read correctly: reaping is housekeeping and must
	// not interfere with the one job this function has.
	if f.ceiling() != 10 || f.burned() != 5 || !f.sealedClean() {
		t.Errorf("the floor loaded as {ceiling:%d burned:%d sealed:%v}, want {10 5 true}", f.ceiling(), f.burned(), f.sealedClean())
	}

	for _, name := range stale {
		if _, serr := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(serr) {
			t.Errorf("the stale temp %s survived the open (%v): nothing else ever removes one, so a data directory accumulates one per crash-during-a-floor-write for ever", name, serr)
		}
	}
	for _, name := range keep {
		if _, serr := os.Stat(filepath.Join(dir, name)); serr != nil {
			t.Errorf("openIndexFloor deleted %s (%v): the reap matches the temp pattern only, and deleting the log or the MAC key to tidy up would be far worse than the litter", name, serr)
		}
	}
}

// TestWALIndexFloorReapFailureDoesNotFailTheOpen: a reap that cannot remove a
// leftover is not a reason to refuse to start. The floor has already been read
// and verified by then, and a stray temp file harms nothing but tidiness --
// whereas refusing here would turn a permissions quirk into a bus that will not
// boot, which is what invariant 6 forbids.
func TestWALIndexFloorReapFailureDoesNotFailTheOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not deny this process, so there is no reap failure to observe")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "data")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	key := floorKey(t, sub)
	writeFloorFile(t, sub, "", encodeFloorBody(10, 5, false))
	if err := os.WriteFile(filepath.Join(sub, ".wal-index-floor-stuck"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("writing the stale temp: %v", err)
	}
	// 0500: readable and executable, so the files can be read and stat'd, but
	// nothing can be unlinked from it.
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatalf("chmod 0500: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })

	f, err := openIndexFloor(sub, key, true)
	if err != nil {
		t.Fatalf("openIndexFloor with an unreapable temp file = %v, want no error: reaping is housekeeping, and failing the open over it would brick a bus for litter", err)
	}
	if f.ceiling() != 10 || f.burned() != 5 {
		t.Errorf("the floor loaded as {ceiling:%d burned:%d}, want {10 5}", f.ceiling(), f.burned())
	}
}
