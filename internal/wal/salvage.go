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

	// LastIndex is the highest record index that SURVIVED. It is the walk's
	// ordering cursor -- a record whose index does not exceed it is out of order
	// and goes -- and it is NOT the answer to "where does the next append
	// start". See HighestSeen for that.
	LastIndex uint64

	// HighestSeen is the highest record index the walk OBSERVED ANYWHERE in the
	// file: survivors AND discarded records alike.
	//
	// This is the field recovery resumes from, and the distinction from
	// LastIndex is the whole of defect e120153b. One past the highest SURVIVOR
	// hands a discarded tail record's index straight back out, and recovery
	// cannot tell a torn write from a bit flipped in a record that was fsynced
	// and acknowledged -- so that index may be one a client has already seen.
	// Invariant 1 was reaffirmed WITHOUT narrowing on 2026-08-02: when recovery
	// discards a record the index advances past the hole, it never rewinds.
	HighestSeen uint64

	// LostUnidentified reports that the walk threw away bytes it could not
	// attribute to a known set of record indices, so HighestSeen is a LOWER
	// BOUND on what this file once carried rather than the answer.
	//
	// The reasoning, because it is not obvious: a discarded FRAMING-stage region
	// is a BYTE RANGE, and a range wide enough for two frames may have held a
	// second record whose index was never readable. A known index inside a
	// discard therefore proves a lower bound on what was lost, NEVER an upper
	// one. So an identified discard is necessary but not sufficient, and the
	// durable index floor (see indexfloor.go) is what covers the rest: when this
	// is set, Open resumes from the floor's CEILING -- the highest index this
	// data directory ever authorised -- instead of from the file's arithmetic.
	LostUnidentified bool

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

	// HeaderMagicOK and HeaderVersion are what the FILE HEADER'S first twelve
	// bytes SAY, whether or not the header's tag verified. They exist for
	// exactly one decision, and it is the wrong-key one: a header that is
	// structurally ours and claims the current version, whose tag does not
	// verify, and behind which not one record verifies either, is
	// indistinguishable from a log opened under the WRONG KEY -- so recovery
	// refuses rather than quarantining it. See repairLog.
	HeaderMagicOK bool
	HeaderVersion uint32
}

func (p *repairPlan) add(d Discard) {
	p.Count++
	p.Bytes += d.Length
	if len(p.Discards) < maxDiscardsRetained {
		p.Discards = append(p.Discards, d)
	}
}

