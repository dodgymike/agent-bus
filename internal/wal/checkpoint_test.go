package wal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dodgymike/agent-bus/internal/logging"
)

type checkpointTestParticipant struct {
	name         string
	kinds        []string
	seen         []Committed
	restoredHigh uint64
	applyOrder   *[]uint64
}

func TestCheckpointMissingCurrentUsesNewestAuthenticatedGeneration(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, err := Open(LogOptions{Dir: d, Checkpoints: m})
	if err != nil {
		t.Fatal(err)
	}
	writeCheckpointEntry(t, l, "a", 1)
	if err = l.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	latest := writeCheckpointEntry(t, l, "a", 2)
	if err = l.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(d, checkpointDirName, checkpointCurrent)); err != nil {
		t.Fatal(err)
	}
	m2, a2, _ := testRegistry(t)
	l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.generation != 2 || a2.restoredHigh != latest.CommitIndex {
		t.Fatalf("generation/high-water=%d/%d want 2/%d", l2.generation, a2.restoredHigh, latest.CommitIndex)
	}
}

func TestCheckpointCurrentDirSyncAmbiguityPoisonsLog(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, err := Open(LogOptions{Dir: d, Checkpoints: m})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	writeCheckpointEntry(t, l, "a", 1)
	original := checkpointSyncDir
	t.Cleanup(func() { checkpointSyncDir = original })
	calls := 0
	checkpointSyncDir = func(path string) error {
		calls++
		if calls == 2 {
			return errors.New("injected directory sync failure")
		}
		return original(path)
	}
	if err = l.Checkpoint(); err == nil {
		t.Fatal("checkpoint succeeded across ambiguous CURRENT durability")
	}
	if _, err = l.Write(Entry{Kind: "a", Body: json.RawMessage(`{"n":2}`)}); err == nil {
		t.Fatal("write succeeded after ambiguous checkpoint publication")
	}
}

func (p *checkpointTestParticipant) Name() string    { return p.name }
func (p *checkpointTestParticipant) Kinds() []string { return p.kinds }
func (p *checkpointTestParticipant) Apply(c Committed) error {
	if p.applyOrder != nil {
		*p.applyOrder = append(*p.applyOrder, c.CommitIndex)
	}
	p.seen = append(p.seen, c)
	return nil
}
func (p *checkpointTestParticipant) Snapshot(h uint64) ([]byte, error) { return json.Marshal(p.seen) }
func (p *checkpointTestParticipant) Restore(b []byte, h uint64) error {
	p.restoredHigh = h
	return json.Unmarshal(b, &p.seen)
}

func testRegistry(t *testing.T) (*MultiApplier, *checkpointTestParticipant, *checkpointTestParticipant) {
	t.Helper()
	a := &checkpointTestParticipant{name: "alpha", kinds: []string{"a"}}
	b := &checkpointTestParticipant{name: "beta", kinds: []string{"b"}}
	m, e := NewMultiApplier(b, a)
	if e != nil {
		t.Fatal(e)
	}
	return m, a, b
}
func writeCheckpointEntry(t *testing.T, l *Log, kind string, n int) Committed {
	t.Helper()
	c, e := l.Write(Entry{Kind: kind, Body: json.RawMessage([]byte(`{"n":` + string(rune('0'+n)) + `}`))})
	if e != nil {
		t.Fatal(e)
	}
	return c
}

func TestCheckpointMultiApplierSharedWAL(t *testing.T) {
	d := t.TempDir()
	m, a, b := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	c1 := writeCheckpointEntry(t, l, "a", 1)
	c2 := writeCheckpointEntry(t, l, "b", 2)
	if len(a.seen) != 1 || len(b.seen) != 1 {
		t.Fatalf("routing a=%d b=%d", len(a.seen), len(b.seen))
	}
	if e = l.Checkpoint(); e != nil {
		t.Fatal(e)
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}
	m2, a2, b2 := testRegistry(t)
	l2, e := Open(LogOptions{Dir: d, Checkpoints: m2})
	if e != nil {
		t.Fatal(e)
	}
	defer l2.Close()
	if a2.restoredHigh != c2.CommitIndex || b2.restoredHigh != c2.CommitIndex {
		t.Fatalf("shared high water = %d/%d want %d", a2.restoredHigh, b2.restoredHigh, c2.CommitIndex)
	}
	if len(a2.seen) != 1 || len(b2.seen) != 1 || a2.seen[0].CommitIndex != c1.CommitIndex || b2.seen[0].CommitIndex != c2.CommitIndex {
		t.Fatalf("restored state differs: %#v %#v", a2.seen, b2.seen)
	}
}

