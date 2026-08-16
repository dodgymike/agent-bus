package ack

import "fmt"

// State is the sender-visible delivery lifecycle state (ACK-CONTRACT.md §8.1).
//
// FIVE DURABLE STATES. THREE ARE TERMINAL AND TERMINAL IS ABSORBING: a terminal
// state is never revisited, never reopened, and never downgraded to a
// non-terminal one.
//
// # THERE IS NO `unknown` MEMBER, AND ONE MUST NOT BE ADDED
//
// `unknown` is a REPORTING value — what GET /v1/ack (ACK-9) answers when no
// record is retained, because it was swept, never created, or is not the
// caller's. It is NOT a state. Writing "I don't know" durably is how a real
// terminal outcome gets overwritten by ignorance, so the enum simply has no
// value that could express it and ParseState refuses the spelling by name with
// a message saying why. That refusal is the enforcement; a comment would not be.
//
// # THERE IS NO `polled` MEMBER EITHER
//
// Delivery to an inbox or a poll is NOT recipient receipt (§4). Adding `polled`
// would require a per-(agent, message) server table that does not exist and
// that is strictly more state than an explicit ACK.
//
// It is stored on disk as a FIXED STRING, never a number — OutboxState.String()'s
// reasoning (internal/relay/outbox.go:297-306): a numeric enum in a durable
// record is unreadable to an operator and silently changes meaning if the
// constants are reordered.
type State uint8

const (
	// StateInvalid is the zero value and is NEVER valid. It exists so that a
	// Record left zero by a partial construction fails validate rather than
	// being silently read as the first real member — which, if the first member
	// were StateAccepted, would turn a forgotten field into a claim.
	StateInvalid State = iota

	// StateAccepted: committed and fsynced on the sender's bus (plane A). This
	// is the ONLY state this build writes from production code.
	StateAccepted

	// StateInFlight: at least one hop is owed — a pending outbox job exists.
	// Remote recipients only; a local recipient never reaches it (§8.3).
	StateInFlight

	// StateDelivered: TERMINAL, positive. The recipient APPLICATION acked
	// (plane C, §4). Never inferred from a cursor advancing.
	StateDelivered

	// StateRefused: TERMINAL, negative. An authenticated terminal NACK arrived.
	// Carries a recipient-emitted class.
	StateRefused

	// StateUndeliverable: TERMINAL, negative. This bus will never deliver it.
	// Carries a bus-emitted class.
	StateUndeliverable
)

// stateNames is the ONE mapping between a State and its durable spelling, used
// by both String and ParseState so an encoder and a decoder cannot disagree.
var stateNames = map[State]string{
	StateAccepted:      "accepted",
	StateInFlight:      "in_flight",
	StateDelivered:     "delivered",
	StateRefused:       "refused",
	StateUndeliverable: "undeliverable",
}

// String returns the durable spelling.
//
// An unknown value returns a spelling that ParseState REFUSES, so a state that
// escaped the enum cannot round-trip through the log and come back looking
// legitimate.
func (s State) String() string {
	if n, ok := stateNames[s]; ok {
		return n
	}
	return fmt.Sprintf("invalid-state(%d)", uint8(s))
}

// Terminal reports whether the state is absorbing.
func (s State) Terminal() bool {
	return s == StateDelivered || s == StateRefused || s == StateUndeliverable
}

// Negative reports whether the state is a NEGATIVE terminal, which is exactly
// the set that must carry a class (§5.4: a positive outcome has nothing to
// explain, and an optional class on it would create a channel where none is
// needed).
func (s State) Negative() bool {
	return s == StateRefused || s == StateUndeliverable
}

// rank orders the state machine's progress: non-terminal states may advance,
// terminal states may not move at all. Monotonicity is keyed on this and on
// nothing else — not on a sequence, not on a timestamp, not on the record's
// position in the log — which is what makes the rule survive replay.
func (s State) rank() int {
	switch {
	case s.Terminal():
		return 2
	case s == StateInFlight:
		return 1
	case s == StateAccepted:
		return 0
	default:
		return -1
	}
}

// ParseState decodes a durable spelling.
//
// AN UNRECOGNISED SPELLING IS AN ERROR, NEVER A DEFAULT — parseOutboxState's
// posture (internal/relay/outbox.go:316-330) and for the same reason: guessing
// turns a corrupt or future-format record into a plausible-looking outcome.
func ParseState(s string) (State, error) {
	for st, name := range stateNames {
		if name == s {
			return st, nil
		}
	}
	if s == "unknown" {
		return StateInvalid, fmt.Errorf("%w: %q is a REPORTING value, not a durable state; a record saying \"I don't know\" would overwrite a real terminal outcome with ignorance (ACK-CONTRACT.md §8.1)", ErrInvalidRecord, elide(s))
	}
	return StateInvalid, fmt.Errorf("%w: %q is not a delivery lifecycle state; the set is closed (accepted, in_flight, delivered, refused, undeliverable) and an unrecognised spelling is refused rather than defaulted", ErrInvalidRecord, elide(s))
}

