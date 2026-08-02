package dirlock

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every test uses t.TempDir(): the tracked ./data directory is never a fixture.

// TestAcquireRecordsPIDAndReleaseIsRepeatable covers the happy path and the
// two properties shutdown code depends on: Release is idempotent, and it does
// NOT unlink the lock file (unlinking would let a second process lock a fresh
// inode at the same path -- two holders on one data dir).
func TestAcquireRecordsPIDAndReleaseIsRepeatable(t *testing.T) {
	dir := t.TempDir()

	lk, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire on a fresh dir: %v", err)
	}
	want := filepath.Join(dir, LockFileName)
	if lk.Path() != want {
		t.Errorf("Path() = %q, want %q", lk.Path(), want)
	}
	if lk.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", lk.Dir(), dir)
	}
	if strings.HasSuffix(LockFileName, ".log") {
		t.Errorf("LockFileName = %q must not end in .log: it would be confusable with wal.log and is gitignored", LockFileName)
	}

	info, err := os.Stat(want)
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if got := info.Mode().Perm(); got != lockFileMode.Perm() {
		t.Errorf("lock file mode = %v, want %v", got, lockFileMode.Perm())
	}
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if got, want := string(body), strconv.Itoa(os.Getpid())+"\n"; got != want {
		t.Errorf("lock file contents = %q, want %q", got, want)
	}

	if err := lk.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("second Release must be a no-op, got %v", err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("Release must not unlink the lock file: %v", err)
	}
	// Released means re-acquirable.
	lk2, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := lk2.Release(); err != nil {
		t.Fatalf("Release lk2: %v", err)
	}

	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Errorf("nil *Lock Release = %v, want nil", err)
	}
	if nilLock.Path() != "" {
		t.Errorf("nil *Lock Path() = %q, want empty", nilLock.Path())
	}
}

// TestSecondAcquireFailsFast is the substance of DUR-8: while a lock is held,
// another Acquire on the same directory must fail immediately (LOCK_NB, never
// block, never silently succeed), report ErrLocked, and name the directory and
// the recorded holder pid.
//
// flock locks the open file description, so a second Acquire inside this same
// process is a genuine conflict -- no subprocess needed. The cross-process and
// SIGKILL cases belong to the test-engineer's crash tests.
func TestSecondAcquireFailsFast(t *testing.T) {
	dir := t.TempDir()

	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer func() {
		if err := first.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
	}()

	second, err := Acquire(dir)
	if err == nil {
		_ = second.Release()
		t.Fatal("second Acquire succeeded; two servers on one data directory destroy the WAL")
	}
	if second != nil {
		t.Errorf("second Acquire returned a non-nil Lock alongside its error")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire error %v does not satisfy errors.Is(err, ErrLocked)", err)
	}
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("second Acquire error %v is not a *BusyError", err)
	}
	if busy.Dir != dir {
		t.Errorf("BusyError.Dir = %q, want %q", busy.Dir, dir)
	}
	if busy.HolderPID != os.Getpid() {
		t.Errorf("BusyError.HolderPID = %d, want %d", busy.HolderPID, os.Getpid())
	}
	if msg := err.Error(); !strings.Contains(msg, dir) || !strings.Contains(msg, strconv.Itoa(os.Getpid())) {
		t.Errorf("error message %q must name the directory and the holder pid", msg)
	}
}

// TestAcquireDoesNotTruncateWhileHeld pins invariant 4: opening the lock file
// must not use O_TRUNC, or a failed Acquire would wipe the live holder's pid.
func TestAcquireDoesNotTruncateWhileHeld(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer first.Release()

	if _, err := Acquire(dir); err == nil {
		t.Fatal("second Acquire unexpectedly succeeded")
	}
	body, err := os.ReadFile(first.Path())
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if got, want := string(body), strconv.Itoa(os.Getpid())+"\n"; got != want {
		t.Errorf("a failed Acquire truncated the holder's lock file: contents = %q, want %q", got, want)
	}
}

// TestAcquireRejectsBadDir covers the input validation: a missing path, a
// regular file mistyped as -data-dir, and the empty string each get a clear
// error rather than a confusing failure deeper in.
func TestAcquireRejectsBadDir(t *testing.T) {
	tmp := t.TempDir()
	regular := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}

	tests := []struct {
		name    string
		dir     string
		wantMsg string
	}{
		{name: "empty", dir: "", wantMsg: "must not be empty"},
		{name: "missing", dir: filepath.Join(tmp, "nope"), wantMsg: "nope"},
		{name: "regular file", dir: regular, wantMsg: "is not a directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lk, err := Acquire(tc.dir)
			if err == nil {
				_ = lk.Release()
				t.Fatalf("Acquire(%q) succeeded, want an error", tc.dir)
			}
			if errors.Is(err, ErrLocked) {
				t.Errorf("Acquire(%q) reported ErrLocked for a bad path: %v", tc.dir, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Acquire(%q) error = %q, want it to contain %q", tc.dir, err, tc.wantMsg)
			}
		})
	}
}

// TestReadHolderPIDSanitises pins the rule that bytes from someone else's file
// never reach a log line verbatim: only a leading run of digits is accepted,
// everything else reports "unknown" (0).
func TestReadHolderPIDSanitises(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{name: "plain pid", content: "4242\n", want: 4242},
		{name: "no newline", content: "7", want: 7},
		{name: "empty", content: "", want: 0},
		{name: "zero", content: "0\n", want: 0},
		{name: "negative", content: "-5\n", want: 0},
		{name: "junk", content: "not-a-pid\n", want: 0},
		{name: "ansi escape", content: "\x1b[31mboom\x1b[0m\n", want: 0},
		{name: "leading space", content: " 12\n", want: 0},
		{name: "trailing junk ignored", content: "99 rm -rf /\n", want: 99},
		{name: "overlong digits", content: strings.Repeat("9", 128), want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), LockFileName)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()
			if got := readHolderPID(f); got != tc.want {
				t.Errorf("readHolderPID(%q) = %d, want %d", tc.content, got, tc.want)
			}
		})
	}
}

// TestBusyErrorMessageWithoutPID checks the "holder pid unknown" wording, which
// is what an operator sees when the lock file could not be read.
func TestBusyErrorMessageWithoutPID(t *testing.T) {
	err := &BusyError{Dir: "/x/y", Path: "/x/y/" + LockFileName}
	msg := err.Error()
	if !strings.Contains(msg, "/x/y") || !strings.Contains(msg, "unknown") {
		t.Errorf("message = %q, want it to name the dir and say the pid is unknown", msg)
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("BusyError must satisfy errors.Is(err, ErrLocked)")
	}
}