func TestCheckpointBoundsTailReplay(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	before := writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil {
		t.Fatal(e)
	}
	after := writeCheckpointEntry(t, l, "a", 2)
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}
	m2, a2, _ := testRegistry(t)
	l2, e := Open(LogOptions{Dir: d, Checkpoints: m2})
	if e != nil {
		t.Fatal(e)
	}
	defer l2.Close()
	if len(a2.seen) != 2 || a2.seen[0].CommitIndex != before.CommitIndex || a2.seen[1].CommitIndex != after.CommitIndex {
		t.Fatalf("snapshot+bounded tail = %#v", a2.seen)
	}
	if got := l2.Recovered().Applied; got != 1 {
		t.Fatalf("tail replay applied %d entries, want exactly 1", got)
	}
}

func TestCheckpointRejectsUnauthenticatedManifest(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil {
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 2)
	if e = l.Checkpoint(); e != nil {
		t.Fatal(e)
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}
	current, e := os.ReadFile(filepath.Join(d, checkpointDirName, checkpointCurrent))
	if e != nil {
		t.Fatal(e)
	}
	manifest := filepath.Join(d, checkpointDirName, string(current[:len(current)-1]), checkpointManifestFile)
	raw, e := os.ReadFile(manifest)
	if e != nil {
		t.Fatal(e)
	}
	raw[len(raw)/2] ^= 1
	if e = os.WriteFile(manifest, raw, 0600); e != nil {
		t.Fatal(e)
	}
	m2, a2, _ := testRegistry(t)
	l2, e := Open(LogOptions{Dir: d, Checkpoints: m2})
	if e != nil {
		t.Fatal(e)
	}
	defer l2.Close()
	if l2.generation != 1 {
		t.Fatalf("generation=%d want authenticated fallback 1", l2.generation)
	}
	if len(a2.seen) != 2 {
		t.Fatalf("fallback snapshot plus its tail entries=%d want 2", len(a2.seen))
	}
}

