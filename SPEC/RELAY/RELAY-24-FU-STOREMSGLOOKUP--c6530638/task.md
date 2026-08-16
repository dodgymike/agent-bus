# RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMessageID field before egress's RecoverMessage can be implemented

| Field | Value |
| --- | --- |
| Public id | `c6530638-7cca-4404-bc61-88ca6c2d30b9` |
| Key | RELAY-24-FU-STOREMSGLOOKUP |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | store |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T22:00:50.162925+00:00 |
| Updated | 2026-08-15T07:28:27.336278+00:00 |
| Completed | 2026-08-15T07:28:27.336264+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh 'go test -race -run "TestStoreLookupByMessageID|TestMessageOriginMessageIDRoundTrips" ./internal/store/'
```

## Status note

CODE-COMPLETE, GATES PASSED, AWAITING COMMIT -- 2026-08-15. Reviewer: PASS (code). Security: CHANGES-REQUIRED on first pass, two medium BLOCKING findings, both fixed and RE-VERIFIED CLOSED by the same gate (final verdict PASS). Owner feature-runner. Not committed: this agent does not commit; the orchestrator lands it. Complete this task with the commit_sha once it is in main.

SURFACE ADDED (internal/store only): Message.OriginMessageID (correlation key, DURABLE), Message.OriginID(), Message.WithOriginMessageID(), Record.OriginMessageID `json:"origin_message_id,omitempty"`, Store.ByID(), Store.ByOriginMessageID(), Store.DuplicateOriginMessageIDs().

ON-DISK: one new OPTIONAL durable field. RecordVersion DELIBERATELY STAYS AT 2 and NO number was reserved -- RecordVersion's own doc says an added optional field does not move it, and Record decoding is deliberately non-strict about unknown fields. Reviewer specifically ruled on this and agreed. Bumping to 3 would have been actively harmful: Decode does an EXACT version match, so it would DISCARD ALL EXISTING MESSAGE HISTORY on upgrade. Old build reading a new record ignores the field; new build reading an old record gets "", which is correct because a pre-relay bus originated everything it holds.

DOCUMENTATION IS NOT DONE and was OUT OF THIS TASK'S FILE BOUNDARY: CONTRACTS-ONDISK.md needs the new optional record field (and the explicit note that RecordVersion is unchanged and why), and DECISIONS.md wants a dated entry for the no-version-bump decision and for the peer-triggerable duplicate-origin resolution. Both were blocked by the boundary, not skipped by choice. Reviewer made the CONTRACTS-ONDISK.md update a completion condition.

NO CLI/AGENT SURFACE MOVED: this is an internal library primitive with no HTTP route and no agent-facing capability, so invariant 7 requires no agent-busctl subcommand and no AGENT_PROTOCOL.md entry. That obligation lands on RELAY-24-BLOCKER-EGRESS, which is what makes the behaviour reachable.

UNBLOCKS RELAY-24-BLOCKER-EGRESS: relay.Forwarder.RecoverMessage is now implementable. The single-hop case (this bus is the origin) works across a restart with NO new durable state via ByOriginMessageID's documented fallback to ByID.

## Description

PREREQUISITE for RELAY-24-BLOCKER-EGRESS (relay.Forwarder.RecoverMessage), filed separately per the RELAY-24 reviewer's instruction: this is an internal/store change with its own file boundary and risk, and internal/store has been the source of two P0s in this backlog already today -- it should not be a buried sub-clause of the egress task.

TWO GAPS, both verified directly against internal/store/message.go and internal/store/store.go at HEAD, 2026-08-14:

1. NO LOOKUP BY MESSAGE ID. `Store`'s exported methods are `Append`, `Since` (cursor-range, not point lookup), `HasVisibleAfter`, `NonMonotonicPositions`, `PosHead`, `Head`, `Stats`, `pruneLocked` -- none takes a message id and returns one record. `relay.Forwarder.RecoverMessage` (forward.go:1492, called from resumeRecovery at :1485-1492) needs exactly this: given an origin message id recovered from a durable outbox job, fetch that message's current body/recipients/signature to re-attempt delivery after a restart.

2. NO `OriginMessageID` ON `store.Message`. The struct (message.go:135-247) carries Seq, Pos, ID, Sender, Broadcast, Recipients, BusPath, SentAt, Body, IdempotencyKey, TimestampUnixMilli, Signature -- no field records which relay job / origin envelope a stored message corresponds to. `relay.OutboxRecord.OriginMessageID` (outbox.go:401-407) already exists on the RELAY side; nothing on the STORE side echoes it back, so even with a lookup method there is no guaranteed way to prove the fetched message is the SAME envelope the outbox job named, as opposed to a same-id message that was pruned and whose id slot was never reused (invariant 1 forbids reuse, but a pruned message's absence must still be distinguishable from a wrong one being returned).

Reviewer's own re-verification (RELAY-24, 2026-08-14): "forward.go:536-543 makes RecoverMessage mandatory whenever Outbox is set, and internal/store carries NO OriginMessageID field and no lookup-by-message-id, so Resume cannot rebuild an envelope." Also note OutboxRecordKind's absence from the WAL applier map, flagged in the same finding -- confirm whether that is a third gap or already covered elsewhere when this task is scoped.

SCOPE THIS TASK MUST DECIDE: (a) the lookup method's shape -- point lookup by full message id is the minimum; whether it needs to survive pruning (a relay job durably queued longer than the local retention window) is a real design question, not a detail -- if the message was pruned, RecoverMessage has nothing to recover and the job should settle abandoned, loudly, not error silently; (b) whether OriginMessageID is populated only for messages that arrived via relay ingest (RELAY-21's AcceptRelay path) or is a general field; (c) on-disk format impact -- a new Message field may need a WAL record version bump; reserve one via the migration/wire-version reservation namespace, never hand-pick it.

Blocks RELAY-24-BLOCKER-EGRESS.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (done)
- [RELAY-24-BLOCKER-EGRESS-ATTEST](../RELAY-24-BLOCKER-EGRESS-ATTEST--3334677e/task.md) — RELAY-24-BLOCKER-EGRESS-ATTEST: no bus can ISSUE an origin attestation for its own agents… (done)
- [RELAY-24-FU-STOREMSGLOOKUP-BYIDGUARD](../RELAY-24-FU-STOREMSGLOOKUP-BYIDGUARD--4d5c1267/task.md) — RELAY-24-FU-STOREMSGLOOKUP-BYIDGUARD: byID's insert/prune are unconditional on local-id u… (todo)
- [RELAY-24-FU-STOREMSGLOOKUP-DOCS](../RELAY-24-FU-STOREMSGLOOKUP-DOCS--e02aa062/task.md) — RELAY-24-FU-STOREMSGLOOKUP-DOCS: CONTRACTS-ONDISK.md + DECISIONS.md + AGENT_LOG.md entrie… (done)
- [RELAY-24-FU-STOREMSGLOOKUP-GUARDGAP](../RELAY-24-FU-STOREMSGLOOKUP-GUARDGAP--0e045086/task.md) — RELAY-24-FU-STOREMSGLOOKUP-GUARDGAP: AST guard bans store point-lookups by selector name… (todo)
- [RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY](../RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY--6e13a7d9/task.md) — RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY: store.copyMessage does not deep-copy Signature -- ali… (todo)
- [RELAY-24-FU-STOREMSGLOOKUP-THROTTLE](../RELAY-24-FU-STOREMSGLOOKUP-THROTTLE--cc7a463e/task.md) — RELAY-24-FU-STOREMSGLOOKUP-THROTTLE: duplicate-origin log throttle is process-wide and bu… (todo)
- [RELAY-48](../RELAY-48--9887b0eb/task.md) — RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
