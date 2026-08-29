# AUTH-ROSTER-RECLAIM: operator-side "agent-bus roster remove &lt;id&gt;" escape hatch -- filesystem authority, not an HTTP route, works even when the roster is already full

| Field | Value |
| --- | --- |
| Public id | `b418638c-e9bc-4666-9998-6806f110e357` |
| Key | _(null in the export)_ |
| Epic | [AUTH](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-07T21:26:19.887450+00:00 |
| Updated | 2026-08-14T22:31:56.642933+00:00 |
| Completed | — |

## Proof command

```sh
go build ./... && ./agent-bus roster --help 2>&1 | grep -qi remove
```

## Status note

REPRIORITISED P0->P1, 2026-08-14 (coordinator-directed AUTH audit), WHY STILL WANTED: invite-gating closes the ATTACK (an unauthenticated flooder saturating the roster) but not the EXHAUSTION -- 4096 legitimate, operator-invited enrolments still end at the only remedy today, deleting the whole data directory (confirmed: internal/httpapi/server.go's route table has no removal/leave route wired, and internal/auth/roster.go has zero Remove/Delete/Evict functions -- grep confirms). Reachability collapses to holders of operator-minted single-use invites, so impact is unchanged (total data-directory loss is still the only remedy) and reachability is merely transformed from anonymous to invited. That is a real downgrade in exploitability (P0->P1), not a closure of the underlying problem, hence this stays a live P1, not superseded or deferred.

CONFLICT TO RECORD, subtler than a red test: this task will FALSIFY internal/auth/rosterdos_test.go's central "permanent" claim WHILE LEAVING THE TEST GREEN. Verified directly (2026-08-14; note this file is CURRENTLY STAGED/UNCOMMITTED in the live working tree -- git status shows "A  internal/auth/rosterdos_test.go", not yet part of any commit including ec14bb8, so cite it as current-tree not ec14bb8-tree): the guard's own comment says plainly "It reflects over the auth.Roster INTERFACE... It is NOT a module-wide proof that nothing can free a slot. Reclamation would slip past it in at least two shapes: a method added only to a CONCRETE roster... and an OFFLINE operator tool that edits the data directory without going through this package at all -- which is exactly the shape AUTH-ROSTER-RECLAIM... proposes, in cmd/. Whoever lands that must update this file deliberately; it will not be caught here." A silently-passing test asserting something no longer true is the worse failure than a red one. THIS TASK MUST ship a deliberate update to rosterdos_test.go alongside the reclaim tool -- not merely avoid breaking it, but actively correct its now-false central claim, per its own author's instruction.

Also required, per this task's existing description and restated here for emphasis: a RESERVED (not hand-picked) WAL record-type number if a new one is needed, and explicit confirmation in the implementation and its tests that removal frees the roster SLOT, never the id/suffix (invariant 1 -- ids are never reused, including across restarts).

## Description

Follow-up 2 of 3 from SEC roster-brick finding (1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa). Today the only remedy for a full roster (whether from an attack or legitimate churn) is an operator deleting the WHOLE data directory, destroying every agent id and the message history. This task adds an operator-facing subcommand on the server binary itself (same precedent as the existing `agent-bus invite mint` -- filesystem/process authority, not a network route) that durably removes one or more roster entries via the two-phase write path, so recovery after a restart reflects the removal. This is DELIBERATELY NOT an HTTP route and does NOT depend on INVITE-GATE, MTLS, or any authenticated in-protocol path -- it exists precisely for the case where the roster is already saturated and no in-protocol path can free space. Judge and record whether this needs a new WAL record type (reserve via POST .../reservations, namespace matching this repo's on-disk record-type convention -- see CONTRACTS-ONDISK.md) or can reuse an existing leave/revoke record type once AUTH-4 lands. Cross-reference AUTH-4 (a853261d, POST /v1/leave, P1, in-protocol/authenticated) -- that is a DIFFERENT mechanism (an agent revoking itself, or an authenticated admin action over HTTP) from this one (operator, out-of-band, works when the bus cannot otherwise be reasoned with). Must be documented in CONTRACTS-CLI.md and AGENT_LOG.md notes the id is never reissued (invariant 1) -- removal frees the roster SLOT, not the id/suffix.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [f505fb57-25ab-46e1-a7a1-2ca5787529ab](../Any-roster-reclamation-path-must-ship-a-bound-on-distinc--f505fb57/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa](../SEC-unauthenticated-enrol-permanently-bricks-the-roster--1c4d3dea/task.md) — SEC: unauthenticated enrol permanently bricks the roster -- 4096-cap fails closed forever… (done)
- [AUTH-4](../AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (done)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa](../SEC-unauthenticated-enrol-permanently-bricks-the-roster--1c4d3dea/task.md) — SEC: unauthenticated enrol permanently bricks the roster -- 4096-cap fails closed forever… (done)
- [f505fb57-25ab-46e1-a7a1-2ca5787529ab](../Any-roster-reclamation-path-must-ship-a-bound-on-distinc--f505fb57/task.md) — Any roster-reclamation path must ship a bound on distinct agent names in the SAME change… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