func TestCheckpointGenerationCrashRecovery(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	c := writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil {
		t.Fatal(e)
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(d, checkpointDirName)
	if e = os.Mkdir(filepath.Join(root, "gen-00000000000000000002.tmp"), 0700); e != nil {
		t.Fatal(e)
	}
	m2, a2, _ := testRegistry(t)
	l2, e := Open(LogOptions{Dir: d, Checkpoints: m2})
	if e != nil {
		t.Fatal(e)
	}
	defer l2.Close()
	if l2.generation != 1 || a2.restoredHigh != c.CommitIndex {
		t.Fatalf("recovered generation/high-water %d/%d", l2.generation, a2.restoredHigh)
	}
	next := l2.w.NextIndex()
	fresh := writeCheckpointEntry(t, l2, "a", 2)
	if fresh.PrepareIndex != next || fresh.PrepareIndex <= c.CommitIndex {
		t.Fatalf("next index reused: got %d prior commit %d", fresh.PrepareIndex, c.CommitIndex)
	}
}

func TestCheckpointForwardAndBackwardTailIdentity(t *testing.T) {
	for _, direction := range []string{"older-into-newer", "newer-into-older"} {
		t.Run(direction, func(t *testing.T) {
			d := t.TempDir()
			m, _, _ := testRegistry(t)
			l, err := Open(LogOptions{Dir: d, Checkpoints: m})
			if err != nil {
				t.Fatal(err)
			}
			writeCheckpointEntry(t, l, "a", 1)
			if err = l.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			writeCheckpointEntry(t, l, "b", 2)
			if err = l.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			writeCheckpointEntry(t, l, "a", 3) // makes the newer tail non-empty and wholly > gen-1 H
			if err = l.Close(); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(d, checkpointDirName)
			g1 := filepath.Join(root, fmt.Sprintf("gen-%020d", 1), checkpointTailFile)
			g2 := filepath.Join(root, fmt.Sprintf("gen-%020d", 2), checkpointTailFile)
			src, dst := g1, g2
			if direction == "newer-into-older" {
				src, dst = g2, g1
			}
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(dst, b, 0600); err != nil {
				t.Fatal(err)
			}
			m2, a2, _ := spyRegistry(t)
			l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
			if direction == "older-into-newer" {
				if err != nil {
					t.Fatal(err)
				}
				defer l2.Close()
				if l2.generation != 1 || len(a2.restores) != 1 {
					t.Fatalf("fallback generation/restores=%d/%d", l2.generation, len(a2.restores))
				}
			} else {
				// Generation 2 remains valid and must be selected; the substituted
				// older candidate is never touched or restored.
				if err != nil {
					t.Fatal(err)
				}
				defer l2.Close()
				if l2.generation != 2 || len(a2.restores) != 1 {
					t.Fatalf("selected generation/restores=%d/%d", l2.generation, len(a2.restores))
				}
			}
		})
	}
}

func TestCheckpointStaleCurrentCannotRollback(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, err := Open(LogOptions{Dir: d, Checkpoints: m})
	if err != nil {
		t.Fatal(err)
	}
	writeCheckpointEntry(t, l, "a", 1)
	if err = l.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	writeCheckpointEntry(t, l, "b", 2)
	newest := writeCheckpointEntry(t, l, "a", 3)
	if err = l.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(d, checkpointDirName, checkpointCurrent), []byte("gen-00000000000000000001\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m2, a2, _ := testRegistry(t)
	l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.generation != 2 || a2.restoredHigh != newest.CommitIndex {
		t.Fatalf("rolled back to generation/high-water %d/%d", l2.generation, a2.restoredHigh)
	}
}

func TestCheckpointTailPreOpenRegularValidation(t *testing.T) {
	for _, typ := range []string{"symlink", "fifo"} {
		t.Run(typ, func(t *testing.T) {
			d := t.TempDir()
			m, _, _ := testRegistry(t)
			l, err := Open(LogOptions{Dir: d, Checkpoints: m})
			if err != nil {
				t.Fatal(err)
			}
			writeCheckpointEntry(t, l, "a", 1)
			if err = l.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			if err = l.Close(); err != nil {
				t.Fatal(err)
			}
			tail := filepath.Join(d, checkpointDirName, fmt.Sprintf("gen-%020d", 1), checkpointTailFile)
			if err = os.Remove(tail); err != nil {
				t.Fatal(err)
			}
			if typ == "symlink" {
				err = os.Symlink(filepath.Join(d, WALFileName), tail)
			} else {
				err = mkfifo(tail)
			}
			if err != nil {
				t.Fatal(err)
			}
			m2, _, _ := testRegistry(t)
			if opened, e := Open(LogOptions{Dir: d, Checkpoints: m2}); e == nil {
				opened.Close()
				t.Fatal("special tail was accepted")
			}
		})
	}
}

func mkfifo(path string) error { return syscall.Mkfifo(path, 0600) }

func TestCheckpointQuarantinesMasqueradingOrphansAndRetries(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, err := Open(LogOptions{Dir: d, Checkpoints: m})
	if err != nil {
		t.Fatal(err)
	}
	writeCheckpointEntry(t, l, "a", 1)
	if err = l.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(d, checkpointDirName)
	fake := "gen-00000000000000000002.tmp"
	if err = os.WriteFile(filepath.Join(root, fake), []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	m2, _, _ := testRegistry(t)
	l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if _, err = os.Lstat(filepath.Join(root, fake+".orphan")); err != nil {
		t.Fatalf("masquerader not quarantined: %v", err)
	}
	if err = l2.Checkpoint(); err != nil {
		t.Fatalf("retry did not progress: %v", err)
	}
}

func TestCheckpointDeterministicCutoverMatrix(t *testing.T) {
	stages := []string{"snapshot-fsync", "tail-fsync", "manifest-fsync", "generation-dir-fsync", "generation-rename", "current-temp-fsync", "current-rename", "current-parent-fsync", "writer-handoff", "old-writer-retirement"}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			d := t.TempDir()
			m, _, _ := testRegistry(t)
			l, err := Open(LogOptions{Dir: d, Checkpoints: m})
			if err != nil {
				t.Fatal(err)
			}
			committed := writeCheckpointEntry(t, l, "a", 1)
			old := checkpointFault
			checkpointFault = func(got string) error {
				if got == stage {
					return errors.New("injected " + stage)
				}
				return nil
			}
			err = l.Checkpoint()
			checkpointFault = old
			if err == nil {
				t.Fatal("fault did not stop checkpoint")
			}
			_ = l.Close()
			m2, a2, _ := testRegistry(t)
			l2, e := Open(LogOptions{Dir: d, Checkpoints: m2})
			if e != nil {
				t.Fatal(e)
			}
			defer l2.Close()
			if len(a2.seen) != 1 || a2.seen[0].CommitIndex != committed.CommitIndex {
				t.Fatalf("accepted history changed: %#v", a2.seen)
			}
			next := l2.w.NextIndex()
			fresh := writeCheckpointEntry(t, l2, "b", 2)
			if fresh.PrepareIndex != next || fresh.PrepareIndex <= committed.CommitIndex {
				t.Fatalf("index reuse/order: next=%d fresh=%d prior=%d", next, fresh.PrepareIndex, committed.CommitIndex)
			}
		})
	}
}

