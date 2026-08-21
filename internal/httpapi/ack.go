package httpapi

// ACK-6 — POST /v1/ack, the RECIPIENT half of the delivery acknowledgement
// plane (ACK-CONTRACT.md §4, §9.1).
//
// # WHAT THIS ROUTE IS, IN ONE SENTENCE
//
// It is the ONLY way a message becomes `delivered` or `refused`: an explicit
// application acknowledgement from the addressed agent, never inferred from a
// poll, a cursor or a delivery. internal/hub/ack.go carries the ruling and the
// three reasons behind it; read that file before changing anything here.
//
// # THE THREE SURFACES OF THE ACK PLANE, AND WHY THIS ONE IS NOT THE PEER ONE
//
//	POST /v1/ack        THIS FILE. Agent surface: a bearer session, plus
//	                    invariant 11's certificate cross-check WHERE THERE IS A
//	                    CERTIFICATE TO CROSS-CHECK. A RECIPIENT declaring plane C
//	                    about a message addressed to IT.
//	POST /v1/peer/ack   internal/relay/ackhttp.go. Peer surface, gated by
//	                    RequirePeerPrincipal — a certificate, no session. An
//	                    adjacent BUS propagating a terminal outcome one hop back.
//	GET  /v1/ack/{key}  ACK-9. The SENDER reading status.
//
// PRECISION ON "cross-checked", because the short form reads as a stronger
// claim than the build makes: the listener is `ClientAuth: tls.RequestClientCert`
// (cmd/agent-bus/tlslisten.go), and crosscheck.go's guard bites only for an agent
// that HAS a live certificate binding. For an agent with none, this route — like
// every other authenticated route — is bearer-token-only. That is inherited from
// the platform and is not narrowed or widened here; it is written out so nobody
// reads this file as evidence that mutual TLS is enforced.
//
// §6.1's narrowing is inherited and MUST NOT be widened: an ACK frame must not
// be accepted on the agent surface on behalf of a peer bus, and an agent session
// token must never be consulted on the peer surface. The concrete expression of
// that here is relay.AckSurfaceAgent, which is passed as a CONSTANT from this
// mount site and is never read from the frame — see relay.AckSurface for what
// went wrong when the label was inferred from a frame's contents instead.
//
// # THE FRAME TYPE IS SHARED WITH THE PEER ROUTE, DELIBERATELY
//
// relay.PeerAckRequest is the wire shape for BOTH surfaces and
// relay.ValidatePeerAckRequest takes the surface as a parameter precisely so
// this route can reuse it. A second, agent-only frame type would be a second
// spelling of one closed vocabulary — twelve NACK classes, three outcomes, one
// wire version — and the day the two disagreed, one surface would accept
// something the other rejected, which is exactly the class of defect §5.1's
// "REJECTED, never defaulted" posture exists to prevent. The type's NAME is
// awkward on this surface and that is the whole cost.
//
// # NOTHING HERE DISCONNECTS ANYBODY (§12, invariant 10)
//
// Both of invariant 10's questions were answered on the record by ACK-1 before
// this route existed. A merely BUGGY client reaches every refusal below — an
// agent that mistypes a correlation key, an agent that re-acknowledges after its
// own restart, an agent whose retry crossed with the 200 it never saw. And a
// single connection does not carry a single principal's traffic. So every
// refusal here is reject-and-log, and `disconnect(w)` must not appear in this
// file. Contrast checkSignedMint's sender-mismatch, which drops the socket: that
// is invariant 10's REPLAY clause and it is reachable only by a caller wearing a
// name it did not authenticate as. The recipient-mismatch refusal below looks
// similar and is NOT the same thing — there are no signed bytes to replay here,
// and an agent embedding client/ under two enrolments reaches it honestly.

import (
	"errors"
	"net/http"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/relay"
)

// RouteAck is the recipient acknowledgement route (§9.1).
//
// It is a BARE path with no trailing slash, so it never collides with ACK-9's
// GET /v1/ack/{correlation_key}: http.ServeMux resolves "/v1/ack" as an exact
// match and "/v1/ack/" as a subtree, and the two can be registered side by side
// by different tasks without either shadowing the other.
const RouteAck = "/v1/ack"

