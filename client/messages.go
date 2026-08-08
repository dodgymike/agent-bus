package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Route paths for the messaging and polling surface. Pinned here as literals
// for the same reason routeEnroll is (see enrol.go and doc.go): this package
// must not import internal/, so it mirrors internal/httpapi's RouteAgents,
// RouteSend, RouteBroadcast, RouteMessages and RouteWait rather than importing
// them. A divergence fails loudly and immediately — a 404 on the first call —
// which is the right direction for a duplicated constant to break in.
const (
	routeAgents    = "/v1/agents"
	routeMint      = "/v1/mint"
	routeSend      = "/v1/send"
	routeBroadcast = "/v1/broadcast"
	routeMessages  = "/v1/messages"
	routeWait      = "/v1/wait"
)

// The operation names /v1/mint scopes a reservation by. They mirror
// internal/idem's OpSend and OpBroadcast, which are the same strings the bus
// scopes its applied-key table with — one agent must not be able to collide
// with itself across two routes by reusing a key.
const (
	mintOpSend      = "send"
	mintOpBroadcast = "broadcast"
)

// Protocol limits, PINNED here as literals mirroring the server's definitions.
//
// They are duplicated, not imported, for the invariant-7 reason above. They are
// duplicated at all — rather than simply letting the bus refuse — because every
// one of them turns a round trip and a terse 400 into a local, remedial error
// that names the actual number the caller exceeded. The server remains
// authoritative: these checks only ever refuse EARLIER than it does, never
// admit something it would reject.
const (
	// MaxBodyBytes is the largest message body the bus accepts, DECODED. It
	// mirrors store.MaxBodyBytes. The wire carries standard base64, so the
	// encoded form is about a third larger again — this bound is on the bytes
	// the caller hands us, which is the number a caller can act on.
	MaxBodyBytes = 64 << 10

	// MaxBatchLimit is the ceiling on a requested batch size, mirroring
	// hub.MaxBatchLimit. Above it the bus answers 400.
	MaxBatchLimit = 256

	// DefaultBatchLimit is the batch size the bus applies when no limit is
	// requested, mirroring hub.DefaultBatchLimit.
	//
	// This client deliberately sends NO limit when the caller asks for none,
	// rather than sending this value: the default belongs to the bus, and a
	// client that pins it locally would freeze an old default the day the bus
	// changed its own. It is exported so a caller can size a buffer or explain
	// its throughput without hard-coding 64 itself.
	DefaultBatchLimit = 64

	// MaxPollTimeout is the hard ceiling on one long poll, mirroring
	// hub.MaxPollTimeout. A request above it is REFUSED with a 400, not clamped
	// — and this client refuses it locally for the same reason (see Read).
	MaxPollTimeout = 5 * time.Minute

	// DefaultPollTimeout is how long a poll parks when the caller names no
	// timeout, mirroring hub.DefaultPollTimeout.
	DefaultPollTimeout = 30 * time.Second
)

// maxCursorLen mirrors hub.MaxCursorLen: the bus refuses to decode a cursor
// longer than this, so sending one is a guaranteed 400, and STORING one is a
// hostile bus growing a local file (see cursorstore.go).
const maxCursorLen = 512

// maxBatchResponseBytes is the response-body ceiling for the two read routes.
//
// The generic 1 MiB bound (transport.go's maxResponseBytes) is not enough here
// and the failure would be baffling: store.MaxBatchBytes lets ONE batch carry a
// full 1 MiB of message BODY, which the wire then base64-encodes to ~1.4 MiB
// before the per-message metadata is added. Truncating that at 1 MiB does not
// produce a short read, it produces invalid JSON — reported as "the bus
// returned a 200 whose body is not the expected JSON", which points a reader at
// the wrong problem entirely. 4 MiB is ~3x the largest legal batch and is still
// finite, which is the property that matters.
const maxBatchResponseBytes = 4 << 20

// minPollSlack is the smallest margin added to a long poll's own timeout when
// deriving the request deadline. See Read.
const minPollSlack = 10 * time.Second

// Message is one message read back off the bus.
//
// The json tags are the WIRE shape (CONTRACTS-HTTP.md, "A <message> on the read
// path is") and are also this type's --json contract, so they do not change.
type Message struct {
	// MessageID is the server-minted "<bus-id>-<seq>" (invariant 1). It is the
	// key an idempotent handler should deduplicate on: delivery is
	// at-least-once, so a message may legitimately arrive twice.
	MessageID string `json:"message_id"`

	// Seq is the server-minted monotonic sequence. Together with the recipient
	// cursor it is the freshness half of the replay defence (invariant 10) — a
	// signature alone cannot stop a verbatim resend.
	Seq uint64 `json:"seq"`

	// From is the fully-qualified sender id (invariant 2).
	From string `json:"from"`

	// Broadcast reports that this went to every agent except the sender.
	Broadcast bool `json:"broadcast"`

	// To lists the recipients: exactly one for a direct message, empty for a
	// broadcast.
	To []string `json:"to"`

	// BusPath is the bus ids this message has traversed, oldest first. It is
	// what prevents a relay loop in a cyclic peer topology.
	BusPath []string `json:"bus_path"`

	// SentAt is the BUS's timestamp, verbatim. It is NOT covered by the
	// signature and must not be confused with TimestampMS below: a bus can write
	// whatever it likes here, and a relay writes its own.
	SentAt string `json:"sent_at"`

	// TimestampMS is the SENDER's clock in Unix milliseconds UTC, and it IS
	// covered by the signature. It and SentAt are different facts about
	// different clocks; conflating them would attribute a bus-chosen value to
	// the sender.
	//
	// It is not freshness: clocks lie, and a sender can put anything here and
	// sign it. Replay protection is the server-minted monotonic Seq plus this
	// agent's cursor (invariant 10).
	TimestampMS int64 `json:"timestamp_ms"`

	// Signature is the sender's detached Ed25519 signature, standard base64 of
	// exactly 64 bytes, over the canonical encoding of this message (see
	// canonical.go).
	//
	// It is carried on the type because it is a wire field, NOT because a caller
	// should verify it: Read has already done that, and a message that reaches a
	// caller in Batch.Messages is one that verified. A caller re-verifying it
	// must resolve the key from From through a trust store and nothing else — the
	// signature is not self-describing on purpose.
	Signature string `json:"signature"`

	// Size is the body length in bytes, as the bus recorded it.
	//
	// It is VERIFIED against len(Body) on the read path, so a Message a caller
	// holds is one whose two answers agreed. See verifyMessageBody.
	Size int `json:"size"`

	// ContentSHA256 is the hex SHA-256 of the DECODED body.
	//
	// It is VERIFIED against sha256(Body) on the read path: a message whose hash
	// disagrees with its bytes fails the whole batch and never reaches a caller
	// (verifyMessageBody). So a Message in hand has an intact body, and a body
	// that looks short afterwards was shortened downstream of this package.
	//
	// It is NOT authenticity. The BUS computes this hash, so it detects
	// corruption and a bus whose own answer is inconsistent — not a forged
	// sender. Sender authenticity is Signature and nothing else.
	ContentSHA256 string `json:"content_sha256"`

	// Body is the DECODED message body.
	//
	// It is []byte rather than string on purpose: encoding/json round-trips a
	// []byte as a standard-base64 STRING, which is exactly the wire form the bus
	// uses, so the decoding is the standard library's rather than something this
	// package hand-rolls — and a caller gets bytes, not a string it has to
	// remember to decode. A body is arbitrary bytes; it is NOT run through
	// safeText, because mangling a payload would be worse than printing it, and
	// deciding how to render it belongs to whoever consumes it.
	Body []byte `json:"body"`
}

