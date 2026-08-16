# CONTEXT-SPEC-TREE: Split SPEC.md mirror into a directory tree

| Field | Value |
| --- | --- |
| Public id | `ff15e9ff-7e2b-4c4a-abf6-28c010dc9bb0` |
| Key | _(null in the export)_ |
| Epic | [CONTEXT](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | tooling |
| Section | backlog |
| Tags | spec-mirror, docs-tooling |
| Created | 2026-08-14T11:05:32.591885+00:00 |
| Updated | 2026-08-14T12:17:10.065877+00:00 |
| Completed | 2026-08-14T12:17:10.065860+00:00 |

## Proof command

```sh
bash scripts/gen-spec-mirror.sh && test "$(wc -c < SPEC.md)" -lt 40000 && test -f SPEC/RELAY/epic.md && grep -q "SPEC/RELAY/" SPEC.md
```

## Status note

HOLD SATISFIED 2026-08-14: the earlier "do not complete -- security pass in flight" note is resolved. Committed b46efab; reviewer PASS; security PASS across three passes, final ESCAPING DELTA CONFIRMED (md5 26dd5da65cec2dc0d992a54f13cbdfbc). Completing now via complete with commit_sha=b46efab.

## Description

Split the generated SPEC.md mirror into a directory tree instead of one monolithic file.

User instruction, verbatim: "do a task to split SPEC.md into SPEC/<epic>/<task>/, with epic.md and task.md files, so that SPEC.md is just a list of epics with pointers to the files, and the epic.md is a list of tasks with pointers and dependencies".

Rationale: SPEC.md was 599,767 B (corrected 2026-08-14 -- an earlier 642,570 B figure named here was the UNCOMMITTED WORKTREE file at session start, another agent's regeneration already staged before implementation began; 599,767 B is the correct pre-commit diff basis, confirmed against b46efab~1:SPEC.md), 92% of it task descriptions. Verified nothing reads it for task content -- every reference in CLAUDE.md and all 14 agent definitions is "never hand-edit", "regenerate", or spec-keeper's server-down fallback; agents get descriptions from claim-next. So SPEC.md is a fallback and human-navigation artefact, and the fix is to make navigation cheap while keeping descriptions COMPLETE -- truncating them would degrade the one case they exist for.

Scope: rewrite scripts/gen-spec-mirror.sh to emit:
- SPEC.md: epic list + pointers + per-epic open/total counts (no task descriptions).
- SPEC/<epic>/epic.md: task list per epic + pointers to each task.md + derived references (dependencies parsed from free-text description, explicitly labelled DERIVED, not authoritative -- see CONTEXT-SPEC-DEPS).
- SPEC/<epic>/<task>/task.md: the full task record (title, status, priority, component, full description, proof_cmd, tags, timestamps).

Preserve BOTH existing guards from the current script: the count cross-check (open task count matches what the server reports) and the structural column-0 assertion. Keep the open-tasks-only default (closed tasks omitted unless --all), matching current gen-spec-mirror.sh behaviour.

Does NOT require the CONTEXT-SPEC-DEPS schema change -- references derived in this task are parsed from existing free-text description content and must be clearly labelled as derived/best-effort, not authoritative.

AMENDED 2026-08-14 (spec-keeper, per coordinator course-correction during implementation -- the
implementer flagged that this record contradicted its own shipped code rather than completing
against a stale description, which is the right instinct and is recorded here so it keeps being
rewarded). Two things in the original scope above were superseded during implementation:

1. NOT open-tasks-only by default, and `--all` is now a NO-OP. The original scope said "Keep the
   open-tasks-only default (closed tasks omitted unless --all)". That was superseded: the tree now
   contains ALL tasks, open and closed -- 522 task.md + 28 epic.md, 550 files total. A closed task's
   file costs nothing until someone opens it (a directory tree, unlike the old flat SPEC.md, does
   not put every task's bytes in front of every reader), and OMITTING closed tasks was exactly what
   made the server-down fallback worse than the live server: a human or agent falling back to
   SPEC.md during an outage would have been missing 39% of the backlog at the worst possible time.
   `--all` is kept as an accepted flag for backward compatibility with the old invocation but does
   nothing now that everything is always included.

2. Real relations (all four kinds -- `blocks`/`supersedes`/`relates`/`follow_up`) are fetched and
   rendered BY DEFAULT, not derived-only. The original scope said epic.md carries "derived references
   (dependencies parsed from free-text description, explicitly labelled DERIVED, not authoritative --
   see CONTEXT-SPEC-DEPS)" as if that were the whole story. The assumption behind that -- that almost
   nothing in this backlog carries a real edge yet -- was WRONG.

   FIGURES, RECONCILED TWICE (2026-08-14) -- use these, not the two earlier passes: a first report said
   237 tasks / 706 edges (this task's own original filing text, above); a second said 184 tasks / 522
   total / ~600 edges (an integrator recount that turned out to be BLOCKS-ONLY, so it under-covered the
   other three relation kinds). The implementer then re-measured against the COMMITTED tree at b46efab,
   scoped to each task.md's own "Relations (authoritative)" section (uncapped -- epic.md's table caps
   each cell at 6 rendered lines with a "+N more" trailer, so counting from epic.md undercounts), and
   VERIFIED it by a directed-pair symmetry check (blocks 300 == blocked-by 300, supersedes 16 ==
   superseded-by 16, follow-up 18 == follow-up-of 18, relates 64 even -- no one-sided edges, meaning
   every relation peer resolved to a real task):

     task.md files                 522
     distinct relations            366  = 300 blocks + 32 relates + 16 supersedes + 18 follow-up
     rendered edge lines           732  (each relation renders on BOTH endpoints)
     tasks with ANY edge           243  (46.6%)
     tasks with a BLOCKING edge    184  (35.2% of all 522; 146/354 = 41.2% of the 354 OPEN tasks)

   FRAMING CORRECTION, the behaviourally important part: the "nearly half the backlog has edges" framing
   is the wrong denominator for judging whether this graph can drive planning. A critical path is built
   from ORDERING edges only -- `relates`/`supersedes`/`follow_up` impose no ordering and are 66 of the
   366. On blocking edges alone, 41.2% of OPEN tasks carry one and 59% carry NONE. The real-relations
   graph is worth reading and covers a substantial minority of open work, but it CANNOT YET REPLACE a
   hand-maintained critical path -- it CAN be used to CHECK one (anything a hand-maintained path orders
   that the graph does not is a missing edge worth filing). Real relations are now the PRIMARY view in
   epic.md; derived free-text references remain as a clearly-separated, explicitly-labelled best-effort
   layer alongside them, per CONTEXT-SPEC-DEPS's convention (`blocks`, target=public_id).

TIMING, recorded because an operator will otherwise assume a hang: a full regeneration (fetching
relations for all ~520 tasks, one request per task -- there is no bulk relations endpoint) costs
roughly 50-70 SECONDS, rate-limited by the Spec Server. `--no-relations` (epic.md renders only the
derived layer, skips the per-task relations fetch) completes in roughly 2.6 seconds. Document both
numbers in scripts/gen-spec-mirror.sh's own usage/comment block, not just here, so the operator
sees the expected runtime before the command appears to hang.

STATUS: COMPLETE. Committed at b46efab ("SPEC mirror becomes a tracked tree: a 600 KB monolith
-> a 6 KB index + 550 files"), 2026-08-14T12:08:09Z. reviewer PASS, security PASS across three
passes, final verdict ESCAPING DELTA CONFIRMED (signed md5 26dd5da65cec2dc0d992a54f13cbdfbc,
independently verified by the integrator against both the working file and the committed blob).
proof-check.sh verdict=PASS in a clean HEAD overlay of 61141d8; same proof verdict=FAIL on HEAD
alone. Independent regeneration reproduces the committed tree exactly.

NOTED FOR THE RECORD, NOT A DEFECT (coordinator, 2026-08-14): the commit's .gitignore change adds
`!id_rsa*/` and `!id_ed25519*/`, which loosens the secrets backstop -- an extension-less file under
a directory named e.g. `id_rsa.d/` becomes trackable. This was gated (security reproduced the
loosening in a throwaway repo before signing off) and the in-file .gitignore comment states the
cost plainly. In scope, not a defect -- recorded here rather than left only inside a 553-file
commit message where nobody would find it later.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-SPEC-DEPS](../CONTEXT-SPEC-DEPS--8280358d/task.md) — CONTEXT-SPEC-DEPS: Adopt and document the blocks-relation convention for task dependencies (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-KEY-IDENTITY](../CONTEXT-KEY-IDENTITY--73dec684/task.md) — CONTEXT-KEY-IDENTITY: Standardize task identity (public_id vs key) before SPEC/&lt;epic&gt;/&lt;ta… (todo)
- [CONTEXT-SPEC-DEPS](../CONTEXT-SPEC-DEPS--8280358d/task.md) — CONTEXT-SPEC-DEPS: Adopt and document the blocks-relation convention for task dependencies (todo)
- [aa2dfd79-9bc5-4e0a-925a-824168b710be](../../TOOLING/scripts-spec-cloud.sh-Cognito-bearer-token-is-on-curl-s--aa2dfd79/task.md) — scripts/spec-cloud.sh: Cognito bearer token is on curl's argv, readable via /proc/*/cmdli… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
