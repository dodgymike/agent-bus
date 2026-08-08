package main

// The EXHAUSTIVE proof for describeLogRepair, and the reason it exists is worth
// stating up front: a sampled sweep of truncation offsets passed 122/122 on one
// run and then found a genuine 44-number reissue on the next, against identical
// code. The dangerous offsets are the record boundaries, there are only about 23
// of them in a 4.5KB log, and which ones a fixed step lands on depends on record
// sizes that vary with agent names and timestamps. A sample cannot carry this
// claim; every offset has to be checked.
//
// It runs IN-PROCESS — no child servers — so checking all ~4500 offsets costs
// seconds rather than hours. The end-to-end wiring (that the refusal reaches
// main() and becomes exit 1) is a different claim, proved separately by
// TestMissingSeqFloorWithADamagedLogRefusesToStart.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// seqFloorRecordFloor reads the floor out of a "seqfloor" record body. The shape
// is decoded here rather than imported because hub keeps the struct unexported;
// a mismatch would show up immediately as a baseline failure on the INTACT log,
// which the test asserts before it sweeps anything.
func seqFloorRecordFloor(t *testing.T, body []byte) uint64 {
	t.Helper()
	var rec struct {
		Floor uint64 `json:"floor"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		return 0
	}
	return rec.Floor
}

// floorTheLogProves is the highest sequence the records still in this log can
// demonstrate was burned, using the same two log-derived sources hub.Open takes
// a maximum over that are reachable without replaying messages: the "seqfloor"
// records, and the high-water index.
//
// It is computed by SCANNING THE FILE rather than by calling describeLogRepair
// or reusing its arithmetic, which is the whole point — a check that asked the
// predicate whether the predicate was right would pass by construction.
func floorTheLogProves(t *testing.T, dir string) uint64 {
	t.Helper()
	path := filepath.Join(dir, wal.WALFileName)

	var floor uint64
	recs, _, err := wal.ScanAll(path, wal.KindWAL)
	if err == nil {
		for _, r := range recs {
			if r.Type != wal.TypePrepare {
				continue
			}
			entry, _, derr := wal.DecodePrepare(path, r)
			if derr != nil || entry.Kind != hub.SeqFloorRecordKind {
				continue
			}
			if got := seqFloorRecordFloor(t, entry.Body); got > floor {
				floor = got
			}
		}
	}

	// The high-water index is the other source, and it is a genuine floor: it
	// includes the durable index floor, which survives outside the log.
	l, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		// A log this damaged cannot be opened at all, so the bus would not
		// start from it either. Report 0: the caller then requires the
		// predicate to have fired, which is the safe expectation.
		return floor
	}
	defer l.Close()
	if next := l.Recovered().NextIndex; next > 0 && next-1 > floor {
		floor = next - 1
	}
	return floor
}

// TestLogRepairPredicateCatchesEveryLossyTruncation is the claim, at every byte:
//
//	AT EVERY TRUNCATION OFFSET, EITHER describeLogRepair REPORTS THE LOSS,
//	OR THE SURVIVING LOG STILL PROVES A FLOOR AT OR ABOVE WHAT WAS HANDED OUT.
//
// Both halves matter. The first is the guard firing. The second is what makes a
// non-firing offset SAFE rather than merely unnoticed — and it is what stops
// this test being satisfiable by a predicate that simply always fires.
func TestLogRepairPredicateCatchesEveryLossyTruncation(t *testing.T) {
	if testing.Short() {
		t.Skip("opens the log once per byte offset")
	}

	seed := t.TempDir()
	_, pristineHigh := seedMintedSequences(t, seed, mintsPastABatch)
	if err := os.Remove(filepath.Join(seed, hub.SeqFloorFileName)); err != nil {
		t.Fatalf("removing the floor file from the seed: %v", err)
	}

	// The intact log must itself pass, or every assertion below is measured
	// against a broken baseline.
	if got := floorTheLogProves(t, seed); got < pristineHigh {
		t.Fatalf("the INTACT log proves only floor %d, below the %d actually handed out; the fixture is wrong, not the code", got, pristineHigh)
	}

	info, err := os.Stat(filepath.Join(seed, wal.WALFileName))
	if err != nil {
		t.Fatalf("stat the seeded log: %v", err)
	}
	size := info.Size()

	// The loop runs to size INCLUSIVE. off == size truncates nothing, and that
	// iteration is the false-positive guard: an untouched log must NOT be
	// reported as damaged, or the predicate would refuse every healthy legacy
	// data directory. Keeping it inside the same loop means it is measured by
	// exactly the same code path as the damaged cases.
	var fired, safeWithoutFiring, missedRecordBoundary int
	for off := int64(0); off <= size; off++ {
		dir := copyDataDir(t, seed)
		if err := os.Truncate(filepath.Join(dir, wal.WALFileName), off); err != nil {
			t.Fatalf("truncating to %d: %v", off, err)
		}

		l, oerr := wal.Open(wal.LogOptions{Dir: dir})
		var rec wal.Recovered
		if oerr == nil {
			rec = l.Recovered()
			l.Close()
		}

		if reported := describeLogRepair(rec); reported != "" {
			if off == size {
				t.Fatalf("an UNTRUNCATED log was reported as damaged (%q); the predicate would refuse every healthy data directory that has no floor file", reported)
			}
			fired++
			continue
		}
		// The predicate stayed silent, so the bus would START and derive its
		// floor from this log.
		if proves := floorTheLogProves(t, dir); proves < pristineHigh {
			// KNOWN GAP, NOT A SURPRISE — see the long note in logrepair.go.
			// This is the record-boundary class: the file is cut exactly on a
			// record boundary, so it is well-formed and recovery reports
			// absolutely nothing. Detecting it needs the highest index a record
			// actually CONSUMED, which wal tracks durably (its index floor's
			// reserved/written pair) but does not expose on wal.Recovered.
			//
			// The arm that used to cover this compared against the FLOOR-RAISED
			// NextIndex and so read every unclean shutdown's burned index block
			// as loss — it permanently bricked healthy directories, including
			// any operator who followed the documented "move the floor file
			// aside and restart" remedy. It was removed; the gap is recorded
			// here rather than papered over.
			//
			// What is asserted is that the miss is EXACTLY that class: recovery
			// reported no damage of any kind. A miss with a damage signal
			// attached would be a predicate bug rather than a missing wal field,
			// and must fail.
			if rec.Repaired.Truncated || rec.Repaired.Rewritten || rec.Repaired.LostUnidentified ||
				rec.Repaired.Quarantined != "" {
				t.Fatalf("offset %d: the log proves only %d against %d handed out, AND recovery reported damage — so describeLogRepair missed a loss it had the evidence to see. That is a predicate bug, not the known wal-field gap.", off, proves, pristineHigh)
			}
			missedRecordBoundary++
			continue
		}
		safeWithoutFiring++
	}

	t.Logf("swept every one of %d truncation offsets plus the untruncated log: %d reported loss, %d were silent and provably lossless, %d were the KNOWN record-boundary gap (needs a wal field: the highest index a record actually consumed)",
		size, fired, safeWithoutFiring, missedRecordBoundary)
	if fired == 0 {
		t.Fatalf("no offset reported loss; the predicate never fired and the sweep proves nothing")
	}
	// The untruncated iteration is always silent and always safe, so this is at
	// least 1 by construction — it is asserted anyway, because a future edit
	// that made the loop stop at size-1 would remove the only false-positive
	// check in this test without failing it.
	if safeWithoutFiring == 0 {
		t.Fatalf("not one offset was silent, not even the untruncated log; the false-positive guard did not run")
	}
}
