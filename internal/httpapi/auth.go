package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
)

// The three routes that ISSUE a credential. All are POST, all take and return
// JSON, and all are UNAUTHENTICATED by necessity: they are how an agent obtains
// the token every other route will require (invariant 3). That is exactly why
// every one of them is bounded, strictly parsed and admission-controlled.
const (
	// RouteEnroll registers an agent and mints its server-authoritative id.
	RouteEnroll = "/v1/enroll"

	// RouteSessionBegin issues an opaque token for the agent to sign.
	RouteSessionBegin = "/v1/session/begin"

	// RouteSessionComplete verifies the signature and activates the token.
	RouteSessionComplete = "/v1/session/complete"
)

// MaxAuthRequestBytes bounds the body of an auth route.
//
// The largest legitimate request here is an enrolment: a 64-byte name, a
// 44-byte base64 Ed25519 public key and a 128-byte idempotency key — under a
// third of a kilobyte with JSON overhead. 8 KiB is generous by an order of
// magnitude and still finite, which is the point: an unauthenticated caller
// must not be able to stream unbounded bytes into json.Decode.
const MaxAuthRequestBytes = 8 << 10

// IdempotencyReplayedHeader marks a response that was replayed from the
// idempotency table rather than produced by a fresh application of the request.
// The BODY is byte-identical to the original, deliberately — a retry must not
// be able to tell the difference in the payload it parses — so the fact that it
// was a replay is carried out of band for operators and clients that care.
const IdempotencyReplayedHeader = "Idempotency-Replayed"

// capacityRetryAfterSeconds is the Retry-After sent with a 503 from an auth
// route. Every capacity limit in internal/auth is a live, in-memory bound that
// a departing agent or an expiring session can relieve within seconds, so this
// is short.
const capacityRetryAfterSeconds = "5"

// EnrolRequestBody is the body of POST /v1/enroll.
type EnrolRequestBody struct {
	// Name is the short agent name being requested. The SERVER decides the
	// actual id (invariant 1); this is only the human-chosen half.
	Name string `json:"name"`

	// PublicKey is the base64 (standard encoding, padded) Ed25519 AUTH public
	// key. The server stores only this half and can therefore VERIFY the
	// agent's calls but never FORGE them.
	PublicKey string `json:"public_key"`

	// IdempotencyKey makes the enrolment safe to retry (invariant 10).
	IdempotencyKey string `json:"idempotency_key"`
}

// EnrolResponseBody is the 201 body of POST /v1/enroll, and is replayed byte
// for byte on an idempotent retry.
type EnrolResponseBody struct {
	AgentID    string `json:"agent_id"`
	BusID      string `json:"bus_id"`
	Name       string `json:"name"`
	EnrolledAt string `json:"enrolled_at"`
}

// SessionBeginRequestBody is the body of POST /v1/session/begin.
type SessionBeginRequestBody struct {
	AgentID string `json:"agent_id"`
}

// SessionBeginResponseBody is the 200 body of POST /v1/session/begin.
//
// It deliberately does NOT carry auth.SessionSigningContext. The client PINS
// that constant; a client that learned the prefix from this response would sign
// whatever a man in the middle chose to put in front of the token, which is the
// signing oracle domain separation exists to prevent.
type SessionBeginResponseBody struct {
	AgentID string `json:"agent_id"`

	// Token is the opaque handle to sign. It is a live credential the moment
	// the challenge completes: never log it, never store it server-side in
	// plaintext, never echo it into an error.
	Token string `json:"token"`

	ChallengeExpiresAt string `json:"challenge_expires_at"`
}

// SessionCompleteRequestBody is the body of POST /v1/session/complete.
type SessionCompleteRequestBody struct {
	Token string `json:"token"`

	// Signature is the base64 (standard encoding, padded) Ed25519 signature
	// over auth.SessionSigningContext + token.
	Signature string `json:"signature"`
}

