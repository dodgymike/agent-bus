package hub_test

// The DURABLE MINT, tested at the level the guarantee is actually made.
//
// # Why this file exists at all
//
// SIGN-2/SIGN-6 shipped /v1/mint — reserve-then-send — with NO unit tests of its
// durability mechanism whatsoever. A reviewer measured what that cost: gutting
// applySeqFloor, replacing the mint-authority check with `if false`, and deleting
// the store record validations each left the whole tree GREEN. Every existing
// test minted and spent in the same breath, so nothing in the suite could see the
// difference between "the number is burned on disk" and "the number is not".
//
// The property under test is ONE sentence, and every test below is a way of
// trying to break it:
//
//	no sequence number this bus has ever handed out is ever handed out again,
//	including across a restart, a crash, and a WAL QUARANTINE
//
// The quarantine clause is the one that was false. Read
// TestQuarantineDoesNotReissueAMintedSequence first: it is the regression test
// for a P0 in which a client's SIGNED assignment could be minted a second time,
// with both signatures verifying and nothing downstream able to tell.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// openMintHub opens a hub over an already-open log in dir, wired exactly as
// cmd/agent-bus wires it — DataDir alongside the log, replay as a read-only pass
// over the log's own path — and enrols one agent per name.
//
// It returns the hub and the buffer its logger writes to, because several
// properties here (the migration warning, the quarantine report) are OBSERVABLE
// ONLY as a log line: they are what an operator is told, and a test that skips
// them lets the wording silently become false.
//
// now may be nil for the real clock. quarantined mirrors the string main passes
// from wal.Repair.Quarantined and only affects what is logged.
func openMintHub(t *testing.T, dir string, lg *wal.Log, now func() time.Time, quarantined string, agents ...string) (*hub.Hub, *bytes.Buffer) {
	t.Helper()
	h, buf, err := tryOpenMintHub(t, dir, lg, now, quarantined, agents...)
	if err != nil {
		t.Fatalf("hub.Open(dir=%s): %v", dir, err)
	}
	return h, buf
}

// tryOpenMintHub is openMintHub for the tests that expect Open to REFUSE. It
// returns the error rather than failing, so a test can assert on it.
func tryOpenMintHub(t *testing.T, dir string, lg *wal.Log, now func() time.Time, quarantined string, agents ...string) (*hub.Hub, *bytes.Buffer, error) {
	t.Helper()
	buf := &bytes.Buffer{}
	roster := hub.NewStaticRoster()
	path := lg.Path()
	h, err := hub.Open(hub.Options{
		BusID:       testBusID,
		DataDir:     dir,
		Durable:     lg,
		Replay:      func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex:   lg.Recovered().NextIndex,
		Quarantined: quarantined,
		Roster:      roster,
		Logger:      logging.New(buf, logging.LevelDebug),
		Now:         now,
	})
	if err != nil {
		return nil, buf, err
	}
	enrolAll(t, roster, testBusID, agents...)
	return h, buf, nil
}

// mustMint reserves an assignment and fails the test if the bus refuses. It is
// distinct from hub_test.go's mintFor, which deliberately SWALLOWS a refusal
// because most publish tests are asserting on the publish error; here the mint
// itself is the thing under test.
func mustMint(t *testing.T, h *hub.Hub, sender, op, key string) hub.Mint {
	t.Helper()
	m, err := h.Mint(hub.MintRequest{Sender: sender, Op: op, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("Mint(%s, %s, %q): %v", sender, op, key, err)
	}
	return m
}

// readSeqFloor reads <dir>/message-seq-floor and returns the floor it records,
// and whether the file exists at all.
//
// It parses the file rather than calling into the package, and that is
// deliberate: the point of this file is that it is READABLE BY AN OPERATOR AND
// BY ANOTHER PROGRAM. A test that only round-tripped through the writer would
// pass just as happily if the format silently changed shape, which is exactly
// the kind of change that strands a data directory (an unknown format is fatal
// and the file is never regenerated).
func readSeqFloor(t *testing.T, dir string) (uint64, bool) {
	t.Helper()
	path := filepath.Join(dir, hub.SeqFloorFileName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%s has %d lines, want a header and one \"floor <n>\" line:\n%s", path, len(lines), data)
	}
	if !strings.HasPrefix(lines[0], "agent-bus-message-seq-floor v5 sha256=") {
		t.Fatalf("%s header = %q, want the magic, the on-disk format version 5 (RESERVED for this file) and a sha256 digest", path, lines[0])
	}
	if !strings.HasPrefix(lines[1], "floor ") {
		t.Fatalf("%s body = %q, want \"floor <n>\"", path, lines[1])
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(lines[1], "floor "), 10, 64)
	if err != nil {
		t.Fatalf("%s floor field: %v", path, err)
	}
	return n, true
}

// quarantineTheLog does to the data directory exactly what a WAL quarantine
// does: it MOVES THE LOG ASIDE and leaves everything else in place, so the next
// wal.Open starts a fresh, empty log in the same directory.
//
// The log must be CLOSED first — this models the next process, not a concurrent
// one. Everything else the data directory holds (wal-mac.key, wal-index-floor,
// and the message sequence floor this wave added) survives, because a real
// quarantine touches only the log file. That detail is the whole test: the
// question is which of the surviving artifacts still bounds the sequence.
func quarantineTheLog(t *testing.T, lg *wal.Log) string {
	t.Helper()
	path := lg.Path()
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log before quarantining it: %v", err)
	}
	aside := path + ".quarantined"
	if err := os.Rename(path, aside); err != nil {
		t.Fatalf("moving %s aside to simulate a quarantine: %v", path, err)
	}
	return aside
}

// ---------------------------------------------------------------------------
// The floor is durable BEFORE the number is handed out
// ---------------------------------------------------------------------------

