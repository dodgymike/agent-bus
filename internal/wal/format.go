package wal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
)

// On-disk sizes and limits. These are format constants: changing any of them
// changes the bytes on disk and therefore requires a FormatVersion bump.
const (
	// FormatVersion is the on-disk format version, reserved through the Spec
	// Server "ondisk-format-version" namespace. It is recorded in the file
	// header and a reader refuses a file that does not carry exactly this
	// value -- with the single, deliberate exception of version 1, which is
	// still READ so that an existing bus can be upgraded in place (see
	// upgradeV1). A WAL is replayed byte-for-byte, so guessing at an unknown
	// layout is never the safe move.
	//
	// Version 2 replaced the unkeyed CRC32C of version 1 with a keyed
	// HMAC-SHA256 (DUR-12, DECISIONS.md 2026-08-02). A CRC is an
	// error-detecting code, not an integrity primitive: an ordinary enrolled
	// client that can get chosen bytes into a payload could craft a byte
	// sequence recovery accepts as a record. A client cannot compute a tag over
	// a key it does not hold.
	FormatVersion = 2

	// formatVersionV1 is the legacy layout: a 16-byte file header and a
	// 20-byte frame header, both authenticated by nothing stronger than a
	// CRC32C. It is READ-ONLY. The only thing this binary ever writes in it is
	// the in-place repair that has to precede an upgrade (see codec.encodeFrame).
	formatVersionV1 = 1

	// MACSize is the length of an HMAC-SHA256 tag. It is the full 32 bytes:
	// truncating a MAC is a design decision about a security margin, and this
	// package does not make those (invariant 9).
	MACSize = 32

	// FileHeaderSize is the size of the version 2 file header written once at
	// file creation: magic[8] ++ version[4] ++ reserved[4] ++ mac[32].
	FileHeaderSize = 16 + MACSize

	// fileHeaderSizeV1 is the version 1 file header: magic[8] ++ version[4] ++
	// crc32c-of-the-first-12-bytes[4].
	fileHeaderSizeV1 = 16

	// FrameHeaderSize is the size of the version 2 per-record header that
	// precedes each payload: payloadLen[4] ++ index[8] ++ type[2] ++
	// reserved[2] ++ mac[32].
	FrameHeaderSize = frameCoveredBytes + MACSize

	// frameHeaderSizeV1 is the version 1 per-record header, whose tag is a
	// 4-byte CRC32C rather than a 32-byte MAC.
	frameHeaderSizeV1 = frameCoveredBytes + crcSize

	// frameCoveredBytes is how many bytes of a frame header precede the tag,
	// and therefore how many of them the tag COVERS. It is 16 in both versions,
	// so the tag always begins at offset 16 of a frame and only its LENGTH
	// changes with the version.
	frameCoveredBytes = 16

	// crcSize is the width of a version 1 tag.
	crcSize = 4

	// MaxPayloadSize bounds a single record's payload at 1 MiB. It is a
	// safety bound, not a tuning knob: a reader that sees a larger length
	// treats the frame as corrupt WITHOUT attempting the allocation, which is
	// the difference between detecting corruption and being OOM-killed by it.
	MaxPayloadSize = 1 << 20
)

// crcTable is the CRC-32 Castagnoli table, built exactly once. It survives
// solely to READ version 1 files. Castagnoli was used rather than the IEEE
// polynomial because it has better error detection for short records and
// because crc32 uses the SSE4.2 hardware instruction for it.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// codec is the on-disk format one file is read and written in: which version
// its frames are laid out in, and -- for version 2 -- the HMAC-SHA256 key that
// authenticates them.
//
// It is a VALUE THREADED THROUGH THE FRAMING LAYER, deliberately, rather than a
// package-level global. A process routinely has two files open at once (the WAL
// and the audit log), and during an upgrade it has two VERSIONS of the SAME file
// open at once; a global would have to be swapped between them, which is exactly
// the kind of ambient state that makes a recovery bug unreproducible.
type codec struct {
	// version is 1 or 2.
	version uint32
	// key is the HMAC-SHA256 key, and is nil if and only if version is 1.
	// It is never logged, never put in an error message, and never compared
	// with anything.
	key []byte
}

// isV1 reports whether c reads and writes the legacy CRC32C layout.
func (c codec) isV1() bool { return c.version == formatVersionV1 }

// tagSize is the width of this version's authentication tag.
func (c codec) tagSize() int64 {
	if c.isV1() {
		return crcSize
	}
	return MACSize
}

// frameHeaderSize is the size of a record's frame header in this version.
func (c codec) frameHeaderSize() int64 { return frameCoveredBytes + c.tagSize() }