// AckStateUnknown is the REPORTING value THIS route answers for a pair it is not
// retaining.
//
// It is not a state, it is never written durably, and internal/ack has no enum
// member for it on purpose — ack.ParseState REFUSES this spelling by name, so a
// record saying "I don't know" cannot round-trip through the log and overwrite a
// real terminal outcome with ignorance. It exists only on the wire.
//
// # IT IS NOT SHARED WITH GET /v1/ack/<key>, AND AN EARLIER DRAFT OF THIS COMMENT SAID IT WAS
//
// ackstatus.go (ACK-9) declares its own unexported ackStateUnknown with the same
// value, and neither file references the other's symbol. That is deliberate: the
// two routes were written in parallel, and a shared constant made each file's
// EXISTENCE load-bearing for the other's compilation — which showed up as a
// redeclaration error the moment either side moved. Two constants that must
// agree is a real (small) hazard; two tasks that cannot compile apart is a
// bigger one, and the agreement is pinned by tests on both sides instead.
//
// The correction is left in place rather than deleted because the claim it
// replaces was TRUE for about an hour and then silently stopped being true — the
// exact stale-note failure this repository keeps paying for, in the exact form
// that reads as freshly checked.
const AckStateUnknown = "unknown"

// AckRequestBody is the request shape, and it is relay.PeerAckRequest — see the
// file header for why the two surfaces share one frame type.
type AckRequestBody = relay.PeerAckRequest

// AckResponseBody is the answer to POST /v1/ack.
//
// # WHY A REFUSAL CAN COME BACK AS 200
//
// TWO SEPARATE RULES, and it is worth keeping them apart because an earlier
// draft of this comment ran them together and got the second one wrong.
//
// FIRST, §13.3: `accepted:false, state:"unknown"` is the UNIFORM ANSWER and is
// returned identically for four different facts — the key was never accepted
// here, the key names a message this agent was not addressed in, the row was
// swept by retention, or the key is malformed. They MUST stay
// INDISTINGUISHABLE FROM EACH OTHER, because a 403 for "not yours" beside a 404
// for "no such key" is a message-existence oracle. That rule constrains the four
// negatives to ONE answer; it says nothing about WHICH answer.
//
// SECOND, and this is what actually chooses 200 over a uniform 404:
//
//   - §9.3's doctrine, stated on the peer route and true here: "a 4xx/5xx on a
//     SETTLED, NOT-YOUR-FAULT outcome makes retry/backoff amplify exactly the
//     traffic the mechanism exists to stop". A swept row is not the caller's
//     fault and re-sending will never change it.
//   - §13.4 maps `unknown` to the CLI's existing ExitEmpty (8) rather than to an
//     error, and mints no new exit code. A 404 would make the client library
//     raise where the contract says report.
//
// So the status line carries the SHAPE of the request and the body carries the
// OUTCOME. A client reads Accepted, never the status line.
type AckResponseBody struct {
	// Accepted reports that this bus recorded the outcome, or had already
	// recorded exactly this outcome.
	Accepted bool `json:"accepted"`

	// Duplicate reports invariant 10's FIRST case: this pair was ALREADY
	// terminal with the SAME outcome, so the original result stands, nothing was
	// re-applied, and nobody was disconnected.
	Duplicate bool `json:"duplicate"`

	// State is the state that now STANDS for the pair — on a duplicate, the
	// ORIGINAL one — or AckStateUnknown when nothing is retained.
	State string `json:"state"`

	// Class is the recorded NACK class, present iff the state is a negative
	// terminal. It is a member of the closed set and there is NO adjacent
	// free-text field (invariant 6, §5.1): a reason string sourced from a
	// recipient is a body by another name.
	Class string `json:"class,omitempty"`
}

