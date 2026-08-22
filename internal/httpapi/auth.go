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
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/logging"
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
// The largest legitimate request here is an enrolment: a 64-byte name, TWO
// 44-byte base64 Ed25519 public keys (auth and messaging), a 128-byte
// idempotency key and — since INVITE-GATE — an invite id (at most
// invite.MaxInviteIDLen) with its base64url secret (at most
// invite.MaxSecretLen), which together move the legitimate maximum by roughly
// another 90 bytes. That is still under half a kilobyte with JSON overhead. 8
// KiB is generous by an order of magnitude and still finite, which is the
// point: an unauthenticated caller must not be able to stream unbounded bytes
// into json.Decode. Neither the second key nor the invite fields came close to
// needing this bound revisited.
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

	// MessagingPublicKey is the base64 (standard encoding, padded) Ed25519
	// MESSAGING public key — the key PEERS verify this agent's signed messages
	// with, and the key this bus attests to a peer bus. It is a DIFFERENT key
	// from PublicKey, it must NOT be the same value, and an enrolment presenting
	// one key for both roles is refused in internal/auth.
	//
	// WHY THEY MUST DIFFER, stated with the right control named. It is NOT that
	// the bus holds anything it could forge with — the bus holds only the PUBLIC
	// half of both keys and can forge with neither.
	//
	// The hazard is that the bus CHOOSES THE BYTES THE AUTH KEY SIGNS: the
	// session handshake has the server issue a token and the client sign it
	// (invariant 3). One key serving both roles would put a server-chosen input
	// under the key peers verify with.
	//
	// What actually closes that today is DOMAIN SEPARATION, not this rule: a
	// session challenge always begins with the 'a' of auth.SessionSigningContext
	// and a canonical message always begins with the 0x00 of a uint32 length, so
	// no session signature can be read as a message signature (see the first-byte
	// argument in internal/signing/canonical.go). Do not overstate this check —
	// it is not the thing standing between an agent and a forged message.
	//
	// Key separation is what makes the property STRUCTURAL: it stops the
	// guarantee depending on every future signing domain staying disjoint, it
	// bounds the blast radius of one compromised key to one role, and it lets the
	// two rotate on independent schedules.
	//
	// It is client-supplied MATERIAL, not identity. It is validated as input —
	// standard base64, decoding to exactly an Ed25519 public key — and it has no
	// influence whatsoever on the agent id the server mints (invariant 1).
	//
	// OPTIONAL TODAY, and an empty/absent value is accepted: agents enrolled
	// before the field existed have none, and their durable records must keep
	// loading. Making it REQUIRED for new enrolments is the intended end state,
	// and AT THE TIME OF WRITING NO FOLLOW-UP TASK HAS BEEN FILED for it — do not
	// read "intended" as "scheduled", and name the task here once one exists. See
	// CONTRACTS-HTTP.md, which is the contract of record for which of the two
	// this build enforces.
	MessagingPublicKey string `json:"messaging_public_key"`

	// IdempotencyKey makes the enrolment safe to retry (invariant 10).
	IdempotencyKey string `json:"idempotency_key"`

	// InviteID is the id of the invite this enrolment redeems. REQUIRED on a bus
	// that enforces invite-only enrolment (invariant 3) — read
	// DiscoveryEnrolment.InviteRequired from /v1/info to find out, rather than
	// assuming either way; the bus cmd/agent-bus builds enforces it, and an
	// enrolment presenting no invite there is refused 403. It must be presented
	// TOGETHER with InviteSecret; one without the other is a 400.
	//
	// Presenting an invite to a build with no invite store is 501, never a
	// silent success: a client must never walk away believing its single-use
	// credential was spent when it was not.
	InviteID string `json:"invite_id"`

	// InviteSecret is the invite's plaintext BEARER CREDENTIAL, exactly as the
	// operator handed it out (invite.Minted.Secret).
	//
	// IT IS NEVER LOGGED, NEVER ECHOED AND NEVER APPEARS IN AN ERROR, on any
	// path in this package — the same discipline a session token gets, and for
	// the same reason: whoever holds it can enrol an agent onto this bus
	// (DECISIONS.md, E6 — the invite blob is the trust anchor). internal/invite
	// drops it the moment it has been verified.
	//
	// REQUIRED whenever InviteID is, and on the same condition — see InviteID.
	InviteSecret string `json:"invite_secret"`
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

	// The messaging key is decoded ONLY when the client sent one. An absent
	// field decodes to "" and must stay a valid enrolment (see
	// EnrolRequestBody.MessagingPublicKey).
	//
	// BE PRECISE ABOUT WHAT THIS GUARD DOES AND DOES NOT BUY. It is NOT
	// correctness: "" is valid base64 for zero bytes, so routing it through
	// decodeBase64Field would yield an empty slice, and internal/auth compares
	// with bytes.Equal, which does not distinguish nil from empty — an earlier
	// draft of this comment claimed the idempotency comparison would change, and
	// it would not. What the guard buys is that a client which simply omitted an
	// optional field does not generate a decode attempt, and cannot appear in a
	// debug log line as a field that was parsed.
	//
	// A PRESENT one is validated to the letter: standard base64 here, exact
	// Ed25519 length in internal/auth, both before it is stored and long before
	// any verifier is handed it.
	var msgPub []byte
	if body.MessagingPublicKey != "" {
		var okMsg bool
		msgPub, okMsg = s.decodeBase64Field(w, r, "messaging_public_key", body.MessagingPublicKey)
		if !okMsg {
			return
		}
	}

	// THE INVITE, WHEN ONE IS PRESENTED. Presenting one is OPTIONAL in this
	// build and enrolment without one is accepted unchanged — the whole no-invite
	// path below is byte-for-byte what it was before INVITE-GATE.
	presented := body.InviteID != "" || body.InviteSecret != ""
	var redemption auth.InviteRedemption
	if presented {
		if body.InviteID == "" || body.InviteSecret == "" {
			// Refused rather than treated as no invite at all: a client that sent
			// half a credential has a bug, and quietly enrolling it WITHOUT the
			// invite would leave it believing its single-use invite was spent.
			// Neither value is echoed.
			s.log.Debug("enrolment presented only half an invite",
				"request_id", RequestIDFromContext(r.Context()),
				"have_invite_id", body.InviteID != "",
			)
			s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "invite_id and invite_secret must be presented together"})
			return
		}
		if s.invites == nil {
			// 501, the same posture /v1/broadcast takes for a capability this
			// build does not implement. NOT a silent ignore: see
			// Options.Invites.
			//
			// The id goes through inviteIDLogFields, NOT into the line raw: this
			// branch is reached before anything has validated it.
			kv := append([]interface{}{
				"request_id", RequestIDFromContext(r.Context()),
			}, inviteIDLogFields(body.InviteID)...)
			s.log.Info("an invite was presented to a bus built without an invite store; refused with 501 rather than enrolling without it", kv...)
			s.writeJSON(w, r, http.StatusNotImplemented, ErrorResponse{Error: "this bus does not redeem invites"})
			return
		}

		// The key's SHAPE is validated here, BEFORE Begin, because invite.Begin
		// rejects a malformed key as ErrInvalidRecord, which writeInviteError maps
		// to a 500 — a client error reported as a server one. idem.ValidateKey is
		// byte-for-byte the same rule auth.validateIdempotencyKey applies (both:
		// non-empty, at most 128 bytes, [A-Za-z0-9._-] only), so the invited and
		// un-invited paths cannot disagree about which keys are acceptable.
		if err := idem.ValidateKey(body.IdempotencyKey); err != nil {
			s.log.Debug("enrolment idempotency key rejected before invite redemption",
				"request_id", RequestIDFromContext(r.Context()),
				"err", err,
			)
			s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: "invalid idempotency key"})
			return
		}

		// THE FINGERPRINT FIELD LIST AND ORDER, DOCUMENTED AT THE CALL SITE as
		// invite.RedeemRequest.Fingerprint requires. It is, in this exact order:
		//
		//	1. name                (the requested agent name)
		//	2. public_key          (the DECODED Ed25519 auth key bytes)
		//	3. messaging_public_key(the DECODED messaging key bytes, empty if absent)
		//	4. invite_id           (the invite being redeemed)
		//
		// That is everything the enrolment ASSERTS, so a key re-presented with any
		// different content is caught as ErrKeyReuse rather than answered with
		// somebody else's original result. The DECODED key bytes are hashed rather
		// than their base64 spelling because the decoder is Strict() — exactly one
		// spelling per key — so the two are equivalent, and the decoded form is
		// what internal/auth compares. The invite SECRET is deliberately NOT in the
		// list: it is a bearer credential, it is already proved by Begin, and
		// hashing it would put a credential-derived value in a durable record.
		fp := idem.ComputeFingerprint([]byte(body.Name), pub, msgPub, []byte(body.InviteID))

		red, err := s.invites.Begin(invite.RedeemRequest{
			InviteID:    body.InviteID,
			Secret:      body.InviteSecret,
			Key:         body.IdempotencyKey,
			Fingerprint: fp,
		})
		if err != nil {
			s.writeInviteError(w, r, body.InviteID, err)
			return
		}

		if red.Outcome() == invite.OutcomeReplay {
			// A LEGITIMATE RETRY of a redemption this bus already applied: return
			// the ORIGINAL result, verbatim, apply nothing, and do NOT punish the
			// client (invariant 10).
			resp := red.Result()
			if len(resp) == 0 {
				// A redeemed record with no stored result is THIS SERVER's bug, not
				// the client's — the record should have carried the 201 body — so it
				// is a 500 and an ERROR line, never a 4xx blaming the caller.
				s.log.Error("an invite redemption replay carries no stored result; the durable record was written without the response it must replay",
					"request_id", RequestIDFromContext(r.Context()),
					"invite_id", body.InviteID,
				)
				s.writeJSON(w, r, http.StatusInternalServerError, ErrorResponse{Error: "internal error"})
				return
			}
			w.Header().Set(IdempotencyReplayedHeader, "true")
			s.log.Info("enrolment replayed from the invite redemption record",
				"request_id", RequestIDFromContext(r.Context()),
				"invite_id", body.InviteID,
			)
			// writePreformattedJSON's rule is that the body must never be DERIVED
			// FROM REQUEST INPUT, and these bytes are not: they are this server's
			// OWN earlier 201 body, read back out of the durable invite record —
			// bounded by idem.MaxResultBytes and already validated as JSON by
			// invite.DecodeRecord on the way in. Returning them VERBATIM is exactly
			// what invariant 10 requires of a legitimate retry: the body a retry
			// parses must be indistinguishable from the original's.
			s.writePreformattedJSON(w, r, http.StatusCreated, append(resp, '\n'))
			return
		}
		redemption = &inviteRedemption{red: red, inviteID: body.InviteID, log: s.log}
	}

	res, err := s.auth.Enrol(auth.EnrolRequest{
		Name:               body.Name,
		PublicKey:          pub,
		MessagingPublicKey: msgPub,
		IdempotencyKey:     body.IdempotencyKey,
		// nil unless an invite was presented; a nil Invite is an UN-INVITED
		// enrolment and is accepted.
		Invite: redemption,
		// THE BINDING (MTLS-BIND, invariant 11). Taken from the CONNECTION, via
		// the middleware that read r.TLS — never from body, which is a
		// client-supplied claim about a certificate anyone could name. nil when
		// the connection presented none, or presented one that was out of date,
		// and nil is accepted: see auth.EnrolRequest.ClientCertFingerprint.
		ClientCertFingerprint: enrolCertFingerprint(r),
	})
	if err != nil {
		// The NAME is logged, the public key is not: a name is what an operator
		// needs to correlate a rejected enrolment, and a key in a log line is
		// noise at best.
		s.writeAuthError(w, r, "enrol", err, agentNameLogFields(body.Name)...)
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
	s.writeJSON(w, r, http.StatusCreated, enrolResponseBody(res))
}

