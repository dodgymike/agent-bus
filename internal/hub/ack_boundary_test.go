package hub_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// testClock is a manual clock shared by the hub, its message store and the
// lifecycle table, so a test can move all three at once. Retention is the whole
// subject of two subtests below and a real clock cannot express it in seconds.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

// newTestClock starts JUST AHEAD of the real wall clock, and that is not a
// detail. hub.StaticRoster stamps an enrolment with the REAL time.Now, while
// every message this hub writes is stamped with the clock below, and the read
// path (store.Since) shows an agent only what arrived after it enrolled. A clock
// anchored to a fixed date is therefore either in the roster's past — in which
// case every poll in this file returns nothing and every "a poll settles
// nothing" assertion passes vacuously — or in its future by an arbitrary amount.
// One second ahead is the smallest offset that makes the fixture DELIVER.
func newTestClock() *testClock {
	return &testClock{t: time.Now().UTC().Add(time.Second)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ackBoundaryOptions are the knobs the boundary's behaviour actually depends
// on. Every one of them defaults to "the production value", so a subtest that
// sets none is exercising the real configuration.
type ackBoundaryOptions struct {
	clock        *testClock
	ackRetention time.Duration // 0 = ack.Retention
	msgMaxAge    time.Duration // 0 = the store default
	msgMaxBytes  int64         // 0 = the store default
	noAckTable   bool          // build the hub with NO lifecycle table at all

	// recorder REPLACES the real ack.Store. It is used by exactly one subtest,
	// to reach switch arms a real store cannot be driven into deterministically.
	recorder hub.AckRecorder
}

// newAckBoundaryHub builds a hub with a real WAL, a real lifecycle table and a
// controllable clock.
//
// It is deliberately NOT openAckHub (ack_seam_test.go): that helper exists to
// prove the SEND-path seam and takes no options, and widening it would make
// every one of its tests depend on knobs they do not use.
func newAckBoundaryHub(t *testing.T, o ackBoundaryOptions, agents ...string) (*hub.Hub, *ack.Store, string) {
	t.Helper()
	if o.clock == nil {
		o.clock = newTestClock()
	}
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	roster := hub.NewStaticRoster()

	var acks *ack.Store
	recorder := o.recorder
	if !o.noAckTable && recorder == nil {
		acks = ack.NewStore(ack.Options{Now: o.clock.Now, Retention: o.ackRetention})
		if err := acks.Attach(lg); err != nil {
			t.Fatalf("ack.Store.Attach: %v", err)
		}
		recorder = acks
	}

	h, err := hub.Open(hub.Options{
		BusID:     testBusID,
		DataDir:   dir,
		Durable:   lg,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(lg.Path(), fn) },
		NextIndex: lg.Recovered().NextIndex,
		Roster:    roster,
		Acks:      recorder,
		Now:       o.clock.Now,
		MaxAge:    o.msgMaxAge,
		MaxBytes:  o.msgMaxBytes,
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	enrolAll(t, roster, testBusID, agents...)
	return h, acks, dir
}

// sendTo sends one message and returns the correlation key, which on the ORIGIN
// bus IS the message's own id (store.Message.OriginID()).
func sendTo(t *testing.T, h *hub.Hub, from, to, key string) string {
	t.Helper()
	res, err := mintedSend(t, h, hub.SendRequest{
		Sender:         from,
		To:             to,
		Body:           []byte("body for " + key),
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("Send(%s): %v", key, err)
	}
	return res.MessageID
}

// ackedState is the row a test asserts against, or ("", false) when nothing is
// retained. It reads through the ack store rather than through the WAL so the
// SERVING copy and the log are both checked by the assertions that use each.
func ackedState(t *testing.T, acks *ack.Store, correlationKey, recipient string) (ack.Record, bool) {
	t.Helper()
	return acks.Lookup(correlationKey, recipient)
}

// deliveredAck is the request a well-behaved recipient makes.
func deliveredAck(correlationKey, recipient string) hub.RecipientAckRequest {
	return hub.RecipientAckRequest{
		Recipient:      recipient,
		CorrelationKey: correlationKey,
		Outcome:        ack.StateDelivered,
		AttestedBy:     ack.AttestedByRecipientSignatureUnverified,
	}
}

// ---------------------------------------------------------------------------
// TestRecipientAcknowledgementBoundary — ACK-6's proof command names this test.
// ---------------------------------------------------------------------------

// TestRecipientAcknowledgementBoundary is ACK-6: the boundary at which a
// message becomes `delivered` or `refused`, and every way it must REFUSE to.
//
// The subtests are grouped by the question they answer, and each one is written
// so that DELETING the guard it covers turns it red. That is the standard the
// task set — three guards in this epic were written to catch a defect and could
// not fire, because their fixture never reached the boundary they asserted on.
func TestRecipientAcknowledgementBoundary(t *testing.T) {
	t.Run("delivery to a poll is NOT receipt", testAckPollIsNotReceipt)
	t.Run("an offline recipient never settles", testAckOfflineRecipient)
	t.Run("a duplicate delivery is acked once", testAckDuplicateDelivery)
	t.Run("a second different terminal is refused", testAckTerminalConflict)
	t.Run("an expired lifecycle row is the uniform answer", testAckExpiredRow)
	t.Run("an expired MESSAGE can still be acked", testAckExpiredMessage)
	t.Run("an agent cannot settle a row it does not name", testAckWrongRecipient)
	t.Run("malformed acknowledgements write nothing", testAckShapeRefusals)
	t.Run("a bus with no lifecycle table says so", testAckNoTable)
	t.Run("every settler refusal maps to a sentinel, never the catch-all", testAckSettlerErrorMapping)
}

// testAckPollIsNotReceipt is THE ACK-1 RULING, asserted rather than assumed:
// delivery to an inbox or a poll is not recipient receipt.
//
// It is the subtest to read first and the one most likely to be broken by a
// well-meaning change. Both read paths are exercised — History (the batch poll)
// and Wait (the long poll) — because "the recipient has seen it" is exactly the
// fact somebody would be tempted to convert into a receipt, and there are two
// places to do it.
func testAckPollIsNotReceipt(t *testing.T) {
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "alpha", "beta")
	alpha, beta := agentID(t, testBusID, "alpha"), agentID(t, testBusID, "beta")
	key := sendTo(t, h, alpha, beta, "k-poll")

	r, ok := ackedState(t, acks, key, beta)
	if !ok || r.State != ack.StateAccepted {
		t.Fatalf("after the send the row is (%+v, %v), want accepted", r, ok)
	}

	// THE POLL. The recipient reads the message — twice, through both read
	// paths — and the sender-visible state must not move a millimetre.
	batch, err := h.History(beta, 0, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(batch.Messages) != 1 {
		t.Fatalf("the poll returned %d messages, want 1; a fixture that delivers nothing cannot prove that delivery settles nothing", len(batch.Messages))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := h.Wait(ctx, beta, 0, 10, 50*time.Millisecond); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if r, _ := ackedState(t, acks, key, beta); r.State != ack.StateAccepted {
		t.Fatalf("polling moved the row to %s; ACK-CONTRACT.md §4 rules that delivery to an inbox or a poll is NOT recipient receipt, because the bus never verifies the sender's signature and so cannot assert, on the recipient's behalf, a fact only the recipient can establish", r.State)
	}
	if rows := ackEntriesIn(t, dir); len(rows) != 1 {
		t.Fatalf("polling brought the log to %d lifecycle records, want 1 (the acceptance); a poll is REPLAYABLE, so an inferred receipt would fire many times for one message", len(rows))
	}

	// And the EXPLICIT acknowledgement — the only path there is — does settle it.
	res, err := h.AcknowledgeDelivery(deliveredAck(key, beta))
	if err != nil {
		t.Fatalf("AcknowledgeDelivery: %v", err)
	}
	if res.State != ack.StateDelivered || res.Duplicate {
		t.Fatalf("the first acknowledgement returned %+v, want delivered and not a duplicate", res)
	}
	if r, _ := ackedState(t, acks, key, beta); r.State != ack.StateDelivered {
		t.Fatalf("after an explicit ack the row is %s, want delivered", r.State)
	}
	// DURABLE BEFORE ACKNOWLEDGED (invariant 4): the settle record is in the log
	// by the time the call has returned, not after it.
	rows := ackEntriesIn(t, dir)
	if len(rows) != 2 {
		t.Fatalf("the log holds %d lifecycle records, want 2 (accepted, then delivered); invariant 4 requires the transition to be committed and fsynced BEFORE the recipient is told anything", len(rows))
	}
	last, err := ack.DecodeRecord(rows[1].Entry.Body)
	if err != nil {
		t.Fatalf("decoding the settle record: %v", err)
	}
	if last.State != ack.StateDelivered || last.AttestedBy != ack.AttestedByRecipientSignatureUnverified {
		t.Fatalf("the settle record is state=%s attested_by=%s, want delivered / recipient_signature_unverified", last.State, last.AttestedBy)
	}
	if last.SettledAt.IsZero() {
		t.Fatal("the settle record has no settled_at; a terminal row without one could never be swept (§7.2)")
	}
}

// testAckOfflineRecipient pins the honest cost §4 states rather than hides: an
// agent that never ACKs leaves its sender's row non-terminal, and NOTHING —
// not time, not another agent's traffic, not a second send — converts silence
// into an outcome.
func testAckOfflineRecipient(t *testing.T) {
	clock := newTestClock()
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{clock: clock}, "alpha", "beta", "gamma")
	alpha, beta, gamma := agentID(t, testBusID, "alpha"), agentID(t, testBusID, "beta"), agentID(t, testBusID, "gamma")

	key := sendTo(t, h, alpha, beta, "k-offline")

	// beta never polls and never acks. Time passes, other agents transact, and
	// one of them acknowledges its OWN message.
	clock.Advance(ack.Retention / 2)
	otherKey := sendTo(t, h, alpha, gamma, "k-offline-other")
	if _, err := h.AcknowledgeDelivery(deliveredAck(otherKey, gamma)); err != nil {
		t.Fatalf("gamma's acknowledgement: %v", err)
	}

	r, ok := ackedState(t, acks, key, beta)
	if !ok {
		t.Fatal("the offline recipient's row is gone before its retention window; nothing about being offline may sweep it early")
	}
	if r.State != ack.StateAccepted {
		t.Fatalf("the offline recipient's row is %s, want accepted; there is no delivery deadline and no path by which NOT acknowledging becomes an outcome", r.State)
	}
	if !r.SettledAt.IsZero() {
		t.Fatalf("the offline recipient's row carries settled_at %s; a non-terminal row has not settled", r.SettledAt)
	}
	// Three records: two acceptances and gamma's settle. beta's row contributed
	// exactly one, which is the point.
	if rows := ackEntriesIn(t, dir); len(rows) != 3 {
		t.Fatalf("the log holds %d lifecycle records, want 3 (two acceptances, one settle)", len(rows))
	}

	// AND THE RECIPIENT MAY STILL ACK LATER. An acknowledgement is not bound to
	// the session or the poll that delivered the message; only the principal is
	// checked, so an agent that was offline for hours can still settle its row.
	if _, err := h.AcknowledgeDelivery(deliveredAck(key, beta)); err != nil {
		t.Fatalf("a late acknowledgement from a recipient that was offline was refused: %v", err)
	}
}

// testAckDuplicateDelivery covers invariant 10's FIRST case on this plane.
//
// Delivery here is AT-LEAST-ONCE and the cursor is client-held and replayable,
// so a recipient genuinely can receive the same message twice — and both copies
// carry the SAME correlation key, so the second acknowledgement is a legitimate
// retry. It returns the original result, re-applies nothing, does not error, and
// disconnects nobody (§12).
func testAckDuplicateDelivery(t *testing.T) {
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "alpha", "beta")
	alpha, beta := agentID(t, testBusID, "alpha"), agentID(t, testBusID, "beta")
	key := sendTo(t, h, alpha, beta, "k-dup")

	first, err := h.AcknowledgeDelivery(deliveredAck(key, beta))
	if err != nil {
		t.Fatalf("the first acknowledgement: %v", err)
	}
	if first.Duplicate {
		t.Fatal("the FIRST acknowledgement was reported as a duplicate")
	}

	for i := 0; i < 3; i++ {
		res, err := h.AcknowledgeDelivery(deliveredAck(key, beta))
		if err != nil {
			t.Fatalf("re-acknowledgement %d returned %v; the same key with the same outcome is a legitimate retry and must return the ORIGINAL result", i, err)
		}
		if res.State != ack.StateDelivered {
			t.Fatalf("re-acknowledgement %d reported state %s, want the original delivered", i, res.State)
		}
		if !res.Duplicate {
			t.Fatalf("re-acknowledgement %d was not reported as a duplicate", i)
		}
	}

	// NOTHING WAS RE-APPLIED. Three retries wrote no record at all, which is
	// what "return the original result" means in an append-only log.
	if rows := ackEntriesIn(t, dir); len(rows) != 2 {
		t.Fatalf("after three retries the log holds %d lifecycle records, want 2 (accepted, delivered); a retry that appends is a retry that re-applied", len(rows))
	}
	if r, _ := ackedState(t, acks, key, beta); r.State != ack.StateDelivered {
		t.Fatalf("the row is %s after the retries, want delivered", r.State)
	}
}

// testAckTerminalConflict covers invariant 10's SECOND case: the same pair with
// a DIFFERENT terminal outcome. Rejected AND logged; the FIRST terminal stands;
// nothing is disconnected. Terminal is ABSORBING (§8.1) — never revisited, never
// reopened, never downgraded.
func testAckTerminalConflict(t *testing.T) {
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "alpha", "beta")
	alpha, beta := agentID(t, testBusID, "alpha"), agentID(t, testBusID, "beta")
	key := sendTo(t, h, alpha, beta, "k-conflict")

	if _, err := h.AcknowledgeDelivery(deliveredAck(key, beta)); err != nil {
		t.Fatalf("the first acknowledgement: %v", err)
	}

	conflicting := hub.RecipientAckRequest{
		Recipient:      beta,
		CorrelationKey: key,
		Outcome:        ack.StateRefused,
		Class:          ack.ClassRecipientRefusedPolicy,
		AttestedBy:     ack.AttestedByRecipientSignatureUnverified,
	}
	_, err := h.AcknowledgeDelivery(conflicting)
	if !errors.Is(err, hub.ErrAckTerminalConflict) {
		t.Fatalf("a second, DIFFERENT terminal returned %v, want ErrAckTerminalConflict", err)
	}

	if r, _ := ackedState(t, acks, key, beta); r.State != ack.StateDelivered || r.Class != "" {
		t.Fatalf("the row is now state=%s class=%s; the FIRST terminal must stand", r.State, r.Class)
	}
	if rows := ackEntriesIn(t, dir); len(rows) != 2 {
		t.Fatalf("the rejected conflict brought the log to %d lifecycle records, want 2; a rejected transition writes nothing", len(rows))
	}
}

// testAckExpiredRow is the EXPIRED case at the layer that actually decides it:
// the lifecycle row has been swept by §11's retention, so there is nothing to
// settle.
//
// Two things are asserted and the second is the one that matters: the refusal is
// the uniform answer, and NO ROW IS CREATED. `unknown` is a REPORTING value that
// must never be written back (§8.1), and a boundary that created a row here
// would let any authenticated agent mint durable rows for correlation keys it
// invented.
func testAckExpiredRow(t *testing.T) {
	clock := newTestClock()
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{clock: clock, ackRetention: time.Hour}, "alpha", "beta")
	alpha, beta := agentID(t, testBusID, "alpha"), agentID(t, testBusID, "beta")
	key := sendTo(t, h, alpha, beta, "k-expired-row")

	clock.Advance(time.Hour + time.Minute)

	_, err := h.AcknowledgeDelivery(deliveredAck(key, beta))
	if !errors.Is(err, hub.ErrAckNotRetained) {
		t.Fatalf("acknowledging a swept row returned %v, want ErrAckNotRetained (the uniform answer of §13.3)", err)
	}
	if r, ok := ackedState(t, acks, key, beta); ok {
		t.Fatalf("the refusal CREATED a row %+v; `unknown` is a reporting value and must never come back to be written, and a boundary that creates rows lets any agent mint them for keys it invented", r)
	}
	if n := acks.Len(); n != 0 {
		t.Fatalf("the lifecycle table holds %d rows after the refusal, want 0", n)
	}
	if rows := ackEntriesIn(t, dir); len(rows) != 1 {
		t.Fatalf("the refusal brought the log to %d lifecycle records, want 1 (the original acceptance)", len(rows))
	}
}

// testAckExpiredMessage is the OTHER meaning of "expired message", and it is the
// one that would be got wrong by a boundary that checked the message store.
//
// A message body is retained for one day OR a bounded number of bytes, whichever
// bites first, so a busy bus prunes bodies long before a lifecycle row expires.
// The recipient has the message — it polled it before the prune — and its ACK
// must still be accepted; refusing would strand the sender's row non-terminal
// for the rest of the window for no reason the recipient could act on.
func testAckExpiredMessage(t *testing.T) {
	// A MESSAGE age bound of one minute against a LIFECYCLE retention of 24h, so
	// the body expires while the row is still comfortably live. That gap is the
	// whole point: in production it is one day of bodies (or a byte cap, whichever
	// bites first) against 24h of rows anchored at a different moment.
	clock := newTestClock()
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{clock: clock, msgMaxAge: time.Minute}, "alpha", "beta")
	alpha, beta := agentID(t, testBusID, "alpha"), agentID(t, testBusID, "beta")

	key := sendTo(t, h, alpha, beta, "k-expired-msg")
	// The recipient polls it while it is still there — the realistic ordering.
	if _, err := h.History(beta, 0, 10); err != nil {
		t.Fatalf("History: %v", err)
	}
	clock.Advance(2 * time.Minute)
	sendTo(t, h, alpha, beta, "k-evictor")

	if _, ok := h.Store().ByID(key); ok {
		t.Fatal("the message store still holds the first message after twice its MaxAge, so this fixture never reaches the boundary it exists to test; FIX THE FIXTURE rather than the assertion -- a subtest that cannot reach its subject is the vacuity this epic has already shipped three times")
	}

	res, err := h.AcknowledgeDelivery(deliveredAck(key, beta))
	if err != nil {
		t.Fatalf("acknowledging a message whose BODY has been pruned returned %v; the lifecycle row is the authority and the message store is never consulted, because the two retention regimes differ and requiring both would strand rows a recipient demonstrably received", err)
	}
	if res.State != ack.StateDelivered {
		t.Fatalf("the acknowledgement reported %s, want delivered", res.State)
	}
	if r, _ := ackedState(t, acks, key, beta); r.State != ack.StateDelivered {
		t.Fatalf("the row is %s, want delivered", r.State)
	}
	if rows := ackEntriesIn(t, dir); len(rows) != 3 {
		t.Fatalf("the log holds %d lifecycle records, want 3 (two acceptances, one settle)", len(rows))
	}
}

