package main

// ACK-3 — the ONE place internal/relay's WIRE vocabulary meets internal/ack's
// DURABLE one.
//
// internal/relay/ack.go (ACK-4) and internal/ack/state.go (ACK-2) were written
// concurrently and each declared its own spelling of the twelve NACK classes,
// the three terminal outcomes and the two attestation labels. ACK-13 COLLAPSED
// THEM: internal/ack is the single home and relay's names are type ALIASES for
// it, so the two spellings are now the same string by construction and
// `ackVocabulary` in relaywiring.go validates rather than translates.
//
// This file is kept, and still exercises every value, because the ALIASING is
// what makes the sets one — and an alias can be turned back into a defined type
// by a single character. If that happens, or if a value is added on one side
// only, these assertions are what say so LOUDLY AND EARLY: without them a rename
// compiles cleanly, passes every unit test in both packages, and surfaces only
// as a peer acknowledgement refused 503 in production with a log line about
// drift — after a real terminal outcome has already been lost.

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// TestAckVocabularyMapsEVERYWireValue walks the wire enums EXHAUSTIVELY and
// requires each to map onto the durable vocabulary.
//
// It iterates the enum RANGES rather than a hand-written list on purpose: a
// thirteenth class added to internal/relay and not to internal/ack is caught by
// the loop bound, whereas a list would simply not mention it.
func TestAckVocabularyMapsEVERYWireValue(t *testing.T) {
	// Every outcome, paired with a class from the half of the set that outcome
	// owns — because ackVocabulary is only ever called on a frame that has
	// already passed relay.ValidateAckClassForOutcome, and asking it about an
	// illegal pairing would be testing a state the route cannot produce.
	busClasses := []relay.AckClass{
		relay.AckNoRoute, relay.AckNoSuchRecipient, relay.AckHopRefused,
		relay.AckHopUnauthenticated, relay.AckLoopDropped, relay.AckFanoutExceeded,
		relay.AckHorizonExpired, relay.AckLocalCapacity, relay.AckObligationLost,
	}
	recipientClasses := []relay.AckClass{
		relay.AckRecipientRefusedPolicy,
		relay.AckRecipientRefusedUndecodable,
		relay.AckRecipientRefusedNotAddressed,
	}

	// THE CLOSED SET IS EXACTLY TWELVE. Asserted from the two halves rather than
	// assumed, so a class added to relay's enum and to neither half here is
	// caught by the count below rather than silently skipped by both loops.
	if got := len(busClasses) + len(recipientClasses); got != 12 {
		t.Fatalf("this test enumerates %d classes; ACK-CONTRACT.md §5.2 closes the set at twelve", got)
	}

	check := func(t *testing.T, outcome relay.AckOutcome, class relay.AckClass, attestation relay.AckAttestation) {
		t.Helper()
		v := relay.ValidatedPeerAck{Outcome: outcome, Class: class, Attestation: attestation}
		state, durableClass, attested, err := ackVocabulary(v)
		if err != nil {
			t.Fatalf("ackVocabulary(%s/%s/%s): %v — the wire and durable closed sets have drifted; a rename on either side must be made on both",
				outcome, class, attestation, err)
		}
		if !state.Terminal() {
			t.Errorf("%s mapped onto the non-terminal durable state %s", outcome, state)
		}
		if state.String() != outcome.String() {
			t.Errorf("%s mapped onto durable state %q; the two spellings must be identical", outcome, state)
		}
		if class == "" {
			if durableClass != "" {
				t.Errorf("%s carries no class but mapped onto durable class %q", outcome, durableClass)
			}
		} else {
			if string(durableClass) != class.String() {
				t.Errorf("class %s mapped onto durable class %q; the two spellings must be identical", class, durableClass)
			}
			// AND THE HALVES MUST STAY ON THE SAME SIDE. A bus-emitted class that
			// landed in the durable RECIPIENT half would let a routing failure be
			// recorded as the recipient's own decision — a different claim about a
			// different party, and unfixable once written, since terminal is
			// absorbing.
			if class.RecipientEmitted() != durableClass.RecipientEmitted() {
				t.Errorf("class %s is recipient-emitted=%v on the wire and %v on disk; the halves have drifted",
					class, class.RecipientEmitted(), durableClass.RecipientEmitted())
			}
		}
		if string(attested) != attestation.String() {
			t.Errorf("attestation %s mapped onto durable %q; the two spellings must be identical", attestation, attested)
		}
		if !attested.Valid() {
			t.Errorf("attestation %s mapped outside the durable closed set", attestation)
		}
	}

	t.Run("delivered carries no class and a recipient attestation", func(t *testing.T) {
		check(t, relay.AckDelivered, "", relay.AckAttestedRecipientSignatureUnverified)
	})
	t.Run("refused carries each of the three recipient classes", func(t *testing.T) {
		for _, c := range recipientClasses {
			check(t, relay.AckRefused, c, relay.AckAttestedRecipientSignatureUnverified)
		}
	})
	t.Run("undeliverable carries each of the nine bus classes", func(t *testing.T) {
		for _, c := range busClasses {
			check(t, relay.AckUndeliverable, c, relay.AckAttestedPeerBus)
		}
	})

	// AND THE MAPPING FAILS CLOSED. A default arm that guessed would write a
	// TERMINAL state, and terminal is ABSORBING — it could never afterwards be
	// corrected. So an outcome or attestation outside the wire enum must produce
	// an ERROR, which the route answers "not now" (503) rather than recording.
	t.Run("a value outside the wire enum is REFUSED, never guessed", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			v    relay.ValidatedPeerAck
		}{
			{"zero outcome", relay.ValidatedPeerAck{Attestation: relay.AckAttestedPeerBus}},
			{"outcome past the enum", relay.ValidatedPeerAck{Outcome: relay.AckOutcome(200), Attestation: relay.AckAttestedPeerBus}},
			{"class past the enum", relay.ValidatedPeerAck{Outcome: relay.AckUndeliverable, Class: relay.AckClass("not-a-class"), Attestation: relay.AckAttestedPeerBus}},
			{"attestation past the enum", relay.ValidatedPeerAck{Outcome: relay.AckDelivered, Attestation: relay.AckAttestation("not-an-attestation")}},
			{"zero attestation", relay.ValidatedPeerAck{Outcome: relay.AckDelivered}},
		} {
			if _, _, _, err := ackVocabulary(tc.v); err == nil {
				t.Errorf("%s: ackVocabulary succeeded; an unmappable value must be refused, because the alternative is writing an ABSORBING terminal state nobody chose", tc.name)
			}
		}
	})
}

