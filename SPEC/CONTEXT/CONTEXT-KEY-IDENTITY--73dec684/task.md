# CONTEXT-KEY-IDENTITY: Standardize task identity (public_id vs key) before SPEC/&lt;epic&gt;/&lt;task&gt;/ directory naming

| Field | Value |
| --- | --- |
| Public id | `73dec684-a46f-4b06-93f4-8b2e81527472` |
| Key | _(null in the export)_ |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | spec-server |
| Section | backlog |
| Tags | spec-mirror, identity, blocks-CONTEXT-SPEC-TREE |
| Created | 2026-08-14T11:11:34.256817+00:00 |
| Updated | 2026-08-14T21:01:07.125605+00:00 |
| Completed | — |

## Proof command

```sh
grep -qi 'public_id.*identity\|standardiz.*public_id' DECISIONS.md && test -d SPEC/UNASSIGNED && echo KEY_IDENTITY_DECISION_RECORDED
```

## Status note

AUDITED 2026-08-14, LEFT OPEN. TWO of three deliverables verified DONE in practice: (1) CONTEXT-SPEC-TREE's directory-naming scheme correctly uses public_id as the leaf identity for EVERY task (e.g. SPEC/SIGN/SIGN-1--43fd21ae/), uniform whether key is populated or not, matching this task's decision items 1/3; (2) epic_key=null tasks land in an explicit SPEC/UNASSIGNED/ bucket rather than being dropped, matching decision item 4. NOT done: the DECISION ITSELF is not recorded in DECISIONS.md (grep for public_id anywhere in DECISIONS.md returns nothing) -- this task's own description requires the decision be written down in DECISIONS.md or the doc that owns spec-mirror conventions, and it currently exists only as the de-facto behaviour of gen-spec-mirror.sh, not as a stated, citable decision. Server-side constraint question (raised by the coordinator): answered NO, not this task's scope -- that half is tracked separately and correctly related as SPEC-API-LIST-SILENT-TRUNCATION (82f35b73, todo, relates edge already wired per this task's own notes from earlier today). Also corrected the proof_cmd: the original just printed counts with no assertion (len(t)>0 is trivially true against 587 tasks) -- it could never fail, so it verified nothing. Replaced with a real check for the DECISIONS.md entry plus the UNASSIGNED bucket, confirmed RED now.

## Description

Blocks CONTEXT-SPEC-TREE (public_id ff15e9ff-7e2b-4c4a-abf6-28c010dc9bb0), which was going to name SPEC/<epic>/<task>/ directories after the human key -- and 185/513 backlog tasks (36%) have key=null, which would produce 185 unnamed directories.

Finding, verified against a fresh pull of /export?format=json on 2026-08-14:
- 513 tasks total.
- 328 have a non-null key; 185 have key=null.
- 405 carry a KEY-style prefix in the TITLE (e.g. "RELAY-41: ..."), which is not the same population as the 328 with a populated key column -- there are keyed tasks whose title doesn't parse back to that key cleanly, and keyless tasks whose title merely LOOKS prefixed. A quick, reasonably-shaped regex probe (^[A-Za-z][A-Za-z0-9_-]*-\d+\s*:) against the live export misclassified 113 of the 328 keyed tasks and 10 of the 185 keyless tasks in a single pass -- concrete evidence, not a hedge, that title-prefix parsing is a heuristic over free text and will misfire if used to mint or backfill identity.
- Separately, 6/513 tasks have epic_key=null -- a stable set (was 6/511 before this task and CONTEXT-SPEC-DEPS were filed), which also needs an explicit bucket (e.g. SPEC/_unassigned/) in the tree rather than being dropped or crashing the generator.
- key is immutable after creation (confirmed via /openapi.json: TaskPatch's property list has no key field; only TaskIn, at create time, does) -- so the collision risk is a CREATE-time race between agents minting the same key independently (the documented MOBILE-21 precedent), not later mutation. This narrows but does not remove the concern for the 328 keys that predate the task-key-<EPIC> reservation-namespace convention.

Decision this task must make and record (in DECISIONS.md or CONTRACTS-relevant doc, whichever owns spec-mirror conventions):
1. Standardize on public_id as the identity ALL machine tooling (CONTEXT-SPEC-TREE's directory names, CONTEXT-SPEC-DEPS's relation targets, any future cross-reference) resolves against. public_id is server-minted, globally unique, and present on 513/513 tasks.
2. key remains a human-readable DISPLAY label wherever populated (surfaced in task.md/epic.md headings), never a join key, and is NEVER backfilled by parsing titles.
3. Directory naming for CONTEXT-SPEC-TREE: use public_id as the directory leaf for every task (uniform for all 513, not just the 185 keyless ones), with key shown as a label inside the file when present.
4. epic_key=null tasks: land in an explicit SPEC/_unassigned/ bucket, not silently dropped.

This task should land BEFORE or alongside CONTEXT-SPEC-TREE's implementation, since it changes what CONTEXT-SPEC-TREE's directory-naming scheme is.

See CONTEXT-SPEC-DEPS (public_id 8280358d-fc62-4376-8e24-52d43236a4a8) kind=report note for the full design writeup and numeric verification this finding is drawn from.

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
- [CONTEXT-SPEC-TREE](../CONTEXT-SPEC-TREE--ff15e9ff/task.md) — CONTEXT-SPEC-TREE: Split SPEC.md mirror into a directory tree (done)
- [RELAY-41](../../RELAY/RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SPEC-API-LIST-SILENT-TRUNCATION](../../UNASSIGNED/SPEC-API-LIST-SILENT-TRUNCATION--82f35b73/task.md) — Task-list API silently truncates at 200 with no total, no next and no working pagination… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-SPEC-DEPS](../CONTEXT-SPEC-DEPS--8280358d/task.md) — CONTEXT-SPEC-DEPS: Adopt and document the blocks-relation convention for task dependencies (todo)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
