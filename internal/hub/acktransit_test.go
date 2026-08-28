package hub_test

// ACK-5 — THE TRANSIT ARM OF THE RECIPIENT ACKNOWLEDGEMENT BOUNDARY.
//
// hub.AcknowledgeDelivery gained a SECOND authorization path for a RELAYED
// correlation key. AMENDED 2026-08-23 BY ACK-12-FU-DESTINATION-ROW: a relayed
// ingest NOW writes a DESTINATION lifecycle row per recipient
// (hub.recordAcceptance no longer early-returns for relayed), keyed (origin id,
// recipient) and left `accepted`, and the transit decision is now made UP FRONT
// off the correlation key's bus half BEFORE Settle. transitAck authorises off
// that destination row first, with retained relay provenance as a fallback.
// Without this path a terminal outcome could never ORIGINATE at the far end of a
// multi-hop route.
//
// What this file must prove is therefore TWO-SIDED. The transit arm has to OPEN
// for a named recipient of a relayed copy — and it has to stay SHUT for
// everything else, byte-identically to before, because every non-transit miss is
// still §13.3's uniform answer. And the transit ACK must SETTLE NOTHING locally:
// deciding transit before Settle is what keeps the origin's row the only writer,
// with invariant 4 held end to end by the caller's SYNCHRONOUS forward rather
// than by a local commit. The destination row itself, written at INGEST, and its
// authorisation surviving message-body pruning and a restart, are covered by
// ackdestrow_relay12fu_test.go.
//
// The fixtures come from ack_boundary_test.go (newAckBoundaryHub, deliveredAck,
// ackedState, sendTo, testClock), relayingest_relay24blocker_test.go (riIngest,
// riOriginMessageID, riOriginBus, riMiddleBus) and hub_test.go (agentID,
// testBusID). Nothing is re-invented here.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/hub"
)

// atRefusedAck is the request a recipient makes when it REFUSES a message: a
// terminal NACK carrying one of the three recipient-emitted classes. It is
// deliveredAck's twin and exists so the transit arm can be shown echoing a
// CLASS as well as a state — an intermediate re-classifies nothing (§9.4), and
// a Class that came back empty would be this bus quietly editing the
// recipient's own declaration.
func atRefusedAck(correlationKey, recipient string) hub.RecipientAckRequest {
	return hub.RecipientAckRequest{
		Recipient:      recipient,
		CorrelationKey: correlationKey,
		Outcome:        ack.StateRefused,
		Class:          ack.ClassRecipientRefusedPolicy,
		AttestedBy:     ack.AttestedByRecipientSignatureUnverified,
	}
}

// atRelayedTo ingests one RELAYED message addressed to recipients, through the
// REAL ingest path, and returns its correlation key.
//
// It goes through hub.IngestRelayed rather than hand-building a store.Message
// on purpose: relayedBusPath is what stamps the stored path ORIGIN-FIRST AND
// ENDING AT THIS BUS, and relay.UpstreamHop's whole refusal to search for this
// bus elsewhere on the path depends on that shape. A hand-built fixture could
// assert the transit arm perfectly while proving nothing about the path the
// caller will hand onward.
func atRelayedTo(t *testing.T, h *hub.Hub, seq uint64, recipients ...string) string {
	t.Helper()
	key := riOriginMessageID(t, seq)
	if _, err := h.IngestRelayed(context.Background(), riIngest(t, key, recipients...)); err != nil {
		t.Fatalf("IngestRelayed(%q): %v", key, err)
	}
	return key
}