// enrolResponseBody renders the 201 body of POST /v1/enroll.
//
// It is a shared helper and not an inline literal because the SAME body is
// produced in two places — here for the live 201, and in inviteRedemption.
// Consume for the bytes stored in the durable invite record and replayed
// verbatim to a legitimate retry. Two literals would be free to drift, and the
// drift would be invisible until a client compared an original with its retry.
func enrolResponseBody(res auth.EnrolResult) EnrolResponseBody {
	return EnrolResponseBody{
		AgentID:    res.AgentID,
		BusID:      res.BusID,
		Name:       res.Name,
		EnrolledAt: formatInstant(res.EnrolledAt),
	}
}

// inviteRedemption adapts *invite.Redemption to auth.InviteRedemption, so
// internal/auth can compose the enrolment and the invite consumption into ONE
// durable transaction without importing internal/invite.
//
// It holds no secret: invite.Store.Begin drops the presented secret the moment
// it verifies it, and nothing here ever sees it again.
type inviteRedemption struct {
	red      *invite.Redemption
	inviteID string
	log      *logging.Logger
}

// InviteID implements auth.InviteRedemption. It returns the id from the REQUEST,
// which Begin has already validated and matched against a stored secret digest.
func (a *inviteRedemption) InviteID() string { return a.inviteID }

