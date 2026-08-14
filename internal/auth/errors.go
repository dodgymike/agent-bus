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
	// retry. The HTTP layer answers 409 and logs it at warn level — and KEEPS
	// THE CONNECTION.
	//
	// THIS DOC SAID "DISCONNECTS THE OFFENDING CLIENT" UNTIL THE INVARIANT WAS
	// NARROWED ON 2026-08-08 (by user decision, after the behaviour was measured
	// at the raw socket). Do not reinstate it. An idempotency key is scoped to
	// the caller's OWN agent, so reuse is overwhelmingly a client that lost track
	// of its keys rather than an attacker; dropping the socket destroys every
	// other request pipelined on it, including that client's parked long-poll,
	// which lands an abuse defence on the party most likely to be honest. A
	// merely BUGGY client reaches this line easily — the first of the two
	// questions invariant 10 demands before ANY disconnect is added. See
	// httpapi.disconnect for the one case that still does disconnect: replay of
	// an already-accepted SIGNED message, which is a different error entirely.
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
	//
	// # On a DURABLE roster this is a safety net, not just an assertion
	//
	// The realistic way to reach it is a restart: the roster is recovered from
	// disk while the suffix counter is not, so the counter resumes from 1 and
	// re-mints an id the recovered roster already holds. WALRoster.Put checks
	// the RECOVERED map before writing and refuses, which is why the outcome is
	// a refused enrolment rather than a live identity rebound to a different
	// keypair. It FAILS CLOSED on purpose: a refused enrolment is recoverable,
	// a rebound identity is not.
	//
	// An operator seeing this repeatedly after a restart should not read it as
	// a roster bug. It is the signal that the suffix allocator is not durable —
	// build the minter on ids.OpenNameSuffixes, never ids.NewNameSuffixes.
	//
	// The residual gap, stated so it is known: an agent whose record was
	// DISCARDED at replay (undecodable, or removed by log repair) is not in the
	// recovered map, so its id is not protected by this check.
	ErrDuplicateAgentID = errors.New("auth: agent id already enrolled")

	// ErrInvalidRecord reports a durable enrolment record that is malformed:
	// unparseable JSON, a schema version this build does not understand, an
	// agent id that does not parse, a name that disagrees with the id, a
	// wrong-size key, or a certificate history past MaxCertBindings.
	//
	// It is raised in BOTH directions and means different things either way. On
	// the way OUT (Encode, before the durable write) it is a server-side bug and
	// the operation fails with nothing written: 500. On the way IN (Decode,
	// during recovery) it is CORRUPTION, and the record is discarded loudly and
	// the bus starts anyway — invariant 6 — so it never reaches a client at all.
	ErrInvalidRecord = errors.New("auth: invalid enrolment record")

	// ErrCertFingerprintBound reports an attempt to bind a client certificate
	// that is ALREADY live on a DIFFERENT agent id (MTLS-BIND).
	//
	// It is the certificate mirror of ErrDuplicateAgentID: that one keeps one
	// agent id from naming two keypairs, this one keeps one certificate from
	// naming two agents. Either collapse would make the identity it guards
	// meaningless, and both fail closed — a refused enrolment is recoverable, a
	// certificate that authenticates as two agents is not.
	//
	// Whether it is a client error or an internal breach depends on how it was
	// reached, so the HTTP layer does not treat it as either: on the enrolment
	// route it means someone presented a certificate another agent already
	// enrolled with, which is a 409 the client can act on by generating a fresh
	// keypair.
	ErrCertFingerprintBound = errors.New("auth: client certificate is already bound to another agent")

	// ErrCertBindingUnknown reports a client-certificate fingerprint that no
	// enrolled agent holds a live binding for. It is the ordinary negative
	// answer, not a malfunction: on this build most connections present no
	// certificate at all, and one that is presented but never enrolled is simply
	// unbound.
	ErrCertBindingUnknown = errors.New("auth: no agent is bound to this client certificate")

	// ErrCertBindingAmbiguous reports ONE client-certificate fingerprint held
	// live by MORE THAN ONE agent, which resolves to nobody.
	//
	// The live enrolment path refuses to create this state
	// (ErrCertFingerprintBound), so reaching it means the state came off DISK:
	// recovery replays records that are already durable and must not refuse them
	// (invariant 6). It is reported rather than resolved because picking a holder
	// would let one key holder be served as a definite agent it may not be —
	// precisely the credential confusion invariant 11 exists to prevent.
	ErrCertBindingAmbiguous = errors.New("auth: this client certificate is bound to more than one agent")

	// ErrNotAttached reports a durable roster asked to record an enrolment
	// before it was bound to a WAL (WALRoster.Attach). It is a startup-SEQUENCING
	// defect, never a client error: 500.
	//
	// It exists rather than a silent in-memory success because succeeding here
	// would acknowledge an enrolment that never reached disk — the exact false
	// durability claim WALRoster exists to remove (invariant 4).
	ErrNotAttached = errors.New("auth: durable roster is not attached to a write-ahead log")
)