// SessionCompleteResponseBody is the 200 body of POST /v1/session/complete.
type SessionCompleteResponseBody struct {
	AgentID   string `json:"agent_id"`
	ExpiresAt string `json:"expires_at"`

	// LifetimeSeconds and RefreshAfterSeconds are advice, so a wrapper does not
	// have to hard-code the schedule. Expiry itself is enforced server-side
	// against the server's clock with no skew grace.
	LifetimeSeconds     int `json:"lifetime_seconds"`
	RefreshAfterSeconds int `json:"refresh_after_seconds"`
}

// handleEnroll serves POST /v1/enroll.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	var body EnrolRequestBody
	if !s.decodeAuthRequest(w, r, &body) {
		return
	}

	pub, ok := s.decodeBase64Field(w, r, "public_key", body.PublicKey)
	if !ok {
		return
	}

	res, err := s.auth.Enrol(auth.EnrolRequest{
		Name:           body.Name,
		PublicKey:      pub,
		IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		// The NAME is logged, the public key is not: a name is what an operator
		// needs to correlate a rejected enrolment, and a key in a log line is
		// noise at best.
		s.writeAuthError(w, r, "enrol", err, "name", body.Name)
		return
	}

	// NOTHING IS REPORTED TO THE HUB HERE, and nothing may be added back.
	//
	// This handler used to call hub.NoteEnrolment to feed a SECOND roster the
	// hub kept for itself. AUTH-7 deleted both: the hub now reads through to the
	// same auth roster this Enrol just wrote to (hub.RosterSource), so the agent
	// is on the hub's list the instant Enrol returns, on the replay path as much
	// as on the first attempt, with no second copy that could be missed, ordered
	// differently, or lost across a restart.
	//
	// Re-adding a notification here would recreate exactly the divergence that
	// change removed.

	if res.Replayed {
		w.Header().Set(IdempotencyReplayedHeader, "true")
		s.log.Info("enrolment replayed from the idempotency table",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", res.AgentID,
		)
	} else {
		s.log.Info("agent enrolled",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", res.AgentID,
		)
	}
	// 201 on the replay too: the response to a retry is the response to the
	// original, status included.
	s.writeJSON(w, r, http.StatusCreated, EnrolResponseBody{
		AgentID:    res.AgentID,
		BusID:      res.BusID,
		Name:       res.Name,
		EnrolledAt: formatInstant(res.EnrolledAt),
	})
}

// handleSessionBegin serves POST /v1/session/begin.
func (s *Server) handleSessionBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	var body SessionBeginRequestBody
	if !s.decodeAuthRequest(w, r, &body) {
		return
	}

	ch, err := s.auth.BeginSession(body.AgentID)
	if err != nil {
		// body.AgentID is untrusted and unbounded, so it is NOT logged here;
		// the wrapped error from internal/auth already carries a bounded,
		// validated description of what was wrong with it.
		s.writeAuthError(w, r, "session begin", err)
		return
	}

	// No token in this log line, now or ever: it is a credential.
	s.log.Info("session challenge issued",
		"request_id", RequestIDFromContext(r.Context()),
		"agent_id", ch.AgentID,
	)
	// This is the ONE response body on the auth surface that carries a live
	// credential, so it is the one that must never be written to a cache: no
	// proxy store, no browser disk cache, no history buffer. The other two auth
	// routes deliberately do NOT set this — /v1/enroll returns only the public
	// half of an identity the server just minted, and /v1/session/complete
	// returns an agent id and an expiry, ECHOING no token back. Adding the
	// header there would blur what it means here.
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, r, http.StatusOK, SessionBeginResponseBody{
		AgentID:            ch.AgentID,
		Token:              ch.Token,
		ChallengeExpiresAt: formatInstant(ch.ChallengeExpiresAt),
	})
}

// handleSessionComplete serves POST /v1/session/complete.
func (s *Server) handleSessionComplete(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	var body SessionCompleteRequestBody
	if !s.decodeAuthRequest(w, r, &body) {
		return
	}

	sig, ok := s.decodeBase64Field(w, r, "signature", body.Signature)
	if !ok {
		return
	}

	sess, err := s.auth.CompleteSession(body.Token, sig)
	if err != nil {
		// Neither the token nor the signature appears in this log line.
		s.writeAuthError(w, r, "session complete", err)
		return
	}

	s.log.Info("session activated",
		"request_id", RequestIDFromContext(r.Context()),
		"agent_id", sess.AgentID,
		"expires_at", formatInstant(sess.ExpiresAt),
	)
	s.writeJSON(w, r, http.StatusOK, SessionCompleteResponseBody{
		AgentID:             sess.AgentID,
		ExpiresAt:           formatInstant(sess.ExpiresAt),
		LifetimeSeconds:     int(auth.SessionLifetime / time.Second),
		RefreshAfterSeconds: int(auth.RefreshAfter() / time.Second),
	})
}

