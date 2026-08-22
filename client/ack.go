package client

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// routeAck is the RECIPIENT half of the acknowledgement plane
// (ACK-CONTRACT.md §13.4, ACK-6): POST /v1/ack. Pinned as a literal mirroring
// internal/httpapi.RouteAck, for the reason routeAckStatus gives — this package
// must not import anything under internal/ (doc.go, invariant 7).
//
// IT IS "/v1/ack" WITH NO TRAILING SLASH AND routeAckStatus IS "/v1/ack/" WITH
// ONE. http.ServeMux resolves the first as an exact match and the second as a
// subtree, which is what lets the two live side by side; a trailing slash added
// here would silently start POSTing into the STATUS route's subtree.
const routeAck = "/v1/ack"

// AckSigningContext is the domain-separation prefix and the FIRST field of the
// canonical bytes a recipient signs. Mirrors internal/signing.AckContext.
//
// It is load-bearing because ONE key — the messaging key — now signs two
// languages: a message (MessageSigningContext) and an acknowledgement. Without
// separate contexts, bytes canonicalized under one could in principle be
// presented as the other. The format version is spelled INSIDE this string, so
// there is exactly one version indicator in the signed bytes and no way for two
// of them to disagree.
const AckSigningContext = "agent-bus/recipient-ack/3"

// AckSigningFormatVersion is the version the layout below implements. Mirrors
// internal/signing.AckFormatVersion — a RESERVED number from the Spec Server
// `signing-format-version` namespace (1 = messages, 2 = attestations, 3 = these
// acknowledgements). Nobody picks one by reading this constant, and changing
// ANY of the layout is a NEW reserved version with a new context string, never
// an in-place edit of this one.
const AckSigningFormatVersion = 3

// The three classes a RECIPIENT may emit (ACK-CONTRACT.md §5.2), mirroring
// internal/signing's AckClassRecipientRefused* constants.
//
// The full NACK vocabulary has twelve members; the other NINE are ROUTING
// claims a bus makes about its own federation (no_route, hop_refused,
// loop_dropped and the rest) and a recipient has no standing to sign one. They
// are deliberately absent from this package: a constant here would be a
// constant somebody could pass.
//
// There is NO free-text reason field beside them and there must never be one
// (invariant 6): a reason string sourced from a recipient is a message body by
// another name, and this trail is append-only.
const (
	AckClassRecipientRefusedPolicy       = "recipient_refused_policy"
	AckClassRecipientRefusedUndecodable  = "recipient_refused_undecodable"
	AckClassRecipientRefusedNotAddressed = "recipient_refused_not_addressed"
)

// RecipientRefusalClasses returns the closed triple above, in the order a
// human-facing list should print them. A fresh slice each call: a caller must
// not be able to edit the set by writing through the return value.
func RecipientRefusalClasses() []string {
	return []string{
		AckClassRecipientRefusedPolicy,
		AckClassRecipientRefusedUndecodable,
		AckClassRecipientRefusedNotAddressed,
	}
}

// ackWireProtocolVersion is the ACK WIRE-protocol version this client
// transmits, mirroring internal/relay.AckWireVersion.
//
// IT IS NOT AckSigningFormatVersion AND THE TWO MUST NOT BE CONFLATED. This one
// numbers the JSON FRAME on one hop; that one numbers the SIGNED BYTES end to
// end. They are independent by design — §9.2's wire version is deliberately NOT
// covered by the signature, so that bumping the hop transport never invalidates
// signatures already in flight.
const ackWireProtocolVersion = 1

// errNotCanonicalAck wraps every failure of canonicalizeAck. Like
// errNotCanonical for messages, canonicalizeAck NEVER returns partial or
// best-effort bytes: either the acknowledgement is well formed and the bytes
// are exact, or there are none at all.
var errNotCanonicalAck = errors.New("acknowledgement cannot be canonicalized")

