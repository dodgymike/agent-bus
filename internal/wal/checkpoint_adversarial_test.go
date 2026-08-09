package wal

// Adversarial / black-box regression coverage for the checkpoint generation
// mechanism (checkpoint.go). These tests treat checkpoint.go and log.go as
// fixed and probe only OBSERVABLE behaviour reachable through Log.Open,
// Log.Checkpoint and the CheckpointParticipant contract, using the same
// in-package seams the existing checkpoint_test.go already relies on
// (checkpointManifest, authenticatedJSON, checkpointTailHeaderMAC,
// loadMACKey/macKeyPath, and the Log struct's own fields). No assertions here
// pin an implementation detail that is not already part of that public
// surface: every check is "Open/Checkpoint behaved this way", never "this
// exact internal code path fired".

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// Tail substitution / generation binding
// ---------------------------------------------------------------------------

// TestCheckpointTailHeaderTamperIsRejected corrupts a single byte inside the
// checkpointed generation's tail.wal file HEADER (not its body) after a valid
// checkpoint has been published. The manifest's authenticated
// TailHeaderMAC binds the manifest's declared (generation, high-water) to
// that header, so this must be detected: Open must not silently accept the
// tampered generation. Because an earlier, still-valid generation exists,
// Open must fall back to it rather than to the pre-checkpoint legacy log.
func TestCheckpointTailHeaderTamperIsRejected(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	first := writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil { // generation 1
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 2)
	if e = l.Checkpoint(); e != nil { // generation 2
		t.Fatal(e)
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	root := filepath.Join(d, checkpointDirName)
	gen2Tail := filepath.Join(root, fmt.Sprintf("gen-%020d", 2), checkpointTailFile)
	corruptByteAt(t, gen2Tail, 0) // inside the 48-byte format-2 header

	m2, a2, _ := testRegistry(t)
	l2, e := Open(LogOptions{Dir: d, Checkpoints: m2})
	if e != nil {
		t.Fatal(e)
	}
	defer l2.Close()
	if l2.generation != 1 {
		t.Fatalf("generation = %d, want authenticated fallback to generation 1 after the newest generation's tail header was tampered with", l2.generation)
	}
	if a2.restoredHigh != first.CommitIndex {
		t.Fatalf("restored high-water = %d, want the fallback generation's own high-water %d", a2.restoredHigh, first.CommitIndex)
	}
}

// TestCheckpointRejectsTailSubstitutionAndPreHighWaterGeneration asserts the
// authoritative Spec Server contract (project agent-bus, task
// a1cbef29-400a-4a1e-9638-cc14d38a7ebf, decision B): a generation whose tail
// does not match what its own manifest authenticates -- whether because the
// tail body was substituted from a different generation, or because a
// forged-but-validly-signed manifest raises HighWater to or above a commit
// genuinely present in that generation's own tail -- must be REJECTED AS A
// WHOLE. Neither Restore nor Apply may run for ANY part of a rejected
// generation (not even its otherwise-legitimate participant snapshot), and
// the only acceptable substitute is falling back to an older generation that
// verifies completely on its own; where no such generation exists, Open must
// refuse outright rather than serve a partial or discarded-record state.
//
// NOTE ON HISTORY: earlier revisions of this file asserted the opposite --
// that generation selection never replays a candidate generation's tail
// against its own declared high-water, and that log.go's replay-time guard
// only DISCARDS the offending commit rather than rejecting the generation
// wholesale (replay.go's documented "discard-not-refuse" policy, "Refusing
// the start here was the old policy"). At that time this test was expected
// to FAIL (red) as a pin for the fix Decision B required. verifyGeneration
// has since been changed (concurrently, by the production owner of
// checkpoint.go) to replay each candidate generation's full tail against its
// own authenticated high-water before any Restore/Apply is allowed, and to
// reject the whole generation -- selecting an older, wholly authenticated
// generation instead, or refusing Open outright if none exists -- when that
// check fails. This test now asserts that behaviour and PASSES (green); it
// remains a regression pin, not merely a target. It must not be relaxed back
// toward the superseded discard-only behaviour -- see the two prior
// revisions of this test (TestCheckpointTailSubstitutionCommitsAtOrBelowHighWaterAreDiscarded
// and TestCheckpointReplayDiscardsCommitAtHighWater), which asserted exactly
// that discard-only behaviour and were superseded by this one under Decision
// B.
// checkpointRestoreSpyCall records one CheckpointParticipant.Restore
// invocation: the high-water it was called with, and how many committed
// entries were encoded in the snapshot bytes handed to it. Recording both
// lets a test distinguish, independent of the participant's FINAL state,
// which generation's snapshot a given Restore call actually carried --
// generation 1's snapshot in these tests always carries exactly 1 entry
// (taken right after the first commit) and is always called with
// highWater == that commit's index, while generation 2's snapshot always
// carries 3 entries and highWater == the third commit's index. Final state
// alone cannot tell "generation 2 was never restored" apart from
// "generation 2 was restored first, then correctly overwritten by a second,
// legitimate Restore call for generation 1" -- both would leave the
// participant holding generation 1's 1-entry state (Restore overwrites
// rather than merges). Recording every call closes that gap.
type checkpointRestoreSpyCall struct {
	highWater uint64
	entries   int
}

// checkpointRestoreSpyParticipant behaves exactly like
// checkpointTestParticipant (Name/Kinds/Apply/Snapshot are inherited
// unchanged via embedding) but additionally appends a
// checkpointRestoreSpyCall for every Restore invocation, so a test can
// assert the total NUMBER of Restore calls and the identity of each one --
// not merely the state left behind after all of them.
type checkpointRestoreSpyParticipant struct {
	checkpointTestParticipant
	restores []checkpointRestoreSpyCall
	// applies records the CommitIndex of every Apply call this participant
	// ever receives, in call order, independent of the (mutable, overwritten
	// by Restore) final `seen` state. A rejected generation's own commit
	// indices must NEVER appear here -- not before the eventual Restore call
	// that selects an older generation instead (which would mean the
	// rejected generation's content reached participant state transiently,
	// ahead of and hidden by a later, correct Restore) and not after it
	// either (which would mean the rejected generation's tail was forward-
	// replayed post-selection). Final `seen`/`applyOrder` state alone cannot
	// distinguish "never applied" from "applied then overwritten by
	// Restore", because Restore replaces state wholesale rather than
	// merging; recording every Apply call closes that gap the same way
	// checkpointRestoreSpyCall closes it for Restore.
	applies []uint64
}

func (p *checkpointRestoreSpyParticipant) Restore(b []byte, h uint64) error {
	var entries []Committed
	if err := json.Unmarshal(b, &entries); err != nil {
		return err
	}
	p.restores = append(p.restores, checkpointRestoreSpyCall{highWater: h, entries: len(entries)})
	return p.checkpointTestParticipant.Restore(b, h)
}

func (p *checkpointRestoreSpyParticipant) Apply(c Committed) error {
	p.applies = append(p.applies, c.CommitIndex)
	return p.checkpointTestParticipant.Apply(c)
}

// spyRegistry mirrors checkpoint_test.go's testRegistry exactly (same
// participant names/kinds, so manifests written against one Open with this
// registry, or with testRegistry, remain structurally interchangeable) but
// returns alpha as a *checkpointRestoreSpyParticipant so Restore calls
// against it can be inspected directly.
func spyRegistry(t *testing.T) (*MultiApplier, *checkpointRestoreSpyParticipant, *checkpointTestParticipant) {
	t.Helper()
	a := &checkpointRestoreSpyParticipant{checkpointTestParticipant: checkpointTestParticipant{name: "alpha", kinds: []string{"a"}}}
	b := &checkpointTestParticipant{name: "beta", kinds: []string{"b"}}
	m, e := NewMultiApplier(b, a)
	if e != nil {
		t.Fatal(e)
	}
	return m, a, b
}

func TestCheckpointRejectsTailSubstitutionAndPreHighWaterGeneration(t *testing.T) {
	t.Run("SubstitutedTailFallsBackToOlderGeneration", func(t *testing.T) {
		d := t.TempDir()
		m, _, _ := testRegistry(t)
		l, e := Open(LogOptions{Dir: d, Checkpoints: m})
		if e != nil {
			t.Fatal(e)
		}
		first := writeCheckpointEntry(t, l, "a", 1)
		if e = l.Checkpoint(); e != nil { // generation 1: tail will carry entries 2 and 3
			t.Fatal(e)
		}
		writeCheckpointEntry(t, l, "a", 2)
		last := writeCheckpointEntry(t, l, "a", 3)
		if e = l.Checkpoint(); e != nil { // generation 2: fresh, empty tail; high-water == last.CommitIndex
			t.Fatal(e)
		}
		if e = l.Close(); e != nil {
			t.Fatal(e)
		}

		root := filepath.Join(d, checkpointDirName)
		gen1Tail := filepath.Join(root, fmt.Sprintf("gen-%020d", 1), checkpointTailFile)
		gen2Tail := filepath.Join(root, fmt.Sprintf("gen-%020d", 2), checkpointTailFile)
		gen1Bytes, e := os.ReadFile(gen1Tail)
		if e != nil {
			t.Fatal(e)
		}
		gen2Bytes, e := os.ReadFile(gen2Tail)
		if e != nil {
			t.Fatal(e)
		}
		if len(gen1Bytes) == len(gen2Bytes) {
			t.Fatalf("test setup: expected the two tails to differ in size (one carries committed entries, the other is fresh), both are %d bytes", len(gen1Bytes))
		}
		// Substitute generation 1's tail (which carries two committed entries,
		// up to and including one at generation 2's own declared high-water)
		// into generation 2's slot, leaving generation 1 itself untouched and
		// wholly valid on its own.
		if e = os.WriteFile(gen2Tail, gen1Bytes, 0600); e != nil {
			t.Fatal(e)
		}

		m2, a2, _ := spyRegistry(t)
		l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
		if err != nil {
			t.Fatalf("Open failed outright; want a fallback to the wholly authenticated generation 1 instead: %v", err)
		}
		defer l2.Close()
		if l2.generation != 1 {
			t.Fatalf("generation = %d, want 1: generation 2's tail does not match what its manifest authenticates, so generation 2 must be rejected as a whole and Open must fall back to the untouched, wholly verifiable generation 1", l2.generation)
		}
		if a2.restoredHigh != first.CommitIndex {
			t.Fatalf("restoredHigh = %d, want %d (generation 1's own high-water) -- Restore must never have run against rejected generation 2's snapshot at all", a2.restoredHigh, first.CommitIndex)
		}
		// Final state alone (checked below) cannot distinguish "generation 2
		// was never restored from" from "generation 2 WAS restored from
		// first, then correctly overwritten by a second Restore call for the
		// fallback generation 1" -- Restore replaces p.seen wholesale, so
		// both sequences leave identical final state. Assert on the actual
		// call log instead: exactly one Restore call must have happened in
		// total, and it must carry generation 1's own snapshot identity
		// (high-water == first.CommitIndex, exactly 1 entry encoded --
		// generation 2's snapshot would instead carry high-water ==
		// last.CommitIndex and 3 entries).
		if got := len(a2.restores); got != 1 {
			t.Fatalf("Restore was called %d times, want exactly 1: generation 2 being rejected as a whole means it must never be restored from at all, not even before falling back to generation 1 -- %#v", got, a2.restores)
		}
		if a2.restores[0].highWater != first.CommitIndex || a2.restores[0].entries != 1 {
			t.Fatalf("the sole Restore call was %+v, want {highWater:%d entries:1} (generation 1's own snapshot identity) -- {highWater:%d entries:3} would be generation 2's rejected snapshot instead", a2.restores[0], first.CommitIndex, last.CommitIndex)
		}
		// Falling back to generation 1's snapshot (state as of commit 2) must
		// still be followed by ordinary forward replay of the genuine,
		// untampered WAL that continues past that snapshot -- that is how
		// full recovery legitimately reaches the current commit (3 entries:
		// commits 2, 4, 6), and it must succeed cleanly with zero discards.
		// What Decision B forbids is any of that state coming FROM rejected
		// generation 2's substituted checkpoint tail -- it does not forbid
		// the independent, authenticated live WAL from being replayed
		// forward as normal.
		if got := len(a2.seen); got != 3 {
			t.Fatalf("participant saw %d entries, want all 3 real commits recovered via generation 1's snapshot plus ordinary forward replay of the genuine WAL (not generation 2's rejected/substituted tail)", got)
		}
		wantIdx := []uint64{first.CommitIndex, first.CommitIndex + 2, last.CommitIndex}
		for i, c := range a2.seen {
			if c.CommitIndex != wantIdx[i] {
				t.Fatalf("participant state = %#v, want commit indices %v in order", a2.seen, wantIdx)
			}
		}
		rec := l2.Recovered()
		if rec.DiscardCount != 0 {
			t.Fatalf("Recovered().DiscardCount = %d, want 0: with generation 2 rejected wholesale and generation 1 wholly authenticated, recovery must be completely clean -- no record should ever need to be discarded", rec.DiscardCount)
		}
		for _, dd := range rec.Discarded {
			if dd.Index == last.CommitIndex {
				t.Fatalf("Recovered().Discarded contains commit %d via a per-record discard; want generation 2 rejected wholesale before replay ever reaches it, so this record must never even be considered for discard under generation 2 -- only generation 1's own (clean) tail should have been replayed", last.CommitIndex)
			}
		}
	})

	t.Run("SoleGenerationWithForgedHighWaterRefusesOutright", func(t *testing.T) {
		d := t.TempDir()
		m, _, _ := testRegistry(t)
		l, e := Open(LogOptions{Dir: d, Checkpoints: m})
		if e != nil {
			t.Fatal(e)
		}
		writeCheckpointEntry(t, l, "a", 1)
		if e = l.Checkpoint(); e != nil { // generation 1, high-water = commit index of entry 1
			t.Fatal(e)
		}
		second := writeCheckpointEntry(t, l, "a", 2) // lands in generation 1's tail
		if e = l.Close(); e != nil {
			t.Fatal(e)
		}

		root := filepath.Join(d, checkpointDirName)
		genDir := filepath.Join(root, fmt.Sprintf("gen-%020d", 1))
		manifestPath := filepath.Join(genDir, checkpointManifestFile)
		tailPath := filepath.Join(genDir, checkpointTailFile)

		raw, e := os.ReadFile(manifestPath)
		if e != nil {
			t.Fatal(e)
		}
		var manifest checkpointManifest
		if e = json.Unmarshal(raw, &manifest); e != nil {
			t.Fatal(e)
		}
		key, e := loadMACKey(macKeyPath(d))
		if e != nil {
			t.Fatal(e)
		}
		// The tail file physically holds one committed entry (second, at
		// CommitIndex). A fresh scan of it therefore reports NextIndex ==
		// second.CommitIndex+1 regardless of what the manifest claims; forge
		// the manifest's NextIndex to that same real value so raising
		// HighWater to second.CommitIndex stays both structurally valid
		// (NextIndex>HighWater) and consistent with the writer's own
		// independent scan -- isolating this test to the high-water forgery
		// alone.
		manifest.NextIndex = second.CommitIndex + 1
		manifest.HighWater = second.CommitIndex // forge: equal, not below -- exercises the "<=" boundary exactly
		// Every participant's own snapshot envelope carries its own copy of
		// HighWater (envelope.HighWater != manifest.HighWater is itself a
		// structural check), so a consistent forgery has to re-sign those too.
		for i, part := range manifest.Participants {
			partPath := filepath.Join(genDir, part.File)
			praw, e := os.ReadFile(partPath)
			if e != nil {
				t.Fatal(e)
			}
			var env snapshotEnvelope
			if e = json.Unmarshal(praw, &env); e != nil {
				t.Fatal(e)
			}
			env.HighWater = manifest.HighWater
			presigned, e := authenticatedJSON(&env, key)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(partPath, presigned, 0600); e != nil {
				t.Fatal(e)
			}
			sum := sha256.Sum256(presigned)
			manifest.Participants[i].SHA256 = hex.EncodeToString(sum[:])
			manifest.Participants[i].Length = int64(len(presigned))
		}
		tf, e := os.Open(tailPath)
		if e != nil {
			t.Fatal(e)
		}
		mac, e := checkpointTailHeaderMAC(tf, manifest.Generation, manifest.HighWater, key)
		if cerr := tf.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		if e != nil {
			t.Fatal(e)
		}
		manifest.TailHeaderMAC = mac
		resigned, e := authenticatedJSON(&manifest, key)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(manifestPath, resigned, 0600); e != nil {
			t.Fatal(e)
		}

		// Generation 1 is the ONLY generation. Per decision B it must be
		// rejected as a whole (its tail contains a commit at its own declared
		// high-water) and there is no older, wholly authenticated generation
		// to fall back to, so Open must refuse outright -- never silently
		// serve generation 1 with the offending commit merely discarded, and
		// never Restore from it at all first (a Restore that is later
		// unwound by the failure would still be an observable side effect
		// this contract forbids, even though nothing from it could
		// ultimately be served).
		m2, a2, _ := spyRegistry(t)
		l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
		if err == nil {
			l2.Close()
			t.Fatal("Open succeeded by serving the sole generation with the offending commit merely discarded; want Open to refuse outright, since that generation must be rejected as a whole and no older authenticated generation exists to fall back to")
		}
		if got := len(a2.restores); got != 0 {
			t.Fatalf("Restore was called %d times (%#v), want 0: the sole generation is rejected as a whole, so it must never be restored from even transiently before Open ultimately refuses", got, a2.restores)
		}
		if !strings.Contains(err.Error(), "wal:") {
			t.Fatalf("Open error does not look like a wal-namespaced refusal: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Missing/corrupt CURRENT must recover the newest authenticated generation
// and must never fall back to the stale pre-checkpoint legacy log.
//
// (A missing-CURRENT variant of this is already covered by
// TestCheckpointMissingCurrentUsesNewestAuthenticatedGeneration in
// checkpoint_test.go; this file adds the CORRUPT -- as opposed to absent --
// CURRENT case, and the case where every generation is unverifiable.)
// ---------------------------------------------------------------------------

func TestCheckpointCorruptCURRENTRecoversNewestGeneration(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil { // generation 1
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 2)
	last := writeCheckpointEntry(t, l, "a", 3)
	if e = l.Checkpoint(); e != nil { // generation 2, the newest
		t.Fatal(e)
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	current := filepath.Join(d, checkpointDirName, checkpointCurrent)
	if e = os.WriteFile(current, []byte("not-a-generation-name\n"), 0600); e != nil {
		t.Fatal(e)
	}

	m2, a2, _ := testRegistry(t)
	l2, e := Open(LogOptions{Dir: d, Checkpoints: m2})
	if e != nil {
		t.Fatal(e)
	}
	defer l2.Close()
	if l2.generation != 2 {
		t.Fatalf("generation = %d, want the newest authenticated generation (2) even with a corrupt CURRENT", l2.generation)
	}
	if a2.restoredHigh != last.CommitIndex {
		t.Fatalf("restoredHigh = %d, want %d, not a stale legacy fallback", a2.restoredHigh, last.CommitIndex)
	}
}

// TestCheckpointAllGenerationsUnverifiableNeverFallsBackToStaleLegacy pairs a
// corrupt CURRENT with a generation whose own manifest cannot authenticate
// either. Even though this leaves NO recoverable checkpoint generation, Open
// must not silently resurrect the pre-checkpoint legacy bus.wal (which the
// data directory still physically contains, unwritten to, from before the
// first Checkpoint) as if it were current: that file is stale the moment a
// checkpoint has ever been published, and serving it would silently discard
// every committed entry the checkpoints recorded.
func TestCheckpointAllGenerationsUnverifiableNeverFallsBackToStaleLegacy(t *testing.T) {
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
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	legacyPath := filepath.Join(d, WALFileName)
	legacyBefore, e := os.ReadFile(legacyPath)
	if e != nil {
		t.Fatal(e)
	}

	current := filepath.Join(d, checkpointDirName, checkpointCurrent)
	if e = os.WriteFile(current, []byte("garbage\n"), 0600); e != nil {
		t.Fatal(e)
	}
	manifestPath := filepath.Join(d, checkpointDirName, "gen-00000000000000000001", checkpointManifestFile)
	corruptByteAt(t, manifestPath, 5)

	m2, _, _ := testRegistry(t)
	_, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err == nil {
		t.Fatal("Open succeeded with every checkpoint generation unverifiable; it must refuse rather than silently resurrect the stale pre-checkpoint legacy log")
	}
	if !strings.Contains(err.Error(), "wal:") {
		t.Fatalf("Open error does not look like a wal-namespaced refusal: %v", err)
	}
	legacyAfter, e := os.ReadFile(legacyPath)
	if e != nil {
		t.Fatal(e)
	}
	if string(legacyBefore) != string(legacyAfter) {
		t.Fatal("the stale legacy log was modified by a failed Open; it must be left untouched, not adopted")
	}
}

// ---------------------------------------------------------------------------
// Orphan / symlink / non-regular generation entries: refusal or quarantine,
// observed only through Open's return value and the recovered state -- never
// through which internal branch produced it.
// ---------------------------------------------------------------------------

// TestCheckpointSymlinkGenerationDirectoryIsRefused places a symlink inside
// wal-generations whose name looks like a newer, valid generation directory.
// It must never be treated as a candidate generation: accepting it would let
// anything with write access to the data directory redirect recovery
// anywhere on the filesystem the process can read.
func TestCheckpointSymlinkGenerationDirectoryIsRefused(t *testing.T) {
	if _, err := os.Readlink("/proc/self"); err != nil {
		t.Skip("symlinks unsupported in this environment")
	}
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	first := writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil { // generation 1
		t.Fatal(e)
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	root := filepath.Join(d, checkpointDirName)
	realGen1 := filepath.Join(root, "gen-00000000000000000001")
	fakeGen2 := filepath.Join(root, "gen-00000000000000000002")
	if e = os.Symlink(realGen1, fakeGen2); e != nil {
		t.Fatal(e)
	}

	m2, a2, _ := testRegistry(t)
	l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err == nil {
		defer l2.Close()
		if l2.generation == 2 {
			t.Fatal("a symlinked directory was accepted and served as generation 2")
		}
		if l2.generation != 1 || a2.restoredHigh != first.CommitIndex {
			t.Fatalf("expected fallback to the genuine generation 1 (high-water %d), got generation %d high-water %d", first.CommitIndex, l2.generation, a2.restoredHigh)
		}
		return
	}
	if !strings.Contains(err.Error(), "wal:") {
		t.Fatalf("Open error does not look like a wal error: %v", err)
	}
}

// TestCheckpointSymlinkTailFileIsRefused makes generation 1's tail.wal a
// symlink to a file with otherwise byte-identical, validly-authenticating
// content (a copy of itself). Even a symlink that resolves to legitimate
// bytes must never be accepted as the tail file: the check is on the
// directory entry's own type, not merely on what it happens to resolve to.
func TestCheckpointSymlinkTailFileIsRefused(t *testing.T) {
	if _, err := os.Readlink("/proc/self"); err != nil {
		t.Skip("symlinks unsupported in this environment")
	}
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
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	genDir := filepath.Join(d, checkpointDirName, "gen-00000000000000000001")
	tailPath := filepath.Join(genDir, checkpointTailFile)
	realTail := filepath.Join(genDir, "tail.wal.real")
	if e = os.Rename(tailPath, realTail); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(realTail, tailPath); e != nil {
		t.Fatal(e)
	}

	m2, _, _ := testRegistry(t)
	_, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err == nil {
		t.Fatal("Open accepted a symlinked tail.wal; a symlink standing in for the generation's tail file must be refused (no earlier generation exists here to fall back to)")
	}
	if !strings.Contains(err.Error(), "wal:") {
		t.Fatalf("Open error does not look like a wal error: %v", err)
	}
}

// TestCheckpointNonRegularTailFileIsRefused replaces generation 1's tail.wal
// with a directory (a non-regular, non-symlink entry) of the same name. This
// must be refused rather than crash or hang.
func TestCheckpointNonRegularTailFileIsRefused(t *testing.T) {
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
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	genDir := filepath.Join(d, checkpointDirName, "gen-00000000000000000001")
	tailPath := filepath.Join(genDir, checkpointTailFile)
	if e = os.Remove(tailPath); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(tailPath, 0700); e != nil {
		t.Fatal(e)
	}

	m2, _, _ := testRegistry(t)
	_, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err == nil {
		t.Fatal("Open accepted a directory standing in for tail.wal")
	}
	if !strings.Contains(err.Error(), "wal:") {
		t.Fatalf("Open error does not look like a wal error: %v", err)
	}
}

// TestCheckpointOrphanTmpGenerationIsIgnored is a small addition alongside
// the existing TestCheckpointGenerationCrashRecovery: an orphan .tmp
// generation directory left behind by a crash mid-Checkpoint must be ignored
// as a candidate even when it is NOT the highest-numbered entry in the
// directory (the existing test only exercises the highest-numbered case).
func TestCheckpointOrphanTmpGenerationIsIgnored(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	first := writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil { // generation 1
		t.Fatal(e)
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	root := filepath.Join(d, checkpointDirName)
	// An orphan .tmp dir with a LOWER number than the real generation: it must
	// still never be considered, regardless of its ordinal position among the
	// directory's entries.
	if e = os.Mkdir(filepath.Join(root, "gen-00000000000000000000.tmp"), 0700); e != nil {
		t.Fatal(e)
	}

	m2, a2, _ := testRegistry(t)
	l2, e := Open(LogOptions{Dir: d, Checkpoints: m2})
	if e != nil {
		t.Fatal(e)
	}
	defer l2.Close()
	if l2.generation != 1 || a2.restoredHigh != first.CommitIndex {
		t.Fatalf("generation=%d restoredHigh=%d, want the real generation 1 (high-water %d) undisturbed by the orphan .tmp entry", l2.generation, a2.restoredHigh, first.CommitIndex)
	}
}

// ---------------------------------------------------------------------------
// Generation overflow
// ---------------------------------------------------------------------------

// TestCheckpointGenerationOverflowRefused drives the Log's in-memory
// generation counter to its maximum value (via the same package-private
// field the rest of this package's checkpoint tests already read, e.g.
// TestCheckpointGenerationCrashRecovery's l2.generation) and asserts that
// Checkpoint refuses to wrap rather than silently reusing generation 0 (which
// selectCheckpoint treats as sentinel "legacy / no checkpoint" and would
// therefore be catastrophic to reissue).
func TestCheckpointGenerationOverflowRefused(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	writeCheckpointEntry(t, l, "a", 1)

	l.mu.Lock()
	l.generation = math.MaxUint64
	l.mu.Unlock()

	err := l.Checkpoint()
	if err == nil {
		t.Fatal("Checkpoint succeeded at generation math.MaxUint64, want refusal rather than wraparound to generation 0")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("Checkpoint error = %v, want it to name generation space exhaustion", err)
	}
	l.mu.Lock()
	got := l.generation
	l.mu.Unlock()
	if got != math.MaxUint64 {
		t.Fatalf("generation = %d after a refused checkpoint, want it left at MaxUint64 (unchanged)", got)
	}
	// The log must remain usable for ordinary writes: overflowing the
	// generation counter must not itself poison the Log.
	if l.diverged != nil {
		t.Fatalf("Log diverged after a plain generation-overflow refusal: %v", l.diverged)
	}
	if _, err = l.Write(Entry{Kind: "a", Body: json.RawMessage(`{"n":9}`)}); err != nil {
		t.Fatalf("ordinary write after a refused checkpoint failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Crash boundary exposed by the existing checkpointSyncDir seam
// ---------------------------------------------------------------------------

// TestCheckpointDirSyncFailureAfterRenamePoisonsLog uses the package's own
// checkpointSyncDir indirection (checkpoint.go: "var checkpointSyncDir =
// syncDir") to simulate a crash boundary where the generation directory has
// already been renamed into place durably-uncertain (fsync of the parent
// directory failed) before Checkpoint can confirm it. This is the ONLY fault
// injection seam checkpoint.go itself exposes for this function, so this test
// restricts itself to it rather than reaching for an unexported crash-harness
// type that does not exist for this file.
func TestCheckpointDirSyncFailureAfterRenamePoisonsLog(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	writeCheckpointEntry(t, l, "a", 1)

	injected := errors.New("injected: fsync failed")
	prev := checkpointSyncDir
	first := true
	checkpointSyncDir = func(dir string) error {
		if first {
			first = false
			return injected
		}
		return prev(dir)
	}
	defer func() { checkpointSyncDir = prev }()

	err := l.Checkpoint()
	if err == nil {
		t.Fatal("Checkpoint succeeded despite an injected directory-fsync failure right after the generation rename")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("Checkpoint error = %v, want it to wrap the injected fsync failure", err)
	}

	// The log must now be poisoned: every subsequent Write/Checkpoint must
	// refuse rather than proceed on a generation whose durability this
	// process could not confirm.
	if l.diverged == nil {
		t.Fatal("Log.diverged is nil after a checkpoint publication failure past the rename; the log must be poisoned until restart")
	}
	if _, werr := l.Write(Entry{Kind: "a", Body: json.RawMessage(`{"n":2}`)}); werr == nil {
		t.Fatal("Write succeeded on a Log that a checkpoint-publication failure should have poisoned")
	}
	if cerr := l.Checkpoint(); cerr == nil {
		t.Fatal("a second Checkpoint succeeded on a poisoned Log")
	}
}

// ---------------------------------------------------------------------------
// Spec Server task a1cbef29-400a-4a1e-9638-cc14d38a7ebf, v5: five further
// adversarial scenarios, each complementing (not duplicating) equivalent
// coverage that has since landed concurrently in checkpoint_test.go.
// ---------------------------------------------------------------------------

// TestCheckpointNewerTailStolenIntoOlderGenerationRejectsBeforeRestore covers
// the DIRECTION checkpoint_test.go's TestCheckpointForwardAndBackwardTailIdentity
// exercises structurally too, but constructed so the naive "commit index <=
// declared high-water" boundary check CANNOT be what catches it: every
// substituted commit index is comfortably ABOVE the high-water of the
// generation it is stolen into (it came from a later point in real history),
// so only the cryptographic per-generation tail context/identity binding
// (codec.context, folded into every frame AND file-header MAC -- see
// format.go's codec.mac and DECISIONS.md) can be what rejects it. Three real
// generations are produced so that after generation 3's real tail is stolen
// into generation 2's slot and generation 3 itself is removed (so generation
// 2 becomes the highest remaining candidate and is actually tried, rather
// than being shadowed by an untouched, still-present generation 3), a
// genuinely older, wholly authenticated generation 1 remains available for
// fallback.
func TestCheckpointNewerTailStolenIntoOlderGenerationRejectsBeforeRestore(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	i1 := writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil { // generation 1, high-water = i1
		t.Fatal(e)
	}
	i2 := writeCheckpointEntry(t, l, "a", 2) // lands in generation 1's own tail
	if e = l.Checkpoint(); e != nil {        // generation 2, high-water = i2
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 3)
	writeCheckpointEntry(t, l, "a", 4) // land in generation 2's own tail (discarded by the theft below)
	if e = l.Checkpoint(); e != nil {  // generation 3, high-water = i4
		t.Fatal(e)
	}
	i5 := writeCheckpointEntry(t, l, "a", 5)
	i6 := writeCheckpointEntry(t, l, "a", 6) // land in generation 3's own tail -- this is the "newer valid tail" to steal
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	root := filepath.Join(d, checkpointDirName)
	gen2Tail := filepath.Join(root, fmt.Sprintf("gen-%020d", 2), checkpointTailFile)
	gen3Dir := filepath.Join(root, fmt.Sprintf("gen-%020d", 3))
	gen3Bytes, e := os.ReadFile(filepath.Join(gen3Dir, checkpointTailFile))
	if e != nil {
		t.Fatal(e)
	}
	// Steal generation 3's real, validly-authenticated tail body -- every
	// commit index in it (5 and 6) is well above generation 2's own
	// high-water (i2) -- into generation 2's slot, then remove generation 3
	// entirely so generation 2 becomes the highest remaining candidate and
	// selection actually has to evaluate it rather than pick generation 3
	// untouched.
	if e = os.WriteFile(gen2Tail, gen3Bytes, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.RemoveAll(gen3Dir); e != nil {
		t.Fatal(e)
	}

	m2, a2, _ := spyRegistry(t)
	l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err != nil {
		t.Fatalf("Open failed outright; want a fallback to the wholly authenticated generation 1 instead: %v", err)
	}
	defer l2.Close()
	if l2.generation != 1 {
		t.Fatalf("generation = %d, want 1: generation 2 now holds a tail body that is not what its own manifest authenticates (every substituted commit index is ABOVE its declared high-water, so only the cryptographic tail-identity binding -- not the high-water boundary check -- can be what rejects it), so it must be rejected as a whole", l2.generation)
	}
	if got := len(a2.restores); got != 1 || a2.restores[0].highWater != i1.CommitIndex || a2.restores[0].entries != 1 {
		t.Fatalf("restores = %#v, want exactly one call carrying generation 1's own identity {highWater:%d entries:1} -- generation 2's rejected (stolen) content must never reach Restore at all, not even transiently", a2.restores, i1.CommitIndex)
	}
	// A bad implementation could apply the rejected generation's own records
	// via forward replay (or any other path) BEFORE falling back to Restore
	// generation 1 -- the final `seen` state after Restore would still look
	// correct (Restore overwrites wholesale), so only recording every Apply
	// call, independent of final state, can catch this. Generation 1's own
	// legitimate forward-replayed entry (commit index i2, from generation 1's
	// own tail file, above its own high-water i1) is the ONLY entry that may
	// ever reach Apply here; the stolen generation's own commit indices (i5,
	// i6) must never appear, whether before OR after the Restore call above.
	for _, applied := range a2.applies {
		if applied == i5.CommitIndex || applied == i6.CommitIndex {
			t.Fatalf("applies = %v, contains rejected generation 2's own (stolen) commit index %d -- a rejected generation's records must never reach Apply, not even transiently before the eventual fallback Restore hides it", a2.applies, applied)
		}
	}
	if got := len(a2.applies); got != 1 || a2.applies[0] != i2.CommitIndex {
		t.Fatalf("applies (pre-fallback-write) = %v, want exactly [%d] -- generation 1's own single legitimate forward-replayed entry, and nothing from the rejected generation", a2.applies, i2.CommitIndex)
	}
	// Invariant 1 (server-authoritative ids, never reused, including across
	// restarts): even though generation 2 and 3's served/replayed state is
	// entirely lost by this rejection (that IS the accepted cost of
	// whole-generation rejection -- no partial credit from a compromised
	// segment), the durable, directory-wide index floor is independent of
	// which checkpoint generation gets selected, so the next index handed
	// out after this recovery must still be strictly greater than the
	// highest index this data directory ever wrote (i6), never merely
	// greater than what generation 1's own tail happens to contain (which
	// would only require > i2 and would silently reuse i3..i6).
	if next := l2.w.NextIndex(); next <= i6.CommitIndex {
		t.Fatalf("NextIndex = %d, want strictly greater than %d (the highest index ever written to this data directory, even though it is now unreachable from served state) -- invariant 1 forbids reusing it", next, i6.CommitIndex)
	}
	fresh := writeCheckpointEntry(t, l2, "a", 7)
	if fresh.PrepareIndex <= i6.CommitIndex {
		t.Fatalf("fresh write's index = %d, reused an index (<=%d) that this data directory already burned before the rejected generation was ever created -- invariant 1 violation", fresh.PrepareIndex, i6.CommitIndex)
	}
	if _, serr := os.Lstat(gen3Dir + ".orphan"); serr == nil {
		t.Fatal("removed generation 3's directory reappeared as a quarantine artefact; it was deleted by this test, not by Open, so no .orphan for it should exist")
	}
	if _, serr := os.Lstat(filepath.Join(root, fmt.Sprintf("gen-%020d", 2)) + ".orphan"); serr != nil {
		t.Fatalf("generation 2 was rejected but never quarantined: %v", serr)
	}
}

// TestCheckpointStaleCurrentCannotHideDeeperNewestGeneration is a deeper
// variant of the stale-CURRENT scenario: CURRENT is left perfectly valid,
// authentic plaintext (not corrupted, not forged) but names a MIDDLE
// generation while a genuinely newer, wholly authenticated THIRD generation
// exists. This is adversarially stronger than naming the very oldest
// generation: a middle value is not "obviously" stale by comparison with a
// single alternative, so it better proves selection is driven by scanning
// every candidate generation directory (newest-first) and never gated on
// trusting CURRENT's claim about what the newest generation is.
func TestCheckpointStaleCurrentCannotHideDeeperNewestGeneration(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil { // generation 1
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 2)
	if e = l.Checkpoint(); e != nil { // generation 2 -- CURRENT will be stuck pointing here
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 3)
	newest := writeCheckpointEntry(t, l, "a", 4)
	if e = l.Checkpoint(); e != nil { // generation 3, the genuine newest
		t.Fatal(e)
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	root := filepath.Join(d, checkpointDirName)
	if e = os.WriteFile(filepath.Join(root, checkpointCurrent), []byte("gen-00000000000000000002\n"), 0600); e != nil {
		t.Fatal(e)
	}

	m2, a2, _ := testRegistry(t)
	l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.generation != 3 || a2.restoredHigh != newest.CommitIndex {
		t.Fatalf("generation/restoredHigh = %d/%d, want 3/%d: a valid, unmodified, but STALE CURRENT naming a middle generation must never suppress the genuinely newest, wholly authenticated generation 3", l2.generation, a2.restoredHigh, newest.CommitIndex)
	}
}

// TestCheckpointFIFOTailRefusedWithoutBlocking creates a checkpoint tail path
// as a FIFO with no reader ever present on the other end. A naive
// implementation that opened this path for reading with a plain blocking
// os.Open/O_RDONLY (no O_NONBLOCK, and no Lstat gate beforehand) would hang
// Open() indefinitely -- there is no writer either, so the open(2) syscall
// itself never returns. This test proves the actual behaviour is bounded: it
// races Open() against a generous timeout on an independent goroutine so a
// regression fails FAST and LOUDLY here rather than hanging the whole test
// binary. It is skipped only on platforms without POSIX FIFO support via
// syscall.Mkfifo.
func TestCheckpointFIFOTailRefusedWithoutBlocking(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skipf("no syscall.Mkfifo-based named pipe support on %s", runtime.GOOS)
	}
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
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}
	tail := filepath.Join(d, checkpointDirName, fmt.Sprintf("gen-%020d", 1), checkpointTailFile)
	if e = os.Remove(tail); e != nil {
		t.Fatal(e)
	}
	if e = mkfifo(tail); e != nil {
		t.Fatal(e)
	}

	m2, _, _ := testRegistry(t)
	done := make(chan error, 1)
	go func() {
		opened, err := Open(LogOptions{Dir: d, Checkpoints: m2})
		if err == nil {
			_ = opened.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open succeeded reading a FIFO as a checkpoint tail; want it refused before any blocking read/open of the pipe")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open blocked for at least 5s on a FIFO checkpoint tail with no reader present; the pre-open regular-file check (Lstat before any open syscall) must reject a FIFO before ever attempting to read it, not hang")
	}
}

// TestCheckpointDualMasqueradeQuarantinedWithoutHidingGenuineFallback plants
// TWO simultaneous masquerading regular files in the checkpoint root: one at
// a plain "gen-N" name (as if it were a published generation directory) and
// one at a "gen-N.tmp" name (as if it were an in-progress one), both at
// generation numbers ABOVE the one genuine, already-published generation.
// Both must be quarantined (renamed aside with a ".orphan" suffix, per
// quarantineCheckpointPath) without either being mistaken for a real
// candidate and, critically, WITHOUT either masquerade preventing selection
// from reaching and serving the genuine older generation underneath them. A
// subsequent Checkpoint() must also succeed and reuse the exact generation
// number the masquerading "gen-N" file had occupied, proving the quarantine
// truly freed that name rather than leaving it wedged.
func TestCheckpointDualMasqueradeQuarantinedWithoutHidingGenuineFallback(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil { // the sole genuine generation: 1
		t.Fatal(e)
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	root := filepath.Join(d, checkpointDirName)
	fakeGen := "gen-00000000000000000002"     // masquerades as a published generation
	fakeTmp := "gen-00000000000000000005.tmp" // masquerades as an in-progress one, further ahead still
	if e = os.WriteFile(filepath.Join(root, fakeGen), []byte("regular file masquerading as a checkpoint generation directory"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, fakeTmp), []byte("regular file masquerading as an in-progress checkpoint generation"), 0600); e != nil {
		t.Fatal(e)
	}

	m2, a2, _ := spyRegistry(t)
	l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err != nil {
		t.Fatalf("Open failed outright; both masquerades must be quarantined and generation 1 must still be reachable underneath them: %v", err)
	}
	defer l2.Close()
	if l2.generation != 1 {
		t.Fatalf("generation = %d, want 1: neither masquerading regular file names a real generation, and the genuine one must still be found", l2.generation)
	}
	if len(a2.restores) != 1 || a2.restores[0].entries != 1 {
		t.Fatalf("restores = %#v, want exactly one call, from generation 1's own (single-entry) snapshot", a2.restores)
	}
	for _, name := range []string{fakeGen, fakeTmp} {
		if _, serr := os.Lstat(filepath.Join(root, name+".orphan")); serr != nil {
			t.Fatalf("masquerading %q was not quarantined: %v", name, serr)
		}
		if _, serr := os.Lstat(filepath.Join(root, name)); serr == nil {
			t.Fatalf("masquerading %q still occupies its original name after quarantine; the name must be freed", name)
		}
	}
	if err = l2.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint after quarantining the dual masquerade failed to progress: %v", err)
	}
	if l2.generation != 2 {
		t.Fatalf("post-checkpoint generation = %d, want 2: quarantining the masquerading %q must free that exact generation name for genuine use, not skip past it or wedge on it", l2.generation, fakeGen)
	}
}

// TestCheckpointRejectedGenerationFallbackPreservesIndexAndGlobalOrder pins
// invariants 1, 4, 5 and 6 together for the specific case this v5 pass is
// about: a REJECTED generation (via tail-header tamper, deliberately a
// DIFFERENT rejection mechanism from the substitution tests above, so this
// test is not coupled to how rejection is triggered, only to what must hold
// once it is) whose fallback must still (a) never reuse an index this data
// directory already burned, (b) keep a fresh MultiApplier's cross-kind Apply
// order exactly the commit order of what genuinely survives the fallback
// (never the order commits were originally issued in across the lost
// segment, and never interleaved incorrectly across participants), and (c)
// leave the participants' served state precisely a PREFIX of the accepted
// history (the part covered by the wholly authenticated fallback generation
// and its own tail) -- never a state that silently includes anything from
// the rejected generation.
func TestCheckpointRejectedGenerationFallbackPreservesIndexAndGlobalOrder(t *testing.T) {
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
	if err = l.Checkpoint(); err != nil { // generation 1, high-water = c2
		t.Fatal(err)
	}
	c3 := writeCheckpointEntry(t, l, "a", 3)
	c4 := writeCheckpointEntry(t, l, "b", 4) // land in generation 1's own tail
	if err = l.Checkpoint(); err != nil {    // generation 2, high-water = c4
		t.Fatal(err)
	}
	c5 := writeCheckpointEntry(t, l, "a", 5)
	c6 := writeCheckpointEntry(t, l, "b", 6) // land in generation 2's own tail -- this is what will be discarded
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(d, checkpointDirName)
	// Tamper generation 2's manifest MAC itself (same single-byte-flip
	// technique as TestCheckpointTailHeaderTamperIsRejected, applied to the
	// manifest instead of the tail header): the simplest possible way to
	// force a rejection, kept deliberately different in mechanism from the
	// substitution tests above.
	corruptByteAt(t, filepath.Join(root, fmt.Sprintf("gen-%020d", 2), checkpointManifestFile), 5)

	var replayOrder []uint64
	a2 := &checkpointTestParticipant{name: "alpha", kinds: []string{"a"}, applyOrder: &replayOrder}
	b2 := &checkpointTestParticipant{name: "beta", kinds: []string{"b"}, applyOrder: &replayOrder}
	m2, err := NewMultiApplier(a2, b2)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := Open(LogOptions{Dir: d, Checkpoints: m2})
	if err != nil {
		t.Fatalf("Open failed outright; want a fallback to the wholly authenticated generation 1: %v", err)
	}
	defer l2.Close()
	if l2.generation != 1 {
		t.Fatalf("generation = %d, want 1: a tampered generation 2 manifest must be rejected as a whole", l2.generation)
	}
	// (b)+(c), captured BEFORE any fresh write below (which would itself
	// append to replayOrder and confound this check): checkpointTestParticipant.Restore
	// replaces p.seen directly and does not append to applyOrder, so
	// generation 1's SNAPSHOT-restored entries (c1, c2) are correctly absent
	// here -- replayOrder is exactly the forward-replay portion recovered
	// from generation 1's own (untouched) tail, i.e. c3 then c4, in commit
	// order. What matters adversarially is what must NOT be there: nothing
	// from the rejected generation 2 (c5, c6) may leak in, at any position.
	recoveredOrder := append([]uint64(nil), replayOrder...)
	wantReplay := []uint64{c3.CommitIndex, c4.CommitIndex}
	if fmt.Sprint(recoveredOrder) != fmt.Sprint(wantReplay) {
		t.Fatalf("recovered forward-replay cross-kind order = %v, want exactly %v (c3, c4 from generation 1's own tail; the rejected generation's c5/c6 must never appear, in any position)", recoveredOrder, wantReplay)
	}
	for _, idx := range []uint64{c5.CommitIndex, c6.CommitIndex} {
		for _, got := range recoveredOrder {
			if got == idx {
				t.Fatalf("recovered order %v contains index %d from the rejected generation", recoveredOrder, idx)
			}
		}
	}
	// The full served state (snapshot restore + forward replay together)
	// must still be exactly the prefix c1..c4, split correctly by kind:
	// alpha (kind "a") sees c1 and c3, beta (kind "b") sees c2 and c4. This
	// is the "memory serves, disk is truth, and a crash recovers to a PREFIX
	// of the accepted history" half of the guarantee (invariant 5) that
	// order-of-Apply alone does not pin.
	if got := fmt.Sprint(indices(a2.seen)); got != fmt.Sprint([]uint64{c1.CommitIndex, c3.CommitIndex}) {
		t.Fatalf("alpha's served state = %v, want exactly [%d %d] (c1 via generation 1's snapshot, c3 via its forward-replayed tail)", got, c1.CommitIndex, c3.CommitIndex)
	}
	if got := fmt.Sprint(indices(b2.seen)); got != fmt.Sprint([]uint64{c2.CommitIndex, c4.CommitIndex}) {
		t.Fatalf("beta's served state = %v, want exactly [%d %d] (c2 via generation 1's snapshot, c4 via its forward-replayed tail)", got, c2.CommitIndex, c4.CommitIndex)
	}
	// (a) no index reuse, checked LAST (and via a fresh write, which itself
	// necessarily perturbs replayOrder -- hence why (b)/(c) were already
	// captured above): c5/c6 were already durably burned before the tamper
	// ever happened, so the next index must skip past them even though
	// generation 2 (and therefore c5, c6 as served state) is gone.
	if next := l2.w.NextIndex(); next <= c6.CommitIndex {
		t.Fatalf("NextIndex = %d, want strictly greater than %d (invariant 1: no id reuse, even across a rejected generation)", next, c6.CommitIndex)
	}
	fresh := writeCheckpointEntry(t, l2, "a", 7)
	if fresh.PrepareIndex <= c6.CommitIndex {
		t.Fatalf("fresh write's index %d reused an index at or below %d that this data directory already burned", fresh.PrepareIndex, c6.CommitIndex)
	}
}

// indices extracts CommitIndex from a []Committed slice in order, for
// concise comparison in test assertions.
func indices(cs []Committed) []uint64 {
	out := make([]uint64, len(cs))
	for i, c := range cs {
		out[i] = c.CommitIndex
	}
	return out
}

// TestCheckpointSelectedTailMidDamageRewrittenExactlyOnceWithLoudDiagnostics
// covers a DIFFERENT repair shape than the pre-existing production test
// TestCheckpointV6SelectedTailRepairAndLegacyV1/selected-tail-repair, which
// appends a torn frame at the very END of a selected generation's tail (a
// pure Repair.Truncated case) and asserts only that Recovered().Repaired and
// the operator log mention the loss. This test corrupts a single record in
// the MIDDLE of the selected generation's tail, with intact records on both
// sides of it -- repairPlan.needsRewrite (salvage.go) forces this down the
// Repair.Rewritten path (the file has to be rebuilt, not merely shortened),
// which is a structurally distinct code path from Truncated and is not
// exercised by that test. It then adds three properties none of the existing
// coverage asserts:
//
//  1. NO SILENT PRE-SELECTION MUTATION: verifyGeneration's own tail scan
//     (validateCheckpointTail) is documented in checkpoint.go as "deliberately
//     read-only ... the selected tail is repaired exactly once by Open below
//     selectCheckpoint" -- but nothing in this package's test suite proves
//     that from outside verifyGeneration. This test calls selectCheckpoint
//     directly (an in-package seam, exactly as this file already calls
//     checkpointTailHeaderMAC/loadMACKey/macKeyPath/authenticatedJSON) BEFORE
//     any repair pass runs, and asserts the on-disk tail bytes are BYTE FOR
//     BYTE unchanged by that call -- selection authenticates and restores
//     without touching the damaged file at all.
//  2. AUTHENTICATED-BEFORE-REPAIR: the damaged tail's header/generation
//     binding MAC (checkpointTailHeaderMAC, the same oracle
//     TestCheckpointTailHeaderTamperIsRejected already relies on) is
//     recomputed directly over the DAMAGED bytes, before Open ever runs, and
//     shown to already match the manifest's authenticated TailHeaderMAC --
//     the damaged generation's identity is observable and authenticatable
//     pre-repair, not something repair has to produce first.
//  3. EXACTLY ONCE, EXACT OUTCOME: the real Open (which does run the one
//     normal repair pass) is asserted to produce an EXACT, specific Repair
//     value (Rewritten, DiscardCount==1, a specific non-zero Removed byte
//     count, a non-empty Reason) and a specific, loud log line -- not merely
//     "some repair happened". A SECOND open of the now-repaired directory is
//     asserted to report the ZERO Repair value and re-emit no repair
//     diagnostic at all, proving the repair happened EXACTLY ONCE rather
//     than being an ongoing condition that keeps re-triggering (or
//     re-logging) on every restart.
func TestCheckpointSelectedTailMidDamageRewrittenExactlyOnceWithLoudDiagnostics(t *testing.T) {
	d := t.TempDir()
	m, _, _ := testRegistry(t)
	l, e := Open(LogOptions{Dir: d, Checkpoints: m})
	if e != nil {
		t.Fatal(e)
	}
	writeCheckpointEntry(t, l, "a", 1)
	if e = l.Checkpoint(); e != nil { // generation 1, snapshot carries entry 1 alone
		t.Fatal(e)
	}
	for n := 2; n <= 6; n++ {
		writeCheckpointEntry(t, l, "a", n) // all five land in generation 1's own tail
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}

	root := filepath.Join(d, checkpointDirName)
	genDir := filepath.Join(root, fmt.Sprintf("gen-%020d", 1))
	tail := filepath.Join(genDir, checkpointTailFile)

	// Offset 639 lands inside the checksum-covered bytes of one interior
	// record while leaving an intact record readable on both sides of it, so
	// exactly one record is discarded from the MIDDLE of the tail --
	// repairPlan.needsRewrite is true (Count=1 > trailing=0, since the
	// damaged region does not run to the file's end), forcing the Rewritten
	// path rather than a simple end-of-file Truncated. This offset and the
	// exact resulting Repair values below were confirmed empirically, against
	// this exact five-entry tail layout, before being pinned here.
	const corruptOffset = 639
	corruptByteAt(t, tail, corruptOffset)
	damaged, e := os.ReadFile(tail)
	if e != nil {
		t.Fatal(e)
	}

	// --- Property 2: authenticated before repair -------------------------
	key, e := loadMACKey(macKeyPath(d))
	if e != nil {
		t.Fatal(e)
	}
	manifestRaw, e := os.ReadFile(filepath.Join(genDir, checkpointManifestFile))
	if e != nil {
		t.Fatal(e)
	}
	var manifest checkpointManifest
	if e = verifyAuthenticatedJSON(manifestRaw, &manifest, key); e != nil {
		t.Fatalf("generation 1's manifest no longer authenticates merely because its tail body was damaged elsewhere: %v", e)
	}
	tf, e := os.Open(tail)
	if e != nil {
		t.Fatal(e)
	}
	gotMAC, e := checkpointTailHeaderMAC(tf, 1, manifest.HighWater, key)
	tf.Close()
	if e != nil {
		t.Fatal(e)
	}
	if gotMAC != manifest.TailHeaderMAC {
		t.Fatalf("damaged tail's header/generation-binding MAC = %q, want it to still match the manifest's authenticated %q: the header binding covers only the fixed-size file header, not this mid-tail record, so the damaged generation's identity must remain observable/authenticatable BEFORE any repair runs", gotMAC, manifest.TailHeaderMAC)
	}

	// --- Property 1: no silent pre-selection mutation ---------------------
	// selectCheckpoint is the exact function Open calls to choose and
	// authenticate a generation; verifyGeneration's own tail scan is
	// documented as read-only. Calling it directly, before any repair pass,
	// proves that documented property from outside rather than merely
	// trusting the comment.
	mSel, aSel, _ := testRegistry(t)
	sel, serr := selectCheckpoint(d, mSel, nil)
	if serr != nil {
		t.Fatalf("selectCheckpoint failed on a generation whose only damage is a single interior record, recoverable by the ordinary repair pass: %v", serr)
	}
	if sel.generation != 1 {
		t.Fatalf("selectCheckpoint chose generation %d, want 1", sel.generation)
	}
	if len(aSel.seen) != 1 {
		t.Fatalf("selectCheckpoint's Restore call delivered %d entries, want exactly 1 (generation 1's own snapshot alone) -- forward tail replay is Open's SEPARATE, LATER pass and must not have happened yet", len(aSel.seen))
	}
	afterSelection, e := os.ReadFile(tail)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(damaged, afterSelection) {
		t.Fatalf("selecting generation 1 changed its tail file's bytes before any repair ran (selection must be read-only): damaged=%d bytes, after-selection=%d bytes, equal=%v", len(damaged), len(afterSelection), bytes.Equal(damaged, afterSelection))
	}

	// --- Property 3: exactly once, exact outcome ---------------------------
	var logs bytes.Buffer
	m2, a2, _ := testRegistry(t)
	l2, err := Open(LogOptions{Dir: d, Checkpoints: m2, Logger: logging.New(&logs, logging.LevelInfo)})
	if err != nil {
		t.Fatal(err)
	}
	rep := l2.Recovered().Repaired
	if !rep.Rewritten || rep.Truncated || rep.DiscardCount != 1 || rep.Removed == 0 || rep.Reason == "" {
		t.Fatalf("Repair after selected-tail mid-damage = %+v, want an exact Rewritten (not Truncated) outcome with DiscardCount==1, a non-zero Removed byte count, and a non-empty Reason", rep)
	}
	if len(a2.seen) != 5 {
		t.Fatalf("alpha.seen has %d entries after repair, want exactly 5 (generation 1's 1-entry snapshot plus the 4 tail records that survive the single interior discard)", len(a2.seen))
	}
	if !bytes.Contains(logs.Bytes(), []byte("wal rewrote a damaged log")) || !bytes.Contains(logs.Bytes(), []byte("discarded_bytes")) {
		t.Fatalf("selected-tail mid-damage repair was not logged loudly with specific evidence: %s", logs.String())
	}
	repaired, e := os.ReadFile(tail)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Equal(damaged, repaired) {
		t.Fatal("tail file bytes are unchanged after Open despite a reported Rewritten repair; the repair did not actually touch the file")
	}
	if e = l2.Close(); e != nil {
		t.Fatal(e)
	}

	// Reopening the now-repaired directory must show the repair happened
	// EXACTLY ONCE, not an ongoing condition.
	var logs2 bytes.Buffer
	m3, a3, _ := testRegistry(t)
	l3, err := Open(LogOptions{Dir: d, Checkpoints: m3, Logger: logging.New(&logs2, logging.LevelInfo)})
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Close()
	rep2 := l3.Recovered().Repaired
	if rep2.Rewritten || rep2.Truncated || rep2.DiscardCount != 0 {
		t.Fatalf("re-opening an already-repaired selected tail reported a fresh repair: %+v, want the zero value -- the repair must not re-trigger on every subsequent start", rep2)
	}
	if bytes.Contains(logs2.Bytes(), []byte("wal rewrote a damaged log")) {
		t.Fatalf("re-opening an already-repaired selected tail re-emitted the repair diagnostic: %s", logs2.String())
	}
	if len(a3.seen) != 5 {
		t.Fatalf("alpha.seen after re-opening the already-repaired tail = %d entries, want the same exact 5 as the first repair", len(a3.seen))
	}
}

// ---------------------------------------------------------------------------
// helpers local to this file
// ---------------------------------------------------------------------------

func corruptByteAt(t *testing.T, path string, offset int) {
	t.Helper()
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if offset >= len(raw) {
		t.Fatalf("corruptByteAt: offset %d out of range for %d-byte file %s", offset, len(raw), path)
	}
	raw[offset] ^= 0xFF
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
}