// writeAuthError maps an internal/auth failure to a status code and answers.
//
// The mapping is by SENTINEL (errors.Is), never by matching error text: the
// text is diagnostic detail for the operator and is free to change without
// silently changing a status code.
//
// Two audiences, deliberately split. The CLIENT gets a terse reason — enough to
// fix its own request, never enough to enumerate agents or learn why a
// signature failed. The LOG gets the wrapped error in full.
func (s *Server) writeAuthError(w http.ResponseWriter, r *http.Request, op string, err error, logKV ...interface{}) {
	kv := append([]interface{}{
		"request_id", RequestIDFromContext(r.Context()),
		"op", op,
		"err", err,
	}, logKV...)

	switch {
	case errors.Is(err, auth.ErrInvalidName),
		errors.Is(err, auth.ErrInvalidPublicKey),
		errors.Is(err, auth.ErrInvalidIdempotencyKey):
		s.log.Debug("auth request rejected", kv...)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: terseAuthError(err)})

	case errors.Is(err, auth.ErrIdempotencyKeyReused):
		// Invariant 10: same key + DIFFERENT payload is a protocol violation,
		// not a retry. Reject it, LOG it, and DISCONNECT the offending client —
		// net/http closes the connection after this response because of the
		// Connection header. Note the contrast with a legitimate retry (same
		// key, same payload), which never reaches here: it returns the original
		// 201 and is not punished in any way.
		w.Header().Set("Connection", "close")
		s.log.Warn("idempotency key reused with a different payload; disconnecting the client", kv...)
		s.writeJSON(w, r, http.StatusConflict, ErrorResponse{Error: "idempotency key already used with a different payload"})

	case errors.Is(err, auth.ErrUnknownAgent), errors.Is(err, auth.ErrUnknownSession):
		s.log.Debug("auth request rejected", kv...)
		s.writeJSON(w, r, http.StatusNotFound, ErrorResponse{Error: terseAuthError(err)})

	case errors.Is(err, auth.ErrBadSignature):
		// Info, not Debug: repeated signature failures are the shape of a
		// credential attack and an operator should see them by default.
		s.log.Info("auth signature rejected", kv...)
		s.writeJSON(w, r, http.StatusUnauthorized, ErrorResponse{Error: "signature does not verify"})

	case errors.Is(err, auth.ErrCapacity):
		w.Header().Set("Retry-After", capacityRetryAfterSeconds)
		s.log.Warn("auth request refused at a capacity limit", kv...)
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{Error: "server at capacity, retry later"})

	default:
		// Includes ErrDuplicateAgentID, which is an internal invariant breach
		// (a suffix issued twice) and not something the client did.
		s.log.Error("auth request failed", kv...)
		s.writeJSON(w, r, http.StatusInternalServerError, ErrorResponse{Error: "internal error"})
	}
}

// terseAuthError renders the CLIENT-facing reason for a validation failure:
// the bare sentinel, with none of the wrapped detail. The detail is for the log
// — it can name agents, byte offsets and internal limits, and none of that
// belongs in an unauthenticated response.
func terseAuthError(err error) string {
	switch {
	case errors.Is(err, auth.ErrInvalidName):
		return "invalid agent name"
	case errors.Is(err, auth.ErrInvalidPublicKey):
		return "invalid public key"
	case errors.Is(err, auth.ErrInvalidIdempotencyKey):
		return "invalid idempotency key"
	case errors.Is(err, auth.ErrUnknownAgent):
		return "unknown agent"
	case errors.Is(err, auth.ErrUnknownSession):
		return "unknown or expired session"
	default:
		return "bad request"
	}
}