// TestMintBurnsTheSequenceOnDiskBeforeHandingItOut is the ordering test, and the
// ordering is the entire guarantee.
//
// "The number is burned" and "the number was handed out" are only safe in ONE
// order. If the client learns the number first, a crash in the window loses the
// claim and a restart hands the same number to somebody else — who then holds a
// second signature over one origin message id.
//
// Two halves, and the second is the one that cannot be faked:
//
//  1. after a mint, the floor file on disk already covers the sequence returned;
//  2. when the floor CANNOT be written, the mint FAILS and hands out NOTHING —
//     proven by making the data directory unwritable and then observing that the
//     sequence the failed mint would have used is still available afterwards.
func TestMintBurnsTheSequenceOnDiskBeforeHandingItOut(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	alpha := agentID(t, testBusID, "alpha")
	h, _ := openMintHub(t, dir, lg, nil, "", "alpha")

	// CHANGED 2026-08-07, and the reversal is load-bearing. This used to assert
	// the file did NOT exist yet, on the grounds that "the file must be created
	// by the first mint, not by opening the hub, or every start would rewrite a
	// file it has nothing new to say about".
	//
	// The stated worry does not happen — the file is written only when it is
	// ABSENT, so it is created once and never rewritten with the same content.
	// And the old behaviour left "the file is missing" meaning two different
	// things: a data directory older than the file, and a fresh directory that
	// has simply never minted. Open REFUSES to start on the first of those when
	// the log has also been damaged, because the floor would have to be rebuilt
	// from a log just proven incomplete — so the two cases must be
	// distinguishable, or a brand-new bus that was kill -9'd before its first
	// mint would be refused as if it were a damaged legacy directory. Writing
	// the file at Open collapses the ambiguity. See seqFloorFile.ensureExists.
	floor0, existed0 := readSeqFloor(t, dir)
	if !existed0 {
		t.Fatalf("opening a hub on a fresh data directory left no %s; its absence is what Open uses to recognise a data directory older than the file, so it must not also describe a fresh one", hub.SeqFloorFileName)
	}
	if floor0 != 0 {
		t.Fatalf("a hub opened on a FRESH data directory recorded floor %d, want 0; nothing has been issued, so claiming anything is burned would skip numbers for no reason", floor0)
	}

	m := mustMint(t, h, alpha, "send", "k-1")
	floor, existed := readSeqFloor(t, dir)
	if !existed {
		t.Fatalf("Mint returned sequence %d but %s does not exist: the number was handed to a client and NOTHING on disk says it is burned, so a restart would hand it out again (invariant 1)", m.Seq, hub.SeqFloorFileName)
	}
	if floor < m.Seq {
		t.Fatalf("Mint returned sequence %d but the durable floor is only %d; every issued number must be at or below the floor that was fsynced BEFORE it was issued", m.Seq, floor)
	}
	// The batch is burned in one write, so the floor runs a whole MintBatchSize
	// ahead. Pinned exactly, because "at least the sequence" would also pass for
	// an implementation that wrote a floor per mint — which is a different
	// mechanism with a different fsync cost, and it should not change silently.
	if want := uint64(hub.MintBatchSize); floor != want {
		t.Fatalf("after the first mint the durable floor is %d, want %d (MintBatchSize burned ahead in one write)", floor, want)
	}

}

// TestAMintWhoseFloorCannotBePersistedIssuesNothing is the second half of the
// ordering proof, and the half that cannot be faked by an implementation that
// writes the floor AFTER handing the number out.
//
// The data directory is made unwritable BEFORE the first mint, so the atomic
// replace (temp file + fsync + rename) cannot even begin. The mint must then
// FAIL and issue nothing — and the proof that it issued nothing is that once the
// directory is writable again, the very first sequence is still available. An
// implementation that returned the number first and persisted afterwards would
// have handed out sequence 1 already and would resume at 2, silently having told
// a client about a number no disk has ever recorded.
func TestAMintWhoseFloorCannotBePersistedIssuesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		// root ignores the mode bits, so the write would succeed and the test
		// would assert nothing. Skipping is honest; passing vacuously is not.
		t.Skip("running as root: directory permissions do not deny writes, so this test cannot create the failure it is about")
	}
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	alpha := agentID(t, testBusID, "alpha")
	h, _ := openMintHub(t, dir, lg, nil, "", "alpha")

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	restored := false
	defer func() {
		if !restored {
			// t.TempDir's cleanup needs the write bit back, or the test binary
			// fails on a directory it cannot remove.
			_ = os.Chmod(dir, 0o700)
		}
	}()

	if m, err := h.Mint(hub.MintRequest{Sender: alpha, Op: "send", IdempotencyKey: "k-1"}); err == nil {
		t.Fatalf("Mint returned %s (sequence %d) on a data directory it cannot write the sequence floor to; the number would be known to a client and to nothing else", m.MessageID, m.Seq)
	}
	// The file itself now exists from Open (see ensureExists), so its PRESENCE
	// no longer distinguishes anything. The claim that actually matters is
	// unchanged and is asserted directly instead: the failed mint burned
	// NOTHING, so the floor is still 0 and does not cover the sequence the mint
	// would have returned. This is the stronger assertion of the two — "no file"
	// would also have passed for an implementation that wrote a file claiming
	// the number WAS burned and then failed.
	if floor, existed := readSeqFloor(t, dir); !existed || floor != 0 {
		t.Fatalf("after a mint that could not persist its floor, %s reports (floor=%d, exists=%v), want (0, true): the mint issued nothing, so nothing may be recorded as burned", hub.SeqFloorFileName, floor, existed)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("restoring the mode on %s: %v", dir, err)
	}
	restored = true

	m := mustMint(t, h, alpha, "send", "k-1")
	if m.Seq != 1 {
		t.Fatalf("after a mint that FAILED to burn its floor, the next mint returned sequence %d, want 1; the failed mint issued nothing, so it must not have consumed a number either — and if it did consume one, it consumed a number that was never durably burned", m.Seq)
	}
	if floor, _ := readSeqFloor(t, dir); floor < m.Seq {
		t.Fatalf("the durable floor is %d, below the issued sequence %d", floor, m.Seq)
	}
}

