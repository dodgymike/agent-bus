package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/store"
)

// The messaging surface. Every one of these routes AUTHENTICATES: none is on
// authMiddleware's allow-list, and none needs to be — an agent reaches them
// only after enrolling and completing a session challenge (invariant 3). They
// are protected by being registered, not by anyone remembering to protect them.
const (
	// RouteAgents lists the agents enrolled on this bus.
	RouteAgents = "/v1/agents"

	// RouteBroadcast sends a message to the whole bus.
	RouteBroadcast = "/v1/broadcast"

	// RouteSend sends a message to one named agent.
	RouteSend = "/v1/send"

	// RouteMessages reads history from a cursor, without parking.
	RouteMessages = "/v1/messages"

	// RouteWait is the long poll: read from a cursor, parking until something
	// arrives or the deadline passes.
	RouteWait = "/v1/wait"
)

// MaxMessageRequestBytes bounds the body of a send or broadcast.
//
// The largest legitimate request is store.MaxBodyBytes (64 KiB) of payload,
// which costs 4/3 in base64, plus a recipient id, an idempotency key and JSON
// overhead: about 88 KiB. 128 KiB is comfortable headroom and still finite. The
// hub enforces the REAL limit on the decoded bytes; this one only stops an
// unbounded stream reaching the decoder.
const MaxMessageRequestBytes = 128 << 10

// pollRetryAfterSeconds is the Retry-After sent with a 503 from the messaging
// surface. The one capacity limit here — the applied-key table — is relieved as
// messages age out of the retention window, so a short retry is honest for a
// transient burst and harmless otherwise.
const pollRetryAfterSeconds = "5"

// AgentInfo is one entry of GET /v1/agents.
type AgentInfo struct {
	AgentID    string `json:"agent_id"`
	Name       string `json:"name"`
	EnrolledAt string `json:"enrolled_at"`
}

// AgentsResponseBody is the 200 body of GET /v1/agents.
type AgentsResponseBody struct {
	Agents []AgentInfo `json:"agents"`

	// Count is len(Agents). It is sent so a client can detect a truncated
	// response without counting, and because a bare empty array reads
	// ambiguously in a log.
	Count int `json:"count"`
}

// BroadcastRequestBody is the body of POST /v1/broadcast.
type BroadcastRequestBody struct {
	// Body is the message payload, standard base64. It is BYTES, not a string:
	// the bus never interprets a payload, and once the CRYPTO epic lands this
	// field carries ciphertext.
	Body string `json:"body"`

	// IdempotencyKey makes the send safe to retry (invariant 10).
	IdempotencyKey string `json:"idempotency_key"`
}

// SendRequestBody is the body of POST /v1/send.
type SendRequestBody struct {
	// To is the fully-qualified "<bus-id>.<agent-id>" of the recipient
	// (invariant 2).
	To string `json:"to"`

	Body           string `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`
}

// SendResponseBody is the 201 body of POST /v1/send and POST /v1/broadcast, and
// is replayed byte for byte on an idempotent retry.
//
// The SENDER is echoed back deliberately: it is the server's view of who the
// caller is, taken from the session rather than from the request, and a client
// that sees an unexpected value there has learned something worth knowing.
type SendResponseBody struct {
	MessageID  string   `json:"message_id"`
	Seq        uint64   `json:"seq"`
	From       string   `json:"from"`
	Broadcast  bool     `json:"broadcast"`
	To         []string `json:"to"`
	SentAt     string   `json:"sent_at"`
	ContentSHA string   `json:"content_sha256"`
}

// WireMessage is one message as a client receives it.
type WireMessage struct {
	MessageID string   `json:"message_id"`
	Seq       uint64   `json:"seq"`
	From      string   `json:"from"`
	Broadcast bool     `json:"broadcast"`
	To        []string `json:"to"`
	BusPath   []string `json:"bus_path"`
	SentAt    string   `json:"sent_at"`
	Size      int      `json:"size"`

	// ContentSHA256 lets a recipient verify the body it received is the body
	// the audit trail records, without the audit trail holding the body
	// (invariant 6).
	ContentSHA256 string `json:"content_sha256"`

	// Body is standard base64 of the opaque payload.
	Body string `json:"body"`
}

