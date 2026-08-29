# CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on

| Field | Value |
| --- | --- |
| Public id | `b3b28f45-54b3-4d0e-bde7-933c9c3923b2` |
| Key | CONTEXT-DOCCHECK |
| Epic | [CONTEXT](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T15:24:33.007888+00:00 |
| Updated | 2026-08-21T21:18:08.030675+00:00 |
| Completed | 2026-08-21T21:18:08.030657+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/doc-check.sh --selftest'
```

## Status note

Awaiting a security round-4 verdict on commit e2c9cd0 (scripts/doc-check.sh, round-4 fixes for the round-3 CHANGES-REQUESTED at 2026-08-21T20:22:33) before completion. Round-4 has a reviewer delta PASS only; no security re-gate has been posted since round-3.

## Description

Priority P1 justification: not for size, but because this repo's known failure mode is a doc proof
that passes on an incidental match elsewhere in a file -- that has already green-lit a wrong task
closure here. Every other CONTEXT task claims a doc changed; without a section-scoped assert, each
of those proofs repeats that exact bug. This is proof-check.sh's sibling and is a hard prerequisite
to trusting the rest of the epic.

Definition of done: scripts/doc-check.sh with three modes:
  - `section <file> '<heading>' '<needle>'...` -- locates the heading, computes its line range to
    the next same-level heading, asserts each needle occurs INSIDE that range. Exits non-zero if
    the heading is absent (cannot pass vacuously).
  - `budget` -- reads docs/doc-budgets.tsv (path, max_bytes), fails on any overrun; and reads
    docs/doc-preserve.tsv (path, literal_phrase), fails if a phrase is MISSING. Ceilings apply only
    to per-spawn and generated files; DECISIONS.md/AGENT_LOG.md are exempt BY DESIGN, with the
    reason recorded in the tsv file itself.
  - `--selftest` -- asserts the assert: heading-absent => FAIL, needle-only-outside-section => FAIL,
    needle-inside => PASS.
  - Must NOT invoke scripts/proof-check.sh (recursion -- see 69eb6f56).

Files: scripts/doc-check.sh, docs/doc-budgets.tsv, docs/doc-preserve.tsv, CONTRACTS-AGENT.md
(repo-tooling section, document the new script there).

RED verification observed (2026-08-08, spec-keeper filing): scripts/doc-check.sh does not exist --
trivially RED, file absent.

Depends on: nothing. Soft-relates to HANDOVER-CHECK (0f909b6c) -- wire `doc-check.sh budget` into
scripts/check.sh THERE, not here; do not duplicate the wiring in this task.

Parallel-safe: yes. Size: half a day. Saving: 0 tokens directly -- this is the enabler every other
task's proof_cmd depends on.

Chain: this ships a shell script, so it needs reviewer + security, not documentation-only sign-off.

Blocks every other CONTEXT task (recorded as real `blocks` relations) -- none of their doc-scoped
proof_cmds are trustworthy until this lands.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [CONTEXT-AGENTDESC-TRIM](../CONTEXT-AGENTDESC-TRIM--eaea0581/task.md)
- **blocks** [CONTEXT-BUDGET-WIRE](../CONTEXT-BUDGET-WIRE--be76c7e2/task.md)
- **blocks** [CONTEXT-CLAUDE-TRIM](../CONTEXT-CLAUDE-TRIM--6ef1d88e/task.md)
- **blocks** [CONTEXT-CLI-SECTIONS](../CONTEXT-CLI-SECTIONS--3b4bd434/task.md)
- **blocks** [CONTEXT-CONTRACTS-PARKING](../CONTEXT-CONTRACTS-PARKING--881dae01/task.md)
- **blocks** [CONTEXT-DEEPDIVE-CONVENTION](../CONTEXT-DEEPDIVE-CONVENTION--cea3880c/task.md)
- **blocks** [CONTEXT-DISPATCH-RULE](../CONTEXT-DISPATCH-RULE--81bc24d6/task.md)
- **blocks** [CONTEXT-DONEGATE-CANON](../CONTEXT-DONEGATE-CANON--b9b0c654/task.md)
- **blocks** [CONTEXT-DRIFT-PHANTOM](../CONTEXT-DRIFT-PHANTOM--08e38aec/task.md)
- **blocks** [CONTEXT-DRIFT-WRAPPERS](../CONTEXT-DRIFT-WRAPPERS--1a9bf503/task.md)
- **blocks** [CONTEXT-FANOUT-COMPRESS](../CONTEXT-FANOUT-COMPRESS--48c0e011/task.md)
- **blocks** [CONTEXT-LOG-GUARD](../CONTEXT-LOG-GUARD--f39083ae/task.md)
- **blocks** [CONTEXT-LOG-RETIRE](../CONTEXT-LOG-RETIRE--116179c8/task.md)
- **blocks** [CONTEXT-NOTESBLOCK](../CONTEXT-NOTESBLOCK--95b091a8/task.md)
- **blocks** [CONTEXT-PLANE-TOC](../CONTEXT-PLANE-TOC--463afaf6/task.md)
- **blocks** [CONTEXT-PROTOCOL-WALFLOOR-DEDUP](../CONTEXT-PROTOCOL-WALFLOOR-DEDUP--1e9cec15/task.md)
- **blocks** [CONTEXT-READRULE](../CONTEXT-READRULE--202ad8d7/task.md)
- **blocks** [CONTEXT-RESERVE-CANON](../CONTEXT-RESERVE-CANON--3aea21a7/task.md)
- **blocks** [CONTEXT-SPEC-BRIEF](../CONTEXT-SPEC-BRIEF--4b0f5e57/task.md)
- **blocks** [CONTEXT-STALE-NOTYET](../CONTEXT-STALE-NOTYET--67b42913/task.md)
- **follow-up of** [5cf1edd0-a678-4072-98f9-4c1cb08c7c92](../doc-check.sh-s-re-entry-probe-reports-suppressed-asserti--5cf1edd0/task.md)
- **follow-up of** [CONTEXT-DOCCHECK-FU-COUNTPIN](../CONTEXT-DOCCHECK-FU-COUNTPIN--5f3a3cd6/task.md)
- **follow-up of** [CONTEXT-DOCCHECK-FU-BREAKCOUNT](../CONTEXT-DOCCHECK-FU-BREAKCOUNT--a9e66edb/task.md)
- **follow-up of** [CONTEXT-DOCCHECK-FU-DASHFILE](../CONTEXT-DOCCHECK-FU-DASHFILE--1b997e84/task.md)
- **follow-up of** [CONTEXT-DOCCHECK-FU-ENVIRON](../CONTEXT-DOCCHECK-FU-ENVIRON--a9bf1905/task.md)
- **follow-up of** [CONTEXT-DOCCHECK-SYMLINK](../CONTEXT-DOCCHECK-SYMLINK--d7277d6f/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [HANDOVER-CHECK](../../HANDOVER/HANDOVER-CHECK--0f909b6c/task.md) — HANDOVER-CHECK: one command that tells you the health of this repo, plus its recorded out… (todo)
- [PROOF-CHECK-FU-RECURSION](../../TOOLING/PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) — PROOF-CHECK-FU-RECURSION: bash scripts/proof-check.sh hangs / spawns runaway processes wh… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [5cf1edd0-a678-4072-98f9-4c1cb08c7c92](../doc-check.sh-s-re-entry-probe-reports-suppressed-asserti--5cf1edd0/task.md) — doc-check.sh's re-entry probe reports "suppressed assertions" while quoting two equal cou… (todo)
- [CONTEXT-AGENTDESC-TRIM](../CONTEXT-AGENTDESC-TRIM--eaea0581/task.md) — CONTEXT-AGENTDESC-TRIM: budget the 14 frontmatter description: fields, the real per-spawn… (done)
- [CONTEXT-CLAUDE-TRIM](../CONTEXT-CLAUDE-TRIM--6ef1d88e/task.md) — CONTEXT-CLAUDE-TRIM: the agent roster descriptions and model-selection rationale leave CL… (done)
- [CONTEXT-CLI-SECTIONS](../CONTEXT-CLI-SECTIONS--3b4bd434/task.md) — CONTEXT-CLI-SECTIONS: CONTRACTS-CLI.md's 857-line mega-section becomes real, range-readab… (todo)
- [CONTEXT-CONTRACTS-PARKING](../CONTEXT-CONTRACTS-PARKING--881dae01/task.md) — CONTEXT-CONTRACTS-PARKING: CONTRACTS.md admits, in its own text, that it is 90% parking l… (todo)
- [CONTEXT-DEEPDIVE-CONVENTION](../CONTEXT-DEEPDIVE-CONVENTION--cea3880c/task.md) — CONTEXT-DEEPDIVE-CONVENTION: stop the next 75 KB deep-dive from landing at the repo root (todo)
- [CONTEXT-DOCCHECK-FU-BREAKCOUNT](../CONTEXT-DOCCHECK-FU-BREAKCOUNT--a9e66edb/task.md) — CONTEXT-DOCCHECK-FU-BREAKCOUNT: doc-check.sh:344's 'breaks 20 assertions' is unsourceable… (todo)
- [CONTEXT-DOCCHECK-FU-COUNTPIN](../CONTEXT-DOCCHECK-FU-COUNTPIN--5f3a3cd6/task.md) — CONTEXT-DOCCHECK-FU-COUNTPIN: the outer doc-check.sh --selftest run's assertion count is… (todo)
- [CONTEXT-DOCCHECK-FU-DASHFILE](../CONTEXT-DOCCHECK-FU-DASHFILE--1b997e84/task.md) — CONTEXT-DOCCHECK-FU-DASHFILE: a file argument of a lone '-' is accepted by containment, c… (todo)
- [CONTEXT-DOCCHECK-FU-ENVIRON](../CONTEXT-DOCCHECK-FU-ENVIRON--a9bf1905/task.md) — CONTEXT-DOCCHECK-FU-ENVIRON: the environment can turn any doc-check.sh verdict green -- d… (todo)
- [CONTEXT-DOCCHECK-SYMLINK](../CONTEXT-DOCCHECK-SYMLINK--d7277d6f/task.md) — CONTEXT-DOCCHECK-SYMLINK: physical (realpath) containment for cmd_section -- a symlink st… (todo)
- [CONTEXT-DONEGATE-CANON](../CONTEXT-DONEGATE-CANON--b9b0c654/task.md) — CONTEXT-DONEGATE-CANON: 'do not mark done when the behaviour is not yet live' said once,… (todo)
- [CONTEXT-DRIFT-PHANTOM](../CONTEXT-DRIFT-PHANTOM--08e38aec/task.md) — CONTEXT-DRIFT-PHANTOM: two agent defs instruct writing to SESSION_REPORT.md, which has ne… (todo)
- [CONTEXT-DRIFT-WRAPPERS](../CONTEXT-DRIFT-WRAPPERS--1a9bf503/task.md) — CONTEXT-DRIFT-WRAPPERS: two per-spawn files still call the retired shell wrappers 'the ON… (todo)
- [CONTEXT-FANOUT-COMPRESS](../CONTEXT-FANOUT-COMPRESS--48c0e011/task.md) — CONTEXT-FANOUT-COMPRESS: shrink the 2,040 B fan-out doctrine duplicated across five revie… (todo)
- [CONTEXT-LOG-GUARD](../CONTEXT-LOG-GUARD--f39083ae/task.md) — CONTEXT-LOG-GUARD: the AGENT_LOG.md freeze is enforced mechanically, not hoped for (todo)
- [CONTEXT-LOG-RETIRE](../CONTEXT-LOG-RETIRE--116179c8/task.md) — CONTEXT-LOG-RETIRE: AGENT_LOG.md freezes its narrative and moves to one line per task (todo)
- [CONTEXT-NOTESBLOCK](../CONTEXT-NOTESBLOCK--95b091a8/task.md) — CONTEXT-NOTESBLOCK: one canonical note-journal instruction, not twelve copies (two of the… (todo)
- [CONTEXT-PLANE-TOC](../CONTEXT-PLANE-TOC--463afaf6/task.md) — CONTEXT-PLANE-TOC: a generated heading index at the top of every large reference doc (todo)
- [CONTEXT-PROTOCOL-WALFLOOR-DEDUP](../CONTEXT-PROTOCOL-WALFLOOR-DEDUP--1e9cec15/task.md) — CONTEXT-PROTOCOL-WALFLOOR-DEDUP: one file owns the WAL-index-floor bytes, not two that ca… (todo)
- [CONTEXT-READRULE](../CONTEXT-READRULE--202ad8d7/task.md) — CONTEXT-READRULE: tell agents to grep and range-read the big docs, in the one file every… (todo)
- [CONTEXT-SPEC-BRIEF](../CONTEXT-SPEC-BRIEF--4b0f5e57/task.md) — CONTEXT-SPEC-BRIEF: the SPEC.md mirror carries the lede of each task, not the full 382 KB… (superseded)
- [CONTEXT-STALE-NOTYET](../CONTEXT-STALE-NOTYET--67b42913/task.md) — CONTEXT-STALE-NOTYET: a doc-check \`forbid\` mode, so a "not yet implemented" note cannot o… (todo)
- [DOCS-4-FU-BUDGET](../DOCS-4-FU-BUDGET--721b51ef/task.md) — DOCS-4-FU-BUDGET: aade191 grew CLAUDE.md 678 bytes past its own recorded 28781-byte ratch… (todo)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (done)
- [RELAY-25-FU-CORRELATION-FU-AGENTDOCS](../../RELAY/RELAY-25-FU-CORRELATION-FU-AGENTDOCS--6a4f6f47/task.md) — RELAY-25-FU-CORRELATION-FU-AGENTDOCS: CONTRACTS-AGENT.md still says fed-smoke.sh is 'expe… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
