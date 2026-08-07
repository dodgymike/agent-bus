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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// AUTHENTICATING THE DURABLE INDEX FLOOR, and surviving the shapes that shipped
// before it was authenticated.
//
// A security gate returned the index-floor work CHANGES-REQUESTED with two P1s.
// Both were PROVED EXECUTABLY, both are regression-tested here, and both were
// re-observed RED against the code as it stood before this file existed.
//
//	P1-1  The v4 body went from two lines to three WITHOUT a version bump, and a
//	      two-line body was declared CORRUPT on the premise that "v4 never
//	      shipped". THE PREMISE WAS FALSE -- commit f56c723 is in main and writes
//	      exactly a two-line v4 body -- so a routine upgrade from main hit
//	      ErrIndexFloorCorrupt and the bus refused to start. Worse, the error's
//	      own printed remedy ("delete the floor and restart ... which is correct
//	      unless the log has ALSO been damaged") then reissued indices: measured
//	      at 2268 of 2289 truncation offsets. The remedy asked the operator to
//	      certify something nobody can know, because a cut at a clean frame
//	      boundary is byte-for-byte a shorter log.
//
//	P1-2  The floor was protected by an UNKEYED sha256. Flipping `sealed 0` to
//	      `sealed 1` and recomputing that digest BY HAND -- reading no key at all,
//	      touching bus.wal not at all -- made the reopened bus reissue indices at
//	      2268 of 2289 truncation offsets. Every frame it then wrote carried a
//	      VALID MAC, because the server itself computes it, so the corruption was
//	      invisible downstream. CLAUDE.md invariant 6 is explicit that integrity
//	      here is a keyed MAC and never an unkeyed checksum; a CRC was removed
//	      from this codebase once already for the same reason.
//
// Every directory here is a t.TempDir(). The tracked ./data directory is never
// touched.
// ---------------------------------------------------------------------------

// mainFormatFloorBody renders the TWO-LINE v4 body that commit f56c723 -- which
// is in main -- writes. It is spelled out rather than generated so that a change
// to this package's encoder cannot silently change what "what main writes" means.
func mainFormatFloorBody(reserved, written uint64) string {
	return fmt.Sprintf("reserved %d\nwritten %d\n", reserved, written)
}

// writeMainFormatFloor overwrites dir's floor with exactly the bytes f56c723
// would have produced: a two-line body under an unkeyed sha256 header.
func writeMainFormatFloor(t *testing.T, dir string, reserved, written uint64) {
	t.Helper()
	writeLegacyFloorFile(t, dir, mainFormatFloorBody(reserved, written))
}

