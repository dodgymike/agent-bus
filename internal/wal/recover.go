package wal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// Repair is the outcome of a RepairLog pass: what, if anything, recovery had to
// remove or rebuild before the log could be replayed.
//
// Every LOSS described here is also written to the operator log by RepairLog.
// That is the contract: after the 2026-08-02 policy change recovery is ALLOWED
// to discard damaged records, so the thing that keeps the system honest is no
// longer "we never discard" -- it is "we never discard SILENTLY". A discard that
// does not appear in the log is a bug, and there are tests that fail if one
// does not.
//
// Not every FIELD is logged, and the difference matters to anyone reading this
// to find out what an operator will see: Kept and Rebuilt appear only on the
// paths that produce them (a rewrite, and a length-field repair respectively),
// and the quarantine path returns early with only Quarantined, DiscardCount and
// DiscardedBytes set -- its own ERROR line carries the rest. The exact discard
// COUNT and BYTE TOTAL are emitted on every repair path without exception.
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
	// IT IS ONLY MEANINGFUL WHEN A REPAIR HAPPENED. On the paths where nothing
	// was repaired -- a clean file, a file that does not exist, a zero-length
	// file -- it is left 0 and the writer establishes the real value itself. The
	// quarantine path is the one exception that reports a value without a
	// repair: it sets 1, because a fresh log genuinely does start there.
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
// # Damage does not cascade, with ONE bounded exception
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
// THE EXCEPTION, named here rather than left to be discovered in a field doc:
// the forward search has a work budget, because the bytes it walks are
// attacker-influenced. If a region is so dense with frame-like headers that the
// budget runs out, the search gives up and everything from the damage to the
// end of the file is discarded -- a cascade, without proof that any of it was
// unreadable. It is reported in Repair.Exhausted and logged at ERROR twice (the
// discard itself, marked Severe, and a separate line saying the search was
// abandoned), and it is the ONLY path by which one damaged record can still
// cost an intact one.
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
	// Checked BEFORE the file is touched. A caller that does not know what kind
	// of file this is must never reach the rewrite, which would otherwise stamp
	// a meaningless magic into the header it writes.
	if kind.magic() == "" {
		return Repair{Path: path}, fmt.Errorf("wal: repair %s: %w: %s", path, ErrUnknownKind, kind)
	}
	// A file that is absent or zero-length provably holds no record, so it is
	// answered BEFORE the codec is resolved: a read of a log that is not there
	// must not create a MAC key as a side effect.
	if empty, err := logIsEmpty(path); err != nil || empty {
		return Repair{Path: path}, err
	}
	c, err := resolveCodec(path, kind, logger)
	if err != nil {
		return Repair{Path: path}, err
	}
	return repairLog(path, kind, c, logger)
}

