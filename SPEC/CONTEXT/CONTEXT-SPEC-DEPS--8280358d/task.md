# CONTEXT-SPEC-DEPS: Adopt and document the blocks-relation convention for task dependencies

| Field | Value |
| --- | --- |
| Public id | `8280358d-fc62-4376-8e24-52d43236a4a8` |
| Key | _(null in the export)_ |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | spec-server |
| Section | backlog |
| Tags | spec-server, schema, dependencies |
| Created | 2026-08-14T11:05:52.915014+00:00 |
| Updated | 2026-08-14T11:15:29.585299+00:00 |
| Completed | — |

## Proof command

```sh
grep -qi 'depends_on.*blocks\|X depends_on Y' DECISIONS.md && grep -n 'kind.*blocks' AGENTS_API.md | grep -qi depend
```

## Description

RESCOPED 2026-08-14 -- the original framing ("add a real depends_on field to the task model") was WRONG: the Spec Server already has a real, stored, queryable dependency-graph primitive on a separate resource -- POST/GET /projects/{slug}/tasks/{ident}/relations, kind in {blocks, supersedes, relates, follow_up}. Verified live against /openapi.json, and the directionality/cycle/side-effect behaviour of `blocks` has now been empirically confirmed on disposable scratch tasks (ZZ-SCRATCH-RELATIONS-A/B, now cancelled -- see this task's notes for the full probe).

CONFIRMED CONVENTION: "X depends_on Y" (Y must finish before X can start) == POST to Y's relations {"kind":"blocks","target":X-public_id}. Y is the source/blocker (the URL ident), X is the target/dependent (the body field). Read back via GET .../relations on either side: the source sees direction=outgoing, the target sees direction=incoming, both under kind=blocks. `blocks` is inert metadata -- confirmed no side effect on either task's status/status_note/version, unlike `supersedes` which mutates the target's status. Cycles are NOT rejected or corrupted by the server -- confirmed by creating a 2-cycle (A blocks B, B blocks A) and reading both sides back cleanly; any consumer of this graph MUST do cycle-safe traversal.

Scope, now:
1. Document the confirmed convention above in DECISIONS.md (dated entry) and in AGENTS_API.md's "Relations" section (or the Spec Server's own docs, whichever owns it) -- the undocumented directionality was the actual gap; the storage mechanism already existed.
2. State plainly, alongside the convention: `blocks` is permissive of cycles (no server-side prevention) and is inert (no status side effect) -- both now measured facts, not assumptions.
3. CONTEXT-SPEC-TREE's epic.md renderer, when it moves from derived/parsed-from-description references to real relations, MUST implement cycle-safe traversal (visited-set/recursion-guard, flag every member of a detected cycle) as a hard requirement, not an aspiration.
4. Backfilling the ~225+ existing free-text dependency references ("blocked by RELAY-34", "gates F9/F12") into real relations edges remains explicitly OUT of scope for this task -- a large one-time migration to be filed separately once this convention is documented and agreed.
5. Target identity for new edges: always public_id (see CONTEXT-KEY-IDENTITY, public_id 73dec684-a46f-4b06-93f4-8b2e81527472) -- never key, since 36% of tasks have key=null.

No schema/server change is required. This is a documentation + convention task.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-KEY-IDENTITY](../CONTEXT-KEY-IDENTITY--73dec684/task.md) — CONTEXT-KEY-IDENTITY: Standardize task identity (public_id vs key) before SPEC/&lt;epic&gt;/&lt;ta… (todo)
- [CONTEXT-SPEC-TREE](../CONTEXT-SPEC-TREE--ff15e9ff/task.md) — CONTEXT-SPEC-TREE: Split SPEC.md mirror into a directory tree (todo)
- [RELAY-34](../../RELAY/RELAY-34--03fd8897/task.md) — RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can… (done)
- [ZZ-SCRATCH-RELATIONS-A](../../ZZ-SCRATCH/ZZ-SCRATCH-RELATIONS-A--08b120b6/task.md) — ZZ-SCRATCH-RELATIONS-A: scratch task, relations-endpoint probe (spec-keeper, safe to igno… (cancelled)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-KEY-IDENTITY](../CONTEXT-KEY-IDENTITY--73dec684/task.md) — CONTEXT-KEY-IDENTITY: Standardize task identity (public_id vs key) before SPEC/&lt;epic&gt;/&lt;ta… (todo)
- [CONTEXT-SPEC-TREE](../CONTEXT-SPEC-TREE--ff15e9ff/task.md) — CONTEXT-SPEC-TREE: Split SPEC.md mirror into a directory tree (todo)
- [ZZ-SCRATCH-RELATIONS-A](../../ZZ-SCRATCH/ZZ-SCRATCH-RELATIONS-A--08b120b6/task.md) — ZZ-SCRATCH-RELATIONS-A: scratch task, relations-endpoint probe (spec-keeper, safe to igno… (cancelled)
- [ZZ-SCRATCH-RELATIONS-B](../../ZZ-SCRATCH/ZZ-SCRATCH-RELATIONS-B--50b6857a/task.md) — ZZ-SCRATCH-RELATIONS-B: scratch task, relations-endpoint probe (spec-keeper, safe to igno… (cancelled)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