// firstIndexIssued reopens dir and reports the index a REAL Write is handed.
//
// It deliberately does NOT read Recovered().NextIndex. That field is a CLAIM
// about what will happen next; the index stamped into a frame is what actually
// happened, and a defect that made the two disagree would be invisible to a test
// that only asked the claim. The security gate's independent probe measured it
// this way for the same reason, and the two agreed.
func firstIndexIssued(t *testing.T, dir string) uint64 {
	t.Helper()
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	defer l.Close()
	c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"probe":true}`)})
	if err != nil {
		t.Fatalf("Write after reopen: %v", err)
	}
	return c.PrepareIndex
}

// TestWALIndexFloorAcceptsTheBodyShippedInMain is P1-1(a): THE UPGRADE PATH.
//
// This is not a hypothetical compatibility gesture. f56c723 is IN MAIN, and
// every data directory a bus built from main has written carries a two-line v4
// body. Declaring that shape corrupt turns a routine binary upgrade into a bus
// that will not start, and hands the operator a remedy that forfeits invariant 1.
func TestWALIndexFloorAcceptsTheBodyShippedInMain(t *testing.T) {
	dir := t.TempDir()

	// A real run, closed cleanly, so the floor holds real numbers.
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var highest uint64
	for i := 0; i < 6; i++ {
		c, werr := l.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
		if werr != nil {
			t.Fatalf("Write %d: %v", i, werr)
		}
		highest = c.CommitIndex
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reserved, written, _ := readFloorFile(t, dir)
	writeMainFormatFloor(t, dir, reserved, written)

	got := firstIndexIssued(t, dir)
	if got <= highest {
		t.Fatalf("after upgrading a data directory written by main, the first index issued was %d but %d had already been handed out (invariant 1)", got, highest)
	}

	// AND THE FILE IS UPGRADED IN PLACE, not merely tolerated for ever. The
	// unkeyed digest is a liability, so a directory that starts once must come
	// out the other side authenticated.
	raw := string(readFile(t, filepath.Join(dir, IndexFloorFileName)))
	if !strings.Contains(raw, indexFloorTagHMAC) {
		t.Errorf("the floor still carries an unkeyed digest after a successful start; it must be rewritten with a keyed tag:\n%s", raw)
	}
	if _, _, _, err := readIndexFloorFileForTest(t, dir); err != nil {
		t.Errorf("the rewritten floor does not verify: %v", err)
	}
}

// readIndexFloorFileForTest reads dir's floor under dir's real key, returning
// the three values and any error, without failing the test itself.
func readIndexFloorFileForTest(t *testing.T, dir string) (reserved, written uint64, sealed bool, err error) {
	t.Helper()
	ff, err := readIndexFloorFile(filepath.Join(dir, IndexFloorFileName), floorKey(t, dir), true)
	return ff.reserved, ff.written, ff.sealed, err
}

// TestWALIndexFloorLegacyDigestNeverCarriesASeal is the security half of P1-1(a)
// and the reason accepting the old shape is safe.
//
// The old shape is ACCEPTED, but it is not BELIEVED about the one field that is
// a trust decision. `sealed` means "a run reached Writer.Close, so `written` is
// EXACT" -- and an unkeyed digest cannot support that claim, because anyone who
// can write the file can recompute it. So a legacy file's seal is discarded and
// the start takes the durable ceiling instead. That costs at most one burned
// reservation block, which is a legal hole; trusting it costs invariant 1.
func TestWALIndexFloorLegacyDigestNeverCarriesASeal(t *testing.T) {
	dir := t.TempDir()
	floorKey(t, dir) // the directory must have a key, as a real one does

	// A legacy THREE-line body claiming a clean close.
	writeLegacyFloorFile(t, dir, encodeFloorBody(4096, 4000, true))

	ff, err := readIndexFloorFile(filepath.Join(dir, IndexFloorFileName), floorKey(t, dir), true)
	if err != nil {
		t.Fatalf("reading a legacy floor: %v: the shape main writes must not be fatal", err)
	}
	if !ff.legacy {
		t.Error("a floor carrying an unkeyed sha256 digest was not flagged as legacy, so nothing would warn the operator or discard its seal")
	}
	if ff.sealed {
		t.Fatal("a legacy floor's `sealed 1` was BELIEVED. It is forgeable with no key at all -- flip the bit, recompute the digest -- and believing it makes the next start resume from the log's own mark, reissuing indices at almost every truncation offset (P1-2)")
	}
	if ff.reserved != 4096 || ff.written != 4000 {
		t.Errorf("legacy floor read as {reserved:%d written:%d}, want {4096 4000}: the NUMBERS are still used, because they are only ever consumed as a raise", ff.reserved, ff.written)
	}
}

// TestWALIndexFloorForgedSealIsNotBelieved is P1-2, as a sweep.
//
// THE ATTACK NEEDS NO KEY AND DOES NOT TOUCH THE LOG. Flip `sealed 0` to
// `sealed 1` in the floor file, recompute the unkeyed sha256 over the body, and
// every truncation of bus.wal at a clean frame boundary then resumes from the
// log's own high-water mark. Measured against the pre-fix code: 2268 of 2289
// offsets reissued an index, with nothing refusing to open.
//
// It is a sweep rather than a hand-picked offset for the same reason
// TestWALIndexFloorP0BTruncationSweepNeverReissues is: the offsets that reissue
// are exactly the frame boundaries, about one percent of the file, and a chosen
// offset would almost certainly miss them.
func TestWALIndexFloorForgedSealIsNotBelieved(t *testing.T) {
	const messages = 12

	src := t.TempDir()
	_, highest := crashedFixture(t, src, messages)
	size := fileSize(t, filepath.Join(src, WALFileName))
	if size < 1000 {
		t.Fatalf("the fixture log is only %d bytes; the sweep needs many frame boundaries to be worth running", size)
	}

	snap := snapshotDataDir(t, src)
	raw := string(snap[IndexFloorFileName])
	if !strings.Contains(raw, "sealed 0") {
		t.Fatalf("the crashed fixture's floor is not `sealed 0`, so there is nothing to forge:\n%s", raw)
	}
	// THE FORGERY, performed exactly as an attacker with directory write access
	// and no key would: rewrite the bit, re-hash the body unkeyed, restore the
	// legacy header spelling.
	nl := strings.IndexByte(raw, '\n')
	body := strings.Replace(raw[nl+1:], "sealed 0", "sealed 1", 1)
	sum := sha256.Sum256([]byte(body))
	snap[IndexFloorFileName] = []byte(indexFloorMagic + " v" + strconv.Itoa(indexFloorFileVersion) +
		" sha256=" + hex.EncodeToString(sum[:]) + "\n" + body)

	reissued, clean, opened := sweepTruncations(t, snap, size, highest)
	if len(reissued) != 0 {
		t.Fatalf("a floor FORGED WITH NO MAC KEY (sealed 0 -> sealed 1, unkeyed digest recomputed) made the bus reissue an already-issued index at %d of %d truncation offsets: %v (first few).\nForging the seal reissues MESSAGE IDS with no log tampering whatsoever, and every frame written afterwards carries a valid MAC because the SERVER computes it, so nothing downstream can see it",
			len(reissued), int(size)+1, reissued[:minInt(len(reissued), 16)])
	}
	// Non-vacuity, both ways: the sweep must have reached the clean-frame-boundary
	// class (the only one that ever reissued), and it must actually have STARTED
	// buses rather than refusing everything.
	if clean < 10 {
		t.Fatalf("only %d of %d offsets left a log with no detectable damage; the dangerous class is not being reached and this sweep proves nothing", clean, int(size)+1)
	}
	if opened < int(size)/2 {
		t.Fatalf("only %d of %d offsets reached a running server; recovery must ALWAYS start (invariant 6), and a sweep that mostly refuses is not testing what it claims", opened, int(size)+1)
	}
}

// sweepTruncations restores snap into a fresh directory for every truncation
// offset of bus.wal from 0 to size, opens it, and reports which offsets issued
// an index at or below highest.
//
// Workers never touch testing.T -- t.Fatalf outside the test goroutine is
// undefined -- so every outcome comes back as data.
func sweepTruncations(t *testing.T, snap map[string][]byte, size int64, highest uint64) (reissued []int, cleanOffsets, opened int) {
	t.Helper()

	type outcome struct {
		next   uint64
		clean  bool
		opened bool
		err    string
	}
	offsets := int(size) + 1
	results := make([]outcome, offsets)
	base := t.TempDir()

	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}
	next := int64(-1)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				off := atomic.AddInt64(&next, 1)
				if off >= int64(offsets) {
					return
				}
				dir := filepath.Join(base, strconv.FormatInt(off, 10))
				if err := restoreDataDir(snap, dir); err != nil {
					results[off] = outcome{err: "restoring the fixture: " + err.Error()}
					continue
				}
				if err := os.Truncate(filepath.Join(dir, WALFileName), off); err != nil {
					results[off] = outcome{err: "truncating: " + err.Error()}
					continue
				}
				l, err := Open(LogOptions{Dir: dir})
				if err != nil {
					// A REFUSAL IS NOT A REISSUE. It is recorded rather than
					// treated as a failure so that a change which trades
					// availability for safety is visible in the `opened` count
					// instead of silently passing the reissue check.
					results[off] = outcome{}
					_ = os.RemoveAll(dir)
					continue
				}
				rec := l.Recovered()
				res := outcome{
					next:   rec.NextIndex,
					clean:  rec.Repaired.DiscardCount == 0 && rec.Repaired.Quarantined == "",
					opened: true,
				}
				if cerr := l.Close(); cerr != nil {
					res.err = "Close: " + cerr.Error()
				}
				results[off] = res
				_ = os.RemoveAll(dir)
			}
		}()
	}
	wg.Wait()

	for off, r := range results {
		if r.err != "" {
			t.Fatalf("offset %d: %s", off, r.err)
		}
		if !r.opened {
			continue
		}
		opened++
		if r.clean {
			cleanOffsets++
		}
		if r.next <= highest {
			reissued = append(reissued, off)
		}
	}
	return reissued, cleanOffsets, opened
}

// TestWALIndexFloorTamperedUnderItsOwnKeyIsFatal is the property the HMAC buys:
// with the directory's own key in hand, an edited body cannot be made to verify.
//
// Under the unkeyed digest this was a two-line attack. Under the HMAC it is not
// an attack at all without the key -- and the key is mode 0600 and is the same
// one every WAL frame is authenticated with.
func TestWALIndexFloorTamperedUnderItsOwnKeyIsFatal(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":1}`)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(dir, IndexFloorFileName)
	before := readFile(t, path)
	tampered := bytes.Replace(before, []byte("sealed 1"), []byte("sealed 0"), 1)
	if bytes.Equal(tampered, before) {
		t.Fatalf("a cleanly closed floor is not `sealed 1`:\n%s", before)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("writing the tampered floor: %v", err)
	}

	_, err = Open(LogOptions{Dir: dir})
	if err == nil {
		t.Fatal("Open accepted a floor whose body was edited under its own HMAC tag; the tag is the only thing standing between a hostile data directory and a silently reissued id space")
	}
	if !errors.Is(err, ErrIndexFloorCorrupt) {
		t.Errorf("err = %v, want errors.Is(err, ErrIndexFloorCorrupt)", err)
	}
	if !strings.Contains(err.Error(), "HMAC-SHA256") {
		t.Errorf("err = %q, want it to say the file failed its HMAC, so an operator knows this is not ordinary media damage", err)
	}
}