// TestTransitAcknowledgementBoundary is ACK-5 at the hub boundary.
func TestTransitAcknowledgementBoundary(t *testing.T) {
	t.Run("a named recipient of a RELAYED message gets Transit and no error", testATNamedRecipientTransits)
	t.Run("an agent NOT named in that message gets the uniform refusal", testATUnnamedRecipientRefused)
	t.Run("a LOCALLY-ORIGINATED message never transits", testATLocalMessageNeverTransits)
	t.Run("the LOCAL id of a relayed message is the uniform refusal", testATLocalIDOfARelayedMessageIsRefused)
	t.Run("an unknown or malformed key is the uniform refusal", testATUnknownKeyRefused)
	t.Run("the transit path writes NOTHING durable", testATTransitWritesNothing)
	// THE BROADCAST CASE IS NOT A SUBTEST HERE, AND THAT IS RECORDED RATHER
	// THAN QUIETLY OMITTED. transitAck's loop over an EMPTY recipient list
	// refuses a broadcast, which is the right answer — under signing format v1
	// a broadcast has no canonical audience, so there is no (message,
	// recipient) pair for any party to acknowledge. Neither half is reachable
	// from this package today: hub.Broadcast fails CLOSED under signing format
	// v1 (skipIfBroadcastHasNoSigningDigest, hub_test.go) so no broadcast can
	// be published to look up, and a relayed broadcast is not representable at
	// all (RelayedIngestRequest carries an explicit recipient list and no flag,
	// and its doc says so). A subtest here could only have SKIPPED, and a
	// skipping leaf asserts nothing while reading green. The membership miss it
	// would share a code path with IS covered, by
	// testATUnnamedRecipientRefused.
}

