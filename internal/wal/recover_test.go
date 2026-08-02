package wal

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// RepairLog (still reachable as RepairTail) is the ONLY code in this package
// that ever removes bytes from a log.
//
// WHAT THESE TESTS ASSERTED UNTIL 2026-08-02, AND WHY THEY NO LONGER DO. The
// property used to be "RepairTail must never shorten a file except for a torn
// tail", and every negative case asserted a REFUSAL TO START plus a
// byte-for-byte unchanged file. The user reversed that policy (DECISIONS.md,
// "Availability over retention"): "always be able to restart, prefer to discard
// messages and/or corruption, with logging". The rule is now
//
//	DAMAGE IS NEVER FATAL. NOT BEING ABLE TO READ THE FILE STILL IS.
//
// so the cases that used to prove "this damage is refused" now have to prove
// something strictly harder, because "it started" on its own is worth nothing:
//
//  1. RECOVERY RUNS -- RepairLog returns no error and the log is usable after.
//  2. DAMAGE DOES NOT CASCADE -- exactly the damaged record goes, and every
//     intact record BEHIND it is still there, with its ORIGINAL index. The
//     mid-file cases below all carry committed records after the damage
//     precisely so that a cascade shows up as a failure.
//  3. THE DISCARD WAS LOGGED. That is what replaced "we never discard": the
//     honest promise is now "we never discard SILENTLY". Tests here assert the
//     specific log line, so deleting the logging fails them.
//
// A record whose LENGTH FIELD alone is damaged is RECOVERED rather than
// discarded (rebuildFrame): its own checksum, recomputed over the bytes actually
// present, proves the payload is all there. Several cases below used to be
// refusals for exactly that reason and are now full recoveries -- which is
// strictly better, since nothing at all is lost.
//
// What is STILL fatal, and still asserted as a refusal with the file untouched:
// an audit file where a WAL was expected, a format version this binary does not
// implement, and an unknown Kind. None of those are damage, and "repairing" them
// would destroy a file that is probably intact.
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

// ---------------------------------------------------------------------------
// Helpers for the post-2026-08-02 policy: recovery discards damage, so the
// assertions are about WHAT SURVIVED and WHETHER THE LOSS WAS LOGGED, not about
// whether the start was refused.
// ---------------------------------------------------------------------------

// pristineByIndex maps a fixture's records by index, so a survivor can be
// compared against exactly the record it used to be even after a repair has
// moved every offset behind it.
func pristineByIndex(recs []Record) map[uint64]Record {
	m := make(map[uint64]Record, len(recs))
	for _, r := range recs {
		m[r.Index] = r
	}
	return m
}

// assertSurvivors is the anti-cascade assertion, and it is the one that matters
// most in this file: it re-scans the repaired file and demands that EXACTLY the
// records named by want are in it, each still carrying its ORIGINAL index, type
// and payload BYTES.
//
// Comparing payload bytes rather than counting records is deliberate. Discarding
// the damaged record is sanctioned; quietly rewriting a record that was fine, or
// renumbering a survivor so a hole disappears, is not -- and both would keep the
// count right.
func assertSurvivors(t *testing.T, path string, kind Kind, pristine []Record, want []uint64) {
	t.Helper()
	got, end, err := ScanAll(path, kind)
	if err != nil {
		t.Fatalf("ScanAll after the repair: %v: a repaired file must scan clean end to end", err)
	}
	byIndex := pristineByIndex(pristine)

	var gotIdx []uint64
	for _, r := range got {
		gotIdx = append(gotIdx, r.Index)
	}
	if len(got) != len(want) {
		t.Fatalf("the repaired file holds records %v, want exactly %v: recovery may discard the DAMAGED record and nothing else",
			gotIdx, want)
	}
	for i, r := range got {
		if r.Index != want[i] {
			t.Fatalf("the repaired file holds records %v, want exactly %v (survivors keep their original indices, so a repaired log has HOLES)",
				gotIdx, want)
		}
		p, ok := byIndex[r.Index]
		if !ok {
			continue // a fixture that was never scanned clean; the index check above is the assertion
		}
		if r.Type != p.Type {
			t.Errorf("surviving record %d is a %s, want a %s: a survivor must come back byte-identical", r.Index, r.Type, p.Type)
		}
		if !bytes.Equal(r.Payload, p.Payload) {
			t.Errorf("surviving record %d has payload %q, want %q: a repair must never rewrite a record it kept",
				r.Index, r.Payload, p.Payload)
		}
	}
	if size := fileSize(t, path); end != size {
		t.Errorf("the repaired file scans to %d but is %d bytes: nothing may be left past the last record", end, size)
	}
}

// logLinesWith returns every captured log line whose message is msg. One
// recovery can emit several -- a file with two discards logs two "wal discarded
// a damaged record" lines -- so the fields, not the message, are what pick one
// out.
func logLinesWith(out, msg string) []string {
	var found []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "msg="+strconv.Quote(msg)) || strings.Contains(line, "msg="+msg) {
			found = append(found, line)
		}
	}
	return found
}

// findLogLine reports whether any line carries msg.
func findLogLine(out, msg string) (string, bool) {
	lines := logLinesWith(out, msg)
	if len(lines) == 0 {
		return "", false
	}
	return lines[0], true
}

// assertLogged demands a log line with the given message, carrying every one of
// fields, at the given level ("" to accept any). The FIELDS select the line, so
// a recovery that logged several discards can be checked one at a time.
//
// THIS IS THE ASSERTION THAT REPLACED "WE NEVER DISCARD". Discarding damaged
// records is sanctioned policy now; discarding one without telling an operator
// is the bug. Every test that provokes a discard calls this, so removing the
// logging in RepairLog or Open fails them.
func assertLogged(t *testing.T, out, level, msg string, fields ...string) string {
	t.Helper()
	lines := logLinesWith(out, msg)
	if len(lines) == 0 {
		t.Fatalf("nothing in the operator log says %q -- A DISCARD THAT IS NOT LOGGED IS THE BUG, since "+
			"recovery is allowed to throw damage away only because it says so out loud:\n%s", msg, out)
	}
	for _, line := range lines {
		matched := true
		for _, f := range fields {
			if !strings.Contains(line, f) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		if level != "" && !strings.Contains(line, "level="+strings.ToLower(level)) {
			t.Errorf("the %q line carrying %v is not at level=%s -- the LEVEL is the difference between "+
				"\"an uncommitted prepare went\" and \"an acknowledged write went\", so it is part of the contract:\n%s",
				msg, fields, strings.ToLower(level), line)
		}
		return line
	}
	t.Fatalf("no %q line carries all of %v, so an operator cannot tell what was lost. Lines found:\n%s",
		msg, fields, strings.Join(lines, "\n"))
	return ""
}

// assertNotLogged is the negative half: a message that must NOT appear.
func assertNotLogged(t *testing.T, out, msg string) {
	t.Helper()
	if line, ok := findLogLine(out, msg); ok {
		t.Errorf("the operator log says %q, and it must not here:\n%s", msg, line)
	}
}

// captureRepair runs RepairLog with a capturing logger at the most verbose
// level and returns the repair, the log text and the error.
func captureRepair(t *testing.T, path string, kind Kind) (Repair, string, error) {
	t.Helper()
	var buf bytes.Buffer
	res, err := RepairLog(path, kind, logging.New(&buf, logging.LevelDebug))
	return res, buf.String(), err
}

// openCapturing starts a Log the way a server does, with a capturing logger, and
// returns what the Applier saw, the recovery record and the log text. It is how
// the replay-stage discard tests get at both halves of the contract at once:
// the server STARTED, and the loss reached the operator log.
func openCapturing(t *testing.T, dir string) ([]Committed, Recovered, string, error) {
	t.Helper()
	var buf bytes.Buffer
	app := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: app, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		return nil, Recovered{}, buf.String(), err
	}
	rec := l.Recovered()
	got := make([]Committed, app.count())
	for i := range got {
		got[i] = app.at(i)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("openCapturing: Close: %v", err)
	}
	return got, rec, buf.String(), nil
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
		// wantRecordType is what the discard's log line must say was lost.
		// "unreadable" is the honest answer when fewer than a frame header's
		// worth of bytes survived -- there is then nothing in the file that says
		// what the record was, and pretending otherwise would be a lie in an
		// audit trail.
		wantRecordType  string
		wantRecordIndex string
	}{
		{
			// (a) The classic: the frame header landed, the payload did not. The
			// header survived, so the log can name the record that went.
			name: "torn payload with an intact frame header",
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, recs[len(recs)-1].Offset+FrameHeaderSize+2)
			},
			wantRecordType:  "commit",
			wantRecordIndex: "4",
		},
		{
			// (b) Fewer than 20 bytes of the final frame reached the disk. That
			// can only happen at end of file, so nothing can follow it -- and
			// nothing survives to say what was in it either.
			name: "torn frame header, two bytes in",
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, recs[len(recs)-1].Offset+2)
			},
			wantRecordType:  "unreadable",
			wantRecordIndex: "unknown",
		},
		{
			// (c) The three boundaries around the frame header: one byte in, one
			// byte short of a whole header, and exactly a whole header. The first
			// two leave too little to read a header at all; the third leaves a
			// header that parsed and declared an extent running past the end of
			// the file. Same verdict either way: a torn tail, discarded, logged.
			name: "exactly one byte of the last frame",
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, recs[len(recs)-1].Offset+1)
			},
			wantRecordType:  "unreadable",
			wantRecordIndex: "unknown",
		},
		{
			name: "one byte short of a whole frame header",
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, recs[len(recs)-1].Offset+FrameHeaderSize-1)
			},
			wantRecordType:  "unreadable",
			wantRecordIndex: "unknown",
		},
		{
			name: "exactly a frame header and none of the payload",
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, recs[len(recs)-1].Offset+FrameHeaderSize)
			},
			wantRecordType:  "commit",
			wantRecordIndex: "4",
		},
	}
	// NOTE ON WHAT IS *NOT* IN THIS TABLE. Damage that leaves the file its full
	// length -- a flipped payload bit in a complete final frame, or a length field
	// corrupted in a record whose bytes are all present -- is not a torn write:
	// an interrupted append leaves FEWER bytes than the header declares, never
	// wrong ones. Those shapes live in TestWALRepairTailDiscardsDamageThatIsNotATornTail
	// below, where a corrupt length is RECOVERED (the record's own checksum proves
	// it complete) and a corrupt payload in the last record is discarded and
	// logged at ERROR as the acknowledged loss it may be.

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

			res, out, err := captureRepair(t, path, KindWAL)
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
			if !strings.Contains(res.Reason, "no intact record follows it anywhere in the file") {
				t.Errorf("TailRepair.Reason = %q, want it to say the search for a record behind the damage found nothing: "+
					"a cut is only a TAIL when there is provably nothing after it", res.Reason)
			}
			if res.DiscardCount != 1 {
				t.Errorf("TailRepair.DiscardCount = %d, want exactly 1: one torn frame was thrown away", res.DiscardCount)
			}
			if res.Rewritten || res.HeaderRepaired || res.Quarantined != "" || res.Exhausted {
				t.Errorf("TailRepair = %+v, want a plain truncation: a torn tail needs no rewrite, no header repair and no quarantine", res)
			}
			// THE DISCARD REACHED THE OPERATOR LOG. This is the whole of what
			// replaced "recovery never discards", so a test that only checked the
			// bytes would now be checking the easy half.
			assertLogged(t, out, "ERROR", "wal discarded a damaged record",
				"path="+path, "stage=framing",
				"record_type="+tc.wantRecordType, "record_index="+tc.wantRecordIndex)
			assertLogged(t, out, "WARN", "wal truncated damage at the end of the log",
				"path="+path, "at="+strconv.FormatInt(res.At, 10), "removed="+strconv.FormatInt(res.Removed, 10))
			assertNotLogged(t, out, "wal rewrote a damaged log, keeping every intact record")

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

