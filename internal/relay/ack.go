package relay

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// ACK/NACK AUTHORIZATION, ANTI-FORGERY AND PRIVACY (ACK-4).
//
// This file is the AUTHORIZATION HALF of the delivery-acknowledgement plane
// specified in ACK-CONTRACT.md. It owns four things and deliberately owns
// nothing else:
//
//  1. the CLOSED NACK class vocabulary and the closed terminal-outcome
//     vocabulary, both of which REJECT an unrecognised spelling rather than
//     defaulting it (§5.1, and parseOutboxState's posture at outbox.go:320);
//  2. the OBLIGATION BINDING RULE (§6.2) — the anti-forgery core, computed over
//     the outbox's EXISTING durable record with no new index;
//  3. the IDEMPOTENCY DECISION (§12) — invariant 10's three cases, as a pure
//     function whose result set contains NO DISCONNECT;
//  4. the PRIVACY constraints — the uniform refusal answer that closes the
//     status oracle (§13.3), the no-free-text rule (§5), and the redaction
//     point for the operator log.
//
// # WHAT THIS FILE IS NOT
//
// It is NOT the wire frame. ACK-3 owns the frame, and ACK-3 must first spend
// the already-reserved relay-wire-version = 1 and add the version field to BOTH
// the relay envelope and the ACK frame in the same change (message.go:25-32,
// whose stated trigger already fired). Nothing here encodes or decodes a frame;
// the types below are the vocabulary a frame is validated AGAINST.
//
// It is NOT the durable ACK record or the sender-visible state machine. ACK-2
// owns those. DecideAck takes the prior terminal outcome as an argument rather
// than reading a table, precisely so this file holds no state that could drift
// from the durable one.
//
// It is NOT a verifier. Read the next paragraph before adding one.
//
// # KNOWN DUPLICATION, FLAGGED RATHER THAN PAPERED OVER
//
// (No task key is cited here on purpose. ACK-4 does not mint task keys, and a
// follow-up id that does not resolve is worse than none — an earlier draft named
// one that had never been filed. The de-duplication is reported to the
// orchestrator to file through spec-keeper.)
//
// ACK-2 is concurrently building an internal/ack package that declares its OWN
// spelling of the same closed vocabulary (ack.Class, ack.Attestation, ack.State).
// AT THE TIME THIS FILE WAS WRITTEN THAT PACKAGE DID NOT EXIST IN HEAD AND DID
// NOT COMPILE, so depending on it would have made ACK-4 unverifiable against the
// committed tree and would have landed a consumer before its definition.
//
// So this file is deliberately SELF-CONTAINED — and that is a sequencing
// decision, NOT a claim that two copies of a closed enum are acceptable. Two
// vocabularies that must agree are two vocabularies that can disagree, which is
// the same defect OutboxRecord.OriginMessageID avoids by having no sibling
// origin-bus field. A follow-up must collapse them to ONE declaration once both
// have landed; the AUTHORIZATION rules below (the half-set check, the
// binding rule, the uniform refusal, the decision enum) are what ACK-4 owns and
// they stay here whichever package ends up owning the spellings.
//
// # WHAT "AUTHENTICATED" CAN MEAN HERE, AND WHAT IT CANNOT
//
// THE BUS VERIFIES NO MESSAGE SIGNATURES. It enforces SHAPE only — present, and
// exactly signing.SignatureSize bytes — and the RECIPIENT enforces authenticity
// (store/message.go:260-270, restated at httpapi/messages.go:238-243). No
// endpoint distributes agents' messaging public keys, so a SENDER cannot verify
// a recipient's attestation either. Therefore an "authenticated NACK" here means
// exactly two things and never a third:
//
//   - BUS-ATTRIBUTED (layer 1): the frame arrived on the peer surface behind
//     RequirePeerPrincipal, so we know WHICH BUS sent it. That gate is
//     fail-closed and it is the ONLY authentication factor on that surface —
//     httpapi/peermount.go:85-92 records the deliberate one-factor NARROWING of
//     invariant 11's cross-check, because a peer handler never sees an agent
//     principal and there is no pair to cross-check. THIS FILE INHERITS THAT
//     NARROWING AND MUST NOT WIDEN IT: an ACK frame must not be accepted on the
//     agent surface on behalf of a peer bus, and an agent session token must not
//     be consulted on the peer surface.
//   - OBLIGATION-BOUND (layer 2): AuthorizePeerAck below.
//
// A recipient attestation is END-TO-END UNVERIFIABLE BY ANYBODY TODAY. That is
// why AckAttestation has no value meaning "verified" and why one MUST NOT be
// added: a label nothing can produce is a lie the status API would tell every
// sender. ACK-11 must document the limitation in those words.

// ---------------------------------------------------------------------------
// Sentinels
// ---------------------------------------------------------------------------

// ackError keeps this file's sentinels distinct from the package's other error
// families, exactly as outboxError does for outbox.go.
type ackError struct{ msg string }

func newAckError(msg string) *ackError { return &ackError{msg: msg} }

func (e *ackError) Error() string { return e.msg }

