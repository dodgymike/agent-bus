package relay

import (
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// ACK-4's acceptance evidence: the obligation binding rule (anti-forgery), the
// three idempotency cases with NO disconnect, and the privacy constraints — the
// uniform refusal that closes the status oracle, the closed no-free-text class
// vocabulary, and the redaction point.
//
// Every fixture here is prefixed ak* so it cannot collide with the ob* fixtures
// outbox_test.go owns or with the rest of the package's.

const (
	akLocalBus  = "bus-ack-local"
	akPeerBus   = "bus-ack-peer"
	akOtherPeer = "bus-ack-other-peer"
	akOriginBus = "bus-ack-origin"
	akThirdBus  = "bus-ack-third"

	akHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// akTable is an in-memory AckObligations. It exists so the binding rule can be
// driven against ADVERSARIAL tables — including one whose record disagrees with
// its own job id, which a real *Outbox refuses to hold — without a WAL.
// TestAckRejectsForgery/real_outbox proves the same rule against the production
// implementation.
type akTable map[string]OutboxRecord

func (t akTable) Lookup(jobID string) (OutboxRecord, bool) {
	r, ok := t[jobID]
	return r, ok
}

func akMessageID(t testing.TB, bus string, seq uint64) string {
	t.Helper()
	id, err := ids.MessageID(bus, seq)
	if err != nil {
		t.Fatalf("ids.MessageID(%s, %d): %v", bus, seq, err)
	}
	return id
}

func akAgentID(t testing.TB, bus, name string) string {
	t.Helper()
	id, err := ids.AgentID(bus, name, 1)
	if err != nil {
		t.Fatalf("ids.AgentID(%s, %s): %v", bus, name, err)
	}
	return id
}

// akObligation builds the durable record this bus would have written for one
// (peer, message) delivery, through the SAME DeriveJobID the production path
// uses — so a change to the job-id grammar breaks this fixture loudly rather
// than leaving it asserting about a shape nothing produces.
func akObligation(peerBus, originMessageID string) OutboxRecord {
	return OutboxRecord{
		JobID:           DeriveJobID(peerBus, originMessageID),
		PeerBusID:       peerBus,
		OriginMessageID: originMessageID,
		Size:            11,
		ContentSHA256:   akHash,
		State:           OutboxPending,
	}
}

func akSig() []byte { return bytes.Repeat([]byte{0xAB}, signing.SignatureSize) }

// ---------------------------------------------------------------------------
// TestAckRejectsForgery — layer 2, the anti-forgery core (§6.2)
// ---------------------------------------------------------------------------

// TestAckRejectsForgery drives the binding rule:
//
//	a peer-hop ACK/NACK from peer P is authoritative for correlation key K if
//	and only if DeriveJobID(P, K) names an outbox job THIS BUS DURABLY WROTE.
//
// Each negative case is one of the three attacks §6.2 names — reflection,
// cross-route forgery, third-party settlement — plus the retention case, and
// each must be refused with the SINGLE uniform error. The positive case is the
// multi-hop shape that a naive "the key's bus half must be the ACKing peer"
// rule would wrongly refuse; it is here so that rule cannot be reintroduced as
// a "hardening" without going red.
func TestAckRejectsForgery(t *testing.T) {
	t.Parallel()

	ours := akMessageID(t, akOriginBus, 7)
	recipient := akAgentID(t, akPeerBus, "callee")

	// The only obligation this bus wrote: we owe akPeerBus a copy of `ours`.
	table := akTable{DeriveJobID(akPeerBus, ours): akObligation(akPeerBus, ours)}

	t.Run("bound_peer_is_authorised", func(t *testing.T) {
		rec, err := AuthorizePeerAck(table, akLocalBus, akPeerBus, ours, recipient)
		if err != nil {
			t.Fatalf("AuthorizePeerAck for the peer we DID write a job to returned %v, want the bound obligation: this is the golden path and refusing it would break every acknowledgement", err)
		}
		if rec.JobID != DeriveJobID(akPeerBus, ours) {
			t.Fatalf("bound record has JobID %q, want %q", rec.JobID, DeriveJobID(akPeerBus, ours))
		}
	})

	t.Run("multi_hop_key_bus_half_is_not_the_acking_peer", func(t *testing.T) {
		// A->B->C: C's outcome reaches A via B. The bus half of the key is the
		// ORIGIN's, never the ACKing peer's. A rule comparing them would break
		// multi-hop (ACK-5), so the binding must succeed here.
		if ids.MaxMessageIDLen == 0 { // keeps the intent visible if the grammar changes
			t.Fatal("message id grammar changed")
		}
		busHalf, _, err := ids.ParseMessageID(ours)
		if err != nil {
			t.Fatalf("ParseMessageID(%q): %v", ours, err)
		}
		if busHalf == akPeerBus {
			t.Fatalf("fixture is vacuous: the correlation key's bus half (%q) equals the ACKing peer, so this case would pass under a wrong rule too", busHalf)
		}
		if _, err := AuthorizePeerAck(table, akLocalBus, akPeerBus, ours, recipient); err != nil {
			t.Fatalf("AuthorizePeerAck refused a key whose bus half is the ORIGIN rather than the ACKing peer: %v — that comparison is exactly the rule §6.2 forbids, because it breaks A->B->C back-propagation", err)
		}
	})

	forgeries := []struct {
		name          string
		peer          string
		correlation   string
		what          string
		table         akTable
		wantNotBound  bool
		wantInvalid   bool
		alsoNoRecord2 bool
	}{
		{
			name:         "reflection_peer_settles_an_obligation_it_was_never_given",
			peer:         akOtherPeer,
			correlation:  ours,
			what:         "a peer we never wrote a job to, settling a key we do hold for a DIFFERENT peer",
			table:        table,
			wantNotBound: true,
		},
		{
			name:         "cross_route_forgery_other_peer_settles_our_copy",
			peer:         akOtherPeer,
			correlation:  akMessageID(t, akOriginBus, 9),
			what:         "the copy that went via a different peer; DeriveJobID is keyed on the PEER",
			table:        table,
			wantNotBound: true,
		},
		{
			name:         "third_party_settlement_key_names_another_bus",
			peer:         akPeerBus,
			correlation:  akMessageID(t, akThirdBus, 7),
			what:         "a key whose bus half names a bus we never forwarded to this peer",
			table:        table,
			wantNotBound: true,
		},
		{
			name:         "unknown_key_entirely",
			peer:         akPeerBus,
			correlation:  akMessageID(t, akOriginBus, 99999),
			what:         "a key this bus has never seen",
			table:        table,
			wantNotBound: true,
		},
		{
			name:         "swept_obligation_is_not_reopened",
			peer:         akPeerBus,
			correlation:  ours,
			what:         "an obligation whose retention window closed; Lookup reports false and the row must not be reopened",
			table:        akTable{},
			wantNotBound: true,
		},
		{
			name:         "spliced_record_describes_a_different_job",
			peer:         akPeerBus,
			correlation:  ours,
			what:         "a table entry filed under our job id but describing another peer's job",
			table:        akTable{DeriveJobID(akPeerBus, ours): akObligation(akOtherPeer, ours)},
			wantNotBound: true,
		},
		{
			name:        "peer_claims_our_own_bus_id",
			peer:        akLocalBus,
			correlation: ours,
			what:        "a peer asserting THIS bus's namespace",
			table:       table,
			wantInvalid: true,
		},
		{
			name:        "correlation_key_is_not_a_message_id",
			peer:        akPeerBus,
			correlation: "not-a-message-id!!",
			what:        "a client-supplied key is INPUT TO VALIDATE, never an identity to trust (invariant 1)",
			table:       table,
			wantInvalid: true,
		},
	}

	for _, tc := range forgeries {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := AuthorizePeerAck(tc.table, akLocalBus, tc.peer, tc.correlation, recipient)
			if err == nil {
				t.Fatalf("AuthorizePeerAck ACCEPTED %s — that is %s", tc.name, tc.what)
			}
			if tc.wantNotBound && !errors.Is(err, ErrAckNotBound) {
				t.Fatalf("AuthorizePeerAck(%s) = %v, want ErrAckNotBound: %s", tc.name, err, tc.what)
			}
			if tc.wantInvalid && !errors.Is(err, ErrInvalidAckFrame) {
				t.Fatalf("AuthorizePeerAck(%s) = %v, want ErrInvalidAckFrame: %s", tc.name, err, tc.what)
			}
		})
	}

	t.Run("unqualified_recipient_is_refused", func(t *testing.T) {
		// Invariant 2: every agent id is fully qualified <bus-id>.<agent-id>.
		for _, bad := range []string{"", "callee", "callee-1", " " + recipient, recipient + " "} {
			if _, err := AuthorizePeerAck(table, akLocalBus, akPeerBus, ours, bad); !errors.Is(err, ErrInvalidAckFrame) {
				t.Fatalf("AuthorizePeerAck with recipient %q = %v, want ErrInvalidAckFrame: an unqualified or padded id names nobody (invariant 2)", bad, err)
			}
		}
	})

	t.Run("oversized_input_is_not_echoed", func(t *testing.T) {
		huge := strings.Repeat("A", 4096)
		_, err := AuthorizePeerAck(table, akLocalBus, akPeerBus, huge, recipient)
		if err == nil {
			t.Fatal("AuthorizePeerAck accepted a 4096-byte correlation key")
		}
		if strings.Contains(err.Error(), huge) {
			t.Fatalf("the error echoes the oversized peer-supplied key verbatim (%d bytes): a peer must not get to choose the size of the line we log about refusing it", len(err.Error()))
		}
	})

	t.Run("real_outbox_satisfies_the_interface", func(t *testing.T) {
		// The interface exists for adversarial fixtures, but the PRODUCTION
		// binding is computed over the outbox's existing durable record with no
		// new index — which is the whole reason §6.2 costs no new state. If
		// *Outbox ever stops satisfying AckObligations this fails to compile.
		ob, err := NewOutbox(OutboxOptions{BusID: akLocalBus, Durable: &obNullDurable{}})
		if err != nil {
			t.Fatalf("NewOutbox: %v", err)
		}
		var obligations AckObligations = ob
		if _, err := AuthorizePeerAck(obligations, akLocalBus, akPeerBus, ours, recipient); !errors.Is(err, ErrAckNotBound) {
			t.Fatalf("an empty production outbox bound an acknowledgement: got %v, want ErrAckNotBound", err)
		}
		job := OutboxJob{PeerBusID: akPeerBus, OriginMessageID: ours, Size: 11, ContentSHA256: akHash}
		if _, err := ob.Enqueue(job); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if _, err := AuthorizePeerAck(obligations, akLocalBus, akPeerBus, ours, recipient); err != nil {
			t.Fatalf("after this bus durably wrote the job, the peer's acknowledgement was refused: %v", err)
		}
		if _, err := AuthorizePeerAck(obligations, akLocalBus, akOtherPeer, ours, recipient); !errors.Is(err, ErrAckNotBound) {
			t.Fatalf("a DIFFERENT peer settled the job we wrote for %s: got %v, want ErrAckNotBound", akPeerBus, err)
		}
	})

	t.Run("nil_table_is_not_a_silent_open_door", func(t *testing.T) {
		_, err := AuthorizePeerAck(nil, akLocalBus, akPeerBus, ours, recipient)
		if err == nil {
			t.Fatal("AuthorizePeerAck with no obligation table returned nil: a missing table must never authorise")
		}
		if errors.Is(err, ErrAckNotBound) {
			t.Fatal("a nil obligation table answered the UNIFORM peer refusal: that is a wiring fault on our side and must be distinguishable in our own logs, and no peer can provoke it")
		}
	})
}