// BatchResponseBody is the 200 body of GET /v1/messages and GET /v1/wait.
//
// One shape for both, on purpose: an agent catches up with /v1/messages and
// then parks on /v1/wait using the SAME cursor and the SAME parser.
type BatchResponseBody struct {
	Messages []WireMessage `json:"messages"`

	// Cursor is the OPAQUE position to pass to the next call. On an empty
	// batch it is byte-identical to the cursor that was sent.
	Cursor string `json:"cursor"`

	// More reports that the batch was cut short by limit and another call will
	// return immediately.
	More bool `json:"more"`

	// TimedOut reports that a long poll reached its deadline with nothing to
	// deliver. It is FALSE on /v1/messages, which never parks. A timeout is
	// NOT an error: the status is 200 and Messages is empty.
	TimedOut bool `json:"timed_out"`
}

// handleAgents serves GET /v1/agents (MSG-1).
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if !s.requireGET(w, r) {
		return
	}
	if _, ok := s.messagingPrincipal(w, r); !ok {
		return
	}

	agents := s.hub.Agents()
	out := make([]AgentInfo, 0, len(agents))
	for _, a := range agents {
		out = append(out, AgentInfo{
			AgentID:    a.AgentID,
			Name:       a.Name,
			EnrolledAt: formatInstant(a.EnrolledAt),
		})
	}
	s.writeJSON(w, r, http.StatusOK, AgentsResponseBody{Agents: out, Count: len(out)})
}

// handleBroadcast serves POST /v1/broadcast (MSG-2).
func (s *Server) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	sender, ok := s.messagingPrincipal(w, r)
	if !ok {
		return
	}
	var body BroadcastRequestBody
	if !s.decodeJSONRequest(w, r, &body, MaxMessageRequestBytes) {
		return
	}
	payload, ok := s.decodeBase64Field(w, r, "body", body.Body)
	if !ok {
		return
	}

	res, err := s.hub.Broadcast(hub.BroadcastRequest{
		// The sender is the AUTHENTICATED principal. There is no field in
		// BroadcastRequestBody for it and there never may be: a client-supplied
		// sender is a spoofed sender (invariant 1).
		Sender:         sender,
		Body:           payload,
		IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		s.writeHubError(w, r, "broadcast", err)
		return
	}
	s.writeSendResult(w, r, res, store.ContentHash(payload))
}

// handleSend serves POST /v1/send (MSG-3).
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	sender, ok := s.messagingPrincipal(w, r)
	if !ok {
		return
	}
	var body SendRequestBody
	if !s.decodeJSONRequest(w, r, &body, MaxMessageRequestBytes) {
		return
	}
	payload, ok := s.decodeBase64Field(w, r, "body", body.Body)
	if !ok {
		return
	}

	res, err := s.hub.Send(hub.SendRequest{
		Sender:         sender,
		To:             body.To,
		Body:           payload,
		IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		s.writeHubError(w, r, "send", err)
		return
	}
	s.writeSendResult(w, r, res, store.ContentHash(payload))
}

// writeSendResult answers an accepted send. The status is 201 on the replay
// too: the response to a retry is the response to the original, status
// included, and the BODY is byte-identical — the fact that it was a replay
// travels out of band in a header, exactly as it does on /v1/enroll.
func (s *Server) writeSendResult(w http.ResponseWriter, r *http.Request, res hub.Result, contentSHA string) {
	if res.Replayed {
		w.Header().Set(IdempotencyReplayedHeader, "true")
		s.log.Info("send replayed from the applied-key table; nothing was re-applied",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", res.Sender,
			"message_id", res.MessageID,
		)
	} else {
		s.log.Info("message accepted",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", res.Sender,
			"message_id", res.MessageID,
			"seq", res.Seq,
			"broadcast", res.Broadcast,
			"recipients", len(res.Recipients),
		)
	}
	to := res.Recipients
	if to == nil {
		// A JSON null and an empty array are different things to a client
		// parser; the contract says this field is always an array.
		to = []string{}
	}
	s.writeJSON(w, r, http.StatusCreated, SendResponseBody{
		MessageID:  res.MessageID,
		Seq:        res.Seq,
		From:       res.Sender,
		Broadcast:  res.Broadcast,
		To:         to,
		SentAt:     formatInstant(res.SentAt),
		ContentSHA: contentSHA,
	})
}

// handleMessages serves GET /v1/messages (MSG-4).
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if !s.requireGET(w, r) {
		return
	}
	agentID, ok := s.messagingPrincipal(w, r)
	if !ok {
		return
	}
	after, limit, ok := s.readBatchParams(w, r, agentID)
	if !ok {
		return
	}
	batch, err := s.hub.History(agentID, after, limit)
	if err != nil {
		s.writeHubError(w, r, "history", err)
		return
	}
	s.writeBatch(w, r, agentID, batch)
}

