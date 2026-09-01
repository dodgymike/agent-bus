package client

import (
	"context"
	"net/http"
)

// Route paths for the send-to-a-conversation surface (CONV-SEND-BY-ID). Pinned
// here as literals mirroring internal/httpapi's RouteConversationMint and
// RouteConversationSend for the invariant-7 reason the other route constants are
// (this package must not import internal/). A divergence fails loudly as a 404 on
// the first call, which is the right direction for a duplicated constant to break.
const (
	routeConversationMint = "/v1/conversations/mint"
	routeConversationSend = "/v1/conversations/send"
)

// conversationMintRequestBody is the body of POST /v1/conversations/mint,
// matching httpapi.ConversationMintRequestBody. There is no sender field: the bus
// takes the principal from the session (invariant 1).
type conversationMintRequestBody struct {
	ConversationID string `json:"conversation_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

// conversationMintResponseBody is the 201 shape of POST /v1/conversations/mint,
// matching httpapi.ConversationMintResponseBody. Recipients is the resolved
// membership the client must sign.
type conversationMintResponseBody struct {
	MessageID      string   `json:"message_id"`
	Seq            uint64   `json:"seq"`
	Sender         string   `json:"sender"`
	Op             string   `json:"op"`
	ConversationID string   `json:"conversation_id"`
	Recipients     []string `json:"recipients"`
	ExpiresAt      string   `json:"expires_at"`
}

// conversationSendRequestBody is the body of POST /v1/conversations/send, matching
// httpapi.ConversationSendRequestBody. The recipient SET is NOT sent — the bus
// resolves it from the conversation (invariant 1). Body is []byte, marshalled by
// encoding/json as standard base64, which is the wire form the bus parses.
type conversationSendRequestBody struct {
	ConversationID string `json:"conversation_id"`
	Body           []byte `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`
	Sender         string `json:"sender"`
	MessageID      string `json:"message_id"`
	Seq            uint64 `json:"seq"`
	TimestampMS    int64  `json:"timestamp_ms"`
	Signature      string `json:"signature"`
}

// ConversationSendOptions is the input to SendToConversation.
type ConversationSendOptions struct {
	// ConversationID is the server-minted conversation id to send to
	// ("<bus-id>.<uuid-v4>"). The bus resolves the membership from it; the caller
	// does not enumerate participants.
	ConversationID string

	// Body is the message payload, DECODED. At most MaxBodyBytes.
	Body []byte

	// IdempotencyKey makes the send safe to retry (invariant 10). Leave it empty
	// and one fresh random key is minted here and carried across both legs of the
	// handshake and every transport retry, so a retried conversation send can never
	// become two messages.
	IdempotencyKey string
}

// conversationReservation is a minted reservation for a conversation send,
// together with the membership the client must sign over.
type conversationReservation struct {
	reservation
	recipients []string
}

// SendToConversation sends one signed message to a conversation's CURRENT
// membership, addressing it by conversation id (CONV-SEND-BY-ID). It returns once
// the message is DURABLE (invariant 4).
//
// # Why the client does not enumerate participants
//
// The whole point of the epic is that a client sends "using the uuid rather than
// tracking the other participants". The bus is authoritative on who the members
// are (invariant 1): the first leg (POST /v1/conversations/mint) resolves the
// conversation, checks this caller is a member, and hands back the reservation
// AND the member list; the client signs that list and sends it back. The caller
// never has to know or track the membership — it only names the conversation.
//
// # Why it is a two-step, like Send
//
// SIGN-6 makes a signature mandatory and the signature covers the recipient set,
// so the client must sign the membership — which means it must first be told the
// membership, and it must first have the server-minted id and sequence to sign
// (SIGN-1). Both legs carry the ONE idempotency key, so the whole handshake is
// retryable as a unit and converges on exactly one message. The bus re-resolves
// the membership on the send leg, server-authoritative, at send time.
func (c *Client) SendToConversation(ctx context.Context, opts ConversationSendOptions) (SendResult, error) {
	const op = "conversation send"

	if err := validateConversationID(op, opts.ConversationID); err != nil {
		return SendResult{}, err
	}
	if err := validateSendBody(op, opts.Body); err != nil {
		return SendResult{}, err
	}
	key, err := resolveIdempotencyKey(op, opts.IdempotencyKey)
	if err != nil {
		return SendResult{}, err
	}

	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	// RESERVE (and resolve membership), then SIGN, then SEND — all under the one
	// key, exactly as Send does.
	res, err := c.conversationReserve(ctx, op, opts.ConversationID, key)
	if err != nil {
		return SendResult{IdempotencyKey: key}, err
	}
	sig, timestampMS, err := c.signOutgoing(op, res.reservation, res.recipients, opts.Body)
	if err != nil {
		return SendResult{IdempotencyKey: key}, err
	}

	return c.submit(ctx, op, routeConversationSend, conversationSendRequestBody{
		ConversationID: opts.ConversationID,
		Body:           opts.Body,
		IdempotencyKey: key,
		Sender:         res.reservation.Sender,
		MessageID:      res.reservation.MessageID,
		Seq:            res.reservation.Seq,
		TimestampMS:    timestampMS,
		Signature:      sig,
	}, key)
}

// conversationReserve performs the first leg: it asks the bus to mint the id and
// sequence for a conversation send and to return the conversation's membership to
// sign. The SAME idempotency key covers this and the send, so re-issuing it
// returns the SAME reservation rather than burning a second (invariant 10).
func (c *Client) conversationReserve(ctx context.Context, op, conversationID, key string) (conversationReservation, error) {
	var out conversationMintResponseBody
	if _, err := c.authorizedRequest(ctx, request{
		method: http.MethodPost,
		path:   routeConversationMint,
		op:     op,
		body:   conversationMintRequestBody{ConversationID: conversationID, IdempotencyKey: key},
		out:    &out,
		// Safe to repeat: the key makes a repeat return the original reservation.
		retryable: true,
	}); err != nil {
		return conversationReservation{}, err
	}

	// The bus is authoritative on ids — authoritative, not unvalidated. These are
	// about to be signed by us and printed to a terminal, and a hostile bus chooses
	// every byte of them (sanitize.go).
	if err := validateServerField(op, "message id", out.MessageID); err != nil {
		return conversationReservation{}, err
	}
	if err := validateServerField(op, "sender id", out.Sender); err != nil {
		return conversationReservation{}, err
	}
	if out.Seq == 0 {
		return conversationReservation{}, newError(KindServer, op,
			"the bus returned sequence 0 for "+safeText(out.MessageID, 60)+", which is never allocated",
			"this is not a well-formed agent-bus response; check that --bus points at the bus you intended")
	}
	if len(out.Recipients) == 0 {
		return conversationReservation{}, newError(KindServer, op,
			"the bus returned no members for conversation "+safeText(conversationID, 60),
			"a conversation always has at least its creator; check that --bus points at an agent-bus server")
	}
	for _, r := range out.Recipients {
		if err := validateServerField(op, "conversation member id", r); err != nil {
			return conversationReservation{}, err
		}
	}
	return conversationReservation{
		reservation: reservation{MessageID: out.MessageID, Seq: out.Seq, Sender: out.Sender},
		recipients:  out.Recipients,
	}, nil
}

// validateConversationID checks the SHAPE of a conversation id locally, before
// the round trip. It is deliberately permissive for the invariant-1 reason
// validateRecipient is: the bus stays authoritative on the id format, so this
// only catches the mistake worth catching locally — an empty or obviously
// malformed id — and refuses anything that could not be an id at all.
func validateConversationID(op, id string) error {
	if id == "" {
		return usagef(op,
			"pass the conversation id as the first argument: `agent-busctl conversation send <conversation-id> --body '…'`; create one with `agent-busctl conversation create`",
			"no conversation id")
	}
	if !serverIDPattern.MatchString(id) {
		return usagef(op,
			"a conversation id is the `<bus-id>.<uuid>` the bus returned from `agent-busctl conversation create`",
			"conversation id %q is not a well-formed id", safeText(id, 60))
	}
	if _, _, ok := splitQualifiedID(id); !ok {
		return usagef(op,
			"use the fully-qualified `<bus-id>.<uuid>` the bus returned from `agent-busctl conversation create`",
			"conversation id %q is not fully qualified", safeText(id, 60))
	}
	return nil
}
