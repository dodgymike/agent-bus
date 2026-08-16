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

	"github.com/dodgymike/agent-bus/internal/idem"
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

	// The relay and roster-sync surfaces (RELAY-2, RELAY-3) extend the same
	// stable-code vocabulary rather than starting a second one: a peer operator
	// reads one list, and ErrorCode stays the single mapping from a sentinel to
	// the string on the wire.
	CodeRelayLoop            = "relay_loop"
	CodeInvalidBusPath       = "invalid_bus_path"
	CodeInvalidRelay         = "invalid_relay"
	CodeIdempotencyViolation = "idempotency_violation"
	CodeUnknownPeer          = "unknown_peer"
	CodeStaleRoster          = "stale_roster"
	CodeInvalidRosterUpdate  = "invalid_roster_update"

	// Signed relay ingest (SIGN-7). THREE codes, not one, because they blame
	// three different things and carry two different statuses:
	//
	//   - CodeUnsigned (400) says the ENVELOPE could never be verified by anyone
	//     — no signature, a malformed one, or a shape the canonical format cannot
	//     encode.
	//   - CodeBadSignature (403) says the envelope is well-formed but is NOT
	//     ATTRIBUTABLE to the agent it names, either because the origin bus
	//     attests no key for that agent or because the signature does not verify
	//     against the key it does attest.
	//   - CodeUnpeeredBus (403) says we hold NO PEERING-TIME PIN for the origin
	//     bus's SIGNING key, so nothing that bus's agents sign can be verified
	//     here at all. It is separate from CodeBadSignature because it is a
	//     different OPERATOR problem with a different remedy: peer the two buses,
	//     as against investigate a forgery. There is deliberately no
	//     trust-on-first-use path that would make this code unreachable.
	//
	// ALL THREE ARE FINAL. None invites a retry: resending identical bytes cannot
	// change any of the verdicts, and a peer that reads these codes must stop.
	CodeUnsigned     = "unsigned"
	CodeBadSignature = "bad_signature"
	CodeUnpeeredBus  = "unpeered_bus"

	// CodeUnknownRecipient (404) says the envelope names an agent in OUR
	// namespace that our roster does not hold, so NOTHING was written
	// (ErrUnknownLocalRecipient, RELAY-21). It is its own code, and not
	// CodeInvalidRelay, because the envelope is perfectly well formed and the
	// remedy is a roster the sending bus can fix — a peer told "invalid_relay"
	// would go looking for a malformed field it does not have.
	CodeUnknownRecipient = "unknown_recipient"

	// CodeUnsupportedAckVersion (400) says the ACK frame declares an
	// acknowledgement wire-protocol version this bus does not implement
	// (ErrUnsupportedAckVersion, ACK-3). Nothing was read beyond the version
	// itself: an unrecognised version is refused, never defaulted.
	//
	// It is its own code for the same reason CodeUnknownRecipient is: the frame
	// is not malformed, and the remedy is not in the sender's encoder. The remedy
	// is that one of the two buses is running an older binary — an OPERATOR
	// action at whichever end is behind — and since failJSON puts only this code
	// on the wire, a code pointing at a malformed field would be the entire, and
	// entirely wrong, diagnosis the far-end operator ever sees.
	//
	// IT IS FINAL. Resending identical bytes cannot change the verdict, and a
	// 4xx tells the sending bus to stop rather than spend its whole horizon
	// re-offering a frame the far end can never read.
	//
	// # WHY IT NAMES THE ACK FRAME RATHER THAN "the relay wire version"
	//
	// RELAY-23 adds the SAME field to the relay envelope, spending the SAME
	// reserved relay-wire-version = 1, and brings its own
	// CodeUnsupportedRelayVersion. That work was unmerged when this constant
	// landed, so sharing its name would have been a duplicate declaration git
	// could not flag — two files, one package, no textual conflict, and a build
	// that breaks only after the merge. Two codes also happen to be the more
	// useful answer: a peer operator reads WHICH FRAME the far end could not
	// parse, rather than having to guess. A follow-up may collapse them if an
	// operator would rather read one string.
	CodeUnsupportedAckVersion = "unsupported_ack_version"
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

	// The idempotency key is read and validated BEFORE the body, which is the
	// reason internal/idem puts it in a header: an oversized or malformed key
	// is refused by the transport-level check without a byte of a possibly
	// large JSON body being read into memory (invariant 10, idem doc point 1).
	idempotencyKey := r.Header.Get(idem.HeaderName)
	if err := idem.ValidateKey(idempotencyKey); err != nil {
		h.fail(w, http.StatusBadRequest, ErrorCode(err), err)
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

	peer, err := ValidatePeerEnrollRequest(h.busID, idempotencyKey, req)
	if err != nil {
		// Everything the peer can get wrong here is a 400, including a roster
		// over MaxRosterAgents: 413 would tell the peer to send fewer BYTES,
		// when the limit it hit is a COUNT and its body may be a fraction of
		// the byte cap. The stable code says which limit it was.
		//
		// A CodeInternal here is OUR bug (a local bus id that stopped
		// validating), not the peer's, so it must not be reported as a bad
		// request: a peer told "400" would stop retrying a fault it did not
		// cause and cannot fix.
		status := http.StatusBadRequest
		code := ErrorCode(err)
		if code == CodeInternal {
			status = http.StatusInternalServerError
		}
		h.fail(w, status, code, err)
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

// fail and writeJSON delegate to the free functions in httputil.go, which the
// relay-ingress and roster-sync handlers share. The behaviour is unchanged; the
// surface is one place instead of three, so the "code on the wire, detail in the
// log" posture cannot drift between the three bus-to-bus handlers. The logger is
// decorated with the handshake's own context so the log line still names the
// surface and the local bus.
func (h *Handler) fail(w http.ResponseWriter, status int, code string, err error) {
	failJSON(w, h.log.With("surface", "peer-enroll", "local_bus", h.busID), status, code, err)
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, body interface{}) {
	writeJSONBody(w, h.log.With("surface", "peer-enroll", "local_bus", h.busID), status, body)
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
