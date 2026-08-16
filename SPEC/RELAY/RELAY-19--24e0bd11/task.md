# RELAY-19: Forwarder writes and settles outbox records (part 2 of 2)

| Field | Value |
| --- | --- |
| Public id | `24e0bd11-7c59-4097-a20e-fb12befc068f` |
| Key | RELAY-19 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | relay |
| Section | backlog |
| Tags | vacuous-today |
| Created | 2026-08-08T15:56:45.377030+00:00 |
| Updated | 2026-08-14T12:32:16.755205+00:00 |
| Completed | 2026-08-14T12:32:16.755188+00:00 |

## Proof command

```sh
go test -race -run TestForwarderSettlesOutboxRecords ./internal/relay
```

## Status note

integrator committing; spec-keeper leaving in_progress per RELAY-19 file-followups request

## Description

FEDERATION phase, wave 3. Deps: RELAY-8 (Registry.PeerBaseURL accessor), RELAY-15 (outbox
record + replay, part 1).

Part 2 of 2: the Forwarder itself now writes and settles durable outbox records (RELAY-15 built
the record/replay machinery; this task wires the Forwarder to use it on the write and settle
paths).

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-15](../RELAY-15--663be37c/task.md) — RELAY-15: Durable outbox record + replay (part 1 of 2) (done)
- [RELAY-8](../RELAY-8--206a89d1/task.md) — RELAY-8: Registry.PeerBaseURL accessor + concurrency contract (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4dfd01e3-eab2-4346-a0ab-4c730f505e93](../RELAY-19-residual-bounded-resolve-then-POST-TOCTOU-on-pe--4dfd01e3/task.md) — RELAY-19 residual: bounded resolve-then-POST TOCTOU on peer revocation (todo)
- [55fe0e43-3cd1-40a6-8f18-73f0fbea69d3](../CONTRACTS-ONDISK.md-1294-1297-retire-the-composition-wit--55fe0e43/task.md) — CONTRACTS-ONDISK.md:1294-1297: retire the "composition with the forwarder remains RELAY-1… (todo)
- [617ffe5a-db42-4aeb-89bb-d9b0889f6c19](../Bound-retained-outbox-tombstone-resources-without-reopen--617ffe5a/task.md) — Bound retained outbox tombstone resources without reopening replay resurrection (done)
- [8fb219ca-1236-4058-9020-afd52a7e93f3](../../UNASSIGNED/WAL-checkpoint-follow-up-exhaustive-in-operation-crash-p--8fb219ca/task.md) — WAL checkpoint follow-up: exhaustive in-operation crash-path evidence (todo)
- [ACK-5](../../ACK/ACK-5--5991ee1a/task.md) — ACK-5: Multi-hop relay ACK/NACK propagation and correlation (todo)
- [LIVE-11](../../LIVE/LIVE-11--3662e698/task.md) — LIVE-11: Federation ownership, multi-hop liveness and partition semantics (todo)
- [RELAY-15](../RELAY-15--663be37c/task.md) — RELAY-15: Durable outbox record + replay (part 1 of 2) (done)
- [RELAY-15-FU-CAPACITY-FAIRNESS](../RELAY-15-FU-CAPACITY-FAIRNESS--4fd2d8d7/task.md) — RELAY-15-FU-CAPACITY-FAIRNESS: Outbox capacity is a 24h throughput ceiling and is not per… (done)
- [RELAY-15-FU-SWEEP-TOMBSTONE](../RELAY-15-FU-SWEEP-TOMBSTONE--da1ba9b7/task.md) — RELAY-15-FU-SWEEP-TOMBSTONE: Horizon-swept outbox jobs leave no durable abandonment record (todo)
- [RELAY-16-FU-SEQUENCING](../RELAY-16-FU-SEQUENCING--83ef0b67/task.md) — RELAY-16-FU-SEQUENCING: RemoteRouter must not be wired before the durable outbox exists (todo)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-42](../RELAY-42--61c00e9f/task.md) — RELAY-42: Registry.PeerBaseURL and Route compare busID exactly while the map key is case-… (cancelled)
- [RELAY-42](../RELAY-42--e13e6b0d/task.md) — RELAY-42: Registry.PeerBaseURL and Route compare busID exactly while the map key is case-… (todo)
- [a1cbef29-400a-4a1e-9638-cc14d38a7ebf](../../UNASSIGNED/WAL-foundation-authenticated-multi-applier-checkpoints-o--a1cbef29/task.md) — WAL foundation: authenticated multi-applier checkpoints over shared bus.wal (done)
- [c6207571-8994-42c4-8f88-91292a259955](../RELAY-19-residual-outbox-record-covers-body-hash-size-bu--c6207571/task.md) — RELAY-19 residual: outbox record covers body hash+size but not sender/recipients/origin b… (todo)
- [d7a0e7c4-6ea8-4fa7-8db5-c8044dce3a8d](../../INVITE/TestInviteNotDurableIsRefused-is-a-time-bomb-hardcoded-2--d7a0e7c4/task.md) — TestInviteNotDurableIsRefused is a time-bomb: hardcoded 2026-08-07 fixture date now falls… (done)
- [eb47af9d-5342-4944-87e8-94f5e2399e8f](../RELAY-19-reviewer-P2s-deliberately-not-applied-preserved--eb47af9d/task.md) — RELAY-19 reviewer P2s deliberately not applied (preserved an md5-pinned PASS) -- apply th… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
