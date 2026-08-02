package wal

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// indexedOp describes one record to lay down at a CHOSEN index, which is the
// one thing the ordinary buildWAL helper cannot do: it appends through Writer,
// and Writer -- correctly -- allocates indices densely. A log with a HOLE in its
// index sequence is now legal, permanent state (recovery discards damaged
// records and never renumbers the survivors), so it has to be constructible.
type indexedOp struct {
	index        uint64
	typ          Type
	kind         string
	body         string
	prepareIndex uint64
}

// buildWALIndexed writes a WAL containing exactly these records, at exactly
// these indices, byte for byte as the writer would have written them.
func buildWALIndexed(t *testing.T, ops []indexedOp) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), WALFileName)
	c := testCodec(t, path)
	buf := c.makeFileHeader(KindWAL)
	ts := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	for _, op := range ops {
		var payload []byte
		var err error
		switch op.typ {
		case TypePrepare:
			payload, err = encodePrepare(op.kind, json.RawMessage(op.body), ts)
		case TypeCommit:
			payload, err = encodeCommit(op.prepareIndex)
		case TypeAbort:
			payload, err = encodeAbort(op.prepareIndex, "test")
		default:
			t.Fatalf("buildWALIndexed: unsupported type %s", op.typ)
		}
		if err != nil {
			t.Fatalf("buildWALIndexed: encode %s: %v", op.typ, err)
		}
		buf = append(buf, c.encodeFrame(op.index, op.typ, payload)...)
	}
	if err := os.WriteFile(path, buf, fileMode); err != nil {
		t.Fatalf("buildWALIndexed: write %s: %v", path, err)
	}
	return path
}

// TestWALPayloadsCannotCarryAFrameHeader pins the invariant that makes
// resyncFrom safe against a forged record while the checksum is still an
// unkeyed CRC32C.
//
// Security demonstrated that a byte sequence shaped like a frame header, placed
// inside a WAL payload, is admitted by the forward search after damage, copied
// into the rewritten log by rewriteLog as if it were genuine, and delivered to
// the Applier as accepted history. The only thing stopping a client doing that
// today is that a frame header CANNOT BE EXPRESSED in a WAL payload: every
// header contains NUL bytes, and the sole writer of WAL payloads runs bodies
// through canonicalBody -> json.Compact, which rejects a raw control byte in a
// string.
//
// That was true by accident and nothing recorded it. This test makes it a
// checked property, so that widening the payload channel to arbitrary bytes --
// binary bodies, base64-decoded bodies, compression, E2E ciphertext -- fails
// here and forces the keyed MAC to land first, rather than silently removing
// the only thing holding the forgery out.
func TestWALPayloadsCannotCarryAFrameHeader(t *testing.T) {
	// A minimal plausible forged header: a small payload length, an index, a
	// known type, reserved zero, and a checksum. Whatever the values, the bytes
	// below are what a candidate must contain to be considered at all.
	var hdr [FrameHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], 8)
	binary.BigEndian.PutUint64(hdr[4:12], 9001)
	binary.BigEndian.PutUint16(hdr[12:14], uint16(TypePrepare))
	binary.BigEndian.PutUint16(hdr[14:16], 0)
	binary.BigEndian.PutUint32(hdr[16:20], 0xdeadbeef)

	nuls := 0
	for _, b := range hdr {
		if b == 0 {
			nuls++
		}
	}
	if nuls == 0 {
		t.Fatalf("a frame header for index 9001 contains no NUL byte (%x); the argument in resyncFrom's doc that a JSON payload cannot express one has just stopped holding, and the keyed MAC is now a blocking precondition for anything that reaches this search", hdr)
	}

	// The write path must refuse a body carrying those bytes. json.Compact is
	// what does it, and canonicalBody is the only door into a WAL payload.
	body := `{"forged":"` + string(hdr[:]) + `"}`
	if _, err := canonicalBody(json.RawMessage(body)); err == nil {
		t.Fatalf("canonicalBody accepted a body containing raw frame-header bytes; a client can now plant a frame header inside a WAL payload, which is exactly the input resyncFrom cannot tell from a real record while the checksum is unkeyed")
	}

	// And through the real front door, so this cannot be satisfied by a helper
	// nobody calls.
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(body)}); err == nil {
		t.Fatal("Log.Write accepted a body containing raw frame-header bytes")
	} else if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("Write rejected it, but not as invalid JSON: %v -- the rejection must come from the JSON validation this invariant depends on", err)
	}
}

