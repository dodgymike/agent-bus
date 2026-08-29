# Stale CONTRACTS.md pointers after the CONTRACTS-SPLIT: README.md:88, AGENT_PROTOCOL.md:122, CLAUDE.md:332

| Field | Value |
| --- | --- |
| Public id | `f0ef1ed9-cbcb-4ddd-9dec-394e1800ae78` |
| Key | _(null in the export)_ |
| Epic | [DOCS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | documentation |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T20:52:59.595769+00:00 |
| Updated | 2026-08-08T10:29:53.212591+00:00 |
| Completed | — |

## Proof command

```sh
grep -qF "CONTRACTS-HTTP.md" README.md && grep -qF "CONTRACTS-CLI.md" README.md && grep -qF "CONTRACTS-ONDISK.md" README.md && ! grep -qF ") — every route, flag, env var, and record type" README.md && grep -qF "CONTRACTS-HTTP.md" AGENT_PROTOCOL.md && ! grep -qF "see `CONTRACTS.md`, `## Authentication`" AGENT_PROTOCOL.md && grep -A2 "remaining shared files" CLAUDE.md | grep -qF "CONTRACTS-" && ! grep -qF "For the remaining shared files (`DECISIONS.md`, `AGENT_LOG.md`, `CONTRACTS.md`), only ONE agent at" CLAUDE.md
```

## Status note

proof-check.sh verdict on file 2026-08-02 (pre-fix): verdict=FAIL class=file-assertion exit=1 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0 -- confirmed RED today: all three stale fragments verified present via individual grep -qF checks (README.md exit 0 match, AGENT_PROTOCOL.md exit 0 match, CLAUDE.md exit 0 match), so the compound FAIL is for the right reason, not a spurious one.

## Description

Discovered by the CONTRACTS-SPLIT agent (360a2679, 2026-08-02) while splitting CONTRACTS.md into per-plane files (CONTRACTS-CLI/HTTP/ONDISK/AGENT.md, with CONTRACTS.md left as an index). That agent flagged but could not fix these -- outside its file-ownership boundary for that pass:

1. README.md:88 -- `- [`CONTRACTS.md`](./CONTRACTS.md) — every route, flag, env var, and record type` still claims CONTRACTS.md directly HOLDS that table. It does not any more; it is now a short index pointing at the four plane files. Fix: reword to describe it as the index, and/or link the plane files directly.

2. AGENT_PROTOCOL.md:122 -- `... see `CONTRACTS.md`, `## Authentication`) ...` cites a specific heading, `## Authentication`, inside CONTRACTS.md. That heading no longer exists there -- it moved verbatim to CONTRACTS-HTTP.md:192 (`## Authentication (added 2026-08-02)`) in the split. Fix: repoint the citation to CONTRACTS-HTTP.md.

3. CLAUDE.md:332 (Parallel-agent coordination section) -- `- For the remaining shared files (`DECISIONS.md`, `AGENT_LOG.md`, `CONTRACTS.md`), only ONE agent at a time; prefer adding a new dated section over editing existing lines.` This is actively MISLEADING post-split: naming CONTRACTS.md alongside DECISIONS.md/AGENT_LOG.md as a single-writer-contended file is exactly the chokepoint the split (360a2679) existed to remove -- three P0s across two triage loops were caused by concurrent agents needing to land a doc update in that one file. Leaving this warning in place would keep agents needlessly serialising on a file that no longer holds the contended content (CONTRACTS.md is now a stable ~36-line index; the actual content lives in CONTRACTS-CLI/HTTP/ONDISK/AGENT.md, each independently editable). Fix: remove CONTRACTS.md from this single-writer list (the plane files still need their own single-writer discipline if a task touches more than one at once, but that is a materially different, narrower risk than the old whole-file chokepoint).

NOTE: CLAUDE.md line ~158 (repository-layout section) and step 9 were ALREADY updated by the split agent to name CONTRACTS.md as INDEX only -- this task is only the three residual pointers above, do not re-touch line 158.

PROOF STRENGTHENED 2026-08-02 (spec-keeper): the original proof_cmd was three negative assertions only, which is satisfiable by DELETING the three stale lines rather than fixing them (the same structural flaw fixed on 5b178dde) -- it now also requires positive evidence that each file points at the correct replacement (README.md cites CONTRACTS-HTTP.md/CONTRACTS-CLI.md/CONTRACTS-ONDISK.md, AGENT_PROTOCOL.md cites CONTRACTS-HTTP.md, and CLAUDE.md's "remaining shared files" bullet now names a CONTRACTS-*.md plane file instead of just dropping CONTRACTS.md from the list).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [CONTEXT-DRIFT-WRAPPERS](../../CONTEXT/CONTEXT-DRIFT-WRAPPERS--1a9bf503/task.md)
- **blocks** [HANDOVER-README](../../HANDOVER/HANDOVER-README--1dc9cf90/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTRACTS-SPLIT](../CONTRACTS-SPLIT--360a2679/task.md) — CONTRACTS-SPLIT: split CONTRACTS.md into per-plane files (pure move) + retarget every pro… (done)
- [DUR-11-FU-CONTRACTS](../DUR-11-FU-CONTRACTS--5b178dde/task.md) — DUR-11-FU-CONTRACTS: CONTRACTS.md still documents the reverted refuse-to-start WAL policy… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-DRIFT-WRAPPERS](../../CONTEXT/CONTEXT-DRIFT-WRAPPERS--1a9bf503/task.md) — CONTEXT-DRIFT-WRAPPERS: two per-spawn files still call the retired shell wrappers 'the ON… (todo)
- [HANDOVER-README](../../HANDOVER/HANDOVER-README--1dc9cf90/task.md) — HANDOVER-README: README stops telling a human things that are false (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
