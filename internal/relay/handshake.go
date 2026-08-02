package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// The stable error codes returned in a handshake error body. They are the
// machine-readable half of a failure; the human-readable half stays in our log.
const (
	CodeMethodNotAllowed      = "method_not_allowed"
	CodeUnsupportedMediaType  = "unsupported_media_type"
	CodePayloadTooLarge       = "payload_too_large"
	CodeInvalidRequest        = "invalid_request"
	CodeInvalidBusID          = "invalid_bus_id"
	CodeBusIDCollision        = "bus_id_collision"
	CodeInvalidRoster         = "invalid_roster"
	CodeRosterTooLarge        = "roster_too_large"
	CodeInvalidIdempotencyKey = "invalid_idempotency_key"
	CodePeerRejected          = "peer_rejected"
	CodeUnavailable           = "unavailable"
	CodeInternal              = "internal_error"
)

// ErrPeerRejected is what an AcceptPeer callback returns to DECLINE a peer that
// was otherwise well-formed — the shape the invite gate (INVITE-PEERGUARD) will
// use to say "this peer redeemed nothing". It is distinguished from an internal
// failure so a declined peer gets 403 and a broken responder gets 503: a peer
// must be able to tell "you will not have me" from "try again later".
var ErrPeerRejected = errors.New("relay: peer rejected")

// ErrorBody is the JSON body of every non-200 handshake response.
type ErrorBody struct {
	// Error is one of the Code* constants. It never echoes peer input.
	Error string `json:"error"`
}

// Config configures the responder side of the handshake.
type Config struct {
	// BusID is THIS bus's server-minted id (invariant 1). It is what the
	// validator measures every peer claim against, so it is validated once at
	// construction rather than trusted per request.
	BusID string

	// LocalRoster returns a snapshot of this bus's fully-qualified agent ids.
	// It is called once per handshake and must be safe for concurrent use — an
	// HTTP handler serves requests in parallel. Returning a fresh slice is the
	// simplest way to be safe; the handler copies nothing back into it.
	//
	// Required: a nil LocalRoster is a construction error rather than an
	// implicit empty roster, because "this bus has no agents" and "nobody wired
	// the roster up" must not look identical to a federating peer.
	LocalRoster func() []string

	// AcceptPeer receives the VALIDATED peer roster. It is where the
	// registration site records the peer — and, once INVITE-PEERGUARD lands,
	// where invite redemption is checked; returning ErrPeerRejected declines
	// the peer.
	//
	// Required, for the same reason as LocalRoster: a nil callback would make
	// the handler silently discard every roster it validated, which looks
	// exactly like a working handshake.
	AcceptPeer func(ctx context.Context, peer PeerRoster) error

	// Logger receives the detailed, peer-supplied-byte-quoting failures that
	// the wire response deliberately omits. Optional; nil discards.
	Logger *logging.Logger

	// MaxRequestBytes overrides MaxHandshakeBytes, for tests that want to prove
	// the bound without allocating a quarter of a megabyte. Zero means
	// MaxHandshakeBytes; a negative value is a construction error.
	MaxRequestBytes int64
}

// Handler is the responder side of the peer handshake.
//
// IT IS NOT REGISTERED ON ANY MUX AND MUST NOT BE until INVITE-PEERGUARD and
// MTLS-RELAYGUARD land — it authenticates nothing. See the package doc.
type Handler struct {
	busID       string
	localRoster func() []string
	acceptPeer  func(context.Context, PeerRoster) error
	log         *logging.Logger
	maxBytes    int64
}

// NewHandler validates cfg and returns the handshake handler.
func NewHandler(cfg Config) (*Handler, error) {
	if err := ids.ValidateBusID(cfg.BusID); err != nil {
		return nil, fmt.Errorf("relay: handler bus id: %w", err)
	}
	if cfg.LocalRoster == nil {
		return nil, errors.New("relay: Config.LocalRoster is required; an absent roster provider must not be indistinguishable from an empty roster")
	}
	if cfg.AcceptPeer == nil {
		return nil, errors.New("relay: Config.AcceptPeer is required; without it the handler would validate a peer roster and silently discard it")
	}
	if cfg.MaxRequestBytes < 0 {
		return nil, fmt.Errorf("relay: Config.MaxRequestBytes is %d; it must be zero (meaning %d) or positive", cfg.MaxRequestBytes, MaxHandshakeBytes)
	}
	max := cfg.MaxRequestBytes
	if max == 0 {
		max = MaxHandshakeBytes
	}
	log := cfg.Logger
	if log == nil {
		log = logging.New(io.Discard, logging.LevelError)
	}
	return &Handler{
		busID:       cfg.BusID,
		localRoster: cfg.LocalRoster,
		acceptPeer:  cfg.AcceptPeer,
		log:         log,
		maxBytes:    max,
	}, nil
}

