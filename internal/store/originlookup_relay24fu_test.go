package store_test

// RELAY-24-FU-STOREMSGLOOKUP — the point-lookup surface and the CORRELATION KEY.
//
// # What is under test
//
// A third number-like notion joined Message: OriginMessageID, the id the ORIGIN
// bus minted. It is NOT a variant of the other two and the whole file is written
// against that sentence:
//
//	Seq                IDENTITY — server-minted, client-signed, spendable out of
//	                   order (SIGN-1).
//	Pos                DELIVERY POSITION — the WAL commit index; what cursors
//	                   point at and what the serving copy is ordered by
//	                   (SIGN-1-FU-REORDER-WATERMARK).
//	OriginMessageID    CORRELATION KEY — which message on the ORIGIN bus this is
//	                   a local copy of. It takes part in NO ordering, NO cursor
//	                   and NO retention decision.
//
// Around it: Message.OriginID, Message.WithOriginMessageID, Record's durable
// `origin_message_id`, Store.ByID, Store.ByOriginMessageID and
// Store.DuplicateOriginMessageIDs.
//
// # Invariants read IN FULL before writing this
//
//	Invariant 1  (ids never reused, including across restarts). The two indexes
//	             MINT nothing and REWIND nothing; they only mirror the retained
//	             window. The property that costs work to pin is the negative one:
//	             a PRUNED id must never be re-resolvable, and it must not even
//	             leave a stale index entry behind — see
//	             TestStoreLookupIsNotResolvableAfterPruning, which asserts on the
//	             LOG for exactly that reason. This bus also never ADOPTS a peer's
//	             id: OriginMessageID is a correlation key and never an identity,
//	             which is what the same-bus refusals in D and E defend.
//	Invariant 4  (nothing acknowledged before durable, NARROWED 2026-08-02).
//	             By the time Append runs the record is committed and fsynced, so
//	             a refusal orphans it on disk and poisons the hub. That is why a
//	             DUPLICATE ORIGIN ID returns nil and retains — see
//	             TestStoreDuplicateOriginMessageIDs.
//	Invariant 5  (memory serves, disk is truth). The origin id has exactly one
//	             consumer — relay.Forwarder.Resume, which runs ONLY after a
//	             restart — so an in-memory-only field would be empty at the only
//	             moment it is read. TestMessageOriginMessageIDRoundTrips is the
//	             unit half of that proof; the crash-injection half is in
//	             originlookup_crash_relay24fu_test.go.
//	Invariant 6  (loud discards; metadata and routing only). Every fault path
//	             here LOGS and COUNTS; silence is the defect, not the discard.
//	Invariant 10 (idempotency). WithOriginMessageID with the SAME value already
//	             set is a legitimate retry and returns nil; a DIFFERENT value is
//	             a protocol violation and is refused. Neither disconnects
//	             anything, and neither may be collapsed into the other.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
)

// originPeerBusID is the bus a relay ingest in this file arrived FROM. It must
// never equal testBusID: the whole soundness argument for
// Store.ByOriginMessageID's fallback is that no message can be in byOrigin under
// a key of THIS bus's shape.
const originPeerBusID = "peerbus"

// originOtherBusID is a THIRD bus, for the "already set to a different origin"
// refusal — which must be reached by a value that is otherwise legal, not by one
// the same-bus check would have caught first.
const originOtherBusID = "otherbus"

// originAgentIDOn mints a fully-qualified agent id on an arbitrary bus
// (invariant 2). agentIDFor only ever speaks testBusID, and a relay-ingested
// message's sender lives on the PEER.
func originAgentIDOn(t *testing.T, busID, name string) string {
	t.Helper()
	id, err := ids.AgentID(busID, name, 1)
	if err != nil {
		t.Fatalf("ids.AgentID(%q, %q, 1): %v", busID, name, err)
	}
	return id
}

// mkRelayMessageAt builds the message a RELAY INGEST produces: a two-hop bus
// path [origin, this bus], and the origin's id recorded as the correlation key.
//
// It goes through NewMessageWithBusPath and WithOriginMessageID rather than
// assembling a Message literal, so a fixture can never carry a combination the
// production write path would have refused.
func mkRelayMessageAt(t *testing.T, sender string, recipients []string, seq, pos uint64, sentAt time.Time, body, originMessageID string) store.Message {
	t.Helper()
	originBus, _, err := ids.ParseMessageID(originMessageID)
	if err != nil {
		t.Fatalf("mkRelayMessageAt: origin message id %q does not parse: %v", originMessageID, err)
	}
	m, err := store.NewMessageWithBusPath(
		testBusID, sender, len(recipients) == 0, recipients, seq, sentAt, []byte(body),
		fmt.Sprintf("relay-key-%d", seq), testTimestampMs, testSignature(t),
		[]string{originBus, testBusID},
	)
	if err != nil {
		t.Fatalf("store.NewMessageWithBusPath(seq=%d): %v", seq, err)
	}
	m, err = m.WithOriginMessageID(originMessageID)
	if err != nil {
		t.Fatalf("WithOriginMessageID(%q) on %s: %v", originMessageID, m.ID, err)
	}
	m.Pos = pos
	return m
}

// mkLocalMessageAt is a locally-originated message: one hop, this bus, and NO
// origin message id — because this bus IS the origin and Message.OriginID reads
// the empty value as exactly that.
func mkLocalMessageAt(t *testing.T, sender string, recipients []string, seq, pos uint64, sentAt time.Time, body string) store.Message {
	t.Helper()
	return mkMessageAt(t, sender, len(recipients) == 0, recipients, seq, pos, sentAt, body)
}

// newOriginLookupStore is a store on the injected clock whose log is CAPTURED.
// Capturing is not tidiness: two assertions below are about a line that must NOT
// be emitted, which is the only way to observe a stale index entry that resolves
// to nothing (see TestStoreLookupIsNotResolvableAfterPruning).
func newOriginLookupStore(clock *testClock, opts store.Options) (*store.Store, *bytes.Buffer) {
	var buf bytes.Buffer
	opts.Now = clock.now
	opts.Logger = logging.New(&buf, logging.LevelWarn)
	return store.New(opts), &buf
}

// ---------------------------------------------------------------------------
// A. Store.ByID — the local point lookup.
// ---------------------------------------------------------------------------

