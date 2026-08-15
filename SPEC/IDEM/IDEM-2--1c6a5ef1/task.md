# IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the effect, rebuilt by WAL replay

| Field | Value |
| --- | --- |
| Public id | `1c6a5ef1-6e04-4b8f-97d6-6a9f3bd8d5b0` |
| Key | IDEM-2 |
| Epic | [IDEM](../epic.md) |
| Status | superseded |
| Priority | P1 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:14:14.028521+00:00 |
| Updated | 2026-08-02T13:17:00.907072+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestAppliedKeyDurability ./internal/store ./internal/wal -- includes a crash-injection case
```

## Status note

DUPLICATE-EPIC MERGE, 2026-08-02. Two spec-keeper agents were dispatched to file the IDEM epic (invariant 10) concurrently and both did: IDEM-1..9 (this set, keys reserved 13:05) and IDEM-10..17 (created 13:10). The task-key RESERVATION namespace did its job -- no two tasks share a key -- but reservations cannot prevent two agents writing the same EPIC, and 17 overlapping claimable tasks would have had two agents independently implementing the same applied-key store. Resolved by superseding IDEM-1..9 and keeping IDEM-10..17, for three reasons: (a) IDEM-10..17 gate more precisely against existing keys (DUR-1/2/3/6, AUTH-1/4, MSG-2/3, RELAY-1/2/3); (b) they escalate the durable applied-key store and its crash-injection test to P0 with an argument that matches why DUR-1/DUR-2/DUR-6 are P0; (c) the alternative -- cancelling another agent's tasks -- was outside this pass's ownership, whereas IDEM-1..9 are this pass's own to withdraw. Nothing was lost: the unique content of this task is preserved either in its counterpart or as an append-only note on it. SUPERSEDED BY: IDEM-11 (durable applied-key store, WAL-replayed, bounded retention).

## Description

GATED on IDEM-1. Invariant 10 says the server's memory of applied keys "survives restart (it is part of the recovered state, not an in-memory cache)" -- this task is that guarantee. THE ONE THING THAT MAKES IT CORRECT: the applied-key record MUST be committed in the SAME two-phase (prepare -> commit -> fsync) transaction as the effect it records. If the message commits and the key record does not, a crash in that window plus a client retry produces a DUPLICATE -- precisely the bug idempotency exists to prevent, and it would be invisible in normal testing because the window is small. Do not implement it as a separate write, and do not order it 'after' the effect. (2) STORE THE RESULT, NOT JUST THE KEY: a retry must return the ORIGINAL response (message id, sequence, timestamp), so the record holds the (caller, operation, key) tuple, the payload fingerprint from IDEM-1, the minted result, and the commit time. A key with no stored result cannot satisfy IDEM-4. (3) RESERVE the on-disk record-type number via POST /api/v1/projects/agent-bus/reservations {"namespace":"record-type"} -- never hand-pick it; that is the classic parallel-agent collision, and DUR-1's framing already has neighbours. Bump the on-disk format version the same way if the framing changes. (4) RECOVERY: replay on start rebuilds the applied-key map alongside the rest of the serving state (invariant 5: memory is the serving copy, disk is the truth); recovery must yield a state that is a prefix of accepted history, so a key whose effect was not committed must NOT appear as applied. (5) CRASH-INJECTION TEST IS MANDATORY per CLAUDE.md: kill between prepare and commit, and between commit and ack, then assert what a post-restart retry does. 'The code looks right' is not evidence for a durability claim.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [IDEM-1](../IDEM-1--3cac3349/task.md)
- **blocks** [IDEM-3](../IDEM-3--e34f9c31/task.md)
- **blocks** [IDEM-4](../IDEM-4--d9c00d0d/task.md)
- **blocks** [IDEM-7](../IDEM-7--1c490a08/task.md)
- **blocks** [IDEM-8](../IDEM-8--d1ecfc75/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [DUR-1](../../DUR/DUR-1--c51e1959/task.md) — DUR-1: WAL record framing + writer (done)
- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (todo)
- [IDEM-4](../IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (todo)
- [IDEM-3](../IDEM-3--e34f9c31/task.md) — IDEM-3: Bounded dedupe window -- retention policy, eviction, and the honest statement of… (superseded)
- [IDEM-4](../IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [IDEM-5](../IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [IDEM-6](../IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)
- [IDEM-7](../IDEM-7--1c490a08/task.md) — IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, co… (superseded)
- [IDEM-8](../IDEM-8--d1ecfc75/task.md) — IDEM-8: Proof suite -- a retried send produces exactly one message, including across a cr… (superseded)
- [IDEM-9](../IDEM-9--b0dc4a12/task.md) — IDEM-9: Wrappers generate the key ONCE and reuse it on retry, + AGENT_PROTOCOL.md / PROTO… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
