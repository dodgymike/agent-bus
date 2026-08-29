# IDEM-6: Idempotent enrol, leave, and peer-enrol

| Field | Value |
| --- | --- |
| Public id | `208c4fb5-f45c-49e0-b8c5-f446fa294041` |
| Key | IDEM-6 |
| Epic | [IDEM](../epic.md) |
| Status | superseded |
| Priority | P2 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:14:15.188208+00:00 |
| Updated | 2026-08-02T13:17:03.088565+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestIdempotentEnrol ./internal/auth ./internal/httpapi
```

## Status note

DUPLICATE-EPIC MERGE, 2026-08-02. Two spec-keeper agents were dispatched to file the IDEM epic (invariant 10) concurrently and both did: IDEM-1..9 (this set, keys reserved 13:05) and IDEM-10..17 (created 13:10). The task-key RESERVATION namespace did its job -- no two tasks share a key -- but reservations cannot prevent two agents writing the same EPIC, and 17 overlapping claimable tasks would have had two agents independently implementing the same applied-key store. Resolved by superseding IDEM-1..9 and keeping IDEM-10..17, for three reasons: (a) IDEM-10..17 gate more precisely against existing keys (DUR-1/2/3/6, AUTH-1/4, MSG-2/3, RELAY-1/2/3); (b) they escalate the durable applied-key store and its crash-injection test to P0 with an argument that matches why DUR-1/DUR-2/DUR-6 are P0; (c) the alternative -- cancelling another agent's tasks -- was outside this pass's ownership, whereas IDEM-1..9 are this pass's own to withdraw. Nothing was lost: the unique content of this task is preserved either in its counterpart or as an append-only note on it. SUPERSEDED BY: IDEM-13 (idempotent enrol / leave / peer-enrol).

## Description

GATED on IDEM-1/IDEM-2. Invariant 10 covers EVERY mutating operation, not just messaging. ENROL is the interesting one: a retried enrolment must return the SAME server-minted agent id and the SAME credential -- it must not mint a second agent. Ids are never reused (invariant 1), so a double-applied enrolment burns an id and leaves a phantom agent in the roster that nothing will ever collect, and the client ends up holding a credential for an identity its peers were never told about. It is also the operation with NO authenticated caller yet, so it uses the alternative key scope IDEM-1 settled (the presented enrolment key, or bus-wide) -- implement exactly that, and make sure the scope cannot be abused by an unauthenticated caller to squat or probe keys. RE-ENROLMENT WITH A DIFFERENT PUBLIC KEY under the same idempotency key is a different-payload violation (IDEM-5), not a retry -- important, because it is also how an attacker would try to take over an identity. LEAVE (AUTH-4): naturally idempotent, but must return success rather than an error on a second call, and must not double-apply revocation side effects (key_epoch bumps in CRYPTO-4 -- a second bump would needlessly invalidate freshly-issued bundles). PEER-ENROL (RELAY-1): two buses enrolling each other concurrently, and a peer retrying after a timeout, must converge on ONE peering, not two half-configured ones. All three persist their applied-key records through IDEM-2's store so they survive restart, and all three keep working after roster recovery (AUTH-3).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [IDEM-1](../IDEM-1--3cac3349/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [AUTH-4](../../AUTH/AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (done)
- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [DUR-1](../../DUR/DUR-1--c51e1959/task.md) — DUR-1: WAL record framing + writer (done)
- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-13](../IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-5](../IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-13](../IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
