package wal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// This file implements the SALVAGE pass: the tolerant walk that lets recovery
// always reach a running server.
//
// POLICY (user decision, DECISIONS.md 2026-08-02, "Availability over
// retention"): "always be able to restart, prefer to discard messages and/or
// corruption, with logging". Damage in the file is NEVER a reason to refuse to
// start. Being unable to READ the file at all still is.
//
// The line between those two, which every function here is written against:
//
//   - "the file contains damage" -- a torn frame, a flipped bit, a lost sector,
//     a record whose payload no longer decodes, a damaged file header. The
//     record is discarded (or repaired, when its bytes prove it recoverable),
//     the discard is LOGGED with its offset, length, index and type, and
//     recovery continues. Never fatal.
//   - "I cannot read the file at all" -- permission denied, an I/O error from
//     the device, a directory that will not open, a file that is a DIFFERENT
//     KIND of log, or a log written in a format version this binary does not
//     implement. None of those are damage: retrying, or guessing at a layout
//     this code does not implement, would destroy a file that is probably fine.
//     Still fatal.
//
// The other half of the policy is that DAMAGE MUST NOT CASCADE. Discarding the
// damaged record is sanctioned; deleting later records that are themselves
// intact is not. That is what resyncFrom exists for: after damage, recovery
// searches FORWARD for the next intact record by INDEX and resumes there,
// rather than assuming that everything after the damage is lost.

// Discard is one thing recovery threw away or rebuilt. It is the unit of this
// package's central promise after the 2026-08-02 policy change: NOTHING IS
// DISCARDED SILENTLY. Every value here is reported to the caller and logged.
type Discard struct {
	// Stage is where the loss was decided: "framing" for the salvage pass over
	// the raw file, "replay" for a record whose frame was intact but whose
	// CONTENT could not be turned into history.
	Stage string

	// Offset is the byte offset of the discarded region, and Length its size.
	// Length is 0 for a gap in the index sequence, which is a record that is
	// already absent rather than bytes being removed now.
	Offset int64
	Length int64

	// Index is the record index the region carried, or 0 when not even that
	// could be established. TypeKnown is false when too few bytes survived to
	// read a frame header, in which case Type means nothing.
	Index     uint64
	Type      Type
	TypeKnown bool

	// Severe forces this discard to ERROR even when its record type would not.
	// It is set for a loss recovery could not bound -- see the Exhausted case in
	// recoverAfterDamage, where records were dropped WITHOUT proof that they
	// were unreadable.
	Severe bool

	// Reason says WHY, in the same words the operator log carries.
	Reason string
}

// severe reports whether losing this region deserves ERROR rather than WARN.
//
// A COMMIT record is the one shape that means a client was told a write was
// durable, so losing one is a broken promise and is always ERROR. So is a
// region whose frame header did not survive, because then it is not known what
// was in it -- and "I do not know what I just deleted" is worse news than "I
// deleted a prepare that never committed".
func (d Discard) severe() bool {
	if d.Severe || !d.TypeKnown {
		return true
	}
	return d.Type == TypeCommit
}

// typeLabel renders the record type for a log line, including the case where
// too few bytes survived to name one.
func (d Discard) typeLabel() string {
	if !d.TypeKnown {
		return "unreadable"
	}
	return d.Type.String()
}

// indexLabel renders the record index for a log line, or "unknown".
func (d Discard) indexLabel() string {
	if d.Index == 0 {
		return "unknown"
	}
	return strconv.FormatUint(d.Index, 10)
}

// Budgets for the salvage pass. All three exist because the bytes being walked
// are ATTACKER-INFLUENCED -- a WAL payload carries a client-supplied message
// body -- so a damaged file must not be able to make startup do unbounded work
// or allocate unbounded memory. Exceeding one is itself reported and logged.
const (
	// maxResyncCandidates bounds how many checksum verifications the forward
	// search for the next intact record will attempt.
	maxResyncCandidates = 4096

	// maxResyncChecksumBytes bounds the total payload bytes that search will
	// checksum, since a count alone bounds nothing when one payload may be
	// MaxPayloadSize.
	maxResyncChecksumBytes = 64 << 20

	// maxDiscardsRetained bounds how many Discard records a Repair or a
	// Recovered keeps for its caller. The COUNTS are always exact; only the
	// detail list is capped, so a file that is damage from end to end cannot
	// make recovery hold the whole file in memory as error text.
	maxDiscardsRetained = 64

	// resyncChunk is the read size of the forward search.
	resyncChunk = 1 << 16
)