// SendOptions is the input to Send.
type SendOptions struct {
	// To is the fully-qualified recipient `<bus-id>.<agent-id>` (invariant 2).
	To string

	// Body is the message payload, DECODED. At most MaxBodyBytes.
	Body []byte

	// IdempotencyKey makes the send safe to retry (invariant 10). Leave it
	// empty and Send mints a fresh random one.
	//
	// Supply one only to RETRY a specific earlier send, with a BYTE-IDENTICAL
	// body. Same key + same payload is a legitimate retry and is answered from
	// the bus's applied-key table; same key + DIFFERENT payload is a protocol
	// violation that earns a 409 AND a disconnection.
	IdempotencyKey string
}

// BroadcastOptions is the input to Broadcast. See SendOptions for the fields'
// meaning; a broadcast has no recipient because it goes to every agent on the
// bus except the sender.
type BroadcastOptions struct {
	Body           []byte
	IdempotencyKey string
}

// SendResult is what the bus returns for an accepted send or broadcast.
//
// It is returned ONLY after the message is committed through the two-phase
// write path and fsynced (invariant 4): a SendResult in hand means the message
// is durable, not merely received.
//
// The json tags are a documented contract surface (CONTRACTS-CLI.md).
type SendResult struct {
	MessageID string   `json:"message_id"`
	Seq       uint64   `json:"seq"`
	From      string   `json:"from"`
	Broadcast bool     `json:"broadcast"`
	To        []string `json:"to"`
	SentAt    string   `json:"sent_at"`

	// ContentSHA256 is the hex SHA-256 of the decoded body, as the bus computed
	// it. A caller that wants end-to-end assurance can compare it with its own.
	//
	// UNLIKE Message.ContentSHA256, this one is NOT compared here — the caller
	// holds the body it sent and is the only party that can do so. (It is the
	// send path; there is no decoded body in the response to compare against.)
	ContentSHA256 string `json:"content_sha256"`

	// Replayed reports that the bus answered from its applied-key table rather
	// than writing a second message — i.e. this was a retry and the ORIGINAL
	// result came back. It is NOT an error: it is idempotency working, and the
	// whole point of invariant 10 is that a well-behaved client can retry.
	Replayed bool `json:"replayed"`

	// IdempotencyKey is the key this send was applied under, minted here when
	// the caller supplied none.
	//
	// It is REPORTED, not merely used, because it is the only handle that can
	// retry this exact logical send later. A caller that loses the answer to a
	// network failure and then invents a fresh key has not retried the send, it
	// has sent a second message.
	IdempotencyKey string `json:"idempotency_key"`
}

// AgentSummary is one entry of the bus's roster. It carries NO key material.
type AgentSummary struct {
	// AgentID is the fully-qualified `<bus-id>.<agent-id>`.
	AgentID string `json:"agent_id"`

	// BusID is DERIVED, not received: /v1/agents does not carry a bus id per
	// entry, and invariant 2 says the part before the first '.' of a
	// fully-qualified id IS the bus. It is split out here so a caller does not
	// have to re-derive it (and get the "first dot, not the last" detail wrong)
	// at every call site. An entry whose id carries no bus prefix is refused
	// rather than reported with an empty BusID — see Agents.
	BusID string `json:"bus_id"`

	Name       string `json:"name"`
	EnrolledAt string `json:"enrolled_at"`
}

// AgentList is the bus's roster.
type AgentList struct {
	Agents []AgentSummary `json:"agents"`

	// Count is len(Agents), RECOMPUTED locally rather than taken from the wire.
	// The bus sends a count too, but a value that can disagree with the slice
	// beside it is a value a consumer will eventually trust over the slice; a
	// hostile bus claiming 10000 agents while sending three should not be able
	// to say so through this type.
	Count int `json:"count"`
}

// ReadOptions is the input to Read.
type ReadOptions struct {
	// Cursor is the opaque position returned by a previous batch. Empty means
	// position 0 — "I have seen nothing" — which reads back through the whole
	// RETAINED window (1 day / 1 GiB, whichever binds first).
	Cursor string

	// Limit caps the batch size, 1..MaxBatchLimit. 0 lets the bus apply its own
	// default (DefaultBatchLimit).
	Limit int

	// Wait selects the form of the read:
	//
	//	0  → history: GET /v1/messages, which never parks and returns whatever
	//	     is available right now, possibly nothing.
	//	>0 → long poll: GET /v1/wait with that timeout, which parks until a
	//	     visible message arrives or the deadline passes.
	//
	// It is refused above MaxPollTimeout rather than clamped, mirroring the bus.
	Wait time.Duration
}

// RejectionReason names WHICH check a message failed on the way in. It is a
// CLOSED vocabulary: a caller branches on it, and the CLI prints it.
//
// The cases are kept distinct on purpose, because they are four different events
// with four different remedies and one of them is not an attack at all:
//
//	no_trusted_key             you have never been given this sender's key.
//	                           Today this is the ORDINARY state of the world —
//	                           see keyring.go's blocker note — and the remedy is
//	                           `agent-busctl trust`, not an investigation.
//	malformed_trusted_key      the key you hold is not a 32-byte Ed25519 key.
//	                           An OPERATOR fault: the trust store is damaged.
//	no_signature               the message carried none. This is the exact case
//	                           SIGN-6 exists to close: "unsigned" must never be
//	                           read as "unsigned but fine".
//	signature_not_base64,
//	signature_length           present but mangled — a different event from
//	                           stripped, and worth telling apart.
//	not_canonicalizable        the message cannot be re-serialised into signable
//	                           bytes at all (a broadcast, a mismatched id/seq, a
//	                           sender from another bus than minted the id).
//	signature_does_not_verify  the verdict. Nothing more can be said: a tampered
//	                           body, a re-labelled sender and a forged signature
//	                           are indistinguishable from inside.
//
// Folding them into one "verification failed" would bury the operator faults
// among the routine ones and make SIGN-5's per-case assertions impossible.
type RejectionReason string

const (
	RejectedNoTrustedKey     RejectionReason = "no_trusted_key"
	RejectedMalformedKey     RejectionReason = "malformed_trusted_key"
	RejectedNoSignature      RejectionReason = "no_signature"
	RejectedSignatureEncoded RejectionReason = "signature_not_base64"
	RejectedSignatureLength  RejectionReason = "signature_length"
	RejectedNotCanonical     RejectionReason = "not_canonicalizable"
	RejectedSignatureInvalid RejectionReason = "signature_does_not_verify"
)