// RiderKind implements auth.InviteRedemption: the wal.Entry.Kind the consumption
// record replays as.
func (a *inviteRedemption) RiderKind() string { return invite.RecordKind }

// Consume implements auth.InviteRedemption: it builds the durable consumption
// record, carrying the agent id the enrolment minted and the EXACT bytes the 201
// will return, so a later retry replays a byte-identical body.
func (a *inviteRedemption) Consume(res auth.EnrolResult) (json.RawMessage, error) {
	b, err := json.Marshal(enrolResponseBody(res))
	if err != nil {
		return nil, err
	}
	return a.red.Consume(invite.Result{AgentID: res.AgentID, Response: b})
}

// Commit implements auth.InviteRedemption. It returns nothing on purpose: by the
// time it runs the enrolment is DURABLE, and the only error invite.Redemption.
// Commit returns is caller misuse (commit without consume), which this path
// cannot produce. Reporting a committed enrolment to the client as failed would
// be strictly worse than a serving-copy discrepancy a restart repairs from the
// log — so the error is LOGGED HERE, at ERROR, and goes no further.
func (a *inviteRedemption) Commit() {
	if err := a.red.Commit(); err != nil {
		a.log.Error("folding a DURABLE invite consumption into the serving copy failed; the durable log is the truth and a restart will rebuild from it",
			"invite_id", a.inviteID, "err", err)
	}
}