// TestWALResyncSurvivesALargeIndexHole is the regression for the data-loss bug
// security reproduced against the first version of resyncFrom.
//
// A repaired log has PERMANENT index holes -- survivors are never renumbered,
// because renumbering would reuse ids. resyncFrom originally narrowed candidates
// by an index-DENSITY window: a candidate's index had to be no larger than the
// number of records that could still fit before the end of the file. After a
// large hole that bound is smaller than the real next index, so the genuine
// record was rejected by a cheap filter, the search reported "no intact record
// follows", and recovery deleted every committed record to the end of the file
// -- WHILE LOGGING THAT IT HAD FOUND A TORN TAIL, so the operator's only signal
// said the opposite of the truth.
//
// The rule this pins: A BOUNDED SEARCH FINDING NOTHING IS NEVER ON ITS OWN
// GROUNDS FOR "NOTHING FOLLOWS".
func TestWALResyncSurvivesALargeIndexHole(t *testing.T) {
	// Indices 1, 2, then a hole of fifty thousand, then an acknowledged
	// prepare/commit pair. Exactly the shape a previous repair leaves behind.
	path := buildWALIndexed(t, []indexedOp{
		{index: 1, typ: TypePrepare, kind: "message", body: `{"n":1}`},
		{index: 2, typ: TypeCommit, prepareIndex: 1},
		{index: 50001, typ: TypePrepare, kind: "message", body: `{"n":2}`},
		{index: 50002, typ: TypeCommit, prepareIndex: 50001},
	})
	recs, _, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("the fixture must scan clean before it is damaged: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("fixture has %d records, want 4", len(recs))
	}

	// One flipped bit in the length field of record 2 -- the record immediately
	// BEFORE the hole -- overshooting the end of the file. That is the ordinary
	// "looks exactly like a torn tail" damage, and putting it here is what makes
	// the search start from lastIndex = 1 and have to cross the hole to find
	// index 50001.
	orig := uint32(len(recs[1].Payload))
	flipped := orig ^ 0x00010000
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], flipped)
	patch(t, path, recs[1].Offset, b[:])

	res, out, err := captureRepair(t, path, KindWAL)
	if err != nil {
		t.Fatalf("RepairLog: %v -- damage must never stop the server starting", err)
	}

	// THE ASSERTION THAT MATTERS: the pair behind the hole is still there.
	after, _, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("the repaired log does not scan: %v", err)
	}
	var kept []uint64
	for _, r := range after {
		kept = append(kept, r.Index)
	}
	for _, want := range []uint64{50001, 50002} {
		found := false
		for _, got := range kept {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("record %d is gone; surviving indices are %v. A bounded index window rejected the real next record across a %d-index hole, and recovery then deleted an ACKNOWLEDGED write to the end of the file. Repair = %+v\nlog:\n%s",
				want, kept, 50001-2, res, out)
		}
	}
	if res.Truncated {
		t.Errorf("Repair.Truncated = true (at %d, removed %d): there IS an intact record after the damage, so this is not a tail\nlog:\n%s",
			res.At, res.Removed, out)
	}
	if res.NextIndex != 50003 {
		t.Errorf("Repair.NextIndex = %d, want 50003: the high-water mark must not roll back over surviving records", res.NextIndex)
	}
}