func TestCheckpointGlobalCrossKindOrder(t *testing.T) {
	d := t.TempDir()
	var liveOrder []uint64
	a := &checkpointTestParticipant{name: "alpha", kinds: []string{"a"}, applyOrder: &liveOrder}
	b := &checkpointTestParticipant{name: "beta", kinds: []string{"b"}, applyOrder: &liveOrder}
	m, err := NewMultiApplier(a, b)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open(LogOptions{Dir: d, Checkpoints: m})
	if err != nil {
		t.Fatal(err)
	}
	c1 := writeCheckpointEntry(t, l, "a", 1)
	c2 := writeCheckpointEntry(t, l, "b", 2)
	if err = l.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	c3 := writeCheckpointEntry(t, l, "a", 3)
	c4 := writeCheckpointEntry(t, l, "b", 4)
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	var replayOrder []uint64
	a2 := &checkpointTestParticipant{name: "alpha", kinds: []string{"a"}, applyOrder: &replayOrder}
	b2 := &checkpointTestParticipant{name: "beta", kinds: []string{"b"}, applyOrder: &replayOrder}
	m2, err := NewMultiApplier(a2, b2)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	wantReplay := []uint64{c3.CommitIndex, c4.CommitIndex}
	if fmt.Sprint(replayOrder) != fmt.Sprint(wantReplay) {
		t.Fatalf("tail replay global order got %v want %v", replayOrder, wantReplay)
	}
	wantLive := []uint64{c1.CommitIndex, c2.CommitIndex, c3.CommitIndex, c4.CommitIndex}
	if fmt.Sprint(liveOrder) != fmt.Sprint(wantLive) {
		t.Fatalf("live global order got %v want %v", liveOrder, wantLive)
	}
}

// TestCheckpointV5SecurityAndCrashAcceptance is the single registered proof.
// Its subtests deliberately invoke every v5 security, rollback, filesystem,
// crash, fallback/index and global-order regression so a partial implementation
// cannot satisfy the task by matching only one alternative test name.
func TestCheckpointV5SecurityAndCrashAcceptance(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*testing.T)
	}{
		{"tail-identity-both-directions", TestCheckpointForwardAndBackwardTailIdentity},
		{"pre-high-water-whole-generation", TestCheckpointRejectsTailSubstitutionAndPreHighWaterGeneration},
		{"stale-current-newest-first", TestCheckpointStaleCurrentCannotRollback},
		{"missing-current", TestCheckpointMissingCurrentUsesNewestAuthenticatedGeneration},
		{"pre-open-special-files", TestCheckpointTailPreOpenRegularValidation},
		{"orphan-quarantine-retry", TestCheckpointQuarantinesMasqueradingOrphansAndRetries},
		{"cutover-crash-matrix-index-nonreuse", TestCheckpointDeterministicCutoverMatrix},
		{"global-cross-kind-order", TestCheckpointGlobalCrossKindOrder},
		{"selected-tail-damage-fallback", TestCheckpointTailHeaderTamperIsRejected},
		{"legacy-migration", TestCheckpointMultiApplierSharedWAL},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.fn)
	}
}

// TestCheckpointV6CrashChild is entered only by the process-death harness.
// SIGKILL deliberately bypasses testing cleanup, defers, Log.Close, and the
// writer floor seal, leaving exactly the bytes a killed server left behind.
func TestCheckpointV6CrashChild(t *testing.T) {
	stage, dir := os.Getenv("WAL_CHECKPOINT_CRASH_STAGE"), os.Getenv("WAL_CHECKPOINT_CRASH_DIR")
	if stage == "" {
		t.Skip("subprocess helper")
	}
	m, _, _ := testRegistry(t)
	l, err := Open(LogOptions{Dir: dir, Checkpoints: m})
	if err != nil {
		t.Fatal(err)
	}
	writeCheckpointEntry(t, l, "b", 2) // acknowledged before the forced death
	checkpointFault = func(got string) error {
		if got == stage {
			if err := syscall.Kill(syscall.Getpid(), syscall.SIGKILL); err != nil {
				t.Fatalf("SIGKILL self: %v", err)
			}
			t.Fatal("SIGKILL returned without terminating child")
		}
		return nil
	}
	_ = l.Checkpoint()
	t.Fatalf("checkpoint did not reach crash stage %q", stage)
}

