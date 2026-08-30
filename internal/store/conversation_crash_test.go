//go:build linux || darwin

// CONV-RECORD's CRASH-INJECTION EVIDENCE: a real SIGKILL inside the two-phase
// write path of a conversation Create, and what recovery then yields.
//
// # WHY A SIGKILL, AND WHY A SECOND CREATE
//
// CLAUDE.md requires durability and recovery code to have crash-injection tests —
// "a test that writes, kills at a chosen point in the write path, and asserts
// what recovery yields" — because "the code looks right" is not evidence for a
// durability claim, and a graceful Close proves only that the happy path tidies
// up. Only a SIGKILL leaves the file in the state a power cut would.
//
// The child creates ONE conversation for real (it must exist before there is a
// second Create to crash, and it is the prefix recovery must preserve), then
// crashes a SECOND Create at a chosen point:
//
//	convCrashBeforeWrite   killed before any second-Create byte reaches the file.
//	                       Recovery yields exactly the first conversation.
//	convCrashPrepare       killed after the PREPARE record is fsynced, before any
//	                       COMMIT. Recovery yields the first conversation only, the
//	                       second is nowhere, and the index the discarded prepare
//	                       burned is never handed out again (invariant 1).
//	convCrashCommit        killed after the COMMIT record is fsynced, before Create
//	                       returns and before foldIn runs. Recovery MUST yield BOTH:
//	                       the second is durable, so it is history whether or not any
//	                       caller was ever told (invariant 4).
package store_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	envConvCrashPoint = "CONV_CRASH_POINT"
	envConvCrashDir   = "CONV_CRASH_DIR"

	convCrashBeforeWrite = "before-write"
	convCrashPrepare     = "prepare"
	convCrashCommit      = "commit"

	// convFirstIDFile holds the first (durable, non-crashed) conversation's id, so
	// the parent can assert it survived by exact id rather than by guessing.
	convFirstIDFile = "first-conv-id"
)

// ---------------------------------------------------------------------------
// The child
// ---------------------------------------------------------------------------

// TestConversationCrashChild is the child half of every crash test here. Without
// envConvCrashPoint it skips immediately, so it costs nothing in a normal run.
func TestConversationCrashChild(t *testing.T) {
	point := os.Getenv(envConvCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envConvCrashPoint + " is unset")
	}
	dir := os.Getenv(envConvCrashDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envConvCrashPoint, point, envConvCrashDir)
	}

	// NO defer Close and NO t.Cleanup that closes: a Close that ran would be
	// exactly the graceful shutdown this file exists to rule out.
	st, err := store.NewConversationStore(store.ConversationStoreOptions{BusID: convBusID})
	if err != nil {
		t.Fatalf("child: NewConversationStore: %v", err)
	}
	l, err := wal.Open(wal.LogOptions{Dir: dir, Applier: st})
	if err != nil {
		t.Fatalf("child: wal.Open: %v", err)
	}

	var at string
	switch point {
	case convCrashBeforeWrite:
		at = convKillBeforeWrite
	case convCrashPrepare:
		at = convKillAtPrepare
	case convCrashCommit:
		at = convKillAtCommit
	default:
		t.Fatalf("child: unknown crash point %q", point)
	}

	// nth = 2: the FIRST conversation Create must complete for real; the second is
	// the one cut into.
	durable := &convKillOnNth{l: l, nth: 2, at: at}
	if err := st.Attach(durable); err != nil {
		t.Fatalf("child: Attach: %v", err)
	}

	first, err := st.Create(convCreator, "first", []string{convRecipient})
	if err != nil {
		t.Fatalf("child: first Create must succeed before the second can be crashed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, convFirstIDFile), []byte(first.ID), 0o600); err != nil {
		t.Fatalf("child: recording the first conversation id: %v", err)
	}

	_, err = st.Create(convCreator, "second", []string{convRecipient})
	t.Fatalf("child: second Create returned %v but the durable log kills this process at %q; THE CRASH WAS NEVER INJECTED and any parent assertion built on it would be meaningless", err, point)
}

