package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/store"
)

// RouteConversations is the conversation surface. POST mints a conversation
// (CONV-CREATE-CLI). It AUTHENTICATES: it is NOT on authMiddleware's allow-list
// (unauthenticatedRoutes), so it is protected by being registered behind the
// default-deny middleware — an agent reaches it only after enrolling and
// completing a session challenge (invariant 3). It is registered only when a
// ConversationCreator is wired; see Options.Conversations.
const RouteConversations = "/v1/conversations"

// MaxConversationRequestBytes bounds the body of a create request before it
// reaches the decoder. The largest legitimate request is
// store.MaxConversationRecipients (64) fully-qualified ids of up to
// ids.MaxAgentIDLen (150) bytes, plus a 128-byte name, a 128-byte idempotency
// key and JSON overhead: about 10 KiB. 32 KiB is comfortable headroom and still
// finite. The store enforces the REAL bounds (recipient count, id shape, name);
// this only stops an unbounded stream reaching the decoder.
const MaxConversationRequestBytes = 32 << 10

// ConversationCreator mints a conversation durably and idempotently. It is the
// seam between the HTTP surface and internal/store.ConversationStore, kept as an
// interface so this package is testable without a real durable log and so the
// route registers only when the composition root wired a real store.
//
// The returned bool is the idempotent-replay flag: true when the call returned
// an EXISTING conversation because the (creator, key) was already applied
// (invariant 10), false for a fresh create.
type ConversationCreator interface {
	CreateIdempotent(creator, name string, recipients []string, idemKey string) (store.ConversationRecord, bool, error)
}

// ConversationCreateRequestBody is the body of POST /v1/conversations.
//
// THERE IS NO CREATOR FIELD, and there never may be one. The creator is the
// AUTHENTICATED principal taken from the request context (invariant 1 — the
// server is authoritative on every id, and a creator a client could name here
// would be a conversation it could create "as" somebody else). This is the same
// rule MintRequestBody states about its absent sender field.
type ConversationCreateRequestBody struct {
	// Recipients is the conversation membership: 1..store.MaxConversationRecipients
	// fully-qualified "<bus-id>.<agent-id>" ids (invariant 2), validated by the
	// store. It is routing metadata, not content.
	Recipients []string `json:"recipients"`

	// Name is the OPTIONAL, bounded, single-line label (CONV-NAME-INV6). Empty is
	// permitted. Over store.MaxConversationNameBytes or carrying a control
	// codepoint it is REFUSED by the store, never truncated.
	Name string `json:"name,omitempty"`

	// IdempotencyKey makes the create safe to retry (invariant 10). It is
	// client-supplied, like every other mutating agent-facing call, and arrives
	// as this JSON body field (not the Idempotency-Key header — that carrier is
	// the bus-to-bus plane only; see idem.HeaderName).
	IdempotencyKey string `json:"idempotency_key"`
}

// ConversationCreateResponseBody is the 201 body of POST /v1/conversations, and
// is replayed byte-for-byte on an idempotent retry (the replay fact travels out
// of band in IdempotencyReplayedHeader, exactly as it does on /v1/send).
//
// The CREATOR is echoed back deliberately: it is the server's view of who the
// caller is, taken from the session rather than from the request, so a client
// that sees an unexpected value there has learned something worth knowing.
type ConversationCreateResponseBody struct {
	ConversationID string   `json:"conversation_id"`
	Creator        string   `json:"creator"`
	Name           string   `json:"name,omitempty"`
	Recipients     []string `json:"recipients"`
	CreatedAt      string   `json:"created_at"`
}

// handleConversationCreate serves POST /v1/conversations (CONV-CREATE-CLI): the
// server mints the id, records the creator from the request context, and writes
// the durable conversation record, idempotently.
func (s *Server) handleConversationCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	creator, ok := s.conversationPrincipal(w, r)
	if !ok {
		return
	}
	var body ConversationCreateRequestBody
	if !s.decodeJSONRequest(w, r, &body, MaxConversationRequestBytes) {
		return
	}

	rec, replayed, err := s.conversations.CreateIdempotent(creator, body.Name, body.Recipients, body.IdempotencyKey)
	if err != nil {
		s.writeConversationError(w, r, err)
		return
	}

	if replayed {
		w.Header().Set(IdempotencyReplayedHeader, "true")
		s.log.Info("conversation create replayed from the applied-key table; nothing was minted",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", creator,
			"conversation_id", rec.ID,
		)
	} else {
		s.log.Info("conversation created",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", creator,
			"conversation_id", rec.ID,
			"recipients", len(rec.Recipients),
		)
	}

	to := rec.Recipients
	if to == nil {
		// A JSON null and an empty array read differently to a client parser; the
		// contract says this field is always an array. (A valid record always has
		// at least one recipient, so this is belt-and-braces.)
		to = []string{}
	}
	s.writeJSON(w, r, http.StatusCreated, ConversationCreateResponseBody{
		ConversationID: rec.ID,
		Creator:        rec.Creator,
		Name:           rec.Name,
		Recipients:     to,
		CreatedAt:      formatInstant(rec.CreatedAt),
	})
}

