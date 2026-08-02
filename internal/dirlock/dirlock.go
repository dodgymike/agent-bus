// Package dirlock holds an exclusive advisory lock on a bus data directory for
// the lifetime of the process that acquired it.
//
// # Why this exists
//
// Two agent-bus servers started on one data directory destroy the write-ahead
// log. internal/wal/log.go's replay-vs-open offset agreement check says so in
// its own words: it "IS NOT A LOCK", it only catches a change inside the window
// between the two passes, and two servers "can both replay the same bytes, both
// agree, and then both append at the same offsets, which destroys the log".
// This package is the real lock that comment asks for, and Acquire is called by
// cmd/agent-bus BEFORE anything reads or writes the data dir, so a WAL replay
// always happens inside the lock.
//
// # Mechanism
//
// A regular file inside the data directory is opened and locked with
// syscall.Flock(LOCK_EX|LOCK_NB) — stdlib only (invariant 8: golang.org/x/sys
// would be a third-party dependency needing a DECISIONS.md entry, and buys us
// nothing here). The lock is associated with the OPEN FILE DESCRIPTION, so:
//
//   - The KERNEL releases it when the holding process exits, by any route,
//     including SIGKILL, a panic, or the OOM killer. There is therefore no such
//     thing as a "stale lock" to clean up — see StaleLocks below.
//   - Go's os.OpenFile passes O_CLOEXEC, so an exec'd child does NOT inherit the
//     descriptor and cannot keep the lock alive past this process.
//
// # Limits an operator must know
//
// flock is ADVISORY. It excludes only other processes that also flock the same
// file — in practice, other agent-bus servers. It does not stop `rm`, `cp`, an
// editor, a backup job, or a program that has never heard of this lock from
// mangling the data directory. It is a guard against the realistic accident
// (starting the server twice), not a mandatory-locking security control.
//
// flock over NFS is unreliable before Linux 2.6.12 (where it started being
// emulated with whole-file POSIX locks), and its behaviour across other network
// filesystems varies. An operator who puts a bus data directory on such a mount
// gets NO protection from this package. We deliberately do not attempt to solve
// that: the fix is "do not run the durable store on a filesystem whose locking
// you do not trust", not a cleverer lock (invariant 8, simple beats clever).
//
// # StaleLocks
//
// A crashed server leaves the lock FILE behind but no LOCK: the kernel dropped
// the flock when the process died, so the next start acquires it normally and
// the leftover pid line is simply overwritten. This package therefore NEVER
// probes whether a recorded pid is alive and NEVER deletes a lock file it did
// not lock. Both are how a locking scheme grows two simultaneous holders: pids
// are recycled, and a liveness probe plus an unlink is a race with every other
// starter. The lock file living forever is correct and intended.
package dirlock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

// LockFileName is the lock file's name within the data directory.
//
// Deliberately NOT "*.log": the WAL is "wal.log" and the repo's .gitignore
// ignores "*.log", so a lock file ending in .log would be one typo away from
// being mistaken for log data by a human, a glob, or a future recovery routine
// — and would be invisible in `git status`. "bus.lock" cannot be confused with
// a WAL segment, sorts next to the other bus-scoped files, and its extension
// says exactly what it is.
//
// The file's ONLY contents are a single "<pid>\n" line, written purely so a
// refusal can name a probable holder. Nothing in this repo may treat it as a
// record store; it is not part of the durable state and replay never reads it.
const LockFileName = "bus.lock"

// lockFileMode matches the 0o700 data directory, which holds agent
// credentials: nothing outside the owner has any business reading, let alone
// locking, the bus's data.
const lockFileMode os.FileMode = 0o600

// maxPIDBytes bounds the best-effort read of a lock file belonging to someone
// else. The file we write is at most a handful of bytes; anything longer is not
// ours, and an unbounded read of an attacker- or accident-supplied file has no
// place on a startup error path.
const maxPIDBytes = 64

// ErrLocked reports that another live process holds the data directory lock.
// Callers test for it with errors.Is; the concrete *BusyError carries the
// actionable detail.
var ErrLocked = errors.New("dirlock: data directory is locked by another process")