// TestWALRepairTailDiscardsDamageThatIsNotATornTail is the half that keeps the
// data -- and after 2026-08-02 it keeps it by RECOVERING rather than by
// refusing.
//
// Every case here is damage that a partial write CANNOT have produced, or damage
// with records after it, or damage in the file header. Under the old policy each
// one was a REFUSAL TO START, on the argument that an operator can recover from a
// server that will not boot and cannot recover from one that quietly deleted
// records. The user reversed that (DECISIONS.md, "Availability over retention"),
// so the argument now has to be carried by the repair itself, and each case
// asserts all four halves of it:
//
//	(i)   RECOVERY RUNS -- no error, and Open starts on the result;
//	(ii)  EXACTLY the damaged record goes, named by index;
//	(iii) every intact record BEHIND the damage is still there, byte for byte,
//	      with its ORIGINAL index -- records 5 and 6 of the fixture are a
//	      prepare/commit pair (acknowledged, accepted history) sitting after every
//	      middle-of-file case precisely so that a cascade fails the test;
//	(iv)  THE DISCARD WAS LOGGED, with its offset, index and type.
//
// Several cases that used to be refusals now lose NOTHING AT ALL: when only a
// record's LENGTH FIELD is damaged, its own checksum recomputed over the bytes
// actually present proves the payload is complete, so the record is rebuilt. Those
// carry wantRebuilt and assert that all six records survive.
func TestWALRepairTailDiscardsDamageThatIsNotATornTail(t *testing.T) {
	// Six records: three complete transactions. Records 5 and 6 are a
	// prepare/commit pair -- ACCEPTED HISTORY, already acknowledged to a client
	// -- that sits after every middle-of-file case below, so any cascade here
	// would be real, permanent data loss.
	sixOps := []walOp{
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opPrepare("message", `{"n":2}`), // 3
		opCommit(3),                     // 4
		opPrepare("message", `{"n":3}`), // 5
		opCommit(5),                     // 6
	}
	allSix := []uint64{1, 2, 3, 4, 5, 6}

	// bitFlipLength is the damage that defeats every shape-only test in its most
	// realistic form: ONE bit flipped in a length field, the way a bad sector or a
	// cosmic ray delivers it. Setting bit 16 of a two-digit payload length yields
	// ~65 KiB -- comfortably legal, comfortably past the end of a small log -- so
	// the error it produces ("truncated payload", extent past EOF) is
	// byte-for-byte the error a genuine torn tail produces.
	bitFlipLength := func(i int) func(*testing.T, string, []Record, int64) {
		return func(t *testing.T, path string, recs []Record, cleanEnd int64) {
			t.Helper()
			flipped := uint32(len(recs[i].Payload)) ^ 0x00010000
			if flipped > MaxPayloadSize {
				t.Fatalf("the flipped length %d is over MaxPayloadSize; this case must reach the salvage walk", flipped)
			}
			if int64(flipped) <= cleanEnd-recs[i].Offset-FrameHeaderSize {
				t.Fatalf("the flipped length %d does not overshoot the end of the file; this case would not look like a tail", flipped)
			}
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], flipped)
			patch(t, path, recs[i].Offset, b[:])
		}
	}

	// overshootLength is the same lie told deliberately rather than by one bit.
	overshootLength := func(i int) func(*testing.T, string, []Record, int64) {
		return func(t *testing.T, path string, recs []Record, cleanEnd int64) {
			t.Helper()
			overshoot := cleanEnd - recs[i].Offset - FrameHeaderSize + 64
			if overshoot > MaxPayloadSize {
				t.Fatalf("the overshoot length %d is over MaxPayloadSize, so this case would be classified by the extent bound instead",
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

		// wantFatal marks the cases that are NOT damage but "I cannot read this
		// file at all". Those still refuse, and still leave the bytes untouched.
		wantFatal  bool
		wantErrMsg string

		// wantSurvivors is the record INDEX of everything that must still be in
		// the file afterwards -- the anti-cascade assertion. A gap in it is a
		// permanent hole, which is correct: survivors are never renumbered,
		// because renumbering would reuse an id (invariant 1).
		wantSurvivors []uint64
		// wantDiscards is the exact number of regions thrown away.
		wantDiscards int
		// wantRebuilt is how many records were RECOVERED because their checksum
		// proved that only their length field was damaged.
		wantRebuilt uint64

		// How the file was changed. Exactly one of these is expected per case
		// (or none, for a file that needed no repair at all).
		wantTruncated   bool
		wantRewritten   bool
		wantQuarantined bool
		wantNoRepair    bool
		wantHeaderFixed bool

		// The discard's log line: its level, and the record it names.
		wantLogLevel  string
		wantLogType   string
		wantLogIndex  string
		wantReasonHas string

		wantNote string // why this shape matters, printed on failure
	}{
		{
			// (e) THE FLAGSHIP DATA-LOSS CASE. One flipped bit in an early
			// record's PAYLOAD, with committed records after it. The payload is
			// what is wrong, so no checksum can rescue this record -- but the
			// declared length is still right, which means the next record starts
			// exactly where the header says and the damage costs ONE record.
			name:  "checksum mismatch in a middle record",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				flipByte(t, path, recs[1].Offset+FrameHeaderSize+1)
			},
			wantSurvivors: []uint64{1, 3, 4, 5, 6},
			wantDiscards:  1,
			wantRewritten: true,
			wantLogLevel:  "ERROR", // a COMMIT record: a client was told this was durable
			wantLogType:   "commit",
			wantLogIndex:  "2",
			wantReasonHas: "the next intact record was found at offset",
			wantNote:      "one bad bit in one payload must cost exactly one record, not the four committed records behind it",
		},
		{
			// (f) An absurd length field -- 4 GiB -- in a middle record. Under the
			// old policy this was refused. It is now RECOVERED: nothing but the
			// length is wrong, the true end of the frame is where the next
			// surviving record begins, and rewriting the length to that reproduces
			// the stored checksum. Only the true length can do that, so the match
			// is a proof and not a guess.
			name:  "absurd length field in a middle record",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[1].Offset, []byte{0xff, 0xff, 0xff, 0xff})
			},
			wantSurvivors: allSix,
			wantRebuilt:   1,
			wantRewritten: true,
			wantNote:      "a 4 GiB declared extent over a complete record loses nothing: the record's own checksum says so",
		},
		{
			// (g) The same corruption in the LAST record: the true end is then the
			// end of the file, and the proof works just as well.
			name:  "absurd length field in the last record",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[len(recs)-1].Offset, []byte{0xff, 0xff, 0xff, 0xff})
			},
			wantSurvivors: allSix,
			wantRebuilt:   1,
			wantRewritten: true,
			wantNote:      "the LAST record is where a torn tail lives, so a complete record there must still be recovered rather than cut",
		},
		{
			// The exact boundary of the legal-frame rule, one byte the wrong side
			// of it: a length no writer could have produced. Still only the length.
			name:  "length field one byte over the maximum in the last record",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[len(recs)-1].Offset, []byte{0x00, 0x10, 0x00, 0x01}) // 1 MiB + 1
			},
			wantSurvivors: allSix,
			wantRebuilt:   1,
			wantRewritten: true,
			wantNote:      "an illegal declared length is still only a length: the bytes of the record are all present",
		},
		{
			// A COMPLETE final frame whose PAYLOAD checksum fails. The file is not
			// short at all, so a crash mid-append cannot have produced this -- it
			// is media rot in a record that may have been fsynced and ACKNOWLEDGED.
			// Under the old policy that was a refusal; the availability decision
			// says discard it instead, and the honesty that replaces the old
			// promise is that it goes at ERROR, naming the commit that was lost.
			name:  "complete final frame with a flipped payload bit",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				flipByte(t, path, recs[len(recs)-1].Offset+FrameHeaderSize+1)
			},
			wantSurvivors: []uint64{1, 2, 3, 4, 5},
			wantDiscards:  1,
			wantTruncated: true,
			wantLogLevel:  "ERROR",
			wantLogType:   "commit",
			wantLogIndex:  "6",
			wantReasonHas: "no intact record follows it anywhere in the file",
			wantNote:      "an acknowledged COMMIT lost to bit rot must be discarded LOUDLY -- at ERROR, by index",
		},
		{
			// The same principle reached through the LENGTH field: every byte of
			// the last record is present and only its declared length is wrong.
			// The writer's own checksum, recomputed against the bytes actually
			// there, matches -- so the record is complete and is kept.
			name:  "length field corrupted in a last record whose bytes are all present",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[len(recs)-1].Offset, []byte{0x00, 0x10, 0x00, 0x00}) // 1 MiB
			},
			wantSurvivors: allSix,
			wantRebuilt:   1,
			wantRewritten: true,
			wantNote:      "the checksum proves every byte of the record is on disk, so nothing may be lost here",
		},
		{
			// THE CASE THAT DEFEATS A SHAPE-ONLY CLASSIFIER. Record 4's length is
			// corrupted to a legal value that overshoots the end of the file, so
			// the error is INDISTINGUISHABLE from a torn tail by shape alone.
			// Records 5 and 6 -- acknowledged, accepted history -- sit in the bytes
			// a naive cut would take. This exact file was demonstrated eating them.
			name:          "length field overshooting EOF with committed records behind it",
			build:         func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage:        overshootLength(3),
			wantSurvivors: allSix,
			wantRebuilt:   1,
			wantRewritten: true,
			wantNote:      "records 5 and 6 are intact committed history sitting inside the region a naive cut would discard",
		},
		{
			// The same hole reached by a SINGLE FLIPPED BIT rather than a crafted
			// value, which is what makes it a durability bug and not just an
			// attack: no adversary is required. Independently reproduced by DUR-4's
			// reviewer against the pre-fix code, where it deleted two of three
			// committed messages and then let Open and Replay succeed with no error
			// at all -- silent, permanent loss.
			name:          "one flipped bit in a middle length field",
			build:         func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage:        bitFlipLength(2),
			wantSurvivors: allSix,
			wantRebuilt:   1,
			wantRewritten: true,
			wantNote:      "one bad bit must not be able to delete every committed record after it",
		},
		{
			// The same lie told by the SECOND-TO-LAST record, so exactly one record
			// follows the damage: the tightest version of the forward search.
			name:          "length field overshooting EOF with a single record behind it",
			build:         func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage:        overshootLength(4),
			wantSurvivors: allSix,
			wantRebuilt:   1,
			wantRewritten: true,
			wantNote:      "record 6 is intact and would be deleted by a cut at record 5",
		},
		{
			// THE EVASION THE FIRST FIX MISSED, found by DUR-4's security audit. A
			// mid-file length flip AND a torn tail: the damaged region no longer
			// ends on a record boundary, so any rule anchored on "a following record
			// ends exactly at EOF" finds nothing and cuts, deleting eight committed
			// records in the reviewer's probe. The audit's point was the one that
			// mattered -- a torn tail is not a rare independent second fault, it is
			// the NORMAL state of every file this code is called on. The forward
			// search is anchored on the record INDEX and does not care where the
			// file ends, so record 3 is rebuilt and only the junk byte goes.
			name:  "length flip in a middle record with a torn tail after it",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				bitFlipLength(2)(t, path, recs, cleanEnd)
				appendBytes(t, path, []byte{0x7b}) // a partial frame's worth of junk
			},
			wantSurvivors: allSix,
			wantDiscards:  1, // the junk byte, and nothing else
			wantRebuilt:   1,
			wantRewritten: true,
			wantLogLevel:  "ERROR", // too few bytes to say what was lost
			wantLogType:   "unreadable",
			wantLogIndex:  "unknown",
			wantNote:      "records after the damage must be found even when the file does not end on a record boundary",
		},
		{
			// The same evasion reached by SHORTENING rather than extending: the
			// file ends one byte inside the final record. Record 3 is rebuilt;
			// record 6 is genuinely incomplete and is the only thing discarded.
			name:  "length flip in a middle record with the last record cut short",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				bitFlipLength(2)(t, path, recs, cleanEnd)
				truncate(t, path, cleanEnd-1)
			},
			wantSurvivors: []uint64{1, 2, 3, 4, 5},
			wantDiscards:  1,
			wantRebuilt:   1,
			wantRewritten: true,
			wantLogLevel:  "ERROR",
			wantLogType:   "commit",
			wantLogIndex:  "6",
			wantNote:      "a record BEFORE the torn tail is committed history and must not be cut away with it",
		},
		{
			// (i) A bad FILE MAGIC. The magic is not the other kind of log and not
			// a version this binary cannot read -- it is simply wrong, which is
			// what damage in a fixed 16-byte preamble looks like. There is nothing
			// in a file header to RECOVER, only 16 constant bytes to rewrite, and
			// refusing to start over a flipped bit there would throw away an
			// entirely readable log.
			name:  "bad file magic",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, 0, []byte("XGNTBUSW"))
			},
			wantSurvivors:   allSix,
			wantRewritten:   true,
			wantHeaderFixed: true,
			wantNote:        "every record behind a damaged header is intact, so the header is rebuilt and nothing is lost",
		},
		{
			// STILL FATAL. A version number under OUR magic is not damage: this
			// binary does not implement that layout, and guessing at it is how a
			// downgrade eats a log.
			// Version 3, not 2: 2 is what this binary writes and 1 is the
			// legacy layout it still READS so that an existing bus can be
			// upgraded, so neither of them is "unknown" any more. The property
			// under test is unchanged -- a version this code does not implement
			// is fatal.
			name:  "unknown format version",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, 8, []byte{0x00, 0x00, 0x00, 0x03})
			},
			wantFatal:  true,
			wantErrMsg: "format version 3, want 2",
			wantNote:   "a file whose layout this code does not know must never be edited by it",
		},
		{
			// A file with FEWER than 16 bytes: the header cannot be verified and
			// there is not one record anywhere behind it. That is not a log this
			// code can make anything of, so it is QUARANTINED -- renamed aside,
			// never deleted -- and startup continues with a fresh log.
			name:  "truncated file header",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				truncate(t, path, 9)
			},
			wantQuarantined: true,
			wantDiscards:    1,
			wantNote:        "an uninterpretable file is moved aside, not deleted, and not allowed to block the start for ever",
		},
		{
			// (j) A NUL tail longer than one frame is what some filesystems expose
			// for a write that never landed. It used to be the DELIBERATE
			// CONSERVATIVE GAP -- unverifiable, so refused. It is now discarded as
			// what it is: unreadable bytes at the end of the file with no record
			// anywhere in them.
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
			wantSurvivors: allSix,
			wantDiscards:  1,
			wantTruncated: true,
			wantReasonHas: "no intact record follows it anywhere in the file",
			wantNote:      "200 NUL bytes hold no record, so they are a tail; the six records in front of them are untouched",
		},
		{
			// (k) reserved != 0 in a middle frame: structurally impossible for this
			// writer, so not even the frame header can be trusted to say what the
			// record was. It is discarded at ERROR with the type "unreadable",
			// because "I do not know what I just deleted" is worse news than "I
			// deleted a prepare", and the log must say which of the two it is.
			name:  "non-zero reserved field in a middle frame",
			build: func(t *testing.T) string { _, p, _, _ := buildWAL(t, sixOps...); return p },
			damage: func(t *testing.T, path string, recs []Record, cleanEnd int64) {
				patch(t, path, recs[1].Offset+14, []byte{0x00, 0x01})
			},
			wantSurvivors: []uint64{1, 3, 4, 5, 6},
			wantDiscards:  1,
			wantRewritten: true,
			wantLogLevel:  "ERROR",
			wantLogType:   "unreadable",
			wantLogIndex:  "unknown",
			wantReasonHas: "the next intact record was found at offset",
			wantNote:      "the four committed records behind an unreadable frame header must survive it",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			path := tc.build(t)
			recs, cleanEnd, err := ScanAll(path, KindWAL)
			if err != nil {
				t.Fatalf("the fixture is not well framed before it is damaged: %v", err)
			}
			tc.damage(t, path, recs, cleanEnd)
			before := readFile(t, path)

			res, out, err := captureRepair(t, path, KindWAL)

			if tc.wantFatal {
				// The "I cannot read this file at all" class. Not damage, so the
				// bytes are left exactly as they are for an operator to inspect.
				if err == nil {
					t.Fatalf("RepairLog returned no error (%+v): %s", res, tc.wantNote)
				}
				if !errors.Is(err, ErrCorrupt) {
					t.Errorf("RepairLog err = %v, want errors.Is(err, ErrCorrupt)", err)
				}
				if !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Errorf("RepairLog err = %q, want it to contain %q", err, tc.wantErrMsg)
				}
				if !strings.Contains(err.Error(), path) {
					t.Errorf("RepairLog err = %q, does not name the file", err)
				}
				repairAssertUntouched(t, path, before, res)

				dir := filepath.Dir(path)
				if l, err := Open(LogOptions{Dir: dir, Applier: &testApplier{}}); err == nil {
					_ = l.Close()
					t.Fatalf("Open succeeded on a file it cannot read: %s", tc.wantNote)
				}
				if after := readFile(t, path); !bytes.Equal(before, after) {
					t.Fatalf("a failed Open changed the WAL: %d bytes before, %d after", len(before), len(after))
				}
				return
			}

			// (i) RECOVERY RUNS. Damage is never fatal.
			if err != nil {
				t.Fatalf("RepairLog refused to repair damage: %v\nDamage is never fatal now (DECISIONS.md 2026-08-02): %s", err, tc.wantNote)
			}

			if tc.wantQuarantined {
				if res.Quarantined == "" {
					t.Fatalf("Repair = %+v, want the unreadable file moved aside: %s", res, tc.wantNote)
				}
				// RENAMED, NEVER DELETED: the original bytes must still exist under
				// the new name, because a file this code cannot read is not
				// necessarily a file NOBODY can read.
				kept, rerr := os.ReadFile(res.Quarantined)
				if rerr != nil {
					t.Fatalf("the quarantined file %s cannot be read back: %v: quarantine RENAMES, it never deletes", res.Quarantined, rerr)
				}
				if !bytes.Equal(kept, before) {
					t.Fatalf("the quarantined file holds %d bytes, want the original %d: an operator is owed the bytes verbatim",
						len(kept), len(before))
				}
				if _, serr := os.Stat(path); !os.IsNotExist(serr) {
					t.Errorf("%s still exists after a quarantine (stat err = %v); startup must continue with a FRESH log", path, serr)
				}
				assertLogged(t, out, "ERROR", "wal quarantined an unreadable log and started a fresh one",
					"path="+path, "moved_to="+res.Quarantined)
			} else {
				// (ii) and (iii): exactly the damaged record went, everything
				// behind it survived byte for byte with its original index.
				assertSurvivors(t, path, KindWAL, recs, tc.wantSurvivors)
			}

			if res.DiscardCount != tc.wantDiscards {
				t.Errorf("Repair.DiscardCount = %d, want %d (discards: %+v): %s", res.DiscardCount, tc.wantDiscards, res.Discards, tc.wantNote)
			}
			if res.Rebuilt != tc.wantRebuilt {
				t.Errorf("Repair.Rebuilt = %d, want %d: a record whose LENGTH FIELD alone is damaged is recovered, not discarded",
					res.Rebuilt, tc.wantRebuilt)
			}
			if res.Truncated != tc.wantTruncated || res.Rewritten != tc.wantRewritten || res.HeaderRepaired != tc.wantHeaderFixed {
				t.Errorf("Repair = {Truncated:%v Rewritten:%v HeaderRepaired:%v}, want {%v %v %v}",
					res.Truncated, res.Rewritten, res.HeaderRepaired, tc.wantTruncated, tc.wantRewritten, tc.wantHeaderFixed)
			}
			if res.Exhausted {
				t.Errorf("Repair.Exhausted = true: none of these files should exhaust the forward search's work budget")
			}
			if tc.wantReasonHas != "" && !strings.Contains(res.Reason, tc.wantReasonHas) {
				t.Errorf("Repair.Reason = %q, want it to contain %q", res.Reason, tc.wantReasonHas)
			}

			// (iv) THE DISCARD WAS LOGGED. Delete the logging in RepairLog and this
			// fails: that is the point of asserting the level and the fields rather
			// than merely that something was written.
			if tc.wantLogLevel != "" {
				assertLogged(t, out, tc.wantLogLevel, "wal discarded a damaged record",
					"path="+path, "stage=framing",
					"record_type="+tc.wantLogType, "record_index="+tc.wantLogIndex)
			}
			if tc.wantDiscards > 0 && !tc.wantQuarantined {
				assertLogged(t, out, "WARN", "wal recovery discarded damaged regions",
					"discards="+strconv.Itoa(tc.wantDiscards))
			}
			if tc.wantDiscards == 0 && !tc.wantQuarantined {
				assertNotLogged(t, out, "wal discarded a damaged record")
			}
			if tc.wantRebuilt > 0 {
				assertLogged(t, out, "WARN", "wal restored records whose length field was corrupt but whose checksum proved them complete",
					"path="+path, "records="+strconv.FormatUint(tc.wantRebuilt, 10))
			}
			if tc.wantHeaderFixed {
				assertLogged(t, out, "ERROR", "wal rebuilding a damaged file header", "path="+path)
			}

			// (i) again, at the level a server actually sees it: Open starts, and
			// the log it starts on is still appendable.
			dir := filepath.Dir(path)
			l, err := Open(LogOptions{Dir: dir, Applier: &testApplier{}})
			if err != nil {
				t.Fatalf("Open after the repair: %v: recovery must always reach a running server", err)
			}
			if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"repair"}`)}); err != nil {
				_ = l.Close()
				t.Fatalf("Write after the repair: %v: a repaired log must be one a server can go on writing to", err)
			}
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			assertIndicesUnique(t, path)
		})
	}
}

// TestWALRepairTailDiscardsDamageToACompleteFinalRecord is the matrix over the
// LAST record of a log, and it is the one that took three rounds of review to get
// right.
//
// The last record is the most dangerous place for damage, because it is where a
// torn tail also lives. The two axes are:
//
//   - WHAT IS CORRUPT in a record whose bytes are all on disk: a payload byte,
//     a byte of the stored checksum, or the length field;
//   - WHAT FOLLOWS IT, because the ordinary state of a crashed log is a torn
//     frame on the end.
//
// Until 2026-08-02 every cell REFUSED TO START, on the argument that each of
// these records was fully written, so its Append returned, so it was fsynced and
// may have been acknowledged. The availability decision reversed that: the record
// is discarded so the bus restarts, and the argument is carried instead by (a)
// discarding EXACTLY that record, never the five acknowledged entries in front of
// it, and (b) logging the loss at ERROR with the record's index and type.
//
// The LENGTH-FIELD row is the interesting one, and it splits:
//
//   - with NOTHING after the record, the true end of the frame is the end of the
//     file, so the checksum recomputed over the bytes present proves the record
//     complete and it is RECOVERED. Nothing is lost at all.
//   - with anything after it, the true end is unknown -- the trailing bytes are
//     indistinguishable from payload -- so the proof cannot be run and the record
//     is discarded with them.
//
// That second half is a real, accepted loss and it is asserted deliberately
// rather than glossed: it is the price of the availability decision, and it is
// bounded to the tail.
func TestWALRepairTailDiscardsDamageToACompleteFinalRecord(t *testing.T) {
	sixOps := []walOp{
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opPrepare("message", `{"n":2}`), // 3
		opCommit(3),                     // 4
		opPrepare("message", `{"n":3}`), // 5
		opCommit(5),                     // 6 -- complete, fsynced, ACKNOWLEDGED
	}

	corruptions := []struct {
		name string
		// lengthOnly marks the row where the record's own bytes are all intact
		// and only its declared length is wrong.
		lengthOnly bool
		damage     func(t *testing.T, path string, last Record)
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
			// The length field is the one whose corruption makes a COMPLETE record
			// produce the error shape of a torn one, because every judgement about
			// "where does this frame end" reads it.
			name:       "the length field",
			lengthOnly: true,
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
		wantEnd bool // nothing follows the damaged frame
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
				appendBytes(t, path, testCodec(t, path).encodeFrame(7, TypePrepare, payload)[:7])
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
				appendBytes(t, path, testCodec(t, path).encodeFrame(7, TypePrepare, payload)[:FrameHeaderSize+5])
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

				// A length-only corruption with NOTHING after it is provably
				// complete and must be recovered in full; every other cell loses
				// exactly record 6 and nothing else.
				recovered := c.lengthOnly && tr.wantEnd

				res, out, err := captureRepair(t, path, KindWAL)
				if err != nil {
					t.Fatalf("RepairLog on damage in the final record: %v: damage is never fatal", err)
				}

				if recovered {
					assertSurvivors(t, path, KindWAL, recs, []uint64{1, 2, 3, 4, 5, 6})
					if res.Rebuilt != 1 || res.DiscardCount != 0 {
						t.Fatalf("Repair = %+v, want exactly one REBUILT record and no discards: every byte of record 6 is on "+
							"disk, and its own checksum over those bytes proves it", res)
					}
					assertLogged(t, out, "WARN", "wal restored records whose length field was corrupt but whose checksum proved them complete",
						"path="+path, "records=1")
					assertNotLogged(t, out, "wal discarded a damaged record")
				} else {
					// Record 6 goes. The five entries in front of it were
					// acknowledged too, and they are the assertion that matters:
					// damage in the last record must not cascade backwards.
					assertSurvivors(t, path, KindWAL, recs, []uint64{1, 2, 3, 4, 5})
					if res.DiscardCount != 1 {
						t.Errorf("Repair.DiscardCount = %d, want exactly 1: only the damaged final record may go", res.DiscardCount)
					}
					if res.NextIndex != 6 {
						t.Errorf("Repair.NextIndex = %d, want 6: the highest surviving index is 5", res.NextIndex)
					}
					// AT ERROR, NAMING THE COMMIT. A discarded commit record is an
					// acknowledged write that is now lost, and the only thing that
					// makes discarding it acceptable is that an operator is told.
					assertLogged(t, out, "ERROR", "wal discarded a damaged record",
						"path="+path, "stage=framing", "record_type=commit", "record_index=6")
				}

				// And the server starts, either way, and can go on writing.
				app := &testApplier{}
				l, err := Open(LogOptions{Dir: dir, Applier: app})
				if err != nil {
					t.Fatalf("Open after the repair: %v: recovery must always reach a running server", err)
				}
				wantApplied := 2
				if recovered {
					wantApplied = 3
				}
				if app.count() != wantApplied {
					_ = l.Close()
					t.Fatalf("Open applied %d entries, want %d", app.count(), wantApplied)
				}
				if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"repair"}`)}); err != nil {
					_ = l.Close()
					t.Fatalf("Write after the repair: %v", err)
				}
				if err := l.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				assertIndicesUnique(t, path)
			})
		}
	}
}

