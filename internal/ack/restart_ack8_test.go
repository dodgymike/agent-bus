// ACK-8's RESTART AND REPLAY EVIDENCE: the delivery lifecycle table exercised
// against a REAL on-disk wal.Log, across a real reopen, with real damage.
//
// # WHY THIS FILE EXISTS AT ALL, WHEN internal/ack ALREADY HAD 40 TESTS
//
// Every pre-existing test in this package writes through `fakeLog`
// (store_test.go) — an in-memory slice of wal.Entry. It never frames, never
// MACs, never fsyncs, and its `replayFrom` hands the store back exactly the
// entries it was given. A stub like that is the right tool for the state
// machine, and it is structurally incapable of producing the three things this
// task is about: a torn record, a discard, and an index that might rewind.
// `TestRestartRebuildsFromTheLogAlone` is therefore a replay test, not a
// restart test — nothing in this package had ever opened a file.
//
// So the rule this file applies is CLAUDE.md's: "the code looks right" is not
// evidence for a durability claim, and neither is a stub that cannot fail.
// Everything below opens a real WAL in a throwaway directory, closes it, damages
// it where the test says, and opens it again.
//
// # WHICH INVARIANT EACH TEST IS THE EVIDENCE FOR
//
//	1  ids/indices are never reused, and recovery advances past a hole rather
//	   than rewinding into it  -> TestAckTornTailIsDiscardedLoudlyAndTheIndexNeverRewinds
//	4  nothing acknowledged before durable                 -> ...RebuildsEveryStateExactly
//	5  recovery yields a PREFIX of accepted history        -> the torn-tail row is ABSENT
//	6  damaged records are discarded, the bus still starts,
//	   and every discard is LOGGED loudly and specifically -> both damage tests
//	10 the three duplicate cases are not collapsed         -> ...ThreeCasesStayDistinct
//
// The kill -9 half of ACK-8 — a crash injected INSIDE the two-phase write path —
// lives in crash_ack8_test.go, which needs a SIGKILL and so carries a build tag.
package ack_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// Fixtures. Deliberately NOT the constants in store_test.go: that file is
// `package ack` and this one is `package ack_test`, which is what proves the
// EXPORTED surface is enough to rebuild this table from disk.
// ---------------------------------------------------------------------------

const (
	ack8Sender    = "testbus.alpha-1"
	ack8Recipient = "testbus.beta-1"
)

// ack8Key is the correlation key for pair n. It is a message id — `<bus>-<n>` —
// because §3 makes the correlation key the message id and Record.validate
// parses it as one.
func ack8Key(n int) string { return fmt.Sprintf("testbus-%d", n) }

// ---------------------------------------------------------------------------
// A capturing logger. The invariant-6 assertions are ABOUT the log lines, so
// the log has to be readable by the test rather than discarded.
// ---------------------------------------------------------------------------