// TestWALIndexFloorRemedyDoesNotUnderstateItsCost is P1-1(b), and it is a test
// about ENGLISH because the defect was in English.
//
// The old remedy said deleting the floor "is correct unless the log has ALSO
// been damaged or quarantined". That caveat is UNSOUND, not merely narrow: a
// truncation at a clean frame boundary is byte-indistinguishable from a
// legitimately shorter log, so "has the log been damaged" is not a question
// recovery or an operator can answer. Following it after a crash reissued an
// index at 2268 of 2289 truncation offsets.
func TestWALIndexFloorRemedyDoesNotUnderstateItsCost(t *testing.T) {
	msg := indexFloorCorrupt("/srv/bus/wal-index-floor", "it fails its own checksum").Error()

	for _, unsound := range []string{
		"which is correct unless",
		"correct unless the log has ALSO been damaged",
	} {
		if strings.Contains(msg, unsound) {
			t.Errorf("the remedy still contains %q. Deleting the floor forfeits invariant 1 whenever the previous run did NOT close cleanly, and nobody can tell a clean-boundary truncation from a shorter log, so this caveat asks the operator to certify something unknowable.\nfull text: %s", unsound, msg)
		}
	}
	for _, want := range []string{
		"FORFEITS INVARIANT 1",
		"unless the previous run shut down CLEANLY",
		"byte-for-byte identical to a log that was simply shorter",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the remedy does not contain %q; an error must name the remedy AND, when the remedy costs an invariant, name the cost.\nfull text: %s", want, msg)
		}
	}
}

