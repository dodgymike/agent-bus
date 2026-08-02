package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// TailRepair is the outcome of a RepairTail pass: what, if anything, was cut
// off the end of a log before it was replayed.
type TailRepair struct {
	// Path is the file that was examined.
	Path string

	// Truncated reports whether a repair actually happened. It is false when the
	// file needed none, which is the overwhelmingly common case.
	Truncated bool

	// At is the offset the file was truncated to: the end of the last verified-
	// good record, and therefore the offset the next append writes at.
	At int64

	// Removed is how many bytes were discarded (the old size minus At).
	Removed int64

	// NextIndex is the record index the discarded frame would have carried --
	// which is also the index the next append will now use, because indices are
	// dense and the discarded frame was the last one in the file.
	//
	// ---------------------------------------------------------------------
	// WHY REISSUING THAT INDEX DOES NOT VIOLATE INVARIANT 1 ("ids are never
	// reused, including across restarts").
	//
	// A frame is only ever discarded here when its fsync provably never
	// completed. Writer.Append returns only after the frame is written AND
	// fsynced, and nothing in this system is acknowledged before Append has
	// returned (invariant 4). So a frame that is torn on disk is a frame whose
	// Append never returned success, which means:
	//
	//   - nothing inside that frame was ever acknowledged to anyone;
	//   - no id it carried -- message id, sequence, transaction id (which IS
	//     the prepare's WAL index) -- can have been observed by any client,
	//     peer, or relay, because observation happens strictly after the ack;
	//   - therefore handing that index out again cannot make two OBSERVED
	//     things share an id, which is the property invariant 1 actually
	//     protects.
	//
	// This argument holds ONLY for the last, never-durable frame. That is
	// exactly what the one-frame rule in truncatableTail guarantees: nothing
	// that ever completed an fsync is inside the discarded region. Every
	// COMPLETE frame survives the repair untouched -- including a dangling
	// PREPARE, which is durable, was potentially observable, and still burns
	// its index for good (Replay discards its ENTRY but still counts its index
	// in the high-water mark).
	//
	// This package does not mint application ids; internal/ids does, from the
	// recovered high-water mark. Nothing there changes.
	// ---------------------------------------------------------------------
	NextIndex uint64

	// Reason is the CorruptError's Reason for the damage that was cut away, so
	// the operator log and any report says WHY the tail went, not merely that it
	// did. It is already length-bounded where it is minted.
	Reason string
}

