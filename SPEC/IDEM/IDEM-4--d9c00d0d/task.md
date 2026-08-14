# IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result and produces no second message

| Field | Value |
| --- | --- |
| Public id | `d9c00d0d-051f-4111-a919-6b8c3f1e0576` |
| Key | IDEM-4 |
| Epic | [IDEM](../epic.md) |
| Status | superseded |
| Priority | P1 |
| Component | msg |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:14:14.605636+00:00 |
| Updated | 2026-08-02T13:17:02.092084+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestRetriedSendReturnsOriginal ./internal/httpapi ./internal/store
```

## Status note

DUPLICATE-EPIC MERGE, 2026-08-02. Two spec-keeper agents were dispatched to file the IDEM epic (invariant 10) concurrently and both did: IDEM-1..9 (this set, keys reserved 13:05) and IDEM-10..17 (created 13:10). The task-key RESERVATION namespace did its job -- no two tasks share a key -- but reservations cannot prevent two agents writing the same EPIC, and 17 overlapping claimable tasks would have had two agents independently implementing the same applied-key store. Resolved by superseding IDEM-1..9 and keeping IDEM-10..17, for three reasons: (a) IDEM-10..17 gate more precisely against existing keys (DUR-1/2/3/6, AUTH-1/4, MSG-2/3, RELAY-1/2/3); (b) they escalate the durable applied-key store and its crash-injection test to P0 with an argument that matches why DUR-1/DUR-2/DUR-6 are P0; (c) the alternative -- cancelling another agent's tasks -- was outside this pass's ownership, whereas IDEM-1..9 are this pass's own to withdraw. Nothing was lost: the unique content of this task is preserved either in its counterpart or as an append-only note on it. SUPERSEDED BY: IDEM-12 (idempotent send/broadcast, retries return the original result).

## Description

GATED on IDEM-1/IDEM-2. The core behaviour, on the paths that matter most (MSG-2 broadcast, MSG-3 send). SAME KEY + SAME PAYLOAD IS A LEGITIMATE RETRY -- the ack was probably lost in flight. Return the ORIGINAL result verbatim: the same message id, the same sequence, the same 2xx status. Do NOT re-apply, do NOT mint a new id or sequence, do NOT return an error or a 409, and do NOT disconnect. Invariant 10 is explicit that punishing this case would break exactly the clients doing the right thing; the disconnect rule belongs to IDEM-5's different-payload case and must not leak into this one. MARK THE RESPONSE so a caller can tell a replayed ack from a fresh one (a response field or header) -- useful for debugging and for the wrapper's logging, but the body must otherwise be identical. THE SUBTLE CASE, which is where implementations usually break: TWO CONCURRENT IN-FLIGHT REQUESTS WITH THE SAME KEY -- the client retried before the first ack landed, so the first operation is committed-in-progress and there is no stored result yet. A naive check-then-act double-applies. Handle it with a single-flight reservation on the key inside the same critical section that mints the sequence: the second caller either blocks and returns the first's result, or gets an explicitly retriable 'in progress' response -- pick one, document it, and TEST IT UNDER -race, since concurrency is this project's product. BROADCAST SPECIFICS: dedupe on the broadcast OPERATION, not per-recipient delivery -- a retried broadcast must not fan out a second time to anyone, including recipients whose delivery failed the first time. Interacts with SIGN-6: a message rejected for a missing/invalid signature was never applied, so its key is NOT recorded as applied and a corrected resend under the same key is a new operation, not a retry -- state this.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [IDEM-1](../IDEM-1--3cac3349/task.md)
- **blocked by** [IDEM-2](../IDEM-2--1c6a5ef1/task.md)
- **blocks** [IDEM-8](../IDEM-8--d1ecfc75/task.md)
- **relates to** [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md)

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
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (todo)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-5](../IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [MSG-3](../../MSG/MSG-3--2655c6ae/task.md) — MSG-3: POST /v1/send -- direct message (done)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (todo)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-5](../IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [IDEM-8](../IDEM-8--d1ecfc75/task.md) — IDEM-8: Proof suite -- a retried send produces exactly one message, including across a cr… (superseded)
- [IDEM-9](../IDEM-9--b0dc4a12/task.md) — IDEM-9: Wrappers generate the key ONCE and reuse it on retry, + AGENT_PROTOCOL.md / PROTO… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
