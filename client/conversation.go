package client

import (
	"context"
	"net/http"
	"strings"
)

// routeConversations mirrors internal/httpapi.RouteConversations. This package
// does not import internal/ (invariant 7 keeps client/ embeddable), so the path
// is duplicated as a constant and pinned equal by the end-to-end composition
// test, where a disagreement surfaces as a 404.
const routeConversations = "/v1/conversations"

// conversationCreateRequestBody is the body of POST /v1/conversations, matching
// httpapi.ConversationCreateRequestBody field for field.
//
// THERE IS NO CREATOR FIELD: the creator is the authenticated principal the bus
// takes from the session, never a value this client supplies (invariant 1).
type conversationCreateRequestBody struct {
	Recipients     []string `json:"recipients"`
	Name           string   `json:"name,omitempty"`
	IdempotencyKey string   `json:"idempotency_key"`
}

// conversationCreateResponseBody is the 201 shape of POST /v1/conversations,
// matching httpapi.ConversationCreateResponseBody field for field.
type conversationCreateResponseBody struct {
	ConversationID string   `json:"conversation_id"`
	Creator        string   `json:"creator"`
	Name           string   `json:"name,omitempty"`
	Recipients     []string `json:"recipients"`
	CreatedAt      string   `json:"created_at"`
}

// CreateConversationOptions is what CreateConversation needs.
type CreateConversationOptions struct {
	// Recipients is the conversation membership: fully-qualified
	// "<bus-id>.<agent-id>" ids (invariant 2). The bus validates them and mints
	// the conversation id; nothing here is trusted as an identity.
	Recipients []string

	// Name is the OPTIONAL single-line label. Empty is permitted.
	Name string

	// IdempotencyKey makes the create safe to retry (invariant 10). Leave it
	// empty and CreateConversation mints a fresh random one, reused across every
	// internal transport retry so a retried create can never become two
	// conversations.
	IdempotencyKey string
}

// ConversationResult reports a created (or replayed) conversation. Its json
// tags are a documented contract surface (CONTRACTS-CLI.md) — the CLI
// `conversation create` subcommand emits it verbatim under --json.
type ConversationResult struct {
	// ConversationID is the SERVER-minted id, "<bus-id>.<uuid-v4>" (invariant 1).
	ConversationID string `json:"conversation_id"`

	// Creator is the bus's view of who created it: the authenticated principal,
	// echoed back. A value other than this identity is worth noticing.
	Creator string `json:"creator"`

	// Name is the label the conversation was created with, or "" if none.
	Name string `json:"name,omitempty"`

	// Recipients is the membership the bus recorded.
	Recipients []string `json:"recipients"`

	// CreatedAt is when the bus minted the conversation, RFC3339Nano UTC.
	CreatedAt string `json:"created_at"`

	// Replayed reports that the bus answered from its applied-key table rather
	// than minting a fresh conversation — the create was retried under a key it
	// had already applied, and this is the ORIGINAL conversation (invariant 10).
	// It arrives as a header, so the body stays identical between the original
	// and the replay.
	Replayed bool `json:"replayed"`

	// IdempotencyKey is the key this create was applied under — the handle that
	// makes a LATER retry the same logical create rather than a second
	// conversation.
	IdempotencyKey string `json:"idempotency_key"`
}

// CreateConversation asks the bus to mint a durable conversation with the given
// recipients and optional name (POST /v1/conversations), and returns the
// server-minted record.
//
// The bus mints the id (invariant 1) and records the CREATOR as the
// authenticated identity of this client — never a value sent in the request.
// The call returns only once the conversation is durable (invariant 4).
//
// It is idempotent (invariant 10): retrying under the same key returns the
// ORIGINAL conversation with Replayed=true rather than minting a second. An
// omitted key is minted once here and reused across transport retries, so a
// retry inside this method can never create two conversations.
func (c *Client) CreateConversation(ctx context.Context, opts CreateConversationOptions) (ConversationResult, error) {
	const op = "conversation create"

	key := opts.IdempotencyKey
	if key == "" {
		var err error
		key, err = newIdempotencyKey()
		if err != nil {
			return ConversationResult{}, err
		}
	}

	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	var body conversationCreateResponseBody
	resp, err := c.authorizedRequest(ctx, request{
		method: http.MethodPost,
		path:   routeConversations,
		op:     op,
		body: conversationCreateRequestBody{
			Recipients:     opts.Recipients,
			Name:           opts.Name,
			IdempotencyKey: key,
		},
		out: &body,
		// SAFE TO REPEAT: the idempotency key makes a repeat verbatim a retry the
		// bus deduplicates from its applied-key table (invariant 10), so do() may
		// replay it across transport retries.
		retryable: true,
	})
	if err != nil {
		// writeFailed carries the key onto the error, so a caller that wants to
		// retry a failed create knows which key to reuse.
		return ConversationResult{}, writeFailed(op, key, err)
	}

	return ConversationResult{
		ConversationID: body.ConversationID,
		Creator:        body.Creator,
		Name:           body.Name,
		Recipients:     body.Recipients,
		CreatedAt:      body.CreatedAt,
		Replayed:       strings.EqualFold(resp.Header.Get(idempotencyReplayedHeader), "true"),
		IdempotencyKey: key,
	}, nil
}
