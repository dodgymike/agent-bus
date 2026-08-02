package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// ScanAll reads every record in path, strictly: any malformed frame is an
// error. Nothing is skipped and nothing is truncated -- deciding that a
// corrupt TAIL is tolerable (and cutting it off) is recovery policy and lives
// in the recovery task, not here. This function's job is to say precisely
// where the file stops being trustworthy.
//
// It returns the records in file order, the byte offset just past the last
// good record (which, on success, is the end of the file, and on failure is
// where a recovery pass would truncate), and the error.
func ScanAll(path string, kind Kind) ([]Record, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("wal: open %s: %w", path, err)
	}
	defer f.Close()

	var recs []Record
	end, err := scanFrom(f, path, kind, func(rec Record) error {
		recs = append(recs, rec)
		return nil
	})
	return recs, end, err
}

// scanFrom reads r from its current position as a whole file of the given
// kind, calling fn for each record in order. It returns the offset just past
// the last record it accepted.
//
// It is the single parsing path: ScanAll and OpenWriter both go through it, so
// the writer can never disagree with the reader about where a file ends. fn
// lets OpenWriter establish the next index without holding an entire log in
// memory, and is the seam recovery will use for a streaming replay.
func scanFrom(r io.Reader, path string, kind Kind, fn func(Record) error) (int64, error) {
	if kind.magic() == "" {
		return 0, fmt.Errorf("wal: scan %s: %w: %s", path, ErrUnknownKind, kind)
	}
	br := bufio.NewReader(r)

	hdr := make([]byte, FileHeaderSize)
	n, err := io.ReadFull(br, hdr)
	if err != nil {
		if err == io.EOF {
			return 0, &CorruptError{Path: path, Offset: 0,
				Reason: fmt.Sprintf("file is empty: it has no %d-byte file header", FileHeaderSize),
				Err:    io.ErrUnexpectedEOF}
		}
		if err == io.ErrUnexpectedEOF {
			return 0, &CorruptError{Path: path, Offset: 0,
				Reason: fmt.Sprintf("truncated file header: have %d of %d bytes", n, FileHeaderSize),
				Err:    err}
		}
		return 0, fmt.Errorf("wal: read file header of %s at offset 0: %w", path, err)
	}
	if err := parseFileHeader(hdr, path, kind); err != nil {
		return 0, err
	}

	off := int64(FileHeaderSize)
	wantIndex := uint64(1)
	for {
		rec, err := readFrame(br, path, off)
		if err == io.EOF { // clean end of file, exactly on a frame boundary
			return off, nil
		}
		if err != nil {
			return off, err
		}
		// The checksum proves the frame is intact; the sequence check proves
		// the FILE is intact. A whole frame lost to a bad sector, or an old
		// frame resurrected underneath us, leaves every individual checksum
		// happy and only shows up as a hole in the index sequence.
		if rec.Index != wantIndex {
			return off, corruptf(path, off, "record index %d out of sequence, want %d", rec.Index, wantIndex)
		}
		if err := fn(rec); err != nil {
			return off, err
		}
		wantIndex++
		off += rec.frameSize()
	}
}

// readFrame reads one record frame from r, which must be positioned at off.
// It returns io.EOF, and only io.EOF, when r is exhausted exactly on a frame
// boundary; every other short read is corruption.
func readFrame(r io.Reader, path string, off int64) (Record, error) {
	var hdr [FrameHeaderSize]byte
	n, err := io.ReadFull(r, hdr[:])
	if err != nil {
		if err == io.EOF && n == 0 {
			return Record{}, io.EOF
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return Record{}, &CorruptError{Path: path, Offset: off,
				Reason: fmt.Sprintf("truncated frame header: have %d of %d bytes", n, FrameHeaderSize),
				Err:    io.ErrUnexpectedEOF}
		}
		return Record{}, fmt.Errorf("wal: read %s at offset %d: %w", path, off, err)
	}

	payloadLen := binary.BigEndian.Uint32(hdr[0:4])
	index := binary.BigEndian.Uint64(hdr[4:12])
	typ := Type(binary.BigEndian.Uint16(hdr[12:14]))
	reserved := binary.BigEndian.Uint16(hdr[14:16])
	stored := binary.BigEndian.Uint32(hdr[16:20])

	// Both structural checks happen BEFORE the payload is allocated. The
	// checksum cannot help here: verifying it needs the payload, and reading
	// the payload needs a length we are not yet entitled to trust. So the
	// length is first bounded, then -- because the checksum covers the header
	// -- proven.
	if reserved != 0 {
		return Record{}, corruptf(path, off, "reserved field is %#04x, want 0", reserved)
	}
	if payloadLen > MaxPayloadSize {
		return Record{}, corruptf(path, off, "payload length %d exceeds the %d-byte maximum (frame rejected without allocating)",
			payloadLen, MaxPayloadSize)
	}

	payload := make([]byte, payloadLen)
	if n, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return Record{}, &CorruptError{Path: path, Offset: off,
				Reason: fmt.Sprintf("truncated payload: have %d of %d bytes", n, payloadLen),
				Err:    io.ErrUnexpectedEOF}
		}
		return Record{}, fmt.Errorf("wal: read %s payload at offset %d: %w", path, off+FrameHeaderSize, err)
	}

	if sum := frameChecksum(hdr[0:16], payload); sum != stored {
		return Record{}, corruptf(path, off, "checksum mismatch: computed %#08x, stored %#08x", sum, stored)
	}

	// The type is deliberately NOT rejected when unknown: the checksum has
	// already proven these bytes are exactly what some writer intended. See
	// Type.Known.
	return Record{Index: index, Type: typ, Payload: payload, Offset: off}, nil
}
