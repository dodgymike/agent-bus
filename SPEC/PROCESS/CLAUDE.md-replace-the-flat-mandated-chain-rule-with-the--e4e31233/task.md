# CLAUDE.md: replace the flat mandated-chain rule with the tiered one-liner, byte-neutral, then re-sync AGENTS.md

| Field | Value |
| --- | --- |
| Public id | `e4e31233-cabe-4af4-986b-f28c84347214` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T08:40:05.421825+00:00 |
| Updated | 2026-08-22T09:03:33.756369+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/doc-check.sh budget && bash scripts/doc-check.sh section CLAUDE.md "## Work in atomic increments" "CHANGE-TIERS" && cmp CLAUDE.md AGENTS.md'
```

## Description

BLOCKED ON A LIVE EDIT: a documentation agent is currently editing both CLAUDE.md and AGENTS.md (both show as modified in the worktree). Do not claim this task until that agent has reported and its work is committed.

The budget is the hard constraint. docs/doc-budgets.tsv caps CLAUDE.md at 28781 as a RATCHET, not headroom. HEAD is 28213 (568 bytes free); the in-flight worktree edit is 28776, leaving 5 bytes. So this change must be byte-neutral or negative -- it replaces prose, it does not add any.

What to replace: CLAUDE.md:390-391 ("For ANY code change the chain spec-keeper -> implementer -> reviewer -> security is MANDATORY; skipping a step requires an explicit one-line justification in AGENT_LOG.md. Only SECURITY's default flips:") with a one-line tiered rule pointing at docs/CHANGE-TIERS.md. Also check :11 and :337 ("reviewer AND security gates as COMPLETED"), which restate the flat chain and will otherwise contradict the new rule. Do NOT copy the tier table, the signal list or the plane map into CLAUDE.md -- they live in docs/CHANGE-TIERS.md and .claude/ORCHESTRATION.md.

Then re-sync AGENTS.md (identical content; the two have drifted before -- PITFALLS.md section 5; and see open task 6a5ece85-5006-4f86-9c61-a33f15a069dc, which is about fixing the sync MECHANISM, not just the text -- coordinate rather than duplicating it).

The proof must include the budget check, since byte-neutrality is the load-bearing property. Adjust the `cmp` if the two files are not byte-identical by design -- verify first; at HEAD both are 28213 bytes.

BLOCKED BY T-01, T-15. RELATES to 6a5ece85.

---

## AMENDMENT D (2026-08-22, planner via orchestrator): re-measure the budget; do not raise the ratchet

**Re-measure the budget AT CLAIM TIME rather than trusting any snapshot in this description.** The
numbers below were true when written and the whole point of the task is that they move.

Measured 2026-08-22 by spec-keeper: `CLAUDE.md` and `AGENTS.md` are **byte-identical at 28776 B**
against the hard **28781** ratchet in `docs/doc-budgets.tsv` -- **5 bytes spare**. (The "HEAD is
28213 / 568 free" figures above are stale; the in-flight documentation edit has landed in the
worktree.)

Constraints, all hard:
- The **28781 ceiling is a deliberate RATCHET and MUST NOT be raised.**
- The `docs/doc-budgets.tsv` row for `CLAUDE.md` **must not be deleted** -- deleting it turns the
  check green and leaves nothing to notice. (Deleting that row is itself a control-plane change that
  NARROWS a check: T4 under Amendment A on T-01.)
- The tiering scheme therefore adds **only a one-line pointer** to `CLAUDE.md`, and even that
  requires tightening existing wording to pay for it. Net byte delta must be <= 0.
- The T0-T4 table and the signal list go in `.claude/ORCHESTRATION.md` (task
  a94dee14-fea7-406c-9c4f-485736f434c4) and `docs/CHANGE-TIERS.md` (task
  4d990ef4-23ee-4971-ab00-84eb5ec137ae). Do NOT copy them here.
- `bash scripts/doc-check.sh budget` **is already present in this task's `proof_cmd`** -- confirmed
  2026-08-22 by spec-keeper. Do not drop it; byte-neutrality is the load-bearing property.

**Observation for the implementer (verified 2026-08-22): `docs/doc-budgets.tsv` has NO row for
`AGENTS.md`** -- only `CLAUDE.md` at line 44. AGENTS.md is budgeted only TRANSITIVELY, by this
task's `cmp CLAUDE.md AGENTS.md` assertion. If that `cmp` is ever weakened or removed, AGENTS.md
becomes unbudgeted while still being injected for AGENTS.md-reading runtimes. Keep the `cmp`, and
consider adding the missing row while you are in the file.

**This task is now CONTROL PLANE under Amendment A on T-01** (`CLAUDE.md`, `AGENTS.md` and
`docs/doc-budgets.tsv` are all control plane), so it self-classifies at **T3+**. Its own tier note
must reflect that; it is not a T0 documentation change.

**2026-08-22 (coordinator ruling, via spec-keeper):** the scheme has **FOUR** lanes -- T0/T2/T3/T4.
T1 is REMOVED and tests-only lands in **T2**, with reviewer + delta-scoped security. Any T1 wording
in this task is superseded; see T-01 (4d990ef4-23ee-4971-ab00-84eb5ec137ae) ruling 2.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [4d990ef4-23ee-4971-ab00-84eb5ec137ae](../Write-docs-CHANGE-TIERS.md-the-normative-tier-and-signal--4d990ef4/task.md)
- **blocked by** [a94dee14-fea7-406c-9c4f-485736f434c4](../claude-ORCHESTRATION.md-the-tier-to-agent-routing-table--a94dee14/task.md)
- **relates to** [6a5ece85-5006-4f86-9c61-a33f15a069dc](../Audit-AGENTS.md-vs-CLAUDE.md-drift-and-fix-the-sync-mech--6a5ece85/task.md)
- **relates to** [85c9854b-6c34-470f-bffe-3eed1116f2b0](../docs-doc-budgets.tsv-give-AGENTS.md-its-own-ceiling-row--85c9854b/task.md)
- **supersedes** [97a315af-70b3-4a64-8456-92335d8c9631](../Make-security-skip-the-default-for-docs-and-tests-only-c--97a315af/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4d990ef4-23ee-4971-ab00-84eb5ec137ae](../Write-docs-CHANGE-TIERS.md-the-normative-tier-and-signal--4d990ef4/task.md) — Write docs/CHANGE-TIERS.md, the normative tier and signal specification (todo)
- [6a5ece85-5006-4f86-9c61-a33f15a069dc](../Audit-AGENTS.md-vs-CLAUDE.md-drift-and-fix-the-sync-mech--6a5ece85/task.md) — Audit AGENTS.md vs CLAUDE.md drift and fix the sync mechanism, not just the text (todo)
- [a94dee14-fea7-406c-9c4f-485736f434c4](../claude-ORCHESTRATION.md-the-tier-to-agent-routing-table--a94dee14/task.md) — .claude/ORCHESTRATION.md: the tier-to-agent routing table (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [85c9854b-6c34-470f-bffe-3eed1116f2b0](../docs-doc-budgets.tsv-give-AGENTS.md-its-own-ceiling-row--85c9854b/task.md) — docs/doc-budgets.tsv: give AGENTS.md its own ceiling row (todo)
- [97a315af-70b3-4a64-8456-92335d8c9631](../Make-security-skip-the-default-for-docs-and-tests-only-c--97a315af/task.md) — Make security skip the default for docs-and-tests-only changes, with a guard-file carve-o… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