// handleAck serves POST /v1/ack.
//
// # THE ORDER OF THE CHECKS IS THE DESIGN
//
//  1. METHOD, then AUTHENTICATION. A route that answered anything to an
//     unauthenticated caller would describe the messaging surface to somebody
//     with no business knowing it exists.
//  2. THE BODY, bounded by relay.MaxAckBytes (4 KiB), which is derived from the
//     widest legal frame rather than guessed — no field of the frame has a
//     length a remote party chooses (§5.3).
//  3. THE RECIPIENT BINDING, before the frame's content is examined. It is an
//     authorization question and it is answered from the CONTEXT PRINCIPAL.
//  4. THE FRAME, through relay.ValidatePeerAckRequest with the AGENT surface —
//     which is where "an agent may not assert a routing outcome" and the
//     class/outcome half-set rule are enforced.
//  5. THE HUB, which re-checks all of it (it is the last gate before a durable
//     write and is reachable by an embedder that never saw a frame) and settles
//     the row durably before this handler is told anything (invariant 4).
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	recipient, ok := s.messagingPrincipal(w, r)
	if !ok {
		return
	}
	var body AckRequestBody
	if !s.decodeJSONRequest(w, r, &body, relay.MaxAckBytes) {
		return
	}

	// THE RECIPIENT IS THE AUTHENTICATED PRINCIPAL, AND THE FIELD IS A CLAIM.
	//
	// body.Recipient is compared against the context principal and then
	// DISCARDED; hub.RecipientAckRequest is built from `recipient`, exactly as
	// handleSend attributes a message to the context principal and discards
	// body.Sender (invariant 1). There is no path by which the frame's value
	// reaches the lookup key.
	//
	// The field is REQUIRED rather than ignored because it is carried on the peer
	// surface, where it is load-bearing, and a client that filled it in wrongly
	// has misunderstood which row it is settling. Failing loudly beats silently
	// settling a different one from the one it named.
	//
	// 403 and NOT a disconnect: see the file header.
	if body.Recipient != recipient {
		s.log.Warn("acknowledgement refused: the recipient named in the frame is not the authenticated principal. The connection is KEPT -- there are no signed bytes to replay here and a client embedding two enrolments reaches this honestly (§12)",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", recipient,
			"claimed_recipient", elideAckLog(body.Recipient),
			"path", r.URL.Path,
		)
		s.writeJSON(w, r, http.StatusForbidden, ErrorResponse{Error: "recipient does not match the authenticated caller"})
		return
	}

	// AckSurfaceAgent is a COMPILE-TIME CONSTANT supplied by this mount site and
	// is never read from the frame. It is what refuses an `undeliverable` outcome
	// here (a claim about the federation's routing, which a recipient application
	// has no standing to make) and what labels the attestation
	// `recipient_signature_unverified` rather than `peer_bus`.
	v, err := relay.ValidatePeerAckRequest(body, relay.AckSurfaceAgent)
	if err != nil {
		// The frame's own values are NOT echoed to the client: it chose them, and
		// relay's error text already elides what it quotes.
		s.log.Debug("acknowledgement rejected: the frame is not one this bus will record",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", recipient,
			"err", err,
		)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "invalid acknowledgement"})
		return
	}

	state, class, attestedBy, err := ackRecordVocabulary(v)
	if err != nil {
		// UNREACHABLE unless the two spellings of the closed vocabulary have
		// drifted apart — internal/relay's wire enums and internal/ack's durable
		// ones. It is checked rather than assumed, and it is an ERROR rather than
		// a 400, because reaching it means a validated frame could not be
		// expressed as a record: a bug in this bus, not in its caller.
		s.log.Error("a VALIDATED acknowledgement frame could not be expressed in the durable vocabulary; internal/relay's wire spellings and internal/ack's record spellings have drifted apart",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", recipient,
			"err", err,
		)
		s.writeJSON(w, r, http.StatusInternalServerError, ErrorResponse{Error: "acknowledgement could not be recorded"})
		return
	}

	res, err := s.hub.AcknowledgeDelivery(hub.RecipientAckRequest{
		Recipient:      recipient,
		CorrelationKey: v.CorrelationKey,
		Outcome:        state,
		Class:          class,
		AttestedBy:     attestedBy,
	})
	if err != nil {
		s.writeAckError(w, r, recipient, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, AckResponseBody{
		Accepted:  true,
		Duplicate: res.Duplicate,
		State:     res.State.String(),
		Class:     string(res.Class),
	})
}

// ackRecordVocabulary translates the VALIDATED wire enums into the DURABLE ones.
//
// internal/relay spells the closed vocabulary as bounded uint8 enums for the
// wire; internal/ack spells the SAME vocabulary as strings for the record, on
// purpose — a numeric enum in a durable record is unreadable to an operator and
// silently changes meaning if the constants are reordered (ack.State's own
// note). The bridge is therefore through the STRING spelling, which is the one
// thing both sides agree is canonical, and every step is CHECKED:
// ack.ParseState refuses a spelling it does not know, and the class is required
// to be a member of one of the two halves of the closed set. Nothing is
// defaulted; a spelling neither side recognises is an error.
func ackRecordVocabulary(v relay.ValidatedPeerAck) (ack.State, ack.Class, ack.Attestation, error) {
	state, err := ack.ParseState(v.Outcome.String())
	if err != nil {
		return 0, "", "", err
	}
	var class ack.Class
	if v.Class != 0 {
		class = ack.Class(v.Class.String())
		if !class.RecipientEmitted() && !class.BusEmitted() {
			return 0, "", "", errors.New("httpapi: the wire class spelling " + elideAckLog(v.Class.String()) + " is not a member of the durable closed set")
		}
	}
	attestedBy := ack.Attestation(v.Attestation.String())
	if !attestedBy.Valid() {
		return 0, "", "", errors.New("httpapi: the wire attestation spelling " + elideAckLog(string(attestedBy)) + " is not a member of the durable closed set")
	}
	return state, class, attestedBy, nil
}

