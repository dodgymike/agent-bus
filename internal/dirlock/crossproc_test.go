//go:build linux || darwin

package dirlock

import (
	"bytes"
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
)

// ---------------------------------------------------------------------------
// CROSS-PROCESS PROOF.
//
// dirlock_test.go proves a second Acquire INSIDE THIS PROCESS is refused. That
// is a real conflict -- flock attaches to the open file description, not to the
// process -- but it is emphatically NOT the claim DUR-8 makes. The claim is
// "two agent-bus SERVERS cannot open one data directory", and an in-process
// test can pass while that is false: the two cases differ in fd inheritance
// (O_CLOEXEC), in what the kernel does on death, and on filesystems (NFS, some
// container overlays) where flock is emulated per-process or not at all.
//
// So these tests RE-EXEC THE TEST BINARY as a genuinely separate process, in the
// same idiom as internal/wal/replay_crash_test.go: TestDirLockChild skips
// instantly when its env var is unset, so a normal suite run is unaffected.
//
// Two things are deliberately NOT trusted here:
//
//   - The child's EXIT CODE alone. A test binary invoked with a -test.run
//     pattern that matches nothing prints "no tests to run" and exits 0 -- the
//     exact vacuous-proof shape scripts/proof-check.sh exists to catch. Every
//     child therefore writes a REPORT FILE describing what it observed, and the
//     parent asserts on that content. No report means the child never ran the
//     assertion, and that is a failure.
//   - "err != nil" from the SIGKILLed child. A child that failed its own
//     assertions also exits non-zero and would silently turn the kill test into
//     a test of nothing, so the parent asserts on the WAIT STATUS: Signaled()
//     and Signal() == SIGKILL.
//
// Every directory used here is under t.TempDir(). The tracked ./data directory
// is never touched.
// ---------------------------------------------------------------------------

const (
	// envChildMode selects what the child does. Unset means "not a child",
	// which is what makes TestDirLockChild a no-op in a normal run.
	envChildMode = "DIRLOCK_CHILD_MODE"
	// envChildDir is the data directory the child locks. It is a t.TempDir()
	// belonging to the parent.
	envChildDir = "DIRLOCK_CHILD_DIR"
	// envChildReport is where the child records what it observed, so the parent
	// checks EVIDENCE rather than an exit code that a no-op run also produces.
	envChildReport = "DIRLOCK_CHILD_REPORT"
	// envChildWantPID is the pid the child expects to find recorded in the lock
	// file (the parent's), or "" for "do not check".
	envChildWantPID = "DIRLOCK_CHILD_WANT_PID"

	// modeExpectBusy: Acquire must FAIL with ErrLocked and name the dir and the
	// holder pid.
	modeExpectBusy = "expect-busy"
	// modeExpectFree: Acquire must SUCCEED. Without this half, a test that only
	// ever asserts refusal would pass just as well against a lock that refuses
	// everybody forever.
	modeExpectFree = "expect-free"
	// modeHoldUntilKilled: Acquire, hand the parent a handshake, then block
	// until SIGKILL. No defers, no Release -- the kernel is the only thing that
	// can drop this lock.
	modeHoldUntilKilled = "hold-until-killed"

	// childHandshakeFD is the fd the hold child writes its handshake to. It is
	// ExtraFiles[0], which the runtime maps to fd 3. A dedicated fd rather than
	// stdout: the test binary's own "=== RUN" chatter shares stdout, and a
	// handshake you have to parse out of log noise is a flake waiting to happen.
	childHandshakeFD = 3

	// childHoldLimit bounds how long an unkilled hold child lives, so a parent
	// that dies before killing it cannot leave a process holding a lock. It is a
	// safety net, never a synchronisation primitive -- the handshake is.
	childHoldLimit = 60 * time.Second

	// childWait bounds how long the parent waits for a child. Generous enough
	// for a cold, race-instrumented start; short enough that a wedge fails the
	// test instead of hanging the suite.
	childWait = 60 * time.Second
)

