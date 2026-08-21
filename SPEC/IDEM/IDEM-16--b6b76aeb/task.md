# IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reuse-different-payload disconnect

| Field | Value |
| --- | --- |
| Public id | `b6b76aeb-76bc-47f7-9e58-6c95a601ae8c` |
| Key | IDEM-16 |
| Epic | [IDEM](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | test |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:10:35.845943+00:00 |
| Updated | 2026-08-02T13:24:07.443032+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestIdemRetryStorm|TestIdemConcurrentRace|TestIdemViolationDisconnect' ./internal/...
```

## Description

GATED on IDEM-12, IDEM-13, IDEM-14. Functional/concurrency coverage proving invariant 10's guarantees under `-race` (CLAUDE.md: concurrency here is the product, a data race is a P0). Required, each as its OWN named test so a future regression names exactly which property broke: (1) RETRY STORM -- fire N (e.g. 50) requests sharing one (agent, key, payload) and assert exactly ONE effect resulted: one sequence allocated, one audit record written, all N responses are byte-identical to the original result, and none of the N connections was disconnected. (2) CONCURRENT RACE -- run under `go test -race`, launching the identical-payload retries genuinely concurrently (goroutines released via a barrier, not serialized one after another) so the applied-key check-then-write path's OWN race safety is exercised, not just its logic in isolation; a naive check-then-insert without a lock/CAS looks correct read serially but double-applies under real concurrency, and this test must be able to catch that. (3) KEY-REUSE-DIFFERENT-PAYLOAD -- reuse an (agent, key) with a different payload and assert IDEM-14's full behaviour: rejection with the pinned status code, the security-event log entry, and the disconnect (whichever form IDEM-14 decided). STATE THE CARVE-OUT EXPLICITLY in the test names/comments so a future reader cannot miscopy the storm test's assertions into the disconnect test or vice versa. Exercise via the actual HTTP routes (send/broadcast at minimum; enrol/leave/peer-enrol if IDEM-13 landed first), not by calling internal functions directly, so this proves the wire behaviour the AGENTIF wrappers actually depend on. Kept separate from IDEM-17's crash-injection test the same way DUR's functional tests are kept separate from DUR-6.

--- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-8, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) ASSERT EXACTLY ONE OF EVERYTHING, NOT MERELY 'NO ERROR': one WAL record, one append-only audit entry (invariant 6), one delivery to the recipient, one sequence consumed. A test that only inspects the response body passes against an implementation that quietly writes two durable records. (b) ADD A RETRIED-BROADCAST CASE: each recipient receives it exactly once, including a recipient whose first-attempt delivery failed. (c) ADD A POST-VIOLATION INTEGRITY CASE: after IDEM-14 rejects and disconnects a key-reuse-with-different-payload attempt, the ORIGINAL message is still intact, still in history, and still deliverable -- a violation must not damage the operation it collided with. (d) ADD A PAST-THE-RETENTION-WINDOW CASE asserting IDEM-11's DOCUMENTED behaviour explicitly, so the honest boundary of the guarantee is pinned by a test rather than left to the reader. NOTE that IDEM-11 currently carries an unresolved contradiction about what that behaviour is (fail-closed vs applied-as-a-new-operation); write this test against whatever DECISIONS.md records, and do NOT write it against whichever one the implementation happens to do.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (todo)
- [IDEM-13](../IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-17](../IDEM-17--8b1e85fd/task.md) — IDEM-17: Crash-injection test -- restart mid-retry-window still yields exactly one effect (done)
- [IDEM-8](../IDEM-8--d1ecfc75/task.md) — IDEM-8: Proof suite -- a retried send produces exactly one message, including across a cr… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (todo)
- [IDEM-17](../IDEM-17--8b1e85fd/task.md) — IDEM-17: Crash-injection test -- restart mid-retry-window still yields exactly one effect (done)
- [IDEM-18](../IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (todo)
- [IDEM-8](../IDEM-8--d1ecfc75/task.md) — IDEM-8: Proof suite -- a retried send produces exactly one message, including across a cr… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
