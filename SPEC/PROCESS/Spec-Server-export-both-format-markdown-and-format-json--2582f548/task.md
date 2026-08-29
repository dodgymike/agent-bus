# Spec Server /export (both format=markdown and format=json) silently drops the commits\[\] array that /complete correctly persists -- SPEC.md and format=json readers see no commit_sha/test_summary even though the server holds it

| Field | Value |
| --- | --- |
| Public id | `2582f548-6493-439c-ba71-7f5cf73650fc` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-07T19:09:36.066024+00:00 |
| Updated | 2026-08-08T10:29:59.792532+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/tasks/8c46dc93-16d0-4eea-8ad3-ac51136551e2" | python3 -c "import json,sys; t=json.load(sys.stdin); assert t.get('commits'), 'expected commits on direct GET'" && bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/export?format=json" | python3 -c "import json,sys; d=json.load(sys.stdin); mt=[t for t in d['tasks'] if t.get('public_id')=='8c46dc93-16d0-4eea-8ad3-ac51136551e2'][0]; assert 'commits' not in mt, 'export now includes commits -- bug fixed, flip this task'" && echo EXPORT_DROPS_COMMITS_BUG_CONFIRMED
```

## Description

CORRECTION TO THE ORIGINAL BRIEF (verified 2026-08-07 by spec-keeper before filing, per instructions): the claim "the Spec Server does not persist commit_sha or test_summary at all" is FALSE. It IS persisted. What is actually broken is narrower: the `/export` endpoint (both `format=markdown`, which is what generates SPEC.md, and `format=json`) silently DROPS the commit record, even though the server holds it and two other surfaces expose it correctly.

REPRODUCTION (ran against the live cloud server, project agent-bus, task MTLS-PIN / public_id 8c46dc93-16d0-4eea-8ad3-ac51136551e2, completed with commit_sha=61e6067):

1. Direct single-task GET DOES carry the commit:
   `bash scripts/spec-cloud.sh -s /api/v1/projects/agent-bus/tasks/8c46dc93-16d0-4eea-8ad3-ac51136551e2`
   -> top-level field `"commits": [{"created_at":"2026-08-07T18:56:39.469539+00:00","repo":null,"sha":"61e6067","test_summary":"proof-check.sh verdict=PASS ..."}]` is present and correct.

2. The task LIST endpoint also carries it:
   `bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/tasks?status=done&limit=500"`
   -> every returned task object includes the same `commits` array (verified: of 64 tasks with status=done, 64/64 -- ALL of them -- have a non-empty `commits` array; 0 are missing it at the source-of-truth level). So the blast radius the original brief worried about ("every task ever completed ... has an unverifiable completion claim") does not exist: nothing has been lost.

3. The `completed` event also carries it independently:
   `bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/events?task=8c46dc93-16d0-4eea-8ad3-ac51136551e2&event_type=completed&limit=5"`
   -> `payload` = `{"commit_sha":"61e6067","proof_cmd":"...","test_summary":"proof-check.sh verdict=PASS ..."}`. Same values, third independent surface.

4. THE ACTUAL BUG -- the export endpoint, both formats, drops the field entirely:
   `bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/export?format=json" | python3 -c "import json,sys; d=json.load(sys.stdin); print(sorted(d['tasks'][0].keys()))"`
   -> `['completed_at', 'component', 'created_at', 'description', 'epic_key', 'key', 'position', 'priority', 'proof_cmd', 'public_id', 'section', 'status', 'status_note', 'tags', 'title', 'updated_at']` -- no `commits`, no `commit_sha`, no `test_summary`. Confirmed the same for the markdown export consumed into SPEC.md: `grep -n '61e6067' SPEC.md` returns nothing; the only occurrence of the literal string "commit_sha" anywhere in SPEC.md (`grep -c commit_sha SPEC.md` = 1) is free prose inside an unrelated task's description ("commit_sha will be 10dd7f4 plus ..."), not a rendered field.

DOES /complete ERROR OR SILENTLY ACCEPT? Neither in the sense the brief feared -- it accepts and CORRECTLY PERSISTS commit_sha/test_summary (see reproduction 1-3 above; 64/64 done tasks have it). There is no silent-drop at the /complete or GET layer. The silent drop is specifically in /export's task-serialisation, which uses a narrower field projection than the GET/list endpoints.

WHY THIS STILL MATTERS: SPEC.md is the human/mirror-reading surface (CLAUDE.md: "SPEC.md is a GENERATED MIRROR ... treat it as read-only history that other agents/tools (and humans) can skim"). Anyone who trusts the mirror for "what commit closed this task" sees nothing, even though the server has the answer -- the same *class* of defect as e109c867 (PATCH rejecting `key`): documented workflow and actual server contract disagreeing, just at the export layer rather than at /complete itself. It is P2, not P0/P1, precisely because reproduction 1-3 show no data has actually been lost -- it is a visibility gap in the mirror, not a durability gap in the store.

INTERIM MITIGATION (already standard practice, now written down rather than left to habit): spec-keeper continues to record commit_sha/test_summary redundantly in a `kind=report` note on the task at completion time, in addition to the `commit` API field -- e.g. "Completed with commit_sha=<sha>." This is now belt-and-braces (the primary record survives fine in `commits[]`), but keep doing it because it is the only copy that reaches SPEC.md today, and because free-text notes are more visible to a human skimming the mirror's linked task detail than a field the export layer currently discards.

FIX (out of scope for this bookkeeping task, left for an implementer): have the export serialiser (both format=json and the markdown renderer that produces SPEC.md) include each task's `commits` array, or at minimum the latest entry's `sha`/`test_summary`, alongside the existing fields.

CROSS-REFERENCE: e109c867 (PATCH rejecting `key`) -- same class, workflow/contract mismatch discovered by direct empirical testing rather than by trusting the docs.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-PIN](../../MTLS/MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [e109c867-fcd2-4ddc-bc4d-55779dc5f5e1](../Spec-Server-PATCH-tasks-id-rejects-the-key-field-outrigh--e109c867/task.md) — Spec Server: PATCH /tasks/{id} rejects the key field outright (422 Unknown field) -- a ke… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
