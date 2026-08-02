package wal

import (
	"bytes"
	"encoding/binary"
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
// the layout, the byte order, the version, the MAC ALGORITHM or the range the
// MAC COVERS fails this test instead of silently rewriting the on-disk format
// on both sides.
//
// Since format version 2 the tags are keyed, so the golden bytes are only
// deterministic under a FIXED key: goldenMACKey is planted in the data
// directory before the writer opens the log. A real key comes from crypto/rand
// and is never a constant -- see createMACKey.
//
//	file header (wal):   "AGNTBUSW"          version 00000002  reserved 00000000
//	                     hmac-sha256(key, header[0:16])
//	file header (audit): "AGNTBUSA"          version 00000002  reserved 00000000
//	frame:               len 00000005  index 0000000000000001  type 0002
//	                     reserved 0000  hmac-sha256(key, frame[0:16] ++ "hello")
//	                     payload "hello"
const (
	goldenMACKey      = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	goldenWALHeader   = "41474e54425553570000000200000000d98e0e5788b454c56eae1474b1bf64b2909b5164a765effbac017a822e1126ca"
	goldenAuditHeader = "41474e544255534100000002000000007b94be168ee8d6ad96c18f4972704b361077a23ff0ae18c2053f4bb5198298bb"
	goldenCommitFrame = "000000050000000000000001000200006defbfc34091d743ace6c2d2053d86c5d66952b68067d4e0eecdb7a47362000d68656c6c6f"
)

// plantGoldenMACKey writes goldenMACKey into dir, so that a log created there
// carries the deterministic tags the golden literals above pin.
func plantGoldenMACKey(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(macKeyPath(dir), []byte(goldenMACKey+"\n"), macKeyMode); err != nil {
		t.Fatalf("plant the golden MAC key in %s: %v", dir, err)
	}
}

// testCodec resolves the codec a hand-built fixture must be encoded with: the
// current format version, keyed with the MAC key in the log's directory, which
// is created if there is not one there yet.
//
// It exists because every frame is now AUTHENTICATED. Before format version 2 a
// test could assemble a frame from constants; now a fixture that the package
// will accept has to be built with the same key the reader will verify it under,
// and that key is a property of the DIRECTORY the log lives in.
func testCodec(t *testing.T, path string) codec {
	t.Helper()
	c, err := currentCodec(path, KindWAL, nil)
	if err != nil {
		t.Fatalf("resolve the codec for %s: %v", path, err)
	}
	return c
}

// v1Record is one record to lay down in a format version 1 fixture.
type v1Record struct {
	Index   uint64
	Type    Type
	Payload []byte
}

// writeV1Log writes a complete FORMAT VERSION 1 log at path: the legacy 16-byte
// file header and 20-byte CRC32C frames, byte for byte as the pre-DUR-12 writer
// would have produced them.
//
// It is the only way to obtain one. Nothing in this package writes version 1 any
// more except the in-place repair that precedes an upgrade, so the upgrade path
// would otherwise be untestable.
func writeV1Log(t *testing.T, path string, kind Kind, recs ...v1Record) {
	t.Helper()
	v1 := codec{version: formatVersionV1}
	buf := v1.makeFileHeader(kind)
	for _, r := range recs {
		buf = append(buf, v1.encodeFrame(r.Index, r.Type, r.Payload)...)
	}
	if err := os.WriteFile(path, buf, fileMode); err != nil {
		t.Fatalf("write the format version 1 log %s: %v", path, err)
	}
}

func TestWALFraming(t *testing.T) {
	// The frame sizes are part of the contract every other durability task
	// builds against; pin them.
	if FileHeaderSize != 48 {
		t.Errorf("FileHeaderSize = %d, want 48", FileHeaderSize)
	}
	if FrameHeaderSize != 48 {
		t.Errorf("FrameHeaderSize = %d, want 48", FrameHeaderSize)
	}
	if MACSize != 32 {
		t.Errorf("MACSize = %d, want 32: a truncated MAC is a security-margin decision this package does not make", MACSize)
	}
	if FormatVersion != 2 {
		t.Errorf("FormatVersion = %d, want 2", FormatVersion)
	}
	if fileHeaderSizeV1 != 16 || frameHeaderSizeV1 != 20 || formatVersionV1 != 1 {
		t.Errorf("the legacy layout changed: fileHeaderSizeV1=%d frameHeaderSizeV1=%d formatVersionV1=%d, want 16/20/1 -- these describe files that are ALREADY ON DISK and cannot be redefined",
			fileHeaderSizeV1, frameHeaderSizeV1, formatVersionV1)
	}
	if MaxPayloadSize != 1<<20 {
		t.Errorf("MaxPayloadSize = %d, want %d", MaxPayloadSize, 1<<20)
	}
	if TypePrepare != 1 || TypeCommit != 2 || TypeAbort != 3 || TypeAuditMessage != 4 {
		t.Errorf("reserved record type values changed: prepare=%d commit=%d abort=%d audit=%d",
			TypePrepare, TypeCommit, TypeAbort, TypeAuditMessage)
	}

	t.Run("golden bytes on disk", func(t *testing.T) {
		dir := t.TempDir()
		plantGoldenMACKey(t, dir)
		path := filepath.Join(dir, "wal.log")
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
		dir := t.TempDir()
		plantGoldenMACKey(t, dir)
		path := filepath.Join(dir, "audit.log")
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
			wantReason: "truncated frame header: have 10 of 48 bytes",
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
			// Version 3: 2 is what this binary writes and 1 is the legacy
			// layout it still READS so an existing bus can be upgraded, so
			// neither is "wrong" any more. The property is unchanged -- a
			// version this code does not implement is refused, never guessed at.
			name:       "wrong format version",
			mutate:     func(t *testing.T, p string, r []Record) { patch(t, p, 8, []byte{0x00, 0x00, 0x00, 0x03}) },
			wantOffset: 0,
			wantReason: "format version 3, want 2",
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
			wantReason: "truncated file header: have 9 of 48 bytes",
		},
		{
			name:       "empty file",
			mutate:     func(t *testing.T, p string, r []Record) { truncate(t, p, 0) },
			wantOffset: 0,
			wantReason: "file is empty",
		},
		{
			// A record RESURRECTED in place, or written twice: the index goes
			// BACKWARDS. Every frame still checksums, so nothing but the sequence
			// rule catches it, and it is still corruption -- replaying it would
			// apply history out of order.
			//
			// Note what is NOT here any more: a record MISSING from the middle,
			// which leaves a rising-but-sparse sequence. That used to be in this
			// table and is now legal (see TestWALFramingAcceptsAnIndexHole): the
			// 2026-08-02 recovery policy discards damaged records and deliberately
			// does not renumber the survivors, so a repaired log has permanent
			// HOLES and a reader that insisted on density would refuse to read the
			// file recovery had just produced.
			name: "a record resurrected in place",
			mutate: func(t *testing.T, p string, r []Record) {
				// Overwrite record 2's frame with a re-encoding of record 1.
				patch(t, p, r[1].Offset, testCodec(t, p).encodeFrame(r[0].Index, r[1].Type, r[1].Payload))
			},
			wantOffset: -1,
			wantReason: "does not follow the previous record",
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

// TestWALFramingAcceptsAnIndexHole pins the rule that changed on 2026-08-02:
// the index sequence must be STRICTLY INCREASING, not dense.
//
// Recovery is now required to discard damaged records so that the bus always
// restarts (DECISIONS.md, "Availability over retention"), and it deliberately
// does NOT renumber the survivors -- renumbering would reuse ids, which
// invariant 1 forbids outright. So a repaired log has permanent HOLES, and a
// reader that demanded a dense sequence would refuse to read the very file the
// repair pass had just written. This test is the reader's half of that contract;
// it used to assert the opposite ("whole record lost from the middle" was a
// corruption case) and the change is deliberate.
//
// A hole is not corruption HERE. It is still a LOSS, and it is still reported:
// Replay counts every gap into Recovered.MissingRecords and Open logs it on
// every start, which is asserted in TestWALReplayReportsIndexHoles.
func TestWALFramingAcceptsAnIndexHole(t *testing.T) {
	path, recs := writeGoodLog(t)

	// Cut record 2 out entirely. Every remaining frame still checksums, and the
	// indices now run 1, 3 -- rising, but with a hole where record 2 was.
	b := readFile(t, path)
	cut := append(append([]byte{}, b[:recs[1].Offset]...), b[recs[2].Offset:]...)
	if err := os.WriteFile(path, cut, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, end, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("ScanAll over a log with an index hole: %v: a hole is what a repair LEAVES, so the reader must accept it", err)
	}
	if len(got) != 2 {
		t.Fatalf("ScanAll returned %d records, want 2", len(got))
	}
	if got[0].Index != recs[0].Index || got[1].Index != recs[2].Index {
		t.Errorf("ScanAll returned indices %d and %d, want %d and %d: survivors keep their ORIGINAL indices",
			got[0].Index, got[1].Index, recs[0].Index, recs[2].Index)
	}
	if !bytes.Equal(got[1].Payload, recs[2].Payload) {
		t.Errorf("the record after the hole came back as %q, want %q", got[1].Payload, recs[2].Payload)
	}
	if size := fileSize(t, path); end != size {
		t.Errorf("ScanAll ended at %d but the file is %d bytes", end, size)
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

// ---------------------------------------------------------------------------
// DUR-12 -- the frame MAC.
//
// Format version 2 replaced an unkeyed CRC32C with a keyed HMAC-SHA256, and the
// test below is the whole reason that was worth an on-disk format bump. It is
// NOT "a corrupt frame is detected" -- the CRC did that. It is the property a
// CRC cannot have:
//
//	A PARTY WITHOUT THE KEY CANNOT PRODUCE A TAG THAT VERIFIES.
//
// Against a CRC, an ordinary enrolled client who can get chosen bytes into a
// payload can compute the checksum of whatever it likes and hand recovery a byte
// sequence it accepts as a complete record. Against an HMAC it cannot, and the
// forgery leg below is what proves that has actually been bought: every
// alteration is re-tagged under a DIFFERENT key, exactly as an attacker with the
// algorithm but not the secret would, and must STILL be rejected.
// ---------------------------------------------------------------------------

// forgerMACKey is the key an attacker holds: a well-formed key that is not this
// log's. It differs from goldenMACKey in every byte so that no test can pass by
// accident on a partial comparison.
const forgerMACKey = "f0e1d2c3b4a5968778695a4b3c2d1e0fffeeddccbbaa99887766554433221100"

// frameVerifiesAt parses the frame header at off out of the file image b and
// reports whether the stored tag verifies over the covered header bytes plus the
// payload the header DECLARES.
//
// It reads the length off disk rather than being told it, because the length
// field is INSIDE the MAC's covered range and one of the alterations below is a
// length inflation: a helper that used the pristine length would quietly repair
// the very damage it is meant to detect.
func frameVerifiesAt(t *testing.T, c codec, b []byte, off int64) bool {
	t.Helper()
	if off+FrameHeaderSize > int64(len(b)) {
		t.Fatalf("frameVerifiesAt: no %d-byte frame header at offset %d of a %d-byte file", FrameHeaderSize, off, len(b))
	}
	hdr := b[off : off+FrameHeaderSize]
	declared := int64(binary.BigEndian.Uint32(hdr[0:4]))
	end := off + FrameHeaderSize + declared
	if declared > MaxPayloadSize || end > int64(len(b)) {
		// The declared extent is not even inside the file, so there is no byte
		// string for the tag to cover. That is a rejection, not a verification.
		return false
	}
	return c.verifyTag(hdr[0:frameCoveredBytes], b[off+FrameHeaderSize:end], hdr[frameCoveredBytes:FrameHeaderSize])
}

// forgeTagUnderAnotherKey rewrites the frame's tag with the tag a forger holding
// forgerMACKey -- and nothing else -- would compute over the frame exactly as it
// now stands on disk. Under a CRC32C this step REPAIRS the damage and the frame
// is accepted; under an HMAC it must change nothing about the verdict.
func forgeTagUnderAnotherKey(t *testing.T, path string, off int64) {
	t.Helper()
	b := readFile(t, path)
	hdr := b[off : off+FrameHeaderSize]
	declared := int64(binary.BigEndian.Uint32(hdr[0:4]))
	end := off + FrameHeaderSize + declared
	if declared > MaxPayloadSize || end > int64(len(b)) {
		t.Fatalf("forgeTagUnderAnotherKey: the frame at %d declares %d payload bytes, which is not inside the %d-byte file",
			off, declared, len(b))
	}
	forger := codec{version: FormatVersion, key: unhex(t, forgerMACKey)}
	patch(t, path, off+frameCoveredBytes, forger.tagFor(hdr[0:frameCoveredBytes], b[off+FrameHeaderSize:end]))
}

// TestWALFrameMACRejectsAlteredPayload alters a committed record every way the
// frame layout allows and demands that the frame stops verifying each time --
// then re-tags each alteration under a key that is not the log's and demands
// that it STILL does not verify.
//
// The alteration sites are every field the tag covers, not just the payload:
// the length, the index, the type and the reserved field are all inside
// frame[0:16], which is what kills the length-inflation class of attack outright.
// The tag itself is altered too, because a flipped bit in the tag must be
// corruption rather than a frame that quietly stops being checked.
func TestWALFrameMACRejectsAlteredPayload(t *testing.T) {
	// Record 2 of the three-record fixture: it has a complete record in front of
	// it (so the good prefix is non-empty and a cascade would show up) and a
	// complete record behind it (so a LENGTH INFLATION has real bytes to run
	// into rather than falling off the end of the file).
	const target = 1

	tests := []struct {
		name string
		// mutate damages the frame at off. It must not change the file's length:
		// every case here is an in-place alteration, which is what an attacker
		// rewriting a record and what a flipped bit both look like.
		mutate func(t *testing.T, path string, off int64)
		// wantReason is what the READER must say. It is not always a tag
		// complaint: the reserved field is a structural check that fires before
		// the tag is consulted, and saying so here keeps the difference honest.
		wantReason string
	}{
		{
			name:       "a flipped payload byte does not verify",
			mutate:     func(t *testing.T, p string, off int64) { flipByte(t, p, off+FrameHeaderSize+1) },
			wantReason: "checksum mismatch: the frame does not verify under the MAC key",
		},
		{
			name:       "a flipped byte of the MAC tag itself does not verify",
			mutate:     func(t *testing.T, p string, off int64) { flipByte(t, p, off+frameCoveredBytes) },
			wantReason: "checksum mismatch: the frame does not verify under the MAC key",
		},
		{
			name:       "a flipped byte of the LAST byte of the MAC tag does not verify",
			mutate:     func(t *testing.T, p string, off int64) { flipByte(t, p, off+FrameHeaderSize-1) },
			wantReason: "checksum mismatch: the frame does not verify under the MAC key",
		},
		{
			// THE POINT OF PUTTING THE LENGTH INSIDE THE COVERED RANGE. The
			// inflated length swallows the first eight bytes of the NEXT frame,
			// so a reader that trusted it would hand back a record that is part
			// of two different records glued together. The tag says no.
			name: "an inflated length field does not verify",
			mutate: func(t *testing.T, p string, off int64) {
				b := readFile(t, p)
				grown := binary.BigEndian.Uint32(b[off:off+4]) + 8
				patch(t, p, off, be32(grown))
			},
			wantReason: "checksum mismatch: the frame does not verify under the MAC key",
		},
		{
			// Deflation is the other half of the same class: a reader that
			// trusted it would return a truncated payload and then resynchronise
			// in the middle of this frame.
			name: "a deflated length field does not verify",
			mutate: func(t *testing.T, p string, off int64) {
				b := readFile(t, p)
				shrunk := binary.BigEndian.Uint32(b[off:off+4]) - 2
				patch(t, p, off, be32(shrunk))
			},
			wantReason: "checksum mismatch: the frame does not verify under the MAC key",
		},
		{
			name:       "a flipped index byte does not verify",
			mutate:     func(t *testing.T, p string, off int64) { flipByte(t, p, off+11) },
			wantReason: "checksum mismatch: the frame does not verify under the MAC key",
		},
		{
			// A commit relabelled as an abort: the bytes are all still there and
			// every one of them is a legal value, so nothing but the tag can
			// possibly catch it.
			name:       "a rewritten type field does not verify",
			mutate:     func(t *testing.T, p string, off int64) { patch(t, p, off+12, be16(uint16(TypeAbort))) },
			wantReason: "checksum mismatch: the frame does not verify under the MAC key",
		},
		{
			name:       "a non-zero reserved field does not verify",
			mutate:     func(t *testing.T, p string, off int64) { patch(t, p, off+14, be16(1)) },
			wantReason: "reserved field",
		},
	}

	probed := 0
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			path, recs := writeGoodLog(t)
			c := testCodec(t, path)
			off := recs[target].Offset
			probed++

			// THE POSITIVE CONTROL, on this fixture, before anything is touched.
			// Without it a helper that always answered "does not verify" would
			// make every case below pass while proving nothing at all.
			if !frameVerifiesAt(t, c, readFile(t, path), off) {
				t.Fatalf("the UNALTERED frame at offset %d does not verify; the fixture is broken and nothing below would mean anything", off)
			}

			tc.mutate(t, path, off)

			if frameVerifiesAt(t, c, readFile(t, path), off) {
				t.Fatalf("the frame at offset %d STILL VERIFIES after %q: the MAC does not cover what it must", off, tc.name)
			}

			// The reader must reject it too, at exactly this offset, keeping the
			// good prefix in front of it.
			got, end, err := ScanAll(path, KindWAL)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ScanAll after %q = %v, want an error matching ErrCorrupt", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("ScanAll error = %q, want it to contain %q", err.Error(), tc.wantReason)
			}
			if end != off {
				t.Errorf("ScanAll ended at %d, want %d (just past the last good record)", end, off)
			}
			if len(got) != target {
				t.Errorf("ScanAll returned %d records before the damage, want %d", len(got), target)
			}

			// ---------------------------------------------------------------
			// THE LEG THAT DISTINGUISHES A MAC FROM A CRC. Re-tag the altered
			// frame under a key the log does not use -- everything an attacker
			// who knows the algorithm but not the secret can do. Against CRC32C
			// this repairs the frame and it is accepted; against HMAC-SHA256 the
			// verdict must not move.
			// ---------------------------------------------------------------
			forgeTagUnderAnotherKey(t, path, off)

			if frameVerifiesAt(t, c, readFile(t, path), off) {
				t.Fatalf("a frame re-tagged under a DIFFERENT key verifies under this log's key after %q: "+
					"that is a forgery, and it is the exact attack format version 2 exists to stop", tc.name)
			}
			forged, forgedEnd, err := ScanAll(path, KindWAL)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ScanAll accepted a frame forged under another key after %q: err = %v", tc.name, err)
			}
			if forgedEnd != off {
				t.Errorf("after the forgery ScanAll ended at %d, want %d", forgedEnd, off)
			}
			if len(forged) != target {
				t.Errorf("after the forgery ScanAll returned %d records, want %d", len(forged), target)
			}
		})
	}

	// The guard sits AFTER the loop that could have been filtered to nothing: a
	// table that silently emptied itself would otherwise report a pass having
	// probed no alteration site whatever.
	if probed == 0 {
		t.Fatalf("no alteration site was probed: this test asserted NOTHING about the frame MAC")
	}
	if probed != len(tests) {
		t.Errorf("probed %d alteration sites, want all %d", probed, len(tests))
	}
}

// be16 and be32 render a big-endian field, so a table entry can patch one
// without three lines of ceremony.
func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
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