// Class is the CLOSED set of reasons a negative terminal carries
// (ACK-CONTRACT.md §5.2). There are exactly twelve and there is NO free-text
// field anywhere in this package.
//
// # WHY THERE IS NO free-text REASON, RESTATED SO IT IS NOT "SIMPLIFIED" BACK IN
//
// Invariant 6: the durable trail records METADATA AND ROUTING ONLY. A reason
// string sourced from a recipient or from a payload is a body by another name —
// "delivery failed because <recipient text>" puts recipient-chosen bytes into an
// append-only, un-rewritable trail. So a class is a COMPILE-TIME CONSTANT chosen
// from this set by the code of the bus that emits it: never assembled, never
// templated, never concatenated.
//
// relay.OutboxRecord.Reason is a DIFFERENT thing and must not be mapped onto
// this. It is a bus-authored, bounded, sanitised LOCAL string for the operator
// log; the mapping is one-way (class -> record) and Reason never reaches a wire
// frame or a sender (§5.1).
type Class string

// Bus-emitted classes: asserted by the sender's own bus or by a hop, about
// routing.
const (
	// ClassNoRoute: no configured peer for the destination bus half.
	ClassNoRoute Class = "no_route"
	// ClassNoSuchRecipient: the destination bus has no such agent.
	ClassNoSuchRecipient Class = "no_such_recipient"
	// ClassHopRefused: the next hop answered finally and negatively.
	ClassHopRefused Class = "hop_refused"
	// ClassHopUnauthenticated: the peer could not be authenticated as a principal.
	ClassHopUnauthenticated Class = "hop_unauthenticated"
	// ClassLoopDropped: already traversed / split horizon.
	ClassLoopDropped Class = "loop_dropped"
	// ClassFanoutExceeded: over the onward-bus fan-out limit.
	ClassFanoutExceeded Class = "fanout_exceeded"
	// ClassHorizonExpired: the retry horizon ran out; the outbox settled abandoned.
	ClassHorizonExpired Class = "horizon_expired"
	// ClassLocalCapacity: a local durable resource refused the work, fail-closed.
	ClassLocalCapacity Class = "local_capacity"
	// ClassObligationLost: a durably-accepted onward obligation was abandoned at
	// restart. It CANNOT occur on the golden path and nothing emits it in this
	// build — detection is ACK-8's, and RELAY-48 must land first (§14 D2).
	ClassObligationLost Class = "obligation_lost"
)

// Recipient-emitted classes: exactly three, chosen by the recipient application.
//
// Each is a fixed constant that reveals THAT something failed, never WHAT.
// ClassRecipientRefusedUndecodable in particular says "decoding failed" and says
// nothing about the bytes that failed to decode — that is the exact line
// invariant 6 draws. A request for a richer explanation is a request for a body
// in the log and MUST be refused; the recipient and sender already have an
// end-to-end message channel for prose.
const (
	ClassRecipientRefusedPolicy       Class = "recipient_refused_policy"
	ClassRecipientRefusedUndecodable  Class = "recipient_refused_undecodable"
	ClassRecipientRefusedNotAddressed Class = "recipient_refused_not_addressed"
)

// busClasses and recipientClasses are the two halves of the closed set, kept
// separate because validate enforces the PAIRING as well as the membership: a
// `refused` (the recipient spoke) carries a recipient class and an
// `undeliverable` (the bus gave up) carries a bus class. Collapsing them into
// one set would let a peer's routing failure be recorded as though the
// recipient's application had refused it, which is a different claim about a
// different party.
var (
	busClasses = map[Class]struct{}{
		ClassNoRoute:            {},
		ClassNoSuchRecipient:    {},
		ClassHopRefused:         {},
		ClassHopUnauthenticated: {},
		ClassLoopDropped:        {},
		ClassFanoutExceeded:     {},
		ClassHorizonExpired:     {},
		ClassLocalCapacity:      {},
		ClassObligationLost:     {},
	}
	recipientClasses = map[Class]struct{}{
		ClassRecipientRefusedPolicy:       {},
		ClassRecipientRefusedUndecodable:  {},
		ClassRecipientRefusedNotAddressed: {},
	}
)

// BusEmitted reports membership of the bus-emitted half of the set.
func (c Class) BusEmitted() bool {
	_, ok := busClasses[c]
	return ok
}

// RecipientEmitted reports membership of the recipient-emitted half.
func (c Class) RecipientEmitted() bool {
	_, ok := recipientClasses[c]
	return ok
}

// Attestation labels WHAT AUTHENTICATED the terminal outcome (§6.3).
//
// There are exactly two values and there is deliberately NO value meaning
// "verified", because nothing in this system can produce one: the bus carries a
// message signature as opaque bytes and never verifies it
// (internal/store/message.go:260-270), and no endpoint distributes agents'
// messaging public keys, so a SENDER cannot verify a recipient attestation
// either (§16 Q1). The status API must LABEL attestation, never imply it.
type Attestation string

const (
	// AttestedByPeerBus: authenticated as a BUS by its client certificate
	// through RequirePeerPrincipal. It says which bus spoke, and nothing about
	// any agent.
	AttestedByPeerBus Attestation = "peer_bus"

	// AttestedByRecipientSignatureUnverified: the recipient supplied a detached
	// signature whose SHAPE was checked (present, exactly signing.SignatureSize
	// bytes) and whose AUTHENTICITY was not, by anybody. The name is long on
	// purpose — a shorter one would be read as a verification claim.
	AttestedByRecipientSignatureUnverified Attestation = "recipient_signature_unverified"
)

var attestations = map[Attestation]struct{}{
	AttestedByPeerBus:                      {},
	AttestedByRecipientSignatureUnverified: {},
}

// Valid reports membership of the closed attestation set.
func (a Attestation) Valid() bool {
	_, ok := attestations[a]
	return ok
}