// RepairTail verifies the FRAMING of a log and, if and only if the damage is
// provably a torn tail, truncates it to the end of the last good record.
//
// It runs BEFORE Replay (see Open) and it is the ONLY place in this package
// that ever shortens a file -- invariant 6 permits exactly one exception to
// append-only, "a verified-corrupt tail during recovery", and this is it.
//
// A cut requires BOTH gates to agree: truncatableTail, which says the damage has
// the shape of a torn tail (a single incomplete frame whose declared extent
// reaches the end of the file), and laterRecordInTail, which says the bytes that
// cut would discard do not still contain a complete record. The second gate is
// there because the first one reasons from a frame header that damage may have
// falsified.
//
// It is a FRAMING-level pass only: it looks at file header, frame headers,
// checksums and the index sequence, and never at what a payload MEANS. That
// separation is deliberate and load-bearing. Every semantic failure -- a
// payload that will not decode, a COMMIT naming no open prepare, a record type
// with no meaning in a WAL -- is fatal where it sits, because such a record's
// checksum verified and there may be committed history after it. Keeping those
// failures in Replay, which never truncates, makes them unreachable from the
// truncation path rather than merely rejected by it.
//
// A missing file and a zero-length file are both "nothing to repair": neither
// can contain a record. (Zero length is the crash window between creating the
// file and writing its header, which OpenWriter heals.)
//
// Damage that is NOT a verified torn tail is returned unchanged as the scan's
// own error, which already names the path and the offset. That is a refusal to
// start, and it is the right answer: an operator can recover from a server that
// will not boot, and cannot recover from a server that quietly deleted records
// it could not verify.
func RepairTail(path string, kind Kind, logger *logging.Logger) (TailRepair, error) {
	res := TailRepair{Path: path}

	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return res, fmt.Errorf("wal: stat %s: %w", path, err)
	}
	if fi.Size() == 0 {
		return res, nil
	}

	records, scanEnd, size, scanErr := scanFraming(path, kind)
	if scanErr == nil {
		return res, nil // the file is well framed end to end
	}

	var ce *CorruptError
	if !errors.As(scanErr, &ce) || !truncatableTail(ce, scanEnd, size) {
		// Fatal where it sits. Returned VERBATIM: the scan error already carries
		// the path, the offset and the reason, and rewrapping it here would only
		// make the operator's grep longer.
		return res, scanErr
	}

	at := scanEnd

	// SECOND GATE: look inside the region about to be discarded for a record that
	// is still there. truncatableTail works from the frame header's OWN account of
	// itself, and that account is exactly what a corrupted length field falsifies,
	// so the shape test alone is not enough. See laterRecordInTail.
	if found, foundAt, err := laterRecordInTail(path, at, size, records+1); err != nil {
		return res, err
	} else if found {
		return res, tailHasRecordsAfterIt(path, ce, foundAt)
	}
	// Logged BEFORE the cut, so that a crash during the truncate still leaves an
	// operator the record of what was about to be discarded.
	logger.Warn("wal truncating a corrupt tail",
		"path", path, "at", at, "removed", size-at, "next_index", records+1, "reason", ce.Reason)

	if err := truncateAt(path, at); err != nil {
		return res, err
	}

	// Prove the result rather than assume it. If the file still does not scan,
	// something is wrong that this pass does not understand, and the answer is
	// to refuse the start -- NEVER to cut again. One cut per start, ever:
	// iterating "truncate until it parses" would happily eat an entire log one
	// frame at a time.
	if _, _, _, err := scanFraming(path, kind); err != nil {
		return res, fmt.Errorf("wal: repair %s: truncated a corrupt tail to offset %d (%s) but the result is still not a readable log; the file will not be cut a second time and needs operator inspection: %w",
			path, at, ce.Reason, err)
	}

	res = TailRepair{
		Path:      path,
		Truncated: true,
		At:        at,
		Removed:   size - at,
		NextIndex: records + 1,
		Reason:    ce.Reason,
	}
	logger.Info("wal truncated a corrupt tail",
		"path", path, "at", res.At, "removed", res.Removed, "next_index", res.NextIndex, "reason", res.Reason)
	return res, nil
}

