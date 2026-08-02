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
//   - REMEMBERING applied idempotency keys, durably — the key is part of the
//     durable record and is restored by replay, so it is recovered state and
//     not an in-memory cache (invariant 10).
//   - PARKING long polls and releasing them on a new message, on the deadline,
//     or when the client's context is done.
//
// # What it deliberately does not do
//
// It does not authenticate. The caller passes the AUTHENTICATED principal in
// every request, taken from the request context by internal/httpapi, and this
// package never reads an identity from anywhere a client could choose it.
//
// It does not hold the authoritative roster either. That lives in
// internal/auth; the view here is fed from the enrolment handler and has the
// same (process-only) lifetime — see (*Hub).NoteEnrolment for the argument and
// for what must change when AUTH-3 makes enrolment durable.
//
// # Delivery is AT-LEAST-ONCE
//
// One ordered stream plus a per-agent cursor, filtered by visibility. A message
// may be delivered more than once — a retry, a stale cursor, and later a cyclic
// relay topology all produce duplicates, which invariant 10 absorbs on the
// write side. Nothing here promises exactly-once and nothing may be documented
// as if it did.
package hub