// ---------------------------------------------------------------------------
// THE P0: a quarantine must not reissue a minted sequence
// ---------------------------------------------------------------------------

// TestQuarantineDoesNotReissueAMintedSequence is the regression test for the
// defect this wave exists to close, and it is the one to run first if any of
// this is ever refactored.
//
// # The defect, exactly
//
// The mint's claim ("every sequence up to N is burned") was written as a
// "seqfloor" record INSIDE THE WAL. A quarantine moves the whole log aside, so
// the claim went with it. Five mints consume five sequences but only TWO WAL
// indices — one floor record covers 256 numbers — so after a quarantine every
// surviving source collapsed: wal.Recovered.NextIndex-1 = 2, the replayed floor
// records = none, the highest replayed message sequence = none. The next mint
// then returned 3, then 4, then 5: numbers a client already held Ed25519
// signatures over, with no way for any recipient to tell the two apart.
//
// It is NOT covered by wal's own durable index floor, and this is the point most
// easily got wrong. That floor bounded the sequence only through a COUNTING
// argument — every sequence was <= the index of the prepare carrying it — and
// minting in batches of 256 made that false on the very first mint.
//
// # Why the assertions are written against the numbers already handed out
//
// The test does not assert a particular resumed value beyond the minimum,
// because gaps are CORRECT here (internal/ids/sequence.go). What must hold is
// only that nothing already issued comes back.
func TestQuarantineDoesNotReissueAMintedSequence(t *testing.T) {
	cases := []struct {
		name string
		// alsoRemove is a file to delete along with the log, to model a harsher
		// loss than a plain quarantine.
		alsoRemove string
	}{
		{
			name: "the log is moved aside",
		},
		{
			// wal's index floor is the other durable number in the directory. It
			// is removed here to prove this property does NOT lean on it: it is a
			// floor on WAL RECORD INDICES, it is only accidentally near the
			// sequence, and one day the two counters will be nowhere near each
			// other.
			name:       "the log is moved aside and the WAL index floor is lost too",
			alsoRemove: "wal-index-floor",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lg := openTestLog(t, dir, false)
			alpha := agentID(t, testBusID, "alpha")
			h, _ := openMintHub(t, dir, lg, nil, "", "alpha")

			// Five mints: five sequences, ONE floor record, two WAL indices. This
			// is the reviewer's probe, reproduced.
			issued := make([]uint64, 0, 5)
			for i := 0; i < 5; i++ {
				m := mustMint(t, h, alpha, "send", fmt.Sprintf("k-%d", i))
				issued = append(issued, m.Seq)
			}

			aside := quarantineTheLog(t, lg)
			if tc.alsoRemove != "" {
				if err := os.Remove(filepath.Join(dir, tc.alsoRemove)); err != nil {
					t.Fatalf("removing %s: %v", tc.alsoRemove, err)
				}
			}

			lg2 := openTestLog(t, dir, true)
			if next := lg2.Recovered().NextIndex; next > issued[len(issued)-1] {
				t.Fatalf("the fresh log resumed at index %d, above every sequence this test issued (%v); the WAL index would then mask the defect and this test would prove nothing", next, issued)
			}
			h2, buf := openMintHub(t, dir, lg2, nil, aside, "alpha")

			m := mustMint(t, h2, alpha, "send", "k-after-quarantine")
			for _, prev := range issued {
				if m.Seq == prev {
					t.Fatalf("after a QUARANTINE the bus reissued sequence %d, which it had already handed out (%v). A client holds a signature over that assignment, so two validly-signed messages would now carry one origin message id and nothing downstream can detect it (invariant 1).\nstartup log: %s", m.Seq, issued, buf.String())
				}
			}
			if m.Seq <= issued[len(issued)-1] {
				t.Fatalf("after a QUARANTINE the bus resumed at sequence %d, at or below the highest it had already issued (%d); every number up to the durable floor is burned for ever, whether or not a message carried it", m.Seq, issued[len(issued)-1])
			}
			// The whole first batch was burned, so the resumption is above it.
			if want := uint64(hub.MintBatchSize); m.Seq <= want {
				t.Fatalf("after a QUARANTINE the bus resumed at sequence %d, inside the batch of %d numbers the first mint durably burned; a burned number is burned even if no message ever carried it", m.Seq, want)
			}
			// The operator must be TOLD, and told the truth: message ids are not at
			// risk on this path, and saying they are would train an operator to
			// ignore the line that matters.
			if out := buf.String(); !strings.Contains(out, "QUARANTINED") {
				t.Fatalf("a quarantined start logged nothing about it; the discard is sanctioned but silence about it is the defect.\nlog: %s", out)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Restart mid-batch
// ---------------------------------------------------------------------------

// TestRestartMidBatchResumesAboveEveryNumberHandedOut covers the ordinary
// restart — no damage, no quarantine, a clean close — while the mint is PART WAY
// through a burned batch.
//
// This is the case the batch created: the client has three numbers, the log has
// one floor record covering 256, and nothing anywhere carries sequences 4..256.
// Resuming from "the highest sequence the log can show" would hand 4 straight
// back out. The durable floor is what makes the burned-but-unused range stay
// burned, and the resulting GAP is correct, not damage to compact.
func TestRestartMidBatchResumesAboveEveryNumberHandedOut(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, false)
	alpha := agentID(t, testBusID, "alpha")
	h, _ := openMintHub(t, dir, lg, nil, "", "alpha")

	var issued []uint64
	for i := 0; i < 3; i++ {
		issued = append(issued, mustMint(t, h, alpha, "send", fmt.Sprintf("k-%d", i)).Seq)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	lg2 := openTestLog(t, dir, true)
	h2, _ := openMintHub(t, dir, lg2, nil, "", "alpha")
	m := mustMint(t, h2, alpha, "send", "k-after-restart")

	for _, prev := range issued {
		if m.Seq == prev {
			t.Fatalf("after a clean restart the bus reissued sequence %d (already handed out: %v)", m.Seq, issued)
		}
	}
	// Pinned exactly: the first batch burned 1..MintBatchSize, so the first number
	// after a restart is MintBatchSize+1. The 253 unused numbers are GONE, and
	// that is the correct outcome — a bus that "recovered" them would be reissuing
	// numbers it had already promised never to issue.
	if want := uint64(hub.MintBatchSize) + 1; m.Seq != want {
		t.Fatalf("after a restart mid-batch the first mint returned sequence %d, want %d (one past the whole burned batch)", m.Seq, want)
	}
}

// ---------------------------------------------------------------------------
// Pre-upgrade data directories
// ---------------------------------------------------------------------------

// TestADataDirWithNoSeqFloorFileIsBackfilledLoudly pins the MIGRATION decision,
// which is a judgement call and therefore has to be a test rather than a comment.
//
// A data directory with history but no message-seq-floor file was written by a
// binary that predates the file. The decision is: NOT FATAL, backfilled from
// what the log can prove, and reported at WARN.
//
// It is deliberately the OPPOSITE call to the agent-suffix floors, where the
// same shape IS fatal (openSuffixAllocator, with a -backfill opt-in, set by a
// security gate). The difference is that a missing agent-suffixes file has NO
// other durable source — enrolment was memory-only, so every name really would
// resume from 1 — whereas here the log still carries three independent sources.
// Making this one fatal would brick every deployed bus on upgrade to buy nothing
// on any start that is not ALSO a quarantine.
//
// The window is closed by the very start that finds it open: Open writes the
// derived floor before it serves. That is asserted here, because a backfill that
// is only in memory would leave the NEXT start — the one that quarantines —
// exactly as exposed as before.
func TestADataDirWithNoSeqFloorFileIsBackfilledLoudly(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, false)
	alpha := agentID(t, testBusID, "alpha")
	h, _ := openMintHub(t, dir, lg, nil, "", "alpha")
	issued := mustMint(t, h, alpha, "send", "k-1").Seq
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	// Model the pre-upgrade directory: the log and its in-log "seqfloor" records
	// are intact, the floor FILE has never been written.
	floorPath := filepath.Join(dir, hub.SeqFloorFileName)
	if err := os.Remove(floorPath); err != nil {
		t.Fatalf("removing %s to model a pre-upgrade data directory: %v", floorPath, err)
	}

	lg2 := openTestLog(t, dir, true)
	_, buf := openMintHub(t, dir, lg2, nil, "", "alpha")

	floor, existed := readSeqFloor(t, dir)
	if !existed {
		t.Fatalf("opening a data directory with history and no %s left the file absent; the migration window would then still be open on the NEXT start, which is the one that might quarantine", hub.SeqFloorFileName)
	}
	if floor < issued {
		t.Fatalf("the backfilled floor is %d, below the sequence %d this directory had already handed out", floor, issued)
	}
	if out := buf.String(); !strings.Contains(out, hub.SeqFloorFileName) {
		t.Fatalf("backfilling the sequence floor logged nothing naming %s; an operator has no other way to know this directory spent a start unprotected.\nlog: %s", hub.SeqFloorFileName, out)
	}
}

// TestSeqFloorMigrationWarningDoesNotClaimTheLogIsComplete pins the HONESTY of
// the migration WARN, which is the only thing an operator gets on this path.
//
// The warning used to end "...this start verified that recovery removed no
// records from that log, so this start closes the window". Literally true —
// recovery discarded nothing — and substantively false, because a truncation
// landing EXACTLY ON A RECORD BOUNDARY removes records without leaving anything
// torn for recovery to discard. Open's guard reads the same discard signal, so
// the guard and its own reassurance share one blind spot.
//
// The size of that blind spot, stated the way it must be stated: on an
// exhaustively swept real specimen the guard failed at 22 of 22 RECORD
// BOUNDARIES — 100%, and necessarily so, since it cannot fire where nothing is
// torn. Thirteen of the 22 reissued a sequence already delivered (one of them
// reissuing a message id end to end); the other nine were harmless only because
// that directory's floor had already stepped past its delivered high-water. The
// harmful fraction is a property of the DIRECTORY'S HISTORY, not of the bug.
// Every one of those 22 starts printed "closes the window".
//
// This test does NOT build a boundary-exact specimen; that belongs to the task
// that closes the hole (Spec Server 9fd58deb). It pins the wording, because the
// wording is the fix: the WARN may say recovery discarded nothing, and must not
// dress that up as proof the log is complete.
//
// It also asserts the line is not TRUNCATED. logging caps a msg at 1024 bytes
// and appends "...(truncated)", and the caveat is at the end — so a message that
// grows past the cap silently loses exactly the half this test exists to keep.
func TestSeqFloorMigrationWarningDoesNotClaimTheLogIsComplete(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, false)
	alpha := agentID(t, testBusID, "alpha")
	h, _ := openMintHub(t, dir, lg, nil, "", "alpha")
	mustMint(t, h, alpha, "send", "k-1")
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	// The pre-upgrade data directory: intact log, no floor FILE.
	if err := os.Remove(filepath.Join(dir, hub.SeqFloorFileName)); err != nil {
		t.Fatalf("removing %s to model a pre-upgrade data directory: %v", hub.SeqFloorFileName, err)
	}
	lg2 := openTestLog(t, dir, true)
	_, buf := openMintHub(t, dir, lg2, nil, "", "alpha")

	var warn string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "level=warn") && strings.Contains(line, "had no durable message sequence floor file") {
			warn = line
			break
		}
	}
	if warn == "" {
		t.Fatalf("backfilling the sequence floor emitted no level=warn line about the missing floor file; an operator is then told nothing at all about a start whose floor is derived rather than recorded.\nlog: %s", buf.String())
	}

	// The false all-clear, in the two forms it took. Neither may come back
	// without the consumed-index check that would make it true.
	for _, banned := range []string{
		"closes the window",
		"verified that recovery removed no records",
	} {
		if strings.Contains(warn, banned) {
			t.Fatalf("the migration WARN says %q. That claim is FALSE on a log truncated exactly on a record boundary: recovery discards nothing, so this check passes while records are missing and the derived floor sits below numbers /v1/mint already handed out. Measured on a real specimen the guard fails at 22 of 22 record boundaries, 13 of them reissuing a delivered sequence and one reissuing a message id end to end — systematic, not a corner case. Do not restore it before Spec Server task 9fd58deb lands the consumed-index check.\nWARN: %s", banned, warn)
		}
	}

	// What it must say instead: the true claim, and the caveat that bounds it.
	//
	// Each of these appears ONLY in the caveat. An earlier draft asserted
	// "already handed out", which also matches the message's second sentence and
	// was present in the false-all-clear version too — so it pinned nothing.
	for _, want := range []string{
		"discarded no records", // the discard check, which IS what ran
		"record boundary",      // the damage that check cannot see
		"sit BELOW numbers",    // the consequence: the floor may be too low
		"UNPROVEN",             // the standing of the floor it just reported
	} {
		if !strings.Contains(warn, want) {
			t.Fatalf("the migration WARN does not contain %q, so it does not tell the operator that a boundary-exact truncation is invisible to the check it just ran and the floor it reports may be below numbers already issued.\nWARN: %s", want, warn)
		}
	}

	if strings.Contains(warn, "(truncated)") {
		t.Fatalf("the migration WARN exceeded logging's 1024-byte msg cap and was truncated, which drops the caveat at its end — the operator keeps the reassuring half and loses the qualifying half.\nWARN: %s", warn)
	}
}

