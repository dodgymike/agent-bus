//go:build linux || darwin

package wal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Crash injection.
//
// "The code looks right" is not evidence for a durability claim, and neither is
// a simulation: a test that merely stops calling Commit proves nothing about
// what an ABRUPT death leaves on the platter, because it still runs every defer,
// every Close and every buffer flush on the way out.
//
// So these tests RE-EXEC THE TEST BINARY as a child, have the child write into a
// directory the parent owns, and have the child SIGKILL ITSELF at a chosen point
// in the write path. SIGKILL cannot be caught, blocked or handled: no deferred
// Close runs, no Go runtime shutdown runs, nothing is flushed on the way out.
// Whatever is in the file afterwards got there because Append fsynced it.
//
// The parent then proves the child really died on SIGKILL -- inspecting the
// wait status, not just "err != nil", because a child that failed its own
// assertions would also return an error and would otherwise silently turn this
// into a test of nothing -- and replays the directory.
// ---------------------------------------------------------------------------

const (
	// envCrashPoint selects where the child kills itself. Unset means "not a
	// crash child", which is what makes TestWALCrashChild a no-op in a normal
	// run of the suite.
	envCrashPoint = "WAL_CRASH_POINT"
	// envCrashDir is the data directory the child writes into. It is a
	// t.TempDir() belonging to the parent.
	envCrashDir = "WAL_CRASH_DIR"

	// crashAfterPrepare: the child prepares a third entry -- so the PREPARE
	// record is fsynced -- and dies before its COMMIT record is ever written.
	crashAfterPrepare = "after-prepare"
	// crashInsideApply: the child's third COMMIT record is fsynced and the
	// child dies INSIDE Apply, so the entry is durable but was never applied to
	// memory and was never acknowledged to the caller.
	crashInsideApply = "inside-apply"
	// crashMidFrameWrite: the child leaves the byte pattern a power loss during
	// an append produces -- a strict PREFIX of one frame past the last fsynced
	// record -- and dies. See TestWALCrashTornFrameTailIsRepaired for exactly
	// what this does and does not prove.
	crashMidFrameWrite = "mid-frame-write"
)

// crashFrameTime is the fixed timestamp the torn frame's prepare payload
// carries, so the child's bytes are reproducible.
var crashFrameTime = time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)

// crashEntries are the entries a crash child writes, in order. The second has a
// nil body on purpose, so recovery of the nil/null normalisation is covered by
// the crash path too.
var crashEntries = []Entry{
	{Kind: "message", Body: json.RawMessage(`{"to":"bus-1.agent-a","seq":1}`)},
	{Kind: "agent", Body: nil},
	{Kind: "message", Body: json.RawMessage(`{"to":"bus-1.agent-b","seq":3}`)},
}