// TestWALRepairTailRecoversAZeroPayloadFinalRecord is the boundary of the
// completeness proof: a record with an EMPTY payload is exactly FrameHeaderSize
// bytes, so the region a cut would discard is exactly one frame header. An
// implementation that treated "only a header's worth of bytes" as too small to
// bother with would skip the proof and throw away a complete record.
//
// The WAL write path emits no empty payloads today -- every prepare, commit and
// abort payload is JSON -- but Writer.Append has an upper bound on payload size
// and no lower one, and DUR-5's audit records go through the same writer. The
// proof must not rest on a property of today's callers.
func TestWALRepairTailRecoversAZeroPayloadFinalRecord(t *testing.T) {
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
	pristine, _ := repairScanClean(t, path, KindAudit, 2)

	// Corrupt only its length, so it declares an extent past the end of the file
	// and looks exactly like a torn tail.
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], 0x00000400)
	patch(t, path, last.Offset, b[:])

	res, out, err := captureRepair(t, path, KindAudit)
	if err != nil {
		t.Fatalf("RepairLog: %v: damage is never fatal", err)
	}
	if res.Rebuilt != 1 || res.DiscardCount != 0 {
		t.Fatalf("Repair = %+v, want one REBUILT record and no discards: the record is complete, its payload is simply empty", res)
	}
	assertSurvivors(t, path, KindAudit, pristine, []uint64{1, 2})
	assertLogged(t, out, "WARN", "wal restored records whose length field was corrupt but whose checksum proved them complete",
		"path="+path, "records=1")
	assertNotLogged(t, out, "wal discarded a damaged record")
}

