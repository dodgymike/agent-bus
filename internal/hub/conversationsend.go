package hub

import (
	"fmt"

	"github.com/dodgymike/agent-bus/internal/ids"
)

// ConversationSendRequest is a directed send to an EXPLICIT, server-resolved
// recipient set — the membership of a conversation (CONV-SEND-BY-ID).
//
// It is the multi-recipient sibling of SendRequest. The two differ ONLY in
// their addressing: SendRequest carries a single To, this carries the whole
// recipient list the httpapi layer resolved from a conversation record at send
// time. They share SignedMint, the same durability, the same idempotency and the
// same wake-up, because they share the ONE write path (publish) — see
// SendConversation.
//
// Recipients is NEVER client input as a set the client may choose: the httpapi
// handler resolves it from the durable conversation record (invariant 1 keeps
// the server authoritative on who the members are), and only then hands it here.
// Every id is a fully-qualified "<bus-id>.<agent-id>" (invariant 2).
type ConversationSendRequest struct {
	// Sender is the AUTHENTICATED principal, fully qualified (invariant 2),
	// supplied by the caller from the request context and never from a request
	// body field.
	Sender string

	// Recipients is the conversation's membership, resolved server-side. It is a
	// directed recipient list exactly like SendRequest.To, just with more than
	// one entry; publish delivers to each member's waiters through the same
	// per-recipient fan-out, and store.Message.VisibleTo excludes the sender from
	// its own copy.
	Recipients []string

	Body           []byte
	IdempotencyKey string
	SignedMint
}

// SendConversation durably records a message addressed to an explicit recipient
// SET and wakes each recipient's waiters. It returns only once the message is
// committed and fsynced (invariant 4).
//
// It is a THIN wrapper over publish — the SAME single durable write path Send
// and Broadcast use — with recipients supplied as a list rather than a single
// To. There is deliberately NO second delivery mechanism: a conversation message
// is an ordinary directed message whose recipient list happens to be more than
// one id, so it must share the write path, the ordering and the idempotency of
// every other send (invariants 4, 10). Every recipient must be addressable — on
// this bus's roster, or (since RELAY-16) a foreign id a peer advertises — or the
// whole send is ErrUnknownRecipient and nothing is written; a conversation whose
// membership has drifted out of the roster fails loudly rather than dropping
// members silently.
func (h *Hub) SendConversation(req ConversationSendRequest) (Result, error) {
	if len(req.Recipients) == 0 {
		// A conversation always has at least the creator, so the httpapi layer
		// never hands an empty set here; this fails closed rather than reaching
		// signing.Canonicalize's own empty-set refusal a step later with a
		// framing-flavoured message.
		return Result{}, fmt.Errorf("%w: a conversation send needs at least one recipient", ErrInvalidRecipient)
	}
	for _, to := range req.Recipients {
		if _, _, _, err := ids.ParseAgentID(to); err != nil {
			return Result{}, fmt.Errorf("%w: %s", ErrInvalidRecipient, err)
		}
	}
	// See Broadcast for why the idempotency outcome is dropped on this route: a
	// retry is reported through Result.Replayed and a violation through
	// ErrIdempotencyKeyReused, which is the whole of what this caller needs.
	res, _, err := h.publish(publishRequest{
		sender:     req.Sender,
		broadcast:  false,
		recipients: req.Recipients,
		body:       req.Body,
		key:        req.IdempotencyKey,
		signedMint: req.SignedMint,
	})
	return res, err
}