// TestWALCrashChild is the child half of the crash tests. It does NOTHING in a
// normal run: without envCrashPoint it skips immediately, so the suite is
// unaffected by its presence.
func TestWALCrashChild(t *testing.T) {
	point := os.Getenv(envCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envCrashPoint + " is unset")
	}
	dir := os.Getenv(envCrashDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envCrashPoint, point, envCrashDir)
	}

	switch point {
	case crashAfterPrepare:
		l, err := Open(LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("child: Open: %v", err)
		}
		for i, e := range crashEntries[:2] {
			if _, err := l.Write(e); err != nil {
				t.Fatalf("child: Write %d: %v", i, err)
			}
		}
		// Begin returns only after the PREPARE record is fsynced. The commit is
		// never written and the Txn is never resolved.
		if _, err := l.Begin(crashEntries[2]); err != nil {
			t.Fatalf("child: Begin: %v", err)
		}
		suicide()

	case crashInsideApply:
		l, err := Open(LogOptions{Dir: dir, Applier: &suicideApplier{killAt: len(crashEntries)}})
		if err != nil {
			t.Fatalf("child: Open: %v", err)
		}
		for i, e := range crashEntries {
			if _, err := l.Write(e); err != nil {
				t.Fatalf("child: Write %d: %v", i, err)
			}
		}
		t.Fatalf("child: wrote every entry and is still alive: the applier never killed the process")

	case crashMidFrameWrite:
		// Two entries through the REAL write path first, so the good prefix is
		// genuinely fsynced accepted history and not a hand-built fixture.
		l, err := Open(LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("child: Open: %v", err)
		}
		for i, e := range crashEntries[:2] {
			if _, err := l.Write(e); err != nil {
				t.Fatalf("child: Write %d: %v", i, err)
			}
		}
		if err := l.Close(); err != nil {
			t.Fatalf("child: Close: %v", err)
		}

		// -------------------------------------------------------------------
		// HONEST ACCOUNT OF WHAT THIS INJECTS.
		//
		// A SIGKILL cannot by itself tear a write: os.File.Write is a single
		// syscall, the bytes land in the PAGE CACHE, and the page cache outlives
		// the process. Killing between two Appends therefore leaves whole frames
		// -- which is the crash shape the other two tests here cover.
		//
		// The byte pattern a POWER LOSS mid-append leaves -- a strict prefix of
		// one frame -- has to be produced deliberately, and that is what the
		// short write below does. It is built with the real encoders, at the
		// index the next Append would have used, so the bytes are exactly the
		// bytes that append would have been putting on the platter.
		//
		// What the kill DOES prove, and it is the part that cannot be faked: no
		// Close, no Sync, no deferred cleanup and no runtime shutdown ran
		// afterwards. The file the parent opens is precisely what the dying
		// process had put there -- nobody tidied the tail up on the way out.
		// -------------------------------------------------------------------
		payload, err := encodePrepare(crashEntries[2].Kind, crashEntries[2].Body, crashFrameTime)
		if err != nil {
			t.Fatalf("child: encodePrepare: %v", err)
		}
		frame := encodeFrame(5, TypePrepare, payload) // records 1-4 exist; 5 is next
		partial := len(frame) / 2
		if partial <= FrameHeaderSize {
			t.Fatalf("child: half a frame is %d bytes, which does not reach past the %d-byte header",
				partial, FrameHeaderSize)
		}
		f, err := os.OpenFile(filepath.Join(dir, WALFileName), os.O_WRONLY|os.O_APPEND, fileMode)
		if err != nil {
			t.Fatalf("child: OpenFile to append a torn frame: %v", err)
		}
		if _, err := f.Write(frame[:partial]); err != nil {
			t.Fatalf("child: writing the torn frame: %v", err)
		}
		// No Close, no Sync, no defer: the next statement is the kill.
		suicide()

	default:
		t.Fatalf("child: unknown crash point %q", point)
	}
	t.Fatalf("child: still running after SIGKILL")
}

// suicideApplier kills the process from inside the killAt'th Apply. Commit
// appends and fsyncs the COMMIT record BEFORE calling Apply, so the entry is
// already accepted history at this point -- but the caller has not been told and
// memory was never updated.
type suicideApplier struct {
	killAt int
	n      int
}

func (a *suicideApplier) Apply(Committed) error {
	a.n++
	if a.n >= a.killAt {
		suicide()
	}
	return nil
}

// suicide kills this process with SIGKILL. SIGKILL cannot be caught or ignored,
// so nothing deferred, buffered or graceful runs afterwards.
func suicide() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	// Unreachable: the signal is delivered before Kill returns. Panicking rather
	// than looping means a platform where that is somehow untrue fails loudly
	// instead of hanging the suite.
	panic("wal crash test: SIGKILL to self did not kill the process")
}

// runCrashChild re-execs this test binary at the given crash point and asserts
// the child really was killed by SIGKILL.
func runCrashChild(t *testing.T, point, dir string) {
	t.Helper()

	// os.Executable is os.Args[0] resolved properly: under `go test` that is the
	// compiled test binary.
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}

	// Bounded, so a child that wedges fails this test in a minute rather than
	// hanging the suite until the package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestWALCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), envCrashPoint+"="+point, envCrashDir+"="+dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child %q did not finish in time: %v\n--- child output ---\n%s", point, ctx.Err(), out.String())
	}

	// A child that failed its OWN assertions also exits non-zero, so "err !=
	// nil" is not the assertion. The wait status is.
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
		t.Fatalf("crash child %q exited normally with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s",
			point, ws.ExitStatus(), out.String())
	}
}

