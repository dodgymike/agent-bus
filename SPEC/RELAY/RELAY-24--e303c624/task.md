# RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go

| Field | Value |
| --- | --- |
| Public id | `e303c624-3062-446d-9efc-86a9284220d3` |
| Key | RELAY-24 |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P0 |
| Component | cli |
| Section | backlog |
| Tags | vacuous-today, critical-path |
| Created | 2026-08-08T15:56:48.063908+00:00 |
| Updated | 2026-08-14T10:06:06.688879+00:00 |
| Completed | — |

## Proof command

```sh
go build ./... && go test -race -run TestRelayWiringComposesRoutesWhenPeersConfigured ./cmd/agent-bus
```

## Description

FEDERATION phase, wave 4. Deps: RELAY-12 (peer subcommand), RELAY-20 (peer routes mounted),
RELAY-21 (AcceptRelay callback).

The composition root in cmd/agent-bus/main.go + new relaywiring.go: loads peer records, builds
CrossBusTrust, constructs the Forwarder/Registry, and registers the /v1/peer/* routes only when
both are non-nil, per RELAY-20's contract.

RELAY-19 LANDS UNWIRED (2026-08-14): NewForwarder has zero production callers as of RELAY-19, so
cross-bus delivery from a running server is still BEST EFFORT until this task lands. RELAY-24 MUST
pass Outbox + RecoverMessage into the Forwarder and MUST call Resume() -- and that call has a hard
ordering constraint, recorded here because it is easy to get wrong in exactly one direction each way:

  - Resume() MUST run AFTER the peer roster (Registry) is restored. Resume settles a job ABANDONED
    (durable, irreversible) whenever the Registry does not know the job's peer -- correct for a
    genuine de-peering, but if Resume runs before the roster is populated it will settle the ENTIRE
    recovered pending outbox as abandoned, a false-abandonment mass-loss bug, not a genuine one.
  - Resume() MUST run BEFORE the server starts serving traffic. Too late and jobs enqueued by live
    traffic race jobs recovered by Resume, and (per RELAY-19's reviewer notes) a live Enqueue racing
    Resume can double-queue the same jobID -- one duplicate relay attempt plus a logged
    ErrOutboxSettled at the second Error call. The cheapest fix flagged for this task specifically:
    make Enqueue refuse until the Outbox has been resumed, so the ordering bug fails loudly at the
    Outbox instead of silently producing a duplicate on the wire.

This sits ALONGSIDE RELAY-24's existing ordering constraint that the peer store must be replayed
from its durable log before its FIRST WRITE (see the request note below from RELAY-10's security
re-verification, and RELAY-34/RELAY-35). Record both ordering requirements TOGETHER when this task is
implemented: they compose into one three-stage startup sequence -- (1) replay peer store, (2) restore
Registry/roster from it, (3) call Forwarder.Resume() -- and each stage's precondition is the previous
stage's postcondition. Getting the relative order of 1↔2 wrong loses peer config; getting 2↔3 wrong
looks like an in-memory recovery bug from the wrong evidence, because the outbox itself replayed
correctly and only the Registry ordering downstream of it was at fault.

Source: RELAY-19 file-followups brief item 3, 2026-08-14.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [RELAY-12](../RELAY-12--069f0607/task.md)
- **blocked by** [RELAY-19](../RELAY-19--24e0bd11/task.md)
- **blocked by** [RELAY-20](../RELAY-20--701dc54d/task.md)
- **blocked by** [RELAY-21](../RELAY-21--f5ce883e/task.md)
- **blocked by** [RELAY-34](../RELAY-34--03fd8897/task.md)
- **blocks** [RELAY-25](../RELAY-25--10491a01/task.md)
- **relates to** [RELAY-16-FU-SEQUENCING](../RELAY-16-FU-SEQUENCING--83ef0b67/task.md)
- **relates to** [RELAY-35](../RELAY-35--2bafb2a5/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-12](../RELAY-12--069f0607/task.md) — RELAY-12: agent-bus peer add\|list\|remove (done)
- [RELAY-19](../RELAY-19--24e0bd11/task.md) — RELAY-19: Forwarder writes and settles outbox records (part 2 of 2) (in_progress)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (todo)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (todo)
- [RELAY-34](../RELAY-34--03fd8897/task.md) — RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can… (done)
- [RELAY-35](../RELAY-35--2bafb2a5/task.md) — RELAY-35: PeerStore composition-root precondition -- replay MUST run before the first wri… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [55fe0e43-3cd1-40a6-8f18-73f0fbea69d3](../CONTRACTS-ONDISK.md-1294-1297-retire-the-composition-wit--55fe0e43/task.md) — CONTRACTS-ONDISK.md:1294-1297: retire the "composition with the forwarder remains RELAY-1… (todo)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-16-FU-SEQUENCING](../RELAY-16-FU-SEQUENCING--83ef0b67/task.md) — RELAY-16-FU-SEQUENCING: RemoteRouter must not be wired before the durable outbox exists (todo)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-34](../RELAY-34--03fd8897/task.md) — RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can… (done)
- [RELAY-34-FU-DIRWIRING](../RELAY-34-FU-DIRWIRING--4b302011/task.md) — RELAY-34-FU-DIRWIRING: cmd/agent-bus/peer.go must pass PeerStoreOptions.Dir or every revo… (todo)
- [RELAY-35](../RELAY-35--2bafb2a5/task.md) — RELAY-35: PeerStore composition-root precondition -- replay MUST run before the first wri… (todo)
- [RELAY-37](../RELAY-37--a613ddc8/task.md) — RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64)… (cancelled)
- [RELAY-37](../RELAY-37--7a7e6e8b/task.md) — RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64)… (todo)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (todo)
- [eb47af9d-5342-4944-87e8-94f5e2399e8f](../RELAY-19-reviewer-P2s-deliberately-not-applied-preserved--eb47af9d/task.md) — RELAY-19 reviewer P2s deliberately not applied (preserved an md5-pinned PASS) -- apply th… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
