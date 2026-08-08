package httpapi

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
)

// The messaging surface. Every one of these routes AUTHENTICATES: none is on
// authMiddleware's allow-list, and none needs to be — an agent reaches them
// only after enrolling and completing a session challenge (invariant 3). They
// are protected by being registered, not by anyone remembering to protect them.
const (
	// RouteAgents lists the agents enrolled on this bus.
	RouteAgents = "/v1/agents"

	// RouteBroadcast sends a message to the whole bus. It currently answers 501;
	// see handleBroadcast for why refusing is the only answer that does not
	// admit unsigned traffic.
	RouteBroadcast = "/v1/broadcast"

	// RouteMint reserves the message id and sequence a sender must SIGN before
	// it may send (SIGN-1 option (a), SIGN-2).
	//
	// It exists because a signature has to cover a server-minted id — invariant
	// 1 makes the server authoritative on every id, so the client cannot choose
	// one to sign, and a signature that did not cover the id would leave the id
	// free to be swapped in flight. That makes a send a TWO-STEP: reserve here,
	// sign, then present the reservation back on /v1/send.
	RouteMint = "/v1/mint"

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

// disconnect marks the connection for closure after the response is written:
// net/http honours a handler-set "Connection: close" by closing the socket once
// the reply is flushed.
//
// # WHO this is for, and who it is NOT for (narrowed 2026-08-07)
//
// Invariant 10 pairs a rejection with a disconnect, and the whole value of the
// disconnect depends on aiming it at the right party. It is for the caller that
// presented ANOTHER AGENT'S signed message — the replay clause. That caller is
// not a confused client: it is holding material it was never issued, and there
// is no benign way to arrive at it.
//
// It is deliberately NOT used for a caller's conflict with ITSELF. Reusing your
// own idempotency key with a different payload is a protocol violation and is
// refused, but it is overwhelmingly a BUG IN AN HONEST CLIENT, and dropping the
// socket destroys every unrelated in-flight request that client had pipelined on
// it — including the long poll it was parked on. That is an abuse defence
// landing on the party most likely to be honest, so those paths reject and log
// and keep the connection. See writeHubError's ErrIdempotencyKeyReused case and
// writeAuthError's, which both carried this header until 2026-08-07.
//
// Before adding a call site, answer TWO questions:
//
//  1. Can a merely BUGGY client reach this line? If it can, it is the wrong
//     line — see the sender-mismatch check in checkSignedMint, whose first
//     draft fired on an omitted or unqualified `sender` field and had to be
//     gated on the claim actually parsing as an agent id.
//  2. Does this connection carry only ONE principal's traffic? Today it does.
//     When the relay ingest path lands it will NOT: a peer bus legitimately
//     presents a `sender` that is not the connection's principal, on a
//     connection multiplexing many agents, so dropping it would punish every
//     agent behind that peer. internal/relay/doc.go already specifies
//     "OFFENDING PEER DISCONNECTED" and will need reconciling with this
//     narrowing rather than inheriting it.
//
// The 409 from hub.ErrUnknownMint is the case that proves question 1 — it is
// reached both by a caller presenting a stranger's reservation and by a caller
// re-presenting its own spent one. For an OUTSTANDING reservation nothing
// available here tells the two apart, so it does not disconnect either.
func disconnect(w http.ResponseWriter) {
	w.Header().Set("Connection", "close")
}

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
//
// The route currently answers 501 and never decodes this (see handleBroadcast),
// so nothing in this package reads it today. It is KEPT rather than deleted for
// the same reason hub.Broadcast is: SIGN-3 re-opens the route by settling what a
// broadcast's canonical audience is, and it should find the shape of the request
// already agreed rather than have to re-invent it.
type BroadcastRequestBody struct {
	// Body is the message payload, standard base64. It is BYTES, not a string:
	// the bus never interprets a payload, and once the CRYPTO epic lands this
	// field carries ciphertext.
	Body string `json:"body"`

	// IdempotencyKey makes the send safe to retry (invariant 10).
	IdempotencyKey string `json:"idempotency_key"`
}

// MintRequestBody is the body of POST /v1/mint.
//
// THERE IS NO SENDER FIELD, and there never may be one. The reservation is
// minted for the AUTHENTICATED principal taken from the request context; a
// sender a client could name here would be a sender it could choose, and a
// sequence minted under a name of the caller's choosing is a signed message id
// attributable to somebody else (invariant 1).
type MintRequestBody struct {
	// Op is the operation the reservation may be spent on: "send" or
	// "broadcast". It is part of the mint's scope rather than decoration —
	// minting under one op and spending under the other must not be the same
	// reservation, or one route's idempotency key would shadow the other's. An
	// unrecognised value is 400, never a silent default (hub.parseMintOp).
	Op string `json:"op"`

	// IdempotencyKey is the key the SUBSEQUENT send will carry. Re-minting under
	// it returns the SAME assignment, allocates nothing and burns no further
	// sequence (invariant 10).
	IdempotencyKey string `json:"idempotency_key"`
}

// MintResponseBody is the 201 body of POST /v1/mint.
//
// It is replayed BYTE-IDENTICALLY on a repeat of the same (agent, op, key),
// ExpiresAt included — the expiry is the ORIGINAL one, not a fresh one computed
// on the retry, so a client cannot extend a reservation by re-minting. The fact
// that a response was a replay travels out of band in IdempotencyReplayedHeader,
// exactly as it does on /v1/enroll and /v1/send.
type MintResponseBody struct {
	// MessageID and Seq are the assignment to sign. The client does not choose
	// them and may not alter them: a send presenting anything else is refused
	// (hub.ErrMintMismatch -> 409).
	MessageID string `json:"message_id"`
	Seq       uint64 `json:"seq"`

	// Sender is the AUTHENTICATED principal echoed back, so a client that sees
	// an unexpected value there has learned something worth knowing.
	Sender string `json:"sender"`

	// Op is the operation this reservation may be spent on.
	Op string `json:"op"`

	// ExpiresAt is when the reservation stops being honoured (hub.MintTTL).
	// Past it — and across a RESTART, because the mint table is deliberately
	// in-memory while only the burned NUMBER is durable — a send is 409 and the
	// remedy is to re-mint under the same idempotency key and re-sign.
	ExpiresAt string `json:"expires_at"`
}

// SendRequestBody is the body of POST /v1/send.
type SendRequestBody struct {
	// To is the fully-qualified "<bus-id>.<agent-id>" of the recipient
	// (invariant 2).
	To string `json:"to"`

	Body           string `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`

	// Sender EXISTS ONLY SO SIGN-6 CHECK (d) CAN BE MADE. IT IS INPUT TO
	// VALIDATE, NEVER AN IDENTITY.
	//
	// It is here because the canonical signed bytes contain the sender
	// (signing.Canonicalize), so the client has already committed to a name and
	// the bus is entitled to insist that name is the one it authenticated. What
	// it must NEVER become is the source of "who sent this": every downstream
	// use — the hub, the durable record, the audit trail, the fan-out — takes
	// the principal from the REQUEST CONTEXT, where authMiddleware put it after
	// internal/auth resolved a live session (invariant 1).
	//
	// The failure mode this guards against is subtle and worth naming, because
	// the obvious "simplification" reintroduces it: if a future edit ever passes
	// this field to hub.SendRequest.Sender "since we checked it anyway", then
	// the day the check is loosened — or reordered after an early return, or
	// skipped on a sibling route — a client names its own sender and the bus
	// signs off on it. Keep the two separate: this field is compared and then
	// DISCARDED.
	Sender string `json:"sender"`

	// MessageID and Seq are the reservation minted by POST /v1/mint and covered
	// by Signature. They are checked for SHAPE here and then checked against the
	// reservation itself in the hub, which WINS on any disagreement.
	MessageID string `json:"message_id"`
	Seq       uint64 `json:"seq"`

	// TimestampMs is the SENDER's clock, Unix milliseconds UTC, and is covered
	// by the signature. It is NOT this bus's clock and orders nothing: the bus
	// stamps its own SentAt and the sequence is what orders messages. It is
	// carried, and required, because a recipient cannot reconstruct the signed
	// bytes without it.
	TimestampMs int64 `json:"timestamp_ms"`

	// Signature is the sender's detached Ed25519 signature over
	// signing.Canonicalize of the message, standard base64 of exactly 64 bytes.
	//
	// The bus checks its SHAPE and NEVER its authenticity — it does not hold the
	// sender's messaging key and must not be trusted to police messages for
	// senders it does not control. Bus enforces shape; recipient enforces
	// authenticity.
	Signature string `json:"signature"`
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

	// SentAt is THIS BUS's clock, RFC3339Nano UTC, and is NOT COVERED BY THE
	// SIGNATURE. It records when the bus accepted the message and is what the
	// audit trail and the retention window are measured from. Do not verify
	// against it and do not conflate it with TimestampMs below — they are two
	// different facts, told by two different parties, and only one of them is
	// something the sender committed to.
	SentAt string `json:"sent_at"`

	Size int `json:"size"`

	// ContentSHA256 lets a recipient verify the body it received is the body
	// the audit trail records, without the audit trail holding the body
	// (invariant 6).
	ContentSHA256 string `json:"content_sha256"`

	// TimestampMs is the SENDER's clock, Unix milliseconds UTC, and IS COVERED
	// BY THE SIGNATURE. A recipient MUST use this value, not SentAt, when it
	// reconstructs the signed bytes: substituting the bus's clock produces
	// different canonical bytes and every signature fails to verify.
	TimestampMs int64 `json:"timestamp_ms"`

	// Signature is the sender's detached Ed25519 signature, standard base64 of
	// 64 bytes, carried through untouched.
	//
	// The recipient reconstructs the signed bytes from MessageID, Seq, From, To,
	// TimestampMs and Body. BusPath is deliberately NOT covered (settled in
	// SIGN-1): it changes as a message is relayed, so signing it would make
	// every relayed message unverifiable.
	//
	// This bus never verified it and cannot: the verification key must be
	// resolved from the roster using the SENDER INSIDE the signed bytes, and
	// trust in a foreign sender's key is not this bus's to grant.
	Signature string `json:"signature"`

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

// broadcastUnsignableReason is the 501 body of POST /v1/broadcast, and the whole
// of the explanation a client gets. It is a constant so the route and any test
// pinning it cannot drift.
const broadcastUnsignableReason = "a broadcast cannot be signed under signing format v1: the canonical format requires a non-empty recipient set and the canonical audience of a broadcast is SIGN-3's undecided question; SIGN-6 admits no unsigned message type, so this route is refused rather than accepting unsigned traffic"

// handleMint serves POST /v1/mint (SIGN-2): it reserves the message id and
// sequence the caller must sign, DURABLY, and hands them back.
//
// The reservation is minted for the AUTHENTICATED principal and for nobody
// else. That is the whole reason this route exists as a separate step rather
// than the client picking an id: invariant 1 makes the server authoritative on
// every id, and SIGN-1 chose to hand that id out EARLY so the sender can sign
// it — which is only safe because the number is durably burned before it leaves
// this process (see hub/mint.go, and note that the mint TABLE is deliberately
// not durable while the burned NUMBER is).
func (s *Server) handleMint(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	sender, ok := s.messagingPrincipal(w, r)
	if !ok {
		return
	}
	var body MintRequestBody
	if !s.decodeJSONRequest(w, r, &body, MaxMessageRequestBytes) {
		return
	}

	m, err := s.hub.Mint(hub.MintRequest{
		// The AUTHENTICATED principal, from the context. MintRequestBody has no
		// sender field and never may have one (invariant 1).
		Sender:         sender,
		Op:             body.Op,
		IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		s.writeHubError(w, r, "mint", err)
		return
	}

	if m.Replayed {
		// A re-mint under a key this agent already holds. NOTHING was allocated
		// and nothing was burned; the body below is byte-identical to the
		// original, expiry included, so a client cannot extend a reservation by
		// asking again. The header is how the fact travels without changing the
		// body — the same arrangement as /v1/send and /v1/enroll.
		w.Header().Set(IdempotencyReplayedHeader, "true")
		s.log.Info("mint replayed from the outstanding-reservation table; no sequence was allocated",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", m.Sender,
			"message_id", m.MessageID,
		)
	} else {
		s.log.Info("message id and sequence reserved",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", m.Sender,
			"message_id", m.MessageID,
			"seq", m.Seq,
			"op", m.Op,
		)
	}

	s.writeJSON(w, r, http.StatusCreated, MintResponseBody{
		MessageID: m.MessageID,
		Seq:       m.Seq,
		Sender:    m.Sender,
		Op:        m.Op,
		ExpiresAt: formatInstant(m.ExpiresAt),
	})
}

// handleBroadcast serves POST /v1/broadcast (MSG-2) — and REFUSES it, 501.
//
// # Why a working code path answers "not implemented"
//
// SIGN-6 admits no unsigned message type. A broadcast cannot be signed under
// signing format v1, for two reasons that are both deliberate and neither of
// which this route may paper over:
//
//   - signing.Canonicalize REJECTS an empty recipient set, because an empty set
//     would sign an audience of nobody — a signature that commits the sender to
//     nothing about who was meant to read the message.
//   - store.Message stores a broadcast as a FLAG, not as an expanded roster
//     snapshot, so there is no recorded audience to canonicalize even if the
//     format allowed one. What the canonical audience of a broadcast should be
//     is SIGN-3's open question, and answering it here by accident would settle
//     it for everybody.
//
// So the two available answers were: leave this route accepting UNSIGNED
// messages — which is precisely the "strip the signature and the epic is
// theatre" hole SIGN-6 exists to close, and it would be the easiest hole in the
// bus to find — or FAIL CLOSED. We fail closed.
//
// The refusal is made HERE, immediately after authentication and BEFORE the
// body is decoded, so no broadcast payload is read, parsed, logged or measured
// on a route that cannot accept one.
//
// DO NOT "fix" this by deleting hub.Broadcast or the broadcast plumbing beneath
// it. That path is whole, signed-by-construction and covered by tests precisely
// so SIGN-3 can re-open this route by settling ONE question rather than by
// re-plumbing the write path. Only the route refuses.
func (s *Server) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}
	if _, ok := s.messagingPrincipal(w, r); !ok {
		// Authentication still runs first. A route that told an ANONYMOUS caller
		// what it does and does not implement would be describing the messaging
		// surface to somebody with no business knowing it exists.
		return
	}
	s.log.Info("broadcast refused: a broadcast has no canonical audience under signing format v1, and SIGN-6 admits no unsigned message type",
		"request_id", RequestIDFromContext(r.Context()),
		"agent_id", AgentIDFromContext(r.Context()),
	)
	s.writeJSON(w, r, http.StatusNotImplemented, ErrorResponse{Error: broadcastUnsignableReason})
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
	signed, ok := s.checkSignedMint(w, r, sender, body)
	if !ok {
		return
	}

	res, err := s.hub.Send(hub.SendRequest{
		// STILL the authenticated principal from the context, NOT body.Sender.
		// body.Sender was compared against this value in checkSignedMint and is
		// then discarded; it is a claim that was validated, never the identity
		// this send is attributed to (invariant 1).
		Sender:         sender,
		To:             body.To,
		Body:           payload,
		IdempotencyKey: body.IdempotencyKey,
		SignedMint:     signed,
	})
	if err != nil {
		s.writeHubError(w, r, "send", err)
		return
	}
	s.writeSendResult(w, r, res, store.ContentHash(payload))
}

// checkSignedMint is SIGN-6's INGEST POLICY: every well-formedness check the bus
// makes on a signed send, all of them BEFORE hub.Send is called.
//
// # Why every check lives here, on this side of the hub call
//
// A failure at any of these must leave NO WAL RECORD, NO DELIVERY, NO ACK and NO
// SEQUENCE CONSUMED BY THIS REQUEST. That is only true if the check happens
// before anything durable is attempted, and hub.Send's very first durable act is
// the two-phase write. Pushing one of these into the hub "for tidiness" would
// move it after the idempotency admission and would make a malformed request
// cost a remembered key.
//
// (The reservation itself was burned earlier, at POST /v1/mint. That is a
// separate, deliberate, earlier act — a mint is spent whether or not a send ever
// follows, and mint.go documents the resulting gaps as correct.)
//
// # What is NOT checked, and must never be added here
//
// The signature is NOT VERIFIED. This bus does not hold the sender's messaging
// key, and a bus must not be trusted to police messages on behalf of senders it
// does not control — a compromised bus that could "verify" could equally forge.
// The division is: THE BUS ENFORCES SHAPE, THE RECIPIENT ENFORCES AUTHENTICITY.
// Adding a verify here would look like an improvement and would quietly move the
// trust boundary onto the bus.
//
// # A rejection is TERMINAL for this idempotency key
//
// Not transient. There is no Retry-After on any of these and there must not be:
// the same key re-presented with the same malformed request is rejected
// identically for ever, and re-presented with a REPAIRED one it is a different
// payload under a used key, which invariant 10 already handles as a protocol
// violation. Dressing a permanent refusal as transient is how a client ends up
// in a retry loop that can never succeed.
func (s *Server) checkSignedMint(w http.ResponseWriter, r *http.Request, sender string, body SendRequestBody) (hub.SignedMint, bool) {
	logKV := func(kv []interface{}) []interface{} {
		return append([]interface{}{
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", sender,
			"path", r.URL.Path,
		}, kv...)
	}
	reject := func(status int, clientMsg, logMsg string, kv ...interface{}) (hub.SignedMint, bool) {
		s.log.Debug(logMsg, logKV(kv)...)
		s.writeJSON(w, r, status, ErrorResponse{Error: clientMsg})
		return hub.SignedMint{}, false
	}

	// (b) The signature must be standard base64. Strict() so a signature has
	// EXACTLY ONE spelling: the permissive decoder accepts trailing bits no
	// encoder produces, which would let one signature be written several ways —
	// and anything with several spellings is something a dedup or audit trail
	// keyed on the string form sees as several things.
	//
	// An ABSENT signature decodes to zero bytes rather than failing here, which
	// is why (a) is asserted by ValidateSignature below and not by this call.
	sig, err := base64.StdEncoding.Strict().DecodeString(body.Signature)
	if err != nil {
		// The value is NOT echoed, here or in the log. It is attacker-choosable
		// input, and the fact that it did not decode is the whole of what either
		// reader needs.
		return reject(http.StatusBadRequest, "signature is not valid base64", "send rejected: the signature is not valid standard base64")
	}

	// (a) and (c): present, and EXACTLY 64 bytes.
	//
	// Delegated to signing.ValidateSignature rather than open-coded, because
	// /v1/send, /v1/broadcast and the relay ingest path (SIGN-7) must all apply
	// the SAME rule. Three copies of "is it 64 bytes" are three chances for one
	// to be written as >= or to be missed on the path a peer bus reaches, and
	// the relay path is exactly the backdoor SIGN-7 names. Both 63 and 65 bytes
	// are refused; there is no tolerance and no truncation.
	if err := signing.ValidateSignature(sig); err != nil {
		if errors.Is(err, signing.ErrNoSignature) {
			// Kept DISTINCT from the length failure on purpose: "no signature"
			// must never read as "unsigned but fine", and an attacker STRIPPING a
			// signature is a different event from one mangling it.
			return reject(http.StatusBadRequest, "a signature is required", "send rejected: no signature was supplied")
		}
		return reject(http.StatusBadRequest, "signature must be exactly 64 bytes", "send rejected: the signature is the wrong length", "err", err)
	}

	// (d) The sender named in the request — and therefore inside the signed
	// bytes — must be the agent that authenticated.
	//
	// 403, not 400: the request is well formed and re-sending it will not help.
	// The check exists because the canonical format binds a signature to a
	// sender, so a client signing as somebody else would produce a message whose
	// signed content contradicts the identity the bus recorded — the recipient
	// would resolve a verification key for the NAMED sender, and either fail
	// confusingly or, worse, succeed against a key the real sender never used.
	// The bus refuses to store that contradiction rather than pass it on.
	//
	// # AND THE CONNECTION IS CLOSED (2026-08-07)
	//
	// This is invariant 10's REPLAY clause, and it is the door a third party
	// actually comes through. A signature does not stop replay: an accepted,
	// signed message can be resent verbatim by anyone who has seen it, and the
	// bytes still verify. What identifies the replayer is that the `sender`
	// inside those bytes is not the agent on the session — exactly this check.
	//
	// The disconnect is aimed HERE, and only at paths like this one, because a
	// caller cannot reach it by being merely buggy about its OWN messages: the
	// sender name is inside the bytes it signed, so a single-identity client
	// signs its own name or it signs nothing. Reaching this line means the
	// caller is presenting a name it did not authenticate as.
	//
	// The residual innocent case, and it is worth naming rather than denying: a
	// process holding TWO enrolments — an agent embedding client/ under two
	// identities — could pair identity A's signed request with identity B's
	// session token and land here without malice. It costs that client one
	// connection and one clear 403 naming the remedy, which is a far smaller
	// price than leaving the replay path open, and it is a client bug that
	// SHOULD be loud.
	//
	// This check runs BEFORE hub.Send, so a disconnected caller has consumed no
	// idempotency key, written no WAL record and delivered nothing.
	//
	// # THE DISCONNECT IS GATED ON THE CLAIM BEING A REAL AGENT ID
	//
	// The status is 403 for every mismatch. The DISCONNECT is not: it fires only
	// when the claim PARSES as a well-formed, fully-qualified "<bus>.<agent>"
	// id (invariant 2). That gate exists because the first draft of this change
	// failed its own rule — "can a merely BUGGY client reach this line?" — on
	// three shapes a single-identity client reaches by accident and not by
	// malice:
	//
	//   - the `sender` field omitted entirely (empty string);
	//   - an UNQUALIFIED id, the bus prefix dropped ("alpha-1");
	//   - a trailing space or other stray whitespace.
	//
	// None of those names another agent; each is a client that failed to fill
	// the field correctly, and dropping its socket costs it every other request
	// on that connection and buys a reconnect storm as it retries. A REPLAYER,
	// by contrast, is carrying somebody's real id inside bytes that somebody
	// really signed, so it always parses.
	if body.Sender != sender {
		// The claimed value is not echoed to the client (it chose it) and IS
		// logged, because an operator hunting an impersonation attempt needs to
		// know which name was being worn.
		if _, _, _, err := ids.ParseAgentID(body.Sender); err != nil {
			// Malformed: a confused client, not a replayer. Debug, and the
			// connection is kept.
			return reject(http.StatusForbidden, "sender does not match the authenticated caller",
				"send rejected: the sender named in the request is malformed and is not the authenticated principal; the connection is KEPT because a claim that is not a well-formed agent id names nobody and is a client bug rather than impersonation",
				"claimed_sender", body.Sender, "err", err)
		}
		// WARN, not Debug: unlike the shape failures above, this is the signature
		// of an impersonation or replay attempt and an operator should see it
		// without turning debug logging on.
		disconnect(w)
		s.log.Warn("send refused and the client disconnected: the sender named in the request is a well-formed id for a DIFFERENT agent than the authenticated principal, which is how a replayed third-party message presents",
			logKV([]interface{}{"claimed_sender", body.Sender})...)
		s.writeJSON(w, r, http.StatusForbidden, ErrorResponse{Error: "sender does not match the authenticated caller"})
		return hub.SignedMint{}, false
	}

	// (e) The message id must be well formed, must be THIS bus's, and must agree
	// with seq.
	//
	// The hub checks the id against the reservation and wins on any
	// disagreement, so this is not the authority check — it is the SHAPE check
	// that stops a malformed or foreign id reaching the write path at all. The
	// bus-half check is the one worth naming: an id minted by ANOTHER bus that
	// happened to match a local reservation's sequence would be a message
	// attributed to this bus's total order while carrying a foreign origin, and
	// origin is what SIGN-1 made the signature cover.
	//
	// ParseMessageID also rejects sequence 0 and every alternate spelling of a
	// sequence ("007", "+7"), so one id has one spelling — two spellings of one
	// id defeat duplicate detection (invariant 10).
	parsedBus, parsedSeq, err := ids.ParseMessageID(body.MessageID)
	if err != nil {
		return reject(http.StatusBadRequest, "invalid message id", "send rejected: the message id is malformed", "err", err)
	}
	if parsedBus != s.identity.BusID() {
		return reject(http.StatusBadRequest, "invalid message id",
			"send rejected: the message id was minted by another bus",
			"message_id_bus", parsedBus, "this_bus", s.identity.BusID())
	}
	if body.Seq == 0 || parsedSeq != body.Seq {
		// Both halves are carried on the wire because the canonical format
		// encodes the sequence separately as well as inside the id, so the two
		// must agree before anything is signed over them. A mismatch is either a
		// client splicing two operations together or an attempt to have one
		// number signed and a different one recorded.
		return reject(http.StatusBadRequest, "invalid message id",
			"send rejected: the seq field disagrees with the sequence inside the message id",
			"message_id_seq", parsedSeq, "seq_field", body.Seq)
	}

	// (f) The sender's clock is REQUIRED. 0 means "unset" and a negative value
	// is a pre-1970 instant, and both are refused rather than defaulted: the
	// timestamp is covered by the signature, so a bus that substituted its own
	// value would store a message whose recorded content no recipient can
	// reproduce, and every signature over it would fail to verify for a reason
	// no client could diagnose.
	if body.TimestampMs <= 0 {
		return reject(http.StatusBadRequest, "timestamp_ms is required",
			"send rejected: the sender's timestamp is missing or not a positive Unix-millisecond instant",
			"timestamp_ms", body.TimestampMs)
	}

	return hub.SignedMint{
		MessageID:          body.MessageID,
		Seq:                body.Seq,
		TimestampUnixMilli: body.TimestampMs,
		Signature:          sig,
	}, true
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
		TimestampMs:   m.TimestampUnixMilli,
		Signature:     encodeBase64(m.Signature),
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
		errors.Is(err, hub.ErrInvalidCursor),
		errors.Is(err, hub.ErrInvalidOp):
		s.log.Debug("message request rejected", kv...)
		s.writeJSON(w, r, http.StatusBadRequest, ErrorResponse{Error: terseHubError(err)})

	case errors.Is(err, hub.ErrUnknownMint), errors.Is(err, hub.ErrMintMismatch):
		// 409 Conflict: the request is well formed, but the reservation it names
		// is not one this bus is holding for this key. The two ways in are very
		// different and the client's remedy is the same, which is why they share
		// a status:
		//
		//   - ErrUnknownMint is ROUTINE — the bus restarted (the mint table is
		//     in-memory; only the burned NUMBER is durable) or the reservation
		//     expired.
		//   - ErrMintMismatch is NOT routine — the client presented an
		//     assignment this bus did not give it, which is either a client bug
		//     splicing two operations together or an attempt to get a signature
		//     over a self-chosen id accepted (invariant 1).
		//
		// Deliberately NO Retry-After: replaying the SAME request cannot ever
		// succeed, so a retry loop on it is a client spinning for nothing. The
		// remedy is a new two-step, and it is stated in the response because the
		// client cannot deduce it from the status alone. It is safe: a re-mint
		// under the same key yields a fresh number, and if the original message
		// did become durable the applied-key table answers the re-send with the
		// ORIGINAL result before the mint is ever consulted (invariant 10).
		//
		// The mismatch case is logged at WARN, not Debug: a client presenting an
		// id it was not given is worth an operator's attention even though the
		// bus is unharmed. The error text — which names the id this bus DID
		// mint — stays in the log and out of the response, per terseHubError's
		// rule.
		if errors.Is(err, hub.ErrMintMismatch) {
			s.log.Warn("send refused: the message id or sequence presented is not the one this bus minted for that idempotency key", kv...)
		} else {
			s.log.Debug("send refused: no outstanding sequence reservation for this idempotency key", kv...)
		}
		s.writeJSON(w, r, http.StatusConflict, ErrorResponse{
			Error: "no matching sequence reservation: mint a fresh message id with POST " + RouteMint + ", re-sign it and re-send",
		})

	case errors.Is(err, hub.ErrIdempotencyKeyReused):
		// Invariant 10: same key + DIFFERENT payload is a protocol violation,
		// not a retry. Reject it and LOG it. A legitimate retry (same key, same
		// payload) never reaches here: it returns the original 201 and is not
		// punished.
		//
		// # THE CONNECTION IS KEPT (narrowed 2026-08-07)
		//
		// This path carried "Connection: close" until 2026-08-07 and no longer
		// does. The reasoning that removed it:
		//
		// The caller is AUTHENTICATED, the key is ITS OWN — keys are scoped per
		// agent (idem.Scope), so one agent cannot collide with another's — and
		// no other agent's material is involved. What that describes is a
		// client that lost track of its own keys, which is a BUG, not an
		// attack; a real attacker gains nothing by conflicting with itself.
		//
		// And the punishment lands on everything BUT the offending request:
		// dropping the socket kills every other request that client had
		// pipelined on it, including a long poll it was parked on, so one
		// mis-keyed retry costs it messages it was legitimately waiting for.
		// That is an abuse defence aimed at the party most likely to be honest.
		//
		// The disconnect now lives where a third party is demonstrably involved
		// — checkSignedMint's sender-mismatch 403, invariant 10's replay clause.
		// See disconnect() for the test to apply before adding another site.
		s.log.Warn("idempotency key reused with a different payload; rejected, and the connection is KEPT because the key is the caller's own", kv...)
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
	case errors.Is(err, hub.ErrInvalidOp):
		// The set of legal answers is the whole of what the caller needs, and
		// the hub deliberately does not echo the value it was given.
		return "op must be \"send\" or \"broadcast\""
	default:
		return "bad request"
	}
}
