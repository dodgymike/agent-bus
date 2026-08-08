package main

// Is the sequence-floor guard ONE-SHOT? This file exists to answer that, because
// a guard that fires only on the start that PERFORMS the repair is not a guard
// at all on the documented runtime target.
//
// # Why the question is not academic
//
// docker-compose.yml carries `restart: unless-stopped`. So a bus that exits 1 is
// restarted by Docker within seconds, unattended. If the refusal depends on a
// TRANSIENT signal — a `Repaired.*` flag that is set on the start that does the
// repairing and zero on every start after it — then the operator sees one exit 1
// and then a healthy bus, and the reissue happens on start #2 with nobody
// watching. "It refused once" is worth nothing against an automatic restarter.
//
// # The answer, per damage shape, measured rather than argued
//
// The guard survives a restart only where the loss leaves a trace that OUTLIVES
// the repair. The answer therefore depends on the damage shape, and this test pins all three rather
// than hiding the gap:
//
//	QUARANTINE      -> refuses on EVERY start. A quarantine leaves an EMPTY log,
//	                   and the emptied-log arm reads the FILE rather than what
//	                   this start did to it, so it does not wash out.
//	TRUNCATED tail  -> ONE-SHOT. Start #2 comes up.
//	INTERIOR loss   -> ONE-SHOT. Start #2 comes up.
//
// The two one-shot shapes are not for want of trying. TWO durable arms were
// built to cover them and both were removed on
// 2026-08-08, because each turned an ordinary unclean shutdown into a PERMANENT
// refusal of a perfectly healthy data directory:
//
//   - comparing the file's reach against the FLOOR-RAISED NextIndex: indices are
//     authorised in BLOCKS, so any unclean shutdown leaves authorised-but-unused
//     indices that this read as loss.
//   - counting MissingRecords: a burned block starts at the END of the file but
//     becomes an INTERIOR hole as soon as the bus writes past it, and then never
//     clears. Measured on an undamaged log: crash once, run cleanly twice, and
//     MissingRecords sits at 58 for ever.
//
// Both bricked the directory an operator gets by following the remedy that
// seqfloorfile.go and CONTRACTS-ONDISK.md print for a damaged floor file ("move
// it aside and restart") — refusing on every start with no automated way out.
//
// Closing the gap honestly needs the highest index a record actually CONSUMED.
// wal tracks it durably (its index floor's reserved/written pair) and logs the
// difference, but does not expose it on wal.Recovered — an internal/wal change,
// outside this task's boundary, reported as a blocker.
//
// So each subtest restarts and asserts the CURRENT truth explicitly. The day wal
// exposes that value, these subtests FAIL and say exactly what to update. A
// reminder, not an endorsement.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// mustRefuseToStart runs the server against dir and requires exit 1, returning
// its stderr. It fails the test if the bus comes up instead.
func mustRefuseToStart(t *testing.T, dir, why string) string {
	t.Helper()
	proc := startServer(t, dir)
	exited, code := exitedWithin(proc, startupTimeout)
	if !exited {
		addr := proc.awaitServerStarted(t)
		probe := enrolNewAgent(t, dir, addr, "restart-probe")
		probe.authenticate(t, dir, addr)
		seq := mintSeq(t, dir, addr, probe)
		t.Fatalf("%s: the bus STARTED and minted sequence %d. On the documented runtime target docker-compose restarts the bus automatically, so a guard that only fires once is silently bypassed within seconds (invariant 1).\nstderr:\n%s",
			why, seq, proc.stderr())
	}
	if code != 1 {
		t.Fatalf("%s: exit code %d, want 1\n%s", why, code, proc.stderr())
	}
	return proc.stderr()
}