// repairLog is RepairLog with the codec already resolved, so that Open resolves
// the format and loads the MAC key exactly once for the whole of recovery, and
// so that the version 1 path can repair a legacy log in its own format before
// upgradeV1 converts it.
func repairLog(path string, kind Kind, c codec, logger *logging.Logger) (Repair, error) {
	res := Repair{Path: path}

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

	if _, _, _, err := scanFraming(path, kind, c); err == nil {
		return res, nil // the file is well framed end to end
	}

	plan, err := salvage(path, kind, c, nil)
	if err != nil {
		return res, err // only the "cannot read this file" class reaches here
	}

	// A file whose header does not verify AND from which not one record can be
	// salvaged is not a log this code can make anything of. Move it aside and
	// start fresh, rather than refuse to boot for ever.
	if plan.HeaderDamaged && !plan.Salvageable {
		// EXCEPT when that is what a WRONG KEY looks like, which is the one
		// deliberate exception to the always-restart policy (DECISIONS.md
		// 2026-08-02, "a missing or wrong key is FATAL").
		//
		// The two cases are separated by evidence, not by guesswork. A wrong key
		// fails the file header's MAC and EVERY record's MAC, because it fails
		// all of them for the same reason. Damage confined to the header leaves
		// the records verifying -- so plan.Salvageable being TRUE proves the key
		// is right, and that case is handled below exactly as it always was: the
		// header is rebuilt, every record is kept, the bus starts.
		//
		// What is left here is a header that is structurally OURS and claims the
		// CURRENT version, whose MAC fails, with not one verifying record behind
		// it. That is byte-indistinguishable from opening an intact log under the
		// wrong key, and quarantining it would rename an entire probably-intact
		// log aside over a misconfiguration that takes seconds to fix. Every
		// other shape -- garbage magic, a file shorter than a header, a version 1
		// log -- keeps today's quarantine behaviour untouched.
		//
		// THE ACCEPTED COST, stated plainly: a genuinely destroyed version 2 log
		// (right key, header gone, no record readable anywhere) no longer
		// self-quarantines. It needs one manual `mv` before the bus will start.
		// That is the price of not deleting a log because someone mounted the
		// wrong volume.
		//
		// One narrowing, and it only ever makes recovery MORE available: a file
		// no longer than its own header holds no record at all, so there is
		// nothing a wrong key could be hiding and nothing quarantining it can
		// destroy. That case keeps the old behaviour -- moved aside, fresh log,
		// bus starts -- rather than demanding an operator delete an empty file
		// by hand.
		if !c.isV1() && plan.HeaderMagicOK && plan.HeaderVersion == FormatVersion && plan.Size > c.fileHeaderSize() {
			keyPath := macKeyPath(filepath.Dir(path))
			return res, &macKeyErr{sentinel: ErrMACKeyMismatch,
				msg: fmt.Sprintf("wal: %s: the file header does not verify under the MAC key at %s, and not one record in the file verifies under it either; that is what a WRONG KEY looks like, and recovery will not discard a log that is probably intact over a misconfiguration (a missing or wrong key is a deliberate exception to the always-restart policy). If the key is genuinely lost, move %s aside by hand and restart.",
					path, keyPath, path)}
		}
		dest, qerr := quarantine(path)
		if qerr != nil {
			return res, qerr // renaming failed: a filesystem problem, not damage
		}
		res.Quarantined = dest
		res.NextIndex = 1 // a fresh log starts here, and that is not a guess
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
		// rewriteLog re-runs the walk and checks it against plan BEFORE it
		// renames anything, so a disagreement leaves the original file untouched.
		after, err := rewriteLog(path, kind, c, plan)
		if err != nil {
			return res, err
		}
		res.Rewritten = true
		res.Reason = firstReason(plan.Discards)
		nowBytes := sizeOf(path)
		if nowBytes >= 0 {
			res.Removed = before - nowBytes
		}
		logger.Warn("wal rewrote a damaged log, keeping every intact record",
			"path", path, "kept", after.Kept, "rebuilt", after.Rebuilt,
			"discards", plan.Count, "discarded_bytes", plan.Bytes,
			"was_bytes", before, "now_bytes", nowBytes, "next_index", res.NextIndex)

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
	// whose tag this code verified, so a failure here means the repair itself is
	// broken -- and the answer to that is NOT to repair again, which would
	// happily eat a log one pass at a time.
	if _, _, _, err := scanFraming(path, kind, c); err != nil {
		return res, fmt.Errorf("wal: repair %s: the repaired log is still not readable; recovery will not repair it a second time and it needs operator inspection: %w", path, err)
	}
	return res, nil
}

// upgradeV1 converts a format version 1 log to version 2, ONCE, at startup.
//
// # Why this exists at all
//
// Format version 2 replaced the unkeyed CRC32C with a keyed HMAC-SHA256. A naive
// version bump would BRICK EVERY EXISTING BUS: a version 2 reader would find a
// version number it does not implement, refuse to start, and there would be no
// route back. So the story, which the file header makes enforceable, is:
//
//	A VERSION 1 LOG IS VERIFIED WITH CRC32C, REPAIRED IF DAMAGED WITH THE
//	VERSION 1 CODEC, THEN CONVERTED ONCE TO VERSION 2 AT STARTUP.
//
// A FILE IS ENTIRELY ONE VERSION. There is never a mixed version-1-then-version-2
// file: the version lives in the file header, one header describes one file, and
// a version 2 writer never emits a version 1 frame. That is why the conversion
// is a whole-file rewrite rather than an append.
//
// # What is preserved
//
// INDICES, TYPES AND PAYLOADS ARE CARRIED ACROSS BYTE FOR BYTE. Nothing is
// renumbered and nothing is compacted -- invariant 1 forbids reusing an id, and
// a log with holes in its index sequence is legal, permanent state. The
// conversion changes the FRAMING and nothing else.
//
// # Crash safety
//
// Everything is written to a temporary file, verified, and renamed over the
// original, which is atomic. THE ORIGINAL IS UNTOUCHED UNTIL THE RENAME, so a
// crash at any point simply re-runs the whole upgrade on the next start -- which
// is why the stale temporary is removed rather than resumed, and why this
// function is idempotent and returns nil when the file is already version 2.
//
// # Verification before the rename
//
// The converted file is re-scanned WITH THE VERSION 2 CODEC and its records
// digested again; the record count and the digest must match what was read out
// of the original. A mismatch is FATAL and leaves the original in place. This is
// the same discipline as rewriteLog's two-pass check and for the same reason: a
// disagreement is a bug in this code, not damage in the file, and restarting
// onto a converted log that nothing verified is worse than not restarting.
func upgradeV1(path string, kind Kind, to codec, logger *logging.Logger) error {
	version, err := detectFormat(path, kind)
	if err != nil {
		return err
	}
	if version != formatVersionV1 {
		// Already converted -- by a previous start, or by the repair above,
		// which rewrites the file in whatever version it read it in. Or gone:
		// an unreadable log may have been quarantined. Either way, nothing to do.
		return nil
	}
	from := codec{version: formatVersionV1}

	tmp := path + ".upgrade"
	// A stale temporary from a crashed upgrade is meaningless: it is a partial
	// copy of a file that is still intact. Remove it rather than resume it.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("wal: upgrade %s: remove the stale temporary %s: %w", path, tmp, err)
	}

	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("wal: upgrade %s: open: %w", path, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("wal: upgrade %s: create the temporary %s: %w", path, tmp, err)
	}
	cleanup := func() {
		dst.Close()
		os.Remove(tmp)
	}

	bw := bufio.NewWriter(dst)
	if _, err := bw.Write(to.makeFileHeader(kind)); err != nil {
		cleanup()
		return fmt.Errorf("wal: upgrade %s: write the file header of %s: %w", path, tmp, err)
	}
	// A RUNNING digest, not a slice of records: a log may be far larger than
	// memory, and the check below must cost O(1) space however big it is.
	digest := sha256.New()
	records := uint64(0)
	if _, err := scanFrom(from, src, path, kind, func(rec Record) error {
		if _, werr := bw.Write(to.encodeFrame(rec.Index, rec.Type, rec.Payload)); werr != nil {
			return werr
		}
		records++
		digestRecord(digest, rec)
		return nil
	}); err != nil {
		cleanup()
		return fmt.Errorf("wal: upgrade %s: read it as on-disk format version %d: %w", path, formatVersionV1, err)
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return fmt.Errorf("wal: upgrade %s: flush %s: %w", path, tmp, err)
	}
	if err := dst.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("wal: upgrade %s: fsync %s: %w", path, tmp, err)
	}

	gotRecords, gotDigest, err := digestLog(tmp, kind, to)
	if err != nil {
		cleanup()
		return fmt.Errorf("wal: upgrade %s: re-read the converted %s: %w", path, tmp, err)
	}
	if gotRecords != records || !bytes.Equal(gotDigest, digest.Sum(nil)) {
		cleanup()
		return fmt.Errorf("wal: upgrade %s: the converted log does not match the original (original %d records digest %x, converted %d records digest %x); this is a bug in the upgrade, not damage in the file, and the log has NOT been changed",
			path, records, digest.Sum(nil), gotRecords, gotDigest)
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("wal: upgrade %s: close %s: %w", path, tmp, err)
	}

	// A BEST-EFFORT backup of the original, by hard link so it costs no space
	// and cannot itself fail half way. If the filesystem will not link, that is
	// logged and the upgrade CONTINUES: refusing to boot for want of a backup
	// would contradict the always-restart policy, and the operator's real backup
	// is not this file.
	backup := fmt.Sprintf("%s.v1-%d", path, time.Now().UTC().UnixNano())
	if err := os.Link(path, backup); err != nil {
		logger.Warn("wal could not keep a backup of the format version 1 log before upgrading it",
			"path", path, "backup", backup, "error", err.Error())
		backup = ""
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("wal: upgrade %s: rename %s over it: %w", path, tmp, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("wal: upgrade %s: fsync the directory after the rename: %w", path, err)
	}
	logger.Info("wal upgraded a log from on-disk format version 1 to 2",
		"path", path, "records", records, "backup", backup,
		"key", macKeyPath(filepath.Dir(path)))
	return nil
}

