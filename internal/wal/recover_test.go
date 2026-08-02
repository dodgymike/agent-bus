package wal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// RepairTail is the ONLY code in this package that ever shortens a file
// (invariant 6 permits exactly one exception to append-only: "a verified-corrupt
// tail during recovery"). So the property these tests exist to defend is not
// "the torn tail goes away" -- it is the far more important converse:
//
//	REPAIRTAIL MUST NEVER SHORTEN A FILE EXCEPT FOR A TORN TAIL.
//
// Every negative case below therefore asserts three things, and the first is the
// one that matters: the file is BYTE-FOR-BYTE unchanged, the error is fatal, and
// TailRepair.Truncated is false. Comparing the full bytes rather than the size
// is deliberate -- a repair pass that rewrote a frame in place would keep the
// size and would be exactly the silent, permanent data loss this is guarding
// against.
//
// The positive cases assert the cut landed where ScanAll says the last good
// record ends -- computed, never hardcoded, because frame sizes are
// payload-dependent -- and that what survives is exactly the good prefix.
// ---------------------------------------------------------------------------

// repairTornOps is the well-framed four-record WAL every torn-tail case starts
// from: two complete transactions. The last record (the COMMIT of prepare 3) is
// the frame each case damages, so the surviving prefix is always records 1..3
// and the discarded frame always carried index 4.
func repairTornOps() []walOp {
	return []walOp{
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opPrepare("message", `{"n":2}`), // 3
		opCommit(3),                     // 4 -- damaged by every case below
	}
}

// repairScanClean is the pristine scan of a freshly built file: it fails the
// test if the fixture itself is not well framed, so a broken fixture can never
// masquerade as the behaviour under test.
func repairScanClean(t *testing.T, path string, kind Kind, wantRecords int) ([]Record, int64) {
	t.Helper()
	recs, end, err := ScanAll(path, kind)
	if err != nil {
		t.Fatalf("the fixture is not well framed before it is damaged: %v", err)
	}
	if len(recs) != wantRecords {
		t.Fatalf("the fixture has %d records, want %d", len(recs), wantRecords)
	}
	if got := fileSize(t, path); end != got {
		t.Fatalf("the fixture scans to %d but is %d bytes", end, got)
	}
	return recs, end
}

// repairAssertPrefix proves the repaired file is exactly the expected prefix:
// it scans clean, and every surviving record matches the pristine one at the
// same position -- index, type, offset and payload BYTES.
func repairAssertPrefix(t *testing.T, path string, kind Kind, want []Record) {
	t.Helper()
	got, end, err := ScanAll(path, kind)
	if err != nil {
		t.Fatalf("ScanAll after the repair: %v: a repaired file must scan clean", err)
	}
	if len(got) != len(want) {
		t.Fatalf("the repaired file holds %d records, want the %d-record good prefix", len(got), len(want))
	}
	for i := range want {
		if got[i].Index != want[i].Index || got[i].Type != want[i].Type || got[i].Offset != want[i].Offset {
			t.Fatalf("surviving record %d is {index:%d type:%s offset:%d}, want {index:%d type:%s offset:%d}",
				i, got[i].Index, got[i].Type, got[i].Offset, want[i].Index, want[i].Type, want[i].Offset)
		}
		if !bytes.Equal(got[i].Payload, want[i].Payload) {
			t.Fatalf("surviving record %d has a different payload than before the repair: %q vs %q",
				i, got[i].Payload, want[i].Payload)
		}
	}
	if size := fileSize(t, path); end != size {
		t.Errorf("the repaired file scans to %d but is %d bytes: nothing may be left past the cut", end, size)
	}
}

// repairAssertUntouched is the negative-case assertion: nothing was cut, and the
// bytes on disk are identical to what they were before RepairTail ran.
func repairAssertUntouched(t *testing.T, path string, before []byte, res TailRepair) {
	t.Helper()
	if res.Truncated {
		t.Errorf("TailRepair.Truncated = true, want false: this damage is not a verified torn tail")
	}
	if res.At != 0 || res.Removed != 0 || res.NextIndex != 0 {
		t.Errorf("TailRepair = %+v, want a zero repair (At, Removed and NextIndex all 0)", res)
	}
	after := readFile(t, path)
	if !bytes.Equal(before, after) {
		t.Fatalf("RepairTail CHANGED the file it refused to repair: %d bytes before, %d after: "+
			"a refusal to start must leave the evidence intact", len(before), len(after))
	}
}