// TestOpenRefusesACorruptSeqFloorFile pins the other half of the missing/corrupt
// judgement: a file that EXISTS and does not verify is FATAL and is NEVER
// regenerated.
//
// Regenerating it would resume the sequence below numbers already handed out and
// already signed, silently. A loud, recoverable startup failure beats that every
// time — and the operator is given a one-step remedy, so the bus is not bricked.
func TestOpenRefusesACorruptSeqFloorFile(t *testing.T) {
	corruptions := []struct {
		name    string
		content string
	}{
		{"a truncated file with no header", "agent-bus-message-seq-floor"},
		{"a foreign file", "hello\nfloor 5\n"},
		{"an unknown on-disk format version", "agent-bus-message-seq-floor v99 sha256=00\nfloor 5\n"},
		{"a body that does not match the digest", "agent-bus-message-seq-floor v5 sha256=" + strings.Repeat("00", 32) + "\nfloor 5\n"},
	}
	for _, tc := range corruptions {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lg := openTestLog(t, dir, true)
			path := filepath.Join(dir, hub.SeqFloorFileName)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("writing the corrupt fixture: %v", err)
			}

			h, _, err := tryOpenMintHub(t, dir, lg, nil, "", "alpha")
			if err == nil {
				t.Fatalf("hub.Open accepted a corrupt %s and returned a usable hub (%p); it would then resume the sequence from whatever it could salvage, below numbers already handed out and already signed", hub.SeqFloorFileName, h)
			}
			if !errors.Is(err, hub.ErrSeqFloorFileCorrupt) {
				t.Fatalf("hub.Open error = %v, want one satisfying errors.Is(err, hub.ErrSeqFloorFileCorrupt) so the HTTP layer and an operator can tell it from an I/O failure", err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("the refusal does not name %s, so an operator does not know which file to move aside: %v", path, err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s after the refusal: %v", path, err)
			}
			if string(got) != tc.content {
				t.Fatalf("the corrupt %s was REWRITTEN by the failed open (now %q); it must never be regenerated, because regenerating it is exactly the silent rewind it exists to prevent", hub.SeqFloorFileName, got)
			}
		})
	}
}

