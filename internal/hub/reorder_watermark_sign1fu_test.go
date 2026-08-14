package hub_test

// SIGN-1-FU-REORDER-WATERMARK — THE REPRODUCTION, now the regression test.
//
// # The defect, in one paragraph
//
// SIGN-1 made a send a TWO-STEP: Hub.Mint durably burns a sequence number so the
// CLIENT can sign it, and the send follows — up to hub.MintTTL (fifteen minutes)
// later. So a message can be committed, fsynced and ACKNOWLEDGED at a sequence
// BELOW a reader's cursor. While a cursor was a SEQUENCE, store.Since
// binary-searched for the first retained message strictly AFTER it and that
// reader was never handed the message; and hub.notify skipped any waiter with
// `m.Seq <= w.after`, so a parked long poll was never even woken. Long-poll-at-
// head is the primary mode of every agent on this bus, which made this the
// ORDINARY outcome for an actively waiting recipient, not a narrow race. The
// sender chose WHEN to spend the reservation, so it chose the victim: a targeted
// SUPPRESSION / FALSE-ACK primitive, reproduced live by the security gate with
// one enrolled agent, two mints and two sends.
//
// # The fix these tests now pin: the seq/pos SPLIT
//
// store.Message.Seq stays the client-signed IDENTITY, minted at RESERVATION time
// and therefore not monotone in arrival order. store.Message.Pos is the DELIVERY
// POSITION — the wal.Committed.CommitIndex of the record that made the message
// durable — and it is what cursors, store.Since, store.HasVisibleAfter and
// hub.notify all speak. A late arrival with a LOW sequence gets a HIGH position,
// lands ABOVE every live cursor, is served to every reader and WAKES EVERY
// PARKED WAITER.
//
// The watermark this task is named after ("no sequence <= W can still arrive")
// was REFUTED by execution, and the evidence is still here:
// TestReorderWatermarkHeadAdvancesFarWhileAMintIsOutstanding shows the head
// advancing arbitrarily far above one unspent reservation that is still honoured
// nearly a full MintTTL later, so a watermark would have handed any enrolled
// agent a bus-wide, fifteen-minute delivery freeze for the price of one mint.
//
// # CURSORS IN THIS FILE ARE POSITIONS, NOT SEQUENCES
//
// The original version of every test here asserted `cursor == res.Seq`, and that
// premise is now false by design: a position is a WAL commit index and counts
// floor records, mint records and enrolment records as well as messages, so it is
// simply a different counter. hub.Result deliberately does not carry the position
// — it is not part of the durable record — so a test that needs it reads it back
// off the stream through posOf below.
//
// # Invariants read IN FULL before writing this
//
// Invariant 1 — server-authoritative ids, NEVER REUSED, and the sequence never
// rewinds. The fix does not rewind the head or re-issue a number; it adds a
// second, separate counter and leaves the sequence alone.
// Invariant 4 — nothing acknowledged before durable. The send IS acknowledged and
// the record IS durable; the hole was DELIVERY, which is why it was worse than a
// lost write — the sender was told it succeeded.
// Invariant 5 — memory serves, disk is truth. The position is the WAL commit
// index precisely so that replay, which folds the log in commit order, reproduces
// it EXACTLY; the crash test asserts that identity directly.
// Invariant 10 — duplicate detection and idempotency everywhere. Delivering the
// straggler must not re-deliver anything else: the cursor assertions below pin
// that each message arrives once per reader.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// reorderPollTimeout is how long a parked long poll is given.
//
// It is the cost of every failing run in this file, so it is short — but it must
// stay comfortably above the microseconds a correct wake needs, or a CORRECT
// implementation could fail here for being slow rather than for being wrong.
const reorderPollTimeout = 2 * time.Second

// reorderWaitResult carries a parked Wait's two return values off its goroutine.
type reorderWaitResult struct {
	batch hub.Batch
	err   error
}

// awaitParked blocks until a long poll has PASSED ITS PARK POINT, and it is the
// difference between a real proof of the wake and a false green.
//
// waitForWaiters is NOT sufficient here and the shortfall was measured, not
// guessed. WaiterCount() reaches one the moment hub.Wait registers in the waiter
// map, but registration is not the park: Wait then performs a SECOND store.Since
// read to close the registration race. A test that sends as soon as the count
// rises therefore lands INSIDE that window, the message comes back from that
// READ — which consults Message.Pos regardless of what hub.notify compares — and
// notify is never exercised. With that shape, mutating notify back to the
// defective `m.Seq <= w.after` left BOTH parked-poll tests in this file PASSING.
//
// hub.SetWaiterParkedHook fires immediately after that second read, so once this
// returns the ONLY path that can still deliver anything to the parked call is
// notify. That makes these tests fail — as they must — under the mutation.
//
// The hook is installed for the whole test and restored on cleanup; the once
// guard is there because it fires for EVERY poll that parks, and only the first
// is the arming edge these tests wait on.
func awaitParked(t *testing.T, h *hub.Hub, why string) func() {
	t.Helper()
	parked := make(chan struct{})
	var once sync.Once
	t.Cleanup(hub.SetWaiterParkedHook(func() { once.Do(func() { close(parked) }) }))

	return func() {
		t.Helper()
		waitForWaiters(t, h, 1, why)
		select {
		case <-parked:
		case <-time.After(5 * time.Second):
			t.Fatalf("a long poll registered as a waiter but never reached its park point (%s). Until it is past "+
				"hub.Wait's registration-race read, a send would be delivered by that READ and hub.notify — the "+
				"thing this test exists to exercise — would never run", why)
		}
	}
}