func TestCheckpointV6ChildProcessCutoverMatrix(t *testing.T) {
	stages := []string{
		"snapshot-write-before", "snapshot-fsync", "tail-write-before", "tail-fsync",
		"manifest-write-before", "manifest-fsync", "generation-dir-fsync-before", "generation-dir-fsync",
		"generation-rename-before", "generation-rename", "current-temp-write-before", "current-temp-fsync",
		"current-rename-before", "current-rename", "current-parent-fsync-before", "current-parent-fsync",
		"writer-handoff-before", "writer-handoff", "old-writer-retirement-before", "old-writer-retirement",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			d := t.TempDir()
			m, _, _ := testRegistry(t)
			l, err := Open(LogOptions{Dir: d, Checkpoints: m})
			if err != nil {
				t.Fatal(err)
			}
			first := writeCheckpointEntry(t, l, "a", 1)
			if err = l.Close(); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointV6CrashChild$")
			cmd.Env = append(os.Environ(), "WAL_CHECKPOINT_CRASH_STAGE="+stage, "WAL_CHECKPOINT_CRASH_DIR="+d)
			out, runErr := cmd.CombinedOutput()
			exit, ok := runErr.(*exec.ExitError)
			status, statusOK := syscall.WaitStatus(0), false
			if ok {
				status, statusOK = exit.Sys().(syscall.WaitStatus)
			}
			if !ok || !statusOK || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child was not killed at %s: err=%v output=%s", stage, runErr, out)
			}
			m2, a2, b2 := testRegistry(t)
			l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
			if err != nil {
				t.Fatal(err)
			}
			defer l2.Close()
			if len(a2.seen) != 1 || len(b2.seen) != 1 || a2.seen[0].CommitIndex != first.CommitIndex {
				t.Fatalf("mixed/lost accepted history after %s: a=%#v b=%#v", stage, a2.seen, b2.seen)
			}
			priorMax := b2.seen[0].CommitIndex
			next := l2.w.NextIndex()
			fresh := writeCheckpointEntry(t, l2, "a", 3)
			if fresh.PrepareIndex != next || fresh.PrepareIndex <= priorMax {
				t.Fatalf("index reuse after process death at %s: next=%d fresh=%d prior=%d", stage, next, fresh.PrepareIndex, priorMax)
			}
		})
	}
}

