# IDEM-8: Proof suite -- a retried send produces exactly one message, including across a crash and under concurrency

| Field | Value |
| --- | --- |
| Public id | `d1ecfc75-1614-49b9-a8bc-25585a75d7f6` |
| Key | IDEM-8 |
| Epic | [IDEM](../epic.md) |
| Status | superseded |
| Priority | P1 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:14:15.808055+00:00 |
| Updated | 2026-08-02T13:17:04.165065+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestExactlyOnce ./internal/... -- one subtest per scenario, each asserting exactly one durable record
```

## Status note

DUPLICATE-EPIC MERGE, 2026-08-02. Two spec-keeper agents were dispatched to file the IDEM epic (invariant 10) concurrently and both did: IDEM-1..9 (this set, keys reserved 13:05) and IDEM-10..17 (created 13:10). The task-key RESERVATION namespace did its job -- no two tasks share a key -- but reservations cannot prevent two agents writing the same EPIC, and 17 overlapping claimable tasks would have had two agents independently implementing the same applied-key store. Resolved by superseding IDEM-1..9 and keeping IDEM-10..17, for three reasons: (a) IDEM-10..17 gate more precisely against existing keys (DUR-1/2/3/6, AUTH-1/4, MSG-2/3, RELAY-1/2/3); (b) they escalate the durable applied-key store and its crash-injection test to P0 with an argument that matches why DUR-1/DUR-2/DUR-6 are P0; (c) the alternative -- cancelling another agent's tasks -- was outside this pass's ownership, whereas IDEM-1..9 are this pass's own to withdraw. Nothing was lost: the unique content of this task is preserved either in its counterpart or as an append-only note on it. SUPERSEDED BY: IDEM-16 (exactly-once functional/concurrency suite) + IDEM-17 (crash-injection).

## Description

GATED on IDEM-2/IDEM-4/IDEM-5 (may be written in parallel against them). Invariant 10 is a correctness claim, and a correctness claim without a test that would FAIL if it were violated is a slogan. Every scenario asserts EXACTLY ONE of everything -- one WAL record, one audit-log entry, one delivery to the recipient, one sequence consumed -- not merely 'no error'. REQUIRED SCENARIOS, each its own named test so a regression names the property that broke: (1) SIMPLE RETRY -- send, ack lost, resend with the same key and payload: one message, and the second response is byte-identical to the first. (2) CRASH BETWEEN EFFECT AND ACK -- crash-injection per CLAUDE.md: kill the server after the message commits but before the client sees the ack, restart, replay, then retry with the same key: still one message. THIS IS THE TEST THAT PROVES IDEM-2's same-transaction claim; without it that claim is unverified. (3) CRASH BETWEEN PREPARE AND COMMIT -- retry after restart produces exactly one message and recovery is a prefix of accepted history (invariant 5). (4) CONCURRENT DUPLICATES -- N goroutines fire the same key simultaneously under -race: one message, N identical responses, no data race. (5) KEY REUSE WITH DIFFERENT PAYLOAD -- rejected and disconnected (IDEM-5), and, importantly, the ORIGINAL message is still intact and deliverable afterwards. (6) PAST THE RETENTION WINDOW -- assert IDEM-3's documented behaviour explicitly, so the honest boundary of the guarantee is pinned by a test rather than left to the reader. (7) BROADCAST -- a retried broadcast delivers to each recipient exactly once. Table-driven where it helps; keep the narrowest check runnable in seconds.

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
- [DUR-1](../../DUR/DUR-1--c51e1959/task.md) — DUR-1: WAL record framing + writer (done)
- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)
- [IDEM-17](../IDEM-17--8b1e85fd/task.md) — IDEM-17: Crash-injection test -- restart mid-retry-window still yields exactly one effect (done)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-3](../IDEM-3--e34f9c31/task.md) — IDEM-3: Bounded dedupe window -- retention policy, eviction, and the honest statement of… (superseded)
- [IDEM-4](../IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [IDEM-5](../IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
