package wal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// The durable index floor's CREDIBILITY CEILING (maxCredibleFloorIndex).
//
// THE ATTACK, reproduced end to end below. The floor file accepts a legacy
// header shape whose tag is a plain unkeyed sha256 over its own body -- which
// anyone able to write the data directory can recompute WITHOUT the MAC key.
// `sealed` was already discarded on that path, but `reserved` and `written` were
// believed. So an attacker could write a legacy floor claiming an index near the
// top of the 64-bit space, and recovery would:
//
//  1. accept it, because the unkeyed digest checks out;
//  2. REWRITE it with a valid HMAC-SHA256 tag, logging "upgraded ... to a keyed
//     MAC" -- laundering a chosen value into an authenticated one that no later
//     forensic check can tell from a number the bus legitimately wrote;
//  3. resume at that index, and then refuse to start for ever, because no index
//     can be issued without reusing one. The message-sequence floor derived from
//     it fails every mint after that. Permanently, with no remedy.
//
// TWO THINGS ABOUT THE FIX ARE LOAD-BEARING, and the second was learned by
// getting it wrong -- both gates independently proved the trapdoor.
//
//  1. THE CEILING IS CHECKED AT READ TIME, before the rewrite. There is no
//     "after the rewrite" that helps, because after it the value is signed.
//  2. IT APPLIES ONLY TO THE UNAUTHENTICATED SHAPES. Bounding the keyed shape as
//     well looks stricter and is strictly worse: `begin` raises reserved by
//     indexReserveBlock on every Open, so a floor accepted AT the ceiling is
//     persisted just above it, and the bus would then refuse to start for ever
//     on a number the attacker chose. That converts a bounded loss into a
//     permanent, attacker-triggered brick.
// ---------------------------------------------------------------------------