// TestWALIndexFloorSurvivesALostMACKey is the case the keyed tag creates and
// therefore has to answer: the key is gone, recovery mints a new one, and
// NOTHING the previous identity wrote can verify -- floor included.
//
// Refusing here would brick a bus over a lost key in a directory recovery has
// ALREADY decided may be re-founded (macKeyFor let the key be created because the
// log provably held no readable record). So the floor is read UNVERIFIED: its
// numbers are kept, because they are only ever consumed as a RAISE and therefore
// can only make the start MORE conservative, while its `sealed` bit -- the one
// field that is a trust decision -- is discarded. And it is logged at ERROR,
// because a data directory that has lost its identity is not a routine start.
func TestWALIndexFloorSurvivesALostMACKey(t *testing.T) {
	dir := t.TempDir()
	_, highest := crashedFixture(t, dir, 4)
	reserved, _, _ := readFloorFile(t, dir)

	// The key AND the log are gone: this is the re-founded directory.
	if err := os.Remove(macKeyPath(dir)); err != nil {
		t.Fatalf("removing the key: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, WALFileName)); err != nil {
		t.Fatalf("removing the log: %v", err)
	}

	var buf bytes.Buffer
	l, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open with a lost MAC key = %v; recovery must ALWAYS reach a running server (invariant 6), and this directory holds no readable record to protect", err)
	}
	c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"after":true}`)})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if c.PrepareIndex <= highest {
		t.Errorf("the first index after a lost key was %d, but %d had already been handed out. An UNVERIFIED floor is still read -- only its seal is discarded -- precisely so this cannot happen", c.PrepareIndex, highest)
	}
	if c.PrepareIndex <= reserved {
		t.Errorf("the first index after a lost key was %d, but the floor had authorised up to %d", c.PrepareIndex, reserved)
	}
	out := buf.String()
	assertLogged(t, out, "ERROR", "wal could not verify the durable record index floor and read it WITHOUT authentication")

	// And the file it leaves behind is authenticated under the NEW key, so the
	// next start is an ordinary one.
	if raw := string(readFile(t, filepath.Join(dir, IndexFloorFileName))); !strings.Contains(raw, indexFloorTagHMAC) {
		t.Errorf("the floor was not rewritten under the new key:\n%s", raw)
	}
}

// TestWALIndexFloorReapIsNotAGlobPattern is P2-1, which a security gate rated
// the most real of the P2s and verified empirically.
//
// The reaper used to be filepath.Glob(filepath.Join(dir, ".wal-index-floor-*")),
// with `dir` coming straight from the operator's -data flag. Two consequences,
// both reproduced here:
//
//   - a directory whose name contains a glob CHARACTER CLASS made the pattern
//     match a DIFFERENT directory, so the reaper unlinked a sibling's temp file
//     while never matching its own;
//   - a directory whose name contains an UNCLOSED bracket made Glob return
//     ErrBadPattern for ever, silently disabling the reaper.
func TestWALIndexFloorReapIsNotAGlobPattern(t *testing.T) {
	for _, name := range []string{"bus[1]", "bus[", "bus*", "bus?", `bus\x`} {
		name := name
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, name)
			if err := os.MkdirAll(dir, dirMode); err != nil {
				t.Skipf("this filesystem will not hold a directory named %q: %v", name, err)
			}
			// The SIBLING a character class would reach into: /…/bus1 for
			// /…/bus[1]. Its temp file must survive.
			sibling := filepath.Join(root, "bus1")
			if err := os.MkdirAll(sibling, dirMode); err != nil {
				t.Fatalf("creating the sibling: %v", err)
			}
			siblingTemp := filepath.Join(sibling, ".wal-index-floor-victim")
			if err := os.WriteFile(siblingTemp, []byte("not yours"), 0o600); err != nil {
				t.Fatalf("writing the sibling temp: %v", err)
			}

			key := floorKey(t, dir)
			writeFloorFile(t, dir, "", encodeFloorBody(64, 0, false))
			ownTemp := filepath.Join(dir, ".wal-index-floor-stale")
			if err := os.WriteFile(ownTemp, []byte("partial"), 0o600); err != nil {
				t.Fatalf("writing the stale temp: %v", err)
			}

			if _, err := openIndexFloor(dir, key, true); err != nil {
				t.Fatalf("openIndexFloor in a directory named %q: %v", name, err)
			}
			if _, err := os.Stat(ownTemp); !os.IsNotExist(err) {
				t.Errorf("the reaper did not remove ITS OWN stale temp %s (stat err = %v); a metacharacter in -data must not disable it", ownTemp, err)
			}
			if _, err := os.Stat(siblingTemp); err != nil {
				t.Errorf("the reaper removed a temp file from the SIBLING directory %s (%v); -data is a path, not a pattern, and one bus must never unlink another's files", sibling, err)
			}
		})
	}
}