// reorderClock is an injectable clock for the mint-TTL measurement. It is
// mutex-guarded because the hub reads it from the request goroutine and from
// the store's retention pass, and this package's tests run with -race.
type reorderClock struct {
	mu sync.Mutex
	t  time.Time
}

// newReorderClock starts at the REAL current instant, not at a fixed date:
// fixtureEnrolledAt is one hour before time.Now(), and store.Message.VisibleTo
// refuses to deliver a message sent before the reader enrolled. A synthetic
// epoch would make every read in this file empty and every assertion vacuous.
func newReorderClock() *reorderClock { return &reorderClock{t: time.Now()} }

func (c *reorderClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *reorderClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// posOf reads back the DELIVERY POSITION the bus assigned to a message, through
// the same read path a client uses.
//
// hub.Result carries the sequence and not the position, and that is deliberate
// rather than an omission: the position is not part of the durable record (it IS
// the record's location in the log), so the stream is the only place it is
// observable. reader must be an agent entitled to see the message — the sender is
// excluded from its own traffic.
func posOf(t *testing.T, h *hub.Hub, reader, messageID string) uint64 {
	t.Helper()
	for _, m := range historyOf(t, h, reader) {
		if m.ID != messageID {
			continue
		}
		if m.Pos == 0 {
			t.Fatalf("message %s is served with delivery position 0, which is the reserved \"seen nothing\" cursor "+
				"value; the hub must stamp it from wal.Committed.CommitIndex", messageID)
		}
		return m.Pos
	}
	t.Fatalf("message %s is not in %s's stream at all", messageID, reader)
	return 0
}

// reorderSetup is the shared fixture: three agents, two reservations held at
// once, and the HIGHER one spent first so alice's is the late arrival.
//
// Nothing is concurrent and nothing is timing-dependent: the mint table holds a
// reservation for hub.MintTTL, so holding two open is the ordinary shape of the
// protocol rather than a race that has to be won.
type reorderSetup struct {
	h         *hub.Hub
	dir       string
	alice     string
	bob       string
	carol     string
	aliceMint hub.Mint
	bobRes    hub.Result
	// bobPos is the DELIVERY POSITION bob's message committed at, which is the
	// cursor a caught-up reader holds. It is NOT bobRes.Seq — see the file header.
	bobPos uint64
}

func newReorderSetup(t *testing.T, lg *wal.Log, dir string) *reorderSetup {
	t.Helper()
	h, _ := openMintHub(t, dir, lg, nil, "", "alice", "bob", "carol")

	s := &reorderSetup{
		h:     h,
		dir:   dir,
		alice: agentID(t, testBusID, "alice"),
		bob:   agentID(t, testBusID, "bob"),
		carol: agentID(t, testBusID, "carol"),
	}

	// BOTH reservations are taken before EITHER is spent.
	s.aliceMint = mustMint(t, h, s.alice, "send", "k-alice")
	bobMint := mustMint(t, h, s.bob, "send", "k-bob")
	if s.aliceMint.Seq >= bobMint.Seq {
		t.Fatalf("PREMISE BROKEN: alice minted sequence %d and bob minted %d; this file needs alice to hold the "+
			"LOWER reservation so that spending bob's first puts alice's message BELOW a reader's cursor",
			s.aliceMint.Seq, bobMint.Seq)
	}

	// The HIGHER reservation is spent first. This lands at the head.
	res, err := h.Send(hub.SendRequest{
		Sender: s.bob, To: s.carol, Body: []byte("bob to carol"),
		IdempotencyKey: "k-bob", SignedMint: signedMintFrom(bobMint),
	})
	if err != nil {
		t.Fatalf("bob spending the HIGHER reservation (sequence %d) first: %v", bobMint.Seq, err)
	}
	s.bobRes = res
	s.bobPos = posOf(t, h, s.carol, res.MessageID)
	return s
}

// catchUp reads carol up to the head and returns the cursor she is told to
// resume from, having checked the two premises every test here rests on: the
// cursor is bob's DELIVERY POSITION, and it is STRICTLY ABOVE alice's unspent
// sequence — which is precisely the reader the old design suppressed.
func (s *reorderSetup) catchUp(t *testing.T) uint64 {
	t.Helper()
	caught := mustHistory(t, s.h, s.carol, 0, hub.MaxBatchLimit)
	if len(caught.Messages) != 1 || caught.Messages[0].ID != s.bobRes.MessageID {
		t.Fatalf("carol's catch-up read returned %d messages, want just bob's (%s)", len(caught.Messages), s.bobRes.MessageID)
	}
	if caught.Cursor != s.bobPos {
		t.Fatalf("carol's cursor after catching up is %d, want bob's DELIVERY POSITION %d. A cursor is a position "+
			"(store.Message.Pos), not a sequence — bob's sequence is %d and the two counters are unrelated",
			caught.Cursor, s.bobPos, s.bobRes.Seq)
	}
	if caught.Cursor <= s.aliceMint.Seq {
		t.Fatalf("PREMISE BROKEN: carol's cursor is %d and alice's outstanding reservation is sequence %d; every "+
			"test in this file needs the cursor to be STRICTLY ABOVE the sequence alice is about to spend, because "+
			"that is the reader the sequence-cursor design lost", caught.Cursor, s.aliceMint.Seq)
	}
	return caught.Cursor
}

// spendAlice spends the LOWER reservation and asserts the ACK: the bus accepts
// it, returns a message id, and has therefore promised delivery. Under the old
// design that promise was false for every reader past its sequence.
func (s *reorderSetup) spendAlice(t *testing.T) hub.Result {
	t.Helper()
	res, err := s.h.Send(hub.SendRequest{
		Sender: s.alice, To: s.carol, Body: []byte("alice to carol"),
		IdempotencyKey: "k-alice", SignedMint: signedMintFrom(s.aliceMint),
	})
	if err != nil {
		t.Fatalf("alice spending the LOWER reservation (sequence %d) second was refused: %v — this file is about "+
			"what happens when it SUCCEEDS; a refusal is SIGN-1-FU-OUTOFORDER-POISON regressing", s.aliceMint.Seq, err)
	}
	if res.MessageID == "" {
		t.Fatal("alice's send returned no message id")
	}
	if res.Seq != s.aliceMint.Seq {
		t.Fatalf("alice's send committed at sequence %d, want the reserved %d", res.Seq, s.aliceMint.Seq)
	}
	if res.Seq >= s.bobRes.Seq {
		t.Fatalf("PREMISE BROKEN: alice committed at sequence %d and bob at %d; alice's must be the LOWER one or "+
			"this is not the out-of-order spend at all", res.Seq, s.bobRes.Seq)
	}
	return res
}

// TestReorderWatermarkParkedLongPollIsWokenByTheLateLowSequence IS THE HEADLINE.
//
// carol catches up, parks a long poll at the head — the primary mode of every
// agent on this bus — and alice then spends the reservation she has been
// holding. The send is ACKNOWLEDGED, and carol must be WOKEN and HANDED IT.
//
// Under the old design hub.notify skipped this waiter (`m.Seq <= w.after`), the
// poll ran to its deadline, and no later read from the cursor she was given ever
// produced the message. Now the message carries a position ABOVE her cursor, so
// the wake filter and the read agree and both reach it.
//
// WHICH PATH DELIVERED IS ASSERTED, not assumed, and it is asserted at the only
// point that actually distinguishes the paths. awaitParked proves the poll is
// past hub.Wait's registration-race read — so the ONLY thing that can still
// deliver to it is notify — and the History probe below proves there was nothing
// at that cursor to return until alice sent. Without the first, a send that
// landed in the registration window would be answered by Wait's SECOND store
// read, which consults Pos whatever notify compares, and this test would pass
// with the defect restored. It did, before awaitParked existed.
func TestReorderWatermarkParkedLongPollIsWokenByTheLateLowSequence(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	s := newReorderSetup(t, lg, dir)

	cursor := s.catchUp(t)

	// carol parks at the head. Nothing about this is unusual or ill-advised: it
	// is exactly what AGENT_PROTOCOL.md tells an agent to do.
	waitParked := awaitParked(t, s.h, "carol must be parked before alice spends her reservation")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan reorderWaitResult, 1)
	go func() {
		b, err := s.h.Wait(ctx, s.carol, cursor, hub.MaxBatchLimit, reorderPollTimeout)
		out <- reorderWaitResult{b, err}
	}()
	waitParked()

	// THE ANTI-VACUOUS PROBE. There is nothing at carol's cursor right now, so
	// whatever she is handed below can only have come from the WAKE.
	if b := mustHistory(t, s.h, s.carol, cursor, hub.MaxBatchLimit); len(b.Messages) != 0 {
		t.Fatalf("PREMISE BROKEN: carol's cursor %d already has %d message(s) waiting before alice sent, so a "+
			"delivery below would prove nothing about the wake", cursor, len(b.Messages))
	}

	// THE LATE SEND. It is accepted and acknowledged.
	aliceRes := s.spendAlice(t)

	var got reorderWaitResult
	select {
	case got = <-out:
	case <-time.After(reorderPollTimeout + 10*time.Second):
		t.Fatal("carol's parked Wait never returned at all, not even at its deadline")
	}
	if got.err != nil {
		t.Fatalf("carol's parked Wait returned err = %v; a wake is not an error", got.err)
	}

	// THE HEADLINE ASSERTION. The bus acknowledged alice's message to a recipient
	// who is parked, entitled to see it, and asking for it right now.
	if got.batch.TimedOut {
		t.Fatalf("carol's long poll TIMED OUT after alice's send at sequence %d was acknowledged (message %s). "+
			"hub.notify must compare the message's DELIVERY POSITION against the waiter's cursor, not its "+
			"sequence: alice's sequence is below the cursor carol holds, and a wake filter on sequences skips her "+
			"for ever (SIGN-1-FU-REORDER-WATERMARK)", aliceRes.Seq, aliceRes.MessageID)
	}
	if got, want := batchIDs(got.batch), []string{aliceRes.MessageID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("carol's long poll from cursor %d returned %v, want %v", cursor, got, want)
	}

	// THE SPLIT, OBSERVED END TO END: the delivered message carries a sequence
	// BELOW carol's cursor and a position ABOVE it. That combination is the whole
	// fix, and asserting it here stops the test passing on a hub that had merely
	// renamed one counter.
	m := got.batch.Messages[0]
	if m.Seq != aliceRes.Seq {
		t.Fatalf("the delivered message carries sequence %d, want alice's %d", m.Seq, aliceRes.Seq)
	}
	if m.Pos <= cursor {
		t.Fatalf("the delivered message's position is %d, which is not above carol's cursor %d — a late arrival "+
			"must take the NEXT commit index, above every live cursor", m.Pos, cursor)
	}
	if got.batch.Cursor != m.Pos {
		t.Fatalf("the wake reported cursor %d, want the delivered message's position %d", got.batch.Cursor, m.Pos)
	}

	// AND IT IS REACHABLE, not a one-shot edge: the wake is a signal and the store
	// is the truth, so a client that missed the wake and re-read from its stored
	// cursor gets the same message.
	again := mustHistory(t, s.h, s.carol, cursor, hub.MaxBatchLimit)
	if got, want := batchIDs(again), []string{aliceRes.MessageID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a follow-up History from carol's cursor %d returned %v, want %v — the read path must reach the "+
			"same message the wake did, or a client that crashed mid-poll loses it", cursor, got, want)
	}

	// AND ONCE. Advancing to the cursor the delivery handed back leaves nothing
	// behind (invariant 10: closing the gap must not become an endless replay).
	if b := mustHistory(t, s.h, s.carol, got.batch.Cursor, hub.MaxBatchLimit); len(b.Messages) != 0 {
		t.Fatalf("reading from the cursor the delivery handed back (%d) returned %v, want nothing",
			got.batch.Cursor, batchIDs(b))
	}

	waitForWaiters(t, s.h, 0, "the poll must deregister however it exited")
}