// testATNamedRecipientTransits is the arm ACK-5 exists to open.
func testATNamedRecipientTransits(t *testing.T) {
	h, acks, _ := newAckBoundaryHub(t, ackBoundaryOptions{}, "bob")
	bob := agentID(t, testBusID, "bob")

	for _, tc := range []struct {
		name  string
		seq   uint64
		req   func(key, recipient string) hub.RecipientAckRequest
		state ack.State
		class ack.Class
	}{
		{"a positive terminal", 101, deliveredAck, ack.StateDelivered, ""},
		{"a negative terminal with its class", 102, atRefusedAck, ack.StateRefused, ack.ClassRecipientRefusedPolicy},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			key := atRelayedTo(t, h, tc.seq, bob)

			// THE PRECONDITION, ASSERTED RATHER THAN ASSUMED. Since
			// ACK-12-FU-DESTINATION-ROW a relayed ingest writes a DESTINATION
			// lifecycle row per recipient, keyed (origin id, recipient) and left
			// `accepted`. The transit arm authorises off THAT row and must still
			// FORWARD the outcome rather than settle it here — so the row must
			// exist and be accepted before the ack, and (asserted after the ack)
			// must be UNCHANGED by it.
			r, ok := ackedState(t, acks, key, bob)
			if !ok {
				t.Fatalf("a relayed ingest wrote NO destination lifecycle row for %s; the transit arm has nothing to authorise off and this subtest proves nothing", bob)
			}
			if r.State != ack.StateAccepted {
				t.Fatalf("the destination row is %s, want accepted; a relayed ingest must leave it non-terminal", r.State)
			}

			res, err := h.AcknowledgeDelivery(tc.req(key, bob))
			if err != nil {
				t.Fatalf("AcknowledgeDelivery on a relayed message = %v, want nil. Without this arm a recipient at the far end of a multi-hop route is told the uniform `unknown` and plane C is unreachable beyond one hop", err)
			}
			if !res.Transit {
				t.Fatalf("Transit = false for a named recipient of a RELAYED message; the caller would answer 200 having carried the outcome NOWHERE, and no test on this bus would notice")
			}
			// State and Class are the recipient's OWN declaration echoed back,
			// not anything this bus looked up.
			if res.State != tc.state {
				t.Errorf("State = %s, want %s", res.State, tc.state)
			}
			if res.Class != tc.class {
				t.Errorf("Class = %q, want %q — an intermediate re-classifies nothing (§9.4)", res.Class, tc.class)
			}
			// Duplicate is ALWAYS false on this path, and that is honest rather
			// than a bug: this bus keeps no record for a relayed message, so
			// there is nothing here for a retry to be a duplicate OF. The
			// duplicate is absorbed WHERE THE RECORD IS, at the origin.
			if res.Duplicate {
				t.Error("Duplicate = true on the transit path; labelling it here would mean this bus asserting something about a table it does not hold")
			}

			// AND THE DESTINATION ROW WAS NOT SETTLED LOCALLY. A relayed key is
			// forwarded, never settled here — deciding transit BEFORE Settle is
			// the whole point of the ACK-12-FU-DESTINATION-ROW reorder. Settling
			// the destination row locally would strand the origin's row
			// non-terminal and break back-propagation, so the row must still read
			// `accepted` after a transit ack.
			if after, ok := ackedState(t, acks, key, bob); !ok || after.State != ack.StateAccepted {
				t.Fatalf("after a transit ack the destination row is (%+v, %v), want STILL accepted; a relayed ack must be forwarded, not settled locally", after, ok)
			}

			// AND A RETRY IS THE SAME ANSWER, not an error and not a conflict.
			// Invariant 10's first case is handled at the origin, and this bus
			// must not manufacture a second opinion about it.
			again, err := h.AcknowledgeDelivery(tc.req(key, bob))
			if err != nil {
				t.Fatalf("the recipient's identical retry errored: %v", err)
			}
			if !again.Transit || again.Duplicate {
				t.Errorf("the retry returned %+v, want Transit:true Duplicate:false", again)
			}
		})
	}

	// THE STORED PATH IS THE SHAPE THE CALLER'S NEXT STEP REQUIRES. relay's
	// UpstreamHop takes index len-2 and REFUSES to search for this bus
	// elsewhere, precisely so a peer-fabricated path cannot choose who this bus
	// contacts — so a stored path that did not end here would make every
	// forward refuse, and a forward that "worked" on a WIRE path would be the
	// steering hole itself.
	t.Run("the stored bus path is origin-first and ends at THIS bus", func(t *testing.T) {
		key := atRelayedTo(t, h, 103, bob)
		prov, ok := h.Store().RelayProvenanceByOriginMessageID(key)
		if !ok {
			t.Fatalf("no provenance for %q", key)
		}
		want := []string{riOriginBus, riMiddleBus, testBusID}
		if strings.Join(prov.BusPath, ",") != strings.Join(want, ",") {
			t.Errorf("stored bus path = %v, want %v", prov.BusPath, want)
		}
		if !prov.Relayed {
			t.Error("Relayed = false for a message that arrived over two hops")
		}
		// AND IT CARRIES NO BODY. Invariant 6's line drawn at the seam: this
		// accessor sits on a path an authenticated agent drives once per
		// POST /v1/ack, and a routing question gets a routing-only answer.
		if len(prov.Recipients) != 1 || prov.Recipients[0] != bob {
			t.Errorf("Recipients = %v, want [%s]", prov.Recipients, bob)
		}
	})
}

