package wal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// THE DURABLE RECORD-INDEX FLOOR (<data-dir>/wal-index-floor).
//
// What is under test, in one sentence: an index this data directory has ever
// AUTHORISED is never authorised again, whatever happens to the log --
// truncation, mid-file rewrite, or a whole-file quarantine that takes the log's
// own answer away entirely.
//
// Two defects motivated the file and both are fixed here rather than accepted:
//
//	e120153b -- a discarded TAIL record's index was handed straight back out.
//	db350e39 -- a whole-log quarantine reset the index to 1, reissuing the bus's
//	            ENTIRE history, and with it every message id internal/hub derives
//	            from it.
//
// The rule they violate is invariant 1, which the user reaffirmed WITHOUT
// narrowing on 2026-08-02: "when recovery discards a record, the sequence
// advances past the hole; it never rewinds". THERE IS NO REFUSE-TO-START
// BEHAVIOUR HERE AND NONE IS WANTED -- invariant 6 is intact, the bus always
// comes up, it simply comes up ABOVE everything it ever minted. Any future test
// in this package that asserts a refusal over a damaged LOG is asserting a
// superseded policy.
//
// The one narrow refusal that does exist is over a corrupt IDENTITY file (the
// floor itself), which is the same exception already granted for the MAC key and
// the persisted bus id, and its error names a one-step remedy so a bus is never
// bricked. That remedy is asserted below, because it is what keeps the refusal
// compatible with invariant 6.
//
// Every directory here is a t.TempDir(). The tracked ./data directory is never
// touched.
// ---------------------------------------------------------------------------

// encodeFloorBody renders the canonical two-line body for a floor file.
func encodeFloorBody(reserved, written uint64) string {
	return fmt.Sprintf("reserved %d\nwritten %d\n", reserved, written)
}

// writeFloorFile lays a floor file down by hand.
//
// header == "" means "compute the CANONICAL header for this body", which is what
// the line-level corruption cases need: readIndexFloorFile verifies the SHA-256
// before it parses a single number, so a fixture with a stale digest would only
// ever exercise the digest check and would silently never reach the parser it
// was written to test.
func writeFloorFile(t *testing.T, dir, header, body string) string {
	t.Helper()
	if header == "" {
		sum := sha256.Sum256([]byte(body))
		header = indexFloorMagic + " v" + strconv.Itoa(indexFloorFileVersion) + " sha256=" + hex.EncodeToString(sum[:])
	}
	path := filepath.Join(dir, IndexFloorFileName)
	if err := os.WriteFile(path, []byte(header+"\n"+body), 0o600); err != nil {
		t.Fatalf("writing the floor fixture %s: %v", path, err)
	}
	return path
}

// readFloorFile reads back the two numbers on disk, through the package's own
// reader, so a test never has to reimplement the format it is checking.
func readFloorFile(t *testing.T, dir string) (reserved, written uint64) {
	t.Helper()
	r, w, existed, err := readIndexFloorFile(filepath.Join(dir, IndexFloorFileName))
	if err != nil {
		t.Fatalf("reading back the floor in %s: %v", dir, err)
	}
	if !existed {
		t.Fatalf("no floor file in %s: it must be created before the first append", dir)
	}
	return r, w
}

// mustOpenFloor opens the floor in dir and fails the test if it will not.
func mustOpenFloor(t *testing.T, dir string) *indexFloor {
	t.Helper()
	f, err := openIndexFloor(dir)
	if err != nil {
		t.Fatalf("openIndexFloor(%s): %v", dir, err)
	}
	return f
}

// TestWALIndexFloorRoundTripsAndOnlyEverRises is the core property, and it is
// the one every other guarantee in this file rests on: both fields survive a
// close/reopen exactly, and NEITHER CAN GO DOWN. A floor that can be lowered is
// not a floor -- it is a suggestion, and invariant 1 would be back to being
// something each recovery path has to remember rather than something the data
// directory enforces.
func TestWALIndexFloorRoundTripsAndOnlyEverRises(t *testing.T) {
	dir := t.TempDir()

	f := mustOpenFloor(t, dir)
	if f.existedAtOpen() {
		t.Errorf("existedAtOpen() = true for a fresh data directory: nothing has written the floor yet")
	}
	if f.burned() != 0 || f.ceiling() != 0 {
		t.Errorf("a fresh floor reports burned=%d ceiling=%d, want 0 and 0", f.burned(), f.ceiling())
	}
	if got, want := f.Path(), filepath.Join(dir, IndexFloorFileName); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}

	// begin(1): nothing is burned yet, and a whole block is authorised ahead.
	if err := f.begin(1); err != nil {
		t.Fatalf("begin(1): %v", err)
	}
	if got, want := f.burned(), uint64(0); got != want {
		t.Errorf("after begin(1) burned() = %d, want %d (start-1: no index below 1 exists)", got, want)
	}
	if got, want := f.ceiling(), uint64(indexReserveBlock); got != want {
		t.Errorf("after begin(1) ceiling() = %d, want %d: begin authorises a whole block ahead so the hot path need not touch the disk", got, want)
	}

	// It round-trips through the file, which is the whole point of it being a
	// file: a fresh process must see the same numbers.
	reopened := mustOpenFloor(t, dir)
	if !reopened.existedAtOpen() {
		t.Error("existedAtOpen() = false after begin wrote the file")
	}
	if reopened.burned() != f.burned() || reopened.ceiling() != f.ceiling() {
		t.Errorf("reopened floor is {burned:%d ceiling:%d}, want {%d %d}: the floor must survive the process that wrote it, or it protects nothing across a restart",
			reopened.burned(), reopened.ceiling(), f.burned(), f.ceiling())
	}

	// seal raises `written` and never touches `reserved` downwards.
	if err := f.seal(100); err != nil {
		t.Fatalf("seal(100): %v", err)
	}
	if got := f.burned(); got != 100 {
		t.Errorf("after seal(100) burned() = %d, want 100", got)
	}
	if got := f.ceiling(); got != indexReserveBlock {
		t.Errorf("after seal(100) ceiling() = %d, want it left at %d: sealing records what was used, it does not release a reservation", got, indexReserveBlock)
	}

	// A LOWER seal narrows NOTHING. This is the case a "we shut down cleanly, so
	// we can give the spare indices back" optimisation would get wrong, and the
	// type has no field that could express it.
	if err := f.seal(3); err != nil {
		t.Fatalf("seal(3) after seal(100): %v", err)
	}
	if got := f.burned(); got != 100 {
		t.Errorf("seal(3) lowered burned() to %d after seal(100): a clean close may never narrow the floor", got)
	}
	if got := f.ceiling(); got != indexReserveBlock {
		t.Errorf("seal(3) moved ceiling() to %d, want %d unchanged", got, indexReserveBlock)
	}

	// A LOWER begin is likewise absorbed: recovery may only ever resume higher.
	if err := f.begin(2); err != nil {
		t.Fatalf("begin(2) after seal(100): %v", err)
	}
	if got := f.burned(); got != 100 {
		t.Errorf("begin(2) lowered burned() to %d, want it held at 100", got)
	}
	if got := f.ceiling(); got < 100 {
		t.Errorf("begin(2) left ceiling() at %d, below burned() 100: an index cannot be burned without having been authorised", got)
	}

	r, w := readFloorFile(t, dir)
	if r != f.ceiling() || w != f.burned() {
		t.Errorf("the file holds {reserved:%d written:%d} but memory holds {%d %d}: memory must never claim more than disk does",
			r, w, f.ceiling(), f.burned())
	}
	if w > r {
		t.Errorf("the file claims %d burned but only %d reserved", w, r)
	}
}