// testAckWrongRecipient is the authorization guard, and it is structural: the
// recipient is the AUTHENTICATED PRINCIPAL and it is the second half of the
// lookup key, so an agent can only ever reach the row that names it.
//
// ack.Store.Settle's own doc names the defect this closes — "without that, agent
// B can mark agent A's message refused" — and notes that its internal sender
// guard CANNOT fire from this path, because Settle copies the sender forward
// from the row it found.
func testAckWrongRecipient(t *testing.T) {
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "alpha", "beta", "gamma")
	alpha, beta, gamma := agentID(t, testBusID, "alpha"), agentID(t, testBusID, "beta"), agentID(t, testBusID, "gamma")
	key := sendTo(t, h, alpha, beta, "k-wrong-recipient")

	// gamma, an enrolled and perfectly well-authenticated agent, tries to settle
	// a message addressed to beta.
	_, err := h.AcknowledgeDelivery(deliveredAck(key, gamma))
	if !errors.Is(err, hub.ErrAckNotRetained) {
		t.Fatalf("a third agent acknowledging somebody else's message returned %v, want ErrAckNotRetained -- and it must be the SAME answer a never-existed key gets, or the refusal is a message-existence oracle", err)
	}
	// The SENDER cannot settle its own message either: it was not addressed.
	if _, err := h.AcknowledgeDelivery(deliveredAck(key, alpha)); !errors.Is(err, hub.ErrAckNotRetained) {
		t.Fatalf("the SENDER acknowledging its own message returned %v, want ErrAckNotRetained", err)
	}
	// A key that never existed answers identically.
	if _, err := h.AcknowledgeDelivery(deliveredAck(testBusID+"-999999", gamma)); !errors.Is(err, hub.ErrAckNotRetained) {
		t.Fatalf("a never-existed key returned %v, want the same ErrAckNotRetained", err)
	}
	// AND SO DOES A MALFORMED ONE. This is the FOURTH fact ErrAckNotRetained's
	// doc promises it collapses, and it was the HIGH finding of ACK-6's security
	// gate: without an id check at this boundary the key fell through to
	// ack.Store's own validatePair, whose ErrInvalidRecord landed in the
	// catch-all — so a malformed or ABSENT key answered a 500 and an unthrottled
	// ERROR line while an unknown one answered `unknown`. A merely buggy client
	// that omits the field reaches it, and it handed any authenticated agent a
	// zero-prerequisite remote 5xx driver.
	for _, malformed := range []string{
		"",                        // the field omitted entirely
		"   ",                     // whitespace, which names nobody
		"not-a-message-id",        // no bus half
		strings.Repeat("x", 4000), // far over ids.MaxMessageIDLen
		testBusID + "-notanumber", // right shape, unparseable sequence
	} {
		if _, err := h.AcknowledgeDelivery(deliveredAck(malformed, gamma)); !errors.Is(err, hub.ErrAckNotRetained) {
			t.Fatalf("a MALFORMED key (%d bytes) returned %v, want the same ErrAckNotRetained as every other miss; a distinct answer here tells a prober which of its guesses were even well-formed, and a 500 hands it a remote error-log driver",
				len(malformed), err)
		}
	}

	if r, _ := ackedState(t, acks, key, beta); r.State != ack.StateAccepted {
		t.Fatalf("beta's row is %s after three foreign acknowledgements, want accepted", r.State)
	}
	if _, ok := ackedState(t, acks, key, gamma); ok {
		t.Fatal("a row was created for gamma, who was never addressed")
	}
	if rows := ackEntriesIn(t, dir); len(rows) != 1 {
		t.Fatalf("three refused acknowledgements brought the log to %d lifecycle records, want 1", len(rows))
	}

	// And the RIGHT recipient still works, so the guard refuses the forgery and
	// not the feature.
	if _, err := h.AcknowledgeDelivery(deliveredAck(key, beta)); err != nil {
		t.Fatalf("the addressed recipient was refused: %v", err)
	}
}

