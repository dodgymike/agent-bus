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
)
