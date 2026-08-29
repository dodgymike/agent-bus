# IDEM-9: Wrappers generate the key ONCE and reuse it on retry, + AGENT_PROTOCOL.md / PROTOCOL.md / CONTRACTS.md

| Field | Value |
| --- | --- |
| Public id | `b0dc4a12-9814-4ca0-a110-a6d3d0701b6d` |
| Key | IDEM-9 |
| Epic | [IDEM](../epic.md) |
| Status | superseded |
| Priority | P1 |
| Component | agentif |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:14:16.107728+00:00 |
| Updated | 2026-08-02T13:17:04.645101+00:00 |
| Completed | — |

## Proof command

```sh
scripts/bus-send.sh forced to retry against a running throwaway bus produces exactly ONE message; grep -q 'Idempotency-Key' AGENT_PROTOCOL.md CONTRACTS.md
```

## Status note

DUPLICATE-EPIC MERGE, 2026-08-02. Two spec-keeper agents were dispatched to file the IDEM epic (invariant 10) concurrently and both did: IDEM-1..9 (this set, keys reserved 13:05) and IDEM-10..17 (created 13:10). The task-key RESERVATION namespace did its job -- no two tasks share a key -- but reservations cannot prevent two agents writing the same EPIC, and 17 overlapping claimable tasks would have had two agents independently implementing the same applied-key store. Resolved by superseding IDEM-1..9 and keeping IDEM-10..17, for three reasons: (a) IDEM-10..17 gate more precisely against existing keys (DUR-1/2/3/6, AUTH-1/4, MSG-2/3, RELAY-1/2/3); (b) they escalate the durable applied-key store and its crash-injection test to P0 with an argument that matches why DUR-1/DUR-2/DUR-6 are P0; (c) the alternative -- cancelling another agent's tasks -- was outside this pass's ownership, whereas IDEM-1..9 are this pass's own to withdraw. Nothing was lost: the unique content of this task is preserved either in its counterpart or as an append-only note on it. SUPERSEDED BY: IDEM-18, filed in this pass -- the ONLY part of IDEM-1..9 with no counterpart in IDEM-10..17.

## Description

GATED on IDEM-1/IDEM-4. Invariant 7: agents never hand-write HTTP, so the idempotency key is the wrappers' job, not the agent's. THE SINGLE MOST LIKELY WAY THIS EPIC SHIPS BROKEN: a wrapper that generates a FRESH key on every attempt. Every retry then looks like a new operation, the server dedupes nothing, and the whole epic is dead weight while every test that only exercises the server keeps passing. So: each scripts/bus-*.sh mutating wrapper (bus-enrol, bus-send, bus-broadcast, bus-leave, bus-peer) generates ONE key per logical operation, holds it for the entire retry loop, and reuses it verbatim on every attempt -- and there is a test that FORCES a retry (kill/refuse the first attempt) and asserts one message resulted. Key generation must be a real random id (no PIDs, no timestamps, no counters that reset -- all of which collide across restarts and processes). DOCUMENT: AGENT_PROTOCOL.md -- agents call the wrapper and do NOT craft keys themselves; what a replayed-ack response means; what a disconnect means and that reconnecting with the SAME key is correct while reusing it for different content is a protocol violation that will disconnect them again. PROTOCOL.md -- the header, the scope tuple, the payload fingerprint, and IDEM-3's retention window stated honestly as the boundary of the guarantee. CONTRACTS.md -- the header, every new error code, the record type IDEM-2 reserved, and any new flag/env var for the retention bound. Verify through the wrappers against a running throwaway bus with its own data dir under /tmp, not hand-written curl -- if the wrapper doesn't retry idempotently, the feature doesn't work.

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
- [DUR-1](../../DUR/DUR-1--c51e1959/task.md) — DUR-1: WAL record framing + writer (done)
- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-18](../IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (in_progress)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-3](../IDEM-3--e34f9c31/task.md) — IDEM-3: Bounded dedupe window -- retention policy, eviction, and the honest statement of… (superseded)
- [IDEM-4](../IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