// handleWait serves GET /v1/wait (POLL-1).
func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	if !s.requireGET(w, r) {
		return
	}
	agentID, ok := s.messagingPrincipal(w, r)
	if !ok {
		return
	}
	after, limit, ok := s.readBatchParams(w, r, agentID)
	if !ok {
		return
	}
	timeout, ok := s.readTimeoutParam(w, r)
	if !ok {
		return
	}

	batch, err := s.hub.Wait(r.Context(), agentID, after, limit, timeout)
	if err != nil {
		if r.Context().Err() != nil {
			// The client hung up, or the server is shutting down. Writing a
			// response would be writing to nobody, and net/http would log the
			// failed write as if it mattered. This is checked on the REQUEST
			// rather than by classifying err, so it cannot be confused with a
			// refusal the caller does need to hear about.
			s.log.Debug("long poll released without a response: the request context is done",
				"request_id", RequestIDFromContext(r.Context()),
				"agent_id", agentID,
				"err", err,
			)
			return
		}
		s.writeHubError(w, r, "wait", err)
		return
	}
	s.writeBatch(w, r, agentID, batch)
}

// writeBatch renders a Batch. A timeout is a 200 with an empty array and the
// cursor the caller sent (POLL-1) — never a 204 and never an error status.
func (s *Server) writeBatch(w http.ResponseWriter, r *http.Request, agentID string, batch hub.Batch) {
	msgs := make([]WireMessage, 0, len(batch.Messages))
	for _, m := range batch.Messages {
		msgs = append(msgs, toWireMessage(m))
	}
	s.writeJSON(w, r, http.StatusOK, BatchResponseBody{
		Messages: msgs,
		Cursor:   hub.EncodeCursor(agentID, batch.Cursor),
		More:     batch.More,
		TimedOut: batch.TimedOut,
	})
}

// toWireMessage renders one message for a client.
func toWireMessage(m store.Message) WireMessage {
	to := m.Recipients
	if to == nil {
		to = []string{}
	}
	busPath := m.BusPath
	if busPath == nil {
		busPath = []string{}
	}
	return WireMessage{
		MessageID:     m.ID,
		Seq:           m.Seq,
		From:          m.Sender,
		Broadcast:     m.Broadcast,
		To:            to,
		BusPath:       busPath,
		SentAt:        formatInstant(m.SentAt),
		Size:          m.Size(),
		ContentSHA256: m.ContentSHA256,
		Body:          encodeBase64(m.Body),
	}
}

// messagingPrincipal resolves the authenticated caller and checks the messaging
// surface is actually available, answering the client itself on failure.
//
// The principal is taken from the CONTEXT, where authMiddleware put it after
// internal/auth resolved a live session. It is never read from a header, a
// query parameter or a body: those are client claims, not identities
// (invariant 1).
func (s *Server) messagingPrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.hub == nil {
		// Unreachable in practice: these routes are registered only when there
		// is a hub. Checked anyway, because "the route exists so the dependency
		// must" is exactly the assumption that turns a wiring change into a nil
		// dereference on a live server.
		s.log.Error("a messaging route was reached on a server with no hub",
			"request_id", RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
		)
		w.Header().Set("Retry-After", pollRetryAfterSeconds)
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{Error: "messaging is not available on this server"})
		return "", false
	}
	agentID := AgentIDFromContext(r.Context())
	if agentID == "" {
		// Also unreachable: authMiddleware is default-deny and these routes are
		// off the allow-list, so a request without a principal cannot get here.
		// It fails CLOSED rather than serving an empty subject, which would
		// match nothing on the read path and be rejected on the write path —
		// but by accident rather than by decision.
		s.log.Error("a messaging route was reached with no authenticated principal",
			"request_id", RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
		)
		s.writeUnauthorized(w, r, wwwAuthenticateInvalidToken, "authentication required")
		return "", false
	}
	return agentID, true
}

// readBatchParams parses ?cursor= and ?limit=, answering the client on failure.
func (s *Server) readBatchParams(w http.ResponseWriter, r *http.Request, agentID string) (after uint64, limit int, ok bool) {
	q := r.URL.Query()

	after, err := hub.DecodeCursor(agentID, q.Get("cursor"))
	if err != nil {
		// The cursor value is NOT echoed: it is untrusted input on its way to a
		// log line, and the hub's error already says what was wrong with it
		// without quoting it.
		s.log.Debug("batch request rejected: bad cursor",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", agentID,
			"err", err,
		)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "invalid cursor"})
		return 0, 0, false
	}

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "limit must be a positive integer"})
			return 0, 0, false
		}
		if n > hub.MaxBatchLimit {
			s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{
				Error: fmt.Sprintf("limit must be at most %d", hub.MaxBatchLimit),
			})
			return 0, 0, false
		}
		limit = n
	}
	return after, limit, true
}

