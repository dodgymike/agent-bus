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

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

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
- [DOCS-10](../../DOCS/DOCS-10--d6c84ff8/task.md) — DOCS-10: \`client\` package documents fail-closed verification while shipping fail-open (todo)
- [DOCS-11](../../DOCS/DOCS-11--a434830e/task.md) — DOCS-11: Invite revocation is documented in three places and implemented in none (todo)
- [DOCS-12](../../DOCS/DOCS-12--7b363ccf/task.md) — DOCS-12: 8 error remedies name \`agent-busctl keygen\` / \`trust\`, which do not exist (todo)
- [DOCS-13](../../DOCS/DOCS-13--8ce01598/task.md) — DOCS-13: \`INVARIANTS.md\` truth pass — 8 false factual claims in the file agents must read… (todo)
- [DOCS-14](../../DOCS/DOCS-14--86741a89/task.md) — DOCS-14: \`CLAUDE.md\`/\`AGENTS.md\`: delete the false \`crypto/ecdh\` toolchain rationale; fix… (todo)
- [DOCS-15](../../DOCS/DOCS-15--e718e0c0/task.md) — DOCS-15: \`AGENTS.md\` writes fabricated model ids (\`Codex-opus-5\`) into the cost audit tra… (todo)
- [DOCS-16](../../DOCS/DOCS-16--57933ce7/task.md) — DOCS-16: \`PROTOCOL.md\`'s on-disk version registry omits versions 5, 6 and 7 — a live rese… (todo)
- [DOCS-17](../../DOCS/DOCS-17--a35d1ec1/task.md) — DOCS-17: Session per-agent cap (32, no eviction) is documented as not existing — caused a… (todo)
- [DOCS-18](../../DOCS/DOCS-18--5b3f4886/task.md) — DOCS-18: Retire two standing directives that outlived their premise and now FORBID the fix (todo)
- [DOCS-19](../../DOCS/DOCS-19--9d8ff93b/task.md) — DOCS-19: Durability inverted: \`internal/auth/service.go:502\` says main injects the MEMORY… (todo)
- [DOCS-20](../../DOCS/DOCS-20--55d5bac2/task.md) — DOCS-20: Mechanical stale-claim detector — likely to MERGE with the in-flight \`scripts/do… (todo)
- [DOCS-21](../../DOCS/DOCS-21--cdf8660c/task.md) — DOCS-21: \`CONTRACTS-CLI.md\` claims a "mechanically enforced" import guard that nothing ru… (todo)
- [DOCS-22](../../DOCS/DOCS-22--2f8ae959/task.md) — DOCS-22: The four agent ENTRY POINTS the invite gate missed — \`README\` Quickstart, \`agent… (done)
- [DOCS-23](../../DOCS/DOCS-23--c9a51528/task.md) — DOCS-23: \`agent-busctl broadcast --help\` never says the route is refused (501) (todo)
- [DOCS-24](../../DOCS/DOCS-24--4aaf2803/task.md) — DOCS-24: \`client/transport.go:429-430\`: the 403 remedy tells an agent to retry a refusal… (todo)
- [DOCS-25](../../DOCS/DOCS-25--9c894053/task.md) — DOCS-25: \`CONTRACTS-AGENT.md\` documents the log-scrape that \`bus-serve.sh\` deliberately r… (todo)
- [DOCS-26](../../DOCS/DOCS-26--fb39c79d/task.md) — DOCS-26: \`docs/THREE-BUS-DOCKER.md\` tells the operator to ignore \`fed-smoke.sh\`, mint an… (todo)
- [DOCS-27](../../DOCS/DOCS-27--ec19df4e/task.md) — DOCS-27: \`AGENT_PROTOCOL.md\`: \`client-cert\` undocumented (invariant-7 gap), TOC lists 10… (todo)
- [DOCS-28](../../DOCS/DOCS-28--7f1030b7/task.md) — DOCS-28: \`docs/comms\` self-audit numbers disagree with the CSVs, and \`LABELLING-KEY.md\`'s… (todo)
- [DOCS-29](../../DOCS/DOCS-29--7b0e66e8/task.md) — DOCS-29: Investigate \`TestWALRepairDoesNotReissueDiscardedIndex\` — a P0's recorded \`proof… (todo)
- [DOCS-5](../../DOCS/DOCS-5--051a9829/task.md) — DOCS-5: \`/v1/discovery\` limitation 5 is false on the wire: cross-bus relay IS served (todo)
- [DOCS-6](../../DOCS/DOCS-6--76879ad1/task.md) — DOCS-6: README is unusable: quickstart 403s, "what works today" curls a TLS port in plain… (todo)
- [DOCS-7](../../DOCS/DOCS-7--a98ffca6/task.md) — DOCS-7: Doc-truth sweep: enrolment is invite-gated (11 passages, 5 files) (todo)
- [DOCS-8](../../DOCS/DOCS-8--1f955f09/task.md) — DOCS-8: Doc-truth sweep: relay is mounted, live and imported (17 passages) — incl. 3 \`MUS… (todo)
- [DOCS-9](../../DOCS/DOCS-9--873417cb/task.md) — DOCS-9: P0-adjacent: reserve a relay wire-protocol version — the envelope is on the wire… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
