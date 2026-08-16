package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// ErrPeerRefused reports a handshake the peer answered with a non-200. It
// wraps, so a caller can tell a refusal apart from a transport failure or a
// malformed reply, and carries the peer's stable error code when it sent one.
var ErrPeerRefused = errors.New("relay: peer refused the handshake")

// PeerRefusedError is a refusal that KEPT THE STATUS CODE, and it exists for
// exactly one reason: RELAY-4's retry loop cannot be correct without it.
//
// Every non-200 used to collapse into one opaque wrapped ErrPeerRefused, which
// leaves a retry policy two equally wrong choices. Retry everything, and a 400
// malformed envelope or a 409 idempotency violation is re-sent until the horizon
// runs out — the traffic amplification relayhttp.go's status argument exists to
// prevent, with the control acting as the amplifier. Retry nothing, and a 503
// from a peer that is merely restarting is discarded, which is the entire case
// RELAY-4 was written for.
//
// It wraps ErrPeerRefused, so every existing errors.Is(err, ErrPeerRefused)
// check keeps working unchanged.
type PeerRefusedError struct {
	// Endpoint is the URL that refused, for the log line.
	Endpoint string
	// StatusCode is the peer's HTTP status.
	StatusCode int
	// Code is the peer's stable error code when it sent one; "" otherwise. It
	// is UNTRUSTED peer input and is only ever logged, never branched on —
	// retriability is decided from the status alone, which is the one field the
	// HTTP layer itself defines.
	Code string
}

func (e *PeerRefusedError) Error() string {
	return fmt.Sprintf("%s: %s returned %d (%s)", ErrPeerRefused.Error(), e.Endpoint, e.StatusCode, e.Code)
}

// Unwrap is what keeps errors.Is(err, ErrPeerRefused) true.
func (e *PeerRefusedError) Unwrap() error { return ErrPeerRefused }

// Retriable reports whether re-sending the identical request could ever produce
// a different answer.
//
// The split is "not now" versus "never", and it is decided from the STATUS, not
// from the peer's error code string:
//
//   - 5xx is NOT NOW. The peer is broken, restarting, or shedding load; the same
//     bytes may well be accepted in a minute. relayhttp.go maps every unclassified
//     AcceptRelay failure to 503 precisely so a peer can tell this apart.
//   - 429 and 408 are NOT NOW for the same reason: both name a condition of the
//     moment rather than of the message.
//   - EVERY OTHER 4xx IS NEVER. 400 (malformed envelope), 403 (bad signature or
//     unpeered bus), 409 (idempotency violation) and 413 (too large) are verdicts
//     on THIS message's content, and no amount of resending changes its content.
//   - 3xx IS NEVER, emphatically. Client.NewClient installs
//     http.ErrUseLastResponse so a redirect is surfaced as a refusal rather than
//     followed; retrying one would just re-present the SSRF the redirect policy
//     was added to close.
//
// Note what is NOT here: a 200 carrying accepted:false, dropped_reason=loop. That
// is a SETTLED answer with a nil error and never reaches this method — which is
// the whole reason relayhttp.go insists a loop is a 200.
func (e *PeerRefusedError) Retriable() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return e.StatusCode >= 500 && e.StatusCode <= 599
}

// ClientConfig configures the initiator side of the handshake.
type ClientConfig struct {
	// BusID is THIS bus's server-minted id (invariant 1): what we claim to the
	// peer, and what we measure the peer's reply against.
	BusID string

	// LocalRoster returns a snapshot of this bus's fully-qualified agent ids.
	// Required, and safe for concurrent use — see Config.LocalRoster.
	LocalRoster func() []string

	// HTTPClient carries the TLS configuration for the link. It is REQUIRED and
	// deliberately not defaulted: bus-to-bus links are mutually authenticated
	// TLS (invariant 11, MTLS-RELAYGUARD), so the certificate material is the
	// caller's to supply, and a silent http.DefaultClient would quietly produce
	// an unauthenticated link that still works — the worst possible default.
	HTTPClient *http.Client

	// Logger is optional; nil discards.
	Logger *logging.Logger
}

// Client is the initiator side of the peer handshake.
//
// Like Handler it authenticates nothing by itself: it requires https and it
// requires the caller to hand it a TLS-configured http.Client, but proving who
// answered is MTLS-RELAYGUARD's job, and proving we are entitled to enrol is
// INVITE-PEERGUARD's. See the package doc.
type Client struct {
	busID       string
	localRoster func() []string
	httpClient  *http.Client
	log         *logging.Logger
}