// TestWALIndexFloorBeginRefusesZero pins the one input begin rejects. start-1
// would underflow to MaxUint64 and burn the entire index space irrecoverably --
// the floor is never lowered, so there would be no way back.
func TestWALIndexFloorBeginRefusesZero(t *testing.T) {
	dir := t.TempDir()
	f := mustOpenFloor(t, dir)
	err := f.begin(0)
	if err == nil {
		t.Fatalf("begin(0) succeeded; the first WAL record index is 1, and start-1 would underflow to MaxUint64 and burn the whole index space")
	}
	if !strings.Contains(err.Error(), "the first WAL record index is 1") {
		t.Errorf("begin(0) = %q, want it to say why 0 is not a state to encode", err)
	}
	if _, err := os.Stat(filepath.Join(dir, IndexFloorFileName)); !os.IsNotExist(err) {
		t.Errorf("begin(0) wrote a floor file (%v); a refused call must persist nothing", err)
	}
}

// TestWALIndexFloorPersistRefusesToLower reaches past the callers to
// persistLocked itself.
//
// Every caller above it computes a maximum, so a decrease can only ever be a
// BUG -- and a bug that lowers this file is precisely the failure no downstream
// check can detect, because the result is a perfectly valid-looking log that
// quietly reissues ids. So the last point before the bytes are written refuses,
// and that refusal is tested directly rather than assumed from the callers.
func TestWALIndexFloorPersistRefusesToLower(t *testing.T) {
	cases := []struct {
		name             string
		reserved, writeN uint64
		wantMsg          string
	}{
		{
			name:     "a lower reserved",
			reserved: 300, writeN: 100,
			wantMsg: "refusing to lower the durable index floor",
		},
		{
			name:     "a lower written",
			reserved: 400, writeN: 99,
			wantMsg: "refusing to lower the durable index floor",
		},
		{
			// Both fields RISE, so the monotonicity check passes -- and the pair
			// is still refused, because it would claim indices were used that
			// were never authorised.
			name:     "written above reserved",
			reserved: 400, writeN: 500,
			wantMsg: "an index cannot be used before it is authorised",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			f := mustOpenFloor(t, dir)
			if err := f.begin(101); err != nil { // written 100, reserved 356
				t.Fatalf("begin(101): %v", err)
			}
			beforeR, beforeW := readFloorFile(t, dir)

			f.mu.Lock()
			err := f.persistLocked(tc.reserved, tc.writeN)
			f.mu.Unlock()
			if err == nil {
				t.Fatalf("persistLocked(%d, %d) succeeded from {reserved:%d written:%d}: the floor is monotonic by construction",
					tc.reserved, tc.writeN, beforeR, beforeW)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("persistLocked error = %q, want it to contain %q", err, tc.wantMsg)
			}
			if !strings.Contains(err.Error(), f.Path()) {
				t.Errorf("persistLocked error = %q, want it to name the file %s", err, f.Path())
			}

			// AND NOTHING WAS WRITTEN. A refusal that had already replaced the
			// file would have done the damage it was refusing to do.
			if r, w := readFloorFile(t, dir); r != beforeR || w != beforeW {
				t.Errorf("the file moved to {reserved:%d written:%d} despite the refusal, want {%d %d}", r, w, beforeR, beforeW)
			}
			if f.ceiling() != beforeR || f.burned() != beforeW {
				t.Errorf("memory moved to {ceiling:%d burned:%d} despite the refusal, want {%d %d}", f.ceiling(), f.burned(), beforeR, beforeW)
			}
		})
	}
}

