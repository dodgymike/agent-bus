# Deep-dive: audit and refactor the repo's tracked .md files, CLAUDE.md primary, fix AGENTS.md syncing in the same work

| Field | Value |
| --- | --- |
| Public id | `f4bd3c9f-3af8-4438-bcb0-18203b857255` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | done |
| Priority | P2 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T20:53:47.306361+00:00 |
| Updated | 2026-08-22T06:16:48.939820+00:00 |
| Completed | 2026-08-22T06:16:48.939804+00:00 |

## Proof command

```sh
bash scripts/doc-check.sh budget
```

## Description

Deep-dive task for the deep-diver agent. Audit and refactor the repo's tracked .md files, with CLAUDE.md as the PRIMARY target, and fix AGENTS.md syncing as part of the SAME work rather than as a separate follow-on. Produce SPEC/deep-diver's standard PLAN.md-shaped deliverable: <TOPIC>_DEEPDIVE.md at the repo root (see DOC-TRUTH_DEEPDIVE.md, committed in 3db3b04, for the established shape: Symptom, Evidence, Root cause(s), the fix, SPEC-ready task breakdown, cost/risk/rollback). This task differs from that precedent in one respect the brief is explicit about: it must produce the PLAN and the REFACTOR itself, not a report alone -- code/doc edits, not just findings, subject to the normal chain (spec-keeper files any atomic follow-up tasks the refactor needs; do not let the deep-diver agent itself flip task state).

MEASURED FACTS (verify again at start of work; the repo moves under parallel agents and these are a snapshot from the filing session, 2026-08-21):