// fileHeaderSize is the size of the file header in this version.
func (c codec) fileHeaderSize() int64 {
	if c.isV1() {
		return fileHeaderSizeV1
	}
	return FileHeaderSize
}

// fileHeaderCovered is how many leading bytes of the file header the tag covers.
// The two versions differ: version 1 covers magic ++ version (12 bytes) and
// stores a CRC32C at [12:16]; version 2 covers magic ++ version ++ reserved
// (16 bytes) and stores a MAC at [16:48].
func (c codec) fileHeaderCovered() int64 {
	if c.isV1() {
		return 12
	}
	return frameCoveredBytes
}

// mac computes the HMAC-SHA256 tag over covered ++ payload.
//
// It is the single definition of a version 2 tag: the writer, the reader, the
// salvage pass and the upgrade all call it, so they cannot drift apart. The
// concatenation is unambiguous without a separator because the payload LENGTH is
// the first four bytes of covered.
//
// crypto/hmac and crypto/sha256 are stdlib and are used exactly as documented.
// Nothing here invents, adapts or assembles a construction (invariant 9).
func (c codec) mac(covered, payload []byte) []byte {
	if c.isV1() || len(c.key) == 0 {
		// Unreachable: a v2 codec always carries a key, and a v1 codec never
		// reaches here. Saying so loudly beats computing an HMAC under a zero
		// key, which would VERIFY and protect nothing.
		panic("wal: internal error: MAC requested without a key")
	}
	m := hmac.New(sha256.New, c.key)
	m.Write(covered)
	m.Write(payload)
	return m.Sum(nil)
}

// verifyTag reports whether stored is the correct tag for covered ++ payload.
//
// Version 2 compares with hmac.Equal, which is constant time: a tag comparison
// that leaks timing is a forgery oracle, and this is the only place a tag is
// ever compared. Version 1's CRC32C is compared with an ordinary integer ==,
// and that is not an oversight -- A CRC IS NOT A MAC. It is an unkeyed
// error-detecting code over bytes an adversary may choose, so there is no
// secret for a timing side channel to leak and comparing it in constant time
// would prove precisely nothing.
func (c codec) verifyTag(covered, payload, stored []byte) bool {
	if c.isV1() {
		if len(stored) != crcSize {
			return false
		}
		return frameChecksum(covered, payload) == binary.BigEndian.Uint32(stored)
	}
	if int64(len(stored)) != MACSize {
		return false
	}
	return hmac.Equal(c.mac(covered, payload), stored)
}

// Type is the record type stored in a frame header. The values are reserved
// through the Spec Server "record-type" namespace and are NOT free to change:
// they are written to disk and read back by every future version.
type Type uint16

// Reserved record types.
const (
	// TypePrepare is phase one of the two-phase write: the message exists and
	// its ids are burned, but it is not yet part of accepted history.
	TypePrepare Type = 1
	// TypeCommit is phase two: the prepared record at the referenced index is
	// now accepted history.
	TypeCommit Type = 2
	// TypeAbort marks a prepared record that will never commit.
	TypeAbort Type = 3
	// TypeAuditMessage is a message record in the append-only audit log
	// (invariant 6).
	TypeAuditMessage Type = 4
)

// String returns the lowercase wire name of the type.
func (t Type) String() string {
	switch t {
	case TypePrepare:
		return "prepare"
	case TypeCommit:
		return "commit"
	case TypeAbort:
		return "abort"
	case TypeAuditMessage:
		return "audit_message"
	default:
		return "unknown(" + strconv.FormatUint(uint64(t), 10) + ")"
	}
}

// Known reports whether t is one of the reserved record types. A writer must
// only ever append a known type; a reader deliberately does NOT reject an
// unknown one (see scanFrom) because a frame whose TAG verifies was written
// intact by some other version of this code -- and under version 2 that means
// written by something holding this log's key -- so refusing to read it would
// turn a forward-compatibility question into data loss.
func (t Type) Known() bool {
	return t >= TypePrepare && t <= TypeAuditMessage
}

// Kind selects which of the two file flavours a path holds. The kind is
// encoded in the file magic on purpose: it needs no second reserved number
// space, and `head -c8 <file>` identifies any file on disk at a glance.
type Kind uint8

// The file kinds.
const (
	// KindWAL is the write-ahead log: prepare/commit/abort records.
	KindWAL Kind = iota + 1
	// KindAudit is the append-only audit log of messages.
	KindAudit
)

// Magic values. Eight ASCII bytes, differing only in the last, which is the
// kind discriminator.
const (
	magicWAL   = "AGNTBUSW"
	magicAudit = "AGNTBUSA"
)