// ackSigned is the set of fields the acknowledgement signature covers — the
// whole set, in the order they are encoded. PINNED MIRROR of
// internal/signing.Ack; see canonical.go's header for why this package copies a
// byte layout instead of importing one, and why divergence FAILS CLOSED.
//
// A field that is not here is NOT protected and may be changed by any bus on
// the path. The omissions are decisions, recorded in internal/signing.Ack: the
// relay wire version (per-hop, would make every bump a flag day), the traversed
// bus path (grows per hop), the ACK's idempotency key (covering it would turn
// invariant 10's legitimate retry into a fresh statement), the sender (already
// named by the origin-minted correlation key) and a key epoch (nothing exposes
// an agent's messaging-key epoch, so it would be structurally always zero — a
// binding that looks real and is not).
type ackSigned struct {
	// CorrelationKey is the ORIGIN bus's server-minted message id,
	// "<bus-id>-<seq>" — what SendResult.MessageID reports and what a received
	// Message carries as MessageID.
	//
	// It is the EXISTING third identifier and NOT a fourth: Seq is identity,
	// Pos is delivery position, and this is correlation. Confusing the three
	// has caused three defects in this repository.
	CorrelationKey string

	// Recipient is the SIGNER's OWN fully-qualified id, "<bus-id>.<agent-id>"
	// (invariant 2), inside the signed bytes. It is what binds the signature to
	// the agent that made it: without it, a signature over (key, outcome,
	// class, time) would be transplantable onto any other recipient of the same
	// message.
	Recipient string

	// Outcome is "delivered" or "refused" AND NOTHING ELSE. In particular a
	// recipient may not sign "undeliverable" — that is a claim about a bus's
	// routing, not about receipt.
	Outcome string

	// Class is the recipient-emitted refusal class, and is EMPTY when Outcome
	// is "delivered": a success has nothing to explain, and an optional class on
	// a positive acknowledgement would be a disclosure channel where none is
	// needed (§5.4). "" is never a valid class token, so the empty string is an
	// unambiguous encoding of "no class".
	Class string

	// EmittedAtUnixMilli is this agent's wall clock in Unix milliseconds UTC,
	// encoded fixed-width. THE SAME INTEGER TRAVELS ON THE WIRE — see Ack for
	// why nothing is converted between the signed form and the frame.
	//
	// It is NOT a freshness mechanism: clocks lie. Replay is handled where
	// replay is always handled here, server-side, by the absorbing terminal
	// state and the idempotency decision (invariant 10).
	EmittedAtUnixMilli int64
}

// canonicalizeAck returns the exact bytes to be signed. The layout is
// normative and mirrors internal/signing.CanonicalizeAck; all integers are
// big-endian and every variable-length field is preceded by its uint32 length:
//
//	uint32 len || AckSigningContext     ("agent-bus/recipient-ack/3")
//	uint32 len || CorrelationKey        ("<origin-bus-id>-<seq>")
//	uint32 len || Recipient             ("<bus-id>.<agent-id>")
//	uint32 len || Outcome               ("delivered" | "refused")
//	uint32 len || Class                 ("" iff Outcome is "delivered")
//	int64         EmittedAtUnixMilli    (two's complement)
//
// The field COUNT is fixed and every field is length-prefixed, so the encoding
// is INJECTIVE: no attacker can shift bytes across a field boundary — move the
// tail of a correlation key into the head of a recipient id, say — to present a
// different logical acknowledgement under a signature that still verifies.
//
// The output is handed to ed25519.Sign UNHASHED. Do not pre-hash it: RFC 8032
// Ed25519 signs the message itself, and a digest handed to ed25519.Sign yields a
// signature no conforming verifier will ever reproduce — it still "signs" and
// protects nothing (invariant 9).
func canonicalizeAck(a ackSigned) ([]byte, error) {
	if err := validateAckSigned(a); err != nil {
		return nil, err
	}
	size := 4 + len(AckSigningContext) +
		4 + len(a.CorrelationKey) +
		4 + len(a.Recipient) +
		4 + len(a.Outcome) +
		4 + len(a.Class) +
		8
	out := make([]byte, 0, size)
	out = appendLenPrefixed(out, []byte(AckSigningContext))
	out = appendLenPrefixed(out, []byte(a.CorrelationKey))
	out = appendLenPrefixed(out, []byte(a.Recipient))
	out = appendLenPrefixed(out, []byte(a.Outcome))
	out = appendLenPrefixed(out, []byte(a.Class))
	// Two's-complement int64, so the LAYOUT admits a pre-1970 clock without a
	// version bump; validateAckSigned nevertheless refuses a non-positive value
	// as an unset field.
	out = appendUint64(out, uint64(a.EmittedAtUnixMilli))
	return out, nil
}