// TestSeqFloorGuardSurvivesARestart is the claim: the refusal is a PROPERTY OF
// THE DATA DIRECTORY, not an event that happens once.
func TestSeqFloorGuardSurvivesARestart(t *testing.T) {
	damage := []struct {
		name string
		// apply damages the seeded log in place.
		apply func(t *testing.T, walPath string, size int64)
		// survivesRestart records whether the guard still fires on start #2.
		//
		// It is FALSE for the truncated tail, and that is a KNOWN GAP being
		// pinned rather than a property being claimed — see the note in
		// describeLogRepair. A truncation leaves no durable trace: the repair
		// is done after start #1, and detecting it afterwards needs the highest
		// index a record actually CONSUMED, which wal tracks in its index
		// floor's reserved/written pair but does not expose on wal.Recovered.
		//
		// The arm that used to cover it compared against the FLOOR-RAISED
		// NextIndex, which made every unclean shutdown look like data loss and
		// PERMANENTLY bricked healthy directories — including any operator
		// following the documented "move the floor file aside and restart"
		// remedy. Removing it reopened this gap deliberately, in the direction
		// that fails safe for availability.
		//
		// Pinning it false means the day wal exposes that value this subtest
		// FAILS, which is the point: it is a reminder, not an endorsement.
		survivesRestart bool
	}{
		{
			// The tail is cut away. Recovery repairs it on start #1 and the
			// file is clean from then on, so the refusal does not repeat.
			name:            "truncated tail",
			survivesRestart: false,
			apply: func(t *testing.T, walPath string, size int64) {
				if err := os.Truncate(walPath, size/2); err != nil {
					t.Fatalf("truncating: %v", err)
				}
			},
		},
		{
			// Damage in the MIDDLE, which recovery fixes by rewriting the file
			// around the hole. This is the shape whose only durable trace is the
			// hole itself: the last index is untouched, so nothing about the
			// file's END reveals the loss on any later start.
			name:            "interior loss",
			survivesRestart: false,
			apply: func(t *testing.T, walPath string, size int64) {
				// DETERMINISTIC, and it has to be. Corrupting a guessed byte
				// offset like size/2 lands inside a different record depending
				// on record sizes that shift with agent names and timestamps,
				// so the SAME fixture sometimes produced a mid-file rewrite and
				// sometimes a tail truncation — a flaky test that would fail
				// roughly one run in ten while the code was fine. The record
				// boundaries are readable, so they are read rather than guessed.
				recs, _, err := wal.ScanAll(walPath, wal.KindWAL)
				if err != nil {
					t.Fatalf("scanning the seeded log to find a middle record: %v", err)
				}
				if len(recs) < 5 {
					t.Fatalf("the seeded log holds only %d records; too few to damage the MIDDLE and leave intact records on both sides", len(recs))
				}
				// A record squarely in the middle, so intact records remain on
				// BOTH sides and recovery must rewrite around the hole instead
				// of simply cutting the tail.
				victim := recs[len(recs)/2]
				f, err := os.OpenFile(walPath, os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatalf("opening the log to corrupt a middle record: %v", err)
				}
				defer f.Close()
				// Land inside the frame's payload rather than on its header, so
				// the frame still parses and the damage is a CONTENT failure.
				if _, err := f.WriteAt([]byte(strings.Repeat("X", 16)), victim.Offset+8); err != nil {
					t.Fatalf("corrupting record %d at offset %d: %v", victim.Index, victim.Offset, err)
				}
			},
		},
		{
			// The whole file replaced with bytes recovery can make nothing of, so
			// it is QUARANTINED and a fresh, empty log is started. Unlike the two
			// above this one IS covered on every start: the emptied-log arm reads
			// the file's own state rather than what this start did to it.
			name:            "quarantine",
			survivesRestart: true,
			apply: func(t *testing.T, walPath string, size int64) {
				if err := os.WriteFile(walPath, []byte(strings.Repeat("X", 64)), 0o600); err != nil {
					t.Fatalf("replacing the log with unreadable bytes: %v", err)
				}
			},
		},
	}

	for _, d := range damage {
		d := d
		t.Run(d.name, func(t *testing.T) {
			dir := t.TempDir()
			_, pristineHigh := seedMintedSequences(t, dir, mintsPastABatch)
			t.Logf("seeded: %d sequences handed out", pristineHigh)

			// Model a data directory written before the floor file existed.
			if err := os.Remove(filepath.Join(dir, hub.SeqFloorFileName)); err != nil {
				t.Fatalf("removing the floor file: %v", err)
			}
			walPath := filepath.Join(dir, wal.WALFileName)
			info, err := os.Stat(walPath)
			if err != nil {
				t.Fatalf("stat %s: %v", walPath, err)
			}
			d.apply(t, walPath, info.Size())

			// START #1 — the start that performs the repair.
			first := mustRefuseToStart(t, dir, "start #1 (performs the repair)")

			if !d.survivesRestart {
				// THE KNOWN GAP, ASSERTED EXPLICITLY. The bus comes up on the
				// automatic restart, and on the documented runtime target
				// (docker-compose `restart: unless-stopped`) that happens within
				// seconds, unattended. Asserting it keeps the gap visible and
				// makes this test fail the moment it is closed.
				p2 := startServer(t, dir)
				exited, _ := exitedWithin(p2, startupTimeout)
				if exited {
					t.Fatalf("start #2 refused after a %s. That is BETTER than expected — the one-shot gap appears to be closed, so update survivesRestart and the note in describeLogRepair.\n%s", d.name, p2.stderr())
				}
				addr := p2.awaitServerStarted(t)
				probe := enrolNewAgent(t, dir, addr, "gap-probe")
				probe.authenticate(t, dir, addr)
				t.Logf("KNOWN GAP (%s): start #2 came up and minted sequence %d against %d handed out; blocked on wal exposing the highest CONSUMED index",
					d.name, mintSeq(t, dir, addr, probe), pristineHigh)
				p2.signal(t, syscall.SIGTERM)
				p2.awaitExit(t, shutdownTimeout)
				return
			}

			// START #2 — the automatic restart. THE REPAIR IS ALREADY DONE, so
			// every transient Repaired.* flag is now zero. This is the start
			// that matters.
			second := mustRefuseToStart(t, dir, "start #2 (the automatic restart, after the repair is durable)")

			// And a third, because "it refused twice" could still be a
			// two-start effect; a guard on the directory's STATE has no
			// countdown in it at all.
			mustRefuseToStart(t, dir, "start #3")

			for _, want := range []string{hub.SeqFloorFileName, "reissue"} {
				if !strings.Contains(second, want) {
					t.Fatalf("the restart refusal does not mention %q:\n%s", want, second)
				}
			}
			// The refusal must tell an operator NOT to do the obvious thing.
			// Without this line the documented remedy list reads as "try these
			// three things", and restarting is what anyone tries first.
			if !strings.Contains(second, "DO NOT SIMPLY RESTART") {
				t.Fatalf("the refusal never tells the operator NOT to just restart, which is the one thing they will otherwise do (and which docker-compose does for them):\n%s", second)
			}

			// The floor file must still be ABSENT. A refusal that had written
			// it would let start #2 through by making the directory look
			// migrated, which is the one-shot failure wearing a different hat.
			if _, err := os.Stat(filepath.Join(dir, hub.SeqFloorFileName)); err == nil {
				t.Fatalf("the refusal CREATED %s; the next start would then see a migrated directory and proceed over the same unproven floor", hub.SeqFloorFileName)
			}
			if !strings.Contains(first, hub.SeqFloorFileName) {
				t.Fatalf("start #1 refusal does not name the floor file:\n%s", first)
			}
		})
	}
}