// TestWALIndexFloorMissingFileIsBenign is the half of the missing/corrupt
// judgement that must NOT be fatal.
//
// A missing floor has a legitimate benign cause -- a data directory written by a
// binary that predates the file -- and making it fatal would BRICK EVERY
// DEPLOYED BUS ON UPGRADE. The fallback (derive the index from the log, as this
// package did before) is strictly no worse than the status quo it replaces, so
// refusing would buy nothing and cost everything.
func TestWALIndexFloorMissingFileIsBenign(t *testing.T) {
	dir := t.TempDir()
	f, err := openIndexFloor(dir)
	if err != nil {
		t.Fatalf("openIndexFloor on a directory with no floor file = %v, want no error: a missing floor is the MIGRATION case, and making it fatal bricks every deployed bus on upgrade", err)
	}
	if f.existedAtOpen() {
		t.Error("existedAtOpen() = true when no file exists")
	}
	if f.burned() != 0 || f.ceiling() != 0 {
		t.Errorf("a missing floor reports {burned:%d ceiling:%d}, want zero: the caller then falls back to the log's own arithmetic", f.burned(), f.ceiling())
	}
	// It must not have CREATED the file either: openIndexFloor reads, the first
	// begin writes.
	if _, serr := os.Stat(f.Path()); !os.IsNotExist(serr) {
		t.Errorf("openIndexFloor created %s (%v); it must write nothing", f.Path(), serr)
	}

	// An empty dir string is a programming error, not a missing file.
	if _, err := openIndexFloor(""); err == nil {
		t.Error("openIndexFloor(\"\") succeeded; an empty data directory is a bug upstream, not a benign absence")
	}
}

