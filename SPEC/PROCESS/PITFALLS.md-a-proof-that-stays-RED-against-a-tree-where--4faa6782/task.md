# PITFALLS.md: a proof that stays RED against a tree where the work IS done

| Field | Value |
| --- | --- |
| Public id | `4faa6782-6b49-4507-9a23-bb2cf42e7d02` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T09:04:47.232087+00:00 |
| Updated | 2026-08-22T09:40:03.935213+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/doc-check.sh section PITFALLS.md "## 2. Proofs that prove nothing" "trains the next agent to edit the proof"'
```

## Description

`PITFALLS.md` section 2 currently runs 2.1--2.7 (verified 2026-08-22: 2.1 non-existent `-run`
filter, 2.2 skipped children, 2.3 unquoted `-run` regex, 2.4 incidental grep match, 2.5 quoting the
wrong number, 2.6 a failing test is not done, 2.7 command-substituted backticks). Every one covers a
proof that PASSES WHEN IT SHOULD NOT. **The inverse is not covered, and it was hit on 2026-08-22.**

The incident. Task `97a315af` stored a `proof_cmd` asserting
`doc-check.sh section CLAUDE.md "Skipping security" ...`. The rule was actually implemented under
the EXISTING `## Agent roster` heading, so the proof returned
`doc-check: FAIL: heading not found in CLAUDE.md: Skipping security` (exit 1) against a tree where
the work was correct and complete.

Why this is worse than having no proof at all: it trains the next agent to EDIT THE PROOF UNTIL IT
FITS THE CODE, which is the precise inversion of 2.4's "confirm the proof is RED before the fix". A
`proof_cmd` authored against a heading nobody has written yet is a guess, and the guess is
discovered only at completion time -- the moment when the cheapest available move is to rewrite the
assertion.

Rule to state in the new subsection (**take the NEXT FREE `2.x` at the time you write it -- do NOT
hard-code 2.8; see the numbering-collision note at the end of this description**):
- A `proof_cmd` that names a heading, section or artefact which DOES NOT EXIST YET must be
  re-verified against the finished tree before the task is completed.
- Any change to a stored `proof_cmd` must be recorded WITH THE REASON. A silently retargeted proof
  is indistinguishable from a proof edited to fit the code.

Scope: `PITFALLS.md` only (one new `### 2.x` subsection -- **the next free number, not a pinned
2.8**), plus whatever one-line pointer the split convention requires. Do not restate 2.4; cite it.

**Proof: CONCRETE AND RUNNABLE, VERIFIED RED 2026-08-22. It replaced a placeholder, and the needle is
FIXED BY THIS TASK -- the prose must be written to contain it, not the other way round.**

The earlier version of this line stored angle-bracket placeholders for both the heading and the
needle, to be filled in at completion. That is now rejected: choosing a needle AFTER the prose is
written produces a proof fitted to the text, which proves only that the text exists. It is the same
failure this task documents, approached from the other side.

The stored command scopes to the PARENT heading `## 2. Proofs that prove nothing`, which ALREADY
EXISTS, instead of to the not-yet-written `### 2.x`. That is what makes it possible to commit to a
real command now: the numbering is still free (see the note below), but the assertion does not
depend on it.

Verified RED by spec-keeper before storing, and **RED FOR THE RIGHT REASON** -- the heading
RESOLVED and only the needle was absent:
`doc-check: FAIL: needle absent from section ## 2. Proofs that prove nothing [lines 57-144]`,
`verdict=FAIL class=wrapper exit=1`. That distinction is the whole subject of this task: the
incident behind it was a proof that failed because its HEADING did not exist, which is a proof that
could never have gone green whatever the code did.

**The text written must contain, verbatim, the phrase: trains the next agent to edit the proof**

---

## NUMBERING: DO NOT HARD-CODE 2.8 (2026-08-22, coordinator via spec-keeper)

**This task no longer pins a subsection number, and its title and `proof_cmd` were de-pinned with it.**

Task `4a24853a-d5f4-4099-97d7-fedb15e38e67` ("a correction placed BELOW the text it corrects") also
adds a `PITFALLS.md` section 2 subsection. Two tasks each hard-coding a `2.x` number is **the same
collision class this epic exists to prevent** -- the migration-number and task-key collision, in a
documentation file.

- **Neither task hard-codes a number. Both take the NEXT FREE `2.x` at authoring time.**
- **This task BLOCKS `4a24853a-d5f4-4099-97d7-fedb15e38e67`**, so the two serialise: whoever writes
  second sees the first's number already in the file and takes the one after it.
- `PITFALLS.md` section 2 ran 2.1--2.7 as of 2026-08-22. **Re-check that at authoring time rather
  than assuming it still holds** -- that count is exactly the kind of restated number that goes
  stale, which is the subject of the sibling task.

**Set the stored `proof_cmd` to the real heading and a real needle before completing**, and record
the reason for the change -- which is this task's OWN rule, applied to itself.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4a24853a-d5f4-4099-97d7-fedb15e38e67](../PITFALLS.md-section-2-a-correction-placed-BELOW-the-text--4a24853a/task.md) — PITFALLS.md section 2: a correction placed BELOW the text it corrects (todo)
- [97a315af-70b3-4a64-8456-92335d8c9631](../Make-security-skip-the-default-for-docs-and-tests-only-c--97a315af/task.md) — Make security skip the default for docs-and-tests-only changes, with a guard-file carve-o… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [461ebf5b-5d0e-4c3d-b95e-a1437e15b31f](../Acceptance-gate-all-four-low-measuring-cases-sort-correc--461ebf5b/task.md) — Acceptance gate: all four low-measuring cases sort correctly (todo)
- [46afc19c-e0dd-48cf-b003-6f5fe3bac48c](../scripts-spec-cloud.sh-reports-a-Cloudflare-WAF-block-as--46afc19c/task.md) — scripts/spec-cloud.sh reports a Cloudflare WAF block as a bare HTTP 403, indistinguishabl… (todo)
- [4a24853a-d5f4-4099-97d7-fedb15e38e67](../PITFALLS.md-section-2-a-correction-placed-BELOW-the-text--4a24853a/task.md) — PITFALLS.md section 2: a correction placed BELOW the text it corrects (todo)
- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)
- [c65a5051-678c-487c-bdae-37183e01f049](../scripts-spec-cloud.sh-a-caller-supplied-w-breaks-status--c65a5051/task.md) — scripts/spec-cloud.sh: a caller-supplied -w breaks status detection and makes a 200 exit 5 (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