// NewClient validates cfg and returns the initiator.
func NewClient(cfg ClientConfig) (*Client, error) {
	if err := ids.ValidateBusID(cfg.BusID); err != nil {
		return nil, fmt.Errorf("relay: client bus id: %w", err)
	}
	if cfg.LocalRoster == nil {
		return nil, errors.New("relay: ClientConfig.LocalRoster is required; an absent roster provider must not be indistinguishable from an empty roster")
	}
	if cfg.HTTPClient == nil {
		return nil, errors.New("relay: ClientConfig.HTTPClient is required; it carries the mutual-TLS configuration of the link, and defaulting it would silently produce an unauthenticated one")
	}
	log := cfg.Logger
	if log == nil {
		log = logging.New(io.Discard, logging.LevelError)
	}

	// A handshake NEVER follows a redirect. Go's default policy would replay
	// the POST — bus id and full roster — at whatever host a 3xx names, over
	// whatever scheme it names, on a fresh connection whose identity was never
	// checked. That defeats the https requirement below (which only ever sees
	// the URL we were given) and would defeat MTLS-RELAYGUARD in advance, since
	// the redirect target's certificate was never the one we meant to pin.
	// Returning ErrUseLastResponse surfaces the 3xx as an ordinary refusal.
	//
	// The caller's client is COPIED rather than mutated: it belongs to the
	// caller, is likely shared, and silently rewriting its redirect policy
	// would be a side effect on somebody else's object.
	hc := *cfg.HTTPClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &Client{busID: cfg.BusID, localRoster: cfg.LocalRoster, httpClient: &hc, log: log}, nil
}

