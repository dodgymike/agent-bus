package hub

import "errors"

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
