# Audit stored proof_cmds for the subtest-skip vacuous shape (parent-PASS/hidden-child-SKIP), catalogue any affected DONE tasks

| Field | Value |
| --- | --- |
| Public id | `932fe938-0e42-42d8-802d-ff018cb6c955` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T15:00:26.594715+00:00 |
| Updated | 2026-08-08T15:00:26.594715+00:00 |
| Completed | — |

## Proof command

```sh
grep -q 'PROOF_CMD SUBTEST-SKIP AUDIT' AGENT_LOG.md
```

## Description

Follow-up to cea09b96-72db-40f1-84b4-c2e227eae1cf (the tool fix: proof-check.sh's plain-text counter is column-0-anchored, so indented subtest '--- SKIP:' lines are invisible, letting a parent-PASS/all-children-SKIP test certify PASS instead of VACUOUS). That task fixes the TOOL. This task is about the DAMAGE: some already-`done` tasks' recorded proof_cmd may rest on exactly this shape, meaning the stored evidence for 'done' is weaker than the record implies.

Not hypothetical: in a randomly-selected batch of four tasks closed on 2026-08-08, three had wrong or non-existent proof commands (see PROCESS epic history, e.g. fc8cd234, a9a433dd).

DELIVERABLE: a list of tasks whose stored proof_cmd is vacuous under the corrected (post-cea09b96) rule -- NOT a re-opening of those tasks, and NOT a requirement to fix them. Record the list in a new dated section of AGENT_LOG.md headed exactly 'PROOF_CMD SUBTEST-SKIP AUDIT', naming every task_id examined and its verdict.

PRELIMINARY PASS already run (2026-08-08, spec-keeper, scoped and reported here so the next agent does not re-derive it from scratch):
  - Of 92 currently-`done` tasks with a non-null proof_cmd, 54 contain a `go test ... -run` invocation.
  - Of those 54, 39 have the SPECIFIC risk shape (tests_run > top_level, i.e. subtests exist, AND top-level skipped==0, i.e. any subtest skip would currently be invisible per the cea09b96 bug).
  - Re-ran all 39 with `go test -v` DIRECTLY (not nested through proof-check.sh -- nesting hits the known PROOF-CHECK-FU-RECURSION defect, task 69eb6f56, and corrupts results; confirmed this the hard way: an initial pass that nested proof-check.sh inside itself falsely reported ID-2-WIRING-SEAL's proof (8c9b6489) as FAILING with 5 failures, which evaporated to a clean PASS the moment the same proof_cmd was run WITHOUT nesting -- do not repeat that mistake) and grepped the raw verbose output for indented ('    --- SKIP:') lines.
  - RESULT: zero indented SKIP lines found across all 39 -- none of the currently-done tasks in this sample are resting on a hidden-skip false pass today.
  - One indented FAIL was observed once, transiently, under -race in 39318208 (CLI-2)'s TestEnrolFailedComposesRemedyAndStampsKey subtest; on immediate re-run it passed cleanly. This is NOT an instance of the cea09b96 defect -- unlike a subtest SKIP, a subtest FAIL DOES propagate to the parent's own --- result line and to the process exit code in Go's testing package, so proof-check.sh's existing 'RC != 0 => FAIL' check already catches it regardless of the indentation-counting bug. Flagging as pre-existing test flakiness for whoever owns internal/client's CLI enrol tests, not as a proof-tooling defect.

REMAINING SCOPE for whoever takes this: the other ~38 done tasks with a non-null proof_cmd that do NOT contain `go test -run` (doc/grep-shaped proofs, wrapper-shaped proofs, etc.) are OUT of this specific bug's blast radius by construction (no go test subtests) and do not need re-auditing under THIS rule -- but confirm that assumption rather than assuming it. Also worth re-running the 39-task sweep again AFTER cea09b96 lands, since the fixed tool may report the -json code path differently or reveal something the manual grep missed. Post the full per-task verdict list to AGENT_LOG.md under the heading below, and to this task's own notes.

proof_cmd confirmed RED on 2026-08-08 (heading does not exist yet, phrase absent from AGENT_LOG.md): grep -q 'PROOF_CMD SUBTEST-SKIP AUDIT' AGENT_LOG.md -> exit 1.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-2](../../CLI/CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [ID-2-WIRING-SEAL](../../ID/ID-2-WIRING-SEAL--8c9b6489/task.md) — ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half impleme… (done)
- [PROOF-CHECK-FU-RECURSION](../../TOOLING/PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) — PROOF-CHECK-FU-RECURSION: bash scripts/proof-check.sh hangs / spawns runaway processes wh… (todo)
- [a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd](../../TOOLING/Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) — Conjunction-masking vacuous-proof family: filtered-clause proof_cmds hidden by an unfilte… (todo)
- [cea09b96-72db-40f1-84b4-c2e227eae1cf](../../TOOLING/proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) — proof-check.sh: subtest SKIP/PASS lines invisible to plain-text counter -- parent-PASS/al… (todo)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
