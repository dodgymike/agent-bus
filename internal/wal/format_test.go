package wal

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Golden bytes. These literals are the format. They are written out by hand
// here rather than computed with the package's own helpers so that a change to
// the layout, the byte order, the version or the CRC POLYNOMIAL fails this
// test instead of silently rewriting the on-disk format on both sides.
//
//	file header (wal):   "AGNTBUSW"          version 00000001  crc32c ed2a1f6d
//	file header (audit): "AGNTBUSA"          version 00000001  crc32c f26f1f56
//	frame:               len 00000005  index 0000000000000001  type 0002
//	                     reserved 0000  crc32c 7aba2684  payload "hello"
const (
	goldenWALHeader   = "41474e544255535700000001ed2a1f6d"
	goldenAuditHeader = "41474e544255534100000001f26f1f56"
	goldenCommitFrame = "000000050000000000000001000200007aba268468656c6c6f"
)

func TestWALFraming(t *testing.T) {
	// The frame sizes are part of the contract every other durability task
	// builds against; pin them.
	if FileHeaderSize != 16 {
		t.Errorf("FileHeaderSize = %d, want 16", FileHeaderSize)
	}
	if FrameHeaderSize != 20 {
		t.Errorf("FrameHeaderSize = %d, want 20", FrameHeaderSize)
	}
	if FormatVersion != 1 {
		t.Errorf("FormatVersion = %d, want 1", FormatVersion)
	}
	if MaxPayloadSize != 1<<20 {
		t.Errorf("MaxPayloadSize = %d, want %d", MaxPayloadSize, 1<<20)
	}
	if TypePrepare != 1 || TypeCommit != 2 || TypeAbort != 3 || TypeAuditMessage != 4 {
		t.Errorf("reserved record type values changed: prepare=%d commit=%d abort=%d audit=%d",
			TypePrepare, TypeCommit, TypeAbort, TypeAuditMessage)
	}

	t.Run("golden bytes on disk", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wal.log")
		w, err := OpenWriter(path, KindWAL)
		if err != nil {
			t.Fatalf("OpenWriter: %v", err)
		}
		rec, err := w.Append(TypeCommit, []byte("hello"))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		if rec.Index != 1 {
			t.Errorf("first record index = %d, want 1", rec.Index)
		}
		if rec.Offset != FileHeaderSize {
			t.Errorf("first record offset = %d, want %d", rec.Offset, FileHeaderSize)
		}

		got := readFile(t, path)
		want := unhex(t, goldenWALHeader+goldenCommitFrame)
		if !bytes.Equal(got, want) {
			t.Fatalf("on-disk bytes changed\n got %s\nwant %s",
				hex.EncodeToString(got), hex.EncodeToString(want))
		}
	})

	t.Run("audit files carry their own magic", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit.log")
		w, err := OpenWriter(path, KindAudit)
		if err != nil {
			t.Fatalf("OpenWriter: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		got := readFile(t, path)
		if want := unhex(t, goldenAuditHeader); !bytes.Equal(got, want) {
			t.Fatalf("audit header = %s, want %s", hex.EncodeToString(got), hex.EncodeToString(want))
		}
		// A file's kind is not negotiable: reading it as the other kind fails.
		if _, _, err := ScanAll(path, KindWAL); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("ScanAll(audit file, KindWAL) error = %v, want ErrCorrupt", err)
		}
	})

	t.Run("payload round-trip", func(t *testing.T) {
		payloads := [][]byte{
			{},                               // zero length
			{0x00},                           // one byte, and a zero byte at that
			bytes.Repeat([]byte{0xa5}, 1024), // ordinary
			make([]byte, MaxPayloadSize),     // the largest frame the format allows
		}
		for i := range payloads[3] {
			payloads[3][i] = byte(i)
		}
		types := []Type{TypePrepare, TypeCommit, TypeAbort, TypeAuditMessage}

		path := filepath.Join(t.TempDir(), "wal.log")
		w, err := OpenWriter(path, KindWAL)
		if err != nil {
			t.Fatalf("OpenWriter: %v", err)
		}
		var appended []Record
		for i, p := range payloads {
			rec, err := w.Append(types[i], p)
			if err != nil {
				t.Fatalf("Append(len=%d): %v", len(p), err)
			}
			if uint64(i+1) != rec.Index {
				t.Errorf("record %d index = %d, want %d", i, rec.Index, i+1)
			}
			if !bytes.Equal(rec.Payload, p) {
				t.Errorf("record %d payload round-trip differs in the returned Record", i)
			}
			appended = append(appended, rec)
		}
		size := w.Size()
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if fi, err := os.Stat(path); err != nil {
			t.Fatalf("Stat: %v", err)
		} else if fi.Size() != size {
			t.Errorf("Size() = %d, file is %d bytes", size, fi.Size())
		}

		recs, end, err := ScanAll(path, KindWAL)
		if err != nil {
			t.Fatalf("ScanAll: %v", err)
		}
		if end != size {
			t.Errorf("ScanAll end offset = %d, want %d", end, size)
		}
		if len(recs) != len(payloads) {
			t.Fatalf("ScanAll returned %d records, want %d", len(recs), len(payloads))
		}
		for i, rec := range recs {
			if rec.Index != uint64(i+1) || rec.Type != types[i] || rec.Offset != appended[i].Offset {
				t.Errorf("record %d = {index %d, type %s, offset %d}, want {index %d, type %s, offset %d}",
					i, rec.Index, rec.Type, rec.Offset, i+1, types[i], appended[i].Offset)
			}
			if !bytes.Equal(rec.Payload, payloads[i]) {
				t.Errorf("record %d payload (%d bytes) differs after round-trip", i, len(payloads[i]))
			}
		}
	})

	t.Run("type and kind names", func(t *testing.T) {
		for _, tc := range []struct {
			t    Type
			want string
			ok   bool
		}{
			{TypePrepare, "prepare", true},
			{TypeCommit, "commit", true},
			{TypeAbort, "abort", true},
			{TypeAuditMessage, "audit_message", true},
			{Type(0), "unknown(0)", false},
			{Type(9), "unknown(9)", false},
		} {
			if got := tc.t.String(); got != tc.want {
				t.Errorf("Type(%d).String() = %q, want %q", uint16(tc.t), got, tc.want)
			}
			if got := tc.t.Known(); got != tc.ok {
				t.Errorf("Type(%d).Known() = %v, want %v", uint16(tc.t), got, tc.ok)
			}
		}
		for _, tc := range []struct {
			k    Kind
			want string
		}{
			{KindWAL, "wal"},
			{KindAudit, "audit"},
			{Kind(0), "unknown(0)"},
			{Kind(7), "unknown(7)"},
		} {
			if got := tc.k.String(); got != tc.want {
				t.Errorf("Kind(%d).String() = %q, want %q", uint8(tc.k), got, tc.want)
			}
		}
	})
}