// TestWALRepairTailDiscardsARegionDenseWithFrameHeaders defends the forward
// search's work budget. The bytes it walks are attacker-influenced -- they are
// the partly-written tail of a record whose payload carries a client-supplied
// message body -- so the search cannot be allowed unbounded verification work.
//
// Running out of budget is the ONE way damage can still cascade: records after
// the exhaustion point are discarded WITHOUT proof that they were unreadable. It
// is not refused (nothing is, now), but it is reported in Repair.Exhausted and
// logged at ERROR in its own words, so an operator can tell "we tidied up a torn
// tail" from "we gave up looking and cut". Without this test the budget is
// invisible: it can be deleted and every other test still passes.
func TestWALRepairTailDiscardsARegionDenseWithFrameHeaders(t *testing.T) {
	_, path, _, _ := buildWAL(t, repairTornOps()...)
	recs, cleanEnd := repairScanClean(t, path, KindWAL, 4)
	wantIndex := recs[len(recs)-1].Index + 1

	// A frame header declaring far more than will follow: the shape of a torn
	// tail, so the walk reaches the forward search.
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
	appendBytes(t, path, bytes.Repeat(plant[:], maxResyncCandidates+64))

	if got := fileSize(t, path) - cleanEnd; got <= 0 {
		t.Fatalf("the planted region is %d bytes, want a positive size", got)
	}

	res, out, err := captureRepair(t, path, KindWAL)
	if err != nil {
		t.Fatalf("RepairLog: %v: even a region built to exhaust the search must not stop the server starting", err)
	}
	if !res.Exhausted {
		t.Fatalf("Repair.Exhausted = false (%+v), want true: %d planted candidates must run the search out of its %d-candidate budget, "+
			"and a budget nothing reports is a budget nobody can audit", res, maxResyncCandidates+64, maxResyncCandidates)
	}
	// The four real records in front of the planted region are untouched: giving
	// up on the search must not become an excuse to cut backwards.
	assertSurvivors(t, path, KindWAL, recs, []uint64{1, 2, 3, 4})
	if !res.Truncated || res.At != cleanEnd {
		t.Errorf("Repair = %+v, want a truncation at %d (the end of the last real record)", res, cleanEnd)
	}
	// LOGGED IN ITS OWN WORDS. "We discarded a damaged record" and "we stopped
	// looking and discarded the rest" are different facts and an operator needs
	// to be able to tell them apart.
	assertLogged(t, out, "ERROR", "wal gave up searching for intact records after damage and discarded the rest of the log",
		"path="+path, "candidates_budget="+strconv.Itoa(maxResyncCandidates))
	// The discard line is ERROR TOO, and deliberately not WARN: the surviving
	// frame header says "prepare", which on its own would mean nothing
	// acknowledged went -- but the search gave up, so that header describes only
	// the FIRST record in an 83 KiB region whose remaining contents are
	// unknown. Discard.Severe exists for exactly this: a loss recovery could not
	// bound is severe whatever the one readable header claims. The two ERROR
	// lines carry different facts (what went, and that it went without proof)
	// and both have to be there.
	assertLogged(t, out, "ERROR", "wal discarded a damaged record",
		"path="+path, "stage=framing",
		"ran out of its work budget", "WITHOUT proof that it was unreadable")
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
		// The check is UP FRONT, before the file is opened, and the sentinel says
		// so. That placement is the point: RepairLog can now REWRITE a file, and
		// a rewrite driven by a kind this package has no magic for would stamp a
		// meaningless header onto a perfectly good log. A caller who does not
		// know what kind of file this is must never get as far as editing it.
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

// TestWALRepairTailLogsTheCut: the task is "detected, LOGGED, and truncated",
// and after the 2026-08-02 policy change the LOGGED half is the one carrying the
// whole argument. Recovery is allowed to throw damage away only because it says
// out loud what it threw away, so this pins the ordering and the fields:
//
//   - the DISCARD is logged BEFORE the file is changed, so a crash during the
//     truncate still leaves an operator a record of what was about to go;
//   - it names the file, the offset, the byte count, and the record's index and
//     type -- everything needed to say WHAT was lost;
//   - the confirmation of the cut comes after it.
func TestWALRepairTailLogsTheCut(t *testing.T) {
	_, path, _, _ := buildWAL(t, repairTornOps()...)
	recs, _ := repairScanClean(t, path, KindWAL, 4)
	last := recs[len(recs)-1]
	truncate(t, path, last.Offset+FrameHeaderSize+2)

	res, out, err := captureRepair(t, path, KindWAL)
	if err != nil {
		t.Fatalf("RepairLog: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("Repair = %+v, want Truncated true", res)
	}

	// WHAT was discarded, in full. Removing any one of these fields from
	// RepairLog fails this test, which is the point: a discard an operator cannot
	// identify is barely better than a silent one.
	assertLogged(t, out, "ERROR", "wal discarded a damaged record",
		"path="+path,
		"stage=framing",
		"offset="+strconv.FormatInt(last.Offset, 10),
		"bytes="+strconv.FormatInt(res.Removed, 10),
		"record_index="+strconv.FormatUint(last.Index, 10),
		"record_type=commit",
		"discarded as a torn tail")
	assertLogged(t, out, "WARN", "wal recovery discarded damaged regions", "path="+path, "discards=1")
	assertLogged(t, out, "WARN", "wal truncated damage at the end of the log",
		"path="+path, "at="+strconv.FormatInt(res.At, 10), "removed="+strconv.FormatInt(res.Removed, 10),
		"next_index="+strconv.FormatUint(res.NextIndex, 10))

	// ORDER: the loss is reported before the file is touched.
	i := strings.Index(out, "wal discarded a damaged record")
	j := strings.Index(out, "wal truncated damage at the end of the log")
	if i < 0 || j < 0 || i > j {
		t.Errorf("the discard must be logged BEFORE the cut is confirmed (discard at %d, cut at %d):\n%s", i, j, out)
	}

	// A nil Logger is the common case in tests and in a server started without
	// one; it must not panic and must repair identically.
	_, path2, _, _ := buildWAL(t, repairTornOps()...)
	recs2, _ := repairScanClean(t, path2, KindWAL, 4)
	truncate(t, path2, recs2[len(recs2)-1].Offset+FrameHeaderSize+2)
	res2, err := RepairTail(path2, KindWAL, nil)
	if err != nil {
		t.Fatalf("RepairTail with a nil Logger: %v", err)
	}
	if !res2.Truncated || res2.At != res.At || res2.Removed != res.Removed || res2.NextIndex != res.NextIndex {
		t.Errorf("a nil Logger repaired differently: %+v vs %+v", res2, res)
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
	frame := testCodec(t, path).encodeFrame(5, TypePrepare, payload)
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

// TestWALSemanticDamageIsDiscardedNotTruncated is the integration-level proof
// that TRUNCATION is unreachable from a SEMANTIC failure -- which survived the
// 2026-08-02 policy change intact, even though the refusal it used to prove did
// not.
//
// The file below is perfectly framed -- every checksum verifies, every index
// rises -- and the damage is in the LAST record: a second COMMIT of prepare 1,
// which names something that is no longer an open prepare. Two rules meet here,
// and both matter:
//
//   - RepairLog is a FRAMING-ONLY pass. It looks at frames, not at what they
//     mean, so it must see nothing wrong at all and must not touch one byte. A
//     semantic failure that could reach the truncation path would let recovery
//     cut a fully readable log -- and because this damage sits at the END, this
//     is exactly where a naive "the error is at the tail, so truncate" rule
//     fires.
//   - Replay no longer REFUSES the start over it (DECISIONS.md, "Availability
//     over retention"). The record is DISCARDED, reported in
//     Recovered.Discarded, and logged at ERROR -- at ERROR because a COMMIT
//     record is a client having been told a write was durable, so a discarded
//     one is an acknowledged write that is now lost.
//
// The file keeps its bytes either way: a replay-stage discard drops the record
// from the rebuilt MEMORY state and does not rewrite the log.
func TestWALSemanticDamageIsDiscardedNotTruncated(t *testing.T) {
	dir, path, _, _ := buildWAL(t,
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opCommit(1),                     // 3 -- prepare 1 was already committed
	)
	before := readFile(t, path)

	// The framing pass has no opinion: it looks at frames, not at what they mean.
	res, err := RepairTail(path, KindWAL, nil)
	if err != nil {
		t.Fatalf("RepairLog on a well-framed file: %v, want no error: it is a framing-only pass", err)
	}
	repairAssertUntouched(t, path, before, res)

	got, rec, out, err := openCapturing(t, dir)
	if err != nil {
		t.Fatalf("Open on a semantically damaged log: %v: damage is never fatal now, it is discarded and logged", err)
	}

	// The one real transaction still recovers; the duplicate commit does not
	// re-apply it.
	want := []Committed{wantC(1, 2, "message", `{"n":1}`)}
	if !sameCommitted(got, want) {
		t.Fatalf("Open applied %s, want %s: a duplicate commit must be discarded, never applied twice", showCommitted(got), showCommitted(want))
	}
	if rec.Applied != 1 || rec.DiscardCount != 1 {
		t.Fatalf("Recovered = %+v, want Applied 1 and exactly one discard (record 3)", rec)
	}
	d := rec.Discarded[0]
	if d.Stage != "replay" || d.Index != 3 || d.Type != TypeCommit || !d.TypeKnown {
		t.Errorf("Recovered.Discarded[0] = %+v, want the replay-stage loss of COMMIT record 3", d)
	}
	if !strings.Contains(d.Reason, "not an open prepare") {
		t.Errorf("the discard reason is %q, want it to name the semantic failure", d.Reason)
	}

	// LOGGED, at ERROR, naming the commit. Delete the logDiscards call in Open
	// and this line goes with it.
	assertLogged(t, out, "ERROR", "wal discarded a damaged record",
		"path="+path, "stage=replay", "record_index=3", "record_type=commit", "not an open prepare")

	// And the bytes are still there. A replay-stage discard changes what is in
	// MEMORY, never what is on disk.
	if after := readFile(t, path); !bytes.Equal(before, after) {
		t.Fatalf("Open changed the WAL: %d bytes before, %d after: a semantic failure must never truncate",
			len(before), len(after))
	}
}

// TestMACKeyCreationRule pins the ONE state in which a missing key file is fatal:
// a log that positively identifies itself as format version 2 -- our magic,
// version field 2 -- and is longer than its own file header.
//
// Everything else creates a key, and the reason is not generosity. Under a fresh
// key an unidentifiable file fails its header tag and verifies no record, which
// is the QUARANTINE branch: renamed aside, every byte preserved, bus starts. The
// destructive paths (truncate, rewrite-and-discard) need a header that verifies,
// so they are unreachable there -- the argument for the fatal does not reach
// those states. A file no longer than its own header holds no record for the
// same reason, hence the Size narrowing, which mirrors repairLog's exactly.
func TestMACKeyCreationRule(t *testing.T) {
	// v2Log lays down a real version 2 log under a known key, then REMOVES the
	// key file: that is the "operator lost the key" shape.
	v2Log := func(t *testing.T, records int) string {
		t.Helper()
		dir := t.TempDir()
		plantGoldenMACKey(t, dir)
		path := filepath.Join(dir, WALFileName)
		w, err := OpenWriter(path, KindWAL)
		if err != nil {
			t.Fatalf("OpenWriter: %v", err)
		}
		for i := 0; i < records; i++ {
			if _, err := w.Append(TypePrepare, []byte(`{"n":1}`)); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := os.Remove(macKeyPath(dir)); err != nil {
			t.Fatalf("removing the key file: %v", err)
		}
		return path
	}

	tests := []struct {
		name      string
		build     func(t *testing.T) string
		mayCreate bool
	}{
		{"the log does not exist", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), WALFileName)
		}, true},

		{"a zero-length log", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), WALFileName)
			if err := os.WriteFile(path, nil, fileMode); err != nil {
				t.Fatalf("writing a zero-length log: %v", err)
			}
			return path
		}, true},

		{"64 bytes of garbage", func(t *testing.T) string {
			// The exact shape cmd/agent-bus seeds to prove the bus quarantines an
			// unreadable log and starts anyway. It has no magic of ours, so it can
			// never reach a destructive path -- refusing to boot over it would
			// hold the bus hostage for nothing.
			path := filepath.Join(t.TempDir(), WALFileName)
			if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 64), fileMode); err != nil {
				t.Fatalf("writing garbage: %v", err)
			}
			return path
		}, true},

		{"a file too short to hold a magic", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), WALFileName)
			if err := os.WriteFile(path, []byte("AGBUS"), fileMode); err != nil {
				t.Fatalf("writing a stub: %v", err)
			}
			return path
		}, true},

		{"a format version 1 log", func(t *testing.T) string {
			// Its records are authenticated by CRC32C; a brand new key is exactly
			// what upgradeV1 needs.
			path := filepath.Join(t.TempDir(), WALFileName)
			writeV1Log(t, path, KindWAL, v1Record{Index: 1, Type: TypePrepare, Payload: []byte(`{"n":1}`)})
			return path
		}, true},

		{"a version 2 file header and nothing else", func(t *testing.T) string {
			// Size == fileHeaderSize: no record exists, so nothing can be lost.
			return v2Log(t, 0)
		}, true},

		{"a version 2 log holding records", func(t *testing.T) string {
			return v2Log(t, 2)
		}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.build(t)

			got, err := macKeyMayBeCreated(path, KindWAL)
			if err != nil {
				t.Fatalf("macKeyMayBeCreated: %v", err)
			}
			if got != tc.mayCreate {
				t.Fatalf("macKeyMayBeCreated = %v, want %v", got, tc.mayCreate)
			}

			// And the predicate must be what macKeyFor actually acts on: creating
			// the key file, or failing with ErrMACKeyMissing and naming both paths.
			key, err := macKeyFor(path, KindWAL, nil)
			keyPath := macKeyPath(filepath.Dir(path))
			if !tc.mayCreate {
				if !errors.Is(err, ErrMACKeyMissing) {
					t.Fatalf("macKeyFor = %v, want ErrMACKeyMissing on a version 2 log with records", err)
				}
				for _, want := range []string{path, keyPath, "on-disk format version 2"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the error does not mention %q: %v", want, err)
					}
				}
				if _, serr := os.Stat(keyPath); !os.IsNotExist(serr) {
					t.Errorf("macKeyFor created %s anyway (stat err = %v)", keyPath, serr)
				}
				return
			}
			if err != nil {
				t.Fatalf("macKeyFor: %v, want a freshly generated key", err)
			}
			if len(key) != macKeySize {
				t.Fatalf("macKeyFor returned %d key bytes, want %d", len(key), macKeySize)
			}
			fi, serr := os.Stat(keyPath)
			if serr != nil {
				t.Fatalf("the key file was not written: %v", serr)
			}
			if fi.Mode().Perm() != macKeyMode {
				t.Errorf("the key file is mode %v, want %v", fi.Mode().Perm(), macKeyMode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DUR-12 -- the format version 1 -> 2 upgrade, and the two key states that are
// a deliberate exception to the always-restart policy.
//
// These three tests are the halves of one contract, and getting the CONTRAST
// right is the whole difficulty:
//
//	a version 1 log with no key   -> generate a key, upgrade, lose nothing
//	a version 2 log with no key   -> REFUSE, and do not touch the log
//	a version 2 log with a wrong key -> REFUSE, and do not touch the log
//
// The refusals exist because under a fresh or wrong key EVERY record fails
// verification, so a discard-the-unverifiable pass would destroy a log that is
// probably intact over a misconfiguration. The permission exists because a
// version 1 log's records are authenticated by an unkeyed CRC32C, so a brand new
// key can cost nothing -- it is exactly what the upgrade needs.
// ---------------------------------------------------------------------------

// v1FixtureClock is a fixed clock, so a version 1 fixture is byte-reproducible
// and a comparison against a backup is a comparison of the same bytes twice.
func v1FixtureClock() time.Time { return time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC) }

// v1TxnLog lays down a genuine FORMAT VERSION 1 WAL in dir -- the legacy 16-byte
// file header and 20-byte CRC32C frames -- holding three transactions: one
// committed, one ABORTED, one committed. It returns the path, the records
// exactly as they were written, and what a correct replay must deliver.
//
// The abort is in there on purpose. The upgrade carries records across the
// framing change byte for byte, so a record type that recovery treats specially
// is the one most likely to be quietly dropped or renumbered by a conversion
// that is subtly wrong.
func v1TxnLog(t *testing.T, dir string) (path string, recs []v1Record, want []Committed) {
	t.Helper()
	ts := v1FixtureClock()
	enc := func(b []byte, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatalf("encoding a version 1 fixture payload: %v", err)
		}
		return b
	}
	recs = []v1Record{
		{Index: 1, Type: TypePrepare, Payload: enc(encodePrepare("message", json.RawMessage(`{"n":1}`), ts))},
		{Index: 2, Type: TypeCommit, Payload: enc(encodeCommit(1))},
		{Index: 3, Type: TypePrepare, Payload: enc(encodePrepare("agent", nil, ts.Add(time.Second)))},
		{Index: 4, Type: TypeAbort, Payload: enc(encodeAbort(3, "no room"))},
		{Index: 5, Type: TypePrepare, Payload: enc(encodePrepare("message", json.RawMessage(`{"n":3}`), ts.Add(2*time.Second)))},
		{Index: 6, Type: TypeCommit, Payload: enc(encodeCommit(5))},
	}
	path = filepath.Join(dir, WALFileName)
	writeV1Log(t, path, KindWAL, recs...)
	want = []Committed{
		wantC(1, 2, "message", `{"n":1}`),
		wantC(5, 6, "message", `{"n":3}`),
	}
	return path, recs, want
}