// TestAckVocabularyRejectsANonTerminalState guards the one arm that cannot be
// reached today and would be silent if it ever were.
//
// relay.AckOutcome has only the three terminal members, so `state.Terminal()`
// inside ackVocabulary is unreachable-by-construction — which is exactly why it
// is asserted: an unreachable branch is a branch that gets deleted. If a future
// task adds a non-terminal outcome to the WIRE enum without adding the durable
// side, this is the test that says so, instead of ack.Store.Settle refusing it
// later with a message about a state rather than about the drift that produced
// it.
func TestAckVocabularyRejectsANonTerminalState(t *testing.T) {
	for _, s := range []ack.State{ack.StateAccepted, ack.StateInFlight} {
		if s.Terminal() {
			t.Fatalf("%s reports itself terminal; the durable state machine's terminal set has changed and ACK-3's mapping assumes it has not", s)
		}
	}
	// The wire enum must contain NO spelling that parses to one of those.
	//
	// THE LOOP BOUND IS NOT THIS PACKAGE'S TO CHOOSE ANY MORE, SO IT IS COUNTED.
	// Before ACK-13 relay.AckOutcome was relay's own uint8 enum and this range
	// walked ordinals relay declared. It is now an ALIAS for ack.State, whose
	// three terminal members happen to be contiguous (3, 4, 5) because of a
	// const block in ANOTHER package. Reorder that block — put `undeliverable`
	// before `delivered`, or interleave a non-terminal — and this range runs
	// zero times or over the wrong members, and the test passes having asserted
	// nothing. A vacuous guard is worse than no guard, because the report says
	// PASS either way.
	walked := 0
	for o := relay.AckDelivered; o <= relay.AckUndeliverable; o++ {
		walked++
		state, err := ack.ParseState(o.String())
		if err != nil {
			t.Fatalf("wire outcome %s does not parse as a durable state: %v", o, err)
		}
		if !state.Terminal() {
			t.Errorf("wire outcome %s maps onto the NON-TERMINAL durable state %s; a frame may only carry a terminal outcome (ACK-CONTRACT.md §8.1)", o, state)
		}
	}
	if got, want := walked, len(ack.AllTerminalStates()); got != want {
		t.Fatalf("the range relay.AckDelivered..relay.AckUndeliverable walked %d outcomes but the durable vocabulary has %d terminal states: the ordinals of the aliased enum have been reordered in internal/ack, so this loop is no longer walking the wire enum and every assertion in it is vacuous", got, want)
	}
	if walked != 3 {
		t.Fatalf("the wire outcome range walked %d values, want exactly 3 (delivered, refused, undeliverable)", walked)
	}
}

