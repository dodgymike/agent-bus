package hub

import (
	"errors"
	"fmt"
)

// Sentinel errors. Every one is checkable with errors.Is, and the HTTP layer
// maps them to status codes BY SENTINEL and never by matching error text — the
// text is diagnostic detail for the operator's log and is free to change
// without silently changing a status code.
var (
	// ErrUnknownSender reports a caller that authenticated but is not on this
	// bus's roster. It is a should-not-happen: the principal came from a live
	// session, and a session can only exist for an enrolled agent — and since
	// AUTH-7 the hub reads the SAME roster the session was issued against (see
	// RosterSource), so the two cannot disagree by construction. It is checked
	// anyway, fail-closed: this is the last gate before a durable write, and the
	// day a second roster is introduced the divergence must be loud rather than
	// silently permissive.
	ErrUnknownSender = errors.New("hub: the sending agent is not on this bus's roster")

	// ErrUnknownRecipient reports a directed send to an agent this bus does not
	// know. The HTTP layer answers 404.
	ErrUnknownRecipient = errors.New("hub: unknown recipient")

	// ErrRelayedSender reports the sender of a RELAYED message that is either
	// malformed or claims THIS bus's namespace.
	//
	// It is ErrUnknownSender's inverse and is a distinct sentinel because it is a
	// distinct fact: a local send is refused for a sender we do NOT hold, and a
	// relayed one for a sender we WOULD hold. Collapsing the two would make a
	// peer asserting an id in our namespace — the permanent id-space injury
	// cca64afd names — indistinguishable in a log from one of our own agents
	// having left the roster. See Hub.checkRelayedSender.
	ErrRelayedSender = errors.New("hub: a relayed message's sender must be a well-formed fully-qualified id belonging to another bus")

	// ErrInvalidRelayedMessage reports a relayed message whose SHAPE this bus
	// cannot record: an unusable origin timestamp, or a signature that is not the
	// right length. Both are refused BEFORE the write path mints a sequence, so a
	// malformed relay costs this bus nothing durable — see validateRelayedShape.
	ErrInvalidRelayedMessage = errors.New("hub: the relayed message cannot be recorded in the shape it arrived in")

	// ErrInvalidBusPath reports the traversed-bus path of a relayed message that
	// is empty, malformed, or too long to record once this bus's hop is
	// appended. The path is the ONLY provenance a relayed record carries and it
	// is written into an append-only trail, so it is refused rather than
	// repaired. See Hub.relayedBusPath.
	ErrInvalidBusPath = errors.New("hub: invalid relayed bus path")

	// ErrBusPathLoop reports a relayed message whose arriving path ALREADY names
	// this bus. It is separate from ErrInvalidBusPath because it is a routing
	// condition rather than a malformed input: the path is well-formed and the
	// message has simply been here before, which a caller may want to count and
	// report differently. relay.CheckIncomingPath is the authority; this is the
	// same refusal at the moment the durable record is built, so appending our
	// hop can never fabricate a second visit.
	ErrBusPathLoop = errors.New("hub: the relayed bus path already contains this bus")

	// ErrInvalidBody reports a message body that is missing or over
	// store.MaxBodyBytes.
	ErrInvalidBody = errors.New("hub: invalid message body")

	// ErrInvalidRecipient reports a malformed recipient id — a string that is
	// not a well-formed fully-qualified "<bus-id>.<agent-id>" (invariant 2).
	ErrInvalidRecipient = errors.New("hub: invalid recipient id")

	// ErrInvalidIdempotencyKey reports a missing or malformed idempotency key.
	// A key is REQUIRED on every mutating call (invariant 10).
	ErrInvalidIdempotencyKey = errors.New("hub: invalid idempotency key")

	// ErrIdempotencyKeyReused reports the SAME key presented with a DIFFERENT
	// payload. That is a protocol violation, not a retry: invariant 10 requires
	// it be rejected, logged, and the offending client DISCONNECTED.
	//
	// Contrast the legitimate retry — same key, same payload — which never
	// produces this error: it returns the ORIGINAL result with Replayed set and
	// is not punished in any way.
	ErrIdempotencyKeyReused = errors.New("hub: idempotency key already used with a different payload")

	// ErrInvalidCursor reports a cursor that is not a cursor this bus issued to
	// this agent. See package cursor semantics in cursor.go.
	ErrInvalidCursor = errors.New("hub: invalid cursor")

	// ErrCapacity reports a bound reached: the applied-key table is full. It
	// fails CLOSED — the send is refused — because the alternative is evicting
	// a remembered key, which silently turns the next retry of it into a SECOND
	// message. A refused send is recoverable; a duplicated one is not.
	ErrCapacity = errors.New("hub: at a capacity limit")

	// ErrAgentQuota reports that ONE AGENT is at its fair share of the
	// applied-key table while that table is under pressure
	// (idem.ErrAgentQuota; see internal/idem/retention.go for the derivation).
	// The table is NOT full and no other agent is affected — the refusal is
	// self-inflicted by the agent it names, and it is keyed on a PROVEN
	// identity, so no third party can consume another agent's share.
	//
	// Every error carrying it ALSO satisfies errors.Is(err, ErrCapacity); see
	// agentQuotaError for why that is required rather than tidy.
	//
	// STATUS CODE CAVEAT, recorded here so it is not silently "fixed" in
	// passing: the HTTP layer maps ErrCapacity to 503 with a Retry-After, and
	// 503 ("the SERVER is overloaded") is arguably the wrong answer for a cap
	// that only this one client has reached — 429 is the better fit. That is
	// already tracked, across all three per-agent caps at once, by task
	// AUTH-1-FU-ACTIVECAP-RETRYAFTER. Do NOT change the status here: a per-cap
	// drive-by fix is how the three end up answering differently.
	ErrAgentQuota = errors.New("hub: at a per-agent fair-share limit")

	// ErrInvalidOp reports a mint asked for an operation this bus does not mint
	// sequences for. Only "send" and "broadcast" are minted; see parseMintOp
	// for why an unrecognised value is refused rather than defaulted.
	ErrInvalidOp = errors.New("hub: invalid mint operation")

	// ErrUnknownMint reports a send presenting an idempotency key that has no
	// outstanding sequence reservation on this bus.
	//
	// It is a ROUTINE, EXPECTED condition, not a fault, and the two ways to
	// reach it are both benign:
	//
	//   - the bus RESTARTED between the mint and the send. The mint table is
	//     deliberately in-memory (only the burned NUMBER is durable — see
	//     mint.go), so a restart invalidates every outstanding reservation.
	//   - the reservation EXPIRED (MintTTL).
	//
	// The remedy in both cases is the same and is safe: re-mint under the SAME
	// idempotency key, re-sign the fresh assignment, re-send. The old number
	// stays burned, leaving a gap, which internal/ids/sequence.go documents as
	// correct. It cannot double-apply: if the crash landed AFTER the message
	// became durable, the re-sent request carries the same key and the same
	// fingerprint, so internal/idem answers OutcomeRetry and returns the
	// ORIGINAL result before the mint is ever consulted (invariant 10).
	ErrUnknownMint = errors.New("hub: no outstanding sequence reservation for this idempotency key")

	// ErrMintMismatch reports a send whose message id or sequence is NOT the one
	// this bus minted for that idempotency key.
	//
	// Unlike ErrUnknownMint this is never routine: the client was handed an
	// assignment and presented a different one, which is either a client bug
	// splicing two operations together or an attempt to get a signature over a
	// number of the client's own choosing accepted. Invariant 1 is the whole
	// answer — a client-supplied id is input to be validated, never an identity
	// to be trusted — so the presented values are checked against the mint and
	// the mint wins.
	ErrMintMismatch = errors.New("hub: the message id or sequence does not match the reservation minted for this idempotency key")

	// ErrPoisoned reports a hub that has stopped accepting writes because an
	// internal invariant failed. See assertSeqFloorLocked for the condition that
	// sets it: a sequence number handed out ABOVE the durably-recorded sequence
	// floor, which would let a restart reissue that sequence (invariant 1). It
	// is a hard stop, never a retry: the fix is to restart with a correctly
	// derived sequence floor.
	ErrPoisoned = errors.New("hub: refusing to write: an id-authority invariant failed and this hub is poisoned")

	// ErrNotDurable reports a hub built without a durable write path. Nothing
	// may be acknowledged before it is durable (invariant 4), so a hub with
	// nowhere to write refuses to accept messages rather than serving from
	// memory alone.
	ErrNotDurable = errors.New("hub: no durable write path")
)