var (
	// ErrAckNotBound is THE UNIFORM REFUSAL. Every well-formed ACK/NACK that
	// this bus will not settle returns THIS ERROR, BY IDENTITY, WITH THIS EXACT
	// TEXT — no wrapping, no %w, no formatted detail.
	//
	// THE UNIFORMITY IS THE SECURITY PROPERTY, NOT AN ECONOMY OF CODE. The
	// distinguishable causes are:
	//
	//   - we never wrote an obligation for this (peer, correlation key) — the
	//     REFLECTION case, a peer settling something it was never given;
	//   - we wrote one, but to a DIFFERENT peer — the CROSS-ROUTE case;
	//   - the key names a message on a bus we never forwarded to this peer —
	//     the THIRD-PARTY SETTLEMENT case;
	//   - the obligation existed and has since been swept.
	//
	// A distinct code or message for any of them is an ORACLE: it would let any
	// peered bus probe "did bus A send message <key> to bus B", and by
	// extension whether a named agent exists and is being written to. So they
	// are answered identically on the wire, and told apart ONLY in the operator
	// log via AckRefusalLogFields.
	//
	// This is the deliberate analogue of the 409 no-matching-reservation
	// indistinguishability invariant 10 preserves — and, like it, it must not be
	// "fixed" by making the cases distinguishable.
	ErrAckNotBound = newAckError("relay: no obligation on this bus binds this acknowledgement")

	// ErrInvalidAckFrame reports an ACK/NACK whose FIELDS are not well formed:
	// a correlation key that is not a message id, a recipient that is not a
	// fully-qualified agent id (invariant 2), an unrecognised class or outcome,
	// a class that the outcome does not own, or an attestation of the wrong
	// shape.
	//
	// It is deliberately DISTINCT from ErrAckNotBound and it is NOT an oracle:
	// every one of these is decidable by the sender from its own bytes, without
	// asking us. Collapsing it into the uniform answer would only make an honest
	// peer's encoder bug undebuggable.
	ErrInvalidAckFrame = newAckError("relay: invalid acknowledgement")

	// ErrAckOutcomeConflict is invariant 10's SECOND case on the ACK plane: the
	// same (correlation key, recipient) already reached a DIFFERENT terminal
	// outcome. Reject and log — and DO NOT DISCONNECT (§12). It is the
	// monotonicity rule the outbox already enforces (outbox.go:1685, :1702):
	// terminal is absorbing and a different terminal never overwrites an
	// existing one.
	ErrAckOutcomeConflict = newAckError("relay: acknowledgement conflicts with an already-recorded terminal outcome")
)

// ---------------------------------------------------------------------------
// The closed NACK class set (§5.2) — exactly twelve
// ---------------------------------------------------------------------------

// AckClass is why a delivery did not succeed. IT IS A CLOSED ENUM OF TWELVE
// COMPILE-TIME CONSTANTS AND IT IS THE ONLY THING A NACK MAY CARRY BESIDES IDS,
// A TIMESTAMP AND A SIGNATURE.
//
// # WHY THERE IS NO FREE-TEXT REASON FIELD, ANYWHERE
//
// Invariant 6: the append-only log records METADATA AND ROUTING ONLY, never
// bodies — enforced structurally, wal.AuditRecord has no body field. A reason
// string sourced from a recipient or from a payload is A BODY BY ANOTHER NAME:
// "delivery failed because <recipient text>" puts recipient-chosen bytes into a
// durable, append-only, un-rewritable trail. So a class is CHOSEN by the code of
// the bus that emits it; it is never assembled, never templated, never
// concatenated, and there is no adjacent string field to put the "detail" in.
//
// Each constant reveals THAT something failed and never WHAT. AckRecipientRefusedUndecodable
// in particular says "decoding failed" and says nothing whatsoever about the
// bytes that failed to decode — that is the exact line invariant 6 draws. A
// request for a richer explanation is a request for a body in the log and must
// be refused: the recipient and the sender already have an end-to-end message
// channel for prose, and it is the right place for it.
//
// # NOT TO BE CONFUSED WITH OutboxRecord.Reason
//
// OutboxRecord.Reason (outbox.go:171-190) is a BUS-AUTHORED, bounded, sanitised
// LOCAL string that a peer's error code can INFLUENCE. It stays on this bus, in
// this bus's durable record and operator log. The mapping is ONE-WAY —
// class -> record — and record.Reason MUST NEVER be returned to a sender or
// forwarded to a peer, because that would re-export a peer-influenced string to
// a third party.
type AckClass uint8

const (
	// --- Bus-emitted (9). Asserted by the sender's own bus or by a hop, about
	// ROUTING. Never chosen by a recipient application.

	// AckNoRoute: no configured peer for the destination bus half.
	AckNoRoute AckClass = iota + 1
	// AckNoSuchRecipient: the destination bus has no such agent
	// (CodeUnknownRecipient, handshake.go:76).
	AckNoSuchRecipient
	// AckHopRefused: the next hop answered finally and negatively. FINAL codes
	// only — CodeUnsigned, CodeBadSignature, CodeUnpeeredBus, CodeInvalidRelay,
	// CodeInvalidBusPath.
	AckHopRefused
	// AckHopUnauthenticated: the peer could not be authenticated as a principal
	// (RequirePeerPrincipal refusal, httpapi/peermount.go:316-363).
	AckHopUnauthenticated
	// AckLoopDropped: already traversed / split horizon (DropLoop,
	// message.go:42).
	AckLoopDropped
	// AckFanoutExceeded: over maxOnwardBusesPerMessage.
	AckFanoutExceeded
	// AckHorizonExpired: the retry horizon ran out and the outbox settled
	// abandoned (OutboxAbandoned, outbox.go:286-291).
	AckHorizonExpired
	// AckLocalCapacity: a local durable resource refused the work, fail-closed.
	AckLocalCapacity
	// AckObligationLost: a durably-accepted onward obligation was abandoned at
	// restart (RELAY-48). It CANNOT occur on the golden path; DETECTION IS
	// DEFERRED TO ACK-8. The constant exists now so the vocabulary is closed
	// once rather than extended later.
	AckObligationLost

	// --- Recipient-emitted (3). Chosen by the recipient APPLICATION from this
	// enum and from nothing else.

	// AckRecipientRefusedPolicy: the application declines it.
	AckRecipientRefusedPolicy
	// AckRecipientRefusedUndecodable: the application could not decode or
	// verify it. It says NOTHING about the bytes that failed.
	AckRecipientRefusedUndecodable
	// AckRecipientRefusedNotAddressed: the application does not consider itself
	// the addressee.
	AckRecipientRefusedNotAddressed

	// ackClassCount bounds the enum for the closedness test. It is NOT a class
	// and never appears on the wire.
	ackClassCount
)