// Abort implements auth.InviteRedemption: it releases the reservation. It is
// called only when nothing became durable — see auth.Service.Enrol's resolve
// guard — and invite.Redemption.Abort is a documented no-op after a successful
// Commit and on a replay.
func (a *inviteRedemption) Abort() { a.red.Abort() }

// writeInviteError maps an internal/invite failure to a status code and answers.
//
// # THE COLLAPSE IS MANDATORY, AND internal/invite/errors.go SAYS SO
//
// ErrUnknownInvite, ErrExpired, ErrRevoked, ErrAlreadyRedeemed and
// ErrInvalidInviteID all get the SAME status and the SAME body. The distinct
// sentinels exist for the OPERATOR — they are logged here, server-side, with the
// invite id when it is a VALID one (inviteIDLogFields) — but the set of answers
// is an oracle for "does invite X exist" and
// "is invite X still live", which is exactly what an attacker enumerating invite
// ids wants. That is why this function exists at all rather than a per-sentinel
// mapping in writeAuthError.
//
// THE INVITE SECRET APPEARS NOWHERE: not in a log line, not in an error, not in
// a response, on any path here. It is a bearer credential.
func (s *Server) writeInviteError(w http.ResponseWriter, r *http.Request, inviteID string, err error) {
	// inviteIDLogFields, not a raw "invite_id": this function is reached for
	// ErrInvalidInviteID too, i.e. precisely when the id is client-chosen junk
	// that nothing has accepted.
	kv := append([]interface{}{
		"request_id", RequestIDFromContext(r.Context()),
		"op", "invite redeem",
	}, inviteIDLogFields(inviteID)...)
	kv = append(kv, "err", err)

	switch {
	case errors.Is(err, invite.ErrUnknownInvite),
		errors.Is(err, invite.ErrExpired),
		errors.Is(err, invite.ErrRevoked),
		errors.Is(err, invite.ErrAlreadyRedeemed),
		errors.Is(err, invite.ErrInvalidInviteID):
		// ONE status, ONE body. Info rather than Debug: a run of these is the
		// shape of somebody guessing invite ids, and an operator should see it by
		// default. The specific sentinel is in "err", server-side only.
		s.log.Info("invite redemption refused", kv...)
		s.writeJSON(w, r, http.StatusForbidden, ErrorResponse{Error: "invite not accepted"})

	case errors.Is(err, invite.ErrKeyReuse):
		// Invariant 10: same key + DIFFERENT payload is a protocol violation, not
		// a retry. Reject it and LOG it — and KEEP THE CONNECTION (narrowed
		// 2026-08-08). No "Connection: close" here, ever.
		//
		// The reasoning is the one writeAuthError already carries for
		// auth.ErrIdempotencyKeyReused: /v1/enroll is UNAUTHENTICATED, so the
		// socket identifies NO principal to punish — whoever owns it need not be
		// whoever sent the request — and dropping it destroys every other request
		// pipelined there, hitting an honest client part-way through obtaining a
		// credential. A merely BUGGY client reaches this line easily, which is the
		// first question invariant 10 demands before adding any disconnect.
		s.log.Warn("invite idempotency key reused with a different payload; rejected, and the connection is KEPT because this route is unauthenticated and the socket identifies no principal to punish", kv...)
		s.writeJSON(w, r, http.StatusConflict, ErrorResponse{Error: "idempotency key already used with a different payload"})

	case errors.Is(err, invite.ErrRedemptionInFlight):
		// A DISTINCT answer, and distinct is safe HERE AND ONLY HERE: Begin
		// reaches the in-flight check only AFTER the presented secret has
		// verified, so it can only be reported to the invite's holder and tells a
		// non-holder nothing. It is worth distinguishing because the remedy is
		// specific and temporary — retry, and the original's result will be there.
		s.log.Info("invite redemption refused: another transition for this invite is in flight", kv...)
		s.writeJSON(w, r, http.StatusConflict, ErrorResponse{Error: "another redemption of this invite is in flight; retry"})

	case errors.Is(err, invite.ErrCapacity):
		w.Header().Set("Retry-After", capacityRetryAfterSeconds)
		s.log.Warn("invite redemption refused at a capacity limit", kv...)
		s.writeJSON(w, r, http.StatusServiceUnavailable, ErrorResponse{Error: "server at capacity, retry later"})

	default:
		// ErrNotDurable, ErrInvalidRecord, ErrResultTooLarge and anything
		// unforeseen. Every one of them is THIS SERVER's problem, not the
		// client's, so it is a 500 with a fixed body and an ERROR line.
		s.log.Error("invite redemption failed", kv...)
		s.writeJSON(w, r, http.StatusInternalServerError, ErrorResponse{Error: "internal error"})
	}
}

