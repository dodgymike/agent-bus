// RELAY-24-BLOCKER-HUBINGEST: the exported relay-ingest entry point.
//
// # What was impossible before this, and is the whole point of the suite
//
// relay.LocalIngest.AcceptRelayed could not be implemented over this package's
// exported API, so relay.NewAcceptor could not be built, so RelayConfig.AcceptRelay
// was nil and httpapi.PeerSurface would not mount. The reason was a single fact
// with three enforcement points: a relayed message's Sender belongs to the ORIGIN
// bus by construction (invariant 2), and EVERY exported write path here refused
// exactly that — publish's ErrUnknownSender, Mint's ErrUnknownSender, and the
// mint reservation publish then consumes, which a peer bus can never obtain.
//
// The first two tests below are that hole, stated as assertions: hub.Mint and
// hub.Send refuse a foreign sender, and hub.IngestRelayed accepts it. Delete
// IngestRelayed and TestForeignSenderIsRefusedByEveryOtherWritePath still passes —
// it is the PROOF THE HOLE WAS REAL, kept deliberately.
package hub_test

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// The federation fixture: an origin bus, a middle hop, and this bus (testBusID)
// as the ingesting one. The ids are literals for the same reason testBusID is —
// a failure message names the same buses every run.
const (
	riOriginBus = "busorigin"
	riMiddleBus = "busmiddle"
)

// riIngest is the standard arriving message: from the origin bus's alpha, to
// this bus's bob, having travelled origin -> middle. Recipients and the local
// agent name are parameters because half these tests vary one of them.
func riIngest(t *testing.T, key string, recipients ...string) hub.RelayedIngestRequest {
	t.Helper()
	return hub.RelayedIngestRequest{
		Sender:             agentID(t, riOriginBus, "alpha"),
		Recipients:         recipients,
		Body:               []byte("a message that crossed two buses to get here"),
		OriginMessageID:    key,
		BusPath:            []string{riOriginBus, riMiddleBus},
		TimestampUnixMilli: fixtureTimestampMs,
		Signature:          fixtureSignature(),
	}
}

// riOriginMessageID is an origin-minted message id, which is what a relayed
// message's idempotency key IS (relay.RelayedMessage.IdempotencyKey EQUALS
// OriginMessageID). Using a real one rather than "k-whatever" keeps the fixture
// honest about the shape the key actually has in production.
func riOriginMessageID(t *testing.T, seq uint64) string {
	t.Helper()
	id, err := ids.MessageID(riOriginBus, seq)
	if err != nil {
		t.Fatalf("ids.MessageID(%q, %d): %v", riOriginBus, seq, err)
	}
	return id
}

// ---------------------------------------------------------------------------
// The hole
// ---------------------------------------------------------------------------

// TestForeignSenderIsRefusedByEveryOtherWritePath pins WHY IngestRelayed has to
// exist. It asserts a refusal, so it passes with or without the new code — and
// that is the point: it is the executable statement of the gap, and if either
// refusal is ever relaxed, THIS test goes red and someone must decide whether a
// peer's id may be asserted through a local route (it may not).
func TestForeignSenderIsRefusedByEveryOtherWritePath(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestHub(t, "bob")
	foreign := agentID(t, riOriginBus, "alpha")
	local := agentID(t, testBusID, "bob")

	if _, err := h.Mint(hub.MintRequest{Sender: foreign, Op: "send", IdempotencyKey: "k-foreign"}); !errors.Is(err, hub.ErrUnknownSender) {
		t.Fatalf("Mint(foreign sender) = %v, want ErrUnknownSender: a peer bus's agent is not on this roster and can never obtain a reservation here", err)
	}
	_, err := h.Send(hub.SendRequest{Sender: foreign, To: local, Body: []byte("x"), IdempotencyKey: "k-foreign"})
	if !errors.Is(err, hub.ErrUnknownSender) {
		t.Fatalf("Send(foreign sender) = %v, want ErrUnknownSender: this is the refusal that made relay.LocalIngest unimplementable", err)
	}
}

