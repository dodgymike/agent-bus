# IDEM-3: Bounded dedupe window -- retention policy, eviction, and the honest statement of what happens past the window

| Field | Value |
| --- | --- |
| Public id | `e34f9c31-e987-420e-bbae-17a7404ac151` |
| Key | IDEM-3 |
| Epic | [IDEM](../epic.md) |
| Status | superseded |
| Priority | P1 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:14:14.349662+00:00 |
| Updated | 2026-08-02T13:17:01.552941+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestDedupeWindowBound ./internal/store
```

## Status note

DUPLICATE-EPIC MERGE, 2026-08-02. Two spec-keeper agents were dispatched to file the IDEM epic (invariant 10) concurrently and both did: IDEM-1..9 (this set, keys reserved 13:05) and IDEM-10..17 (created 13:10). The task-key RESERVATION namespace did its job -- no two tasks share a key -- but reservations cannot prevent two agents writing the same EPIC, and 17 overlapping claimable tasks would have had two agents independently implementing the same applied-key store. Resolved by superseding IDEM-1..9 and keeping IDEM-10..17, for three reasons: (a) IDEM-10..17 gate more precisely against existing keys (DUR-1/2/3/6, AUTH-1/4, MSG-2/3, RELAY-1/2/3); (b) they escalate the durable applied-key store and its crash-injection test to P0 with an argument that matches why DUR-1/DUR-2/DUR-6 are P0; (c) the alternative -- cancelling another agent's tasks -- was outside this pass's ownership, whereas IDEM-1..9 are this pass's own to withdraw. Nothing was lost: the unique content of this task is preserved either in its counterpart or as an append-only note on it. SUPERSEDED BY: IDEM-11, which carries the bounded retention window in its title and scope.

## Description

GATED on IDEM-2. An applied-key store that never forgets grows without bound and eventually is the process's memory footprint and the WAL's replay time; one that forgets carelessly resurrects duplicates. This task bounds it and STATES THE RESULTING GUARANTEE HONESTLY. DELIVER: (1) the retention policy -- a time window, a count cap, or both; whichever is chosen, the bound must be provable and testable, not aspirational. (2) THE WINDOW MUST EXCEED THE MAXIMUM CLIENT RETRY HORIZON, or the guarantee is a lie in exactly the case that matters: a peer reconnecting after a long outage (RELAY-4's backoff ceiling) and a long-poll client resuming after a network partition are the realistic worst cases -- derive the number from them, do not pick a round one. (3) EVICTION MUST BE CONSISTENT ACROSS MEMORY AND DISK: evicting in memory while the record survives on disk (or the reverse) makes behaviour depend on whether a restart happened since, which is the worst kind of intermittent bug. State how eviction interacts with DUR-7 (snapshot/compaction) -- a snapshot must not silently reinstate evicted keys, nor drop live ones. (4) SAY PLAINLY IN PROTOCOL.md what happens to a retry that arrives AFTER its key is evicted: it is applied as a NEW operation and produces a second message. That is the true guarantee -- 'duplicates are suppressed within the retention window' -- and it must be documented as such rather than described as unconditional exactly-once, which the system does not and cannot provide. (5) Expose the current applied-key count/oldest-key age wherever CORE-5's inspect/metrics endpoint lands, so the bound is observable in production rather than assumed.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CORE-5](../../CORE/CORE-5--06c5b1f5/task.md) — CORE-5: Observability: metrics/inspect endpoint (follow-up) (superseded)
- [DUR-1](../../DUR/DUR-1--c51e1959/task.md) — DUR-1: WAL record framing + writer (done)
- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [DUR-7](../../DUR/DUR-7--ba6739e6/task.md) — DUR-7: Snapshot/compaction follow-up (bounds WAL replay time) (todo)
- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)
- [RELAY-4](../../RELAY/RELAY-4--5ac738b4/task.md) — RELAY-4: Peer-down retry/backoff (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-8](../IDEM-8--d1ecfc75/task.md) — IDEM-8: Proof suite -- a retried send produces exactly one message, including across a cr… (superseded)
- [IDEM-9](../IDEM-9--b0dc4a12/task.md) — IDEM-9: Wrappers generate the key ONCE and reuse it on retry, + AGENT_PROTOCOL.md / PROTO… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