// dirEntryNames lists the names in dir, sorted. It is how a "the log was not
// touched" assertion proves the negative as well as the positive: no quarantine
// copy, no `.upgrade` temporary, no backup, nothing new at all.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// globIn returns the sorted names in dir matching pattern.
func globIn(t *testing.T, dir, pattern string) []string {
	t.Helper()
	var found []string
	for _, name := range dirEntryNames(t, dir) {
		ok, err := filepath.Match(pattern, name)
		if err != nil {
			t.Fatalf("bad glob %q: %v", pattern, err)
		}
		if ok {
			found = append(found, name)
		}
	}
	return found
}

// assertRecordsIdentical demands that the log holds exactly the records the
// version 1 fixture held, with the SAME indices, types and payload bytes.
//
// The indices are the point. The upgrade rewrites every frame, which is the one
// moment in this package's life when renumbering would be easy and invisible --
// and invariant 1 forbids reusing an id, so a record that came out of the
// upgrade with a different index would be a violation the record count cannot
// see.
func assertRecordsIdentical(t *testing.T, path string, want []v1Record) {
	t.Helper()
	got, _, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("ScanAll after the upgrade: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("the upgraded log holds %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Index != want[i].Index {
			t.Errorf("record %d has index %d after the upgrade, want %d: the upgrade must NEVER renumber (invariant 1)",
				i, got[i].Index, want[i].Index)
		}
		if got[i].Type != want[i].Type {
			t.Errorf("record %d is a %s after the upgrade, want a %s", i, got[i].Type, want[i].Type)
		}
		if !bytes.Equal(got[i].Payload, want[i].Payload) {
			t.Errorf("record %d has payload %q after the upgrade, want %q: payloads cross the framing change byte for byte",
				i, got[i].Payload, want[i].Payload)
		}
	}
}