// Enroll performs the handshake against peerBaseURL and returns the VALIDATED
// peer roster.
//
// peerBaseURL is the peer's base URL ("https://peer.example:8443"); Enroll
// appends PeerEnrollPath itself, so the path is never a caller's typo. The
// scheme must be https: there is no plaintext listener anywhere in this system
// (invariant 11), and a bus id plus a full roster is exactly the material an
// on-path observer would want.
//
// idempotencyKey is sent in the idem.HeaderName header — the one canonical
// carrier (internal/idem, IDEM-10) — and makes the call safe to retry
// (invariant 10). A retried
// handshake is the steady state in a cyclic peer topology, not an exception, so
// the caller should reuse ONE key for the retries of one logical enrolment and
// mint a new one only for a new enrolment.
func (c *Client) Enroll(ctx context.Context, peerBaseURL, idempotencyKey string) (PeerRoster, error) {
	if err := idem.ValidateKey(idempotencyKey); err != nil {
		return PeerRoster{}, err
	}
	endpoint, err := peerEnrollURL(peerBaseURL)
	if err != nil {
		return PeerRoster{}, err
	}
	roster, err := validateLocalRoster(c.busID, c.localRoster())
	if err != nil {
		return PeerRoster{}, err
	}

	body, err := json.Marshal(PeerEnrollRequest{
		BusID:  c.busID,
		Agents: roster,
	})
	if err != nil {
		return PeerRoster{}, fmt.Errorf("relay: encoding peer handshake: %w", err)
	}
	if len(body) > MaxHandshakeBytes {
		// Cannot happen once validateLocalRoster has passed — MaxHandshakeBytes
		// is derived from MaxRosterAgents precisely so it cannot — but sending
		// a body we would refuse to receive would be an asymmetry that only
		// ever shows up as a confusing 413 from the far end.
		return PeerRoster{}, fmt.Errorf("%w: encoded handshake is %d bytes, over the %d cap", ErrPayloadTooLarge, len(body), MaxHandshakeBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return PeerRoster{}, fmt.Errorf("relay: building peer handshake request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// The ONE canonical carrier for the key (internal/idem): a header, never a
	// body field, and never both.
	req.Header.Set(idem.HeaderName, idempotencyKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return PeerRoster{}, fmt.Errorf("relay: peer handshake to %s: %w", endpoint, err)
	}
	defer func() {
		// Drain a bounded amount so the connection can be reused, then close.
		// The drain is bounded for the same reason the read below is: the peer
		// chooses how much it sends.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxHandshakeBytes))
		_ = resp.Body.Close()
	}()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, MaxHandshakeBytes+1))
	if err != nil {
		return PeerRoster{}, fmt.Errorf("relay: reading peer handshake response from %s: %w", endpoint, err)
	}
	if len(buf) > MaxHandshakeBytes {
		return PeerRoster{}, fmt.Errorf("%w: response from %s exceeds %d bytes", ErrPayloadTooLarge, endpoint, MaxHandshakeBytes)
	}

	if resp.StatusCode != http.StatusOK {
		return PeerRoster{}, &PeerRefusedError{Endpoint: endpoint, StatusCode: resp.StatusCode, Code: peerErrorCode(buf)}
	}

	var decoded PeerEnrollResponse
	if err := decodeStrict(buf, &decoded); err != nil {
		return PeerRoster{}, fmt.Errorf("decoding peer handshake response from %s: %w", endpoint, err)
	}

	if decoded.Count != len(decoded.Agents) {
		// Count exists so a truncated or mis-assembled response is caught
		// rather than quietly federating a short roster — an agent missing
		// from a roster is an agent whose messages misroute until someone
		// notices. A field nobody checks would be decoration.
		return PeerRoster{}, fmt.Errorf("%w: %s reported count=%d for %d agents", ErrInvalidRoster, endpoint, decoded.Count, len(decoded.Agents))
	}

	peer, err := ValidatePeerEnrollResponse(c.busID, decoded)
	if err != nil {
		return PeerRoster{}, err
	}
	c.log.Info("peer handshake completed",
		"local_bus", c.busID,
		"peer_bus", peer.BusID,
		"peer_agents", len(peer.Agents),
		"local_agents", len(roster),
		"idempotency_key", idempotencyKey,
	)
	return peer, nil
}

// peerEnrollURL turns a peer base URL into the handshake endpoint. It is a thin
// wrapper over peerURL, kept so the handshake's call sites and tests name the
// endpoint they mean.
func peerEnrollURL(base string) (string, error) { return peerURL(base, PeerEnrollPath) }

// peerURL turns a peer base URL plus one of this package's path constants into
// an endpoint, rejecting anything that is not a plain https origin.
//
// EVERY bus-to-bus request in this package goes through here, which is the
// point: the https requirement (invariant 11 — there is no plaintext listener,
// and a roster, a sender id and a message body are exactly the material an
// on-path observer would want) is enforced once, and a new surface cannot
// accidentally ship without it. path is always a constant from this package,
// never caller input, so the endpoint's path can never be a caller's typo.
func peerURL(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("relay: peer base URL %q is unparseable: %v", base, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("relay: peer base URL %q has scheme %q, but a bus-to-bus link is always https — there is no plaintext listener (invariant 11)", base, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("relay: peer base URL %q has no host", base)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("relay: peer base URL %q must be a bare origin: no query, no fragment, no userinfo", base)
	}
	return strings.TrimRight(u.String(), "/") + path, nil
}

// peerErrorCode extracts the peer's stable error code from an error body, for
// the message only. An unparseable or unexpected body yields a placeholder
// rather than an echo: the far end is untrusted, including when it is failing.
func peerErrorCode(buf []byte) string {
	var body ErrorBody
	if err := json.Unmarshal(buf, &body); err != nil {
		return "no parseable error code"
	}
	switch body.Error {
	case CodeMethodNotAllowed, CodeUnsupportedMediaType, CodePayloadTooLarge,
		CodeInvalidRequest, CodeInvalidBusID, CodeBusIDCollision,
		CodeInvalidRoster, CodeRosterTooLarge, CodeInvalidIdempotencyKey,
		CodePeerRejected, CodeUnavailable, CodeInternal,
		// The relay and roster-sync codes are recognised here too: this
		// allow-list is what keeps an unrecognised string OUT of our log, so a
		// code we genuinely emit but do not list would be reported as
		// "unrecognised" and would send an operator looking for a bug that is
		// not there.
		CodeRelayLoop, CodeInvalidBusPath, CodeInvalidRelay,
		CodeIdempotencyViolation, CodeUnknownPeer, CodeStaleRoster,
		CodeInvalidRosterUpdate,
		// Signed relay ingest (SIGN-7, handshake.go) is a THIRD such surface.
		// RelayHandler emits all three of these on the responder side
		// (relayhttp.go) whenever it cannot attribute an envelope to the agent
		// it names or holds no peering-time pin for the origin bus — they are
		// FINAL, operator-actionable verdicts, not internal failures, and
		// belong in this log-legibility allow-list for the same reason as
		// every code above: without them here a peer's signature refusal is
		// reported as "unrecognised error code" instead of the real reason
		// (RELAY-9).
		CodeUnsigned, CodeBadSignature, CodeUnpeeredBus,
		// RELAY-21's ingest refusal, for the same reason: the responder emits
		// it whenever a message names an agent in its namespace that its roster
		// does not hold, and an operator whose federation is mis-rostered must
		// read that reason rather than "unrecognised error code".
		CodeUnknownRecipient,
		// ACK-3's version refusal, and this one matters MORE than most: it is
		// emitted precisely during a PARTIAL ROLLOUT, when the two buses run
		// different binaries, and failJSON puts only the code on the wire — so
		// this string IS the entire diagnosis the sending operator ever gets.
		// Omitted from this list it would arrive as "unrecognised error code",
		// which is the one message guaranteed to send them looking in the wrong
		// place. A security gate caught the omission (ACK-3, 2026-08-16).
		CodeUnsupportedAckVersion:
		return body.Error
	default:
		return "unrecognised error code"
	}
}