// TestDirLockChild is the child half of the cross-process tests. It does
// NOTHING in a normal run: without envChildMode it skips immediately.
func TestDirLockChild(t *testing.T) {
	mode := os.Getenv(envChildMode)
	if mode == "" {
		t.Skip("not a dirlock child: " + envChildMode + " is unset")
	}
	dir := os.Getenv(envChildDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envChildMode, mode, envChildDir)
	}
	report := os.Getenv(envChildReport)
	if report == "" {
		t.Fatalf("child: %s is empty; the parent would have no evidence the child ran", envChildReport)
	}

	switch mode {
	case modeExpectBusy:
		lk, err := Acquire(dir)
		if err == nil {
			// Release before failing: leaving the lock held would poison the
			// parent's follow-up assertions with a confusing second cause.
			_ = lk.Release()
			t.Fatalf("child: Acquire(%q) SUCCEEDED while the parent holds the lock: "+
				"two servers on one data directory destroy the write-ahead log", dir)
		}
		if lk != nil {
			t.Fatalf("child: Acquire returned a non-nil Lock alongside its error %v", err)
		}
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("child: Acquire failed with %v, which is not errors.Is(err, ErrLocked): "+
				"the refusal must be the lock, not some unrelated error", err)
		}
		var busy *BusyError
		if !errors.As(err, &busy) {
			t.Fatalf("child: Acquire error %v is not a *BusyError", err)
		}
		if busy.Dir != dir {
			t.Fatalf("child: BusyError.Dir = %q, want %q", busy.Dir, dir)
		}
		if want := filepath.Join(dir, LockFileName); busy.Path != want {
			t.Fatalf("child: BusyError.Path = %q, want %q", busy.Path, want)
		}
		if !strings.Contains(err.Error(), dir) {
			t.Fatalf("child: error %q does not name the directory the operator has to fix", err)
		}
		if want := os.Getenv(envChildWantPID); want != "" {
			if got := strconv.Itoa(busy.HolderPID); got != want {
				t.Fatalf("child: BusyError.HolderPID = %s, want the parent's pid %s", got, want)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("child: error %q does not name the holder pid %s", err, want)
			}
		}
		writeReport(t, report, fmt.Sprintf("busy dir=%s holder=%d", busy.Dir, busy.HolderPID))

	case modeExpectFree:
		lk, err := Acquire(dir)
		if err != nil {
			t.Fatalf("child: Acquire(%q) = %v, want success: the lock was supposed to be free", dir, err)
		}
		body, rerr := os.ReadFile(lk.Path())
		if rerr != nil {
			t.Fatalf("child: read lock file: %v", rerr)
		}
		if got, want := string(body), strconv.Itoa(os.Getpid())+"\n"; got != want {
			t.Fatalf("child: lock file contents = %q, want this process's pid line %q", got, want)
		}
		if err := lk.Release(); err != nil {
			t.Fatalf("child: Release: %v", err)
		}
		writeReport(t, report, fmt.Sprintf("acquired pid=%d", os.Getpid()))

	case modeHoldUntilKilled:
		lk, err := Acquire(dir)
		if err != nil {
			t.Fatalf("child: Acquire(%q) = %v, want success", dir, err)
		}
		writeReport(t, report, fmt.Sprintf("holding pid=%d", os.Getpid()))

		// Tell the parent we hold it. Everything after this point may be
		// obliterated by SIGKILL at any instant -- which is the point. There is
		// deliberately no defer lk.Release() here: a defer would not run under
		// SIGKILL anyway, and pretending otherwise would weaken the claim.
		hs := os.NewFile(childHandshakeFD, "dirlock-handshake")
		if hs == nil {
			t.Fatalf("child: fd %d is not the handshake pipe", childHandshakeFD)
		}
		if _, err := fmt.Fprintf(hs, "holding pid=%d\n", os.Getpid()); err != nil {
			t.Fatalf("child: writing the handshake: %v", err)
		}
		if err := hs.Close(); err != nil {
			t.Fatalf("child: closing the handshake pipe: %v", err)
		}

		// Block. The parent's SIGKILL ends this process; the sleep is only a
		// leak guard for a parent that died first.
		time.Sleep(childHoldLimit)
		_ = lk
		t.Fatalf("child: still alive after %v holding %q: the parent never killed it", childHoldLimit, dir)

	default:
		t.Fatalf("child: unknown mode %q", mode)
	}
}

