package httpapi

import (
	"net/http"
	"strconv"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/store"
)

// The send-to-a-conversation surface (CONV-SEND-BY-ID): address a message by a
// conversation id and let the bus resolve WHO the members are, so a client sends
// "using the uuid rather than tracking the other participants".
//
// It is a TWO-STEP, exactly like /v1/mint + /v1/send and for the same reason:
// SIGN-6 requires every message to carry a signature, the signature covers the
// recipient SET (internal/signing.Canonicalize), and the bus never verifies it —
// so the client must SIGN the membership, which means it must first be TOLD the
// membership. RouteConversationMint resolves the conversation, checks the caller
// is a participant, and hands back the reservation PLUS the resolved member list
// to sign; RouteConversationSend re-resolves the membership server-authoritatively
// AT SEND TIME and publishes one directed multi-recipient message through the hub's
// existing fan-out.
//
// Both routes AUTHENTICATE: neither is on authMiddleware's allow-list
// (unauthenticatedRoutes), so both are protected by being registered behind the
// default-deny middleware. They are registered only when a ConversationLookup and
// a Hub are both wired; see Options.ConversationLookup.
const (
	// RouteConversationMint reserves the message id and sequence for a
	// conversation send AND returns the conversation's current membership for the
	// client to sign. A distinct exact pattern from RouteConversations — Go 1.19's
	// ServeMux matches "/v1/conversations" (create) and this by longest exact
	// pattern, so the two never shadow one another.
	RouteConversationMint = "/v1/conversations/mint"

	// RouteConversationSend sends one signed message to a conversation's resolved
	// membership.
	RouteConversationSend = "/v1/conversations/send"
)

// ConversationLookup resolves a durable conversation record by id. It is the
// seam between the send surface and internal/store.ConversationStore, kept as an
// interface so this package is testable without a real durable log and so the
// send routes register only when the composition root wired a real store. It is
// satisfied by *store.ConversationStore.
type ConversationLookup interface {
	// Get returns the retained conversation record for id and whether it exists.
	// The returned record's recipient slice is a copy, safe for the caller to
	// read and hold.
	Get(id string) (store.ConversationRecord, bool)
}

// ConversationMintRequestBody is the body of POST /v1/conversations/mint.
//
// THERE IS NO SENDER FIELD and there never may be one: the reservation is minted
// for the AUTHENTICATED principal taken from the request context, exactly as
// MintRequestBody states (invariant 1).
type ConversationMintRequestBody struct {
	// ConversationID is the server-minted conversation id to send to. It is
	// resolved against the durable conversation table; the caller must be a
	// member of the conversation it names.
	ConversationID string `json:"conversation_id"`

	// IdempotencyKey is the key the SUBSEQUENT send will carry. Re-minting under
	// it returns the SAME assignment and burns no further sequence (invariant 10),
	// exactly as /v1/mint does.
	IdempotencyKey string `json:"idempotency_key"`
}

// ConversationMintResponseBody is the 201 body of POST /v1/conversations/mint.
//
// It carries everything /v1/mint returns PLUS Recipients — the resolved
// membership the client must sign over. The membership is what makes a
// conversation send possible without the client tracking participants itself: the
// bus tells the client who the members are so it can produce a signature covering
// exactly the set the send will deliver to.
type ConversationMintResponseBody struct {
	MessageID string `json:"message_id"`
	Seq       uint64 `json:"seq"`

	// Sender is the AUTHENTICATED principal echoed back.
	Sender string `json:"sender"`

	// Op is the operation this reservation may be spent on — always "send", since
	// a conversation send is an ordinary directed send under the hood.
	Op string `json:"op"`

	// ConversationID is echoed back so a client that pipelines several mints can
	// pair each reservation with its conversation.
	ConversationID string `json:"conversation_id"`

	// Recipients is the conversation's membership resolved at mint time, the SET
	// the client must sign. It is the durable record's creator plus its
	// recipients, de-duplicated (see conversationMembers).
	Recipients []string `json:"recipients"`

	// ExpiresAt is when the reservation stops being honoured (hub.MintTTL).
	ExpiresAt string `json:"expires_at"`
}