// String returns the wire spelling. It is a fixed STRING and not a number, for
// OutboxState.String's reasoning (outbox.go:297-306): a numeric enum in a
// durable record is unreadable to an operator and silently changes meaning if
// the constants are ever reordered.
func (c AckClass) String() string {
	switch c {
	case AckNoRoute:
		return "no_route"
	case AckNoSuchRecipient:
		return "no_such_recipient"
	case AckHopRefused:
		return "hop_refused"
	case AckHopUnauthenticated:
		return "hop_unauthenticated"
	case AckLoopDropped:
		return "loop_dropped"
	case AckFanoutExceeded:
		return "fanout_exceeded"
	case AckHorizonExpired:
		return "horizon_expired"
	case AckLocalCapacity:
		return "local_capacity"
	case AckObligationLost:
		return "obligation_lost"
	case AckRecipientRefusedPolicy:
		return "recipient_refused_policy"
	case AckRecipientRefusedUndecodable:
		return "recipient_refused_undecodable"
	case AckRecipientRefusedNotAddressed:
		return "recipient_refused_not_addressed"
	default:
		return fmt.Sprintf("AckClass(%d)", uint8(c))
	}
}

// RecipientEmitted reports whether the class is one of the three a recipient
// application may choose. The distinction is load-bearing rather than
// descriptive: ValidateAckClassForOutcome uses it to refuse a peer that dresses
// a routing failure up as a recipient refusal, or the reverse.
func (c AckClass) RecipientEmitted() bool {
	switch c {
	case AckRecipientRefusedPolicy, AckRecipientRefusedUndecodable, AckRecipientRefusedNotAddressed:
		return true
	default:
		return false
	}
}

// ParseAckClass maps the wire spelling back onto a class.
//
// AN UNRECOGNISED CLASS IS AN ERROR, NEVER A DEFAULT — parseOutboxState's
// posture (outbox.go:316-330) and for the same reason: guessing turns a corrupt
// or future-format frame into a plausible-looking outcome, and here the
// plausible-looking outcome would be a TERMINAL one that can never be revisited.
//
// The offending spelling is ELIDED in the error, because a peer chooses it and
// must not get to choose the size of the line we log about refusing it.
func ParseAckClass(s string) (AckClass, error) {
	for c := AckClass(1); c < ackClassCount; c++ {
		if c.String() == s {
			return c, nil
		}
	}
	return 0, fmt.Errorf("%w: %q is not one of the twelve acknowledgement classes", ErrInvalidAckFrame, elideAck(s))
}

// ---------------------------------------------------------------------------
// The closed terminal-outcome set (§8.1, §9.2)
// ---------------------------------------------------------------------------

// AckOutcome is the TERMINAL outcome an ACK/NACK frame carries. It is the three
// terminal members of §8.1's five-state sender-visible machine; the two
// non-terminal states (accepted, in_flight) are never carried on a frame
// because they are facts about THIS bus, not about the recipient.
//
// The full state machine and its durable record belong to ACK-2. This enum is
// here because AUTHORIZATION needs it: the outcome is what decides which half of
// the class vocabulary is legal and whether an attestation must be present.
//
// "unknown" is deliberately absent. It is a REPORTING value of the status API
// (§8.1), never a state and never a frame outcome: an "I don't know" that could
// travel on the wire is how a real terminal outcome gets overwritten by
// ignorance.
type AckOutcome uint8

const (
	// AckDelivered: the recipient application ACKed (plane C). Positive, and it
	// carries NO CLASS — a success has nothing to explain, and an optional class
	// on it would create a disclosure channel where none is needed (§5.4).
	AckDelivered AckOutcome = iota + 1
	// AckRefused: an authenticated terminal NACK from the RECIPIENT. Carries a
	// recipient-emitted class.
	AckRefused
	// AckUndeliverable: this bus (or a hop) will never deliver it. Carries a
	// bus-emitted class.
	AckUndeliverable

	// ackOutcomeCount bounds the enum for the closedness test.
	ackOutcomeCount
)

// String returns the wire spelling — a fixed string, for OutboxState.String's
// reasoning.
func (o AckOutcome) String() string {
	switch o {
	case AckDelivered:
		return "delivered"
	case AckRefused:
		return "refused"
	case AckUndeliverable:
		return "undeliverable"
	default:
		return fmt.Sprintf("AckOutcome(%d)", uint8(o))
	}
}

// Negative reports whether the outcome is a negative terminal, i.e. whether a
// class is REQUIRED. It is the single spelling of that branch; re-spelling it at
// a call site is how the two ends of "class iff negative" come to disagree.
func (o AckOutcome) Negative() bool { return o == AckRefused || o == AckUndeliverable }

// RecipientSourced reports whether the outcome originates with the RECIPIENT
// APPLICATION (plane C) rather than with a bus's routing layer. It is what
// decides whether an attestation must be present.
func (o AckOutcome) RecipientSourced() bool { return o == AckDelivered || o == AckRefused }

// ParseAckOutcome maps the wire spelling back onto an outcome. Unrecognised is
// an ERROR, never a default — see ParseAckClass.
func ParseAckOutcome(s string) (AckOutcome, error) {
	for o := AckOutcome(1); o < ackOutcomeCount; o++ {
		if o.String() == s {
			return o, nil
		}
	}
	return 0, fmt.Errorf("%w: outcome %q is not one of delivered, refused, undeliverable", ErrInvalidAckFrame, elideAck(s))
}