func TestCheckpointV6MalformedMaterialIsQuarantined(t *testing.T) {
	for _, kind := range []string{"manifest", "snapshot", "path", "length", "version"} {
		t.Run(kind, func(t *testing.T) {
			d := t.TempDir()
			m, _, _ := testRegistry(t)
			l, err := Open(LogOptions{Dir: d, Checkpoints: m})
			if err != nil {
				t.Fatal(err)
			}
			writeCheckpointEntry(t, l, "a", 1)
			if err = l.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			writeCheckpointEntry(t, l, "b", 2)
			if err = l.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			if err = l.Close(); err != nil {
				t.Fatal(err)
			}
			gen := filepath.Join(d, checkpointDirName, "gen-00000000000000000002")
			manifestPath := filepath.Join(gen, checkpointManifestFile)
			key, err := loadMACKey(macKeyPath(d))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var manifest checkpointManifest
			if err = verifyAuthenticatedJSON(raw, &manifest, key); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "manifest":
				raw[0] ^= 1
			case "snapshot":
				p := filepath.Join(gen, manifest.Participants[0].File)
				b, e := os.ReadFile(p)
				if e != nil {
					t.Fatal(e)
				}
				b[len(b)/2] ^= 1
				if e = os.WriteFile(p, b, 0600); e != nil {
					t.Fatal(e)
				}
			case "path":
				manifest.Participants[0].File = "../escape.snapshot"
				raw, err = authenticatedJSON(&manifest, key)
			case "length":
				manifest.Participants[0].Length++
				raw, err = authenticatedJSON(&manifest, key)
			case "version":
				manifest.Version++
				raw, err = authenticatedJSON(&manifest, key)
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind != "snapshot" {
				if err = os.WriteFile(manifestPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			m2, _, _ := testRegistry(t)
			l2, err := Open(LogOptions{Dir: d, Checkpoints: m2, Logger: logging.New(&logs, logging.LevelInfo)})
			if err != nil {
				t.Fatal(err)
			}
			l2.Close()
			if _, err = os.Lstat(gen + ".orphan"); err != nil {
				t.Fatalf("%s candidate not quarantined: %v", kind, err)
			}
			if !bytes.Contains(logs.Bytes(), []byte("wal quarantined orphan checkpoint generation")) ||
				!bytes.Contains(logs.Bytes(), []byte("why=")) {
				t.Fatalf("%s quarantine was not logged loudly with a reason: %s", kind, logs.String())
			}
		})
	}
}

func TestCheckpointV6SelectedTailRepairAndLegacyV1(t *testing.T) {
	t.Run("selected-tail-repair", func(t *testing.T) {
		d := t.TempDir()
		m, _, _ := testRegistry(t)
		l, err := Open(LogOptions{Dir: d, Checkpoints: m})
		if err != nil {
			t.Fatal(err)
		}
		writeCheckpointEntry(t, l, "a", 1)
		if err = l.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		writeCheckpointEntry(t, l, "b", 2)
		if err = l.Close(); err != nil {
			t.Fatal(err)
		}
		tail := filepath.Join(d, checkpointDirName, "gen-00000000000000000001", checkpointTailFile)
		f, err := os.OpenFile(tail, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte("torn-frame")); err != nil {
			t.Fatal(err)
		}
		f.Close()
		var logs bytes.Buffer
		m2, a, b := testRegistry(t)
		l2, err := Open(LogOptions{Dir: d, Checkpoints: m2, Logger: logging.New(&logs, logging.LevelInfo)})
		if err != nil {
			t.Fatal(err)
		}
		defer l2.Close()
		if len(a.seen) != 1 || len(b.seen) != 1 {
			t.Fatalf("repair lost state: %d/%d", len(a.seen), len(b.seen))
		}
		repair := l2.Recovered().Repaired
		if !repair.Truncated || repair.DiscardCount == 0 || repair.DiscardedBytes == 0 {
			t.Fatalf("selected tail repair was not observable in Recovered: %+v", repair)
		}
		if !bytes.Contains(logs.Bytes(), []byte("discarded")) || !bytes.Contains(logs.Bytes(), []byte("torn tail")) {
			t.Fatalf("selected tail repair was not logged loudly and specifically: %s", logs.String())
		}
	})
	t.Run("real-v1-migration", func(t *testing.T) {
		d := t.TempDir()
		path, _, _ := v1TxnLog(t, d)
		legacy := &checkpointTestParticipant{name: "legacy", kinds: []string{"message", "agent"}}
		m, err := NewMultiApplier(legacy)
		if err != nil {
			t.Fatal(err)
		}
		l, err := Open(LogOptions{Dir: d, Checkpoints: m})
		if err != nil {
			t.Fatal(err)
		}
		if v, e := detectFormat(path, KindWAL); e != nil || v != FormatVersion {
			t.Fatalf("v1 not migrated: version=%d err=%v", v, e)
		}
		if err = l.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if len(legacy.seen) != 2 {
			t.Fatal("v1 migration did not restore participant state")
		}
		if err = l.Close(); err != nil {
			t.Fatal(err)
		}
		legacy2 := &checkpointTestParticipant{name: "legacy", kinds: []string{"message", "agent"}}
		m2, err := NewMultiApplier(legacy2)
		if err != nil {
			t.Fatal(err)
		}
		l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
		if err != nil {
			t.Fatalf("post-checkpoint v1 reopen: %v", err)
		}
		if l2.generation != 1 {
			t.Fatalf("post-checkpoint v1 reopen generation=%d want 1", l2.generation)
		}
		l2.Close()
	})
}

func TestCheckpointV7DeterministicTornAndDirtyPublicationStates(t *testing.T) {
	for _, state := range []string{"partial-snapshot", "partial-manifest", "torn-current", "dirty-current-temp"} {
		t.Run(state, func(t *testing.T) {
			d := t.TempDir()
			m, _, _ := testRegistry(t)
			l, err := Open(LogOptions{Dir: d, Checkpoints: m})
			if err != nil {
				t.Fatal(err)
			}
			first := writeCheckpointEntry(t, l, "a", 1)
			if err = l.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			second := writeCheckpointEntry(t, l, "b", 2)
			if err = l.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			if err = l.Close(); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(d, checkpointDirName)
			gen2 := filepath.Join(root, "gen-00000000000000000002")
			switch state {
			case "partial-snapshot":
				if err = os.WriteFile(filepath.Join(gen2, "alpha.snapshot"), []byte(`{"domain":`), 0600); err != nil {
					t.Fatal(err)
				}
			case "partial-manifest":
				if err = os.WriteFile(filepath.Join(gen2, checkpointManifestFile), []byte(`{"domain":`), 0600); err != nil {
					t.Fatal(err)
				}
			case "torn-current":
				if err = os.WriteFile(filepath.Join(root, checkpointCurrent), []byte("gen-000000"), 0600); err != nil {
					t.Fatal(err)
				}
			case "dirty-current-temp":
				if err = os.WriteFile(filepath.Join(root, checkpointCurrent+".tmp"), []byte("gen-000000"), 0600); err != nil {
					t.Fatal(err)
				}
			}

			var logs bytes.Buffer
			m2, a2, b2 := testRegistry(t)
			l2, err := Open(LogOptions{Dir: d, Checkpoints: m2, Logger: logging.New(&logs, logging.LevelInfo)})
			if err != nil {
				t.Fatal(err)
			}
			defer l2.Close()
			wantGen, wantHigh := uint64(2), second.CommitIndex
			if state == "partial-snapshot" || state == "partial-manifest" {
				wantGen, wantHigh = 1, first.CommitIndex
			}
			if l2.generation != wantGen || a2.restoredHigh != wantHigh || b2.restoredHigh != wantHigh {
				t.Fatalf("%s mixed generation: selected=%d alpha-H=%d beta-H=%d want generation/H=%d/%d",
					state, l2.generation, a2.restoredHigh, b2.restoredHigh, wantGen, wantHigh)
			}
			if state == "partial-snapshot" || state == "partial-manifest" {
				if _, err = os.Lstat(gen2 + ".orphan"); err != nil || !bytes.Contains(logs.Bytes(), []byte("why=")) {
					t.Fatalf("%s fallback lacked quarantine diagnostics: err=%v logs=%s", state, err, logs.String())
				}
			} else if state == "torn-current" {
				if !bytes.Contains(logs.Bytes(), []byte("inconsistent CURRENT pointer")) {
					t.Fatalf("torn CURRENT recovery was silent: %s", logs.String())
				}
			} else if _, err = os.Lstat(filepath.Join(root, checkpointCurrent+".tmp.orphan")); err != nil {
				t.Fatalf("dirty CURRENT temp was not quarantined: %v", err)
			}
			fresh := writeCheckpointEntry(t, l2, "a", 3)
			if fresh.PrepareIndex <= second.CommitIndex {
				t.Fatalf("%s reused an index: fresh=%d prior=%d", state, fresh.PrepareIndex, second.CommitIndex)
			}
		})
	}
}

// TestCheckpointV7SignalCrashAndRecoveryObservability is the sole v7 proof.
// It retains the complete v5 security suite and adds genuine signal-death and
// invariant-6 recovery evidence, so a green aggregate cannot omit an older
// load-bearing case.
func TestCheckpointV7SignalCrashAndRecoveryObservability(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*testing.T)
	}{
		{"v5-security", TestCheckpointV5SecurityAndCrashAcceptance},
		{"signal-cutover", TestCheckpointV6ChildProcessCutoverMatrix},
		{"malformed-loud-quarantine", TestCheckpointV6MalformedMaterialIsQuarantined},
		{"selected-repair-and-v1-reopen", TestCheckpointV6SelectedTailRepairAndLegacyV1},
		{"torn-and-dirty-publication", TestCheckpointV7DeterministicTornAndDirtyPublicationStates},
		{"bounded-tail", TestCheckpointBoundsTailReplay},
		{"global-order", TestCheckpointGlobalCrossKindOrder},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.fn)
	}
}

