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
| Updated | 2026-08-14T20:26:03.953184+00:00 |
| Completed | — |

## Proof command

```sh
go build ./... && go test -race -run TestRelayWiringComposesRoutesWhenPeersConfigured ./cmd/agent-bus
```

## Status note

COMPLETE SET AS OF 2026-08-14 (coordinator + spec-keeper, corrected pass 2 -- superseded item swapped, one priority corrected, and blocker-independence made explicit because the coordinator flagged overstating this set's internal coupling): watermark task (SIGN-1-FU-REORDER-WATERMARK, 86c7d368, still todo) is wired blocks->RELAY-24. NONE OF THE ITEMS BELOW BLOCKS OR IS BLOCKED BY THE WATERMARK, AND THE WATERMARK BLOCKS NONE OF THEM -- verified against the live relations graph on both sides (86c7d368 carries exactly one outgoing edge, blocks->RELAY-24; none of the items below carry any edge to or from 86c7d368). They are independent, parallel blockers of RELAY-24 only, not a sequenced or interdependent wave. RELAY-21's reviewer gate returned PASS -- RELAY-21 is now DONE (14eafd9), no longer a blocker.

Seven items remain, all wired blocks->RELAY-24, independently of each other and of the watermark:
  - RELAY-FU-IDEM-METER-BY-PEER (8774f265, P0)
  - RELAY-FU-PEERBUSID-CROSSCHECK (b2c28232, P0) -- umbrella over the next two
  - RELAY-FU-ROSTER-VERSION-BOUND (b361d9e2, P2 -- DOWNGRADED from P0 2026-08-14: registry.go:396 already enforces strict monotonicity; the Version-bound-alone fix narrows a self-inflicted wedge's ceiling but is not the root-cause fix, which is the BusID-binding gap already tracked at P0 under RELAY-FU-PEERBUSID-CROSSCHECK)
  - RELAY-FU-PEERENROLL-BUSID-BIND (12f39697, P0, doc.go gap 5 inbound twin)
  - RELAY-FU-BUSPATH-BIND-PEER (f6a9fad0, P2, doc.go gap 6)
  - RELAY-22 (b4e45cda, P1, wave-3 abuse-control primitive) -- REPLACES RELAY-FU-INGEST-RATELIMIT (e7c66d83): that task was a duplicate filed before RELAY-22 was found, now superseded by RELAY-22 via the native relation; RELAY-22's blocks->RELAY-24 edge added directly since the superseded task's edge no longer represents a live blocker.
  - RELAY-FU-BUSPATH-OFFBYONE (97fc6038, P1)

JUDGEMENT CALL, recorded not silently applied (coordinator's question: separable prerequisites vs requirements OF this task's own wiring): SPLIT INTO TWO GROUPS.

GROUP A -- GENUINELY SEPARABLE, correct as pure blocks prerequisites, dispatchable independently at any time: RELAY-FU-BUSPATH-OFFBYONE (97fc6038) and RELAY-22 (b4e45cda) -- neither depends on the peer-identity-context machinery; RELAY-22 is its own wave-3 task with its own DECISIONS.md ownership.

GROUP B -- SHARE ONE STRUCTURAL CONSTRAINT THAT MAKES THEM REQUIREMENTS OF THIS TASK'S OWN WIRING, NOT SEPARABLE PREREQUISITES: RELAY-FU-IDEM-METER-BY-PEER, RELAY-FU-PEERBUSID-CROSSCHECK, RELAY-FU-ROSTER-VERSION-BOUND, RELAY-FU-PEERENROLL-BUSID-BIND, RELAY-FU-BUSPATH-BIND-PEER. Confirmed structural fact (coordinator): internal/httpapi already imports internal/relay (peermount.go:115) and guards_test.go forbids the reverse, so httpapi.PeerBusIDFromContext is UNREACHABLE from internal/relay's own callback bodies without inverting the dependency or rewriting AcceptPeer/AcceptRelay/RosterConfig.Apply to take an explicit peerBusID parameter instead of reading context -- doc.go itself argues for the explicit-parameter form for the same silently-forgettable-context reason security prefers it elsewhere. That places the ACTUAL comparison/metering logic for all five at the wiring site by construction, not by preference.

CAVEAT WORTH RECORDING: each of these five likely has a SEPARABLE SUB-PART that can land independently and reduce what this task itself has to write -- RELAY-FU-ROSTER-VERSION-BOUND's Version-bound-alone half (registry.go:440) is self-contained internal/relay code; RELAY-FU-PEERENROLL-BUSID-BIND's outbound half (Client.Enroll validating against the pinned cert) likewise; RELAY-22's core limiter data structure/algorithm can plausibly be built and unit-tested without the peer-identity wiring, only its per-peer KEYING needs this task. But the FULL closure of every one of the five needs this task's own composition code (or a signature-change PR immediately upstream of it) to actually wire httpapi.PeerBusIDFromContext through to the relay callbacks.

RECOMMENDATION: dispatch this task ONCE with the five Group-B items folded into its own acceptance criteria (their separable sub-parts can land beforehand to reduce scope, but their closure is this task's to deliver), rather than sequencing five separate dispatches first. RELAY-FU-BUSPATH-OFFBYONE and RELAY-22 (Group A) can be dispatched independently, any time, with no ordering dependency on this task or on each other.

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
- **blocked by** [RELAY-22](../RELAY-22--b4e45cda/task.md)
- **blocked by** [RELAY-24-BLOCKER-HUBINGEST](../RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md)
- **blocked by** [RELAY-34](../RELAY-34--03fd8897/task.md)
- **blocked by** [RELAY-FU-BUSPATH-BIND-PEER](../RELAY-FU-BUSPATH-BIND-PEER--f6a9fad0/task.md)
- **blocked by** [RELAY-FU-BUSPATH-OFFBYONE](../RELAY-FU-BUSPATH-OFFBYONE--97fc6038/task.md)
- **blocked by** [RELAY-FU-IDEM-METER-BY-PEER](../RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md)
- **blocked by** [RELAY-FU-INGEST-RATELIMIT](../RELAY-FU-INGEST-RATELIMIT--e7c66d83/task.md)
- **blocked by** [RELAY-FU-PEERBUSID-CROSSCHECK](../RELAY-FU-PEERBUSID-CROSSCHECK--b2c28232/task.md)
- **blocked by** [RELAY-FU-PEERENROLL-BUSID-BIND](../RELAY-FU-PEERENROLL-BUSID-BIND--12f39697/task.md)
- **blocked by** [RELAY-FU-ROSTER-VERSION-BOUND](../RELAY-FU-ROSTER-VERSION-BOUND--b361d9e2/task.md)
- **blocked by** [SIGN-1-FU-OUTOFORDER-POISON](../../SIGN/SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md)
- **blocked by** SIGN-1-FU-REORDER-WATERMARK (unresolved)
- **blocks** [RELAY-25](../RELAY-25--10491a01/task.md)
- **relates to** [RELAY-16-FU-SEQUENCING](../RELAY-16-FU-SEQUENCING--83ef0b67/task.md)
- **relates to** [RELAY-35](../RELAY-35--2bafb2a5/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-12](../RELAY-12--069f0607/task.md) — RELAY-12: agent-bus peer add\|list\|remove (done)
- [RELAY-19](../RELAY-19--24e0bd11/task.md) — RELAY-19: Forwarder writes and settles outbox records (part 2 of 2) (done)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-22](../RELAY-22--b4e45cda/task.md) — RELAY-22: Choose and wire the multi-principal relay abuse-control primitive (todo)
- [RELAY-34](../RELAY-34--03fd8897/task.md) — RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can… (done)
- [RELAY-35](../RELAY-35--2bafb2a5/task.md) — RELAY-35: PeerStore composition-root precondition -- replay MUST run before the first wri… (todo)
- [RELAY-FU-BUSPATH-BIND-PEER](../RELAY-FU-BUSPATH-BIND-PEER--f6a9fad0/task.md) — RELAY-FU-BUSPATH-BIND-PEER: Bind the arriving BusPath's last hop to the authenticated pee… (todo)
- [RELAY-FU-BUSPATH-OFFBYONE](../RELAY-FU-BUSPATH-OFFBYONE--97fc6038/task.md) — RELAY-FU-BUSPATH-OFFBYONE: bus-path off-by-one between internal/relay/path.go:128 and int… (todo)
- [RELAY-FU-IDEM-METER-BY-PEER](../RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) — RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not t… (todo)
- [RELAY-FU-INGEST-RATELIMIT](../RELAY-FU-INGEST-RATELIMIT--e7c66d83/task.md) — RELAY-FU-INGEST-RATELIMIT: no rate limit, quota or concurrency cap of any kind on relayed… (superseded)
- [RELAY-FU-PEERBUSID-CROSSCHECK](../RELAY-FU-PEERBUSID-CROSSCHECK--b2c28232/task.md) — RELAY-FU-PEERBUSID-CROSSCHECK: invariant 11's PEER cross-check is documented but unimplem… (todo)
- [RELAY-FU-PEERENROLL-BUSID-BIND](../RELAY-FU-PEERENROLL-BUSID-BIND--12f39697/task.md) — internal/relay/doc.go gap 5 (inbound twin): peer B presenting its own valid certificate a… (todo)
- [RELAY-FU-ROSTER-VERSION-BOUND](../RELAY-FU-ROSTER-VERSION-BOUND--b361d9e2/task.md) — internal/relay/doc.go gap 3: RosterUpdate.BusID is not bound to the authenticated connect… (todo)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [55fe0e43-3cd1-40a6-8f18-73f0fbea69d3](../CONTRACTS-ONDISK.md-1294-1297-retire-the-composition-wit--55fe0e43/task.md) — CONTRACTS-ONDISK.md:1294-1297: retire the "composition with the forwarder remains RELAY-1… (todo)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-16-FU-SEQUENCING](../RELAY-16-FU-SEQUENCING--83ef0b67/task.md) — RELAY-16-FU-SEQUENCING: RemoteRouter must not be wired before the durable outbox exists (todo)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-21-FU-DOCGAP4](../RELAY-21-FU-DOCGAP4--9972d0ed/task.md) — RELAY-21-FU-DOCGAP4: internal/relay/doc.go known-gaps item 4 falsely claims forward-only-… (todo)
- [RELAY-24-BLOCKER-HUBINGEST](../RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) — RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sen… (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-34](../RELAY-34--03fd8897/task.md) — RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can… (done)
- [RELAY-34-FU-DIRWIRING](../RELAY-34-FU-DIRWIRING--4b302011/task.md) — RELAY-34-FU-DIRWIRING: cmd/agent-bus/peer.go must pass PeerStoreOptions.Dir or every revo… (todo)
- [RELAY-35](../RELAY-35--2bafb2a5/task.md) — RELAY-35: PeerStore composition-root precondition -- replay MUST run before the first wri… (todo)
- [RELAY-37](../RELAY-37--a613ddc8/task.md) — RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64)… (cancelled)
- [RELAY-37](../RELAY-37--7a7e6e8b/task.md) — RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64)… (todo)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-45-FU-CLI](../RELAY-45-FU-CLI--b9d645be/task.md) — RELAY-45-FU-CLI: operator CLI surface for the inbound peer client-certificate binding (todo)
- [RELAY-FU-BUSPATH-OFFBYONE](../RELAY-FU-BUSPATH-OFFBYONE--97fc6038/task.md) — RELAY-FU-BUSPATH-OFFBYONE: bus-path off-by-one between internal/relay/path.go:128 and int… (todo)
- [RELAY-FU-IDEM-METER-BY-PEER](../RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) — RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not t… (todo)
- [RELAY-FU-INGEST-RATELIMIT](../RELAY-FU-INGEST-RATELIMIT--e7c66d83/task.md) — RELAY-FU-INGEST-RATELIMIT: no rate limit, quota or concurrency cap of any kind on relayed… (superseded)
- [RELAY-FU-PEERBUSID-CROSSCHECK](../RELAY-FU-PEERBUSID-CROSSCHECK--b2c28232/task.md) — RELAY-FU-PEERBUSID-CROSSCHECK: invariant 11's PEER cross-check is documented but unimplem… (todo)
- [RELAY-FU-ROSTER-VERSION-BOUND](../RELAY-FU-ROSTER-VERSION-BOUND--b361d9e2/task.md) — internal/relay/doc.go gap 3: RosterUpdate.BusID is not bound to the authenticated connect… (todo)
- [SIGN-1-FU-OUTOFORDER-POISON](../../SIGN/SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) — SIGN-1-FU-OUTOFORDER-POISON: Reserve-then-send lets mints be spent out of order, which pe… (done)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)
- [c716f8e7-ad9c-4af9-9fac-1bdb75c8f900](../../DOCS/PROTOCOL.md-1002-says-internal-relay-is-imported-by-noth--c716f8e7/task.md) — PROTOCOL.md:1002 says internal/relay is 'imported by nothing' -- false since ed77bba (int… (todo)
- [eb47af9d-5342-4944-87e8-94f5e2399e8f](../RELAY-19-reviewer-P2s-deliberately-not-applied-preserved--eb47af9d/task.md) — RELAY-19 reviewer P2s deliberately not applied (preserved an md5-pinned PASS) -- apply th… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