// truncatableTail decides whether a framing failure has the SHAPE of a torn tail
// that may be cut away. The caller has already established that err is a
// *CorruptError.
//
// It is the FIRST of two gates and is not sufficient on its own. Everything it
// reasons about comes from the damaged frame's own header -- its offset, its
// declared extent -- and a corrupted length field is exactly the damage that
// makes that self-description false. RepairTail therefore also inspects the bytes
// the cut would discard (laterRecordInTail) before cutting anything. Do not call
// this alone and act on the answer.
//
// It answers NO unless the damage is provably confined to a single frame at the
// very end of the file. Every clause below is a veto, and each one exists
// because without it a different class of damage would be silently deleted.
//
// THE ONE-FRAME RULE, and why it is a proof rather than a heuristic:
// Writer.Append assembles one whole frame in a buffer, issues ONE write for it,
// fsyncs before returning, and POISONS the Writer if either step fails so that
// nothing is ever appended after a torn write. At most one frame is therefore
// ever in flight, so the only thing a crash can leave behind is a strict PREFIX
// of a single frame at the end of the file. "The damaged region fits inside one
// declared frame, and nothing follows it" is exactly the observable form of
// that fact.
//
// THE DELIBERATE CONSERVATIVE GAP: a tail of NUL bytes LONGER than one frame --
// which some filesystems expose for a write that never actually landed -- is
// NOT truncated. It fails rule (b) below (a zero length field declares a
// 20-byte frame, which does not reach the end of the file) and so it is a fatal
// startup error. That is the intended trade: refusing to start is recoverable
// by an operator inspecting the file, and truncating an unverifiable region is
// not.
func truncatableTail(ce *CorruptError, scanEnd, size int64) bool {
	// A frame whose CHECKSUM VERIFIED cannot have been produced by a partial
	// write, so the damage is a record lost from, or resurrected in, the middle
	// of the file. That is fatal WHEREVER it sits, including at the end.
	if ce.FrameIntact {
		return false
	}
	// A bad FILE header -- bad magic, wrong format version, header checksum --
	// is never truncated: the cut would be at offset 0 and would delete the
	// whole log.
	if ce.Offset < FileHeaderSize {
		return false
	}
	// The scan must have stopped exactly at the damaged frame. This is NOT a live
	// case: scanFrom returns the failing frame's offset as its end on every error
	// path, so the two cannot currently disagree. It is kept as a cheap pin on
	// that contract -- if a future change to scanFrom ever makes them differ, the
	// disagreement is about WHERE TO CUT, and recovery must not guess.
	if scanEnd != ce.Offset {
		return false
	}

	// (a) The 20-byte frame header itself was a short read. That can only happen
	// at end of file, so there is provably nothing after the damage.
	if ce.FrameEnd == 0 {
		return size-ce.Offset < FrameHeaderSize
	}

	// (b) The frame header was readable and therefore DECLARED an extent. The
	// extent must reach or pass the end of the file (nothing follows it) AND be
	// a legal frame size.
	//
	// The upper bound is not decoration. readFrame reports an absurd payloadLen
	// (one over MaxPayloadSize, up to 4 GiB) with FrameEnd = off + 20 +
	// payloadLen, which is gigantic and would sail past the ">= size" test on
	// its own. A single corrupted length field in the MIDDLE of a healthy file
	// would then look like a torn tail and truncate away every committed record
	// after it. Bounding the DECLARED extent to a frame the writer could
	// actually have produced makes that case fatal instead.
	return ce.FrameEnd >= size && ce.FrameEnd <= ce.Offset+FrameHeaderSize+MaxPayloadSize
}