// TestCheckpointSupportedReportsTheOPENNotTheTYPE pins the distinction that
// wedged cross-bus egress (RELAY-24-BLOCKER-EGRESS), and it is a distinction no
// type assertion can make.
//
// *Log ALWAYS has a Checkpoint method, so `x.(interface{ Checkpoint() error })`
// succeeds on EVERY log — including one opened with no MultiApplier, where
// Checkpoint returns "checkpoint requires a MultiApplier" unconditionally and can
// never succeed. relay.Outbox read that successful assertion as "this log can
// checkpoint", deferred reclaiming swept records until a publication that could
// never happen, and therefore stopped accepting work for a peer for the life of
// the process.
//
// The FIRST row is the composition root's own wiring: cmd/agent-bus opens the
// log with an Applier and NO Checkpoints. It is the row the defect lived in, and
// it is why "false" here is the production answer rather than a corner case.
//
// Each row asserts three things together, because only the three together
// distinguish the property from the type:
//
//	the type assertion succeeds either way   (the trap)
//	CheckpointSupported() reports the OPEN   (the fix)
//	Checkpoint() itself agrees with it       (the fix means what it says)
func TestCheckpointSupportedReportsTheOPENNotTheTYPE(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(t *testing.T, dir string) *Log
		want bool
		why  string
	}{
		{
			name: "opened the way cmd/agent-bus opens it: an Applier and no Checkpoints",
			open: func(t *testing.T, dir string) *Log {
				t.Helper()
				l, err := Open(LogOptions{Dir: dir, Applier: &testApplier{}})
				if err != nil {
					t.Fatalf("Open(Applier, no Checkpoints): %v", err)
				}
				return l
			},
			want: false,
			why:  "this is the composition root's literal call. A participant that treats this log as checkpointable defers its reclaim forever",
		},
		{
			name: "opened with no applier at all",
			open: func(t *testing.T, dir string) *Log {
				t.Helper()
				l, err := Open(LogOptions{Dir: dir})
				if err != nil {
					t.Fatalf("Open(bare): %v", err)
				}
				return l
			},
			want: false,
			why:  "a bare log is no more checkpointable than an appliered one; the capability comes from Checkpoints and from nowhere else",
		},
		{
			name: "opened WITH LogOptions.Checkpoints",
			open: func(t *testing.T, dir string) *Log {
				t.Helper()
				m, _, _ := testRegistry(t)
				l, err := Open(LogOptions{Dir: dir, Checkpoints: m})
				if err != nil {
					t.Fatalf("Open(Checkpoints): %v", err)
				}
				return l
			},
			want: true,
			why:  "there is a dispatcher to publish through, so a participant may legitimately defer to the next checkpoint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := tc.open(t, t.TempDir())
			t.Cleanup(func() { _ = l.Close() })

			// THE TRAP, asserted so it cannot quietly stop being one. If this ever
			// fails, the type assertion has become discriminating and the comment
			// on CheckpointSupported needs rewriting rather than deleting.
			if _, ok := interface{}(l).(interface{ Checkpoint() error }); !ok {
				t.Fatalf("*Log no longer satisfies interface{ Checkpoint() error }; the whole reason CheckpointSupported exists is that this assertion succeeds on every log")
			}

			if got := l.CheckpointSupported(); got != tc.want {
				t.Fatalf("CheckpointSupported() = %v, want %v.\n  why it matters: %s", got, tc.want, tc.why)
			}

			// AND THE METHOD AGREES. A CheckpointSupported that answered
			// independently of what Checkpoint actually does would be a second
			// answer that can drift from the first — the exact shape of the bug.
			err := l.Checkpoint()
			switch {
			case tc.want && err != nil:
				t.Fatalf("CheckpointSupported() reported true but Checkpoint() = %v, want a published generation", err)
			case !tc.want && err == nil:
				t.Fatalf("CheckpointSupported() reported false but Checkpoint() succeeded; the property and the method disagree")
			case !tc.want && !strings.Contains(err.Error(), "requires a MultiApplier"):
				t.Fatalf("Checkpoint() on an unsupported log = %v, want the \"requires a MultiApplier\" refusal that makes the deferral permanent", err)
			}
		})
	}
}

// TestCheckpointV6ProcessCrashRecoveryAcceptance is the sole v6 completion
// proof and invokes every required class directly.
func TestCheckpointV6ProcessCrashRecoveryAcceptance(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*testing.T)
	}{
		{"child-process-cutover", TestCheckpointV6ChildProcessCutoverMatrix},
		{"malformed-and-quarantine", TestCheckpointV6MalformedMaterialIsQuarantined},
		{"repair-and-v1", TestCheckpointV6SelectedTailRepairAndLegacyV1},
		{"bounded-tail", TestCheckpointBoundsTailReplay},
		{"global-order", TestCheckpointGlobalCrossKindOrder},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.fn)
	}
}
