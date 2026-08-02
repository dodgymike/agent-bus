package wal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// Repair is the outcome of a RepairLog pass: what, if anything, recovery had to
// remove or rebuild before the log could be replayed.
//
// EVERY field here is also written to the operator log by RepairLog. That is
// deliberate and it is the contract: after the 2026-08-02 policy change
// recovery is ALLOWED to discard damaged records, so the thing that keeps the
// system honest is no longer "we never discard" -- it is "we never discard
// SILENTLY". A discard that does not appear in the log is a bug, and there are
// tests that fail if one does not.
type Repair struct {
	// Path is the file that was examined.
	Path string

	// Truncated reports that the file was SHORTENED -- damage at the very end
	// was cut away. It is false when the file needed no repair, which is the
	// overwhelmingly common case, and also when the repair was a rewrite rather
	// than a cut (see Rewritten).
	Truncated bool

	// At is the offset the file was truncated to: the end of the last surviving
	// record, and therefore the offset the next append writes at. It is
	// meaningful only when Truncated.
	At int64

	// Removed is how many bytes that truncation discarded.
	Removed int64

	// NextIndex is the index the next append will use after the repair, which
	// is one past the highest index that SURVIVED.
	//
	// ---------------------------------------------------------------------
	// WHAT REISSUING THAT INDEX DOES AND DOES NOT PROMISE (invariant 1, "ids
	// are never reused, including across restarts").
	//
	// This comment previously claimed that an index is "only ever discarded
	// when its fsync provably never completed". THAT CLAIM WAS FALSE, both
	// reviewer and security said so, and it is now false by POLICY as well as
	// in fact: recovery is required to discard damaged records so that the bus
	// always restarts, and a record damaged by media rot may well have been
	// fsynced and acknowledged. So here is the honest, narrower statement.
	//
	// WHAT IS STILL TRUE, and is the property invariant 1 actually protects:
	// ids that were OBSERVED are never reissued for as long as the records
	// carrying them survive. Writer.Append publishes an index only after the
	// frame is fsynced, nothing in this system is acknowledged before Append
	// returns (invariant 4), and every surviving record keeps its original
	// index through a repair -- rewriteLog never renumbers survivors, which is
	// why a repaired log has HOLES rather than a dense sequence. So the normal
	// crash case (a torn final frame whose Append never returned) reissues an
	// index that nothing ever saw, and that is safe.
	//
	// WHAT IS NO LONGER PROMISED: if the LAST record in the file is damaged,
	// recovery cannot tell "a write that was interrupted" from "a write that
	// completed, was acknowledged, and then had a bit flipped" -- the two are
	// byte-indistinguishable, and no check inside this format can separate
	// them. That record is discarded and its index is reissued. If it had been
	// acknowledged, an id a client saw is handed out again. That is a real,
	// accepted consequence of the availability-over-retention decision
	// (DECISIONS.md, 2026-08-02): the alternative is a bus that will not start,
	// and the user chose the restart. It is bounded to the tail, it is logged
	// at ERROR with the offset, index and length, and it does not apply to any
	// record with a surviving record behind it, because damage no longer
	// cascades (see resyncFrom).
	//
	// This package does not mint application ids; internal/ids does, from the
	// recovered high-water mark. Nothing there changes.
	// ---------------------------------------------------------------------
	NextIndex uint64

	// Reason is why the tail went, in the same words the log carries.
	Reason string

	// Rewritten reports that surviving records had to be copied into a new file
	// -- because damage sat in the MIDDLE of the log, or the file header needed
	// rebuilding, or a record's length field was restored. Bytes cannot be
	// removed from the middle of a file in place, so this is the only way to
	// discard mid-file damage without deleting the intact records behind it.
	Rewritten bool

	// HeaderRepaired reports a file header that did not verify and was rewritten
	// from the constant this code would have written anyway.
	HeaderRepaired bool

	// Quarantined is the path the original file was MOVED to when it could not
	// be interpreted at all and startup continued with a fresh log. It is ""
	// normally. The file is renamed, never deleted: an operator is owed the
	// bytes even when this code can make nothing of them.
	Quarantined string

	// Rebuilt is how many records were RECOVERED rather than discarded, because
	// their own checksum proved that only their length field was damaged.
	Rebuilt uint64

	// Kept is how many records survived the repair.
	Kept uint64

	// Discards is the detail of what was thrown away, capped at
	// maxDiscardsRetained entries so a file that is damage from end to end
	// cannot make recovery hold it all in memory. DiscardCount and
	// DiscardedBytes are EXACT regardless of the cap.
	Discards       []Discard
	DiscardCount   int
	DiscardedBytes int64

	// Exhausted reports that a forward search for the next intact record ran
	// out of its work budget, so records may have been discarded without proof
	// that they were unreadable. It is the one remaining way damage can cascade
	// and it is logged at ERROR.
	Exhausted bool
}

