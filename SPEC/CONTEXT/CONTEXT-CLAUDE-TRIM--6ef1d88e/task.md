# CONTEXT-CLAUDE-TRIM: the agent roster descriptions and model-selection rationale leave CLAUDE.md's per-spawn path

| Field | Value |
| --- | --- |
| Public id | `6ef1d88e-cae6-425a-a8ff-16d4b9f34331` |
| Key | CONTEXT-CLAUDE-TRIM |
| Epic | [CONTEXT](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T15:24:33.363747+00:00 |
| Updated | 2026-08-14T20:56:41.126622+00:00 |
| Completed | 2026-08-14T20:56:41.126605+00:00 |

## Proof command

```sh
grep -q 'ORCHESTRATION.md' CLAUDE.md && grep -q '^## Agent roster' CLAUDE.md && grep -q '^## Model selection' .claude/ORCHESTRATION.md && grep -q 'claude-opus-5' .claude/ORCHESTRATION.md && grep -q 'feature-runner' .claude/ORCHESTRATION.md && echo CONTENT_RELOCATED_OK
```

## Status note

AMENDED 2026-08-14 (spec-keeper, coordinator-directed audit): the proof_cmd's `-le 21500` absolute byte ceiling was ARITHMETICALLY UNREACHABLE by this task alone even at filing time -- HEAD was 27,547 B, roster removal saves ~2,279 B, model-selection removal saves ~938 B, and 27547-2279-938=24330 > 21500 at ZERO replacement text (a bare pointer costs more than zero). The commit that did the work (5a4f885) measured this precisely and reported the REAL net change as 27,547 -> 26,995 (-552 B), far short of -2,587, because the SAME commit had to ADD invariant-reading mandates to four agent bodies (implementer, reviewer, security, documentation) as a correctness requirement -- a redistribution, not a pure trim. CLAUDE.md has grown further since (28,781 B as of this audit, via four unrelated necessary commits) and will keep moving for reasons outside this task's control. An absolute final-state ceiling is the wrong acceptance criterion for one of six SERIALISED tasks contributing to it; that ceiling belongs to CONTEXT-BUDGET-WIRE (last epic task, 'the byte ceilings from this whole epic become a standing, wired-in check'), which can legitimately gate on the post-all-six-tasks state. This task's proof_cmd is corrected to verify the CONTENT MOVE itself (roster + model-selection rationale live in .claude/ORCHESTRATION.md, CLAUDE.md keeps only the bare roster line + rule + pointer) rather than an unreachable byte target. scripts/doc-check.sh does not exist yet (CONTEXT-DOCCHECK, separately open) so the corrected proof_cmd is an inlined grep equivalent, consistent with how other agents have worked around the missing instrument; re-verify with real doc-check.sh once CONTEXT-DOCCHECK lands.

## Description

Priority P1 justification: CLAUDE.md is injected into EVERY agent spawn (per-spawn cost). Two
sections in it benefit only the ~4 agent types that spawn others, and today all ~30 spawns in a
session pay for them regardless.

Definition of done: new `.claude/ORCHESTRATION.md` (read ON DEMAND, not per-spawn) takes: the 14
roster descriptions (one paragraph each), the model-selection RATIONALE, and the feature-runner
override note. CLAUDE.md keeps: (a) the bare 14 agent names, one line; (b) the one-line rule "ALWAYS
pass model explicitly: sonnet = mechanical, opus = judgment/correctness-critical"; (c) an imperative
pointer -- "Before spawning ANY sub-agent, read .claude/ORCHESTRATION.md."  This is the same
rule-inline / rationale-relocated pattern already applied to CLAUDE.md -> INVARIANTS.md.

Who loses what: a non-spawning agent loses the one-paragraph description of every OTHER agent --
it never used them. A spawning agent pays one extra Read per session for ORCHESTRATION.md.

Depends on: CONTEXT-DOCCHECK (proof instrument). HARD PRE-REQUISITE: the in-flight CLAUDE.md split
(rule inline, rationale to INVARIANTS.md) must be COMMITTED before this task starts -- otherwise
this edits a file with uncommitted deletions in it, which is exactly the `MM` pathspec-commit trap
CLAUDE.md itself warns about (a pathspec commit takes the WORKTREE, not the index).

Parallel-safe: NO -- this is the first of six tasks that serialise on CLAUDE.md, in this order:
CONTEXT-CLAUDE-TRIM -> CONTEXT-READRULE -> CONTEXT-NOTESBLOCK -> CONTEXT-DONEGATE-CANON ->
CONTEXT-DRIFT-WRAPPERS -> CONTEXT-LOG-RETIRE. That serialisation is this epic's schedule risk --
each of the six must land, in order, before the next starts; do not parallelise any pair of them.

Size: 2 hours.

Saving basis -- PER-SPAWN (paid on every one of ~30 spawns/session, NOT the same order of magnitude
as a per-read saving): roughly 2,279 B (roster descriptions) + 938 B (model-selection rationale)
collapsing to ~630 B of pointer text => approximately -2,587 B/spawn, approximately -647 tokens/spawn,
approximately -19,400 tokens/session at 30 spawns (4 bytes/token, markdown-with-code).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md)
- **blocks** [CONTEXT-DISPATCH-RULE](../CONTEXT-DISPATCH-RULE--81bc24d6/task.md)
- **blocks** [CONTEXT-READRULE](../CONTEXT-READRULE--202ad8d7/task.md)
- **blocks** [CONTEXT-RESERVE-CANON](../CONTEXT-RESERVE-CANON--3aea21a7/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-BUDGET-WIRE](../CONTEXT-BUDGET-WIRE--be76c7e2/task.md) — CONTEXT-BUDGET-WIRE: the byte ceilings from this whole epic become a standing, wired-in c… (todo)
- [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md) — CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on (done)
- [CONTEXT-DONEGATE-CANON](../CONTEXT-DONEGATE-CANON--b9b0c654/task.md) — CONTEXT-DONEGATE-CANON: 'do not mark done when the behaviour is not yet live' said once,… (todo)
- [CONTEXT-DRIFT-WRAPPERS](../CONTEXT-DRIFT-WRAPPERS--1a9bf503/task.md) — CONTEXT-DRIFT-WRAPPERS: two per-spawn files still call the retired shell wrappers 'the ON… (todo)
- [CONTEXT-LOG-RETIRE](../CONTEXT-LOG-RETIRE--116179c8/task.md) — CONTEXT-LOG-RETIRE: AGENT_LOG.md freezes its narrative and moves to one line per task (todo)
- [CONTEXT-NOTESBLOCK](../CONTEXT-NOTESBLOCK--95b091a8/task.md) — CONTEXT-NOTESBLOCK: one canonical note-journal instruction, not twelve copies (two of the… (todo)
- [CONTEXT-READRULE](../CONTEXT-READRULE--202ad8d7/task.md) — CONTEXT-READRULE: tell agents to grep and range-read the big docs, in the one file every… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-CLAUDEMD-INVARIANTS-SPLIT](../CONTEXT-CLAUDEMD-INVARIANTS-SPLIT--1ec63b91/task.md) — CONTEXT-CLAUDEMD-INVARIANTS-SPLIT: retroactively file and close the CLAUDE.md/INVARIANTS.… (done)
- [CONTEXT-DISPATCH-RULE](../CONTEXT-DISPATCH-RULE--81bc24d6/task.md) — CONTEXT-DISPATCH-RULE: dispatch briefs stop restating standing rules already in every sub… (todo)
- [CONTEXT-NOTESBLOCK](../CONTEXT-NOTESBLOCK--95b091a8/task.md) — CONTEXT-NOTESBLOCK: one canonical note-journal instruction, not twelve copies (two of the… (todo)
- [CONTEXT-READRULE](../CONTEXT-READRULE--202ad8d7/task.md) — CONTEXT-READRULE: tell agents to grep and range-read the big docs, in the one file every… (todo)
- [CONTEXT-RESERVE-CANON](../CONTEXT-RESERVE-CANON--3aea21a7/task.md) — CONTEXT-RESERVE-CANON: the reservation guidance stops disagreeing with itself across four… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
