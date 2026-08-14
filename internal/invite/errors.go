package invite

import "errors"

// The sentinel errors of this package. A caller reacts to a failure with
// errors.Is against one of these, NEVER by matching error text — the text is
// diagnostic detail for an operator and is free to change. This is the
// convention internal/auth and internal/idem already follow.
//
// # THE SENTINELS ARE DELIBERATELY DISTINCT, AND THE HTTP LAYER MUST COLLAPSE
// # THEM
//
// An operator needs to know WHICH failure occurred — "the invite expired" and
// "that secret is wrong" require completely different responses — so this
// package keeps them apart. A CLIENT must not learn the difference: the set of
// answers below is an oracle for "does invite X exist" and "is invite X still
// live", which is exactly what an attacker enumerating invite ids wants.
//
// Collapsing them into ONE indistinguishable response on the wire is
// INVITE-HARDEN's task, not this one. Until it lands, any handler built on this
// package MUST map every one of ErrUnknownInvite, ErrExpired, ErrRevoked,
// ErrAlreadyRedeemed and ErrInvalidInviteID to the SAME status and the SAME
// body, and log the specific sentinel server-side only.
var (
	// ErrInvalidInviteID reports an invite id that fails ValidateInviteID:
	// empty, over MaxInviteIDLen bytes, or not matching InviteIDPattern. The
	// offending id is never echoed back when it is oversized.
	ErrInvalidInviteID = errors.New("invite: invalid invite id")

	// ErrInvalidRecord reports an invite Record that is not self-consistent: a
	// bad id, an absent secret digest, a zero or inverted validity window, a
	// state that disagrees with the fields present, an oversized bounded field,
	// or — on the way back IN — unknown JSON fields, trailing data or a
	// malformed digest.
	//
	// It is returned in BOTH directions on purpose. On the way out it runs
	// BEFORE the durable write, so a record that could not be stored fails the
	// operation with nothing written instead of being discovered at replay,
	// when the effect is already durable and every remaining option is bad. On
	// the way in it treats a record read off disk as untrusted input (invariant
	// 1) even though this server wrote it — because "this server wrote it" is
	// precisely what corruption disproves.
	ErrInvalidRecord = errors.New("invite: invalid invite record")

	// ErrUnknownInvite reports that no live invite answers to this id WITH this
	// secret. It covers BOTH "no such invite" and "wrong secret", and that
	// conflation is deliberate: distinguishing them would tell a caller holding
	// no secret at all whether an invite id exists.
	//
	// It is also what a DROPPED invite produces — expired past its window,
	// discarded at the capacity bound, or discarded at replay because the
	// record would not decode. Every drop makes the invite UNKNOWN, and an
	// unknown invite is REJECTED; see doc.go's fail-closed rule for why that
	// direction is safe here and is NOT safe in idem's applied-key table.
	ErrUnknownInvite = errors.New("invite: no such invite")

	// ErrExpired reports an invite whose ExpiresAt has passed. Expiry is a pure
	// predicate over the record and the clock (Record.Expired), never a stored
	// flag — see Record.State.
	ErrExpired = errors.New("invite: the invite has expired")

	// ErrRevoked reports an invite an operator revoked before it was redeemed.
	ErrRevoked = errors.New("invite: the invite has been revoked")

	// ErrAlreadyRedeemed reports a SPENT invite. Single use is exhausted: the
	// one redemption this invite authorised has already happened, and it is not
	// this key's.
	//
	// It is ALSO what Revoke returns for an already-redeemed invite, and that is
	// not an oversight: revocation does NOT reach an agent that already redeemed
	// (DECISIONS.md, E5). Cascading revocation of an enrolled agent's credential
	// is AUTH-4's job. Returning success here would give an operator a false
	// expectation of reach at exactly the moment they are trying to shut
	// something down.
	ErrAlreadyRedeemed = errors.New("invite: the invite has already been redeemed")

	// ErrKeyReuse reports the SAME idempotency key presented with a DIFFERENT
	// payload fingerprint (invariant 10). It is a PROTOCOL VIOLATION, not a
	// retry: the caller must REJECT it and LOG it, and MUST NOT DISCONNECT.
	//
	// # THE CONNECTION IS KEPT (invariant 10, NARROWED 2026-08-08)
	//
	// This doc said "disconnect" until the narrowing, which was made by user
	// decision after the behaviour was measured at the raw socket: same key +
	// different payload is rejected and logged with the connection KEPT, and only
	// replay of an already-accepted SIGNED MESSAGE disconnects.
	//
	// The argument is at its STRONGEST on the enrolment route, and it is the one
	// internal/httpapi/auth.go already makes for auth.ErrIdempotencyKeyReused:
	// /v1/enroll is UNAUTHENTICATED, so the socket identifies NO PRINCIPAL to
	// punish — the party disconnected is simply whoever owns that socket, which on
	// a shared address need not be the party that sent the request. And dropping
	// it destroys every other request pipelined on that connection, hitting an
	// honest client part-way through obtaining a credential, with no session yet
	// to fall back on. A merely BUGGY client reaches this line easily; that is the
	// first of the two questions invariant 10 requires before adding any
	// disconnect, and it answers itself here.
	//
	// It is a separate sentinel from ErrAlreadyRedeemed precisely because the
	// two demand opposite reactions — one is "you are too late", the other is
	// "you are misbehaving" — and because collapsing them would make a
	// legitimate retry (same key, same fingerprint) indistinguishable from an
	// attack in the server's own logs.
	ErrKeyReuse = errors.New("invite: idempotency key reused with a different payload")

	// ErrRedemptionInFlight reports that another LIFECYCLE TRANSITION for this
	// invite — a redemption between Begin and Commit/Abort, or a revocation
	// mid-write — is already in progress.
	//
	// It is returned even to a caller presenting the SAME key as the in-flight
	// redemption. A retry racing its own original cannot be answered until the
	// original resolves: answering it early would mean either inventing a result
	// the original has not produced yet, or starting a second redemption of a
	// single-use invite. Refusing is what makes concurrent double-redemption
	// impossible rather than merely unlikely; the client retries and gets the
	// original result once it exists.
	ErrRedemptionInFlight = errors.New("invite: another transition for this invite is already in flight")

	// ErrCapacity reports that the invite table is full at MaxInvites.
	//
	// NOTHING is evicted to make room, exactly as in idem.ErrCapacity. Here the
	// refusal is even easier to justify: an evicted invite is an UNKNOWN invite
	// and therefore an unredeemable one, so eviction could never produce a
	// second redemption — but it would silently make an operator's live invite
	// stop working, and a refused mint is loud and recoverable.
	ErrCapacity = errors.New("invite: the invite table is full")

	// ErrInvalidTTL reports a mint whose requested lifetime is negative or over
	// MaxTTL. An over-long TTL is REJECTED rather than silently clamped: quietly
	// issuing a shorter-lived credential than the operator asked for is how an
	// invite mysteriously stops working an hour before it is needed. A ZERO TTL
	// is not an error — it means "unset" and takes DefaultTTL, the zero-value
	// convention every options struct in this repo uses.
	ErrInvalidTTL = errors.New("invite: invalid invite lifetime")

	// ErrResultTooLarge reports a redemption Result over idem.MaxResultBytes.
	// The stored result is what a legitimate retry replays verbatim, and it is
	// the largest single term in the memory bound (retention.go), so it is
	// capped at the point of encoding rather than trusted to stay small.
	ErrResultTooLarge = errors.New("invite: redemption result is too large")

	// ErrNotDurable reports a Store built with no DurableLog being asked to
	// perform a durable operation (mint, revoke, redeem).
	//
	// It is a refusal and not a degraded mode ON PURPOSE. Single-use is the
	// property everything here rests on, and single-use held only in memory is
	// decorative: a restart would forget which invites were spent and every one
	// of them would be redeemable again. A Store with no log may still be
	// READ (Lookup) and REBUILT (Apply), because neither claims durability.
	ErrNotDurable = errors.New("invite: this invite store has no durable log")
)
