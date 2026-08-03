package idem

import "errors"

// The sentinel errors of this package. A caller reacts to a failure with
// errors.Is against one of these, never by matching error text — the text is
// diagnostic detail for an operator and is free to change. See auth/errors.go
// for the same convention this package follows.
var (
	// ErrMissingKey reports that a mutating route was called with no
	// Idempotency-Key header at all (or an empty one). Per invariant 10 and
	// point 5 of doc.go, the server NEVER mints a substitute — this is always
	// a client error (400), never silently repaired.
	ErrMissingKey = errors.New("idem: idempotency key is required on this route")

	// ErrInvalidKey reports a key that is present but fails validation: over
	// MaxKeyLen bytes, or containing a byte outside KeyCharset. Client error
	// (400). The offending key is never echoed whole if it is oversized (see
	// ValidateKey).
	ErrInvalidKey = errors.New("idem: invalid idempotency key")

	// ErrInvalidAgent reports an empty agent id passed to NewAgentScope. A
	// Scope without an agent component is not a per-agent scope at all, so
	// this is refused rather than silently treated as bus-wide (only
	// NewEnrolScope may build a bus-wide Scope, and only for OpEnrol).
	ErrInvalidAgent = errors.New("idem: idempotency scope requires a non-empty agent id")

	// ErrInvalidOperation reports an Operation value that is not one of the
	// fixed constants this package defines. The Operation component of Scope
	// exists to keep one agent's reuse of a key across two different routes
	// from colliding with itself (doc.go point 3); an unrecognised operation
	// would defeat that by construction, so it is refused rather than
	// accepted as an opaque string.
	ErrInvalidOperation = errors.New("idem: invalid idempotency operation")

	// ErrInvalidRecord reports an applied-key Record that is not
	// self-consistent: an unknown operation, an invalid key, an agent id that
	// disagrees with the bus-wide-enrol discriminant, a zero commit time, or —
	// on the way back IN — unknown JSON fields, trailing data, or a malformed
	// fingerprint.
	//
	// It is returned in BOTH directions on purpose. On the way out it runs
	// BEFORE the durable write, so a record that could not be stored fails the
	// operation with nothing written instead of being discovered at replay,
	// when it is far too late. On the way in it treats a record read off disk
	// as untrusted input (invariant 1) even though this server wrote it —
	// because "this server wrote it" is exactly what corruption disproves.
	ErrInvalidRecord = errors.New("idem: invalid applied-key record")

	// ErrResultTooLarge reports a Record.Result over MaxResultBytes. The stored
	// result is what IDEM-12 replays verbatim to a retrying client, and it is
	// also the single largest term in the memory bound (see retention.go), so
	// it is capped at the point of encoding rather than trusted to stay small.
	ErrResultTooLarge = errors.New("idem: applied-key record result is too large")

	// ErrCapacity reports that the applied-key table is full at MaxEntries.
	//
	// NOTHING is evicted to make room. Evicting a live key silently turns its
	// next retry into a SECOND effect, which is the double-apply invariant 10
	// forbids; a refused operation is recoverable, a duplicated one is not.
	ErrCapacity = errors.New("idem: the applied-key table is full")
)