// TestWALFramingCorruption drives every way a file can stop making sense past
// ScanAll. Each case works on its own copy of a known-good three-record log,
// must produce an error matching ErrCorrupt, and must name the byte offset --
// an operator's first move on a broken log is a hex dump at that offset.
func TestWALFramingCorruption(t *testing.T) {
	tests := []struct {
		name       string
		kind       Kind // zero means KindWAL
		mutate     func(t *testing.T, path string, recs []Record)
		wantOffset int64
		wantReason string
	}{
		{
			name:       "flipped payload byte",
			mutate:     func(t *testing.T, p string, r []Record) { flipByte(t, p, r[1].Offset+FrameHeaderSize+1) },
			wantOffset: -1, // filled in from recs[1].Offset
			wantReason: "checksum mismatch",
		},
		{
			name:       "flipped length byte",
			mutate:     func(t *testing.T, p string, r []Record) { flipByte(t, p, r[1].Offset+3) },
			wantOffset: -1,
			wantReason: "checksum mismatch",
		},
		{
			name:       "flipped index byte",
			mutate:     func(t *testing.T, p string, r []Record) { flipByte(t, p, r[1].Offset+11) },
			wantOffset: -1,
			wantReason: "checksum mismatch",
		},
		{
			name:       "non-zero reserved field",
			mutate:     func(t *testing.T, p string, r []Record) { patch(t, p, r[1].Offset+14, []byte{0x00, 0x01}) },
			wantOffset: -1,
			wantReason: "reserved field",
		},
		{
			name:       "truncated frame header",
			mutate:     func(t *testing.T, p string, r []Record) { truncate(t, p, r[1].Offset+10) },
			wantOffset: -1,
			wantReason: "truncated frame header: have 10 of 20 bytes",
		},
		{
			name:       "truncated payload",
			mutate:     func(t *testing.T, p string, r []Record) { truncate(t, p, r[1].Offset+FrameHeaderSize+2) },
			wantOffset: -1,
			wantReason: "truncated payload: have 2 of 5 bytes",
		},
		{
			name:       "absurd payload length",
			mutate:     func(t *testing.T, p string, r []Record) { patch(t, p, r[1].Offset, []byte{0xff, 0xff, 0xff, 0xff}) },
			wantOffset: -1,
			wantReason: "rejected without allocating",
		},
		{
			name:       "bad magic",
			mutate:     func(t *testing.T, p string, r []Record) { patch(t, p, 0, []byte("XGNTBUSW")) },
			wantOffset: 0,
			wantReason: "bad magic",
		},
		{
			name:       "right magic, wrong kind",
			kind:       KindAudit,
			mutate:     func(t *testing.T, p string, r []Record) {},
			wantOffset: 0,
			wantReason: "file is a wal file, want a audit file",
		},
		{
			name:       "wrong format version",
			mutate:     func(t *testing.T, p string, r []Record) { patch(t, p, 8, []byte{0x00, 0x00, 0x00, 0x02}) },
			wantOffset: 0,
			wantReason: "format version 2, want 1",
		},
		{
			name:       "corrupt file header checksum",
			mutate:     func(t *testing.T, p string, r []Record) { flipByte(t, p, 13) },
			wantOffset: 0,
			wantReason: "file header checksum mismatch",
		},
		{
			name:       "truncated file header",
			mutate:     func(t *testing.T, p string, r []Record) { truncate(t, p, 9) },
			wantOffset: 0,
			wantReason: "truncated file header: have 9 of 16 bytes",
		},
		{
			name:       "empty file",
			mutate:     func(t *testing.T, p string, r []Record) { truncate(t, p, 0) },
			wantOffset: 0,
			wantReason: "file is empty",
		},
		{
			name: "whole record lost from the middle",
			mutate: func(t *testing.T, p string, r []Record) {
				// Cut record 2 out entirely. Every remaining frame still
				// checksums; only the index sequence gives it away.
				b := readFile(t, p)
				cut := append(append([]byte{}, b[:r[1].Offset]...), b[r[2].Offset:]...)
				if err := os.WriteFile(p, cut, 0600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
			wantOffset: -1,
			wantReason: "out of sequence",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			path, recs := writeGoodLog(t)
			tc.mutate(t, path, recs)

			kind := tc.kind
			if kind == 0 {
				kind = KindWAL
			}
			wantOffset := tc.wantOffset
			if wantOffset < 0 {
				wantOffset = recs[1].Offset
			}

			got, end, err := ScanAll(path, kind)
			if err == nil {
				t.Fatalf("ScanAll succeeded on a corrupt file, returning %d records", len(got))
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ScanAll error = %v, want one matching ErrCorrupt", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("offset %d", wantOffset)) {
				t.Errorf("error does not name offset %d: %v", wantOffset, err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("error reason = %q, want it to contain %q", err.Error(), tc.wantReason)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error does not name the path %s: %v", path, err)
			}
			if end != wantOffset {
				t.Errorf("ScanAll end offset = %d, want %d (just past the last good record)", end, wantOffset)
			}
			// The corrupt frame is rejected, and so is everything after it:
			// this reader hands back only the good prefix.
			var wantRecs int
			for _, r := range recs {
				if r.Offset < wantOffset {
					wantRecs++
				}
			}
			if len(got) != wantRecs {
				t.Errorf("ScanAll returned %d records before the corruption, want %d", len(got), wantRecs)
			}
		})
	}
}

// TestWALFramingHugeLengthRejectedWithoutAllocating proves the ordering that
// matters: the length bound is checked BEFORE the payload buffer is made, so a
// corrupt 4 GiB length is detected rather than acted on.
func TestWALFramingHugeLengthRejectedWithoutAllocating(t *testing.T) {
	path, recs := writeGoodLog(t)
	patch(t, path, recs[1].Offset, []byte{0xff, 0xff, 0xff, 0xff}) // 4294967295

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, err := ScanAll(path, KindWAL)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ScanAll error = %v, want ErrCorrupt", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Errorf("ScanAll allocated %d bytes handling a 4 GiB length field; it must reject before allocating", grew)
	}
}

// writeGoodLog builds a known-good three-record WAL in a fresh temp dir and
// returns its path and its records.
func writeGoodLog(t *testing.T) (string, []Record) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wal.log")
	w, err := OpenWriter(path, KindWAL)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	var recs []Record
	for i, p := range []string{"alpha", "bravo", "charlie"} {
		rec, err := w.Append([]Type{TypePrepare, TypeCommit, TypeAuditMessage}[i], []byte(p))
		if err != nil {
			t.Fatalf("Append(%q): %v", p, err)
		}
		recs = append(recs, rec)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, recs
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return b
}

// patch overwrites len(b) bytes at off, in place.
func patch(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatalf("WriteAt(%d): %v", off, err)
	}
}

// flipByte flips the low bit of the byte at off: a single-bit disk error.
func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	b := readFile(t, path)
	if off >= int64(len(b)) {
		t.Fatalf("flipByte: offset %d past end of %d-byte file", off, len(b))
	}
	patch(t, path, off, []byte{b[off] ^ 0x01})
}

func truncate(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("Truncate(%d): %v", size, err)
	}
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad golden hex: %v", err)
	}
	return b
}