// laterRecordInTail reports whether the region [at, size) -- the bytes a tail
// truncation is about to discard -- still contains a complete record.
//
// WHY THIS EXISTS. truncatableTail decides from the damaged frame's DECLARED
// extent, and a corrupted length field is precisely the damage that makes that
// declaration a lie. A record whose 4-byte length is bit-flipped to a value that
// is still legal (at most MaxPayloadSize) but overshoots the end of the file
// produces byte-for-byte the same error shape as a genuine torn tail --
// "truncated payload: have M of N bytes", FrameEnd past EOF -- while intact,
// COMMITTED records sit in the region behind it. Cutting there would delete
// accepted history. That is not hypothetical: it was demonstrated against this
// package with a six-record log, a single patched length field, and two
// committed records lost.
//
// HOW IT DECIDES, and why it is cheap. Records are written contiguously, and at
// most one frame is ever incomplete (Writer.Append writes one frame per fsync and
// poisons on failure). So IF any record follows the damage, the last of them ends
// EXACTLY at the end of the file. That anchor turns the search into an integer
// compare per offset -- does a frame starting here declare a payload that reaches
// precisely the end of the region? -- with a checksum computed only on a match.
// A random region produces a match with probability 2^-32 per offset, so this is
// a linear scan of at most a frame's worth of bytes and, in practice, no
// checksums at all. Anchoring at the end is also what keeps it safe against a
// hostile payload: a torn tail is attacker-influenced data (message bodies are
// client-supplied), and a search that CRC'd every plausible-looking header inside
// it could be made to do quadratic work at startup.
//
// KNOWN LIMIT, stated rather than hidden: this finds records after the damage
// only when the file ends on a record boundary. A file suffering BOTH a corrupted
// length in the middle AND a torn tail from a crash -- two independent faults --
// would not be caught by the anchor. The answer there is the same as everywhere
// else in this file: the shape tests must already have said "tail", and if this
// misses, the outcome is the one bad case. It is accepted because it needs two
// unrelated failures at once, and noted here so nobody rediscovers it as a
// surprise.
func laterRecordInTail(path string, at, size int64, wantIndex uint64) (bool, int64, error) {
	n := size - at
	if n <= FrameHeaderSize {
		return false, 0, nil // too small to hold a frame header, let alone a record
	}
	// truncatableTail has already bounded the region to one legal frame, so this
	// read is bounded by MaxPayloadSize. Belt and braces: refuse rather than
	// allocate if that ever stops being true.
	if n > int64(FrameHeaderSize)+MaxPayloadSize {
		return false, 0, fmt.Errorf("wal: repair %s: refusing to examine a %d-byte tail at offset %d: it is larger than the biggest legal frame, so it cannot be one torn record",
			path, n, at)
	}

	f, err := os.Open(path)
	if err != nil {
		return false, 0, fmt.Errorf("wal: repair %s: open to inspect the tail: %w", path, err)
	}
	defer f.Close()

	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, at); err != nil {
		return false, 0, fmt.Errorf("wal: repair %s: read the %d bytes at offset %d that a truncation would discard: %w",
			path, n, at, err)
	}

	for o := int64(0); o+FrameHeaderSize <= n; o++ {
		hdr := buf[o : o+FrameHeaderSize]
		// The anchor: a record here would have to end exactly at the end of file.
		if int64(binary.BigEndian.Uint32(hdr[0:4])) != n-o-FrameHeaderSize {
			continue
		}
		if binary.BigEndian.Uint16(hdr[14:16]) != 0 { // reserved
			continue
		}
		// A record that follows the damaged frame carries an index at or above the
		// one the damaged frame itself would have had.
		if binary.BigEndian.Uint64(hdr[4:12]) < wantIndex {
			continue
		}
		if frameChecksum(hdr[0:16], buf[o+FrameHeaderSize:]) != binary.BigEndian.Uint32(hdr[16:20]) {
			continue
		}
		return true, at + o, nil
	}
	return false, 0, nil
}

// tailHasRecordsAfterIt reports damage that LOOKED like a torn tail until the
// region behind it was inspected and found to still hold a complete record. It
// carries FrameIntact because that flag's meaning is "a partial write cannot
// explain this, so never truncate it", and a verifying record after the damage is
// exactly that: a crash mid-append leaves a prefix of one frame and nothing
// beyond it.
func tailHasRecordsAfterIt(path string, ce *CorruptError, foundAt int64) *CorruptError {
	e := corruptf(path, ce.Offset,
		"%s -- and a complete record whose checksum verifies begins at offset %d, inside the bytes a tail truncation would discard, so this is damage with accepted history AFTER it (most likely a corrupted length field) and the log will not be cut",
		ce.Reason, foundAt)
	e.FrameIntact = true
	e.FrameEnd = ce.FrameEnd
	e.Err = ce.Err
	return e
}

// scanFraming walks path as a whole file of the given kind and reports how many
// records it accepted, the offset just past the last of them, and the file's
// size. The file is opened and CLOSED here, so no descriptor is held across a
// truncate.
//
// The size is taken from THIS descriptor and AFTER the scan, not from a stat
// taken before it, so that a file which GREW during the scan reports the larger
// size: the declared extent of the damaged frame then no longer reaches the end
// of the file and truncatableTail refuses. A size measured beforehand could be
// stale-small and would turn bytes written during the scan into a "tail".
//
// This narrows a window; it does not close one. There is no lock on the data
// directory (see the note in Open), so a second process writing to the same log
// can still interleave with the whole repair -- between this scan and the
// truncate, or during it. Excluding that needs a real directory lock, and this
// ordering is only the cheap half of the answer.
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
