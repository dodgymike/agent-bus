# TUI-4: the read-only MONITOR view — renders LIVE/ADMIN data, re-specifies none of it

| Field | Value |
| --- | --- |
| Public id | `11898d9b-5baf-4a24-9d33-d22f2d4a8961` |
| Key | TUI-4 |
| Epic | [TUI](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | CLI |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:00:49.432358+00:00 |
| Updated | 2026-08-15T08:00:49.432358+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestTUIMonitorModel' ./client/... ./cmd/... && echo TUI4_MODEL_OK
```

## Description

The "monitor" half of the request: a terminal view of roster, liveness, status and flow. BLOCKED ON TUI-1 and
TUI-3.

## DEPEND, DO NOT RE-SPECIFY

"Monitor" is mostly already specified elsewhere and this task must consume it, not restate it:
- **LIVE** (15 open) owns the liveness contract, the status state machine, heartbeats, and authorised status
  subscriptions (LIVE-1, LIVE-6, LIVE-8).
- **ADMIN-8** owns `GET /v1/status` -- "authenticated, in-process counters, exhaustive field-set pin".
- **ADMIN-2** owns `client.Info/Health/Discovery` + `agent-busctl status [--json]`.
- **ADMIN-5** owns the roster + live flow view from the console's OWN long-poll, metadata only.

If a datum the view needs does not exist, THE TASK FOR IT BELONGS IN LIVE OR ADMIN, not here. File it there and
add a `blocks` edge. Inventing a bus route for the UI's convenience is explicitly out of scope for this epic.

## Invariant 6 — state which side of the line this view sits on

The append-only log records METADATA AND ROUTING ONLY, never message bodies. A monitor view built from the log
or the audit reader sees metadata; it MUST NOT surface other principals' message bodies. (Contrast TUI-5,
where the human reads bodies addressed to ITSELF over its own `/v1/wait` -- which is ordinary and allowed.)
Say in the code which source each pane draws from. ADMIN-5 set the safe precedent.

## Be honest about what is provable

**A proof asserting that a terminal renders correctly is not a real proof**, and one must not be stored here.
Separate the VIEW MODEL from the rendering: the model is a pure function from bus state to what should be
displayed, and THAT is unit-testable -- ordering, truncation, staleness thresholds, empty-roster and
one-hung-bus behaviour. The proof targets the model.

**Acceptance for the visual layer is HUMAN JUDGEMENT, stated plainly here so nobody manufactures a fake proof
for it.** Record the human check in `AGENT_LOG.md` (what was run, what was seen); do not encode "it looked
right" as a passing command.

Carry forward ADMIN-4's hard-won requirement if multiple buses are shown: ONE HUNG BUS MUST NOT STALL THE
VIEW.

## PROOF STATUS -- READ BEFORE COMPLETING

The test named in `proof_cmd` DOES NOT EXIST YET; writing it is part of this task's deliverable, not a
pre-existing artefact. `scripts/proof-check.sh` reports VACUOUS until it is written, and THAT VACUOUS IS THE
RED OBSERVATION -- record it before starting. If the design lands under a different name, have spec-keeper
UPDATE this `proof_cmd`; never complete behind a proof naming a test nobody wrote (88 broken proofs in this
backlog, 2 tasks closed on targets that never existed). Do NOT put `<angle brackets>` in a proof_cmd --
proof-check.sh classifies them as an unfilled template and REFUSES TO RUN IT (caught on INVMINT-6).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [TUI-1](../TUI-1--3ea68265/task.md)
- **blocked by** [TUI-3](../TUI-3--140aadf7/task.md)
- **relates to** [ADMIN-8](../../ADMIN/ADMIN-8--7f550309/task.md)
- **relates to** [LIVE-6](../../LIVE/LIVE-6--5825cf57/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ADMIN-2](../../ADMIN/ADMIN-2--786e0de1/task.md) — ADMIN-2: client.Info/Health/Discovery + \`agent-busctl status \[--json\]\`, shipped together… (todo)
- [ADMIN-4](../../ADMIN/ADMIN-4--e12b4149/task.md) — ADMIN-4: N buses from a config file, polled concurrently -- one hung bus must not stall t… (todo)
- [ADMIN-5](../../ADMIN/ADMIN-5--fc0b4a88/task.md) — ADMIN-5: roster + live flow view from the console's OWN long-poll (/v1/wait) -- metadata… (todo)
- [ADMIN-8](../../ADMIN/ADMIN-8--7f550309/task.md) — ADMIN-8: GET /v1/status -- authenticated, in-process counters, exhaustive field-set pin,… (todo)
- [INVMINT-6](../../INVMINT/INVMINT-6--cedb8d6f/task.md) — INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… (todo)
- [LIVE-1](../../LIVE/LIVE-1--354e378c/task.md) — LIVE-1: Liveness contract and status state machine (todo)
- [LIVE-6](../../LIVE/LIVE-6--5825cf57/task.md) — LIVE-6: Authorized status subscription HTTP API and CLI watch (todo)
- [LIVE-8](../../LIVE/LIVE-8--742dd0ec/task.md) — LIVE-8: Durable status-change notifications, cursors and idempotency (todo)
- [TUI-1](../TUI-1--3ea68265/task.md) — TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or com… (todo)
- [TUI-3](../TUI-3--140aadf7/task.md) — TUI-3: GUARD — the TUI sits on the client package and cannot bypass it, and agent-busctl'… (todo)
- [TUI-5](../TUI-5--b2a44ce9/task.md) — TUI-5: the human as a bus PARTICIPANT — read and send messages as a person (message bodie… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [TUI-1](../TUI-1--3ea68265/task.md) — TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or com… (todo)
- [TUI-3](../TUI-3--140aadf7/task.md) — TUI-3: GUARD — the TUI sits on the client package and cannot bypass it, and agent-busctl'… (todo)
- [TUI-5](../TUI-5--b2a44ce9/task.md) — TUI-5: the human as a bus PARTICIPANT — read and send messages as a person (message bodie… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
