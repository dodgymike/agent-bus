package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// PeerRosterPath is the path the ongoing roster sync is EXPECTED to occupy once
// it is gated by INVITE-PEERGUARD and MTLS-RELAYGUARD (see the package doc).
//
// A CONSTANT, NOT A REGISTRATION — exactly like PeerEnrollPath and
// PeerRelayPath.
const PeerRosterPath = "/v1/peer/roster"

// RosterConfig configures the roster-sync ingress.
type RosterConfig struct {
	// BusID is THIS bus's server-minted id, validated once at construction.
	BusID string

	// Apply receives the DECODED update, the peer's idempotency key, and the
	// canonical fingerprint of the update's content. It is where the wiring
	// site calls Registry.ApplyRosterUpdate — and, once the gate tasks land,
	// checks that the authenticated peer is the bus the update claims to
	// describe.
	//
	// # THE KEY AND FINGERPRINT ARE PARAMETERS BECAUSE INVARIANT 10 REQUIRES IT
	//
	// An earlier version validated the key, logged it, and threw it away. That
	// was a real defect, not an omission of convenience: a roster push is a
	// MUTATING operation, so invariant 10's rule applies — same key, same
	// payload is a legitimate retry that must return the ORIGINAL result and
	// must NOT error. With the key discarded, a peer whose acknowledgement was
	// lost in flight retried, hit the version-monotonicity check
	// (u.Version <= st.version) and got a 409 STALE. The retry was punished,
	// which is exactly the behaviour invariant 10 exists to prevent, and it
	// punished specifically the peers that were retrying correctly.
	//
	// The version and the key answer DIFFERENT questions and neither replaces
	// the other. The per-peer version makes a LATE or REORDERED update harmless
	// — it is about ordering. The key makes a REPEATED update harmless — it is
	// about at-most-once application. A cyclic, at-least-once topology produces
	// both, so both are needed.
	//
	// This package deliberately owns NO applied-key table (see
	// RelayConfig.AcceptRelay): internal/idem owns that, its memory is part of
	// recovered state, and a second table here would drift from the durable one.
	// relay's job stops at handing over the key and the fingerprint.
	//
	// Required, for the same reason RelayConfig.AcceptRelay is: a nil callback
	// would make the handler accept an update and silently discard it, which
	// looks exactly like a working sync while the routing table quietly rots.
	Apply func(ctx context.Context, u RosterUpdate, idempotencyKey string, fingerprint idem.Fingerprint) error

	// Logger is optional; nil discards.
	Logger *logging.Logger

	// MaxRequestBytes overrides MaxRosterUpdateBytes. Zero means
	// MaxRosterUpdateBytes; a negative value is a construction error.
	MaxRequestBytes int64
}

// RosterHandler is the responder side of the ongoing roster sync.
//
// IT IS NOT REGISTERED ON ANY MUX AND MUST NOT BE until INVITE-PEERGUARD and
// MTLS-RELAYGUARD land. It authenticates nothing — and this surface in
// particular MUST NOT be served ungated, because Registry.ApplyRosterUpdate's
// "a known peer only" rule is the ONLY thing standing between an anonymous POST
// and our routing table. See the package doc.
type RosterHandler struct {
	busID    string
	apply    func(context.Context, RosterUpdate, string, idem.Fingerprint) error
	log      *logging.Logger
	maxBytes int64
}

// NewRosterHandler validates cfg and returns the roster-sync handler.
func NewRosterHandler(cfg RosterConfig) (*RosterHandler, error) {
	if err := ids.ValidateBusID(cfg.BusID); err != nil {
		return nil, fmt.Errorf("relay: roster handler bus id: %w", err)
	}
	if cfg.Apply == nil {
		return nil, errors.New("relay: RosterConfig.Apply is required; without it the handler would accept a roster update and silently discard it, which looks exactly like a working sync")
	}
	if cfg.MaxRequestBytes < 0 {
		return nil, fmt.Errorf("relay: RosterConfig.MaxRequestBytes is %d; it must be zero (meaning %d) or positive", cfg.MaxRequestBytes, MaxRosterUpdateBytes)
	}
	max := cfg.MaxRequestBytes
	if max == 0 {
		max = MaxRosterUpdateBytes
	}
	log := cfg.Logger
	if log == nil {
		log = logging.New(io.Discard, logging.LevelError)
	}
	return &RosterHandler{busID: cfg.BusID, apply: cfg.Apply, log: log, maxBytes: max}, nil
}

