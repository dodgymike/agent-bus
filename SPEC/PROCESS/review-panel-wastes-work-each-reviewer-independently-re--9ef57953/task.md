# review panel wastes work: each reviewer independently re-runs the full \`go test -race ./...\` suite instead of sharing one result

| Field | Value |
| --- | --- |
| Public id | `9ef57953-e746-4f7b-90b5-f8ddaee087f3` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | done |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T13:01:17.101784+00:00 |
| Updated | 2026-08-22T13:07:16.973996+00:00 |
| Completed | 2026-08-22T13:07:16.973980+00:00 |

## Proof command

```sh
bash scripts/doc-check.sh section .claude/agents/feature-runner.md "## Verify — and tell the truth" "Run the full \`-race\` suite ONCE per task" && bash scripts/doc-check.sh section .claude/agents/reviewer.md "## Fanning out (you have the Agent tool)" "Do NOT run the whole test suite"
```

## Description

BUG (reported by user, 2026-08-22): on one task, security, architecture-reviewer and
reliability-reviewer EACH ran the full `go test -race ./...` suite independently. Nothing told them a
suite result already existed for the task/panel, and each agent's instructions said it "prefers
proof," so each re-ran the whole suite from scratch. N reviewers on one task = N-1 wasted full
`-race` runs.

FIX (7 files, additions only, zero deletions):
- `.claude/agents/feature-runner.md` — under `## Verify — and tell the truth`: run the full
  `go test -race ./...` suite ONCE per task, either feature-runner itself or ONE dedicated
  sub-agent (test-engineer), before dispatching the review panel. Capture the command, the sha (or
  `working-tree @ <sha>`), and the pass/fail summary, and paste that verbatim into EVERY reviewer's
  brief. If code changes after the suite ran, RE-run it and update the briefs before dispatch.
- `.claude/ORCHESTRATION.md` — under `## Review panel (full-system review)`: the same rule for the
  standalone review-panel/audit convening path (the convener or one dedicated sub-agent runs the
  suite once against the exact tree the panel sees and pastes command/sha/pass-fail into every
  panelist's brief).
- `.claude/agents/security.md`, `.claude/agents/reliability-reviewer.md`,
  `.claude/agents/architecture-reviewer.md`, `.claude/agents/performance-reviewer.md`,
  `.claude/agents/reviewer.md` — each gets a bullet, under `## Fanning out (you have the Agent
  tool)`, right before "Verify before you relay": "Do NOT run the whole test suite" (worded
  per-file for that reviewer's dimension), stating the brief already carries a full
  `go test -race ./...` result (command, sha, pass/fail) that the orchestrator ran once for the
  panel, and that the reviewer must trust it and run only its own dimension's narrow checks, never
  `./...`. Each carries the same safe escape hatch: re-run the SPECIFIC test (never `./...`) if the
  result is absent, HEAD has moved since it ran, or the reviewer has a concrete reason to distrust
  it.

CLASSIFICATION: every changed path is under `.claude/**`, which is CONTROL PLANE per CLAUDE.md
"Agent roster" (it decides WHAT is checked / how review is convened) — it does not qualify for the
docs-and-tests security carve-out even though no product code moved. Both reviewer AND security
gates ran on this change.

SCOPE CHECK: `git diff --stat` over the 7 files shows 48 insertions(+), 0 deletions(-) — additions
only, confirmed 2026-08-22 against the working tree ahead of HEAD 7de9bd1.

STATUS: implementation is in the working tree right now (not yet committed) — filed as `todo` per
the requester; they will run `complete` themselves with the commit sha once it lands.

PROOF, verified RED at HEAD 7de9bd1 in a clean `git archive HEAD` overlay (not the live working
tree, which already has the uncommitted fix) on 2026-08-22:
  $ T=$(mktemp -d); git archive HEAD | tar -x -C "$T"; cd "$T"
  $ bash scripts/doc-check.sh section .claude/agents/feature-runner.md \
      '## Verify — and tell the truth' 'Run the full `-race` suite ONCE per task'
  doc-check: FAIL: needle absent from section ... (exit 1)
  $ bash scripts/doc-check.sh section .claude/agents/reviewer.md \
      '## Fanning out (you have the Agent tool)' 'Do NOT run the whole test suite'
  doc-check: FAIL: needle absent from section ... (exit 1)
Both FAIL (RED) confirmed before this task was filed, per CLAUDE.md 'Verify — and tell the truth'.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [29ac1f66-efb9-4b05-afda-a2775e50f1c6](../../CONTEXT/gen-spec-mirror-guard-trips-on-column-0-shell-in-two-CON--29ac1f66/task.md) — gen-spec-mirror guard trips on column-0 shell in two CONTEXT-epic task descriptions, bloc… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