// repairPlan is what one salvage walk found. It is produced twice for a file
// that needs rewriting -- once to decide, once to copy -- and the walk is a
// pure function of the bytes, so the two passes agree by construction.
type repairPlan struct {
	// Size is the file size the walk saw.
	Size int64

	// HeaderDamaged reports a file header that did not verify but that the
	// records behind it prove is ours; it is rewritten, not obeyed.
	HeaderDamaged bool

	// LastIndex is the highest record index that SURVIVED, so LastIndex+1 is
	// the index the next append uses.
	LastIndex uint64

	// Kept is how many records survived; Rebuilt is how many of those had a
	// corrupt LENGTH field that the record's own checksum proved recoverable.
	Kept    uint64
	Rebuilt uint64

	// Discards is the capped detail list; Count and Bytes are exact.
	Discards []Discard
	Count    int
	Bytes    int64

	// TailAt is the offset of a discarded region that runs to the end of the
	// file, or -1 when the last thing in the file is a surviving record.
	TailAt int64

	// Exhausted reports that a forward search hit its work budget and gave up,
	// so records after that point were discarded WITHOUT proof that they were
	// unreadable. It is the one cascade this pass can still produce and it is
	// logged as such.
	Exhausted bool

	// Salvageable reports whether at least one record survived. A file with a
	// damaged header and nothing salvageable is not a log this code can
	// interpret at all, and is quarantined rather than rewritten.
	Salvageable bool
}

func (p *repairPlan) add(d Discard) {
	p.Count++
	p.Bytes += d.Length
	if len(p.Discards) < maxDiscardsRetained {
		p.Discards = append(p.Discards, d)
	}
}

// needsRewrite reports whether the file has to be rebuilt to become readable,
// as opposed to merely shortened.
//
// A single discarded region at the END is a truncation, which is cheap and is
// the overwhelmingly common case (a crash mid-append). Anything else -- damage
// with intact records behind it, a header to rebuild, a record whose length
// field must be restored -- means the surviving frames have to be copied into a
// new file, because there is no way to remove bytes from the middle of a file
// in place.
func (p *repairPlan) needsRewrite() bool {
	if p.HeaderDamaged || p.Rebuilt > 0 {
		return true
	}
	trailing := 0
	if p.TailAt >= 0 {
		trailing = 1
	}
	return p.Count > trailing
}

