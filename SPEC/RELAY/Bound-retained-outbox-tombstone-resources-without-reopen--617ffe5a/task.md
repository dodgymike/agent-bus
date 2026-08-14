# Bound retained outbox tombstone resources without reopening replay resurrection

| Field | Value |
| --- | --- |
| Public id | `617ffe5a-db42-4aeb-89bb-d9b0889f6c19` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | relay |
| Section | in_progress |
| Tags | — |
| Created | 2026-08-08T23:48:45.734709+00:00 |
| Updated | 2026-08-09T14:28:27.502376+00:00 |
| Completed | 2026-08-09T14:28:27.502361+00:00 |

## Proof command

```sh
go test -race -count=1 -run ^TestOutboxCheckpointedTombstoneBoundAcceptance ./internal/relay
```

## Status note

V8 gate FAIL: capture canonical body/version for each omitted pending record; cleanup requires current still-expired and exact version match. Add settlement-during-publication/cleanup-boundary restart parity test. Keep in_progress; RELAY-19 remains blocked.

## Description

RELAY-15-FU-CAPACITY-FAIRNESS correctly stops terminal tombstones consuming LIVE pending admission, but terminal count/bytes and historical outbox replay remain unbounded over the 24h anti-resurrection window. Once RELAY-19 wires forwarding, authenticated relay traffic can create terminal records at traffic rate and peer-supplied origin IDs influence pressure.

Prerequisite COMPLETE: WAL-owned multi-applier authenticated checkpoint foundation landed at ddf42b78ecb1a8bc8536fc506481f98834e30e92 (format 7). Implement this task as an Outbox checkpoint participant using that API; do not add a relay-only side store, new WAL compaction substrate or Forwarder wiring. The checkpoint must serialize/restore the complete canonical live pending set and every unretired terminal tombstone at the shared WAL high-water, preserve monotonic anti-resurrection through restart/fallback, and let WAL replay only the bounded post-checkpoint tail.

Deliver finite, explicit resource accounting for serving map plus participant snapshot and tail: retained count and encoded-byte bounds under peer-influenced traffic; durable/fair admission/backpressure that reserves enough capacity before acceptance so Settle is never refused/evicted; pre-upgrade excess remains safely recoverable and loudly reported as debt but does not admit new over-limit work. A retention-window rate limit is allowed only if explicit, documented and fair—never represented as free capacity. Never discard a correctness-critical terminal state merely to satisfy a cap.

Acceptance: checkpoint/restart/fallback cannot turn terminal back to pending; stale pending after restored terminal is refused; concurrent admission cannot oversubscribe count/bytes; checkpoint snapshot plus replay tail are bounded; live/replay accounting agrees; byte accounting uses canonical encoded records; legacy excess does not cause startup failure; crash at enqueue/settle/checkpoint boundaries recovers an acknowledged prefix with no resurrection. Read invariants 1,2,4,5,6,10. Scope: internal/relay/outbox.go, internal/relay/outbox_checkpoint.go (or equivalent), internal/relay/outbox*_test.go, and CONTRACTS-ONDISK.md/DECISIONS.md. Do not change Forwarder/RELAY-19 wiring.

V7 gate remediation (2026-08-09): Pending() is a live forwarding surface and MUST exclude a job immediately once it is expired/marked expired, even before a checkpoint publishes; otherwise RELAY-19 can retry beyond RetryHorizon and peer idempotency retention. Checkpoint reclamation is generation-scoped: Snapshot must capture the exact omitted/reclaim ID set associated with the candidate snapshot/generation; successful publication may reclaim/rebase ONLY that captured set, and failed/ambiguous publication reclaims NONE. Concurrent sweeps after Snapshot may create later omission sets but must remain charged/live until their own checkpoint succeeds. Add deterministic blocked-publication concurrency plus restart proof: cause checkpoint publication to block after Snapshot, expire/mark a record concurrently, then prove success/failure accounting and restart state agree, no ID included by the published snapshot was prematurely reclaimed, no terminal/pending resurrection, and count/byte/per-peer admission remains bounded.

V8 gate remediation (2026-08-09): generation-scoped omission IDs alone are insufficient. An omitted pending job can Settle while checkpoint publication is blocked, append its terminal record to the new tail, and update the serving record before cleanup. Snapshot must capture an immutable canonical body/version (or equally exact identity) for each omitted pending record. Success cleanup may reclaim only when the current job is still expired AND its canonical body/version exactly matches the captured omitted pending version; if it has transitioned to terminal or otherwise changed, retain it and its exact accounting. Failed/ambiguous publication still reclaims none. Add deterministic test interleaving settlement at the publication/cleanup boundary, then restart: the terminal tail state survives, no cleanup deletion/resurrection occurs, and live/replay count/byte/per-peer accounting agrees.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [a1cbef29-400a-4a1e-9638-cc14d38a7ebf](../../UNASSIGNED/WAL-foundation-authenticated-multi-applier-checkpoints-o--a1cbef29/task.md)
- **blocks** [ACK-2](../../ACK/ACK-2--9564f953/task.md)
- **blocks** [RELAY-19](../RELAY-19--24e0bd11/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-15-FU-CAPACITY-FAIRNESS](../RELAY-15-FU-CAPACITY-FAIRNESS--4fd2d8d7/task.md) — RELAY-15-FU-CAPACITY-FAIRNESS: Outbox capacity is a 24h throughput ceiling and is not per… (done)
- [RELAY-19](../RELAY-19--24e0bd11/task.md) — RELAY-19: Forwarder writes and settles outbox records (part 2 of 2) (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [8fb219ca-1236-4058-9020-afd52a7e93f3](../../UNASSIGNED/WAL-checkpoint-follow-up-exhaustive-in-operation-crash-p--8fb219ca/task.md) — WAL checkpoint follow-up: exhaustive in-operation crash-path evidence (todo)
- [ACK-2](../../ACK/ACK-2--9564f953/task.md) — ACK-2: Durable local send acceptance and ACK/NACK lifecycle record (todo)
- [RELAY-15-FU-CAPACITY-FAIRNESS](../RELAY-15-FU-CAPACITY-FAIRNESS--4fd2d8d7/task.md) — RELAY-15-FU-CAPACITY-FAIRNESS: Outbox capacity is a 24h throughput ceiling and is not per… (done)
- [a1cbef29-400a-4a1e-9638-cc14d38a7ebf](../../UNASSIGNED/WAL-foundation-authenticated-multi-applier-checkpoints-o--a1cbef29/task.md) — WAL foundation: authenticated multi-applier checkpoints over shared bus.wal (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
