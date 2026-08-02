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
	// The argument has two halves, and BOTH are load-bearing. An earlier
	// version of this comment asserted only the first and was therefore
	// false; two independent reviews demonstrated the counterexample.
	//
	// HALF ONE -- nothing inside an incomplete frame ever escaped.
	// Writer.Append returns only after the frame is written AND fsynced, it
	// publishes the next index only then, and it poisons the Writer if either
	// step fails. Nothing in this system is acknowledged before Append has
	// returned (invariant 4). So for a frame whose Append never returned
	// success: nothing inside it was acknowledged to anyone, and no id it
	// carried -- message id, sequence, transaction id (which IS the prepare's
	// WAL index) -- can have been observed by any client, peer or relay,
	// because observation happens strictly after the ack. Reissuing that index
	// cannot make two OBSERVED things share an id, which is the property
	// invariant 1 actually protects.
	//
	// HALF TWO -- only provably incomplete frames are ever discarded, so half
	// one always applies. This is the half that has to be enforced in code
	// rather than assumed, because "the frame is damaged" does NOT imply "the
	// frame was never fsynced": media rot damages records that were written in
	// full, fsynced, and acknowledged. Two rules keep the discarded region to
	// bytes that are provably missing:
	//
	//   - truncatableTail requires the declared extent to run PAST the end of
	//     the file, strictly. A frame ending exactly at EOF has every byte it
	//     declared, so a failed checksum there is damage to durable bytes and
	//     is fatal, not truncatable.
	//   - inspectTail refuses when any complete record is found inside the
	//     region, and when the damaged frame's OWN checksum verifies over the
	//     bytes actually present at any plausible frame boundary -- which is
	//     proof that the record is complete and only its length field is
	//     corrupt. Checking every boundary rather than only "the frame ends at
	//     EOF" is what covers the common shape: the last complete record
	//     damaged, with a torn frame behind it.
	//
	// The residual exposure is honest and bounded: these are CHECKSUM proofs,
	// so they hold against random corruption, not against someone with write
	// access to the data directory who can recompute a CRC32C at will. A WAL
	// is not authenticated (see format.go); that is a separate problem and not
	// one this function can solve.
	//
	// Under a SINGLE fault -- one crash mid-append, or one region of
	// corruption in one frame -- every COMPLETE frame therefore survives the
	// repair, including a dangling PREPARE, which is durable, was potentially
	// observable, and still burns its index for good (Replay discards its
	// ENTRY but still counts its index in the high-water mark). That is
	// measured, not asserted: an exhaustive single-fault sweep run for DUR-10
	// (~345k cases) produced no silent loss, and the committed evidence is
	// TestCrashInjectionSingleBitCorruptionSweep and
	// TestCrashInjectionTruncationPrefixSweep, which quantify over every
	// offset in a fixture log and accept only "recovered in full" or "refused
	// to start".
	//
	// TWO faults in the SAME final frame are NOT covered, and that gap is
	// demonstrated rather than theoretical. Corrupt the last frame's LENGTH
	// field and ALSO flip a bit of its payload, and both proofs above fail for
	// the same reason -- they are CHECKSUM proofs, and a damaged payload no
	// longer reproduces the stored checksum at any hypothesised boundary --
	// while there is no later complete record to find either. The frame is
	// then cut, an acknowledged COMMIT can be lost, the high-water mark rolls
	// back over it, and Open returns no error at all.
	//
	// So the honest statement of the guarantee is the narrower one: the
	// high-water mark is never lowered across bytes that completed an fsync,
	// PROVIDED a single fault damaged them. Closing the two-fault case is not
	// a matter of a better check here -- once both the length field and the
	// payload are wrong there is nothing left in the format to check a
	// doubly-corrupted frame against.
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
// reaches the end of the file), and inspectTail, which says the bytes that
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

	// SECOND GATE: read the bytes the cut would discard and look for a reason not
	// to. truncatableTail works from the frame header's OWN account of itself, and
	// that account is exactly what a corrupted length field falsifies, so the
	// shape test alone is not enough. See inspectTail.
	refusal, evidenceAt, err := inspectTail(path, at, size, records+1)
	if err != nil {
		return res, err
	}
	if refusal != "" {
		e := tailHasRecordsAfterIt(path, ce, refusal, evidenceAt)
		// The refusal is logged here as well as returned. The error reaches the
		// operator only if the caller prints it, and "the server would not start"
		// is exactly the moment the reason must be in the log.
		logger.Warn("wal refusing to repair a tail",
			"path", path, "at", at, "would_have_removed", size-at, "reason", e.Reason)
		return res, e
	}

	// Logged BEFORE the cut, so that a crash during the truncate still leaves an
	// operator the record of what was about to be discarded. The discarded
	// frame's TYPE is named when its header survived: a discarded COMMIT is the
	// one shape that means a client was waiting on a write that will now never
	// exist, and that deserves an ERROR rather than a WARN.
	discarded, typeKnown := discardedFrameType(path, at, size)
	if typeKnown && discarded == TypeCommit {
		logger.Error("wal truncating a corrupt tail that carried a commit record",
			"path", path, "at", at, "removed", size-at, "next_index", records+1,
			"record_type", discarded, "reason", ce.Reason)
	} else {
		logger.Warn("wal truncating a corrupt tail",
			"path", path, "at", at, "removed", size-at, "next_index", records+1,
			"record_type", tailTypeLabel(discarded, typeKnown), "reason", ce.Reason)
	}

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
// the cut would discard (inspectTail) before cutting anything. Do not call
// this alone and act on the answer.
//
// It answers NO unless the damage is provably confined to a single frame at the
// very end of the file. Every clause below is a veto, and each one exists
// because without it a different class of damage would be silently deleted.
//
// THE ONE-FRAME RULE: Writer.Append assembles one whole frame in a buffer,
// issues ONE write for it, fsyncs before returning, and POISONS the Writer if
// either step fails so that nothing is ever appended after a torn write. At most
// one frame is therefore ever in flight, so the only thing a CRASH can leave
// behind is a strict PREFIX of a single frame at the end of the file.
//
// That bounds what a crash produces. It does NOT bound what this function is
// shown -- media rot, a hostile edit, or a filesystem that exposes a partly
// written region can all produce damage of any shape, and each of those has
// already been mistaken for a torn tail here at least once. So the rules below
// are a NECESSARY condition only, and RepairTail confirms them against the bytes
// themselves (inspectTail) before cutting. Treating the shape test as sufficient
// is the specific mistake that made this function delete committed records.
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
	// extent must run PAST the end of the file -- so the frame is provably
	// missing bytes -- and must be a legal frame size.
	//
	// STRICTLY GREATER, not ">=", and the difference is the whole invariant-1
	// argument. An extent landing EXACTLY on the end of the file describes a
	// COMPLETE frame: every byte it declares is present. If such a frame fails
	// its checksum, the damage is in bytes that were fully written -- media rot
	// in a record that may well have been fsynced and acknowledged -- and
	// discarding it would lose accepted history and roll the index high-water
	// mark backwards. A crash mid-append cannot produce it: Append issues one
	// write per frame, so an interrupted append leaves FEWER bytes than the
	// header declares, never exactly enough. So "complete but wrong" is fatal
	// and only "provably short" is a tail.
	//
	// The upper bound is not decoration either. readFrame reports an absurd
	// payloadLen (one over MaxPayloadSize, up to 4 GiB) with FrameEnd = off + 20
	// + payloadLen, which is gigantic and would sail past the "> size" test on
	// its own. A single corrupted length field in the MIDDLE of a healthy file
	// would then look like a torn tail and truncate away every committed record
	// after it. Bounding the DECLARED extent to a frame the writer could
	// actually have produced makes that case fatal instead.
	return ce.FrameEnd > size && ce.FrameEnd <= ce.Offset+FrameHeaderSize+MaxPayloadSize
}