// writeReport records what the child observed. The parent asserts on this file,
// because a child that never ran its assertions also exits 0.
func writeReport(t *testing.T, path, line string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("child: writing report %s: %v", path, err)
	}
}

// childCmd builds the re-exec of this test binary in the given mode.
func childCmd(t *testing.T, mode, dir, report, wantPID string) *exec.Cmd {
	t.Helper()
	// os.Executable is os.Args[0] resolved properly: under `go test` that is the
	// compiled test binary. There is deliberately NO fallback to os.Args[0]: a
	// value without a path separator would send exec.Command through a PATH
	// lookup, and a test that silently execs something else off PATH is worse
	// than a test that fails.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v (cannot re-exec the test binary, so the cross-process claim cannot be proved)", err)
	}
	cmd := exec.Command(self, "-test.run=^TestDirLockChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		envChildMode+"="+mode,
		envChildDir+"="+dir,
		envChildReport+"="+report,
		envChildWantPID+"="+wantPID,
	)
	return cmd
}

// runChildToCompletion runs a non-hold child, asserts it exited cleanly (not
// signalled, status 0) and returns the report it wrote. A missing report is
// fatal: it means the child skipped or never reached its assertion, which is
// exactly the vacuous pass this whole file exists to rule out.
func runChildToCompletion(t *testing.T, mode, dir, wantPID string) string {
	t.Helper()

	report := filepath.Join(t.TempDir(), "report-"+mode)
	cmd := childCmd(t, mode, dir, report, wantPID)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the %q child: %v", mode, err)
	}
	go func() { done <- cmd.Wait() }()

	var err error
	select {
	case err = <-done:
	case <-time.After(childWait):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("the %q child did not finish within %v\n--- child output ---\n%s", mode, childWait, out.String())
	}
	if err != nil {
		t.Fatalf("the %q child failed: %v\n--- child output ---\n%s", mode, err, out.String())
	}
	// Belt and braces: a clean exit must also be an UNSIGNALLED one.
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		t.Fatalf("the %q child died on signal %v\n--- child output ---\n%s", mode, ws.Signal(), out.String())
	}

	body, rerr := os.ReadFile(report)
	if rerr != nil {
		t.Fatalf("the %q child exited 0 but wrote no report (%v): it never ran its assertion, so this proves nothing"+
			"\n--- child output ---\n%s", mode, rerr, out.String())
	}
	return string(body)
}

