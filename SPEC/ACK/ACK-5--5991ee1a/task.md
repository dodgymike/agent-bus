# ACK-5: Multi-hop relay ACK/NACK propagation and correlation

| Field | Value |
| --- | --- |
| Public id | `5991ee1a-fc26-443b-a459-428b14dc18da` |
| Key | ACK-5 |
| Epic | [ACK](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | ack |
| Section | backlog |
| Tags | — |
| Created | 2026-08-09T08:25:38.013443+00:00 |
| Updated | 2026-08-22T06:30:25.865791+00:00 |
| Completed | 2026-08-22T06:30:25.865774+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh 'go test -race -run "^TestThreeBusAckNackPropagation$" ./internal/relay'
```

## Description

Propagate terminal recipient outcomes backward over multiple relays without confusing hop receipt with end-to-end delivery. Preserve traversed-path loop rules and correlate one terminal outcome exactly once to the originating durable lifecycle. Depends on existing RELAY-19 forwarding rather than reimplementing it. Invariants: 1,2,4,5,6,10,11. Prospective files: internal/relay/, internal/hub/, PROTOCOL.md.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-19](../../RELAY/RELAY-19--24e0bd11/task.md) — RELAY-19: Forwarder writes and settles outbox records (part 2 of 2) (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4547cb42-f7a2-4cf0-8f5c-8220b2f76246](../../DOCS/DECISIONS.md-dated-correction-beneath-the-ACK-5-NOT-LAND--4547cb42/task.md) — DECISIONS.md: dated correction beneath the ACK-5 "NOT LANDED" caveat, now false (todo)
- [51e0993f-76e0-40fd-b6a0-cd7d83d83548](../../PROCESS/DECISIONS.md-record-the-tiered-review-chain-and-the-rais--51e0993f/task.md) — DECISIONS.md: record the tiered review chain and the raise-only asymmetry (todo)
- [727dc387-dd95-48e4-9616-9b9b1584ac90](../../PROCESS/Security-re-gates-must-be-delta-scoped-citing-the-prior--727dc387/task.md) — Security re-gates must be delta-scoped, citing the prior verdict (todo)
- [ACK-12-FU-DESTINATION-ROW](../ACK-12-FU-DESTINATION-ROW--7d564118/task.md) — ACK-12-FU-DESTINATION-ROW: a relayed message gets no ack row on the DESTINATION bus, so i… (done)
- [ACK-12-FU-WATCH-CORRELATION-KEY](../ACK-12-FU-WATCH-CORRELATION-KEY--f423959c/task.md) — ACK-12-FU-WATCH-CORRELATION-KEY: \`watch\` never exposes the origin message id, so a recipi… (done)
- [ACK-12-FU-WATCH-CORRELATION-KEY-FU-EXITEIGHTCOUNT](../ACK-12-FU-WATCH-CORRELATION-KEY-FU-EXITEIGHTCOUNT--a74dd477/task.md) — ACK-12-FU-WATCH-CORRELATION-KEY-FU-EXITEIGHTCOUNT: exit 8 unknown is four answers at once… (todo)
- [ACK-14](../ACK-14--1884218d/task.md) — ACK-14: retry exhaustion must BOUNCE to the sender -- today the horizon expires and nobod… (todo)
- [ACK-5-FU-AGENTACK-METER](../ACK-5-FU-AGENTACK-METER--058673d6/task.md) — ACK-5-FU-AGENTACK-METER: POST /v1/ack is an unmetered origination point for federation wo… (todo)
- [ACK-5-FU-BUSPATH-SENDER](../ACK-5-FU-BUSPATH-SENDER--57fe695f/task.md) — ACK-5-FU-BUSPATH-SENDER: relayedBusPath never requires the arriving path to END at the au… (todo)
- [ACK-5-FU-EXIT7-AMBIGUOUS](../ACK-5-FU-EXIT7-AMBIGUOUS--26c0b25f/task.md) — ACK-5-FU-EXIT7-AMBIGUOUS: agent-busctl exit 7 now carries two unrelated meanings on POST… (todo)
- [ACK-5-FU-ONDISK-STALEREF](../ACK-5-FU-ONDISK-STALEREF--a299d922/task.md) — ACK-5-FU-ONDISK-STALEREF: CONTRACTS-ONDISK.md still forward-references intermediate ACK r… (todo)
- [ACK-5-FU-PROVENANCE-BODYCOPY](../ACK-5-FU-PROVENANCE-BODYCOPY--eea4f722/task.md) — ACK-5-FU-PROVENANCE-BODYCOPY: the transit authorization deep-copies a message body per PO… (todo)
- [ACK-5-FU-REGISTRY-BASEURL-GUARD](../ACK-5-FU-REGISTRY-BASEURL-GUARD--699c108f/task.md) — ACK-5-FU-REGISTRY-BASEURL-GUARD: guard test: a peer must never be able to write its own R… (todo)
- [ACK-5-FU-STALEWATCHCOMMENT](../ACK-5-FU-STALEWATCHCOMMENT--b5ffc730/task.md) — ACK-5-FU-STALEWATCHCOMMENT: internal/hub/ack.go still says agent-busctl watch does not ex… (todo)
- [ACK-5-FU-TRANSIT-503-IS-PERMANENT](../ACK-5-FU-TRANSIT-503-IS-PERMANENT--ce287d71/task.md) — ACK-5-FU-TRANSIT-503-IS-PERMANENT: a permanent origin 409 reaches the recipient as a retr… (todo)
- [ACK-5-FU-TRANSIT-DUPLICATE](../ACK-5-FU-TRANSIT-DUPLICATE--cb10d713/task.md) — ACK-5-FU-TRANSIT-DUPLICATE: an intermediate discards the origins duplicate flag and alway… (todo)
- [ACK-7](../ACK-7--b7bf9631/task.md) — ACK-7: ACK/NACK retry, idempotency and exactly-once terminal handling (done)
- [ACK-7-FU-MIRROR-REPOINT](../ACK-7-FU-MIRROR-REPOINT--ee253a27/task.md) — ACK-7-FU-MIRROR-REPOINT: a7AssertMirrorMatchesProduction must follow the settle path, not… (done)
- [ACK-7-FU-SETTLEPATH-GUARD](../ACK-7-FU-SETTLEPATH-GUARD--e3218b15/task.md) — ACK-7-FU-SETTLEPATH-GUARD: a7SettlePath's default-arm assertion accepts the wrong switch,… (todo)
- [ACK-BROADCAST-NO-LIFECYCLE-ROW](../ACK-BROADCAST-NO-LIFECYCLE-ROW--e8510bb3/task.md) — ACK-BROADCAST-NO-LIFECYCLE-ROW: a same-bus BROADCAST opens no lifecycle row, so agent-bus… (todo)
- [ACK-RETRY-ENGINE](../ACK-RETRY-ENGINE--81ce7331/task.md) — ACK-RETRY-ENGINE: sender-side retry of an unacknowledged relayed message, with a defined… (todo)
- [ACK-TRANSIT-CAP-BOUNDS-CONCURRENCY-NOT-RATE](../ACK-TRANSIT-CAP-BOUNDS-CONCURRENCY-NOT-RATE--2b63d938/task.md) — the outbound ACK transit cap bounds CONCURRENCY, not RATE (todo)
- [CLI-ENROL-E2E-SIGTERM-STARTUP-RACE](../../UNASSIGNED/CLI-ENROL-E2E-SIGTERM-STARTUP-RACE--1691873b/task.md) — TestCLIEnrolEndToEnd SIGTERMs the priming bus ~1300 lines before its signal handler exists (todo)
- [RELAY-47](../../RELAY/RELAY-47--dd69c4d3/task.md) — RELAY-47: ONWARD RELAY -- WIRE an intermediate bus to forward a relayed message to a THIR… (done)
- [RELAY-54](../../RELAY/RELAY-54--911841af/task.md) — RELAY-54: an abandoned outbox job is invisible to every subcommand -- the drain a rollout… (done)
- [cf8feb8e-b845-43d7-bd5f-7b5e9074e4d2](../cmd-agent-busctl-ackstatus.go-correct-the-stale-P0-7d564--cf8feb8e/task.md) — cmd/agent-busctl/ackstatus.go: correct the stale "P0 7d564118 is CLOSED" comment (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
