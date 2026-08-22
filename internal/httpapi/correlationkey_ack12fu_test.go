package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/store"
)

// ACK-12-FU-WATCH-CORRELATION-KEY — the SERVER half, pinned at the branch.
//
// The end-to-end proof is TestThreeBusEndToEndAckNack in tests/e2e: a recipient
// takes the key off its own `watch --json` stream and acks a relayed message
// with it. This file does NOT restate that. It pins the two things a live
// three-bus run cannot show:
//
//  1. the BRANCH — toWireMessage must read store.Message.OriginID(), not
//     m.OriginMessageID and not m.ID. Serving m.OriginMessageID raw is empty on
//     a same-bus message; serving m.ID is the one id the ack path REFUSES for a
//     relayed one. Both are silent: each still yields a well-formed field.
//  2. that the field is NOT `omitempty`. Against a live bus OriginID() is never
//     empty, so no e2e run can ever observe the encoding of the empty case —
//     yet `omitempty` there is exactly what would push `.correlation_key //
//     .message_id` (the re-spelled branch, in shell) into every consumer.
func TestToWireMessageServesOriginIDAsCorrelationKey(t *testing.T) {
	const (
		localBus  = "bus-localaaaaaaaaaa"
		originBus = "bus-originbbbbbbbbb"
	)

	base := store.Message{
		Seq:        11,
		Pos:        3,
		Sender:     originBus + ".sender-1",
		Recipients: []string{localBus + ".recipient-1"},
		SentAt:     time.Unix(1700000000, 0).UTC(),
		Body:       []byte("hello"),
	}

	tests := []struct {
		name string
		// mutate produces the message under test from base.
		mutate func(m store.Message) store.Message
		// wantKey is the correlation_key toWireMessage must serve.
		wantKey string
		// wantEqualsMessageID says whether the key and message_id coincide.
		wantEqualsMessageID bool
		why                 string
	}{
		{
			name: "same_bus_message_keys_on_its_own_id",
			mutate: func(m store.Message) store.Message {
				m.ID = localBus + "-11"
				m.OriginMessageID = "" // this bus minted it; there is no origin id
				m.BusPath = []string{localBus}
				return m
			},
			wantKey:             localBus + "-11",
			wantEqualsMessageID: true,
			why: "this bus IS the origin, so OriginID() falls back to ID. A correlation_key of \"\" here " +
				"means the field was taken from m.OriginMessageID directly instead of through OriginID(), " +
				"and an empty correlation key acks nothing.",
		},
		{
			name: "relayed_message_keys_on_the_ORIGIN_bus_id",
			mutate: func(m store.Message) store.Message {
				m.ID = localBus + "-11"                 // what THIS bus minted (invariant 1)
				m.OriginMessageID = originBus + "-4224" // what the ORIGIN bus minted
				m.BusPath = []string{originBus, localBus}
				return m
			},
			wantKey:             originBus + "-4224",
			wantEqualsMessageID: false,
			why: "the §3 key is the ORIGIN bus's server-minted id. Serving this bus's own id here is the " +
				"exact string POST /v1/ack refuses, and it is well-formed, so the mistake is silent.",
		},
		{
			name: "relayed_message_whose_origin_seq_collides_with_the_local_seq",
			mutate: func(m store.Message) store.Message {
				// Same SEQ on both buses, different bus halves: the case where
				// keying on the seq rather than on the whole id would pass.
				m.ID = localBus + "-11"
				m.OriginMessageID = originBus + "-11"
				m.BusPath = []string{originBus, localBus}
				return m
			},
			wantKey:             originBus + "-11",
			wantEqualsMessageID: false,
			why:                 "the bus half is load-bearing: two buses mint the same seq all the time.",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := tc.mutate(base)
			w := toWireMessage(m)

			if w.CorrelationKey != tc.wantKey {
				t.Fatalf("correlation_key = %q, want %q (message_id = %q, origin_message_id = %q)\n%s",
					w.CorrelationKey, tc.wantKey, m.ID, m.OriginMessageID, tc.why)
			}
			// Belt and braces: it must be OriginID() itself, whatever that
			// returns, so this stays true if OriginID's own contract moves.
			if w.CorrelationKey != m.OriginID() {
				t.Fatalf("correlation_key = %q but store.Message.OriginID() = %q; the read path must carry "+
					"OriginID() verbatim and never re-spell the branch", w.CorrelationKey, m.OriginID())
			}
			if got := w.CorrelationKey == w.MessageID; got != tc.wantEqualsMessageID {
				t.Fatalf("correlation_key == message_id is %v, want %v (key %q, id %q)",
					got, tc.wantEqualsMessageID, w.CorrelationKey, w.MessageID)
			}
			if w.MessageID != m.ID {
				t.Fatalf("message_id = %q, want this bus's own mint %q; adding the correlation key must not "+
					"make this bus adopt a peer's id (invariant 1)", w.MessageID, m.ID)
			}
		})
	}
}

// TestWireMessageAlwaysEncodesCorrelationKey pins the DELIBERATE absence of
// `omitempty`. A live bus never serves an empty key, so this is the only place
// the empty encoding can be observed at all.
func TestWireMessageAlwaysEncodesCorrelationKey(t *testing.T) {
	raw, err := json.Marshal(WireMessage{})
	if err != nil {
		t.Fatalf("marshal WireMessage: %v", err)
	}
	if !strings.Contains(string(raw), `"correlation_key":""`) {
		t.Fatalf("an empty WireMessage encoded to %s, which does not contain `\"correlation_key\":\"\"`.\n"+
			"The field must NOT be `omitempty`: `jq -r .correlation_key` has to be ONE instruction for a "+
			"relayed and a same-bus message alike. Omitting it pushes `.correlation_key // .message_id` — "+
			"the re-spelled origin-id branch, now in shell — into every consumer, and its wrong arm names "+
			"the wrong bus's message.", raw)
	}
}