// inviteIDLogFields renders a client-supplied invite id for a log line on the
// ENROL path: the value itself when it is a VALID id, and its LENGTH — never
// its bytes — when it is not.
//
// THIS IS ABOUT VOLUME, NOT ESCAPING, and restoring the raw value would undo
// it. logging.writeValue already runs every field through strconv.Quote, so an
// invite id is SAFE to write; the problem is that /v1/enroll needs no
// credential, this server rate-limits nothing, and MaxAuthRequestBytes lets an
// anonymous caller put ~1 KiB (internal/logging's per-value cap) of chosen
// bytes into an Info-level record, several times over, per cheap request. That
// is log amplification against a 161-byte baseline.
//
// A VALID id is logged in full and must stay that way: an operator correlating
// a refused redemption needs it, and it is bounded by
// invite.MaxInviteIDLen. Everything else gets "invite_id_len" only —
// deliberately NOT a truncated prefix, because a prefix of an attacker-chosen
// id is still attacker-chosen bytes and it invites the next reader to "just log
// a bit more". This is the same discipline invite.ValidateInviteID already
// applies when it refuses to echo an OVERSIZED id back (internal/invite/id.go).
//
// THE "err" FIELD IS THE OTHER HALF OF THIS, AND IT IS EASY TO MISS. This
// helper controls only the "invite_id" field; every writeInviteError line ALSO
// carries "err", and a wrapped sentinel can smuggle the id back into the record
// that this helper just took it out of. invite.ValidateInviteID's malformed-id
// branch did exactly that — it quoted the offending id — so before that was
// fixed (internal/invite/id.go) the raw value still reached the log through
// "err", bounded only by MaxInviteIDLen rather than by this function. Both the
// reviewer and the security gate found that independently, which is why it is
// written down rather than assumed obvious.
//
// So the rule is not "sanitise the id field", it is: NO ERROR ON THIS PATH MAY
// ECHO A CLIENT-SUPPLIED INVITE ID. A future sentinel that starts quoting one
// reopens this hole without touching a line of this file.
//
// The invite SECRET appears here on no path at all; it is a bearer credential.
func inviteIDLogFields(inviteID string) []interface{} {
	if err := invite.ValidateInviteID(inviteID); err != nil {
		return []interface{}{"invite_id_len", len(inviteID)}
	}
	return []interface{}{"invite_id", inviteID}
}