// maxTailCandidates bounds how many checksum verifications the tail inspection
// will attempt before giving up and REFUSING the repair.
//
// The region being searched is attacker-influenced -- it is the partly-written
// tail of a record whose payload carries a client-supplied message body -- and a
// payload full of plausible-looking frame headers would otherwise make startup do
// quadratic checksum work. Hitting the cap means the region is dense with things
// that look like records, which is not what a torn tail looks like, so the
// fail-closed answer is also the honest one.
const maxTailCandidates = 4096

// inspectTail reads the bytes a truncation would discard -- the region
// [at, size) -- and returns a non-empty refusal reason if any of them argue that
// this is not a torn tail after all, together with the offset of the evidence.
// An empty reason means the cut is safe as far as the bytes can show.
//
// WHY THIS EXISTS. truncatableTail decides from the damaged frame's DECLARED
// extent, and a corrupted length field is precisely the damage that makes that
// declaration a lie. A record whose 4-byte length is bit-flipped to a value that
// is still legal (at most MaxPayloadSize) but overshoots the end of the file
// produces byte-for-byte the same error shape as a genuine torn tail --
// "truncated payload: have M of N bytes", FrameEnd past EOF -- while intact,
// COMMITTED records sit in the region behind it. That is not hypothetical: it was
// demonstrated three times against this package, twice by reviewers, once from a
// SINGLE FLIPPED BIT, deleting committed messages and then letting Open and Replay
// succeed with no error at all.
//
// Two independent proofs are applied, and the order they are listed in below is
// the order the CODE runs them in. That order is load-bearing, not incidental:
// both searches spend the SAME checksum budget (maxTailCandidates, shared via
// the candidates counter), so whichever runs first is the one that can exhaust
// it. The region scan goes first deliberately, because it is the proof that
// protects COMMITTED RECORDS SITTING BEHIND THE DAMAGE -- the case that actually
// destroyed accepted history -- so it is the one that must get the budget.
// Exhausting the budget is itself a refusal in both searches, so no ordering can
// turn a budget overrun into a silent cut; what the ordering decides is which
// evidence a region dense with frame-like headers is allowed to hide.
//
//  1. IS THERE A COMPLETE RECORD IN THE REGION? Any frame lying inside the bytes
//     to be discarded whose checksum verifies and whose index continues the file's
//     sequence is a record that a cut would delete. The index window is what makes
//     this cheap AND sensitive: indices are dense, so a genuine follower's index
//     lies within one region's worth of the damaged frame's own, an 8-byte match
//     against a narrow range. Searching by index rather than by "the frame ends at
//     EOF" is deliberate -- the earlier end-of-file anchor found nothing whenever
//     the file ALSO had a torn tail, and "the file has a torn tail" is the normal
//     state of every file this function is called on, not a rare second fault.
//
//  2. IS THE DAMAGED FRAME ACTUALLY COMPLETE? Hypothesise that the frame ends
//     exactly at the end of the file -- that its true payload length is the bytes
//     remaining -- and recompute its checksum on that basis. If it matches, every
//     byte the record needs IS present and the only thing wrong is the length
//     field itself. A crash cannot produce that (an interrupted append leaves
//     fewer bytes, not a mangled header), the record may well have been fsynced
//     and acknowledged, and truncating it would destroy accepted history. This is
//     a proof, not a heuristic: it is the writer's own checksum agreeing.
//
// Both are checksum proofs, which bounds what they can establish: a frame whose
// length field AND payload are both corrupt reproduces its checksum at no
// boundary and leaves no complete record behind it, so it passes both proofs and
// is cut. See the invariant-1 note on TailRepair.NextIndex for that residual
// two-fault hole; it is not closed here.
//
// Cost is O(region) integer work with a checksum only for candidates that pass
// both the index window and the length check, capped by maxTailCandidates.
func inspectTail(path string, at, size int64, wantIndex uint64) (string, int64, error) {
	n := size - at
	// Strictly LESS THAN, not <=. A complete record with a zero-length payload is
	// exactly FrameHeaderSize bytes, so at n == FrameHeaderSize there is still a
	// completeness proof to run; skipping it would truncate that record when its
	// length field was the corrupt part. (The write path emits no empty payloads
	// today -- every prepare/commit/abort payload is JSON -- but Writer.Append has
	// no lower bound on payload size, so this must not depend on that.)
	if n < FrameHeaderSize {
		return "", 0, nil // too small even to hold a frame header
	}
	// truncatableTail has already bounded the region to one legal frame, so this
	// read is bounded by MaxPayloadSize. Belt and braces: refuse rather than
	// allocate if that ever stops being true.
	if n > int64(FrameHeaderSize)+MaxPayloadSize {
		return "", 0, fmt.Errorf("wal: repair %s: refusing to examine a %d-byte tail at offset %d: it is larger than the biggest legal frame, so it cannot be one torn record",
			path, n, at)
	}

	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("wal: repair %s: open to inspect the tail: %w", path, err)
	}
	defer f.Close()

	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, at); err != nil {
		return "", 0, fmt.Errorf("wal: repair %s: read the %d bytes at offset %d that a truncation would discard: %w",
			path, n, at, err)
	}

	maxIndex := wantIndex + uint64(n/FrameHeaderSize) + 1
	// candidates is the shared checksum budget for BOTH searches below. The region
	// is attacker-influenced, so neither search may be allowed to do unbounded
	// verification work; running out is itself a refusal.
	candidates := 0

	// (1) A complete record still sitting in the bytes the cut would take.
	for o := int64(0); o+FrameHeaderSize <= n; o++ {
		hdr := buf[o : o+FrameHeaderSize]
		if binary.BigEndian.Uint16(hdr[14:16]) != 0 { // reserved
			continue
		}
		// The index window. The floor is deliberately >= rather than > : one
		// permissive, which errs towards refusing a repair, never towards making
		// one.
		idx := binary.BigEndian.Uint64(hdr[4:12])
		if idx < wantIndex || idx > maxIndex {
			continue
		}
		payloadLen := int64(binary.BigEndian.Uint32(hdr[0:4]))
		if payloadLen > MaxPayloadSize || o+FrameHeaderSize+payloadLen > n {
			continue // not a whole record inside the region
		}
		candidates++
		if candidates > maxTailCandidates {
			return "the region is dense with frame-like headers, which a torn tail is not; the checksum budget for inspecting it was exhausted and recovery will not cut a region it could not finish checking", at + o, nil
		}
		if frameChecksum(hdr[0:16], buf[o+FrameHeaderSize:o+FrameHeaderSize+payloadLen]) != binary.BigEndian.Uint32(hdr[16:20]) {
			continue
		}
		return "a complete record whose checksum verifies begins here, inside the bytes a tail truncation would discard, so this is damage with accepted history AFTER it", at + o, nil
	}

	// (2) The damaged frame itself, reconstructed as if only its LENGTH were wrong.
	if trueLen, ok := lengthOnlyDamage(buf, wantIndex, maxIndex, &candidates); ok {
		if trueLen < 0 {
			return "the region is dense with frame-like headers, which a torn tail is not; the checksum budget for inspecting it was exhausted and recovery will not cut a region it could not finish checking", at, nil
		}
		return "the frame's own checksum verifies once its length field is read as the bytes actually present, so the record is COMPLETE and only its length is corrupt -- it may have been fsynced and acknowledged", at, nil
	}
	return "", 0, nil
}