// TestWALIndexFloorBoundRejectsAnAbsurdUnkeyedIndex is the security
// regression test.
func TestWALIndexFloorBoundRejectsAnAbsurdUnkeyedIndex(t *testing.T) {
	// A real bus first, so the directory has a genuine MAC key the attacker does
	// not hold and cannot compute a keyed tag with.
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.Write(Entry{Kind: "agent", Body: nil}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	floorPath := filepath.Join(dir, IndexFloorFileName)

	// The forgery: an unkeyed floor claiming an index one below the top of the
	// 64-bit space. No MAC key is used to build it.
	const absurd = "18446744073709551614"
	writeLegacyFloorFile(t, dir, "reserved "+absurd+"\nwritten "+absurd+"\nsealed 1\n")
	forged, err := os.ReadFile(floorPath)
	if err != nil {
		t.Fatalf("reading the forged floor: %v", err)
	}

	var buf bytes.Buffer
	l2, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err == nil {
		// NOT t.Fatalf. The laundering assertions below are the ones that matter,
		// and aborting here would make them unreachable in exactly the scenario
		// they exist to describe -- a reviewer pointed out they had therefore
		// never been observed able to fail. Close and keep going.
		t.Errorf("Open accepted an unkeyed floor claiming record index %s. Believing it burns the entire index space, and the bus then refuses to start for ever with no remedy\n--- log ---\n%s",
			absurd, buf.String())
		if cerr := l2.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	} else {
		if !errors.Is(err, ErrIndexFloorCorrupt) {
			t.Fatalf("err = %v, want one matching ErrIndexFloorCorrupt", err)
		}
		// The operator must be told it is TAMPERING, not ordinary damage, and must
		// NOT be sent to restore the MAC key: this check never uses the key, so
		// that is the wrong first move on what may be an intrusion.
		for _, want := range []string{"TREAT IT AS TAMPERING", "UNKEYED", "HAS NOT BEEN MODIFIED", "will NOT help"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not say %q; an unkeyed file carrying an impossible number is evidence, and the operator's next move depends on knowing that.\nerr = %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "CHECK "+MACKeyFileName+" FIRST") {
			t.Errorf("the error tells the operator to check the MAC key FIRST. This diagnosis is key-INDEPENDENT and the file was never authenticated under that key, so that is the wrong first move.\nerr = %v", err)
		}
	}

	// THE LOAD-BEARING ASSERTIONS: the file was NOT rewritten, so the attacker's
	// number was never signed under this data directory's key. A refusal that
	// happened after the rewrite would leave a laundered value behind, which is
	// strictly worse than the brick -- no later forensic check could tell it from
	// a number the bus legitimately wrote.
	after, rerr := os.ReadFile(floorPath)
	if rerr != nil {
		t.Fatalf("reading the floor after the refused Open: %v", rerr)
	}
	if !bytes.Equal(after, forged) {
		t.Errorf("the floor file changed during a refused Open.\nbefore: %q\nafter:  %q\nThe rewrite is exactly where an unauthenticated number becomes an authenticated one; nothing may touch this file until it has been believed",
			forged, after)
	}
	if bytes.Contains(after, []byte(indexFloorTagHMAC)) {
		t.Errorf("the forged floor now carries a keyed %s tag: an attacker-chosen index has been signed with the real key and is now indistinguishable from one the bus wrote",
			indexFloorTagHMAC)
	}
	if strings.Contains(buf.String(), "upgraded the durable record index floor") {
		t.Errorf("recovery logged the keyed-MAC upgrade for a file it refused.\n--- log ---\n%s", buf.String())
	}
}

// TestWALIndexFloorBoundDoesNotCreateAPermanentBrickAtItsOwnBoundary is the
// trapdoor both gates found, pinned so it cannot come back.
//
// THE TRAP: `begin` raises `reserved` by indexReserveBlock on EVERY Open. So a
// floor planted at exactly maxCredibleFloorIndex is accepted, and the bus then
// persists a value just ABOVE the ceiling under the real key. If the ceiling
// also applied to the keyed shape, every subsequent Open would refuse -- for
// ever, on a number the attacker chose. Bounding the keyed shape would therefore
// have converted a bounded loss into a permanent, attacker-triggered brick: a
// worse outcome than the one the ceiling was added to prevent.
//
// So the ceiling bounds UNTRUSTED input only, and this test asserts the property
// that matters to an operator: the bus starts, and KEEPS starting.
func TestWALIndexFloorBoundDoesNotCreateAPermanentBrickAtItsOwnBoundary(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Exactly AT the ceiling, in the unkeyed shape an attacker can produce.
	writeLegacyFloorFile(t, dir, mainFormatFloorBody(maxCredibleFloorIndex, maxCredibleFloorIndex))

	// Three restarts: the first adopts and re-signs it, the next two read back
	// the keyed file the first one wrote -- which is where the brick would show.
	for i := 1; i <= 3; i++ {
		l2, err := Open(LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("Open %d refused a floor planted at exactly the ceiling: %v\nThe bus must not be permanently unstartable on a number an attacker chose. The ceiling bounds untrusted INPUT; it must not be applied to the keyed file the bus itself then writes",
				i, err)
		}
		if _, err := l2.Write(Entry{Kind: "agent", Body: nil}); err != nil {
			t.Fatalf("Write on start %d: %v", i, err)
		}
		if err := l2.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
	// And the residual is what the design comment claims it is: index space was
	// burned, invariant 1 still holds (the sequence only moved UP), and there is
	// an enormous amount of it left.
	reserved, _, _ := readFloorFile(t, dir)
	if reserved < maxCredibleFloorIndex {
		t.Errorf("reserved = %d, want at least the planted %d: the floor is monotonic and must never move down", reserved, maxCredibleFloorIndex)
	}
	if remaining := ^uint64(0) - reserved; remaining < 1<<60 {
		t.Errorf("only %d indices remain after the forgery was absorbed; the residual is supposed to be bounded, not exhausting", remaining)
	}
}

// TestWALIndexFloorBoundUnverifiedArmDoesNotTellTheOperatorToDeleteARealFloor is
// second round's finding, and it is a state a reviewer REPRODUCED rather than
// imagined.
//
// The ceiling deliberately lets a legacy value AT the ceiling be absorbed and
// re-signed just above it, so a data directory can legitimately hold a KEYED
// floor above the ceiling. Lose wal-mac.key after that -- with a log recovery
// cannot identify, so a new key is minted -- and that GENUINE floor arrives at
// indexFloorAbsurd on the `unverified` arm.
//
// The legacy arm's advice is right for a planted file and catastrophic here: it
// says the file "never encoded a real floor" and that "DELETING IT COSTS
// NOTHING". Deleting this one rewinds the WAL record index, and the message
// sequence derived from it, below numbers already handed out -- the exact
// invariant-1 violation the floor exists to prevent.
func TestWALIndexFloorBoundUnverifiedArmDoesNotTellTheOperatorToDeleteARealFloor(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.Write(Entry{Kind: "agent", Body: nil}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Absorbed at the ceiling and re-signed just above it -- a genuine keyed
	// floor above the ceiling, reached the way a real directory reaches one.
	writeLegacyFloorFile(t, dir, mainFormatFloorBody(maxCredibleFloorIndex, maxCredibleFloorIndex))
	l2, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open absorbing the floor at the ceiling: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if reserved, _, _ := readFloorFile(t, dir); reserved <= maxCredibleFloorIndex {
		t.Fatalf("reserved = %d, want above the ceiling %d: the fixture has not reached the state under test", reserved, maxCredibleFloorIndex)
	}

	// Now lose the key, with a log that cannot be identified, so recovery mints a
	// new one and the genuine floor becomes UNVERIFIABLE.
	if err := os.Remove(filepath.Join(dir, MACKeyFileName)); err != nil {
		t.Fatalf("removing the MAC key: %v", err)
	}
	if err := os.Truncate(filepath.Join(dir, WALFileName), 0); err != nil {
		t.Fatalf("truncating the WAL: %v", err)
	}

	_, err = Open(LogOptions{Dir: dir})
	if err == nil {
		t.Skip("this build no longer refuses an unverifiable floor above the ceiling; the text under test is unreachable")
	}
	msg := err.Error()
	// The two sentences that are TRUE for a planted file and FALSE here.
	for _, mustNotSay := range []string{
		"it never encoded a real floor",
		"DELETING IT COSTS NOTHING",
		"has NOT been signed under this data directory's key",
		// Tampering IS modification; an unqualified denial contradicts the
		// sentence that has just said this directory can no longer tell.
		"IT HAS NOT BEEN MODIFIED",
	} {
		if strings.Contains(msg, mustNotSay) {
			t.Errorf("the unverified arm says %q. This floor MAY be this directory's own, written under the key that was lost -- deleting it rewinds the WAL index and the message sequence below numbers already issued, which is the invariant-1 violation this file exists to prevent.\nerr = %v",
				mustNotSay, err)
		}
	}
	// And what it must say instead: restore the key first, do not delete.
	for _, mustSay := range []string{"RESTORE " + MACKeyFileName, "DO NOT DELETE IT"} {
		if !strings.Contains(msg, mustSay) {
			t.Errorf("the unverified arm does not say %q; restoring the key is the only move that can settle whether this number is real.\nerr = %v", mustSay, err)
		}
	}
}

// TestWALIndexFloorBoundBelievesAKeyedFloorAboveTheCeiling documents the deliberate
// asymmetry directly, because it is the part most likely to look like a bug to
// someone reading only the ceiling.
//
// A tag that verifies under this data directory's own wal-mac.key says the bus
// wrote the number. The remaining explanations are media damage that somehow
// preserved an HMAC-SHA256 tag, and a defect in our own writer -- and refusing to
// boot over the latter IS the brick. log.go's MaxUint64 refusal is the backstop.
func TestWALIndexFloorBoundBelievesAKeyedFloorAboveTheCeiling(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	above := maxCredibleFloorIndex + 1
	// The KEYED shape, signed with this directory's real key. Note this is
	// writeFloorFile with a THREE-line body: writeMainFormatFloor writes the
	// UNKEYED shape despite its name, and a keyed tag over a two-line body is
	// refused outright as a combination no agent-bus has ever written -- so a
	// fixture built either of those ways would never reach the ceiling at all.
	// A gate measured exactly that mistake in an earlier draft of these tests.
	writeFloorFile(t, dir, "", encodeFloorBody(above, above, true))

	l2, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open refused a floor above the ceiling whose keyed tag VERIFIED: %v\nThe ceiling bounds untrusted input. A file the bus itself signed is believed, and bounding it is what created an attacker-reachable permanent brick", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWALIndexFloorBoundAcceptsCredibleIndexes is the other half: the ceiling must be
// absurd enough that nothing real trips it, or it is a new way to brick a bus.
//
// It also pins the boundary exactly, so a later change to maxCredibleFloorIndex
// has to come here and say what it is doing.
func TestWALIndexFloorBoundAcceptsCredibleIndexes(t *testing.T) {
	cases := []struct {
		name     string
		reserved uint64
		wantOK   bool
	}{
		{"Zero", 0, true},
		{"OneRecord", 1, true},
		{"ABusyYear", 1 << 32, true},
		{"AtTheCeiling", maxCredibleFloorIndex, true},
		{"OnePastTheCeiling", maxCredibleFloorIndex + 1, false},
		{"TopOfTheSpace", ^uint64(0) - 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l, err := Open(LogOptions{Dir: dir})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			// THE UNKEYED SHAPE -- which is the shape the ceiling governs, and the
			// ONLY one it governs. The keyed path is covered separately by
			// TestWALIndexFloorBoundBelievesAKeyedFloorAboveTheCeiling, and deliberately
			// asserts the opposite: a floor the bus itself signed is believed.
			writeMainFormatFloor(t, dir, tc.reserved, tc.reserved)

			l2, err := Open(LogOptions{Dir: dir})
			if tc.wantOK {
				if err != nil {
					t.Fatalf("Open refused a credible floor claiming %d: %v\nA ceiling that rejects a real deployment is a new way to brick a bus", tc.reserved, err)
				}
				if err := l2.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Open accepted a floor claiming %d, which is above the credible ceiling %d", tc.reserved, maxCredibleFloorIndex)
			}
			if !errors.Is(err, ErrIndexFloorCorrupt) {
				t.Fatalf("err = %v, want one matching ErrIndexFloorCorrupt", err)
			}
		})
	}
}
