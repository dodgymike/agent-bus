# TUI-6: "and other people" — multiple humans as distinct enrolled identities, DMs between them

| Field | Value |
| --- | --- |
| Public id | `cb4e3fd7-07ee-4d18-9665-5a220b944d30` |
| Key | TUI-6 |
| Epic | [TUI](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | CLI |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:00:49.868540+00:00 |
| Updated | 2026-08-15T08:00:49.868540+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestTUIMultiHumanIdentities' ./client/... ./cmd/... && echo TUI6_MULTIHUMAN_OK
```

## Description

The "and other people" half of the request: more than one HUMAN on the bus, each a distinct principal, able to
message each other and be messaged by agents. BLOCKED ON TUI-1 and TUI-3.

## Clear up the ADMIN exclusion before anyone reads it as a blocker

ADMIN's "EXPLICITLY NOT COVERED" list names "multi-user auth". That means MULTIPLE USERS OF ONE CONSOLE --
a role/permission tier inside the operator console. It is NOT the same thing as several humans each holding
their OWN enrolled bus identity, which needs no new authority at all: the bus already namespaces every
principal as `<bus-id>.<agent-id>` (invariant 2) and already has exactly one way in (redeem an invite,
invariant 3). TUI-1 should have recorded this distinction; if it did not, get it recorded before starting.

**So the cheap and probably correct answer is that a human IS an agent id.** The bus does not need to learn
what a person is. Verify that before designing anything richer -- if it holds, most of this task is naming,
presentation and invite logistics rather than protocol work, and it must NOT grow a "human" principal type,
a role tier, or a new record kind.

## Requirements

- NO new privilege tier inside the bus. Explicitly out of scope for the whole epic.
- Each human enrols by redeeming their own invite (invariant 3). Multiple humans means multiple invites, which
  is exactly the pooled-minting pain the `INVMINT` epic addresses -- `relates`, do not duplicate. Invite files
  are `0600` or the CLI refuses them.
- Distinguishing humans from agents in the UI, if done at all, is a LOCAL PRESENTATION concern (a local
  nickname map), not a wire field and not a durable record. Anything else needs a `DECISIONS.md` entry and
  probably a protocol version.
- Roster presentation must not imply an authority that does not exist.
- Privacy: do not build an enumeration oracle. LIVE-7 already owns "liveness subscription authorization,
  privacy and anti-enumeration" -- read it and stay consistent rather than inventing a second answer.

## Provability

Identity handling and id qualification are testable; the social layer is not. Proof targets that multiple
distinct enrolled identities are handled without collapsing or truncating ids (invariant 2), NOT that a chat
feels good.

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
- **relates to** [INVMINT-7](../../INVMINT/INVMINT-7--174c7ba9/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-6](../../INVMINT/INVMINT-6--cedb8d6f/task.md) — INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… (todo)
- [LIVE-7](../../LIVE/LIVE-7--09bc72d0/task.md) — LIVE-7: Liveness subscription authorization, privacy and anti-enumeration (todo)
- [TUI-1](../TUI-1--3ea68265/task.md) — TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or com… (todo)
- [TUI-3](../TUI-3--140aadf7/task.md) — TUI-3: GUARD — the TUI sits on the client package and cannot bypass it, and agent-busctl'… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [TUI-1](../TUI-1--3ea68265/task.md) — TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or com… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