// TestOpenRefusesADurableHubWithNoDataDir pins the wiring failure, which is
// silent in every other way.
//
// A hub with a durable log and no data directory would mint, burn its numbers
// only inside the log, pass every health check, and reissue the lot after a
// quarantine. "Serves the defect" must not be reachable by forgetting a field —
// the same rule, and the same reasoning, as the missing-roster refusal.
func TestOpenRefusesADurableHubWithNoDataDir(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	path := lg.Path()
	h, err := hub.Open(hub.Options{
		BusID:     testBusID,
		Durable:   lg,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: lg.Recovered().NextIndex,
		Roster:    hub.NewStaticRoster(),
		// DataDir deliberately omitted.
	})
	if err == nil {
		t.Fatalf("hub.Open with a durable log and no DataDir returned a usable hub (%p); it can mint numbers it cannot durably burn", h)
	}
	if !strings.Contains(err.Error(), hub.SeqFloorFileName) {
		t.Fatalf("hub.Open error = %q, want it to name %s so the wiring mistake is obvious in a composition root", err, hub.SeqFloorFileName)
	}
}

// TestSeqFloorAtTheEndOfTheSequenceSpaceFailsClosed pins what happens at the top
// of the sequence space — and it REVERSES a decision this test used to encode.
//
// # What it used to assert, and why that was wrong
//
// It used to seed math.MaxUint64 and require the hub to OPEN, on the stated
// grounds that "an exhausted id space is a legitimate state to recover, not
// corruption. Refusing to start here would be indistinguishable from a damaged
// file and would send an operator after the wrong problem."
//
// That reasoning treats a physically unreachable state as legitimate — the test's
// own comment conceded the fixture was "the only way to reach this state without
// issuing 1.8e19 sequences" — and in doing so it left the file's most damaging
// forgery indistinguishable from a normal start. A security review demonstrated
// the consequence: because the file's digest is UNKEYED, anyone who can write the
// data directory writes floor=2^64-1 with a valid digest, and the bus then boots
// completely healthy (/healthz ok, roster intact, log replayed, no warning) while
// every /v1/mint answers 500 for ever, across every restart, because the file
// persists. The operator is sent after the wrong problem WITH NO MESSAGE AT ALL.
//
// # Why refusing is better on availability too, not just security
//
// The comparison is not "refuse" versus "keep working". It is:
//
//	adopt  -> a bus that serves, enrols, and cannot deliver a single message,
//	          permanently, with no diagnosis anywhere;
//	refuse -> a bus that stops, names the file, the value, and a one-step remedy.
//
// A legitimately exhausted bus is equally unable to mint either way, so refusing
// costs it nothing it still had. See maxPlausibleSeqFloor for the bound and for
// the one honest caveat about the word "tampered".
//
// # What was lost, and why that is acceptable
//
// The saturation branch in ensureSeqFloorLocked (target wraps -> clamp to
// MaxUint64) can no longer be reached through the file, so it is no longer
// exercised here. It is now unreachable BY CONSTRUCTION rather than merely
// untested: reaching it requires a floor above 2^56 to be loadable, and it is
// not. The boundary that IS reachable is pinned instead, below and in
// seqfloorbound_test.go.
//
// The fixture is still written BY THE TEST, in the documented format, because
// that proves the file is readable and writable by something other than its own
// writer — an operator with a recovery job to do.
func TestSeqFloorAtTheEndOfTheSequenceSpaceFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeSeqFloorFixture(t, dir, math.MaxUint64)
	lg := openTestLog(t, dir, true)

	_, err := hub.Open(hub.Options{
		BusID:     testBusID,
		DataDir:   dir,
		Durable:   lg,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(lg.Path(), fn) },
		NextIndex: lg.Recovered().NextIndex,
		Roster:    hub.NewStaticRoster(),
	})
	if err == nil {
		t.Fatalf("hub.Open ADOPTED a floor of MaxUint64; the bus would start looking perfectly healthy and then fail every send for ever, with no diagnosis and no remedy")
	}
	if !errors.Is(err, hub.ErrSeqFloorFileCorrupt) {
		t.Fatalf("hub.Open error = %v, want it to wrap ErrSeqFloorFileCorrupt so the caller treats it as fatal", err)
	}
	for _, want := range []string{"implausibly high", "TAMPERED WITH", hub.SeqFloorFileName} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not mention %q, so an operator cannot act on it: %v", want, err)
		}
	}

	// The refusal must NOT rewrite the file. Leaving the bytes alone is what
	// lets an operator see what was actually on disk — and lowering a floor is
	// the one thing this file must never do, even to a value it distrusts.
	if floor, _ := readSeqFloor(t, dir); floor != math.MaxUint64 {
		t.Fatalf("the durable floor is now %d, want it unchanged at MaxUint64; a refusal that edits the file destroys the evidence and could LOWER a floor", floor)
	}
}

