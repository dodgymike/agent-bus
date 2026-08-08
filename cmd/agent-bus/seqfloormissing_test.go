package main

// The crash-injection proof for the ABSENT message-seq-floor file, which is a
// different defect from the FORGED one in seqfloorforge_test.go and has the
// opposite failure mode: not a bus that refuses every send, but a bus that
// cheerfully REISSUES sequence numbers a client already holds a signature over.
//
// # The two cases, and why only one of them is safe
//
//	A. floor file absent, log INTACT     -> start, rebuild the floor from the log.
//	B. floor file absent, log DAMAGED    -> the log can no longer prove the floor.
//
// Case A is a SUPPORTED UPGRADE PATH, not damage: a data directory written by an
// agent-bus that predates the floor file has no such file, and hub.Open says so
// in as many words. It must keep working, or the guard below has broken the very
// migration the floor file exists to serve.
//
// Case B was reproduced, and it is an invariant-1 violation. With the floor file
// deleted and the log truncated, the bus starts and resumes the sequence from
// what the truncated log can still prove — far BELOW numbers already minted,
// handed to clients and signed. The counter then walks back up through them and
// mints a second message under a message id that already exists, with different
// content.
//
// # Why this is worse than a corner case
//
// The client-visible harm is silent and is worst for the most correct clients.
// Consumers are REQUIRED to deduplicate on message_id. After a reissue a
// correctly-implemented consumer sees a message id it has already seen,
// concludes it is the duplicate it was told to expect, and DROPS the new
// message. The data is lost invisibly at both ends.
//
// # The knowledge was already in the tree; only the guard was missing
//
// seqfloorfile.go's openSeqFloorFile comment names "missing-file plus quarantine
// on the SAME start" as the one uncovered case. seqFloorCorrupt's remedy says
// the log fallback is "correct ONLY if that log has not also been damaged or
// quarantined" — which is exactly case B's precondition. The CORRUPT path
// refused and explained itself; the MISSING path performed the same unsafe
// fallback silently and logged that it had "closed the window".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// mintsPastABatch is how many reservations the fixtures mint, and the number is
// load-bearing rather than arbitrary.
//
// The defect only becomes VISIBLE once the sequence has outrun the WAL index,
// and that is the entire structural point of hub.MintBatchSize: one fsynced
// "seqfloor" record burns 256 sequences, so 300 mints consume 300 sequences
// against a WAL holding only a handful of records. Lose those two seqfloor
// records and every log-derived fallback collapses to about the record count,
// which is two orders of magnitude below what was handed out.
//
// A fixture that minted three would consume three sequences against four or
// five indices, the index high-water would still exceed them, and the bus would
// resume ABOVE the damage by luck. Measured: at 3 mints HEAD resumes at 5 after
// the same truncation and looks perfectly safe. The bug is real at both sizes;
// only one of them can see it.
const mintsPastABatch = 300

// mintSeq mints one reservation and returns the sequence the server assigned.
func mintSeq(t *testing.T, dataDir, addr string, a *busAgent) uint64 {
	t.Helper()
	body := mustPostJSON(t, dataDir, addr, "/v1/mint", a.token, map[string]string{
		"op":              "send",
		"idempotency_key": fmt.Sprintf("mint-%d", time.Now().UnixNano()),
	}, http.StatusCreated)
	var out struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding the mint response %s: %v", body, err)
	}
	if out.Seq == 0 {
		t.Fatalf("mint returned sequence 0: %s", body)
	}
	return out.Seq
}

// seedMintedSequences starts a bus on dir, mints `mints` reservations and
// returns one enrolled agent plus the HIGHEST sequence the server handed out.
// The bus is stopped cleanly before it returns.
//
// The returned high-water mark is the whole point: every one of those numbers
// has left the process, and a client could hold a signature over any of them.
// Reissuing any value at or below it is the violation.
//
// The mints are spread over SEVERAL agents because hub.MaxOutstandingMintsPerAgent
// caps one agent at 64 unspent reservations and answers the 65th with 503. That
// cap is correct and this fixture must not disable it: the shape being modelled
// is a busy bus, which is many agents minting, not one agent defeating a quota.
func seedMintedSequences(t *testing.T, dir string, mints int) (*busAgent, uint64) {
	t.Helper()
	proc := startServer(t, dir)
	addr := proc.awaitServerStarted(t)

	perAgent := hub.MaxOutstandingMintsPerAgent / 2
	var first *busAgent
	var highest uint64
	for minted := 0; minted < mints; {
		agent := enrolNewAgent(t, dir, addr, fmt.Sprintf("seq-victim-%d", minted))
		agent.authenticate(t, dir, addr)
		if first == nil {
			first = agent
		}
		for i := 0; i < perAgent && minted < mints; i, minted = i+1, minted+1 {
			if seq := mintSeq(t, dir, addr, agent); seq > highest {
				highest = seq
			}
		}
	}
	if highest == 0 {
		t.Fatalf("seeding minted no sequences; the fixture proves nothing")
	}
	agent := first

	proc.signal(t, syscall.SIGTERM)
	if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
	}
	return agent, highest
}

// copyDataDir makes an independent copy of a seeded data directory, so a sweep
// can damage many copies without paying to seed each one.
//
// It copies FILES ONLY and does not recurse: a data directory is flat by
// construction (CONTRACTS-ONDISK.md), and a subdirectory appearing in one would
// be a change worth failing on rather than silently skipping.
func copyDataDir(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "copy")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("%s contains a subdirectory %q; this helper assumes a flat data directory", src, e.Name())
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, info.Mode().Perm()); err != nil {
			t.Fatalf("writing %s: %v", e.Name(), err)
		}
	}
	return dst
}

