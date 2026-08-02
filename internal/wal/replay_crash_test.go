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
)

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