// BusyError is the error returned when the lock is already held. It names the
// directory (the thing an operator has to fix) and, when it could be read, the
// pid recorded by the holder.
//
// HolderPID is BEST-EFFORT AND ADVISORY, never authoritative: it is read from
// the lock file AFTER our flock failed, so the holder may already have exited,
// the file may predate the current holder, or the value may have been written
// by something else entirely. Treat it as a hint for `ps`, not as proof.
// HolderPID is 0 when no plausible pid could be read.
type BusyError struct {
	// Dir is the data directory that could not be locked.
	Dir string
	// Path is the lock file within Dir.
	Path string
	// HolderPID is the best-effort, advisory pid of the presumed holder, or 0.
	HolderPID int
}

// Error names the directory and, when known, the probable holder — and says why
// refusing to start is the right outcome.
func (e *BusyError) Error() string {
	if e.HolderPID > 0 {
		return fmt.Sprintf("dirlock: data directory %q is locked by another agent-bus process (pid %d, best-effort: read from %s after the lock failed, so it may be stale); refusing to start — two servers on one data directory destroy the write-ahead log",
			e.Dir, e.HolderPID, e.Path)
	}
	return fmt.Sprintf("dirlock: data directory %q is locked by another agent-bus process (holder pid unknown); refusing to start — two servers on one data directory destroy the write-ahead log",
		e.Dir)
}

// Is makes errors.Is(err, ErrLocked) true for every BusyError.
func (e *BusyError) Is(target error) bool { return target == ErrLocked }

// Lock is an exclusive advisory lock held on a bus data directory for the
// lifetime of the process that acquired it. The zero value is not usable; get
// one from Acquire. A nil *Lock is safe to Release.
type Lock struct {
	// f is the open lock file. Holding it open is what holds the lock: the
	// flock lives on the open file description, not on the path.
	f *os.File
	// dir and path are kept for Path and for error/log messages.
	dir  string
	path string

	// mu guards released so Release is idempotent under a defer plus an
	// explicit call, and safe if two goroutines race to shut down.
	mu       sync.Mutex
	released bool
}

// Acquire takes the exclusive lock on dir, or fails immediately.
//
// It never blocks: LOCK_NB means a second server fails fast and loudly with a
// *BusyError (errors.Is(err, ErrLocked)) instead of hanging on startup or,
// worse, quietly proceeding. There is deliberately no blocking variant — every
// caller we have wants "refuse to start", and a bus that waits for another bus
// to exit is a confusing failure mode, not a feature.
//
// The caller must Release the returned Lock (a defer is fine), though process
// exit releases it too.
//
// THE RETURNED *Lock MUST STAY REACHABLE for as long as the lock is wanted.
// Discarding it (`_, err := dirlock.Acquire(dir)`) is a silent bug, not a
// leak-and-forget: the lock lives on the open file description, and the
// *os.File inside carries a finalizer that closes the descriptor once nothing
// references it — so the next GC would quietly hand the data directory to a
// second server. cmd/agent-bus keeps it reachable by capturing it in the
// deferred Release closure that runs for the whole life of run().
func Acquire(dir string) (*Lock, error) {
	// Validate the input before touching the filesystem: a mistyped -data-dir
	// that happens to name a regular file must produce a clear error, not a
	// confusing open failure inside it.
	if dir == "" {
		return nil, errors.New("dirlock: data directory must not be empty")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("dirlock: data directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("dirlock: data directory %q is not a directory", dir)
	}

	path := filepath.Join(dir, LockFileName)

	// O_CREATE|O_RDWR and emphatically NOT O_TRUNC: truncation before we own the
	// lock would wipe the live holder's pid line out from under it, so a third
	// starter could no longer name the holder. We truncate only once the flock
	// is ours. os.OpenFile sets O_CLOEXEC, so this descriptor — and therefore
	// the lock — does not survive into an exec'd child.
	//
	// O_NOFOLLOW because we TRUNCATE this path once the lock is ours: a symlink
	// planted at <dir>/bus.lock would otherwise turn Acquire into a "truncate
	// any file the bus user can write, and create it if missing" primitive, with
	// the damage landing outside the data directory. Planting it already
	// requires being the bus user (the data dir is 0o700, and that user can
	// shred wal.log directly), so this crosses no privilege boundary — but it
	// keeps the blast radius of a data-dir compromise inside the data dir, for
	// the cost of one flag. A symlink here now fails with ELOOP.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, lockFileMode)
	if err != nil {
		return nil, fmt.Errorf("dirlock: opening lock file %q: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// EWOULDBLOCK (== EAGAIN) is the expected "someone else holds it";
		// anything else is a real failure worth surfacing verbatim.
		busy := errors.Is(err, syscall.EWOULDBLOCK)
		pid := 0
		if busy {
			pid = readHolderPID(f)
		}
		closeErr := f.Close()
		if !busy {
			if closeErr != nil {
				return nil, fmt.Errorf("dirlock: locking %q: %w (close: %v)", path, err, closeErr)
			}
			return nil, fmt.Errorf("dirlock: locking %q: %w", path, err)
		}
		return nil, &BusyError{Dir: dir, Path: path, HolderPID: pid}
	}

	// The lock is ours from here, so it is now safe to replace the previous
	// holder's pid line. Truncate first: a shorter pid must not leave trailing
	// digits of a longer one behind.
	if err := writePID(f, os.Getpid()); err != nil {
		// Best effort to leave nothing locked behind on a failed Acquire.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("dirlock: recording pid in %q: %w", path, err)
	}

	return &Lock{f: f, dir: dir, path: path}, nil
}

// writePID truncates the lock file and writes "<pid>\n", then fsyncs it.
//
// The fsync is not a durability guarantee anyone depends on — the LOCK is
// enforced by the kernel, not by these bytes — but a pid that never reaches the
// disk is useless to the operator debugging a refusal, which is the file's only
// purpose.
func writePID(f *os.File, pid int) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(pid)+"\n"), 0); err != nil {
		return err
	}
	return f.Sync()
}