// TestIngestRelayedAcceptsAForeignSenderThatHoldsNoMint is the entry point doing
// the one thing no other write path can: recording a message whose sender is NOT
// on this roster and NEVER presented a reservation.
//
// It asserts the whole shape of one acceptance at once — the outcome, the local
// id, the serving copy, the durable record and the recorded provenance —
// because each of those is a separate way for the seam to look like it works
// while being useless to the relay.
func TestIngestRelayedAcceptsAForeignSenderThatHoldsNoMint(t *testing.T) {
	t.Parallel()
	h, lg, _ := newTestHub(t, "bob")
	bob := agentID(t, testBusID, "bob")
	req := riIngest(t, riOriginMessageID(t, 7), bob)

	res, err := h.IngestRelayed(context.Background(), req)
	if err != nil {
		t.Fatalf("IngestRelayed(foreign sender, no mint) = %v; this is the hole RELAY-24 is blocked on", err)
	}
	if res.Outcome != idem.OutcomeNew {
		t.Errorf("Outcome = %s, want %s on a first arrival", res.Outcome, idem.OutcomeNew)
	}
	// INVARIANT 1: the id is OURS, minted here, never the origin's adopted.
	if !strings.HasPrefix(res.MessageID, testBusID+"-") {
		t.Errorf("MessageID = %q, want an id in THIS bus's namespace (%q-<seq>): a bus mints its own ids and never adopts a peer's", res.MessageID, testBusID)
	}
	if res.MessageID == req.OriginMessageID {
		t.Errorf("MessageID = %q, which is the ORIGIN's id: adopting a peer's id makes two buses claim one identity", res.MessageID)
	}
	if res.Seq == 0 {
		t.Error("Seq = 0: the local sequence must be minted internally, because a peer bus holds no reservation to spend")
	}

	// The serving copy: the message is deliverable to the local recipient.
	msgs, _, _ := h.Store().Since(bob, fixtureEnrolledAt, 0, 10)
	if len(msgs) != 1 {
		t.Fatalf("the serving copy holds %d messages for %s, want 1", len(msgs), bob)
	}
	if msgs[0].Sender != req.Sender {
		t.Errorf("the served message's sender = %q, want the ORIGIN's %q", msgs[0].Sender, req.Sender)
	}

	// THE PROVENANCE, which is what makes the hop auditable (invariant 6) and
	// what going through Send would have destroyed: Send records
	// store.LocalBusPath(busID) — a claim that the message originated here.
	wantPath := []string{riOriginBus, riMiddleBus, testBusID}
	if !riEqualPath(msgs[0].BusPath, wantPath) {
		t.Errorf("the served message's bus path = %v, want %v (received hops, then THIS bus appended)", msgs[0].BusPath, wantPath)
	}

	// The DURABLE record, read back off disk: memory is the serving copy, disk is
	// the truth (invariant 5).
	replayed := riReplayMessages(t, lg)
	if len(replayed) != 1 {
		t.Fatalf("the durable log holds %d message records, want 1", len(replayed))
	}
	if replayed[0].ID != res.MessageID {
		t.Errorf("the durable record names message %q, IngestRelayed returned %q", replayed[0].ID, res.MessageID)
	}
	if !riEqualPath(replayed[0].BusPath, wantPath) {
		t.Errorf("the DURABLE record's bus path = %v, want %v: a restart must not rewrite the provenance of a relayed message", replayed[0].BusPath, wantPath)
	}
	// The origin's clock is recorded as PROVENANCE and is NOT the acceptance
	// time: SentAt is an authorization input (the enrolment epoch), so a peer
	// must not be able to choose it.
	if replayed[0].TimestampUnixMilli != fixtureTimestampMs {
		t.Errorf("the durable record's origin timestamp = %d, want %d", replayed[0].TimestampUnixMilli, fixtureTimestampMs)
	}
	if replayed[0].SentAt.UnixMilli() == fixtureTimestampMs {
		t.Error("SentAt equals the ORIGIN's signed clock reading; this bus must stamp its own acceptance time, or a peer chooses which local agents can see the message")
	}
}

// TestIngestRelayedNeitherRequiresNorConsumesAMint is the second half of "no
// reservation". The first test proves none is REQUIRED; this one proves an
// unrelated one is not silently SPENT — the zero mintKey is a real key in the
// mint table, so a relayed ingest that ran the local delete path would evict
// whatever sat under it.
func TestIngestRelayedNeitherRequiresNorConsumesAMint(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestHub(t, "alice", "bob")
	alice := agentID(t, testBusID, "alice")
	bob := agentID(t, testBusID, "bob")

	// A local agent holds a reservation across the relayed ingest.
	mint := mintFor(t, h, alice, "send", "k-local")
	if mint.MessageID == "" {
		t.Fatal("the fixture mint was refused")
	}
	if _, err := h.IngestRelayed(context.Background(), riIngest(t, riOriginMessageID(t, 11), bob)); err != nil {
		t.Fatalf("IngestRelayed: %v", err)
	}

	// THE RESERVATION IS STILL THERE, proven by RE-MINTING under the same key: a
	// re-mint is answered from the outstanding-mint table and returns the
	// ORIGINAL assignment with Replayed set (invariant 10). If the ingest had run
	// the local delete path, mk would have been the ZERO mintKey and this key
	// would be untouched — but the ingest also must not have allocated a
	// reservation of its own, and a fresh assignment here would show either
	// mistake.
	again, err := h.Mint(hub.MintRequest{Sender: alice, Op: "send", IdempotencyKey: "k-local"})
	if err != nil {
		t.Fatalf("re-minting after a relayed ingest = %v, want the original assignment replayed", err)
	}
	if !again.Replayed || again.MessageID != mint.MessageID || again.Seq != mint.Seq {
		t.Fatalf("re-mint returned %s (seq %d, replayed=%v), want the ORIGINAL %s (seq %d) replayed: the relayed ingest must not disturb the mint table — a client has already SIGNED that assignment",
			again.MessageID, again.Seq, again.Replayed, mint.MessageID, mint.Seq)
	}
	// NOTE ON WHAT THIS TEST DELIBERATELY DOES NOT DO: it does not then SPEND the
	// reservation. It cannot, and neither can any client, because of a
	// PRE-EXISTING defect that has nothing to do with the relay ingest —
	// store.Append requires strictly increasing sequences while SIGN-1's
	// reserve-then-send lets reservations be spent in ANY order, so a lower
	// sequence spent after a higher one POISONS the bus. Two purely local clients
	// reproduce it with no relay involved: alice mints 1, bob mints 2, bob sends,
	// alice sends, and the bus stops for ever. It is reported as a P0 follow-up;
	// asserting the spend here would attribute someone else's defect to this
	// change.
}