// RejectedMessage is a message the bus delivered that this client REFUSED to
// hand to the calling agent because it could not be verified.
//
// It carries the metadata needed to investigate — which message, from whom, and
// which check failed — and DELIBERATELY NOT THE BODY. The body is discarded at
// the moment verification fails and never reaches the caller, because a body
// that reaches an agent has been acted on whatever the accompanying warning
// said. There is no flag to get it back.
//
// The json tags are a contract surface (CONTRACTS-CLI.md).
type RejectedMessage struct {
	// MessageID and Seq identify the message in the bus's audit log, so an
	// operator can correlate this rejection with what the bus actually stored.
	MessageID string `json:"message_id"`
	Seq       uint64 `json:"seq"`

	// From is the sender the ENVELOPE claimed. It is the id the verification key
	// was looked up under, and — precisely because the message did not verify —
	// it is UNPROVEN: treat it as a lead, never as an attribution.
	From string `json:"from"`

	// Reason is which check failed.
	Reason RejectionReason `json:"reason"`

	// Detail is a one-line human explanation. It never contains the body and
	// never contains key material.
	Detail string `json:"detail"`
}

// Batch is one page of messages plus the position to resume from.
type Batch struct {
	// Messages are the VERIFIED messages, unchanged. Every message here carried
	// a signature that verified against a key resolved from its own From field
	// through the local trust store.
	Messages []Message `json:"messages"`

	// Rejected are the messages that did NOT verify and whose bodies were
	// discarded. It is empty in the ordinary case.
	//
	// THE CURSOR STILL ADVANCED PAST THEM, and that is settled policy, not an
	// oversight: fail-closed applies to the BODY, not to the cursor. Blocking the
	// cursor on an unverifiable message would hand anyone who can inject a single
	// bad message a PERMANENT denial of service against this agent — it would
	// never read anything again. Discard the body, record the event, move on.
	Rejected []RejectedMessage `json:"rejected,omitempty"`

	// Cursor is the position to pass to the NEXT call. An empty batch returns
	// the cursor unchanged, byte for byte, which is what makes a timed-out long
	// poll resumable — a cursor is never advanced past messages the caller was
	// not handed.
	Cursor string `json:"cursor"`

	// More reports that the batch was cut short (by limit or by the 1 MiB batch
	// byte budget) and another call will return more immediately.
	More bool `json:"more"`

	// TimedOut reports that a long poll reached its deadline with nothing to
	// deliver. It is NOT an error and it is not an anomaly: on a quiet bus it is
	// the steady state, and the bus answers it with a 200.
	TimedOut bool `json:"timed_out"`
}

// sendRequestBody mirrors httpapi.SendRequestBody. The server rejects unknown
// fields, so this struct is exactly the wire shape and nothing more.
//
// Body is []byte and is marshalled by encoding/json as a standard-base64
// string, which is the wire form the bus parses.
type sendRequestBody struct {
	To             string `json:"to"`
	Body           []byte `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`

	// Sender is this agent's own fully-qualified id, echoed up so the bus can
	// make SIGN-6's third ingest check — that the CLAIMED sender equals the
	// AUTHENTICATED caller. It is not how the bus learns who we are (it takes
	// that from the session), and a bus that trusted this field instead would be
	// accepting a spoofed sender (invariant 1). It is here because the sender is
	// INSIDE the signed bytes, so the bus must be able to see the two agree.
	Sender string `json:"sender"`

	// MessageID and Seq are the reservation /v1/mint handed back, presented
	// again so the bus can match them against what it minted. The client does
	// not choose them and must not alter them: they are covered by Signature,
	// and a value that disagrees with the reservation is refused (409) rather
	// than accepted.
	MessageID string `json:"message_id"`
	Seq       uint64 `json:"seq"`

	// TimestampMS is OUR clock in Unix milliseconds UTC, and it is covered by
	// the signature. It is not the bus's clock and orders nothing — the bus
	// stamps its own sent_at, which is deliberately NOT signed, because a
	// recipient verifying a signature must not need to trust any bus's clock.
	TimestampMS int64 `json:"timestamp_ms"`

	// Signature is the detached Ed25519 signature over canonicalize() of this
	// message, standard base64 of exactly 64 bytes. It is made with the
	// MESSAGING private key, which never leaves this machine and is never sent
	// to the bus.
	Signature string `json:"signature"`
}

// mintRequestBody and mintResponseBody mirror httpapi's /v1/mint shapes.
//
// There is deliberately no sender field in the REQUEST: the bus takes the
// principal from the session and echoes it back, and a request that could name
// its own sender would be a request that could reserve a sequence in someone
// else's name.
type mintRequestBody struct {
	Op             string `json:"op"`
	IdempotencyKey string `json:"idempotency_key"`
}

type mintResponseBody struct {
	MessageID string `json:"message_id"`
	Seq       uint64 `json:"seq"`
	Sender    string `json:"sender"`
	Op        string `json:"op"`
	ExpiresAt string `json:"expires_at"`
}

// reservation is a minted, not-yet-spent message id and sequence.
type reservation struct {
	MessageID string
	Seq       uint64
	Sender    string
}

// reserve performs the FIRST half of the reserve-then-send handshake: it asks
// the bus to mint the message id and sequence that this send will carry.
//
// # Why a send costs two round trips
//
// SIGN-1 settled that the signature covers the ORIGIN bus's minted message id
// and sequence (option (a)). Those are server-authoritative (invariant 1) and
// the client cannot guess them, so it cannot sign until it has them — hence
// reserve, then sign, then send. The alternative considered and rejected was to
// sign only the fields the client controls and bind the server's numbers
// alongside; that leaves the id and sequence unsigned, and an unsigned sequence
// is exactly the field the replay defence rests on.
//
// # The SAME idempotency key covers both calls, and that is what makes it safe
//
// A reservation is scoped by (agent, operation, key), so re-issuing this call
// with the same key returns the SAME id and sequence rather than burning a
// second one. That is what makes the whole two-step retryable: a client that
// loses the response, or that crashes between the two calls, repeats both with
// the same key and converges on one message. Minting a fresh key on the retry
// would produce a second reservation and, if the first send had actually
// landed, a second message.
func (c *Client) reserve(ctx context.Context, op, mintOp, key string) (reservation, error) {
	var out mintResponseBody
	if _, err := c.authorizedRequest(ctx, request{
		method: http.MethodPost,
		path:   routeMint,
		op:     op,
		body:   mintRequestBody{Op: mintOp, IdempotencyKey: key},
		out:    &out,
		// Safe to repeat for the reason in the doc comment above: the key makes
		// a repeat return the original reservation rather than a new one.
		retryable: true,
	}); err != nil {
		return reservation{}, err
	}

	// The bus is authoritative on ids — authoritative, not unvalidated. These
	// strings are about to be signed by us and printed to a terminal, and a
	// hostile bus chooses every byte of them.
	if err := validateServerField(op, "message id", out.MessageID); err != nil {
		return reservation{}, err
	}
	if err := validateServerField(op, "sender id", out.Sender); err != nil {
		return reservation{}, err
	}
	if out.Seq == 0 {
		// Sequence 0 is never allocated, so it means "unset" rather than a real
		// assignment. Refusing here rather than signing it keeps the failure
		// local: canonicalize would reject it too, but a step later and with a
		// message about framing rather than about the bus.
		return reservation{}, newError(KindServer, op,
			"the bus returned sequence 0 for "+safeText(out.MessageID, 60)+", which is never allocated",
			"this is not a well-formed agent-bus response; check that --bus points at the bus you intended")
	}
	return reservation{MessageID: out.MessageID, Seq: out.Seq, Sender: out.Sender}, nil
}