// replayDir replays the WAL in dir and returns the delivered stream. Any error
// is fatal: both crash points leave a log that must replay cleanly.
func replayDir(t *testing.T, dir string) ([]Committed, Recovered) {
	t.Helper()
	var c collector
	r, err := Replay(filepath.Join(dir, WALFileName), c.fn)
	if err != nil {
		t.Fatalf("Replay after the crash: %v (recovered %+v)", err, r)
	}
	return c.got, r
}

// assertIndicesUnique proves no index is ever handed out twice in a file, which
// is the recovery half of invariant 1.
func assertIndicesUnique(t *testing.T, path string) {
	t.Helper()
	recs, shape := scanTypes(t, path)
	last := uint64(0)
	for _, rec := range recs {
		if rec.Index <= last {
			t.Fatalf("index %d follows %d in %s: indices must be strictly increasing and never reused",
				rec.Index, last, shape)
		}
		last = rec.Index
	}
}

// TestWALReplayCrashBetweenPrepareAndCommit is the "an uncommitted prepare is
// never visible after a restart" proof, with a real process kill in the window
// invariant 4 is about: the PREPARE record is on stable storage, the COMMIT
// record does not exist, and the client that was waiting on that write was never
// told anything.
//
// Recovery must therefore DISCARD it -- and must still burn its index, because
// the entry did exist on disk and reissuing the index would let two different
// entries share one.
func TestWALReplayCrashBetweenPrepareAndCommit(t *testing.T) {
	dir := t.TempDir()
	runCrashChild(t, crashAfterPrepare, dir)
	path := filepath.Join(dir, WALFileName)

	// Two committed entries (records 1-4) plus the orphaned prepare (record 5).
	const burned = 5
	got, r := replayDir(t, dir)

	want := []Committed{
		{PrepareIndex: 1, CommitIndex: 2, Entry: Entry{Kind: crashEntries[0].Kind, Body: crashEntries[0].Body}},
		{PrepareIndex: 3, CommitIndex: 4, Entry: Entry{Kind: crashEntries[1].Kind, Body: crashEntries[1].Body}},
	}
	if !sameCommitted(got, want) {
		t.Fatalf("replay after the crash delivered %s, want %s: the uncommitted prepare must not be visible",
			showCommitted(got), showCommitted(want))
	}
	if r.Records != burned {
		t.Errorf("Records = %d, want %d (two prepare/commit pairs plus the orphaned prepare)", r.Records, burned)
	}
	if r.Applied != 2 || r.Aborted != 0 {
		t.Errorf("Applied = %d, Aborted = %d, want 2 and 0", r.Applied, r.Aborted)
	}
	if !reflect.DeepEqual(r.Dangling, []uint64{burned}) {
		t.Fatalf("Dangling = %v, want [%d]: the crash left exactly one unresolved prepare", r.Dangling, burned)
	}
	if r.NextIndex != burned+1 {
		t.Fatalf("NextIndex = %d, want %d: a discarded prepare still burns its index", r.NextIndex, burned+1)
	}
	if size := fileSize(t, path); r.EndOffset != size {
		t.Errorf("EndOffset = %d, want the file size %d: Append fsyncs whole frames, so a crash leaves no torn tail",
			r.EndOffset, size)
	}

	// Now restart for real and keep writing.
	app := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: app})
	if err != nil {
		t.Fatalf("Open after the crash: %v", err)
	}
	defer l.Close()

	if app.count() != len(want) {
		t.Fatalf("Apply called %d times on restart, want %d", app.count(), len(want))
	}
	restored := make([]Committed, app.count())
	for i := range restored {
		restored[i] = app.at(i)
	}
	if !sameCommitted(restored, want) {
		t.Fatalf("the restarted Log rebuilt memory from %s, want %s", showCommitted(restored), showCommitted(want))
	}

	c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"seq":4}`)})
	if err != nil {
		t.Fatalf("Write after the crash: %v", err)
	}
	if c.PrepareIndex != burned+1 {
		t.Fatalf("the first write after the crash got prepare index %d, want %d: the burned index must never be reissued",
			c.PrepareIndex, burned+1)
	}
	assertIndicesUnique(t, path)
}

// TestWALReplayCrashInsideApply is the other half of invariant 5, and the one
// that is easy to get backwards: the child died INSIDE Apply for its third
// entry, so that entry's COMMIT record had already been fsynced but memory was
// never updated and the caller never got an answer.
//
// Disk is the truth. A commit that reached the platter is accepted history
// whether or not anyone was told, so recovery must deliver ALL THREE entries.
// Dropping the third -- on the grounds that nobody saw it -- would make the
// recovered state something other than a prefix of accepted history, and would
// mean an entry that a relay or a peer may already have observed had vanished.
func TestWALReplayCrashInsideApply(t *testing.T) {
	dir := t.TempDir()
	runCrashChild(t, crashInsideApply, dir)
	path := filepath.Join(dir, WALFileName)

	got, r := replayDir(t, dir)

	want := make([]Committed, len(crashEntries))
	for i, e := range crashEntries {
		want[i] = Committed{
			PrepareIndex: uint64(2*i + 1),
			CommitIndex:  uint64(2*i + 2),
			Entry:        Entry{Kind: e.Kind, Body: e.Body},
		}
	}
	if !sameCommitted(got, want) {
		t.Fatalf("replay after a crash inside Apply delivered %s, want %s: a commit record that reached the disk is accepted history even though the caller was never acknowledged",
			showCommitted(got), showCommitted(want))
	}
	if n := uint64(2 * len(crashEntries)); r.Records != n {
		t.Errorf("Records = %d, want %d", r.Records, n)
	}
	if r.Applied != uint64(len(crashEntries)) {
		t.Errorf("Applied = %d, want %d", r.Applied, len(crashEntries))
	}
	if len(r.Dangling) != 0 {
		t.Errorf("Dangling = %v, want none: the crash was after the commit fsync, not before it", r.Dangling)
	}
	if wantNext := uint64(2*len(crashEntries)) + 1; r.NextIndex != wantNext {
		t.Errorf("NextIndex = %d, want %d", r.NextIndex, wantNext)
	}
	if size := fileSize(t, path); r.EndOffset != size {
		t.Errorf("EndOffset = %d, want the file size %d", r.EndOffset, size)
	}

	// Restarting rebuilds exactly that state and carries on.
	app := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: app})
	if err != nil {
		t.Fatalf("Open after the crash: %v", err)
	}
	defer l.Close()

	if app.count() != len(want) {
		t.Fatalf("Apply called %d times on restart, want %d", app.count(), len(want))
	}
	restored := make([]Committed, app.count())
	for i := range restored {
		restored[i] = app.at(i)
	}
	if !sameCommitted(restored, want) {
		t.Fatalf("the restarted Log rebuilt memory from %s, want %s", showCommitted(restored), showCommitted(want))
	}
	c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"seq":4}`)})
	if err != nil {
		t.Fatalf("Write after the crash: %v", err)
	}
	if c.PrepareIndex != r.NextIndex {
		t.Errorf("the first write after the crash got prepare index %d, want %d", c.PrepareIndex, r.NextIndex)
	}
	assertIndicesUnique(t, path)
}