// TestIngestRelayedMintsLocalSequencesThatNeverRepeat: invariant 1, across the
// two allocators that now exist. The relayed path allocates from the same
// ids.Sequence under the same lock, so relayed and minted traffic interleave
// without ever producing a number twice.
func TestIngestRelayedMintsLocalSequencesThatNeverRepeat(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestHub(t, "alice", "bob")
	alice := agentID(t, testBusID, "alice")
	bob := agentID(t, testBusID, "bob")

	seen := map[uint64]string{}
	note := func(seq uint64, what string) {
		t.Helper()
		if prev, dup := seen[seq]; dup {
			t.Fatalf("sequence %d was issued twice: first to %s, then to %s (invariant 1: ids are never reused)", seq, prev, what)
		}
		seen[seq] = what
	}
	for i := 0; i < 3; i++ {
		key := "k-mixed-" + string(rune('a'+i))
		m := mintFor(t, h, alice, "send", key)
		if m.MessageID == "" {
			t.Fatalf("mint %d refused", i)
		}
		note(m.Seq, "a local mint")
		if _, err := h.Send(hub.SendRequest{Sender: alice, To: bob, Body: []byte("local"), IdempotencyKey: key, SignedMint: m}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		res, err := h.IngestRelayed(context.Background(), riIngest(t, riOriginMessageID(t, uint64(100+i)), bob))
		if err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
		note(res.Seq, "a relayed ingest")
	}
}

// ---------------------------------------------------------------------------
// Invariant 10: the three-way outcome, uncollapsed
// ---------------------------------------------------------------------------

// TestIngestRelayedSecondArrivalReportsOutcomeRetry is the test RELAY-24 needs
// and the one the whole "return the outcome, not a bool" requirement exists for.
//
// relay.Acceptor re-forwards on idem.OutcomeNew ALONE. idem.Outcome's zero value
// IS OutcomeNew, so a seam that reported a bool and synthesised the outcome would
// answer "new" for the second arrival and forward it onward — which in a cyclic
// topology is an amplification loop, and is exactly the failure this assertion
// can detect only because the outcome travels uncollapsed.
//
// The second arrival deliberately comes by a DIFFERENT PATH: dedupe is scoped on
// (origin sender, OpRelay, origin message id), never on the route, so one message
// reaching us twice through a mesh resolves to ONE key.
func TestIngestRelayedSecondArrivalReportsOutcomeRetry(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestHub(t, "bob")
	bob := agentID(t, testBusID, "bob")
	key := riOriginMessageID(t, 42)

	first, err := h.IngestRelayed(context.Background(), riIngest(t, key, bob))
	if err != nil {
		t.Fatalf("first arrival: %v", err)
	}
	if first.Outcome != idem.OutcomeNew {
		t.Fatalf("first arrival Outcome = %s, want %s", first.Outcome, idem.OutcomeNew)
	}

	again := riIngest(t, key, bob)
	again.BusPath = []string{riOriginBus} // the same message, a shorter route
	second, err := h.IngestRelayed(context.Background(), again)
	if err != nil {
		t.Fatalf("second arrival of the same message = %v, want a replayed acceptance: a duplicate is the NORMAL steady state of a cyclic topology and must not be an error", err)
	}
	if second.Outcome != idem.OutcomeRetry {
		t.Fatalf("second arrival Outcome = %s, want %s. relay.Acceptor re-forwards on %s ALONE, and %s is idem.Outcome's ZERO VALUE — reporting it here is how a duplicate becomes an amplification loop",
			second.Outcome, idem.OutcomeRetry, idem.OutcomeNew, idem.OutcomeNew)
	}
	if second.MessageID != first.MessageID {
		t.Errorf("second arrival MessageID = %q, want the ORIGINAL %q replayed verbatim (invariant 10)", second.MessageID, first.MessageID)
	}
	if second.Seq != first.Seq {
		t.Errorf("second arrival Seq = %d, want the original %d; a fresh number means a second message was written", second.Seq, first.Seq)
	}
	msgs, _, _ := h.Store().Since(bob, fixtureEnrolledAt, 0, 10)
	if len(msgs) != 1 {
		t.Fatalf("the serving copy holds %d messages after ONE message arrived twice, want 1: the duplicate must not be re-applied", len(msgs))
	}
}

// TestIngestRelayedKeyReusedWithADifferentPayloadIsAViolation: invariant 10's
// third case, which must not collapse into either of the other two. It is
// reported BOTH as the sentinel and as the value, and both are asserted — a
// caller may classify it either way, and relay.Acceptor's own suite exercises
// both shapes.
func TestIngestRelayedKeyReusedWithADifferentPayloadIsAViolation(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestHub(t, "bob")
	bob := agentID(t, testBusID, "bob")
	key := riOriginMessageID(t, 43)

	if _, err := h.IngestRelayed(context.Background(), riIngest(t, key, bob)); err != nil {
		t.Fatalf("first arrival: %v", err)
	}
	other := riIngest(t, key, bob)
	other.Body = []byte("a DIFFERENT payload presented under the same origin message id")

	res, err := h.IngestRelayed(context.Background(), other)
	if !errors.Is(err, hub.ErrIdempotencyKeyReused) {
		t.Fatalf("same key + different payload = %v, want ErrIdempotencyKeyReused", err)
	}
	if res.Outcome != idem.OutcomeViolation {
		t.Fatalf("Outcome = %s, want %s. The ZERO RelayedIngestResult claims %s, which is the answer that RE-FORWARDS, so the violation must be carried out on the error path too",
			res.Outcome, idem.OutcomeViolation, idem.OutcomeNew)
	}
	if res.MessageID != "" {
		t.Errorf("MessageID = %q on a refusal, want empty: nothing was applied", res.MessageID)
	}
	msgs, _, _ := h.Store().Since(bob, fixtureEnrolledAt, 0, 10)
	if len(msgs) != 1 {
		t.Fatalf("the serving copy holds %d messages, want 1: a violation applies NOTHING", len(msgs))
	}
}

// ---------------------------------------------------------------------------
// Invariant 2: whose namespace is whose
// ---------------------------------------------------------------------------

// TestIngestRelayedRefusesASenderInThisBusNamespace is invariant 2's inverse
// check, and it is a SECURITY assertion rather than a validation one: an id in
// our namespace admitted by anything other than our roster burns that short name
// for ever (cmd/agent-bus/suffixfloors.go derives each name's suffix floor from
// the sender of every recovered message, and the log is append-only).
func TestIngestRelayedRefusesASenderInThisBusNamespace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		sender func(t *testing.T) string
		why    string
	}{
		{
			name:   "exactly this bus",
			sender: func(t *testing.T) string { return agentID(t, testBusID, "alice") },
			why:    "only this bus's roster may admit an id in this bus's namespace",
		},
		{
			name:   "a case-confusable spelling of this bus",
			sender: func(t *testing.T) string { return agentID(t, strings.ToUpper(testBusID), "alice") },
			why:    "a bus half that folds to ours is not routable anywhere, so admitting it durably records a message from somebody nothing can ever name",
		},
		{
			name:   "an enrolled local agent, spelled exactly",
			sender: func(t *testing.T) string { return agentID(t, testBusID, "bob") },
			why:    "being ON the roster makes it worse, not better: a peer would be speaking as one of our own agents",
		},
		{
			name:   "not fully qualified",
			sender: func(t *testing.T) string { return "alpha-1" },
			why:    "an unqualified id names nobody (invariant 2)",
		},
		{
			name:   "empty",
			sender: func(t *testing.T) string { return "" },
			why:    "an absent sender names nobody",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, lg, _ := newTestHub(t, "bob")
			req := riIngest(t, riOriginMessageID(t, 5), agentID(t, testBusID, "bob"))
			req.Sender = c.sender(t)

			_, err := h.IngestRelayed(context.Background(), req)
			if !errors.Is(err, hub.ErrRelayedSender) {
				t.Fatalf("IngestRelayed(sender=%q) = %v, want ErrRelayedSender: %s", req.Sender, err, c.why)
			}
			if n := len(riReplayMessages(t, lg)); n != 0 {
				t.Fatalf("%d message records reached the durable log for a refused sender, want 0: the refusal must cost nothing durable, or the id-space injury is permanent", n)
			}
		})
	}
}