// signOutgoing builds the canonical bytes for an outgoing message and signs
// them with this identity's MESSAGING private key.
//
// # The sender that gets signed is OURS, not the one the bus echoed
//
// res.Sender is checked against the local credential and then discarded. That
// direction is load-bearing: signing whatever the bus said we are would let a
// bus induce us to produce a validly-signed message attributing our words to
// another id — a signature is worth nothing if the signer will put any name in
// it. The bus's echo is useful only as a cross-check, and a mismatch is an
// error rather than something to reconcile.
func (c *Client) signOutgoing(op string, res reservation, recipients []string, body []byte) (sig string, timestampMS int64, err error) {
	cred, err := c.credential()
	if err != nil {
		return "", 0, err
	}
	if res.Sender != cred.AgentID {
		return "", 0, newError(KindServer, op,
			"the bus minted a reservation for "+safeText(res.Sender, 60)+" but this identity is "+safeText(cred.AgentID, 60),
			"point this identity at the bus that issued it; do not sign under an id this bus disputes")
	}
	priv, err := c.messagingKey()
	if err != nil {
		return "", 0, err
	}

	// Milliseconds UTC, and the SAME integer travels on the wire — see
	// signedMessage.TimestampUnixMilli for why no conversion happens between the
	// signed form and the wire form.
	timestampMS = c.now().UTC().UnixNano() / int64(time.Millisecond)

	raw, err := signSignedMessage(priv, signedMessage{
		MessageID:          res.MessageID,
		Sequence:           res.Seq,
		Sender:             cred.AgentID,
		Recipients:         recipients,
		TimestampUnixMilli: timestampMS,
		Body:               body,
	})
	if err != nil {
		// canonicalize failed closed: there are no bytes, so there is nothing to
		// sign. This is a local fault — a malformed recipient, an id that will
		// not parse — and it is reported as usage rather than as a bus failure.
		return "", 0, usagef(op, "check the recipient id and try again",
			"this message cannot be canonicalized for signing: %s", err)
	}
	return base64.StdEncoding.EncodeToString(raw), timestampMS, nil
}

