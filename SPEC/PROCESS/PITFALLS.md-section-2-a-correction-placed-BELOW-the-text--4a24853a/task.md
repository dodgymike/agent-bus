# PITFALLS.md section 2: a correction placed BELOW the text it corrects

| Field | Value |
| --- | --- |
| Public id | `4a24853a-d5f4-4099-97d7-fedb15e38e67` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T09:31:01.482663+00:00 |
| Updated | 2026-08-22T09:40:27.599796+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/doc-check.sh section PITFALLS.md "## 2. Proofs that prove nothing" "a correction goes AT the thing it corrects"'
```

## Description

**Measured 2026-08-22 on task `4d990ef4-23ee-4971-ab00-84eb5ec137ae` itself**, while applying the
PROCESS tier rulings. This is an incident, not a style preference.

**What happened.** That task's "diff-basis contract" bullet prescribed `git diff HEAD -- <pathspec>`.
The correction -- that the basis must be `git status --porcelain --no-renames` -- sat SEVERAL
PARAGRAPHS BELOW it, in a later amendment. A reader working top-down transcribes the stale command
and never reaches the fix. Nothing in the text at the point of the error said the error was there.

**The check existed and the reader would never reach it. That is a guard that cannot fire, in prose
form** -- the same defect class this repo keeps finding in code (`PITFALLS.md` section 6), arriving
in documentation instead.

**The rule to state:**

> **A correction goes AT the thing it corrects, or REPLACES it. Never below it.**

**State the distinction EXPLICITLY, because the two conventions look identical and only one of them
is safe:**

- **Appending a dated correction rather than rewriting is CORRECT** for `DECISIONS.md` and
  `AGENT_LOG.md`. Those are read as **HISTORY**; the superseded entry IS the record, and rewriting it
  destroys the thing the file exists to hold.
- **The same convention is DANGEROUS in a normative spec** -- `docs/CHANGE-TIERS.md`,
  `CONTRACTS-*.md`, `AGENT_PROTOCOL.md`, `.claude/agents/*.md`, and task descriptions that INSTRUCT.
  Those are read as **INSTRUCTION**, and **the first matching line wins**. A reader who finds a
  command that answers their question stops reading.

The practical form for a normative file: replace the wrong text outright, or put the marker
IN PLACE -- at the sentence, before the stale command, not in a later section -- with a pointer to
where the corrected rule is written in full.

**Where it goes:** a new subsection of `PITFALLS.md` section 2. Do not restate section 2.4 or
section 6; cite them.

---

## NUMBERING COLLISION -- HANDLE IT EXPLICITLY, DO NOT HARD-CODE A NUMBER

Task `4faa6782-6b49-4507-9a23-bb2cf42e7d02` also adds a `PITFALLS.md` section 2 subsection. Both
tasks hard-coding a `2.x` number is **the same collision class this epic exists to prevent** -- the
migration-number and task-key collisions, in a documentation file.

Therefore:

- **NEITHER task hard-codes a number.** Both say: **take the next free `2.x` at the time you write.**
  `4faa6782` has been updated accordingly (its title, description and `proof_cmd` no longer pin 2.8).
- **`4faa6782` BLOCKS this task**, so the two serialise and the second author sees the first's
  number already present.

## PROOF -- CONCRETE AND RUNNABLE, NEEDLE FIXED NOW (corrected 2026-08-22)

**CORRECTION, APPLIED AT THE POINT OF THE ERROR RATHER THAN BELOW IT -- which is this task's own
rule, applied to itself.** This task was filed with a `proof_cmd` containing angle-bracket
placeholders for both the heading and the needle, and this paragraph previously said those
placeholders were carried "deliberately", to be resolved at completion. **That was wrong, and the
stored `proof_cmd` has been replaced with a real command.**

Why it was wrong, stated plainly because the placeholder version had a defensible-sounding
justification:

1. **A placeholder is not a proof, and it is worse than having none.** `CLAUDE.md` forbids
   completing a task with no `proof_cmd`. A placeholder LOOKS present, so it survives inspection and
   fails only at completion -- the exact moment when the cheapest available move is to invent an
   assertion that fits whatever was written.
2. **A needle chosen AFTER the prose is a proof fitted to the text.** It can only demonstrate that
   the text exists. To mean anything, the needle must be a COMMITMENT made in advance that the
   implementer is obliged to satisfy.
3. **The numbering argument does not require a placeholder.** The reason given for deferring was
   that the `### 2.x` number is not yet known. That is true and it stays true -- but the proof does
   not need the subsection number. It scopes to the PARENT heading
   `## 2. Proofs that prove nothing`, which already exists. The numbering stays free; the assertion
   does not depend on it.

**THE TEXT WRITTEN MUST CONTAIN, VERBATIM, THE PHRASE: a correction goes AT the thing it corrects**

Verified RED by spec-keeper 2026-08-22 before storing, and RED FOR THE RIGHT REASON -- the heading
RESOLVED (reported as lines 57-144) and only the needle was absent:
`doc-check: FAIL: needle absent from section ## 2. Proofs that prove nothing [lines 57-144]: a
correction goes AT the thing it corrects`, `verdict=FAIL class=wrapper exit=1`. A proof that failed
because its HEADING did not resolve would be the defect described by the blocking task
`4faa6782-6b49-4507-9a23-bb2cf42e7d02`, not evidence of anything.

`4faa6782`'s rule still applies to any FUTURE change to this command: re-verify it against the
finished tree before completing, and record the reason for any edit. It does not license shipping a
placeholder in the meantime.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4d990ef4-23ee-4971-ab00-84eb5ec137ae](../Write-docs-CHANGE-TIERS.md-the-normative-tier-and-signal--4d990ef4/task.md) — Write docs/CHANGE-TIERS.md, the normative tier and signal specification (todo)
- [4faa6782-6b49-4507-9a23-bb2cf42e7d02](../PITFALLS.md-a-proof-that-stays-RED-against-a-tree-where--4faa6782/task.md) — PITFALLS.md: a proof that stays RED against a tree where the work IS done (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4faa6782-6b49-4507-9a23-bb2cf42e7d02](../PITFALLS.md-a-proof-that-stays-RED-against-a-tree-where--4faa6782/task.md) — PITFALLS.md: a proof that stays RED against a tree where the work IS done (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