// testAckShapeRefusals is the closed-vocabulary guard, table-driven.
//
// The load-bearing row is the BUS-EMITTED CLASS: a recipient sending
// outcome=refused with class=horizon_expired would have this bus record ITS OWN
// routing failure as THE RECIPIENT'S DECISION, durably, in a trail a sender
// reads. Every row asserts the same two things — ErrInvalidAck, and that the log
// is untouched — because a refusal that still wrote would be worse than one that
// was allowed.
func testAckShapeRefusals(t *testing.T) {
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "alpha", "beta")
	alpha, beta := agentID(t, testBusID, "alpha"), agentID(t, testBusID, "beta")
	key := sendTo(t, h, alpha, beta, "k-shape")

	for _, tc := range []struct {
		name string
		req  hub.RecipientAckRequest
	}{
		{"a BUS-EMITTED class on a recipient refusal is a forged routing claim", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateRefused,
			Class: ack.ClassHorizonExpired, AttestedBy: ack.AttestedByRecipientSignatureUnverified,
		}},
		{"local_capacity is bus-emitted too", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateRefused,
			Class: ack.ClassLocalCapacity, AttestedBy: ack.AttestedByRecipientSignatureUnverified,
		}},
		{"a refusal with NO class explains nothing", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateRefused,
			AttestedBy: ack.AttestedByRecipientSignatureUnverified,
		}},
		{"an unrecognised class is REJECTED, never defaulted", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateRefused,
			Class: ack.Class("recipient_refused_because_i_said_so"), AttestedBy: ack.AttestedByRecipientSignatureUnverified,
		}},
		{"a POSITIVE terminal carries no class", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateDelivered,
			Class: ack.ClassRecipientRefusedPolicy, AttestedBy: ack.AttestedByRecipientSignatureUnverified,
		}},
		{"undeliverable is a ROUTING claim an agent may not make", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateUndeliverable,
			Class: ack.ClassNoRoute, AttestedBy: ack.AttestedByRecipientSignatureUnverified,
		}},
		{"a NON-TERMINAL state is not declarable", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateAccepted,
			AttestedBy: ack.AttestedByRecipientSignatureUnverified,
		}},
		{"in_flight is not declarable either", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateInFlight,
			AttestedBy: ack.AttestedByRecipientSignatureUnverified,
		}},
		{"the zero state is not a state", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key,
			AttestedBy: ack.AttestedByRecipientSignatureUnverified,
		}},
		{"an agent may not claim PEER BUS attestation", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateDelivered,
			AttestedBy: ack.AttestedByPeerBus,
		}},
		{"an absent attestation label is refused", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateDelivered,
		}},
		{"an invented attestation label is refused", hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: key, Outcome: ack.StateDelivered,
			AttestedBy: ack.Attestation("verified"),
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.AcknowledgeDelivery(tc.req)
			if !errors.Is(err, hub.ErrInvalidAck) {
				t.Fatalf("returned %v, want ErrInvalidAck", err)
			}
			if r, _ := ackedState(t, acks, key, beta); r.State != ack.StateAccepted {
				t.Fatalf("the row moved to %s; a refused acknowledgement must write nothing", r.State)
			}
			if rows := ackEntriesIn(t, dir); len(rows) != 1 {
				t.Fatalf("the log holds %d lifecycle records, want 1 (the acceptance only)", len(rows))
			}
		})
	}

	// The vocabulary is refused, the feature is not: the three recipient-emitted
	// classes are all accepted, on their own fresh rows.
	for i, class := range []ack.Class{
		ack.ClassRecipientRefusedPolicy,
		ack.ClassRecipientRefusedUndecodable,
		ack.ClassRecipientRefusedNotAddressed,
	} {
		k := sendTo(t, h, alpha, beta, "k-shape-ok-"+string(rune('a'+i)))
		res, err := h.AcknowledgeDelivery(hub.RecipientAckRequest{
			Recipient: beta, CorrelationKey: k, Outcome: ack.StateRefused,
			Class: class, AttestedBy: ack.AttestedByRecipientSignatureUnverified,
		})
		if err != nil {
			t.Fatalf("refusing with the legitimate class %s returned %v", class, err)
		}
		if res.State != ack.StateRefused || res.Class != class {
			t.Fatalf("the result is %+v, want refused with class %s", res, class)
		}
	}
}