// ServeHTTP implements http.Handler. It authenticates NOTHING.
//
// The status mapping matches the relay ingress so a peer sees one vocabulary
// across all three surfaces, with two roster-specific arms:
//
//   - ErrStaleRosterUpdate is 409, not 400. The update was well-formed; it
//     simply lost a race, or the peer regressed its counter. 409 (conflict with
//     the current state) says "your view and mine disagree", which is the
//     signal that tells an operator to look for a restarted peer that needs to
//     re-handshake — 400 would read as "you sent rubbish", which it did not.
//
//   - ErrUnknownPeer is 403, not 404. Both are "we will not do this"; 403 is
//     the accurate one, because the peer is not unknown to the ROUTE, it is
//     unknown to US.
//
//     AN EARLIER VERSION OF THIS COMMENT CLAIMED 403 AVOIDS A
//     PEER-ENUMERATION ORACLE. THAT WAS WRONG, and the correction is kept
//     visible rather than quietly deleted: the 403-versus-409 SPLIT is itself
//     the oracle. A single POST of {"bus_id":"X","version":1} distinguishes a
//     bus we federate with (409, stale) from one we do not (403), whatever
//     status codes the two arms use. Choosing different codes cannot close
//     that; only authenticating the caller can, which is exactly what the gate
//     tasks do before this handler is reachable at all. Recorded as an accepted
//     residual in the package doc.
func (h *RosterHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.fail(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, fmt.Errorf("relay: %s is not allowed on %s", r.Method, PeerRosterPath))
		return
	}
	if err := checkJSONContentType(r.Header.Get("Content-Type")); err != nil {
		h.fail(w, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, err)
		return
	}

	// A roster push is a MUTATING operation and carries an idempotency key like
	// every other one (invariant 10). It is validated before the body is read.
	idempotencyKey := r.Header.Get(idem.HeaderName)
	if err := idem.ValidateKey(idempotencyKey); err != nil {
		h.fail(w, http.StatusBadRequest, ErrorCode(err), err)
		return
	}

	buf, err := io.ReadAll(io.LimitReader(r.Body, h.maxBytes+1))
	if err != nil {
		h.fail(w, http.StatusBadRequest, CodeInvalidRequest, fmt.Errorf("%w: reading body: %v", ErrInvalidRequest, err))
		return
	}
	if int64(len(buf)) > h.maxBytes {
		h.fail(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, fmt.Errorf("%w: body exceeds %d bytes", ErrPayloadTooLarge, h.maxBytes))
		return
	}
	var u RosterUpdate
	if err := decodeStrict(buf, &u); err != nil {
		h.fail(w, http.StatusBadRequest, ErrorCode(err), err)
		return
	}

	// The claimed bus id is checked HERE as well as inside the registry, so a
	// peer claiming OUR namespace is refused before the callback — the callback
	// is the wiring site's code, and the id rules must not depend on it having
	// remembered to apply them.
	if err := ValidatePeerBusID(h.busID, u.BusID); err != nil {
		h.fail(w, http.StatusBadRequest, ErrorCode(err), err)
		return
	}

	if err := h.apply(r.Context(), u, idempotencyKey, RosterUpdateFingerprint(u)); err != nil {
		switch {
		case errors.Is(err, ErrIdempotencyViolation):
			// The same arm RelayHandler has, for the same reason: invariant 10
			// makes this a PROTOCOL VIOLATION, not a service failure. Without
			// this case it fell through to the 503 default, which tells a peer
			// that reused a key with new content to RETRY — the one response
			// that guarantees it keeps doing the thing being refused.
			h.log.Warn("roster update REJECTED: idempotency key reused with a different payload — THE SENDING PEER SHOULD BE DISCONNECTED (invariant 10); this handler cannot close the connection, so the gate task MTLS-RELAYGUARD (8192c3c7) must wire that",
				"local_bus", h.busID,
				"peer_bus", u.BusID,
				"version", u.Version,
			)
			h.fail(w, http.StatusConflict, CodeIdempotencyViolation, err)
		case errors.Is(err, ErrUnknownPeer):
			h.fail(w, http.StatusForbidden, CodeUnknownPeer, err)
		case errors.Is(err, ErrStaleRosterUpdate):
			h.fail(w, http.StatusConflict, CodeStaleRoster, err)
		case errors.Is(err, ErrPeerBusIDCollision):
			// Named explicitly so a registry-level refusal surfaced by the
			// callback is the 400 it is rather than the 503 the default would
			// make it: a confusable bus id is the PEER's to fix, and retrying
			// it will never succeed. Unreachable through ApplyRosterUpdate
			// today; reachable the moment a wiring site upserts a peer here.
			//
			// ErrTooManyPeers is deliberately NOT in this arm and falls to the
			// 503 default, matching ErrorCode's own judgement that a capacity
			// refusal is "not now" rather than "never" — the one registry
			// failure a peer SHOULD retry.
			h.fail(w, http.StatusBadRequest, ErrorCode(err), err)
		case errors.Is(err, ErrInvalidRosterUpdate), errors.Is(err, ErrBusIDCollision),
			errors.Is(err, ErrRosterTooLarge), errors.Is(err, ErrInvalidRoster):
			h.fail(w, http.StatusBadRequest, ErrorCode(err), err)
		default:
			h.fail(w, http.StatusServiceUnavailable, CodeUnavailable, err)
		}
		return
	}

	h.log.Info("peer roster update accepted",
		"local_bus", h.busID,
		"peer_bus", u.BusID,
		"version", u.Version,
		"added", len(u.Added),
		"removed", len(u.Removed),
		"idempotency_key", idempotencyKey,
	)
	writeJSONBody(w, h.log, http.StatusOK, RosterUpdateResponse{Applied: true, Version: u.Version})
}