// TestDirLockCrossProcessExclusion is the claim DUR-8 actually makes: a
// SEPARATE PROCESS -- not another Acquire in this one -- is refused the data
// directory while it is held, with ErrLocked and the holder's pid; and it is
// admitted once the holder releases.
//
// The second half is not decoration. Without it, a lock that refused everyone
// unconditionally (a permission error, a missing directory, a typo in the
// child's mode) would pass the first half perfectly.
func TestDirLockCrossProcessExclusion(t *testing.T) {
	dir := t.TempDir()

	held, err := Acquire(dir)
	if err != nil {
		t.Fatalf("parent Acquire: %v", err)
	}

	// (1) A separate process is refused, for the right reason, and can name us.
	got := runChildToCompletion(t, modeExpectBusy, dir, strconv.Itoa(os.Getpid()))
	want := fmt.Sprintf("busy dir=%s holder=%d", dir, os.Getpid())
	if got != want {
		t.Fatalf("expect-busy child reported %q, want %q", got, want)
	}

	// (2) The refusal did not damage the holder's state: no O_TRUNC, no unlink.
	body, err := os.ReadFile(held.Path())
	if err != nil {
		t.Fatalf("read lock file after the refused child: %v", err)
	}
	if got, want := string(body), strconv.Itoa(os.Getpid())+"\n"; got != want {
		t.Fatalf("a refused child mangled the holder's lock file: contents = %q, want %q", got, want)
	}

	// (3) Release, and a fresh separate process now gets in.
	if err := held.Release(); err != nil {
		t.Fatalf("parent Release: %v", err)
	}
	got = runChildToCompletion(t, modeExpectFree, dir, "")
	if !strings.HasPrefix(got, "acquired pid=") {
		t.Fatalf("expect-free child reported %q, want an \"acquired pid=...\" line", got)
	}
	childPID, err := strconv.Atoi(strings.TrimPrefix(got, "acquired pid="))
	if err != nil || childPID <= 0 {
		t.Fatalf("expect-free child reported an unusable pid in %q: %v", got, err)
	}
	if childPID == os.Getpid() {
		t.Fatalf("the child reported OUR pid (%d): the test never re-exec'd a separate process", childPID)
	}
	// The lock file now carries the child's pid, which is the on-disk proof that
	// a different process really took the lock.
	body, err = os.ReadFile(filepath.Join(dir, LockFileName))
	if err != nil {
		t.Fatalf("read lock file after the successful child: %v", err)
	}
	if got, want := string(body), strconv.Itoa(childPID)+"\n"; got != want {
		t.Fatalf("lock file contents = %q, want the child's pid line %q", got, want)
	}
}