const (
	convKillBeforeWrite = "before-write"
	convKillAtPrepare   = "prepare"
	convKillAtCommit    = "commit"
)

// convKillOnNth kills the process at a chosen point in the write path, on the nth
// conversation write. Counting matters: killing on the first would cut the wrong
// Create and the test would still look like an injected crash while asserting
// something else. Every other kind is passed through untouched.
type convKillOnNth struct {
	l   *wal.Log
	nth int
	at  string
	n   int
}

func (k *convKillOnNth) Write(e wal.Entry) (wal.Committed, error) {
	if e.Kind != store.ConversationRecordKind {
		return k.l.Write(e)
	}
	k.n++
	if k.n != k.nth {
		return k.l.Write(e)
	}
	switch k.at {
	case convKillBeforeWrite:
		convKillSelf()
	case convKillAtPrepare:
		if _, err := k.l.Begin(e); err != nil {
			return wal.Committed{}, err
		}
		// Begin returns only once the PREPARE record is FSYNCED. The Txn is never
		// resolved and the lock never released: nothing runs after the kill.
		convKillSelf()
	}
	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	// Write returns only once the COMMIT record is fsynced. The entry is accepted
	// history; the caller is about to not be told.
	convKillSelf()
	return c, nil
}

// convKillSelf is a real SIGKILL to this process. Not os.Exit: os.Exit still runs
// the runtime exit path, and the point is that NOTHING runs.
func convKillSelf() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {} // unreachable; the signal is not catchable
}

// ---------------------------------------------------------------------------
// The parent
// ---------------------------------------------------------------------------

func runConvCrashChild(t *testing.T, dir, point string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConversationCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		envConvCrashPoint+"="+point,
		envConvCrashDir+"="+dir,
	)
	out, err := cmd.CombinedOutput()

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child at %q returned err=%v, want an ExitError from SIGKILL.\nIf it exited cleanly the crash was never injected and nothing below is evidence.\nchild output:\n%s", point, err, out)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child at %q did not die of SIGKILL (waitstatus %v).\nchild output:\n%s", point, ee.Sys(), out)
	}
	if strings.Contains(string(out), "--- SKIP") {
		t.Fatalf("crash child at %q SKIPPED; the crash window was never entered.\nchild output:\n%s", point, out)
	}
}

func readFirstConvID(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, convFirstIDFile))
	if err != nil {
		t.Fatalf("reading the first conversation id the child recorded: %v", err)
	}
	return string(b)
}

// convIndexSpy sits between the store and the real *wal.Log and remembers the
// indices the log handed out, so invariant 1 can be asserted on the PrepareIndex
// of a post-recovery Create.
type convIndexSpy struct {
	l    *wal.Log
	mu   sync.Mutex
	last wal.Committed
}

func (s *convIndexSpy) Write(e wal.Entry) (wal.Committed, error) {
	c, err := s.l.Write(e)
	if err == nil {
		s.mu.Lock()
		s.last = c
		s.mu.Unlock()
	}
	return c, err
}

func (s *convIndexSpy) lastCommitted() wal.Committed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// openConvStoreSpy opens a real wal.Log over dir with a fresh store, attaching a
// spy so a post-recovery Create's indices are observable.
func openConvStoreSpy(t *testing.T, dir string) (*store.ConversationStore, *wal.Log, *convIndexSpy) {
	t.Helper()
	lg := logging.New(&convCapLog{}, logging.LevelDebug)
	st, err := store.NewConversationStore(store.ConversationStoreOptions{BusID: convBusID, Logger: lg})
	if err != nil {
		t.Fatalf("NewConversationStore: %v", err)
	}
	l, err := wal.Open(wal.LogOptions{Dir: dir, Logger: lg, Applier: st})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	spy := &convIndexSpy{l: l}
	if err := st.Attach(spy); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return st, l, spy
}