// ConversationSendRequestBody is the body of POST /v1/conversations/send.
//
// It is /v1/send's body with the single To replaced by a ConversationID: the
// recipient SET is resolved by the bus from the conversation record, never
// supplied by the client, so there is deliberately no recipients field here
// (invariant 1 — the server is authoritative on who the members are). The signed
// fields mirror SendRequestBody exactly and are checked by the SAME ingest policy
// (checkSignedMint).
type ConversationSendRequestBody struct {
	// ConversationID names the conversation to send to. Resolved server-side; the
	// caller must be a member.
	ConversationID string `json:"conversation_id"`

	Body           string `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`

	// Sender EXISTS ONLY SO SIGN-6 CHECK (d) CAN BE MADE. IT IS INPUT TO
	// VALIDATE, NEVER AN IDENTITY — see SendRequestBody.Sender for the whole
	// argument. It is compared against the authenticated principal in
	// checkSignedMint and then discarded.
	Sender string `json:"sender"`

	// MessageID and Seq are the reservation minted by POST /v1/conversations/mint
	// and covered by Signature. Checked for SHAPE here and against the reservation
	// in the hub, which wins on any disagreement.
	MessageID string `json:"message_id"`
	Seq       uint64 `json:"seq"`

	// TimestampMs is the SENDER's clock, Unix milliseconds UTC, covered by the
	// signature.
	TimestampMs int64 `json:"timestamp_ms"`

	// Signature is the sender's detached Ed25519 signature over the canonical
	// message, standard base64 of exactly 64 bytes. The signed recipient set is
	// the conversation's membership the mint returned.
	Signature string `json:"signature"`
}

// handleConversationMint serves POST /v1/conversations/mint (CONV-SEND-BY-ID):
// resolve the conversation, check the caller is a participant, and hand back the
// reservation together with the membership to sign.
func (s *Server) handleConversationMint(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	sender, ok := s.conversationSendPrincipal(w, r)
	if !ok {
		return
	}
	var body ConversationMintRequestBody
	if !s.decodeJSONRequest(w, r, &body, MaxConversationRequestBytes) {
		return
	}
	members, ok := s.resolveConversationMembers(w, r, sender, body.ConversationID)
	if !ok {
		return
	}

	m, err := s.hub.Mint(hub.MintRequest{
		// The AUTHENTICATED principal, from the context. The body has no sender
		// field and never may (invariant 1). A conversation send is an ordinary
		// directed send, so the reservation is scoped to "send".
		Sender:         sender,
		Op:             string(idem.OpSend),
		IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		s.writeHubError(w, r, "conversation mint", err)
		return
	}

	if m.Replayed {
		w.Header().Set(IdempotencyReplayedHeader, "true")
		s.log.Info("conversation mint replayed from the outstanding-reservation table; no sequence was allocated",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", m.Sender,
			"conversation_id", body.ConversationID,
			"message_id", m.MessageID,
		)
	} else {
		s.log.Info("conversation send reservation minted",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", m.Sender,
			"conversation_id", body.ConversationID,
			"message_id", m.MessageID,
			"seq", m.Seq,
			"members", len(members),
		)
	}

	s.writeJSON(w, r, http.StatusCreated, ConversationMintResponseBody{
		MessageID:      m.MessageID,
		Seq:            m.Seq,
		Sender:         m.Sender,
		Op:             m.Op,
		ConversationID: body.ConversationID,
		Recipients:     members,
		ExpiresAt:      formatInstant(m.ExpiresAt),
	})
}

// handleConversationSend serves POST /v1/conversations/send (CONV-SEND-BY-ID):
// resolve the membership AT SEND TIME, apply SIGN-6's ingest policy, and publish
// one directed multi-recipient message through the hub's existing fan-out.
func (s *Server) handleConversationSend(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	sender, ok := s.conversationSendPrincipal(w, r)
	if !ok {
		return
	}
	var body ConversationSendRequestBody
	if !s.decodeJSONRequest(w, r, &body, MaxMessageRequestBytes) {
		return
	}
	// Membership is resolved HERE, server-authoritative, at send time — never
	// from the request. The mint returned this same set for the client to sign;
	// membership is fixed at create today, so the two agree, and the durable
	// record freezes the send-time membership, which is exactly what authorised
	// delivery (DECISIONS.md, CONV-SEND-BY-ID).
	members, ok := s.resolveConversationMembers(w, r, sender, body.ConversationID)
	if !ok {
		return
	}
	payload, ok := s.decodeBase64Field(w, r, "body", body.Body)
	if !ok {
		return
	}
	// The SAME SIGN-6 ingest policy /v1/send uses, including the replay
	// disconnect: the signed fields are lifted into a SendRequestBody so the one
	// implementation adjudicates both routes and cannot drift.
	signed, ok := s.checkSignedMint(w, r, sender, SendRequestBody{
		Sender:      body.Sender,
		MessageID:   body.MessageID,
		Seq:         body.Seq,
		TimestampMs: body.TimestampMs,
		Signature:   body.Signature,
	})
	if !ok {
		return
	}

	res, err := s.hub.SendConversation(hub.ConversationSendRequest{
		// STILL the authenticated principal from the context, NOT body.Sender.
		Sender:         sender,
		Recipients:     members,
		Body:           payload,
		IdempotencyKey: body.IdempotencyKey,
		SignedMint:     signed,
	})
	if err != nil {
		s.writeHubError(w, r, "conversation send", err)
		return
	}
	s.writeSendResult(w, r, res, store.ContentHash(payload))
}

// resolveConversationMembers resolves the conversation and enforces the
// participant rule, answering the client itself on failure.
//
// # 404 FOR BOTH "NOT FOUND" AND "NOT A MEMBER" (the leak-less choice)
//
// A caller that is not a member of a conversation gets the SAME 404 as one that
// named a conversation that does not exist. That is deliberate: a 403 would
// confirm the conversation exists, letting a non-participant probe for valid
// conversation ids it has no business knowing about. The distinction is recorded
// in the SERVER log at Debug so an operator can still tell the two apart
// (invariants 1, 2, 3; DECISIONS.md, CONV-SEND-BY-ID).
//
// It also bounds the membership: a conversation whose creator-plus-recipients
// exceeds store.MaxRecipients cannot be delivered as one message, and that is
// refused with a clear 400 rather than a framing-flavoured error deeper down. A
// conversation create bounds recipients at store.MaxConversationRecipients (64),
// and the creator can be one more, so the only way to reach this is a full 64
// recipients none of which is the creator.
func (s *Server) resolveConversationMembers(w http.ResponseWriter, r *http.Request, sender, conversationID string) ([]string, bool) {
	rec, found := s.conversationLookup.Get(conversationID)
	if !found {
		s.log.Debug("conversation send refused: no such conversation",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", sender,
			"conversation_id", conversationID,
		)
		s.writeJSON(w, r, http.StatusNotFound, ErrorResponse{Error: "conversation not found"})
		return nil, false
	}
	members := conversationMembers(rec)
	if !containsString(members, sender) {
		// Not a member. Answered as 404, not 403, so it is indistinguishable from
		// "no such conversation" to the caller — see the function doc.
		s.log.Debug("conversation send refused: the caller is not a member of this conversation (answered 404 so it cannot be told apart from a nonexistent conversation)",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", sender,
			"conversation_id", conversationID,
		)
		s.writeJSON(w, r, http.StatusNotFound, ErrorResponse{Error: "conversation not found"})
		return nil, false
	}
	if len(members) > store.MaxRecipients {
		s.log.Warn("conversation send refused: the membership exceeds the durable message recipient bound",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", sender,
			"conversation_id", conversationID,
			"members", len(members),
			"limit", store.MaxRecipients,
		)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "conversation has more than " + strconv.Itoa(store.MaxRecipients) + " members and cannot be delivered as a single message"})
		return nil, false
	}
	return members, true
}