// validateAckSigned checks every field and fails closed.
func validateAckSigned(a ackSigned) error {
	// The correlation key must parse as a server-minted message id. Parsing it
	// is what makes it the correlation key of §3 rather than any string a caller
	// fancied.
	if _, _, err := parseSignedMessageID(a.CorrelationKey); err != nil {
		return fmt.Errorf("%w: correlation key: %v", errNotCanonicalAck, err)
	}
	if _, err := qualifyingBusID(a.Recipient); err != nil {
		return fmt.Errorf("%w: recipient: %v", errNotCanonicalAck, err)
	}

	// THERE IS DELIBERATELY NO CHECK THAT THE TWO BUS HALVES AGREE, and adding
	// one would be a DEFECT rather than a hardening.
	//
	// The MESSAGE format does bind them, because a message is signed by an agent
	// of the bus that minted its id (validateSignedMessage's origin binding). An
	// ACKNOWLEDGEMENT is the opposite case by construction: the whole point of
	// the relay plane is that the recipient is usually on a DIFFERENT bus from
	// the origin, so in A -> B the correlation key's bus half is A's and this
	// agent's is B's. §6.2 names this exact trap; the local-delivery case where
	// the halves coincide is a special case of the general rule, not the rule.
	switch a.Outcome {
	case AckDelivered:
		if a.Class != "" {
			// Refused rather than silently dropped: dropping would let two
			// different logical statements share one signature.
			return fmt.Errorf("%w: outcome %q carries class %q, but a positive acknowledgement has nothing to explain and carries no class at all",
				errNotCanonicalAck, a.Outcome, safeText(a.Class, 32))
		}
	case AckRefused:
		switch a.Class {
		case AckClassRecipientRefusedPolicy, AckClassRecipientRefusedUndecodable, AckClassRecipientRefusedNotAddressed:
		case "":
			return fmt.Errorf("%w: outcome %q requires a recipient-emitted class; an unexplained refusal is not signable",
				errNotCanonicalAck, a.Outcome)
		default:
			return fmt.Errorf("%w: class %q is not one of the three a recipient may emit (%s); the nine bus-emitted classes are routing claims a recipient has no standing to sign",
				errNotCanonicalAck, safeText(a.Class, 32), strings.Join(RecipientRefusalClasses(), ", "))
		}
	case "":
		return fmt.Errorf("%w: outcome is empty; the closed pair this format signs is %s, %s", errNotCanonicalAck, AckDelivered, AckRefused)
	default:
		return fmt.Errorf("%w: outcome %q is not one of %s, %s; in particular a recipient may not sign %q, which is a routing claim asserted by a bus",
			errNotCanonicalAck, safeText(a.Outcome, 32), AckDelivered, AckRefused, AckUndeliverable)
	}

	if a.EmittedAtUnixMilli <= 0 {
		return fmt.Errorf("%w: emitted-at %d is not a positive Unix millisecond value; 0 means \"unset\"", errNotCanonicalAck, a.EmittedAtUnixMilli)
	}
	return nil
}

// signAck canonicalizes a and returns the detached Ed25519 signature over those
// exact bytes, made with this agent's MESSAGING private key.
//
// THE MESSAGING KEY, NEVER THE AUTH KEY (invariant 3). The auth key proves this
// agent to its BUS; the messaging key proves this agent to its PEERS, and an
// acknowledgement is a statement to the SENDER, not to any bus on the path.
// Signing with the auth key would still produce 64 plausible bytes that every
// bus would accept — every bus checks SHAPE ONLY — so nothing on the wire would
// ever report the mistake.
func signAck(priv ed25519.PrivateKey, a ackSigned) ([]byte, error) {
	// Checked BEFORE ed25519.Sign, which PANICS on a wrong-size private key.
	// The key's LENGTH is reported; the key is not, and must never be.
	if len(priv) != MessagingPrivateKeySize {
		return nil, fmt.Errorf("%w: messaging private key is %d bytes, want exactly %d", errNotCanonicalAck, len(priv), MessagingPrivateKeySize)
	}
	b, err := canonicalizeAck(a)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, b), nil
}

// ackAttestationEnvelope wraps the detached signature on the wire, mirroring
// internal/relay.AckAttestationEnvelope. It is an OBJECT rather than a bare
// field because §6.3 anticipates a key identifier joining it once something
// distributes messaging public keys, and growing an object is a smaller change
// than replacing a scalar.
type ackAttestationEnvelope struct {
	// Signature is the detached Ed25519 signature, exactly SignatureSize (64)
	// bytes. encoding/json carries a []byte as standard base64, which is the
	// wire form the bus decodes.
	Signature []byte `json:"signature"`
}