// TestReorderWatermarkCatchUpReaderStillReceivesIt IS THE CONTROL.
//
// A reader whose cursor is still below everything is served both messages, IN
// DELIVERY ORDER — bob's first, alice's second, because that is the order this
// bus took responsibility for them. That is what makes the headline test a
// statement about delivery ORDER and reachability rather than about durability or
// visibility: the message is on disk, in the serving copy, addressed to carol,
// and readable from any cursor below it.
//
// The expectation is deliberately NOT ascending sequence. Sorting the stream by
// sequence is what put the late spend below the cursors that had already passed
// it, so an ascending-sequence assertion here would be the suppression bug
// written down as a test.
func TestReorderWatermarkCatchUpReaderStillReceivesIt(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	s := newReorderSetup(t, lg, dir)

	// carol has NOT read anything yet: her cursor is 0, below both positions.
	aliceRes := s.spendAlice(t)

	if got, want := historyIDs(t, s.h, s.carol), []string{s.bobRes.MessageID, aliceRes.MessageID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a catch-up reader at cursor 0 sees %v, want %v — both messages, in the order they were SPENT "+
			"(bob's sequence %d first, alice's %d second). If THIS is failing the defect is worse than "+
			"SIGN-1-FU-REORDER-WATERMARK: the message is not being served at all",
			got, want, s.bobRes.Seq, aliceRes.Seq)
	}

	// The fast path of a long poll agrees with History, so an agent that parks
	// from cursor 0 is served immediately rather than parking on messages that
	// already exist.
	b, err := s.h.Wait(context.Background(), s.carol, 0, hub.MaxBatchLimit, reorderPollTimeout)
	if err != nil {
		t.Fatalf("Wait from cursor 0: %v", err)
	}
	if b.TimedOut {
		t.Fatal("a long poll from cursor 0 parked and timed out with two messages already visible")
	}
	if got, want := batchIDs(b), []string{s.bobRes.MessageID, aliceRes.MessageID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Wait from cursor 0 returned %v, want %v", got, want)
	}

	// THE TWO COUNTERS DISAGREE, IN THE DIRECTION THAT USED TO LOSE MAIL: alice's
	// sequence is the LOWER of the two and her position is the HIGHER. Asserted
	// through the served messages so it is the READ PATH's view, not the mint's.
	bobMsg, aliceMsg := b.Messages[0], b.Messages[1]
	if !(aliceMsg.Seq < bobMsg.Seq && aliceMsg.Pos > bobMsg.Pos) {
		t.Fatalf("alice's message is (seq %d, pos %d) and bob's is (seq %d, pos %d); this fixture requires alice's "+
			"SEQUENCE to be lower and her POSITION higher — that disagreement is the entire subject of this file",
			aliceMsg.Seq, aliceMsg.Pos, bobMsg.Seq, bobMsg.Pos)
	}
	if b.Cursor != aliceMsg.Pos {
		t.Fatalf("Wait from cursor 0 reported cursor %d, want the last delivered position %d", b.Cursor, aliceMsg.Pos)
	}
}