// TestWALRepairTailTruncatesTornTail is the positive half of the matrix: the
// shapes a crash mid-append can actually leave behind, all of which are a strict
// PREFIX of a single frame at the very end of the file (Append issues one write
// per frame and poisons the Writer if it fails, so nothing is ever appended
// after a torn write).
func TestWALRepairTailTruncatesTornTail(t *testing.T) {
	cases := []struct {
		name string
		// damage mutates the last frame of a pristine file. recs and cleanEnd
		// describe that file before the damage.
		damage func(t *testing.T, path string, recs []Record, cleanEnd int64)
		// sizePreserved marks the cases where the damage does NOT shorten the
		// file, so the "torn tail" is a whole frame that fails its checksum.
		sizePreserved bool
		wantReason    string
	}{
		{
			// (a) The classic: the frame header landed, the payload did not.
			name: "torn payload with an intact frame header",
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, recs[len(recs)-1].Offset+FrameHeaderSize+2)
			},
			wantReason: "truncated payload: have 2 of",
		},
		{
			// (b) Fewer than 20 bytes of the final frame reached the disk. That
			// can only happen at end of file, so nothing can follow it.
			name: "torn frame header, two bytes in",
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, recs[len(recs)-1].Offset+2)
			},
			wantReason: "truncated frame header: have 2 of 20 bytes",
		},
		{
			// (c) The three boundaries around the frame header: one byte in, one
			// byte short of a whole header, and exactly a whole header. The first
			// two report a short header (FrameEnd 0, so the "nothing follows"
			// proof is "fewer than a header's worth of bytes remain"); the third
			// reports a short PAYLOAD, because the header parsed and declared an
			// extent that runs past the end of the file. Different errors, same
			// verdict: a torn tail.
			name: "exactly one byte of the last frame",
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, recs[len(recs)-1].Offset+1)
			},
			wantReason: "truncated frame header: have 1 of 20 bytes",
		},
		{
			name: "one byte short of a whole frame header",
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, recs[len(recs)-1].Offset+FrameHeaderSize-1)
			},
			wantReason: "truncated frame header: have 19 of 20 bytes",
		},
		{
			name: "exactly a frame header and none of the payload",
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, recs[len(recs)-1].Offset+FrameHeaderSize)
			},
			wantReason: "truncated payload: have 0 of",
		},
		{
			// (d) A COMPLETE final frame whose checksum fails. The file is not
			// short at all -- FrameEnd lands exactly on the end of the file -- so
			// nothing follows the damage and it is still a tail. This is the shape
			// a write that reached the platter torn (rather than short) leaves.
			name:          "complete final frame with a flipped payload bit",
			sizePreserved: true,
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				flipByte(t, path, recs[len(recs)-1].Offset+FrameHeaderSize+1)
			},
			wantReason: "checksum mismatch",
		},
		{
			// (e) The exact upper boundary of the one-frame rule: a length field
			// corrupted to precisely MaxPayloadSize declares the largest extent a
			// writer could legally have produced, so it is still a frame and still
			// reaches past the end of the file. One byte more is fatal -- see the
			// refusal table.
			name:          "length field corrupted to exactly the maximum payload size",
			sizePreserved: true,
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[len(recs)-1].Offset, []byte{0x00, 0x10, 0x00, 0x00}) // 1 MiB
			},
			wantReason: "truncated payload",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, path, _, _ := buildWAL(t, repairTornOps()...)
			recs, cleanEnd := repairScanClean(t, path, KindWAL, 4)

			// Computed from the scan, never hardcoded: the cut must land at the
			// end of the last record that verified, which is where the damaged
			// frame starts.
			wantAt := recs[len(recs)-1].Offset
			wantNextIndex := recs[len(recs)-1].Index // the index the torn frame carried

			tc.damage(t, path, recs, cleanEnd)
			damagedSize := fileSize(t, path)
			if tc.sizePreserved && damagedSize != cleanEnd {
				t.Fatalf("the damage changed the file size (%d -> %d); this case is meant to leave a whole frame in place",
					cleanEnd, damagedSize)
			}

			// A bare Replay must FAIL on these bytes, or the case proves nothing:
			// the whole point is that recovery policy turns a fatal read into a
			// repaired start.
			if _, err := Replay(path, nil); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Replay of the damaged file = %v, want ErrCorrupt: the tail is not actually torn", err)
			}

			res, err := RepairTail(path, KindWAL, nil)
			if err != nil {
				t.Fatalf("RepairTail on a torn tail: %v, want a repair", err)
			}
			if !res.Truncated {
				t.Fatalf("TailRepair = %+v, want Truncated true", res)
			}
			if res.Path != path {
				t.Errorf("TailRepair.Path = %q, want %q", res.Path, path)
			}
			if res.At != wantAt {
				t.Fatalf("TailRepair.At = %d, want %d (the end of the last verified-good record)", res.At, wantAt)
			}
			if want := damagedSize - wantAt; res.Removed != want {
				t.Errorf("TailRepair.Removed = %d, want %d (the file was %d bytes and was cut to %d)",
					res.Removed, want, damagedSize, wantAt)
			}
			if res.NextIndex != wantNextIndex {
				t.Errorf("TailRepair.NextIndex = %d, want %d: the next append reissues the index the discarded frame carried",
					res.NextIndex, wantNextIndex)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("TailRepair.Reason = %q, want it to contain %q: the operator log must say WHY the tail went",
					res.Reason, tc.wantReason)
			}
			if got := fileSize(t, path); got != res.At {
				t.Fatalf("the file is %d bytes after the repair, want exactly At = %d", got, res.At)
			}

			// The survivors are exactly the good prefix, byte for byte, and the
			// file now scans clean.
			repairAssertPrefix(t, path, KindWAL, recs[:len(recs)-1])

			// And it replays: one committed entry, with prepare 3 left dangling
			// because the COMMIT that resolved it was the frame that was torn.
			var c collector
			r, err := Replay(path, c.fn)
			if err != nil {
				t.Fatalf("Replay after the repair: %v", err)
			}
			want := []Committed{wantC(1, 2, "message", `{"n":1}`)}
			if !sameCommitted(c.got, want) {
				t.Fatalf("the repaired log replayed %s, want %s", showCommitted(c.got), showCommitted(want))
			}
			if len(r.Dangling) != 1 || r.Dangling[0] != 3 {
				t.Errorf("Dangling = %v, want [3]: the prepare whose commit was torn away is unresolved", r.Dangling)
			}
			if r.NextIndex != wantNextIndex {
				t.Errorf("NextIndex after the repair = %d, want %d", r.NextIndex, wantNextIndex)
			}
		})
	}
}