// requirePOST answers 405 with an Allow header for anything but POST, and
// reports whether the handler should continue. Sibling of requireGET.
func (s *Server) requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost {
		return true
	}
	w.Header().Set("Allow", http.MethodPost)
	s.writeJSON(w, r, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
	return false
}

// decodeAuthRequest reads a bounded, strictly-parsed JSON body into dst. It
// answers the client itself on failure and reports whether the handler should
// continue.
//
// Every restriction here is load-bearing on an unauthenticated route:
//
//   - Content-Type must be application/json (a charset parameter is allowed).
//     Refusing anything else keeps these routes off the list of things a
//     cross-origin form post can reach.
//   - The body is bounded by MaxAuthRequestBytes before json.Decode sees a
//     byte, so a caller cannot stream without limit into the decoder.
//   - Unknown fields are REJECTED. A client that misspells "public_key" gets an
//     error instead of silently enrolling with an empty key.
//   - Trailing content after the JSON value is REJECTED, so there is exactly
//     one request per body and no room for a smuggled second object.
func (s *Server) decodeAuthRequest(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	return s.decodeJSONRequest(w, r, dst, MaxAuthRequestBytes)
}

// decodeJSONRequest is decodeAuthRequest with the byte bound as a parameter, so
// the messaging routes — whose bodies legitimately carry a 64 KiB payload —
// share ONE strict decoder with the auth routes rather than growing a second,
// subtly different one. Every rule documented on decodeAuthRequest applies here
// and is enforced here; that doc comment is the canonical description.
func (s *Server) decodeJSONRequest(w http.ResponseWriter, r *http.Request, dst interface{}, limit int64) bool {
	if !s.requireJSONContentType(w, r) {
		return false
	}

	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.log.Debug("request body too large",
				"request_id", RequestIDFromContext(r.Context()),
				"path", r.URL.Path,
				"limit_bytes", limit,
			)
			s.writeJSON(w, r, http.StatusRequestEntityTooLarge, ErrorResponse{
				Error: fmt.Sprintf("request body exceeds %d bytes", limit),
			})
			return false
		}
		s.log.Debug("request body rejected",
			"request_id", RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
			"err", err,
		)
		// The decoder's own message is NOT returned to the client: it can quote
		// the offending input back, and this is an unauthenticated route.
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "malformed JSON request body"})
		return false
	}

	// Exactly one JSON value per body.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		s.log.Debug("request body has trailing content",
			"request_id", RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
		)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "request body must contain exactly one JSON object"})
		return false
	}
	return true
}

// requireJSONContentType answers 415 unless the request declares JSON.
func (s *Server) requireJSONContentType(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct != "" {
		// ParseMediaType strips and validates any parameters, so
		// "application/json; charset=utf-8" is accepted and
		// "application/json-ish" is not.
		if mt, _, err := mime.ParseMediaType(ct); err == nil && strings.EqualFold(mt, "application/json") {
			return true
		}
	}
	s.writeJSON(w, r, http.StatusUnsupportedMediaType, ErrorResponse{Error: "Content-Type must be application/json"})
	return false
}

// decodeBase64Field decodes a standard-encoding base64 field, answering 400 on
// failure. The field's VALUE is never echoed to the client or the log: these
// fields carry keys and signatures.
//
// Strict() is used so a value has exactly one spelling — the non-strict decoder
// accepts trailing bits that no encoder produces, which would let one key or
// signature be written several ways.
func (s *Server) decodeBase64Field(w http.ResponseWriter, r *http.Request, field, value string) ([]byte, bool) {
	b, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		s.log.Debug("auth request field is not valid base64",
			"request_id", RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
			"field", field,
		)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("%s must be standard base64", field),
		})
		return nil, false
	}
	return b, true
}

// formatInstant renders a timestamp for the wire: UTC, RFC3339 with nanosecond
// precision. One representation everywhere, so a client never has to reason
// about the server's local zone.
func formatInstant(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// encodeBase64 renders opaque bytes for the wire, the exact inverse of
// decodeBase64Field: STANDARD encoding, padded. One spelling in each direction,
// so a body a client sends and the body it reads back are byte-identical
// strings as well as byte-identical bytes.
func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
