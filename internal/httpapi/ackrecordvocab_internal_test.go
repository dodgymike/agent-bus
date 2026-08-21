package httpapi

import (
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/relay"
)

// TestAckRecordVocabularyRefusesANonTerminalOutcome pins the guard ACK-13's
// type change silently disabled, and which THREE separate gates (reviewer,
// security and mutation testing) each found independently.
//
// # WHY THIS TEST HAS TO EXIST RATHER THAN THE CHECK BEING "OBVIOUSLY FINE"
//
// ackRecordVocabulary's doc has always claimed that a non-terminal state is an
// ERROR. Until ACK-13 that was true WITHOUT A CHECK: relay.AckOutcome was a
// three-member uint8, so `accepted` and `in_flight` had no wire spelling and
// ack.ParseState refused them structurally. ACK-13 made AckOutcome an alias of
// ack.State — five members — and both spellings began to parse cleanly. The
// doc comment went on asserting a guarantee the code had stopped providing,
// which is the more dangerous half: a false claim ABOUT a guard, in the file
// that describes it.
//
// Nothing reaches this function with a non-terminal outcome today, because
// handleAck feeds it relay.ParseAckOutcome's terminal-only output. That is
// unreachability by AGREEMENT BETWEEN TWO PACKAGES, not by construction, and it
// is precisely the kind that dissolves without anything going red. What would
// be written if it dissolved is an ABSORBING terminal (ACK-CONTRACT.md §8.1)
// that can never afterwards be corrected — so the failure is permanent and
// silent, which is why the guard is worth a test even while unreachable.
//
// mutation-proof: delete the `!state.Terminal()` check in ackRecordVocabulary
// and the two non-terminal rows below go RED.
func TestAckRecordVocabularyRefusesANonTerminalOutcome(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		outcome ack.State
		class   ack.Class
		attest  ack.Attestation
		// wantErrContains is the substring the refusal must NAME. It is per-row
		// rather than one blanket string because these rows are refused by
		// DIFFERENT gates for different reasons, and asserting one shared word
		// across all of them hides which gate actually fired — the first draft
		// of this test did exactly that and went red against correct code.
		wantErrContains string
	}{
		// The two states the ACK-13 alias newly made REPRESENTABLE here. Both
		// parse — that is the whole point — so only the explicit terminality
		// check can refuse them.
		{"accepted is not a terminal outcome", ack.StateAccepted, "", ack.AttestedByPeerBus, "NOT a terminal state"},
		{"in_flight is not a terminal outcome", ack.StateInFlight, "", ack.AttestedByPeerBus, "NOT a terminal state"},

		// The zero value must not be readable as a positive terminal. It is
		// refused ONE GATE EARLIER, by ParseState, because StateInvalid has no
		// durable spelling at all — so the message names the closed set rather
		// than terminality. Asserting the earlier gate's own words is what
		// makes this row prove the zero value cannot slip through EITHER check.
		{"the zero state is refused before terminality is even reached", ack.StateInvalid, "", ack.AttestedByPeerBus, "is not a delivery lifecycle state"},

		// The three legal terminals still pass, so the guard is not merely
		// refusing everything — a check that rejects its own golden path is
		// indistinguishable from a working one if only negatives are asserted.
		{"delivered is accepted", ack.StateDelivered, "", ack.AttestedByRecipientSignatureUnverified, ""},
		{"refused is accepted", ack.StateRefused, ack.ClassRecipientRefusedPolicy, ack.AttestedByRecipientSignatureUnverified, ""},
		{"undeliverable is accepted", ack.StateUndeliverable, ack.ClassHorizonExpired, ack.AttestedByPeerBus, ""},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state, class, attested, err := ackRecordVocabulary(relay.ValidatedPeerAck{
				Outcome:     tc.outcome,
				Class:       tc.class,
				Attestation: tc.attest,
			})
			if tc.wantErrContains != "" {
				if err == nil {
					t.Fatalf("ackRecordVocabulary(%s) = (%s, %q, %q, <nil>), want an error: a non-terminal outcome would be written as an ABSORBING terminal that could never be corrected",
						tc.outcome, state, class, attested)
				}
				if got := err.Error(); !strings.Contains(got, tc.wantErrContains) {
					t.Errorf("error %q does not contain %q, so it does not name the gate that actually refused this and will not tell an operator what went wrong", got, tc.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ackRecordVocabulary(%s) = %v, want it accepted: %s is one of the three legal terminals", tc.outcome, err, tc.outcome)
			}
			if state != tc.outcome {
				t.Errorf("state = %s, want %s", state, tc.outcome)
			}
			if class != tc.class {
				t.Errorf("class = %q, want %q", class, tc.class)
			}
			if attested != tc.attest {
				t.Errorf("attested_by = %q, want %q", attested, tc.attest)
			}
		})
	}
}