// TailRepair is the former name of Repair, from when the only thing recovery
// could remove was a torn tail. It is kept as an alias so existing callers and
// tests keep compiling; new code should say Repair.
type TailRepair = Repair

// RepairTail is the former name of RepairLog, from when the only damage
// recovery would act on was a torn tail. It is kept so existing callers keep
// compiling and behaves identically; new code should call RepairLog.
func RepairTail(path string, kind Kind, logger *logging.Logger) (TailRepair, error) {
	return RepairLog(path, kind, logger)
}

// RepairLog makes a log READABLE, discarding whatever damage stands in the way
// and logging every byte it discards.
//
// It runs BEFORE Replay (see Open) and it is the only place in this package
// that ever removes bytes from a log.
//
// # The policy this implements, and how it changed
//
// Until 2026-08-02 this function refused to start the server on most damage: it
// cut a tail only when two checks could PROVE the damage was a torn write, and
// returned an error otherwise, on the reasoning that an operator can recover
// from a server that will not boot and cannot recover from one that quietly
// deleted records. The user reversed that (DECISIONS.md, "Availability over
// retention"): "always be able to restart, prefer to discard messages and/or
// corruption, with logging".
//
// So the rule is now:
//
//	DAMAGE IS NEVER FATAL. NOT BEING ABLE TO READ THE FILE STILL IS.
//
// Damage -- a torn frame, a flipped bit, a lost sector, a payload that no
// longer decodes, a corrupt file header -- is discarded and logged, and
// recovery continues to a running server. Being unable to read the file at all
// -- permission denied, an I/O error from the device, an audit file where a WAL
// was expected, a format version this binary does not implement -- is still an
// error, because none of those are damage and "repairing" them would destroy a
// file that is probably intact.
//
// # Damage does not cascade
//
// Discarding the DAMAGED record is sanctioned. Deleting later records that are
// themselves intact is not, and that distinction is the whole of the difference
// between this function and the one it replaces. After damage, recovery
// searches FORWARD for the next intact record by RECORD INDEX (resyncFrom) and
// resumes there. Anchoring that search on the index rather than on the end of
// the file is not a detail: an end-of-file anchor only ever fires when the file
// ends exactly on a record boundary, which is precisely the case recovery does
// not exist for, and a reviewer's probe showed one flipped bit in a mid-file
// length field deleting eight committed records because of it.
//
// # What it does to the file
//
//   - Nothing, when the file scans clean. That is the fast path and it is the
//     normal one.
//   - A TRUNCATION, when the only damage runs to the end of the file. Fsynced.
//   - A REWRITE, when damage sits in the middle, or the file header needs
//     rebuilding, or a record's length field was restored: surviving frames are
//     copied to a temporary file and renamed over the original, atomically.
//     Survivors keep their original indices, so a repaired log has HOLES in its
//     index sequence -- renumbering would reuse ids and is never done.
//   - A QUARANTINE, when the file cannot be interpreted at all and nothing can
//     be salvaged from it: it is RENAMED aside and startup continues with a
//     fresh log. Renamed, never deleted.
//
// Invariant 6's append-only rule now has exactly these exceptions, and the
// decision that widened it from "a verified-corrupt tail" to "damaged records
// anywhere" is recorded and dated.
func RepairLog(path string, kind Kind, logger *logging.Logger) (Repair, error) {
	res := Repair{Path: path}

	// Checked BEFORE the file is touched. A caller that does not know what kind
	// of file this is must never reach the rewrite, which would otherwise stamp
	// a meaningless magic into the header it writes.
	if kind.magic() == "" {
		return res, fmt.Errorf("wal: repair %s: %w: %s", path, ErrUnknownKind, kind)
	}

	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil // nothing to repair
		}
		// NOT damage: the file could not be examined at all.
		return res, fmt.Errorf("wal: stat %s: %w", path, err)
	}
	if fi.Size() == 0 {
		// The crash window between creating the file and writing its header. It
		// provably holds no record, and OpenWriter heals it.
		return res, nil
	}

	if _, _, _, err := scanFraming(path, kind); err == nil {
		return res, nil // the file is well framed end to end
	}

	plan, err := salvage(path, kind, nil)
	if err != nil {
		return res, err // only the "cannot read this file" class reaches here
	}

	// A file whose header does not verify AND from which not one record can be
	// salvaged is not a log this code can make anything of. Move it aside and
	// start fresh, rather than refuse to boot for ever.
	if plan.HeaderDamaged && !plan.Salvageable {
		dest, qerr := quarantine(path)
		if qerr != nil {
			return res, qerr // renaming failed: a filesystem problem, not damage
		}
		res.Quarantined = dest
		res.DiscardCount = 1
		res.DiscardedBytes = plan.Size
		res.Discards = []Discard{{Stage: "framing", Offset: 0, Length: plan.Size,
			Reason: "the file header does not verify and no intact record could be found anywhere in the file"}}
		logger.Error("wal quarantined an unreadable log and started a fresh one",
			"path", path, "moved_to", dest, "bytes", plan.Size)
		return res, nil
	}

	res.Kept = plan.Kept
	res.Rebuilt = plan.Rebuilt
	res.HeaderRepaired = plan.HeaderDamaged
	res.Exhausted = plan.Exhausted
	res.Discards = plan.Discards
	res.DiscardCount = plan.Count
	res.DiscardedBytes = plan.Bytes
	res.NextIndex = plan.LastIndex + 1

	// EVERY discard is logged BEFORE the file is changed, so that a crash during
	// the repair still leaves the operator a record of what was about to go.
	logDiscards(logger, path, plan.Discards, plan.Count)
	if plan.Exhausted {
		logger.Error("wal gave up searching for intact records after damage and discarded the rest of the log",
			"path", path, "candidates_budget", maxResyncCandidates, "checksum_bytes_budget", maxResyncChecksumBytes)
	}
	if plan.HeaderDamaged {
		logger.Error("wal rebuilding a damaged file header",
			"path", path, "kind", kind, "records_salvaged", plan.Kept)
	}
	if plan.Rebuilt > 0 {
		logger.Warn("wal restored records whose length field was corrupt but whose checksum proved them complete",
			"path", path, "records", plan.Rebuilt)
	}

	switch {
	case plan.needsRewrite():
		before := plan.Size
		after, err := rewriteLog(path, kind)
		if err != nil {
			return res, err
		}
		res.Rewritten = true
		res.Removed = before - sizeOf(path)
		res.Reason = firstReason(plan.Discards)
		logger.Warn("wal rewrote a damaged log, keeping every intact record",
			"path", path, "kept", after.Kept, "rebuilt", after.Rebuilt,
			"discards", plan.Count, "discarded_bytes", plan.Bytes,
			"was_bytes", before, "now_bytes", sizeOf(path), "next_index", res.NextIndex)

	case plan.TailAt >= 0:
		at := plan.TailAt
		res.Reason = firstReason(plan.Discards)
		if err := truncateAt(path, at); err != nil {
			return res, err
		}
		res.Truncated = true
		res.At = at
		res.Removed = plan.Size - at
		logger.Warn("wal truncated damage at the end of the log",
			"path", path, "at", at, "removed", res.Removed, "next_index", res.NextIndex,
			"reason", res.Reason)

	default:
		// scanFraming failed but the salvage walk found nothing to remove.
		// Unreachable unless the two disagree, which would be a bug in this
		// package rather than damage in the file -- so say so plainly instead of
		// starting on a file nothing has verified.
		return res, fmt.Errorf("wal: repair %s: the file does not scan but the salvage pass found nothing to discard; this is a bug in recovery, not damage in the file, and it will not be repaired blindly", path)
	}

	// Prove the result rather than assume it. The repair only ever writes frames
	// whose checksum this code verified, so a failure here means the repair
	// itself is broken -- and the answer to that is NOT to repair again, which
	// would happily eat a log one pass at a time.
	if _, _, _, err := scanFraming(path, kind); err != nil {
		return res, fmt.Errorf("wal: repair %s: the repaired log is still not readable; recovery will not repair it a second time and it needs operator inspection: %w", path, err)
	}
	return res, nil
}