// TestReorderWatermarkLaterTrafficDeliversTheStragglerToo closes the most likely
// misreading of the headline test, and it INVERTS the assertion it originally
// carried.
//
// The original hoped-for-but-false comfort was that the straggler was merely
// DELAYED — that the next message on the bus would wake the poll and the re-read
// would pick up both. It did not: the wake is an edge and the store is the truth,
// and the truth was read with the same below-the-cursor search, so ordinary
// traffic healed nothing. Now it does, and there is nothing left to heal in the
// first place.
//
// Two phases, and each states WHICH PATH delivered:
//
//	A. THE WAKE. carol parks BEFORE alice's late send — this is the only way to
//	   test notify at all, because once the message is committed Wait's fast path
//	   returns it and a parked waiter can no longer be established. (That is the
//	   scaffolding defect in the original file: it parked carol at a cursor that,
//	   under the fix, already had traffic behind it, so waitForWaiters never saw
//	   a parked poll and the test failed for a reason unrelated to its subject.)
//	B. ORDINARY TRAFFIC. bob sends again afterwards, and a read from carol's
//	   ORIGINAL cursor hands back the straggler AND the later message, in delivery
//	   order. This one is the FAST path by construction — both messages are already
//	   committed — which is exactly why phase A had to park.
func TestReorderWatermarkLaterTrafficDeliversTheStragglerToo(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	s := newReorderSetup(t, lg, dir)

	cursor := s.catchUp(t)

	// --- PHASE A: the wake -------------------------------------------------
	//
	// awaitParked, not merely waitForWaiters: the poll must be past Wait's
	// registration-race read before alice sends, or the second read answers it and
	// notify is never reached. See awaitParked.
	waitParked := awaitParked(t, s.h, "carol must be parked before the late arrival commits, or the wake is untested")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan reorderWaitResult, 1)
	go func() {
		b, err := s.h.Wait(ctx, s.carol, cursor, hub.MaxBatchLimit, reorderPollTimeout)
		out <- reorderWaitResult{b, err}
	}()
	waitParked()

	aliceRes := s.spendAlice(t)

	var got reorderWaitResult
	select {
	case got = <-out:
	case <-time.After(reorderPollTimeout + 10*time.Second):
		t.Fatal("carol's parked Wait never returned at all")
	}
	if got.err != nil {
		t.Fatalf("carol's parked Wait returned err = %v", got.err)
	}
	if got.batch.TimedOut {
		t.Fatalf("carol's poll timed out even though alice committed message %s (sequence %d) while she was "+
			"parked at cursor %d — notify must wake on the DELIVERY POSITION", aliceRes.MessageID, aliceRes.Seq, cursor)
	}
	if ids, want := batchIDs(got.batch), []string{aliceRes.MessageID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("the wake delivered %v, want %v", ids, want)
	}
	waitForWaiters(t, s.h, 0, "the woken poll must deregister")

	// --- PHASE B: ordinary later traffic -----------------------------------
	laterMint := mustMint(t, s.h, s.bob, "send", "k-bob-2")
	later, err := s.h.Send(hub.SendRequest{
		Sender: s.bob, To: s.carol, Body: []byte("bob to carol, later"),
		IdempotencyKey: "k-bob-2", SignedMint: signedMintFrom(laterMint),
	})
	if err != nil {
		t.Fatalf("bob's later send: %v", err)
	}

	// THE INVERTED ASSERTION. A reader at the ORIGINAL cursor — a client that had
	// not yet consumed the wake, or one that reconnected with its stored cursor —
	// is handed BOTH, in delivery order. Ordinary traffic does not step over the
	// straggler.
	b := mustHistory(t, s.h, s.carol, cursor, hub.MaxBatchLimit)
	want := []string{aliceRes.MessageID, later.MessageID}
	if ids := batchIDs(b); !reflect.DeepEqual(ids, want) {
		t.Fatalf("a read from carol's original cursor %d delivered %v, want %v. The wake is an edge and the store "+
			"is the truth; the truth is read by DELIVERY POSITION, so alice's message (sequence %d, acknowledged) "+
			"sits above that cursor alongside bob's later one (SIGN-1-FU-REORDER-WATERMARK)",
			cursor, ids, want, aliceRes.Seq)
	}

	// And a long poll from that cursor returns the same pair WITHOUT PARKING —
	// stated as an assertion because it is the structural reason phase A had to
	// park first: there is no longer anything to park on.
	fast, err := s.h.Wait(context.Background(), s.carol, cursor, hub.MaxBatchLimit, reorderPollTimeout)
	if err != nil {
		t.Fatalf("Wait from carol's original cursor %d: %v", cursor, err)
	}
	if fast.TimedOut {
		t.Fatalf("a long poll from cursor %d parked and timed out with two messages already above it", cursor)
	}
	if ids := batchIDs(fast); !reflect.DeepEqual(ids, want) {
		t.Fatalf("Wait's fast path from cursor %d returned %v, want %v — it must agree with History exactly, or a "+
			"poll and a history read disagree about what a cursor means", cursor, ids, want)
	}
}