// lengthOnlyDamage reports whether buf -- the damaged frame's header followed by
// every byte remaining in the file -- is a COMPLETE frame whose length field is
// the only thing wrong. It returns the recovered true payload length, or -1 with
// ok=true when the checksum budget ran out (which the caller treats as a refusal).
//
// It works by rewriting the length field with a hypothesised length and asking
// the writer's own checksum. Only the TRUE length reproduces the stored value, so
// a match is proof that the payload is all there: the record was fully written,
// and a crash mid-append could not have left it that way.
//
// WHICH LENGTHS ARE HYPOTHESISED, and why more than one. An earlier version tried
// exactly one -- "the frame ends at the end of the file" -- and that is false
// whenever anything follows the damaged frame. Since the ordinary state of a file
// reaching this code is "a crash left a torn frame on the end", the single
// hypothesis missed the most likely real case: the last COMPLETE record's length
// field corrupted, with a torn next frame behind it. That shape was cut, losing an
// acknowledged COMMIT and rolling the index high-water mark back by one. Records
// are contiguous, so the damaged frame's true end is a frame boundary, and every
// plausible boundary in the region is tried:
//
//   - the end of the file (nothing follows);
//   - any offset whose bytes look like the header of the NEXT record -- reserved
//     clear and an index inside the window;
//   - any offset in the last FrameHeaderSize-1 bytes, where a trailing scrap is
//     too short to be a header but is exactly what a torn next frame leaves.
//
// A frame that is genuinely torn verifies at NO boundary: the checksum has to
// match the bytes that are actually there, and they are not.
func lengthOnlyDamage(buf []byte, wantIndex, maxIndex uint64, candidates *int) (int64, bool) {
	n := int64(len(buf))
	if n < FrameHeaderSize {
		return 0, false
	}
	stored := binary.BigEndian.Uint32(buf[16:20])
	try := func(payloadLen int64) (int64, bool) {
		if payloadLen < 0 || payloadLen > MaxPayloadSize || FrameHeaderSize+payloadLen > n {
			return 0, false
		}
		*candidates++
		if *candidates > maxTailCandidates {
			return -1, true
		}
		var hdr [16]byte
		copy(hdr[:], buf[0:16])
		binary.BigEndian.PutUint32(hdr[0:4], uint32(payloadLen))
		if frameChecksum(hdr[:], buf[FrameHeaderSize:FrameHeaderSize+payloadLen]) != stored {
			return 0, false
		}
		return payloadLen, true
	}

	// Boundary one: the record ends at the end of the file.
	if got, ok := try(n - FrameHeaderSize); ok {
		return got, true
	}
	// Boundary two and on: the record ends where the next one begins.
	for o := int64(FrameHeaderSize); o < n; o++ {
		if n-o < FrameHeaderSize {
			// A trailing scrap too short to be a frame header -- the signature of a
			// torn next frame, and a plausible boundary for that reason alone.
			if got, ok := try(o - FrameHeaderSize); ok {
				return got, true
			}
			continue
		}
		if binary.BigEndian.Uint16(buf[o+14:o+16]) != 0 { // reserved
			continue
		}
		idx := binary.BigEndian.Uint64(buf[o+4 : o+12])
		if idx <= wantIndex || idx > maxIndex {
			continue
		}
		if got, ok := try(o - FrameHeaderSize); ok {
			return got, true
		}
	}
	return 0, false
}