// TestWALRepairTailRefusesDamageThatIsNotATornTail is the half that keeps the
// data. Every case here is damage that a partial write CANNOT have produced, or
// damage with records after it, or damage in the file header -- and for each,
// truncating would delete records that are sitting on disk perfectly intact.
// The verdict is a refusal to start, which an operator can recover from; a
// silent truncation is not.
func TestWALRepairTailRefusesDamageThatIsNotATornTail(t *testing.T) {
	// Six records: three complete transactions. Records 5 and 6 are a
	// prepare/commit pair -- ACCEPTED HISTORY, already acknowledged to a client
	// -- that sits after every middle-of-file case below, so any truncation here
	// would be real, permanent data loss.
	sixOps := []walOp{
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opPrepare("message", `{"n":2}`), // 3
		opCommit(3),                     // 4
		opPrepare("message", `{"n":3}`), // 5
		opCommit(5),                     // 6
	}

	// The four vetoes in truncatableTail, named. Asserting WHICH one fired is
	// what stops a case passing for an accidental reason -- and vetoIllegalExtent
	// in particular is the one that is easy to lose, because the damage there
	// DOES declare an extent past the end of the file and a naive "the extent
	// reaches EOF, so it is a tail" rule would truncate it.
	const (
		vetoFileHeader    = "the damage is in the file header, so a cut would be at offset 0"
		vetoFrameIntact   = "the frame's checksum verified, so a partial write cannot explain it"
		vetoRecordsFollow = "the frame's declared extent ends before EOF, so records follow the damage"
		vetoIllegalExtent = "the declared extent is not a frame this writer could have produced"
	)

	cases := []struct {
		name string
		// build lays down the file. It returns the path.
		build func(t *testing.T) string
		// damage mutates it. recs describes the pristine file.
		damage func(t *testing.T, path string, recs []Record, cleanEnd int64)
		// wantVeto is the rule that must be the reason this is not a tail.
		wantVeto string
		wantMsg  string
		wantNote string // why this must be fatal, printed on failure
	}{
		{
			// (e) THE FLAGSHIP DATA-LOSS CASE. One flipped bit in an early
			// record, with committed records after it.
			name:  "checksum mismatch in a middle record",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				flipByte(t, path, recs[1].Offset+FrameHeaderSize+1)
			},
			wantVeto: vetoRecordsFollow,
			wantMsg:  "checksum mismatch",
			wantNote: "the frame declares an extent that ends well before the end of the file, so records follow it",
		},
		{
			// (f) Without the upper bound in truncatableTail this one truncates
			// away everything after it: readFrame reports an absurd payloadLen
			// with a gigantic FrameEnd, which sails past the ">= size" test.
			name:  "absurd length field in a middle record",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[1].Offset, []byte{0xff, 0xff, 0xff, 0xff})
			},
			wantVeto: vetoIllegalExtent,
			wantMsg:  "rejected without allocating",
			wantNote: "a 4 GiB declared extent must not be mistaken for a frame that reaches the end of the file",
		},
		{
			// (g) The same corruption in the LAST record. Still fatal: the
			// declared extent is not a legal frame, so nothing about it is
			// verifiable as a torn tail.
			name:  "absurd length field in the last record",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[len(recs)-1].Offset, []byte{0xff, 0xff, 0xff, 0xff})
			},
			wantVeto: vetoIllegalExtent,
			wantMsg:  "rejected without allocating",
			wantNote: "being at the end of the file does not make an illegal frame a torn tail",
		},
		{
			// The exact boundary of the one-frame rule, one byte the wrong side
			// of it. MaxPayloadSize is repairable (see the truncation table);
			// MaxPayloadSize+1 is a length no writer could have produced.
			name:  "length field one byte over the maximum in the last record",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[len(recs)-1].Offset, []byte{0x00, 0x10, 0x00, 0x01}) // 1 MiB + 1
			},
			wantVeto: vetoIllegalExtent,
			wantMsg:  "exceeds the 1048576-byte maximum",
			wantNote: "the declared extent must be a frame the writer could actually have produced",
		},
		{
			// (h) THE SECOND FLAGSHIP CASE. The frame is at the very end of the
			// file and its CRC verifies -- which a partial write cannot produce.
			// So a record was lost from, or resurrected in, the file, and that is
			// fatal even at the tail.
			name: "index out of sequence at the tail",
			build: func(t *testing.T) string {
				_, p, _, _ := buildWAL(t, repairTornOps()...)
				payload, err := encodePrepare("message", jsonBody(`{"n":9}`), time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatalf("encodePrepare: %v", err)
				}
				// Index 6 where 5 is due: a whole record is missing before it.
				appendRawFrame(t, p, 6, TypePrepare, payload)
				return p
			},
			damage:   func(t *testing.T, path string, recs []Record, cleanEnd int64) {},
			wantVeto: vetoFrameIntact,
			wantMsg:  "out of sequence",
			wantNote: "the checksum verified, so a torn write cannot explain it, so it is not a tail no matter where it sits",
		},
		{
			// (i) A bad FILE header. The cut would be at offset 0 and would
			// delete the entire log.
			name:  "bad file magic",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, 0, []byte("XGNTBUSW"))
			},
			wantVeto: vetoFileHeader,
			wantMsg:  "bad magic",
			wantNote: "damage at offset 0 is never a tail: cutting there deletes the whole file",
		},
		{
			name:  "unknown format version",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, 8, []byte{0x00, 0x00, 0x00, 0x02})
			},
			wantVeto: vetoFileHeader,
			wantMsg:  "format version 2, want 1",
			wantNote: "a file whose layout this code does not know must never be edited by it",
		},
		{
			// The case that makes the file-header veto load-bearing rather than
			// belt-and-braces. A file with FEWER than 16 bytes is a short read at
			// offset 0 with no declared extent -- which is, structurally, the exact
			// signature of a torn frame header ("fewer than a header's worth of
			// bytes remain, so nothing can follow"). Only the "damage below
			// FileHeaderSize is never a tail" rule tells them apart; without it the
			// cut would be at offset 0 and the file would be emptied.
			name:  "truncated file header",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, 9)
			},
			wantVeto: vetoFileHeader,
			wantMsg:  "truncated file header: have 9 of 16 bytes",
			wantNote: "a short FILE header must never be mistaken for a short FRAME header; the cut would be at offset 0",
		},
		{
			// (j) THE DELIBERATE CONSERVATIVE GAP, pinned so it stays deliberate.
			// A NUL tail longer than one frame is what some filesystems expose for
			// a write that never landed. It is NOT truncated: a zero length field
			// declares a 20-byte frame, which does not reach the end of the file,
			// so the region is unverifiable and the answer is to refuse to start.
			name:  "a NUL tail longer than one frame",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, fileMode)
				if err != nil {
					t.Fatalf("OpenFile to append NULs: %v", err)
				}
				defer f.Close()
				if _, err := f.Write(make([]byte, 200)); err != nil {
					t.Fatalf("appending NULs: %v", err)
				}
			},
			wantVeto: vetoRecordsFollow,
			wantMsg:  "corrupt at offset",
			wantNote: "200 NUL bytes are more than one frame, so the region cannot be proven to be a single torn frame",
		},
		{
			// (k) reserved != 0 in a middle frame: structurally impossible for
			// this writer, and there are records after it.
			name:  "non-zero reserved field in a middle frame",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[1].Offset+14, []byte{0x00, 0x01})
			},
			wantVeto: vetoRecordsFollow,
			wantMsg:  "reserved field",
			wantNote: "the declared extent ends before the end of the file, so committed records follow",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			path := tc.build(t)
			recs, cleanEnd, err := ScanAll(path, KindWAL)
			if err != nil {
				// The out-of-sequence case is damaged by build, not by damage, so
				// its fixture does not scan -- that is the point of it.
				recs, cleanEnd = nil, 0
			}
			tc.damage(t, path, recs, cleanEnd)

			before := readFile(t, path)
			res, err := RepairTail(path, KindWAL, nil)
			if err == nil {
				t.Fatalf("RepairTail returned no error (%+v) for damage that is not a torn tail: %s", res, tc.wantNote)
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("RepairTail err = %v, want errors.Is(err, ErrCorrupt)", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("RepairTail err = %q, want it to contain %q", err, tc.wantMsg)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("RepairTail err = %q, does not name the file", err)
			}
			repairAssertUntouched(t, path, before, res)

			// WHICH rule saved the file. Asserting the veto, not merely the
			// refusal, is what keeps a case honest: without it, a case could pass
			// because the classifier happened to reject it for an unrelated
			// reason, and would go on passing after the rule it was written to
			// defend had been deleted.
			var ce *CorruptError
			if !errors.As(err, &ce) {
				t.Fatalf("RepairTail err = %v, want a *CorruptError", err)
			}
			size := int64(len(before))
			switch tc.wantVeto {
			case vetoFileHeader:
				if ce.Offset >= FileHeaderSize {
					t.Errorf("CorruptError.Offset = %d, want it inside the %d-byte file header: %s",
						ce.Offset, FileHeaderSize, tc.wantVeto)
				}
			case vetoFrameIntact:
				if !ce.FrameIntact {
					t.Errorf("CorruptError.FrameIntact = false, want true: %s", tc.wantVeto)
				}
				// And it really is the LAST frame, which is the whole point: this
				// damage sits exactly where a torn tail would, and is still fatal.
				if ce.FrameEnd < size {
					t.Errorf("CorruptError.FrameEnd = %d, want it at the end of the %d-byte file: "+
						"the case is meant to put intact-frame damage AT the tail", ce.FrameEnd, size)
				}
			case vetoRecordsFollow:
				if ce.FrameIntact {
					t.Errorf("CorruptError.FrameIntact = true, want false for %q", tc.name)
				}
				if ce.FrameEnd == 0 || ce.FrameEnd >= size {
					t.Errorf("CorruptError.FrameEnd = %d in a %d-byte file, want a declared extent that ends BEFORE "+
						"the end of the file: %s", ce.FrameEnd, size, tc.wantVeto)
				}
			case vetoIllegalExtent:
				if ce.FrameIntact {
					t.Errorf("CorruptError.FrameIntact = true, want false for %q", tc.name)
				}
				// THE HAZARD, stated as an assertion: the declared extent DOES
				// reach past the end of the file, so a rule that only asked
				// "does anything follow the damage?" would have truncated here --
				// in the middle-record cases, taking every committed record after
				// it. Only the upper bound on the extent stops that.
				if ce.FrameEnd < size {
					t.Errorf("CorruptError.FrameEnd = %d in a %d-byte file: this case is meant to declare an extent "+
						"that runs past the end of the file", ce.FrameEnd, size)
				}
				if max := ce.Offset + FrameHeaderSize + MaxPayloadSize; ce.FrameEnd <= max {
					t.Errorf("CorruptError.FrameEnd = %d, want it past the largest legal frame end %d: %s",
						ce.FrameEnd, max, tc.wantVeto)
				}
			default:
				t.Fatalf("case %q has no wantVeto", tc.name)
			}

			// And the refusal reaches an operator: Open fails and STILL leaves the
			// bytes alone. This is the assertion that would catch a future change
			// wiring truncation in somewhere other than RepairTail.
			dir := filepath.Dir(path)
			if filepath.Base(path) == WALFileName {
				app := &testApplier{}
				if l, err := Open(LogOptions{Dir: dir, Applier: app}); err == nil {
					_ = l.Close()
					t.Fatalf("Open succeeded on damage that is not a torn tail: %s", tc.wantNote)
				}
				if after := readFile(t, path); !bytes.Equal(before, after) {
					t.Fatalf("a failed Open changed the WAL: %d bytes before, %d after", len(before), len(after))
				}
			}
		})
	}
}