// maxDiscardsLogged bounds how many discards are named individually in the log.
// The COUNT is always exact and always logged; a file that is damage from end to
// end must not be able to turn one restart into a hundred thousand log lines.
const maxDiscardsLogged = 16

// logDiscards writes one record per discard, at ERROR when what was lost was
// acknowledged (a commit record) or is unidentifiable, and WARN otherwise. The
// total is always emitted, so a capped list never hides how much went.
func logDiscards(logger *logging.Logger, path string, discards []Discard, total int) {
	for i, d := range discards {
		if i == maxDiscardsLogged {
			break
		}
		kv := []interface{}{
			"path", path, "stage", d.Stage, "offset", d.Offset, "bytes", d.Length,
			"record_index", d.indexLabel(), "record_type", d.typeLabel(), "reason", d.Reason,
		}
		if d.severe() {
			logger.Error("wal discarded a damaged record", kv...)
		} else {
			logger.Warn("wal discarded a damaged record", kv...)
		}
	}
	if total > 0 {
		logged := len(discards)
		if logged > maxDiscardsLogged {
			logged = maxDiscardsLogged
		}
		logger.Warn("wal recovery discarded damaged regions", "path", path,
			"discards", total, "logged_individually", logged)
	}
}

// firstReason returns the reason of the first discard, for the Repair summary.
func firstReason(discards []Discard) string {
	if len(discards) == 0 {
		return ""
	}
	return discards[0].Reason
}