// broadcastRequestBody mirrors httpapi.BroadcastRequestBody.
type broadcastRequestBody struct {
	Body           []byte `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Send delivers a direct message to one agent and returns once it is DURABLE.
//
// # Idempotency is the property this call turns on (invariant 10)
//
// The idempotency key is minted ONCE, here, before anything is marshalled — not
// per attempt. That matters because the transport retries: do() marshals the
// body a single time and replays those exact bytes on every attempt, so all
// attempts of one Send carry ONE key and one payload. The bus therefore sees
// "same key + same payload", answers the second attempt from its applied-key
// table, and writes exactly one message. If the key were minted per attempt, a
// retry after a lost acknowledgement would be a SECOND message; if the payload
// varied between attempts it would be "same key + different payload", which is
// a protocol violation that disconnects the client.
//
// That is also why the request is marked retryable at all. A POST is not safe
// to repeat in general; it is safe here precisely because it carries the key.
//
// A 409 — the key reused with different content — is surfaced as its own loud
// KindRejected error rather than the transport's generic wording, because the
// bus's answer to it is a disconnection and the caller needs to know it must
// use a fresh key rather than keep retrying.
func (c *Client) Send(ctx context.Context, opts SendOptions) (SendResult, error) {
	const op = "send"

	to := strings.TrimSpace(opts.To)
	if err := validateRecipient(op, to); err != nil {
		return SendResult{}, err
	}
	if err := validateSendBody(op, opts.Body); err != nil {
		return SendResult{}, err
	}
	key, err := resolveIdempotencyKey(op, opts.IdempotencyKey)
	if err != nil {
		return SendResult{}, err
	}

	// RESERVE, then SIGN, then SEND. All three carry the one idempotency key, so
	// the whole handshake is retryable as a unit — see reserve for why that is
	// what makes two round trips safe rather than merely tolerable.
	//
	// The context is shared across both calls deliberately: a deadline the
	// caller set for "this send" must bound the send, not each leg of it.
	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	res, err := c.reserve(ctx, op, mintOpSend, key)
	if err != nil {
		return SendResult{IdempotencyKey: key}, err
	}
	sig, timestampMS, err := c.signOutgoing(op, res, []string{to}, opts.Body)
	if err != nil {
		return SendResult{IdempotencyKey: key}, err
	}

	return c.submit(ctx, op, routeSend, sendRequestBody{
		To:             to,
		Body:           opts.Body,
		IdempotencyKey: key,
		Sender:         res.Sender,
		MessageID:      res.MessageID,
		Seq:            res.Seq,
		TimestampMS:    timestampMS,
		Signature:      sig,
	}, key)
}

// Broadcast delivers a message to every agent on the bus EXCEPT the sender, and
// returns once it is durable. See Send for the idempotency contract, which is
// identical.
//
// # Broadcast is refused, deliberately and permanently, until SIGN-3
//
// SIGN-6 made a signature mandatory on every message. A broadcast has no
// canonical audience to sign against under signing format v1:
// internal/signing.Canonicalize rejects an empty recipient set, and
// store.Message records a broadcast as a FLAG rather than an expanded
// recipient roster, so there is nothing stable to canonicalize and sign. The
// bus therefore answers every /v1/broadcast with 501 Not Implemented, refusing
// BEFORE it even decodes the body, rather than carrying an unsigned message.
// That server behaviour is correct and settled (SIGN-6); SIGN-3 is the task
// that decides a canonical broadcast audience and reopens this route.
//
// # Why this call still makes the round trip rather than failing locally
//
// The refusal is mapped from the bus's own 501 (statusError, transport.go),
// not pre-judged here before anything is sent. The bus is authoritative on
// whether a route is open (invariant 1 is about ids, but the same reasoning
// applies to routes: a client that decides "broadcast never works" for itself
// can drift from the server the moment SIGN-3 lands and the route reopens,
// silently blocking a now-valid operation with no signal that anything
// changed). A round trip on a call that is certain to be refused costs one
// request; a client hard-coded to assume the refusal costs a correctness bug
// that only shows up after a deploy nobody thought to touch this function for.
// See annotateBroadcastRefused for how the 501 is turned into an error an
// agent can act on.
func (c *Client) Broadcast(ctx context.Context, opts BroadcastOptions) (SendResult, error) {
	const op = "broadcast"

	if err := validateSendBody(op, opts.Body); err != nil {
		return SendResult{}, err
	}
	key, err := resolveIdempotencyKey(op, opts.IdempotencyKey)
	if err != nil {
		return SendResult{}, err
	}
	return c.submit(ctx, op, routeBroadcast, broadcastRequestBody{
		Body:           opts.Body,
		IdempotencyKey: key,
	}, key)
}

// submit is the shared write path of Send and Broadcast.
//
// The key is threaded through as a parameter rather than read back out of the
// marshalled body so that the FAILURE path can report it too: a send that
// failed with a network error may or may not have been applied, and the key is
// the only thing that lets the caller retry it as the same logical send instead
// of producing a second message.
func (c *Client) submit(ctx context.Context, op, route string, body interface{}, key string) (SendResult, error) {
	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	// Decoding straight into SendResult is safe because its json tags are the
	// wire shape for every field the bus sends. The two fields the bus does NOT
	// send — replayed and idempotency_key — are overwritten unconditionally
	// below, so a bus that put them in the body cannot influence either.
	var result SendResult
	resp, err := c.authorizedRequest(ctx, request{
		method: http.MethodPost,
		path:   route,
		op:     op,
		body:   body,
		out:    &result,
		// Safe to repeat: the payload was marshalled once, before the retry
		// loop, and carries the idempotency key. See Send.
		retryable: true,
	})
	if err != nil {
		return SendResult{IdempotencyKey: key}, writeFailed(op, key, annotateBroadcastRefused(op, annotateIdempotencyConflict(op, key, err)))
	}

	// The bus is authoritative on ids (invariant 1) — authoritative, not
	// unvalidated. Everything here is printed to a terminal and some of it is
	// stored, and a hostile bus chooses every byte of it. See sanitize.go.
	if err := validateServerField(op, "message id", result.MessageID); err != nil {
		return SendResult{IdempotencyKey: key}, writeFailed(op, key, err)
	}
	if err := validateServerField(op, "sender id", result.From); err != nil {
		return SendResult{IdempotencyKey: key}, writeFailed(op, key, err)
	}
	for _, to := range result.To {
		if err := validateServerField(op, "recipient id", to); err != nil {
			return SendResult{IdempotencyKey: key}, writeFailed(op, key, err)
		}
	}
	if err := validateServerContentHash(op, "content hash", result.ContentSHA256); err != nil {
		return SendResult{IdempotencyKey: key}, writeFailed(op, key, err)
	}
	if err := validateServerTimestamp(op, "sent_at", result.SentAt); err != nil {
		return SendResult{IdempotencyKey: key}, writeFailed(op, key, err)
	}

	// A replay is signalled OUT OF BAND because the body of a replay is
	// byte-identical to the original by design. It is not an error.
	result.Replayed = strings.EqualFold(resp.Header.Get(idempotencyReplayedHeader), "true")
	result.IdempotencyKey = key
	return result, nil
}

// Agents returns the bus's roster.
//
// Every field is validated before it is returned, because this data is printed
// to a terminal and a hostile bus controls all of it (sanitize.go). A field that
// fails validation FAILS THE WHOLE CALL rather than dropping that one entry: the
// roster is small, and a malformed entry does not mean "one bad agent", it means
// the thing on the other end is not the bus we think it is — silently returning
// the rest would hide exactly that.
func (c *Client) Agents(ctx context.Context) (AgentList, error) {
	const op = "agents"

	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	var list AgentList
	if _, err := c.authorizedRequest(ctx, request{
		method: http.MethodGet,
		path:   routeAgents,
		op:     op,
		out:    &list,
		// A GET that reads a roster changes nothing; repeating it is safe by
		// construction.
		retryable: true,
	}); err != nil {
		return AgentList{}, err
	}

	for i := range list.Agents {
		a := &list.Agents[i]
		if err := validateServerField(op, "agent id", a.AgentID); err != nil {
			return AgentList{}, err
		}
		if err := validateServerField(op, "name", a.Name); err != nil {
			return AgentList{}, err
		}
		if err := validateServerTimestamp(op, "enrolled_at", a.EnrolledAt); err != nil {
			return AgentList{}, err
		}
		// The wire carries no bus id on this route. Invariant 2 says every agent
		// id is `<bus-id>.<agent-id>`, so the prefix up to the FIRST '.' is the
		// bus — first, not last, because an agent id may itself contain dots
		// while a bus id may not.
		busID, _, ok := splitQualifiedID(a.AgentID)
		if !ok {
			return AgentList{}, newError(KindServer, op,
				"the bus listed an agent id that is not fully qualified: "+safeText(a.AgentID, 60),
				"every agent id is `<bus-id>.<agent-id>` (invariant 2); check that --bus points at an agent-bus server")
		}
		a.BusID = busID
	}

	// The bus already sorts by agent id. Sorting again costs nothing on a list
	// this size and means the ORDER of this package's output is a property of
	// this package, not of a remote service's current implementation.
	sort.Slice(list.Agents, func(i, j int) bool { return list.Agents[i].AgentID < list.Agents[j].AgentID })
	if list.Agents == nil {
		list.Agents = []AgentSummary{}
	}
	list.Count = len(list.Agents)
	return list, nil
}

// Read fetches one batch of messages, either as history or as a long poll.
//
// # The two forms, and why the deadline differs between them
//
// With Wait == 0 this is GET /v1/messages: it never parks, and it is bounded by
// Config.Timeout like every other request/response call.
//
// With Wait > 0 this is GET /v1/wait, which PARKS on the bus for up to that
// long. Such a call must NOT inherit Config.Timeout: the default is 30s and the
// bus's ceiling is 5 minutes, so a caller asking for a 5-minute poll would have
// its request killed at 30 seconds — every time, on a quiet bus, looking exactly
// like a broken connection. The wait form therefore gets its own deadline of
// the poll timeout PLUS a slack margin (the larger of minPollSlack and
// Config.Timeout) to cover the round trip and the bus's own scheduling, and a
// caller's tighter deadline still wins because context.WithTimeout never
// extends a parent.
//
// # A timed-out poll is a SUCCESS
//
// The bus answers a poll that found nothing with 200, an empty message list,
// timed_out true, and the cursor unchanged. That is returned as an ordinary
// Batch, never as an error: on a quiet bus it is the steady state, and treating
// it as a failure is how a watcher ends up logging an error every 30 seconds
// for a bus that is working perfectly.
func (c *Client) Read(ctx context.Context, opts ReadOptions) (Batch, error) {
	op, route := "read", routeMessages
	if opts.Wait != 0 {
		op, route = "wait", routeWait
	}

	q := url.Values{}
	if opts.Cursor != "" {
		if len(opts.Cursor) > maxCursorLen {
			return Batch{}, usagef(op,
				"pass a cursor this bus issued, or none at all to start from the beginning of the retained window",
				"cursor is %d bytes, the limit is %d", len(opts.Cursor), maxCursorLen)
		}
		q.Set("cursor", opts.Cursor)
	}
	switch {
	case opts.Limit < 0:
		return Batch{}, usagef(op, "use a limit between 1 and "+strconv.Itoa(MaxBatchLimit)+", or 0 to let the bus choose",
			"limit %d is negative", opts.Limit)
	case opts.Limit > MaxBatchLimit:
		return Batch{}, usagef(op, "use a limit of at most "+strconv.Itoa(MaxBatchLimit)+", and page with the returned cursor",
			"limit %d is above the bus's ceiling of %d", opts.Limit, MaxBatchLimit)
	case opts.Limit > 0:
		q.Set("limit", strconv.Itoa(opts.Limit))
	}

	var cancel context.CancelFunc
	if opts.Wait != 0 {
		secs, err := pollTimeoutSeconds(op, opts.Wait)
		if err != nil {
			return Batch{}, err
		}
		q.Set("timeout", strconv.Itoa(secs))

		if ctx == nil {
			ctx = context.Background()
		}
		slack := c.cfg.Timeout
		if slack < minPollSlack {
			slack = minPollSlack
		}
		ctx, cancel = context.WithTimeout(ctx, time.Duration(secs)*time.Second+slack)
	} else {
		ctx, cancel = c.contextWithTimeout(ctx)
	}
	defer cancel()

	var batch Batch
	if _, err := c.authorizedRequest(ctx, request{
		method: http.MethodGet,
		path:   route,
		query:  q,
		op:     op,
		out:    &batch,
		// A GET with a cursor is safe to repeat by construction: it reads a
		// position, it moves nothing, and the bus advances no state on our
		// behalf. Re-reading the same position returns the same messages.
		retryable: true,
		// A full batch is far larger than the generic response bound allows.
		maxResponse: maxBatchResponseBytes,
	}); err != nil {
		return Batch{}, err
	}
	if err := validateBatch(op, &batch); err != nil {
		return Batch{}, err
	}
	if batch.Messages == nil {
		batch.Messages = []Message{}
	}
	return batch, nil
}

// pollTimeoutSeconds converts a poll duration to the WHOLE SECONDS the bus's
// ?timeout= parameter takes.
//
// The bus rejects anything that is not a positive whole number of seconds with
// a 400, so a sub-second poll is rounded UP to 1s rather than truncated to 0.
// A value above MaxPollTimeout is REFUSED rather than silently clamped, exactly
// as the bus refuses it: a caller that asked for an hour and was quietly given
// five minutes would conclude its request had been dropped.
func pollTimeoutSeconds(op string, d time.Duration) (int, error) {
	if d < 0 {
		return 0, usagef(op, "use a positive poll timeout, e.g. 30s", "poll timeout %s is negative", d)
	}
	if d > MaxPollTimeout {
		return 0, usagef(op,
			"use a poll timeout of at most "+MaxPollTimeout.String()+" and poll again; the bus refuses a longer one rather than clamping it",
			"poll timeout %s is above the bus's ceiling of %s", d, MaxPollTimeout)
	}
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs, nil
}

// validateBatch checks everything a batch carries that will be printed or
// stored. See sanitize.go for why the bus is not trusted to produce safe text.
func validateBatch(op string, b *Batch) error {
	if err := validateServerCursor(op, b.Cursor); err != nil {
		return err
	}
	for i := range b.Messages {
		m := &b.Messages[i]
		if err := validateServerField(op, "message id", m.MessageID); err != nil {
			return err
		}
		if err := validateServerField(op, "sender id", m.From); err != nil {
			return err
		}
		if err := validateServerTimestamp(op, "sent_at", m.SentAt); err != nil {
			return err
		}
		for _, to := range m.To {
			if err := validateServerField(op, "recipient id", to); err != nil {
				return err
			}
		}
		for _, bus := range m.BusPath {
			if err := validateServerField(op, "bus id in bus_path", bus); err != nil {
				return err
			}
		}
		if err := validateServerContentHash(op, "content hash", m.ContentSHA256); err != nil {
			return err
		}
		if err := verifyMessageBody(op, m); err != nil {
			return err
		}
	}
	return nil
}

// verifyMessageBody compares size and content_sha256 against the DECODED body.
//
// # What this proves, and what it emphatically does not
//
// It proves the bytes and the metadata beside them AGREE. It detects corruption
// in transit, a proxy that mangled a body, and a bus whose own answer is
// internally inconsistent. It is NOT authenticity: the BUS computes the hash, so
// a bus that wants to lie signs nothing and simply hashes the body it invented.
// Sender authenticity is the signature (canonical.go, the SIGN epic) and nothing
// here weakens or substitutes for it. Do not let a passing hash be read as "this
// message really came from that agent".
//
// # Why it is a HARD failure for the whole batch
//
// Invariant 7 names "verification of inbound messages" as the client's job, and
// this is the cheap half of it. The alternative — a per-message flag — puts the
// decision in every caller and will be ignored by most of them, which is the
// failure mode a verification step exists to prevent. Failing the batch also
// matches what Agents already does with a malformed roster entry, and it leaves
// the WATCH CURSOR EXACTLY WHERE IT WAS: Read returns an error and Watch never
// advances, so NOTHING IS SKIPPED.
//
// Note what that does NOT mean. The failure is marked fatal (see
// bodyIntegrityError), so the watch STOPS rather than re-reading the batch — the
// position is preserved for a later run, not retried in this one. Preserving the
// position and retrying it are different things, and only the first is promised
// here: a retry would fetch the same damaged message for ever.
//
// # Why both fields, and in this order
//
// Size is checked first because it gives the far more useful diagnosis. A
// truncated body fails both checks, but "size says 4096, body is 3 bytes" names
// the fault; "hash mismatch" only says something is wrong. The hash then catches
// everything a length check cannot: a body of the right length with the wrong
// bytes.
//
// An ABSENT field is not verified. A bus that sends no content_sha256 (Size 0,
// or the field omitted) is an older bus, and refusing to read from it would turn
// a version skew into an outage for a check that — see above — is not an
// authenticity control anyway. An empty body is refused on the send path, so
// Size 0 unambiguously means "not sent" rather than "a zero-length message".
//
// Note the test is `!= 0`, NOT `> 0`. `size` is a plain JSON integer, so a bus
// can send a NEGATIVE one; `> 0` would have let it skip the length check
// entirely while the field's doc comment promised the two had been compared. A
// negative size can never equal len(Body), so it now fails, which is right.
//
// Cost: one SHA-256 over each body, bounded by the response reader rather than
// by the nominal per-message limit: maxBatchResponseBytes caps a batch at 4 MiB,
// so that is the real worst case per poll — a few milliseconds, against a call
// that has just done a network round trip.
func verifyMessageBody(op string, m *Message) error {
	if m.Size != 0 && m.Size != len(m.Body) {
		return bodyIntegrityError(op,
			fmt.Sprintf("the bus sent message %s with size %d but a body of %d bytes",
				safeText(m.MessageID, 60), m.Size, len(m.Body)))
	}
	if m.ContentSHA256 != "" {
		sum := sha256.Sum256(m.Body)
		if got := hex.EncodeToString(sum[:]); got != m.ContentSHA256 {
			return bodyIntegrityError(op,
				fmt.Sprintf("the bus sent message %s with a body that hashes to %s but a content_sha256 of %s",
					safeText(m.MessageID, 60), got, m.ContentSHA256))
		}
	}
	return nil
}

// bodyIntegrityError builds the failure, and marks it FATAL.
//
// The fatal bit is not decoration, it is what makes this a hard error rather
// than a hang. Without it the error is an ordinary KindServer, which both the
// transport retry loop and watchShouldRetry treat as transient — so `agent-busctl
// watch` re-read the SAME cursor, got the SAME damaged message, and looped until
// --for expired, exiting 8 ("no messages arrived") with the remedy never once
// reaching the agent. That is the exact mis-attribution this task exists to
// remove, in a new costume: the reader is told nothing arrived when in fact
// something arrived damaged.
//
// It is also simply true. Retrying re-reads a position, the bus is deterministic
// about what lives there, and no number of attempts turns a body that disagrees
// with its digest into one that agrees.
func bodyIntegrityError(op, message string) *Error {
	e := newError(KindServer, op, message, bodyIntegrityRemedy)
	e.fatal = true
	return e
}

// bodyIntegrityRemedy names WHICH SIDE is at fault, because getting that wrong
// is what this check exists to prevent.
//
// The failure it came out of: an agent saw short message bodies and nearly filed
// a bus defect. The bus was innocent — an over-limit body is REJECTED on send,
// never cut — and the truncation was in the agent's own consumer. Establishing
// that took hours of detective work that this one sentence replaces, in both
// directions: if the check FAILS the damage is on the bus side of this client,
// and if it PASSES the body was intact when it was handed over, so anything
// short after that was shortened by the consumer.
//
// # Two things it must NOT say, and the second is easy to get wrong
//
// It must not say "retry": this failure is marked fatal precisely because a
// retry re-reads the same position and gets the same damaged message, so the
// advice would send the reader round the loop the fatal bit exists to break.
//
// It must not offer `--replay` either, which is the subtler mistake — an earlier
// draft of this string did. `--replay` restarts at position 0 and therefore
// walks straight back into the same damaged message. So does resuming from the
// stored cursor, which sits BEFORE it. The only position that gets past a
// damaged message is one AFTER it, so `--cursor` is the sole escape and the
// remedy says exactly that rather than listing every flag that sounds relevant.
// CLAUDE.md's rule is that a remedy names the remedy; a flag that will not work
// costs the reader a second failure on top of the first.
//
// The message id is named in the Message half of the error, which is what makes
// "the one after this" a thing the reader can actually identify.
const bodyIntegrityRemedy = "the body and its metadata arrived already disagreeing, so the fault is on the BUS side of this client (the bus, or something between you and it) and not in your handler. Retrying will not clear it and neither will --replay: both re-read a position at or before the damaged message and get it again. Check the bus's logs for the message id above; to step over it, resume with `agent-busctl watch --cursor <a position AFTER that message>`. Note the converse, which is usually the question being asked: this check runs BEFORE a body reaches a caller, so a body that looks truncated after a SUCCESSFUL read was truncated by the consumer, not by the bus"

// cursorPattern is the shape a cursor the bus issued must have.
//
// The cursor is opaque and this client never parses it — the check is a SAFETY
// bound, not a re-implementation. It admits the base64url alphabet plus padding
// and the few punctuation characters a future encoding might reasonably use,
// and admits nothing that can move a terminal cursor. The length bound is what
// stops a hostile bus from handing back a value that is then written to the
// local cursor file on every poll.
var cursorPattern = regexp.MustCompile(`^[A-Za-z0-9._~=-]{1,512}$`)

func validateServerCursor(op, cursor string) error {
	if cursor == "" {
		return nil
	}
	if !cursorPattern.MatchString(cursor) {
		return newError(KindServer, op,
			"the bus returned a cursor that is not a well-formed opaque token",
			"check that --bus points at an agent-bus server and not at another service")
	}
	return nil
}

// validateSendBody refuses a body the bus would refuse, before the round trip.
func validateSendBody(op string, body []byte) error {
	if len(body) == 0 {
		// Refusing an ambiguous or empty send rather than sending nothing: an
		// empty body is almost always a caller that read from an empty file, an
		// empty pipe or an unset variable, and the bus rejects it anyway.
		//
		// The remedy names the REAL surface: there is no --body flag. The body
		// is a positional argument, --file <path>, or stdin. A remedy that names
		// a flag which does not exist costs the reader a second failure ("flag
		// provided but not defined: -body") on top of the first, which is the
		// opposite of invariant 7's "errors that name the remedy".
		return usagef(op,
			"pass a non-empty body as a quoted argument, with --file <path>, or on stdin; an empty send is refused rather than delivered",
			"no message body")
	}
	if len(body) > MaxBodyBytes {
		return usagef(op,
			fmt.Sprintf("the bus accepts at most %d bytes of body; split the payload or send a reference to it", MaxBodyBytes),
			"message body is %d bytes, the limit is %d", len(body), MaxBodyBytes)
	}
	return nil
}

// validateRecipient checks the SHAPE of a recipient id, locally.
//
// It is deliberately permissive, and this is not laziness: invariant 1 keeps the
// SERVER authoritative on ids, so a client that re-derived the id grammar would
// start refusing legitimate ids the day the server's format grew. All this does
// is catch the mistake that is worth catching locally — a bare name where a
// fully-qualified `<bus-id>.<agent-id>` belongs (invariant 2) — and refuse
// anything that could not be an id at all.
func validateRecipient(op, to string) error {
	if to == "" {
		// Again: the recipient is POSITIONAL, not a --to flag. See
		// validateSendBody for why naming a flag that does not exist is worse
		// than saying nothing.
		return usagef(op,
			"pass the fully-qualified <bus-id>.<agent-id> as the first argument: `agent-busctl send <to> 'message'`; list them with `agent-busctl agents`",
			"no recipient")
	}
	if !serverIDPattern.MatchString(to) {
		return usagef(op,
			"a recipient is 1-256 bytes of [A-Za-z0-9._-]; find the exact id with `agent-busctl agents`",
			"recipient %q is not a well-formed agent id", safeText(to, 60))
	}
	if _, _, ok := splitQualifiedID(to); !ok {
		return usagef(op,
			"use the fully-qualified `<bus-id>.<agent-id>`, not the short name; find it with `agent-busctl agents`",
			"recipient %q is not fully qualified", safeText(to, 60))
	}
	return nil
}

// splitQualifiedID splits a fully-qualified agent id at the FIRST '.'
// (invariant 2: the prefix is the bus id). Both halves must be non-empty.
func splitQualifiedID(id string) (busID, agentID string, ok bool) {
	i := strings.Index(id, ".")
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// resolveIdempotencyKey returns the caller's key, validated, or a fresh one.
func resolveIdempotencyKey(op, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return newIdempotencyKey()
	}
	if err := validateIdempotencyKey(op, key); err != nil {
		return "", err
	}
	return key, nil
}

// writeFailed stamps the idempotency key onto a failed send or broadcast, and
// — for the AMBIGUOUS failures only — names the flag that retries it.
//
// # Why the key belongs on the error and not only on the result
//
// The failures where the key matters most are exactly the ones with no usable
// result. A transport failure or a 5xx means the message MAY OR MAY NOT have
// been applied: the request reached the bus, the answer did not come back, and
// nothing on this side can tell the two apart. The key is the only thing that
// makes the retry the SAME logical send. A caller told only "it failed", who
// then retries with a freshly minted key, has not retried at all — it has sent
// a SECOND message, which is precisely the double-apply invariant 10 exists to
// prevent. So `agent-busctl send --help`'s promise that "the key is always printed
// back" has to hold on the failure path or it is worth nothing.
//
// # Why the remedy is only added to SOME failures
//
// It is added to KindNetwork and KindServer, which are the ambiguous ones. It is
// deliberately NOT added to a 409, whose remedy already says the opposite and
// correctly so: that key was used with different content, and retrying under it
// is the protocol violation, not the fix. Nor to a KindRejected 404 (an unknown
// recipient will be unknown next time either). Those failures still CARRY the
// key — an embedder may want it — they just are not told to reuse it.
//
// # The key clause COMPOSES with the existing remedy, it does not replace it
//
// The transport's remedy is the DIAGNOSIS ("check --bus / AGENT_BUS_URL and that
// the bus is running"); the key clause is the mechanics of retrying. Overwriting
// the first with the second destroys the only sentence that says what is
// actually wrong, so the clause is appended after "; " when a remedy is already
// present, and stands alone only when there is none.
//
// # Why the fatal wording is different
//
// A 503 with no Retry-After is classified fatal (see the 503 split in
// transport.go): the bus is refusing because its write path cannot durably
// accept, and by invariant 4 refusing is exactly what it SHOULD do rather than
// acknowledge something it might lose. Nothing on the client side clears that,
// so telling the operator to "retry" would be both false and harmful — it
// hammers a dead write path and contradicts IsFatalUnavailable, which still
// reports the failure as not-retryable. For those the key is a handle for LATER,
// once the bus can durably accept again; it is not an invitation to retry now.
//
// enrolFailed (enrol.go) solves the same problem for enrolment and now draws
// both distinctions the same way (45b2e17a / 799aea40, fixed): it COMPOSES
// the remedy rather than replacing it, checks e.fatal, and always stamps
// Error.IdempotencyKey. The two are not literally shared code — enrolment's
// clause names "this enrol" and "the SAME enrolment" where a send names the
// message — but the composition logic (the TrimRight/"; " join, the
// wantRemedyUnchanged-shaped default branch) is now byte-for-byte the same
// pattern, and a future edit to one of these should check the other still
// agrees.
func writeFailed(op, key string, err error) error {
	// errors.As, not a type assertion — a wrapped *Error would otherwise slip
	// through and lose both the key and the remedy.
	var e *Error
	if !errors.As(err, &e) {
		return err
	}
	e.IdempotencyKey = key
	switch e.Kind {
	case KindNetwork, KindServer:
		var clause string
		if e.fatal {
			clause = "this " + op + " may or may not have been applied; do NOT retry until the bus can durably accept again, then use --idempotency-key " + key +
				" so the retry is the SAME message rather than a second one (invariant 10)"
		} else {
			clause = "this " + op + " may or may not have been applied; retry with --idempotency-key " + key +
				" so the retry is the SAME message rather than a second one (invariant 10)"
		}
		// TrimRight so a remedy that already ends in a separator does not
		// produce ";; " — the join must add exactly one.
		if base := strings.TrimRight(e.Remedy, "; "); base != "" {
			e.Remedy = base + "; " + clause
		} else {
			e.Remedy = clause
		}
	}
	return e
}

// mintLostMarker is the substring internal/httpapi's writeHubError puts in the
// body of a 409 caused by hub.ErrUnknownMint / hub.ErrMintMismatch — it points
// the caller at POST routeMint by name. Matching on it is how this package
// tells that 409 apart from the OTHER one (invariant 10's key-reused-with-
// different-payload) without importing internal/ (invariant 7): both are
// literal duplicates of a server string, same as routeMint itself already is.
const mintLostMarker = "POST " + routeMint

// annotateIdempotencyConflict replaces the transport's generic 409 wording with
// the specific one the actual failure deserves — and a 409 on this bus is TWO
// different failures wearing the same status code, not one:
//
//   - invariant 10's one unforgivable case: the SAME idempotency key presented
//     with a DIFFERENT payload. The bus treats that as a protocol violation —
//     it answers 409, REJECTS it and LOGS it (narrowed 2026-08-08,
//     IDEM-14-FU-CLIENTTEXT — it does NOT disconnect; an earlier version of
//     this comment claimed it did) — so the caller must be told plainly that
//     retrying will not help and a FRESH key is the fix.
//   - hub.ErrUnknownMint / hub.ErrMintMismatch: the reservation this key named
//     is not one the bus is holding. ErrUnknownMint is ROUTINE — the mint
//     table is in-memory only and does not survive a restart — so this is the
//     ordinary post-restart case, not a protocol violation. Telling the caller
//     to use a FRESH key here is actively harmful: if the original send had
//     already landed, a fresh key produces a SECOND message, which is exactly
//     the double-apply invariant 10 forbids. The correct remedy is to redo the
//     WHOLE reserve-then-send under the SAME idempotency key.
//
// The generic text ("an idempotency key was reused with different content")
// invites the wrong reaction either way — a retry loop in the first case, a
// fresh key in the second — so both are replaced, and which replacement is
// used is decided by mintLostMarker, not by assuming every 409 is the same
// failure.
func annotateIdempotencyConflict(op, key string, err error) error {
	// errors.As, not a type assertion: a wrapped *Error would otherwise slip
	// through and keep the generic wording.
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict {
		return err
	}
	detail := strings.TrimPrefix(e.Message, "the bus refused the request: ")
	if strings.Contains(detail, mintLostMarker) {
		e.Kind = KindRejected
		e.Message = "the bus has no matching reservation for this " + op + " under idempotency key " + key + ": " + detail
		e.Remedy = "re-mint under the SAME idempotency key (POST " + routeMint + "), re-sign, and re-send — do NOT switch to a fresh key: if the original " + op + " already landed, a fresh key would apply it a SECOND time (invariant 10). This is routine after a bus restart; the mint table is memory-only and does not survive one."
		return e
	}
	e.Kind = KindRejected
	e.Message = "the bus refused this " + op + ": idempotency key " + key +
		" was already used with a DIFFERENT payload"
	// NOT "the bus disconnects the client": measured field evidence (2026-08-07,
	// IDEM-14-FU-CLIENTTEXT) is that it does not — the connection survives this
	// 409 today, and asserting otherwise risks an agent taking destructive
	// recovery (re-enrol, rebuild identity, restart supervisors) it does not
	// need. IDEM-14 (b0facce9), still open, is what decides and implements the
	// actual disconnect mechanics; this text must not get ahead of it again.
	//
	// Removing a false claim is not enough on its own — a reader must be told
	// what DOES happen, not just that the old sentence was wrong, or "not
	// disconnected" reads as "nothing happened" and invites its own bad guess
	// (retry under the same key, say). So this says explicitly: rejected,
	// logged, connection kept open. That is the positive half of invariant
	// 10's narrowing, not just the negative one.
	e.Remedy = "use a FRESH idempotency key for new content — reusing one with different content is a protocol violation: the bus rejects it with this 409 and logs it, but does NOT drop the connection (invariant 10), so this session and any other request pipelined on it remain usable; to RETRY the original message, resend it byte for byte under the same key"
	return e
}

// annotateBroadcastRefused replaces the transport's generic 501 wording with
// the specific, actionable one a refused broadcast deserves.
//
// The bus's own detail already says WHY (see broadcastUnsignableReason in
// internal/httpapi/messages.go), but not what to do about it, and "an internal
// error, maybe retry" is exactly the wrong read of a deliberate, permanent
// refusal — see the SIGN-6(6) note on Client.Broadcast. This is what turns that
// into "use send instead, this is settled until SIGN-3".
func annotateBroadcastRefused(op string, err error) error {
	// errors.As, not a type assertion, for the same reason as above.
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusNotImplemented {
		return err
	}
	e.Kind = KindRejected
	e.Message = "the bus refuses to " + op + ": a broadcast cannot be signed under signing format v1 " +
		"(SIGN-6 requires every message to carry a signature, and a broadcast has no canonical recipient " +
		"set to sign against — that is SIGN-3's open question); nothing was applied, the bus refused before " +
		"reading the message body"
	e.Remedy = "this is deliberate and will not change until SIGN-3 decides a canonical broadcast audience — " +
		"do not retry; send the message directly to each recipient with `agent-busctl send` (or Client.Send) instead"
	return e
}