// ---------------------------------------------------------------------------
// TestAckDirectArmBindsRecipientHomeBus — ACK-4-FU-RECIPIENT-BINDING
// ---------------------------------------------------------------------------

// TestAckDirectArmBindsRecipientHomeBus pins the recipient conjunct that
// ACK-4-FU-RECIPIENT-BINDING adds to the DIRECT arm: a peer P legitimately
// holding an obligation for key K may settle ONLY a recipient whose home bus is
// P, never a SIBLING recipient of the SAME K whose home bus is a DIFFERENT bus.
//
// Why the old (peer, key)-only direct arm was a forgery the moment a key has
// more than one recipient row: the outbox job is keyed on the recipient's HOME
// bus (Forwarder.targets -> Registry.Route(recipient)), so DeriveJobID(P, K) is
// the job for recipients whose home bus is P. Before this binding a peer bound
// for its OWN recipient of K could name any sibling recipient of K and the
// direct arm authorised it on the (peer, key) job alone; SettleAck then found
// that sibling's row and — terminal being ABSORBING — burned an outcome the peer
// was never routed, uncorrectably. Invariant 2 makes the fix computable: R is
// fully qualified, so its bus half names its home bus, and the rule is
// EqualFold(homeBus(R), P), the same case-folded comparison the indirect arm
// already makes.
//
// The refusal is the UNIFORM ErrAckNotBound and NEVER a disconnect (invariant
// 10: an ACK frame is not a signed message, and this link carries a whole
// roster's traffic).
func TestAckDirectArmBindsRecipientHomeBus(t *testing.T) {
	t.Parallel()

	ours := akMessageID(t, akOriginBus, 7)
	// This bus wrote exactly one obligation: a copy of `ours` owed to akPeerBus,
	// the home bus of every recipient it delivers for.
	table := akTable{DeriveJobID(akPeerBus, ours): akObligation(akPeerBus, ours)}

	onPeer := akAgentID(t, akPeerBus, "callee")    // home bus == the acking peer
	sibling := akAgentID(t, akThirdBus, "sibling") // home bus is a DIFFERENT bus

	if _, _, _, err := ids.ParseAgentID(sibling); err != nil {
		t.Fatalf("fixture sibling id is malformed, so this proves nothing: %v", err)
	}

	t.Run("recipient_on_the_acking_peer_is_authorised", func(t *testing.T) {
		rec, err := AuthorizePeerAck(table, akLocalBus, akPeerBus, ours, onPeer)
		if err != nil {
			t.Fatalf("AuthorizePeerAck refused the peer settling a recipient that lives ON it: %v — this is the golden single-hop path and refusing it would break every direct acknowledgement", err)
		}
		if rec.JobID != DeriveJobID(akPeerBus, ours) {
			t.Fatalf("bound record has JobID %q, want %q", rec.JobID, DeriveJobID(akPeerBus, ours))
		}
		// The binding rule is a pure read: a legitimate retry of the SAME
		// (peer, key, recipient) returns the identical answer, no error and no
		// state change — the authorization half of invariant 10's first case,
		// and never a disconnect.
		rec2, err2 := AuthorizePeerAck(table, akLocalBus, akPeerBus, ours, onPeer)
		if err2 != nil || rec2.JobID != rec.JobID {
			t.Fatalf("a retry of the SAME (peer, key, recipient) must return the ORIGINAL binding: got (%q, %v)", rec2.JobID, err2)
		}
	})

	t.Run("cross_recipient_forgery_on_the_direct_arm_is_REFUSED", func(t *testing.T) {
		// akPeerBus is legitimately bound for `ours` (it holds the obligation),
		// then names a SIBLING recipient of the same key whose home bus is
		// akThirdBus — a recipient this bus routed through a DIFFERENT peer.
		// BEFORE ACK-4-FU-RECIPIENT-BINDING this returned nil: THE VULNERABILITY.
		_, err := AuthorizePeerAck(table, akLocalBus, akPeerBus, ours, sibling)
		if err == nil {
			t.Fatal("AuthorizePeerAck AUTHORISED a peer to settle a sibling recipient whose home bus it is not — the cross-recipient/cross-peer forgery ACK-4-FU-RECIPIENT-BINDING closes; terminal is ABSORBING so the wrong outcome could never be corrected")
		}
		if !errors.Is(err, ErrAckNotBound) {
			t.Fatalf("the forgery must be refused with the UNIFORM ErrAckNotBound — no distinguishable oracle, no disconnect (invariant 10): got %v", err)
		}
	})

	t.Run("forgery_is_refused_end_to_end_through_AuthorizePeerAckVia", func(t *testing.T) {
		// The one production call site is AuthorizePeerAckVia. With a routing
		// table where the sibling's home bus and the acking peer dial to
		// DIFFERENT addresses, the indirect arm cannot rescue the forgery.
		nextHop := func(busID string) (string, bool) {
			switch busID {
			case akPeerBus:
				return "https://peer.example:8443", true
			case akThirdBus:
				return "https://third.example:8443", true
			default:
				return "", false
			}
		}
		_, err := AuthorizePeerAckVia(table, nextHop, akLocalBus, akPeerBus, ours, sibling)
		if !errors.Is(err, ErrAckNotBound) {
			t.Fatalf("the full authorisation path authorised the cross-recipient forgery: got %v, want ErrAckNotBound", err)
		}
	})
}