// agentNameLogFields renders the REQUESTED agent name for a log line, bounded
// and character-restricted, exactly as inviteIDLogFields does for an invite id.
//
// # Why a raw name may not be logged from the enrolment path
//
// POST /v1/enroll is unauthenticated by construction (invariant 3) and this
// server rate-limits nothing, so every field it echoes into a log line is a
// write primitive an anonymous caller controls the contents and the volume of.
// The name is bounded by the request body limit, not by anything smaller, so a
// refused enrolment could put roughly a kilobyte of attacker-chosen bytes into
// the operator's log — and INVITE-GATE-ENFORCE made refusals the COMMON case on
// a gated bus, which is what turned a latent issue into a real one (raised by
// the security gate, M1).
//
// ids.ValidateAgentName is the same rule Enrol validates against
// (^[a-z0-9][a-z0-9_-]{0,63}$), so a name that passes is at most 64 bytes from a
// safe alphabet and is exactly what an operator needs to correlate a refusal. A
// name that fails contributes its LENGTH only: an operator can still see that
// something malformed arrived, and learns its size, without the bytes.
//
// THE SAME RULE AS inviteIDLogFields, AND FOR THE SAME REASON — see that
// helper's comment. It is stated per-field rather than centrally because the
// hazard is per-field: a future log line that starts quoting body.Name directly
// reopens this without touching this function.
func agentNameLogFields(name string) []interface{} {
	if err := ids.ValidateAgentName(name); err != nil {
		return []interface{}{"name_len", len(name)}
	}
	return []interface{}{"name", name}
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

	// INVARIANT 11'S CROSS-CHECK, BEFORE A CHALLENGE IS MINTED
	// (MTLS-CROSSCHECK). body.AgentID is UNTRUSTED and UNVALIDATED here — it is
	// whatever the client put in the body — and that is fine, because the check
	// is not "is this caller that agent" (BeginSession's challenge is what proves
	// that). It is "may a credential for the agent this request NAMES be issued
	// over THIS connection". Running it first means no token is minted for a
	// mismatched connection at all, rather than minted and then found unusable:
	// a challenge is server state with a lifetime, and an unauthenticated caller
	// must not be able to create some for an agent whose certificate it does not
	// hold.
	//
	// IT DOES ADD AN ENUMERATION ORACLE, AND IT IS ACCEPTED (security gate,
	// MTLS-CROSSCHECK). An earlier version of this comment asserted the opposite —
	// "IT ADDS NO ENUMERATION ORACLE" — on the reasoning that this route already
	// separates an existing agent from an unknown one, so a 403 disclosed nothing
	// NEW about existence. That reasoning is sound and the conclusion still does
	// not follow, because existence is not the only thing now readable. Measured,
	// for an anonymous caller presenting NO certificate:
	//
	//	unknown agent          -> 404
	//	known, NOT cert-bound  -> 200, with a live challenge token
	//	known, cert-bound      -> 403
	//
	// So a 403 means precisely "this agent holds a live certificate binding", and
	// sweeping guessable ids (<bus-id>.<name>-<n>, the bus id being public from
	// /v1/info) maps which agents are NOT yet bound — i.e. which are still
	// vulnerable to the token replay this task exists to stop.
	//
	// It is accepted rather than closed, deliberately. Collapsing the 403 into a
	// 404 would take from an honest client the only signal that tells it to
	// re-enrol with its current keypair rather than retry forever, and moving the
	// gate AFTER BeginSession to equalise the shapes would reintroduce exactly the
	// mint-then-refuse defect MTLS-BIND's security gate found on the enrolment
	// path. What is disclosed is bounded: that an agent is not yet bound, never
	// what any certificate is and never whose. It shrinks to nothing as agents
	// re-enrol under MTLS-CLIENTCERT.
	//
	// The single fixed refusal string still does real work — it hides WHICH guard
	// fired — and crosscheck.go's crossCheckRefusal states the same trade. The two
	// comments must not drift apart again; that drift is what produced the false
	// claim above. The agent id never reaches the log raw; see agentIDLogFields.
	if !s.enforceCertBinding(w, r, body.AgentID) {
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

	// INVARIANT 11'S CROSS-CHECK, ON THE SERVER-SIDE AGENT ID (MTLS-CROSSCHECK).
	//
	// sess.AgentID comes from the COMPLETED SESSION — it is the id the server
	// recorded when the challenge was issued, never a value from this body. There
	// is no agent id in SessionCompleteRequestBody at all, and none may be added
	// for this check: a client-supplied id here would let a caller choose which
	// binding it is measured against, which is the whole attack.
	//
	// WHY IT RUNS AFTER CompleteSession AND NOT BEFORE. The agent id is simply not
	// knowable before: the request carries a token and a signature, and only
	// resolving the token yields the agent. Running after means the session is
	// left ACTIVATED even when this refuses, and that is acceptable rather than
	// merely unavoidable:
	//
	//   (a) completing a challenge requires a valid Ed25519 signature over the
	//       SERVER-CHOSEN token under the agent's own auth private key
	//       (invariant 3), so the caller reaching this line already IS the agent —
	//       it is not an attacker who has activated somebody else's session; and
	//   (b) every subsequent request bearing that token is gated by the SAME
	//       check in authMiddleware, over the same connection or any other, so an
	//       activated-but-refused token authorises nothing anywhere.
	//
	// So the residue is one live-but-useless session handle, which expires on its
	// own. DO NOT INVENT A REVOKE PATH HERE to tidy it up: none exists in
	// internal/auth today, and adding one under a refusal branch is how a
	// half-built revocation surface gets its first caller. AUTH-4 owns it.
	if !s.enforceCertBinding(w, r, sess.AgentID) {
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
		// not a retry. Reject it and LOG it. Note the contrast with a legitimate
		// retry (same key, same payload), which never reaches here: it returns
		// the original 201 and is not punished in any way.
		//
		// # THE CONNECTION IS KEPT (narrowed 2026-08-07)
		//
		// This path carried "Connection: close" until 2026-08-07 and no longer
		// does, for the reason set out in full at httpapi.disconnect: reusing
		// one's own key with different content is a client BUG, and dropping the
		// socket punishes every other request on it rather than the offending
		// one.
		//
		// The argument is if anything STRONGER on this route than on /v1/send,
		// because /v1/enroll is UNAUTHENTICATED. There is no principal here to
		// hold responsible, so the connection is not a proxy for an identity —
		// the party disconnected is simply whoever owns that socket, which on a
		// shared address is not necessarily the party that sent the request. And
		// the honest client it hits is one part-way through obtaining a
		// credential, with no session yet to fall back on.
		// The log line deliberately does NOT borrow /v1/send's "the key is the
		// caller's own" wording: enrolment keys are a GLOBAL namespace
		// (idem.NewEnrolScope, no agent component), precisely because there is no
		// authenticated agent yet, so two different clients CAN collide here. The
		// reason the connection is kept is the one in the block comment above —
		// there is no principal to hold responsible — not key ownership.
		s.log.Warn("enrolment idempotency key reused with different key material; rejected, and the connection is KEPT because this route is unauthenticated and the socket identifies no principal to punish", kv...)
		s.writeJSON(w, r, http.StatusConflict, ErrorResponse{Error: "idempotency key already used with a different payload"})

	case errors.Is(err, auth.ErrCertFingerprintBound):
		// 409, and the CONNECTION IS KEPT (invariant 10's two questions: a
		// merely buggy client reaches this line by retrying an enrolment from a
		// machine whose certificate is already enrolled, and this route is
		// unauthenticated so the socket identifies no principal to punish).
		//
		// Warn, not Debug: one client certificate presented for two agent ids is
		// either a client re-enrolling without regenerating its keypair — the
		// benign case, and the one an operator can fix — or someone trying to
		// attach their certificate to a second identity. An operator should see
		// both by default.
		//
		// The reply does NOT name the agent that already holds the binding. That
		// would turn enrolment into an oracle mapping a certificate an anonymous
		// caller possesses to an agent id on this bus; the server LOG names it,
		// which is where an operator can act on it.
		s.log.Warn("enrolment refused: the client certificate on this connection is already bound to another agent, and one certificate must never name two agents (invariant 11)", kv...)
		s.writeJSON(w, r, http.StatusConflict, ErrorResponse{Error: "this client certificate is already bound to an agent; enrol with a fresh client keypair"})

	case errors.Is(err, auth.ErrAuthKeyBound):
		// 409, and the CONNECTION IS KEPT — the auth-key mirror of
		// ErrCertFingerprintBound above (AUTH-DUP-ENROL-KEY). Invariant 10's two
		// questions answer the same way: a merely buggy client reaches this line by
		// re-enrolling with a keypair it already enrolled, and /v1/enroll is
		// unauthenticated so the socket identifies no principal to punish.
		//
		// Warn, not Debug: one enrolment public key presented for two agent ids is
		// either a client re-enrolling without regenerating its keypair — benign,
		// and operator-fixable — or someone attaching one keypair to a second
		// identity to impersonate. An operator should see both by default.
		//
		// The reply does NOT name the agent that already holds the key: enrolment
		// must not become an oracle mapping a (public) key to an agent id on this
		// bus. The server LOG names it, which is where an operator can act.
		s.log.Warn("enrolment refused: this enrolment public key is already bound to another agent, and one keypair must never name two agents (AUTH-DUP-ENROL-KEY, invariant 1)", kv...)
		s.writeJSON(w, r, http.StatusConflict, ErrorResponse{Error: "this enrolment public key is already bound to an agent; enrol with a fresh keypair"})

	case errors.Is(err, auth.ErrInviteRequired):
		// 403, and the CONNECTION IS KEPT (invariant 10's two questions are
		// answered in full on auth.ErrInviteRequired; the short form is that
		// every pre-gate client reaches this on its first call after the gate is
		// turned on, and this route is unauthenticated so the socket names no
		// principal to punish).
		//
		// Info, not Warn and not Debug. This is the ORDINARY refusal on an
		// invite-only bus — an agent without an invite is the expected caller,
		// not an anomaly — so Warn would train an operator to ignore it. But it
		// is also the line that tells an operator "someone tried to join and
		// could not", which is exactly what they need when an agent they meant to
		// admit cannot get on, so it must not be invisible at the default level.
		//
		// The reply NAMES THE REMEDY rather than just refusing. A bare "forbidden"
		// on an unauthenticated route leaves an agent with no idea whether it is
		// blocked, misconfigured or unlucky; this bus's whole client story is an
		// agent reading a message and acting on it (invariant 7), so the message
		// says what to obtain and which flag carries it. It discloses nothing: the
		// gate's state is already public in the discovery document's
		// `enrolment.invite_required` (GET /v1/discovery).
		s.log.Info("enrolment refused: this bus is invite-only and the enrolment presented no invite (invariant 3)", kv...)
		s.writeJSON(w, r, http.StatusForbidden, ErrorResponse{Error: "this bus is invite-only; obtain an invite from the bus operator and present it with `agent-busctl enrol --invite-file <path>`"})

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
