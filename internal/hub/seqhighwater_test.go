package hub_test

// THE SEQUENCE HIGH-WATER MARK UNDER DEEP LOG DAMAGE.
//
// This file holds exactly one test, and it exists because a claim that was
// written into DECISIONS.md — that wal's own durable RECORD-INDEX floor
// subsumes the message-SEQUENCE floor, so internal/hub needs no floor file of
// its own — is measurably false. See DECISIONS.md, 2026-08-07, "The WAL
// record-index floor does NOT subsume the message-sequence floor".
//
// # Why an exhaustive sweep rather than one hand-picked truncation
//
// A single truncation point proves one thing about one byte offset. The
// interesting property here is a UNIVERSAL one — "no matter where the log is
// cut, the bus never hands out a sequence it has already handed out" — and the
// failures it is guarding against are precisely the ones that hide at an
// offset nobody thought to pick. So every offset from an empty file to the
// intact file is tried, each against a pristine copy of the data directory.
//
// # The negative control is not decoration; it is what stops this test rotting
//
// The second arm removes <data-dir>/message-seq-floor and REQUIRES the sweep to
// find reissues. Without it, this test would still pass on the day somebody
// makes the sweep toothless — a copy that silently drops the log, an offset
// range that collapses to nothing, a mint that stops allocating — and CLAUDE.md
// is explicit that a proof never observed failing is not evidence that it can
// fail. Here that observation is permanent and runs on every invocation.
//
// Measured, and quoted in DECISIONS.md: 0 of 248 offsets reissue with the floor
// file present, 247 of 248 reissue without it. The one survivor in the control
// arm is the undamaged file, where the in-log "seqfloor" record still replays.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// mintsBeforeDamage is how many sequences are handed out before the log is
// damaged.
//
// It must be MORE than the number of WAL record indices those mints consume,
// or the test proves nothing: the whole defect is that the two counters have
// come apart, and if the WAL index happened to sit above every issued sequence
// then NextIndex-1 alone would mask the reissue. Five mints consume five
// sequences and TWO indices (one floor record covers MintBatchSize=256
// numbers), which is the reviewer's original probe reproduced. The sweep
// asserts that gap rather than assuming it — see the guard in the sweep body.
const mintsBeforeDamage = 5

// TestSequenceHighWaterSurvivesDeepDamage sweeps every truncation offset of the
// durable log and checks, at each one, that the bus does not resume its message
// sequence at or below a number it has already handed to a client.
//
// # What "reissued" means here, and why it is measured with a REAL MINT
//
// The resumed floor is read by calling Hub.Mint on the recovered bus and
// looking at the number it returns. That is deliberate: an accessor for the
// internal floor would test what the hub BELIEVES, and the thing that hurts a
// client is what the hub HANDS OUT. The two were the same on every reading so
// far, and the day they differ is the day this test has to notice.
//
// An offset counts as a reissue if the minted sequence is at or below the
// highest already issued — not merely if it is equal to one of them. Every
// number up to the durable floor is BURNED whether or not a message carried it
// (internal/ids/sequence.go), so resuming anywhere inside that range is the
// violation, and requiring exact equality would let a floor that rewinds by a
// few numbers slip through.
//
// # Why nothing but the log is damaged
//
// A truncation of agent-bus.wal is what a torn write, a full disk or a media
// error at the tail actually looks like, and it is the one artifact recovery is
// permitted to truncate, rewrite or quarantine wholesale. wal-mac.key and
// wal-index-floor are copied through intact ON PURPOSE: the point is not that
// the bus survives losing everything, it is that the surviving WAL index floor
// — the thing DECISIONS.md claimed was sufficient — demonstrably is not.
func TestSequenceHighWaterSurvivesDeepDamage(t *testing.T) {
	pristine, issued := mintThenCloseCleanly(t)
	highest := issued[len(issued)-1]

	walBytes, err := os.ReadFile(filepath.Join(pristine, wal.WALFileName))
	if err != nil {
		t.Fatalf("reading the pristine log: %v", err)
	}

	cases := []struct {
		name string
		// removeSeqFloorFile models a data directory that never had the file, or
		// one an operator moved aside — which is the state the whole tree was in
		// before aad611c, and the state DECISIONS.md argued was safe.
		removeSeqFloorFile bool
		// wantReissues is the NEGATIVE CONTROL switch. When true the sweep MUST
		// find at least one reissue, or it has lost its power to detect the
		// defect and is proving nothing.
		wantReissues bool
	}{
		{
			name:               "the durable message-sequence floor file is present",
			removeSeqFloorFile: false,
			wantReissues:       false,
		},
		{
			name:               "NEGATIVE CONTROL: the message-sequence floor file is gone",
			removeSeqFloorFile: true,
			wantReissues:       true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var reissued []int
			offsets := 0
			for offset := 0; offset <= len(walBytes); offset++ {
				offsets++
				seq := resumeAfterTruncation(t, pristine, walBytes, offset, tc.removeSeqFloorFile)
				if seq <= highest {
					reissued = append(reissued, offset)
				}
			}
			t.Logf("swept %d truncation offsets of %s (%d bytes); %d offsets REISSUED a sequence at or below %d (already handed out: %v)",
				offsets, wal.WALFileName, len(walBytes), len(reissued), highest, issued)

			if tc.wantReissues && len(reissued) == 0 {
				t.Fatalf("the NEGATIVE CONTROL found no reissue across %d truncation offsets. That does not mean the bus is safe; it means this sweep can no longer detect the defect it exists to detect, so the other arm proves nothing. Check that the log is really being copied and truncated, that %d mints still consume fewer WAL indices than sequences, and that the mint still allocates",
					offsets, mintsBeforeDamage)
			}
			if !tc.wantReissues && len(reissued) != 0 {
				t.Fatalf("with %s present, %d of %d truncation offsets resumed the message sequence at or below %d, a number already handed to a client: offsets %v. A client holds a signature over that assignment, so two validly-signed messages would carry one origin message id and nothing downstream can detect it (invariant 1)",
					hub.SeqFloorFileName, len(reissued), offsets, highest, clipOffsets(reissued))
			}
		})
	}
}