// testAckNoTable covers the build with no lifecycle table wired. It must say so
// rather than 404 or silently succeed: an acknowledgement that is accepted and
// discarded is indistinguishable, to the recipient, from one that was recorded.
func testAckNoTable(t *testing.T) {
	h, _, dir := newAckBoundaryHub(t, ackBoundaryOptions{noAckTable: true}, "alpha", "beta")
	alpha, beta := agentID(t, testBusID, "alpha"), agentID(t, testBusID, "beta")
	key := sendTo(t, h, alpha, beta, "k-no-table")

	if _, err := h.AcknowledgeDelivery(deliveredAck(key, beta)); !errors.Is(err, hub.ErrNoAckTable) {
		t.Fatalf("returned %v, want ErrNoAckTable", err)
	}
	if rows := ackEntriesIn(t, dir); len(rows) != 0 {
		t.Fatalf("a bus with no lifecycle table wrote %d lifecycle records", len(rows))
	}
}

// stubSettler is an AckSettler that returns exactly what a test tells it to. It
// exists for ONE purpose: reaching the arms of AcknowledgeDelivery's switch that
// a real ack.Store cannot be driven into deterministically.
type stubSettler struct {
	row   ack.Record
	found bool
	err   error
}

func (s *stubSettler) Accept(correlationKey, sender, recipient string) error { return nil }

