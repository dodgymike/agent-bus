# Spec Server: PATCH /tasks/{id} rejects the key field outright (422 Unknown field) -- a keyless task can never acquire one in place

| Field | Value |
| --- | --- |
| Public id | `e109c867-fcd2-4ddc-bc4d-55779dc5f5e1` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-07T19:02:10.722685+00:00 |
| Updated | 2026-08-08T10:29:59.439468+00:00 |
| Completed | — |

## Proof command

```sh
PID=$(bash scripts/spec-cloud.sh -s -X POST "$B/projects/agent-bus/tasks" -H "Content-Type: application/json" -d '{"title":"keypatch-probe"}' | jq -r .public_id) && bash scripts/spec-cloud.sh -s -X PATCH "$B/projects/agent-bus/tasks/$PID" -H "Content-Type: application/json" -d '{"key":"KEYPATCH-PROBE-1"}' >/dev/null 2>&1; bash scripts/spec-cloud.sh -s "$B/projects/agent-bus/tasks/$PID" | jq -r .key | grep -q KEYPATCH-PROBE-1
```

## Description

Observed 2026-08-07 while bookkeeping the agent-bus backlog. CLI-1-FU-BINARYNAME (public_id 6a1eb5fa-5cfe-4808-a47d-224092f69c14) was created with key: null, and CLAUDE.md / task descriptions across this project cite it by the title-embedded name "CLI-1-FU-BINARYNAME" as if it were a real key -- it is not; it has no key at all.

CORRECTION TO THE ORIGINAL DISPATCH BRIEF, recorded here rather than silently fixed: the brief that raised this described the bug as "PATCH silently ignores key". Empirically that is NOT what happens -- confirmed live against the running server, 2026-08-07. PATCH /tasks/{id} with a body containing "key":"..." returns HTTP 422 {"errors":{"json":{"key":["Unknown field."]}}}. The request is REJECTED, not silently accepted-and-dropped. The observable consequence is the same either way -- a keyless task can never acquire a key post-creation through the documented PATCH surface -- but the mechanism is a loud validation error, not a silent no-op, and the earlier characterisation should not be repeated.

CONSEQUENCE: key is accepted only at creation time (POST .../tasks {"key":"...", ...}) per AGENTS_API.md's 'Create a task' example. There is no documented way to add or change a key on an existing task via the single-task PATCH endpoint. Our own docs and task descriptions routinely cite tasks by key (e.g. "BLOCKS: INVITE-GATE", "DEPENDS ON: MTLS-BUSCERT"); a keyless task silently breaks that convention for anyone or anything resolving by key.

WORKAROUND on record: an export/import round-trip. GET /projects/{slug}/export?format=json returns every task including keyless ones with stable public_id; import is documented as idempotent on public_id (POST /projects/{slug}/import), so editing the key field in the exported JSON before re-importing should update it in place -- not verified end-to-end in this pass, flagged for whoever picks this up to confirm import actually treats key as updatable where PATCH does not.

REPRODUCTION (run 2026-08-07, task subsequently cancelled -- public_id e36661b0-687e-465e-b72f-e33245088e38):
  1. POST /projects/agent-bus/tasks {"title":"probe"}  (no key field) -> 201, public_id=P, key=null
  2. PATCH /projects/agent-bus/tasks/{P} {"key":"PROBE-1"} -> 422 {"errors":{"json":{"key":["Unknown field."]}}}
  3. GET /projects/agent-bus/tasks/{P} -> key is still null, confirming (2) was rejected outright, not applied

Fix: either add key to PATCH's accepted schema (uniqueness-checked, same as at creation), or -- if key is deliberately immutable-after-creation by design -- say so explicitly in AGENTS_API.md's PATCH section so the export/import workaround is the documented path rather than something an agent has to discover by trial and error.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-1-FU-BINARYNAME](../../CLI/CLI-1-FU-BINARYNAME--6a1eb5fa/task.md) — CLI-1-FU-BINARYNAME: Decide the INSTALLED name of the client binary (done)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (todo)
- [MTLS-BUSCERT](../../MTLS/MTLS-BUSCERT--93f0dc19/task.md) — MTLS-BUSCERT: generate/load the bus's self-signed certificate + private key in the data d… (done)
- [e36661b0-687e-465e-b72f-e33245088e38](../../UNASSIGNED/keypatch-probe-spec-keeper-bug-repro-safe-to-cancel--e36661b0/task.md) — keypatch-probe (spec-keeper bug repro, safe to cancel) (cancelled)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2582f548-6493-439c-ba71-7f5cf73650fc](../Spec-Server-export-both-format-markdown-and-format-json--2582f548/task.md) — Spec Server /export (both format=markdown and format=json) silently drops the commits\[\] a… (todo)
- [TRIAGE-LOCK](../TRIAGE-LOCK--25f0eac6/task.md) — TRIAGE-LOCK: backlog-triage mutex (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
