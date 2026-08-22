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

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// THE PEER-HOP ACK/NACK ROUTE (ACK-3).
//
// ackframe.go owns the frame; this file owns the ROUTE that authenticates a
// frame, meters it, binds it to an obligation this bus durably wrote, and hands
// the settled outcome to the durable ACK record.
//
// # WHY AckHandler IS NOT AN http.Handler, AND WHY THAT IS THE WHOLE SECURITY ARGUMENT
//
// It has no ServeHTTP. It has ServeAuthenticated(w, r, peerBusID), and peerBusID
// MUST be httpapi.PeerPrincipal.BusID — the bus id RequirePeerPrincipal resolved
// from the TLS CLIENT CERTIFICATE.
//
// AuthorizePeerAck's binding rule is only as strong as that argument: it
// authorises DeriveJobID(peerBusID, correlationKey), so a peerBusID taken from
// anywhere a remote party can influence authorises THE NAME THE PEER CHOSE
// rather than the peer that sent the frame — and every anti-forgery guarantee in
// ack.go evaporates while every positive test still passes. ack.go could only
// state that as a caller precondition. Two things make it structural here:
//
//  1. THE FRAME HAS NO BUS-ID FIELD (ackframe.go), and decodeStrict refuses an
//     invented one, so there is nothing in the request body to read; and
//  2. THIS TYPE CANNOT BE MOUNTED WITHOUT SUPPLYING THE PARAMETER. A
//     *AckHandler does not satisfy http.Handler, so `mux.Handle(path, h)` does
//     not compile. The one mount site — internal/httpapi/peermount.go, which
//     internal/relay/guards_test.go pins as the only file outside this package
//     permitted to name a peer path — must write the adapter that reads the
//     principal out of the request context, and there is no other place a bus id
//     can come from at that point.
//
// A handler that "forgot the principal" is therefore a compile error rather than
// a silent forgery hole. That is the strongest form available in Go without a
// type nobody outside this package can construct, and a constructor-guarded type
// would buy nothing here: any exported constructor is callable with any string.
//
// # THE ORDER OF THE STEPS IS THE DESIGN, AND STEP 3 IS WHY
//
//	1. method, content type                     — free
//	2. ADMISSION, keyed on the AUTHENTICATED peer — one map lookup under a
//	                                              short-held mutex
//	3. read a bounded body and decode strictly  — at most MaxAckBytes
//	4. validate the frame                       — pure, no locks
//	5. AuthorizePeerAck                         — *** TAKES THE OUTBOX'S
//	                                              EXCLUSIVE MUTEX AND RUNS AN
//	                                              O(n) SWEEP ***
//	6. settle, durably                          — one fsync
//
// Step 5 is the expensive one and ack.go says so in its own doc: Outbox.Lookup
// takes the same exclusive mutex Enqueue and Settle need and sweeps up to
// MaxOutboxJobs records, so an unmetered path in front of it lets one peer drive
// an O(n) exclusive sweep per bogus frame and contend with REAL DELIVERY. The
// meter is therefore at step 2 — ahead of the body read as well, so a metered
// peer does not even get to spend our memory bandwidth.
//
// # THE METER IS THE EXISTING ONE. THIS TASK FORKS NO SECOND SCHEME
//
// Admit is satisfied in production by cmd/agent-bus's peerAdmission.enter — the
// per-authenticated-peer in-flight bound RELAY-22 already built for relay
// ingest, keyed on the same certificate-resolved principal, refusing with the
// same errRelayPeerBusy. ACK-CONTRACT.md §16 Q3 asks whether the ACK surface
// needs its own rate limit and defers the ANSWER to the open abuse-control task
// (48223968 "Choose the abuse-control primitive for a MULTI-PRINCIPAL relay
// link", consumed by RELAY-22). This route deliberately does not answer it: it
// reuses the primitive that exists, through an INTERFACE-SHAPED seam, so that
// when 48223968 rules, the ruling lands in ONE place and this route inherits it.
//
// THE DESIGN CHOICE THAT WAS FORCED, STATED RATHER THAN BURIED: a bound was
// needed here and 48223968 has not ruled, so the CONCURRENCY half of the
// existing meter is used and the QUOTA half (peerAdmission.reserve) is NOT. The
// quota counts applied-key entries and an ACK creates none, so charging one
// would meter this route against a table it does not touch. Concurrency is also
// the half that actually bounds the harm: the harm is contention on the outbox
// mutex, which is a function of how many ACKs are IN FLIGHT, not of how many
// arrive per hour. A rate limit was considered and rejected for the reason
// peerAdmission's own doc records at length — the first version of that meter
// was a token bucket and it was a permanent speed limit on honest peers that did
// not even bound the thing it cost so much to bound.
//
// # NO REFUSAL ON THIS ROUTE DISCONNECTS ANYTHING (§12, invariant 10)
//
// Invariant 10's two questions were answered on the record in ACK-CONTRACT §12
// and again in ack.go, and both point the same way: a merely buggy peer reaches
// every refusal here trivially, and a peer connection carries EVERY AGENT BEHIND
// THAT PEER. So every refusal is reject-and-log. There is no code path in this
// file that closes a connection, and TestRelayHopAckNackAuthentication asserts
// it.

