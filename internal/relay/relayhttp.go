package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// RelayAcceptance is what an AcceptRelay callback reports back: what THIS bus
// did with a validated relayed message.
type RelayAcceptance struct {
	// LocalMessageID is the id this bus minted for its OWN copy (invariant 1 —
	// ids are minted by the server that owns the namespace, never carried in
	// from a peer). It is never the origin's id: PROTOCOL.md §8.5 has the
	// receiving bus mint its own local delivery sequence outside the relayed
	// envelope, so neither bus cedes id authority to the other.
	LocalMessageID string

	// Duplicate reports idem.OutcomeRetry — the same key with the SAME payload
	// was already applied, and LocalMessageID is the ORIGINAL result being
	// replayed. Nothing was applied a second time and nobody is disconnected
	// (invariant 10).
	Duplicate bool
}

// ErrIdempotencyViolation is what an AcceptRelay callback returns for
// idem.OutcomeViolation: the same idempotency key presented with a DIFFERENT
// payload.
//
// # THE 409 PLUS THE LOG LINE IS THE COMPLETE REMEDY. NO GATE TASK OWES MORE.
//
// CLAUDE.md invariant 10 as NARROWED on 2026-08-08 (code: 1c6c540; contract:
// 0dbb025) requires that key reuse with a different payload is REJECTED AND
// LOGGED — and nothing else. It does NOT disconnect. An earlier version of this
// comment told MTLS-RELAYGUARD (8192c3c7) to close the connection here; THAT
// INSTRUCTION IS WITHDRAWN and must not be reinstated.
//
// Two reasons, and the second is specific to this package. First, an
// idempotency key is scoped to the CALLER'S OWN agent, so reusing one for new
// content is overwhelmingly a client that lost track of its keys — the party
// most likely to be honest. Second, a relay link MULTIPLEXES AN ENTIRE PEER
// BUS'S ROSTER: closing this connection would drop every agent behind that peer
// over one agent's traffic. See the package doc, "Key reuse is REJECT-AND-LOG",
// for the two questions invariant 10 now requires before any disconnect is added
// to a relay surface, and for the open design question that would have to be
// answered first.
var ErrIdempotencyViolation = errors.New("relay: idempotency key reused with a different payload")

// RelayConfig configures the relay ingress.
type RelayConfig struct {
	// BusID is THIS bus's server-minted id (invariant 1). It is what the loop
	// check measures every incoming path against, so it is validated once at
	// construction rather than trusted per request.
	BusID string

	// AcceptRelay receives the VALIDATED message and reports what this bus did
	// with it. It is where the wiring site consults the idem.Store — with
	// m.Scope() and m.Fingerprint — mints the local sequence, and makes the
	// message durable before answering (invariant 4: nothing is acknowledged
	// before it is durable, and this handler's 200 IS an acknowledgement).
	//
	// THIS PACKAGE DELIBERATELY OWNS NO APPLIED-KEY TABLE. internal/idem owns
	// that (invariant 10), its memory is part of RECOVERED STATE rather than an
	// in-memory cache, and a second table here would be a second answer to
	// "have I applied this?" that could drift from the durable one. relay's job
	// stops at computing the Scope and the Fingerprint and handing them over.
	//
	// THE PRODUCTION IMPLEMENTATION IS Acceptor.Accept (accept.go, RELAY-21).
	// A wiring site builds one with NewAcceptor and assigns its Accept method
	// here; it owns the two orderings that are not the handler's to enforce —
	// the roster check BEFORE the durable write (finding cca64afd) and the
	// onward hop ONLY on idem.OutcomeNew — and it delegates the applied-key
	// table to the local bus rather than keeping one, for the reason above.
	// Write another one only with a reason to; the two orderings are the whole
	// of it and both are easy to get silently wrong.
	//
	// Required: a nil callback would make the handler validate a message and
	// silently discard it, which looks exactly like a working relay.
	AcceptRelay func(ctx context.Context, m RelayedMessage) (RelayAcceptance, error)

	// Trust yields the ORIGIN bus's peering-time signing-key pins and the
	// messaging key that bus attests for a relayed message's sender (SIGN-7).
	// Every ingested envelope is verified through it before AcceptRelay ever sees
	// the message.
	//
	// Required, and required AT CONSTRUCTION rather than defaulted to nil-means-
	// reject, even though VerifyRelayed already treats a nil trust as
	// ErrNoSignerKey. A handler built without one would answer 403 to every
	// well-formed message a correct peer sent — an outage that looks exactly
	// like a peer with the wrong keys, and one that no test supplying a
	// CrossBusTrust would ever reveal. Failing at construction says which side is
	// broken.
	//
	// READ CrossBusTrust'S DOC BEFORE IMPLEMENTING ONE. In particular: the pins
	// come from PEERING (never from the TLS certificate, which is a different key
	// pinned at a different moment by a different party), the origin's attestation
	// is relayed intact and never re-attested by an intermediate, and there is no
	// trust-on-first-use fallback to add.
	Trust CrossBusTrust

	// Logger receives the detailed, peer-supplied-byte-quoting failures that
	// the wire response deliberately omits. Optional; nil discards.
	Logger *logging.Logger

	// MaxRequestBytes overrides MaxRelayBytes, for tests that want to prove the
	// bound without allocating a quarter of a megabyte. Zero means
	// MaxRelayBytes; a negative value is a construction error.
	MaxRequestBytes int64
}