// TestWALReadsFormatVersion1Log is the do-not-brick-existing-buses leg.
//
// A bus that is running today has a version 1 log. A version bump with no read
// path would meet a version number it does not implement, refuse to start, and
// leave no route back. So a version 1 log must be READ, converted once, and lose
// absolutely nothing in the process -- and the conversion must be idempotent,
// because a start that upgraded twice would mean the first one did not stick.
func TestWALReadsFormatVersion1Log(t *testing.T) {
	dir := t.TempDir()
	path, recs, want := v1TxnLog(t, dir)
	original := readFile(t, path)

	// The fixture really is version 1, and really is laid out the legacy way.
	// Without this the whole test could be passing against a version 2 file.
	if v, err := detectFormat(path, KindWAL); err != nil || v != formatVersionV1 {
		t.Fatalf("the fixture reports format version %d (err %v), want %d", v, err, formatVersionV1)
	}
	wantSize := int64(fileHeaderSizeV1)
	for _, r := range recs {
		wantSize += int64(frameHeaderSizeV1 + len(r.Payload))
	}
	if int64(len(original)) != wantSize {
		t.Fatalf("the fixture is %d bytes, want %d: it is not laid out in the legacy 16/20-byte framing", len(original), wantSize)
	}
	if _, err := OpenWriter(path, KindWAL); err == nil {
		t.Fatalf("OpenWriter accepted a version 1 log: there is no downgrade write, so it must refuse one outright")
	}

	// ---- the upgrade ----
	got, rec, out, err := openCapturing(t, dir)
	if err != nil {
		t.Fatalf("Open on a format version 1 log: %v: an existing bus must not be bricked by the format bump", err)
	}
	if !sameCommitted(got, want) {
		t.Fatalf("replay of the upgraded log delivered %s, want %s", showCommitted(got), showCommitted(want))
	}
	if rec.NextIndex != 7 {
		t.Errorf("Recovered.NextIndex = %d, want 7 (one past the highest index that crossed the upgrade)", rec.NextIndex)
	}
	if rec.DiscardCount != 0 || rec.MissingRecords != 0 {
		t.Errorf("the upgrade lost something: Recovered = {discards %d, missing %d}, want 0 and 0",
			rec.DiscardCount, rec.MissingRecords)
	}
	assertLogged(t, out, "info", "wal upgraded a log from on-disk format version 1 to 2",
		"records=6", "path=")

	// ---- the file is now version 2, and holds exactly what it held before ----
	if v, err := detectFormat(path, KindWAL); err != nil || v != FormatVersion {
		t.Fatalf("after Open the log reports format version %d (err %v), want %d", v, err, FormatVersion)
	}
	upgraded := readFile(t, path)
	if v := binary.BigEndian.Uint32(upgraded[8:12]); v != FormatVersion {
		t.Errorf("the file header's version field is %d, want %d", v, FormatVersion)
	}
	assertRecordsIdentical(t, path, recs)

	// ---- the version 1 bytes were kept ----
	backups := globIn(t, dir, WALFileName+".v1-*")
	if len(backups) != 1 {
		t.Fatalf("the directory holds %d version 1 backups (%v), want exactly 1", len(backups), backups)
	}
	if b := readFile(t, filepath.Join(dir, backups[0])); !bytes.Equal(b, original) {
		t.Fatalf("the backup %s is %d bytes and does not match the %d-byte original: the backup must be the ORIGINAL version 1 file",
			backups[0], len(b), len(original))
	}
	if leftovers := globIn(t, dir, WALFileName+".upgrade"); len(leftovers) != 0 {
		t.Errorf("the upgrade temporary %v was left behind", leftovers)
	}

	// ---- a second start is a NO-OP ----
	got2, _, out2, err := openCapturing(t, dir)
	if err != nil {
		t.Fatalf("the second Open: %v", err)
	}
	if !sameCommitted(got2, want) {
		t.Fatalf("the second start delivered %s, want %s", showCommitted(got2), showCommitted(want))
	}
	assertNotLogged(t, out2, "wal upgraded a log from on-disk format version 1 to 2")
	if again := readFile(t, path); !bytes.Equal(again, upgraded) {
		t.Errorf("the second start CHANGED the log (%d bytes vs %d): the upgrade must run exactly once",
			len(again), len(upgraded))
	}
	if b := globIn(t, dir, WALFileName+".v1-*"); len(b) != 1 {
		t.Errorf("after the second start the directory holds %d version 1 backups (%v), want still exactly 1", len(b), b)
	}

	// ---- appends carry on from the right index ----
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open to append after the upgrade: %v", err)
	}
	defer l.Close()
	c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"upgrade"}`)})
	if err != nil {
		t.Fatalf("Write after the upgrade: %v", err)
	}
	if c.PrepareIndex != 7 || c.CommitIndex != 8 {
		t.Fatalf("the first write after the upgrade got prepare %d / commit %d, want 7 / 8: indices continue, they do not restart",
			c.PrepareIndex, c.CommitIndex)
	}
}

// v2LogWithRecords writes n complete transactions into a fresh data directory
// through the REAL two-phase path under the deterministic golden key, and
// returns the directory, the WAL path and the exact bytes on disk.
func v2LogWithRecords(t *testing.T, n int) (dir, path string, image []byte) {
	t.Helper()
	dir = t.TempDir()
	plantGoldenMACKey(t, dir)
	ts := v1FixtureClock()
	i := 0
	l, err := Open(LogOptions{Dir: dir, Now: func() time.Time {
		i++
		return ts.Add(time.Duration(i) * time.Second)
	}})
	if err != nil {
		t.Fatalf("v2LogWithRecords: Open: %v", err)
	}
	for k := 0; k < n; k++ {
		if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":` + strconv.Itoa(k) + `}`)}); err != nil {
			t.Fatalf("v2LogWithRecords: Write %d: %v", k, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("v2LogWithRecords: Close: %v", err)
	}
	path = filepath.Join(dir, WALFileName)
	return dir, path, readFile(t, path)
}