// AckStats is the observable state of one AckHandler.
//
// Refused counts EVERY refusal this route makes; Conflicts counts invariant 10's
// protocol-violation case as well, because the two mean different things to an
// operator: refusals are mostly a mis-wired or probing peer, whereas a rising
// conflict count means two parties disagree about a TERMINAL outcome, which is
// never normal.
//
// A CONFLICT THEREFORE INCREMENTS BOTH, and that is deliberate rather than
// double-counting: Refused is "how much of this route's traffic was not
// recorded" and Conflicts is "how much of THAT was a disagreement". Subtracting
// gives the rest. The alternative — making them disjoint — would mean an
// operator watching Refused alone would miss conflicts entirely, which is the
// number that matters most.
type AckStats struct {
	Accepted   uint64
	Duplicates uint64
	Conflicts  uint64
	Refused    uint64
	Throttled  uint64
}

// AckSettlement is what a SettleAck callback reports back.
type AckSettlement struct {
	// Duplicate reports invariant 10's FIRST case: this (correlation key,
	// recipient) was already terminal with EXACTLY this outcome and class, so
	// the original result stands and nothing was re-applied.
	Duplicate bool
}

// SettledAck is the VALIDATED and AUTHORIZED outcome handed to SettleAck: the
// frame, plus the two things the frame could not carry.
type SettledAck struct {
	// Ack is the validated frame content.
	Ack ValidatedPeerAck

	// PeerBusID is the AUTHENTICATED adjacent bus — RequirePeerPrincipal's
	// certificate resolution, never a frame field. See the file header.
	PeerBusID string

	// Obligation is the outbox record AuthorizePeerAck bound this frame to: the
	// job THIS BUS durably wrote to THIS PEER for THIS correlation key. Its
	// presence is the proof that the binding ran; a callback that receives one
	// of these has not been asked to trust anything the peer said.
	Obligation OutboxRecord
}