// RelayStats is the observable state of one RelayHandler. LoopDrops is the
// number that matters operationally: a rising loop-drop count is how an
// operator SEES a cyclic topology doing its normal thing, and a loop-drop count
// that tracks total traffic is how they see a mesh that needs pruning.
type RelayStats struct {
	Accepted   uint64
	Duplicates uint64
	LoopDrops  uint64
	Rejected   uint64
}

// RelayHandler is the responder side of a bus-to-bus message relay.
//
// IT IS NOT REGISTERED ON ANY MUX AND MUST NOT BE until INVITE-PEERGUARD and
// MTLS-RELAYGUARD land — it authenticates nothing, exactly like Handler. See
// the package doc.
type RelayHandler struct {
	busID       string
	acceptRelay func(context.Context, RelayedMessage) (RelayAcceptance, error)
	trust       CrossBusTrust
	log         *logging.Logger
	maxBytes    int64

	accepted   atomic.Uint64
	duplicates atomic.Uint64
	loopDrops  atomic.Uint64
	rejected   atomic.Uint64
}

// NewRelayHandler validates cfg and returns the relay ingress handler.
func NewRelayHandler(cfg RelayConfig) (*RelayHandler, error) {
	if err := ids.ValidateBusID(cfg.BusID); err != nil {
		return nil, fmt.Errorf("relay: relay handler bus id: %w", err)
	}
	if cfg.AcceptRelay == nil {
		return nil, errors.New("relay: RelayConfig.AcceptRelay is required; without it the handler would validate a relayed message and silently discard it, which is indistinguishable from delivering it")
	}
	if cfg.Trust == nil {
		return nil, errors.New("relay: RelayConfig.Trust is required; every relayed message is verified through it before it is accepted (SIGN-7), and a handler without one would refuse every well-formed message a correct peer sent")
	}
	if cfg.MaxRequestBytes < 0 {
		return nil, fmt.Errorf("relay: RelayConfig.MaxRequestBytes is %d; it must be zero (meaning %d) or positive", cfg.MaxRequestBytes, MaxRelayBytes)
	}
	max := cfg.MaxRequestBytes
	if max == 0 {
		max = MaxRelayBytes
	}
	log := cfg.Logger
	if log == nil {
		log = logging.New(io.Discard, logging.LevelError)
	}
	return &RelayHandler{busID: cfg.BusID, acceptRelay: cfg.AcceptRelay, trust: cfg.Trust, log: log, maxBytes: max}, nil
}

// Stats reports this handler's counters.
func (h *RelayHandler) Stats() RelayStats {
	return RelayStats{
		Accepted:   h.accepted.Load(),
		Duplicates: h.duplicates.Load(),
		LoopDrops:  h.loopDrops.Load(),
		Rejected:   h.rejected.Load(),
	}
}