// ---------------------------------------------------------------------------
// TestAckRejectsReplay — invariant 10's three cases, and NO DISCONNECT (§12)
// ---------------------------------------------------------------------------

// TestAckRejectsReplay pins invariant 10's cases on the ACK plane and pins the
// absence of a disconnect three ways: the decision enum has no such member, the
// implementation file contains no disconnect mechanism, and no refusal path
// returns anything a caller could act on as one.
func TestAckRejectsReplay(t *testing.T) {
	t.Parallel()

	delivered := AckTerminal{Outcome: AckDelivered}
	refusedPolicy := AckTerminal{Outcome: AckRefused, Class: AckRecipientRefusedPolicy}
	refusedUndecodable := AckTerminal{Outcome: AckRefused, Class: AckRecipientRefusedUndecodable}
	undeliverable := AckTerminal{Outcome: AckUndeliverable, Class: AckHorizonExpired}

	cases := []struct {
		name     string
		prior    AckTerminal
		hasPrior bool
		incoming AckTerminal
		want     AckDecision
		wantErr  error
		why      string
	}{
		{
			name: "first_terminal_applies", hasPrior: false, incoming: delivered,
			want: AckApply, why: "nothing recorded yet",
		},
		{
			name: "same_outcome_is_a_legitimate_retry", prior: delivered, hasPrior: true, incoming: delivered,
			want: AckReplay,
			why:  "invariant 10 case 1: return the ORIGINAL result, re-apply nothing, do not error, do not disconnect",
		},
		{
			name: "same_negative_outcome_and_class_is_a_retry", prior: refusedPolicy, hasPrior: true, incoming: refusedPolicy,
			want: AckReplay, why: "a peer re-sending because our 200 was lost in flight is honest",
		},
		{
			name: "different_terminal_outcome_is_a_violation", prior: delivered, hasPrior: true, incoming: refusedPolicy,
			want: AckConflict, wantErr: ErrAckOutcomeConflict,
			why: "invariant 10 case 2: reject and LOG. Terminal is absorbing and is never downgraded",
		},
		{
			name: "same_outcome_different_class_is_a_violation", prior: refusedPolicy, hasPrior: true, incoming: refusedUndecodable,
			want: AckConflict, wantErr: ErrAckOutcomeConflict,
			why: "the class is part of the terminal outcome; silently keeping the first would hide a conflicting claim",
		},
		{
			name: "positive_never_downgrades_to_undeliverable", prior: delivered, hasPrior: true, incoming: undeliverable,
			want: AckConflict, wantErr: ErrAckOutcomeConflict,
			why: "a late routing failure must not overwrite a recipient's delivery. §8.2 is AMBIGUOUS here and the reading is deliberate: the delivered/E4 cell says IGNORE, but E4 is a HOP settlement (OutboxAbandoned), which never reaches DecideAck; a TERMINAL ACK FRAME carrying a different outcome is E8, which says violation. See the report's flagged gaps",
		},
		{
			name: "unrecognised_class_is_rejected_never_defaulted", hasPrior: false,
			incoming: AckTerminal{Outcome: AckRefused, Class: AckClass("not-a-class")},
			wantErr:  ErrInvalidAckFrame,
			why:      "parseOutboxState's posture: guessing turns a corrupt frame into a plausible TERMINAL outcome",
		},
		{
			name: "bus_class_on_a_recipient_outcome_is_rejected", hasPrior: false,
			incoming: AckTerminal{Outcome: AckRefused, Class: AckNoRoute},
			wantErr:  ErrInvalidAckFrame,
			why:      "a routing failure dressed up as a RECIPIENT'S decision; the two are asserted by different parties",
		},
		{
			name: "recipient_class_on_a_bus_outcome_is_rejected", hasPrior: false,
			incoming: AckTerminal{Outcome: AckUndeliverable, Class: AckRecipientRefusedPolicy},
			wantErr:  ErrInvalidAckFrame,
			why:      "a recipient's policy refusal attributed to the federation",
		},
		{
			name: "positive_outcome_may_not_carry_a_class", hasPrior: false,
			incoming: AckTerminal{Outcome: AckDelivered, Class: AckRecipientRefusedPolicy},
			wantErr:  ErrInvalidAckFrame,
			why:      "§5.4: a success explains nothing, and an optional class on it is a disclosure channel nobody needs",
		},
		{
			name: "negative_outcome_must_carry_a_class", hasPrior: false,
			incoming: AckTerminal{Outcome: AckRefused},
			wantErr:  ErrInvalidAckFrame,
			why:      "a NACK that cannot say which closed class it is, is a silent discard with a timestamp",
		},
		// THE TWO CASES BELOW ARE THE `undeliverable` HALF OF THE TWO ROWS ABOVE.
		// ADDED BY ACK-13's MUTATION RE-PROOF, BECAUSE WITHOUT THEM THE TABLE
		// COULD NOT OBSERVE `!class.Valid()` FIRING AT ALL.
		//
		// ValidateAckClassForOutcome ends in four checks, and on a `refused`
		// frame the LAST two subsume the first two: the empty string and an
		// unrecognised spelling are both non-members of the recipient half, so
		// `outcome == AckRefused && !class.RecipientEmitted()` refuses them
		// whatever `class == ackNoClass` and `!class.Valid()` do. Deleting
		// `!class.Valid()` therefore left this whole table GREEN.
		//
		// On `undeliverable` the subsumption runs the other way — the half-set
		// check is `class.RecipientEmitted()`, which answers FALSE for "" and
		// FALSE for garbage alike — so nothing downstream catches either, and
		// with these rows present the deletion goes red. That matters most for
		// the second row: without `!class.Valid()` a peer chooses ARBITRARY
		// BYTES for the class of an absorbing terminal this bus records.
		//
		// `class == ackNoClass` is a separate story and is recorded here rather
		// than papered over: deleting it ALONE is still green on both halves,
		// because `!class.Valid()` refuses "" too. It is behaviourally
		// redundant and survives only to name the fault ("REQUIRES a class")
		// instead of the generic "not one of the twelve". Deleting BOTH is red
		// on both rows below, which is what pins the pairing rule itself.
		//
		// All of this is a consequence of ACK-13 aliasing AckClass to
		// ack.Class: "" is now both the zero value AND what a frame that
		// OMITTED the field decodes to, and any byte sequence is representable
		// where AckClass(200) used to be the only shape a non-member took.
		{
			name: "bus_outcome_must_carry_a_class", hasPrior: false,
			incoming: AckTerminal{Outcome: AckUndeliverable},
			wantErr:  ErrInvalidAckFrame,
			why:      "the `undeliverable` half of `negative_outcome_must_carry_a_class`, and the ONLY case in which the `class == ackNoClass` check is observable: a routing failure that names no class is a silent discard with a timestamp, and the empty string is what an OMITTED field decodes to",
		},
		{
			name: "unrecognised_class_on_a_bus_outcome_is_rejected", hasPrior: false,
			incoming: AckTerminal{Outcome: AckUndeliverable, Class: AckClass("not-a-class")},
			wantErr:  ErrInvalidAckFrame,
			why:      "the `undeliverable` half of `unrecognised_class_is_rejected_never_defaulted`, and the ONLY case in which the `!class.Valid()` check is observable: without it a peer chooses arbitrary bytes for the class of an ABSORBING terminal this bus records",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecideAck(tc.prior, tc.hasPrior, tc.incoming)
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("DecideAck = (%v, %v), want error %v: %s", got, err, tc.wantErr, tc.why)
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("DecideAck = (%v, %v), want no error: %s", got, err, tc.why)
			}
			if tc.want != 0 && got != tc.want {
				t.Fatalf("DecideAck = %v, want %v: %s", got, tc.want, tc.why)
			}
		})
	}

	t.Run("a_retry_never_errors", func(t *testing.T) {
		// The whole point of idempotency is that a well-behaved client can
		// safely retry. Erroring here would break exactly the clients doing the
		// right thing.
		if d, err := DecideAck(refusedPolicy, true, refusedPolicy); err != nil || d != AckReplay {
			t.Fatalf("DecideAck for an identical retry = (%v, %v), want (replay, nil)", d, err)
		}
	})

	t.Run("decision_enum_has_no_disconnect_member", func(t *testing.T) {
		// The STRUCTURAL half of "no new disconnect": a caller cannot act on a
		// decision the type cannot express. If somebody adds one, this fails.
		want := map[string]bool{"apply": true, "replay": true, "conflict": true}
		if int(ackDecisionCount)-1 != len(want) {
			t.Fatalf("AckDecision has %d members, want exactly %d; a new one needs invariant 10's two questions answered on the record first", int(ackDecisionCount)-1, len(want))
		}
		for d := AckDecision(1); d < ackDecisionCount; d++ {
			if !want[d.String()] {
				t.Fatalf("AckDecision(%d) spells %q, which is not one of apply/replay/conflict", uint8(d), d.String())
			}
			if strings.Contains(strings.ToLower(d.String()), "disconnect") ||
				strings.Contains(strings.ToLower(d.String()), "close") ||
				strings.Contains(strings.ToLower(d.String()), "drop") {
				t.Fatalf("AckDecision(%d) spells %q: NO ACK-plane refusal may drop a connection — a peer link multiplexes an entire bus's roster", uint8(d), d.String())
			}
		}
	})

	t.Run("implementation_carries_no_disconnect_mechanism", func(t *testing.T) {
		// The source-level half. net/http closes a socket when a handler sets
		// "Connection: close" (httpapi.disconnect), so the ACK plane's own file
		// must contain no such thing and must call no helper that does.
		src := akReadSource(t, "ack.go")
		for _, banned := range []string{`"Connection"`, "Connection: close", "disconnect(w", "Hijack(", "CloseNotify"} {
			if akCodeContains(t, "ack.go", banned) {
				t.Fatalf("internal/relay/ack.go contains %q in CODE: §12 rules that NO new disconnect is introduced anywhere in the ACK plane. A peer bus relays for every agent behind it, so dropping the socket over one frame punishes every bystander.", banned)
			}
		}
		// And the reasoning must stay in the file, because the next reader's
		// first instinct on a forged frame is to drop the connection.
		for _, must := range []string{"NO DISCONNECT MEMBER", "BUGGY CLIENT", "ONE PRINCIPAL'S TRAFFIC"} {
			if !strings.Contains(src, must) {
				t.Fatalf("internal/relay/ack.go no longer records %q: invariant 10 requires both questions answered ON THE RECORD before any disconnect is added, and deleting the answers is how one gets added by accident", must)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// TestAckDoesNotLeakRecipientState — the privacy half (§5, §13.3)
// ---------------------------------------------------------------------------

// TestAckDoesNotLeakRecipientState pins what an ACK/NACK discloses and to whom.
//
// The threat is an ORACLE. A peered bus, or any authenticated agent, must not
// be able to use the ACK plane to learn whether a given agent exists, is
// enrolled, is online, or received something — nor to learn the size of another
// bus's roster.
func TestAckDoesNotLeakRecipientState(t *testing.T) {
	t.Parallel()

	ours := akMessageID(t, akOriginBus, 7)
	recipient := akAgentID(t, akPeerBus, "callee")
	table := akTable{DeriveJobID(akPeerBus, ours): akObligation(akPeerBus, ours)}

	t.Run("every_unbound_cause_gives_a_BYTE_IDENTICAL_answer", func(t *testing.T) {
		// THIS IS THE ORACLE-CLOSING ASSERTION. Four causes that a peer could
		// otherwise tell apart — and telling them apart is exactly how you probe
		// "did bus A send message K to bus B", and from there whether a named
		// agent exists and is being written to.
		probes := []struct{ name, peer, key string }{
			{"no obligation for this peer", akOtherPeer, ours},
			{"obligation exists but for another peer", akOtherPeer, akMessageID(t, akOriginBus, 9)},
			{"key names a bus we never forwarded to", akPeerBus, akMessageID(t, akThirdBus, 7)},
			{"key never existed at all", akPeerBus, akMessageID(t, akOriginBus, 424242)},
		}
		var first string
		for i, p := range probes {
			_, err := AuthorizePeerAck(table, akLocalBus, p.peer, p.key, recipient)
			if err == nil {
				t.Fatalf("probe %q was ACCEPTED", p.name)
			}
			if err != error(ErrAckNotBound) {
				t.Fatalf("probe %q returned %#v, want the UNIFORM sentinel ErrAckNotBound BY IDENTITY: a wrapped or formatted error carries the cause, and the cause is the oracle", p.name, err)
			}
			if i == 0 {
				first = err.Error()
				continue
			}
			if err.Error() != first {
				t.Fatalf("probe %q answers %q but probe 0 answers %q: the refusal text must be byte-identical across every cause, or the difference IS the oracle", p.name, err.Error(), first)
			}
		}
		if strings.Contains(first, akPeerBus) || strings.Contains(first, ours) || strings.Contains(first, recipient) {
			t.Fatalf("the uniform refusal %q echoes an id from the request: it must describe nothing about what this bus does or does not hold", first)
		}
	})

	t.Run("unbound_and_conflicting_share_one_wire_code", func(t *testing.T) {
		// "I have no obligation for you" and "this already settled differently"
		// must be indistinguishable on the wire for the same reason.
		if got, want := ErrorCode(ErrAckNotBound), CodeIdempotencyViolation; got != want {
			t.Fatalf("ErrorCode(ErrAckNotBound) = %q, want %q", got, want)
		}
		if got, want := ErrorCode(fmt.Errorf("wrapped: %w", ErrAckOutcomeConflict)), CodeIdempotencyViolation; got != want {
			t.Fatalf("ErrorCode(ErrAckOutcomeConflict) = %q, want %q", got, want)
		}
		if got, want := ErrorCode(fmt.Errorf("wrapped: %w", ErrInvalidAckFrame)), CodeInvalidRequest; got != want {
			t.Fatalf("ErrorCode(ErrInvalidAckFrame) = %q, want %q: a malformed field is decidable by the sender from its own bytes, so it leaks nothing and must stay debuggable", got, want)
		}
	})

	t.Run("status_row_is_visible_only_to_the_original_sender", func(t *testing.T) {
		sender := akAgentID(t, akLocalBus, "sender")
		stranger := akAgentID(t, akLocalBus, "stranger")
		cases := []struct {
			name                 string
			recordSender, caller string
			found                bool
			want                 bool
		}{
			{"the sender sees its own row", sender, sender, true, true},
			{"a stranger sees unknown", sender, stranger, true, false},
			{"a swept row is unknown", sender, sender, false, false},
			{"a row that never existed is unknown", "", sender, false, false},
			{"an empty caller is not a wildcard", sender, "", true, false},
			{"an unattributed row is shown to nobody", "", "", true, false},
		}
		for _, tc := range cases {
			if got := AckStatusVisible(tc.recordSender, tc.caller, tc.found); got != tc.want {
				t.Fatalf("AckStatusVisible(%q, %q, %v) = %v, want %v (%s): every non-sender case must be answered 200 %q, because a 403 would confirm the message exists",
					tc.recordSender, tc.caller, tc.found, got, tc.want, tc.name, AckStatusUnknown)
			}
		}
	})

	t.Run("no_free_text_anywhere_in_the_vocabulary", func(t *testing.T) {
		// Invariant 6: a recipient-sourced reason string is a body by another
		// name. There must be exactly twelve compile-time classes and no
		// adjacent string field to put "detail" in.
		// ACK-13: the set is ack's, ranged over rather than re-declared here.
		if got, want := len(ack.AllClasses()), 12; got != want {
			t.Fatalf("the NACK class set has %d members, want exactly %d: it is CLOSED, and a thirteenth needs the §5.2 reasoning redone", got, want)
		}
		seen := map[string]bool{}
		recipientEmitted := 0
		for _, c := range ack.AllClasses() {
			s := c.String()
			if seen[s] {
				t.Fatalf("class spelling %q appears twice", s)
			}
			seen[s] = true
			if strings.ContainsAny(s, " \t\n\"%") || strings.HasPrefix(s, "invalid-class(") {
				t.Fatalf("class %q spells %q: a class is a fixed constant, never assembled, templated or concatenated", string(c), s)
			}
			parsed, err := ParseAckClass(s)
			if err != nil || parsed != c {
				t.Fatalf("ParseAckClass(%q) = (%v, %v), want (%v, nil): the round trip is what keeps the durable spelling and the constant from drifting", s, parsed, err, c)
			}
			if c.RecipientEmitted() {
				recipientEmitted++
			}
		}
		if recipientEmitted != 3 {
			t.Fatalf("%d classes are recipient-emitted, want exactly 3: each says THAT something failed and never WHAT, which is the line invariant 6 draws", recipientEmitted)
		}
		for _, unknown := range []string{"", "custom", "no_route ", "NO_ROUTE", "recipient_refused_because_the_body_was_ugly"} {
			if _, err := ParseAckClass(unknown); !errors.Is(err, ErrInvalidAckFrame) {
				t.Fatalf("ParseAckClass(%q) accepted an unrecognised class: it must be REJECTED, never defaulted", unknown)
			}
		}
	})

	t.Run("attestation_has_no_value_meaning_verified", func(t *testing.T) {
		// Nothing in this system can verify a recipient attestation: the bus
		// checks SHAPE only and no endpoint distributes messaging public keys.
		// A "verified" label would be the status API asserting a fact nobody
		// established.
		if got, want := len(ack.AllAttestations()), 2; got != want {
			t.Fatalf("AckAttestation has %d values, want exactly %d (peer_bus, recipient_signature_unverified)", got, want)
		}
		for _, claim := range []string{"verified", "recipient_signature_verified", "signature_verified", "trusted"} {
			if _, err := ParseAckAttestation(claim); err == nil {
				t.Fatalf("ParseAckAttestation(%q) succeeded: there is deliberately NO value meaning verified, because nothing can produce one", claim)
			}
			for _, a := range ack.AllAttestations() {
				if a.String() == claim {
					t.Fatalf("attestation %q spells %q", string(a), claim)
				}
			}
		}
		// Shape only, and the shape is required exactly where a recipient is in
		// the story and forbidden where one is not.
		if got, err := ValidateAckAttestation(AckSurfacePeer, AckRefused, akSig()); err != nil || got != AckAttestedRecipientSignatureUnverified {
			t.Fatalf("ValidateAckAttestation(peer, refused, 64 bytes) = (%v, %v), want (recipient_signature_unverified, nil)", got, err)
		}
		if got, err := ValidateAckAttestation(AckSurfacePeer, AckUndeliverable, nil); err != nil || got != AckAttestedPeerBus {
			t.Fatalf("ValidateAckAttestation(peer, undeliverable, nil) = (%v, %v), want (peer_bus, nil)", got, err)
		}
		if _, err := ValidateAckAttestation(AckSurfacePeer, AckDelivered, akSig()[:63]); !errors.Is(err, ErrInvalidAckFrame) {
			t.Fatalf("a 63-byte attestation was accepted: shape is the ONLY check available, so it must be exact")
		}
		if _, err := ValidateAckAttestation(AckSurfacePeer, AckUndeliverable, akSig()); !errors.Is(err, ErrInvalidAckFrame) {
			t.Fatalf("a bus-asserted outcome carried 64 unexplained bytes: there is no recipient in that story to have signed anything, and no field whose length a remote party chooses (§5.3)")
		}
	})

	t.Run("an_agent_cannot_earn_a_peer_bus_attestation", func(t *testing.T) {
		// The label must reflect WHICH GATE authenticated the frame, and that is
		// knowable only from the surface. Deriving it from the outcome alone let
		// an agent-surface frame be recorded as attested by an adjacent BUS —
		// this bus telling a sender that a peer vouched for what an agent said.
		if got, err := ValidateAckAttestation(AckSurfaceAgent, AckUndeliverable, nil); err == nil {
			t.Fatalf("an agent asserted the ROUTING outcome undeliverable and was labelled %v: a recipient application has no standing to make a claim about the federation's routing, and §6.1's one-factor narrowing is per-surface and must not be widened", got)
		}
		if got, err := ValidateAckAttestation(AckSurfaceAgent, AckRefused, akSig()); err != nil || got != AckAttestedRecipientSignatureUnverified {
			t.Fatalf("ValidateAckAttestation(agent, refused, 64 bytes) = (%v, %v), want (recipient_signature_unverified, nil): plane C is exactly what the agent surface is for", got, err)
		}
		for _, bad := range []AckSurface{0, ackSurfaceCount, AckSurface(200)} {
			if _, err := ValidateAckAttestation(bad, AckRefused, akSig()); !errors.Is(err, ErrInvalidAckFrame) {
				t.Fatalf("surface %v was accepted: the mount site must NAME which gate authenticated the frame; an unset surface must never fall through to a label", bad)
			}
		}
		if got, want := int(ackSurfaceCount)-1, 2; got != want {
			t.Fatalf("AckSurface has %d members, want exactly %d (peer, agent)", got, want)
		}
	})

	t.Run("outcome_enum_is_closed_and_zero_is_not_a_positive_terminal", func(t *testing.T) {
		// THE REGRESSION THIS PINS: Negative() and RecipientSourced() both answer
		// FALSE outside the enum, so an unchecked zero or out-of-range outcome
		// fell through the POSITIVE arm and was accepted. A never-populated
		// struct, or a conversion from ACK-2's parallel vocabulary that returned
		// a zero value on an unmapped input, would then have been recorded as a
		// valid delivered-shaped ABSORBING terminal that can never be corrected.
		// ACK-13 aliased AckOutcome to ack.State, which has FIVE members. The
		// three a FRAME may carry are the terminal ones, and the two
		// non-terminal states are checked below to be rejected by name.
		if got, want := len(ack.AllTerminalStates()), 3; got != want {
			t.Fatalf("AckOutcome has %d terminal members, want exactly %d (delivered, refused, undeliverable)", got, want)
		}
		for _, o := range ack.AllTerminalStates() {
			parsed, err := ParseAckOutcome(o.String())
			if err != nil || parsed != o {
				t.Fatalf("ParseAckOutcome(%q) = (%v, %v), want (%v, nil)", o.String(), parsed, err, o)
			}
		}
		for _, unknown := range []string{"", "unknown", "accepted", "in_flight", "DELIVERED", "delivered "} {
			if _, err := ParseAckOutcome(unknown); !errors.Is(err, ErrInvalidAckFrame) {
				t.Fatalf("ParseAckOutcome(%q) accepted an unrecognised outcome: it must be REJECTED, never defaulted — and %q in particular is a REPORTING value that must never travel as a frame outcome", unknown, AckStatusUnknown)
			}
		}
		// The non-terminal states are in this list because the alias makes them
		// REPRESENTABLE: !Terminal() is what refuses them, and it is stricter
		// than the numeric bound it replaced, not looser.
		for _, bad := range []AckOutcome{0, ack.StateAccepted, ack.StateInFlight, AckOutcome(99)} {
			if err := ValidateAckClassForOutcome(bad, ""); !errors.Is(err, ErrInvalidAckFrame) {
				t.Fatalf("ValidateAckClassForOutcome(%v, no class) = %v, want ErrInvalidAckFrame: an out-of-range outcome answers false to Negative() and would be waved through as a POSITIVE terminal", bad, err)
			}
			if _, err := ValidateAckAttestation(AckSurfacePeer, bad, nil); !errors.Is(err, ErrInvalidAckFrame) {
				t.Fatalf("ValidateAckAttestation(peer, %v, nil) = %v, want ErrInvalidAckFrame: it would otherwise be labelled peer_bus — this bus attesting a frame it could not classify", bad, err)
			}
			if d, err := DecideAck(AckTerminal{}, false, AckTerminal{Outcome: bad}); err == nil {
				t.Fatalf("DecideAck for out-of-range outcome %v returned %v with no error: %v writes an ABSORBING terminal that is never revisited", bad, d, d)
			}
		}
		// The zero VALUE of the struct is the most likely way this arrives.
		if d, err := DecideAck(AckTerminal{}, false, AckTerminal{}); err == nil {
			t.Fatalf("DecideAck on a zero-value AckTerminal returned (%v, nil): a never-populated struct must not decide anything", d)
		}
	})

	t.Run("recipient_on_a_different_bus_is_bound_out_the_uniform_way", func(t *testing.T) {
		// FORMERLY the documented-gap test that PINNED the missing recipient
		// binding (ACK-4-FU-RECIPIENT-BINDING). The gap is now CLOSED: the direct
		// arm binds the recipient's HOME bus to the acking peer, so a peer holding
		// an obligation for K may no longer settle a sibling recipient whose home
		// bus is a different bus. It must be refused with the SAME uniform
		// ErrAckNotBound as every other unbound cause — telling this apart from
		// "no such key" would reopen the oracle this test's neighbours protect.
		// The forgery reproduction and the golden positive live in
		// TestAckDirectArmBindsRecipientHomeBus; this asserts the REFUSAL stays on
		// the uniform-answer path here in the leak test.
		stranger := akAgentID(t, akThirdBus, "never-addressed")
		if _, err := AuthorizePeerAck(table, akLocalBus, akPeerBus, ours, stranger); !errors.Is(err, ErrAckNotBound) {
			t.Fatalf("a peer settling a recipient whose home bus is a DIFFERENT bus got %v, want the uniform ErrAckNotBound: the recipient binding must refuse on the SAME path as every other unbound cause", err)
		}
	})

	t.Run("refusal_log_fields_redact_and_disclose_no_recipient_state", func(t *testing.T) {
		huge := strings.Repeat("Z", 4096)
		fields := AckRefusalLogFields(huge, huge, huge)
		if len(fields)%2 != 0 {
			t.Fatalf("AckRefusalLogFields returned %d values, want key/value pairs", len(fields))
		}
		for i := 1; i < len(fields); i += 2 {
			s, ok := fields[i].(string)
			if !ok {
				continue
			}
			if len(s) > 4*maxElidedAckChars {
				t.Fatalf("log value %d is %d bytes: a peer must not choose the size of our log line (the MaxOutboxReasonLen discipline)", i, len(s))
			}
			if s == huge {
				t.Fatalf("log value %d echoes 4096 peer-chosen bytes verbatim", i)
			}
		}
		// No parameter exists through which free text could be smuggled in: the
		// signature takes three ids and nothing else. If a fourth "reason"
		// argument ever appears, this stops compiling.
		var _ func(string, string, string) []interface{} = AckRefusalLogFields

		// And the job id — the field that exists to BE GREPPED — must survive
		// intact for ordinary VALID ids. Eliding it at maxElidedAckChars cut a
		// legitimate job id in half while the untruncated halves sat beside it
		// on the same line, which is the opposite of what the field is for.
		// THE FIXTURE MUST REACH THE BOUNDARY, OR IT PROVES NOTHING. Short ids
		// (akPeerBus + akOriginBus derive a 30-character job id) fit under
		// maxElidedAckChars, so they cannot tell a correctly-bounded job id from
		// an elided one and this assertion passed either way — the mutation run
		// caught that. A 43-character bus id was the second version, and it was
		// ALSO too short: the trap is that the two halves have DIFFERENT limits
		// (MaxPeerBusIDLen 64, ids.MaxMessageIDLen 85), so only a key longer
		// than maxElidedAckChars (64) exercises the case where the key half is
		// legitimate but would be cut by display elision.
		realPeer := strings.Repeat("p", MaxPeerBusIDLen)
		realKey := akMessageID(t, strings.Repeat("o", 60), 1234567)
		if err := ids.ValidateBusID(realPeer); err != nil {
			t.Fatalf("fixture peer bus id is not valid, so this proves nothing: %v", err)
		}
		if len(realKey) <= maxElidedAckChars {
			t.Fatalf("fixture correlation key is %d bytes, which is <= maxElidedAckChars (%d): display elision would not touch it, so this assertion could not fail and would be vacuous", len(realKey), maxElidedAckChars)
		}
		if len(realKey) > ids.MaxMessageIDLen {
			t.Fatalf("fixture correlation key is %d bytes, beyond the %d a real message id can be — it is not a legitimate id and proves the wrong thing", len(realKey), ids.MaxMessageIDLen)
		}
		want := DeriveJobID(realPeer, realKey)
		got := AckRefusalLogFields(realPeer, realKey, akAgentID(t, akPeerBus, "callee"))
		var jobID string
		for i := 0; i+1 < len(got); i += 2 {
			if got[i] == "job_id" {
				jobID, _ = got[i+1].(string)
			}
		}
		if jobID != want {
			t.Fatalf("job_id logged as %q, want the whole %q: an operator greps the WAL for this exact string, so a truncated one names nothing", jobID, want)
		}
		if len(want) > MaxOutboxJobIDLen {
			t.Fatalf("fixture is not a valid job id: %d bytes exceeds MaxOutboxJobIDLen %d", len(want), MaxOutboxJobIDLen)
		}
	})

	t.Run("no_aggregate_or_roster_sized_answer_exists", func(t *testing.T) {
		// §5.5: any roll-up ("3 of 5 delivered") is a ROSTER-SIZE ORACLE. The
		// per-recipient shape must not acquire a count, a quorum or a total.
		src := akReadSource(t, "ack.go")
		for _, banned := range []string{"Quorum", "RollUp", "Rollup", "TotalRecipients", "DeliveredCount"} {
			if strings.Contains(src, banned) {
				t.Fatalf("internal/relay/ack.go mentions %q: an aggregate discloses bus membership to any sender, and §5.5 fixes the shape as per-recipient with no roll-up and no quorum", banned)
			}
		}
		if !strings.Contains(src, "ROSTER-SIZE ORACLE") {
			t.Fatal("internal/relay/ack.go no longer records WHY there is no aggregate form; the reasoning is the only thing stopping one being added as a convenience")
		}
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func akReadSource(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// akCodeContains reports whether needle appears in the file's CODE rather than
// in a comment.
//
// IT HAS TO BE COMMENT-AWARE, NOT A GREP. ack.go's doc comments necessarily
// DISCUSS disconnects and "Connection: close" — that discussion is invariant
// 10's two questions answered on the record, and it is required to stay. A
// plain grep would fire on the very prose that explains why there is no
// disconnect, so the guard would be permanently red and would be deleted. This
// blanks every comment via the parser's own comment map and searches what is
// left, so only real code can trip it.
func akCodeContains(t testing.TB, name, needle string) bool {
	t.Helper()
	src := akReadSource(t, name)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	base := fset.File(f.Pos()).Base()
	code := []byte(src)
	for _, cg := range f.Comments {
		start := int(cg.Pos()) - base
		end := int(cg.End()) - base
		for i := start; i < end && i < len(code); i++ {
			if i >= 0 && code[i] != '\n' {
				code[i] = ' '
			}
		}
	}
	return strings.Contains(string(code), needle)
}

// akCodeContainsCanActuallyFail is the guard's self-check. A guard that has
// never been seen firing is not evidence — akCodeContains blanks comments, so
// the failure mode to rule out is that it blanks EVERYTHING.
func TestAckDisconnectGuardCanActuallyFail(t *testing.T) {
	t.Parallel()
	// A string that exists only in ack_test.go's own CODE, never in a comment.
	if !akCodeContains(t, "ack_test.go", "akSentinelForGuardSelfCheck") {
		t.Fatal("akCodeContains found nothing in code it should have found: the comment-blanking is eating real code, so every disconnect guard built on it is vacuous")
	}
	// And a token that appears ONLY inside a comment must NOT be found. The
	// needle is assembled from halves so that this very call site does not put
	// the whole token into CODE and make the check self-defeating:
	// akSentinelOnlyInA-Comment
	if akCodeContains(t, "ack_test.go", "akSentinelOnlyInA"+"-Comment") {
		t.Fatal("akCodeContains matched a token that appears only in a comment: the guard would fire on ack.go's required invariant-10 reasoning and would be deleted")
	}
}

const akSentinelForGuardSelfCheck = "present in code only"