// TestWALRepairTailNoOp covers the three shapes that need no repair at all --
// which is the overwhelmingly common case, so getting it wrong would be
// spectacular. None of them may touch the file.
func TestWALRepairTailNoOp(t *testing.T) {
	t.Run("a clean, well-framed WAL", func(t *testing.T) {
		_, path, _, _ := buildWAL(t, repairTornOps()...)
		before := readFile(t, path)

		res, err := RepairTail(path, KindWAL, nil)
		if err != nil {
			t.Fatalf("RepairTail on a clean log: %v", err)
		}
		repairAssertUntouched(t, path, before, res)
		if res.Path != path {
			t.Errorf("TailRepair.Path = %q, want %q", res.Path, path)
		}
		if res.Reason != "" {
			t.Errorf("TailRepair.Reason = %q, want empty: nothing was cut", res.Reason)
		}
	})

	t.Run("a file that does not exist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), WALFileName)

		res, err := RepairTail(path, KindWAL, nil)
		if err != nil {
			t.Fatalf("RepairTail on a missing file: %v, want it treated as nothing to repair", err)
		}
		if res.Truncated {
			t.Errorf("TailRepair.Truncated = true for a file that does not exist")
		}
		// It must not have CREATED one either: OpenWriter owns file creation, and
		// a recovery pass that makes files would mask a wrong -dir.
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("RepairTail created %s (stat err = %v); it must never create a file", path, err)
		}
	})

	t.Run("a zero-length file", func(t *testing.T) {
		// The crash window between creating the file and writing its header. It
		// provably holds no record, so it is nothing to repair -- OpenWriter heals
		// it, and the two must agree.
		path := filepath.Join(t.TempDir(), WALFileName)
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
		if err != nil {
			t.Fatalf("creating a zero-length WAL: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("closing the zero-length WAL: %v", err)
		}

		res, err := RepairTail(path, KindWAL, nil)
		if err != nil {
			t.Fatalf("RepairTail on a zero-length file: %v", err)
		}
		if res.Truncated {
			t.Errorf("TailRepair.Truncated = true for a zero-length file")
		}
		if got := fileSize(t, path); got != 0 {
			t.Errorf("the file is %d bytes after RepairTail, want it left at 0", got)
		}
	})
}