// TestStoreLookupByMessageID pins Store.ByID: the exact message or nothing, a
// deep copy, and never a neighbour — including inside a run of messages that
// share one delivery position.
func TestStoreLookupByMessageID(t *testing.T) {
	// A1. What went in is what comes out, INCLUDING the correlation key. A
	// lookup that resolved the message but dropped the origin id would leave
	// relay.Forwarder.Resume with a message it cannot correlate, which is the
	// only reason the field exists.
	t.Run("ResolvesTheExactLocalIDIncludingTheOriginMessageID", func(t *testing.T) {
		clock := newClock()
		s, logBuf := newOriginLookupStore(clock, store.Options{})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")
		peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")

		local := mkLocalMessageAt(t, alpha, []string{beta}, 1, 1, clock.now(), "a local send")
		relayed := mkRelayMessageAt(t, peerAlpha, []string{beta}, 2, 2, clock.now(), "a relay ingest", originPeerBusID+"-11")
		mustAppend(t, s, local)
		mustAppend(t, s, relayed)

		for _, want := range []store.Message{local, relayed} {
			got, ok := s.ByID(want.ID)
			if !ok {
				t.Fatalf("ByID(%q) reported NOT FOUND for a message just appended", want.ID)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ByID(%q) returned a different message than was appended\n got: %+v\nwant: %+v", want.ID, got, want)
			}
		}

		// Stated separately from the DeepEqual because it is the property the
		// consumer actually reads, and because OriginID() is the ONE place the
		// "empty means this bus is the origin" rule is written down.
		gotLocal, _ := s.ByID(local.ID)
		if gotLocal.OriginMessageID != "" {
			t.Fatalf("a LOCALLY-ORIGINATED message came back carrying origin message id %q; the field is set only on a relay ingest and the empty value is what means \"this bus is the origin\"", gotLocal.OriginMessageID)
		}
		if gotLocal.OriginID() != local.ID {
			t.Fatalf("OriginID() = %q for a locally-originated message, want its own id %q", gotLocal.OriginID(), local.ID)
		}
		gotRelayed, _ := s.ByID(relayed.ID)
		if gotRelayed.OriginMessageID != originPeerBusID+"-11" {
			t.Fatalf("ByID lost the correlation key: OriginMessageID = %q, want %q", gotRelayed.OriginMessageID, originPeerBusID+"-11")
		}
		if gotRelayed.OriginID() != originPeerBusID+"-11" {
			t.Fatalf("OriginID() = %q for a relay ingest, want the ORIGIN's id %q — never this bus's own %q", gotRelayed.OriginID(), originPeerBusID+"-11", relayed.ID)
		}
		// The relay ingest is a copy of a peer's message, never an adoption of a
		// peer's id (invariant 1).
		if gotRelayed.ID == gotRelayed.OriginMessageID {
			t.Fatalf("the local id and the origin id are the same string (%q); this bus mints its own id and never adopts a peer's", gotRelayed.ID)
		}

		if logBuf.Len() != 0 {
			t.Fatalf("two ordinary appends and four clean lookups logged something; the fault paths in this package are for server bugs only:\n%s", logBuf.String())
		}
	})

	// A2. NOT FOUND, and specifically for an id of the RIGHT SHAPE. Gibberish
	// alone would pass against an implementation that resolved anything parseable.
	t.Run("AnUnknownIDResolvesToNotFound", func(t *testing.T) {
		clock := newClock()
		s, logBuf := newOriginLookupStore(clock, store.Options{})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")
		mustAppend(t, s, mkLocalMessageAt(t, alpha, []string{beta}, 1, 1, clock.now(), "the only message"))

		cases := []struct {
			name string
			id   string
		}{
			// The load-bearing one: well-formed, on THIS bus, in the sequence
			// neighbourhood of a message that IS retained, and never appended.
			{"WellFormedOnThisBusButNeverAppended", testBusID + "-2"},
			{"WellFormedButFarPastTheHead", testBusID + "-999999"},
			{"WellFormedOnAnotherBus", originPeerBusID + "-1"},
			{"Empty", ""},
			{"Gibberish", "not a message id"},
			{"AnAgentIDRatherThanAMessageID", alpha},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if got, ok := s.ByID(c.id); ok {
					t.Fatalf("ByID(%q) resolved to %s; nothing with that id was ever appended", c.id, got.ID)
				}
			})
		}
		if logBuf.Len() != 0 {
			t.Fatalf("a miss in the id index must be a silent NOT FOUND — the ERROR line is reserved for the index and the serving copy DISAGREEING:\n%s", logBuf.String())
		}
	})

	// A3. NEVER A NEIGHBOUR. The caller is a relay forward, so returning the
	// nearest message would send one agent's traffic to another bus under another
	// message's correlation id. The fixture deliberately includes a
	// SHARED-POSITION run, because that is the only shape in which "the message at
	// this position" and "the message with this id" can differ.
	t.Run("NeverReturnsANeighbour", func(t *testing.T) {
		clock := newClock()
		s, _ := newOriginLookupStore(clock, store.Options{})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")
		peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")

		appended := []store.Message{
			mkLocalMessageAt(t, alpha, []string{beta}, 1, 1, clock.now(), "one"),
			mkLocalMessageAt(t, alpha, []string{beta}, 2, 2, clock.now(), "two"),
			mkRelayMessageAt(t, peerAlpha, []string{beta}, 3, 3, clock.now(), "three", originPeerBusID+"-31"),
			// Position 3 AGAIN — the non-monotone branch of Append retains it, so
			// position 3 now locates a RUN of three messages rather than one.
			mkLocalMessageAt(t, alpha, []string{beta}, 4, 3, clock.now(), "four"),
			mkRelayMessageAt(t, peerAlpha, []string{beta}, 5, 3, clock.now(), "five", originPeerBusID+"-51"),
			mkLocalMessageAt(t, alpha, []string{beta}, 6, 4, clock.now(), "six"),
		}
		for _, m := range appended {
			mustAppend(t, s, m)
		}

		for _, want := range appended {
			got, ok := s.ByID(want.ID)
			if !ok {
				t.Fatalf("ByID(%q) reported NOT FOUND; every one of these messages is retained", want.ID)
			}
			if got.ID != want.ID {
				t.Fatalf("ByID(%q) returned message %q — a NEIGHBOUR, not the message asked for. The caller is a relay forward: this would send one agent's message to another bus under another message's correlation id", want.ID, got.ID)
			}
			if got.Seq != want.Seq || !bytes.Equal(got.Body, want.Body) {
				t.Fatalf("ByID(%q) returned id %q but seq %d / body %q, want seq %d / body %q: the id index and the serving copy disagree",
					want.ID, got.ID, got.Seq, got.Body, want.Seq, want.Body)
			}
		}
	})

	// A4. DEEP COPY. copyMessage exists because NewMessage copies carefully on the
	// way IN, and a caller reaching into the stored slices on the way OUT would
	// defeat that entirely — silently, right up to the moment something mutated a
	// body the store still believes it holds.
	t.Run("TheReturnedMessageIsADeepCopy", func(t *testing.T) {
		clock := newClock()
		s, _ := newOriginLookupStore(clock, store.Options{})
		beta := agentIDFor(t, "beta")
		peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")

		// A relay ingest: it is the only shape carrying a recipient list AND a
		// multi-hop bus path, so all three copied slices are non-empty.
		appended := mkRelayMessageAt(t, peerAlpha, []string{beta}, 1, 1, clock.now(), "original body", originPeerBusID+"-11")
		mustAppend(t, s, appended)

		// THE EXPECTED VALUES ARE SNAPSHOTTED, and this line is load-bearing.
		//
		// The FIRST version of this test compared the second lookup against the
		// fixture variable, and it was UNFAILABLE: Append stores the Message
		// value, whose slice headers point at the arrays NewMessageWithBusPath
		// allocated — the SAME arrays the fixture variable holds. Aliasing the
		// serving copy therefore made the test mutate its own expectation, and
		// the comparison was the mutated array against itself. Caught by mutating
		// byIDLocked to drop copyMessage and watching the test stay green.
		wantBody := append([]byte(nil), appended.Body...)
		wantRecipients := append([]string(nil), appended.Recipients...)
		wantBusPath := append([]string(nil), appended.BusPath...)
		wantSignature := append([]byte(nil), appended.Signature...)

		got, ok := s.ByID(appended.ID)
		if !ok {
			t.Fatalf("ByID(%q) reported NOT FOUND for a message just appended", appended.ID)
		}
		if len(got.Body) == 0 || len(got.Recipients) == 0 || len(got.BusPath) == 0 || len(got.Signature) == 0 {
			t.Fatalf("the fixture is not exercising all four copied slices: body=%d recipients=%d bus_path=%d signature=%d", len(got.Body), len(got.Recipients), len(got.BusPath), len(got.Signature))
		}
		got.Body[0] = 'X'
		got.Recipients[0] = "testbus.attacker-1"
		got.BusPath[0] = "attackerbus"
		got.Signature[0] ^= 0xFF

		again, ok := s.ByID(appended.ID)
		if !ok {
			t.Fatalf("ByID(%q) reported NOT FOUND on the second call", appended.ID)
		}
		if !bytes.Equal(again.Body, wantBody) {
			t.Fatalf("the store's BODY was mutated through the value ByID returned: %q, want %q — ByID must return a deep copy", again.Body, wantBody)
		}
		if !reflect.DeepEqual(again.Recipients, wantRecipients) {
			t.Fatalf("the store's RECIPIENT LIST was mutated through the value ByID returned: %v, want %v — a message that is already durable would have been silently re-addressed", again.Recipients, wantRecipients)
		}
		if !reflect.DeepEqual(again.BusPath, wantBusPath) {
			t.Fatalf("the store's BUS PATH was mutated through the value ByID returned: %v, want %v — that is the provenance field invariant 6 names, rewritten after acceptance", again.BusPath, wantBusPath)
		}
		// Signature is the sender's detached Ed25519 signature — metadata invariant
		// 6 names, and copyMessage aliased it before RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY.
		// Without the deep copy, mutating the returned slice rewrites the stored
		// signature and this assertion fails (RED-before-fix).
		if !bytes.Equal(again.Signature, wantSignature) {
			t.Fatalf("the store's SIGNATURE was mutated through the value ByID returned: %x, want %x — copyMessage must deep-copy Message.Signature", again.Signature, wantSignature)
		}
	})

	// A5. THE DUPLICATE-POSITION RUN, on its own, because it is the entire reason
	// byIDLocked scans after its binary search. Append RETAINS a message whose
	// position is not above the head and returns nil (SIGN-1-FU-REORDER-WATERMARK:
	// refusing it would orphan a committed record), so equal positions are
	// reachable and a position is NOT an identity.
	t.Run("DuplicatePositionsAreBothIndividuallyResolvable", func(t *testing.T) {
		clock := newClock()
		s, logBuf := newOriginLookupStore(clock, store.Options{})
		alpha := agentIDFor(t, "alpha")
		beta := agentIDFor(t, "beta")

		first := mkLocalMessageAt(t, alpha, []string{beta}, 1, 7, clock.now(), "first at position seven")
		// SAME Pos, different Seq and therefore a different id.
		second := mkLocalMessageAt(t, alpha, []string{beta}, 2, 7, clock.now(), "second at position seven")
		if first.Pos != second.Pos {
			t.Fatalf("the fixture does not actually produce two equal positions: %d and %d", first.Pos, second.Pos)
		}
		if first.ID == second.ID {
			t.Fatalf("the fixture produced one id twice (%q); the two messages must be distinct", first.ID)
		}
		mustAppend(t, s, first)
		mustAppend(t, s, second)

		// The second append MUST have taken the non-monotone branch — otherwise
		// the positions were not equal in the store either and this test is
		// measuring the ordinary path while claiming to measure the run.
		if n := s.NonMonotonicPositions(); n != 1 {
			t.Fatalf("NonMonotonicPositions() = %d, want 1: the second message was supposed to land on the equal-position branch, which is what creates the RUN this test exists for", n)
		}
		if !strings.Contains(logBuf.String(), "delivery position") {
			t.Fatalf("the equal-position append logged nothing recognisable; invariant 6 makes the SILENCE the defect:\n%s", logBuf.String())
		}

		for _, want := range []store.Message{first, second} {
			got, ok := s.ByID(want.ID)
			if !ok {
				t.Fatalf("ByID(%q) reported NOT FOUND. Both messages share delivery position %d and both are retained, so a lookup that stops at the binary-search hit resolves only one of them", want.ID, want.Pos)
			}
			if got.ID != want.ID {
				t.Fatalf("ByID(%q) returned %q: the scan over the equal-position run is what stops a lookup returning the OTHER message at that position", want.ID, got.ID)
			}
			if !bytes.Equal(got.Body, want.Body) {
				t.Fatalf("ByID(%q) returned the right id but body %q, want %q", want.ID, got.Body, want.Body)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// B. INVARIANT 1 — a pruned id must NEVER be re-resolvable.
// ---------------------------------------------------------------------------

// TestStoreLookupIsNotResolvableAfterPruning is the invariant-1 property stated
// as a negative: the point-lookup indexes cover the RETAINED WINDOW ONLY, they
// mint nothing and they rewind nothing, so a message retention has dropped is
// GONE — and stays gone.
//
// # Why it also asserts the log is EMPTY
//
// Because "returns false" alone cannot fail. byIDLocked never returns a message
// whose id differs from the one asked for, and ids are never reused (invariant
// 1), so a STALE index entry left behind by a prune would ALSO resolve to false
// — through the "the index and the serving copy have disagreed" branch, which
// logs at ERROR. The absence of that line is therefore the only observable
// difference between "the entry was removed" and "the entry was orphaned", and
// an orphaned entry is an unbounded map keyed on every message the bus ever
// held, which is precisely the growth retention exists to stop.
func TestStoreLookupIsNotResolvableAfterPruning(t *testing.T) {
	clock := newClock()
	s, logBuf := newOriginLookupStore(clock, store.Options{MaxAge: time.Hour})
	alpha := agentIDFor(t, "alpha")
	beta := agentIDFor(t, "beta")
	peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")

	originID := originPeerBusID + "-11"
	local := mkLocalMessageAt(t, alpha, []string{beta}, 1, 1, clock.now(), "a local send")
	relayed := mkRelayMessageAt(t, peerAlpha, []string{beta}, 2, 2, clock.now(), "a relay ingest", originID)
	mustAppend(t, s, local)
	mustAppend(t, s, relayed)

	// The control. Without it this test could pass against a store that never
	// resolved anything at all.
	if _, ok := s.ByID(local.ID); !ok {
		t.Fatalf("ByID(%q) is NOT FOUND before pruning; the rest of this test would be vacuous", local.ID)
	}
	if _, ok := s.ByOriginMessageID(originID); !ok {
		t.Fatalf("ByOriginMessageID(%q) is NOT FOUND before pruning; the rest of this test would be vacuous", originID)
	}

	// Past the retention window. Retention is enforced INSIDE the lookups, so no
	// append is needed to drive it — that is deliberate: an IDLE bus must not go
	// on resolving a message whose content retention has already retired, or a
	// relay job would resurrect it.
	clock.advance(2 * time.Hour)

	lookups := []struct {
		name string
		fn   func() (store.Message, bool)
	}{
		{"ByID/local", func() (store.Message, bool) { return s.ByID(local.ID) }},
		{"ByID/relayed", func() (store.Message, bool) { return s.ByID(relayed.ID) }},
		{"ByOriginMessageID/originID", func() (store.Message, bool) { return s.ByOriginMessageID(originID) }},
		{"ByOriginMessageID/localIDFallback", func() (store.Message, bool) { return s.ByOriginMessageID(local.ID) }},
		{"ByOriginMessageID/relayedLocalIDFallback", func() (store.Message, bool) { return s.ByOriginMessageID(relayed.ID) }},
	}
	// Twice: once to prune, once to prove there is no resurrection on the second
	// look. A cache repopulated by a miss would pass the first round and fail here.
	for round := 1; round <= 2; round++ {
		for _, c := range lookups {
			if got, ok := c.fn(); ok {
				t.Fatalf("round %d: %s resolved to %s AFTER the message was pruned. The point-lookup indexes cover the retained window only, and invariant 1 requires that a pruned id is never re-resolvable", round, c.name, got.ID)
			}
		}
	}

	// And the head never rewound, even though everything it counted is gone.
	if got := s.Head(); got != 2 {
		t.Fatalf("Head() = %d after pruning, want 2: the highest sequence ever appended survives retention and never rewinds (invariant 1)", got)
	}
	if n, _, _, _, dropped := s.Stats(); n != 0 || dropped != 2 {
		t.Fatalf("Stats() retained %d / dropped %d, want 0 / 2: the fixture did not actually prune", n, dropped)
	}

	if logBuf.Len() != 0 {
		t.Fatalf("pruning left a STALE entry in a point-lookup index: the lookups above reached the \"index names a position holding no such message\" branch, which is the only thing in this package that logs on a clean path.\n"+
			"The entry must be DELETED when the message is pruned, not merely shadowed — an index keyed on every message the bus ever held is exactly the growth retention exists to stop.\n%s", logBuf.String())
	}
}

// ---------------------------------------------------------------------------
// C. The DURABILITY of the correlation key — the unit half.
// ---------------------------------------------------------------------------

// TestMessageOriginMessageIDRoundTrips is invariant 5 for one field: the origin
// id's only consumer runs AFTER A RESTART, so a field that lived in memory alone
// would be empty at the one moment it is read.
func TestMessageOriginMessageIDRoundTrips(t *testing.T) {
	alpha := agentIDFor(t, "alpha")
	beta := agentIDFor(t, "beta")
	peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")
	sentAt := time.Date(2026, 8, 14, 9, 30, 0, 0, time.UTC)
	originID := originPeerBusID + "-7"

	// C1. Set -> Encode -> Decode -> preserved exactly.
	t.Run("ARelayIngestSurvivesEncodeAndDecode", func(t *testing.T) {
		m, err := store.NewMessageWithBusPath(testBusID, peerAlpha, false, []string{beta}, 4, sentAt,
			[]byte("relayed payload"), "k", testTimestampMs, testSignature(t), []string{originPeerBusID, testBusID})
		if err != nil {
			t.Fatalf("store.NewMessageWithBusPath: %v", err)
		}
		m, err = m.WithOriginMessageID(originID)
		if err != nil {
			t.Fatalf("WithOriginMessageID(%q): %v", originID, err)
		}

		raw, err := m.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		// The key IS on disk for a relay ingest. Asserted on the bytes because
		// that is the fact a restart depends on, and a Record field that never
		// reached JSON would still round-trip through a Go value.
		if !bytes.Contains(raw, []byte(`"origin_message_id":"`+originID+`"`)) {
			t.Fatalf("the durable record does not carry origin_message_id=%q. Its only consumer is relay.Forwarder.Resume, which runs ONLY after a restart, so a field that is not written here is gone at the one moment it is read (invariant 5):\n%s", originID, raw)
		}

		got, err := store.Decode(raw)
		if err != nil {
			t.Fatalf("Decode of a record carrying an origin message id: %v", err)
		}
		if got.OriginMessageID != originID {
			t.Fatalf("after Encode/Decode OriginMessageID = %q, want %q", got.OriginMessageID, originID)
		}
		if got.OriginID() != originID {
			t.Fatalf("after Encode/Decode OriginID() = %q, want the ORIGIN's id %q — not this bus's own %q", got.OriginID(), originID, got.ID)
		}
		if got.ID != m.ID {
			t.Fatalf("Decode changed the LOCAL id: %q, want %q; the origin id is a correlation key and never an identity (invariant 1)", got.ID, m.ID)
		}
	})

	// C2. A locally-originated message writes NO KEY AT ALL. That is what
	// `omitempty` buys, and it is why RecordVersion did not have to move: an OLD
	// build reading this record sees a shape it already understands.
	t.Run("ALocallyOriginatedMessageWritesNoOriginKey", func(t *testing.T) {
		m, err := store.NewMessage(testBusID, alpha, false, []string{beta}, 5, sentAt,
			[]byte("local payload"), "k", testTimestampMs, testSignature(t))
		if err != nil {
			t.Fatalf("store.NewMessage: %v", err)
		}
		if m.OriginMessageID != "" {
			t.Fatalf("a locally-originated message carries OriginMessageID %q; the field is set only on a relay ingest", m.OriginMessageID)
		}

		raw, err := m.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if bytes.Contains(raw, []byte("origin_message_id")) {
			t.Fatalf("the record of a LOCALLY-ORIGINATED message carries the key origin_message_id. It must be absent entirely — that is what `omitempty` buys and what lets a pre-relay build read this record unchanged:\n%s", raw)
		}

		got, err := store.Decode(raw)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if got.OriginMessageID != "" {
			t.Fatalf("Decode invented an origin message id %q for a record that has no such key", got.OriginMessageID)
		}
		if got.OriginID() != got.ID {
			t.Fatalf("OriginID() = %q, want the message's own id %q: an empty OriginMessageID means THIS BUS IS THE ORIGIN", got.OriginID(), got.ID)
		}
	})

	// C3. A LEGACY record — a v2 record written before the field existed. It
	// decodes, and it decodes to "", which is not a loss of information but the
	// right answer: a pre-relay bus originated every message it holds.
	t.Run("ALegacyRecordWithNoOriginKeyDecodes", func(t *testing.T) {
		m, err := store.NewMessage(testBusID, alpha, false, []string{beta}, 6, sentAt,
			[]byte("legacy payload"), "k", testTimestampMs, testSignature(t))
		if err != nil {
			t.Fatalf("store.NewMessage: %v", err)
		}
		// Built by DELETING the key from a known-good record rather than by
		// relying on omitempty, so this stays a genuine legacy-shape test even if
		// the tag ever changes.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encodeRecord(t, recordOf(t, m)), &fields); err != nil {
			t.Fatalf("unmarshalling a known-good record into fields: %v", err)
		}
		delete(fields, "origin_message_id")
		if _, present := fields["origin_message_id"]; present {
			t.Fatalf("the legacy fixture still carries origin_message_id")
		}
		if got := fields["v"]; string(got) != "2" {
			t.Fatalf("the legacy fixture declares schema version %s, want 2", got)
		}
		legacy, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("re-marshalling the legacy record: %v", err)
		}

		got, err := store.Decode(legacy)
		if err != nil {
			t.Fatalf("Decode REFUSED a v2 record with no origin_message_id key: %v\nAn absent key is the ordinary shape of every record written before the field existed; refusing it would have needed a version bump and would discard the history of every upgraded bus", err)
		}
		if got.OriginMessageID != "" {
			t.Fatalf("a record with no origin_message_id key decoded to OriginMessageID = %q", got.OriginMessageID)
		}
		if got.OriginID() != got.ID {
			t.Fatalf("OriginID() = %q, want %q", got.OriginID(), got.ID)
		}
	})

	// C4. And therefore the version did NOT move. Asserted so a future bump has
	// to come past this test and its reasoning.
	t.Run("RecordVersionIsStillTwo", func(t *testing.T) {
		if store.RecordVersion != 2 {
			t.Fatalf("store.RecordVersion = %d, want 2.\norigin_message_id is an OPTIONAL ADDED FIELD, which is the case Record was shaped for: decoding is not strict about unknown fields, an old build reading a new record ignores it, and a new build reading an old record gets \"\" — which is the RIGHT answer, not a loss. If this number moved for that field, it moved for nothing, and every existing bus's message history is discarded on upgrade.", store.RecordVersion)
		}
	})
}

// ---------------------------------------------------------------------------
// D. WithOriginMessageID — one case per refusal.
// ---------------------------------------------------------------------------

// TestMessageWithOriginMessageIDRefusals is the write-path validator. The
// SAME-BUS case is the load-bearing one: Store.ByOriginMessageID's fallback is
// sound ONLY because no message can ever sit in byOrigin under a key of this
// bus's own shape.
func TestMessageWithOriginMessageIDRefusals(t *testing.T) {
	peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")
	beta := agentIDFor(t, "beta")
	sentAt := time.Date(2026, 8, 14, 9, 30, 0, 0, time.UTC)

	// base is a relay-shaped message with NO origin id yet.
	base, err := store.NewMessageWithBusPath(testBusID, peerAlpha, false, []string{beta}, 3, sentAt,
		[]byte("payload"), "k", testTimestampMs, testSignature(t), []string{originPeerBusID, testBusID})
	if err != nil {
		t.Fatalf("store.NewMessageWithBusPath: %v", err)
	}
	// alreadySet carries one origin already, for the two "already set" cases.
	alreadySet, err := base.WithOriginMessageID(originPeerBusID + "-1")
	if err != nil {
		t.Fatalf("seeding the already-set fixture: %v", err)
	}

	cases := []struct {
		name string
		on   store.Message
		set  string
		// wantErr false means the call must SUCCEED and leave this value in place.
		wantErr bool
		// wantIn, when non-empty, must appear in the error text, so a case cannot
		// pass by tripping a DIFFERENT refusal.
		wantIn string
	}{
		{
			// An explicit clear is not a no-op: the zero value already means "this
			// bus is the origin", so erasing a set value would leave a durable
			// record claiming a relayed message originated here — unfalsifiable
			// afterwards.
			name: "Empty", on: alreadySet, set: "", wantErr: true, wantIn: "EMPTY origin message id",
		},
		{
			name: "EmptyOnAMessageWithNoOriginYet", on: base, set: "", wantErr: true, wantIn: "EMPTY origin message id",
		},
		{
			name: "Unparseable", on: base, set: "not a message id", wantErr: true, wantIn: "origin message id",
		},
		{
			// A non-canonical spelling of a real id. Two spellings of one id defeat
			// duplicate detection (invariant 10).
			name: "UnparseableLeadingZeroSequence", on: base, set: originPeerBusID + "-007", wantErr: true, wantIn: "origin message id",
		},
		{
			name: "UnparseableSequenceZero", on: base, set: originPeerBusID + "-0", wantErr: true, wantIn: "origin message id",
		},
		{
			// THE LOAD-BEARING REFUSAL. A message this bus minted is its own
			// origin; recording that twice is a second copy of one fact, free to
			// disagree with the first — and it would put a key of this bus's own
			// shape into byOrigin, which is exactly what ByOriginMessageID's
			// fallback assumes can never happen.
			name: "OriginNamesThisBus", on: base, set: testBusID + "-42", wantErr: true, wantIn: "names THIS bus",
		},
		{
			name: "OriginNamesThisBusWithTheMessagesOwnID", on: base, set: base.ID, wantErr: true, wantIn: "names THIS bus",
		},
		{
			// Reached by a value that is otherwise entirely legal, so the
			// already-set check is what fires rather than the same-bus one.
			name: "AlreadySetToADifferentValue", on: alreadySet, set: originOtherBusID + "-2", wantErr: true, wantIn: "already carries origin message id",
		},
		{
			// INVARIANT 10: same key + SAME payload is a legitimate retry. An
			// ingest step that runs twice must not be punished.
			name: "AlreadySetToTheSameValueIsIdempotent", on: alreadySet, set: originPeerBusID + "-1", wantErr: false,
		},
		{
			name: "AFreshSetSucceeds", on: base, set: originPeerBusID + "-9", wantErr: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := c.on.OriginMessageID
			got, err := c.on.WithOriginMessageID(c.set)

			if !c.wantErr {
				if err != nil {
					t.Fatalf("WithOriginMessageID(%q) = %v, want success", c.set, err)
				}
				if got.OriginMessageID != c.set {
					t.Fatalf("WithOriginMessageID(%q) returned OriginMessageID %q", c.set, got.OriginMessageID)
				}
				if got.ID != c.on.ID {
					t.Fatalf("WithOriginMessageID changed the LOCAL id from %q to %q; the origin id is a correlation key and never an identity", c.on.ID, got.ID)
				}
				return
			}

			if err == nil {
				t.Fatalf("WithOriginMessageID(%q) SUCCEEDED and returned OriginMessageID %q, want a refusal wrapping store.ErrInvalidMessage", c.set, got.OriginMessageID)
			}
			if !errors.Is(err, store.ErrInvalidMessage) {
				t.Fatalf("WithOriginMessageID(%q) = %v, which is not errors.Is(store.ErrInvalidMessage); every refusal here wraps that sentinel so a caller can classify it", c.set, err)
			}
			if c.wantIn != "" && !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("WithOriginMessageID(%q) was refused, but for the wrong reason: %v\nwant an error mentioning %q", c.set, err, c.wantIn)
			}
			// A refused call returns the ZERO Message and must not have touched
			// the receiver — WithOriginMessageID returns a COPY.
			if got.OriginMessageID != "" || got.ID != "" {
				t.Fatalf("a refused WithOriginMessageID returned a non-zero Message %+v", got)
			}
			if c.on.OriginMessageID != before {
				t.Fatalf("WithOriginMessageID mutated its RECEIVER: OriginMessageID is now %q, was %q", c.on.OriginMessageID, before)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// E. Decode applies the SAME origin rule.
// ---------------------------------------------------------------------------

// TestDecodeRefusesABadOriginMessageID pins the recovery-path half of D. Decode
// is the boundary for bytes THIS PROCESS DID NOT VALIDATE — a file written by
// another build, damaged media, or a record handed over by a peer — so the rule
// has to hold on the way back IN as well, or a restart could reload the very
// state the write path refuses to create and break ByOriginMessageID's fallback.
func TestDecodeRefusesABadOriginMessageID(t *testing.T) {
	alpha := agentIDFor(t, "alpha")
	beta := agentIDFor(t, "beta")
	sentAt := time.Date(2026, 8, 14, 9, 30, 0, 0, time.UTC)

	// A KNOWN-GOOD local record. Every case below mutates EXACTLY ONE field of
	// it, so the size and content-hash fields stay consistent and a refusal
	// cannot be the wrong refusal in disguise.
	good, err := store.NewMessage(testBusID, alpha, false, []string{beta}, 12, sentAt,
		[]byte("payload"), "k", testTimestampMs, testSignature(t))
	if err != nil {
		t.Fatalf("store.NewMessage: %v", err)
	}

	// The control, and it runs FIRST: if the base record did not decode, every
	// refusal below would be meaningless.
	t.Run("ControlTheUnmutatedRecordDecodes", func(t *testing.T) {
		if _, err := store.Decode(encodeRecord(t, recordOf(t, good))); err != nil {
			t.Fatalf("the base record does not decode: %v — every refusal in this test would then be measuring the wrong fault", err)
		}
	})

	// And the same base record WITH a legal peer origin id decodes too, which is
	// what makes the refusals below about the VALUE rather than about the field
	// being present at all.
	t.Run("ControlALegalPeerOriginDecodes", func(t *testing.T) {
		rec := recordOf(t, good)
		rec.OriginMessageID = originPeerBusID + "-5"
		got, err := store.Decode(encodeRecord(t, rec))
		if err != nil {
			t.Fatalf("Decode refused a record carrying a LEGAL peer origin id: %v", err)
		}
		if got.OriginMessageID != originPeerBusID+"-5" {
			t.Fatalf("Decode returned OriginMessageID %q, want %q", got.OriginMessageID, originPeerBusID+"-5")
		}
	})

	cases := []struct {
		name   string
		origin string
		wantIn string
	}{
		{
			// THE LOAD-BEARING ONE. A record asserting that a message THIS BUS
			// minted originated somewhere else on this same bus is a second copy of
			// one fact, free to disagree with rec.MessageID — and it would seed
			// byOrigin with a key of this bus's own shape, breaking the fallback.
			name: "OriginNamesTheRecordsOwnBus", origin: testBusID + "-99", wantIn: "SAME bus",
		},
		{
			name: "OriginIsTheRecordsOwnMessageID", origin: good.ID, wantIn: "SAME bus",
		},
		{
			name: "OriginIsUnparseable", origin: "not a message id", wantIn: "origin message id",
		},
		{
			name: "OriginHasNoSequence", origin: originPeerBusID + "-", wantIn: "origin message id",
		},
		{
			name: "OriginHasSequenceZero", origin: originPeerBusID + "-0", wantIn: "origin message id",
		},
		{
			name: "OriginHasAnInvalidBusHalf", origin: "not a bus id!-3", wantIn: "origin message id",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := recordOf(t, good)
			rec.OriginMessageID = c.origin

			got, err := store.Decode(encodeRecord(t, rec))
			if err == nil {
				t.Fatalf("Decode ACCEPTED a record whose origin_message_id is %q and returned %+v.\nDecode is the boundary for bytes this process did not validate, and it must apply exactly the rule Message.WithOriginMessageID applies on the way in — otherwise a restart reloads state the write path refuses to create", c.origin, got)
			}
			if !errors.Is(err, store.ErrInvalidMessage) {
				t.Fatalf("Decode(origin=%q) = %v, which is not errors.Is(store.ErrInvalidMessage)", c.origin, err)
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("Decode(origin=%q) was refused, but for the wrong reason: %v\nwant an error mentioning %q — only one field was mutated, so any other refusal means this case is measuring something else", c.origin, err, c.wantIn)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// F. Store.ByOriginMessageID — the hit path and the fallback.
// ---------------------------------------------------------------------------

// TestStoreLookupByOriginMessageID pins the correlation lookup, including the
// FALLBACK to a local id, which is what makes the single-hop egress case — this
// bus is the origin, one peer downstream — recover across a restart with no new
// durable state at all (RELAY-24-BLOCKER-EGRESS).
func TestStoreLookupByOriginMessageID(t *testing.T) {
	clock := newClock()
	s, logBuf := newOriginLookupStore(clock, store.Options{})
	alpha := agentIDFor(t, "alpha")
	beta := agentIDFor(t, "beta")
	peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")

	originID := originPeerBusID + "-3"
	local := mkLocalMessageAt(t, alpha, []string{beta}, 1, 1, clock.now(), "a local send")
	relayed := mkRelayMessageAt(t, peerAlpha, []string{beta}, 2, 2, clock.now(), "a relay ingest", originID)
	mustAppend(t, s, local)
	mustAppend(t, s, relayed)

	cases := []struct {
		name string
		// ask is the id handed to ByOriginMessageID.
		ask string
		// want is the LOCAL id that must come back, or "" for NOT FOUND.
		want string
		why  string
	}{
		{
			name: "ARelayIngestIsFoundByItsOriginID", ask: originID, want: relayed.ID,
			why: "the hit path: the ingest recorded the origin's id, so byOrigin resolves it to this bus's local copy",
		},
		{
			name: "ALocallyOriginatedMessageIsFoundByItsOwnID", ask: local.ID, want: local.ID,
			why: "THE FALLBACK, and the case RELAY-24-BLOCKER-EGRESS turns on: this bus IS the origin, so its local id already IS the origin id (Message.OriginID) and no durable state is needed to say so",
		},
		{
			name: "ARelayIngestIsAlsoFoundByItsOwnLocalID", ask: relayed.ID, want: relayed.ID,
			why: "the fallback resolves any retained local id; a relay ingest is not excluded from its own identity",
		},
		{
			name: "APeerIDThatWasNeverIngestedIsNotFound", ask: originPeerBusID + "-4", want: "",
			why: "nothing was ever ingested under it, and the fallback cannot rescue it: an id of the PEER's shape is never a local id",
		},
		{
			name: "APeerIDOnAThirdBusIsNotFound", ask: originOtherBusID + "-3", want: "",
			why: "same sequence as the ingested origin id, different bus — the bus half is part of the key",
		},
		{
			name: "AWellFormedLocalIDThatWasNeverAppendedIsNotFound", ask: testBusID + "-77", want: "",
			why: "the fallback resolves through the SAME id index ByID uses, so it cannot invent a message",
		},
		{
			name: "GibberishIsNotFound", ask: "not a message id", want: "",
		},
		{
			name: "EmptyIsNotFound", ask: "", want: "",
			why: "the empty value means \"this bus is the origin\" on a Message; it is not a key and must never resolve to the first message in the store",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := s.ByOriginMessageID(c.ask)
			if c.want == "" {
				if ok {
					t.Fatalf("ByOriginMessageID(%q) resolved to %s, want NOT FOUND.\n%s", c.ask, got.ID, c.why)
				}
				return
			}
			if !ok {
				t.Fatalf("ByOriginMessageID(%q) reported NOT FOUND, want %s.\n%s", c.ask, c.want, c.why)
			}
			if got.ID != c.want {
				t.Fatalf("ByOriginMessageID(%q) resolved to %s, want %s.\n%s", c.ask, got.ID, c.want, c.why)
			}
		})
	}

	// The negative that matters most for a relay forward: asking under ANOTHER
	// message's local id must not hand back the relay ingest. Doing so would ship
	// one agent's message to a peer under another message's correlation id.
	t.Run("ARelayIngestIsNotFoundUnderAnotherMessagesLocalID", func(t *testing.T) {
		got, ok := s.ByOriginMessageID(local.ID)
		if !ok {
			t.Fatalf("ByOriginMessageID(%q) reported NOT FOUND; the fallback must resolve a locally-originated message", local.ID)
		}
		if got.ID == relayed.ID {
			t.Fatalf("ByOriginMessageID(%q) returned the RELAY-INGESTED message %s. The two live under different keys and must never alias", local.ID, relayed.ID)
		}
		if !bytes.Equal(got.Body, local.Body) {
			t.Fatalf("ByOriginMessageID(%q) returned body %q, want %q", local.ID, got.Body, local.Body)
		}
	})

	// The returned message is a deep copy here too — ByOriginMessageID resolves
	// through the same byIDLocked, and this pins that it is not a second exit
	// point with different rules.
	t.Run("TheReturnedMessageIsADeepCopy", func(t *testing.T) {
		// Snapshotted BEFORE anything is mutated, for the reason spelled out in
		// TestStoreLookupByMessageID/TheReturnedMessageIsADeepCopy: the fixture
		// variable aliases the very arrays the serving copy holds, so comparing
		// against it would be comparing a mutated array with itself.
		wantBody := append([]byte(nil), relayed.Body...)
		wantBusPath := append([]string(nil), relayed.BusPath...)

		got, ok := s.ByOriginMessageID(originID)
		if !ok {
			t.Fatalf("ByOriginMessageID(%q) reported NOT FOUND", originID)
		}
		got.Body[0] = 'X'
		got.BusPath[0] = "attackerbus"
		again, _ := s.ByOriginMessageID(originID)
		if !bytes.Equal(again.Body, wantBody) {
			t.Fatalf("the store's body was mutated through the value ByOriginMessageID returned: %q, want %q", again.Body, wantBody)
		}
		if !reflect.DeepEqual(again.BusPath, wantBusPath) {
			t.Fatalf("the store's bus path was mutated through the value ByOriginMessageID returned: %v, want %v", again.BusPath, wantBusPath)
		}
	})

	if got := s.DuplicateOriginMessageIDs(); got != 0 {
		t.Fatalf("DuplicateOriginMessageIDs() = %d on a healthy fixture, want 0", got)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("the ordinary path logged something; every line this package emits is a server-bug fault:\n%s", logBuf.String())
	}
}

// ---------------------------------------------------------------------------
// G. Two retained messages carrying ONE origin id.
// ---------------------------------------------------------------------------

// TestStoreDuplicateOriginMessageIDs pins the three-part resolution Append makes
// when the relay applied-key memory has been lost: RETAIN, RETURN NIL, BE LOUD.
//
// Returning an error here would be the SIGN-1-FU-OUTOFORDER-POISON P0 all over
// again: by the time Append runs the record is committed and fsynced (invariant
// 4), so a refusal orphans it on disk and poisons the hub. Returning nil WITHOUT
// retaining would be silent suppression of an acknowledged message, which
// invariant 6 names as the actual defect.
func TestStoreDuplicateOriginMessageIDs(t *testing.T) {
	// G1. Both retained, both individually resolvable, counted, and the index
	// resolves to the LAST one.
	t.Run("BothAreRetainedAndTheIndexResolvesToTheLastOne", func(t *testing.T) {
		clock := newClock()
		s, logBuf := newOriginLookupStore(clock, store.Options{})
		beta := agentIDFor(t, "beta")
		peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")

		originID := originPeerBusID + "-3"
		first := mkRelayMessageAt(t, peerAlpha, []string{beta}, 1, 1, clock.now(), "the first local copy", originID)
		second := mkRelayMessageAt(t, peerAlpha, []string{beta}, 2, 2, clock.now(), "the second local copy", originID)
		if first.ID == second.ID {
			t.Fatalf("the fixture produced one local id twice (%q); the point is TWO local messages carrying ONE origin id", first.ID)
		}
		if first.OriginMessageID != second.OriginMessageID {
			t.Fatalf("the fixture did not actually produce a duplicate origin id: %q and %q", first.OriginMessageID, second.OriginMessageID)
		}

		if err := s.Append(first); err != nil {
			t.Fatalf("Append(first) = %v, want nil", err)
		}
		if err := s.Append(second); err != nil {
			t.Fatalf("Append of a SECOND message carrying origin id %q was REFUSED: %v\n"+
				"The record is already committed and fsynced by the time the serving copy sees it (invariant 4), so a refusal orphans it on disk and poisons the hub — exactly the P0 SIGN-1-FU-OUTOFORDER-POISON fixed. Retain, stay up, and be LOUD instead.", originID, err)
		}

		// RETAINED — both of them, and both individually resolvable by their own
		// LOCAL id. Losing the older copy's identity would be losing an
		// acknowledged message.
		for _, want := range []store.Message{first, second} {
			got, ok := s.ByID(want.ID)
			if !ok {
				t.Fatalf("ByID(%q) reported NOT FOUND. Both copies are retained and delivered; only the ORIGIN-ID index is last-writer-wins", want.ID)
			}
			if !bytes.Equal(got.Body, want.Body) {
				t.Fatalf("ByID(%q) returned body %q, want %q", want.ID, got.Body, want.Body)
			}
		}
		if n, _, _, _, _ := s.Stats(); n != 2 {
			t.Fatalf("Stats() retained %d messages, want 2: both copies stay in the serving copy", n)
		}

		// COUNTED. A log line is not queryable, and this is the signal that the
		// durable applied-key memory for the relay ingest scope was lost.
		if got := s.DuplicateOriginMessageIDs(); got != 1 {
			t.Fatalf("DuplicateOriginMessageIDs() = %d, want 1", got)
		}
		// LOUD (invariant 6 — the silence is the defect, not the last-wins).
		logged := logBuf.String()
		for _, want := range []string{"SAME origin message id", originID, first.ID, second.ID} {
			if !strings.Contains(logged, want) {
				t.Fatalf("the duplicate-origin log line does not mention %q; the discard of the OLD index entry must be logged loudly and SPECIFICALLY:\n%s", want, logged)
			}
		}

		// LAST WINS in the index — stated, bounded and tested, so nobody
		// "improves" it into first-wins without meeting this assertion.
		got, ok := s.ByOriginMessageID(originID)
		if !ok {
			t.Fatalf("ByOriginMessageID(%q) reported NOT FOUND after two messages claimed it", originID)
		}
		if got.ID != second.ID {
			t.Fatalf("ByOriginMessageID(%q) resolved to %s, want the NEWER copy %s: the index is last-writer-wins, so at worst one relay job resumes against the newer message", originID, got.ID, second.ID)
		}
	})

	// G2. THE PRUNE GUARD. byOrigin is last-writer-wins, so an unconditional
	// delete when the OLDER copy is pruned would remove an entry pointing at a
	// message that is STILL RETAINED — and the survivor would silently stop being
	// resolvable by its origin id, abandoning a relay job that was perfectly
	// recoverable.
	t.Run("PruningTheOlderCopyLeavesTheSurvivorResolvable", func(t *testing.T) {
		clock := newClock()
		s, _ := newOriginLookupStore(clock, store.Options{MaxAge: 2 * time.Hour})
		beta := agentIDFor(t, "beta")
		peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")

		originID := originPeerBusID + "-3"
		older := mkRelayMessageAt(t, peerAlpha, []string{beta}, 1, 1, clock.now(), "the older local copy", originID)
		mustAppend(t, s, older)

		clock.advance(time.Hour)
		newer := mkRelayMessageAt(t, peerAlpha, []string{beta}, 2, 2, clock.now(), "the newer local copy", originID)
		mustAppend(t, s, newer)

		if got := s.DuplicateOriginMessageIDs(); got != 1 {
			t.Fatalf("DuplicateOriginMessageIDs() = %d, want 1: the fixture did not produce the duplicate this test is about", got)
		}
		if _, ok := s.ByID(older.ID); !ok {
			t.Fatalf("the older copy is already gone before pruning; the rest of this test would be vacuous")
		}

		// Advance so the cutoff falls BETWEEN the two SentAt values: the older
		// copy expires, the newer does not.
		clock.advance(90 * time.Minute)

		if _, ok := s.ByID(older.ID); ok {
			t.Fatalf("the OLDER copy was not pruned; this test needs exactly one of the two to age out")
		}
		if _, ok := s.ByID(newer.ID); !ok {
			t.Fatalf("the NEWER copy was pruned too; this test needs exactly one of the two to age out")
		}

		got, ok := s.ByOriginMessageID(originID)
		if !ok {
			t.Fatalf("ByOriginMessageID(%q) is NOT FOUND after only the OLDER of the two copies was pruned.\n"+
				"The index is last-writer-wins, so it pointed at the SURVIVOR — a prune that deletes the entry unconditionally removes a live correlation and abandons a relay job that was perfectly recoverable. The delete must fire only when the entry still names the message being pruned.", originID)
		}
		if got.ID != newer.ID {
			t.Fatalf("ByOriginMessageID(%q) resolved to %s, want the surviving newer copy %s", originID, got.ID, newer.ID)
		}
		if !bytes.Equal(got.Body, newer.Body) {
			t.Fatalf("ByOriginMessageID(%q) returned body %q, want %q", originID, got.Body, newer.Body)
		}

		// And once the survivor ages out too, the origin id is gone for good
		// (invariant 1: a pruned id is never re-resolvable).
		clock.advance(2 * time.Hour)
		if got, ok := s.ByOriginMessageID(originID); ok {
			t.Fatalf("ByOriginMessageID(%q) resolved to %s after BOTH copies were pruned", originID, got.ID)
		}
	})
}