// ServeHTTP implements http.Handler.
//
// It authenticates NOTHING. Every caller reaching here is anonymous, which is
// precisely why nothing serves it yet.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.fail(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, fmt.Errorf("relay: %s is not allowed on %s", r.Method, PeerEnrollPath))
		return
	}
	if err := checkJSONContentType(r.Header.Get("Content-Type")); err != nil {
		h.fail(w, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, err)
		return
	}

	req, err := h.decodeRequest(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrPayloadTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		h.fail(w, status, ErrorCode(err), err)
		return
	}

	peer, err := ValidatePeerEnrollRequest(h.busID, req)
	if err != nil {
		h.fail(w, http.StatusBadRequest, ErrorCode(err), err)
		return
	}

	// Our own roster is assembled BEFORE the peer is accepted, so a bus that
	// cannot describe itself never records a peer it then failed to answer.
	resp, err := h.localResponse()
	if err != nil {
		h.fail(w, http.StatusServiceUnavailable, CodeUnavailable, err)
		return
	}

	if err := h.acceptPeer(r.Context(), peer); err != nil {
		if errors.Is(err, ErrPeerRejected) {
			h.fail(w, http.StatusForbidden, CodePeerRejected, err)
			return
		}
		h.fail(w, http.StatusServiceUnavailable, CodeUnavailable, err)
		return
	}

	h.log.Info("peer handshake accepted",
		"local_bus", h.busID,
		"peer_bus", peer.BusID,
		"peer_agents", len(peer.Agents),
		"local_agents", len(resp.Agents),
		"idempotency_key", peer.IdempotencyKey,
	)
	h.writeJSON(w, http.StatusOK, resp)
}

// decodeRequest reads at most maxBytes and decodes strictly.
//
// The read is bounded by an io.LimitReader over the body: Content-Length is a
// claim the peer makes about itself, and a chunked body does not have to make
// it at all. Reading limit+1 bytes is what distinguishes "exactly at the cap"
// from "over it" without ever buffering more than one byte of excess.
//
// Decoding is strict — unknown fields are rejected and trailing bytes after the
// object are rejected — because there is no version field to negotiate with
// (see the package doc). An unrecognised field means the sender believes
// something about this protocol that is not true, and failing loudly is the
// only honest answer available.
func (h *Handler) decodeRequest(body io.Reader) (PeerEnrollRequest, error) {
	buf, err := io.ReadAll(io.LimitReader(body, h.maxBytes+1))
	if err != nil {
		return PeerEnrollRequest{}, fmt.Errorf("%w: reading body: %v", ErrInvalidRequest, err)
	}
	if int64(len(buf)) > h.maxBytes {
		return PeerEnrollRequest{}, fmt.Errorf("%w: body exceeds %d bytes", ErrPayloadTooLarge, h.maxBytes)
	}

	var req PeerEnrollRequest
	if err := decodeStrict(buf, &req); err != nil {
		return PeerEnrollRequest{}, err
	}
	return req, nil
}

// localResponse snapshots our roster and checks it before publishing it.
//
// Our own ids are validated on the way OUT for two reasons: a malformed local
// id would teach a peer to route to something that cannot exist, and this is
// the cheapest place to notice that a roster provider handed us something from
// the wrong namespace. It is a server bug if it fires, so it is reported as one
// (503) rather than silently filtered — dropping the bad entry would federate a
// roster that is quietly missing an agent, which misroutes messages for as long
// as nobody looks.
func (h *Handler) localResponse() (PeerEnrollResponse, error) {
	out, err := validateLocalRoster(h.busID, h.localRoster())
	if err != nil {
		return PeerEnrollResponse{}, err
	}
	return PeerEnrollResponse{BusID: h.busID, Agents: out, Count: len(out)}, nil
}

func (h *Handler) fail(w http.ResponseWriter, status int, code string, err error) {
	h.log.Warn("peer handshake rejected",
		"local_bus", h.busID,
		"status", status,
		"code", code,
		"err", err.Error(),
	)
	h.writeJSON(w, status, ErrorBody{Error: code})
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, body interface{}) {
	buf, err := json.Marshal(body)
	if err != nil {
		// Both body types are plain structs of strings and ints, so this cannot
		// fire; if it ever does, do not emit a half-written body.
		h.log.Error("peer handshake response could not be encoded", "err", err.Error())
		http.Error(w, `{"error":"`+CodeInternal+`"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// checkJSONContentType requires a JSON media type. A handshake body is always
// JSON, and requiring the header keeps a form-encoded cross-origin POST from
// reaching the decoder at all.
func checkJSONContentType(header string) error {
	if header == "" {
		return fmt.Errorf("%w: a peer handshake must set Content-Type: application/json", ErrInvalidRequest)
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return fmt.Errorf("%w: unparseable Content-Type: %v", ErrInvalidRequest, err)
	}
	if !strings.EqualFold(mediaType, "application/json") {
		return fmt.Errorf("%w: Content-Type %q is not application/json", ErrInvalidRequest, mediaType)
	}
	return nil
}

// decodeStrict decodes exactly one JSON object from buf into v, rejecting
// unknown fields and anything following the object.
func decodeStrict(buf []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: trailing data after the JSON object", ErrInvalidRequest)
	}
	return nil
}
