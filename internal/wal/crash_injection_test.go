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
	"syscall"
	"testing"
	"time"
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

// TestCrashInjectionSingleBitCorruptionSweep is the "corrupt the file at a chosen
// byte offset" half of DUR-6, quantified over every offset in the file.
//
// It flips ONE BIT at each offset in turn -- the shape of a bit-rot or a torn
// sector -- and asserts the rule that keeps acknowledged history: RECOVERY MAY
// DISCARD A TORN TAIL AND NOTHING ELSE. Concretely, when the damaged frame has
// undamaged, fully committed transactions after it, the only acceptable answers
// are (i) recover everything anyway, or (ii) REFUSE TO START. Quietly truncating
// is not an option: those records are sitting on the disk intact, they were
// acknowledged to a caller, and they may already have been relayed to a peer.
//
// Refusing to start is recoverable by an operator. Deleting acknowledged history
// is not.
func TestCrashInjectionSingleBitCorruptionSweep(t *testing.T) {
	pristine, acked, ackedAt := acceptedHistory(t)
	full := readFile(t, pristine)
	recs, _, err := ScanAll(pristine, KindWAL)
	if err != nil {
		t.Fatalf("the fixture is not well framed: %v", err)
	}
	parent := t.TempDir()

	// frameOf reports the frame containing an offset, as [start, end).
	frameOf := func(off int64) (start, end int64, ok bool) {
		for _, r := range recs {
			if off >= r.Offset && off < r.Offset+r.frameSize() {
				return r.Offset, r.Offset + r.frameSize(), true
			}
		}
		return 0, 0, false
	}

	type loss struct {
		off        int64
		start, end int64
		got, want  int
		rec        Recovered
	}
	var losses []loss

	for off := int64(0); off < int64(len(full)); off++ {
		dir, path := crashFixture(t, parent, int(off), full)
		flipByte(t, path, off)

		got, rec, err := recoverFixture(t, dir)
		if err != nil {
			// A refusal to start is ALWAYS a safe answer to corruption. It loses
			// nothing and an operator can inspect the file.
			continue
		}

		// It started. Whatever it serves must be a prefix of accepted history.
		if len(got) > len(acked) || !sameCommitted(got, acked[:len(got)]) {
			t.Fatalf("bit flip at offset %d: recovery served %s, which is not a prefix of the accepted history %s",
				off, showCommitted(got), showCommitted(acked))
		}

		start, end, inFrame := frameOf(off)
		if !inFrame {
			// The damage is in the FILE header. Starting at all means the header
			// still parsed, so the full history must be intact.
			if len(got) != len(acked) {
				t.Fatalf("bit flip at offset %d (in the file header): recovery served %d of %d accepted entries",
					off, len(got), len(acked))
			}
			continue
		}

		// Everything acknowledged strictly BEFORE the damaged frame is
		// byte-for-byte untouched and must always survive.
		if min := visibleAt(ackedAt, start); len(got) < min {
			t.Fatalf("bit flip at offset %d (frame [%d,%d)): recovery served %d entries, but %d had completed their commit fsync before that frame even begins -- those bytes are undamaged",
				off, start, end, len(got), min)
		}

		// And the rule this whole test exists for: entries lying entirely AFTER
		// the damaged frame are undamaged too. Losing them is silent, permanent
		// loss of acknowledged history.
		want := visibleAt(ackedAt, int64(len(full)))
		if intact := visibleAt(ackedAt, end); intact < want {
			// There is committed history after the damage. It must not vanish.
			if len(got) < want {
				losses = append(losses, loss{off: off, start: start, end: end, got: len(got), want: want, rec: rec})
			}
		}
	}

	if len(losses) > 0 {
		first := losses[0]
		offs := make([]int64, len(losses))
		for i, l := range losses {
			offs[i] = l.off
		}
		t.Fatalf(`ACKNOWLEDGED HISTORY WAS SILENTLY DELETED by recovery.

%d of the %d single-bit corruptions in this %d-byte log caused recovery to START
successfully while serving FEWER entries than were acknowledged. The lost records
were not damaged: they sat after the corrupted frame, whole and checksum-valid,
and recovery truncated the file out from under them.

First offence: bit flip at offset %d, inside the frame at [%d,%d).
  recovery served %d of the %d acknowledged entries
  Recovered = %+v

That is a violation of invariant 4 (nothing acknowledged is ever lost),
invariant 5 (recovery must yield a prefix of accepted history) and invariant 6
(the only permitted truncation is a VERIFIED-corrupt tail).

All offending offsets: %v`,
			len(losses), len(full), len(full),
			first.off, first.start, first.end, first.got, first.want, first.rec,
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