// AckConfig configures the peer ACK route.
type AckConfig struct {
	// BusID is THIS bus's server-minted id (invariant 1). AuthorizePeerAck
	// measures the acknowledging peer against it so a peer claiming OUR
	// namespace is refused before anything else.
	BusID string

	// Obligations is the durable record of what this bus owes which peer,
	// satisfied by *Outbox and by nothing else in production — §6.2's whole
	// point is that the binding is computed from state this bus ALREADY DURABLY
	// WROTE, and a second source would be a second answer to "did we owe this?".
	//
	// Required. A nil table would make every ACK unbindable, which LOOKS like a
	// working anti-forgery rule and is actually a total outage.
	Obligations AckObligations

	// NextHopAddress resolves the address THIS BUS would dial to reach a given
	// bus id, and is the third clause of AuthorizePeerAckVia's indirect arm — the
	// transit case a multi-hop chain needs, where the acknowledging peer is the
	// NEXT HOP and the outbox job is keyed on the DESTINATION. Pass
	// Registry.PeerBaseURL as the METHOD VALUE; it is called concurrently and
	// takes the registry's RLock.
	//
	// Optional. NIL MEANS THE DIRECT ARM ONLY — correct for a bus with no static
	// routes, and byte-for-byte the pre-ACK-5 behaviour. It never widens a
	// refusal: a nil table fails closed to AuthorizePeerAck's own answer.
	NextHopAddress NextHopAddress

	// Admit meters this surface BY THE AUTHENTICATED PEER, before the outbox
	// mutex is ever taken. It returns a release to call when the request is
	// done, or an error to refuse with.
	//
	// Required, and required AT CONSTRUCTION rather than defaulted to
	// "unmetered": see the file header for what sits behind it. A route built
	// without one would look identical in every test and would hand any peered
	// bus an O(n) exclusive sweep per frame.
	Admit func(peerBusID string) (release func(), err error)

	// SettleAck makes the outcome DURABLE and reports whether it was a
	// duplicate. It is where the wiring site drives internal/ack's Store —
	// invariant 4: this handler's 200 IS an acknowledgement, so the transition
	// must be committed and fsynced before it is sent.
	//
	// It owns the vocabulary translation onto internal/ack's spellings, and it
	// must return relay's own sentinels for the two cases the wire cares about:
	// ErrAckNotBound when no ACK row exists for this (key, recipient) — §8.2's
	// "(none)" row, which is the half of authorization the job binding cannot do
	// — and ErrAckOutcomeConflict for invariant 10's protocol-violation case.
	// Anything else is answered "not now".
	//
	// Required: a nil callback would validate and authorize an ACK and then
	// silently discard it, which is indistinguishable from recording it.
	SettleAck func(ctx context.Context, s SettledAck) (AckSettlement, error)

	// Logger receives the detailed, peer-supplied-byte-quoting failures the wire
	// response deliberately omits. Optional; nil discards.
	Logger *logging.Logger

	// MaxRequestBytes overrides MaxAckBytes, for tests that want to prove the
	// bound without building a maximum frame. Zero means MaxAckBytes; a negative
	// value is a construction error.
	MaxRequestBytes int64
}

// AckHandler answers PeerAckPath. It is deliberately not an http.Handler; see
// the file header.
type AckHandler struct {
	busID       string
	obligations AckObligations
	nextHop     NextHopAddress
	admit       func(string) (func(), error)
	settle      func(context.Context, SettledAck) (AckSettlement, error)
	log         *logging.Logger
	maxBytes    int64

	accepted   atomic.Uint64
	duplicates atomic.Uint64
	conflicts  atomic.Uint64
	refused    atomic.Uint64
	throttled  atomic.Uint64
}

// NewAckHandler validates cfg and returns the peer ACK route.
func NewAckHandler(cfg AckConfig) (*AckHandler, error) {
	if err := ids.ValidateBusID(cfg.BusID); err != nil {
		return nil, fmt.Errorf("relay: ack handler bus id: %w", err)
	}
	if cfg.Obligations == nil {
		return nil, errors.New("relay: AckConfig.Obligations is required; the obligation binding rule (ACK-CONTRACT.md §6.2) is the anti-forgery core of the ACK plane, and a handler with no obligation table would refuse every acknowledgement — an outage that looks exactly like a working guard")
	}
	if cfg.Admit == nil {
		return nil, errors.New("relay: AckConfig.Admit is required; AuthorizePeerAck takes the outbox's EXCLUSIVE mutex and runs an O(n) sweep, so an unmetered ACK surface is a denial of service against every writer on this bus")
	}
	if cfg.SettleAck == nil {
		return nil, errors.New("relay: AckConfig.SettleAck is required; without it the handler would authorize an acknowledgement and silently discard it, which is indistinguishable from recording it")
	}
	if cfg.MaxRequestBytes < 0 {
		return nil, fmt.Errorf("relay: AckConfig.MaxRequestBytes is %d; it must be zero (meaning %d) or positive", cfg.MaxRequestBytes, MaxAckBytes)
	}
	max := cfg.MaxRequestBytes
	if max == 0 {
		max = MaxAckBytes
	}
	log := cfg.Logger
	if log == nil {
		log = logging.New(io.Discard, logging.LevelError)
	}
	return &AckHandler{
		busID:       cfg.BusID,
		obligations: cfg.Obligations,
		nextHop:     cfg.NextHopAddress,
		admit:       cfg.Admit,
		settle:      cfg.SettleAck,
		log:         log,
		maxBytes:    max,
	}, nil
}