// testATUnnamedRecipientRefused is the arm that must stay SHUT.
//
// The membership test IS the entire authorization: req.Recipient is the
// AUTHENTICATED PRINCIPAL and there is no request field another agent's id
// could arrive in, so an agent can only ever ask whether IT is a recipient.
func testATUnnamedRecipientRefused(t *testing.T) {
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "bob", "carol")
	bob, carol := agentID(t, testBusID, "bob"), agentID(t, testBusID, "carol")
	key := atRelayedTo(t, h, 111, bob)

	before := len(ackEntriesIn(t, dir))

	res, err := h.AcknowledgeDelivery(deliveredAck(key, carol))
	if !errors.Is(err, hub.ErrAckNotRetained) {
		t.Fatalf("an agent NOT named in the relayed message got %v, want ErrAckNotRetained — the UNIFORM answer, unchanged. A distinguishable refusal would tell any authenticated agent which messages this bus is holding for other agents", err)
	}
	if res.Transit {
		t.Error("Transit = true alongside a refusal; the caller would forward an outcome nobody authorized")
	}
	// A NON-MEMBER AND A MISS ARE INDISTINGUISHABLE: the same sentinel, and
	// nothing written either way.
	if r, ok := ackedState(t, acks, key, carol); ok {
		t.Fatalf("the refusal CREATED a row %+v", r)
	}
	if got := len(ackEntriesIn(t, dir)); got != before {
		t.Errorf("the refusal wrote %d new lifecycle records, want 0", got-before)
	}

	// The refusal for a foreign agent is byte-identical to the refusal for an
	// agent on ANOTHER bus entirely, which is what closes the oracle.
	_, other := agentID(t, testBusID, "bob"), agentID(t, riOriginBus, "alpha")
	if _, err := h.AcknowledgeDelivery(deliveredAck(key, other)); !errors.Is(err, hub.ErrAckNotRetained) {
		t.Errorf("an agent on the ORIGIN bus got %v, want ErrAckNotRetained", err)
	}
	// And the legitimate recipient still succeeds, so the refusals above are
	// not passing because the fixture reaches nothing.
	if res, err := h.AcknowledgeDelivery(deliveredAck(key, bob)); err != nil || !res.Transit {
		t.Fatalf("the NAMED recipient got (%+v, %v), want Transit:true and no error; without this the refusals above prove nothing", res, err)
	}
}

// testATLocalMessageNeverTransits is the `prov.Relayed` condition, and it is not
// a formality.
//
// A locally-originated message ALWAYS had a row written for it, so reaching
// ack.ErrNoRecord for one means the row was SWEPT — and that must keep the
// uniform refusal. Letting it transit would make this bus forward a terminal
// outcome for a message it is the ORIGIN of, to a bus that never owed it one:
// an expired row silently becoming an unsolicited network contact.
func testATLocalMessageNeverTransits(t *testing.T) {
	clock := newTestClock()
	// A SHORT lifecycle retention against the DEFAULT message retention, so the
	// row expires while the MESSAGE is still comfortably held. That gap is the
	// whole fixture: with the message gone the store lookup would miss and the
	// subtest would pass for the wrong reason.
	h, acks, _ := newAckBoundaryHub(t, ackBoundaryOptions{clock: clock, ackRetention: time.Hour}, "alpha", "beta")
	alpha, beta := agentID(t, testBusID, "alpha"), agentID(t, testBusID, "beta")
	key := sendTo(t, h, alpha, beta, "k-local-never-transits")

	clock.Advance(time.Hour + time.Minute)
	if _, ok := acks.Lookup(key, beta); ok {
		t.Fatal("the lifecycle row survived its retention, so this subtest never reaches the ErrNoRecord arm it exists to test; FIX THE FIXTURE, not the assertion")
	}

	// THE PRECONDITIONS THAT MAKE THIS THE `Relayed` TEST AND NOTHING ELSE:
	// the message is still held, and beta IS a named recipient of it. So the
	// ONLY thing standing between here and a transit is prov.Relayed.
	prov, ok := h.Store().RelayProvenanceByOriginMessageID(key)
	if !ok {
		t.Fatal("the MESSAGE has also been pruned; the store lookup would miss and this subtest would pass without exercising the Relayed condition at all")
	}
	if prov.Relayed {
		t.Fatalf("a locally-originated message reports Relayed=true; RelayProvenance's discriminator is broken and the transit arm would open for this bus's OWN messages")
	}
	if len(prov.Recipients) != 1 || prov.Recipients[0] != beta {
		t.Fatalf("Recipients = %v, want [%s]; the membership test would refuse this for the wrong reason", prov.Recipients, beta)
	}

	res, err := h.AcknowledgeDelivery(deliveredAck(key, beta))
	if !errors.Is(err, hub.ErrAckNotRetained) {
		t.Fatalf("acknowledging a SWEPT row for a LOCALLY-ORIGINATED message returned %v, want ErrAckNotRetained. This bus IS the origin of that message: forwarding would send a terminal outcome to a bus that never owed us one, turning an expired row into an unsolicited network contact", err)
	}
	if res.Transit {
		t.Fatal("Transit = true for a message this bus ORIGINATED. There is nowhere for an origin's own outcome to go, and §8.4's stop rule cannot see this shape because the correlation key never changes")
	}
}