// TestWALRepairTailKinds proves the pass is kind-aware in both directions: it
// repairs an AUDIT file (DUR-5 writes those), and it refuses a file whose magic
// says it is something else -- because reading a WAL as an audit log means the
// caller's idea of what this file is has come apart from the file's own, and
// cutting bytes off it on that basis is how a log gets destroyed by a
// configuration mistake.
func TestWALRepairTailKinds(t *testing.T) {
	// buildAudit lays down a two-record audit log by hand: audit records have no
	// codec in this package yet, so the payloads are opaque JSON.
	buildAudit := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "bus.audit")
		w, err := OpenWriter(path, KindAudit)
		if err != nil {
			t.Fatalf("OpenWriter(audit): %v", err)
		}
		for _, p := range []string{`{"message_id":"m1","seq":1}`, `{"message_id":"m2","seq":2}`} {
			if _, err := w.Append(TypeAuditMessage, []byte(p)); err != nil {
				_ = w.Close()
				t.Fatalf("Append(audit): %v", err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close(audit): %v", err)
		}
		return path
	}

	t.Run("repairs a torn tail in an audit file", func(t *testing.T) {
		path := buildAudit(t)
		recs, cleanEnd := repairScanClean(t, path, KindAudit, 2)
		last := recs[len(recs)-1]
		if cleanEnd-last.Offset < FrameHeaderSize+4 {
			t.Fatalf("the last audit frame is %d bytes: too small to tear inside its payload", cleanEnd-last.Offset)
		}
		truncate(t, path, last.Offset+FrameHeaderSize+3)

		res, err := RepairTail(path, KindAudit, nil)
		if err != nil {
			t.Fatalf("RepairTail on a torn audit tail: %v", err)
		}
		if !res.Truncated || res.At != last.Offset {
			t.Fatalf("TailRepair = %+v, want Truncated true at %d", res, last.Offset)
		}
		if res.NextIndex != last.Index {
			t.Errorf("TailRepair.NextIndex = %d, want %d", res.NextIndex, last.Index)
		}
		repairAssertPrefix(t, path, KindAudit, recs[:len(recs)-1])

		// The repaired audit log is appendable again, and the next record takes
		// the index the torn one would have had.
		w, err := OpenWriter(path, KindAudit)
		if err != nil {
			t.Fatalf("OpenWriter on the repaired audit file: %v", err)
		}
		defer w.Close()
		if got := w.NextIndex(); got != res.NextIndex {
			t.Errorf("the writer resumes at index %d, want %d", got, res.NextIndex)
		}
	})

	t.Run("a WAL read as an audit file is fatal", func(t *testing.T) {
		_, path, _, _ := buildWAL(t, repairTornOps()...)
		before := readFile(t, path)

		res, err := RepairTail(path, KindAudit, nil)
		if err == nil {
			t.Fatalf("RepairTail succeeded (%+v) reading a WAL as an audit file", res)
		}
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
		if !strings.Contains(err.Error(), "file is a wal file, want a audit file") {
			t.Errorf("err = %q, want it to say the file is the wrong kind", err)
		}
		repairAssertUntouched(t, path, before, res)
	})

	t.Run("an unknown kind is fatal", func(t *testing.T) {
		_, path, _, _ := buildWAL(t, repairTornOps()...)
		before := readFile(t, path)

		res, err := RepairTail(path, Kind(0), nil)
		if err == nil {
			t.Fatalf("RepairTail succeeded (%+v) for an unknown kind", res)
		}
		if !errors.Is(err, ErrUnknownKind) {
			t.Errorf("err = %v, want ErrUnknownKind", err)
		}
		repairAssertUntouched(t, path, before, res)
	})
}