// TestReorderWatermarkLateArrivalIsDurableAndReachableAfterRecovery is the
// CRASH-INJECTION half, and it now proves three things rather than two.
//
//   - DURABLE. The bus is killed immediately after the fsync that acknowledged
//     the late message — the WAL copy is cut at exactly the byte boundary commit
//     reached — and the record is still there and replayable (invariant 4).
//   - REBUILT IDENTICALLY. The recovered stream carries the SAME delivery
//     positions, not merely the same messages. That is the load-bearing reason the
//     position is the wal.Committed.CommitIndex rather than a counter this process
//     increments: a counter would restart at 1 and silently renumber the retained
//     window, so every client's stored cursor would point somewhere else after a
//     restart (invariant 5).
//   - REACHABLE. The same reader presenting the same PERSISTED cursor to the
//     RECOVERED hub is handed the message. A real client persists its cursor; it
//     does not restart at 0 because the server did. Under the old design this was
//     the assertion that proved the loss was a property of the accepted history
//     and survived a restart.
func TestReorderWatermarkLateArrivalIsDurableAndReachableAfterRecovery(t *testing.T) {
	dir := t.TempDir()
	// closeOnCleanup = false: this test closes the log itself before copying the
	// bytes, the way the crash sweep in recovery_test.go does.
	lg := openTestLog(t, dir, false)
	s := newReorderSetup(t, lg, dir)

	cursor := s.catchUp(t)
	aliceRes := s.spendAlice(t)
	alicePos := posOf(t, s.h, s.carol, aliceRes.MessageID)
	if alicePos <= cursor {
		t.Fatalf("alice's message committed at position %d, which is not above carol's persisted cursor %d", alicePos, cursor)
	}

	// THE KILL POINT: immediately after alice's commit record was fsynced and
	// the ack returned. Cutting at the full length is the crash that loses
	// nothing, which is the one that isolates delivery from durability.
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the fixture log: %v", err)
	}
	walPath := filepath.Join(dir, wal.WALFileName)
	st, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat %s: %v", walPath, err)
	}
	crashDir := crashAt(t, crashFixture{dir: dir}, st.Size())

	// DURABLE — asserted off the bytes, without going through the hub, so
	// "recovery served it" and "recovery has it" cannot be confused.
	crashWAL := filepath.Join(crashDir, wal.WALFileName)
	if _, ok := findByID(replayMessages(t, crashWAL), aliceRes.MessageID); !ok {
		t.Fatalf("alice's acknowledged message %s (sequence %d) is NOT in the crashed WAL; invariant 4 says "+
			"nothing is acknowledged before it is durable", aliceRes.MessageID, aliceRes.Seq)
	}

	lg2, err := wal.Open(wal.LogOptions{Dir: crashDir})
	if err != nil {
		t.Fatalf("wal.Open on the crashed copy: %v — recovery must always reach a running server (invariant 6)", err)
	}
	defer func() { _ = lg2.Close() }()
	h2, _ := openMintHub(t, crashDir, lg2, nil, "", "alice", "bob", "carol")

	if p := h2.Poisoned(); p != nil {
		t.Fatalf("the recovered hub is poisoned: %v", p)
	}

	// REBUILT: exactly what was served, in the same delivery order, AT THE SAME
	// POSITIONS.
	recovered := historyOf(t, h2, s.carol)
	if got, want := idsOfMessages(recovered), []string{s.bobRes.MessageID, aliceRes.MessageID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after recovery a cursor-0 reader sees %v, want %v", got, want)
	}
	for _, c := range []struct {
		what string
		id   string
		want uint64
	}{
		{"bob's", s.bobRes.MessageID, s.bobPos},
		{"alice's late", aliceRes.MessageID, alicePos},
	} {
		m, ok := findByID(recovered, c.id)
		if !ok {
			t.Fatalf("%s message %s is missing from the recovered stream", c.what, c.id)
		}
		if m.Pos != c.want {
			t.Fatalf("after recovery %s message %s is at delivery position %d, want %d — replay folds the log in "+
				"commit order and must reproduce the position EXACTLY, or every client's stored cursor silently "+
				"points somewhere else after a restart (invariant 5)", c.what, c.id, m.Pos, c.want)
		}
	}

	// REACHABLE: the same reader at the cursor it actually holds.
	after := mustHistory(t, h2, s.carol, cursor, hub.MaxBatchLimit)
	if got, want := batchIDs(after), []string{aliceRes.MessageID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after recovery carol at her persisted cursor %d is served %v, want %v (sequence %d). The record "+
			"survived the crash intact; if it is not served here it is UNREACHABLE data rather than lost data, and "+
			"a restart does not clear that (SIGN-1-FU-REORDER-WATERMARK)", cursor, got, want, aliceRes.Seq)
	}

	// And a long poll on the RECOVERED hub from that cursor returns it too, so an
	// agent that reconnects after the restart and parks is served rather than left
	// waiting. It returns on the FAST path — there is something there — which is
	// the point: the wake path is exercised on a live hub above.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan reorderWaitResult, 1)
	go func() {
		b, err := h2.Wait(ctx, s.carol, cursor, hub.MaxBatchLimit, reorderPollTimeout)
		out <- reorderWaitResult{b, err}
	}()
	select {
	case got := <-out:
		if got.err != nil {
			t.Fatalf("the recovered hub's Wait returned err = %v", got.err)
		}
		if got.batch.TimedOut {
			t.Fatalf("a long poll on the RECOVERED hub from carol's cursor %d timed out; the message is on disk "+
				"and in the serving copy above that cursor", cursor)
		}
		if ids, want := batchIDs(got.batch), []string{aliceRes.MessageID}; !reflect.DeepEqual(ids, want) {
			t.Fatalf("the recovered hub's Wait from cursor %d returned %v, want %v", cursor, ids, want)
		}
	case <-time.After(reorderPollTimeout + 10*time.Second):
		t.Fatal("the recovered hub's Wait never returned at all")
	}
}