// String returns the lowercase name of the kind.
func (k Kind) String() string {
	switch k {
	case KindWAL:
		return "wal"
	case KindAudit:
		return "audit"
	default:
		return "unknown(" + strconv.FormatUint(uint64(k), 10) + ")"
	}
}

// magic returns the 8-byte file magic for the kind, or "" if k is not a kind.
func (k Kind) magic() string {
	switch k {
	case KindWAL:
		return magicWAL
	case KindAudit:
		return magicAudit
	default:
		return ""
	}
}

// kindForMagic maps a magic back onto its kind, returning 0 when the bytes are
// not a magic this package writes.
func kindForMagic(m string) Kind {
	switch m {
	case magicWAL:
		return KindWAL
	case magicAudit:
		return KindAudit
	default:
		return 0
	}
}

// Sentinel errors. All of them are checkable with errors.Is; the concrete
// errors returned by this package wrap them and add the path and the byte
// offset, because the first question asked of a broken log is always "where".
var (
	// ErrPoisoned is reported by every subsequent Append and Sync once a write
	// or fsync has failed. See Writer.Append.
	ErrPoisoned = errors.New("wal: writer is poisoned")

	// ErrCorrupt is reported for any malformed file header or record frame.
	ErrCorrupt = errors.New("wal: corrupt")

	// ErrClosed is reported by Append and Sync after Close.
	ErrClosed = errors.New("wal: writer is closed")

	// ErrPayloadTooLarge is reported by Append for a payload over
	// MaxPayloadSize.
	ErrPayloadTooLarge = errors.New("wal: payload too large")

	// ErrUnknownType is reported by Append for a record type outside the
	// reserved set.
	ErrUnknownType = errors.New("wal: unknown record type")

	// ErrUnknownKind is reported for a Kind outside the reserved set.
	ErrUnknownKind = errors.New("wal: unknown file kind")
)

// CorruptError says exactly where a file stopped making sense. Offset is the
// byte offset of the structure that failed to parse -- the frame header for a
// bad record, 0 for a bad file header -- so an operator reading one log line
// learns where to look with a hex dump.
type CorruptError struct {
	Path   string
	Offset int64
	Reason string
	// Err is the underlying cause, when there is one (io.ErrUnexpectedEOF for
	// a short read, for instance). It is exposed through Unwrap.
	Err error

	// FrameEnd is the byte offset just past the frame that failed --
	// Offset+frameHeaderSize+payloadLen -- for the cases where the frame header
	// was fully readable and therefore DECLARED a length. It is 0
	// when the header itself could not be read, and for a bad file header.
	//
	// It exists so that recovery can answer one question, and only that
	// question: is there anything in the file AFTER the damage? A declared
	// extent that reaches or passes the end of the file proves nothing follows;
	// an extent that ends strictly before the end of the file proves something
	// does, which means the damage is not a torn tail and must never be
	// truncated away. The length is used even when it is absurd or when the
	// frame is otherwise malformed, because it is still what the bytes on disk
	// say and the point is to locate the damage, not to trust it.
	FrameEnd int64

	// FrameIntact means A PARTIAL WRITE CANNOT EXPLAIN THIS DAMAGE, and therefore
	// that recovery must treat it as fatal no matter where in the file it sits.
	// It is set for three things, all of which share that property:
	//
	//   - a frame whose CHECKSUM VERIFIED but which is in the wrong place, i.e.
	//     an index out of sequence: a record was lost from, or resurrected in,
	//     the middle of the file, and a partially written frame does not
	//     checksum;
	//   - damage found ABOVE the framing layer (see frameCorruptf) -- a payload
	//     that will not decode, a reference to no open prepare, a record type
	//     with no meaning here -- where the frame's checksum has already been
	//     verified before the record was ever interpreted;
	//   - damage with a complete, verifying record still sitting AFTER it (see
	//     tailHasRecordsAfterIt): a crash mid-append leaves a prefix of one frame
	//     and nothing beyond it, so bytes beyond the damage mean it is not a tail.
	//
	// Recovery still treats this flag as "a partial write cannot explain it",
	// but since 2026-08-02 that is no longer a veto on removing the record -- it
	// is a reason to look for intact records AFTER it rather than assume the log
	// ends here. See RepairLog.
	FrameIntact bool
}

func (e *CorruptError) Error() string {
	s := fmt.Sprintf("wal: %s: corrupt at offset %d: %s", e.Path, e.Offset, e.Reason)
	if e.Err != nil {
		// The cause is bounded, not printed raw. A JSON or time-parse error
		// quotes the offending text back at you IN FULL, and that text came off
		// disk -- so an underlying error is just as much a route for a corrupt
		// record to write a megabyte into a log line as the Reason is. Unwrap
		// still exposes the cause intact for a caller that wants all of it.
		s += ": " + elide(e.Err.Error(), maxCauseChars)
	}
	return s
}