// discardedFrameType reports the record type in the frame header at the head of
// the region about to be discarded, when enough of that header survived to hold
// one. It is for the operator log only -- nothing is decided from it.
func discardedFrameType(path string, at, size int64) (Type, bool) {
	if size-at < FrameHeaderSize {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var hdr [FrameHeaderSize]byte
	if _, err := f.ReadAt(hdr[:], at); err != nil {
		return 0, false
	}
	return Type(binary.BigEndian.Uint16(hdr[12:14])), true
}

// tailTypeLabel renders a discarded frame's type for a log line, including the
// case where too few bytes survived to name one.
func tailTypeLabel(t Type, known bool) string {
	if !known {
		return "unreadable"
	}
	return t.String()
}

// tailHasRecordsAfterIt reports damage that LOOKED like a torn tail until the
// bytes behind it were inspected and found to argue otherwise. It carries
// FrameIntact because that flag's meaning is "a partial write cannot explain
// this, so never truncate it", and everything inspectTail refuses on is exactly
// that: a crash mid-append leaves a prefix of one frame, with no complete record
// inside it and no intact checksum over it.
func tailHasRecordsAfterIt(path string, ce *CorruptError, refusal string, evidenceAt int64) *CorruptError {
	e := corruptf(path, ce.Offset,
		"%s -- but at offset %d, %s; the log will not be cut",
		ce.Reason, evidenceAt, refusal)
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