// conversationPrincipal resolves the authenticated caller for the conversation
// surface. It reads the principal from the CONTEXT, where authMiddleware put it
// after internal/auth resolved a live session — never from a header, a query
// parameter or the body, which are client claims and not identities (invariant
// 1). It deliberately does NOT require a hub: a conversation is a durable object
// of its own plane.
func (s *Server) conversationPrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.conversations == nil {
		// Unreachable: the route is registered only when the creator is non-nil.
		// Checked anyway, because "the route exists so the dependency must" is the
		// assumption that turns a wiring change into a nil dereference on a live
		// server.
		s.log.Error("the conversation route was reached on a server with no conversation store",
			"request_id", RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
		)
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{Error: "conversations are not available on this server"})
		return "", false
	}
	agentID := AgentIDFromContext(r.Context())
	if agentID == "" {
		// Also unreachable: authMiddleware is default-deny and this route is off
		// the allow-list, so a request without a principal cannot get here. It
		// fails CLOSED rather than creating a conversation with an empty creator.
		s.log.Error("the conversation route was reached with no authenticated principal",
			"request_id", RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
		)
		s.writeUnauthorized(w, r, wwwAuthenticateInvalidToken, "authentication required")
		return "", false
	}
	return agentID, true
}

// writeConversationError maps an internal/store or internal/idem failure from
// the create path to a status code and answers the client. The mapping is by
// SENTINEL (errors.Is), never by matching error text — the text is diagnostic
// detail for the operator and must be free to change without silently changing
// a status code, the same convention writeHubError follows.
func (s *Server) writeConversationError(w http.ResponseWriter, r *http.Request, err error) {
	kv := []interface{}{
		"request_id", RequestIDFromContext(r.Context()),
		"agent_id", AgentIDFromContext(r.Context()),
		"err", err,
	}

	switch {
	case errors.Is(err, store.ErrConversationKeyReused):
		// Invariant 10: same key + DIFFERENT payload is a protocol violation, not
		// a retry. Reject it and LOG it. THE CONNECTION IS KEPT — the key is the
		// caller's own (scoped per agent), so this is overwhelmingly a buggy
		// client rather than an attacker, and dropping the socket would punish
		// every unrelated request it had in flight (invariant 10, narrowed
		// 2026-08-08; the same posture as writeHubError's ErrIdempotencyKeyReused).
		s.log.Warn("conversation create rejected: idempotency key reused with a different payload; the connection is KEPT because the key is the caller's own", kv...)
		s.writeJSON(w, r, http.StatusConflict, ErrorResponse{Error: "idempotency key already used with a different conversation"})

	case errors.Is(err, idem.ErrMissingKey):
		s.log.Debug("conversation create rejected: no idempotency key", kv...)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "an idempotency key is required"})

	case errors.Is(err, idem.ErrInvalidKey), errors.Is(err, idem.ErrInvalidAgent):
		s.log.Debug("conversation create rejected: invalid idempotency key", kv...)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "invalid idempotency key"})

	case errors.Is(err, store.ErrInvalidConversation):
		// A malformed recipient id, an over-long or control-bearing name, a
		// duplicate or empty recipient list. The store's detail names limits and
		// offsets and stays in the log; the client gets the terse reason.
		s.log.Debug("conversation create rejected: invalid conversation", kv...)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "invalid conversation: check the recipient ids are fully-qualified <bus-id>.<agent-id>, there is at least one and at most " + strconv.Itoa(store.MaxConversationRecipients) + ", and any name is a single-line label of at most " + strconv.Itoa(store.MaxConversationNameBytes) + " bytes"})

	case errors.Is(err, idem.ErrCapacity), errors.Is(err, idem.ErrAgentQuota), errors.Is(err, store.ErrConversationCapacity):
		// A bound was met and nothing was created. Retryable once pressure passes;
		// the fair-share and cap detail stays in the log.
		w.Header().Set("Retry-After", pollRetryAfterSeconds)
		s.log.Warn("conversation create refused at a capacity limit", kv...)
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{Error: "server at capacity, retry later"})

	case errors.Is(err, store.ErrConversationNotDurable):
		// This bus cannot durably record a conversation, and invariant 4 forbids
		// acknowledging one it has not committed. Not dressed up as transient.
		s.log.Error("conversation create refused: this bus cannot durably record a conversation", kv...)
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{Error: "this bus cannot durably record a conversation"})

	default:
		s.log.Error("conversation create failed", kv...)
		s.writeJSON(w, r, http.StatusInternalServerError, ErrorResponse{Error: "internal error"})
	}
}
