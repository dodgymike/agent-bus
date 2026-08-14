# RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.NewForwarder has zero production callers

| Field | Value |
| --- | --- |
| Public id | `85ae8b32-3a46-4e85-bdfe-ea29730670fb` |
| Key | RELAY-24-BLOCKER-EGRESS |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T22:01:19.181797+00:00 |
| Updated | 2026-08-14T22:01:19.181797+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'go test -race -run TestLocalMessageForPeerRecipientReachesForwarder|TestForwarderResumeOrderingSurvivesCrash ./cmd/agent-bus/... ./internal/hub/... ./internal/relay/...'
```

## Description

HARD BLOCKER on the three-bus deliverable, peer of RELAY-24's ingress wiring, not a detail of it. RELAY-24 wires INGRESS (a bus receiving a relayed message) end to end. A bus SENDING one to a peer has NO wiring at all -- this was tracked for hours as "the last thing" on ingress before it became clear egress is a separate, equally-sized gap. Verified directly against HEAD, 2026-08-14, independently re-derived by the RELAY-24 reviewer on re-verification.

THREE SPECIFIC GAPS, recorded here so whoever takes this does not rediscover them:

1. `relay.NewForwarder` (forward.go:517) has ZERO production callers -- confirmed by grep across the whole tree excluding _test.go files. The type exists, is tested in isolation, and is never constructed by cmd/agent-bus.

2. `internal/hub` has NO SEAM handing a locally-published message to a forwarder for onward relay. hub.go's publish path enrols, mints, durably writes and delivers to local readers -- there is no hook point where a message whose recipient (or broadcast fan-out) includes an agent behind a peer bus gets handed to anything that would call `Forwarder.Enqueue`.

3. `Forwarder.RecoverMessage` is UNIMPLEMENTABLE TODAY. forward.go:536-543 makes it mandatory whenever an Outbox is configured (resumeRecovery calls it at :1492 to rebuild an envelope for a job recovered from the durable outbox after restart), but `internal/store` has no lookup by message id and `store.Message` has no `OriginMessageID` field -- there is nothing to look up and nothing to look it up BY. Filed separately as RELAY-24-FU-STOREMSGLOOKUP (prerequisite, blocks this task) since it is an internal/store change with its own risk profile, not a sub-clause here.

SCOPE THIS TASK MUST COVER, once the store prerequisite lands: (a) the hub-side seam -- when a locally-published message's recipient set includes an agent whose bus differs from this bus (per the roster/registry), hand it to the Forwarder instead of (or in addition to) local delivery; (b) construct and wire the Forwarder in cmd/agent-bus's composition root, symmetric with RELAY-24's ingress wiring; (c) the THREE-STAGE STARTUP ORDERING already documented on RELAY-24 for the (now removed from RELAY-24's own mandate, see the paired spec-amendment) Resume() call -- restated here since it belongs to THIS task now: peer store replay, THEN Registry/roster restore, THEN `Forwarder.Resume()`; getting replay-vs-roster wrong loses peer config, getting roster-vs-resume wrong either mass-abandons a recovered outbox against an empty roster or races live Enqueue against Resume (a live Enqueue racing Resume can double-queue the same jobID -- the cheapest fix flagged: make Enqueue refuse until the Outbox has been resumed, so the ordering bug fails loudly instead of silently producing a wire duplicate). (d) `OutboxRecordKind`'s absence from the WAL applier map, flagged by the reviewer alongside the RecoverMessage finding -- confirm and close if still open when this task starts.

TESTS: an end-to-end test proving a message published locally for a recipient behind a peer bus reaches `Forwarder.Enqueue`; a crash-injection test for the three-stage startup ordering (kill mid-recovery, assert no false-abandonment and no double-queue); the three-bus federation smoke test (fed-smoke.sh) exercising BOTH directions once RELAY-24-BLOCKER-PEERCERTFLAG also lands, since fed-smoke.sh needs a bindable peer on both sides to prove anything.

Blocks RELAY-25 (10491a01). Blocked by RELAY-24-FU-STOREMSGLOOKUP (the store prerequisite).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [RELAY-24-FU-STOREMSGLOOKUP](../RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md)
- **blocks** [RELAY-25](../RELAY-25--10491a01/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-24-BLOCKER-PEERCERTFLAG](../RELAY-24-BLOCKER-PEERCERTFLAG--0e6b5a49/task.md) — RELAY-24-BLOCKER-PEERCERTFLAG: agent-bus peer add has no flag to bind a peer's inbound cl… (in_progress)
- [RELAY-24-FU-STOREMSGLOOKUP](../RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) — RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMess… (in_progress)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-24-FU-STOREMSGLOOKUP](../RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) — RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMess… (in_progress)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