// TestReorderWatermarkHeadAdvancesFarWhileAMintIsOutstanding QUANTIFIES THE
// STARVATION HAZARD. It is the evidence that REFUTED the design this task is
// named after, and it is kept so the refutation is reproducible rather than
// remembered.
//
// The obvious fix was a reorder watermark holding every reader's cursor at
// (lowest outstanding mint − 1) until that reservation is spent or expires. This
// test measures what that would have cost: an agent may hold a reservation for
// the whole of hub.MintTTL — fifteen minutes — while the head advances
// arbitrarily far above it, and the reservation is still honoured at the end. So
// a watermark hands ANY enrolled agent a bus-wide delivery freeze for fifteen
// minutes at the cost of one unspent mint: the same denial of service
// SIGN-1-FU-OUTOFORDER-POISON closed, wearing a different hat. Read the numbers
// this test logs before proposing one again.
//
// The closing assertion is what the seq/pos split delivers instead: the reader
// that kept up was stalled by NOTHING, and is handed the straggler on its very
// next read.
//
// The clock is injected rather than slept on, so the fifteen-minute window costs
// nothing to exercise.
func TestReorderWatermarkHeadAdvancesFarWhileAMintIsOutstanding(t *testing.T) {
	// Large enough to be obviously unbounded in practice, small enough that
	// fifty durable mint+send round trips stay fast. Nothing in the hub caps it:
	// bob's reservations are spent immediately, so his per-agent mint quota is
	// never approached. The store-level half of this measurement runs the same
	// shape at 200 (store.TestReorderWatermarkTheGapCanBeArbitrarilyWide).
	const above = 50

	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	clock := newReorderClock()
	h, _ := openMintHub(t, dir, lg, clock.now, "", "alice", "bob", "carol")

	alice := agentID(t, testBusID, "alice")
	bob := agentID(t, testBusID, "bob")
	carol := agentID(t, testBusID, "carol")

	// ONE reservation, held and not spent.
	aliceMint := mustMint(t, h, alice, "send", "k-alice")

	var lastSeq uint64
	for i := 0; i < above; i++ {
		key := fmt.Sprintf("k-bob-%d", i)
		m := mustMint(t, h, bob, "send", key)
		res, err := h.Send(hub.SendRequest{
			Sender: bob, To: carol, Body: []byte(fmt.Sprintf("bob to carol #%d", i)),
			IdempotencyKey: key, SignedMint: signedMintFrom(m),
		})
		if err != nil {
			t.Fatalf("bob's send #%d while alice holds sequence %d: %v", i, aliceMint.Seq, err)
		}
		lastSeq = res.Seq
	}
	if gap := lastSeq - aliceMint.Seq; gap != above {
		t.Fatalf("the head reached %d with alice holding %d, a gap of %d; want %d — the fixture must commit "+
			"exactly %d messages above the outstanding reservation", lastSeq, aliceMint.Seq, gap, above, above)
	}

	// A reader that keeps up is now sitting at the head of the DELIVERY order.
	caught := mustHistory(t, h, carol, 0, hub.MaxBatchLimit)
	if len(caught.Messages) != above {
		t.Fatalf("the catch-up reader was served %d messages, want %d", len(caught.Messages), above)
	}
	headPos := caught.Messages[len(caught.Messages)-1].Pos
	if caught.Cursor != headPos {
		t.Fatalf("the catch-up reader's cursor is %d, want the last delivered POSITION %d (its sequence is %d — a "+
			"different counter)", caught.Cursor, headPos, lastSeq)
	}

	// NEARLY THE WHOLE TTL LATER, the reservation is still honoured. This is the
	// load-bearing half: the window is not theoretical, it is a documented
	// fifteen minutes, and the bus keeps its promise for all of it.
	clock.advance(hub.MintTTL - time.Minute)
	res, err := h.Send(hub.SendRequest{
		Sender: alice, To: carol, Body: []byte("alice to carol, held for nearly the whole TTL"),
		IdempotencyKey: "k-alice", SignedMint: signedMintFrom(aliceMint),
	})
	if err != nil {
		t.Fatalf("alice spending her reservation (sequence %d) after %v was refused: %v — the reservation must be "+
			"honoured for the whole of hub.MintTTL (%v)", aliceMint.Seq, hub.MintTTL-time.Minute, err, hub.MintTTL)
	}
	if res.Seq != aliceMint.Seq {
		t.Fatalf("alice's held reservation committed at sequence %d, want %d", res.Seq, aliceMint.Seq)
	}

	// WHAT THE SPLIT DELIVERS INSTEAD OF THE WATERMARK: the reader at the head was
	// never held back by the outstanding mint for a moment, and it is handed the
	// straggler on the next read after it commits.
	b := mustHistory(t, h, carol, caught.Cursor, hub.MaxBatchLimit)
	if ids, want := batchIDs(b), []string{res.MessageID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("the reader at the head cursor %d was served %v, want %v — a reservation held for nearly the "+
			"whole TTL still lands ABOVE every live cursor when it is finally spent", caught.Cursor, ids, want)
	}
	if b.Messages[0].Seq != aliceMint.Seq {
		t.Fatalf("the straggler served to the head reader carries sequence %d, want %d", b.Messages[0].Seq, aliceMint.Seq)
	}

	t.Logf("QUANTIFIED — WHY THE WATERMARK WAS REJECTED: one agent held ONE reservation (sequence %d) for %v of a "+
		"%v TTL while %d further messages were acknowledged above it (head sequence %d, head position %d). A "+
		"watermark that stalled every reader at (lowest outstanding mint - 1) would have let any enrolled agent "+
		"withhold those %d durable, acknowledged messages from every reader on the bus for %v, at the cost of one "+
		"unspent mint. The seq/pos split stalled nobody: all %d were delivered as they committed, and the "+
		"straggler was delivered on the next read after it.",
		aliceMint.Seq, hub.MintTTL-time.Minute, hub.MintTTL, above, lastSeq, headPos, above, hub.MintTTL, above)
}

// batchIDs is the ordered message ids in a batch, so a failure message names
// what was delivered rather than dumping whole structs.
func batchIDs(b hub.Batch) []string {
	out := make([]string, 0, len(b.Messages))
	for _, m := range b.Messages {
		out = append(out, m.ID)
	}
	return out
}

// idsOfMessages is batchIDs for a bare message slice — historyOf returns one.
func idsOfMessages(msgs []store.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out
}