// TestConversationCrashedAfterCommitIsRecovered — invariant 4. The COMMIT record
// is fsynced and the process dies before Create returns and before foldIn touches
// the serving copy. Nobody was told, and memory never learned. The record is
// nonetheless on stable storage, so both conversations are history.
func TestConversationCrashedAfterCommitIsRecovered(t *testing.T) {
	dir := t.TempDir()
	runConvCrashChild(t, dir, convCrashCommit)
	firstID := readFirstConvID(t, dir)

	st, l, _ := openConvStoreSpy(t, dir)
	defer l.Close()

	if _, ok := st.Get(firstID); !ok {
		t.Fatalf("the first conversation is missing; its Create completed before the crash window, so losing it is a record acknowledged then lost (invariant 4)")
	}
	if n := st.Len(); n != 2 {
		t.Errorf("Len = %d, want 2: the second Create's COMMIT was fsynced before the kill, so it is accepted history and recovering fewer than 2 loses a durable record", n)
	}
}

// TestConversationCrashedAtPrepareIsNotRecovered — invariants 5 and 1. The
// PREPARE record is fsynced; no COMMIT is ever written. Recovery must yield a
// PREFIX: the first conversation only, and the burned index is never re-used.
func TestConversationCrashedAtPrepareIsNotRecovered(t *testing.T) {
	dir := t.TempDir()
	runConvCrashChild(t, dir, convCrashPrepare)
	firstID := readFirstConvID(t, dir)

	st, l, spy := openConvStoreSpy(t, dir)
	defer l.Close()

	if _, ok := st.Get(firstID); !ok {
		t.Fatalf("the first conversation is missing; its write completed before the crash window (invariant 4)")
	}
	if n := st.Len(); n != 1 {
		t.Errorf("Len = %d, want 1: the second Create reached PREPARE and never COMMIT, so it is not accepted history and must not be served (invariant 5)", n)
	}

	rec := l.Recovered()
	if len(rec.Dangling) == 0 {
		t.Fatalf("recovery reports NO dangling prepare, but the child was killed between the prepare fsync and the commit. Either the crash was not injected where this test believes, or the discarded prepare left no trace")
	}
	burned := rec.Dangling[len(rec.Dangling)-1]
	if rec.NextIndex <= burned {
		t.Errorf("Recovered().NextIndex = %d but index %d was burned by the discarded prepare. Invariant 1: when recovery discards a record the sequence ADVANCES PAST THE HOLE; it never rewinds", rec.NextIndex, burned)
	}

	// Prove it with a real append, not only with the reported number.
	if _, err := st.Create(convCreator, "after-recovery", []string{convRecipient}); err != nil {
		t.Fatalf("creating a conversation after recovery: %v", err)
	}
	if got := spy.lastCommitted().PrepareIndex; got <= burned {
		t.Errorf("the first write after recovery took prepare index %d, but the discarded prepare had already burned %d. A salvage path that re-uses a discarded index is a DEFECT, not a licence to narrow invariant 1", got, burned)
	}
}

// TestConversationCrashedBeforeAnyWriteLeavesTheFirst is the control: killed
// before a single second-Create byte reaches the file, so recovery yields exactly
// the first conversation and nothing else. It pins the boundary from the other
// side, so the prepare case cannot pass for the wrong reason.
func TestConversationCrashedBeforeAnyWriteLeavesTheFirst(t *testing.T) {
	dir := t.TempDir()
	runConvCrashChild(t, dir, convCrashBeforeWrite)
	firstID := readFirstConvID(t, dir)

	st, l, _ := openConvStoreSpy(t, dir)
	defer l.Close()

	if _, ok := st.Get(firstID); !ok {
		t.Fatalf("the first conversation is missing; its write completed before the crash window opened")
	}
	if n := st.Len(); n != 1 {
		t.Errorf("Len = %d, want exactly 1", n)
	}
	if d := l.Recovered().Dangling; len(d) != 0 {
		t.Errorf("recovery reports dangling prepares %v, want none: the child died before the second Write was called at all", d)
	}
}