// ackRequestBody mirrors internal/relay.PeerAckRequest, the frame POST /v1/ack
// decodes. Field names and `omitempty` are the WIRE CONTRACT and do not change.
type ackRequestBody struct {
	ProtocolVersion    int                     `json:"protocol_version,omitempty"`
	CorrelationKey     string                  `json:"correlation_key"`
	Recipient          string                  `json:"recipient"`
	Outcome            string                  `json:"outcome"`
	Class              string                  `json:"class,omitempty"`
	EmittedAtUnixMilli int64                   `json:"emitted_at"`
	Attestation        *ackAttestationEnvelope `json:"attestation,omitempty"`
}

// AckOptions is the input to Ack.
type AckOptions struct {
	// CorrelationKey identifies the message being acknowledged: the ORIGIN
	// bus's server-minted "<bus-id>-<seq>" (invariant 1), as
	// Message.CorrelationKey carries it.
	//
	// NOT Message.MessageID. That is the id the LOCAL bus minted, and for a
	// message that arrived over a relay hop it is a different string that the
	// ack path refuses. The two are equal only when the local bus is the origin.
	//
	// NOT Message.Seq and NOT a delivery position. Seq is identity, Pos is
	// position, this is correlation, and confusing them has caused three
	// defects here.
	CorrelationKey string

	// Outcome is AckDelivered or AckRefused. IT IS REQUIRED AND HAS NO DEFAULT.
	//
	// A zero-value AckOptions must not be able to assert receipt: "delivered" is
	// a claim only the recipient application can make (ACK-1 — inbox delivery is
	// NOT receipt), so a library that guessed it for an uninitialised struct
	// would be asserting it on the caller's behalf. The CLI supplies the default
	// where a human or an agent asked for one explicitly.
	Outcome string

	// Class is one of RecipientRefusalClasses, required IFF Outcome is
	// AckRefused and forbidden otherwise.
	Class string
}

// AckResult is the bus's answer to POST /v1/ack. The json tags are the wire
// shape (internal/httpapi.AckResponseBody) and are also this type's --json
// contract, so they do not change.
type AckResult struct {
	// Accepted reports that the bus recorded this outcome, or had already
	// recorded exactly this outcome. FALSE with State AckUnknown is the uniform
	// §13.3 answer — see Ack.
	Accepted bool `json:"accepted"`

	// Duplicate reports invariant 10's FIRST case: this (key, recipient) was
	// ALREADY terminal with the SAME outcome, so the original result stands,
	// nothing was re-applied, and nobody was disconnected. It is a SUCCESS.
	Duplicate bool `json:"duplicate"`

	// State is the state that now STANDS for the pair — on a duplicate, the
	// ORIGINAL one — or AckUnknown when the bus retains nothing for it.
	State string `json:"state"`

	// Class is the recorded class, present only on a negative terminal.
	Class string `json:"class,omitempty"`
}