// Bounds on file-derived text in an error message. A record's payload is
// attacker-influenced (message bodies are client-supplied) and may be up to
// MaxPayloadSize; an error message is not the place to paste it.
const (
	// maxValueChars bounds a single value quoted into a Reason.
	maxValueChars = 64
	// maxCauseChars bounds a rendered underlying error, which is looser because
	// a decoder's message is mostly its own words.
	maxCauseChars = 160
)

// elide truncates s to at most max characters plus a marker. It is a size
// bound, not a sanitiser: %q is what neutralises control characters.
func elide(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[elided]"
}

// Is reports a match for ErrCorrupt so callers can classify without a type
// assertion, while Unwrap still exposes the underlying I/O cause.
func (e *CorruptError) Is(target error) bool { return target == ErrCorrupt }

// Unwrap returns the underlying cause, if any.
func (e *CorruptError) Unwrap() error { return e.Err }

func corruptf(path string, off int64, format string, args ...interface{}) *CorruptError {
	return &CorruptError{Path: path, Offset: off, Reason: fmt.Sprintf(format, args...)}
}

// frameCorruptf reports damage found in a record whose FRAME WAS ALREADY
// VERIFIED -- a payload that will not decode, a reference that names no open
// prepare, a record type with no meaning here. The bytes arrived exactly as
// some writer wrote them; what is wrong is what they SAY.
//
// It exists so that this class of damage is never mistaken for a torn tail.
// The distinction is not cosmetic: a torn tail may be truncated away during
// recovery, and truncating at a semantic error would throw away committed
// records that are sitting there perfectly intact. FrameIntact says "the
// checksum verified, so a partial write cannot explain this" and FrameEnd says
// where the frame ended, which together let recovery see that the file
// continues past the damage.
func frameCorruptf(path string, rec Record, format string, args ...interface{}) *CorruptError {
	e := corruptf(path, rec.Offset, format, args...)
	e.FrameIntact = true
	e.FrameEnd = rec.Offset + rec.frameSize()
	return e
}

// Record is one framed record as it exists on disk.
type Record struct {
	// Index is the record's position in the file's monotonic sequence. The
	// first record in a file has index 1 and each append adds exactly 1. An
	// index is never reused.
	Index uint64
	// Type is the reserved record type.
	Type Type
	// Payload is the opaque record body. This package never interprets it;
	// later tasks put JSON in it.
	Payload []byte
	// Offset is the byte offset of the frame HEADER in the file, so
	// Offset+frameSize() is the start of the next frame.
	Offset int64

	// legacyV1 marks a record read from a format version 1 file, whose frame
	// header is 20 bytes rather than 48.
	//
	// It is unexported and its ZERO VALUE MEANS THE CURRENT FORMAT, on purpose:
	// every Record literal in this package and its tests keeps meaning "a
	// version 2 record" without being touched, and only the version 1 reader
	// sets it. Nothing outside this package can construct a legacy record, which
	// is the right way round -- a caller must not be able to talk this package
	// into measuring a frame with the wrong header size.
	legacyV1 bool
}

// frameSize is the total on-disk size of the record, header included, in the
// version the record was read from.
func (r Record) frameSize() int64 {
	if r.legacyV1 {
		return int64(frameHeaderSizeV1 + len(r.Payload))
	}
	return int64(FrameHeaderSize + len(r.Payload))
}

// makeFileHeader renders the file header for a kind, in this codec's version.
//
// Version 2, 48 bytes:
//
//	[0:8]   magic  -- "AGNTBUSW" (wal) or "AGNTBUSA" (audit)
//	[8:12]  uint32 format version (2)
//	[12:16] uint32 reserved (always 0)
//	[16:48] HMAC-SHA256(key, header[0:16])
//
// Version 1, 16 bytes: magic ++ version ++ CRC32C over bytes [0:12].
//
// THE VERSION 2 HEADER MAC IS ALSO THE KEY CHECK VALUE, and that is its main
// job: it is how a wrong key is detected BEFORE a single record is touched. Be
// clear about what it does NOT do -- it authenticates 16 CONSTANT bytes, so it
// does not bind the header to this particular file and does not stop a header
// being copied between two logs protected by the same key. That is honest and
// accepted: an attacker who can rewrite the file can read the key next to it
// and forge freely anyway (DECISIONS.md 2026-08-02).
func (c codec) makeFileHeader(k Kind) []byte {
	b := make([]byte, c.fileHeaderSize())
	copy(b[0:8], k.magic())
	binary.BigEndian.PutUint32(b[8:12], c.version)
	n := c.fileHeaderCovered()
	copy(b[n:], c.tagFor(b[:n], nil))
	return b
}

