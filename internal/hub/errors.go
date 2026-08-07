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
	// session, and a session can only exist for an enrolled agent. It is
	// checked anyway, because the roster is the hub's own view (see
	// Hub.NoteEnrolment) and a divergence between it and internal/auth's is
	// exactly the thing that must be loud rather than silently permissive.
	ErrUnknownSender = errors.New("hub: the sending agent is not on this bus's roster")

	// ErrUnknownRecipient reports a directed send to an agent this bus does not
	// know. The HTTP layer answers 404.
	ErrUnknownRecipient = errors.New("hub: unknown recipient")

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

	// ErrPoisoned reports a hub that has stopped accepting writes because an
	// internal invariant failed. See Hub.publish for the one condition that
	// sets it: a message whose sequence number came out ABOVE the WAL index
	// that carried it, which would let a restart reissue that sequence
	// (invariant 1). It is a hard stop, never a retry: the fix is to restart
	// with a correctly derived sequence floor.
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
