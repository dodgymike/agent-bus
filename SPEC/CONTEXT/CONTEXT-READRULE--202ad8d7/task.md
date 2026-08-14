# CONTEXT-READRULE: tell agents to grep and range-read the big docs, in the one file every agent gets

| Field | Value |
| --- | --- |
| Public id | `202ad8d7-c729-4c71-b7df-9e2002fbea17` |
| Key | CONTEXT-READRULE |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T15:24:33.727666+00:00 |
| Updated | 2026-08-08T15:24:33.727666+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/doc-check.sh section CLAUDE.md "## Reading the documents in this repo" "never whole-read" "SPEC.md" "DECISIONS-INDEX.md" "offset" "first 20 lines"'
```

## Description

Priority P1 justification: highest-expected-value item in the epic, and it is ADDITIVE, not a
deletion -- it changes HOW a document is fetched, not what information exists.

Definition of done: a ~14-line "## Reading the documents in this repo" section in CLAUDE.md: current
line/byte sizes of the eight large docs; SPEC.md is NEVER whole-read (`claim-next` and the task API
give you the task directly -- the mirror exists for humans without credentials); DECISIONS.md ->
read DECISIONS-INDEX.md first, then `Read` with offset/limit, never whole; CONTRACTS-* -> `grep -n
'^## '` to locate a section, then range-read it; before whole-reading ANY file over 600 lines, read
its first 20 lines first (that is where frozen/superseded banners live).

Who loses what: nobody loses information -- this only constrains HOW it is fetched. The bet is that
a grepped/range-read answer is as good as a whole-read one. Falsifier: an agent asserting something
false about a doc it grep-sampled instead of reading in full. Two occurrences => narrow the rule to
"grep to locate, then range-read +/-60 lines" rather than deleting it.

Depends on: CONTEXT-DOCCHECK; CONTEXT-CLAUDE-TRIM (second of the six CLAUDE.md-serialised tasks --
same file, must run after CLAUDE-TRIM, before CONTEXT-NOTESBLOCK). Soft dependency on
HANDOVER-DECISIONS-INDEX: the pointer text here names DECISIONS-INDEX.md, so land this after that
task or the pointer names a file that does not yet exist.

Parallel-safe: no (owns CLAUDE.md, position 2 of 6 in the serialised chain). Size: 1 hour.

Saving basis -- mixed and must NOT be conflated: a PER-SPAWN COST of approximately +900 B
(~+225 tokens/spawn, ~+6.8k tokens/session) against a PER-READ saving of roughly -105k tokens each
time it prevents one whole-read of SPEC.md (or roughly -76k tokens for a whole-read of DECISIONS.md)
-- these are different denominators and differ by orders of magnitude; do not add them as if
comparable. Breaks even if it prevents one whole-read per approximately 15 sessions, which is
plausible but is an estimate (EV), not a guarantee -- record it as such.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [CONTEXT-CLAUDE-TRIM](../CONTEXT-CLAUDE-TRIM--6ef1d88e/task.md)
- **blocked by** [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md)
- **blocks** [CONTEXT-NOTESBLOCK](../CONTEXT-NOTESBLOCK--95b091a8/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-CLAUDE-TRIM](../CONTEXT-CLAUDE-TRIM--6ef1d88e/task.md) — CONTEXT-CLAUDE-TRIM: the agent roster descriptions and model-selection rationale leave CL… (todo)
- [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md) — CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on (todo)
- [CONTEXT-NOTESBLOCK](../CONTEXT-NOTESBLOCK--95b091a8/task.md) — CONTEXT-NOTESBLOCK: one canonical note-journal instruction, not twelve copies (two of the… (todo)
- [HANDOVER-DECISIONS-INDEX](../../HANDOVER/HANDOVER-DECISIONS-INDEX--8cb6c2a7/task.md) — HANDOVER-DECISIONS-INDEX: generated table of contents for DECISIONS.md (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-CLAUDE-TRIM](../CONTEXT-CLAUDE-TRIM--6ef1d88e/task.md) — CONTEXT-CLAUDE-TRIM: the agent roster descriptions and model-selection rationale leave CL… (todo)
- [CONTEXT-DEEPDIVE-CONVENTION](../CONTEXT-DEEPDIVE-CONVENTION--cea3880c/task.md) — CONTEXT-DEEPDIVE-CONVENTION: stop the next 75 KB deep-dive from landing at the repo root (todo)
- [CONTEXT-NOTESBLOCK](../CONTEXT-NOTESBLOCK--95b091a8/task.md) — CONTEXT-NOTESBLOCK: one canonical note-journal instruction, not twelve copies (two of the… (todo)
- [CONTEXT-PLANE-TOC](../CONTEXT-PLANE-TOC--463afaf6/task.md) — CONTEXT-PLANE-TOC: a generated heading index at the top of every large reference doc (todo)
- [CONTEXT-SPEC-BRIEF](../CONTEXT-SPEC-BRIEF--4b0f5e57/task.md) — CONTEXT-SPEC-BRIEF: the SPEC.md mirror carries the lede of each task, not the full 382 KB… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