// ValidateAckClassForOutcome enforces the class/outcome coupling IN BOTH
// DIRECTIONS, the way OutboxRecord.validate enforces Reason against State
// (outbox.go:435-441).
//
// The rule, from §5.4 and §8.1:
//
//	delivered     -> NO class. A success explains nothing.
//	refused       -> a RECIPIENT-EMITTED class, and only those three.
//	undeliverable -> a BUS-EMITTED class, and only those nine.
//
// THE HALF-SET CHECK IS AN ANTI-FORGERY CHECK, NOT TIDINESS. Without it a peer
// could send outcome=refused with class=no_route and have this bus record a
// ROUTING failure of its own as a RECIPIENT'S DECISION — or the reverse, and
// attribute a recipient's policy refusal to the federation. The two halves are
// asserted by different parties (§1) and the frame must not be allowed to
// blur them.
//
// zero is the "no class" spelling; a frame that omits the field decodes to it.
//
// # THE OUTCOME IS BOUNDS-CHECKED FIRST, AND THAT ORDER IS LOAD-BEARING
//
// Negative and RecipientSourced both answer FALSE for a value outside the enum,
// so an unchecked zero or out-of-range outcome would fall through the positive
// arm and be accepted — turning a never-populated struct, or a conversion from
// ACK-2's parallel vocabulary that returned a zero value on an unmapped input,
// into a valid POSITIVE TERMINAL. Terminal is absorbing, so that record could
// never be corrected. Rejecting up front is the same posture ParseAckClass
// takes: an unrecognised value is an ERROR, never a default.
func ValidateAckClassForOutcome(outcome AckOutcome, class AckClass) error {
	if outcome == 0 || outcome >= ackOutcomeCount {
		return fmt.Errorf("%w: outcome %s is outside the closed set delivered, refused, undeliverable; an unrecognised outcome is REJECTED, never treated as a positive terminal", ErrInvalidAckFrame, outcome)
	}
	if !outcome.Negative() {
		if class != 0 {
			return fmt.Errorf("%w: outcome %s is positive and carries no class, but class %s was set", ErrInvalidAckFrame, outcome, class)
		}
		return nil
	}
	if class == 0 {
		return fmt.Errorf("%w: outcome %s is a negative terminal and REQUIRES a class", ErrInvalidAckFrame, outcome)
	}
	if class >= ackClassCount {
		return fmt.Errorf("%w: class %s is not one of the twelve acknowledgement classes", ErrInvalidAckFrame, class)
	}
	if outcome == AckRefused && !class.RecipientEmitted() {
		return fmt.Errorf("%w: outcome refused is a RECIPIENT decision, but class %s is bus-emitted", ErrInvalidAckFrame, class)
	}
	if outcome == AckUndeliverable && class.RecipientEmitted() {
		return fmt.Errorf("%w: outcome undeliverable is a ROUTING failure, but class %s is recipient-emitted", ErrInvalidAckFrame, class)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Attestation labelling (§6.3) — and the value that must never exist
// ---------------------------------------------------------------------------

// AckAttestation says WHAT KIND of authentication stands behind an outcome. The
// status API must LABEL attestation, never imply it.
//
// THERE ARE EXACTLY TWO VALUES AND THERE IS DELIBERATELY NO VALUE MEANING
// "VERIFIED". Nothing in this system can produce one: the bus checks signature
// SHAPE only and never verifies (store/message.go:260-270), and no endpoint
// distributes agents' messaging public keys, so a sender cannot verify either.
// Adding a "verified" value would be the status API asserting, on the
// recipient's behalf, a fact nobody established.
type AckAttestation uint8

const (
	// AckAttestedPeerBus: authenticated as coming from an adjacent bus by
	// RequirePeerPrincipal's certificate check, and bound to an obligation this
	// bus wrote. This is the STRONGEST claim available and it is a claim about a
	// BUS, not about an agent.
	AckAttestedPeerBus AckAttestation = iota + 1
	// AckAttestedRecipientSignatureUnverified: the frame carried a detached
	// signature of the right SHAPE over canonical ACK bytes. NOBODY VERIFIED IT.
	// The name says so on purpose, so a reader of a status response cannot
	// mistake presence for validity.
	AckAttestedRecipientSignatureUnverified

	// ackAttestationCount bounds the enum for the closedness test.
	ackAttestationCount
)

// String returns the wire spelling.
func (a AckAttestation) String() string {
	switch a {
	case AckAttestedPeerBus:
		return "peer_bus"
	case AckAttestedRecipientSignatureUnverified:
		return "recipient_signature_unverified"
	default:
		return fmt.Sprintf("AckAttestation(%d)", uint8(a))
	}
}

// ParseAckAttestation maps the wire spelling back. Unrecognised is an error,
// never a default — and in particular "verified" does not parse, because no
// such attestation exists.
func ParseAckAttestation(s string) (AckAttestation, error) {
	for a := AckAttestation(1); a < ackAttestationCount; a++ {
		if a.String() == s {
			return a, nil
		}
	}
	return 0, fmt.Errorf("%w: attestation %q is not one of peer_bus, recipient_signature_unverified", ErrInvalidAckFrame, elideAck(s))
}

// AckSurface is WHICH AUTHENTICATED SURFACE a frame arrived on. It is supplied
// by the mount site and is NEVER read from a frame.
//
// It exists because the attestation label cannot be derived from the frame's
// contents: only the caller knows whether RequirePeerPrincipal's certificate
// check ran or whether an agent session did. An earlier version of
// ValidateAckAttestation inferred "peer_bus" from the OUTCOME alone, which meant
// an agent-surface frame saying outcome=undeliverable was labelled as attested
// by an adjacent BUS — this bus telling a sender, in a durable record, that a
// peer vouched for something an agent said. §6.1's narrowing is one factor per
// surface; conflating the two surfaces widens it, which this file may not do.
type AckSurface uint8

const (
	// AckSurfacePeer is POST /v1/peer/ack, gated by RequirePeerPrincipal
	// (certificate only — the documented one-factor narrowing,
	// httpapi/peermount.go:85-92).
	AckSurfacePeer AckSurface = iota + 1
	// AckSurfaceAgent is POST /v1/ack, gated by session AND mTLS, cross-checked
	// (invariant 11). Only a RECIPIENT reaches it, and only about plane C.
	AckSurfaceAgent

	// ackSurfaceCount bounds the enum for the closedness test.
	ackSurfaceCount
)

// String is for logs and test failures.
func (s AckSurface) String() string {
	switch s {
	case AckSurfacePeer:
		return "peer"
	case AckSurfaceAgent:
		return "agent"
	default:
		return fmt.Sprintf("AckSurface(%d)", uint8(s))
	}
}

// ValidateAckAttestation checks the attestation SHAPE that surface and outcome
// require, and returns the label the durable record and the status API carry.
//
// SHAPE ONLY, BYTE-FOR-BYTE THE POSTURE ALREADY TAKEN FOR MESSAGE SIGNATURES:
// present, and exactly signing.SignatureSize bytes. NOTHING HERE VERIFIES, and
// nothing here may be changed to verify without a key-distribution endpoint that
// does not exist — see the file header, and ACK-CONTRACT.md §16 Q1.
//
// A recipient-sourced outcome (delivered, refused) MUST carry a signature; a
// bus-sourced one (undeliverable) MUST NOT, because there is no recipient in the
// story to have signed anything and an unexplained 64 bytes on a routing failure
// is a channel nobody asked for (§5.3: no field whose length a remote party
// chooses).
//
// AN AGENT MAY NOT ASSERT A ROUTING OUTCOME. On AckSurfaceAgent the only legal
// outcomes are the recipient-sourced ones: "undeliverable" is a claim about the
// federation's routing, which a recipient application has no standing to make
// and which would otherwise be recorded as though a bus had said it.
func ValidateAckAttestation(surface AckSurface, outcome AckOutcome, signature []byte) (AckAttestation, error) {
	// Both enums are bounds-checked FIRST, for ValidateAckClassForOutcome's
	// reason: RecipientSourced answers false outside the enum, so an unchecked
	// out-of-range outcome would be labelled peer_bus — this bus vouching, in a
	// durable record, for a frame it could not even classify.
	if surface == 0 || surface >= ackSurfaceCount {
		return 0, fmt.Errorf("%w: surface %s is outside the closed set peer, agent; the mount site must name which gate authenticated this frame", ErrInvalidAckFrame, surface)
	}
	if outcome == 0 || outcome >= ackOutcomeCount {
		return 0, fmt.Errorf("%w: outcome %s is outside the closed set delivered, refused, undeliverable; it is REJECTED rather than attested", ErrInvalidAckFrame, outcome)
	}
	if surface == AckSurfaceAgent && !outcome.RecipientSourced() {
		return 0, fmt.Errorf("%w: outcome %s is a ROUTING claim and arrived on the agent surface; a recipient application may only assert delivered or refused", ErrInvalidAckFrame, outcome)
	}
	if outcome.RecipientSourced() {
		if len(signature) != signing.SignatureSize {
			return 0, fmt.Errorf("%w: outcome %s is recipient-sourced and requires a detached signature of exactly %d bytes, got %d (SHAPE ONLY — no bus verifies it)",
				ErrInvalidAckFrame, outcome, signing.SignatureSize, len(signature))
		}
		return AckAttestedRecipientSignatureUnverified, nil
	}
	if len(signature) != 0 {
		return 0, fmt.Errorf("%w: outcome %s is asserted by a bus, not a recipient, so it must carry no attestation; got %d bytes",
			ErrInvalidAckFrame, outcome, len(signature))
	}
	return AckAttestedPeerBus, nil
}

// ---------------------------------------------------------------------------
// Layer 2 — the obligation binding rule (§6.2). THE ANTI-FORGERY CORE.
// ---------------------------------------------------------------------------

// AckObligations is the durable record of what this bus owes which peer. It is
// satisfied by *Outbox.
//
// It is an INTERFACE rather than a *Outbox so the binding rule can be tested
// against adversarial tables without a WAL — but the production implementation
// is the outbox and only the outbox, because the whole point of §6.2 is that
// the binding is computed from state THIS BUS ALREADY DURABLY WROTE. A second
// source of obligations would be a second answer to "did we owe this?", and the
// two could disagree.
type AckObligations interface {
	// Lookup returns the record for a job id, and whether the table holds one.
	// It must report false for a swept or expired job — *Outbox.Lookup does.
	Lookup(jobID string) (OutboxRecord, bool)
}

// AuthorizePeerAck is THE BINDING RULE:
//
//	A peer-hop ACK/NACK from peer P is authoritative for correlation key K if
//	and only if DeriveJobID(P, K) names an outbox job THIS BUS DURABLY WROTE.
//
// It returns the bound obligation, or ErrAckNotBound — by identity, uniformly,
// with no detail (see ErrAckNotBound).
//
// # TWO PRECONDITIONS THE CALLER OWNS. BOTH FAIL SILENTLY IF IGNORED.
//
//  1. peerBusID MUST be httpapi.PeerPrincipal.BusID — the bus id
//     RequirePeerPrincipal resolved from the CLIENT CERTIFICATE — and NEVER a
//     field read out of the frame. If ACK-3 takes it from JSON, the binding
//     rule authorises the NAME a peer chose rather than the peer that sent it,
//     and every guarantee below evaporates while every test still passes.
//  2. THIS FUNCTION IS ONLY HALF OF AUTHORIZATION, despite its name. It binds
//     the PEER to the KEY. It does NOT bind the RECIPIENT — see the "what it
//     deliberately does not do" section. A nil error here means "this peer may
//     speak about this key", NOT "this peer may settle this recipient".
//
// # DENIAL OF SERVICE: A QUOTA MUST SIT IN FRONT OF THIS CALL (ACK-CONTRACT §16 Q3)
//
// Outbox.Lookup takes the outbox's EXCLUSIVE mutex and runs sweepLocked, an O(n)
// scan over up to MaxOutboxJobs records — the same mutex Enqueue and Settle
// need. So an unmetered peer surface would let one peer drive an O(n) exclusive
// sweep per bogus frame and contend with real delivery. It also leaves a coarse
// TIMING channel that the uniform error text cannot close, because latency
// scales with THIS bus's outbox backlog rather than with the answer. Neither is
// fixable in this function; ACK-3 MUST rate-limit the peer ACK surface before
// reaching here, and ACK-11 must document the timing channel rather than claim
// the refusal is indistinguishable in every respect. It is indistinguishable in
// CONTENT, which is what closes the existence oracle.
//
// # WHAT IT CLOSES, AND WHY IT NEEDS NO NEW STATE
//
//   - REFLECTION — a peer settling an obligation it was never given: we never
//     wrote a job to that peer for that key, so the lookup misses.
//   - CROSS-ROUTE FORGERY — peer B settling the copy that went via peer C: the
//     job id is keyed on the PEER (outbox.go:390-396), so B's derivation names a
//     job that does not exist.
//   - THIRD-PARTY SETTLEMENT — a peer settling a key whose bus half names some
//     other bus: same miss, because we never wrote a job to THIS peer for it.
//   - RESURRECTION AFTER RETENTION — *Outbox.Lookup sweeps first and reports
//     false for an expired record, so a key whose window closed is refused with
//     the same uniform answer rather than reopened.
//
// # WHAT IT DELIBERATELY DOES NOT DO — READ THIS BEFORE "STRENGTHENING" IT
//
// It does NOT require the correlation key's bus half to equal the ACKing peer.
// In an A->B->C chain, C's ACK reaches A VIA B, and the bus half of K is A's,
// not C's. A "bus half must equal the peer" rule would be wrong and would break
// multi-hop (ACK-5). The job-id binding is the correct test AT EVERY HOP.
//
// It does NOT bind the RECIPIENT, because the outbox record does not carry one:
// an OutboxRecord is (peer, origin message id) and nothing else identifying.
// The recipient half of the binding is ACK-2's, and the two tests are
// CONJUNCTIVE: §8.2's "(none)" row requires that a frame naming a (key,
// recipient) pair for which NO ACK RECORD EXISTS is rejected with this same
// uniform answer. The ACK record is created for the recipients the SENDER
// NAMED, so that row is what stops a legitimately-bound peer settling on behalf
// of an agent that was never addressed. NEITHER HALF IS SUFFICIENT ALONE. This
// function validates the recipient's SHAPE (invariant 2) so a malformed one
// never reaches that table, and stops there.
//
// localBusID is required so a peer claiming OUR bus id is refused before
// anything else: ValidatePeerBusID compares case-insensitively and a peer may
// never assert our namespace.
//
// # WIRING REQUIREMENT FOR ACK-3, STATED HERE BECAUSE GETTING IT WRONG FAILS SILENTLY
//
// peerBusID MUST come from RequirePeerPrincipal's certificate resolution — the
// authenticated adjacent bus — and NEVER from a field in the frame. A frame-
// supplied peer id would let any peer name any peer and the binding rule would
// authorise the naming rather than the sender.
//
// And it must be the SAME CANONICAL SPELLING the forwarder used at Enqueue.
// DeriveJobID is deliberately CASE-SENSITIVE while the rest of the system folds
// bus ids (outbox.go:372's doc: registry.go lowercases, path.go folds,
// ValidatePeerBusID compares with EqualFold, and RELAY-19 owns normalising at
// the ENQUEUE boundary). A case mismatch between the two sides derives two
// different job ids, so this function would refuse a LEGITIMATE acknowledgement
// with the uniform answer — an availability failure that is indistinguishable
// from a forgery in the log, and therefore very hard to diagnose. It is not a
// forgery hole (a mismatch only ever makes the rule STRICTER), but it is a
// silent outage, and it is closed by normalising at enqueue, not by folding
// here: folding here would make every mixed-case record fail its own integrity
// check in validate.
func AuthorizePeerAck(obligations AckObligations, localBusID, peerBusID, correlationKey, recipient string) (OutboxRecord, error) {
	if obligations == nil {
		// A nil table would make every ACK unbindable, which LOOKS like a
		// working anti-forgery rule and is actually a total outage. Fail with a
		// distinguishable error rather than the uniform refusal, because this is
		// a wiring fault on OUR side and no peer should be able to provoke it.
		return OutboxRecord{}, errors.New("relay: AuthorizePeerAck has no obligation table; the binding rule cannot be evaluated")
	}
	if err := ValidatePeerBusID(localBusID, peerBusID); err != nil {
		return OutboxRecord{}, fmt.Errorf("%w: acknowledging peer: %v", ErrInvalidAckFrame, err)
	}
	// The correlation key is the ORIGIN bus's server-minted message id (§3). It
	// is INPUT TO BE VALIDATED, never an identity to be trusted (invariant 1),
	// and it is bounded before anything quotes it.
	if len(correlationKey) > ids.MaxMessageIDLen {
		return OutboxRecord{}, fmt.Errorf("%w: correlation key is %d bytes, but a message id is at most %d; it is not echoed here because it is oversized",
			ErrInvalidAckFrame, len(correlationKey), ids.MaxMessageIDLen)
	}
	if _, _, err := ids.ParseMessageID(correlationKey); err != nil {
		return OutboxRecord{}, fmt.Errorf("%w: correlation key: %v", ErrInvalidAckFrame, err)
	}
	// Invariant 2: every agent id on the wire is fully qualified. ParseAgentID
	// enforces the <bus-id>.<agent-id> form and one spelling per id.
	if len(recipient) > ids.MaxAgentIDLen {
		return OutboxRecord{}, fmt.Errorf("%w: recipient is %d bytes, but an agent id is at most %d; it is not echoed here because it is oversized",
			ErrInvalidAckFrame, len(recipient), ids.MaxAgentIDLen)
	}
	if _, _, _, err := ids.ParseAgentID(recipient); err != nil {
		return OutboxRecord{}, fmt.Errorf("%w: recipient: %v", ErrInvalidAckFrame, err)
	}

	rec, ok := obligations.Lookup(DeriveJobID(peerBusID, correlationKey))
	if !ok {
		return OutboxRecord{}, ErrAckNotBound
	}
	// Defence in depth against a table that has been corrupted or spliced: the
	// record we found must actually describe the obligation we asked for.
	// OutboxRecord.validate already re-derives the job id from these two fields
	// on the way in and out of the log, so a mismatch here is impossible for any
	// record that came from Encode or DecodeOutboxRecord — which is exactly why
	// checking is cheap and why a hit would be worth knowing about. It answers
	// the UNIFORM refusal, not a distinguishable one, so it cannot be used to
	// probe the table either.
	if rec.PeerBusID != peerBusID || rec.OriginMessageID != correlationKey {
		return OutboxRecord{}, ErrAckNotBound
	}
	return rec, nil
}

// ---------------------------------------------------------------------------
// Idempotency (§12) — invariant 10's three cases, and NO DISCONNECT
// ---------------------------------------------------------------------------

// AckDecision is what to do with an incoming terminal ACK/NACK given what this
// bus has already recorded for the same (correlation key, recipient).
//
// # THERE IS NO DISCONNECT MEMBER, AND ADDING ONE IS FORBIDDEN
//
// Invariant 10 requires two questions be answered before ANY disconnect is
// added. Both are answered here, on the record, and both point the same way:
//
//  1. CAN A MERELY BUGGY CLIENT REACH THIS LINE? Yes, trivially — an agent that
//     ACKs a correlation key it mistyped; an agent that re-ACKs after its own
//     restart; a peer that re-sends because our 200 was lost in flight. All
//     three are honest.
//  2. DOES THIS CONNECTION CARRY ONLY ONE PRINCIPAL'S TRAFFIC? No. A peer bus
//     relays for EVERY AGENT BEHIND IT. Dropping that socket over one frame
//     punishes every bystanding agent on the far side, and takes their parked
//     long-polls with it.
//
// So every ACK-plane refusal is REJECT-AND-LOG. The one disconnect on this bus
// — replay of an already-accepted SIGNED MESSAGE — belongs to the message
// plane; AN ACK FRAME IS NOT A MESSAGE AND MUST NEVER REACH THAT PATH.
//
// The enum being closed and disconnect-free is the STRUCTURAL half of that
// guarantee: a caller cannot act on a decision this type cannot express.
type AckDecision uint8

const (
	// AckApply: no terminal outcome is recorded for this (key, recipient).
	// Apply it — subject to invariant 4, the transition is committed and
	// fsynced BEFORE anything is acknowledged to the peer.
	AckApply AckDecision = iota + 1
	// AckReplay: invariant 10's FIRST case. Same key, SAME outcome. This is a
	// legitimate retry of an acknowledgement that was probably lost in flight.
	// Return the ORIGINAL result, RE-APPLY NOTHING, do not error, and do not
	// disconnect. (§9.3: answered 200 with duplicate:true.)
	AckReplay
	// AckConflict: invariant 10's SECOND case. Same key, DIFFERENT terminal
	// outcome. Terminal is ABSORBING: it is never revisited, never reopened and
	// never downgraded. Reject and LOG — and do not disconnect.
	AckConflict

	// ackDecisionCount bounds the enum for the closedness test.
	ackDecisionCount
)

// String is for logs and test failures.
func (d AckDecision) String() string {
	switch d {
	case AckApply:
		return "apply"
	case AckReplay:
		return "replay"
	case AckConflict:
		return "conflict"
	default:
		return fmt.Sprintf("AckDecision(%d)", uint8(d))
	}
}

// AckTerminal is one terminal outcome and its class — the whole of what two
// acknowledgements are compared on. It carries NO free text, no timestamp and
// no attestation, because none of those may make two otherwise-identical
// outcomes count as different: a peer re-sending the same settlement one second
// later is a RETRY, and comparing on emitted_at would turn every retry into a
// protocol violation.
type AckTerminal struct {
	Outcome AckOutcome
	Class   AckClass
}

// DecideAck is invariant 10's three cases on the ACK plane, as a pure function.
//
// It reads no table by design: ACK-2 owns the durable ACK record, and a second
// copy of "what did we already decide" in this file would be a second answer
// that could drift from the durable one. The caller passes what it read.
//
// hasPrior=false means no terminal outcome is recorded YET for this (key,
// recipient) — NOT that the pair is unknown. A pair with no ACK record at all is
// refused earlier, by §8.2's "(none)" row, with ErrAckNotBound.
func DecideAck(prior AckTerminal, hasPrior bool, incoming AckTerminal) (AckDecision, error) {
	if err := ValidateAckClassForOutcome(incoming.Outcome, incoming.Class); err != nil {
		return 0, err
	}
	if !hasPrior {
		return AckApply, nil
	}
	if prior == incoming {
		return AckReplay, nil
	}
	// Reject and log. NOT a disconnect — see AckDecision's doc for invariant
	// 10's two questions answered. The error carries only closed-enum spellings,
	// so logging it verbatim discloses nothing a peer chose the bytes of.
	return AckConflict, fmt.Errorf("%w: already %s/%s, refusing %s/%s",
		ErrAckOutcomeConflict, prior.Outcome, prior.Class, incoming.Outcome, incoming.Class)
}

// ---------------------------------------------------------------------------
// Privacy: the status oracle (§13.3) and the redaction point (§5.1)
// ---------------------------------------------------------------------------

// AckStatusUnknown is the ONE state string GET /v1/ack/{correlation_key}
// returns for every case that is not "you sent this, here is its row".
//
// It is never written to a durable record: writing "I don't know" durably is how
// a real terminal outcome gets overwritten by ignorance (§8.1).
const AckStatusUnknown = "unknown"

// AckStatusVisible decides whether a status row may be shown to caller.
//
// > ONLY THE ORIGINAL SENDER MAY READ A ROW. Every other case — the key never
// > existed, the key was swept, the key belongs to somebody else — returns the
// > SAME answer: 200 with state "unknown".
//
// A 403 would confirm the message EXISTS, which is the oracle ACK-4 is required
// to close. It is the same reasoning handleBroadcast already applies when it
// authenticates BEFORE answering 501 (httpapi/messages.go:456-460): a route that
// told a caller what it does and does not hold would be describing the messaging
// surface to somebody with no business knowing it exists.
//
// # WHAT A CALLER WHO IS NOT THE SENDER LEARNS FROM THIS ROUTE: NOTHING
//
// Not whether the correlation key names a real message. Not whether the named
// recipient exists, is enrolled, or is online. Not whether anything was ever
// delivered to them. Not the traversed bus path, not which peer refused, not the
// recipient's poll activity, not any roster membership. The sender learns the
// outcome for the recipients IT NAMED and nothing else about the federation.
//
// # AND WHY THERE IS NO AGGREGATE FORM
//
// Any roll-up ("3 of 5 delivered") is a ROSTER-SIZE ORACLE: the count discloses
// bus membership to any sender. §5.5 fixes the shape as PER-RECIPIENT WITH NO
// ROLL-UP AND NO QUORUM for exactly this reason, and §3.2's (key, recipient)
// keying means broadcast can later produce N rows through the same schema
// without one ever being needed.
//
// Comparison is exact rather than case-folded: agent ids have ONE spelling by
// construction (ids.ParseAgentID rejects any other), so a fold here would only
// widen who counts as the sender.
func AckStatusVisible(recordSender, caller string, found bool) bool {
	if !found {
		return false
	}
	if recordSender == "" || caller == "" {
		// An unattributed row is shown to nobody. Empty is not a wildcard.
		return false
	}
	return recordSender == caller
}

// AckRefusalLogFields is the ONE redaction point for an ACK-plane refusal.
//
// Invariant 6 makes the SILENT discard the defect, so a refusal must be logged
// loudly and specifically — but "specifically" is about naming WHICH obligation
// and WHY WE CAN SAY, not about echoing what a peer sent. So:
//
//   - the peer bus id, the correlation key and the recipient are ELIDED to
//     maxElidedAckChars, because a remote party chooses those bytes and must not
//     choose the size of our log line (the MaxOutboxReasonLen discipline);
//
//   - the derived job id is included, because it is what an operator greps the
//     WAL for. IT IS BOUND SEPARATELY FROM THE DISPLAY FIELDS, and that split is
//     the whole point: each half is clamped at ITS OWN ID MAXIMUM before the
//     derivation, so the job id is bounded (by MaxOutboxJobIDLen, exactly and by
//     construction) without any LEGITIMATE id being cut.
//
//     Two wrong versions of this were written before the right one, so both are
//     recorded. Deriving from the RAW halves concatenates two unbounded strings
//     before truncating, allocating whatever a peer sent. Deriving from the
//     DISPLAY-ELIDED halves is subtler and looked correct: it bounds the
//     allocation, but maxElidedAckChars is 64 while ids.MaxMessageIDLen is 85,
//     so a perfectly legitimate long correlation key produced a job id that
//     greps nothing — the exact defect, moved from the whole id onto one half,
//     and invisible to a test whose fixture was shorter than the boundary.
//
//   - NO FREE TEXT FROM ANY REMOTE PARTY IS INCLUDED, and there is no parameter
//     through which any could be. A "reason" argument here would be the body in
//     the log that §5.1 forbids.
//
// The returned pairs go to logging.Logger's variadic kv. THEY ARE FOR THE
// OPERATOR LOG ONLY and must never be returned to a sender or forwarded to a
// peer: on the wire, every one of these refusals is the single uniform answer.
func AckRefusalLogFields(peerBusID, correlationKey, recipient string) []interface{} {
	// Clamped at each half's OWN id maximum, so the sum is MaxOutboxJobIDLen by
	// construction (64 + 1 + 85) and no legitimate id is shortened.
	jobID := DeriveJobID(clampAck(peerBusID, MaxPeerBusIDLen), clampAck(correlationKey, ids.MaxMessageIDLen))
	return []interface{}{
		"peer_bus", elideAck(peerBusID),
		"correlation_key", elideAck(correlationKey),
		"recipient", elideAck(recipient),
		"job_id", jobID,
		"outcome", "refused_uniformly",
		"disconnected", false,
	}
}

// clampAck truncates s to at most max BYTES on a rune boundary, adding no
// marker.
//
// The absent marker is deliberate and is why this is not elideAck: the result is
// used to build a job id an operator greps for, and "…(truncated)" in the middle
// of an identifier makes it match nothing while still looking like an id. A
// silently short id is the honest failure here; a decorated one is not.
func clampAck(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// maxElidedAckChars bounds untrusted text in an ACK-plane error or log line. It
// is the outbox's bound, by reference rather than by a second literal.
const maxElidedAckChars = maxElidedOutboxChars

// elideAck truncates untrusted text for an error message or a log line. It is a
// thin alias for elideOutbox so there is ONE truncation implementation in this
// package: two would be two rune-boundary bugs waiting to disagree.
func elideAck(s string) string { return elideOutbox(s) }
