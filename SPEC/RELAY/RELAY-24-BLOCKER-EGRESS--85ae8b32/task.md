# RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.NewForwarder has zero production callers

| Field | Value |
| --- | --- |
| Public id | `85ae8b32-3a46-4e85-bdfe-ea29730670fb` |
| Key | RELAY-24-BLOCKER-EGRESS |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T22:01:19.181797+00:00 |
| Updated | 2026-08-15T13:47:14.817481+00:00 |
| Completed | 2026-08-15T13:47:14.817463+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh 'go test -race -run "TestLocalMessageForPeerRecipientReachesForwarder|TestForwarderResumeOrderingSurvivesCrash" ./cmd/agent-bus/... ./internal/hub/... ./internal/relay/...'
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

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-24-BLOCKER-PEERCERTFLAG](../RELAY-24-BLOCKER-PEERCERTFLAG--0e6b5a49/task.md) — RELAY-24-BLOCKER-PEERCERTFLAG: agent-bus peer add has no flag to bind a peer's inbound cl… (done)
- [RELAY-24-FU-STOREMSGLOOKUP](../RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) — RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMess… (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0f8c5332-1236-4e22-a249-72119401003f](../../PROCESS/Spec-Server-API-gap-no-relation-delete-endpoint-wrong-bl--0f8c5332/task.md) — Spec Server API gap: no relation-delete endpoint -- wrong blocks/relates/supersedes/follo… (todo)
- [0fb4d032-efff-4815-ac2b-4b8f1682ba08](../../PROCESS/Four-proof_cmds-are-UNVERIFIABLE-BY-CONSTRUCTION-ACK-3-A--0fb4d032/task.md) — Four proof_cmds are UNVERIFIABLE BY CONSTRUCTION (ACK-3, ACK-4, LIVE-3, AGENTIF-10) -- un… (todo)
- [6f82180f-8f57-473d-bd87-30f6d9d9695d](../PROTOCOL.md-599-cites-a-deleted-test-TestHandshakeHandle--6f82180f/task.md) — PROTOCOL.md:599 cites a deleted test (TestHandshakeHandlerIsNotWiredIntoAnyMux) as a live… (todo)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-24-BLOCKER-EGRESS-ATTEST](../RELAY-24-BLOCKER-EGRESS-ATTEST--3334677e/task.md) — RELAY-24-BLOCKER-EGRESS-ATTEST: no bus can ISSUE an origin attestation for its own agents… (done)
- [RELAY-24-BLOCKER-EGRESS-FU-BROADCASTFSYNC](../RELAY-24-BLOCKER-EGRESS-FU-BROADCASTFSYNC--2d3224f0/task.md) — RELAY-24-BLOCKER-EGRESS-FU-BROADCASTFSYNC: Broadcast egress fsync amplification -- MUST b… (todo)
- [RELAY-24-BLOCKER-EGRESS-FU-CHECKPOINTWIRING](../RELAY-24-BLOCKER-EGRESS-FU-CHECKPOINTWIRING--d92abfe9/task.md) — RELAY-24-BLOCKER-EGRESS-FU-CHECKPOINTWIRING: Outbox.Checkpoint has no production caller -… (todo)
- [RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE](../RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE--3e96dae2/task.md) — RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE: three-bus federation smoke test (fed-smoke.sh, both… (todo)
- [RELAY-24-BLOCKER-EGRESS-FU-LOCKORDERNOTE](../RELAY-24-BLOCKER-EGRESS-FU-LOCKORDERNOTE--dca8ac10/task.md) — RELAY-24-BLOCKER-EGRESS-FU-LOCKORDERNOTE: hub.go lock-order comment omits log.mu and ob.m… (todo)
- [RELAY-24-BLOCKER-EGRESS-FU-LOGFLOOD](../RELAY-24-BLOCKER-EGRESS-FU-LOGFLOOD--40d412fc/task.md) — RELAY-24-BLOCKER-EGRESS-FU-LOGFLOOD: unthrottled WARN + full ed25519 mint under the hub's… (todo)
- [RELAY-24-BLOCKER-EGRESS-FU-NOROUTEWORDING](../RELAY-24-BLOCKER-EGRESS-FU-NOROUTEWORDING--5c465133/task.md) — RELAY-24-BLOCKER-EGRESS-FU-NOROUTEWORDING: no-route drop log line does not spell out inva… (todo)
- [RELAY-24-BLOCKER-EGRESS-FU-SESSIONCACHEGUARD](../RELAY-24-BLOCKER-EGRESS-FU-SESSIONCACHEGUARD--65773db7/task.md) — RELAY-24-BLOCKER-EGRESS-FU-SESSIONCACHEGUARD: AST guard gap over the new outbound dial pa… (todo)
- [RELAY-24-BLOCKER-EGRESS-HANDSHAKE](../RELAY-24-BLOCKER-EGRESS-HANDSHAKE--0ab31d26/task.md) — RELAY-24-BLOCKER-EGRESS-HANDSHAKE: this bus never DIALS a peer, so its relay Registry nev… (todo)
- [RELAY-24-FU-STOREMSGLOOKUP](../RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) — RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMess… (done)
- [RELAY-25-FU-INBOUNDBIND](../RELAY-25-FU-INBOUNDBIND--336c3b76/task.md) — RELAY-25-FU-INBOUNDBIND: fed-smoke.sh never binds each peer's INBOUND client-certificate… (done)
- [RELAY-FU-IDEM-METER-BY-PEER](../RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) — RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not t… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