// TestUncleanShutdownWithNoFloorFileStillStarts is the ENFORCEMENT of the
// lesson, and it exists because nothing else in the tree pinned it.
//
// Two separate "durable" arms of describeLogRepair were built and removed on
// 2026-08-08, and BOTH failed the same way: they read the index reservation that
// an unclean shutdown legitimately burns as though it were lost data, and so
// PERMANENTLY refused to start a perfectly healthy data directory. Both were
// reachable by following the remedy this project's own docs print for a damaged
// floor file ("move it aside and restart").
//
// Every other test here uses SIGTERM, so a re-added arm of either kind would
// pass the whole suite green. This one kills the bus and then removes the floor
// file — exactly the shape both defects needed — and requires the bus to COME
// UP. It is the cheap check that stops a third rebuild.
//
// It deliberately asserts availability rather than refusal. The refusal cases
// are covered above; what is fragile, twice proven, is the false positive.
func TestUncleanShutdownWithNoFloorFileStillStarts(t *testing.T) {
	dir := t.TempDir()

	// A crash, then CLEAN runs that write past the burned reservation. The
	// second half matters: the burned block only becomes an INTERIOR hole once
	// the bus appends after it, and that interior hole is what the second
	// removed arm mistook for loss. A test that stopped after the crash would
	// miss it.
	for i, kill := range []bool{true, false, false} {
		proc := startServer(t, dir)
		addr := proc.awaitServerStarted(t)
		agent := enrolNewAgent(t, dir, addr, fmt.Sprintf("unclean-%d", i))
		agent.authenticate(t, dir, addr)
		mintSeq(t, dir, addr, agent)
		if kill {
			proc.signal(t, syscall.SIGKILL)
			proc.awaitExit(t, shutdownTimeout)
			continue
		}
		proc.signal(t, syscall.SIGTERM)
		if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
			t.Fatalf("clean shutdown %d exited %d\n%s", i, code, proc.stderr())
		}
	}

	// The remedy seqfloorfile.go and CONTRACTS-ONDISK.md both print.
	floorPath := filepath.Join(dir, hub.SeqFloorFileName)
	if err := os.Remove(floorPath); err != nil {
		t.Fatalf("removing %s (the documented remedy): %v", floorPath, err)
	}

	// Twice, because the failure being guarded against was PERMANENT: a
	// one-start check would pass against an arm that refused for ever.
	for i := 1; i <= 2; i++ {
		proc := startServer(t, dir)
		exited, code := exitedWithin(proc, startupTimeout)
		if exited {
			t.Fatalf("start #%d REFUSED (exit %d) on an UNDAMAGED log. The data directory is healthy: it was killed once, ran cleanly twice, and its floor file was removed exactly as this project's own documentation instructs. "+
				"Some predicate is reading an unclean shutdown's burned index reservation as lost records — that is the defect removed twice on 2026-08-08, and it permanently bricks real deployments.\n%s",
				i, code, proc.stderr())
		}
		proc.awaitServerStarted(t)
		proc.signal(t, syscall.SIGTERM)
		if c := proc.awaitExit(t, shutdownTimeout); c != 0 {
			t.Fatalf("start #%d exited %d on shutdown\n%s", i, c, proc.stderr())
		}
	}
}