// ServeHTTP implements http.Handler.
//
// It authenticates NOTHING. Every caller reaching here is anonymous, which is
// precisely why nothing serves it yet.
//
// # The status mapping, and why each one is what it is
//
//   - Non-POST 405, non-JSON 415, over the byte cap 413, malformed 400 — the
//     transport-level answers, identical to the handshake's so a peer sees one
//     vocabulary across both surfaces.
//
//   - A LOOP IS HTTP 200 with {"accepted":false,"dropped_reason":"loop"}. This
//     is the design call that matters most after the fingerprint. A loop drop
//     is the EXPECTED outcome in a cyclic topology and is not a fault of the
//     sender, which cannot know our federation graph. THREE reasons, and the
//     third is the load-bearing one:
//
//     (i) A 5xx would have RELAY-4's retry/backoff re-deliver — forever — a
//     message that can NEVER be accepted, which is precisely the traffic
//     amplification RELAY-3 exists to stop: the control would become the
//     amplifier. (This argument does NOT extend to a 4xx: a correct backoff
//     policy does not retry those, as this same file argues 60 lines below.)
//
//     (ii) A 4xx would blame the sender for something it cannot know and
//     cannot fix, and would be indistinguishable from the malformed-envelope
//     rejections that ARE its fault.
//
//     (iii) STRUCTURALLY, AND THIS IS WHY IT IS NOT MERELY A PREFERENCE:
//     Client.Relay collapses EVERY non-200 into ErrPeerRefused, so
//     Forwarder.deliver would count an ordinary cyclic drop as Failed and
//     Warn-log it. A perfectly healthy mesh would then look like a failing
//     link — its steady state indistinguishable from a broken peer — and the
//     DropLoop signal, which is the one number that shows an operator the
//     shape of their topology, would never be recorded at all. The 200 is what
//     keeps a settled outcome legible as settled.
//
//   - AN UNSIGNED OR UNSIGNABLE ENVELOPE IS 400 CodeUnsigned; A SIGNATURE WE
//     CANNOT ATTRIBUTE TO THE NAMED SENDER IS 403 CodeBadSignature; AND AN
//     ORIGIN BUS WE HOLD NO PEERING-TIME PIN FOR IS 403 CodeUnpeeredBus
//     (SIGN-7). All are FINAL and none is the loop's 200: a loop is a
//     settled non-fault the sender could not have avoided, whereas these are the
//     sender's own envelope being refused, and telling a peer "200, dropped"
//     for a forged message would file an attack under normal operation. They are
//     also not 503: a retry cannot change any of the three verdicts, and a peer
//     that keeps resending an unverifiable message is a peer we want to stop, not
//     schedule. The 400/403 split is "nobody could verify this" versus "we will
//     not attribute this to that agent" — the second is an authorization answer.
//
//     CodeUnpeeredBus is split out from CodeBadSignature because the two are
//     different OPERATOR problems: "we have never peered with your bus, so we can
//     verify nothing you send" is fixed by completing a peering, while "your
//     signature is wrong" starts a forgery investigation. One code for both would
//     send an operator hunting an attack on what is the ordinary day-one state of
//     an unfinished federation.
//
//   - A MESSAGE NAMING A LOCAL AGENT WE DO NOT HAVE IS 404 CodeUnknownRecipient
//     (RELAY-21), and NOTHING WAS WRITTEN for it — the callback asks the roster
//     before the durable write precisely so that a name nobody holds costs this
//     bus nothing permanent (finding cca64afd; see Acceptor.Accept). It is FINAL
//     rather than a 503 so the sending bus stops retrying a message it can only
//     fix by fixing its own roster.
//
//   - An idempotency VIOLATION is 409 and a Warn line, AND THAT IS THE WHOLE
//     RESPONSE (invariant 10 as narrowed 2026-08-08). The peer is NOT
//     disconnected: the key is its own, so this is far more likely a confused
//     peer than a hostile one, and this link carries a whole roster's traffic.
//     See ErrIdempotencyViolation.
//
//   - A DUPLICATE is 200 with accepted:true, duplicate:true and the ORIGINAL
//     local message id. Invariant 10: return the original result, re-apply
//     nothing, DISCONNECT NOBODY. Punishing a retry would break exactly the
//     peers doing the right thing.
//
//   - A PEER-CLAIM MISMATCH is 403 CodePeerRejected (RELAY-24-FU-RELAYHTTP-4XX):
//     the authenticated peer does not match the last hop it claimed in the
//     traversed bus path (invariant 2, bound at the wiring site), so nothing was
//     written. It is FINAL rather than 503 for the same reason as the 404 above —
//     the peer cannot fix a mis-stamped path by resending — and it matches the
//     two sibling claim-mismatch surfaces (403 on enrol, 403 on roster). See
//     ErrPeerRejected.
//
//   - Any other callback failure is 503, so a peer can tell "not now" from
//     "never" and knows retrying is the correct response.
func (h *RelayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.fail(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, fmt.Errorf("relay: %s is not allowed on %s", r.Method, PeerRelayPath))
		return
	}
	if err := checkJSONContentType(r.Header.Get("Content-Type")); err != nil {
		h.fail(w, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, err)
		return
	}

	// The key is read and validated BEFORE the body, which is the reason
	// internal/idem puts it in a header: an oversized or malformed key is
	// refused without a byte of a possibly 100 KiB body being read into memory.
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

	// Validation and signature verification are ONE call, on purpose: there is no
	// point in this function where a validated-but-unverified message exists (see
	// ValidateRelayRequest, "Why the resolver is a REQUIRED PARAMETER").
	m, err := ValidateRelayRequest(h.busID, idempotencyKey, req, h.trust)
	if err != nil {
		if errors.Is(err, ErrRelayLoop) {
			// SETTLED, NOT FAILED. See the doc comment above.
			h.loopDrops.Add(1)
			// ONLY VALIDATED FIELDS ARE LOGGED HERE. The loop check is check 2 of
			// ValidateRelayRequest, so origin_bus, message_id and sender have NOT
			// been through their bounds checks at this point — they are raw peer
			// bytes. req.BusPath has: CheckIncomingPath validated every hop
			// (length, charset, MaxPeerBusIDLen) before it found us on the path,
			// so BusPath[0] is a known-bounded bus id and is the same value
			// origin_bus is required to equal anyway.
			//
			// This matters more here than on any other path precisely because a
			// loop drop is the EXPECTED STEADY STATE of a cyclic topology: it is
			// the highest-volume log line in the package, so it is the one a peer
			// would choose to inflate.
			h.log.Info("relayed message dropped: it has already traversed this bus",
				"local_bus", h.busID,
				"origin_bus", req.BusPath[0],
				"path_hops", len(req.BusPath),
				"loop_drops", h.loopDrops.Load(),
			)
			writeJSONBody(w, h.log, http.StatusOK, RelayResponse{Accepted: false, DroppedReason: DropLoop})
			return
		}
		// Everything else a peer can get wrong here is a 400. A CodeInternal is
		// OUR bug (a local bus id that stopped validating), not the peer's, so
		// it must not be reported as a bad request: a peer told "400" would
		// stop retrying a fault it did not cause and cannot fix.
		status := http.StatusBadRequest
		code := ErrorCode(err)
		switch code {
		case CodeInternal:
			status = http.StatusInternalServerError
		case CodeBadSignature, CodeUnpeeredBus:
			// Authorization answers, not malformed-request ones: the envelope
			// parsed, and we are refusing to attribute it to the agent it names
			// (SIGN-7) — either because the signature does not verify, or because
			// we hold no peering-time pin for its origin bus and therefore cannot
			// verify anything it sends. Both are FINAL; do not invite a retry, and
			// note that ErrUnpeeredBus in particular is NOT a 503: no amount of
			// retrying establishes a peering, which is an operator action on both
			// ends.
			status = http.StatusForbidden
		}
		h.fail(w, status, code, err)
		return
	}

	acc, err := h.acceptRelay(r.Context(), m)
	if err != nil {
		if errors.Is(err, ErrUnknownLocalRecipient) {
			// 404, AND NOT 503, AND THE DIFFERENCE IS AN AMPLIFICATION BOUND.
			// This message names an agent we do not have; nothing was written
			// (see Acceptor.Accept). A 503 would tell the peer's retry machinery
			// to keep re-sending it for the whole retry horizon — a peer could
			// aim a stream of messages at names that do not exist here and have
			// our own control retry each one, which is the amplification
			// relayhttp's status argument exists to prevent. A 4xx is FINAL
			// (PeerRefusedError.Retriable), so the sending bus stops and its
			// operator gets a code whose remedy is its own roster.
			//
			// It does not leak roster membership to anyone who could not already
			// ask: only a peered bus reaches this handler, and peers exchange
			// full rosters over the roster-sync surface by design.
			h.fail(w, http.StatusNotFound, CodeUnknownRecipient, err)
			return
		}
		if errors.Is(err, ErrIdempotencyViolation) {
			// Neither payload is echoed — invariant 10's violation case is
			// exactly the situation where two payloads exist and neither may be
			// shown to the other party.
			h.log.Warn("relayed message REJECTED: idempotency key reused with a different payload (invariant 10). Rejected and logged, and that is the WHOLE remedy — the peer is deliberately NOT disconnected, because the key is its own and this link carries its entire roster's traffic",
				"local_bus", h.busID,
				"origin_bus", m.OriginBus,
				"origin_message_id", m.OriginMessageID,
				"sender", m.Sender,
			)
			h.fail(w, http.StatusConflict, CodeIdempotencyViolation, err)
			return
		}
		if errors.Is(err, ErrPeerRejected) {
			// 403, AND NOT 503, FOR THE SAME PERMANENT-VS-RETRYABLE REASON AS THE
			// 404 ABOVE. The authenticated peer does not match the last hop it
			// claimed in the traversed bus path (invariant 2, bound at the wiring
			// site): the request cannot be attributed to the bus it names, and
			// nothing was written. A 503 would tell the peer's retry machinery to
			// resend for its whole retry horizon a refusal it can never satisfy —
			// the claim is fixed only by stamping the path correctly, never by
			// resending the same bytes. This is the SAME final-403 answer the two
			// sibling claim-mismatch surfaces already give: ErrPeerRejected on the
			// handshake (handshake.go) and ErrUnknownPeer on roster sync
			// (rosterhttp.go). It is a REJECT-AND-RESPOND that does NOT disconnect
			// the peer (invariant 10): a merely buggy peer can mis-stamp a hop, and
			// this link multiplexes a whole peer roster's traffic.
			h.fail(w, http.StatusForbidden, CodePeerRejected, err)
			return
		}
		h.fail(w, http.StatusServiceUnavailable, CodeUnavailable, err)
		return
	}

	if acc.Duplicate {
		h.duplicates.Add(1)
	} else {
		h.accepted.Add(1)
	}
	h.log.Info("relayed message accepted",
		"local_bus", h.busID,
		"origin_bus", m.OriginBus,
		"origin_message_id", m.OriginMessageID,
		"local_message_id", acc.LocalMessageID,
		"duplicate", acc.Duplicate,
		"path_hops", len(m.BusPath),
	)
	writeJSONBody(w, h.log, http.StatusOK, RelayResponse{
		Accepted:  true,
		Duplicate: acc.Duplicate,
		MessageID: acc.LocalMessageID,
	})
}

