package auth

import "errors"

// The sentinel errors of this package. Every failure a caller has to react to
// differently wraps exactly one of them, so the HTTP layer maps a failure to a
// status code with errors.Is and NEVER by matching on error text — the text is
// diagnostic detail for the log and is free to change.
//
// The detail wrapped around a sentinel is written for an OPERATOR reading the
// server log, not for the client: it may name the agent, the requested name or
// the reason a validation failed. It never contains a token, a signature or a
// key.
var (
	// ErrInvalidName reports a requested agent name that ids.ValidateAgentName
	// rejected. Client error: 400.
	ErrInvalidName = errors.New("auth: invalid agent name")

	// ErrInvalidPublicKey reports a public key that is absent, empty or not
	// exactly ed25519.PublicKeySize bytes.
	//
	// This is the sentinel behind the length check that MUST precede every
	// ed25519.Verify call in this package: ed25519.Verify PANICS on a
	// wrong-size public key (it returns false for a wrong-size SIGNATURE, which
	// is what makes the trap so easy to miss), so an unchecked client-supplied
	// key is a remote denial of service. Client error: 400.
	ErrInvalidPublicKey = errors.New("auth: invalid public key")

	// ErrInvalidIdempotencyKey reports an idempotency key that is empty, too
	// long, or contains a byte outside [A-Za-z0-9._-]. Client error: 400.
	ErrInvalidIdempotencyKey = errors.New("auth: invalid idempotency key")

	// ErrIdempotencyKeyReused reports the SAME idempotency key presented with a
	// DIFFERENT payload. Per invariant 10 this is a protocol violation, not a
	// retry: the client is either seriously broken or hostile. The HTTP layer
	// answers 409, logs it at warn level and DISCONNECTS the offending client.
	//
	// Note what this is not: same key + same payload is a legitimate retry of a
	// call whose acknowledgement was probably lost, and returns the original
	// result with no error at all. Collapsing those two cases would punish
	// exactly the clients that retry correctly.
	ErrIdempotencyKeyReused = errors.New("auth: idempotency key reused with a different payload")

	// ErrCapacity reports that an in-memory table is full and the operation was
	// refused. Every bound in this package FAILS CLOSED — it never evicts a
	// record whose loss would let an already-applied operation be applied a
	// second time. Server-side, retryable: 503 with Retry-After.
	ErrCapacity = errors.New("auth: capacity limit reached")

	// ErrUnknownAgent reports an agent id that is malformed, or well-formed but
	// not in the roster. The two are deliberately not distinguished to the
	// client: 404.
	ErrUnknownAgent = errors.New("auth: unknown agent")

	// ErrUnknownSession reports a session token that is unknown, expired, or
	// not in the state the call requires. "Never existed" and "existed and
	// expired" are deliberately indistinguishable from the outside — a caller
	// holding a token it did not obtain learns nothing from the difference.
	// Client error: 404.
	ErrUnknownSession = errors.New("auth: unknown or expired session")

	// ErrBadSignature reports a signature that is the wrong length or does not
	// verify against the agent's recorded public key. Client error: 401.
	ErrBadSignature = errors.New("auth: signature does not verify")

	// ErrDuplicateAgentID reports an attempt to record a roster entry for an
	// agent id that is already present. Agent ids are never reused (invariant
	// 1), so this is an internal invariant breach — a minter handing out a
	// suffix twice — not a client error: 500.
	ErrDuplicateAgentID = errors.New("auth: agent id already enrolled")
)
