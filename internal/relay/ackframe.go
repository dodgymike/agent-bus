package relay

import (
	"errors"
	"fmt"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// THE PEER-HOP ACK/NACK WIRE FRAME (ACK-3).
//
// This file is the WIRE HALF of the delivery-acknowledgement plane specified in
// ACK-CONTRACT.md §9. It owns the frame's SHAPE, its VERSION, its byte bound and
// its decode; ackhttp.go owns the route that authenticates and correlates it.
//
// It owns NOTHING ELSE, and in particular it re-declares no vocabulary:
// AckOutcome, AckClass, AckAttestation, AckSurface, their parsers and
// ValidateAckClassForOutcome / ValidateAckAttestation all come from ack.go
// (ACK-4). This file is the frame those rules are applied TO.
//
// # THE ONE FIELD THAT IS NOT HERE, AND WHY ITS ABSENCE IS THE SECURITY PROPERTY
//
// THERE IS NO PEER-BUS FIELD ON THIS FRAME, AND ONE MUST NEVER BE ADDED.
//
// AuthorizePeerAck's binding rule (§6.2) is only as strong as the peer id handed
// to it: it authorises `DeriveJobID(peerBusID, correlationKey)`, so a frame-
// supplied peerBusID would authorise THE NAME A PEER CHOSE rather than the peer
// that sent the frame — and every guarantee in ack.go would evaporate while
// every positive test still passed. ack.go states that as a caller precondition
// (ack.go, "TWO PRECONDITIONS THE CALLER OWNS"); this file makes it STRUCTURAL,
// in the only way a wire format can:
//
//   - the frame HAS NO FIELD a bus id could be read out of, so there is nothing
//     for a handler to read even by mistake; and
//   - the decoder uses decodeStrict, which sets DisallowUnknownFields, so a peer
//     that INVENTS one is refused 400 rather than having it silently ignored.
//
// The authenticated peer id enters at ackhttp.go's AckHandler.ServeAuthenticated,
// as a Go PARAMETER supplied by internal/httpapi/peermount.go from
// PeerPrincipal.BusID — the bus id RequirePeerPrincipal resolved from the TLS
// CLIENT CERTIFICATE. That is why AckHandler is deliberately NOT an http.Handler:
// a type with no ServeHTTP cannot be mounted without somebody supplying the
// parameter, so "forgot to pass the principal" is a compile error rather than a
// silent forgery hole.
//
// # THE RECIPIENT IS ON THE FRAME AND THAT IS NOT THE SAME THING
//
// `recipient` names WHICH (key, recipient) row is being settled, and it is
// UNTRUSTED INPUT TO BE VALIDATED, never an identity to be trusted (invariant
// 1). It is bounded to a well-formed fully-qualified agent id (invariant 2) and
// then checked against a row this bus itself created for a recipient the SENDER
// NAMED. A peer naming an agent that was never addressed finds no row and gets
// the uniform refusal. See ack.go, "IT DOES NOT BIND THE RECIPIENT".

// PeerAckPath is the route this frame is POSTed to. It is a peer-surface path
// and, like the other three, it may be registered ONLY through
// internal/httpapi/peermount.go — internal/relay/guards_test.go enforces that,
// and peerRoutePathIdents there carries this constant's name for the same
// reason it carries the others.
const PeerAckPath = "/v1/peer/ack"

// ---------------------------------------------------------------------------
// Versioning (§10)
// ---------------------------------------------------------------------------

// AckWireVersion is the wire-protocol version of the ACK frame below.
//
// IT SPENDS THE ALREADY-RESERVED `relay-wire-version` = 1 AND RESERVES NOTHING.
// Value 1 was allocated through the Spec Server `relay-wire-version` namespace
// on 2026-08-08 ("FEDERATION phase, RELAY-23 will spend this"). §10's ruling is
// that the ACK frame and the relay envelope spend THE SAME reserved value —
// they are two frames of one peer protocol, versioned together — so this
// constant is that value and NOT a second allocation. Nobody picks the next one
// by reading this line: bump it only against a fresh reservation.
//
// # WHY THIS IS A SEPARATE CONSTANT FROM THE RELAY ENVELOPE'S, TODAY ONLY
//
// RELAY-23 adds `RelayRequest.ProtocolVersion` and a `relay.WireVersion`
// constant to message.go. At the time this file was written THAT WORK WAS
// UNMERGED — it lives on branch worktree-agent-a3b41d07f84017fc1 and is not in
// HEAD — so declaring `WireVersion` here would have collided with it in a way
// git CANNOT flag: two files in one package, no textual conflict, and a package
// that stops compiling only after the merge. An ACK-scoped spelling merges
// cleanly and compiles, and the duplication is then visible and collapsible.
//
// THIS IS A SEQUENCING DECISION, NOT A CLAIM THAT TWO CONSTANTS ARE ACCEPTABLE.
// It is the same call ack.go made and recorded for the class vocabulary, for the
// same reason. A follow-up must collapse this onto relay.WireVersion once
// RELAY-23 lands; the RULES below (absent reads as 1, unrecognised is refused)
// are what this file owns and they stay whichever constant survives.
const AckWireVersion = 1

// ackWireVersionAbsent is what a frame carrying NO protocol_version decodes to,
// because encoding/json leaves an absent int at its zero value and this frame
// has no way to tell "absent" from "explicitly 0" without a pointer.
//
// The two are treated identically ON PURPOSE: nobody may transmit 0, so the only
// producer of a 0 here is a frame written before the field existed.
const ackWireVersionAbsent = 0

// ErrUnsupportedAckVersion reports an ACK frame whose declared wire-protocol
// version this bus does not implement.
//
// It is its own sentinel rather than an ErrInvalidAckFrame because the two are
// different OPERATOR problems with different remedies, and the error TEXT never
// crosses the wire — failJSON puts only the stable code in the body. Folded
// together, a peer's operator would read `invalid_request` and go hunting a
// malformed field it does not have, when the real remedy is to upgrade one of
// the two buses. That is the misdiagnosis ErrorCode's own comment warns about
// for the signature codes.
//
// IT IS STILL A 400, NOT A 503. A retry cannot change the verdict: the frame is
// well formed and we do not speak its format, and no amount of resending
// installs a new binary at either end. See resolveAckWireVersion.
var ErrUnsupportedAckVersion = errors.New("relay: unsupported acknowledgement wire-protocol version")

// resolveAckWireVersion maps a frame's declared version onto the version this
// bus will interpret it as, or refuses it.
//
// # AN UNRECOGNISED VERSION IS REJECTED, NEVER DEFAULTED
//
// parseOutboxState's posture exactly (outbox.go:316-330) and for the same
// reason: guessing turns a corrupt or FUTURE-FORMAT frame into a
// plausible-looking valid one. The stakes are higher here than for an outbox
// row, because this frame carries a TERMINAL outcome and terminal is ABSORBING
// (§8.1) — a v2 frame read under v1's rules could write a `delivered` or a
// `refused` that can never afterwards be corrected, from fields whose meaning we
// did not know. Fail closed and make an operator upgrade one of the two buses;
// that remedy is reachable and a wrongly-settled terminal is not.
//
// # AN ABSENT VERSION READS AS 1, AND THE 1 BELOW IS A LITERAL ON PURPOSE
//
// This is the only backward-compatible read and it is exact: version 1 IS the
// format being introduced here, so a frame that omits the field is a version-1
// frame by definition.
//
// THE LITERAL 1 MUST NOT BE RESPELLED AS AckWireVersion. When the version is
// bumped to 2 against a fresh reservation, a versionless frame is STILL a v1
// frame — it was encoded by a binary that had never heard of v2 — and spelling
// this as the constant would silently reinterpret every legacy frame as the new
// format on the day of the bump. That is the same defect as defaulting an
// unrecognised version, arriving by a different door. RELAY-23 states the
// identical rule for the relay envelope; this is not a coincidence, it is the
// same rule applied to the second frame of the same protocol.
func resolveAckWireVersion(declared int) (int, error) {
	switch declared {
	case ackWireVersionAbsent, 1:
		return 1, nil
	default:
		return 0, fmt.Errorf("%w: the frame declares acknowledgement wire-protocol version %d and this bus speaks version %d; a version this bus does not implement is refused rather than guessed at, because the fields of an unknown format have unknown meaning and reading a TERMINAL outcome under the wrong rules would durably settle a message in a way that can never be revisited",
			ErrUnsupportedAckVersion, declared, AckWireVersion)
	}
}

// ---------------------------------------------------------------------------
// The frame (§9.2)
// ---------------------------------------------------------------------------

// MaxAckBytes bounds the encoded ACK frame read from the network, before it is
// decoded.
//
// It is DERIVED, not guessed, exactly as MaxRelayBytes and MaxHandshakeBytes
// are — and here the derivation is EXACT rather than generous, because §5.3
// fixes that NO FIELD'S LENGTH IS CHOSEN BY A REMOTE PARTY. Every field is a
// server-minted id with a maximum, a closed enum, an integer, or a
// fixed-size signature:
//
//	protocol_version  int, key + value                                =   25
//	correlation_key   ids.MaxMessageIDLen (85) + key + quotes         =  105
//	recipient         ids.MaxAgentIDLen (150) + key + quotes          =  165
//	outcome           longest spelling "undeliverable" (13) + key     =   35
//	class             longest "recipient_refused_not_addressed" (31)  =   45
//	emitted_at        int64 (20 digits) + key                         =   35
//	attestation       64 raw bytes base64-expanded 4/3 and padded
//	                  (88) + the two key names and braces             =  120
//	braces, commas, whitespace                                        =   30
//	                                                           total  ~  560
//
// 4 KiB leaves ~7x headroom, so a legal maximum-size ACK frame can always be
// encoded and can never be rejected by this cap, while an unbounded stream still
// stops at four kilobytes. It is three orders of magnitude below MaxRelayBytes
// BECAUSE it has no body and no recipient list — sharing the relay's quarter-
// megabyte cap would let an ACK-shaped stream cost 64x what an ACK can legally
// cost. TestMaxAckBytesFitsAMaximumFrame pins the derivation, because a bound
// nothing checks is a description.
const MaxAckBytes = 4 << 10

// PeerAckRequest is the body one bus POSTs to another at PeerAckPath: ONE
// terminal delivery outcome for ONE (correlation key, recipient) pair.
//
// EVERY FIELD IS UNTRUSTED. Nothing in this struct proves anything; the peer's
// identity comes from the TLS client certificate and never from here, and there
// is deliberately no field it could come from. See the file header.
//
// # WHY THERE IS NO IDEMPOTENCY-KEY HEADER, UNLIKE THE RELAY ENVELOPE
//
// PeerRelayPath carries idem.HeaderName because its key — the origin message id
// — is what internal/idem stores an APPLIED-KEY entry under. An ACK creates no
// applied-key entry: its idempotency is the ACK record's own (correlation key,
// recipient) row and the ABSORBING-TERMINAL rule over it (§8.2 notes 2 and 4),
// which is durable in the ack store rather than in the idem table. A second key
// naming the same fact would be a second answer to "have I applied this?" that
// could drift from the durable one — the exact defect relayhttp.go's
// AcceptRelay doc refuses for the same reason.
type PeerAckRequest struct {
	// ProtocolVersion is the ACK wire-protocol version of this frame. It is
	// FIRST because it governs the reading of every field below it, and it is
	// resolved before any of them is looked at.
	//
	// THE JSON KEY IS "protocol_version" AND IT MUST NEVER BE "version".
	// RosterUpdate already owns the key "version" on a neighbouring peer
	// envelope and that one is NOT a protocol version — it is the peer's
	// monotonic ROSTER EPOCH. Two meanings on one key is how a peer ends up
	// applying a roster epoch as a format number. RELAY-23 pins the same
	// spelling on the relay envelope; the two frames of one protocol must not
	// disagree about the name of their version field.
	//
	// ACK-CONTRACT.md §9.2 sketches this field as "wire_version". THE
	// CONTRACT'S SKETCH IS SUPERSEDED HERE, deliberately and with the reason
	// recorded: §10's ruling is that this frame and the relay envelope spend ONE
	// reserved version, and a single version spelled two different ways on two
	// frames of the same protocol is how a future negotiation task (ACK-10) ends
	// up writing two parsers. The spelling that wins is the relay envelope's,
	// because that frame is older and already has a task landing it.
	//
	// It is `omitempty` so the zero value is ABSENT rather than an explicit 0:
	// 0 is not a version anyone may transmit, it is only ever the shape of a
	// frame written before this field existed (see ackWireVersionAbsent).
	// Egress always sets it — Client.PeerAck writes AckWireVersion — so every
	// frame this bus emits carries it.
	ProtocolVersion int `json:"protocol_version,omitempty"`

	// CorrelationKey is THE correlation key (§3): the ORIGIN bus's
	// server-minted message id, `<origin-bus>-<seq>`, reached on the sending
	// side through store.Message.OriginID().
	//
	// IT IS NOT A FOURTH IDENTIFIER. It is the same value the relay layer
	// already uses as the wire idempotency key and stores as
	// OutboxRecord.OriginMessageID, which is exactly what lets
	// AuthorizePeerAck bind this frame to an obligation this bus durably wrote
	// with no new index. Seq, Pos and OriginMessageID have produced three
	// defects in this repository; this frame carries the third one and neither
	// of the others, and it takes part in NO ordering, NO cursor and NO
	// retention decision here either.
	CorrelationKey string `json:"correlation_key"`

	// Recipient is the fully-qualified `<bus-id>.<agent-id>` (invariant 2) whose
	// row is being settled. The ACK record is keyed on (correlation key,
	// recipient) from day one (§3.2), never on the key alone, which is what lets
	// a directed message to N recipients settle independently.
	Recipient string `json:"recipient"`

	// Outcome is the terminal outcome: "delivered", "refused" or
	// "undeliverable". Parsed by ParseAckOutcome, which REFUSES an unrecognised
	// spelling rather than defaulting it.
	//
	// The two NON-terminal states (accepted, in_flight) are deliberately not
	// carriable: they are facts about a bus's own bookkeeping, not about the
	// recipient, and "unknown" is a REPORTING value that must never travel — an
	// "I don't know" on the wire is how a real terminal outcome gets overwritten
	// by ignorance (§8.1).
	Outcome string `json:"outcome"`

	// Class is the closed NACK class (§5.2), set IFF Outcome is a negative
	// terminal and forbidden otherwise, validated in both directions by
	// ValidateAckClassForOutcome.
	//
	// THERE IS NO FREE-TEXT REASON FIELD ON THIS FRAME AND ONE MUST NOT BE
	// ADDED. Invariant 6: the durable trail records metadata and routing only,
	// and a reason string sourced from a peer or a recipient is a body by
	// another name. A class is a compile-time constant chosen by the code of the
	// bus that emits it — never assembled, never templated, never concatenated —
	// and there is deliberately no adjacent string field to put "the detail" in.
	Class string `json:"class,omitempty"`

	// EmittedAtUnixMilli is the emitting party's wall clock in Unix
	// milliseconds. It is PROVENANCE FOR THE OPERATOR LOG AND NOTHING ELSE.
	//
	// IT IS UNTRUSTED PEER INPUT AND IT IS NEVER PERSISTED. The durable record's
	// AcceptedAt and SettledAt are THIS bus's clock (ack.Store stamps them), for
	// the reason RelayedMessage.TimestampUnixMilli gives at length: a peer that
	// could choose a stored timestamp could choose when a row falls out of
	// §11's retention window, which is an authorization-shaped power handed to
	// the party being authorised.
	//
	// IT IS ALSO NOT PART OF THE IDEMPOTENCY COMPARISON. AckTerminal carries
	// the outcome and the class and deliberately not this, because a peer
	// re-sending the same settlement one second later is a RETRY (invariant
	// 10's first case) and comparing on the clock would turn every honest retry
	// into a protocol violation.
	//
	// It is REQUIRED and must be positive: §9.2 lists it, so a frame without one
	// is malformed rather than tolerated. Being an int64 bounds it by
	// construction — there is no length here for a remote party to choose.
	EmittedAtUnixMilli int64 `json:"emitted_at"`

	// Attestation is the recipient's detached signature over canonical ACK
	// bytes, present IFF the outcome is recipient-sourced (delivered, refused)
	// and forbidden on a bus-sourced one (undeliverable).
	//
	// EVERY BUS TREATS IT AS OPAQUE BYTES AND CHECKS SHAPE ONLY — present, and
	// exactly signing.SignatureSize bytes — byte-for-byte the posture already
	// taken for message signatures. NO BUS VERIFIES IT AND NO BUS MAY CLAIM TO:
	// no endpoint distributes agents' messaging public keys, so it is
	// end-to-end unverifiable by anybody today, including the sender (§16 Q1).
	// That is why ValidateAckAttestation's positive answer is labelled
	// `recipient_signature_unverified` and why no value meaning "verified"
	// exists.
	//
	// It is a POINTER so that "absent" and "present but empty" are
	// distinguishable: an `undeliverable` frame must carry no attestation at
	// all, and a frame carrying `"attestation":{"signature":""}` is asserting
	// something different from one that omits it. Collapsing the two would make
	// the "bus-sourced outcomes carry none" rule unenforceable.
	Attestation *AckAttestationEnvelope `json:"attestation,omitempty"`
}

// AckAttestationEnvelope wraps the detached signature.
//
// It is a nested object rather than a bare field because §6.3 anticipates a key
// identifier joining it once something distributes messaging public keys (§16
// Q1), and growing an object is a smaller change than replacing a scalar. It has
// exactly one field today and decodeStrict refuses any other.
type AckAttestationEnvelope struct {
	// Signature is the detached Ed25519 signature, exactly
	// signing.SignatureSize (64) bytes. encoding/json carries a []byte as
	// base64, the same way RelayRequest.Signature travels.
	Signature []byte `json:"signature"`
}

// PeerAckResponse is the answer to an ACK POST (§9.3).
//
// It is deliberately as small as it is: an ACK settles a fact this bus already
// knows the shape of, so there is no id to mint and hand back. Accepted is
// always true on a 200 — there is no ACK analogue of the relay's
// `accepted:false, dropped_reason:loop`, because an ACK that cannot be applied
// is refused with a status rather than settled negatively.
type PeerAckResponse struct {
	// Accepted reports that this bus recorded the outcome, or had already
	// recorded exactly this outcome.
	Accepted bool `json:"accepted"`

	// Duplicate reports invariant 10's FIRST case: this (key, recipient) was
	// already terminal with the SAME outcome and class, so the ORIGINAL result
	// stands and NOTHING was re-applied. Nobody is disconnected (§12).
	Duplicate bool `json:"duplicate"`
}

// ---------------------------------------------------------------------------
// Decode + validate
// ---------------------------------------------------------------------------

// ValidatedPeerAck is the VALIDATED, still-UNAUTHORIZED content of an ACK frame:
// what this bus is entitled to believe about the BYTES a peer sent, before it
// has asked whether that peer is entitled to say it.
//
// The split matters. Producing one of these means the frame parses, its version
// is one we speak, its outcome and class are members of the closed sets and
// agree with each other, and its attestation has the shape the outcome
// requires. It means NOTHING about whether this bus ever owed this peer an
// obligation for this key — that is AuthorizePeerAck's answer and it is taken
// separately, at the route, from the AUTHENTICATED peer id.
//
// IT CARRIES NO PEER BUS ID, for the reason the file header gives. The route
// adds the authenticated one; there is no field here for a frame-supplied one to
// arrive in.
type ValidatedPeerAck struct {
	// ProtocolVersion is the RESOLVED version: what resolveAckWireVersion made
	// of the declared one. It is never 0 on a value this package returns — an
	// absent declaration resolves to 1, and an unrecognised one never produces a
	// ValidatedPeerAck at all. It is PROVENANCE, worth logging during a
	// rollout, and it is not an input to any decision below.
	ProtocolVersion int

	// CorrelationKey is the validated correlation key: a well-formed message id
	// within ids.MaxMessageIDLen.
	CorrelationKey string

	// Recipient is the validated fully-qualified recipient (invariant 2).
	Recipient string

	// Outcome is the parsed terminal outcome.
	Outcome AckOutcome

	// Class is the parsed class, or 0 for a positive terminal.
	Class AckClass

	// Attestation is the LABEL ValidateAckAttestation returned — what kind of
	// authentication stands behind this outcome. There is no value meaning
	// "verified" and one must not be added.
	Attestation AckAttestation

	// Signature is the attestation bytes, shape-checked and NEVER verified. It
	// is nil for a bus-sourced outcome.
	Signature []byte

	// EmittedAtUnixMilli is the emitting party's clock. PROVENANCE ONLY — never
	// persisted, never compared, never an input to a decision. See the field it
	// came from.
	EmittedAtUnixMilli int64
}

// Terminal reduces the frame to the pair two acknowledgements are COMPARED on
// (ack.go's AckTerminal): the outcome and the class, and deliberately not the
// timestamp, not the attestation label and not the signature bytes.
//
// A peer re-sending an identical settlement a second later, or re-signing it,
// is invariant 10's LEGITIMATE RETRY. Comparing on anything that can differ
// between two honest sends of the same fact would turn that retry into a
// protocol violation and answer 409 to a peer doing exactly the right thing.
func (v ValidatedPeerAck) Terminal() AckTerminal {
	return AckTerminal{Outcome: v.Outcome, Class: v.Class}
}

// ValidatePeerAckRequest decodes the CONTENT of an ACK frame and applies every
// rule that is decidable from the bytes alone.
//
// surface names which authenticated gate the frame arrived through and is
// supplied by the CALLER — never read from the frame — because the attestation
// label cannot be derived from a frame's contents (see AckSurface). Passing
// AckSurfacePeer for something that did not arrive behind RequirePeerPrincipal
// would have this bus record, durably, that an adjacent BUS vouched for
// something it never said.
//
// # THE ORDER OF THE CHECKS IS THE DESIGN
//
//  0. THE WIRE VERSION, before any field it governs is read. The fields of a
//     format we do not implement have meanings we do not know, so validating
//     them under v1's rules would be validating the wrong question. It costs one
//     integer comparison.
//
//  1. THE OUTCOME AND CLASS, together, through ValidateAckClassForOutcome —
//     which bounds-checks the outcome FIRST, so a zero or out-of-range value
//     cannot fall through the positive arm and be accepted as `delivered`.
//     That half-set check is anti-forgery, not tidiness: without it a peer
//     sends outcome=refused with class=no_route and this bus records ITS OWN
//     routing failure as THE RECIPIENT'S DECISION.
//
//  2. THE ATTESTATION SHAPE, which also enforces that an AGENT may not assert a
//     routing outcome. Shape only, never verification.
//
// The IDS are NOT validated here, and that is not an omission: AuthorizePeerAck
// validates the correlation key and the recipient itself, in the same call that
// binds them, so validating them here as well would put two spellings of one
// rule in two files — which is how the two come to disagree. What this function
// guarantees about them is only that they were carried; the route must not use
// them before AuthorizePeerAck has returned.
func ValidatePeerAckRequest(req PeerAckRequest, surface AckSurface) (ValidatedPeerAck, error) {
	// 0. The version, before anything it governs.
	version, err := resolveAckWireVersion(req.ProtocolVersion)
	if err != nil {
		return ValidatedPeerAck{}, err
	}

	// 1. The outcome and its class, in both directions.
	outcome, err := ParseAckOutcome(req.Outcome)
	if err != nil {
		return ValidatedPeerAck{}, err
	}
	var class AckClass
	if req.Class != "" {
		if class, err = ParseAckClass(req.Class); err != nil {
			return ValidatedPeerAck{}, err
		}
	}
	if err := ValidateAckClassForOutcome(outcome, class); err != nil {
		return ValidatedPeerAck{}, err
	}

	// 2. The attestation's SHAPE, and which outcomes this surface may assert.
	//
	// A nil envelope and an envelope holding no bytes are handed to
	// ValidateAckAttestation identically — as a zero-length signature — because
	// from the RULE's point of view they say the same thing: no attestation was
	// supplied. The pointer exists so that `{"signature":""}` on an
	// `undeliverable` frame is refused as loudly as a 64-byte one would be,
	// rather than being silently indistinguishable from omitting the field.
	var signature []byte
	if req.Attestation != nil {
		signature = req.Attestation.Signature
		if len(signature) == 0 {
			return ValidatedPeerAck{}, fmt.Errorf("%w: an attestation object was supplied with no signature in it; omit the field entirely, or carry exactly %d bytes",
				ErrInvalidAckFrame, signing.SignatureSize)
		}
	}
	attestation, err := ValidateAckAttestation(surface, outcome, signature)
	if err != nil {
		return ValidatedPeerAck{}, err
	}

	// 3. The emitted-at clock. REQUIRED and positive: §9.2 lists it, so a frame
	//    without one is malformed rather than tolerated, and a zero is exactly
	//    what a struct nobody populated produces. It is refused here and USED
	//    NOWHERE — provenance for the log line and nothing else.
	if req.EmittedAtUnixMilli <= 0 {
		return ValidatedPeerAck{}, fmt.Errorf("%w: emitted_at is %d; it is required and must be a positive Unix-millisecond clock reading (it is PROVENANCE ONLY and is never persisted, but a frame that omits it is malformed)",
			ErrInvalidAckFrame, req.EmittedAtUnixMilli)
	}

	return ValidatedPeerAck{
		ProtocolVersion:    version,
		CorrelationKey:     req.CorrelationKey,
		Recipient:          req.Recipient,
		Outcome:            outcome,
		Class:              class,
		Attestation:        attestation,
		Signature:          signature,
		EmittedAtUnixMilli: req.EmittedAtUnixMilli,
	}, nil
}

// widestLegalAckFrame builds the widest frame this route will ever accept, so
// TestMaxAckBytesFitsAMaximumFrame can pin the derivation in the comment on
// MaxAckBytes against the REAL ENCODER rather than against arithmetic somebody
// did once.
//
// It is a function rather than a test helper so that the widest-legal-frame
// CONSTRUCTION lives beside the fields it enumerates: a field added to
// PeerAckRequest without a line here makes the bound a description again, and
// the test that calls this is the thing that notices.
func widestLegalAckFrame() PeerAckRequest {
	// IT MUST ACTUALLY BE LEGAL, and an earlier version was not: it paired
	// `undeliverable` with a RECIPIENT class and an attestation, which
	// ValidatePeerAckRequest refuses in two different ways. The bound still held
	// (the illegal frame was WIDER), but a fixture the validator rejects cannot
	// witness "the widest frame this route will ever ACCEPT" — it witnesses a
	// frame the route never sees. TestMaxAckBytesFitsAMaximumFrame now runs it
	// through the real validator, so this can never drift back.
	//
	// `refused` is the widest of the three outcomes despite its shorter spelling:
	// it is the only one that takes BOTH the longest class in the whole closed
	// set (recipient_refused_not_addressed, 31 bytes) AND a 64-byte attestation
	// that base64-expands to 88. `undeliverable` (13) with the longest bus class
	// (hop_unauthenticated, 19) and no attestation is ~95 bytes narrower.
	return PeerAckRequest{
		ProtocolVersion: AckWireVersion,
		// The widest legal ids, not the widest bytes a peer can send: an
		// oversized one is refused by AuthorizePeerAck, and a cap sized for what
		// we REFUSE rather than for what we ACCEPT is a cap sized by the
		// attacker.
		CorrelationKey:     stringOfLen(ids.MaxMessageIDLen),
		Recipient:          stringOfLen(ids.MaxAgentIDLen),
		Outcome:            AckRefused.String(),
		Class:              AckRecipientRefusedNotAddressed.String(),
		EmittedAtUnixMilli: 9223372036854775807,
		Attestation:        &AckAttestationEnvelope{Signature: make([]byte, signing.SignatureSize)},
	}
}

// stringOfLen builds an n-byte filler string for the bound derivation above. It
// is not a validator and the result is not a legal id — the derivation cares
// about LENGTH, and using a real id would silently shrink the bound if the
// fixture id got shorter.
func stringOfLen(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