// TestWALWrongMACKeyIsFatal is the one place recovery is allowed to refuse to
// start, and the assertion that matters most is not the error -- it is that THE
// LOG IS STILL THERE, byte for byte.
//
// A wrong key makes an intact log look exactly like a destroyed one: the file
// header's MAC fails and so does every record's, because they fail for the same
// reason. Under the always-restart policy the natural response would be to
// quarantine or discard, and that would turn "someone mounted the wrong volume"
// or "the key file was restored from the wrong backup" into permanent data loss
// over a misconfiguration that takes seconds to fix. So this refuses, names the
// key file, and touches nothing.
func TestWALWrongMACKeyIsFatal(t *testing.T) {
	tests := []struct {
		name string
		// key is what replaces the correct key file.
		key string
		// want is the sentinel errors.Is must match.
		want error
		// names are strings the message must carry. %L is the log path and %K
		// the key path, substituted per case.
		names []string
	}{
		{
			name:  "a different, well-formed key refuses and names both paths",
			key:   forgerMACKey + "\n",
			want:  ErrMACKeyMismatch,
			names: []string{"%L", "%K", "WRONG KEY"},
		},
		{
			name:  "a key of the wrong length refuses",
			key:   "0123456789\n",
			want:  ErrMACKeyMalformed,
			names: []string{"%K", "10 characters", "64 hexadecimal characters"},
		},
		{
			name:  "a key that is not hexadecimal refuses",
			key:   strings.Repeat("z", 64) + "\n",
			want:  ErrMACKeyMalformed,
			names: []string{"%K", "not hexadecimal"},
		},
		{
			name:  "an empty key file refuses rather than being regenerated",
			key:   "",
			want:  ErrMACKeyMalformed,
			names: []string{"%K", "0 characters"},
		},
	}

	probed := 0
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir, path, before := v2LogWithRecords(t, 3)
			keyPath := macKeyPath(dir)
			entriesBefore := dirEntryNames(t, dir)
			probed++

			if err := os.WriteFile(keyPath, []byte(tc.key), macKeyMode); err != nil {
				t.Fatalf("replacing the key file: %v", err)
			}

			_, _, out, err := openCapturing(t, dir)
			if err == nil {
				t.Fatalf("Open SUCCEEDED with %s; a key that cannot read this log is fatal on purpose", tc.name)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open error = %v, want one matching %v", err, tc.want)
			}
			for _, want := range tc.names {
				want = strings.ReplaceAll(strings.ReplaceAll(want, "%L", path), "%K", keyPath)
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not name %q, so an operator cannot tell which file to fix: %v", want, err)
				}
			}

			// THE ASSERTION THIS TEST EXISTS FOR. Nothing was truncated,
			// rewritten, quarantined or deleted: the log is byte-for-byte what it
			// was, and no new file appeared beside it.
			after := readFile(t, path)
			if !bytes.Equal(before, after) {
				t.Fatalf("the log CHANGED after a refused start (%d bytes before, %d after): "+
					"a misconfiguration must never cost a byte of a log that is probably intact", len(before), len(after))
			}
			if got := dirEntryNames(t, dir); !reflect.DeepEqual(got, entriesBefore) {
				t.Errorf("the data directory now holds %v, want the unchanged %v: nothing may be quarantined or copied aside",
					got, entriesBefore)
			}
			assertNotLogged(t, out, "wal quarantined an unreadable log and started a fresh one")
			assertNotLogged(t, out, "wal truncated damage at the end of the log")
			assertNotLogged(t, out, "wal rewrote a damaged log, keeping every intact record")
		})
	}
	if probed == 0 {
		t.Fatalf("no key state was probed: this test asserted NOTHING about a wrong key")
	}
}