// digestRecord folds one record's IDENTITY -- index, type and payload -- into a
// running digest.
//
// It is an internal SELF-CHECK, not a security primitive: it answers "did the
// conversion carry every record across unchanged?" and nothing else. The fields
// are length-prefixed so that two different record streams cannot produce the
// same byte string, which is the only property it needs.
func digestRecord(h hash.Hash, rec Record) {
	var b [14]byte
	binary.BigEndian.PutUint64(b[0:8], rec.Index)
	binary.BigEndian.PutUint16(b[8:10], uint16(rec.Type))
	binary.BigEndian.PutUint32(b[10:14], uint32(len(rec.Payload)))
	h.Write(b[:])
	h.Write(rec.Payload)
}

// digestLog scans path with codec c and returns the record count and the running
// digest of every record's identity. See digestRecord.
func digestLog(path string, kind Kind, c codec) (uint64, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	h := sha256.New()
	records := uint64(0)
	if _, err := scanFrom(c, f, path, kind, func(rec Record) error {
		records++
		digestRecord(h, rec)
		return nil
	}); err != nil {
		return 0, nil, err
	}
	return records, h.Sum(nil), nil
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

// sizeOf reports a file's size, or -1 if it cannot be stat'd. It is used only
// for log lines and for the Removed figure, never for a decision.
//
// It returns -1 rather than 0 on failure because the caller subtracts it from
// the pre-repair size: a 0 would turn "I could not measure the file" into "the
// entire file was removed", which is the most alarming possible reading of a
// transient stat error. Callers must treat a negative result as unknown.
func sizeOf(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
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
func scanFraming(path string, kind Kind, c codec) (records uint64, end, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("wal: open %s: %w", path, err)
	}
	defer f.Close()

	end, scanErr := scanFrom(c, f, path, kind, func(Record) error {
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
