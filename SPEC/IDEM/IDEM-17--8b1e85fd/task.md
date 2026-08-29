# IDEM-17: Crash-injection test -- restart mid-retry-window still yields exactly one effect

| Field | Value |
| --- | --- |
| Public id | `8b1e85fd-e4db-43eb-b665-1b429fe66e98` |
| Key | IDEM-17 |
| Epic | [IDEM](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | test |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:10:36.069349+00:00 |
| Updated | 2026-08-14T23:01:45.721617+00:00 |
| Completed | 2026-08-14T23:01:45.721601+00:00 |

## Proof command

```sh
go test -race -count=1 -run TestIdemCrashInjectionRestart ./internal/idem/
```

## Status note

CODE-COMPLETE, NOT COMMITTED. Two files, both inside the feature-runner's boundary: internal/idem/crashinjection_test.go (new, 1153 lines) and internal/idem/doc.go (comment-only). Test-only change; no production code touched. proof_cmd corrected from a VACUOUS one naming ./internal/store/... ./internal/wal/... (zero tests, empty_pkgs=2) to ./internal/idem/; re-run verdict=PASS class=test exit=0 tests_run=11 top_level=8 skipped=1 failed=0 empty_pkgs=0. Gates COMPLETED: security PASS (upgraded from PASS-WITH-FINDINGS on re-verification of the final state, no new findings); reviewer PASS-WITH-NITS with all actionable nits fixed. Awaiting the integrator's commit; complete with that sha.

## Description

PRIORITY P0, matching DUR-6's own P0 for the identical reason: per CLAUDE.md's durability discipline, 'the code looks right' is not evidence for a durability claim, and this is IDEM-11's crash-injection test -- kept as its own task, separate from IDEM-16's functional suite, the same way DUR-6 is kept separate from the rest of the DUR epic. GATED on IDEM-11 (durable applied-key store) and reuses the DUR-3/DUR-6 crash-injection harness pattern rather than inventing a second one. Test shape: issue a mutating request (send/broadcast at minimum) carrying an idempotency key, kill the process at a chosen point in the write path -- at minimum BEFORE the applied-key record is committed, and separately AFTER it is committed but before the ack reaches the client (both are the interesting crash points, matching DUR-2's two-phase prepare/commit boundary) -- restart, replay the WAL, then retry the SAME request with the SAME key and payload. Assert exactly ONE effect survives regardless of which crash point was hit: if the crash was pre-commit, the post-restart retry is correctly treated as a FRESH operation (nothing was durably applied) and produces exactly one effect; if the crash was post-commit, the post-restart retry is recognized via the recovered applied-key store and returns the ORIGINAL result with no second effect. THE FAILURE MODE THIS TEST EXISTS TO CATCH: a crash landing between 'operation applied' and 'applied-key record durably written' that, on restart, forgets the key was ever used and lets a retry silently re-apply -- that is a torn record by invariant 10's own definition even though invariant 5's general prefix-of-history property might otherwise look satisfied by the rest of the state. This is exactly the kind of claim CLAUDE.md says an ordinary test suite cannot detect by inspection alone.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-3](../../DUR/DUR-3--d8a991ea/task.md) — DUR-3: Replay/recovery on start (done)
- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (in_progress)
- [IDEM-15](../IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)
- [IDEM-17-FU-CHILDNONCE](../IDEM-17-FU-CHILDNONCE--8b392400/task.md) — Per-run nonce to gate crash-injection self-SIGKILL children (repo-wide) (todo)
- [IDEM-17-FU-CROSSAGENT](../IDEM-17-FU-CROSSAGENT--0cd0ce79/task.md) — Crash-injection coverage for cross-agent applied-key isolation across recovery (todo)
- [IDEM-17-FU-PLACEMENT](../IDEM-17-FU-PLACEMENT--998e1c19/task.md) — Decide crash-suite package placement: internal/idem vs internal/hub (todo)
- [IDEM-18](../IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (in_progress)
- [IDEM-8](../IDEM-8--d1ecfc75/task.md) — IDEM-8: Proof suite -- a retried send produces exactly one message, including across a cr… (superseded)
- [MSG-5](../../MSG/MSG-5--9d125bc6/task.md) — MSG-5: Messaging durability integration test (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
