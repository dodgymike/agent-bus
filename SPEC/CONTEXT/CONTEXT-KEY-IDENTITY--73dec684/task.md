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
| Updated | 2026-08-14T11:11:34.256817+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/spec-cloud.sh -sf '/api/v1/projects/agent-bus/export?format=json' | python3 -c "import json,sys; d=json.load(sys.stdin); t=d['tasks']; assert len(t)>0; nullkey=[x for x in t if not x.get('key')]; nullepic=[x for x in t if not x.get('epic_key')]; print('keyless:',len(nullkey),'epicless:',len(nullepic))"
```

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

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-SPEC-DEPS](../CONTEXT-SPEC-DEPS--8280358d/task.md) — CONTEXT-SPEC-DEPS: Adopt and document the blocks-relation convention for task dependencies (todo)
- [CONTEXT-SPEC-TREE](../CONTEXT-SPEC-TREE--ff15e9ff/task.md) — CONTEXT-SPEC-TREE: Split SPEC.md mirror into a directory tree (todo)
- [RELAY-41](../../RELAY/RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-SPEC-DEPS](../CONTEXT-SPEC-DEPS--8280358d/task.md) — CONTEXT-SPEC-DEPS: Adopt and document the blocks-relation convention for task dependencies (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
