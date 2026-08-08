package main

// describeLogRepair: the predicate that decides whether a MISSING
// message-seq-floor file may be rebuilt from the durable log.
//
// # The question it answers
//
// hub.Open derives the message-sequence floor from three log-derived sources —
// the high-water index, the replayed "seqfloor" records and the highest replayed
// message sequence. All three assume the log is COMPLETE. When the floor FILE is
// also absent (a data directory written before that file existed), a log with
// holes in it produces a floor BELOW numbers /v1/mint already handed out, and
// the bus reissues them. Measured on a real directory: 300 sequences minted and
// signable, floor file deleted, log truncated — the bus started and minted 25.
//
// So the hub needs exactly one bit from this layer: DID RECOVERY REMOVE RECORDS
// FROM THE FILE? This function answers it, and returns the answer as the
// sentence an operator will read in the refusal, because a bare bool would make
// the refusal say "the log was damaged somehow".
//
// # What counts, and — the harder half — what deliberately does not
//
// COUNTS. Each of these means bytes that were records are no longer readable:
//
//	Quarantined       the whole file was moved aside and a fresh one started
//	Truncated         the tail was cut away
//	Rewritten         damage in the MIDDLE was dropped by copying the survivors
//	LostUnidentified  bytes went whose record indices could not even be enumerated
//
// # THE Repaired.* SIGNALS ARE ALL TRANSIENT. SAY THE CONSEQUENCE PLAINLY
//
// Every `Repaired.*` flag describes what THIS start did to the file. The repair
// is durable, so on the NEXT start there is nothing left to repair and all four
// read zero. docker-compose.yml runs the bus with `restart: unless-stopped`, so
// the automatic restart arrives within seconds: the operator sees one exit 1 and
// then a healthy bus, and a reissue can happen on start #2 unattended.
//
// PER SHAPE, MEASURED (TestSeqFloorGuardSurvivesARestart pins all three):
//
//	QUARANTINE      -> refuses on EVERY start. Not because of a Repaired.* flag,
//	                   but because a quarantine leaves an EMPTY log and the
//	                   emptied-log arm below is a property of the file, not of
//	                   this start. This is the one shape that is fully covered.
//	TRUNCATED tail  -> ONE-SHOT. Start #2 comes up.
//	INTERIOR loss   -> ONE-SHOT. Start #2 comes up.
//
// THE LAST TWO ARE A KNOWN, DOCUMENTED GAP, pinned by TestSeqFloorGuardSurvivesARestart
// rather than papered over. It is not for want of trying: TWO different durable
// arms were built and both had to be removed on 2026-08-08 because each turned
// an ordinary unclean shutdown into a PERMANENT refusal of a healthy data
// directory — see the notes on each inside describeLogRepair. Guessing at
// durability here has now cost more availability than the gap it closed.
//
// Closing it honestly needs the highest index a record actually CONSUMED, which
// wal tracks durably (its index floor's reserved/written pair) but does not
// expose on wal.Recovered. That is an internal/wal change and is reported as a
// blocker rather than approximated here for a third time.
//
// DOES NOT COUNT, and this is where a wrong call costs availability on
// directories that are actually fine:
//
//   - Dangling prepares — the ordinary signature of a crash between the prepare
//     fsync and the commit fsync. The record is still IN the file and its index
//     is still counted; recovery declined to apply it, it did not lose it.
//
//   - HeaderRepaired and Rebuilt — a rebuilt header removes no records, and
//     Rebuilt counts records RECOVERED rather than discarded. Both are the
//     repair path succeeding.
//
// The general rule, so a future edit does not have to re-derive it: this
// predicate is about bytes REMOVED FROM THE FILE, not about recovery having had
// something to say. "Replay read it and chose not to apply it" is not loss.
//
// # Failing in which direction
//
// A false POSITIVE refuses one start of a legacy directory. A false NEGATIVE
// silently reissues signed message ids. So where the two are genuinely
// ambiguous, this errs towards counting it — but not so far as to count the
// signatures of an ordinary crash, which would fire constantly and teach an
// operator to work around the guard.

import (
	"fmt"
	"strings"

	"github.com/dodgymike/agent-bus/internal/wal"
)