// TestWALIndexFloorCorruptFileIsFatalAndNamesTheRemedy is the other half, and
// the reasoning is the inverse of the one above.
//
// A crash can NEVER produce a corrupt floor: the write is temp file, fsync,
// rename, directory fsync, so a reader sees the whole old file or the whole new
// one. Corruption therefore means media damage or tampering, and there is no
// benign cause to be generous to. Regenerating the file would resume the record
// index -- and with it the message sequence internal/hub derives from it -- BELOW
// numbers already handed out, silently, with nothing downstream able to detect
// it.
//
// THE REMEDY IS PART OF THE CONTRACT and is asserted in every cell. Without a
// one-step way out, this error is a permanently bricked bus, which is exactly
// what invariant 6 forbids -- so "the message names the fix" is what makes the
// refusal legitimate rather than a policy regression.
func TestWALIndexFloorCorruptFileIsFatalAndNamesTheRemedy(t *testing.T) {
	goodBody := encodeFloorBody(512, 300)
	goodSum := sha256.Sum256([]byte(goodBody))
	goodHeader := indexFloorMagic + " v" + strconv.Itoa(indexFloorFileVersion) + " sha256=" + hex.EncodeToString(goodSum[:])

	cases := []struct {
		name string
		// header == "" means "compute the canonical header for body", so a
		// body-level case is not intercepted by the digest check.
		header  string
		body    string
		wantMsg string
	}{
		{
			name:    "no header line at all",
			header:  "",
			body:    "", // written as a single line with no newline; see below
			wantMsg: "no header line",
		},
		{
			name:    "the wrong magic",
			header:  "agent-bus-something-else v4 sha256=" + hex.EncodeToString(goodSum[:]),
			body:    goodBody,
			wantMsg: "does not start with a",
		},
		{
			name:    "an OLDER on-disk format version",
			header:  indexFloorMagic + " v3 sha256=" + hex.EncodeToString(goodSum[:]),
			body:    goodBody,
			wantMsg: "this binary understands only v4",
		},
		{
			// The dangerous direction: a newer binary may encode a HIGHER floor in
			// a shape this one cannot see, so reading the part it understands
			// would LOWER the floor -- the one thing the file exists to prevent.
			name:    "a NEWER on-disk format version",
			header:  indexFloorMagic + " v5 sha256=" + hex.EncodeToString(goodSum[:]),
			body:    goodBody,
			wantMsg: "may encode a higher floor this binary cannot see",
		},
		{
			name:    "no digest in the header",
			header:  indexFloorMagic + " v4 crc32=deadbeef",
			body:    goodBody,
			wantMsg: "has no sha256= digest",
		},
		{
			name:    "a short digest",
			header:  indexFloorMagic + " v4 sha256=abcd",
			body:    goodBody,
			wantMsg: "is not 32 hex bytes",
		},
		{
			name:    "a digest that is not hex",
			header:  indexFloorMagic + " v4 sha256=" + strings.Repeat("z", 64),
			body:    goodBody,
			wantMsg: "is not 32 hex bytes",
		},
		{
			name:    "a digest that does not match the body",
			header:  goodHeader,
			body:    encodeFloorBody(512, 301), // one digit different
			wantMsg: "fails its own checksum",
		},
		{
			name:    "a missing body line",
			body:    "reserved 512\n",
			wantMsg: "expected exactly 2",
		},
		{
			name:    "an extra body line",
			body:    encodeFloorBody(512, 300) + "extra 1\n",
			wantMsg: "expected exactly 2",
		},
		{
			name:    "the fields in the wrong order",
			body:    "written 300\nreserved 512\n",
			wantMsg: `expected a "reserved" line`,
		},
		{
			name:    "a leading zero",
			body:    "reserved 0512\nwritten 300\n",
			wantMsg: "has a leading zero",
		},
		{
			name:    "a signed number",
			body:    "reserved +512\nwritten 300\n",
			wantMsg: "must be decimal digits only",
		},
		{
			name:    "a negative number",
			body:    "reserved 512\nwritten -1\n",
			wantMsg: "must be decimal digits only",
		},
		{
			name:    "an empty field",
			body:    "reserved \nwritten 300\n",
			wantMsg: "is empty",
		},
		{
			name:    "a number that overflows 64 bits",
			body:    "reserved 18446744073709551616\nwritten 300\n",
			wantMsg: "is not a 64-bit decimal number",
		},
		{
			// The file contradicting itself: an index cannot have been USED before
			// it was AUTHORISED.
			name:    "written above reserved",
			body:    encodeFloorBody(300, 512),
			wantMsg: "contradicts itself",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, IndexFloorFileName)
			if tc.name == "no header line at all" {
				// A file with no newline anywhere: there is no header to read.
				if err := os.WriteFile(path, []byte("agent-bus-wal-index-floor v4 sha256=deadbeef"), 0o600); err != nil {
					t.Fatalf("writing the fixture: %v", err)
				}
			} else {
				writeFloorFile(t, dir, tc.header, tc.body)
			}
			before := readFile(t, path)

			f, err := openIndexFloor(dir)
			if err == nil {
				t.Fatalf("openIndexFloor succeeded on a corrupt floor (%+v); a floor that does not verify may be LOWER than the one persisted, and resuming from it reissues ids nothing downstream can detect", f)
			}
			if !errors.Is(err, ErrIndexFloorCorrupt) {
				t.Errorf("err = %v, want errors.Is(err, ErrIndexFloorCorrupt): callers distinguish corruption from an I/O failure", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %q, want it to contain %q so an operator can tell WHICH way the file is wrong", err, tc.wantMsg)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("err = %q, want it to name the file %s", err, path)
			}
			// THE ONE-STEP REMEDY. This is what keeps a refusal over a corrupt
			// IDENTITY file compatible with invariant 6: the bus is never
			// permanently bricked, because the error says exactly what to do.
			if !strings.Contains(err.Error(), "delete "+path+" and restart") {
				t.Errorf("err = %q, want it to name the one-step operator remedy (\"delete %s and restart\"); without it this error is a permanently bricked bus, which invariant 6 forbids", err, path)
			}
			if !strings.Contains(err.Error(), "It will NOT be regenerated") {
				t.Errorf("err = %q, want it to say the file is not regenerated automatically, and why", err)
			}

			// A REFUSAL CHANGES NOTHING ON DISK. Recovery that "helpfully"
			// rewrote a floor it could not read would have done the exact damage
			// it refused to do.
			if after := readFile(t, path); !bytes.Equal(before, after) {
				t.Errorf("the corrupt floor was modified by the failed open:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}

// TestWALIndexFloorCorruptFragmentIsClipped: the bytes in a corrupt file are
// arbitrary -- that is what "corrupt" means -- so a damaged or HOSTILE file must
// not be able to put a megabyte of anything into the operator's startup log,
// several times over once the errors wrap. %q escapes a control byte to four
// characters, so the multiplier is real.
func TestWALIndexFloorCorruptFragmentIsClipped(t *testing.T) {
	dir := t.TempDir()
	const floodLen = 64 << 10
	flood := strings.Repeat("A", floodLen)
	writeFloorFile(t, dir, flood+" v4 sha256=deadbeef", encodeFloorBody(1, 1))

	_, err := openIndexFloor(dir)
	if err == nil {
		t.Fatal("openIndexFloor succeeded on a floor file with a 64 KiB header line")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bytes total, truncated") {
		t.Errorf("err = %.200q..., want it to say the echoed fragment was truncated", msg)
	}
	if strings.Contains(msg, flood) {
		t.Error("the error echoed the whole 64 KiB header line: a hostile floor file must not be able to flood the operator log")
	}
	// Generous but finite: the bound is 128 characters of fragment plus the
	// fixed prose, and the prose is a few hundred characters.
	if len(msg) > 4<<10 {
		t.Errorf("the error is %d bytes long for a 64 KiB fragment; it must be clipped to a bounded size", len(msg))
	}
}

// TestWALIndexFloorIOFailureIsNotReportedAsCorruption keeps two operator actions
// apart. "I could not read the file" and "the file is not what was written" call
// for opposite responses, and conflating them would send someone to DELETE a
// file that is probably perfectly good -- which, for this file, means resuming
// the index below numbers already handed out.
func TestWALIndexFloorIOFailureIsNotReportedAsCorruption(t *testing.T) {
	t.Run("a directory where the file should be", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, IndexFloorFileName), 0o700); err != nil {
			t.Fatalf("creating a directory in the floor's place: %v", err)
		}
		_, err := openIndexFloor(dir)
		if err == nil {
			t.Fatal("openIndexFloor succeeded when the floor's path is a directory")
		}
		if errors.Is(err, ErrIndexFloorCorrupt) {
			t.Errorf("err = %v is reported as CORRUPTION; it is an I/O failure, and the remedy for the two is opposite: one says delete the file, the other says do not", err)
		}
		if !strings.Contains(err.Error(), "reading the durable index floor") {
			t.Errorf("err = %q, want it to say the read failed", err)
		}
	})

	t.Run("an unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: mode 000 does not deny this process, so there is no I/O failure to observe")
		}
		dir := t.TempDir()
		path := writeFloorFile(t, dir, "", encodeFloorBody(10, 5))
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatalf("chmod 000: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

		_, err := openIndexFloor(dir)
		if err == nil {
			t.Fatal("openIndexFloor succeeded on a mode-000 floor file")
		}
		if errors.Is(err, ErrIndexFloorCorrupt) {
			t.Errorf("err = %v is reported as CORRUPTION; permission denied is not damage, and telling an operator to delete a file they merely cannot read would destroy a good floor", err)
		}
	})
}

