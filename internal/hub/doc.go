// Package hub owns message fan-out, idempotency and the long-poll waiter
// registry — the messaging core of the bus.
//
// # What it is responsible for
//
//   - MINTING the message sequence and message id (invariant 1: the server is
//     authoritative on every id, and an id is never reused, including across
//     restarts). See the floor derivation in Open, which is the load-bearing
//     part and is argued there in full.
//   - WRITING every message through the two-phase durable path and returning
//     only once it is committed and fsynced (invariant 4). One function,
//     publish, does this for broadcasts and directed messages alike, so the two
//     cannot drift apart in their durability, their idempotency or their
//     wake-up.
//   - REBUILDING the serving copy by replaying that durable log at startup
//     (invariant 5: memory is the serving copy, disk is the truth).
//   - REMEMBERING applied idempotency keys, durably — the applied-key RECORD
//     (internal/idem, IDEM-11) rides in the SAME two-phase transaction as the
//     message it belongs to and is restored by replay, so it is recovered state
//     and not an in-memory cache (invariant 10). The table is bounded by a
//     DERIVED retention window (idem.RetentionWindow) and by
//     MaxIdempotencyEntries, and its state is observable through
//     Hub.IdempotencyStats. The guarantee is "duplicates are suppressed within
//     the retention window", NOT unconditional exactly-once — see idem.Store's
//     doc comment for why fail-closed past the window is not implementable over
//     opaque client-supplied keys.
//   - PARKING long polls and releasing them on a new message, on the deadline,
//     or when the client's context is done.
//   - INGESTING a message relayed from a PEER BUS (IngestRelayed) down that
//     SAME publish path — same durability, same applied-key adjudication, same
//     wake-up. It is the only write entry whose sender is NOT one of this bus's
//     own agents, and it inverts exactly the checks that fact makes wrong: the
//     sender must NOT be ours (invariant 2 — a peer asserts ids only in its own
//     namespace), the sequence is minted internally because a peer bus holds no
//     reservation here, and the traversed bus path is recorded rather than
//     defaulted to this bus. It returns the idempotency outcome UNCOLLAPSED,
//     because the relay re-forwards on exactly one of the three (invariant 10).
//     It is the durable half of relay.LocalIngest and nothing more: signature
//     verification, loop detection and the onward hop belong to internal/relay,
//     which this package does not import.
//
// # What it deliberately does not do
//
// It does not authenticate. The caller passes the AUTHENTICATED principal in
// every request, taken from the request context by internal/httpapi, and this
// package never reads an identity from anywhere a client could choose it.
//
// It does not hold the authoritative roster either. That lives in internal/auth,
// which since AUTH-3 records every enrolment durably, and this package READS
// THROUGH to it via an injected hub.RosterSource rather than keeping a copy —
// see RosterSource for why a copy is a defect and not an optimisation, and for
// the failure it produced (a restarted bus that authenticated everyone and
// served nobody). The adapter from auth's roster to this one lives in
// cmd/agent-bus, the composition root; this package does not import
// internal/auth.
//
// # Delivery is AT-LEAST-ONCE
//
// One ordered stream plus a per-agent cursor, filtered by visibility. A message
// may be delivered more than once — a retry, a stale cursor, and later a cyclic
// relay topology all produce duplicates, which invariant 10 absorbs on the
// write side. Nothing here promises exactly-once and nothing may be documented
// as if it did.
package hub
