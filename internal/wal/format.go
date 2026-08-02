package wal

import (
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
	// value: a WAL is replayed byte-for-byte, so guessing at an unknown layout
	// is never the safe move.
	FormatVersion = 1

	// FileHeaderSize is the size of the fixed header written once at file
	// creation: magic[8] ++ version[4] ++ crc32c-of-the-first-12-bytes[4].
	FileHeaderSize = 16

	// FrameHeaderSize is the size of the per-record header that precedes each
	// payload: payloadLen[4] ++ index[8] ++ type[2] ++ reserved[2] ++ crc[4].
	FrameHeaderSize = 20

	// MaxPayloadSize bounds a single record's payload at 1 MiB. It is a
	// safety bound, not a tuning knob: a reader that sees a larger length
	// treats the frame as corrupt WITHOUT attempting the allocation, which is
	// the difference between detecting corruption and being OOM-killed by it.
	MaxPayloadSize = 1 << 20
)

// crcTable is the CRC-32 Castagnoli table, built exactly once. Castagnoli is
// used rather than the IEEE polynomial because it has better error detection
// for short records and because crc32 uses the SSE4.2 hardware instruction for
// it, so the checksum is never the reason an append is slow.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

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
// unknown one (see scanFrom) because a frame whose CRC verifies was written
// intact by some other version of this code, and refusing to read it would
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
	// Offset+FrameHeaderSize+payloadLen -- for the cases where the 20-byte
	// frame header was fully readable and therefore DECLARED a length. It is 0
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

	// FrameIntact reports a frame whose CHECKSUM VERIFIED but which is in the
	// wrong place -- an index out of sequence. It is the one signature a torn
	// write cannot produce: a partially written frame does not checksum. So it
	// means a record was lost from, or resurrected in, the middle of the file,
	// and recovery must treat it as fatal no matter where in the file it sits.
	FrameIntact bool
}

func (e *CorruptError) Error() string {
	s := fmt.Sprintf("wal: %s: corrupt at offset %d: %s", e.Path, e.Offset, e.Reason)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
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
	// Offset+FrameHeaderSize+len(Payload) is the start of the next frame.
	Offset int64
}

// frameSize is the total on-disk size of the record.
func (r Record) frameSize() int64 { return int64(FrameHeaderSize + len(r.Payload)) }

// makeFileHeader renders the 16-byte file header for a kind.
//
//	[0:8]   magic  -- "AGNTBUSW" (wal) or "AGNTBUSA" (audit)
//	[8:12]  uint32 format version
//	[12:16] uint32 CRC32C over bytes [0:12]
func makeFileHeader(k Kind) []byte {
	b := make([]byte, FileHeaderSize)
	copy(b[0:8], k.magic())
	binary.BigEndian.PutUint32(b[8:12], FormatVersion)
	binary.BigEndian.PutUint32(b[12:16], crc32.Checksum(b[0:12], crcTable))
	return b
}

// parseFileHeader validates a 16-byte file header against the expected kind.
// The checksum is verified LAST so that the more specific "this is an audit
// file, you asked for a WAL" and "this is format version 7" diagnoses win over
// a generic checksum complaint.
func parseFileHeader(b []byte, path string, want Kind) error {
	if len(b) < FileHeaderSize {
		return &CorruptError{Path: path, Offset: 0,
			Reason: fmt.Sprintf("truncated file header: have %d of %d bytes", len(b), FileHeaderSize)}
	}
	magic := string(b[0:8])
	if magic != want.magic() {
		if got := kindForMagic(magic); got != 0 {
			return corruptf(path, 0, "file is a %s file, want a %s file", got, want)
		}
		return corruptf(path, 0, "bad magic %q, want %q", magic, want.magic())
	}
	if v := binary.BigEndian.Uint32(b[8:12]); v != FormatVersion {
		return corruptf(path, 0, "format version %d, want %d", v, FormatVersion)
	}
	if sum, stored := crc32.Checksum(b[0:12], crcTable), binary.BigEndian.Uint32(b[12:16]); sum != stored {
		return corruptf(path, 0, "file header checksum mismatch: computed %#08x, stored %#08x", sum, stored)
	}
	return nil
}

// encodeFrame renders one complete record frame: a 20-byte header followed by
// the payload.
//
//	[0:4]    uint32 payloadLen
//	[4:12]   uint64 index
//	[12:14]  uint16 type
//	[14:16]  uint16 reserved (always 0)
//	[16:20]  uint32 CRC32C over frame[0:16] ++ payload
//	[20:...] payload
//
// The checksum covers the length and the index as well as the payload, so a
// corrupted length field is DETECTED rather than acted on.
func encodeFrame(index uint64, t Type, payload []byte) []byte {
	frame := make([]byte, FrameHeaderSize+len(payload))
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint64(frame[4:12], index)
	binary.BigEndian.PutUint16(frame[12:14], uint16(t))
	binary.BigEndian.PutUint16(frame[14:16], 0) // reserved
	copy(frame[FrameHeaderSize:], payload)
	binary.BigEndian.PutUint32(frame[16:20], frameChecksum(frame[0:16], frame[FrameHeaderSize:]))
	return frame
}

// frameChecksum is the CRC32C over the first 16 header bytes followed by the
// payload. It is the single definition of the record checksum: the writer and
// the reader both call it, so they cannot drift apart.
func frameChecksum(hdr16, payload []byte) uint32 {
	return crc32.Update(crc32.Checksum(hdr16, crcTable), crcTable, payload)
}