1. SIZE CEILING. CLAUDE.md measured 31023 B against docs/doc-budgets.tsv's recorded 28781 B ceiling for that path -- 2242 B over. `bash scripts/doc-check.sh budget` FAILS on it today: observed output 'doc-check: FAIL: CLAUDE.md is 31023 B, over its 28781 B ceiling by 2242 B'. Note docs/doc-budgets.tsv's own header comment is itself stale (it says CLAUDE.md is 'currently 30063 B, i.e. OVER by 1282 B' -- both those numbers are wrong at HEAD; the file has grown further since that comment was written). CONTEXT-BUDGET-WIRE (public_id be76c7e2-8d7a-4c78-a1f5-9ea1c2bd3502, epic CONTEXT, P2, todo) intends to wire `doc-check.sh budget` into scripts/check.sh as a standing commit gate, at which point commits touching CLAUDE.md would be BLOCKED until the ceiling is satisfied. DOCS-4-FU-BUDGET (public_id 721b51ef-8daf-421b-b56e-3fb77a17f7cf, epic CONTEXT, P2, todo) already owns the ceiling DECISION itself -- whether 28781 is still the right number, or whether a documented re-baseline is warranted, and settling that BEFORE the enforcement wiring lands. This deep-dive task must COORDINATE with DOCS-4-FU-BUDGET, not duplicate its decision: the deep-dive should identify what can be trimmed or relocated out of CLAUDE.md (the refactor's job) and let DOCS-4-FU-BUDGET's owner make the final ceiling call using that output, rather than this task unilaterally re-baselining the number itself.

2. AGENTS.md SYNC. AGENTS.md at repo root is a real, independently-tracked file, NOT a symlink to CLAUDE.md. Re-measured at filing time: `diff AGENTS.md CLAUDE.md | grep -c '^[<>]'` = 127 differing lines (this number moves session to session as both files are edited independently -- re-measure, do not trust this figure). Both files' most recent commit is currently the SAME sha (b95d22d, 2026-08-21), but CLAUDE.md carries two additional prior commits (401f112, 2828dcf) that never propagated to AGENTS.md before b95d22d landed, which is exactly the drift mechanism this task must close, not merely re-sync once more.

   AN EXISTING TASK ALREADY OWNS THIS: public_id 6a5ece85-5006-4f86-9c61-a33f15a069dc, 'Audit AGENTS.md vs CLAUDE.md drift and fix the sync mechanism, not just the text', epic PROCESS, P2, status todo. DO NOT DUPLICATE ITS SCOPE. That task owns: (a) triaging every diff line between the two files, (b) deciding and recording the steady-state sync mechanism (symlink vs generated-from-CLAUDE.md vs a doc-check divergence guard) with a DECISIONS.md entry, and (c) the small README.md unauthenticated-routes wording fix. THIS task's relationship to it: this deep-dive's CLAUDE.md refactor will very likely CHANGE what AGENTS.md needs to sync (e.g. if content moves out of CLAUDE.md into a companion file, the sync mechanism must track the split, not just the top-level file). So sequence matters -- this task should either (a) land its CLAUDE.md restructuring FIRST and hand 6a5ece85 a stable shape to sync against, or (b) explicitly coordinate on which task implements the sync mechanism, so two agents do not build two different answers. State in the deep-dive doc which ordering was chosen and why. This task must NOT independently decide the sync mechanism if 6a5ece85 is still open and unowned -- link the two (relates, or blocks if the ordering makes it a hard dependency) and say explicitly in the deep-dive doc that 6a5ece85 is the task that owns the sync-mechanism DECISION.

3. WHY CLAUDE.md GROWS. Its size grows because every work session tends to append another warning learned the hard way -- the gofmt exit-127 false-pass trap, the 'gofmt -l . && echo CLEAN' exits-0-while-listing trap, the pathspec-commit-takes-the-WORKTREE-not-the-index trap and its MM / (space)M variants, the several vacuous-proof shapes (skipped-children, grep-based doc proofs, a missing proof_cmd), and the clean-overlay-of-HEAD verification rule. THE REFACTOR MUST NOT LOSE THESE. State this constraint plainly in the deep-dive plan: anything moved OUT of CLAUDE.md must remain REACHABLE from it (a one-line pointer, same pattern INVARIANTS.md already demonstrates: one-line rule in CLAUDE.md, full reasoning in the companion file) -- deleting a warning rather than relocating it is a regression, not a cleanup. docs/doc-preserve.tsv (see CONTEXT-BUDGET-WIRE's definition of done) is the existing mechanism for marking load-bearing phrases that must never be deleted; consult it and extend it as content moves.

4. 'HOW TO WRITE' SECTION. CLAUDE.md gained a 'How to write (agent output, commit messages, docs, notes)' section on 2026-08-21, positioned directly above 'Go conventions' (confirmed at CLAUDE.md line 158 at filing time). It mandates plain, literal language and forbids metaphor, praise and editorial commentary. The refactor MUST apply that rule to every file it touches or produces (including this deep-dive's own prose), and MUST NOT delete the section -- relocate-with-pointer only, per fact 3's constraint, if it needs to move at all.

5. PRIOR ART. DOC-TRUTH_DEEPDIVE.md (repo root, committed 3db3b04) audited where docs contradict code and is a useful starting INVENTORY for this deep-dive, not a substitute for redoing the audit against the current tree (docs move fast here -- see fact 2's own example of drift that happened AFTER that deep-dive). There are 43 open DOCS-epic tasks at filing time (confirmed by listing epic_key=DOCS tasks with status != done via GET /api/v1/projects/agent-bus/tasks, paginated). The deep-dive's output must include a section naming which of those 43 its refactor would CLOSE outright or make substantially EASIER to close (e.g. a task whose whole scope is 'fix a stale cross-reference in a file this refactor is restructuring anyway'), rather than filing more overlapping tasks into that pile.

SCOPE (minimum -- the deep-dive may find more tracked .md files in-scope, but must at least cover these): CLAUDE.md, AGENTS.md, INVARIANTS.md, README.md, AGENT_PROTOCOL.md, PROTOCOL.md, CONTRACTS.md and its four CONTRACTS-*.md plane files (CONTRACTS-CLI.md, CONTRACTS-HTTP.md, CONTRACTS-ONDISK.md, CONTRACTS-AGENT.md), DECISIONS.md, AGENT_LOG.md.

DECISIONS.md and AGENT_LOG.md are APPEND-ONLY by long-standing convention in this repo. The deep-dive must explicitly DECIDE whether that convention still serves them well at their CURRENT size (both are large, growing, append-only logs) rather than silently reorganising or truncating either one. If the answer is 'keep append-only', say why; if the answer is 'needs a different shape at this size' (e.g. periodic archival, an index/summary head with detail below), propose it as a SPEC task rather than doing it inline in this deep-dive, since it is a bigger and more consequence-laden decision than the CLAUDE.md/AGENTS.md refactor itself.

RESERVATION / KEYING: do NOT reserve a numbered task-key for any follow-up this deep-dive files. The task-key-<EPIC> reservation counters are independently known to be drifted below their epics' true max task keys (see public_id dd2cdc20-8920-4e5b-bf0a-668f439cc3a6, 'Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS, ACK, AUTH)', confirmed FAIL as of 2026-08-21, task-key-DOCS specifically recorded at max=5 reserved while the true max live task key is DOCS-30, a drift of 25 -- every reservation from it today returns an already-colliding value). The operator ruling on this task (dd2cdc20) is to use descriptive titles / server-assigned public_ids for new work rather than reserving numbered keys until the counters are reconciled. Follow that ruling for every follow-up task this deep-dive files.

PROOF_CMD for THIS filing task: `test -z "$($(go env GOROOT)/bin/gofmt -l . 2>/dev/null)" ; bash scripts/doc-check.sh budget` -- confirmed RED at filing time: `scripts/doc-check.sh budget` printed 'doc-check: FAIL: CLAUDE.md is 31023 B, over its 28781 B ceiling by 2242 B' and 'doc-check: FAIL: budget -- 1 failure(s) over 3 sized file(s) and 5 preserved phrase(s)', exit 1. This is the CLAUDE.md-size half of done; it deliberately does NOT duplicate 6a5ece85's own proof_cmd ('diff AGENTS.md CLAUDE.md | grep -c ... SYNC_OK') for the AGENTS.md-sync half -- that task already carries that proof and owns the sync-mechanism decision (see fact 2). This task's own definition of done is: the deep-dive doc exists and is committed; the CLAUDE.md refactor lands such that `bash scripts/doc-check.sh budget` exits 0 with the SAME preserved-phrase count (5) it has today, i.e. no trap was deleted to make room, only relocated with the count re-verified after the move; and any decision this task makes about the AGENTS.md sync mechanism is either deferred entirely to 6a5ece85 or explicitly coordinated with it in the deep-dive doc, never decided twice.

Proof for the OUTCOME (not just this filing): re-run the same proof_cmd once the refactor lands; it must go from the FAIL above to exit 0. Additionally re-run 6a5ece85's own proof_cmd (diff AGENTS.md CLAUDE.md | grep -c '^[<>]' | grep -qx 0 && echo SYNC_OK) and confirm whether it also passes -- if the sync mechanism this task's refactor enables is what finally makes 6a5ece85 pass, say so explicitly when reporting, but let spec-keeper flip 6a5ece85 to done separately rather than this task claiming credit for it.

RELATED, NOT DUPLICATE: DOCS-4-FU-BUDGET (721b51ef, owns the ceiling number decision), CONTEXT-BUDGET-WIRE (be76c7e2, owns wiring the ceiling into a commit gate), 6a5ece85 (owns the AGENTS.md sync-mechanism decision), dd2cdc20 (owns the reservation-counter reconciliation, unrelated mechanism but cited here for the no-numbered-key ruling).

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [6a5ece85-5006-4f86-9c61-a33f15a069dc](../Audit-AGENTS.md-vs-CLAUDE.md-drift-and-fix-the-sync-mech--6a5ece85/task.md) — Audit AGENTS.md vs CLAUDE.md drift and fix the sync mechanism, not just the text (todo)
- [CONTEXT-BUDGET-WIRE](../../CONTEXT/CONTEXT-BUDGET-WIRE--be76c7e2/task.md) — CONTEXT-BUDGET-WIRE: the byte ceilings from this whole epic become a standing, wired-in c… (todo)
- [DOCS-30](../../DOCS/DOCS-30--a311a067/task.md) — DOCS-30: clientcert help says the bus ignores the client certificate; the bus refuses 409… (todo)
- [DOCS-4-FU-BUDGET](../../CONTEXT/DOCS-4-FU-BUDGET--721b51ef/task.md) — DOCS-4-FU-BUDGET: aade191 grew CLAUDE.md 678 bytes past its own recorded 28781-byte ratch… (todo)
- [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](../../UNASSIGNED/Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) — Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [3d1b47d9-1395-4f61-a848-e1c06ced2ff8](../../CONTEXT/PITFALLS.md-has-no-row-in-doc-budgets.tsv-so-the-prose-r--3d1b47d9/task.md) — PITFALLS.md has no row in doc-budgets.tsv, so the prose relocated out of CLAUDE.md is unm… (todo)
- [5cf1edd0-a678-4072-98f9-4c1cb08c7c92](../../CONTEXT/doc-check.sh-s-re-entry-probe-reports-suppressed-asserti--5cf1edd0/task.md) — doc-check.sh's re-entry probe reports "suppressed assertions" while quoting two equal cou… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