// testATUnknownKeyRefused keeps the other three causes of a miss on the uniform
// answer. A malformed key is a MISS, not a fourth distinguishable case: a caller
// that could tell "malformed" from "unknown" would learn which of its guesses
// were even well-formed.
func testATUnknownKeyRefused(t *testing.T) {
	h, _, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "bob")
	bob := agentID(t, testBusID, "bob")
	// One real relayed message, so the store is not simply empty.
	atRelayedTo(t, h, 121, bob)
	before := len(ackEntriesIn(t, dir))

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"a well-formed key this bus never saw", riOriginMessageID(t, 999)},
		{"a key in THIS bus's namespace that was never minted", testBusID + "-4242"},
		{"an empty key", ""},
		{"no separator", "nodashhere"},
		{"a non-numeric sequence", riOriginBus + "-notanumber"},
		{"a leading-zero sequence", riOriginBus + "-007"},
		{"an oversized key", strings.Repeat("z", 4096)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			res, err := h.AcknowledgeDelivery(deliveredAck(tc.key, bob))
			if !errors.Is(err, hub.ErrAckNotRetained) {
				t.Fatalf("err = %v, want ErrAckNotRetained", err)
			}
			if res.Transit {
				t.Error("Transit = true alongside a refusal")
			}
		})
	}
	if got := len(ackEntriesIn(t, dir)); got != before {
		t.Errorf("%d lifecycle records were written by refusals, want 0; a boundary that creates rows lets any agent mint them for keys it invented", got-before)
	}
}

// testATTransitWritesNothing is the durability half: the transit ACKNOWLEDGEMENT
// writes nothing and settles nothing. AMENDED for ACK-12-FU-DESTINATION-ROW —
// the destination row is now written at INGEST, so "nothing durable happens on
// this bus" no longer holds for the ingest; what still holds, and is the whole
// point, is that the ACK PATH neither appends a record nor settles the row.
//
// Invariant 4 is satisfied END TO END by the caller's synchronous forward — the
// recipient is not told "accepted" until the ORIGIN has fsynced — which is only
// correct if the transit ack itself writes nothing here. Settling the
// destination row locally would put a terminal outcome on this bus for a message
// the origin owns, stranding the origin's row non-terminal.
func testATTransitWritesNothing(t *testing.T) {
	h, acks, dir := newAckBoundaryHub(t, ackBoundaryOptions{}, "bob")
	bob := agentID(t, testBusID, "bob")
	key := atRelayedTo(t, h, 131, bob)

	// Captured AFTER the ingest, so these baselines already include the one
	// destination row the relayed ingest wrote. The assertions below are about
	// what the three ACKS add, which must be nothing.
	rowsBefore := acks.Len()
	walBefore := len(ackEntriesIn(t, dir))

	for i := 0; i < 3; i++ {
		res, err := h.AcknowledgeDelivery(deliveredAck(key, bob))
		if err != nil || !res.Transit {
			t.Fatalf("acknowledgement %d returned (%+v, %v), want Transit:true and no error", i, res, err)
		}
	}

	if got := acks.Len(); got != rowsBefore {
		t.Errorf("the lifecycle table went from %d rows to %d across three transit acknowledgements, want unchanged. The transit ack settles nothing and opens no new row",
			rowsBefore, got)
	}
	// The destination row is STILL there and STILL accepted: a transit ack
	// settles nothing locally (the transit case is decided before Settle).
	if r, ok := ackedState(t, acks, key, bob); !ok || r.State != ack.StateAccepted {
		t.Errorf("after three transit acks the destination row is (%+v, %v), want STILL accepted and unsettled", r, ok)
	}
	if got := len(ackEntriesIn(t, dir)); got != walBefore {
		t.Errorf("three transit acknowledgements appended %d lifecycle records to the durable log, want 0", got-walBefore)
	}
}