// Ack acknowledges ONE message this agent received: the RECIPIENT half of the
// delivery-acknowledgement plane (ACK-CONTRACT.md §13.4, invariant 7).
//
// # AN ACKNOWLEDGEMENT IS AN APPLICATION STATEMENT, NOT A TRANSPORT EVENT
//
// Reading a message off the bus is NOT receipt and does not settle anything.
// ACK-1 ruled that an EXPLICIT application acknowledgement is required, because
// the bus does not verify signatures and an auto-ACK would have the bus assert,
// on the recipient's behalf, a fact only the recipient can establish. This call
// is that statement. Make it when your application has actually taken
// responsibility for the message.
//
// # WHAT THE SIGNATURE IS WORTH TODAY — SAY THIS PLAINLY, DO NOT OVERSELL IT
//
// The frame carries a detached signature over the canonical bytes above, made
// with this agent's messaging key. EVERY BUS CHECKS ITS SHAPE ONLY — present,
// exactly 64 bytes — and NO BUS VERIFIES IT OR MAY CLAIM TO. Nothing carries a
// recipient's messaging public key back UPSTREAM to a sender either (no route
// distributes it, §16 Q1), so today this signature is END TO END UNVERIFIABLE
// BY ANYONE, including the sender who reads it back through AckStatus as the
// label `recipient_signature_unverified`. It is signed anyway so that the
// binding exists in the durable record from day one, for the day something can
// verify it; do not present it to a third party as proof of receipt.
//
// # TERMINAL IS ABSORBING, AND A RETRY IS NOT A CONFLICT
//
// The first terminal outcome for a (key, recipient) pair stands forever. Sending
// the SAME outcome again is a legitimate retry: the bus answers Accepted with
// Duplicate true, re-applies nothing, and does not disconnect anybody
// (invariant 10). Sending a DIFFERENT outcome for a pair already settled is a
// protocol violation: it is refused with KindRejected and NOTHING is written —
// re-sending can never make it succeed.
//
// # `unknown` IS FOUR ANSWERS AT ONCE
//
// Accepted false with State AckUnknown means the bus retains nothing for this
// (key, recipient): the key never existed, it was swept past the retention
// window, this agent was not addressed in that message, or the key is
// malformed. The bus answers all four identically and on purpose — telling them
// apart would confirm to anyone who guessed a key that the message exists
// (§13.3). It is NOT an error and it is NOT a permission failure; do not write
// code that tries to narrow it.
func (c *Client) Ack(ctx context.Context, opts AckOptions) (AckResult, error) {
	const op = "ack"

	key := opts.CorrelationKey
	switch {
	case key == "":
		return AckResult{}, usagef(op, "pass the message_id of the message you are acknowledging",
			"a correlation key is required")
	case len(key) > MaxCorrelationKeyLen:
		return AckResult{}, usagef(op, "pass the message_id of the message you are acknowledging, e.g. bus-abc123-42",
			"correlation key is %d bytes, the limit is %d", len(key), MaxCorrelationKeyLen)
	case strings.ContainsAny(key, " \t\r\n"):
		// Refused locally because whitespace in a key is always a shell or
		// copy-paste accident. This judges only the CALLER'S OWN input and
		// discloses nothing about the bus.
		return AckResult{}, usagef(op, "remove the whitespace from the correlation key",
			"a correlation key contains no whitespace")
	}

	switch opts.Outcome {
	case AckDelivered:
		if opts.Class != "" {
			return AckResult{}, usagef(op, "drop the class, or acknowledge with outcome "+AckRefused+" if you are refusing the message",
				"outcome %q carries no class: a positive acknowledgement has nothing to explain (§5.4)", opts.Outcome)
		}
	case AckRefused:
		switch opts.Class {
		case AckClassRecipientRefusedPolicy, AckClassRecipientRefusedUndecodable, AckClassRecipientRefusedNotAddressed:
		case "":
			return AckResult{}, usagef(op, "name one of: "+strings.Join(RecipientRefusalClasses(), ", "),
				"outcome %q requires a class; an unexplained refusal is not signable", opts.Outcome)
		default:
			return AckResult{}, usagef(op, "name one of: "+strings.Join(RecipientRefusalClasses(), ", "),
				"class %q is not one a recipient may emit", safeText(opts.Class, 40))
		}
	case AckUndeliverable:
		// NAMED SEPARATELY FROM THE GENERIC REFUSAL BELOW, and it is not a
		// tidier way of spelling the same check.
		//
		// `undeliverable` is a routing claim: it says a BUS will never deliver
		// this message, which is a statement about a federation the recipient
		// cannot see. A recipient asserting it would be signing on the bus's
		// behalf. canonicalizeAck refuses it too, and the bus refuses it a third
		// time on the agent surface (relay.AckSurfaceAgent) — three layers,
		// because the failure this prevents is a durable, ABSORBING terminal
		// written from a claim nobody was entitled to make.
		return AckResult{}, usagef(op, "if you are refusing the message, use outcome "+AckRefused+" with one of: "+strings.Join(RecipientRefusalClasses(), ", "),
			"a recipient may not assert %q: that is a routing claim a BUS makes about its own federation, never a statement about receipt", AckUndeliverable)
	case "":
		return AckResult{}, usagef(op, "set Outcome to "+AckDelivered+" or "+AckRefused,
			"an outcome is required and is never defaulted: %q is a claim only the recipient application can make", AckDelivered)
	default:
		return AckResult{}, usagef(op, "set Outcome to "+AckDelivered+" or "+AckRefused,
			"outcome %q is not one a recipient may acknowledge with", safeText(opts.Outcome, 40))
	}

	cred, err := c.credential()
	if err != nil {
		return AckResult{}, err
	}
	priv, err := c.messagingKey()
	if err != nil {
		return AckResult{}, err
	}

	// Milliseconds UTC, and THE SAME INTEGER travels on the wire below — there
	// is deliberately no conversion between the signed form and the frame,
	// because every conversion is a place the two can drift.
	emitted := c.now().UTC().UnixNano() / int64(time.Millisecond)

	// The recipient inside the signed bytes is THIS AGENT'S OWN id, taken from
	// the credential and never from a caller-supplied field. The bus compares
	// the frame's recipient against the authenticated principal and refuses a
	// mismatch (403), so there is no useful way to fill this in wrongly — but
	// the reason it is right is that a recipient may only speak for itself.
	sig, err := signAck(priv, ackSigned{
		CorrelationKey:     key,
		Recipient:          cred.AgentID,
		Outcome:            opts.Outcome,
		Class:              opts.Class,
		EmittedAtUnixMilli: emitted,
	})
	if err != nil {
		// Canonicalization failed closed: there are no bytes, so there is
		// nothing to sign and nothing was sent. A local fault — a malformed
		// message id, usually — reported as usage rather than as a bus failure.
		return AckResult{}, usagef(op, "check the message id you are acknowledging; it is the `message_id` the bus minted, e.g. bus-abc123-42",
			"this acknowledgement cannot be canonicalized for signing: %s", err)
	}

	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	var out AckResult
	if _, err := c.authorizedRequest(ctx, request{
		method: http.MethodPost,
		path:   routeAck,
		op:     op,
		body: ackRequestBody{
			ProtocolVersion:    ackWireProtocolVersion,
			CorrelationKey:     key,
			Recipient:          cred.AgentID,
			Outcome:            opts.Outcome,
			Class:              opts.Class,
			EmittedAtUnixMilli: emitted,
			Attestation:        &ackAttestationEnvelope{Signature: sig},
		},
		out: &out,
		// SAFE TO REPEAT, and this is not the usual idempotency-key argument.
		//
		// This frame carries no idempotency key by design (§4): an ACK's
		// idempotency is the durable (correlation key, recipient) row and the
		// ABSORBING-TERMINAL rule over it. do() marshals the body once and
		// replays those exact bytes, so every attempt of one Ack asserts the
		// SAME outcome for the SAME pair — which the bus answers as
		// Duplicate rather than as a conflict (invariant 10's first case).
		retryable: true,
	}); err != nil {
		return AckResult{}, annotateAckConflict(err)
	}
	if err := validateAckResult(op, &out); err != nil {
		return AckResult{}, err
	}
	return out, nil
}