// salvage walks path tolerantly from its file header to its end, calling keep
// for every record that survives recovery, in file order, and recording every
// region it discards.
//
// It never stops at damage. On a frame that does not parse or does not verify
// it searches forward for the next intact record (resyncFrom) and resumes
// there, so one damaged record costs one record. The ONLY errors it returns are
// the "cannot read this file at all" class described at the top of this file.
//
// keep may be nil, in which case the walk only decides and counts.
func salvage(path string, kind Kind, keep func(Record) error) (repairPlan, error) {
	plan := repairPlan{TailAt: -1}

	f, err := os.Open(path)
	if err != nil {
		return plan, fmt.Errorf("wal: repair %s: open: %w", path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return plan, fmt.Errorf("wal: repair %s: stat: %w", path, err)
	}
	size := fi.Size()
	plan.Size = size

	if err := checkSalvageHeader(f, path, kind, size, &plan); err != nil {
		return plan, err
	}

	off := int64(FileHeaderSize)
	if size < FileHeaderSize {
		// Nothing but a torn header. There is no record in these bytes, so the
		// whole file is the discard.
		plan.add(Discard{Stage: "framing", Offset: 0, Length: size,
			Reason: fmt.Sprintf("the file is %d bytes, too short to hold even a %d-byte file header", size, FileHeaderSize)})
		plan.TailAt = 0
		return plan, nil
	}

	var br *bufio.Reader
	for off < size {
		if br == nil {
			br = bufio.NewReader(io.NewSectionReader(f, off, size-off))
		}

		rec, err := readFrame(br, path, off)
		if err == io.EOF {
			break
		}
		if err != nil {
			var ce *CorruptError
			if !errors.As(err, &ce) {
				// Not damage: a read failed at the device. Refusing here is
				// correct -- retrying the same read is not going to help, and
				// treating an unread region as "discardable" would delete
				// records that are probably still there.
				return plan, err
			}
			resumeAt, err := plan.recoverAfterDamage(f, off, size, keep)
			if err != nil {
				return plan, err
			}
			off = resumeAt
			br = nil
			continue
		}

		if rec.Index <= plan.LastIndex {
			// The frame's checksum verified, so these bytes are exactly what
			// some writer wrote -- but they are in the wrong place: an old
			// record resurrected under us, or the same record twice. Keeping it
			// would replay history out of order, so it goes, loudly.
			plan.add(Discard{Stage: "framing", Offset: off, Length: rec.frameSize(),
				Index: rec.Index, Type: rec.Type, TypeKnown: true,
				Reason: fmt.Sprintf("record index %d does not follow the previous surviving record (index %d): the record is intact but out of order, so it is an old record resurrected in place or a duplicate",
					rec.Index, plan.LastIndex)})
			off += rec.frameSize()
			continue
		}
		plan.LastIndex = rec.Index
		plan.Kept++
		plan.Salvageable = true
		if keep != nil {
			if err := keep(rec); err != nil {
				return plan, err
			}
		}
		off += rec.frameSize()
	}
	return plan, nil
}

// recoverAfterDamage handles one damaged frame at off: it decides where good
// data resumes, works out whether the damaged frame is recoverable after all,
// and records the discard. It returns the offset the walk continues from.
func (p *repairPlan) recoverAfterDamage(f *os.File, off, size int64, keep func(Record) error) (int64, error) {
	hdr, hdrOK := frameHeaderAt(f, off, size)

	// The declared extent of the damaged frame. When only the PAYLOAD is
	// damaged the length field is still right, so the next record starts
	// exactly there -- one checksum instead of a byte-by-byte search.
	declared := int64(-1)
	if hdrOK {
		if n := int64(binary.BigEndian.Uint32(hdr[0:4])); n <= MaxPayloadSize {
			declared = FrameHeaderSize + n
		}
	}

	next, exhausted, err := resyncFrom(f, size, off, p.LastIndex, declared)
	if err != nil {
		return 0, err
	}
	if exhausted {
		p.Exhausted = true
	}

	end := size
	if next > 0 {
		end = next
	}

	// Is the damaged frame actually COMPLETE, with only its length field wrong?
	// Records are contiguous, so the frame's true end is where the next
	// surviving record begins (or the end of the file). Rewriting the length to
	// that and asking the writer's own checksum is a PROOF, not a guess: only
	// the true length reproduces the stored value. When it matches, every byte
	// the record needs is present and it is kept rather than thrown away.
	if hdrOK {
		if rec, ok := rebuildFrame(f, off, end, hdr); ok && rec.Index > p.LastIndex {
			p.LastIndex = rec.Index
			p.Kept++
			p.Rebuilt++
			p.Salvageable = true
			if keep != nil {
				if err := keep(rec); err != nil {
					return 0, err
				}
			}
			return end, nil
		}
	}

	d := Discard{Stage: "framing", Offset: off, Length: end - off}
	if hdrOK {
		d.Index = binary.BigEndian.Uint64(hdr[4:12])
		d.Type = Type(binary.BigEndian.Uint16(hdr[12:14]))
		d.TypeKnown = true
	}
	switch {
	case exhausted:
		// Records were dropped without proof that they were unreadable, so this
		// is ERROR whatever the discarded record's type says.
		d.Severe = true
		d.Reason = fmt.Sprintf("the frame at offset %d is damaged, and the search for the next intact record after it ran out of its work budget, so everything from here to the end of the file (%d bytes) was discarded WITHOUT proof that it was unreadable",
			off, end-off)
	case next > 0:
		d.Reason = fmt.Sprintf("the frame at offset %d is damaged; the next intact record was found at offset %d, so exactly this record was discarded and the ones behind it were kept",
			off, next)
	default:
		d.Reason = fmt.Sprintf("the frame at offset %d is damaged and no intact record follows it anywhere in the file, so it and the %d bytes after it were discarded as a torn tail",
			off, end-off-1)
	}
	p.add(d)
	if end >= size {
		p.TailAt = off
	}
	return end, nil
}

// frameHeaderAt reads the 20 bytes at off, when there are that many left.
func frameHeaderAt(f *os.File, off, size int64) ([]byte, bool) {
	if size-off < FrameHeaderSize {
		return nil, false
	}
	hdr := make([]byte, FrameHeaderSize)
	if _, err := f.ReadAt(hdr, off); err != nil {
		return nil, false
	}
	if binary.BigEndian.Uint16(hdr[14:16]) != 0 {
		return nil, false // reserved is non-zero: this is not a header we wrote
	}
	return hdr, true
}

// rebuildFrame asks whether the damaged frame at off is a COMPLETE record whose
// LENGTH FIELD is the only thing wrong, on the hypothesis that it ends at end.
//
// It is the same checksum proof the pre-2026-08-02 code used as a VETO on
// truncation, repurposed: under the old refuse-to-start policy the only thing
// that could be done with "this record is really intact" was to refuse the
// repair; now the record can simply be kept, which loses nothing at all. Only
// the true length reproduces the stored checksum, so a match cannot be
// manufactured by a partial write -- an interrupted append leaves fewer bytes
// than the header declares, never a mangled header over a complete payload.
func rebuildFrame(f *os.File, off, end int64, hdr []byte) (Record, bool) {
	trueLen := end - off - FrameHeaderSize
	if trueLen < 0 || trueLen > MaxPayloadSize {
		return Record{}, false
	}
	payload := make([]byte, trueLen)
	if _, err := f.ReadAt(payload, off+FrameHeaderSize); err != nil && err != io.EOF {
		return Record{}, false
	}
	var probe [16]byte
	copy(probe[:], hdr[0:16])
	binary.BigEndian.PutUint32(probe[0:4], uint32(trueLen))
	if frameChecksum(probe[:], payload) != binary.BigEndian.Uint32(hdr[16:20]) {
		return Record{}, false
	}
	return Record{
		Index:   binary.BigEndian.Uint64(hdr[4:12]),
		Type:    Type(binary.BigEndian.Uint16(hdr[12:14])),
		Payload: payload,
		Offset:  off,
	}, true
}

// resyncFrom finds where good data resumes after damage at from: the offset of
// the next INTACT record, or -1 when there is none anywhere in the file.
//
// THIS IS THE FUNCTION THAT STOPS DAMAGE CASCADING. Before it existed, damage
// whose declared extent overshot the end of the file was either refused (which
// meant the server would not start) or cut (which deleted every intact record
// sitting behind it -- a single flipped bit in a mid-file length field removed
// eight committed records in a reviewer's probe). The search is anchored on the
// RECORD INDEX, not on the end of the file, precisely because "the file also
// has a torn tail" is the normal state of every file recovery is called on: an
// end-of-file anchor finds nothing in exactly the case recovery exists for.
//
// A candidate at offset o must satisfy, cheapest test first:
//
//   - reserved == 0, which the writer always writes and random damage rarely
//     reproduces;
//   - an index STRICTLY GREATER than the last surviving record's, and no
//     greater than the most records that could still fit in the file -- indices
//     are dense, so this is a narrow window and it is what makes the search
//     both cheap and selective;
//   - a payload length that is legal and that fits inside the file;
//   - and finally the writer's own checksum over the bytes actually there.
//
// The declared boundary of the damaged frame is tried FIRST, before any
// scanning: when only a payload byte was flipped, the length field is still
// correct and the next record starts exactly there, so the common case costs
// one checksum rather than a walk.
//
// WHAT THIS SEARCH CANNOT DO, stated honestly: the checksum is CRC32C, which is
// unkeyed, so a client who can get a chosen payload into the log can put a
// byte sequence in it that this search will accept as a record. That is a known
// property of the format, not of this function (see format.go), and it is being
// closed by replacing CRC32C with a keyed MAC in a separate task -- at which
// point a candidate a client could forge stops existing. Until then the index
// window is the only thing narrowing it, and it is narrow.
func resyncFrom(f *os.File, size, from int64, lastIndex uint64, declared int64) (int64, bool, error) {
	budget := resyncBudget{}

	if declared > 0 && from+declared+FrameHeaderSize <= size {
		ok, err := validFrameAt(f, size, from+declared, lastIndex, &budget)
		if err != nil {
			return 0, false, err
		}
		if ok {
			return from + declared, false, nil
		}
	}

	buf := make([]byte, resyncChunk+FrameHeaderSize)
	for base := from + 1; base+FrameHeaderSize <= size; base += resyncChunk {
		n := size - base
		if n > int64(len(buf)) {
			n = int64(len(buf))
		}
		if _, err := f.ReadAt(buf[:n], base); err != nil && err != io.EOF {
			return 0, false, fmt.Errorf("wal: repair: read %d bytes at offset %d while searching for the next intact record: %w", n, base, err)
		}
		for i := int64(0); i+FrameHeaderSize <= n; i++ {
			o := base + i
			if o+FrameHeaderSize > size {
				break
			}
			hdr := buf[i : i+FrameHeaderSize]
			if binary.BigEndian.Uint16(hdr[14:16]) != 0 {
				continue
			}
			idx := binary.BigEndian.Uint64(hdr[4:12])
			if idx <= lastIndex || idx > lastIndex+uint64((size-o)/FrameHeaderSize)+1 {
				continue
			}
			payloadLen := int64(binary.BigEndian.Uint32(hdr[0:4]))
			if payloadLen > MaxPayloadSize || o+FrameHeaderSize+payloadLen > size {
				continue
			}
			ok, err := verifyFrame(f, o, hdr, payloadLen, &budget)
			if err != nil {
				return 0, false, err
			}
			if budget.exhausted {
				return -1, true, nil
			}
			if ok {
				return o, false, nil
			}
		}
		if base+resyncChunk <= base { // overflow guard; unreachable for real files
			break
		}
	}
	return -1, false, nil
}

// resyncBudget is the shared work allowance of one forward search.
type resyncBudget struct {
	candidates int
	bytes      int64
	exhausted  bool
}

func (b *resyncBudget) spend(n int64) bool {
	b.candidates++
	b.bytes += n
	if b.candidates > maxResyncCandidates || b.bytes > maxResyncChecksumBytes {
		b.exhausted = true
		return false
	}
	return true
}

// validFrameAt applies the full candidate test at a single offset.
func validFrameAt(f *os.File, size, o int64, lastIndex uint64, budget *resyncBudget) (bool, error) {
	hdr, ok := frameHeaderAt(f, o, size)
	if !ok {
		return false, nil
	}
	idx := binary.BigEndian.Uint64(hdr[4:12])
	if idx <= lastIndex || idx > lastIndex+uint64((size-o)/FrameHeaderSize)+1 {
		return false, nil
	}
	payloadLen := int64(binary.BigEndian.Uint32(hdr[0:4]))
	if payloadLen > MaxPayloadSize || o+FrameHeaderSize+payloadLen > size {
		return false, nil
	}
	return verifyFrame(f, o, hdr, payloadLen, budget)
}

// verifyFrame reads a candidate's payload and checks the record checksum,
// spending the search's work budget.
func verifyFrame(f *os.File, o int64, hdr []byte, payloadLen int64, budget *resyncBudget) (bool, error) {
	if !budget.spend(payloadLen) {
		return false, nil
	}
	payload := make([]byte, payloadLen)
	if _, err := f.ReadAt(payload, o+FrameHeaderSize); err != nil && err != io.EOF {
		return false, fmt.Errorf("wal: repair: read a %d-byte payload at offset %d while searching for the next intact record: %w", payloadLen, o+FrameHeaderSize, err)
	}
	return frameChecksum(hdr[0:16], payload) == binary.BigEndian.Uint32(hdr[16:20]), nil
}

// checkSalvageHeader classifies the file header, which is the one place the
// damage/unreadable line has to be drawn by hand.
//
//   - The header verifies: nothing to do.
//   - The header does not verify but the magic and version are ours, or are
//     garbage: DAMAGE. It is rebuilt, because the header's content is a
//     constant -- there is nothing in it to recover, only 16 bytes to rewrite
//     -- and refusing to start over a flipped bit in a fixed preamble would
//     throw away an entire readable log.
//   - The magic names the OTHER kind of log: NOT damage. An audit file is not a
//     WAL, and "repairing" its header would relabel it and replay records that
//     were never write-ahead records. Fatal.
//   - The version is a different number under OUR magic: NOT damage. This
//     binary does not implement that layout, and guessing at it is how a
//     downgrade eats a log. Fatal.
func checkSalvageHeader(f *os.File, path string, kind Kind, size int64, plan *repairPlan) error {
	if size < FileHeaderSize {
		plan.HeaderDamaged = true
		return nil
	}
	hdr := make([]byte, FileHeaderSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return fmt.Errorf("wal: repair %s: read the file header: %w", path, err)
	}
	magic := string(hdr[0:8])
	version := binary.BigEndian.Uint32(hdr[8:12])

	if got := kindForMagic(magic); got != 0 && got != kind {
		return corruptf(path, 0, "file is a %s file, want a %s file; recovery will not reinterpret one log as the other", got, kind)
	}
	if magic == kind.magic() && version != FormatVersion {
		return corruptf(path, 0, "format version %d, want %d; this binary does not implement that layout and recovery will not guess at it", version, FormatVersion)
	}
	if err := parseFileHeader(hdr, path, kind); err != nil {
		plan.HeaderDamaged = true
	}
	return nil
}

// rewriteLog rebuilds path from the records that survive salvage: a fresh file
// header followed by every surviving frame, in file order, with their ORIGINAL
// INDICES. Survivors are never renumbered -- an index is an identity (invariant
// 1) and reissuing one would let two different messages share an id -- so the
// rebuilt file has HOLES where records were discarded, which is why scanFrom
// accepts a rising index rather than a dense one.
//
// It is crash-safe by construction: everything is written to a temporary file
// in the same directory, fsynced, and then renamed over the original, which is
// atomic. A crash at any point before the rename leaves the ORIGINAL file
// exactly as it was, so the worst case is that the same repair runs again.
func rewriteLog(path string, kind Kind) (repairPlan, error) {
	tmp := path + ".repair"
	// A stale temporary from a crashed repair is meaningless: it is a partial
	// copy of a file that is still intact. Remove it rather than reuse it.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return repairPlan{}, fmt.Errorf("wal: repair %s: remove the stale temporary %s: %w", path, tmp, err)
	}

	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return repairPlan{}, fmt.Errorf("wal: repair %s: create the temporary %s: %w", path, tmp, err)
	}
	cleanup := func() {
		f.Close()
		os.Remove(tmp)
	}

	bw := bufio.NewWriter(f)
	if _, err := bw.Write(makeFileHeader(kind)); err != nil {
		cleanup()
		return repairPlan{}, fmt.Errorf("wal: repair %s: write the file header of %s: %w", path, tmp, err)
	}
	// encodeFrame reproduces a surviving frame BYTE FOR BYTE: the checksum this
	// walk verified covers the length, the index, the type and the reserved
	// field as well as the payload, so every input to encodeFrame is already
	// proven to be what was on disk. For a frame whose length field was rebuilt
	// it emits the frame the writer originally wrote, which is the whole point
	// of the proof in rebuildFrame.
	plan, err := salvage(path, kind, func(rec Record) error {
		_, werr := bw.Write(encodeFrame(rec.Index, rec.Type, rec.Payload))
		return werr
	})
	if err != nil {
		cleanup()
		return plan, err
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return plan, fmt.Errorf("wal: repair %s: flush %s: %w", path, tmp, err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return plan, fmt.Errorf("wal: repair %s: fsync %s: %w", path, tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return plan, fmt.Errorf("wal: repair %s: close %s: %w", path, tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return plan, fmt.Errorf("wal: repair %s: rename %s over it: %w", path, tmp, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return plan, fmt.Errorf("wal: repair %s: fsync the directory after the rename: %w", path, err)
	}
	return plan, nil
}

// quarantine moves a log that cannot be interpreted at all out of the way, so
// that startup can continue with a fresh one.
//
// It RENAMES rather than deletes, always. The policy is "prefer to discard
// corruption, with logging" -- but a file this code cannot read is not
// necessarily a file NOBODY can read, and an operator with a hex editor is
// owed the bytes. The new name carries a timestamp so repeated quarantines do
// not overwrite each other.
func quarantine(path string) (string, error) {
	dest := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UTC().UnixNano())
	if err := os.Rename(path, dest); err != nil {
		return "", fmt.Errorf("wal: repair %s: move the unreadable log aside to %s: %w", path, dest, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return dest, fmt.Errorf("wal: repair %s: fsync the directory after moving the unreadable log to %s: %w", path, dest, err)
	}
	return dest, nil
}
