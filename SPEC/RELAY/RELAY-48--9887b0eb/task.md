# RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at restart, and the envelope cannot be rebuilt from durable state

| Field | Value |
| --- | --- |
| Public id | `9887b0eb-8e8a-45d9-8a10-bd3161f720e2` |
| Key | RELAY-48 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | relay |
| Section | backlog |
| Tags | relay, durability, crash-safety, from-review, relay-47-followup |
| Created | 2026-08-15T12:54:28.921278+00:00 |
| Updated | 2026-08-21T10:28:15.549958+00:00 |
| Completed | 2026-08-21T10:28:15.549941+00:00 |

## Proof command

```sh
go test -race -timeout 1200s -run 'TestOnwardRelayPendingJobRequeuesAfterRestart' ./cmd/agent-bus
```

## Description

Filed 2026-08-15 by spec-keeper on behalf of the RELAY-47 feature-runner. OUT OF BOUNDS for RELAY-47 (touches internal/store and internal/hub, neither in RELAY-47's file boundary). Found INDEPENDENTLY by BOTH the reviewer and the security gate on RELAY-47, and CONFIRMED by the reviewer with a live crash+restart harness over the real wal/outbox/hub/forwarder -- not by reading code. The harness result was identical on every run:

    pending=1  ->  requeued=0  ->  state=abandoned

== WHAT IS BROKEN ==

A bus B accepts a relayed message from A that is addressed to an agent on C. B answers A **200** (durable acceptance -- and per the RELAY-47 operator ruling of 2026-08-15 that 200 is CORRECT and is not in scope here), and writes a durable outbox job recording that it owes C a copy. B then restarts. The obligation is DESTROYED: the job settles `abandoned`, C never hears about the message, and A does not retry because A was told 200.

The discard IS logged loudly and specifically, so **invariant 6 holds**. What does not hold is the promise implied by accepting the obligation durably in the first place.

== MECHANISM (verified against the RELAY-47 build) ==

1. `relay.Forwarder.Enqueue` (internal/relay/forward.go:888) stores `OutboxJob.OriginMessageID` = the **ORIGIN bus's** message id (A's id), which is correct as a correlation key.
2. At boot, `resumeJob` calls `RecoverMessage` (cmd/agent-bus/main.go:980), which looks the message up via `Store.ByOriginMessageID`.
3. **Nothing in this build ever SETS `store.Message.OriginMessageID`.** The field exists (RELAY-24-FU-STOREMSGLOOKUP shipped it) but has no production writer, so `Store.byOrigin` is empty. The fallback to `ByID` cannot match either -- the key is another bus's id, and this bus mints its own (invariant 1).
4. The lookup MISSES; the `!ok` arm settles the job **abandoned** (internal/relay/forward.go:1496-1498).

== ROOT CAUSE -- WHY THIS IS NOT A ONE-LINER ==

`store.Message` has **no `OriginAttestation` field**, while `RelayRequest.OriginAttestation` is REQUIRED on the wire and is VERIFIED by the next hop. So a relayed-in envelope is **UNBUILDABLE from durable state by construction**. The live (no-restart) path works only because the in-memory `relay.RelayedMessage` still carries the attestation. Recovery has nothing to rebuild from.

**DANGER -- do not apply the obvious partial fix.** Setting `store.Message.OriginMessageID` on ingest ALONE is not sufficient and is NOT SAFE on its own: it makes `RecoverMessage`'s `OriginMessageID != ""` guard start firing, which returns an ERROR rather than rebuilding the envelope. The half-fix changes one failure mode into another.

== SCOPE ==

Design decision first (record it in DECISIONS.md), then implement:
- what durable state is required to rebuild a relayed-in envelope byte-faithfully enough for the next hop to verify it (at minimum: origin message id AND the origin attestation, plus whatever SIGN-7 byte-exactness requires);
- where it lives (a `store.Message` field vs a record carried by the outbox job itself -- note the outbox job is on-disk surface, so a record-type/field addition needs a `POST /reservations` number, never an eyeballed one);
- and the ingest-side writer that populates it, plus the `RecoverMessage` guard change that stops the partial fix above from erroring.

Touches `internal/store`, `internal/hub`, `internal/relay`, `cmd/agent-bus`. Coordinate: `internal/hub` was being rewritten by a concurrent agent on 2026-08-15 -- re-confirm the ingest site against COMMITTED code before editing.

== PROOF ==

Crash-injection, per CLAUDE.md ('durability and recovery code must have crash-injection tests'):

    go test -race -run TestOnwardRelayPendingJobRequeuesAfterRestart ./cmd/agent-bus

The test must (a) enqueue a real onward job for a foreign destination, (b) kill mid-flight before the hop completes, (c) restart over the SAME data dir, and (d) assert the job is REQUEUED (not abandoned) and that the rebuilt envelope still carries a next-hop-verifiable origin attestation. Asserting only `requeued=1` without checking the rebuilt attestation is a VACUOUS pass for this task. `bash scripts/fed-smoke.sh` is NOT a proof for this -- it never restarts a bus.

RELATES: RELAY-47 (the wiring this exposes), RELAY-24-FU-STOREMSGLOOKUP (shipped the unwritten field), RELAY-24-BLOCKER-EGRESS-ATTEST.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24-BLOCKER-EGRESS-ATTEST](../RELAY-24-BLOCKER-EGRESS-ATTEST--3334677e/task.md) — RELAY-24-BLOCKER-EGRESS-ATTEST: no bus can ISSUE an origin attestation for its own agents… (done)
- [RELAY-24-FU-STOREMSGLOOKUP](../RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) — RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMess… (done)
- [RELAY-47](../RELAY-47--dd69c4d3/task.md) — RELAY-47: ONWARD RELAY -- WIRE an intermediate bus to forward a relayed message to a THIR… (done)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-8-FU-D2-OBLIGATIONLOST](../../ACK/ACK-8-FU-D2-OBLIGATIONLOST--6926fb51/task.md) — ACK-8-FU-D2-OBLIGATIONLOST: detect and emit obligation_lost (todo)
- [AUTH-10](../../AUTH/AUTH-10--37993b49/task.md) — AUTH-10: An operator/admin principal -- the missing noun blocking AUTH-7, INVMINT and CON… (done)
- [AUTH-10-WIRING](../../AUTH/AUTH-10-WIRING--b11ef24c/task.md) — AUTH-10-WIRING: wire the operator principal into cmd/agent-bus/main.go — until this lands… (done)
- [RELAY-25-FU-CORRELATION](../RELAY-25-FU-CORRELATION--3f009222/task.md) — RELAY-25-FU-CORRELATION: fed-smoke.sh asserts the SAME message_id string in A's, B's and… (done)
- [RELAY-47-FU-DOCS](../RELAY-47-FU-DOCS--6f7281e8/task.md) — RELAY-47-FU-DOCS: three shipped docs still tell agents multi-hop relay does not work, aft… (done)
- [RELAY-52](../RELAY-52--67c6248d/task.md) — RELAY-52: invariant 6's loud-discard line at hub.go:1104 has no test anywhere in the repo (done)
- [RELAY-52-FU-HUBDISCARDS](../RELAY-52-FU-HUBDISCARDS--d2cad9e7/task.md) — RELAY-52-FU-HUBDISCARDS: remaining untested hub/mint/roster discard-and-recovery log lines (done)
- [RELAY-54](../RELAY-54--911841af/task.md) — RELAY-54: an abandoned outbox job is invisible to every subcommand -- the drain a rollout… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