// TestWALRepairTailIsOneShot pins "one cut per start, ever". A second RepairTail
// over an already-repaired file must find nothing to do: iterating "truncate
// until it parses" would happily eat an entire log one frame at a time, so the
// no-op second pass is the observable form of that rule.
func TestWALRepairTailIsOneShot(t *testing.T) {
	_, path, _, _ := buildWAL(t, repairTornOps()...)
	recs, _ := repairScanClean(t, path, KindWAL, 4)
	truncate(t, path, recs[len(recs)-1].Offset+FrameHeaderSize+2)

	first, err := RepairTail(path, KindWAL, nil)
	if err != nil {
		t.Fatalf("first RepairTail: %v", err)
	}
	if !first.Truncated {
		t.Fatalf("first RepairTail = %+v, want Truncated true", first)
	}
	repaired := readFile(t, path)

	second, err := RepairTail(path, KindWAL, nil)
	if err != nil {
		t.Fatalf("second RepairTail on an already-repaired file: %v", err)
	}
	repairAssertUntouched(t, path, repaired, second)

	// And a third, for good measure: repeated recovery passes are idempotent, so
	// a restart loop cannot nibble the log away.
	third, err := RepairTail(path, KindWAL, nil)
	if err != nil {
		t.Fatalf("third RepairTail: %v", err)
	}
	repairAssertUntouched(t, path, repaired, third)
}