// TestSeqFloorAtThePlausibleBoundStillMints is the false-positive guard on the
// test above: the bound refuses implausible values WITHOUT refusing large
// legitimate ones. Without it, maxPlausibleSeqFloor could be 0 and every
// assertion above would still pass.
func TestSeqFloorAtThePlausibleBoundStillMints(t *testing.T) {
	dir := t.TempDir()
	const justBelowTheBound = uint64(1<<56) - 1
	writeSeqFloorFixture(t, dir, justBelowTheBound)
	lg := openTestLog(t, dir, true)
	alpha := agentID(t, testBusID, "alpha")

	h, _ := openMintHub(t, dir, lg, nil, "", "alpha")
	m := mustMint(t, h, alpha, "send", "k-1")
	if m.Seq <= justBelowTheBound {
		t.Fatalf("Mint returned sequence %d, at or below the durable floor %d it inherited", m.Seq, justBelowTheBound)
	}
}

// writeSeqFloorFixture writes a floor file by hand, in the documented on-disk
// format, digest and all. See TestSeqFloorAtTheEndOfTheSequenceSpaceFailsClosed
// for why a test writes this rather than driving the writer.
func writeSeqFloorFixture(t *testing.T, dir string, floor uint64) {
	t.Helper()
	body := fmt.Sprintf("floor %d\n", floor)
	sum := sha256.Sum256([]byte(body))
	data := fmt.Sprintf("agent-bus-message-seq-floor v5 sha256=%x\n%s", sum, body)
	if err := os.WriteFile(filepath.Join(dir, hub.SeqFloorFileName), []byte(data), 0o600); err != nil {
		t.Fatalf("writing the sequence floor fixture: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Spending a reservation: ErrUnknownMint and ErrMintMismatch
// ---------------------------------------------------------------------------

// TestSendWithoutAReservationIsErrUnknownMint covers the sentinel that, before
// this file, appeared in the suite only inside COMMENTS.
//
// It is the ROUTINE refusal — a restart or an expiry lost the in-memory
// reservation — and its remedy (re-mint under the same key, re-sign, re-send)
// is safe because a message that DID become durable is answered from the
// applied-key table before the mint is ever consulted.
func TestSendWithoutAReservationIsErrUnknownMint(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	alpha := agentID(t, testBusID, "alpha")
	beta := agentID(t, testBusID, "beta")
	h, _ := openMintHub(t, dir, lg, nil, "", "alpha", "beta")

	_, err := h.Send(hub.SendRequest{
		Sender:         alpha,
		To:             beta,
		Body:           []byte("no reservation was ever made for this key"),
		IdempotencyKey: "k-never-minted",
		SignedMint: hub.SignedMint{
			MessageID:          "testbus-1",
			Seq:                1,
			TimestampUnixMilli: fixtureTimestampMs,
			Signature:          fixtureSignature(),
		},
	})
	if !errors.Is(err, hub.ErrUnknownMint) {
		t.Fatalf("Send with no reservation = %v, want ErrUnknownMint; a send that allocates its own sequence on the fly is the reserve-then-send path bypassed entirely", err)
	}
	if n, _, _, _, _ := h.Store().Stats(); n != 0 {
		t.Fatalf("a send refused for want of a reservation still stored %d messages; nothing may be written before the reservation is matched", n)
	}
}

// TestSendPresentingTheWrongAssignmentIsErrMintMismatch covers the sentinel that
// is NEVER routine: the client was handed an assignment and presented a
// different one.
//
// Invariant 1 is the whole answer — a client-supplied id is input to be
// validated, never an identity to be trusted — so the MINT wins and the send is
// refused. The two halves (wrong sequence, wrong message id) are separate cases
// because a check on only one of them would let the other through, and the
// canonical signing format covers both.
func TestSendPresentingTheWrongAssignmentIsErrMintMismatch(t *testing.T) {
	alpha := agentID(t, testBusID, "alpha")
	beta := agentID(t, testBusID, "beta")

	cases := []struct {
		name string
		// mangle turns the honest assignment into the one the client presents.
		mangle func(hub.Mint) hub.SignedMint
	}{
		{
			name: "a sequence the bus did not mint",
			mangle: func(m hub.Mint) hub.SignedMint {
				return hub.SignedMint{MessageID: m.MessageID, Seq: m.Seq + 1, TimestampUnixMilli: fixtureTimestampMs, Signature: fixtureSignature()}
			},
		},
		{
			name: "a message id the bus did not mint",
			mangle: func(m hub.Mint) hub.SignedMint {
				return hub.SignedMint{MessageID: "testbus-999999", Seq: m.Seq, TimestampUnixMilli: fixtureTimestampMs, Signature: fixtureSignature()}
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lg := openTestLog(t, dir, true)
			h, _ := openMintHub(t, dir, lg, nil, "", "alpha", "beta")

			m := mustMint(t, h, alpha, "send", "k-1")
			_, err := h.Send(hub.SendRequest{
				Sender:         alpha,
				To:             beta,
				Body:           []byte("an assignment of the client's own choosing"),
				IdempotencyKey: "k-1",
				SignedMint:     tc.mangle(m),
			})
			if !errors.Is(err, hub.ErrMintMismatch) {
				t.Fatalf("Send presenting a mangled assignment = %v, want ErrMintMismatch; the bus must take its own minted values, never the client's", err)
			}
			if n, _, _, _, _ := h.Store().Stats(); n != 0 {
				t.Fatalf("a mismatched send still stored %d messages", n)
			}

			// The reservation SURVIVES the refusal. It is not consumed until the
			// message it names is durable, so an honest retry with the correct
			// assignment — and the signature the client already computed — still
			// works. Punishing a client by burning its reservation on a refusal
			// would make every transient failure cost a re-sign.
			if _, err := h.Send(hub.SendRequest{
				Sender:         alpha,
				To:             beta,
				Body:           []byte("an assignment of the client's own choosing"),
				IdempotencyKey: "k-1",
				SignedMint: hub.SignedMint{
					MessageID:          m.MessageID,
					Seq:                m.Seq,
					TimestampUnixMilli: fixtureTimestampMs,
					Signature:          fixtureSignature(),
				},
			}); err != nil {
				t.Fatalf("the honest retry after a mismatch was refused (%v); the reservation must survive a refused send", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The bounds
// ---------------------------------------------------------------------------

// TestMintBoundsFailClosedAndNeverEvict covers both bounds on the
// outstanding-mint table and, more importantly, the thing they must NOT do.
//
// Evicting somebody else's reservation to make room would take a sequence back
// from a client that has already SIGNED it: that client's next send is refused
// for a reason it cannot see, caused entirely by a stranger. So both bounds fail
// CLOSED, and the test asserts the eviction has not happened by re-minting the
// FIRST key and requiring the original assignment back.
//
// The per-agent bound is checked before the bus-wide one, and the order matters:
// an agent at its own share must be told it is ITS fault, not that the bus is
// full, or an operator goes looking at the bus instead of at one client.
func TestMintBoundsFailClosedAndNeverEvict(t *testing.T) {
	t.Run("the per-agent bound", func(t *testing.T) {
		dir := t.TempDir()
		lg := openTestLog(t, dir, true)
		alpha := agentID(t, testBusID, "alpha")
		h, _ := openMintHub(t, dir, lg, nil, "", "alpha")

		first := mustMint(t, h, alpha, "send", "k-0")
		for i := 1; i < hub.MaxOutstandingMintsPerAgent; i++ {
			mustMint(t, h, alpha, "send", fmt.Sprintf("k-%d", i))
		}

		_, err := h.Mint(hub.MintRequest{Sender: alpha, Op: "send", IdempotencyKey: "k-one-too-many"})
		if !errors.Is(err, hub.ErrAgentQuota) {
			t.Fatalf("the %dth outstanding mint for one agent = %v, want ErrAgentQuota", hub.MaxOutstandingMintsPerAgent+1, err)
		}
		if !errors.Is(err, hub.ErrCapacity) {
			t.Fatalf("the per-agent refusal does not also satisfy ErrCapacity, so the HTTP layer's capacity mapping would miss it: %v", err)
		}

		again, err := h.Mint(hub.MintRequest{Sender: alpha, Op: "send", IdempotencyKey: "k-0"})
		if err != nil {
			t.Fatalf("re-minting the FIRST key after the bound was hit failed (%v); it must still be in the table — nothing is evicted", err)
		}
		if again.Seq != first.Seq || again.MessageID != first.MessageID {
			t.Fatalf("re-minting k-0 returned %s/%d, want the original %s/%d; a reservation that changes under a client invalidates the signature it already computed", again.MessageID, again.Seq, first.MessageID, first.Seq)
		}
		if !again.Replayed {
			t.Fatalf("re-minting an outstanding key did not report Replayed; the caller cannot tell a fresh allocation from a returned one, and invariant 10 requires the retry be answered, not re-applied")
		}
	})

	t.Run("the bus-wide bound", func(t *testing.T) {
		dir := t.TempDir()
		lg := openTestLog(t, dir, true)

		// One more agent than the bus-wide bound divided by the per-agent share, so
		// the LAST agent's FIRST mint hits the bus-wide bound rather than its own.
		// That is what makes the two errors distinguishable at all.
		perAgent := hub.MaxOutstandingMintsPerAgent
		agents := make([]string, 0, hub.MaxOutstandingMints/perAgent+1)
		for i := 0; i <= hub.MaxOutstandingMints/perAgent; i++ {
			agents = append(agents, fmt.Sprintf("agent%04d", i))
		}
		h, _ := openMintHub(t, dir, lg, nil, "", agents...)

		for i := 0; i < hub.MaxOutstandingMints/perAgent; i++ {
			sender := agentID(t, testBusID, agents[i])
			for j := 0; j < perAgent; j++ {
				mustMint(t, h, sender, "send", fmt.Sprintf("k-%d-%d", i, j))
			}
		}

		last := agentID(t, testBusID, agents[len(agents)-1])
		_, err := h.Mint(hub.MintRequest{Sender: last, Op: "send", IdempotencyKey: "k-over"})
		if !errors.Is(err, hub.ErrCapacity) {
			t.Fatalf("the mint past the bus-wide bound = %v, want ErrCapacity", err)
		}
		if errors.Is(err, hub.ErrAgentQuota) {
			t.Fatalf("a bus-wide refusal was reported as a per-agent one (%v); this agent holds NO reservations and would be blamed for a bus-wide limit", err)
		}
	})
}

// ---------------------------------------------------------------------------
// MintTTL
// ---------------------------------------------------------------------------

// TestMintTTLExpiry is the test whose absence hid a real bug.
//
// mint.go claimed expiry "runs on EVERY mint and EVERY send". It did not:
// publish never called expireMintsLocked, so an expired reservation was still
// spendable and Mint.ExpiresAt — a value this bus RETURNS TO CLIENTS — was not a
// fact. Nothing caught it because every other test in the repository mints and
// sends in the same instant.
//
// The fix was to the CODE, not the comment, and the reasoning is worth keeping:
// honouring a reservation past the expiry the bus published is not generosity,
// it makes a documented bound unobservable, and a bound nobody can observe is
// one that silently stops holding. Expiring costs a client nothing it was
// promised — ErrUnknownMint is documented as routine and its remedy cannot
// double-apply — while NOT expiring makes MintTTL decorative.
//
// Both directions are asserted. A test that only proved "expires eventually"
// would pass for an implementation that expired everything immediately, which
// would refuse every honest send on a slow link.
func TestMintTTLExpiry(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	alpha := agentID(t, testBusID, "alpha")
	beta := agentID(t, testBusID, "beta")

	// A clock the test drives. It starts in the past by an hour so the fixture
	// roster's enrolment epoch (fixtureEnrolledAt) still precedes every message —
	// otherwise store.Message.VisibleTo would hide the traffic and the assertions
	// below would be vacuous for the wrong reason.
	base := time.Now().Add(-time.Hour)
	offset := time.Duration(0)
	now := func() time.Time { return base.Add(offset) }

	h, _ := openMintHub(t, dir, lg, now, "", "alpha", "beta")

	send := func(key string, m hub.Mint) error {
		_, err := h.Send(hub.SendRequest{
			Sender:         alpha,
			To:             beta,
			Body:           []byte("body for " + key),
			IdempotencyKey: key,
			SignedMint: hub.SignedMint{
				MessageID:          m.MessageID,
				Seq:                m.Seq,
				TimestampUnixMilli: fixtureTimestampMs,
				Signature:          fixtureSignature(),
			},
		})
		return err
	}

	// (a) A reservation is honoured right up to its expiry. Nothing about a slow
	// client is a fault.
	live := mustMint(t, h, alpha, "send", "k-live")
	if want := base.Add(hub.MintTTL); !live.ExpiresAt.Equal(want) {
		t.Fatalf("Mint.ExpiresAt = %s, want %s (now + MintTTL); a client is told this value and plans around it", live.ExpiresAt, want)
	}
	offset = hub.MintTTL - time.Second
	if err := send("k-live", live); err != nil {
		t.Fatalf("a send one second BEFORE the published expiry was refused (%v); expiring early refuses honest clients for nothing", err)
	}

	// (b) Past the expiry the reservation is GONE, and the send is refused with
	// the routine sentinel rather than silently accepted.
	offset = 0
	stale := mustMint(t, h, alpha, "send", "k-stale")
	offset = hub.MintTTL + time.Second
	err := send("k-stale", stale)
	if !errors.Is(err, hub.ErrUnknownMint) {
		t.Fatalf("a send with a reservation %s past its published expiry = %v, want ErrUnknownMint. MintTTL is a bound this bus publishes in Mint.ExpiresAt; honouring a reservation past it makes that value a fiction and the bound untestable", time.Second, err)
	}

	// (c) The remedy works, and the expired NUMBER stays burned: re-minting under
	// the same key yields a NEW, strictly higher sequence. Expiry frees a table
	// slot, never a number.
	fresh := mustMint(t, h, alpha, "send", "k-stale")
	if fresh.Seq <= stale.Seq {
		t.Fatalf("re-minting after an expiry returned sequence %d, at or below the expired %d; expiry must never un-burn a number", fresh.Seq, stale.Seq)
	}
	if fresh.Replayed {
		t.Fatalf("re-minting after an expiry reported Replayed; the original reservation is gone, so this is a fresh allocation and must say so")
	}
	if err := send("k-stale", fresh); err != nil {
		t.Fatalf("the documented remedy (re-mint under the same key, re-sign, re-send) was refused: %v", err)
	}
}
