package client

import (
	"strings"
	"testing"
)

// TestValidateAckStatusRefusesAHostileBus covers the one thing between a
// compromised or buggy bus and a caller's terminal: every string in a delivery
// status row is printed by `agent-busctl ack-status`, so a row carrying an ANSI
// escape, a carriage return or a NUL would be executed by the terminal that
// rendered it.
//
// The validators themselves are tested in sanitize_test.go. What is tested HERE
// is that this route's shape actually routes every field through one — the
// failure mode is not a broken validator, it is a field nobody remembered to
// check.
//
// MUTATION: deleting either loop in validateAckStatus makes the matching
// subtests below FAIL by returning a nil error.
func TestValidateAckStatusRefusesAHostileBus(t *testing.T) {
	const esc = "bus-x-7\x1b[2J"

	for _, tc := range []struct {
		name string
		in   AckStatus
	}{
		{"an escape sequence in the correlation key", AckStatus{Rows: []AckRow{{State: AckDelivered, CorrelationKey: esc}}}},
		{"an escape sequence in the recipient", AckStatus{Rows: []AckRow{{State: AckDelivered, Recipient: "bus-y.beta-1\x1b[2J"}}}},
		{"a control character in the class", AckStatus{Rows: []AckRow{{State: AckRefused, Class: "recipient_refused_policy\r"}}}},
		{"a control character in attested_by", AckStatus{Rows: []AckRow{{State: AckDelivered, AttestedBy: "peer_bus\x00"}}}},
		{"prose where accepted_at belongs", AckStatus{Rows: []AckRow{{State: AckDelivered, AcceptedAt: "yesterday\x1b[2J"}}}},
		{"prose where settled_at belongs", AckStatus{Rows: []AckRow{{State: AckDelivered, SettledAt: "soon\x1b[2J"}}}},

		// The closed set, enforced rather than passed through. "polled" is the
		// exact spelling ack.State refuses to have: delivery to an inbox is NOT
		// recipient receipt.
		{"a state outside the closed set", AckStatus{Rows: []AckRow{{State: "polled"}}}},
		{"an empty state", AckStatus{Rows: []AckRow{{State: ""}}}},

		// Rows is never empty on the wire; the unknown answer is one row saying
		// "unknown". An empty array would make "not yours" and "yours" different
		// shapes, which is the oracle in a different alphabet.
		{"no rows at all", AckStatus{Rows: nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.in
			err := validateAckStatus("ack-status", &s)
			if err == nil {
				t.Fatalf("validateAckStatus accepted %+v; every printed field must be refused when the bus sends something a terminal could act on", tc.in)
			}
			if KindOf(err) != KindServer {
				t.Errorf("kind = %q, want %q — a malformed response is the BUS's fault, not the caller's", KindOf(err), KindServer)
			}
			// The refusal must not paste the hostile bytes back onto the
			// terminal it is protecting.
			if strings.ContainsAny(err.Error(), "\x00\r\x1b") {
				t.Errorf("the refusal echoes a control character verbatim: %q", err.Error())
			}
		})
	}
}

// TestValidateAckStatusAcceptsEveryLegalShape is the other half: the optional
// fields are omitempty on the wire, so a validator that refused an empty value
// would reject the legal rows — the `unknown` answer above all, which carries
// nothing but a state.
func TestValidateAckStatusAcceptsEveryLegalShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   AckStatus
	}{
		{"the unknown answer", AckStatus{Rows: []AckRow{{State: AckUnknown}}}},
		{"accepted, no class and no settled_at", AckStatus{Rows: []AckRow{{
			State: AckAccepted, CorrelationKey: "bus-x-7", Recipient: "bus-y.beta-1",
			AcceptedAt: "2026-08-16T09:00:00Z",
		}}}},
		{"delivered carries NO class", AckStatus{Rows: []AckRow{{
			State: AckDelivered, CorrelationKey: "bus-x-7", Recipient: "bus-y.beta-1",
			AttestedBy: "recipient_signature_unverified",
			AcceptedAt: "2026-08-16T09:00:00Z", SettledAt: "2026-08-16T09:00:02Z",
		}}}},
		{"undeliverable carries a bus class", AckStatus{Rows: []AckRow{{
			State: AckUndeliverable, CorrelationKey: "bus-x-7", Recipient: "bus-y.beta-1",
			Class: "no_route", AttestedBy: "peer_bus",
			AcceptedAt: "2026-08-16T09:00:00Z", SettledAt: "2026-08-16T09:00:02Z",
		}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.in
			if err := validateAckStatus("ack-status", &s); err != nil {
				t.Fatalf("validateAckStatus refused a legal row: %v", err)
			}
		})
	}
}

// TestAckStatusPredicates pins the three predicates the CLI's exit code is
// decided from. They are exported, so an agent EMBEDDING this package branches
// on them too (invariant 7's third audience).
func TestAckStatusPredicates(t *testing.T) {
	unknown := AckStatus{Rows: []AckRow{{State: AckUnknown}}}
	switch {
	case !unknown.Unknown():
		t.Error("the unknown answer does not report Unknown()")
	case unknown.Settled():
		t.Error("the unknown answer reports Settled(); \"nothing retained\" is the ABSENCE of an outcome, not one")
	case unknown.AnyNegative():
		t.Error("the unknown answer reports AnyNegative()")
	}

	mixed := AckStatus{Rows: []AckRow{{State: AckDelivered}, {State: AckInFlight}}}
	if mixed.Settled() {
		t.Error("a set with one non-terminal row reports Settled(); a broadcast is not settled until every recipient is")
	}

	negative := AckStatus{Rows: []AckRow{{State: AckDelivered}, {State: AckRefused, Class: "recipient_refused_policy"}}}
	switch {
	case !negative.Settled():
		t.Error("two terminal rows do not report Settled()")
	case !negative.AnyNegative():
		t.Error("a set containing a refusal does not report AnyNegative(); the CLI would exit 0 on a message nobody took")
	}

	if !(AckRow{State: AckUndeliverable}).Negative() || (AckRow{State: AckDelivered}).Negative() {
		t.Error("AckRow.Negative() does not name exactly refused and undeliverable")
	}
}