func (s *stubSettler) Settle(correlationKey, recipient string, state ack.State, class ack.Class, attestedBy ack.Attestation) error {
	return s.err
}

func (s *stubSettler) Lookup(correlationKey, recipient string) (ack.Record, bool) {
	return s.row, s.found
}

// testAckSettlerErrorMapping pins the SENTINEL every lifecycle-table refusal
// comes back as.
//
// It exists because ACK-6's security gate found TWO arms missing, and both were
// invisible to every other test here: ack.ErrConcurrentTransition (four
// concurrent BYTE-IDENTICAL acknowledgements answered one 500, breaking
// invariant 10's first case with the loudest status there is) and
// ack.ErrInvalidRecord (a malformed key answered 500 rather than the uniform
// refusal). Both fell into the catch-all, which is the arm that must be reached
// by NOTHING a client can send.
//
// A real ack.Store cannot be driven into ErrConcurrentTransition on demand — it
// is a race by definition — so the seam is stubbed. That is the ONE thing
// stubbed in this file, and it is stubbed because the alternative is a flaky
// test that proves nothing on the run where it does not race.
func testAckSettlerErrorMapping(t *testing.T) {
	beta := agentID(t, testBusID, "beta")
	key := testBusID + "-42"

	for _, tc := range []struct {
		name string
		from error
		want error
	}{
		{"no record is the uniform refusal", ack.ErrNoRecord, hub.ErrAckNotRetained},
		{"a malformed record is ALSO the uniform refusal", ack.ErrInvalidRecord, hub.ErrAckNotRetained},
		{"a concurrent transition is transient, not a fault", ack.ErrConcurrentTransition, hub.ErrAckInFlight},
		{"a different terminal is the conflict", ack.ErrTerminal, hub.ErrAckTerminalConflict},
		{"no durable log is invariant 4's refusal", ack.ErrNotDurable, hub.ErrNotDurable},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newAckBoundaryHub(t, ackBoundaryOptions{recorder: &stubSettler{err: tc.from}}, "alpha", "beta")
			_, err := h.AcknowledgeDelivery(deliveredAck(key, beta))
			if !errors.Is(err, tc.want) {
				t.Fatalf("a settler returning %v produced %v, want %v; an unmapped refusal falls into the catch-all, which is a 500 and an ERROR log line for something a client can send at will", tc.from, err, tc.want)
			}
		})
	}
}