// TestWALCrashTornFrameTailIsRepaired is DUR-4's crash-injection test: a process
// that died with a half-written frame on the end of its log, and a restart that
// has to turn that into a clean prefix of accepted history without losing
// anything that was ever acknowledged.
//
// WHAT THE CHILD LEAVES, AND WHY: see the long comment at crashMidFrameWrite in
// TestWALCrashChild. In short -- the two committed entries are real, fsynced,
// accepted history written through the real Log; the torn frame after them is
// written deliberately, because a SIGKILL cannot tear a write on its own (the
// page cache outlives the process). The kill's contribution is that NOTHING
// graceful ran afterwards: no Close, no Sync, no defer. What the parent opens is
// exactly what the dying process had put on disk.
func TestWALCrashTornFrameTailIsRepaired(t *testing.T) {
	dir := t.TempDir()
	runCrashChild(t, crashMidFrameWrite, dir)
	path := filepath.Join(dir, WALFileName)

	tornSize := fileSize(t, path)

	// (1) The tail really IS torn. Without this the rest of the test would pass
	// just as happily against a perfectly healthy file and would prove nothing.
	if _, err := Replay(path, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Replay of the crashed log = %v, want ErrCorrupt: the child did not leave a torn tail", err)
	}

	// (2) Open repairs it and starts.
	app := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: app})
	if err != nil {
		t.Fatalf("Open after a crash mid-frame: %v, want a repaired start", err)
	}
	defer l.Close()

	rec := l.Recovered()
	rep := rec.Repaired
	if !rep.Truncated {
		t.Fatalf("Recovered().Repaired = %+v, want Truncated true", rep)
	}
	repairedSize := fileSize(t, path)
	if rep.At != repairedSize {
		t.Errorf("Repaired.At = %d but the file is %d bytes: the cut must land exactly at At", rep.At, repairedSize)
	}
	if rep.At+rep.Removed != tornSize {
		t.Errorf("Repaired.At+Removed = %d, want the pre-repair size %d", rep.At+rep.Removed, tornSize)
	}
	if rep.Removed <= FrameHeaderSize {
		t.Errorf("Repaired.Removed = %d bytes, want more than the %d-byte frame header: "+
			"the child is meant to have torn the frame inside its PAYLOAD", rep.Removed, FrameHeaderSize)
	}
	// Records 1-4 are the two committed transactions; the torn frame would have
	// been record 5.
	if rep.NextIndex != 5 {
		t.Errorf("Repaired.NextIndex = %d, want 5", rep.NextIndex)
	}
	if rep.Path != path || rep.Reason == "" {
		t.Errorf("Repaired = %+v, want it to name the path and the reason", rep)
	}

	// (3) The repaired file is a clean four-record log.
	recs, end, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("ScanAll after the repair: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("the repaired file holds %d records, want 4 (two prepare/commit pairs)", len(recs))
	}
	if end != repairedSize {
		t.Errorf("the repaired file scans to %d but is %d bytes", end, repairedSize)
	}

	// (4) Memory was rebuilt from exactly the two committed entries, in order,
	// with byte-identical bodies -- including the nil body of the second.
	want := []Committed{
		{PrepareIndex: 1, CommitIndex: 2, Entry: Entry{Kind: crashEntries[0].Kind, Body: crashEntries[0].Body}},
		{PrepareIndex: 3, CommitIndex: 4, Entry: Entry{Kind: crashEntries[1].Kind, Body: crashEntries[1].Body}},
	}
	got := make([]Committed, app.count())
	for i := range got {
		got[i] = app.at(i)
	}
	if !sameCommitted(got, want) {
		t.Fatalf("the restarted Log rebuilt memory from %s, want %s", showCommitted(got), showCommitted(want))
	}
	if rec.Applied != 2 || rec.Aborted != 0 || len(rec.Dangling) != 0 {
		t.Errorf("Recovered() = %+v, want 2 applied, 0 aborted, no dangling", rec)
	}

	// (5) The first write after recovery takes index 5 -- the index the torn
	// frame carried. That REISSUE is deliberate and documented on
	// TailRepair.NextIndex: the frame never completed its fsync, Append returns
	// only after fsync, and nothing is acknowledged before Append returns, so no
	// id inside that frame can ever have been observed by a client, peer or
	// relay. Invariant 1 protects OBSERVED ids, and none of these were.
	c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"seq":9}`)})
	if err != nil {
		t.Fatalf("Write after the repair: %v", err)
	}
	if c.PrepareIndex != 5 || c.CommitIndex != 6 {
		t.Fatalf("the write after the repair got {prepare:%d commit:%d}, want {5 6}", c.PrepareIndex, c.CommitIndex)
	}
	assertIndicesUnique(t, path)
}
