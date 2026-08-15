# CONTEXT-STALE-NOTYET: a doc-check \`forbid\` mode, so a "not yet implemented" note cannot outlive its implementation

| Field | Value |
| --- | --- |
| Public id | `67b42913-5707-4fa0-b898-0c2dd654c801` |
| Key | CONTEXT-STALE-NOTYET |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | TOOLING |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:14:11.933751+00:00 |
| Updated | 2026-08-15T08:14:11.933751+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/doc-check.sh --selftest && bash scripts/doc-check.sh forbid && echo NOTYET_GUARD_OK
```

## Description

A "not yet implemented" note that outlived its implementation is MORE dangerous than no note at all, because
it reads as freshly checked. Three instances surfaced on 2026-08-15 ALONE:

1. `CLAUDE.md` / `AGENTS.md` line 20 -- "enrolment is NOT yet invite-gated" -- corrected at `aade191`
   (docs-only). It sat INSIDE the very paragraph that warns "Do not build on a guarantee without checking it
   holds", which is the sharpest possible illustration of the problem.
2. `internal/idem/store.go` -- a "not live at that commit" note that is no longer true.
3. `client/enrol.go:63-66` -- the EnrolOptions.Invite doc still says a nil invite "is still accepted, because
   the bus still accepts an un-invited enrolment". Contradicted by `enrolmentInviteRequired = true`
   (`cmd/agent-bus/main.go:66`, set by `3cedcb7`). Owned by INVITE-GATE-ENFORCE-FU-CLIENTREMEDY
   (`d4ff825f-1c89-49b0-9bb1-d23e51af6adb`) -- NOT duplicated here.

**THIS TASK IS THE CLASS, NOT A FOURTH INSTANCE.** Each individual fix stays with its own owner; this one
builds the mechanism that stops the fourth instance from needing a human to notice it. Do not "fix" the three
above here.

## Why the existing instruments do not catch it

`doc-check.sh` (CONTEXT-DOCCHECK, `b3b28f45-54b3-4d0e-bde7-933c9c3923b2`) has `section`, `budget` and
`--selftest`. Its preserve check asserts a phrase is **MISSING** -- it fires when required text has been
deleted. The stale-note failure is the MIRROR IMAGE: text that should have been deleted is still PRESENT. No
current mode expresses that, which is why all three instances were caught by humans reading code.

## Deliverable

Add a `forbid` mode to `scripts/doc-check.sh`, symmetric with the existing preserve check: read a
`docs/doc-forbid.tsv` of (path, literal_phrase) pairs and FAIL if the phrase is present. Register the phrases
from the three instances above so each is permanently pinned once its owner fixes it.

Requirements:
- **It must not pass vacuously.** A missing path, an empty tsv, or a malformed row must FAIL, not silently
  succeed -- the exact failure mode CONTEXT-DOCCHECK already guards against for `section` ("exits non-zero if
  the heading is absent, cannot pass vacuously"). Extend `--selftest` to cover `forbid`: phrase-present must
  FAIL, phrase-absent must PASS, missing-file must FAIL.
- Match a LITERAL phrase, not a regex, for the same reason the repo's grep proofs keep failing: an
  incidental or over-broad match is worse than no check.
- **Must NOT invoke `scripts/proof-check.sh`** -- recursion, per CONTEXT-DOCCHECK's own constraint and the
  observed incident in `69eb6f56`.
- Document the new mode in CONTRACTS-AGENT.md's repo-tooling section, where `doc-check.sh` is documented.

## Scope discipline

This is a MECHANISM task. It does not attempt to detect stale notes in general -- that is undecidable, and an
approximate detector would produce false positives and be switched off within a day (the same reasoning
`proof-check.sh` records for why its default check is deliberately the weakest useful one). It only makes a
KNOWN stale phrase impossible to reintroduce, and gives the next such discovery a place to be recorded in one
line instead of a new orphan task.

## Blocked on CONTEXT-DOCCHECK

`scripts/doc-check.sh` must exist and be COMMITTED first. Note it is currently UNTRACKED in the worktree, so a
proof that depends on it PASSES in the working tree and FAILS in a clean overlay of HEAD -- verified by
spec-keeper at filing: in an overlay the script is simply absent. That is exactly the hazard CLAUDE.md's
"verify in a clean overlay of HEAD, not in your working tree" rule exists for. Run this task's proof in an
overlay.

## Proof

RED verified by spec-keeper at filing (2026-08-15): proof-check reports verdict=FAIL, exit 2 -- `doc-check.sh`
has no `forbid` mode today (its dispatch handles only section, budget and --selftest). Also verified RED in a
clean overlay of HEAD for the separate reason that the script is untracked there.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md)
- **relates to** [CONTEXT-STALE-INPLACE](../CONTEXT-STALE-INPLACE--ec7fc25e/task.md)
- **relates to** [INVITE-CLIENT-FU-DOCSTALE](../../INVITE/INVITE-CLIENT-FU-DOCSTALE--66266b5f/task.md)
- **relates to** [INVITE-GATE-ENFORCE-FU-CLIENTREMEDY](../../INVITE/INVITE-GATE-ENFORCE-FU-CLIENTREMEDY--d4ff825f/task.md)
- **relates to** [INVITE-GATE-ENFORCE-FU-INVITEDOCS](../../INVITE/INVITE-GATE-ENFORCE-FU-INVITEDOCS--47c7bae9/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md) — CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on (todo)
- [INVITE-GATE-ENFORCE-FU-CLIENTREMEDY](../../INVITE/INVITE-GATE-ENFORCE-FU-CLIENTREMEDY--d4ff825f/task.md) — INVITE-GATE-ENFORCE-FU-CLIENTREMEDY: fix client/enrol.go remedy text for the no-invite 403 (todo)
- [PROOF-CHECK-FU-RECURSION](../../TOOLING/PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) — PROOF-CHECK-FU-RECURSION: bash scripts/proof-check.sh hangs / spawns runaway processes wh… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-STALE-INPLACE](../CONTEXT-STALE-INPLACE--ec7fc25e/task.md) — CONTEXT-STALE-INPLACE: DECISIONS.md section 2 and the Dockerfile CMD block state a supers… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
