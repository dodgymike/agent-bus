# EPIC ACK — ACK: End-to-end authenticated delivery ACK/NACK

[← all epics](../../SPEC.md)

**20 open / 27 total.** Full records live in `SPEC/ACK/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (20)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| ACK-12 | ACK-12: Three-bus end-to-end ACK/NACK smoke acceptance | in_progress | P0 | [task.md](ACK-12--17406b3a/task.md) | _not fetched_ | [DEPLOY-3](../DEPLOY/DEPLOY-3--9eaf2d19/task.md) |
| ACK-3 | ACK-3: Authenticated peer-hop ACK/NACK wire semantics and correlation | in_progress | P0 | [task.md](ACK-3--263c47fe/task.md) | _not fetched_ | — |
| ACK-7 | ACK-7: ACK/NACK retry, idempotency and exactly-once terminal handling | todo | P0 | [task.md](ACK-7--b7bf9631/task.md) | _not fetched_ | — |
| ACK-8 | ACK-8: ACK/NACK restart, replay and crash-consistency recovery | in_progress | P0 | [task.md](ACK-8--bc12541b/task.md) | _not fetched_ | — |
| ACK-10 | ACK-10: ACK/NACK compatibility, version negotiation and downgrade safety | todo | P1 | [task.md](ACK-10--cf417e18/task.md) | _not fetched_ | — |
| ACK-13 | ACK-13: the closed ACK vocabulary is declared TWICE with different underlying types | in_progress | P1 | [task.md](ACK-13--a998ae43/task.md) | _not fetched_ | [ACK-4](ACK-4--aeb32123/task.md) [ACK-1](ACK-1--e0ac42e1/task.md) [ACK-2](ACK-2--9564f953/task.md) |
| ACK-14 | ACK-14: retry exhaustion must BOUNCE to the sender -- today the horizon expires and nobod… | todo | P1 | [task.md](ACK-14--1884218d/task.md) | _not fetched_ | [ACK-3](ACK-3--263c47fe/task.md) [ACK-6](ACK-6--d3c50d33/task.md) [ACK-9](ACK-9--08f9987f/task.md) |
| ACK-4-FU-RECIPIENT-BINDING | ACK-4-FU-RECIPIENT-BINDING: the obligation binding does not bind the RECIPIENT to the ack… | todo | P1 | [task.md](ACK-4-FU-RECIPIENT-BINDING--ec4a1ac8/task.md) | _not fetched_ | [ACK-3](ACK-3--263c47fe/task.md) |
| ACK-5 | ACK-5: Multi-hop relay ACK/NACK propagation and correlation | in_progress | P1 | [task.md](ACK-5--5991ee1a/task.md) | _not fetched_ | [RELAY-19](../RELAY/RELAY-19--24e0bd11/task.md) |
| ACK-6-FU-CLI | ACK-6-FU-CLI: agent-busctl ack, and the canonical ACK bytes it must sign | in_progress | P1 | [task.md](ACK-6-FU-CLI--836c9ff8/task.md) | _not fetched_ | [ACK-6](ACK-6--d3c50d33/task.md) [ACK-9](ACK-9--08f9987f/task.md) |
| ACK-11 | ACK-11: Document ACK/NACK semantics, operations and privacy limits | todo | P2 | [task.md](ACK-11--5567f490/task.md) | _not fetched_ | [ACK-1](ACK-1--e0ac42e1/task.md) |
| ACK-17 | ACK-17: four keying mutations still leave internal/httpapi green -- session-keyed is the… | in_progress | P2 | [task.md](ACK-17--d4a2d828/task.md) | _not fetched_ | [ACK-16](ACK-16--f60cdd30/task.md) |
| ACK-3-FU-COLLAPSE-WIREVERSION | ACK-3-FU-COLLAPSE-WIREVERSION: collapse relay.AckWireVersion onto relay.WireVersion once… | todo | P2 | [task.md](ACK-3-FU-COLLAPSE-WIREVERSION--8c6d6765/task.md) | _not fetched_ | [ACK-3](ACK-3--263c47fe/task.md) [RELAY-23](../RELAY/RELAY-23--220d36f4/task.md) |
| ACK-6-FU-ACKVOCAB-ENUM | ACK-6-FU-ACKVOCAB-ENUM: export a class enumerator from internal/ack so the frozen signing… | todo | P2 | [task.md](ACK-6-FU-ACKVOCAB-ENUM--a08571b1/task.md) | _not fetched_ | — |
| ACK-6-FU-PROTOCOL-DOC | ACK-6-FU-PROTOCOL-DOC: PROTOCOL.md carries no normative byte table for the ACK format or… | todo | P2 | [task.md](ACK-6-FU-PROTOCOL-DOC--cd5a022a/task.md) | _not fetched_ | [RELAY-14](../RELAY/RELAY-14--7db695ee/task.md) |
| ACK-6-FU-SETTLE-ERRORLOG | ACK-6-FU-SETTLE-ERRORLOG: throttle the terminal-conflict ERROR in ack.Store.Settle | todo | P2 | [task.md](ACK-6-FU-SETTLE-ERRORLOG--dc58e7ee/task.md) | _not fetched_ | [ACK-6](ACK-6--d3c50d33/task.md) [ACK-9](ACK-9--08f9987f/task.md) |
| ACK-6-FU-VERIFYACK-ASTGUARD | ACK-6-FU-VERIFYACK-ASTGUARD: an AST guard so no bus ingest path calls signing.VerifyAck o… | todo | P2 | [task.md](ACK-6-FU-VERIFYACK-ASTGUARD--19af3033/task.md) | _not fetched_ | — |
| ACK-18 | ACK-18: no GLOBAL ceiling on parked ack-status waits -- 32 x enrolled principals | todo | P3 | [task.md](ACK-18--ac5f5fb2/task.md) | _not fetched_ | [ACK-16](ACK-16--f60cdd30/task.md) |
| ACK-3-FU-SETTLEACK-RACE-ARM | ACK-3-FU-SETTLEACK-RACE-ARM: the ack.ErrTerminal arm of federation.settleAck is unreachab… | todo | P3 | [task.md](ACK-3-FU-SETTLEACK-RACE-ARM--d829afb2/task.md) | _not fetched_ | [ACK-3](ACK-3--263c47fe/task.md) |
| ACK-6-FU-SETTLE-WALERR-SENTINEL | ACK-6-FU-SETTLE-WALERR-SENTINEL: a WAL failure in ack.Store.Settle answers 500, not 503 | todo | P3 | [task.md](ACK-6-FU-SETTLE-WALERR-SENTINEL--c4dc6b6b/task.md) | _not fetched_ | [ACK-6](ACK-6--d3c50d33/task.md) [ACK-9](ACK-9--08f9987f/task.md) |

## Closed tasks (7) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| ACK-1 | ACK-1: Define end-to-end ACK/NACK delivery contract and terminal state machine | done | P0 | [task.md](ACK-1--e0ac42e1/task.md) | _not fetched_ | — |
| ACK-2 | ACK-2: Durable local send acceptance and ACK/NACK lifecycle record | done | P0 | [task.md](ACK-2--9564f953/task.md) | _not fetched_ | [a1cbef29-400a-4a1e-9638-cc14d38a7ebf](../UNASSIGNED/WAL-foundation-authenticated-multi-applier-checkpoints-o--a1cbef29/task.md) [617ffe5a-db42-4aeb-89bb-d9b0889f6c19](../RELAY/Bound-retained-outbox-tombstone-resources-without-reopen--617ffe5a/task.md) |
| ACK-4 | ACK-4: ACK/NACK authorization, anti-forgery and privacy review implementation | done | P0 | [task.md](ACK-4--aeb32123/task.md) | _not fetched_ | — |
| ACK-15 | ACK-15: POST /v1/ack has no CLI subcommand -- until it does, no row can ever reach delive… | done | P1 | [task.md](ACK-15--a63b133d/task.md) | _not fetched_ | [ACK-9](ACK-9--08f9987f/task.md) [ACK-6](ACK-6--d3c50d33/task.md) [ACK-1](ACK-1--e0ac42e1/task.md) [ACK-6-FU-CLI](ACK-6-FU-CLI--836c9ff8/task.md) |
| ACK-16 | ACK-16: the per-principal wait cap is untested -- a global bucket passes the whole package | done | P1 | [task.md](ACK-16--f60cdd30/task.md) | _not fetched_ | [ACK-6](ACK-6--d3c50d33/task.md) [ACK-9](ACK-9--08f9987f/task.md) |
| ACK-6 | ACK-6: Recipient delivery acknowledgement boundary | done | P1 | [task.md](ACK-6--d3c50d33/task.md) | _not fetched_ | [ACK-1](ACK-1--e0ac42e1/task.md) |
| ACK-9 | ACK-9: Sender CLI/API acknowledgement status and observability | done | P1 | [task.md](ACK-9--08f9987f/task.md) | _not fetched_ | — |

## Epic description

Planning epic for end-to-end message acknowledgement. Assumption: the user’s “ack/ack” means ACK/NACK (positive and negative acknowledgement), not two positive ACK phases. Planning only: creating this epic implements and promises no ACK/NACK semantics.

Goal: authenticated, durable, correlation-safe sender-visible terminal delivery outcome across local acceptance, relay hops, recipient acknowledgement, retries/restart and a three-bus topology. Existing federation work is reused: WAL checkpoint foundation a1cbef29, bounded tombstone follow-on 617ffe5a, RELAY-19 forwarding, and DEPLOY-3 compose acceptance remain dependencies rather than duplicated.

Open decisions / risks (ACK-1 must resolve before implementation): (1) whether “delivery ACK” means durable recipient inbox receipt or explicit application consumption; (2) ACK/NACK timeout, retry horizon and terminal-record retention without reopening resurrection/resource exhaustion; (3) broadcast fan-out aggregation, partial NACKs and sender-visible quorum/recipient privacy; (4) durable NACK taxonomy—transient hop failure versus terminal policy/recipient refusal—and which details may be disclosed; (5) correlation identifiers and signatures across routed paths, anti-forgery/replay, downgraded peers and version negotiation. Any choice that weakens invariants requires a dated DECISIONS.md record.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