// TestDirLockReleasedAfterSIGKILL is the "there is no such thing as a stale
// lock" claim, asserted rather than assumed.
//
// A child acquires the lock and blocks. SIGKILL cannot be caught, blocked or
// handled: no defer, no Release, no Go runtime shutdown runs. Only the KERNEL
// drops the flock, and only because it closes the dead process's descriptors.
// The parent then proves that (a) the child really died on SIGKILL, by wait
// status -- a child that exited normally would silently make this a test of
// nothing -- (b) the lock file SURVIVES, because dirlock never unlinks, and
// (c) the very next Acquire succeeds with no cleanup, no liveness probe and no
// sleep.
func TestDirLockReleasedAfterSIGKILL(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	report := filepath.Join(base, "report-hold")

	// A dedicated pipe carries the handshake, so the parent waits on an EVENT
	// rather than on a sleep.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pr.Close()

	cmd := childCmd(t, modeHoldUntilKilled, dir, report, "")
	cmd.ExtraFiles = []*os.File{pw} // becomes fd 3 in the child
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		pw.Close()
		t.Fatalf("starting the hold child: %v", err)
	}
	// Drop the parent's copy of the write end: otherwise a dead child would
	// never produce EOF and the read below would block until the deadline.
	pw.Close()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
			<-waited
		}
	}()

	// Handshake: the child writes one line AFTER Acquire returned.
	line := make(chan string, 1)
	go func() {
		buf := make([]byte, 128)
		n, _ := pr.Read(buf)
		line <- string(buf[:n])
	}()
	var hs string
	select {
	case hs = <-line:
	case <-time.After(childWait):
		// Reap before reading out: exec's stdout/stderr copy goroutines write
		// into that buffer, so touching it while the child lives is a genuine
		// data race layered on top of an already-failing test.
		_ = cmd.Process.Kill()
		<-waited
		reaped = true
		t.Fatalf("the hold child never signalled that it holds the lock within %v\n--- child output ---\n%s",
			childWait, out.String())
	}
	wantHS := fmt.Sprintf("holding pid=%d\n", cmd.Process.Pid)
	if hs != wantHS {
		t.Fatalf("handshake = %q, want %q (an empty handshake means the child died before acquiring)"+
			"\n--- child output ---\n%s", hs, wantHS, out.String())
	}
	childPID := cmd.Process.Pid
	if childPID == os.Getpid() {
		t.Fatalf("the child pid equals ours (%d): nothing was re-exec'd", childPID)
	}

	lockPath := filepath.Join(dir, LockFileName)
	if body, err := os.ReadFile(lockPath); err != nil {
		t.Fatalf("read lock file while the child holds it: %v", err)
	} else if got, want := string(body), strconv.Itoa(childPID)+"\n"; got != want {
		t.Fatalf("lock file contents = %q, want the child's pid line %q", got, want)
	}

	// The child's lock is real and enforced against this process too.
	if lk, err := Acquire(dir); err == nil {
		_ = lk.Release()
		t.Fatalf("Acquire succeeded while a separate process holds the lock")
	} else {
		var busy *BusyError
		if !errors.As(err, &busy) || !errors.Is(err, ErrLocked) {
			t.Fatalf("Acquire against the holding child = %v, want a *BusyError satisfying ErrLocked", err)
		}
		if busy.HolderPID != childPID {
			t.Fatalf("BusyError.HolderPID = %d, want the child's pid %d", busy.HolderPID, childPID)
		}
	}

	// ---- the kill. Uncatchable: nothing of the child's runs afterwards. ----
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL the hold child: %v", err)
	}
	var runErr error
	select {
	case runErr = <-waited:
		reaped = true
	case <-time.After(childWait):
		// No child output here on purpose: the process survived SIGKILL long
		// enough to time out, so its copy goroutines may still be writing into
		// out and reading it would race. A process that outlives a SIGKILL is
		// the finding; its stdout is not.
		t.Fatalf("the hold child did not die within %v of SIGKILL", childWait)
	}

	// "err != nil" is NOT the assertion: a child that failed its own checks also
	// exits non-zero. The wait status is.
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("the hold child returned %v, want an *exec.ExitError from a signalled death"+
			"\n--- child output ---\n%s", runErr, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("wait status is %T, want syscall.WaitStatus", ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		how := fmt.Sprintf("exited normally with status %d", ws.ExitStatus())
		if ws.Signaled() {
			// A catchable signal is NOT the same experiment: the runtime gets to
			// run handlers, so a pass would not prove the kernel released anything.
			how = fmt.Sprintf("died on the catchable signal %v", ws.Signal())
		}
		t.Fatalf("the hold child %s instead of dying on SIGKILL; "+
			"the crash was never injected and this test proves nothing\n--- child output ---\n%s",
			how, out.String())
	}

	// The child got far enough to record that it held the lock.
	if body, err := os.ReadFile(report); err != nil {
		t.Fatalf("the hold child left no report (%v): it never acquired\n--- child output ---\n%s", err, out.String())
	} else if got, want := string(body), fmt.Sprintf("holding pid=%d", childPID); got != want {
		t.Fatalf("hold child report = %q, want %q", got, want)
	}

	// (b) The lock FILE survives the crash -- nothing unlinks it -- and still
	// carries the dead process's pid. Nobody tidied up on the way out.
	body, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("the lock file vanished when the holder was killed: %v", err)
	}
	if got, want := string(body), strconv.Itoa(childPID)+"\n"; got != want {
		t.Fatalf("after the kill the lock file holds %q, want the dead holder's pid line %q", got, want)
	}

	// (c) ...and yet the lock itself is gone: the next start just works, with no
	// stale-lock cleanup, no pid liveness probe and no waiting.
	lk, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after the holder was SIGKILLed = %v, want success: "+
			"a kernel-released flock must leave NO stale lock", err)
	}
	defer func() {
		if err := lk.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	}()
	body, err = os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock file after re-acquiring: %v", err)
	}
	if got, want := string(body), strconv.Itoa(os.Getpid())+"\n"; got != want {
		t.Fatalf("after re-acquiring, the lock file holds %q, want our pid line %q "+
			"(the dead holder's pid must have been overwritten, not appended to)", got, want)
	}
}