// A subprocess sweep over truncation offsets USED TO LIVE HERE and was removed
// on 2026-08-08. It is recorded rather than silently dropped, because deleting a
// test is the kind of change that should have to justify itself.
//
// It started one child server per offset and asserted "refuse, or do not
// reissue". It was flaky in a way that wasted real time: it sampled with a fixed
// step, the dangerous offsets are the RECORD BOUNDARIES, and record sizes shift
// run to run with agent names and timestamps — so it passed 122/122 on one run,
// failed at offset 3478 on the next against identical code, and later passed
// alone while failing beside other tests. Its final form also classified the
// damage by re-opening the log AFTER the child server had already run and
// changed it, so it was judging the wrong state.
//
// Nothing was lost by removing it. The claim it was reaching for — that no
// truncation offset lets the bus rebuild a floor below what it handed out — is
// carried EXHAUSTIVELY and deterministically, at every single byte offset, by
// TestLogRepairPredicateCatchesEveryLossyTruncation, which runs in-process and
// classifies the recovery state the guard actually saw. The end-to-end wiring it
// also touched (that the refusal reaches main() and becomes exit 1) is proved by
// TestMissingSeqFloorWithADamagedLogRefusesToStart below, and the restart
// behaviour by TestSeqFloorGuardSurvivesARestart.
//
// A flaky proof is worse than no proof: it teaches whoever sees it to re-run
// until green.

// TestMissingSeqFloorWithAnIntactLogStillStarts is CASE A: the upgrade path.
//
// It is not decoration around the case-B guard, it is the constraint ON it. A
// guard that refused whenever the floor file was absent would pass every case-B
// assertion and brick every legacy data directory on upgrade — so this test is
// what stops the fix being "refuse if the file is missing".
func TestMissingSeqFloorWithAnIntactLogStillStarts(t *testing.T) {
	dir := t.TempDir()
	agent, pristineHigh := seedMintedSequences(t, dir, mintsPastABatch)

	floorPath := filepath.Join(dir, hub.SeqFloorFileName)
	if err := os.Remove(floorPath); err != nil {
		t.Fatalf("removing %s to model a pre-floor-file data directory: %v", floorPath, err)
	}

	proc := startServer(t, dir)
	addr := proc.awaitServerStarted(t)

	agent.authenticate(t, dir, addr)
	if seq := mintSeq(t, dir, addr, agent); seq <= pristineHigh {
		t.Fatalf("after losing the floor file with an INTACT log the bus reissued sequence %d, at or below the %d already handed out (invariant 1)\n%s",
			seq, pristineHigh, proc.stderr())
	}

	if _, err := os.Stat(floorPath); err != nil {
		t.Fatalf("the start that rebuilt the floor left no %s behind, so the next start would repeat the migration: %v", floorPath, err)
	}

	proc.signal(t, syscall.SIGTERM)
	if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
	}
}

// TestMissingSeqFloorWithADamagedLogRefusesToStart is CASE B: the P0.
//
// The failure message deliberately reports the REISSUED SEQUENCE rather than
// just failing, because that number IS the finding.
func TestMissingSeqFloorWithADamagedLogRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	agent, pristineHigh := seedMintedSequences(t, dir, mintsPastABatch)

	floorPath := filepath.Join(dir, hub.SeqFloorFileName)
	if err := os.Remove(floorPath); err != nil {
		t.Fatalf("removing %s to model a pre-floor-file data directory: %v", floorPath, err)
	}

	// Damage the log the same way the sweep that found this did: truncate it.
	// 512 bytes is past the header and into the records, so the file stays
	// interpretable — recovery REPAIRS it rather than giving up — which is
	// exactly the case that starts silently today.
	walPath := filepath.Join(dir, wal.WALFileName)
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat %s: %v", walPath, err)
	}
	const truncateTo = 512
	if info.Size() <= truncateTo {
		t.Fatalf("the seeded log is only %d bytes, so truncating to %d removes nothing and the test would prove nothing", info.Size(), truncateTo)
	}
	if err := os.Truncate(walPath, truncateTo); err != nil {
		t.Fatalf("truncating %s to %d: %v", walPath, truncateTo, err)
	}

	proc := startServer(t, dir)
	exited, code := exitedWithin(proc, startupTimeout)
	if !exited {
		addr := proc.awaitServerStarted(t)
		agent.authenticate(t, dir, addr)
		seq := mintSeq(t, dir, addr, agent)
		t.Fatalf("with the floor file ABSENT and the log DAMAGED the bus started anyway and minted sequence %d, "+
			"at or below the %d it had already handed out and a client may have SIGNED (invariant 1). "+
			"Reissued=%v. A correctly-implemented consumer deduplicating on message_id will DROP the new message and lose it silently.\n"+
			"The floor must not be rebuilt from a log this start has just proven incomplete.\nstderr:\n%s",
			seq, pristineHigh, seq <= pristineHigh, proc.stderr())
	}
	if code != 1 {
		t.Fatalf("exit code with an absent floor and a damaged log = %d, want 1\n%s", code, proc.stderr())
	}

	// The refusal must be actionable, and must name BOTH halves of the
	// precondition — the missing file AND the damaged log. Naming only one
	// sends the operator to fix the wrong thing.
	stderr := proc.stderr()
	for _, want := range []string{
		hub.SeqFloorFileName, // the file that is missing
		floorPath,            // where it belongs
		"was REPAIRED",       // what happened to the log
		"reissue",            // the consequence being prevented
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the refusal does not mention %q; an operator cannot act on it.\nstderr:\n%s", want, stderr)
		}
	}
}