// TestWALRepairTailLogsTheCut: the task is "detected, LOGGED, and truncated".
// The warning is emitted BEFORE the cut, so that a crash during the truncate
// still leaves an operator a record of what was about to be discarded, and the
// confirmation after it. Both must name the file, the offset and the reason.
func TestWALRepairTailLogsTheCut(t *testing.T) {
	_, path, _, _ := buildWAL(t, repairTornOps()...)
	recs, _ := repairScanClean(t, path, KindWAL, 4)
	truncate(t, path, recs[len(recs)-1].Offset+FrameHeaderSize+2)

	var buf bytes.Buffer
	res, err := RepairTail(path, KindWAL, logging.New(&buf, logging.LevelDebug))
	if err != nil {
		t.Fatalf("RepairTail: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"wal truncating a corrupt tail", // before
		"wal truncated a corrupt tail",  // after
		path,
		"truncated payload",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the repair log does not mention %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "truncating"), strings.Index(out, "wal truncated"); i < 0 || j < 0 || i > j {
		t.Errorf("the warning must be logged BEFORE the cut is confirmed:\n%s", out)
	}
	// A nil Logger is the common case in tests and in a server started without
	// one; it must not panic and must repair identically.
	if !res.Truncated {
		t.Fatalf("TailRepair = %+v, want Truncated true", res)
	}
}

// TestWALRepairTailThroughOpen is the whole feature seen from where a server
// sees it: a WAL with a genuinely torn final frame, an Applier, and a restart
// that has to produce a state which is a PREFIX OF ACCEPTED HISTORY and then
// carry on writing.
func TestWALRepairTailThroughOpen(t *testing.T) {
	dir, path, nextIndex, cleanEnd := buildWAL(t,
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opPrepare("agent", ""),          // 3 -- a nil body, which must survive as nil
		opCommit(3),                     // 4
	)
	if nextIndex != 5 {
		t.Fatalf("the fixture ends at next index %d, want 5", nextIndex)
	}

	// The torn frame: exactly what a crash mid-append leaves -- a strict PREFIX
	// of the one frame that was in flight. It is built with the real encoder, so
	// the bytes are the bytes Append would have written.
	payload, err := encodePrepare("message", jsonBody(`{"n":9}`), time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("encodePrepare: %v", err)
	}
	frame := encodeFrame(5, TypePrepare, payload)
	partial := len(frame) / 2
	if partial <= FrameHeaderSize {
		t.Fatalf("half a frame is %d bytes, which is not past the %d-byte header", partial, FrameHeaderSize)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		t.Fatalf("OpenFile to append a partial frame: %v", err)
	}
	if _, err := f.Write(frame[:partial]); err != nil {
		t.Fatalf("appending a partial frame: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A bare Replay refuses these bytes; Open repairs them. That contrast is the
	// DUR-3/DUR-4 boundary.
	if _, err := Replay(path, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Replay of the torn file = %v, want ErrCorrupt", err)
	}

	app := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: app})
	if err != nil {
		t.Fatalf("Open on a torn tail: %v, want a repaired start", err)
	}
	defer l.Close()

	rec := l.Recovered()
	rep := rec.Repaired
	if !rep.Truncated {
		t.Fatalf("Recovered().Repaired = %+v, want Truncated true", rep)
	}
	if rep.Path != path {
		t.Errorf("Repaired.Path = %q, want %q", rep.Path, path)
	}
	if rep.At != cleanEnd {
		t.Errorf("Repaired.At = %d, want %d (the end of the last good record)", rep.At, cleanEnd)
	}
	if rep.Removed != int64(partial) {
		t.Errorf("Repaired.Removed = %d, want %d (the half-frame that was appended)", rep.Removed, partial)
	}
	if rep.NextIndex != 5 {
		t.Errorf("Repaired.NextIndex = %d, want 5", rep.NextIndex)
	}
	if rep.Reason == "" {
		t.Error("Repaired.Reason is empty: the repair must say why the tail went")
	}
	if got := fileSize(t, path); got != rep.At {
		t.Errorf("the file is %d bytes, want %d", got, rep.At)
	}
	if rec.NextIndex != 5 || rec.EndOffset != cleanEnd {
		t.Errorf("Recovered() = {NextIndex:%d EndOffset:%d}, want {5 %d}", rec.NextIndex, rec.EndOffset, cleanEnd)
	}
	if len(rec.Dangling) != 0 {
		t.Errorf("Dangling = %v, want none: the good prefix ends on a commit", rec.Dangling)
	}

	// The Applier saw exactly the committed entries of the good prefix, in order,
	// with byte-identical bodies -- including the nil body, which must not come
	// back as an empty slice.
	want := []Committed{
		wantC(1, 2, "message", `{"n":1}`),
		wantC(3, 4, "agent", ""),
	}
	got := make([]Committed, app.count())
	for i := range got {
		got[i] = app.at(i)
	}
	if !sameCommitted(got, want) {
		t.Fatalf("Open applied %s, want %s", showCommitted(got), showCommitted(want))
	}

	// And it keeps writing. Index 5 is REISSUED on purpose: the frame that
	// carried it never completed its fsync, so nothing it held was ever
	// acknowledged and no client, peer or relay can have observed that id.
	c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":10}`)})
	if err != nil {
		t.Fatalf("Write after the repair: %v", err)
	}
	if c.PrepareIndex != 5 || c.CommitIndex != 6 {
		t.Fatalf("the write after the repair got {prepare:%d commit:%d}, want {5 6}", c.PrepareIndex, c.CommitIndex)
	}
	if _, _, err := ScanAll(path, KindWAL); err != nil {
		t.Fatalf("the file does not scan clean after the repair and a write: %v", err)
	}
	assertIndicesUnique(t, path)
}

// TestWALRepairTailNotReachableFromSemanticDamage is the integration-level proof
// that truncation is unreachable from a SEMANTIC failure.
//
// The file below is perfectly framed -- every checksum verifies, the index
// sequence has no holes -- and the damage is in the LAST record: a second COMMIT
// of prepare 1, which names something that is no longer an open prepare. Replay
// rejects it. If that rejection could reach the truncation path, recovery would
// cut a fully readable log; and because the damage happens to be at the end
// here, this is precisely the case where a naive "the error is at the tail, so
// truncate" rule would fire.
//
// RepairTail must see nothing wrong at all (framing is fine), and Open must fail
// with the file untouched.
func TestWALRepairTailNotReachableFromSemanticDamage(t *testing.T) {
	dir, path, _, _ := buildWAL(t,
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opCommit(1),                     // 3 -- prepare 1 was already committed
	)
	before := readFile(t, path)

	// The framing pass has no opinion: it looks at frames, not at what they mean.
	res, err := RepairTail(path, KindWAL, nil)
	if err != nil {
		t.Fatalf("RepairTail on a well-framed file: %v, want no error: it is a framing-only pass", err)
	}
	repairAssertUntouched(t, path, before, res)

	// The refusal comes from Replay, and it is fatal.
	app := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: app})
	if err == nil {
		_ = l.Close()
		t.Fatal("Open succeeded on a semantically damaged log; recovery must be a refusal to start")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("Open err = %v, want ErrCorrupt", err)
	}
	var ce *CorruptError
	if !errors.As(err, &ce) {
		t.Fatalf("Open err = %v, want a *CorruptError", err)
	}
	if !ce.FrameIntact {
		t.Errorf("CorruptError.FrameIntact = false, want true: this frame checksummed, so it is not a torn tail")
	}
	if !strings.Contains(err.Error(), "not an open prepare") {
		t.Errorf("Open err = %q, want it to name the semantic failure", err)
	}
	if after := readFile(t, path); !bytes.Equal(before, after) {
		t.Fatalf("a failed Open changed the WAL: %d bytes before, %d after: a semantic error must never truncate",
			len(before), len(after))
	}
}