// mintThenCloseCleanly builds the pristine data directory the sweep copies: a
// real log, a real hub, mintsBeforeDamage real mints, and a CLEAN close.
//
// The clean close matters. It seals wal's index floor, so the reopened copies
// resume at the file's own dense arithmetic rather than at a reserve-block
// ceiling — which keeps the WAL index far below the issued sequences and keeps
// the sweep pointed at the property under test instead of at an artifact of
// index reservation.
//
// It returns the directory and the sequences that were handed out, ascending.
func mintThenCloseCleanly(t *testing.T) (string, []uint64) {
	t.Helper()
	dir := t.TempDir()
	lg := openTestLog(t, dir, false)
	alpha := agentID(t, testBusID, "alpha")
	h, _ := openMintHub(t, dir, lg, nil, "", "alpha")

	issued := make([]uint64, 0, mintsBeforeDamage)
	for i := 0; i < mintsBeforeDamage; i++ {
		issued = append(issued, mustMint(t, h, alpha, "send", fmt.Sprintf("k-%d", i)).Seq)
	}
	sort.Slice(issued, func(i, j int) bool { return issued[i] < issued[j] })
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	// The guard that keeps this test honest. If the WAL had consumed at least as
	// many indices as the mint consumed sequences, then NextIndex-1 would bound
	// the sequence all by itself and every assertion below would pass for the
	// wrong reason — which is exactly the retired counting argument.
	next := reopenNextIndex(t, dir)
	if next > issued[len(issued)-1] {
		t.Fatalf("%d mints issued sequences up to %d but the log resumes at record index %d, at or above them. The WAL index would then bound the sequence on its own and this sweep would pass without testing anything; raise mintsBeforeDamage or check MintBatchSize (%d)",
			mintsBeforeDamage, issued[len(issued)-1], next, hub.MintBatchSize)
	}
	return dir, issued
}

// reopenNextIndex reports the record index a fresh open of dir would append at,
// then closes the log again so the directory is left exactly as it was found.
func reopenNextIndex(t *testing.T, dir string) uint64 {
	t.Helper()
	lg := openTestLog(t, dir, false)
	next := lg.Recovered().NextIndex
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the probe log: %v", err)
	}
	return next
}

// resumeAfterTruncation copies the pristine data directory, truncates its log to
// offset bytes, opens a bus over the wreckage and returns the sequence a REAL
// mint hands out.
//
// Every iteration gets its own directory. Sharing one and rewinding it would
// leak the previous iteration's floor file — and a floor file left behind by a
// healthier truncation is precisely the thing that would make a broken build
// look correct.
func resumeAfterTruncation(t *testing.T, pristine string, walBytes []byte, offset int, removeSeqFloorFile bool) uint64 {
	t.Helper()
	dir := t.TempDir()
	copyDataDir(t, pristine, dir)
	if err := os.WriteFile(filepath.Join(dir, wal.WALFileName), walBytes[:offset], 0o600); err != nil {
		t.Fatalf("truncating the copied log to %d bytes: %v", offset, err)
	}
	if removeSeqFloorFile {
		if err := os.Remove(filepath.Join(dir, hub.SeqFloorFileName)); err != nil {
			t.Fatalf("removing %s from the copy: %v", hub.SeqFloorFileName, err)
		}
	}

	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		// Recovery ALWAYS reaches a running server (invariant 6): a damaged log
		// is truncated, rewritten or quarantined, never a refusal to boot. A
		// failure here is therefore a real regression and not this test's
		// business to tolerate.
		t.Fatalf("wal.Open on a log truncated to %d bytes: %v; invariant 6 requires recovery to reach a running server over damage", offset, err)
	}
	defer lg.Close()

	alpha := agentID(t, testBusID, "alpha")
	h, _ := openMintHub(t, dir, lg, nil, "", "alpha")
	m, err := h.Mint(hub.MintRequest{Sender: alpha, Op: "send", IdempotencyKey: "k-after-damage"})
	if err != nil {
		t.Fatalf("Mint after truncating the log to %d bytes: %v", offset, err)
	}
	return m.Seq
}

// copyDataDir copies the regular files of src into dst. It is a flat copy
// because a bus data directory is flat; a subdirectory appearing in one is a
// change this test should be told about rather than silently skip.
func copyDataDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("%s contains a subdirectory %q; a bus data directory is flat, and copying it flat would silently drop that content", src, e.Name())
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Join(src, e.Name()), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil {
			t.Fatalf("writing %s: %v", filepath.Join(dst, e.Name()), err)
		}
	}
}

// clipOffsets bounds the offset list a failure message prints. A total failure
// names hundreds of offsets, and the first handful plus the count says
// everything the next reader needs.
func clipOffsets(offsets []int) string {
	const max = 12
	if len(offsets) <= max {
		return fmt.Sprint(offsets)
	}
	return fmt.Sprintf("%v ... (%d in total)", offsets[:max], len(offsets))
}