// tagFor renders the tag bytes for covered ++ payload in this codec's version.
func (c codec) tagFor(covered, payload []byte) []byte {
	if c.isV1() {
		t := make([]byte, crcSize)
		binary.BigEndian.PutUint32(t, frameChecksum(covered, payload))
		return t
	}
	return c.mac(covered, payload)
}

// parseFileHeader validates a file header against the expected kind. The tag is
// verified LAST so that the more specific "this is an audit file, you asked for
// a WAL" and "this is format version 7" diagnoses win over a generic tag
// complaint.
func (c codec) parseFileHeader(b []byte, path string, want Kind) error {
	size := c.fileHeaderSize()
	if int64(len(b)) < size {
		return &CorruptError{Path: path, Offset: 0,
			Reason: fmt.Sprintf("truncated file header: have %d of %d bytes", len(b), size)}
	}
	magic := string(b[0:8])
	if magic != want.magic() {
		if got := kindForMagic(magic); got != 0 {
			return corruptf(path, 0, "file is a %s file, want a %s file", got, want)
		}
		return corruptf(path, 0, "bad magic %q, want %q", magic, want.magic())
	}
	if v := binary.BigEndian.Uint32(b[8:12]); v != c.version {
		return corruptf(path, 0, "format version %d, want %d", v, c.version)
	}
	n := c.fileHeaderCovered()
	if !c.verifyTag(b[:n], nil, b[n:size]) {
		if c.isV1() {
			return corruptf(path, 0, "file header checksum mismatch: computed %#08x, stored %#08x",
				frameChecksum(b[:n], nil), binary.BigEndian.Uint32(b[n:size]))
		}
		// The tag itself is NOT quoted back. It is a MAC over a constant, so
		// printing the expected value would publish a key check value for the
		// key, and printing the stored one tells an operator nothing they can
		// act on. What they need is the diagnosis, which RepairLog gives.
		return corruptf(path, 0, "file header checksum mismatch: the header does not verify under the MAC key, so either the header is damaged or the key is the wrong one")
	}
	return nil
}

// encodeFrame renders one complete record frame: the frame header followed by
// the payload.
//
// Version 2:
//
//	[0:4]    uint32 payloadLen
//	[4:12]   uint64 index
//	[12:14]  uint16 type
//	[14:16]  uint16 reserved (always 0)
//	[16:48]  HMAC-SHA256(key, frame[0:16] ++ payload)
//	[48:...] payload
//
// Version 1 is the same shape with a 4-byte CRC32C at [16:20].
//
// THE LENGTH FIELD IS INSIDE THE COVERED RANGE. That is what kills the whole
// length-inflation class of attack: a corrupted or crafted length is DETECTED
// rather than acted on, and under version 2 an adversary cannot repair the tag
// to match a length they chose. The concatenation of header and payload needs
// no separator because the length is the first four covered bytes, so no two
// distinct records share a covered byte string.
//
// THERE IS NO DOWNGRADE WRITE OF A LIVE LOG. A version 1 codec CAN encode, and
// exactly one caller may use that: rewriteLog, repairing a damaged version 1
// log IN PLACE AND IN ITS OWN FORMAT immediately before upgradeV1 converts it
// (the upgrade's strict scan cannot read a damaged log, so the repair has to
// come first). Nothing else may: OpenWriter REFUSES a positively-version-1
// file, and Open resolves a version 2 codec for the writer, so no record is
// ever APPENDED to a log this server is serving from in version 1.
func (c codec) encodeFrame(index uint64, t Type, payload []byte) []byte {
	hs := c.frameHeaderSize()
	frame := make([]byte, hs+int64(len(payload)))
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint64(frame[4:12], index)
	binary.BigEndian.PutUint16(frame[12:14], uint16(t))
	binary.BigEndian.PutUint16(frame[14:16], 0) // reserved
	copy(frame[hs:], payload)
	copy(frame[frameCoveredBytes:hs], c.tagFor(frame[0:frameCoveredBytes], frame[hs:]))
	return frame
}

// frameChecksum is the version 1 CRC32C over the covered header bytes followed
// by the payload. It exists only to READ version 1 files and to repair one
// before it is upgraded.
func frameChecksum(hdr16, payload []byte) uint32 {
	return crc32.Update(crc32.Checksum(hdr16, crcTable), crcTable, payload)
}