// decodeRequest reads at most maxBytes and decodes strictly — the same posture
// as Handler.decodeRequest, for the same reasons: Content-Length is a claim the
// peer makes about itself, and there is no version field to negotiate with, so
// an unrecognised field means the sender believes something untrue about this
// protocol and failing loudly is the only honest answer available.
func (h *RelayHandler) decodeRequest(body io.Reader) (RelayRequest, error) {
	buf, err := io.ReadAll(io.LimitReader(body, h.maxBytes+1))
	if err != nil {
		return RelayRequest{}, fmt.Errorf("%w: reading body: %v", ErrInvalidRequest, err)
	}
	if int64(len(buf)) > h.maxBytes {
		return RelayRequest{}, fmt.Errorf("%w: body exceeds %d bytes", ErrPayloadTooLarge, h.maxBytes)
	}
	var req RelayRequest
	if err := decodeStrict(buf, &req); err != nil {
		return RelayRequest{}, err
	}
	return req, nil
}

// fail is the ONE place Rejected is incremented, so the counter cannot
// double-count a path that both logs a specific reason and then fails.
func (h *RelayHandler) fail(w http.ResponseWriter, status int, code string, err error) {
	h.rejected.Add(1)
	failJSON(w, h.log.With("surface", "peer-relay", "local_bus", h.busID), status, code, err)
}

