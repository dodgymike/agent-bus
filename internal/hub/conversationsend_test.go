package hub_test

import (
	"errors"
	"testing"

	"github.com/dodgymike/agent-bus/internal/hub"
)

// TestConversationSendByID proves the hub half of CONV-SEND-BY-ID: a directed
// send to an EXPLICIT recipient SET (a conversation's membership) rides the SAME
// publish path as an ordinary send — one durable write, per-recipient delivery,
// the sender excluded from its own copy, and the same idempotency (invariant 10).
//
// It is the hub-level companion of the httpapi test of the same name, which
// proves the route, the participant authorisation and the 404s. Here the concern
// is only that SendConversation fans one signed message out to every member and
// reuses the existing delivery rather than a second path.
func TestConversationSendByID(t *testing.T) {
	t.Run("delivers one message to every member except the sender", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alice", "bob", "carol")
		a := agentID(t, testBusID, "alice")
		b := agentID(t, testBusID, "bob")
		c := agentID(t, testBusID, "carol")

		signed := mintFor(t, h, a, "send", "k-conv")
		res, err := h.SendConversation(hub.ConversationSendRequest{
			Sender:         a,
			Recipients:     []string{a, b, c},
			Body:           []byte("hello conversation"),
			IdempotencyKey: "k-conv",
			SignedMint:     signed,
		})
		if err != nil {
			t.Fatalf("SendConversation: %v", err)
		}
		if res.MessageID == "" || res.Seq == 0 {
			t.Fatalf("SendConversation returned message_id %q seq %d; both must be set", res.MessageID, res.Seq)
		}

		// BOTH other members receive it; the SENDER does NOT (store.Message.VisibleTo
		// excludes a sender from its own copy, exactly as for a direct message).
		if got := historyIDs(t, h, b); !contains(got, res.MessageID) {
			t.Fatalf("bob did not receive the conversation message; history %v", got)
		}
		if got := historyIDs(t, h, c); !contains(got, res.MessageID) {
			t.Fatalf("carol did not receive the conversation message; history %v", got)
		}
		if got := historyIDs(t, h, a); contains(got, res.MessageID) {
			t.Fatalf("alice (the sender) received her own conversation message; history %v — the sender must be excluded", got)
		}
	})

	t.Run("same key + same payload is a legitimate retry: the ORIGINAL result, replayed, one message", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alice", "bob")
		a := agentID(t, testBusID, "alice")
		b := agentID(t, testBusID, "bob")
		body := []byte("retry me")

		first, err := h.SendConversation(hub.ConversationSendRequest{
			Sender: a, Recipients: []string{a, b}, Body: body, IdempotencyKey: "k-retry",
			SignedMint: mintFor(t, h, a, "send", "k-retry"),
		})
		if err != nil {
			t.Fatalf("first SendConversation: %v", err)
		}
		again, err := h.SendConversation(hub.ConversationSendRequest{
			Sender: a, Recipients: []string{a, b}, Body: body, IdempotencyKey: "k-retry",
			SignedMint: mintFor(t, h, a, "send", "k-retry"),
		})
		if err != nil {
			t.Fatalf("retry SendConversation: %v", err)
		}
		if !again.Replayed {
			t.Fatalf("retry was not reported as replayed; a same-key same-body retry must return the original (invariant 10)")
		}
		if again.MessageID != first.MessageID {
			t.Fatalf("retry produced a SECOND message %q, want the original %q (invariant 10)", again.MessageID, first.MessageID)
		}
		// bob holds exactly one copy.
		n := 0
		for _, id := range historyIDs(t, h, b) {
			if id == first.MessageID {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("bob holds %d copies of the conversation message, want exactly 1", n)
		}
	})

	t.Run("an empty recipient set is refused", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alice")
		a := agentID(t, testBusID, "alice")
		_, err := h.SendConversation(hub.ConversationSendRequest{
			Sender: a, Recipients: nil, Body: []byte("x"), IdempotencyKey: "k-empty",
			SignedMint: mintFor(t, h, a, "send", "k-empty"),
		})
		if !errors.Is(err, hub.ErrInvalidRecipient) {
			t.Fatalf("SendConversation with no recipients = %v, want ErrInvalidRecipient", err)
		}
	})

	t.Run("a member not on the roster fails the whole send (nothing delivered silently)", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alice", "bob")
		a := agentID(t, testBusID, "alice")
		b := agentID(t, testBusID, "bob")
		ghost := agentID(t, testBusID, "ghost") // never enrolled
		_, err := h.SendConversation(hub.ConversationSendRequest{
			Sender: a, Recipients: []string{a, b, ghost}, Body: []byte("x"), IdempotencyKey: "k-ghost",
			SignedMint: mintFor(t, h, a, "send", "k-ghost"),
		})
		if !errors.Is(err, hub.ErrUnknownRecipient) {
			t.Fatalf("SendConversation with an unenrolled member = %v, want ErrUnknownRecipient", err)
		}
	})
}