// conversationMembers is the conversation's delivery/participant set: the
// creator plus the recipients, de-duplicated, creator first.
//
// The creator is a first-class member — it may send to the conversation and it
// receives every other member's messages. De-duplication matters because a
// creator that is ALSO in its own recipient list must appear once, or the
// durable message record would carry a duplicate recipient (store.NewMessage
// rejects duplicates) and the signed set would disagree with it. The sender is
// NOT removed here: store.Message.VisibleTo already excludes a sender from its
// own copy, so the member set is the same for everyone and delivery is decided at
// read time.
func conversationMembers(rec store.ConversationRecord) []string {
	out := make([]string, 0, len(rec.Recipients)+1)
	seen := make(map[string]struct{}, len(rec.Recipients)+1)
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	add(rec.Creator)
	for _, r := range rec.Recipients {
		add(r)
	}
	return out
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// conversationSendPrincipal resolves the authenticated caller for the
// conversation SEND surface and checks the dependencies it needs are wired,
// answering the client itself on failure. It mirrors conversationPrincipal but
// also requires the hub, because a conversation send both resolves a
// conversation and publishes a message.
func (s *Server) conversationSendPrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.conversationLookup == nil || s.hub == nil {
		// Unreachable: these routes are registered only when BOTH are wired.
		// Checked anyway, because "the route exists so the dependency must" is the
		// assumption that turns a wiring change into a nil dereference on a live
		// server.
		s.log.Error("a conversation send route was reached on a server missing the conversation store or the hub",
			"request_id", RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
		)
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{Error: "conversations are not available on this server"})
		return "", false
	}
	agentID := AgentIDFromContext(r.Context())
	if agentID == "" {
		// Also unreachable: authMiddleware is default-deny and these routes are
		// off the allow-list, so a request without a principal cannot get here. It
		// fails CLOSED rather than sending as an empty subject.
		s.log.Error("a conversation send route was reached with no authenticated principal",
			"request_id", RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
		)
		s.writeUnauthorized(w, r, wwwAuthenticateInvalidToken, "authentication required")
		return "", false
	}
	return agentID, true
}
