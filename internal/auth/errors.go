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

	// ErrInviteRequired reports an enrolment that presented NO invite to a
	// service configured for invite-only enrolment (Options.RequireInvite,
	// invariant 3). Client error: 403.
	//
	// # 403, not 401 and not 503
	//
	// 401 would be wrong: there is no authentication scheme the caller could
	// retry under, and no credential it could present on THIS request that would
	// change the answer. 503 would be worse — it is what ErrCapacity returns, it
	// carries Retry-After, and it invites a client to hammer a route that will
	// never accept it. 403 says exactly what is true: the request was understood,
	// it is refused, and retrying it unchanged is pointless. What the caller must
	// do is get an invite from the operator, which is not a protocol action.
	//
	// # It is NOT a disconnect, and this is invariant 10's two questions answered
	//
	// Can a merely BUGGY client reach this line? Yes, trivially and constantly:
	// every agent built against the pre-gate bus reaches it on its first call
	// after an operator turns the gate on, as does any client whose invite file
	// is missing, misspelt or already spent. Does this connection carry only ONE
	// principal's traffic? No — /v1/enroll is unauthenticated by construction
	// (invariant 3 lists it as one of the three routes that necessarily cannot
	// authenticate), so the socket identifies no principal to punish and on a
	// shared address is not even necessarily the offending party's. The refusal
	// is therefore an ordinary status code, logged, with the connection KEPT.
	//
	// Re-presenting a SPENT invite does not reach here at all: that is refused by
	// internal/invite, and a re-presented invite with the same idempotency key
	// and the same payload is a legitimate RETRY that replays the original 201
	// (invariant 10) rather than any kind of refusal.
	ErrInviteRequired = errors.New("auth: enrolment requires an invite")

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
	//
	// It is shared by OperatorRegistry.Add and .Revoke, which make the identical
	// claim about the identical hazard.
	ErrNotAttached = errors.New("auth: durable roster is not attached to a write-ahead log")

	// ---------------------------------------------------------------------
	// The OPERATOR/ADMIN PRINCIPAL sentinels (AUTH-10).
	//
	// They are SEPARATE FROM THE AGENT SENTINELS ABOVE and must stay separate,
	// for the reason OperatorPrincipal is a separate type: if an admin route
	// reused AGENT authentication, an AGENT credential would authorise minting
	// the credentials that CREATE AGENTS — any enrolled agent could mint itself
	// an unlimited supply of new identities, collapsing invariant 3. A caller
	// that matched ErrUnknownAgent to decide an ADMIN question would be one
	// errors.Is away from the same confusion, so the two planes do not share a
	// sentinel anywhere the answer differs.
	//
	// The three that DO stay shared are shared on purpose, because the fact
	// being reported is identical on both planes and the remedy is identical
	// too: ErrUnknownSession (a token that is unknown, expired or in the wrong
	// state), ErrBadSignature, ErrInvalidPublicKey and ErrCapacity.
	// ---------------------------------------------------------------------

	// ErrUnknownOperator reports an operator id that is malformed, or
	// well-formed but not in the registry. The two are deliberately not
	// distinguished: 404.
	//
	// EVERY AGENT ID REACHES THIS, and that is the point rather than a side
	// effect — an agent id cannot parse as an operator id (ParseOperatorID), so
	// an enrolled agent asking for an operator challenge is refused before the
	// registry is consulted at all.
	ErrUnknownOperator = errors.New("auth: unknown operator")

	// ErrOperatorRevoked reports an operator that exists and has been revoked.
	//
	// It is DISTINCT from ErrUnknownOperator because the two are distinguishable
	// anyway to the only party that can reach them — the holder of that
	// operator's certificate private key, who necessarily knows the principal
	// existed — and because an operator who has been revoked and is told
	// "unknown" will waste an incident re-checking their id.
	ErrOperatorRevoked = errors.New("auth: operator is revoked")

	// ErrOperatorCertMismatch reports invariant 11's cross-check FAILING: the
	// client certificate on this connection is not the one bound to the operator
	// the request names. It also covers the absent certificate (the zero
	// fingerprint), which names nobody and is never a pass.
	//
	// This is the sentinel that says the two credentials disagree — mTLS proves
	// which key holder is on the connection, the session token is the revocable
	// application credential, and a token presented over somebody else's
	// connection is stronger evidence of theft than either mechanism alone can
	// produce.
	ErrOperatorCertMismatch = errors.New("auth: the presented client certificate does not belong to this operator")

	// ErrOperatorCertUnknown reports a client-certificate fingerprint that no
	// LIVE operator holds. It is the ordinary negative answer, not a
	// malfunction: almost every connection to this bus is an agent's.
	ErrOperatorCertUnknown = errors.New("auth: no operator is bound to this client certificate")

	// ErrOperatorCertAmbiguous reports ONE client-certificate fingerprint held
	// LIVE by MORE THAN ONE operator, which resolves to nobody.
	//
	// OperatorRegistry.Add refuses to create this state, so reaching it means it
	// came off DISK: Apply replays records that are already durable and must not
	// refuse them (invariant 6). It is reported rather than resolved because
	// picking a holder would serve one key holder as a definite ADMIN it may not
	// be — the credential confusion invariant 11 exists to prevent, on the plane
	// where it is most expensive.
	ErrOperatorCertAmbiguous = errors.New("auth: this client certificate is bound to more than one operator")

	// ErrOperatorCertBound reports an attempt to bind a client certificate that
	// is ALREADY live on a DIFFERENT operator. It is the operator-plane mirror
	// of ErrCertFingerprintBound: that one keeps one certificate from naming two
	// agents, this one keeps it from naming two operators.
	ErrOperatorCertBound = errors.New("auth: client certificate is already bound to another operator")

	// ErrDuplicateOperatorID reports an attempt to register an operator id that
	// is already present. Operator ids are never reused (invariant 1), and the
	// suffix is 16 characters of base32 over crypto/rand, so this is either an
	// astronomically unlikely collision or a replayed command — and in both
	// cases refusing is right, because overwriting would rebind a live ADMIN
	// identity to a different keypair.
	ErrDuplicateOperatorID = errors.New("auth: operator id already registered")

	// ErrInvalidOperatorRecord reports a durable operator record that is
	// malformed: unparseable JSON, a schema version this build does not
	// understand, an id that does not parse, a name that disagrees with the id,
	// the ZERO certificate fingerprint (which is the absence of a certificate),
	// an oversized label, or a revocation with no instant or no reason.
	//
	// Like ErrInvalidRecord it is raised in BOTH directions and means different
	// things either way: on the way OUT it is a bug and the operation fails with
	// nothing written; on the way IN it is CORRUPTION, and the record is
	// discarded loudly and the bus starts anyway (invariant 6).
	ErrInvalidOperatorRecord = errors.New("auth: invalid operator record")

	// ErrInvalidOperatorName reports a requested operator name that
	// ValidateOperatorName rejected: empty, oversized, or outside
	// OperatorNamePattern. Client error: 400.
	ErrInvalidOperatorName = errors.New("auth: invalid operator name")
)
