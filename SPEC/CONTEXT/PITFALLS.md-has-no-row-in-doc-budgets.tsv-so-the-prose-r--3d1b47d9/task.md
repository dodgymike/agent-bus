# PITFALLS.md has no row in doc-budgets.tsv, so the prose relocated out of CLAUDE.md is unmeasured

| Field | Value |
| --- | --- |
| Public id | `3d1b47d9-1395-4f61-a848-e1c06ced2ff8` |
| Key | _(null in the export)_ |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T22:18:57.063907+00:00 |
| Updated | 2026-08-21T22:18:57.063907+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'grep -Pq "^PITFALLS\.md\t[0-9]+$" docs/doc-budgets.tsv && bash scripts/doc-check.sh budget'
```

## Description

Task f4bd3c9f (in progress) moved 8267 B of incident prose out of CLAUDE.md into a NEW file
PITFALLS.md (21539 B measured at filing time, was 20166 B when this task was first drafted --
re-measure, do not trust either number). docs/doc-budgets.tsv has rows for CLAUDE.md, SPEC.md and
README.md only -- three entries, all pre-dating PITFALLS.md's existence. `bash scripts/doc-check.sh
budget` therefore reports "3 file(s) within ceiling" and PASSES while PITFALLS.md is entirely
unmeasured.

CONSEQUENCES

- The refactor RELOCATED the growth rather than BOUNDING it. CLAUDE.md is genuinely smaller (31023
  -> 28213 B worktree at filing time) and that saving is real and recurring because CLAUDE.md is
  injected into every sub-agent spawn -- but the total is not controlled.
- CLAUDE.md's "How to write" section now designates PITFALLS.md as the destination for EVERY future
  trap. So the one file guaranteed to grow is the one nothing watches.
- docs/doc-preserve.tsv has the mirror-image gap: all 5 rows point at CLAUDE.md, and the reasoning
  they protect has moved into PITFALLS.md.

WHY f4bd3c9f DID NOT FIX IT (two independent reasons, both worth recording)

1. f4bd3c9f's definition of done requires the preserved-phrase count to remain exactly 5; adding
   preserve rows for PITFALLS.md would break the condition that task is measured by.
2. docs/doc-budgets.tsv and scripts/doc-check.sh are the instrument that judges f4bd3c9f's own
   proof (`bash scripts/doc-check.sh budget`), and an agent must not edit the instrument judging its
   own change.

IMPORTANT NUANCE FOR WHOEVER PICKS THIS UP

The ceiling number is a policy decision, not a mechanical one, and the existing rows do NOT follow
one convention. docs/doc-budgets.tsv's own header says so: CLAUDE.md's ceiling was set AT the
file's size when the ratchet landed (0a9a674) -- a ratchet, deliberately zero headroom -- while
SPEC.md and README.md were given a round 8192 B, which IS headroom. So "add a row for PITFALLS.md"
requires choosing WHICH convention applies, and the answer differs depending on whether PITFALLS.md
is expected to keep growing by design (it is -- see above, CLAUDE.md's own "How to write" section
now routes new traps there).

Coordinate with DOCS-4-FU-BUDGET (721b51ef-8daf-421b-b56e-3fb77a17f7cf), which owns the
ceiling-number decision generally for CLAUDE.md; this task may end up folded into it, or may stay
separate because it concerns a DIFFERENT file with a DIFFERENT growth expectation. Do not duplicate
CONTEXT-BUDGET-WIRE's (be76c7e2-8d7a-4c78-a1f5-9ea1c2bd3502) scope either -- that task wires the
budget check into a standing commit gate and populates doc-budgets.tsv "with the ceilings
established by every sizing task in this epic", but PITFALLS.md did not exist when it was filed and
is not covered by it.

VERIFIED AT FILING TIME (2026-08-21)

- `bash scripts/doc-check.sh budget` in the live repo: "doc-check: PASS: budget -- 3 file(s) within
  ceiling, 5 preserved phrase(s) present" -- exit 0, confirming PITFALLS.md is unmeasured and the
  check is blind to it.
- PITFALLS.md is untracked (new from f4bd3c9f), 21539 B. CLAUDE.md is 28213 B worktree (modified,
  same in-progress task).

PROOF_CMD

  bash -c 'grep -Pq "^PITFALLS\.md\t[0-9]+$" docs/doc-budgets.tsv && bash scripts/doc-check.sh budget'

Confirmed RED at filing time via `bash scripts/proof-check.sh '<cmd>'`:
  proof-check: FAIL -- proof command exited 1
  proof-check: verdict=FAIL class=other exit=1 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0

Confirmed the same command goes GREEN once a row exists, demonstrated in an isolated overlay (docs/
+ scripts/ + PITFALLS.md copied out of the repo into a scratch dir, a PITFALLS.md row appended to
the COPY only -- the live repo was never touched):
  doc-check: PASS: budget -- 4 file(s) within ceiling, 5 preserved phrase(s) present
  proof-check: PASS -- proof command exited 0.
  proof-check: verdict=PASS class=other exit=0 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0

This proof does not pass merely by adding an unsatisfiable ceiling (that would leave doc-check.sh
budget FAILing, so the conjunction stays RED) -- it requires BOTH a PITFALLS.md row matching the
`<path>\t<digits>` format AND the whole budget check passing with that row present.

FILING NOTE: per the operator ruling on dd2cdc20-8920-4e5b-bf0a-668f439cc3a6, the task-key-<EPIC>
reservation counters are known-drifted and hand out colliding values -- this task deliberately does
NOT reserve a numbered CONTEXT-N key; it uses a descriptive title and the server-assigned public_id.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up of** [f4bd3c9f-3af8-4438-bcb0-18203b857255](../../PROCESS/Deep-dive-audit-and-refactor-the-repo-s-tracked-.md-file--f4bd3c9f/task.md)
- **relates to** [DOCS-4-FU-BUDGET](../DOCS-4-FU-BUDGET--721b51ef/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-BUDGET-WIRE](../CONTEXT-BUDGET-WIRE--be76c7e2/task.md) — CONTEXT-BUDGET-WIRE: the byte ceilings from this whole epic become a standing, wired-in c… (todo)
- [DOCS-4-FU-BUDGET](../DOCS-4-FU-BUDGET--721b51ef/task.md) — DOCS-4-FU-BUDGET: aade191 grew CLAUDE.md 678 bytes past its own recorded 28781-byte ratch… (todo)
- [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](../../UNASSIGNED/Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) — Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… (todo)
- [f4bd3c9f-3af8-4438-bcb0-18203b857255](../../PROCESS/Deep-dive-audit-and-refactor-the-repo-s-tracked-.md-file--f4bd3c9f/task.md) — Deep-dive: audit and refactor the repo's tracked .md files, CLAUDE.md primary, fix AGENTS… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