// TestWALMissingMACKeyOnV1LogIsNotFatal is the sharp edge of the whole task: the
// SAME missing key file is harmless in one state and fatal in another, and the
// two are asserted together because the contrast is the contract.
//
//	version 1 log, no key      -> generate one, upgrade, lose nothing.
//	                              Version 1 records are authenticated by an
//	                              unkeyed CRC32C, so a fresh key costs nothing.
//	version 2 log with records -> REFUSE and touch nothing. Under a fresh key
//	                              every record would fail, and a
//	                              discard-the-unverifiable pass would destroy an
//	                              intact log over a lost key file.
//	version 2 header only      -> generate one and start. The file provably holds
//	                              no record, so there is nothing to lose, and
//	                              refusing would hold the bus hostage to an empty
//	                              file.
func TestWALMissingMACKeyOnV1LogIsNotFatal(t *testing.T) {
	probed := 0

	t.Run("a version 1 log with no key generates one and upgrades", func(t *testing.T) {
		probed++
		dir := t.TempDir()
		path, recs, want := v1TxnLog(t, dir)
		keyPath := macKeyPath(dir)
		if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
			t.Fatalf("the fixture already has a key file (stat err = %v); there is nothing to prove", err)
		}

		got, rec, _, err := openCapturing(t, dir)
		if err != nil {
			t.Fatalf("Open on a version 1 log with no key: %v: this is the state EVERY existing bus is in", err)
		}
		if !sameCommitted(got, want) {
			t.Fatalf("replay delivered %s, want %s: the upgrade under a fresh key must lose nothing", showCommitted(got), showCommitted(want))
		}
		if rec.DiscardCount != 0 || rec.MissingRecords != 0 {
			t.Errorf("Recovered = {discards %d, missing %d}, want 0 and 0", rec.DiscardCount, rec.MissingRecords)
		}
		assertRecordsIdentical(t, path, recs)

		// The generated key is a real key file: 0600, and 64 hexadecimal
		// characters that decode to the key length this package uses.
		fi, err := os.Stat(keyPath)
		if err != nil {
			t.Fatalf("no key file was generated at %s: %v", keyPath, err)
		}
		if fi.Mode().Perm() != macKeyMode {
			t.Errorf("the generated key file is mode %v, want %v: anything that can read it can forge every record",
				fi.Mode().Perm(), macKeyMode)
		}
		text := strings.TrimRight(string(readFile(t, keyPath)), "\r\n")
		if len(text) != hex.EncodedLen(macKeySize) {
			t.Fatalf("the generated key holds %d characters, want %d", len(text), hex.EncodedLen(macKeySize))
		}
		if _, err := hex.DecodeString(text); err != nil {
			t.Errorf("the generated key is not hexadecimal: %v", err)
		}
		if text == goldenMACKey || text == forgerMACKey {
			t.Errorf("the generated key is a CONSTANT from this test file; it must come from crypto/rand")
		}
	})

	t.Run("a version 2 log with records and no key is FATAL and leaves the log untouched", func(t *testing.T) {
		probed++
		dir, path, before := v2LogWithRecords(t, 3)
		keyPath := macKeyPath(dir)
		if err := os.Remove(keyPath); err != nil {
			t.Fatalf("removing the key file: %v", err)
		}
		entriesBefore := dirEntryNames(t, dir)

		_, _, out, err := openCapturing(t, dir)
		if err == nil {
			t.Fatalf("Open SUCCEEDED on a version 2 log whose key is missing; under a fresh key every record would fail verification")
		}
		if !errors.Is(err, ErrMACKeyMissing) {
			t.Fatalf("Open error = %v, want one matching ErrMACKeyMissing", err)
		}
		for _, want := range []string{path, keyPath} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q; it must name BOTH the log and the key: %v", want, err)
			}
		}
		if _, serr := os.Stat(keyPath); !os.IsNotExist(serr) {
			t.Errorf("a key file was generated at %s anyway (stat err = %v)", keyPath, serr)
		}
		if after := readFile(t, path); !bytes.Equal(before, after) {
			t.Fatalf("the log CHANGED after a refused start (%d bytes before, %d after)", len(before), len(after))
		}
		if got := dirEntryNames(t, dir); !reflect.DeepEqual(got, entriesBefore) {
			t.Errorf("the data directory now holds %v, want the unchanged %v", got, entriesBefore)
		}
		assertNotLogged(t, out, "wal quarantined an unreadable log and started a fresh one")
	})

	t.Run("a version 2 file header and nothing else starts, because it holds no record", func(t *testing.T) {
		probed++
		dir, path, before := v2LogWithRecords(t, 0)
		if int64(len(before)) != FileHeaderSize {
			t.Fatalf("the fixture is %d bytes, want exactly the %d-byte file header", len(before), FileHeaderSize)
		}
		if err := os.Remove(macKeyPath(dir)); err != nil {
			t.Fatalf("removing the key file: %v", err)
		}

		got, _, _, err := openCapturing(t, dir)
		if err != nil {
			t.Fatalf("Open on a header-only version 2 log with no key: %v: it provably holds no record, so refusing would hold the bus hostage for nothing", err)
		}
		if len(got) != 0 {
			t.Fatalf("replay delivered %d entries from a header-only log", len(got))
		}
		if _, serr := os.Stat(macKeyPath(dir)); serr != nil {
			t.Errorf("no key file was generated: %v", serr)
		}
		// The unreadable header was moved aside, never deleted: the operator is
		// owed the bytes even when this code can make nothing of them.
		aside := globIn(t, dir, WALFileName+".corrupt-*")
		if len(aside) != 1 {
			t.Fatalf("the directory holds %d quarantined files (%v), want exactly 1: the old header is renamed, never deleted", len(aside), aside)
		}
		if b := readFile(t, filepath.Join(dir, aside[0])); !bytes.Equal(b, before) {
			t.Errorf("the quarantined file is not the original %d bytes", len(before))
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("no fresh log was created at %s: %v", path, err)
		}
	})

	if probed == 0 {
		t.Fatalf("no key state was probed: this test asserted NOTHING")
	}
}

// TestWALV2HeaderDamageWithTheRightKeyStillRepairs is the case that must NOT be
// mistaken for a wrong key, and it is the reason the wrong-key diagnosis is made
// on EVIDENCE rather than on the header alone.
//
// A wrong key fails the file header's MAC and every record's MAC. Damage
// confined to the header fails the header's MAC and NOTHING else -- so a single
// record that verifies PROVES the key is right, and the answer is the ordinary
// header rebuild: every record kept, the bus starts. If this test ever starts
// reporting ErrMACKeyMismatch, recovery has begun refusing to boot over a
// flipped bit in 32 bytes it can regenerate from a constant.
func TestWALV2HeaderDamageWithTheRightKeyStillRepairs(t *testing.T) {
	dir, path, before := v2LogWithRecords(t, 3)
	pristine, _, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("the fixture does not scan clean: %v", err)
	}
	if len(pristine) != 6 {
		t.Fatalf("the fixture holds %d records, want 6", len(pristine))
	}

	// Clobber the file header's 32-byte MAC and nothing else. The magic and the
	// version field survive, so the file still positively identifies itself as
	// ours and as version 2 -- which is exactly the shape a wrong key produces.
	patch(t, path, frameCoveredBytes, bytes.Repeat([]byte{0xAB}, MACSize))
	if bytes.Equal(readFile(t, path), before) {
		t.Fatalf("the header tag was not actually clobbered")
	}

	got, rec, out, err := openCapturing(t, dir)
	if err != nil {
		t.Fatalf("Open with a damaged header and the RIGHT key: %v: one verifying record proves the key is right, and this must be an ordinary header rebuild",
			err)
	}
	if errors.Is(err, ErrMACKeyMismatch) {
		t.Fatalf("header damage was diagnosed as a wrong key")
	}
	if !rec.Repaired.HeaderRepaired {
		t.Fatalf("Repaired = %+v, want HeaderRepaired true", rec.Repaired)
	}
	if rec.Repaired.DiscardCount != 0 || rec.Repaired.DiscardedBytes != 0 {
		t.Errorf("Repaired discarded %d regions / %d bytes, want 0: only the header was damaged",
			rec.Repaired.DiscardCount, rec.Repaired.DiscardedBytes)
	}
	if rec.Repaired.Kept != uint64(len(pristine)) {
		t.Errorf("Repaired.Kept = %d, want %d: every record must survive a header rebuild", rec.Repaired.Kept, len(pristine))
	}
	if len(got) != 3 {
		t.Errorf("replay delivered %d entries, want 3", len(got))
	}
	assertLogged(t, out, "error", "wal rebuilding a damaged file header", "records_salvaged=6")

	// Every record is still there, with its original index, type and bytes...
	var want []uint64
	for _, r := range pristine {
		want = append(want, r.Index)
	}
	assertSurvivors(t, path, KindWAL, pristine, want)

	// ...and the bus is genuinely usable afterwards, which "Open returned nil"
	// on its own does not prove.
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open after the header rebuild: %v", err)
	}
	defer l.Close()
	c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"header repair"}`)})
	if err != nil {
		t.Fatalf("Write after the header rebuild: %v", err)
	}
	if c.PrepareIndex != 7 {
		t.Errorf("the write after the header rebuild got prepare index %d, want 7", c.PrepareIndex)
	}
}