// agentQuotaError is the refusal returned when an agent is at its fair share of
// the applied-key table. It matches BOTH ErrAgentQuota and ErrCapacity under
// errors.Is, and BOTH matches are required:
//
//   - ErrAgentQuota is the precise fact, for a caller that wants to say WHICH
//     client to look at.
//   - ErrCapacity is the class the HTTP layer already maps
//     (internal/httpapi/messages.go: ErrCapacity -> 503 + Retry-After). That
//     mapping is outside this package's boundary, so an error that did not
//     satisfy errors.Is(err, ErrCapacity) would fall through the switch and the
//     route would silently degrade from a considered 503 to a generic 500 —
//     turning a "retry later" into "the server broke", for a condition that is
//     entirely routine.
//
// It is a TYPE with an Is method rather than a double fmt.Errorf("%w … %w")
// because go.mod pins go 1.19 and multi-%w wrapping arrived in go 1.20. Do not
// "simplify" it back to one wrap; whichever sentinel were dropped, one of the
// two consumers above breaks silently.
type agentQuotaError struct{ detail string }

// newAgentQuotaError builds the refusal with the same text shape a
// fmt.Errorf("%w: …", ErrAgentQuota) would have produced.
func newAgentQuotaError(format string, a ...interface{}) error {
	return &agentQuotaError{detail: fmt.Sprintf(format, a...)}
}

func (e *agentQuotaError) Error() string { return ErrAgentQuota.Error() + ": " + e.detail }

func (e *agentQuotaError) Is(target error) bool {
	return target == ErrAgentQuota || target == ErrCapacity
}