// annotateAckConflict replaces the transport's generic 409 remedy — which talks
// about idempotency keys — with the one that is true on this route.
//
// The frame carries NO idempotency key, so "use a fresh key" is advice a caller
// cannot act on: it would send them looking for a header that does not exist.
// The real condition is invariant 10's SECOND case, and the real remedy is that
// there isn't one — the first terminal stands and nothing was written.
func annotateAckConflict(err error) error {
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict {
		return err
	}
	annotated := *e
	annotated.Message = "this delivery outcome is already terminal with a DIFFERENT outcome; the first terminal stands and nothing was written"
	annotated.Remedy = "do not retry: a terminal acknowledgement is absorbing and can never be revisited, reopened or downgraded (§8.1). Check what you already acknowledged with `agent-busctl ack-status`"
	return &annotated
}

// validateAckResult checks everything that will be printed or branched on. See
// sanitize.go for why the bus is not trusted to produce safe text.
//
// An UNRECOGNISED STATE IS AN ERROR, never a default: the set is closed by
// contract, and a client that passed a typo through would let a caller
// branching on `== "delivered"` silently take the wrong branch forever.
func validateAckResult(op string, r *AckResult) error {
	if r.State == "" {
		return newError(KindServer, op, "the bus accepted the acknowledgement without saying what state now stands",
			"retry, and report this to the bus operator: the route always answers with a state")
	}
	if _, ok := ackStates[r.State]; !ok {
		return newError(KindServer, op, "the bus reported a delivery state this client does not know",
			"upgrade agent-busctl; the delivery state set is closed and an unrecognised spelling is refused rather than guessed at")
	}
	if r.Class != "" {
		if err := validateServerField(op, "class", r.Class); err != nil {
			return err
		}
	}
	return nil
}