// writeAckError maps the boundary's sentinels to status codes BY SENTINEL,
// never by matching error text.
func (s *Server) writeAckError(w http.ResponseWriter, r *http.Request, recipient string, err error) {
	kv := []interface{}{
		"request_id", RequestIDFromContext(r.Context()),
		"op", "ack",
		"agent_id", recipient,
		"err", err,
	}
	switch {
	case errors.Is(err, hub.ErrAckNotRetained):
		// THE UNIFORM ANSWER (§13.3). 200 with `unknown`, identical for a key
		// that never existed, a key that was swept, and a key that belongs to
		// somebody else. See AckResponseBody for why a 403/404 here would be a
		// message-existence oracle. Logged at Debug: an agent acknowledging after
		// the retention window, or after its own restart, is routine.
		s.log.Debug("acknowledgement not retained; answering the uniform `unknown` (§13.3)", kv...)
		s.writeJSON(w, r, http.StatusOK, AckResponseBody{Accepted: false, State: AckStateUnknown})

	case errors.Is(err, hub.ErrAckTerminalConflict):
		// Invariant 10's SECOND case: same pair, DIFFERENT terminal outcome. The
		// FIRST terminal stands and nothing was written. 409, no Retry-After —
		// re-sending cannot ever succeed, and dressing a permanent refusal as
		// transient is how a client ends up in a retry loop that cannot end.
		// ack.Store already logged it at ERROR with both outcomes named, so it is
		// not logged a second time here.
		s.writeJSON(w, r, http.StatusConflict, ErrorResponse{
			Error: "this delivery outcome is already terminal with a different outcome; the first terminal stands",
		})

	case errors.Is(err, hub.ErrInvalidAck):
		// UNREACHABLE from this route today — relay.ValidatePeerAckRequest
		// refuses every shape the hub refuses, one layer earlier. It is mapped
		// anyway, fail-closed, because the hub is the authority and the day the
		// two layers disagree the answer must be a 400 and not a 500.
		s.log.Debug("acknowledgement rejected by the hub boundary", kv...)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "invalid acknowledgement"})

	case errors.Is(err, hub.ErrAckInFlight):
		// TRANSIENT and RETRYABLE, unlike every other refusal here: another
		// transition for this exact pair is being fsynced, and the retry lands on
		// the row that transition wrote and is absorbed as a duplicate. So this is
		// the ONE arm that carries a Retry-After, and it must never be a 500 —
		// invariant 10's first case says a same-key/same-payload retry returns the
		// original result and DOES NOT ERROR, and an eager client retrying its own
		// acknowledgement is the ordinary way to reach it.
		s.log.Debug("acknowledgement deferred: another transition for this pair is in flight", kv...)
		w.Header().Set("Retry-After", "1")
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{
			Error: "another delivery outcome for this pair is being recorded; retry",
		})

	case errors.Is(err, hub.ErrNoAckTable):
		// 501, not 503 and not 404. It is a fact about this BUILD rather than a
		// transient condition, so a Retry-After would be a lie; and 404 would
		// tell a client the protocol does not have this route when it does. Same
		// shape as handleBroadcast's 501, and after authentication for the same
		// reason.
		s.log.Warn("acknowledgement refused: this bus has no delivery lifecycle table wired, so no acknowledgement can be recorded", kv...)
		s.writeJSON(w, r, http.StatusNotImplemented, ErrorResponse{
			Error: "delivery acknowledgement is not available on this bus",
		})

	default:
		// ErrNotDurable and anything unforeseen, through the mapping every other
		// messaging route already uses.
		s.writeHubError(w, r, "ack", err)
	}
}

// maxAckLogChars bounds a caller-chosen value on its way into an operator log
// line. Nothing here is persisted, but an unbounded echo of attacker-chosen
// input into a log is the expansion internal/ack's elide exists to prevent.
const maxAckLogChars = 64

func elideAckLog(s string) string {
	if len(s) <= maxAckLogChars {
		return s
	}
	return s[:maxAckLogChars] + "...(elided)"
}