// readTimeoutParam parses ?timeout= (whole seconds) for the long poll.
//
// An out-of-range value is REFUSED rather than silently clamped. Silently
// clamping is friendlier right up to the moment a client that asked for an hour
// and got five minutes concludes the server dropped its request; a 400 that
// names the ceiling is information the client can act on.
func (s *Server) readTimeoutParam(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	raw := r.URL.Query().Get("timeout")
	if raw == "" {
		return 0, true // the hub applies the server default
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "timeout must be a positive whole number of seconds"})
		return 0, false
	}
	d := time.Duration(n) * time.Second
	if d > hub.MaxPollTimeout {
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("timeout must be at most %d seconds", int(hub.MaxPollTimeout/time.Second)),
		})
		return 0, false
	}
	return d, true
}

// writeHubError maps an internal/hub failure to a status code and answers.
//
// The mapping is by SENTINEL (errors.Is), never by matching error text, for the
// same reason writeAuthError is: the text is diagnostic detail for the operator
// and must be free to change without silently changing a status code.
func (s *Server) writeHubError(w http.ResponseWriter, r *http.Request, op string, err error) {
	kv := []interface{}{
		"request_id", RequestIDFromContext(r.Context()),
		"op", op,
		"agent_id", AgentIDFromContext(r.Context()),
		"err", err,
	}

	switch {
	case errors.Is(err, hub.ErrInvalidBody),
		errors.Is(err, hub.ErrInvalidRecipient),
		errors.Is(err, hub.ErrInvalidIdempotencyKey),
		errors.Is(err, hub.ErrInvalidCursor):
		s.log.Debug("message request rejected", kv...)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: terseHubError(err)})

	case errors.Is(err, hub.ErrIdempotencyKeyReused):
		// Invariant 10: same key + DIFFERENT payload is a protocol violation,
		// not a retry. Reject it, LOG it, and DISCONNECT the offending client —
		// net/http closes the connection after this response because of the
		// Connection header. A legitimate retry (same key, same payload) never
		// reaches here: it returns the original 201 and is not punished.
		w.Header().Set("Connection", "close")
		s.log.Warn("idempotency key reused with a different payload; disconnecting the client", kv...)
		s.writeJSON(w, r, http.StatusConflict, ErrorResponse{Error: "idempotency key already used with a different payload"})

	case errors.Is(err, hub.ErrUnknownRecipient):
		s.log.Debug("message request rejected: unknown recipient", kv...)
		s.writeJSON(w, r, http.StatusNotFound, ErrorResponse{Error: "unknown recipient"})

	case errors.Is(err, hub.ErrUnknownSender):
		// Authenticated, but not on this bus's roster. 403, not 401: the
		// credential is fine and re-authenticating will not help.
		s.log.Warn("a message was refused: the authenticated sender is not on the roster", kv...)
		s.writeJSON(w, r, http.StatusForbidden, ErrorResponse{Error: "sender is not enrolled on this bus"})

	case errors.Is(err, hub.ErrCapacity):
		w.Header().Set("Retry-After", pollRetryAfterSeconds)
		s.log.Warn("message refused at a capacity limit", kv...)
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{Error: "server at capacity, retry later"})

	case errors.Is(err, hub.ErrPoisoned), errors.Is(err, hub.ErrNotDurable):
		// NOT retryable, and deliberately not dressed up as transient: no
		// Retry-After. The bus cannot make this message durable, and invariant
		// 4 forbids acknowledging it anyway.
		s.log.Error("message refused: this bus cannot durably accept messages", kv...)
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{Error: "this bus cannot durably accept messages"})

	default:
		s.log.Error("message request failed", kv...)
		s.writeJSON(w, r, http.StatusInternalServerError, ErrorResponse{Error: "internal error"})
	}
}

// terseHubError renders the CLIENT-facing reason for a validation failure: the
// bare sentinel, with none of the wrapped detail. The detail is for the log — it
// names internal limits and byte offsets, and none of that belongs in a
// response.
func terseHubError(err error) string {
	switch {
	case errors.Is(err, hub.ErrInvalidBody):
		return "invalid message body"
	case errors.Is(err, hub.ErrInvalidRecipient):
		return "invalid recipient id"
	case errors.Is(err, hub.ErrInvalidIdempotencyKey):
		return "invalid idempotency key"
	case errors.Is(err, hub.ErrInvalidCursor):
		return "invalid cursor"
	default:
		return "bad request"
	}
}