// RosterUpdateResponse is the 200 body of a roster push. It echoes the version
// that is now in force, so the pusher can detect — without a second round trip
// — that its counter and ours have diverged.
type RosterUpdateResponse struct {
	Applied bool   `json:"applied"`
	Version uint64 `json:"version"`
}

func (h *RosterHandler) fail(w http.ResponseWriter, status int, code string, err error) {
	failJSON(w, h.log.With("surface", "peer-roster", "local_bus", h.busID), status, code, err)
}

// PushRoster sends one incremental roster update to a peer.
//
// idempotencyKey is the caller's, in the idem.HeaderName header (invariant 10).
// Unlike Relay — where the key IS the origin message id, because that identity
// is what makes cross-route dedupe work — a roster push has no natural
// server-minted id to reuse, so the caller mints one and reuses it across the
// RETRIES OF ONE UPDATE. The per-peer version is what makes a LATE or
// DUPLICATED update harmless independently of the key.
//
// It inherits NewClient's redirect refusal, and must: a peer answering 307
// would otherwise replay our roster deltas at an attacker-chosen host.
//
// # THIS CALL BLOCKS ON THE PEER. NEVER CALL IT ON A REQUEST PATH.
//
// It is a bare synchronous POST, and that is a deliberate asymmetry with the
// message path rather than an oversight: Forwarder makes relaying a message
// STRUCTURALLY unable to slow a local send (its Enqueue cannot block), whereas
// this method has no such protection and will happily wait out a peer's dial
// timeout.
//
// The constraint it can therefore break is the same one Forwarder exists to
// honour — a slow or dead peer must never make a LOCAL operation slow or fail
// (RELAY-4). The tempting wiring is exactly the wrong one: calling PushRoster
// inline from enrol or leave, so that one unreachable peer adds its full
// timeout to every agent's enrolment, and a peer that is down makes enrolment
// FAIL for a reason that has nothing to do with the enrolling agent.
//
// So the wiring site MUST call this from its own background goroutine or queue,
// and must treat a failure as a missed sync to be retried later — never as a
// failure of the roster change that triggered it. The roster change is already
// durable locally; propagating it is best effort. A background pusher with the
// Forwarder's shape (bounded per-peer queue, non-blocking offer) is the natural
// home for that and is a RELAY-4 follow-up, deliberately not built here.
func (c *Client) PushRoster(ctx context.Context, peerBaseURL string, u RosterUpdate, idempotencyKey string) error {
	if err := idem.ValidateKey(idempotencyKey); err != nil {
		return err
	}
	if u.BusID != c.busID {
		// We describe OUR OWN roster and nobody else's — the same rule we
		// enforce on the way in, applied on the way out so a bug on this bus
		// cannot make us assert a third bus's membership to a peer.
		return fmt.Errorf("relay: a roster push describes this bus (%q), but the update names %q; a bus speaks only for its own agents", c.busID, u.BusID)
	}
	if n := len(u.Added) + len(u.Removed); n > MaxRosterUpdateEntries {
		return fmt.Errorf("%w: %d entries, over the %d cap; send a fresh handshake instead", ErrInvalidRosterUpdate, n, MaxRosterUpdateEntries)
	}
	endpoint, err := peerURL(peerBaseURL, PeerRosterPath)
	if err != nil {
		return err
	}

	body, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("relay: encoding roster update: %w", err)
	}
	if len(body) > MaxRosterUpdateBytes {
		return fmt.Errorf("%w: encoded roster update is %d bytes, over the %d cap", ErrPayloadTooLarge, len(body), MaxRosterUpdateBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("relay: building roster update request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(idem.HeaderName, idempotencyKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("relay: pushing roster update to %s: %w", endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxRosterUpdateBytes))
		_ = resp.Body.Close()
	}()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, MaxRosterUpdateBytes+1))
	if err != nil {
		return fmt.Errorf("relay: reading roster update response from %s: %w", endpoint, err)
	}
	if len(buf) > MaxRosterUpdateBytes {
		return fmt.Errorf("%w: response from %s exceeds %d bytes", ErrPayloadTooLarge, endpoint, MaxRosterUpdateBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s returned %d (%s)", ErrPeerRefused, endpoint, resp.StatusCode, peerErrorCode(buf))
	}
	var decoded RosterUpdateResponse
	if err := decodeStrict(buf, &decoded); err != nil {
		return fmt.Errorf("decoding roster update response from %s: %w", endpoint, err)
	}
	if !decoded.Applied {
		return fmt.Errorf("%w: %s answered 200 but did not apply the update", ErrPeerRefused, endpoint)
	}
	return nil
}