type ack8Log struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *ack8Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *ack8Log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// lines returns only the lines at level=error, which is what "loudly" is
// measured as. A discard reported at debug is a silent discard with extra steps.
func (l *ack8Log) errorLines() []string {
	var out []string
	for _, ln := range strings.Split(l.String(), "\n") {
		if strings.Contains(ln, "level=error") {
			out = append(out, ln)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// indexSpy sits between the Store and the real *wal.Log and remembers the
// indices the log actually handed out.
//
// It exists because invariant 1 is a claim about INDICES, and Store.Accept
// returns only an error. Without observing the wal.Committed there is no way to
// assert "the index that was handed out after recovery is above every index
// handed out before it" — only to assert the weaker, and much less useful,
// "recovery did not crash".
// ---------------------------------------------------------------------------

type indexSpy struct {
	l *wal.Log

	mu      sync.Mutex
	highest uint64
	last    wal.Committed
	n       int
}

func (s *indexSpy) Write(e wal.Entry) (wal.Committed, error) {
	c, err := s.l.Write(e)
	s.mu.Lock()
	if err == nil {
		s.n++
		s.last = c
		if c.CommitIndex > s.highest {
			s.highest = c.CommitIndex
		}
		if c.PrepareIndex > s.highest {
			s.highest = c.PrepareIndex
		}
	}
	s.mu.Unlock()
	return c, err
}

func (s *indexSpy) highestIndex() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.highest
}

func (s *indexSpy) lastCommitted() wal.Committed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// ---------------------------------------------------------------------------
// openAck8 opens a REAL wal.Log over dir with a fresh ack.Store registered as
// its applier, exactly as cmd/agent-bus does: build the store, Open (which
// REPLAYS into it), then Attach.
//
// The Attach target is the spy, not the log, so every test can see the indices.
// In production both are the one *wal.Log.
// ---------------------------------------------------------------------------

func openAck8(t *testing.T, dir string) (*ack.Store, *wal.Log, *indexSpy, *ack8Log) {
	t.Helper()
	cap := &ack8Log{}
	lg := logging.New(cap, logging.LevelDebug)

	st := ack.NewStore(ack.Options{Logger: lg})
	l, err := wal.Open(wal.LogOptions{Dir: dir, Logger: lg, Applier: st})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v\nInvariant 6: recovery ALWAYS reaches a running server. A bus that refuses to boot over its own log is the failure this rule exists to prevent.", dir, err)
	}
	spy := &indexSpy{l: l}
	if err := st.Attach(spy); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return st, l, spy, cap
}

func mustAccept8(t *testing.T, s *ack.Store, key, recipient string) {
	t.Helper()
	if err := s.Accept(key, ack8Sender, recipient); err != nil {
		t.Fatalf("Accept(%s -> %s): %v", key, recipient, err)
	}
}

func mustSettle8(t *testing.T, s *ack.Store, key, recipient string, state ack.State, class ack.Class, by ack.Attestation) {
	t.Helper()
	if err := s.Settle(key, recipient, state, class, by); err != nil {
		t.Fatalf("Settle(%s -> %s, %s): %v", key, recipient, state, err)
	}
}

// walPath is the file the tests damage.
func walPath(dir string) string { return filepath.Join(dir, wal.WALFileName) }

// ---------------------------------------------------------------------------
// 1. THE BASELINE: a real restart reproduces every row exactly.
// ---------------------------------------------------------------------------

// ack8Row is what a restart must reproduce. Comparing the WHOLE record rather
// than just the state is the point: a restart that recovered `refused` but lost
// its class would satisfy a state-only assertion while destroying the only field
// that says WHY, which invariant 6 rates as the actual defect.
type ack8Row struct {
	state ack.State
	class ack.Class
	by    ack.Attestation
}

// ack8Fixture writes one row in each of the five states and returns what the
// table must look like afterwards, keyed by correlation key.
func ack8Fixture(t *testing.T, s *ack.Store) map[string]ack8Row {
	t.Helper()
	want := map[string]ack8Row{}

	// p1: accepted, and left there.
	mustAccept8(t, s, ack8Key(1), ack8Recipient)
	want[ack8Key(1)] = ack8Row{state: ack.StateAccepted}

	// p2: accepted -> in_flight (E2).
	mustAccept8(t, s, ack8Key(2), ack8Recipient)
	if err := s.MarkInFlight(ack8Key(2), ack8Recipient); err != nil {
		t.Fatalf("MarkInFlight: %v", err)
	}
	want[ack8Key(2)] = ack8Row{state: ack.StateInFlight}

	// p3: accepted -> delivered (E5). Positive terminal: no class, §5.4.
	mustAccept8(t, s, ack8Key(3), ack8Recipient)
	mustSettle8(t, s, ack8Key(3), ack8Recipient, ack.StateDelivered, "", ack.AttestedByRecipientSignatureUnverified)
	want[ack8Key(3)] = ack8Row{state: ack.StateDelivered, by: ack.AttestedByRecipientSignatureUnverified}

	// p4: accepted -> refused (E6). Negative terminal: a RECIPIENT-emitted class.
	mustAccept8(t, s, ack8Key(4), ack8Recipient)
	mustSettle8(t, s, ack8Key(4), ack8Recipient, ack.StateRefused, ack.ClassRecipientRefusedPolicy, ack.AttestedByRecipientSignatureUnverified)
	want[ack8Key(4)] = ack8Row{state: ack.StateRefused, class: ack.ClassRecipientRefusedPolicy, by: ack.AttestedByRecipientSignatureUnverified}

	// p5: accepted -> undeliverable (E4). Negative terminal: a BUS-emitted class.
	mustAccept8(t, s, ack8Key(5), ack8Recipient)
	mustSettle8(t, s, ack8Key(5), ack8Recipient, ack.StateUndeliverable, ack.ClassNoRoute, ack.AttestedByPeerBus)
	want[ack8Key(5)] = ack8Row{state: ack.StateUndeliverable, class: ack.ClassNoRoute, by: ack.AttestedByPeerBus}

	return want
}

func assertRows(t *testing.T, s *ack.Store, want map[string]ack8Row, when string) {
	t.Helper()
	for key, w := range want {
		got, ok := s.Lookup(key, ack8Recipient)
		if !ok {
			t.Errorf("%s: Lookup(%s) reports NO ROW, want state %s. An acknowledged row that a restart cannot produce is invariant 5's acknowledged-but-lost case: recovery must reach a PREFIX of the accepted history, and a missing row makes it a proper subset instead.", when, key, w.state)
			continue
		}
		if got.State != w.state {
			t.Errorf("%s: %s state = %s, want %s", when, key, got.State, w.state)
		}
		if got.Class != w.class {
			t.Errorf("%s: %s class = %q, want %q. The class is the field that says WHY a negative terminal happened; losing it across a restart turns a reported failure into a silent one (invariant 6).", when, key, got.Class, w.class)
		}
		if got.AttestedBy != w.by {
			t.Errorf("%s: %s attested_by = %q, want %q. The status API must LABEL attestation rather than imply it (ACK-CONTRACT.md §6.3), so a restart that loses the label makes every recovered terminal unattributable.", when, key, got.AttestedBy, w.by)
		}
		if got.Sender != ack8Sender {
			t.Errorf("%s: %s sender = %q, want %q", when, key, got.Sender, ack8Sender)
		}
		if got.AcceptedAt.IsZero() {
			t.Errorf("%s: %s accepted_at is zero; it is the retention anchor for a non-terminal row and a zero anchor makes Expired report true for ever", when, key)
		}
		if w.state.Terminal() && got.SettledAt.IsZero() {
			t.Errorf("%s: %s is terminal (%s) but settled_at is zero; the terminal retention anchor is SettledAt, so a zero one retires the row on the next sweep", when, key, got.State)
		}
	}
	if n := s.Len(); n != len(want) {
		t.Errorf("%s: Len = %d, want %d", when, n, len(want))
	}
}

// TestAckRestartOnARealWALRebuildsEveryStateExactly is ACK-CONTRACT.md §14 D1's
// first clause: restart yields the SAME terminal state.
//
// It covers all five states, and it compares the whole record, because the two
// fields a state-only assertion would miss — class and attested_by — are the two
// that carry the reason and the provenance.
func TestAckRestartOnARealWALRebuildsEveryStateExactly(t *testing.T) {
	dir := t.TempDir()

	st, l, _, _ := openAck8(t, dir)
	want := ack8Fixture(t, st)
	assertRows(t, st, want, "before restart")
	before := snapshotTimes(t, st, want)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A SECOND, EMPTY Store. Nothing carries over in memory: everything below
	// came off the disk.
	st2, l2, _, _ := openAck8(t, dir)
	defer l2.Close()
	assertRows(t, st2, want, "after restart")

	// The timestamps must be reproduced too, not merely present. AcceptedAt is
	// the retention anchor, so a restart that re-stamped it would silently
	// extend every row's life by the downtime.
	for key, ts := range before {
		got, ok := st2.Lookup(key, ack8Recipient)
		if !ok {
			continue
		}
		if !got.AcceptedAt.Equal(ts.accepted) {
			t.Errorf("after restart: %s accepted_at = %s, want %s. Re-stamping the anchor on replay would push the retention window out by the downtime every time the bus restarted.",
				key, got.AcceptedAt.UTC(), ts.accepted.UTC())
		}
		if !got.SettledAt.Equal(ts.settled) {
			t.Errorf("after restart: %s settled_at = %s, want %s", key, got.SettledAt.UTC(), ts.settled.UTC())
		}
	}
}

type ack8Times struct{ accepted, settled time.Time }

func snapshotTimes(t *testing.T, s *ack.Store, want map[string]ack8Row) map[string]ack8Times {
	t.Helper()
	out := map[string]ack8Times{}
	for key := range want {
		r, ok := s.Lookup(key, ack8Recipient)
		if !ok {
			t.Fatalf("snapshotTimes: %s missing before the restart even began", key)
		}
		out[key] = ack8Times{accepted: r.AcceptedAt, settled: r.SettledAt}
	}
	return out
}

// ---------------------------------------------------------------------------
// 2. NO RESURRECTION ACROSS A RESTART.
// ---------------------------------------------------------------------------

// TestAckRestartCannotResurrectASettledRow is D1's second clause: no
// resurrection.
//
// The dangerous shape is not a replay bug, it is a LIVE retry arriving after a
// restart. The sender's client retries a send whose acknowledgement it lost; the
// row it names has since been delivered and settled. Accept must absorb that as
// invariant 10's legitimate retry — no error, nothing written — and must NOT
// move the row back to `accepted`, which would tell the sender a delivered
// message is still open.
func TestAckRestartCannotResurrectASettledRow(t *testing.T) {
	dir := t.TempDir()

	st, l, _, _ := openAck8(t, dir)
	mustAccept8(t, st, ack8Key(3), ack8Recipient)
	mustSettle8(t, st, ack8Key(3), ack8Recipient, ack.StateDelivered, "", ack.AttestedByRecipientSignatureUnverified)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2, l2, spy2, _ := openAck8(t, dir)
	defer l2.Close()

	if r, ok := st2.Lookup(ack8Key(3), ack8Recipient); !ok || r.State != ack.StateDelivered {
		t.Fatalf("after restart the row is (%v, present=%v), want delivered", r.State, ok)
	}
	writesBefore := spy2.writes()

	// The retry. It must be absorbed, not errored and not applied.
	if err := st2.Accept(ack8Key(3), ack8Sender, ack8Recipient); err != nil {
		t.Errorf("Accept after restart on a DELIVERED row returned %v, want nil. This is invariant 10's legitimate-retry case: the client lost an acknowledgement for an operation that demonstrably succeeded, and telling it the send failed would be a lie about durable history.", err)
	}
	r, ok := st2.Lookup(ack8Key(3), ack8Recipient)
	if !ok || r.State != ack.StateDelivered {
		t.Errorf("after the retry the row is (%v, present=%v), want it STILL delivered. Terminal is absorbing: a resurrected row reports a settled message as open.", r.State, ok)
	}
	if got := spy2.writes(); got != writesBefore {
		t.Errorf("the retry performed %d durable write(s); want 0. A legitimate retry returns the ORIGINAL result and re-applies nothing — re-writing it would grow the log without bound under a retrying client.", got-writesBefore)
	}
}

// ---------------------------------------------------------------------------
// 3. THE TORN TAIL — invariants 1, 5 and 6 together.
// ---------------------------------------------------------------------------

// TestAckTornTailIsDiscardedLoudlyAndTheIndexNeverRewinds is the centre of
// ACK-8.
//
// A partial write at the end of the log is the ordinary shape of a power cut: a
// record is half on the disk. This test produces exactly that by truncating the
// file into its final frame, and then asserts the FOUR things the invariants
// require, which are easy to conflate and are separated here on purpose:
//
//	(a) the bus STARTS.                      invariant 6 — availability wins.
//	(b) the discard is LOGGED, at ERROR,
//	    naming the record and the reason.    invariant 6 — silent discard is THE defect.
//	(c) the torn row is ABSENT, and every
//	    row before it is intact.             invariant 5 — recovery yields a PREFIX.
//	(d) the next index handed out is ABOVE
//	    every index handed out before the
//	    damage.                              invariant 1 — advance past the hole, NEVER rewind.
//
// (d) is the one with live precedent. A sibling agent found a bus re-issuing
// sequence 257 over a record already written at 1000 when a mint floor was
// missing; the same shape here would let a recovered bus write a NEW ack record
// at an index a DISCARDED one already carried. What stops it is the durable
// index floor in <data-dir>/wal-index-floor, and this test is what proves the
// ack plane is actually behind it rather than assumed to be.
func TestAckTornTailIsDiscardedLoudlyAndTheIndexNeverRewinds(t *testing.T) {
	dir := t.TempDir()

	st, l, spy, _ := openAck8(t, dir)
	// Four rows. The fourth is the one the tear destroys.
	for i := 1; i <= 4; i++ {
		mustAccept8(t, st, ack8Key(i), ack8Recipient)
	}
	// Every index the log handed out before the damage. Nothing at or below this
	// may EVER be handed out again, including the indices the tear discards.
	highestBeforeDamage := spy.highestIndex()
	tornIndex := spy.lastCommitted().CommitIndex
	if highestBeforeDamage == 0 {
		t.Fatalf("the spy observed no indices at all; the test cannot assert anything about index reuse")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// TEAR THE TAIL.
	//
	// NINE BYTES IS NOT ARBITRARY, and the bound that makes it safe is in the
	// format: wal.FrameHeaderSize is 48 (frameCoveredBytes 16 + MACSize 32,
	// internal/wal/format.go). Nine bytes therefore CANNOT remove a whole frame
	// — so this is always DAMAGE and never merely a shorter log — and cannot
	// reach back into the 16-byte covered header that carries the record's index,
	// type and length, so recovery can always still say WHICH record it threw
	// away. Both halves of assertion (b) below depend on that second property.
	//
	// It is also independent of the ack body's size, because the cut is from the
	// END and the last frame is a fixed-size COMMIT record.
	fi, err := os.Stat(walPath(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if int64(9) >= int64(wal.FrameHeaderSize) {
		t.Fatalf("this test cuts 9 bytes on the assumption that a frame header is larger than that, but wal.FrameHeaderSize is %d; the cut could now remove a whole frame and the test would no longer be tearing anything", wal.FrameHeaderSize)
	}
	if err := os.Truncate(walPath(dir), fi.Size()-9); err != nil {
		t.Fatalf("truncating into the final frame: %v", err)
	}

	// (a) THE BUS STARTS. openAck8 fails the test if Open errors.
	st2, l2, spy2, cap2 := openAck8(t, dir)
	defer l2.Close()

	// (b) THE DISCARD IS LOGGED, LOUDLY AND SPECIFICALLY.
	//
	// "Loudly" is level=error. "Specifically" is that it names the record index
	// it threw away — a line saying only "recovery completed with warnings" tells
	// an operator nothing about WHICH record is gone, and that is the silent
	// discard invariant 6 rates as the defect.
	//
	// ONLY record_index IS ASSERTED, and dropping the rest was deliberate.
	// Checking for `reason=` and `offset=` looks like extra rigour and is
	// TRUE BY CONSTRUCTION: wal's logDiscards emits both keys on every line it
	// writes, and `reason=` would match even an EMPTY reason. An assertion that
	// cannot fail is not evidence, and a list of them next to a real one makes
	// the real one look weaker than it is.
	var discard string
	for _, ln := range cap2.errorLines() {
		if strings.Contains(ln, "discarded a damaged record") {
			discard = ln
			break
		}
	}
	if discard == "" {
		t.Fatalf("recovery discarded a torn tail record and logged NO line at level=error saying so.\nInvariant 6: damaged records are discarded and the bus starts, but every discard must be logged loudly and specifically — SILENT DISCARD IS THE DEFECT, not the discard.\nfull log:\n%s", cap2.String())
	}
	if want := fmt.Sprintf("record_index=%d", tornIndex); !strings.Contains(discard, want) {
		t.Errorf("the discard line does not contain %q, so it does not identify WHICH record was thrown away:\n\t%s", want, discard)
	}

	// (c) A PREFIX OF ACCEPTED HISTORY. Rows 1-3 survive; row 4, whose commit
	// was torn, is absent — it was never acknowledged, so nothing promised it.
	for i := 1; i <= 3; i++ {
		r, ok := st2.Lookup(ack8Key(i), ack8Recipient)
		if !ok {
			t.Errorf("row %d is missing after recovery from a tail tear. The damage was at the END of the log; losing a record BEFORE it means recovery did not yield a prefix of accepted history (invariant 5).", i)
			continue
		}
		if r.State != ack.StateAccepted {
			t.Errorf("row %d recovered as %s, want accepted", i, r.State)
		}
	}
	if r, ok := st2.Lookup(ack8Key(4), ack8Recipient); ok {
		t.Errorf("row 4 is PRESENT as %s after its commit record was torn. A record whose COMMIT never survived was never accepted history; serving it would make recovery a superset of what was acknowledged rather than a prefix (invariant 5).", r.State)
	}

	// (d) THE INDEX ADVANCES PAST THE HOLE AND NEVER REWINDS.
	rec := l2.Recovered()
	if rec.NextIndex <= highestBeforeDamage {
		t.Errorf("Recovered().NextIndex = %d, but index %d was already handed out before the damage.\nInvariant 1: when recovery discards a record the sequence ADVANCES PAST THE HOLE; it never rewinds. Reissuing %d would write a NEW record at an index a discarded one already carried.",
			rec.NextIndex, highestBeforeDamage, rec.NextIndex)
	}

	// And prove it with an actual write, not only with the reported number: the
	// index that matters is the one the next append USES.
	mustAccept8(t, st2, ack8Key(99), ack8Recipient)
	got := spy2.lastCommitted()
	if got.PrepareIndex <= highestBeforeDamage {
		t.Errorf("the first write after recovery took prepare index %d, but %d was already handed out before the damage. This is index REUSE across a restart — invariant 1's absolute prohibition, and the exact shape of the live finding where a recovered bus re-issued sequence 257 over a record already written at 1000.",
			got.PrepareIndex, highestBeforeDamage)
	}
}

// ---------------------------------------------------------------------------
// 3b. AN *ACKNOWLEDGED* ROW LOST TO DAMAGED MEDIA — where 4 and 6 actually meet.
// ---------------------------------------------------------------------------

// TestAckAcknowledgedRowLostToMediaDamageStartsAnywayAndSaysSo is the case that
// makes invariant 4's 2026-08-02 NARROWING checkable rather than merely stated,
// and it is the one the torn-tail test above deliberately does not reach.
//
// The torn-tail test tears a record whose COMMIT never completed. Nothing was
// ever acknowledged for it, so no promise is broken and the narrowing is never
// engaged — which is exactly why it is not sufficient on its own.
//
// THIS test damages a record that WAS fully committed and fsynced, and whose
// caller WAS told the write succeeded. The two invariants then say different
// things about the same byte, and both are right:
//
//	invariant 4: we never lose acknowledged data THROUGH OUR OWN WRITE PATH.
//	             It does NOT promise acknowledged data survives damaged MEDIA.
//	invariant 6: recovery ALWAYS reaches a running server — availability wins —
//	             and every discard is logged loudly and specifically.
//
// So the correct behaviour is the uncomfortable one: the acknowledged row is
// GONE, the bus starts anyway, and the loss is stated at ERROR rather than
// swallowed. A bus that refused to boot here would be "safer" only in the sense
// that a bus held hostage by one bad sector is safe. A bus that started and said
// nothing would be the actual defect.
//
// If reading this makes invariants 4 and 6 look contradictory, that is the
// narrowing rather than a bug — INVARIANTS.md says so directly.
func TestAckAcknowledgedRowLostToMediaDamageStartsAnywayAndSaysSo(t *testing.T) {
	dir := t.TempDir()

	st, l, spy, _ := openAck8(t, dir)
	// FIVE acknowledged rows. Every one of these Accept calls RETURNED NIL,
	// which under invariant 4 means "this row is on stable storage".
	for i := 1; i <= 5; i++ {
		mustAccept8(t, st, ack8Key(i), ack8Recipient)
	}
	highestBeforeDamage := spy.highestIndex()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// DAMAGE THE MEDIA UNDER AN ACKNOWLEDGED RECORD, in the MIDDLE of the log.
	//
	// The record is located by its own correlation key, which appears verbatim
	// in the record body, so the damage lands inside a known record rather than
	// at a guessed offset. Flipping one bit there breaks the keyed MAC over that
	// frame — which is the point of a MAC rather than a CRC (invariant 6): the
	// damage is DETECTED rather than served as though it were history.
	raw, err := os.ReadFile(walPath(dir))
	if err != nil {
		t.Fatalf("reading the wal: %v", err)
	}
	victim := ack8Key(2)
	at := bytes.Index(raw, []byte(victim))
	if at < 0 {
		t.Fatalf("could not find %q in the wal; the record body is supposed to carry the correlation key verbatim, so this test can no longer aim its damage", victim)
	}
	if at == bytes.LastIndex(raw, []byte(victim)) {
		// One occurrence is what we expect (one record for this pair). If that
		// ever changes the damage may land somewhere else than intended.
		raw[at] ^= 0xFF
	} else {
		t.Fatalf("%q appears more than once in the wal; the damage would be ambiguous", victim)
	}
	if err := os.WriteFile(walPath(dir), raw, 0o600); err != nil {
		t.Fatalf("writing the damaged wal: %v", err)
	}

	// THE BUS STARTS. openAck8 fails the test if Open returns an error.
	st2, l2, spy2, cap2 := openAck8(t, dir)
	defer l2.Close()

	// THE LOSS IS STATED, AT ERROR, AND IT SAYS AN ACKNOWLEDGED WRITE IS GONE.
	//
	// This is the whole of invariant 6's requirement here: discarding is
	// sanctioned, discarding SILENTLY is not.
	//
	// The specific line is pinned rather than "some error line exists", because
	// the two are not the same claim. Breaking the MAC over this frame orphans
	// its COMMIT, and it is the COMMIT discard that carries the ERROR and the
	// words about acknowledged loss; the matching prepare discard is only a WARN.
	// An assertion satisfied by ANY error line would also be satisfied by a
	// WARN-level tail truncation logged for unrelated reasons, and would stop
	// distinguishing "an acknowledged write was lost and recovery said so" from
	// "recovery said something".
	var loss string
	for _, ln := range cap2.errorLines() {
		if strings.Contains(ln, "wal discarded a damaged record") && strings.Contains(ln, "record_type=commit") {
			loss = ln
			break
		}
	}
	if loss == "" {
		t.Fatalf("an ACKNOWLEDGED delivery lifecycle row was lost to media damage and recovery logged NO level=error line naming a discarded COMMIT record.\nInvariant 6 rates the SILENT discard as the defect — it was rated P0 — not the discard itself. Without this line an operator has no way to learn that an acknowledged row is gone, which is the whole reason invariant 4's 2026-08-02 narrowing is tolerable.\nfull log:\n%s", cap2.String())
	}
	if !strings.Contains(loss, "an acknowledged write is lost here") {
		t.Errorf("the discard line does not say that an ACKNOWLEDGED write was lost:\n\t%s\nThat phrase is the difference between reporting damage and reporting DATA LOSS, and it is the only thing telling an operator this was not a harmless uncommitted tail.", loss)
	}

	// AND THE ROW IS ACTUALLY GONE — the honest, documented outcome. Asserted so
	// that this test cannot quietly become a "nothing happened" test if the
	// damage stops landing where it is aimed.
	if _, ok := st2.Lookup(victim, ack8Recipient); ok {
		t.Errorf("the damaged row %s is still being served. Either the damage missed, or a corrupt record was admitted as history — and admitting it would mean the keyed MAC is not actually gating replay.", victim)
	}

	// THE REST OF THE TABLE SURVIVES — ALL FOUR OF THEM, ENUMERATED.
	//
	// Damage costs the records it damaged and not the log behind them: recovery
	// searches forward for the next intact record rather than treating the first
	// damage as the end of the log.
	//
	// EACH SURVIVOR IS NAMED RATHER THAN COUNTED, and the difference is not
	// cosmetic. A `survivors == 0` check CANNOT FIRE for the regression its
	// message describes: row 1 sits BEFORE the damage and survives any
	// prefix-shaped recovery, so "recovery stopped at the first damage" — which
	// would silently lose rows 3, 4 and 5 — would leave that count at 1 and the
	// assertion green. Naming the rows is what makes the claim checkable.
	for i := 1; i <= 5; i++ {
		if ack8Key(i) == victim {
			continue
		}
		if _, ok := st2.Lookup(ack8Key(i), ack8Recipient); !ok {
			t.Errorf("row %s was lost along with the damaged row %s. One bad region must cost the records it damaged and NOTHING behind them — recovery searches forward for the next intact record, and treating the first damage as the end of the log would turn a single flipped bit into total loss of the delivery lifecycle history from that point on.",
				ack8Key(i), victim)
		}
	}

	// AND STILL NO INDEX REUSE. This is the invariant-1 half, re-checked on the
	// damage path rather than assumed to carry over from the tail path: a
	// discard is precisely when a naive implementation is most tempted to reuse
	// the freed number.
	mustAccept8(t, st2, ack8Key(99), ack8Recipient)
	if got := spy2.lastCommitted(); got.PrepareIndex <= highestBeforeDamage {
		t.Errorf("the first write after recovering from media damage took prepare index %d, but %d had already been handed out. Invariant 1 is absolute and was REAFFIRMED WITHOUT NARROWING: recovery advances past the hole, it never rewinds into it.",
			got.PrepareIndex, highestBeforeDamage)
	}
}

// ---------------------------------------------------------------------------
// 4. AN UNDECODABLE ack RECORD — ack's OWN invariant-6 obligation.
// ---------------------------------------------------------------------------

// TestAckUndecodableRecordIsDiscardedLoudlyAndTheTableSurvives exercises the
// discard path that belongs to THIS package rather than to wal.
//
// A torn frame never reaches Store.Apply — wal rejects it at the framing layer.
// The record that DOES reach Apply and cannot be used is one whose frame is
// perfect, whose MAC verifies, and whose BODY this schema version refuses:
// DecodeRecord's version gate (record.go) rejects any record_version that is
// not 1, on purpose, so a
// record written by a future binary is never read with this version's field
// meanings.
//
// That is reachable in production by a downgrade, and Apply's contract for it is
// exact: DISCARD it, log at ERROR naming the pair, and NEVER return an error —
// because an error from an applier whose COMMIT is already durable poisons the
// whole log with wal.ErrDiverged. This test asserts all three, including the
// third, which is the one whose absence would take the bus down rather than
// degrade it.
func TestAckUndecodableRecordIsDiscardedLoudlyAndTheTableSurvives(t *testing.T) {
	dir := t.TempDir()

	st, l, _, cap := openAck8(t, dir)
	mustAccept8(t, st, ack8Key(1), ack8Recipient)

	// A well-formed WAL entry of ack's kind carrying a body from another schema
	// version. Written through the log directly, which is what a binary one
	// version ahead would have left behind.
	future := map[string]any{
		"record_version":  99,
		"correlation_key": ack8Key(2),
		"recipient":       ack8Recipient,
		"sender":          ack8Sender,
		"state":           "delivered",
		"accepted_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"settled_at":      time.Now().UTC().Format(time.RFC3339Nano),
		"attested_by":     string(ack.AttestedByRecipientSignatureUnverified),
	}
	body, err := json.Marshal(future)
	if err != nil {
		t.Fatalf("marshalling the future-version record: %v", err)
	}
	if _, err := l.Write(wal.Entry{Kind: ack.RecordKind, Body: body}); err != nil {
		t.Fatalf("writing the future-version record: %v", err)
	}

	// THE LOG MUST NOT BE POISONED. If Apply had returned an error for that
	// entry, wal would have marked the Log diverged and every later write would
	// fail with ErrDiverged — an undecodable OBSERVABILITY row would have taken
	// the whole bus down.
	if err := st.Accept(ack8Key(3), ack8Sender, ack8Recipient); err != nil {
		if errors.Is(err, wal.ErrDiverged) {
			t.Fatalf("the log is POISONED (%v) after one undecodable ack record. Apply must DISCARD a record it cannot use and return nil: an error from an applier whose COMMIT record is already durable poisons the log, so a single unreadable lifecycle row would refuse every later write on the bus.", err)
		}
		t.Fatalf("Accept after the undecodable record: %v", err)
	}

	// The live discard is logged at ERROR, naming the indices.
	if !hasErrorContaining(cap, "DISCARDING a delivery lifecycle record that could not be decoded") {
		t.Errorf("no level=error line reported discarding the undecodable record on the LIVE path.\nInvariant 6: silent discard is the defect.\nfull log:\n%s", cap.String())
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// AND THE SAME ON REPLAY, which is the path that matters: every restart from
	// here on re-reads that record and must keep starting.
	st2, l2, _, cap2 := openAck8(t, dir)
	defer l2.Close()

	if !hasErrorContaining(cap2, "DISCARDING a delivery lifecycle record that could not be decoded") {
		t.Errorf("recovery replayed the undecodable record and logged NO level=error line about discarding it.\nfull log:\n%s", cap2.String())
	}
	if _, ok := st2.Lookup(ack8Key(2), ack8Recipient); ok {
		t.Errorf("the future-version row is PRESENT after recovery. record.go refuses a record_version it does not know precisely so it is never read with THIS version's field meanings; admitting it anyway is that refusal not happening.")
	}
	// The rows either side of the bad one are untouched: one poisoned record
	// costs its own row and nothing else.
	for _, i := range []int{1, 3} {
		if r, ok := st2.Lookup(ack8Key(i), ack8Recipient); !ok || r.State != ack.StateAccepted {
			t.Errorf("row %d is (%v, present=%v) after recovery, want accepted. A discard must cost exactly the record it discarded.", i, r.State, ok)
		}
	}
}

func hasErrorContaining(cap *ack8Log, sub string) bool {
	for _, ln := range cap.errorLines() {
		if strings.Contains(ln, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 5. REPLAY IS IDEMPOTENT.
// ---------------------------------------------------------------------------

// TestAckReplayingTheSameLogTwiceIsIdempotent: recovery is not a one-shot
// operation. A bus restarted twice replays the same records twice, and a crash
// loop replays them many times. The table must land in the same place every
// time.
func TestAckReplayingTheSameLogTwiceIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	st, l, _, _ := openAck8(t, dir)
	want := ack8Fixture(t, st)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for round := 1; round <= 3; round++ {
		st2, l2, _, cap := openAck8(t, dir)
		assertRows(t, st2, want, fmt.Sprintf("replay round %d", round))
		// A replay that refused a record would log it. Nothing here is
		// refusable, so any error line is a real regression.
		for _, ln := range cap.errorLines() {
			if strings.Contains(ln, "DISCARDING") {
				t.Errorf("replay round %d discarded a record from a log with no damage in it:\n\t%s", round, ln)
			}
		}
		if err := l2.Close(); err != nil {
			t.Fatalf("round %d Close: %v", round, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 6. INVARIANT 10's FIRST TWO CASES, ACROSS A RESTART.
// ---------------------------------------------------------------------------

// TestAckRetryAndConflictStayDistinctAcrossARestart.
//
// Invariant 10 names THREE cases and says they must not be collapsed. This test
// covers the FIRST TWO, re-checked AFTER a restart — because the state each is
// judged against is rebuilt state, and a check that is binding in memory is not
// automatically binding against a table that came off the disk.
//
//	same key + SAME outcome      -> absorbed. nil, ORIGINAL RESULT STANDS,
//	                                nothing written, nothing logged at error.
//	same key + DIFFERENT outcome -> REFUSED and logged. First terminal stands,
//	                                and NOTHING unrelated is taken down.
//
// # THE THIRD CASE IS DELIBERATELY NOT HERE, AND CALLING IT "COVERED" WOULD BE THE COLLAPSE
//
// Invariant 10's third case is REPLAY OF AN ALREADY-ACCEPTED SIGNED MESSAGE,
// which is rejected outright AND DISCONNECTS THE SENDER. That is a different
// subject with the OPPOSITE behaviour to the two above, and it lives at the
// signed-ingest boundary: this package has no signed plane, no principal and no
// connection, so it cannot be exercised here and is out of scope.
//
// WAL replay of an already-applied record — which IS exercised, by
// TestAckReplayingTheSameLogTwiceIsIdempotent — is a no-op, and it is NOT
// invariant 10's third case. Listing it as though it were would collapse the
// very distinction this file exists to keep, in a file whose whole thesis is
// that the three cases must stay distinct. Hence the test's name says "retry and
// conflict" rather than "the three cases".
//
// # THE 2026-08-08 NARROWING
//
// The last assertion in CASE 2 is the one worth being explicit about: invariant
// 10 was narrowed on 2026-08-08 so a protocol violation of this shape does NOT
// drop the connection, because the offender is overwhelmingly a client that lost
// track of its own keys. The store has no connection to drop, so the closest
// honest reading of "does not disconnect" is "stays usable" — asserted directly,
// and labelled as the analogy it is rather than passed off as the rule itself.
func TestAckRetryAndConflictStayDistinctAcrossARestart(t *testing.T) {
	dir := t.TempDir()

	st, l, _, _ := openAck8(t, dir)
	mustAccept8(t, st, ack8Key(1), ack8Recipient)
	mustSettle8(t, st, ack8Key(1), ack8Recipient, ack.StateDelivered, "", ack.AttestedByRecipientSignatureUnverified)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2, l2, spy2, cap2 := openAck8(t, dir)
	defer l2.Close()

	// CASE 1 — same key, same outcome. Absorbed silently.
	writes := spy2.writes()
	errorsBefore := len(cap2.errorLines())
	// THE ROW AS IT STANDS BEFORE THE RETRY. Captured because "return the
	// ORIGINAL result" is a claim about the RECORD, and asserting only that the
	// call returned nil and wrote nothing would miss a retry that silently
	// re-stamped the row in memory — which would let a client that retries in a
	// loop push the retention anchor out indefinitely and drift the serving copy
	// away from the disk it is supposed to be a copy of.
	original, ok := st2.Lookup(ack8Key(1), ack8Recipient)
	if !ok {
		t.Fatalf("the settled row is missing after the restart; there is nothing to retry against")
	}

	if err := st2.Settle(ack8Key(1), ack8Recipient, ack.StateDelivered, "", ack.AttestedByRecipientSignatureUnverified); err != nil {
		t.Errorf("re-settling with the IDENTICAL outcome returned %v, want nil. Same key + same payload is a legitimate retry: return the original result, do not re-apply, do not error.", err)
	}
	if got := spy2.writes(); got != writes {
		t.Errorf("the identical re-settle performed %d durable write(s), want 0", got-writes)
	}
	after, ok := st2.Lookup(ack8Key(1), ack8Recipient)
	if !ok {
		t.Fatalf("the row VANISHED after an absorbed retry")
	}
	if after.State != original.State || after.Class != original.Class || after.AttestedBy != original.AttestedBy {
		t.Errorf("the absorbed retry changed the row from (%s,%q,%q) to (%s,%q,%q); the ORIGINAL result must stand",
			original.State, original.Class, original.AttestedBy, after.State, after.Class, after.AttestedBy)
	}
	// The TIMESTAMPS specifically. These are the retention anchors, and they are
	// what a re-stamping retry would move while leaving every other assertion in
	// this block green.
	if !after.AcceptedAt.Equal(original.AcceptedAt) {
		t.Errorf("the absorbed retry moved accepted_at from %s to %s. A retry that re-stamps the anchor lets a client retrying in a loop keep a row alive for ever, and it drifts memory from the record on disk.",
			original.AcceptedAt.UTC(), after.AcceptedAt.UTC())
	}
	if !after.SettledAt.Equal(original.SettledAt) {
		t.Errorf("the absorbed retry moved settled_at from %s to %s; the terminal retention anchor must be the moment it FIRST settled",
			original.SettledAt.UTC(), after.SettledAt.UTC())
	}
	// Scoped to NEW lines rather than "no error lines at all": recovery earlier
	// in this test is entitled to log, and an assertion that forbids every error
	// line in the whole capture would fail for reasons that have nothing to do
	// with the retry it is about.
	if got := cap2.errorLines(); len(got) != errorsBefore {
		t.Errorf("the identical re-settle logged %d NEW line(s) at level=error; a legitimate retry is the case invariant 10 exists to make cheap and quiet:\n%s",
			len(got)-errorsBefore, strings.Join(got[errorsBefore:], "\n"))
	}

	// CASE 2 — same key, DIFFERENT outcome. Refused, logged, first stands.
	writes = spy2.writes()
	err := st2.Settle(ack8Key(1), ack8Recipient, ack.StateRefused, ack.ClassRecipientRefusedPolicy, ack.AttestedByRecipientSignatureUnverified)
	if !errors.Is(err, ack.ErrTerminal) {
		t.Errorf("offering a DIFFERENT terminal returned %v, want ErrTerminal. Same key + different payload is a protocol violation: reject it and log it.", err)
	}
	if r, _ := st2.Lookup(ack8Key(1), ack8Recipient); r.State != ack.StateDelivered {
		t.Errorf("after the conflicting settle the row is %s, want delivered. THE FIRST TERMINAL STANDS: two contradicting terminals cannot both be true, and keeping the first keeps the outcome that actually happened.", r.State)
	}
	if got := spy2.writes(); got != writes {
		t.Errorf("the refused settle performed %d durable write(s), want 0: a rejected transition must not reach the log", got-writes)
	}
	if !hasErrorContaining(cap2, "REFUSING a second, DIFFERENT terminal delivery outcome") {
		t.Errorf("the protocol violation was refused but NOT logged at level=error. Rejecting silently is how a client that has lost track of its keys goes undiagnosed:\n%s", cap2.String())
	}

	// ...AND THE TABLE IS STILL USABLE. This is the store-level reading of the
	// 2026-08-08 narrowing: a protocol violation is refused, not punished.
	if err := st2.Accept(ack8Key(2), ack8Sender, ack8Recipient); err != nil {
		t.Errorf("the store refused an unrelated, well-formed Accept (%v) after a protocol violation. The violation must cost the offending call and nothing else — invariant 10 was narrowed precisely so this class of mistake does not take out traffic that was never in question.", err)
	}

	// CASE 3 — an already-applied record replayed. A no-op, and it must not
	// disturb the row it names.
	st3, l3, _, _ := openAck8(t, dir)
	defer l3.Close()
	r3, ok := st3.Lookup(ack8Key(1), ack8Recipient)
	if !ok || r3.State != ack.StateDelivered {
		t.Errorf("after a further restart the row is (%v, present=%v), want delivered", r3.State, ok)
	}
}

// writes reports how many entries the spy has passed to the log. It is how the
// tests assert "nothing was written" rather than merely "no error was returned"
// — the difference between an operation that was absorbed and one that was
// re-applied.
func (s *indexSpy) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}
