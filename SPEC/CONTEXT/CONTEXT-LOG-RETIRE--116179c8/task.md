# CONTEXT-LOG-RETIRE: AGENT_LOG.md freezes its narrative and moves to one line per task

| Field | Value |
| --- | --- |
| Public id | `116179c8-da6b-40df-8d00-6121d6caa039` |
| Key | CONTEXT-LOG-RETIRE |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T15:24:36.383760+00:00 |
| Updated | 2026-08-08T15:24:36.383760+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'grep -q "narrative entries end here" AGENT_LOG.md && bash scripts/doc-check.sh section CLAUDE.md "## Work in atomic increments" "one line per task" && bash scripts/doc-check.sh section AGENT_LOG.md "## Convention" "reviewer=" "SKIPPED:"'
```

## Description

Priority P2 justification: AGENT_LOG.md has grown to 43 entries averaging 5,963 B each, all written
in 6 days (roughly 43 KB/day, the fastest-growing file in the repo), with no committed tooling that
reads it -- the only automated readers were two `todo` proof_cmds. It is also the file that
repeatedly shows `MM` in git status and blocks pathspec commits. Every sampled entry has a
corresponding Spec Server task whose notes are equal or richer.

USER DECISION -- ALREADY RULED, not a blocker (2026-08-08): APPROVED as "freeze + one-line entries".
The earlier "needs a user decision before implementation" gate is REMOVED. The ruling: a dated
"narrative entries end here" marker; the existing ~3,451 lines of narrative stay UNTOUCHED
(append-only is respected, nothing is deleted or rewritten); the new convention going forward is one
line per task, <= 240 B, carrying task id, sha, gate verdicts and proof verdict; REVIEWER/SECURITY
SKIP JUSTIFICATIONS STAY IN AGENT_LOG.md -- that is the one category CLAUDE.md step 10 uniquely
mandates recording there, and it fits on one line. All other narrative moves to `kind=report` task
notes on the Spec Server.

RECORDED FALSIFIER (keep this, it is load-bearing): if HANDOVER-CONTRIBUTING finds that Spec Server
credentials cannot transfer to a new maintainer, REVERSE THIS TASK -- a credential-less maintainer
would lose all in-repo narrative from the freeze date forward with no way to read the note journal
that replaced it. In that event, build a notes-to-WORKLOG.md exporter instead of retiring the
narrative.

Definition of done:
  1. A dated "## 2026-0X-XX -- narrative entries end here" marker appended to AGENT_LOG.md. Existing
     lines untouched.
  2. New convention documented and enforced going forward: one line per task, <= 240 B --
     `date . <task-key/public_id> . <sha> . gates: reviewer=... security=... (or SKIPPED: <one-line
     reason>) . proof: <proof-check verdict>`.
  3. Skip justifications stay in AGENT_LOG.md per CLAUDE.md step 10 -- confirm this explicitly in the
     new convention text so nobody "simplifies" it away later.
  4. CLAUDE.md steps 8 and 10 updated to describe the new convention. A DECISIONS.md entry recording
     the change, the loss (credential-less narrative access from the freeze date), and the falsifier
     above -- do NOT write DECISIONS.md as part of THIS task's own execution if a concurrent appender
     risk exists; coordinate per CLAUDE.md's shared-file rules.

Who loses what: a reader WITHOUT Spec Server credentials loses in-repo narrative detail from the
freeze date on. Genuine loss, mitigated by the one-liner carrying the task's public_id so the full
narrative is one API call away for a credentialed reader.

CONFLICT recorded as a real relation, MUST land first or be rescoped: open task 0ba2372a
("Journal catch-up: DECISIONS.md + AGENT_LOG.md entries owed by INVITE-MINT and MTLS-ROTATE") greps
AGENT_LOG.md for narrative content as part of its own proof. If it has not landed before this task
starts, either land it first or rescope it to write to the note journal instead of AGENT_LOG.md
narrative.

Depends on: CONTEXT-DOCCHECK; CONTEXT-DONEGATE-CANON (sixth and LAST of the six CLAUDE.md-serialised
tasks). Also depends on 0ba2372a per the conflict above.

Parallel-safe: no. Size: half a day.

Saving basis -- PER-TASK OUTPUT (distinct from per-spawn and per-read: this is narrative text an
agent no longer WRITES, once per completed task, not once per spawn or once per file read): roughly
5,700 B of narrative not written per task => approximately -1,425 output tokens/task, approximately
-14.3k output tokens/session at 10 tasks/session. Plus the file stops growing ~43 KB/day, plus a
large drop in `MM`-blocked commits.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [0ba2372a-09f7-4f05-bd33-98a5f80e0e6f](../../DOCS/Journal-catch-up-DECISIONS.md-AGENT_LOG.md-entries-owed--0ba2372a/task.md)
- **blocked by** [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md)
- **blocked by** [CONTEXT-DRIFT-WRAPPERS](../CONTEXT-DRIFT-WRAPPERS--1a9bf503/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0ba2372a-09f7-4f05-bd33-98a5f80e0e6f](../../DOCS/Journal-catch-up-DECISIONS.md-AGENT_LOG.md-entries-owed--0ba2372a/task.md) — Journal catch-up: DECISIONS.md + AGENT_LOG.md entries owed by INVITE-MINT and MTLS-ROTATE (todo)
- [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md) — CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on (todo)
- [CONTEXT-DONEGATE-CANON](../CONTEXT-DONEGATE-CANON--b9b0c654/task.md) — CONTEXT-DONEGATE-CANON: 'do not mark done when the behaviour is not yet live' said once,… (todo)
- [HANDOVER-CONTRIBUTING](../../HANDOVER/HANDOVER-CONTRIBUTING--39484b80/task.md) — HANDOVER-CONTRIBUTING: CONTRIBUTING.md -- how this repo is actually developed, and how to… (todo)
- [INVITE-MINT](../../INVITE/INVITE-MINT--1d0d0e60/task.md) — INVITE-MINT: an operator mints a single-use, expiring invite -- the server is authoritati… (done)
- [MTLS-ROTATE](../../MTLS/MTLS-ROTATE--c2e8df5b/task.md) — MTLS-ROTATE: a client accepts a SET of pinned bus certificates so a rotation does not for… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-CLAUDE-TRIM](../CONTEXT-CLAUDE-TRIM--6ef1d88e/task.md) — CONTEXT-CLAUDE-TRIM: the agent roster descriptions and model-selection rationale leave CL… (done)
- [CONTEXT-DONEGATE-CANON](../CONTEXT-DONEGATE-CANON--b9b0c654/task.md) — CONTEXT-DONEGATE-CANON: 'do not mark done when the behaviour is not yet live' said once,… (todo)
- [CONTEXT-DRIFT-WRAPPERS](../CONTEXT-DRIFT-WRAPPERS--1a9bf503/task.md) — CONTEXT-DRIFT-WRAPPERS: two per-spawn files still call the retired shell wrappers 'the ON… (todo)
- [CONTEXT-LOG-GUARD](../CONTEXT-LOG-GUARD--f39083ae/task.md) — CONTEXT-LOG-GUARD: the AGENT_LOG.md freeze is enforced mechanically, not hoped for (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