// describeLogRepair returns "" when recovery removed nothing from the log, and
// otherwise a description beginning "was REPAIRED", suitable for dropping into a
// sentence after the log's path.
func describeLogRepair(rec wal.Recovered) string {
	var parts []string
	if q := rec.Repaired.Quarantined; q != "" {
		parts = append(parts, fmt.Sprintf("was QUARANTINED to %s and a fresh log was started in its place, so nothing it held is readable by this bus", q))
	}
	if rec.Repaired.Truncated {
		parts = append(parts, fmt.Sprintf("had its tail TRUNCATED at offset %d, removing %d bytes", rec.Repaired.At, rec.Repaired.Removed))
	}
	if rec.Repaired.Rewritten {
		parts = append(parts, "was REWRITTEN to drop damage in the middle of the file, so the records that sat there are gone")
	}
	if rec.Repaired.LostUnidentified {
		parts = append(parts, "lost bytes whose record indices could not be enumerated at all, so how much it once held is unknown")
	}
	// MissingRecords IS NOT COUNTED, and this is the second arm removed for the
	// same reason on the same day. Do not add it back without new evidence.
	//
	// It looked like the one durable signal — an interior hole is permanent, so
	// it survives the restart that defeats every Repaired.* flag. But wal names
	// "an index range BURNED BY A RESERVATION A CRASH NEVER USED" as a cause of
	// it, and while that gap starts at the END of the file it becomes INTERIOR
	// the moment the bus writes anything after it, and then never goes away.
	//
	// Measured on a completely undamaged log: crash once, then run and stop
	// CLEANLY twice, and MissingRecords sits at 58 on every subsequent start.
	// Combined with a missing floor file — which is exactly what an operator
	// gets by following the remedy seqfloorfile.go and CONTRACTS-ONDISK.md both
	// print for a damaged floor file — the bus then refuses on EVERY start,
	// permanently, with no automated way out. That is the same permanent brick
	// the NextIndex arm was removed for, re-entering through a different door.
	//
	// Distinguishing "hole because records were lost" from "hole because a
	// reservation was burned" needs the highest index a record actually
	// CONSUMED. wal tracks it durably in the index floor's reserved/written
	// pair and even logs the difference, but does not expose it on
	// wal.Recovered. Until it does, there is no honest durable signal here.

	// THE EMPTIED LOG. Narrow on purpose, and NOT the general accounting arm
	// removed below.
	//
	// It is also the ONLY arm that is a property of the FILE rather than of this
	// start, so it is the only one that survives a restart — which is why a
	// QUARANTINE (whose whole point is that it leaves an empty log behind) is
	// refused on every start, while a truncation or an interior loss is not.
	//
	// A log truncated to zero bytes is treated by wal exactly like a file that
	// does not exist: no repair flags, no discards, no missing records. Nothing
	// reports it, yet the durable index floor still says this directory
	// authorised indices, so every record it held is gone. Measured: without
	// this, offset 0 starts and mints 25 against 300 handed out.
	//
	// Why this does NOT reproduce the false positive that killed the general
	// arm: that arm fired whenever the file's reach fell short of the
	// FLOOR-RAISED NextIndex, which an ordinary unclean shutdown causes by
	// burning an index block while the log still holds all its records. This
	// requires the log to hold NO RECORDS AT ALL. A crashed bus that had done
	// any work has records, so it cannot trip this — verified against the exact
	// five-mints-then-SIGKILL reproduction.
	//
	// The residual false positive is a LEGACY directory whose log is genuinely
	// empty and which was killed with a reservation outstanding. It is accepted:
	// such a directory has minted nothing, and since ensureExists writes the
	// floor file on every start, any directory this binary has opened is exempt.
	if rec.Records == 0 && rec.NextIndex > 1 {
		parts = append(parts, fmt.Sprintf("holds NO records at all, yet this data directory has durably authorised indices up to %d — so everything it once held is gone, and nothing about the recovery itself reports that", rec.NextIndex-1))
	}

	// REMOVED 2026-08-08 — AN "UNACCOUNTED-FOR INDICES" ARM USED TO LIVE HERE,
	// AND IT BRICKED HEALTHY DATA DIRECTORIES. Do not reinstate it in this form.
	//
	// It compared highestIndexSeen(rec) against rec.NextIndex-1 and reported a
	// shortfall as lost records. That is wrong, because rec.NextIndex from
	// Log.Recovered is the FLOOR-RAISED value, not the file's own high-water
	// mark (internal/wal/replay.go documents this explicitly). Indices are
	// authorised in BLOCKS, so any unclean shutdown legitimately leaves up to a
	// block of authorised-but-never-used indices, and the arm read every one of
	// them as loss.
	//
	// Reproduced end to end: a healthy bus, five mints, SIGKILL, then the
	// remedy that seqfloorfile.go and CONTRACTS-ONDISK.md BOTH print for a
	// damaged floor file ("move it aside and restart") — exit 1 on start #1, #2
	// and #3, with the log byte-identical and undamaged throughout. Following
	// our own documentation permanently bricked the directory. The refusal
	// returns before ensureExists, so no start can ever migrate it out of that
	// state.
	//
	// WHAT A CORRECT VERSION NEEDS, and why it is not here: the reference has to
	// be the highest index a record actually CONSUMED, not the highest
	// AUTHORISED. wal's durable index floor already tracks both (its
	// reserved/written pair), and wal computes the difference internally — it
	// logs indices_skipped — but neither value is exposed on wal.Recovered.
	// Adding that field is an internal/wal change, which is outside this task's
	// boundary, so it is reported as a blocker rather than guessed at here.
	//
	// CONSEQUENCE, STATED HONESTLY SO IT IS NOT FORGOTTEN: without this arm the
	// guard is ONE-SHOT for a truncated tail and for an interior loss. Both
	// leave only transient Repaired.* flags, so the start that performs the
	// repair refuses and the NEXT start comes up. A QUARANTINE is the exception
	// and is covered on every start, because it leaves an empty log and the
	// emptied-log arm reads the FILE rather than this start's actions. See
	// TestSeqFloorGuardSurvivesARestart, which pins all three.
	//
	// # The measurement that forced it
	//
	// Sweeping EVERY byte offset of a 4491-byte log: 23 offsets produce a file
	// that recovery calls perfectly clean — Truncated false, LostUnidentified
	// false, no quarantine, records present. Those 23 are exactly the RECORD
	// BOUNDARIES. A log cut precisely at a boundary is indistinguishable, from
	// the log alone, from a log that simply ended there; there is no torn record
	// to notice and nothing to repair. Truncating to ZERO is the same failure at
	// the other end: wal treats a zero-length file like a file that does not
	// exist, which is right for wal and silent for us.
	//
	// At one of those boundaries the bus started and minted 257 against a
	// pristine high-water mark of 300 — a clean-looking log, and a 44-number
	// reissue. A sampling sweep found it only by luck, which is precisely why
	// the predicate must not depend on recovery having noticed something.
	//
	// # What makes it detectable at all
	//
	// The DURABLE INDEX FLOOR lives outside the log (wal-index-floor) and so
	// survives any damage to it, and wal folds it into NextIndex. So NextIndex-1
	// is what this data directory has AUTHORISED, while the records still in the
	// file are what SURVIVED. When the file cannot account for the authorised
	// range, records are gone — whatever recovery thought of the file it read.
	//
	// # The false positive that killed it, stated plainly (PAST TENSE — this
	// describes the REMOVED arm, not anything below)
	//
	// Indices are authorised in BLOCKS, so an unclean shutdown legitimately
	// left up to a block of authorised-but-never-used indices, and the removed
	// arm fired on them. That cost was ARGUED to be acceptable on the grounds
	// that only a data directory with no floor file could pay it — and the
	// argument was WRONG in practice, because the documented remedy for a
	// damaged floor file produces exactly that directory. The refusal was
	// permanent, not "exactly once".
	//
	// The underlying difficulty is real and remains: nothing wal exposes today
	// distinguishes "burned and unused" from "used and then lost". That is why
	// the answer is a blocker on wal rather than a cleverer predicate here.
	if len(parts) == 0 {
		return ""
	}
	// "was REPAIRED" is the fixed lead-in. The refusal reads "the durable log
	// was REPAIRED on this start: …", and tests pin that phrase rather than the
	// variable detail behind it.
	return fmt.Sprintf("was REPAIRED on this start (%d records, %d bytes discarded): it %s",
		rec.Repaired.DiscardCount, rec.Repaired.DiscardedBytes, strings.Join(parts, "; and it "))
}