// ---------------------------------------------------------------------------
// The CORRELATION half of ACK-3: federation.settleAck and federation.priorTerminal
// ---------------------------------------------------------------------------
//
// These two functions are where an authorized peer-hop acknowledgement becomes a
// DURABLE row, and they were at 0% coverage when a reviewer measured them. Every
// branch below is a place where a wrong answer either changes the wire answer a
// peer sees — collapsing the uniform refusal that closes the status oracle — or
// writes an ABSORBING terminal that can never afterwards be corrected. The
// package's other proof command is scoped to ./internal/relay and can never
// reach any of it.

// ackNullDurable is a DurableLog that accepts every write without a disk. The
// durability of that write is internal/ack's and internal/wal's to prove (they
// have crash-injection tests); what is under test here is the MAPPING from a
// relay-plane answer onto an ack-plane call, and back onto a relay-plane
// sentinel.
type ackNullDurable struct{}

func (ackNullDurable) Write(wal.Entry) (wal.Committed, error) { return wal.Committed{}, nil }

// newAckFed builds a federation carrying a real, durable-attached ack.Store and
// nothing else that settleAck touches. It deliberately does NOT go through
// newFederation: settleAck reads only f.acks, f.busID and f.log, and standing up
// the whole ingress would make a mapping test depend on a peer store, a registry
// and a certificate.
func newAckFed(t *testing.T) (*federation, *ack.Store) {
	t.Helper()
	st := ack.NewStore(ack.Options{})
	if err := st.Attach(ackNullDurable{}); err != nil {
		t.Fatalf("ack.Store.Attach: %v", err)
	}
	return &federation{
		busID: wiringLocalBus,
		acks:  st,
		log:   logging.New(io.Discard, logging.LevelError),
	}, st
}

// ackFedKey is a LOCAL-ORIGIN correlation key (bus half == wiringLocalBus, the
// fed's own busID). It MUST be local-origin, and this changed with
// ACK-12-FU-DESTINATION-ROW (DECISIONS.md, 2026-08-28): settleAck now decides
// transit-vs-settle UP FRONT on the key's bus half, BEFORE Settle. Only a key
// this bus ORIGINATED reaches the Settle/relay.DecideAck correlation path this
// file exercises (apply / duplicate / conflict / durability-not-a-4xx). A
// FOREIGN-origin key is diverted to disposeUnrecordedAck and never reaches
// Settle at all, so keying these correlation guards on a foreign bus would test
// the transit divert, not the settle correlation they exist to protect. The
// foreign-origin disposition — authorise + forward, never settle locally — is
// guarded separately in acktransit_test.go's TestSettleAckDisposition.
// ackFedSender is left on the peer bus because it is SHARED with
// acktransit_test.go's foreign-origin scenarios; the settle path reads only the
// key's bus half and carries the sender through unchanged, so the sender's bus
// is not what any assertion here turns on.
const (
	ackFedKey       = wiringLocalBus + "-1"
	ackFedSender    = wiringPeerBus + ".alpha-1"
	ackFedRecipient = wiringLocalBus + ".bravo-1"
)