// Stats reports this handler's counters.
func (h *AckHandler) Stats() AckStats {
	return AckStats{
		Accepted:   h.accepted.Load(),
		Duplicates: h.duplicates.Load(),
		Conflicts:  h.conflicts.Load(),
		Refused:    h.refused.Load(),
		Throttled:  h.throttled.Load(),
	}
}

// ServeAuthenticated answers one ACK POST on behalf of an ALREADY-AUTHENTICATED
// peer bus.
//
// peerBusID MUST be httpapi.PeerPrincipal.BusID and MUST NOT come from anything
// a remote party can influence — not a frame field (there is none), not a
// header, not a query parameter. The whole of ack.go's anti-forgery argument
// rests on this one string. See the file header.
//
// # THE STATUS MAPPING (§9.3), AND WHY EACH ONE IS WHAT IT IS
//
// It mirrors the relay ingress deliberately, for the reason relayhttp.go already
// gives at length: a 4xx/5xx on a settled, not-your-fault outcome makes
// retry/backoff amplify exactly the traffic the mechanism exists to stop.
//
//   - 405 / 415 / 413 — the transport answers, identical to the other three peer
//     surfaces so a peer operator sees one vocabulary.
//
//   - 503 CodeUnavailable FOR A METERED REFUSAL. It is "not now", and
//     PeerRefusedError.Retriable treats 5xx as retriable, so an honest peer backs
//     off and re-offers the SAME TERMINAL OUTCOME later. This is the one status
//     choice on this route where getting it wrong LOSES DATA: a 4xx is FINAL, so
//     a throttled ACK answered 4xx would be abandoned by the sender and the
//     recipient's decision would never reach the origin at all. Nothing durable
//     was written for it either way.
//
//   - 400 CodeInvalidRequest for a malformed frame, an unrecognised class or
//     outcome, a class the outcome does not own, or an attestation of the wrong
//     shape. Every one of those is decidable by the sender from its own bytes
//     without asking us, so telling it exactly which is not an oracle — it just
//     makes an honest peer's encoder bug debuggable.
//
//   - 400 CodeUnsupportedAckVersion for a version we do not speak. Its own code
//     because the remedy is an operator upgrading a binary, not a developer
//     fixing an encoder, and the code is the entire diagnosis the far end sees.
//
//   - 409 CodeIdempotencyViolation for BOTH "no obligation binds you to this
//     key" and "this pair already settled differently", which share one code
//     DELIBERATELY. ack.go's ErrAckNotBound doc has the argument: distinct
//     answers would let any peered bus probe "did bus A send message K to bus
//     B", and by extension whether a named agent exists and is being written to.
//     It is the deliberate analogue of the 409 no-matching-reservation
//     indistinguishability invariant 10 preserves, and like it, it must not be
//     "fixed" by making the cases distinguishable.
//
//   - 200 for an accept AND for an idempotent replay, with duplicate telling
//     them apart. Invariant 10: return the original result, re-apply nothing,
//     disconnect nobody. Punishing a retry would break exactly the peers doing
//     the right thing.
//
// NOTHING HERE CLOSES A CONNECTION. See the file header.
func (h *AckHandler) ServeAuthenticated(w http.ResponseWriter, r *http.Request, peerBusID string) {
	if peerBusID == "" {
		// UNREACHABLE behind RequirePeerPrincipal, which refuses before the
		// handler runs. Checked anyway and answered as OUR fault, because the one
		// way this fires is a mount that forgot the principal — and an empty peer
		// id would derive a job id nobody owns, refuse every legitimate ACK with
		// the uniform answer, and look exactly like a working anti-forgery rule.
		h.fail(w, http.StatusInternalServerError, CodeInternal,
			errors.New("relay: the ack route was reached with no authenticated peer bus id; the mount must supply httpapi.PeerPrincipal.BusID and must never read it from the frame"))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.fail(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, fmt.Errorf("relay: %s is not allowed on %s", r.Method, PeerAckPath))
		return
	}
	if err := checkJSONContentType(r.Header.Get("Content-Type")); err != nil {
		h.fail(w, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, err)
		return
	}

	// THE METER, BEFORE THE BODY AND LONG BEFORE THE OUTBOX MUTEX. See the file
	// header for why this is here rather than beside AuthorizePeerAck: the point
	// is to bound the number of concurrent O(n) exclusive sweeps one peer can
	// drive, and to do it before spending any of this bus's memory bandwidth on
	// its body.
	release, err := h.admit(peerBusID)
	if err != nil {
		h.throttled.Add(1)
		// 503, NOT 429 and NOT a 4xx. Both 429 and 503 are Retriable, but 503 is
		// the answer this package already gives for every "not now" on the relay
		// surface, and one vocabulary across the two peer routes is worth more
		// than the extra shade of meaning. NOTHING WAS WRITTEN.
		h.fail(w, http.StatusServiceUnavailable, CodeUnavailable, err)
		return
	}
	if release != nil {
		// A nil release from a non-error admit is a bug in the METER, not in this
		// handler, and `defer release()` would panic on it. net/http recovers a
		// handler panic per connection, so the symptom would be a peer's request
		// mysteriously dropped mid-federation — the least diagnosable failure
		// this route could have. Tolerating nil here costs one branch and turns a
		// meter bug into a metered request that never releases its slot, which is
		// LOUD (that peer stops being admitted) rather than silent.
		defer release()
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

	// AckSurfacePeer is passed as a LITERAL, from the mount, and is never read
	// from the frame: only the caller knows which gate authenticated this
	// request, and inferring the label from the frame's contents would let an
	// agent-surface outcome be recorded as though an adjacent BUS had vouched
	// for it. See AckSurface.
	ack, err := ValidatePeerAckRequest(req, AckSurfacePeer)
	if err != nil {
		h.fail(w, http.StatusBadRequest, ErrorCode(err), err)
		return
	}

	// LAYER 2, THE ANTI-FORGERY CORE (§6.2). peerBusID is the AUTHENTICATED one.
	obligation, err := AuthorizePeerAckVia(h.obligations, h.nextHop, h.busID, peerBusID, ack.CorrelationKey, ack.Recipient)
	if err != nil {
		h.refuse(w, peerBusID, req, err)
		return
	}

	settlement, err := h.settle(r.Context(), SettledAck{Ack: ack, PeerBusID: peerBusID, Obligation: obligation})
	if err != nil {
		switch {
		case errors.Is(err, ErrAckNotBound):
			// §8.2's "(none)" row: the job binding passed, but no ACK row exists
			// for this (key, recipient) — the recipient was never one the sender
			// named. The two halves of authorization are CONJUNCTIVE and neither
			// is sufficient alone (ack.go). Answered with the SAME uniform 409 as
			// an unbound key, on purpose: telling them apart would disclose which
			// recipients a message named.
			h.refuse(w, peerBusID, req, err)
			return
		case errors.Is(err, ErrAckOutcomeConflict):
			h.conflicts.Add(1)
			// REJECT AND LOG, AND THAT IS THE WHOLE REMEDY. Terminal is
			// absorbing: the first terminal stands. The peer is deliberately NOT
			// disconnected — this link carries its entire roster's traffic.
			//
			// The error carries only closed-enum spellings (DecideAck builds it
			// from AckOutcome and AckClass), so logging it verbatim discloses
			// nothing a peer chose the bytes of. The IDS are elided separately.
			h.log.Warn("acknowledgement REJECTED: a DIFFERENT terminal outcome is already recorded for this pair (invariant 10). Rejected and logged, and that is the WHOLE remedy — the peer is deliberately NOT disconnected, because this link carries every agent behind it",
				append([]interface{}{"local_bus", h.busID, "conflict", err.Error()},
					AckRefusalLogFields(peerBusID, req.CorrelationKey, req.Recipient)...)...)
			h.fail(w, http.StatusConflict, CodeIdempotencyViolation, err)
			return
		}
		// "NOT NOW". A durable write failed, or the store is not attached.
		// Retriable, and nothing was recorded.
		h.fail(w, http.StatusServiceUnavailable, CodeUnavailable, err)
		return
	}

	if settlement.Duplicate {
		h.duplicates.Add(1)
	} else {
		h.accepted.Add(1)
	}
	// ONLY VALIDATED, CLOSED-ENUM AND BOUNDED VALUES ARE LOGGED. The correlation
	// key and the recipient have been through AuthorizePeerAck's id validation by
	// this point, so they are bounded by construction; the outcome and class are
	// enum spellings this package owns.
	h.log.Info("peer acknowledgement recorded",
		"local_bus", h.busID,
		"peer_bus", peerBusID,
		"correlation_key", ack.CorrelationKey,
		"recipient", ack.Recipient,
		"outcome", ack.Outcome.String(),
		"class", ack.Class.String(),
		"attested_by", ack.Attestation.String(),
		"job_id", obligation.JobID,
		"duplicate", settlement.Duplicate,
		"protocol_version", ack.ProtocolVersion,
		"emitted_at_unix_ms", ack.EmittedAtUnixMilli,
	)
	writeJSONBody(w, h.log, http.StatusOK, PeerAckResponse{Accepted: true, Duplicate: settlement.Duplicate})
}

// refuse answers THE ONE UNIFORM REFUSAL and logs the redacted fields that tell
// an operator which obligation it was about.
//
// It is a single function so that every uniform refusal produces a
// byte-identical response: two call sites building the answer separately is how
// one of them acquires a distinguishing detail and re-opens the oracle
// ErrAckNotBound exists to close.
//
// ErrorCode maps ErrAckNotBound, ErrAckOutcomeConflict and ErrInvalidAckFrame
// (ACK-4 added those arms), so a malformed id inside AuthorizePeerAck is
// correctly answered 400 rather than folded into the 409 — a bad id is decidable
// by the sender from its own bytes and leaks nothing.
func (h *AckHandler) refuse(w http.ResponseWriter, peerBusID string, req PeerAckRequest, err error) {
	// A MALFORMED ID IS 400 AND EVERYTHING ELSE IS THE UNIFORM 409.
	// AuthorizePeerAck validates the correlation key and the recipient itself and
	// reports a bad one as ErrInvalidAckFrame, which is decidable by the sender
	// from its own bytes and therefore leaks nothing; folding it into the uniform
	// answer would only make an honest peer's encoder bug undebuggable. Every
	// other error reaching here — the reflection, cross-route and third-party
	// cases, and the missing ACK row — is ErrAckNotBound and gets the ONE answer.
	status := http.StatusConflict
	if errors.Is(err, ErrInvalidAckFrame) {
		status = http.StatusBadRequest
	}
	// The RAW request fields are passed, not the validated ones: on this path
	// validation may not have run, so these are unbounded peer bytes and
	// AckRefusalLogFields is the ONE redaction point that clamps them.
	h.log.Warn("acknowledgement REFUSED. Every well-formed acknowledgement this bus will not settle is answered IDENTICALLY on the wire — an unknown key, a key routed via a different peer and a key naming a third bus are told apart ONLY here, because distinguishing them on the wire would let any peered bus probe which messages this bus sent to whom. NOT a disconnect",
		append([]interface{}{"local_bus", h.busID, "status", status},
			AckRefusalLogFields(peerBusID, req.CorrelationKey, req.Recipient)...)...)
	h.fail(w, status, ErrorCode(err), err)
}

// decodeRequest reads at most maxBytes and decodes strictly.
//
// decodeStrict sets DisallowUnknownFields, and on THIS frame that is load-bearing
// rather than merely tidy: it is what makes "there is no bus-id field" a rule a
// peer cannot route around by inventing one (see ackframe.go's header). It is
// also the hazard RELAY-51 owns — an OLDER peer receiving a frame with a field
// it does not know answers 400, which PeerRefusedError.Retriable treats as
// FINAL. That is a rollout-ordering constraint on whoever ADDS a field to this
// frame, not a reason to loosen the decoder: see Client.PeerAck.
func (h *AckHandler) decodeRequest(body io.Reader) (PeerAckRequest, error) {
	buf, err := io.ReadAll(io.LimitReader(body, h.maxBytes+1))
	if err != nil {
		return PeerAckRequest{}, fmt.Errorf("%w: reading body: %v", ErrInvalidRequest, err)
	}
	if int64(len(buf)) > h.maxBytes {
		return PeerAckRequest{}, fmt.Errorf("%w: body exceeds %d bytes", ErrPayloadTooLarge, h.maxBytes)
	}
	var req PeerAckRequest
	if err := decodeStrict(buf, &req); err != nil {
		// THE DECODER'S MESSAGE IS ELIDED, and this is the one place on this
		// route that would otherwise skip the redaction point.
		// encoding/json reports an unrecognised field as `unknown field "…"`,
		// quoting the peer's own bytes verbatim — so an authenticated peer could
		// choose most of MaxAckBytes' worth of operator-log line, once per frame.
		// It is bounded either way and is 64x smaller than the same path on the
		// relay surface, but "bounded" is not the standard the rest of this file
		// holds: every other peer-chosen string here goes through elideAck, and
		// a security gate found this one did not (ACK-3, 2026-08-16).
		//
		// The SENTINEL is preserved by re-wrapping rather than replaced, so
		// ErrorCode still answers CodeInvalidRequest.
		return PeerAckRequest{}, fmt.Errorf("%w: %s", ErrInvalidRequest, elideAck(err.Error()))
	}
	return req, nil
}

// fail is the ONE place Refused is incremented, so a path that both logs a
// specific reason and then fails cannot double-count.
func (h *AckHandler) fail(w http.ResponseWriter, status int, code string, err error) {
	h.refused.Add(1)
	failJSON(w, h.log.With("surface", "peer-ack", "local_bus", h.busID), status, code, err)
}

// ---------------------------------------------------------------------------
// The sending half
// ---------------------------------------------------------------------------

// PeerAck POSTs ONE terminal outcome to ONE adjacent peer and returns its
// settled answer.
//
// peerBaseURL is the peer's base URL; PeerAck appends PeerAckPath itself, so the
// path is never a caller's typo, and peerURL enforces the https-only origin rule
// (invariant 11). The redirect policy NewClient installs (http.ErrUseLastResponse)
// is inherited and MUST NOT be weakened, for the reason Client.Relay gives: Go's
// default policy would replay this POST at whatever host a 3xx names.
//
// # THE ROLLOUT ORDERING THIS FRAME REQUIRES — READ BEFORE DEPLOYING
//
// PeerAckPath is a NEW ROUTE. A peer running a binary from before this task does
// not serve it and answers 404, which PeerRefusedError.Retriable treats as FINAL
// (every 4xx except 408/429 is "never"). So an ACK sent to a not-yet-upgraded
// peer is ABANDONED, not retried, and the recipient's decision never reaches the
// origin.
//
// THE ORDERING IS THEREFORE: UPGRADE RECEIVERS BEFORE SENDERS. Every bus that
// might be ACKed must serve PeerAckPath before any bus starts emitting to it.
// Concretely, in a federation, roll the whole fleet before enabling any ACK
// emission — which is why nothing in this build calls this method yet: ACK-5
// owns the emission, and it lands after every bus can receive.
//
// A 404 is DISTINGUISHABLE from every other refusal here (CodeUnknownRecipient
// is a 404 on the relay route, but this route never emits it), so an operator
// diagnosing a partial rollout has an unambiguous signal. It is NOT converted
// into a retriable status: pretending a route exists would have the sender
// re-offer it for the whole retry horizon against a bus that will never grow one
// until it is restarted.
//
// # AND THE HAZARD THIS METHOD MUST NOT MAKE WORSE (RELAY-51)
//
// The RESPONDER decodes with DisallowUnknownFields, so a peer that adds a field
// to this frame in a later version breaks OLDER peers with a 400 — which is
// again FINAL, so the outcome is lost rather than retried. RELAY-51 (P0) owns
// that defect on the relay envelope and it is identical here. THIS TASK DOES NOT
// FIX IT AND MUST NOT MAKE IT WORSE: the frame ships complete, with its version
// field spent, so the first task that needs a new field has a version to bump and
// ACK-10 has a field to negotiate on.
func (c *Client) PeerAck(ctx context.Context, peerBaseURL string, req PeerAckRequest) (PeerAckResponse, error) {
	// The version is set HERE rather than trusted from the caller: every frame
	// this bus emits declares the version this bus speaks, which is what makes
	// an ABSENT version unambiguously mean "written before the field existed".
	req.ProtocolVersion = AckWireVersion

	endpoint, err := peerURL(peerBaseURL, PeerAckPath)
	if err != nil {
		return PeerAckResponse{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return PeerAckResponse{}, fmt.Errorf("relay: encoding acknowledgement for %s: %w", req.CorrelationKey, err)
	}
	if len(body) > MaxAckBytes {
		// Sending a body we would refuse to receive is an asymmetry that only
		// ever shows up as a confusing 413 from the far end. It cannot fire for a
		// legal frame — MaxAckBytes is derived to fit the widest one — so if it
		// does, an id got past its own maximum somewhere upstream.
		return PeerAckResponse{}, fmt.Errorf("%w: encoded acknowledgement is %d bytes, over the %d cap", ErrPayloadTooLarge, len(body), MaxAckBytes)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return PeerAckResponse{}, fmt.Errorf("relay: building acknowledgement request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return PeerAckResponse{}, fmt.Errorf("relay: acknowledging %s to %s: %w", req.CorrelationKey, endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxAckBytes))
		_ = resp.Body.Close()
	}()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, MaxAckBytes+1))
	if err != nil {
		return PeerAckResponse{}, fmt.Errorf("relay: reading acknowledgement response from %s: %w", endpoint, err)
	}
	if len(buf) > MaxAckBytes {
		return PeerAckResponse{}, fmt.Errorf("%w: response from %s exceeds %d bytes", ErrPayloadTooLarge, endpoint, MaxAckBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return PeerAckResponse{}, &PeerRefusedError{Endpoint: endpoint, StatusCode: resp.StatusCode, Code: peerErrorCode(buf)}
	}

	var decoded PeerAckResponse
	if err := decodeStrict(buf, &decoded); err != nil {
		return PeerAckResponse{}, fmt.Errorf("decoding acknowledgement response from %s: %w", endpoint, err)
	}
	c.log.Debug("acknowledgement answered",
		"local_bus", c.busID,
		"peer", endpoint,
		"correlation_key", req.CorrelationKey,
		"accepted", decoded.Accepted,
		"duplicate", decoded.Duplicate,
	)
	return decoded, nil
}
