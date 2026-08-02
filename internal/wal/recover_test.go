package wal

import (
	"bytes"
	"encoding/binary"
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

// appendBytes tacks raw bytes onto the end of a file. It is how a test puts a
// torn tail AFTER some other damage, so the file no longer ends on a record
// boundary.
func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		t.Fatalf("OpenFile(%s) to append: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatalf("appending %d bytes to %s: %v", len(b), path, err)
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
	}
	// NOTE ON WHAT IS *NOT* IN THIS TABLE. Damage that leaves the file its full
	// length -- a flipped payload bit in a complete final frame, or a length field
	// corrupted in a record whose bytes are all present -- is NOT a torn tail and
	// lives in the refusal table below. Both were once asserted here as
	// truncations, and both were wrong: an interrupted append leaves FEWER bytes
	// than the header declares, never exactly enough, so a full-length frame was
	// fully written and may have been fsynced and acknowledged. Discarding one
	// loses accepted history and rolls the index high-water mark backwards.

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
		vetoRecordsInTail = "a complete record is still sitting inside the region the cut would discard"
		vetoCompleteFrame = "the frame's declared extent ends exactly at EOF, so every byte it needs is present"
		vetoLengthOnly    = "the frame is complete and only its length field is corrupt"
	)

	// overshootLength corrupts the length field of the record at recs[i] to a
	// value that is still LEGAL (at most MaxPayloadSize) but runs past the end of
	// the file. It is the damage that defeats every shape-only test: the frame's
	// own header now lies about how long it is, and the error it produces --
	// "truncated payload", declared extent past EOF -- is byte-for-byte the error
	// a genuine torn tail produces.
	// bitFlipLength is the same damage in its most realistic form: ONE bit
	// flipped in a length field, the way a bad sector or a cosmic ray delivers it.
	// Setting bit 16 of a two-digit payload length yields ~65 KiB -- comfortably
	// legal, comfortably past the end of a small log.
	bitFlipLength := func(i int) func(*testing.T, string, []Record, int64) {
		return func(t *testing.T, path string, recs []Record, cleanEnd int64) {
			t.Helper()
			flipped := uint32(len(recs[i].Payload)) ^ 0x00010000
			if flipped > MaxPayloadSize {
				t.Fatalf("the flipped length %d is over MaxPayloadSize; this case must reach the tail inspection", flipped)
			}
			if int64(flipped) <= cleanEnd-recs[i].Offset-FrameHeaderSize {
				t.Fatalf("the flipped length %d does not overshoot the end of the file; this case would not look like a tail", flipped)
			}
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], flipped)
			patch(t, path, recs[i].Offset, b[:])
		}
	}

	overshootLength := func(i int) func(*testing.T, string, []Record, int64) {
		return func(t *testing.T, path string, recs []Record, cleanEnd int64) {
			t.Helper()
			overshoot := cleanEnd - recs[i].Offset - FrameHeaderSize + 64
			if overshoot > MaxPayloadSize {
				t.Fatalf("the overshoot length %d is over MaxPayloadSize, so this case would be caught by the extent bound instead",
					overshoot)
			}
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(overshoot))
			patch(t, path, recs[i].Offset, b[:])
		}
	}

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
			// A COMPLETE final frame whose checksum fails. The file is not short at
			// all: the frame's declared extent lands exactly on the end of the file,
			// so every byte the record needs is present and the append that wrote it
			// ran to completion. A crash mid-append cannot produce this -- it leaves
			// fewer bytes, not wrong ones -- so this is media rot in a record that
			// may have been fsynced and ACKNOWLEDGED. Truncating it would lose
			// accepted history and roll the high-water mark back, which is precisely
			// what TailRepair.NextIndex's invariant-1 argument promises never
			// happens. Refusing to start is the price of keeping that promise true.
			name:  "complete final frame with a flipped payload bit",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				flipByte(t, path, recs[len(recs)-1].Offset+FrameHeaderSize+1)
			},
			wantVeto: vetoCompleteFrame,
			wantMsg:  "checksum mismatch",
			wantNote: "a full-length final frame was fully written, so it may have been acknowledged",
		},
		{
			// The same principle reached through the LENGTH field: every byte of the
			// last record is present, and only its declared length is wrong. The
			// writer's own checksum proves it -- recomputed against the bytes
			// actually there, it matches -- so the record is complete and must not be
			// discarded. This case previously truncated.
			name:  "length field corrupted in a last record whose bytes are all present",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[len(recs)-1].Offset, []byte{0x00, 0x10, 0x00, 0x00}) // 1 MiB
			},
			wantVeto: vetoLengthOnly,
			wantMsg:  "only its length is corrupt",
			wantNote: "the checksum proves every byte of the record is on disk, so it is not a torn write",
		},
		{
			// THE THIRD FLAGSHIP CASE, and the one that defeats a shape-only
			// classifier. Record 4's length is corrupted to a legal value that
			// overshoots the end of the file, so the error it produces is
			// INDISTINGUISHABLE from a torn tail by shape alone -- FrameIntact
			// false, extent past EOF, extent well inside MaxPayloadSize. Records 5
			// and 6 (a prepare/commit pair -- acknowledged, accepted history) are
			// sitting intact in the bytes the cut would take. This exact file was
			// demonstrated eating those two records before laterRecordInTail
			// existed.
			name:     "length field overshooting EOF with committed records behind it",
			build:    func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage:   overshootLength(3),
			wantVeto: vetoRecordsInTail,
			wantMsg:  "a complete record whose checksum verifies begins here",
			wantNote: "records 5 and 6 are intact, committed history sitting inside the region a cut would discard",
		},
		{
			// The same hole reached by a SINGLE FLIPPED BIT rather than a crafted
			// value, which is what makes it a durability bug and not just an attack:
			// no adversary is required, one bad bit in a length field is enough.
			// Independently reproduced by DUR-4's reviewer against the pre-fix code,
			// where it deleted two of three committed messages and then let Open and
			// Replay succeed with no error at all -- silent, permanent loss.
			name:     "one flipped bit in a middle length field",
			build:    func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage:   bitFlipLength(2),
			wantVeto: vetoRecordsInTail,
			wantMsg:  "a complete record whose checksum verifies begins here",
			wantNote: "one bad bit must not be able to delete every committed record after it",
		},
		{
			// The same lie told by the SECOND-TO-LAST record, so exactly one record
			// follows the damage. The anchor that finds it is "a following record
			// ends exactly at the end of the file", and with one follower that is
			// the tightest case there is.
			name:     "length field overshooting EOF with a single record behind it",
			build:    func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage:   overshootLength(4),
			wantVeto: vetoRecordsInTail,
			wantMsg:  "a complete record whose checksum verifies begins here",
			wantNote: "record 6 is intact and would be deleted by a cut at record 5",
		},
		{
			// THE EVASION THE FIRST FIX MISSED, found by DUR-4's security audit. A
			// mid-file length flip AND a torn tail: the corruption's region no longer
			// ends on a record boundary, so the original "a following record ends
			// exactly at EOF" anchor found nothing and the cut went ahead, deleting
			// eight committed records. The audit's point was the one that mattered --
			// a torn tail is not a rare independent second fault, it is the NORMAL
			// state of every file this code is called on, so any rule that assumes
			// the file ends cleanly is assuming away the whole problem. The index
			// window does not care where the file ends.
			name:  "length flip in a middle record with a torn tail after it",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				bitFlipLength(2)(t, path, recs, cleanEnd)
				appendBytes(t, path, []byte{0x7b}) // a partial frame's worth of junk
			},
			wantVeto: vetoRecordsInTail,
			wantMsg:  "a complete record whose checksum verifies begins here",
			wantNote: "records after the damage must be found even when the file does not end on a record boundary",
		},
		{
			// The same evasion reached by SHORTENING rather than extending: the file
			// ends one byte inside the final record.
			name:  "length flip in a middle record with the last record cut short",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				bitFlipLength(2)(t, path, recs, cleanEnd)
				truncate(t, path, cleanEnd-1)
			},
			wantVeto: vetoRecordsInTail,
			wantMsg:  "a complete record whose checksum verifies begins here",
			wantNote: "a record before the torn tail is still committed history and must not be cut away with it",
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
			case vetoCompleteFrame:
				// The frame declares an extent that lands EXACTLY on the end of the
				// file. Nothing follows it -- so a rule that only asked "does anything
				// follow the damage?" would truncate here -- and yet every byte the
				// record needs is present, which is what makes it not a torn write.
				if ce.FrameEnd != size {
					t.Errorf("CorruptError.FrameEnd = %d, want exactly the file size %d: this case is meant to be a "+
						"COMPLETE final frame", ce.FrameEnd, size)
				}
			case vetoLengthOnly:
				if !ce.FrameIntact {
					t.Errorf("CorruptError.FrameIntact = false, want true: the record's own checksum verified over the "+
						"bytes present, so a partial write cannot explain it: %s", tc.wantVeto)
				}
				if ce.FrameEnd <= size {
					t.Errorf("CorruptError.FrameEnd = %d in a %d-byte file: this case is meant to declare an extent "+
						"past the end of the file, i.e. to LOOK like a torn tail", ce.FrameEnd, size)
				}
			case vetoRecordsInTail:
				// This is the case shape-only classification CANNOT tell from a torn
				// tail, so the assertions here are the mirror image of the positive
				// table: the damage looks exactly like a tail, and is refused anyway
				// because a record was found in the region behind it.
				if ce.FrameEnd < size {
					t.Errorf("CorruptError.FrameEnd = %d in a %d-byte file: this case is meant to look like a torn tail",
						ce.FrameEnd, size)
				}
				if max := ce.Offset + FrameHeaderSize + MaxPayloadSize; ce.FrameEnd > max {
					t.Errorf("CorruptError.FrameEnd = %d, want it within the largest legal frame end %d: "+
						"this case must be refused by the tail INSPECTION, not by the extent bound", ce.FrameEnd, max)
				}
				if !ce.FrameIntact {
					t.Errorf("CorruptError.FrameIntact = false, want true: a partial write cannot leave a complete "+
						"record behind the damage, so the flag that vetoes truncation must be set: %s", tc.wantVeto)
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

// TestWALRepairTailRefusesDamageToACompleteFinalRecord is the matrix that keeps
// the LAST acknowledged record alive, and it is the one that took three rounds of
// review to get right.
//
// The last record in a log is the most dangerous place for damage, because it is
// where a torn tail also lives -- so every rule that decides "this is a tail"
// gets its hardest test here. The two axes are:
//
//   - WHAT IS CORRUPT in a record whose bytes are all on disk: a payload byte,
//     a byte of the stored checksum, or the length field;
//   - WHAT FOLLOWS IT, because the ordinary state of a crashed log is a torn
//     frame on the end, and every "nothing follows, so it is a tail" shortcut
//     is wrong exactly when something does.
//
// Every cell must REFUSE and leave the file byte-for-byte intact. Each of these
// records was fully written, so its Append returned, so it was fsynced and may
// have been acknowledged to a client -- discarding one loses accepted history and
// rolls the index high-water mark backwards, which is the thing
// TailRepair.NextIndex's invariant-1 argument promises cannot happen.
//
// The cell {length field, torn next frame} is not hypothetical: it was found by
// review against an implementation that tested only the hypothesis "the record
// ends at the end of the file", and it silently deleted an acknowledged COMMIT.
func TestWALRepairTailRefusesDamageToACompleteFinalRecord(t *testing.T) {
	sixOps := []walOp{
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opPrepare("message", `{"n":2}`), // 3
		opCommit(3),                     // 4
		opPrepare("message", `{"n":3}`), // 5
		opCommit(5),                     // 6 -- complete, fsynced, ACKNOWLEDGED
	}

	corruptions := []struct {
		name   string
		damage func(t *testing.T, path string, last Record)
	}{
		{
			name: "a payload byte",
			damage: func(t *testing.T, path string, last Record) {
				flipByte(t, path, last.Offset+FrameHeaderSize+1)
			},
		},
		{
			name: "a byte of the stored checksum",
			damage: func(t *testing.T, path string, last Record) {
				flipByte(t, path, last.Offset+16)
			},
		},
		{
			// The length field is the dangerous one: it is the single field whose
			// corruption makes a COMPLETE record produce the error shape of a torn
			// one, because every judgement about "where does this frame end" reads it.
			name: "the length field",
			damage: func(t *testing.T, path string, last Record) {
				var b [4]byte
				binary.BigEndian.PutUint32(b[:], uint32(len(last.Payload))^0x00000400)
				patch(t, path, last.Offset, b[:])
			},
		},
	}

	trailers := []struct {
		name    string
		append  func(t *testing.T, path string)
		wantEnd bool // the damaged frame is the end of the file
	}{
		{name: "nothing after it", append: func(*testing.T, string) {}, wantEnd: true},
		{
			name:   "one junk byte",
			append: func(t *testing.T, path string) { appendBytes(t, path, []byte{0x5a}) },
		},
		{
			name: "a scrap too short to be a frame header",
			append: func(t *testing.T, path string) {
				appendBytes(t, path, bytes.Repeat([]byte{0x5a}, FrameHeaderSize-1))
			},
		},
		{
			name: "seven bytes of the next frame header",
			append: func(t *testing.T, path string) {
				payload, err := encodePrepare("message", jsonBody(`{"n":9}`), time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatalf("encodePrepare: %v", err)
				}
				appendBytes(t, path, encodeFrame(7, TypePrepare, payload)[:7])
			},
		},
		{
			// The realistic one: a genuine torn NEXT frame, header complete and
			// payload cut off. This is what a crash actually leaves.
			name: "a genuinely torn next frame",
			append: func(t *testing.T, path string) {
				payload, err := encodePrepare("message", jsonBody(`{"n":9}`), time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatalf("encodePrepare: %v", err)
				}
				appendBytes(t, path, encodeFrame(7, TypePrepare, payload)[:FrameHeaderSize+5])
			},
		},
	}

	for _, c := range corruptions {
		for _, tr := range trailers {
			c, tr := c, tr
			t.Run(c.name+", "+tr.name, func(t *testing.T) {
				dir, path, _, _ := buildWAL(t, sixOps...)
				recs, _ := repairScanClean(t, path, KindWAL, 6)
				last := recs[len(recs)-1]
				if last.Type != TypeCommit {
					t.Fatalf("the last record is a %s, want a commit: this test is about losing an acknowledged write", last.Type)
				}

				c.damage(t, path, last)
				tr.append(t, path)
				before := readFile(t, path)

				res, err := RepairTail(path, KindWAL, nil)
				if err == nil {
					t.Fatalf("RepairTail returned no error (%+v): record 6 is COMPLETE on disk -- every byte it declares is "+
						"present -- so its Append returned, it was fsynced, and it may have been acknowledged", res)
				}
				if !errors.Is(err, ErrCorrupt) {
					t.Errorf("RepairTail err = %v, want errors.Is(err, ErrCorrupt)", err)
				}
				repairAssertUntouched(t, path, before, res)

				// And the refusal survives the trip through Open, which is where a
				// server would have served the truncated log.
				app := &testApplier{}
				if l, err := Open(LogOptions{Dir: dir, Applier: app}); err == nil {
					_ = l.Close()
					t.Fatalf("Open SUCCEEDED on a damaged but complete final record: it would have served %d of 3 "+
						"acknowledged entries and rolled the high-water mark back", app.count())
				}
				if after := readFile(t, path); !bytes.Equal(before, after) {
					t.Fatalf("a failed Open changed the WAL: %d bytes before, %d after", len(before), len(after))
				}
			})
		}
	}
}

// TestWALRepairTailRefusesAZeroPayloadFinalRecord is the boundary of the
// completeness proof: a record with an EMPTY payload is exactly FrameHeaderSize
// bytes, so the region a cut would discard is exactly one frame header. An
// inspection that treats "only a header's worth of bytes" as too small to bother
// with would skip the proof and truncate a complete record.
//
// The WAL write path emits no empty payloads today -- every prepare, commit and
// abort payload is JSON -- but Writer.Append has an upper bound on payload size
// and no lower one, and DUR-5's audit records go through the same writer. The
// proof must not rest on a property of today's callers.
func TestWALRepairTailRefusesAZeroPayloadFinalRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	w, err := OpenWriter(path, KindAudit)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, err := w.Append(TypeAuditMessage, []byte(`{"m":1}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	last, err := w.Append(TypeAuditMessage, nil) // the zero-payload record
	if err != nil {
		t.Fatalf("Append an empty payload: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := fileSize(t, path) - last.Offset; got != FrameHeaderSize {
		t.Fatalf("the final record occupies %d bytes, want exactly the %d-byte header", got, FrameHeaderSize)
	}

	// Corrupt only its length, so it declares an extent past the end of the file
	// and looks exactly like a torn tail.
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], 0x00000400)
	patch(t, path, last.Offset, b[:])
	before := readFile(t, path)

	res, err := RepairTail(path, KindAudit, nil)
	if err == nil {
		t.Fatalf("RepairTail returned no error (%+v): the record is complete, its payload is simply empty", res)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("RepairTail err = %v, want errors.Is(err, ErrCorrupt)", err)
	}
	if !strings.Contains(err.Error(), "only its length is corrupt") {
		t.Errorf("RepairTail err = %q, want the completeness proof to be what refused it", err)
	}
	repairAssertUntouched(t, path, before, res)
}

// TestWALRepairTailRefusesARegionDenseWithFrameHeaders defends the checksum
// budget. The bytes a cut would discard are attacker-influenced -- they are the
// partly-written tail of a record whose payload carries a client-supplied message
// body -- so the inspection cannot be allowed unbounded verification work. When
// the budget runs out the answer is a REFUSAL, not a cut: a region dense with
// things that look like records is not what a torn tail looks like, and recovery
// must not discard bytes it did not finish checking.
//
// Without this test the budget is invisible: it can be deleted and every other
// test still passes.
func TestWALRepairTailRefusesARegionDenseWithFrameHeaders(t *testing.T) {
	_, path, _, _ := buildWAL(t, repairTornOps()...)
	recs, cleanEnd := repairScanClean(t, path, KindWAL, 4)
	wantIndex := recs[len(recs)-1].Index + 1

	// A frame header declaring far more than will follow: the shape of a torn
	// tail, so the first gate lets it through to the inspection.
	var hdr [FrameHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], 900000)
	binary.BigEndian.PutUint64(hdr[4:12], wantIndex)
	binary.BigEndian.PutUint16(hdr[12:14], uint16(TypePrepare))
	appendBytes(t, path, hdr[:])

	// ... followed by a region packed with plausible-looking headers: reserved
	// clear, an in-window index, and a length that fits. Each one costs a
	// checksum.
	var plant [FrameHeaderSize]byte
	binary.BigEndian.PutUint32(plant[0:4], 0)
	binary.BigEndian.PutUint64(plant[4:12], wantIndex+1)
	binary.BigEndian.PutUint16(plant[12:14], uint16(TypePrepare))
	appendBytes(t, path, bytes.Repeat(plant[:], maxTailCandidates+64))
	before := readFile(t, path)

	if got := int64(len(before)) - cleanEnd; got <= 0 {
		t.Fatalf("the planted region is %d bytes, want a positive size", got)
	}
	res, err := RepairTail(path, KindWAL, nil)
	if err == nil {
		t.Fatalf("RepairTail returned no error (%+v): it must not cut a region it could not finish checking", res)
	}
	if !strings.Contains(err.Error(), "checksum budget") {
		t.Errorf("RepairTail err = %q, want the exhausted-budget refusal", err)
	}
	repairAssertUntouched(t, path, before, res)
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

// TestWALRepairTailToHeaderOnly is the degenerate end of the range: the FIRST
// record a brand-new log ever wrote was the one that was torn, so the cut lands
// on the file header itself and the repaired log holds no records at all.
//
// It is worth its own test because it is the one repair whose result is a file
// with nothing in it but a header, and three separate pieces of code have to
// agree that this is a valid log rather than a broken one: the scan (which must
// hit EOF exactly on the frame boundary), Replay (which must report an empty
// history with NextIndex 1, not an error), and Open's cross-check that the
// replay's EndOffset and the writer's size still match. A cut to
// FileHeaderSize is also the closest this package ever legitimately comes to
// cutting at offset 0, which truncatableTail refuses outright.
func TestWALRepairTailToHeaderOnly(t *testing.T) {
	dir, path, _, _ := buildWAL(t, opPrepare("message", `{"n":1}`))
	recs, _ := repairScanClean(t, path, KindWAL, 1)
	if recs[0].Offset != FileHeaderSize {
		t.Fatalf("the only record starts at offset %d, want %d", recs[0].Offset, FileHeaderSize)
	}
	truncate(t, path, FileHeaderSize+FrameHeaderSize+1) // header landed, payload did not

	res, err := RepairTail(path, KindWAL, nil)
	if err != nil {
		t.Fatalf("RepairTail: %v", err)
	}
	if !res.Truncated || res.At != FileHeaderSize || res.NextIndex != 1 {
		t.Fatalf("TailRepair = %+v, want Truncated true, At %d, NextIndex 1", res, FileHeaderSize)
	}
	if got := fileSize(t, path); got != FileHeaderSize {
		t.Fatalf("the repaired file is %d bytes, want exactly the %d-byte header", got, FileHeaderSize)
	}
	repairAssertPrefix(t, path, KindWAL, nil)

	// The header-only file is a valid EMPTY log, not a corrupt one.
	var c collector
	r, err := Replay(path, c.fn)
	if err != nil {
		t.Fatalf("Replay of the header-only file: %v, want an empty log", err)
	}
	if r.Records != 0 || r.Applied != 0 || len(r.Dangling) != 0 || r.NextIndex != 1 {
		t.Errorf("Recovered = %+v, want an empty log with NextIndex 1", r)
	}
	if len(c.got) != 0 {
		t.Errorf("the header-only file delivered %s, want nothing", showCommitted(c.got))
	}

	// And a server starts on it and writes record 1 -- the index the torn frame
	// carried, reissued because that frame never completed an fsync.
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open on the repaired header-only log: %v", err)
	}
	defer l.Close()
	if rep := l.Recovered().Repaired; rep.Truncated {
		t.Errorf("Recovered().Repaired = %+v, want no repair: RepairTail already cut this file", rep)
	}
	c1, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":2}`)})
	if err != nil {
		t.Fatalf("Write after the repair: %v", err)
	}
	if c1.PrepareIndex != 1 || c1.CommitIndex != 2 {
		t.Fatalf("the first write got {prepare:%d commit:%d}, want {1 2}", c1.PrepareIndex, c1.CommitIndex)
	}
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