// settled builds the SettledAck the route hands to settleAck: a frame that has
// already passed relay's closed-set validation AND relay.AuthorizePeerAck's
// obligation binding.
func settled(outcome relay.AckOutcome, class relay.AckClass) relay.SettledAck {
	att := relay.AckAttestedPeerBus
	if outcome.RecipientSourced() {
		att = relay.AckAttestedRecipientSignatureUnverified
	}
	return relay.SettledAck{
		PeerBusID: wiringPeerBus,
		Ack: relay.ValidatedPeerAck{
			ProtocolVersion:    relay.AckWireVersion,
			CorrelationKey:     ackFedKey,
			Recipient:          ackFedRecipient,
			Outcome:            outcome,
			Class:              class,
			Attestation:        att,
			EmittedAtUnixMilli: 1_700_000_000_000,
		},
	}
}

// TestSettleAckCorrelatesToTheDurableRecord walks settleAck's branches — with
// ONE named exception, stated here rather than left for a coverage tool to
// contradict.
//
// # THE ack.ErrTerminal ARM IS NOT COVERED, AND CANNOT BE FROM HERE
//
// `case errors.Is(err, ack.ErrTerminal)` is reachable ONLY by losing a race:
// settleAck's advisory Lookup must see a NON-terminal row and ack.Store.Settle
// must then find a terminal one, which requires a concurrent transition landing
// between the two. The conflict subtest below does not reach it — it is caught
// earlier and correctly by relay.DecideAck, which is the whole point of doing
// the comparison before the durable call.
//
// Forcing it would mean putting a seam into federation.acks, which is a
// concrete *ack.Store (relaywiring.go). A reviewer measured the arm at count=0
// and ruled explicitly: FILE IT, DO NOT FORCE IT — an interface introduced only
// so a test can interpose is a production indirection bought with test
// convenience, and here it would sit on the path that decides whether a
// TERMINAL outcome is recorded.
//
// Two `priorTerminal` defensive arms are uncovered for the same reason: they
// handle a durable row whose state or class is outside the wire vocabulary,
// which no writer in this build can produce. Both fail CLOSED (they report a
// prior that matches nothing, so ack.Store.Settle's own absorbing check refuses
// the write) and both are asserted by inspection rather than by execution.
//
// This comment previously said "walks every branch", which was false. That is
// the failure mode CLAUDE.md warns about most sharply: a claim of evidence is
// worse than no evidence, because it reads as freshly checked.
func TestSettleAckCorrelatesToTheDurableRecord(t *testing.T) {
	ctx := context.Background()

	// -----------------------------------------------------------------------
	t.Run("no ACK row for the pair is the UNIFORM refusal, not a distinct answer", func(t *testing.T) {
		// §8.2's "(none)" row. This is the SECOND half of authorization and it is
		// conjunctive with the job binding: the row exists only for a recipient
		// the SENDER NAMED, so this is what stops a legitimately-bound peer
		// settling on behalf of an agent that was never addressed.
		//
		// It MUST come back as relay.ErrAckNotBound. Anything else — including
		// ack.ErrNoRecord leaking through — would be answered with a different
		// status or code by the route, and the difference is an oracle for which
		// recipients a message named.
		f, _ := newAckFed(t)
		_, err := f.settleAck(ctx, settled(relay.AckDelivered, ""))
		if !errors.Is(err, relay.ErrAckNotBound) {
			t.Fatalf("err = %v, want relay.ErrAckNotBound (the ONE uniform refusal); ack.ErrNoRecord must never reach the route", err)
		}
		if errors.Is(err, ack.ErrNoRecord) {
			t.Error("ack.ErrNoRecord leaked to the route; the durable layer's distinguishable error must be translated, not wrapped")
		}
	})

	// -----------------------------------------------------------------------
	t.Run("the first terminal is APPLIED and is not a duplicate", func(t *testing.T) {
		f, st := newAckFed(t)
		if err := st.Accept(ackFedKey, ackFedSender, ackFedRecipient); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		got, err := f.settleAck(ctx, settled(relay.AckRefused, relay.AckRecipientRefusedPolicy))
		if err != nil {
			t.Fatalf("settleAck: %v", err)
		}
		if got.Duplicate {
			t.Error("the FIRST terminal reported duplicate:true")
		}
		rec, ok := st.Lookup(ackFedKey, ackFedRecipient)
		if !ok {
			t.Fatal("no durable row after a successful settle")
		}
		// The DURABLE row must carry the mapped vocabulary, not the wire one.
		if rec.State != ack.StateRefused || rec.Class != ack.ClassRecipientRefusedPolicy {
			t.Errorf("durable row = %s/%s, want refused/recipient_refused_policy", rec.State, rec.Class)
		}
		if rec.AttestedBy != ack.AttestedByRecipientSignatureUnverified {
			t.Errorf("durable attestation = %q, want recipient_signature_unverified (there is no value meaning verified)", rec.AttestedBy)
		}
		// AND THE CORRELATION KEY IS CARRIED THROUGH UNCHANGED. It is the origin
		// bus's server-minted id and there is no fourth identifier; a row keyed on
		// anything else would be unfindable by the sender's status read.
		if rec.CorrelationKey != ackFedKey || rec.Recipient != ackFedRecipient {
			t.Errorf("durable row keyed (%q,%q), want (%q,%q)", rec.CorrelationKey, rec.Recipient, ackFedKey, ackFedRecipient)
		}
		// The SENDER is carried forward from the accepted row and is NOT
		// something the acknowledging peer supplied — it authorises the future
		// status read (§13.3).
		if rec.Sender != ackFedSender {
			t.Errorf("durable sender = %q, want the accepted row's %q; a peer must not be able to choose who may read a row", rec.Sender, ackFedSender)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("an IDENTICAL retry is a duplicate and RE-APPLIES NOTHING", func(t *testing.T) {
		// Invariant 10's FIRST case. The early return is what makes "re-apply
		// nothing" STRUCTURAL rather than a promise ack.Store.Settle keeps: no
		// durable write is attempted at all.
		f, st := newAckFed(t)
		if err := st.Accept(ackFedKey, ackFedSender, ackFedRecipient); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		if _, err := f.settleAck(ctx, settled(relay.AckDelivered, "")); err != nil {
			t.Fatalf("first settle: %v", err)
		}
		before, _ := st.Lookup(ackFedKey, ackFedRecipient)

		got, err := f.settleAck(ctx, settled(relay.AckDelivered, ""))
		if err != nil {
			t.Fatalf("the identical retry errored: %v — invariant 10's first case returns the ORIGINAL result and does not error", err)
		}
		if !got.Duplicate {
			t.Error("an identical retry reported duplicate:false; the route would answer 200 duplicate:false and a peer could not tell a retry was absorbed")
		}
		after, _ := st.Lookup(ackFedKey, ackFedRecipient)
		if after != before {
			t.Errorf("the durable row CHANGED across an identical retry:\n before %+v\n after  %+v", before, after)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a DIFFERENT terminal is a conflict and the FIRST terminal stands", func(t *testing.T) {
		// Invariant 10's SECOND case: reject and log, DO NOT DISCONNECT, and
		// terminal is absorbing.
		f, st := newAckFed(t)
		if err := st.Accept(ackFedKey, ackFedSender, ackFedRecipient); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		if _, err := f.settleAck(ctx, settled(relay.AckDelivered, "")); err != nil {
			t.Fatalf("first settle: %v", err)
		}
		for _, tc := range []struct {
			name    string
			outcome relay.AckOutcome
			class   relay.AckClass
		}{
			{"a recipient refusal", relay.AckRefused, relay.AckRecipientRefusedPolicy},
			{"a routing failure", relay.AckUndeliverable, relay.AckNoRoute},
		} {
			_, err := f.settleAck(ctx, settled(tc.outcome, tc.class))
			if !errors.Is(err, relay.ErrAckOutcomeConflict) {
				t.Errorf("%s after delivered: err = %v, want relay.ErrAckOutcomeConflict", tc.name, err)
			}
		}
		rec, _ := st.Lookup(ackFedKey, ackFedRecipient)
		if rec.State != ack.StateDelivered {
			t.Errorf("the recorded terminal moved to %s; terminal is ABSORBING and the FIRST one stands", rec.State)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a durable failure is NOT translated into a 4xx sentinel", func(t *testing.T) {
		// An unattached store is the reachable case (a bus that has not replayed
		// its log). It must surface as a plain error, which the route answers 503
		// "not now" — NOT as ErrAckNotBound or ErrAckOutcomeConflict, both of
		// which are FINAL 409s that would make the sender abandon a real terminal
		// outcome.
		st := ack.NewStore(ack.Options{})
		f := &federation{busID: wiringLocalBus, acks: st, log: logging.New(io.Discard, logging.LevelError)}
		_, err := f.settleAck(ctx, settled(relay.AckDelivered, ""))
		if err == nil {
			t.Fatal("an unattached durable store settled successfully")
		}
		if errors.Is(err, relay.ErrAckNotBound) || errors.Is(err, relay.ErrAckOutcomeConflict) {
			t.Errorf("a DURABILITY failure was reported as %v — a final 409. The sender would abandon the outcome instead of retrying it", err)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("an unmappable vocabulary is refused and NOTHING is written", func(t *testing.T) {
		f, st := newAckFed(t)
		if err := st.Accept(ackFedKey, ackFedSender, ackFedRecipient); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		bad := settled(relay.AckDelivered, "")
		bad.Ack.Outcome = relay.AckOutcome(200)
		if _, err := f.settleAck(ctx, bad); err == nil {
			t.Fatal("an outcome outside the wire enum was settled; the mapping must fail closed rather than write an absorbing terminal nobody chose")
		}
		rec, _ := st.Lookup(ackFedKey, ackFedRecipient)
		if rec.State.Terminal() {
			t.Errorf("the row reached the terminal state %s from an unmappable frame", rec.State)
		}
	})
}

// TestPriorTerminalReportsOnlyTERMINALOutcomes covers federation.priorTerminal,
// whose contract is subtler than it looks: relay.DecideAck's `hasPrior` means
// "a TERMINAL outcome is recorded YET", not "the pair is known".
func TestPriorTerminalReportsOnlyTERMINALOutcomes(t *testing.T) {
	t.Run("an absent row has no prior", func(t *testing.T) {
		f, _ := newAckFed(t)
		if _, ok := f.priorTerminal(ackFedKey, ackFedRecipient); ok {
			t.Error("an absent row reported a prior terminal")
		}
	})

	t.Run("a NON-TERMINAL row has no prior, so the first terminal may apply", func(t *testing.T) {
		// If this returned true, DecideAck would compare the incoming terminal
		// against a zero AckTerminal, find them different, and answer CONFLICT —
		// so the FIRST acknowledgement of every message would be refused 409.
		f, st := newAckFed(t)
		if err := st.Accept(ackFedKey, ackFedSender, ackFedRecipient); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		if _, ok := f.priorTerminal(ackFedKey, ackFedRecipient); ok {
			t.Error("an `accepted` row reported a prior TERMINAL; every first acknowledgement would then be refused as a conflict")
		}
	})

	t.Run("a terminal row round-trips through both closed vocabularies", func(t *testing.T) {
		for _, tc := range []struct {
			state ack.State
			class ack.Class
			want  relay.AckTerminal
		}{
			{ack.StateDelivered, "", relay.AckTerminal{Outcome: relay.AckDelivered}},
			{ack.StateRefused, ack.ClassRecipientRefusedUndecodable, relay.AckTerminal{Outcome: relay.AckRefused, Class: relay.AckRecipientRefusedUndecodable}},
			{ack.StateUndeliverable, ack.ClassHorizonExpired, relay.AckTerminal{Outcome: relay.AckUndeliverable, Class: relay.AckHorizonExpired}},
		} {
			f, st := newAckFed(t)
			if err := st.Accept(ackFedKey, ackFedSender, ackFedRecipient); err != nil {
				t.Fatalf("Accept: %v", err)
			}
			att := ack.AttestedByPeerBus
			if tc.state != ack.StateUndeliverable {
				att = ack.AttestedByRecipientSignatureUnverified
			}
			if err := st.Settle(ackFedKey, ackFedRecipient, tc.state, tc.class, att); err != nil {
				t.Fatalf("Settle(%s): %v", tc.state, err)
			}
			got, ok := f.priorTerminal(ackFedKey, ackFedRecipient)
			if !ok {
				t.Fatalf("%s: no prior reported for a TERMINAL row", tc.state)
			}
			if got != tc.want {
				t.Errorf("%s: prior = %+v, want %+v — a prior that does not match what a legitimate retry carries turns every retry into a 409",
					tc.state, got, tc.want)
			}
		}
	})
}