// seeIndex records that this index was OBSERVED in the file, whether the record
// carrying it survived or not. It only ever raises HighestSeen: an index that
// was once authorised stays authorised however the walk later feels about the
// bytes around it.
func (p *repairPlan) seeIndex(idx uint64) {
	if idx > p.HighestSeen {
		p.HighestSeen = idx
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
func salvage(path string, kind Kind, c codec, keep func(Record) error) (repairPlan, error) {
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

	if err := checkSalvageHeader(f, path, kind, c, size, &plan); err != nil {
		return plan, err
	}

	headerSize := c.fileHeaderSize()
	off := headerSize
	if size < headerSize {
		// Nothing but a torn header. There is no record in these bytes, so the
		// whole file is the discard.
		plan.add(Discard{Stage: "framing", Offset: 0, Length: size,
			Reason: fmt.Sprintf("the file is %d bytes, too short to hold even a %d-byte file header", size, headerSize)})
		plan.TailAt = 0
		// Nothing in these bytes was readable, so nothing about the indices this
		// file once carried can be asserted from them.
		plan.LostUnidentified = true
		return plan, nil
	}

	var br *bufio.Reader
	for off < size {
		if br == nil {
			br = bufio.NewReader(io.NewSectionReader(f, off, size-off))
		}

		rec, err := readFrame(c, br, path, off)
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
			resumeAt, err := plan.recoverAfterDamage(f, c, off, size, keep)
			if err != nil {
				return plan, err
			}
			off = resumeAt
			br = nil
			continue
		}

		if rec.Index <= plan.LastIndex {
			// The frame's tag verified, so these bytes are exactly what
			// some writer wrote -- but they are in the wrong place: an old
			// record resurrected under us, or the same record twice. Keeping it
			// would replay history out of order, so it goes, loudly.
			plan.add(Discard{Stage: "framing", Offset: off, Length: rec.frameSize(),
				Index: rec.Index, Type: rec.Type, TypeKnown: true,
				Reason: fmt.Sprintf("record index %d does not follow the previous surviving record (index %d): the record is intact but out of order, so it is an old record resurrected in place or a duplicate",
					rec.Index, plan.LastIndex)})
			// This discard does NOT set LostUnidentified. Its tag verified, so
			// its index is known EXACTLY -- there is no byte range here whose
			// contents could not be read, which is the condition that flag
			// describes. seeIndex is called anyway, and is a no-op today because
			// this branch only runs when the index is not above LastIndex; it is
			// here so a future change to the ordering rule cannot silently drop
			// an observed index.
			plan.seeIndex(rec.Index)
			off += rec.frameSize()
			continue
		}
		plan.LastIndex = rec.Index
		plan.seeIndex(rec.Index)
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
func (p *repairPlan) recoverAfterDamage(f *os.File, c codec, off, size int64, keep func(Record) error) (int64, error) {
	hdr, hdrOK := frameHeaderAt(f, c, off, size)

	// The declared extent of the damaged frame. When only the PAYLOAD is
	// damaged the length field is still right, so the next record starts
	// exactly there -- one verification instead of a byte-by-byte search.
	declared := int64(-1)
	if hdrOK {
		if n := int64(binary.BigEndian.Uint32(hdr[0:4])); n <= MaxPayloadSize {
			declared = c.frameHeaderSize() + n
		}
	}

	next, exhausted, err := resyncFrom(f, c, size, off, p.LastIndex, declared)
	if err != nil {
		return 0, err
	}
	if exhausted {
		p.Exhausted = true
		// The search gave up, so everything from here to the end of the file went
		// without proof that any of it was unreadable -- and without any way to
		// name the indices that went with it.
		p.LostUnidentified = true
	}

	end := size
	if next > 0 {
		end = next
	}

	// Is the damaged frame actually COMPLETE, with only its length field wrong?
	// Records are contiguous, so the frame's true end is where the next
	// surviving record begins (or the end of the file). Rewriting the length to
	// that and asking the writer's own tag is a PROOF, not a guess: only the
	// true length reproduces the stored value. When it matches, every byte the
	// record needs is present and it is kept rather than thrown away.
	if hdrOK {
		if rec, ok := rebuildFrame(f, c, off, end, hdr); ok && rec.Index > p.LastIndex {
			p.LastIndex = rec.Index
			p.seeIndex(rec.Index)
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

	// This region is going, and its contents can no longer be enumerated: it is a
	// BYTE RANGE, not a record, and a range this wide may have held more records
	// than the one header at its start describes. Say so, so that Open consults
	// the durable ceiling rather than trusting this file's own arithmetic.
	p.LostUnidentified = true

	d := Discard{Stage: "framing", Offset: off, Length: end - off}
	// indexRejected records that the discarded frame's DECLARED index was not
	// plausible for this file and was therefore not believed. It is appended to
	// the reason below so the decision is visible to an operator rather than
	// silent.
	indexRejected := ""
	if hdrOK {
		idx := binary.BigEndian.Uint64(hdr[4:12])
		d.Index = idx
		d.Type = Type(binary.BigEndian.Uint16(hdr[12:14]))
		d.TypeKnown = true

		// SECURITY: this index comes from a frame header whose MAC DID NOT
		// VERIFY. On a WAL the payload is client-supplied, so these bytes are
		// ATTACKER-INFLUENCED: one flipped or forged length-and-index field
		// could otherwise set the durable floor to near MaxUint64 and
		// permanently exhaust this bus's id space -- a denial of service that
		// survives every restart, because the floor is deliberately never
		// lowered.
		//
		// So the declared index is believed only when it is PLAUSIBLE FOR THIS
		// FILE: it must advance on the last survivor, and by no more than the
		// number of records the remaining bytes could physically have held (a
		// region of B bytes cannot have held more than B/frameHeaderSize
		// records, and the +1 admits the frame at off itself).
		//
		// The bound only ever makes recovery LESS aggressive, and that is safe
		// because it is not the last line of defence: the DURABLE FLOOR is the
		// trusted backstop for whatever this declines to believe. Rejecting a
		// real index costs at most a smaller jump here, and LostUnidentified
		// above already sends Open to the ceiling.
		maxAdvance := (size-off)/c.frameHeaderSize() + 1
		if idx > p.LastIndex && idx-p.LastIndex <= uint64(maxAdvance) {
			p.seeIndex(idx)
		} else {
			indexRejected = fmt.Sprintf(" the damaged frame declares record index %d, which is not plausible for this file (the last surviving index is %d and the %d bytes from here to the end of the file could hold at most %d records), so the declared index was NOT used to advance the index sequence -- it comes from a header whose MAC did not verify and is therefore untrusted input; recovery resumes from the durable index floor instead.",
				idx, p.LastIndex, size-off, maxAdvance)
		}
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
	d.Reason += indexRejected
	p.add(d)
	if end >= size {
		p.TailAt = off
	}
	return end, nil
}

// frameHeaderAt reads one frame header's worth of bytes at off, when there are
// that many left.
func frameHeaderAt(f *os.File, c codec, off, size int64) ([]byte, bool) {
	headerSize := c.frameHeaderSize()
	if size-off < headerSize {
		return nil, false
	}
	hdr := make([]byte, headerSize)
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
// It is the same tag check the pre-2026-08-02 code used as a VETO on
// truncation, repurposed: under the old refuse-to-start policy the only thing
// that could be done with "this record is really intact" was to refuse the
// repair; now the record can simply be kept, which loses nothing at all.
//
// HOW STRONG THIS IS, stated exactly, and it is stronger since format version 2.
//
// The length field is INSIDE the covered range, so only the true length
// reproduces the stored tag. Against ACCIDENT that was already overwhelming
// evidence -- an interrupted append leaves fewer bytes than the header declares,
// never a mangled header over a complete payload.
//
// What changed is the ADVERSARIAL half. Under version 1 this was a 32-bit
// unkeyed CRC32C: about one wrong length in 2^32 collides by chance, and anyone
// who could choose payload bytes could construct a collision ON PURPOSE, so the
// check held against accident only. Under version 2 the tag is HMAC-SHA256 over
// a key the client does not hold, so a chosen-payload collision is no longer a
// thing an attacker can compute: THE LENGTH-FIELD REPAIR IS NOW A REAL PROOF
// AGAINST AN ADVERSARY, not merely against accident.
//
// The honest limit, unchanged: none of this helps against an attacker who
// already has WRITE ACCESS TO THE DATA DIRECTORY, because the key file sits in
// that directory and such an attacker can simply read it and forge whatever
// they like (DECISIONS.md 2026-08-02). The MAC defends the log from its own
// CLIENTS, not from someone who owns the disk.
func rebuildFrame(f *os.File, c codec, off, end int64, hdr []byte) (Record, bool) {
	headerSize := c.frameHeaderSize()
	trueLen := end - off - headerSize
	if trueLen < 0 || trueLen > MaxPayloadSize {
		return Record{}, false
	}
	payload := make([]byte, trueLen)
	if _, err := f.ReadAt(payload, off+headerSize); err != nil && err != io.EOF {
		return Record{}, false
	}
	probe := make([]byte, frameCoveredBytes)
	copy(probe, hdr[0:frameCoveredBytes])
	binary.BigEndian.PutUint32(probe[0:4], uint32(trueLen))
	if !c.verifyTag(probe, payload, hdr[frameCoveredBytes:headerSize]) {
		return Record{}, false
	}
	return Record{
		Index:    binary.BigEndian.Uint64(hdr[4:12]),
		Type:     Type(binary.BigEndian.Uint16(hdr[12:14])),
		Payload:  payload,
		Offset:   off,
		legacyV1: c.isV1(),
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
//   - and finally the writer's own tag over the bytes actually there.
//
// The declared boundary of the damaged frame is tried FIRST, before any
// scanning: when only a payload byte was flipped, the length field is still
// correct and the next record starts exactly there, so the common case costs
// one verification rather than a walk.
//
// WHAT THIS SEARCH IS AND IS NOT PROTECTED AGAINST, stated honestly.
//
// Under format version 1 the only authenticity check available was CRC32C,
// which is UNKEYED, so a client who could get chosen bytes into a payload could
// embed a byte sequence this search accepts as a record -- and because the first
// candidate in file order wins, they did not even need to know the current
// index; a ladder of ascending forged indices would do. Security demonstrated
// exactly that against a hand-built file: forged prepare+commit frames were
// admitted, copied into the rewritten log by rewriteLog as if genuine, and
// delivered to the Applier as accepted history.
//
// FORMAT VERSION 2 CLOSES THAT OUTRIGHT. The tag is HMAC-SHA256 over a key that
// lives in the data directory and that no client ever sees, so a client cannot
// compute a tag for bytes it chooses, and this search cannot be made to accept
// a payload-embedded frame. The forgery argument is DEAD, not mitigated.
//
// What that retires, and it is worth saying because it used to be the ONLY thing
// holding the forgery out: the argument that A FRAME HEADER CANNOT BE EXPRESSED
// IN A WAL PAYLOAD -- every header contains NUL bytes, and the sole writer of
// WAL payloads runs bodies through canonicalBody -> json.Compact, which rejects
// a raw control byte in a string. That was true only by ACCIDENT, and it was
// load-bearing. It is no longer load-bearing: the payload channel may now widen
// to arbitrary bytes (binary bodies, compression, E2E ciphertext) without this
// search becoming forgeable. TestWALPayloadsCannotCarryAFrameHeader may stay as
// defence in depth, but nothing here depends on it any more.
//
// THE LIMIT THAT REMAINS: none of this helps against an attacker who already has
// WRITE ACCESS TO THE DATA DIRECTORY. The key file sits in that directory; such
// an attacker reads it and forges freely. That is an accepted, recorded limit
// (DECISIONS.md 2026-08-02), not an oversight.
//
// AND WHAT THE MAC DOES NOT SOLVE, so that none of the machinery below is
// mistaken for redundant: the MAC answers "are these bytes intact and ours?".
// It does not answer "WHERE IS THE NEXT RECORD?". Finding that offset in a file
// with a hole blown in it is what the two stages, the density window and
// rebuildFrame are for, and the MAC makes each of those checks STRONGER -- their
// final test is now unforgeable -- rather than unnecessary. Removing any of them
// would reintroduce a data-loss bug that has already been measured once (see
// stage 2).
func resyncFrom(f *os.File, c codec, size, from int64, lastIndex uint64, declared int64) (int64, bool, error) {
	// STAGE 0: the damaged frame's own declared boundary. When only a payload
	// byte was flipped the length field is still correct, so the next record
	// starts exactly there and the whole search costs one verification.
	if declared > 0 && from+declared+c.frameHeaderSize() <= size {
		// One candidate cannot exhaust a 4096-verification, 64 MiB budget: a
		// single payload is at most MaxPayloadSize. The budget is passed for
		// uniformity, not because it can fire here.
		budget := resyncBudget{}
		ok, err := validFrameAt(f, c, size, from+declared, lastIndex, &budget)
		if err != nil {
			return 0, false, err
		}
		if ok {
			return from + declared, false, nil
		}
	}

	// STAGE 1: scan with the DENSITY window, which is a tight filter and keeps
	// the ordinary case cheap.
	o, exhausted, err := scanForFrame(f, c, size, from, lastIndex, true)
	if err != nil || o >= 0 || exhausted {
		return o, exhausted, err
	}

	// STAGE 2: scan again WITHOUT the density window.
	//
	// This stage exists because of a data-loss bug security reproduced against
	// stage 1 alone, and the reasoning behind it is the whole of why it must
	// stay. A repaired log has permanent index HOLES -- survivors are never
	// renumbered -- so the gap between the last survivor's index and the next
	// record's can be arbitrarily large. The density window bounds a candidate's
	// index by how many records could still FIT before the end of the file,
	// which after a big hole is smaller than the real next index. The genuine
	// next record was then rejected by a cheap filter, the search reported "no
	// intact record follows", and recovery deleted every committed record to the
	// end of the file WHILE LOGGING THAT IT HAD FOUND A TORN TAIL. Measured: a
	// log with indices 1, 2, 50001, 50002 and one flipped bit in a length field
	// lost an acknowledged write and reported no error.
	//
	// So the rule is: A BOUNDED SEARCH FINDING NOTHING IS NEVER ON ITS OWN
	// GROUNDS FOR "NOTHING FOLLOWS". Stage 2 keeps the two filters that are
	// actually sound -- an index strictly greater than the last survivor's, and
	// the record's own tag -- and drops only the heuristic one. It runs with a
	// FRESH budget so that a stage-1 near-miss cannot starve it, and it costs
	// nothing in the common case because stage 1 almost always answers first.
	return scanForFrame(f, c, size, from, lastIndex, false)
}

// scanForFrame walks [from+1, size) for the first offset holding an intact
// record whose index follows lastIndex, and returns -1 when there is none.
//
// dense selects the extra index heuristic: with it on, a candidate's index must
// also be no larger than the number of records that could still fit before the
// end of the file. That is a cheap, sharp filter and it is WRONG on its own --
// see the stage-2 comment in resyncFrom -- so it is only ever used as a first
// pass whose failure is not conclusive.
//
// Each call carries its own budget. Exhausting it is reported rather than
// hidden, because a search that gave up is not the same fact as a search that
// finished and found nothing.
func scanForFrame(f *os.File, c codec, size, from int64, lastIndex uint64, dense bool) (int64, bool, error) {
	budget := resyncBudget{}
	headerSize := c.frameHeaderSize()
	buf := make([]byte, int64(resyncChunk)+headerSize)
	for base := from + 1; base+headerSize <= size; base += resyncChunk {
		n := size - base
		if n > int64(len(buf)) {
			n = int64(len(buf))
		}
		if _, err := f.ReadAt(buf[:n], base); err != nil && err != io.EOF {
			return 0, false, fmt.Errorf("wal: repair: read %d bytes at offset %d while searching for the next intact record: %w", n, base, err)
		}
		for i := int64(0); i+headerSize <= n; i++ {
			o := base + i
			hdr := buf[i : i+headerSize]
			if binary.BigEndian.Uint16(hdr[14:16]) != 0 {
				continue
			}
			idx := binary.BigEndian.Uint64(hdr[4:12])
			if idx <= lastIndex {
				continue
			}
			if dense && idx > lastIndex+uint64((size-o)/headerSize)+1 {
				continue
			}
			payloadLen := int64(binary.BigEndian.Uint32(hdr[0:4]))
			if payloadLen > MaxPayloadSize || o+headerSize+payloadLen > size {
				continue
			}
			ok, err := verifyFrame(f, c, o, hdr, payloadLen, &budget)
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
func validFrameAt(f *os.File, c codec, size, o int64, lastIndex uint64, budget *resyncBudget) (bool, error) {
	hdr, ok := frameHeaderAt(f, c, o, size)
	if !ok {
		return false, nil
	}
	// NO density bound here, deliberately: this offset is not a guess, it is the
	// boundary the damaged frame itself declared, and after a repair the index
	// gap across a hole can be arbitrarily large (see resyncFrom stage 2).
	if idx := binary.BigEndian.Uint64(hdr[4:12]); idx <= lastIndex {
		return false, nil
	}
	payloadLen := int64(binary.BigEndian.Uint32(hdr[0:4]))
	if payloadLen > MaxPayloadSize || o+c.frameHeaderSize()+payloadLen > size {
		return false, nil
	}
	return verifyFrame(f, c, o, hdr, payloadLen, budget)
}

// verifyFrame reads a candidate's payload and checks the record's tag, spending
// the search's work budget.
func verifyFrame(f *os.File, c codec, o int64, hdr []byte, payloadLen int64, budget *resyncBudget) (bool, error) {
	if !budget.spend(payloadLen) {
		return false, nil
	}
	headerSize := c.frameHeaderSize()
	payload := make([]byte, payloadLen)
	if _, err := f.ReadAt(payload, o+headerSize); err != nil && err != io.EOF {
		return false, fmt.Errorf("wal: repair: read a %d-byte payload at offset %d while searching for the next intact record: %w", payloadLen, o+headerSize, err)
	}
	return c.verifyTag(hdr[0:frameCoveredBytes], payload, hdr[frameCoveredBytes:headerSize]), nil
}

// checkSalvageHeader classifies the file header, which is the one place the
// damage/unreadable line has to be drawn by hand.
//
//   - The header verifies: nothing to do.
//   - The header does not verify but the magic and version are ours, or are
//     garbage: DAMAGE. It is rebuilt, because the header's content is a
//     constant -- there is nothing in it to recover, only a fixed preamble to
//     rewrite -- and refusing to start over a flipped bit there would throw
//     away an entire readable log.
//   - The magic names the OTHER kind of log: NOT damage. An audit file is not a
//     WAL, and "repairing" its header would relabel it and replay records that
//     were never write-ahead records. Fatal.
//   - The version is a different number under OUR magic: NOT damage. This
//     binary does not implement that layout, and guessing at it is how a
//     downgrade eats a log. Fatal.
//
// It also records what the header SAYS -- HeaderMagicOK and HeaderVersion --
// whether or not it verified, because that is what lets repairLog separate
// "the header is damaged" from "the key is wrong". See repairLog.
func checkSalvageHeader(f *os.File, path string, kind Kind, c codec, size int64, plan *repairPlan) error {
	headerSize := c.fileHeaderSize()
	if size < headerSize {
		plan.HeaderDamaged = true
		return nil
	}
	hdr := make([]byte, headerSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return fmt.Errorf("wal: repair %s: read the file header: %w", path, err)
	}
	magic := string(hdr[0:8])
	version := binary.BigEndian.Uint32(hdr[8:12])
	plan.HeaderMagicOK = magic == kind.magic()
	plan.HeaderVersion = version

	if got := kindForMagic(magic); got != 0 && got != kind {
		return corruptf(path, 0, "file is a %s file, want a %s file; recovery will not reinterpret one log as the other", got, kind)
	}
	if plan.HeaderMagicOK && version != c.version {
		return corruptf(path, 0, "format version %d, want %d; this binary does not implement that layout and recovery will not guess at it", version, c.version)
	}
	if err := c.parseFileHeader(hdr, path, kind); err != nil {
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
func rewriteLog(path string, kind Kind, c codec, want repairPlan) (repairPlan, error) {
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
	if _, err := bw.Write(c.makeFileHeader(kind)); err != nil {
		cleanup()
		return repairPlan{}, fmt.Errorf("wal: repair %s: write the file header of %s: %w", path, tmp, err)
	}
	// A REPAIR REWRITES THE FILE IN THE VERSION IT ALREADY IS -- c is the codec
	// the walk above read it with, so a version 1 log is repaired as version 1
	// and upgradeV1 converts it afterwards. That ordering is forced: the
	// upgrade's strict scan cannot read a damaged log, so the damage has to go
	// first. This is the ONE caller permitted to encode a legacy frame (see
	// codec.encodeFrame); the file it produces is immediately upgraded and is
	// never appended to in version 1.
	//
	// encodeFrame reproduces a surviving frame BYTE FOR BYTE: the tag this walk
	// verified covers the length, the index, the type and the reserved field as
	// well as the payload, so every input to encodeFrame is already proven to be
	// what was on disk. For a frame whose length field was rebuilt it emits the
	// frame the writer originally wrote, which is the whole point of the proof
	// in rebuildFrame.
	plan, err := salvage(path, kind, c, func(rec Record) error {
		_, werr := bw.Write(c.encodeFrame(rec.Index, rec.Type, rec.Payload))
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
	// CHECKED BEFORE THE RENAME, deliberately. The salvage walk runs twice over
	// the same bytes -- once to decide, once to copy -- and the whole design
	// rests on it being a pure function of those bytes. Verifying afterwards
	// could only ever report the bug; verifying here PREVENTS it, and the
	// original file is still untouched at this point, so the failure costs
	// nothing but a temporary file. A disagreement is not damage in the file --
	// it can only be a bug in recovery itself -- so it is fatal, and the
	// always-restart policy does not cover it: restarting into a file rewritten
	// from a decision nothing verified is worse than not starting.
	if plan.Kept != want.Kept || plan.Rebuilt != want.Rebuilt || plan.Count != want.Count {
		cleanup()
		return plan, fmt.Errorf("wal: repair %s: the two salvage passes disagreed (deciding pass kept %d, rebuilt %d, discarded %d; copying pass kept %d, rebuilt %d, discarded %d); this is a bug in recovery, not damage in the file, and the log has NOT been changed",
			path, want.Kept, want.Rebuilt, want.Count, plan.Kept, plan.Rebuilt, plan.Count)
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