// TestWALIndexFloorReserveOnlyTouchesDiskAtTheBlockBoundary pins the amortised
// hot path from both sides.
//
// The WAL already fsyncs once per append. Reserving exactly one index per append
// would DOUBLE the fsyncs on the send path to buy nothing -- the property being
// protected (an index is never reissued) is equally protected by a block. So the
// common case must be a pure in-memory no-op, and the crossing must be a real,
// fsynced, atomic replacement.
//
// The detector is os.SameFile rather than a timestamp: atomicReplaceFile creates
// a NEW temp file and renames it over the old one, so a write always changes the
// identity of the file. That is immune to filesystem timestamp granularity,
// which a mtime comparison is not.
func TestWALIndexFloorReserveOnlyTouchesDiskAtTheBlockBoundary(t *testing.T) {
	dir := t.TempDir()
	f := mustOpenFloor(t, dir)
	if err := f.begin(1); err != nil {
		t.Fatalf("begin(1): %v", err)
	}
	path := f.Path()

	stat := func() os.FileInfo {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		return fi
	}

	// (1) EVERY index inside the reserved block is free. The boundaries are the
	// interesting values, so they are named rather than swept blindly.
	before := stat()
	beforeBytes := readFile(t, path)
	for _, n := range []uint64{1, 2, indexReserveBlock - 1, indexReserveBlock} {
		if err := f.reserve(n); err != nil {
			t.Fatalf("reserve(%d) inside the reserved block: %v", n, err)
		}
	}
	if after := stat(); !os.SameFile(before, after) {
		t.Errorf("reserve() replaced %s for an index at or below the ceiling %d: the hot path must not touch the disk, or the WAL pays a second fsync on every append",
			path, indexReserveBlock)
	}
	if after := readFile(t, path); !bytes.Equal(beforeBytes, after) {
		t.Errorf("the floor file changed for a reservation already covered:\nbefore %q\nafter  %q", beforeBytes, after)
	}

	// (2) The FIRST index past the ceiling does touch the disk, and the new
	// ceiling is durable before reserve returns -- which is the post-condition
	// Append relies on when it stamps that number into a frame.
	crossing := uint64(indexReserveBlock) + 1
	if err := f.reserve(crossing); err != nil {
		t.Fatalf("reserve(%d) crossing the ceiling: %v", crossing, err)
	}
	if after := stat(); os.SameFile(before, after) {
		t.Fatalf("reserve(%d) did NOT replace %s: the index was about to be stamped into a frame while nothing on stable storage had authorised it",
			crossing, path)
	}
	if got, want := f.ceiling(), crossing+indexReserveBlock-1; got != want {
		t.Errorf("after reserve(%d) ceiling() = %d, want %d (a whole block ahead again)", crossing, got, want)
	}
	// Durable, not merely in memory. reserve fsyncs before returning; this
	// asserts the observable half of that -- the bytes are readable by a fresh
	// reader. (The fsync itself is not observable from a test process; what is
	// observable, and is what a crash test then exercises, is that the file has
	// already been replaced by the time reserve returns.)
	if r, _ := readFloorFile(t, dir); r != f.ceiling() {
		t.Errorf("the file holds reserved=%d but memory holds %d: the ceiling must be on disk BEFORE the index is used", r, f.ceiling())
	}

	// (3) And the next 255 are free again.
	before = stat()
	if err := f.reserve(crossing + indexReserveBlock - 1); err != nil {
		t.Fatalf("reserve at the new ceiling: %v", err)
	}
	if after := stat(); !os.SameFile(before, after) {
		t.Error("reserve() touched the disk inside the NEW block: the amortisation is not working, so the block buys nothing")
	}
}

