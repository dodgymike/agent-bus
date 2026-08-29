# IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations require one

| Field | Value |
| --- | --- |
| Public id | `3cac3349-a311-4c80-927f-5bb576295113` |
| Key | IDEM-1 |
| Epic | [IDEM](../epic.md) |
| Status | superseded |
| Priority | P1 |
| Component | core |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:14:13.654927+00:00 |
| Updated | 2026-08-02T13:17:00.410816+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestIdempotencyKeyContract ./internal/httpapi
```

## Status note

DUPLICATE-EPIC MERGE, 2026-08-02. Two spec-keeper agents were dispatched to file the IDEM epic (invariant 10) concurrently and both did: IDEM-1..9 (this set, keys reserved 13:05) and IDEM-10..17 (created 13:10). The task-key RESERVATION namespace did its job -- no two tasks share a key -- but reservations cannot prevent two agents writing the same EPIC, and 17 overlapping claimable tasks would have had two agents independently implementing the same applied-key store. Resolved by superseding IDEM-1..9 and keeping IDEM-10..17, for three reasons: (a) IDEM-10..17 gate more precisely against existing keys (DUR-1/2/3/6, AUTH-1/4, MSG-2/3, RELAY-1/2/3); (b) they escalate the durable applied-key store and its crash-injection test to P0 with an argument that matches why DUR-1/DUR-2/DUR-6 are P0; (c) the alternative -- cancelling another agent's tasks -- was outside this pass's ownership, whereas IDEM-1..9 are this pass's own to withdraw. Nothing was lost: the unique content of this task is preserved either in its counterpart or as an append-only note on it. SUPERSEDED BY: IDEM-10 (idempotency key format/validation/per-agent scope).

## Description

Implements the wire half of CLAUDE.md invariant 10 ("duplicate detection and idempotency, everywhere"), recorded in DECISIONS.md via commit bfe391c. FIRST TASK OF THE EPIC -- everything else depends on this contract, so land it before any dedupe logic. SPECIFY AND ENFORCE: (1) TRANSPORT -- one canonical way to carry the key (an `Idempotency-Key` request header is the conventional choice; if it goes in the body instead, say why and be consistent). One place, never two. (2) WHICH OPERATIONS -- every MUTATING operation: enrol, send, broadcast, leave, peer-enrol, and relay ingest. Read-only routes (/v1/agents, /v1/wait, /v1/messages, /healthz, /v1/info) do NOT take one. (3) MISSING KEY IS AN ERROR (4xx), never a server-generated substitute: silently minting a key per attempt would make every retry look new and quietly defeat the entire epic. (4) VALIDATION -- opaque to the server, but bounded: a documented max length (e.g. 128 bytes) and a restricted charset, rejected with a clear error otherwise. Invariant 1 applies with full force: the key is CLIENT-supplied, so it is input to VALIDATE and never an identity to trust -- it must NEVER become, seed, or be derivable into a message id, an agent id, or a sequence number, all of which stay server-minted. (5) SCOPE -- the dedupe identity is the tuple (authenticated caller's fully-qualified <bus-id>.<agent-id>, operation, key), NOT the bare key. Per-caller scoping is required for two reasons: two agents independently choosing "1" must not collide, and without it one agent can burn another's keys and suppress their real messages -- a trivial griefing attack. CALL OUT THE AWKWARD CASE: enrolment has no authenticated caller yet, so its key needs a different scope (the presented enrolment key, or bus-wide) -- decide it here, and hand IDEM-6 the answer. (6) PAYLOAD FINGERPRINT -- define the canonical hash of the request payload that is stored with the key, because invariant 10 turns on distinguishing same-key-same-payload (legitimate retry) from same-key-different-payload (protocol violation). Specify exactly which bytes are hashed, the same way SIGN-1 pins its signed bytes; an ambiguous fingerprint makes the distinction unreliable in both directions. Use crypto/sha256 (stdlib). Document all of it in PROTOCOL.md/CONTRACTS.md via IDEM-9.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [IDEM-2](../IDEM-2--1c6a5ef1/task.md)
- **blocks** [IDEM-4](../IDEM-4--d9c00d0d/task.md)
- **blocks** [IDEM-5](../IDEM-5--9631dfcb/task.md)
- **blocks** [IDEM-6](../IDEM-6--208c4fb5/task.md)
- **blocks** [IDEM-9](../IDEM-9--b0dc4a12/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [DUR-1](../../DUR/DUR-1--c51e1959/task.md) — DUR-1: WAL record framing + writer (done)
- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-6](../IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)
- [IDEM-9](../IDEM-9--b0dc4a12/task.md) — IDEM-9: Wrappers generate the key ONCE and reuse it on retry, + AGENT_PROTOCOL.md / PROTO… (superseded)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-3](../IDEM-3--e34f9c31/task.md) — IDEM-3: Bounded dedupe window -- retention policy, eviction, and the honest statement of… (superseded)
- [IDEM-4](../IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [IDEM-5](../IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [IDEM-6](../IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)
- [IDEM-7](../IDEM-7--1c490a08/task.md) — IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, co… (superseded)
- [IDEM-8](../IDEM-8--d1ecfc75/task.md) — IDEM-8: Proof suite -- a retried send produces exactly one message, including across a cr… (superseded)
- [IDEM-9](../IDEM-9--b0dc4a12/task.md) — IDEM-9: Wrappers generate the key ONCE and reuse it on retry, + AGENT_PROTOCOL.md / PROTO… (superseded)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