// sizeOf reports a file's size, or 0 if it cannot be stat'd. It is used only
// for log lines, never for a decision.
func sizeOf(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// scanFraming walks path as a whole file of the given kind and reports how many
// records it accepted, the offset just past the last of them, and the file's
// size. The file is opened and CLOSED here, so no descriptor is held across a
// truncate.
//
// The size is taken from THIS descriptor and AFTER the scan, not from a stat
// taken before it, so that a file which GREW during the scan reports the larger
// size.
//
// This narrows a window; it does not close one. There is no lock on the data
// directory (see the note in Open), so a second process writing to the same log
// can still interleave with the whole repair. Excluding that needs a real
// directory lock, and this ordering is only the cheap half of the answer.
func scanFraming(path string, kind Kind) (records uint64, end, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("wal: open %s: %w", path, err)
	}
	defer f.Close()

	end, scanErr := scanFrom(f, path, kind, func(Record) error {
		records++
		return nil
	})

	fi, err := f.Stat()
	if err != nil {
		return records, end, 0, fmt.Errorf("wal: stat %s: %w", path, err)
	}
	return records, end, fi.Size(), scanErr
}

// truncateAt shortens path to at and makes that durable. The fsync is not
// optional: a truncation that is not fsynced can be lost by the next crash, and
// the torn bytes would come back -- turning a repaired log into a log that
// needs repairing again, but now possibly with records appended after the
// damage. The parent directory is synced too, for the same reason initFile
// syncs it: the metadata change has to survive as well as the data.
func truncateAt(path string, at int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, fileMode)
	if err != nil {
		return fmt.Errorf("wal: repair %s: open to truncate at offset %d: %w", path, at, err)
	}
	if err := f.Truncate(at); err != nil {
		f.Close()
		return fmt.Errorf("wal: repair %s: truncate to offset %d: %w", path, at, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("wal: repair %s: fsync after truncating to offset %d: %w", path, at, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("wal: repair %s: close after truncating to offset %d: %w", path, at, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("wal: repair %s: fsync directory after truncating to offset %d: %w", path, at, err)
	}
	return nil
}