// readHolderPID reads the presumed holder's pid from an already-open lock file,
// best effort: any problem yields 0 and a message that simply omits the pid.
//
// SANITISATION: the contents belong to another process and must never reach a
// log line or an error string verbatim. We read at most maxPIDBytes, accept
// ONLY a run of ASCII digits, and return an int — so the only thing that can
// escape into a message is a number we parsed ourselves. Anything else (an
// empty file just created by us, junk, a huge value, a negative number, control
// characters, ANSI escapes) is discarded as "unknown".
func readHolderPID(f *os.File) int {
	buf := make([]byte, maxPIDBytes)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return 0
	}
	digits := make([]byte, 0, n)
	for _, b := range buf[:n] {
		if b >= '0' && b <= '9' {
			digits = append(digits, b)
			continue
		}
		break // stop at the first non-digit; "12\n" yields 12, "x12" yields none
	}
	if len(digits) == 0 {
		return 0
	}
	pid, err := strconv.Atoi(string(digits))
	if err != nil || pid <= 0 {
		return 0 // overflow or nonsense: report "unknown" rather than a lie
	}
	return pid
}

// Release drops the lock. It is idempotent and safe in a defer, and a nil
// *Lock is a no-op, so shutdown paths never have to guard the call.
//
// Release deliberately does NOT unlink the lock file. Unlinking races: process
// B can still hold its descriptor (and its flock) on the now-unlinked inode
// while process C creates a FRESH file at the same path and locks that
// successfully — two holders on one data directory, which is exactly the
// disaster this package exists to prevent. Leaving the file in place means
// every process always locks the same inode. The file persisting forever is
// correct and intended; see the StaleLocks note in the package doc.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true

	// A zero-value Lock{} has no file. It is not a usable lock (only Acquire
	// mints one), so releasing it is a no-op rather than a confusing
	// "unlocking "": bad file descriptor".
	if l.f == nil {
		return nil
	}

	// Close alone releases the flock (it drops the last reference to the open
	// file description), but the explicit LOCK_UN documents the intent and
	// releases promptly even if something else has dup'd the descriptor. Close
	// runs unconditionally, even when LOCK_UN failed, so the kernel drops the
	// lock either way and Release can never report success without releasing.
	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	if unlockErr != nil {
		return fmt.Errorf("dirlock: unlocking %q: %w", l.path, unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("dirlock: closing %q: %w", l.path, closeErr)
	}
	return nil
}

// Path reports the lock file's path, for logging and tests. Safe on a nil
// *Lock, which reports "".
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Dir reports the locked data directory. Safe on a nil *Lock.
func (l *Lock) Dir() string {
	if l == nil {
		return ""
	}
	return l.dir
}
