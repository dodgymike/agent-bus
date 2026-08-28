# ACK-15: POST /v1/ack has no CLI subcommand -- until it does, no row can ever reach delivered

| Field | Value |
| --- | --- |
| Public id | `a63b133d-f8d1-461f-bec1-1261bdd6414b` |
| Key | ACK-15 |
| Epic | [ACK](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T09:45:59.228960+00:00 |
| Updated | 2026-08-21T14:17:16.454127+00:00 |
| Completed | 2026-08-21T14:17:16.454109+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh "go test -race -run 'TestAckRecipientCLI' ./cmd/agent-busctl"
```

## Description

FILED 2026-08-21 by main, on ACK-9's recommendation and correcting an earlier misattribution.

# What is missing

`POST /v1/ack` -- the RECIPIENT delivery-acknowledgement route landed by ACK-6 -- ships with NO CLI
subcommand. Invariant 7 says every capability ships with its subcommand and its AGENT_PROTOCOL.md
entry in the SAME task; a capability an agent cannot reach through the compiled CLI is half a task.

# Who owns it -- corrected

ACK-CONTRACT.md section 13.4 assigns this to ACK-9, NOT ACK-6. I had previously recorded it against
ACK-6. ACK-9 did not build it because its brief scoped it to the SENDER side and the recipient wire
shape was still changing during the task (ACK-6's tree briefly stopped compiling). That was the right
call at the time; this task is the consequence.

# WHY IT MATTERS MORE THAN A MISSING CONVENIENCE

Until this exists, NOTHING CAN MOVE A ROW TO `delivered`. So `agent-busctl ack-status` -- which
shipped and works -- can in practice only ever report `accepted` or `in_flight`. The sender-visible
half of the epic is live but has nothing interesting to say. This is the task that makes the ACK
epic actually usable end to end.

# HARD DEPENDENCY -- do not start before it

Task 836c9ff8: internal/signing has NO canonical-ACK-bytes function, which ACK-CONTRACT.md section
6.3 mandates. `relay.ValidateAckAttestation` requires a 64-byte signature on every recipient-sourced
outcome. Writing this CLI first would mean signing SOMETHING and freezing an unspecified encoding
onto the wire, permanently. ACK-6 made this argument and it is correct.

Sequence: 836c9ff8 (canonical bytes) -> this task.

# Scope
  - `agent-busctl ack <correlation-key> --outcome <delivered|refused|undeliverable> [--class <c>]`
    or whatever shape section 13.4 actually specifies -- READ IT, do not infer from this description
  - signs the canonical ACK bytes from 836c9ff8
  - --json, stable exit codes, never an interactive prompt (invariant 7's three audiences)
  - AGENT_PROTOCOL.md + CONTRACTS-CLI.md entries in the SAME task

# Constraints inherited
  - the closed 12-class NACK enum, NO free text anywhere (invariant 6: a recipient-supplied reason
    string is a body by another name)
  - inbox delivery is NOT receipt -- ACK-1 ruled an EXPLICIT application ACK is required, because
    the bus does not verify signatures and auto-ACK would have it assert on the recipient's behalf a
    fact only the recipient can establish
  - section 13.3's uniform answer: malformed, swept, never-existed and someone-else's must be
    byte-identical. ACK-9 enforces this on the read side; do not undo it on the write side
  - NO NEW DISCONNECT (invariant 10; ACK-1 answered both mandated questions on the record)

# Acceptance
  - a recipient can move a row to `delivered` through the compiled CLI, and `ack-status` then reports
    it -- prove it LIVE against a running bus, not only in tests
  - every guard mutation-proven RED; three guards in this project were written to catch a defect and
    could not fire, and mutation found all three where review found none

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-1](../ACK-1--e0ac42e1/task.md) — ACK-1: Define end-to-end ACK/NACK delivery contract and terminal state machine (done)
- [ACK-6](../ACK-6--d3c50d33/task.md) — ACK-6: Recipient delivery acknowledgement boundary (done)
- [ACK-6-FU-CLI](../ACK-6-FU-CLI--836c9ff8/task.md) — ACK-6-FU-CLI: agent-busctl ack, and the canonical ACK bytes it must sign (done)
- [ACK-9](../ACK-9--08f9987f/task.md) — ACK-9: Sender CLI/API acknowledgement status and observability (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](../../UNASSIGNED/Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) — Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
