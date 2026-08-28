# HANDOVER-BACKLOG-RECONCILE: the inherited backlog stops lying about what is in flight

| Field | Value |
| --- | --- |
| Public id | `43d14776-44af-4f48-a1c7-1f279166ae61` |
| Key | HANDOVER-BACKLOG-RECONCILE |
| Epic | [HANDOVER](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T14:49:26.994053+00:00 |
| Updated | 2026-08-08T14:49:26.994053+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/export?format=json" | python3 -c "import json,sys; t=json.load(sys.stdin)[\"tasks\"]; n=[x for x in t if x[\"status\"]==\"in_progress\"]; print(len(n)); sys.exit(0 if len(n)<=3 else 1)"'
```

## Description

Audience: maintainer. Priority P2 -- FILED BUT DELIBERATELY OFF THE HANDOVER CRITICAL PATH (planner's explicit recommendation; see also disagreement (f) in the planner's notes).

Justification: 15 tasks sit in_progress, several already shipped. A recipient cannot tell what is being worked on. But the fix is large and the instrument (proof-check.sh) is itself broken in ways that would produce confidently wrong reconciliation.

WHY THIS IS OFF THE CRITICAL PATH (record explicitly, per planner + user instruction):
- It is blocked on two tooling fixes (521d68b5, a9a433dd) -- reconciling against a broken evidence instrument produces confidently wrong results.
- It MUTATES SHARED TASK STATE that P0 work depends on.
- It COMPETES FOR SPEC-KEEPER, the single agent permitted to mutate task state.
- A SEPARATE AUDIT of the 15 in_progress tasks is running right now (as of filing, 2026-08-08) and covers the cheap half of this work -- do not duplicate it.

SPLIT POINT if attempted (task is over a day -- FLAG): split by epic (DUR/MTLS/IDEM/other).

Definition of done: each of the 15 in_progress tasks is either completed with a real commit_sha and a quoted proof-check.sh verdict, or reset to todo with a status_note stating precisely what remains; SPEC.md mirror refreshed. spec-keeper owns this -- it is the only agent permitted to mutate task state.

Depends on: 521d68b5 (proof-check cannot distinguish executed from asserted) and a9a433dd (conjunction-masking vacuous proofs). Related but distinct: fc8cd234 backfills MISSING proof_cmds; this reconciles STATUS. A third concern -- that 3 of 4 sampled stored proof_cmds were WRONG -- is a separate sweep and should be its own task filed after 521d68b5 lands, not smuggled in here.

Parallel-safe: NO (mutates shared task state).

Model: sonnet, driven by spec-keeper. Size: over a day -- FLAG (see split point above).

RED verification observed (2026-08-08): confirmed via a REAL check (not file-absence) -- ran the exact proof_cmd's python check against the live Spec Server export: currently 15 tasks have status=in_progress, which is > 3, so the proof correctly exits 1 (RED) today.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [521d68b5-4181-4df6-b3c2-ef660ff5461d](../../TOOLING/proof-check.sh-cannot-tell-executed-from-asserted-adopt--521d68b5/task.md) — proof-check.sh cannot tell "executed" from "asserted" -- adopt a zero-probe guard convent… (todo)
- [a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd](../../TOOLING/Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) — Conjunction-masking vacuous-proof family: filtered-clause proof_cmds hidden by an unfilte… (todo)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [315899be-dd43-4462-baf4-eae2fd94364b](../../PROCESS/scripts-backlog-drift.sh-read-only-detector-listing-in_p--315899be/task.md) — scripts/backlog-drift.sh: read-only detector listing in_progress/todo tasks whose stored… (todo)
- [7befde72-488e-4cf4-a05b-b16e2c2ffd15](../../PROCESS/Integrator-flips-the-task-to-done-atomically-after-a-suc--7befde72/task.md) — Integrator flips the task to done atomically after a successful commit -- close the commi… (todo)
- [HANDOVER-REGISTER](../HANDOVER-REGISTER--7fddae9d/task.md) — HANDOVER-REGISTER: KNOWN_ISSUES.md, the known-defect register (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