// Relay POSTs one message to a peer and returns its settled answer.
//
// peerBaseURL is the peer's base URL ("https://peer.example:8443"); Relay
// appends PeerRelayPath itself, so the path is never a caller's typo, and
// peerURL enforces the https-only origin rule (invariant 11).
//
// The idempotency key is NOT a parameter: it IS req.MessageID, the ORIGIN's
// message id, and is sent in the idem.HeaderName header. See
// ValidateRelayRequest for why that identity is a protocol rule — a per-hop key
// would make every copy of one message look new and would defeat duplicate
// suppression silently.
//
// A NON-200 IS A REFUSAL, AND SO IS A 3XX. The redirect policy installed by
// NewClient (http.ErrUseLastResponse) is inherited here and MUST NOT be
// weakened: Go's default policy would replay this POST — the sender's id, the
// recipients and the body — at whatever host a 3xx names, over whatever scheme
// it names, on a fresh connection whose identity was never checked. That is the
// exfiltration route a security review found on the handshake path, and it is
// the same route here with a message body attached.
//
// A 200 carrying accepted:false with a dropped_reason is a SETTLED outcome, not
// an error: it is returned with a nil error, and a caller must not retry it.
func (c *Client) Relay(ctx context.Context, peerBaseURL string, req RelayRequest) (RelayResponse, error) {
	// The key is the origin message id; validating it here means a malformed
	// one is caught before anything is sent, rather than as a 400 from the peer.
	if err := idem.ValidateKey(req.MessageID); err != nil {
		return RelayResponse{}, fmt.Errorf("relay: the relay idempotency key is the origin message id, and this one is not a legal key: %w", err)
	}
	endpoint, err := peerURL(peerBaseURL, PeerRelayPath)
	if err != nil {
		return RelayResponse{}, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return RelayResponse{}, fmt.Errorf("relay: encoding relayed message %s: %w", req.MessageID, err)
	}
	if len(body) > MaxRelayBytes {
		// Sending a body we would refuse to receive is an asymmetry that only
		// ever shows up as a confusing 413 from the far end.
		return RelayResponse{}, fmt.Errorf("%w: encoded relay envelope is %d bytes, over the %d cap", ErrPayloadTooLarge, len(body), MaxRelayBytes)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RelayResponse{}, fmt.Errorf("relay: building relay request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set(idem.HeaderName, req.MessageID)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return RelayResponse{}, fmt.Errorf("relay: relaying %s to %s: %w", req.MessageID, endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxRelayBytes))
		_ = resp.Body.Close()
	}()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, MaxRelayBytes+1))
	if err != nil {
		return RelayResponse{}, fmt.Errorf("relay: reading relay response from %s: %w", endpoint, err)
	}
	if len(buf) > MaxRelayBytes {
		return RelayResponse{}, fmt.Errorf("%w: response from %s exceeds %d bytes", ErrPayloadTooLarge, endpoint, MaxRelayBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return RelayResponse{}, &PeerRefusedError{Endpoint: endpoint, StatusCode: resp.StatusCode, Code: peerErrorCode(buf)}
	}

	var decoded RelayResponse
	if err := decodeStrict(buf, &decoded); err != nil {
		return RelayResponse{}, fmt.Errorf("decoding relay response from %s: %w", endpoint, err)
	}
	c.log.Debug("relayed message answered",
		"local_bus", c.busID,
		"peer", endpoint,
		"origin_message_id", req.MessageID,
		"accepted", decoded.Accepted,
		"duplicate", decoded.Duplicate,
		"dropped_reason", decoded.DroppedReason,
	)
	return decoded, nil
}
