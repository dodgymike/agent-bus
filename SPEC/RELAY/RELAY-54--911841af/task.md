# RELAY-54: an abandoned outbox job is invisible to every subcommand -- the drain a rollout needs cannot be verified

| Field | Value |
| --- | --- |
| Public id | `911841af-83d7-445f-bf46-9097eeb0661d` |
| Key | RELAY-54 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T10:09:02.870792+00:00 |
| Updated | 2026-08-23T09:11:28.084930+00:00 |
| Completed | 2026-08-23T09:11:28.084881+00:00 |

## Proof command

```sh
go test -race ./cmd/agent-bus ./internal/relay
```

## Status note

Dispatched 2026-08-21 pass2 by RELAY epic owner (feature-runner/opus). Operator granted option (ii): main.go wiring proven in OVERLAY ONLY, patch handed back; cmd/agent-bus/main.go NOT to be touched. Record carries TWO false premises being corrected first: (1) 'origin logs nothing when middle bus refuses' misquotes RELAY-51 - A logs 3 lines + durable abandoned record when B refuses A; the silent case is B's ONWARD hop, which is STRUCTURAL (zero production .PeerAck emitters, ACK-5 owns it) and scoped OUT; (2) forward.go:1125 is the shutdown queue drain, not an abandonment count.

## Description

FILED 2026-08-21 by main, from the RELAY-51 rollout rehearsal.

# The gap

When a relay forward is permanently refused, the outbox settles the job and writes a durable record
with "state":"abandoned". That record lives in the WAL. NO SUBCOMMAND SURFACES IT. `agent-bus log`
reads the AUDIT file, which is a different artefact.

So an operator cannot ask "is anything stuck or lost?" and cannot answer it from the CLI at all.

# Why it is not a convenience

RELAY-51 REJECTED the drain-and-restart rollout order specifically because of this. A drain must be
VERIFIED before you restart, and the only instrument that would verify it does not exist. An
unverifiable drain is an assumption -- and RELAY-51 exists because of an assumption about deploy
safety.

Worse, RELAY-51 also found that in a three-bus chain A -> B -> C, when the middle bus refuses, THE
ORIGIN LOGS NOTHING. Only the transit bus logs the abandonment. So the bus that owes the delivery has
no record, and the operator has no view. Both halves of the observability are missing at once.

# Scope
  - an operator-facing view of outbox jobs and their terminal states, reachable through the compiled
    CLI (invariant 7 -- and note operator commands belong on `agent-bus`, the server binary, NOT on
    `agent-busctl` which is the AGENT's client)
  - at minimum: is anything pending, is anything abandoned, and for which peer
  - AGENT_PROTOCOL.md / CONTRACTS-*.md entries in the SAME task

# Related
  RELAY-51 -- the rollout gate that could not use drain-and-restart because of this.
  RELAY-48 (7c8bec9) made onward hops resumable across restart; this is about SEEING the ones that
  were not.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [8cfd52e7-6cbd-4d29-b34d-c3dee87e73e5](../agent-bus-peer-list-mints-wal-mac.key-as-a-side-effect-o--8cfd52e7/task.md)
- **relates to** [b5089ddf-5a5a-41e0-8278-036f6a195e2a](../../AUTH/agent-bus-operator-list-mints-wal-mac.key-as-a-side-effe--b5089ddf/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-5](../../ACK/ACK-5--5991ee1a/task.md) — ACK-5: Multi-hop relay ACK/NACK propagation and correlation (done)
- [RELAY-48](../RELAY-48--9887b0eb/task.md) — RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at… (done)
- [RELAY-51](../RELAY-51--0135d297/task.md) — RELAY-51: RELAY-23 rollout -- a PARTIAL deploy of the wire-version field abandons message… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [8cfd52e7-6cbd-4d29-b34d-c3dee87e73e5](../agent-bus-peer-list-mints-wal-mac.key-as-a-side-effect-o--8cfd52e7/task.md) — agent-bus peer list mints wal-mac.key as a side effect of a read-only command (todo)
- [ACK-RETRY-ENGINE](../../ACK/ACK-RETRY-ENGINE--81ce7331/task.md) — ACK-RETRY-ENGINE: sender-side retry of an unacknowledged relayed message, with a defined… (todo)
- [TRIAGE-LOCK](../../PROCESS/TRIAGE-LOCK--25f0eac6/task.md) — TRIAGE-LOCK: backlog-triage mutex (in_progress)
- [a9bcdc54-fe1c-4497-9294-13efe2fca8fc](../agent-bus-outbox-bound-the-replay-tally-maps-which-are-k--a9bcdc54/task.md) — agent-bus outbox: bound the replay tally maps, which are keyed off attacker-influenced fi… (todo)
- [b5089ddf-5a5a-41e0-8278-036f6a195e2a](../../AUTH/agent-bus-operator-list-mints-wal-mac.key-as-a-side-effe--b5089ddf/task.md) — agent-bus operator list mints wal-mac.key as a side effect of a read-only command (done)
- [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](../../UNASSIGNED/Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) — Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