// testATLocalIDOfARelayedMessageIsRefused pins the THIRD condition — the
// correlation key must name ANOTHER bus (ACK-CONTRACT.md §3).
//
// # THIS IS THE CASE THAT LOOKS IMPOSSIBLE AND IS THE MOST REACHABLE OF ALL
//
// Every bus mints its own id for a message it accepts (invariant 1), so ONE
// logical message has two ids: the ORIGIN bus's — which IS the correlation key —
// and the local one this bus minted and served to the recipient. A recipient
// holds the LOCAL one: `agent-busctl watch` prints `message_id` and does not
// expose the origin id at all. So acknowledging with the local id is not a
// perverse input, it is the FIRST thing an agent will try.
//
// It must be the uniform refusal, and getting there is not automatic.
// store.ByOriginMessageID falls back to resolving an unmatched key as a LOCAL
// id — correct and documented behaviour of that method — so the local id
// RESOLVES, to the very same relayed message. Relayed is true and the recipient
// is a member, so the two obvious conditions both PASS. Without the bus-half
// check the answer would be "transit", the caller would ask where to send it,
// relay.DisposeAck would answer AckStopAtOrigin because the bus half is ours,
// and the route would return a RETRIABLE 503 to an acknowledgement that can
// never succeed at any point in the future.
//
// MUTATION-PROVED: deleting the bus-half check in transitAck turns this subtest
// RED, and turns nothing else in this package red.
func testATLocalIDOfARelayedMessageIsRefused(t *testing.T) {
	h, acks, _ := newAckBoundaryHub(t, ackBoundaryOptions{}, "bob")
	bob := agentID(t, testBusID, "bob")

	originKey := riOriginMessageID(t, 771)
	res, err := h.IngestRelayed(context.Background(), riIngest(t, originKey, bob))
	if err != nil {
		t.Fatalf("IngestRelayed(%q): %v", originKey, err)
	}
	localID := res.MessageID

	// THE PRECONDITIONS, ASSERTED RATHER THAN ASSUMED — without both of them
	// this subtest could pass for the wrong reason.
	if localID == originKey {
		t.Fatalf("the local id and the origin id are both %q; this bus did not mint its own (invariant 1) and the case under test does not exist here", localID)
	}
	// The DESTINATION row exists under the ORIGIN key (ACK-12-FU-DESTINATION-ROW),
	// which is exactly why acking under the LOCAL id must still be refused: the
	// local id names THIS bus and must never resolve to a transit. Asserted so a
	// broken ingest that wrote no row cannot make the refusal below pass
	// vacuously.
	if r, ok := ackedState(t, acks, originKey, bob); !ok || r.State != ack.StateAccepted {
		t.Fatalf("the relayed ingest wrote no accepted destination row under the origin key %q (got %+v, %v); the control for this subtest is broken", originKey, r, ok)
	}
	// The ORIGIN key MUST work, or "the local id is refused" would be proved by
	// a bus on which nothing works at all.
	if ok, err := h.AcknowledgeDelivery(deliveredAck(originKey, bob)); err != nil || !ok.Transit {
		t.Fatalf("the ORIGIN key was refused (%+v, %v); the control for this subtest is broken, so a refusal of the local id proves nothing", ok, err)
	}

	got, err := h.AcknowledgeDelivery(deliveredAck(localID, bob))
	if !errors.Is(err, hub.ErrAckNotRetained) {
		t.Fatalf("acknowledging a relayed message with the id THIS bus minted (%q) returned (%+v, %v), want ErrAckNotRetained — the uniform refusal (§13.3). Anything else is either a 503 no client can ever clear, or this bus forwarding a terminal outcome for a key it is itself the origin of", localID, got, err)
	}
	if got.Transit {
		t.Fatal("Transit = true for this bus's OWN message id; the caller would ask where to send an outcome whose correlation key names THIS bus")
	}
}