// TestIngestRelayedRecipientRules covers the OTHER direction of the same
// question, including the one place the relay path deliberately differs from a
// local send: a foreign recipient is recorded WITHOUT asking whether anyone can
// route it, because the message is already this bus's responsibility.
func TestIngestRelayedRecipientRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		recipients func(t *testing.T) []string
		wantErr    error
		why        string
	}{
		{
			name:       "a local agent on the roster",
			recipients: func(t *testing.T) []string { return []string{agentID(t, testBusID, "bob")} },
			why:        "the ordinary destination case",
		},
		{
			name:       "a foreign recipient with no route configured",
			recipients: func(t *testing.T) []string { return []string{agentID(t, "busfar", "carol")} },
			why:        "a leaf bus with no onward peer still records the message durably; whether it travels further is the relay's separate decision (RELAY-16)",
		},
		{
			name: "a local recipient beside a foreign one",
			recipients: func(t *testing.T) []string {
				return []string{agentID(t, testBusID, "bob"), agentID(t, "busfar", "carol")}
			},
			why: "a mixed audience is one message, recorded once",
		},
		{
			name:       "a local id the roster does not hold",
			recipients: func(t *testing.T) []string { return []string{agentID(t, testBusID, "nobody")} },
			wantErr:    hub.ErrUnknownRecipient,
			why:        "the roster is the ONLY authority on our own namespace, and it is asked BEFORE the durable write (finding cca64afd)",
		},
		{
			name:       "a case-confusable spelling of a local agent",
			recipients: func(t *testing.T) []string { return []string{agentID(t, strings.ToUpper(testBusID), "bob")} },
			wantErr:    hub.ErrUnknownRecipient,
			why:        "it claims our namespace with a confusable, and nothing in this system would ever deliver it",
		},
		{
			name:       "a malformed recipient",
			recipients: func(t *testing.T) []string { return []string{"not-an-id"} },
			wantErr:    hub.ErrInvalidRecipient,
			why:        "every recipient is a fully-qualified id (invariant 2)",
		},
		{
			name:       "no recipients at all",
			recipients: func(t *testing.T) []string { return nil },
			wantErr:    hub.ErrInvalidRecipient,
			why:        "a directed message naming nobody would be durable, invisible and undeliverable; a relayed BROADCAST is not representable here at all",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, lg, _ := newTestHub(t, "bob")
			_, err := h.IngestRelayed(context.Background(), riIngest(t, riOriginMessageID(t, 6), c.recipients(t)...))
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("IngestRelayed = %v, want success: %s", err, c.why)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("IngestRelayed = %v, want %v: %s", err, c.wantErr, c.why)
			}
			if n := len(riReplayMessages(t, lg)); n != 0 {
				t.Fatalf("%d message records reached the durable log for a refused recipient, want 0: the roster is asked BEFORE the write precisely so a refusal costs nothing permanent", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The bus path: the only provenance a relayed record carries
// ---------------------------------------------------------------------------

// TestIngestRelayedBusPathRules. Every refusal here is a durable-record problem:
// the path goes into an APPEND-ONLY trail (invariant 6) that nothing can edit
// afterwards, so a lost, forged or fabricated path is permanent.
func TestIngestRelayedBusPathRules(t *testing.T) {
	t.Parallel()
	longPath := make([]string, store.MaxBusPath)
	for i := range longPath {
		longPath[i] = "hop" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	cases := []struct {
		name    string
		path    []string
		want    []string // the recorded path, when accepted
		wantErr error
		why     string
	}{
		{
			name: "one hop: straight from the origin",
			path: []string{riOriginBus},
			want: []string{riOriginBus, testBusID},
			why:  "the minimum honest path: the origin, then us",
		},
		{
			name: "two hops",
			path: []string{riOriginBus, riMiddleBus},
			want: []string{riOriginBus, riMiddleBus, testBusID},
			why:  "the received hops are preserved in order and ours is appended last",
		},
		{
			name:    "empty",
			path:    nil,
			wantErr: hub.ErrInvalidBusPath,
			why:     "a message that arrived from a peer has traversed its origin, so an empty path is a LOST one; defaulting it to this bus would claim the message originated here",
		},
		{
			name:    "already contains this bus",
			path:    []string{riOriginBus, testBusID, riMiddleBus},
			wantErr: hub.ErrBusPathLoop,
			why:     "appending our hop to a path we are already on fabricates a second visit in an append-only trail",
		},
		{
			name:    "contains a case-confusable spelling of this bus",
			path:    []string{riOriginBus, strings.ToUpper(testBusID)},
			wantErr: hub.ErrBusPathLoop,
			why:     "the fold is wider than the exact comparison on purpose: each side errs towards its own safe answer",
		},
		{
			name:    "a malformed hop",
			path:    []string{riOriginBus, "not a bus id"},
			wantErr: hub.ErrInvalidBusPath,
			why:     "every hop is peer-supplied input echoed to every reader of the message",
		},
		{
			name:    "one hop too long once ours is appended",
			path:    longPath,
			wantErr: hub.ErrInvalidBusPath,
			why:     "store.MaxBusPath is the durable bound; refusing here means no number is burned for a record that cannot be built",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, lg, _ := newTestHub(t, "bob")
			bob := agentID(t, testBusID, "bob")
			req := riIngest(t, riOriginMessageID(t, 8), bob)
			req.BusPath = c.path

			_, err := h.IngestRelayed(context.Background(), req)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("IngestRelayed(path=%v) = %v, want %v: %s", c.path, err, c.wantErr, c.why)
				}
				if n := len(riReplayMessages(t, lg)); n != 0 {
					t.Fatalf("%d message records reached the durable log for a refused path, want 0", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("IngestRelayed(path=%v) = %v, want success: %s", c.path, err, c.why)
			}
			replayed := riReplayMessages(t, lg)
			if len(replayed) != 1 {
				t.Fatalf("the durable log holds %d message records, want 1", len(replayed))
			}
			if !riEqualPath(replayed[0].BusPath, c.want) {
				t.Fatalf("the durable record's bus path = %v, want %v: %s", replayed[0].BusPath, c.want, c.why)
			}
		})
	}
}

// TestIngestRelayedCopiesTheCallersPath: the caller still holds the slice it
// handed over — in production it is a peer-decoded one the onward forward is
// about to read — so a later mutation must not reach a message that is already
// durable, and the append must not write through the caller's backing array.
func TestIngestRelayedCopiesTheCallersPath(t *testing.T) {
	t.Parallel()
	h, lg, _ := newTestHub(t, "bob")
	bob := agentID(t, testBusID, "bob")

	// Spare capacity is the dangerous shape: append(received, ours) would write
	// into the caller's array instead of allocating.
	given := make([]string, 2, 8)
	given[0], given[1] = riOriginBus, riMiddleBus
	req := riIngest(t, riOriginMessageID(t, 9), bob)
	req.BusPath = given

	if _, err := h.IngestRelayed(context.Background(), req); err != nil {
		t.Fatalf("IngestRelayed: %v", err)
	}
	if len(given) != 2 || given[0] != riOriginBus || given[1] != riMiddleBus {
		t.Fatalf("the caller's path slice is now %v; the ingest must not write through it — the onward forward reads the path AS RECEIVED", given)
	}
	given[0] = "tampered"

	replayed := riReplayMessages(t, lg)
	if len(replayed) != 1 {
		t.Fatalf("the durable log holds %d message records, want 1", len(replayed))
	}
	want := []string{riOriginBus, riMiddleBus, testBusID}
	if !riEqualPath(replayed[0].BusPath, want) {
		t.Fatalf("the durable record's bus path = %v, want %v: a caller's later mutation must not rewrite the provenance of an accepted message", replayed[0].BusPath, want)
	}
}

// ---------------------------------------------------------------------------
// Refusals that must cost nothing
// ---------------------------------------------------------------------------

// TestIngestRelayedRefusesACancelledRequest. The context is honoured ONLY before
// any durable work: a peer that has gone away is owed nothing, and nothing is
// written. It is deliberately not threaded into the write — see IngestRelayed.
func TestIngestRelayedRefusesACancelledRequest(t *testing.T) {
	t.Parallel()
	h, lg, _ := newTestHub(t, "bob")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.IngestRelayed(ctx, riIngest(t, riOriginMessageID(t, 10), agentID(t, testBusID, "bob")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("IngestRelayed(cancelled ctx) = %v, want context.Canceled", err)
	}
	if n := len(riReplayMessages(t, lg)); n != 0 {
		t.Fatalf("%d message records reached the durable log for a cancelled request, want 0", n)
	}
}

// TestIngestRelayedRefusesAnInvalidBody keeps the body bounds identical to the
// local path: they are publish's, and a relayed message must not be able to
// write something a local send could not.
func TestIngestRelayedRefusesAnInvalidBody(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestHub(t, "bob")
	bob := agentID(t, testBusID, "bob")

	empty := riIngest(t, riOriginMessageID(t, 12), bob)
	empty.Body = nil
	if _, err := h.IngestRelayed(context.Background(), empty); !errors.Is(err, hub.ErrInvalidBody) {
		t.Errorf("IngestRelayed(empty body) = %v, want ErrInvalidBody", err)
	}

	huge := riIngest(t, riOriginMessageID(t, 13), bob)
	huge.Body = make([]byte, store.MaxBodyBytes+1)
	if _, err := h.IngestRelayed(context.Background(), huge); !errors.Is(err, hub.ErrInvalidBody) {
		t.Errorf("IngestRelayed(oversized body) = %v, want ErrInvalidBody", err)
	}
}

// TestIngestRelayedRefusesAMalformedIdempotencyKey. A relayed message's key IS
// the origin's message id — there is no second thing it could be — so it must
// parse as one, and a value that does not is reported under the same sentinel a
// malformed key always was. Sequence 0 is included because 0 is never allocated
// and means "unset", which a shape-only check would let through.
func TestIngestRelayedRefusesAMalformedIdempotencyKey(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestHub(t, "bob")
	bob := agentID(t, testBusID, "bob")
	for _, key := range []string{"", "has spaces", strings.Repeat("k", hub.MaxIdempotencyKeyLen+1), "not-a-message-id", riOriginBus + "-0"} {
		if _, err := h.IngestRelayed(context.Background(), riIngest(t, key, bob)); !errors.Is(err, hub.ErrInvalidIdempotencyKey) {
			t.Errorf("IngestRelayed(key=%q) = %v, want ErrInvalidIdempotencyKey", key, err)
		}
	}
}

// TestIngestRelayedRefusesAnUnrecordableShapeBeforeMintingANumber. The relayed
// path mints its sequence INSIDE the write path, so anything that would fail in
// store.NewMessageWithBusPath — a few lines later — would fail with a number
// already burned, and the number is one a PEER provoked. A gap is not damage
// (invariant 1 prefers a gap to a reissue), but a peer must not be able to open
// one per malformed request for free.
//
// The last assertion is the one that matters: after every refusal below, the
// FIRST sequence this bus ever issues is still 1.
func TestIngestRelayedRefusesAnUnrecordableShapeBeforeMintingANumber(t *testing.T) {
	t.Parallel()
	h, lg, _ := newTestHub(t, "bob")
	bob := agentID(t, testBusID, "bob")

	tooMany := make([]string, 0, store.MaxRecipients+1)
	for i := 0; i <= store.MaxRecipients; i++ {
		tooMany = append(tooMany, agentID(t, "busfar", "carol"))
	}
	cases := []struct {
		name    string
		mutate  func(r *hub.RelayedIngestRequest)
		wantErr error
		why     string
	}{
		{
			name:    "more recipients than a message may carry",
			mutate:  func(r *hub.RelayedIngestRequest) { r.Recipients = tooMany },
			wantErr: hub.ErrInvalidRecipient,
			why:     "store.MaxRecipients is the durable bound",
		},
		{
			name:    "an unset origin timestamp",
			mutate:  func(r *hub.RelayedIngestRequest) { r.TimestampUnixMilli = 0 },
			wantErr: hub.ErrInvalidRelayedMessage,
			why:     "0 means unset, and a record carrying one could never be canonicalized and so never audited",
		},
		{
			name:    "a pre-1970 origin timestamp",
			mutate:  func(r *hub.RelayedIngestRequest) { r.TimestampUnixMilli = -1 },
			wantErr: hub.ErrInvalidRelayedMessage,
			why:     "the canonical format can encode it but refuses to validate it",
		},
		{
			name: "a recipient far longer than any agent id",
			mutate: func(r *hub.RelayedIngestRequest) {
				// The amplification case: this string reaches the duplicate check
				// (%q) and relayedResultFits (JSON) BEFORE publish ever parses a
				// recipient, so without a length bound here a caller chooses a
				// multi-megabyte error string. It must be refused unechoed.
				r.Recipients = []string{strings.Repeat("\x00", 1<<16)}
			},
			wantErr: hub.ErrInvalidRecipient,
			why:     "nothing may assume a recipient is bounded before publish parses it, and this function runs first",
		},
		{
			name:    "a signature of the wrong length",
			mutate:  func(r *hub.RelayedIngestRequest) { r.Signature = []byte{0x01, 0x02} },
			wantErr: hub.ErrInvalidRelayedMessage,
			why:     "every message carries a detached Ed25519 signature of exactly signing.SignatureSize bytes",
		},
		{
			name:    "no signature at all",
			mutate:  func(r *hub.RelayedIngestRequest) { r.Signature = nil },
			wantErr: hub.ErrInvalidRelayedMessage,
			why:     "an unsigned relayed message is not recordable",
		},
	}
	for i, c := range cases {
		req := riIngest(t, riOriginMessageID(t, uint64(200+i)), bob)
		c.mutate(&req)
		if _, err := h.IngestRelayed(context.Background(), req); !errors.Is(err, c.wantErr) {
			t.Errorf("%s: IngestRelayed = %v, want %v: %s", c.name, err, c.wantErr, c.why)
		}
	}
	if n := len(riReplayMessages(t, lg)); n != 0 {
		t.Fatalf("%d message records reached the durable log across %d refusals, want 0", n, len(cases))
	}

	// THE HEADLINE: nothing above consumed a number.
	res, err := h.IngestRelayed(context.Background(), riIngest(t, riOriginMessageID(t, 299), bob))
	if err != nil {
		t.Fatalf("the first well-formed ingest after the refusals = %v, want success", err)
	}
	if res.Seq != 1 {
		t.Fatalf("the first sequence this bus issued is %d, want 1: a refused relay burned %d number(s), which is a durable cost a peer chose", res.Seq, res.Seq-1)
	}
}

// TestIngestRelayedAuditHashIsTakenUnderTheOriginAssignment is the value pin
// audit.go's own doc demands, for the relayed case.
//
// # Why a value pin, and why the two negative assertions are the point
//
// A relayed record's local identity — OUR message id, THEIR sender — cannot be
// canonicalized at all: signing.Canonicalize refuses that pair unconditionally.
// So the hash is taken under the ORIGIN's assignment, which is the same rule
// ("hash the bytes the signature covers") applied to a message signed elsewhere.
//
// The wrong answers are invisible to every other check. store.ContentHash(body)
// is also 64 lowercase hex characters, so wal's validation cannot tell it apart
// and neither can any assertion on shape — that substitution compiles and passes
// every other test in this package. And a digest taken under the LOCAL id would
// be a hash of bytes nobody ever signed. Both are asserted against explicitly.
//
// The expected value is rebuilt INDEPENDENTLY from the message's parts, never by
// calling the hub's own helper, so a helper that changed what it hashes fails
// this test rather than moving it.
func TestIngestRelayedAuditHashIsTakenUnderTheOriginAssignment(t *testing.T) {
	t.Parallel()
	h, lg, dir := newTestHub(t, "bob")
	bob := agentID(t, testBusID, "bob")
	const originSeq = 7
	req := riIngest(t, riOriginMessageID(t, originSeq), bob)

	res, err := h.IngestRelayed(context.Background(), req)
	if err != nil {
		t.Fatalf("IngestRelayed: %v", err)
	}

	// Closed first: the assertion is about what is DURABLE, and a reader racing
	// the writer's buffers would be testing memory.
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the durable log: %v", err)
	}
	auditPath := filepath.Join(dir, wal.AuditFileName)
	recs, _, err := wal.ScanAll(auditPath, wal.KindAudit)
	if err != nil {
		t.Fatalf("scanning the audit log: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("the audit log holds %d records after ONE relayed ingest, want 1", len(recs))
	}
	// Decoded through the SAME reader an fsck would use, so a record only this
	// process can interpret fails here.
	got, _, err := wal.DecodeAudit(auditPath, recs[0])
	if err != nil {
		t.Fatalf("decoding the audit record: %v", err)
	}
	if got.MessageID != res.MessageID {
		t.Fatalf("the audit record names %q, the ingest returned %q", got.MessageID, res.MessageID)
	}

	// THE ORIGIN'S canonical bytes: the origin's id and sequence, with the
	// sender, recipients, sender-clock and body the message actually carries.
	// This is byte-for-byte what relay.RelayedMessage.CanonicalBytes builds and
	// what internal/relay verified the signature against.
	want, err := signing.CanonicalDigest(signing.Message{
		MessageID:          req.OriginMessageID,
		Sequence:           originSeq,
		Sender:             req.Sender,
		Recipients:         req.Recipients,
		TimestampUnixMilli: req.TimestampUnixMilli,
		Body:               req.Body,
	})
	if err != nil {
		t.Fatalf("building the expected canonical digest: %v", err)
	}
	if wantHex := hex.EncodeToString(want[:]); got.ContentSHA256 != wantHex {
		t.Errorf("audit content_sha256 = %q, want the digest over the ORIGIN's canonical bytes %q: the hash must cover the bytes the stored signature covers", got.ContentSHA256, wantHex)
	}
	// The bare-body hash — the substitution audit.go warns about, which compiles
	// and passes everything else.
	if bare := store.ContentHash(req.Body); got.ContentSHA256 == bare {
		t.Errorf("audit content_sha256 is store.ContentHash(body) = %q — the BARE BODY hash, which fingerprints content while proving nothing about who sent it, to whom, or in what order", bare)
	}
	// And the digest under OUR id, which covers bytes nobody signed. It is not
	// merely wrong: signing.Canonicalize REFUSES to produce it, so this asserts
	// the local derivation is impossible rather than just unused.
	if _, err := signing.CanonicalDigest(signing.Message{
		MessageID:          res.MessageID,
		Sequence:           res.Seq,
		Sender:             req.Sender,
		Recipients:         req.Recipients,
		TimestampUnixMilli: req.TimestampUnixMilli,
		Body:               req.Body,
	}); err == nil {
		t.Fatal("signing.Canonicalize accepted a LOCAL message id with a FOREIGN sender; the whole reason the audit hash is taken under the origin's assignment is that this pair has no canonical bytes")
	}
}

// TestIngestRelayedOriginMessageIDMustAgreeWithTheSenderAndThePath covers
// relayedOrigin's two coherence checks. Each disagreement would otherwise
// produce a durable, append-only record that is permanently self-contradictory —
// or, for the bus-half case, one that dies in auditRecordFor with a sequence
// already burned.
func TestIngestRelayedOriginMessageIDMustAgreeWithTheSenderAndThePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(t *testing.T, r *hub.RelayedIngestRequest)
		wantErr error
		why     string
	}{
		{
			name: "the origin id was minted by a different bus than the sender's",
			mutate: func(t *testing.T, r *hub.RelayedIngestRequest) {
				r.OriginMessageID = "busother-4"
			},
			wantErr: hub.ErrRelayedSender,
			why:     "a message is signed by an agent of the bus that minted its id; signing.Canonicalize enforces it EXACTLY and would refuse the content hash",
		},
		{
			name: "the origin id's bus half differs from the sender's only by case",
			mutate: func(t *testing.T, r *hub.RelayedIngestRequest) {
				r.OriginMessageID = strings.ToUpper(riOriginBus) + "-4"
			},
			wantErr: hub.ErrRelayedSender,
			why:     "signing compares these two EXACTLY, so a fold here would mint a sequence and then fail in auditRecordFor with the number already burned",
		},
		{
			name: "the path says the message originated somewhere else",
			mutate: func(t *testing.T, r *hub.RelayedIngestRequest) {
				r.BusPath = []string{"busother", riMiddleBus}
			},
			wantErr: hub.ErrInvalidBusPath,
			why:     "the path is origin-first, so BusPath[0] is the bus that minted the id; two different origins can never be reconciled afterwards",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, lg, _ := newTestHub(t, "bob")
			req := riIngest(t, riOriginMessageID(t, 4), agentID(t, testBusID, "bob"))
			c.mutate(t, &req)

			if _, err := h.IngestRelayed(context.Background(), req); !errors.Is(err, c.wantErr) {
				t.Fatalf("IngestRelayed = %v, want %v: %s", err, c.wantErr, c.why)
			}
			if n := len(riReplayMessages(t, lg)); n != 0 {
				t.Fatalf("%d message records reached the durable log, want 0", n)
			}
			// And no number was burned: the next well-formed ingest gets 1.
			res, err := h.IngestRelayed(context.Background(), riIngest(t, riOriginMessageID(t, 5), agentID(t, testBusID, "bob")))
			if err != nil {
				t.Fatalf("the well-formed ingest after the refusal = %v, want success", err)
			}
			if res.Seq != 1 {
				t.Fatalf("the first sequence this bus issued is %d, want 1: the refusal burned a number", res.Seq)
			}
		})
	}
}

// TestIngestRelayedRefusesDuplicateRecipients. signing.Canonicalize rejects a
// repeated recipient rather than collapsing it ("collapsing would change the
// recipient set the sender signed"), so without this check the refusal arrives
// from auditRecordFor with the sequence already minted.
func TestIngestRelayedRefusesDuplicateRecipients(t *testing.T) {
	t.Parallel()
	h, lg, _ := newTestHub(t, "bob")
	bob := agentID(t, testBusID, "bob")

	if _, err := h.IngestRelayed(context.Background(), riIngest(t, riOriginMessageID(t, 21), bob, bob)); !errors.Is(err, hub.ErrInvalidRecipient) {
		t.Fatalf("IngestRelayed(duplicate recipient) = %v, want ErrInvalidRecipient", err)
	}
	if n := len(riReplayMessages(t, lg)); n != 0 {
		t.Fatalf("%d message records reached the durable log, want 0", n)
	}
	res, err := h.IngestRelayed(context.Background(), riIngest(t, riOriginMessageID(t, 22), bob))
	if err != nil {
		t.Fatalf("the well-formed ingest after the refusal = %v, want success", err)
	}
	if res.Seq != 1 {
		t.Fatalf("the first sequence this bus issued is %d, want 1: the refusal burned a number", res.Seq)
	}
}

// TestIngestRelayedRefusesAnAudienceItCannotRemember is the multi-recipient
// bound, and it is the one refusal here that is NOT the peer's fault.
//
// The applied-key result stores the recipient list and idem.MaxResultBytes caps
// it at 512 bytes, which a relayed message can exceed long before it reaches
// store.MaxRecipients — the local paths never can, because Send carries one
// recipient and Broadcast carries none. Refusing early makes the answer
// immediate and free instead of a 503 that an honest peer retries for ever,
// burning a sequence each time.
//
// THIS TEST DOCUMENTS A WORKAROUND, NOT A FEATURE: the message is legitimate and
// this bus still will not deliver it. Reconciling the two bounds is tracked as
// IDEM-FU-RESULTBYTES-VS-MAXRECIPIENTS, and when that lands this test should
// start failing and be rewritten to assert the message is ACCEPTED.
func TestIngestRelayedRefusesAnAudienceItCannotRemember(t *testing.T) {
	t.Parallel()
	h, lg, _ := newTestHub(t, "bob")

	// Production-length foreign ids, well under store.MaxRecipients.
	many := make([]string, 0, 32)
	for i := 0; i < 32; i++ {
		id, err := ids.AgentID("busfar", "carol", uint64(i+1))
		if err != nil {
			t.Fatalf("ids.AgentID: %v", err)
		}
		many = append(many, id)
	}
	if len(many) > store.MaxRecipients {
		t.Fatalf("the fixture uses %d recipients, above store.MaxRecipients (%d); it must be refused for the RESULT SIZE, not the count", len(many), store.MaxRecipients)
	}

	_, err := h.IngestRelayed(context.Background(), riIngest(t, riOriginMessageID(t, 31), many...))
	if !errors.Is(err, hub.ErrInvalidRelayedMessage) {
		t.Fatalf("IngestRelayed(%d recipients) = %v, want ErrInvalidRelayedMessage: the applied-key result does not fit and no number may be burned finding that out", len(many), err)
	}
	if n := len(riReplayMessages(t, lg)); n != 0 {
		t.Fatalf("%d message records reached the durable log, want 0", n)
	}
	res, err := h.IngestRelayed(context.Background(), riIngest(t, riOriginMessageID(t, 32), agentID(t, testBusID, "bob")))
	if err != nil {
		t.Fatalf("the well-formed ingest after the refusal = %v, want success", err)
	}
	if res.Seq != 1 {
		t.Fatalf("the first sequence this bus issued is %d, want 1: an honest peer retrying an over-large audience would burn one number per attempt", res.Seq)
	}
}

// TestIngestRelayedDoesNotAmplifyAnOversizedRecipientIntoItsError is the
// amplification half of the length bound, and it asserts the SIZE OF THE ERROR
// rather than merely that there was one.
//
// A caller-chosen string reaching a %q verb comes back roughly 4x larger (a
// megabyte of NUL bytes renders as four megabytes of "\x00"), and reaching
// json.Marshal about 6x. That error text goes to a log line. ids.ParseAgentID
// refuses to echo an oversized id for exactly this reason — but it runs inside
// publish, which is AFTER validateRelayedShape, so this is the one place that
// has to refuse it first.
func TestIngestRelayedDoesNotAmplifyAnOversizedRecipientIntoItsError(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestHub(t, "bob")
	const oversized = 1 << 20
	req := riIngest(t, riOriginMessageID(t, 41), strings.Repeat("\x00", oversized))

	_, err := h.IngestRelayed(context.Background(), req)
	if !errors.Is(err, hub.ErrInvalidRecipient) {
		t.Fatalf("IngestRelayed(1 MiB recipient) = %v, want ErrInvalidRecipient", err)
	}
	// Generously bounded: the point is that it is CONSTANT-ish, not that it is
	// any particular length. A refusal that echoed the input would be >= 1 MiB.
	if n := len(err.Error()); n > 1024 {
		t.Fatalf("the refusal for a %d-byte recipient is %d bytes of error text; an oversized value must not be echoed, because the caller then chooses the size of our log line", oversized, n)
	}
}

// riReplayMessages reads the MESSAGE records off the durable log, read-only.
// Every "nothing was written" assertion in this file goes through it: the
// serving copy can look empty for reasons that have nothing to do with the
// write path, and disk is the truth (invariant 5).
func riReplayMessages(t *testing.T, lg *wal.Log) []store.Message {
	t.Helper()
	var out []store.Message
	if _, err := wal.Replay(lg.Path(), func(c wal.Committed) error {
		if c.Entry.Kind != store.RecordKind {
			return nil
		}
		m, err := store.Decode(c.Entry.Body)
		if err != nil {
			return err
		}
		out = append(out, m)
		return nil
	}); err != nil {
		t.Fatalf("replaying the durable log: %v", err)
	}
	return out
}

// riEqualPath compares two bus paths element by element. reflect.DeepEqual would
// also do it, but it treats a nil slice and an empty one as different, which is
// not a distinction any assertion here makes. (buspath_test.go has the same
// helper; it is in the INTERNAL test package and cannot be seen from here, which
// is deliberate — this suite exercises the EXPORTED surface a wiring site has.)
func riEqualPath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
