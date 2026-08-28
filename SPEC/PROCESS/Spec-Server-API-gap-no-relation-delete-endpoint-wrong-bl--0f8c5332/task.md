# Spec Server API gap: no relation-delete endpoint -- wrong blocks/relates/supersedes/follow_up edges cannot be removed

| Field | Value |
| --- | --- |
| Public id | `0f8c5332-1236-4e22-a249-72119401003f` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T10:14:32.744232+00:00 |
| Updated | 2026-08-22T09:24:23.508383+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/spec-cloud.sh -s /openapi.json | python3 -c "import json,sys; d=json.load(sys.stdin); m=d['paths']['/api/v1/projects/{slug}/tasks/{ident}/relations']; assert 'delete' in m, 'no DELETE method on relations endpoint'; print('DELETE_METHOD_PRESENT')"
```

## Description

P3. Filed against agent-bus (project slug agent-bus) because that is the project whose backlog is affected day to day; the root cause lives in the spec-keeper tool itself, not in agent-bus code -- flagging that choice explicitly in case a spec-server-slug project is preferred instead.

Confirmed against GET /openapi.json: paths["/api/v1/projects/{slug}/tasks/{ident}/relations"] lists only `get` and `post`. Also confirmed against the local blueprint source ~/source/spec-keeper/app/blueprints/tasks.py:360-380 -- no DELETE handler exists for a relation.

Consequence, observed directly in this backlog on 2026-08-15: two tasks, RELAY-24-BLOCKER-EGRESS-ATTEST (3334677e-b0d1-4e2f-addf-04ca28cd16f0) and RELAY-24-BLOCKER-EGRESS-HANDSHAKE (0ab31d26-4a45-420d-930b-69f77346e4dd), carry `blocks` edges onto RELAY-24-BLOCKER-EGRESS (85ae8b32-3a46-4e85-bdfe-ea29730670fb) that are now known to be WRONG (both were split out and re-scoped after filing, and neither actually blocks the egress work) and cannot be removed through the API. The backlog therefore permanently misreports a P0 as blocked on prerequisites it does not have. The corrections had to be recorded as prose status_notes on the affected tasks instead, where no dependency query will ever surface them -- an agent or human trusting `blocks` edges over prose will be misled.

DEFINITION OF DONE (on the spec-keeper side, tracked here as an external dependency): add DELETE /api/v1/projects/{slug}/tasks/{ident}/relations/{relation_id} (or equivalent), documented in AGENTS_API.md, and use it here to remove the two wrong blocks edges named above once it ships.

This is a TOOLING gap, not a code change in agent-bus -- do not touch agent-bus source for this task.
== CORRECTION 2026-08-22 (spec-keeper) -- the core claim above is now FALSE ==

Re-measured against GET /openapi.json (cloud API): paths["/api/v1/projects/{slug}/tasks/{ident}/relations"]
now lists get, post, delete, parameters. The DELETE method exists, with target/kind as required
query parameters and documented 204/404/409/422 responses -- exactly matching the "DEFINITION OF
DONE" shape asked for below ("DELETE /api/v1/projects/{slug}/tasks/{ident}/relations?target=...&kind=..."
returns 204 No Content on success). Do not tell anyone there is no relation-delete endpoint, or
have them work around a limit that no longer exists; AGENTS_API.md should be checked/updated to
document it if it is not already.

NOT closing this task: the DELETE endpoint shipping does not by itself remove the two wrong `blocks`
edges named above (RELAY-24-BLOCKER-EGRESS-ATTEST and RELAY-24-BLOCKER-EGRESS-HANDSHAKE onto
RELAY-24-BLOCKER-EGRESS, 85ae8b32-3a46-4e85-bdfe-ea29730670fb) -- re-checked 2026-08-22 via
GET .../tasks/85ae8b32.../relations and both incoming `blocks` edges (from 3334677e-b0d1-4e2f-addf-04ca28cd16f0
and 0ab31d26-4a45-420d-930b-69f77346e4dd) are still present. That cleanup -- calling the now-available
DELETE against both edges -- is the remaining work on this task; leaving it open rather than closing
it on the API-gap fix alone.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24-BLOCKER-EGRESS](../../RELAY/RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (done)
- [RELAY-24-BLOCKER-EGRESS-ATTEST](../../RELAY/RELAY-24-BLOCKER-EGRESS-ATTEST--3334677e/task.md) — RELAY-24-BLOCKER-EGRESS-ATTEST: no bus can ISSUE an origin attestation for its own agents… (done)
- [RELAY-24-BLOCKER-EGRESS-HANDSHAKE](../../RELAY/RELAY-24-BLOCKER-EGRESS-HANDSHAKE--0ab31d26/task.md) — RELAY-24-BLOCKER-EGRESS-HANDSHAKE: this bus never DIALS a peer, so its relay Registry nev… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ed3537a8-9c5f-489e-8aa8-8d3f61514d5f](../Correct-0f8c5332-the-relation-delete-endpoint-EXISTS--ed3537a8/task.md) — Correct 0f8c5332: the relation-delete endpoint EXISTS (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