// TestWALIndexFloorSaturatesRatherThanWrapping: satAdd is the one piece of
// arithmetic in this file that cannot afford to be wrong. A wrap would silently
// RESET the ceiling to a tiny number, which is a lowered floor by another name.
func TestWALIndexFloorSaturatesRatherThanWrapping(t *testing.T) {
	const max = ^uint64(0)
	cases := []struct {
		a, b, want uint64
	}{
		{1, indexReserveBlock - 1, indexReserveBlock},
		{max - 1, 1, max},
		{max, 1, max},
		{max - 1, indexReserveBlock, max},
		{max / 2, max/2 + 4, max},
	}
	for _, tc := range cases {
		if got := satAdd(tc.a, tc.b); got != tc.want {
			t.Errorf("satAdd(%d, %d) = %d, want %d: a wrap here resets the ceiling to a tiny number, which is a lowered floor by another name",
				tc.a, tc.b, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// NO FALSE ALARMS.
//
// These matter exactly as much as the safety properties. A loss channel that
// cries wolf is the MIRROR IMAGE of the silent-discard defect: an operator who
// sees "records 1..756 are missing" on every single start learns to ignore the
// line, and then misses the one that is real.
// ---------------------------------------------------------------------------

// TestWALIndexFloorCleanCycleLeavesNoHole is the baseline nobody may break: a
// clean write/close/reopen reports NO loss and NO gap. If the ceiling branch
// fired on an ordinary restart, this is where it would show up -- and it would
// burn up to a block on every single start, for ever, for nothing.
func TestWALIndexFloorCleanCycleLeavesNoHole(t *testing.T) {
	dir := t.TempDir()

	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const txns = 5
	var handed []uint64
	for i := 0; i < txns; i++ {
		c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		handed = append(handed, c.PrepareIndex, c.CommitIndex)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var buf bytes.Buffer
	l2, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rec := l2.Recovered()

	if rec.MissingRecords != 0 {
		t.Errorf("MissingRecords = %d after a clean cycle, want 0: %+v", rec.MissingRecords, rec.Discarded)
	}
	if rec.DiscardCount != 0 || rec.Repaired.DiscardCount != 0 {
		t.Errorf("a clean cycle discarded something: replay %d, framing %d", rec.DiscardCount, rec.Repaired.DiscardCount)
	}
	if rec.FirstIndex != 1 {
		t.Errorf("FirstIndex = %d, want 1: this log genuinely starts at 1", rec.FirstIndex)
	}
	if want := uint64(2*txns) + 1; rec.NextIndex != want {
		t.Errorf("NextIndex = %d, want %d: an ordinary restart must resume at the log's own high-water mark. RESERVING an index is not ISSUING it, so the block reserved ahead must NOT be burned on a clean start",
			rec.NextIndex, want)
	}
	// The indices in the file are contiguous, 1..2*txns.
	got := scanIndices(t, filepath.Join(dir, WALFileName), KindWAL)
	for i, idx := range got {
		if idx != uint64(i+1) {
			t.Fatalf("the clean log holds indices %v, want a contiguous 1..%d: nothing here was discarded, so there is nothing to step over", got, 2*txns)
		}
	}
	// Nothing was skipped, so the loud skip line must NOT appear. A warning on
	// every clean start is noise that trains an operator to ignore the real one.
	assertNotLogged(t, buf.String(), "wal resumed the record index above what the log file alone would have given")
	assertNotLogged(t, buf.String(), "wal resumed the record index above the durable floor after a quarantine")

	c, err := l2.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"reopen"}`)})
	if err != nil {
		t.Fatalf("Write after reopen: %v", err)
	}
	if c.PrepareIndex != uint64(2*txns)+1 {
		t.Errorf("the write after a clean reopen got prepare index %d, want %d", c.PrepareIndex, 2*txns+1)
	}
	for _, seen := range handed {
		if c.PrepareIndex == seen || c.CommitIndex == seen {
			t.Fatalf("index %d was handed out twice across a clean restart", seen)
		}
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWALIndexFloorLeadingGapIsNotReportedAsLoss is the regression guard for a
// bug the fix would otherwise have INTRODUCED, which is why it is explicit.
//
// A WAL no longer necessarily begins at index 1: a fresh log started after a
// quarantine begins above the durable floor. Replay's old expectation started at
// 1, so such a log reported "records 1..756 are missing from the index sequence"
// on EVERY START, FOR EVER -- hundreds of records that were never in that file at
// all. Where the file STARTS is not a hole, and it is reported separately as
// FirstIndex.
func TestWALIndexFloorLeadingGapIsNotReportedAsLoss(t *testing.T) {
	const first = 757

	path := buildWALIndexed(t, []indexedOp{
		{index: first, typ: TypePrepare, kind: "message", body: `{"n":1}`},
		{index: first + 1, typ: TypeCommit, prepareIndex: first},
		{index: first + 2, typ: TypePrepare, kind: "message", body: `{"n":2}`},
		{index: first + 3, typ: TypeCommit, prepareIndex: first + 2},
	})
	dir := filepath.Dir(path)

	// (1) Replay alone, which is the read-only fsck view.
	var c collector
	r, err := Replay(path, c.fn)
	if err != nil {
		t.Fatalf("Replay of a log starting at index %d: %v", first, err)
	}
	if r.MissingRecords != 0 {
		t.Errorf("MissingRecords = %d for a log whose FIRST record is index %d, want 0: the %d indices below it were never in this file, and claiming them as lost on every start turns the loss channel into noise. Discards: %+v",
			r.MissingRecords, first, first-1, r.Discarded)
	}
	if r.FirstIndex != first {
		t.Errorf("FirstIndex = %d, want %d: where the file STARTS must be reportable separately from where a loss ENDS", r.FirstIndex, first)
	}
	if r.NextIndex != first+4 {
		t.Errorf("NextIndex = %d, want %d", r.NextIndex, first+4)
	}
	if r.DiscardCount != 0 {
		t.Errorf("Replay discarded %d records from a perfectly good log: %+v", r.DiscardCount, r.Discarded)
	}

	// (2) And through Open, which is where an operator would actually see it.
	var buf bytes.Buffer
	l, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	if got := l.Recovered().MissingRecords; got != 0 {
		t.Errorf("Open reports MissingRecords = %d for a log starting at %d, want 0", got, first)
	}
	if got := l.Recovered().FirstIndex; got != first {
		t.Errorf("Open reports FirstIndex = %d, want %d", got, first)
	}
	if !strings.Contains(buf.String(), "first_index="+strconv.Itoa(first)) {
		t.Errorf("the replay log line does not carry first_index=%d, so an operator cannot tell where the file starts:\n%s", first, buf.String())
	}

	// An INTERIOR hole is still reported: this test must not have bought its
	// silence by switching the whole check off.
	interior := buildWALIndexed(t, []indexedOp{
		{index: first, typ: TypePrepare, kind: "message", body: `{"n":1}`},
		{index: first + 1, typ: TypeCommit, prepareIndex: first},
		{index: first + 9, typ: TypePrepare, kind: "message", body: `{"n":2}`}, // a 7-index hole
		{index: first + 10, typ: TypeCommit, prepareIndex: first + 9},
	})
	var c2 collector
	r2, err := Replay(interior, c2.fn)
	if err != nil {
		t.Fatalf("Replay of a log with an interior hole: %v", err)
	}
	if r2.MissingRecords != 7 {
		t.Errorf("MissingRecords = %d for a 7-index interior hole, want 7: suppressing the LEADING gap must not suppress a real one -- silence here is what let a lost sector look like a clean boot",
			r2.MissingRecords)
	}
}

// ---------------------------------------------------------------------------
// THE ATTACKER BOUND, and the MIGRATION window.
// ---------------------------------------------------------------------------

// TestWALIndexFloorRejectsAnImplausibleForgedIndex is a security property, not a
// correctness one.
//
// The index in a DAMAGED frame's header is read out of bytes whose MAC DID NOT
// VERIFY. On a WAL the payload is client-supplied, so those bytes are
// attacker-influenced: if recovery believed the declared index unconditionally,
// one forged header claiming index 2^62 would push the durable ceiling to near
// MaxUint64 and PERMANENTLY EXHAUST this bus's id space -- a denial of service
// that survives every restart, precisely because the floor is never lowered.
//
// The bound only ever makes recovery LESS aggressive, which is safe because it
// is not the last line of defence: LostUnidentified still sends Open to the
// durable ceiling for whatever this declines to believe.
func TestWALIndexFloorRejectsAnImplausibleForgedIndex(t *testing.T) {
	const forged = uint64(1) << 62

	dir, path, _, _ := buildWAL(t,
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
	)
	// A frame whose header parses -- correct length, reserved zero -- and whose
	// index field claims 2^62. Flipping a payload byte afterwards is what makes it
	// DAMAGED, which is the only state in which an unverified header is read at
	// all.
	appendRawFrame(t, path, forged, TypePrepare, []byte(`{"forged":true}`))
	flipByte(t, path, fileSize(t, path)-1)

	res, out, err := captureRepair(t, path, KindWAL)
	if err != nil {
		t.Fatalf("RepairLog: %v: damage is never fatal", err)
	}
	if res.NextIndex != 3 {
		t.Fatalf("Repair.NextIndex = %d, want 3: the forged index %d came from a header whose MAC did not verify and must NOT be used to advance the index sequence",
			res.NextIndex, forged)
	}
	if !res.LostUnidentified {
		t.Error("Repair.LostUnidentified = false: refusing to believe the declared index means the file's arithmetic is a lower bound, so Open must still be sent to the durable ceiling")
	}
	// THE REJECTION IS RECORDED, not silent. An operator who later finds a hole
	// needs to be able to see that recovery declined to trust a number.
	if len(res.Discards) != 1 {
		t.Fatalf("Repair.Discards = %+v, want exactly one", res.Discards)
	}
	reason := res.Discards[0].Reason
	if !strings.Contains(reason, "not plausible for this file") {
		t.Errorf("the discard reason %q does not say the declared index was implausible", reason)
	}
	if !strings.Contains(reason, "NOT used to advance the index sequence") {
		t.Errorf("the discard reason %q does not say the declared index was refused", reason)
	}
	if !strings.Contains(reason, strconv.FormatUint(forged, 10)) {
		t.Errorf("the discard reason %q does not name the index it rejected (%d)", reason, forged)
	}
	// WARN, not ERROR: the discarded record declares itself a PREPARE, and a
	// prepare acknowledged nothing to anybody. The level tracks what was lost,
	// not how surprising the index was.
	assertLogged(t, out, "WARN", "wal discarded a damaged record", "not plausible for this file")

	// AND THE ID SPACE SURVIVES. This is the assertion the whole test is for: the
	// bus starts, and its durable ceiling is a small number rather than one that
	// has burned 2^62 indices for ever.
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open after the forged frame: %v", err)
	}
	defer l.Close()
	if got := l.Recovered().NextIndex; got != 3 {
		t.Fatalf("Recovered().NextIndex = %d, want 3: one forged frame header must not be able to move the index space", got)
	}
	reserved, written := readFloorFile(t, dir)
	if reserved > uint64(indexReserveBlock)+3 {
		t.Errorf("the durable ceiling is %d after one forged frame claiming index %d: a single unverified header has permanently exhausted the id space, which is a restart-proof denial of service",
			reserved, forged)
	}
	if written >= forged {
		t.Errorf("the durable burned mark is %d, at or above the forged index %d", written, forged)
	}
}

// TestWALIndexFloorMigratesAnExistingDataDirectory is the upgrade path: a data
// directory holding a HEALTHY log and no floor file at all.
//
// It must open, resume at the log's own high-water mark (which is correct --
// nothing has been discarded, so there is nothing to step over), create the file,
// and SAY SO. The warning is the point: until that file existed a quarantine
// could still have reissued ids, and an operator is owed the fact that the
// directory has just been migrated.
func TestWALIndexFloorMigratesAnExistingDataDirectory(t *testing.T) {
	// Six records laid down through OpenWriter, which -- deliberately -- attaches
	// no floor. That is exactly the shape a pre-2026-08-07 binary leaves behind.
	dir, path, nextIndex, _ := buildWAL(t,
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opPrepare("message", `{"n":2}`), // 3
		opCommit(3),                     // 4
		opPrepare("message", `{"n":3}`), // 5
		opCommit(5),                     // 6
	)
	if nextIndex != 7 {
		t.Fatalf("the fixture ends at next index %d, want 7", nextIndex)
	}
	floorPath := filepath.Join(dir, IndexFloorFileName)
	if _, err := os.Stat(floorPath); !os.IsNotExist(err) {
		t.Fatalf("the fixture already has a floor file (%v); this test is about the directory that does NOT", err)
	}

	var buf bytes.Buffer
	app := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: app, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open on a data directory with no floor file: %v, want a normal start: a missing floor is BENIGN, and making it fatal would brick every deployed bus on upgrade", err)
	}
	out := buf.String()

	// (1) It resumed at the log's own high-water mark. Nothing was lost, so
	// nothing may be burned: a migration that jumped would put a permanent,
	// pointless hole in every upgraded bus's log.
	if got := l.Recovered().NextIndex; got != 7 {
		t.Errorf("Recovered().NextIndex = %d, want 7: a healthy migrated log has nothing to step over", got)
	}
	if app.count() != 3 {
		t.Errorf("Open applied %d entries, want 3: migration must not cost history", app.count())
	}

	// (2) The file now exists, is 0600, and says what the run just did.
	fi, err := os.Stat(floorPath)
	if err != nil {
		t.Fatalf("the floor file was not created at %s: %v", floorPath, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the floor file is mode %v, want 0600: it sits next to the MAC key and carries the directory's identity", perm)
	}
	if got := l.IndexFloorPath(); got != floorPath {
		t.Errorf("Log.IndexFloorPath() = %q, want %q", got, floorPath)
	}
	reserved, written := readFloorFile(t, dir)
	if written != 6 {
		t.Errorf("the migrated floor claims %d indices burned, want 6 (start-1): recording the start is what stops a run that jumped and then wrote nothing from having the jump forgotten", written)
	}
	if want := uint64(7 + indexReserveBlock - 1); reserved != want {
		t.Errorf("the migrated floor reserved %d, want %d (a block ahead of the start)", reserved, want)
	}

	// (3) It is NOT announced as a SKIP: nothing was skipped, and a warning on a
	// clean migration would train an operator to ignore the real one.
	assertNotLogged(t, out, "wal resumed the record index above what the log file alone would have given")
	assertNotLogged(t, out, "wal resumed the record index above the durable floor after a quarantine")

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// (4) A SECOND start is an ordinary one, and still resumes at 7: a clean
	// restart must not burn the reserved block.
	l2, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer l2.Close()
	if got := l2.Recovered().NextIndex; got != 7 {
		t.Errorf("the second start resumed at %d, want 7: RESERVING an index is not ISSUING it, so a clean restart must not step over the reserved block", got)
	}
	_ = path
}

// TestWALIndexFloorAnnouncesTheMigrationWindow is separated from the migration
// test above because IT CURRENTLY FAILS, and it is failing on the
// IMPLEMENTATION rather than on the expectation.
//
// THE CLAIM. A data directory that already holds WAL records but no floor file
// was written by a binary that predates the file. Until that file exists, a
// quarantine or an unidentifiable discard could still have reissued record
// indices -- and the message ids internal/hub derives from them. That window is
// real, it closes silently, and an operator is owed the fact that their data
// directory has just been migrated. log.go says exactly this in the comment
// above the branch, so the intent is not in doubt.
//
// THE DEFECT, at internal/wal/log.go:485:
//
//	if err := floor.begin(start); err != nil { ... }   // line 458
//	...
//	if !floor.existedAtOpen() && (rec.Records > 0 || ...) {   // line 485
//
// floor.begin persists, and indexfloor.go's persistLocked sets f.existed = true
// (line 352). So by the time the branch is evaluated, existedAtOpen() is ALWAYS
// true and the warning is UNREACHABLE ON EVERY PATH. It is dead code.
//
// THE FIX IS ONE LINE and belongs to the implementer, not to this test: capture
// the flag BEFORE the begin --
//
//	migrating := !floor.existedAtOpen()
//
// -- and test `migrating` at line 485. This test is deliberately left RED rather
// than rewritten to assert the silence, because asserting the silence would
// enshrine the defect exactly the way the old reissue tests enshrined e120153b.
func TestWALIndexFloorAnnouncesTheMigrationWindow(t *testing.T) {
	dir, _, _, _ := buildWAL(t,
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
	)
	floorPath := filepath.Join(dir, IndexFloorFileName)
	if _, err := os.Stat(floorPath); !os.IsNotExist(err) {
		t.Fatalf("the fixture already has a floor file (%v)", err)
	}

	var buf bytes.Buffer
	l, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	assertLogged(t, buf.String(), "WARN", "wal created the durable record index floor for an existing data directory",
		"path="+floorPath, "start_index=3")
}

// TestWALIndexFloorSurvivesAQuarantine is db350e39 at the level a server sees
// it, WITHOUT a subprocess: the whole-log quarantine that used to reset the
// index to 1 and reissue the bus's entire history.
//
// The crash-injected version of this claim lives in indexfloor_crash_test.go;
// this one is the fast, deterministic check that runs in milliseconds.
func TestWALIndexFloorSurvivesAQuarantine(t *testing.T) {
	dir := t.TempDir()

	// A real run through the real write path, so the indices below are indices a
	// client was actually given.
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	handed := map[uint64]bool{}
	var highest uint64
	for i := 0; i < 4; i++ {
		c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		handed[c.PrepareIndex], handed[c.CommitIndex] = true, true
		highest = c.CommitIndex
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(dir, WALFileName)

	// Nine bytes: too short even for the file header, so the layout cannot be
	// established and not one record can be salvaged. This is the quarantine
	// path, and it is the one where the FILE's own answer to "what index comes
	// next" is 1.
	truncate(t, path, 9)

	var buf bytes.Buffer
	l2, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open after a quarantine: %v, want a started server: invariant 6 is intact, there is no refuse-to-start behaviour here and none is wanted", err)
	}
	defer l2.Close()
	out := buf.String()
	rec := l2.Recovered()

	// (1) THE QUARANTINE ACTUALLY HAPPENED. Without this the test could pass
	// vacuously by never reaching the path it is about.
	if rec.Repaired.Quarantined == "" {
		t.Fatalf("Recovered().Repaired.Quarantined is empty: this test proves nothing unless the log was really moved aside. Repair: %+v", rec.Repaired)
	}
	if _, err := os.Stat(rec.Repaired.Quarantined); err != nil {
		t.Errorf("the quarantined log is not at %s (%v): it is RENAMED, never deleted -- a file this code cannot read is not necessarily one nobody can read",
			rec.Repaired.Quarantined, err)
	}

	// (2) AND THE INDEX DID NOT REWIND. This is defect db350e39: the file that
	// replaces the quarantined one is empty, so its own answer is 1, and taking
	// it reissued the entire history.
	if rec.NextIndex <= highest {
		t.Fatalf("Recovered().NextIndex = %d after a quarantine, but index %d had already been handed to a client: a quarantine must not reissue the bus's history (defect db350e39). The floor lives OUTSIDE the log precisely so a quarantine cannot take it.",
			rec.NextIndex, highest)
	}
	if want := uint64(indexReserveBlock) + 1; rec.NextIndex != want {
		t.Errorf("Recovered().NextIndex = %d, want %d: one past the durable CEILING -- everything this data directory ever authorised", rec.NextIndex, want)
	}

	c, err := l2.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":"quarantine"}`)})
	if err != nil {
		t.Fatalf("Write after a quarantine: %v", err)
	}
	if handed[c.PrepareIndex] || handed[c.CommitIndex] {
		t.Fatalf("the write after a quarantine got {prepare:%d commit:%d}, and one of those was already handed out before the quarantine", c.PrepareIndex, c.CommitIndex)
	}

	// (3) IT WAS LOUD. Skipping index space silently is the same failure as
	// discarding silently, applied to the id space instead of the message space.
	assertLogged(t, out, "ERROR", "wal quarantined an unreadable log and started a fresh one", "moved_to=")
	assertLogged(t, out, "ERROR", "wal resumed the record index above the durable floor after a quarantine",
		"indices_skipped=", "index_floor="+filepath.Join(dir, IndexFloorFileName))
}
