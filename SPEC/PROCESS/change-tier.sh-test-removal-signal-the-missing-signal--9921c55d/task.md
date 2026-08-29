# change-tier.sh: test-removal signal (the missing signal)

| Field | Value |
| --- | --- |
| Public id | `9921c55d-d8a0-460c-ac5f-91a6bb6adcf2` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T08:40:03.041915+00:00 |
| Updated | 2026-08-22T09:14:33.172369+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/change-tier_test.sh'
```

## Description

"The change DELETES OR WEAKENS a test" is listed in the design as a never-auto-lower override but has no signal computing it -- it is the most important condition in the scheme and, as specified, nothing measures it. Add it.

Mechanical part (in scope): removed _test.go file; removed or renamed-away `func Test...`/`func Fuzz...`/`func Benchmark...`; added t.Skip/t.SkipNow; net-negative count of test functions across the diff.

Non-mechanical part (explicitly OUT of scope, and must be documented as such in the script's output and in docs/CHANGE-TIERS.md): "weakens". Loosening an assertion inside a retained test is semantic and cannot be computed. The script must not claim to cover it; the reviewer owns it (T-12).

This signal is what stops the T1 lane (tests-only, security skipped by default) from becoming the route by which a change that removes a check skips review. It must be evaluated as an override AFTER the floor, and it may only raise.

Proof detail: fixtures for a deleted test file; a deleted func Test; an added t.Skip; and a control that adds tests and must NOT fire. RED first.

BLOCKED BY T-03.

---

## RULING 2026-08-22 (coordinator, via spec-keeper): EXPLICITLY RETAINED

This signal sits on the T1/T2 line, and **T1 has been REMOVED** by the tier collapse (T-01 /
4d990ef4-23ee-4971-ab00-84eb5ec137ae, ruling 2 -- four lanes T0/T2/T3/T4). The task is **retained
anyway**. The coordinator's reason, recorded:

It is the signal that stops the tiering scheme from becoming the route by which a check gets
removed. And the finding that "weakens" is not computable makes the DELETION half **more** important,
not less -- deletion is the only part enforceable mechanically, so it is the only part that cannot be
argued away.

Read the sentence above about "the T1 lane (tests-only, security skipped by default)" as the **T2**
lane now. The mechanism is unchanged: the override is evaluated AFTER the floor and may only raise.

**Limit statement: see T-01 ruling 5.** The mechanical floor covers test DELETION only; semantic
weakening -- loosening an assertion inside a retained test -- is reviewer-owned (T-12), and the
script must not claim to cover it.

---

## INHERITS F1 AND F2 FROM T-03 (2026-08-22, security gate)

**This signal classifies by PATH, and therefore inherits findings F1 and F2 recorded on T-03
(b2567ffd-190d-4aff-8cc2-f6a2eb2d613e).** Both are measured, not theorised:

- **F1 (renames):** `git status --porcelain` prints a rename as ONE line, `R  old -> new`, so a
  check anchored at `^` never sees the target and a check testing the line end never sees the
  source. **This signal must consume the `git status --porcelain --no-renames` file set**, in which
  the rename is split into `D old` + `A new` and both halves are classified.
- **F2 (fails open):** `git status --porcelain -- <pathspec matching nothing>` prints nothing and
  exits **0**. **This signal must NOT treat an EMPTY file set as low-risk** -- "measured T0" and
  "could not measure" are different outcomes, and the second is an error exit, not a result.

**This signal needs a RENAME FIXTURE in its own right** -- not merely coverage in T-03's tests --
shown RED before the signal is implemented, with the RED output quoted in the task's `kind=report`.

---

## WORKED EXAMPLE: why this task is retained (2026-08-22)

The interim docs-and-tests-only carve-out's own review is a REAL case study, not a constructed one.
Three rounds, in order:

1. **The first version EXEMPTED ITSELF.** The rule as written would have classified the change that
   introduced it as docs-only. Caught before commit.
2. **The control-plane narrowing fixed the PRINCIPLE** -- and a security gate STILL found three
   bypasses (F1 renames, F2 fail-open path matching, F3 inheritance) in the MECHANICAL FORM, while
   agreeing the principle was right.
3. **Every one of those findings was a gap between the PROSE and the GREPS implementing it.** The
   prose was correct in each case; the implementation did not do what the prose said.

**The lesson to state in `docs/CHANGE-TIERS.md`: each of these passed a careful reading and failed a
MEASUREMENT.** Three rounds of competent review did not surface what one fixture did. That is the
argument for fixtures over review, and it is the argument for retaining this task:

> **A check that CANNOT FIRE is indistinguishable from a check that PASSES.**

Which is why the test-removal signal is the missing signal, and why every signal in this epic must
show its fixture RED before the signal exists.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4d990ef4-23ee-4971-ab00-84eb5ec137ae](../Write-docs-CHANGE-TIERS.md-the-normative-tier-and-signal--4d990ef4/task.md) — Write docs/CHANGE-TIERS.md, the normative tier and signal specification (todo)
- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4d990ef4-23ee-4971-ab00-84eb5ec137ae](../Write-docs-CHANGE-TIERS.md-the-normative-tier-and-signal--4d990ef4/task.md) — Write docs/CHANGE-TIERS.md, the normative tier and signal specification (todo)
- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
