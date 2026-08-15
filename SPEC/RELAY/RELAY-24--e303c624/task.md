# RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go

| Field | Value |
| --- | --- |
| Public id | `e303c624-3062-446d-9efc-86a9284220d3` |
| Key | RELAY-24 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | cli |
| Section | backlog |
| Tags | vacuous-today, critical-path |
| Created | 2026-08-08T15:56:48.063908+00:00 |
| Updated | 2026-08-15T13:46:29.035552+00:00 |
| Completed | 2026-08-15T13:46:29.035534+00:00 |

## Proof command

```sh
go build ./... && go test -race -run TestRelayWiringComposesRoutesWhenPeersConfigured ./cmd/agent-bus
```

## Status note

CODE-COMPLETE, NOT LIVE, NOT DONE -- 2026-08-14. Reviewer verdict is CHANGES-REQUIRED, narrowed to non-code work on re-verification: "the new code is sound, and I found no defect requiring a code change. What still blocks completion is spec + record, not implementation." Do not read code-complete as done -- this has been mis-read that way twice today already. Do not flip this task to done until every item below is closed.

WHAT IS ACTUALLY LIVE: ingress wiring only (a bus RECEIVING a relayed message), gated behind httpapi's mount guard, which itself refuses to register unless at least one peer is BINDABLE. Bindable peer count is 0 in every operator-reachable configuration today (RELAY-24-BLOCKER-PEERCERTFLAG, filed 2026-08-14) -- so even the ingress half this task built is not reachable from a real deployment until that sibling P0 lands. Egress (a bus SENDING a relayed message) has no wiring at all and is split out to RELAY-24-BLOCKER-EGRESS, also filed 2026-08-14 -- see that task for the three specific gaps. This task's own description was amended 2026-08-14 to remove the Resume()/RecoverMessage mandate that made the old description unimplementable, the same defect class SIGN-4 had that morning.

REMAINING ACCEPTANCE ITEMS before this task can complete, per the reviewer's re-verification (2026-08-14), all non-code:

1. DECISIONS.md, FOUR dated entries (documentation agent): (i) enrolMemo/rosterMemo are IN-MEMORY -- reviewer ruled this NOT a weakening (no memory existed before; the protected state, the Registry, is itself in-memory and rebuilt by re-handshake) but a deliberate deviation from invariant 10's durable-idempotency-memory default belongs on the record with that reasoning; (ii) the fail-soft NewPeerStore (federation disabled, bus starts) -- same availability-over-configuration shape as invariant 6's trade, correct and record-worthy; (iii) the enrolViolation double-sentinel -- note for that entry: the 403-vs-409 difference is cosmetic on retriability (PeerRefusedError.Retriable() makes every 4xx final), the residue is only the diagnosis code an operator reads; (iv) RELAY-22's primitive-choice entry (per-peer quota + concurrency cap), folded into this task's acceptance since RELAY-24's own code (peerAdmission) already delivers RELAY-22 -- see RELAY-22's (b4e45cda) status_note for the folding decision.

2. CONTRACTS-ONDISK.md: the server now registers PeerRecordKind + BusTrustRecordKind WAL appliers and replays them at startup -- document.

3. CONTRACTS-HTTP.md: the three peer routes are now actually registered, and the exact mount condition (at least one ACTIVE trust record carrying an inbound client-cert fingerprint) -- document. Note this condition is currently unsatisfiable in practice until RELAY-24-BLOCKER-PEERCERTFLAG lands; say so in the doc rather than implying the surface is reachable today.

4. `main.go` commit hazard, state to the integrator verbatim: the worktree main.go holds TWO agents' work with one MIXED hunk (RELAY-24's Peer/PeerPrincipals fields are in the SAME httpapi.Options literal as INVITE-GATE's Invites comment rewrite, and the INVITE-GATE half does not compile against HEAD as of the reviewer's check). A pathspec commit on main.go takes the WORKTREE and would ship both agents' work under one title, possibly leaving main un-compilable. Two safe routes only: (a) INVITE-GATE lands first as its own gated commit, RELAY-24 commits the remainder -- RECOMMENDED; or (b) the integrator constructs a main.go containing only RELAY-24's hunks (reviewer has already proven that overlay green). Re-check `git status --porcelain -- cmd/agent-bus/main.go` for MM and diff the WORKTREE immediately before committing, not the index.

Non-blocking code nits for whoever next touches the file (reviewer, 2026-08-14): relaywiring.go:275-279's comment overstates the meter's bound (real bound is the applied-key table's own size, not "share"); :928-933 overstates step 2 as bounding rate rather than concurrency; the three arms of main.go's federate switch and the peerStore==nil fail-soft path are untested (not blocking, but the nil path was added in response to a security finding and its failure mode is a silent half-outage).

Sibling/dependent tasks filed 2026-08-14, all verified independently against HEAD before filing: RELAY-24-BLOCKER-PEERCERTFLAG (P0, blocks RELAY-25), RELAY-24-BLOCKER-EGRESS (P0, blocks RELAY-25, blocked by RELAY-24-FU-STOREMSGLOOKUP), RELAY-24-FU-STOREMSGLOOKUP (P0, blocks EGRESS), RELAY-24-FU-RELAYHTTP-4XX (P1, relates to this task).

## Description

FEDERATION phase, wave 4. Deps: RELAY-12 (peer subcommand), RELAY-20 (peer routes mounted),
RELAY-21 (AcceptRelay callback).

AMENDED 2026-08-14 (spec-keeper, coordinator-directed) -- this description previously MANDATED work
that is UNIMPLEMENTABLE within this task's scope, the same defect class as SIGN-4 that morning: it
required RELAY-24 to "pass Outbox + RecoverMessage into the Forwarder and MUST call Resume()". The
reviewer independently re-derived the impossibility: forward.go:536-543 makes RecoverMessage
mandatory whenever Outbox is set, and internal/store carries NO OriginMessageID field and no
lookup-by-message-id, so Resume cannot rebuild an envelope -- there is nothing to look up and
nothing to look it up BY. That work is SPLIT OUT to RELAY-24-BLOCKER-EGRESS (blocked by
RELAY-24-FU-STOREMSGLOOKUP, the store prerequisite), both blocking RELAY-25.

RELAY-24's SCOPE IS NOW INGRESS ONLY: the composition root in cmd/agent-bus/main.go + new
relaywiring.go loads peer records, builds CrossBusTrust, constructs the Registry, wires
httpapi.Options.Peer/PeerPrincipals and relay.NewAcceptor(...).Accept as RelayConfig.AcceptRelay
behind a metering wrapper, and registers the /v1/peer/* routes only when the surface is fully
populated AND at least one peer is bindable (see RELAY-24-BLOCKER-PEERCERTFLAG for why that count
is currently 0 for every operator-reachable configuration -- a SIBLING P0 blocker on RELAY-25,
outside this task's file boundary). Forwarder/Outbox/Resume() are NOT this task's concern; a bus's
OUTBOUND relay path to a peer remains unwired until RELAY-24-BLOCKER-EGRESS lands.

ORDERING CONSTRAINT THIS TASK STILL OWNS (implemented, per the 2026-08-14 feature-runner report):
the peer store must be replayed from its durable log BEFORE its first write (see RELAY-10's security
re-verification, and RELAY-34/RELAY-35), and the Registry/roster must be restored from that replay
before the peer routes accept traffic. This two-stage precondition is now structural: the peer
store is registered as the WAL applier for relay.PeerRecordKind and relay.BusTrustRecordKind, so
wal.Open replays it before anything serves. The THIRD stage (Forwarder.Resume(), after Registry
restore, before the server serves traffic) belongs to RELAY-24-BLOCKER-EGRESS now, not here --
that task must compose it with the two stages this task already implements into the same
one-sequence contract described in the removed text below, since each stage's precondition remains
the previous stage's postcondition.

Original ordering note, preserved for RELAY-24-BLOCKER-EGRESS to inherit verbatim rather than
re-derive: Resume() MUST run AFTER the peer roster (Registry) is restored -- Resume settles a job
ABANDONED (durable, irreversible) whenever the Registry does not know the job's peer, correct for a
genuine de-peering, but if Resume runs before the roster is populated it will settle the ENTIRE
recovered pending outbox as abandoned, a false-abandonment mass-loss bug, not a genuine one. Resume()
MUST run BEFORE the server starts serving traffic -- too late and jobs enqueued by live traffic race
jobs recovered by Resume, and a live Enqueue racing Resume can double-queue the same jobID (one
duplicate relay attempt plus a logged ErrOutboxSettled at the second Error call); the cheapest fix
flagged: make Enqueue refuse until the Outbox has been resumed, so the ordering bug fails loudly at
the Outbox instead of silently producing a duplicate on the wire.

Source: RELAY-19 file-followups brief item 3, 2026-08-14. Egress split: RELAY-24 reviewer
re-verification, 2026-08-14.

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
- **blocks** [RELAY-25-FU-INBOUNDBIND](../RELAY-25-FU-INBOUNDBIND--336c3b76/task.md)
- **blocks** [RELAY-25](../RELAY-25--10491a01/task.md)
- **relates to** [RELAY-24-FU-PEERWITHDRAWNMSG](../RELAY-24-FU-PEERWITHDRAWNMSG--2a202eca/task.md)
- **relates to** [RELAY-16-FU-SEQUENCING](../RELAY-16-FU-SEQUENCING--83ef0b67/task.md)
- **relates to** [RELAY-24-FU-RELAYHTTP-4XX](../RELAY-24-FU-RELAYHTTP-4XX--b2fb4b36/task.md)
- **relates to** [RELAY-35](../RELAY-35--2bafb2a5/task.md)
- **relates to** [RELAY-24-FU-AGENTSSORTONHANDSHAKE](../RELAY-24-FU-AGENTSSORTONHANDSHAKE--aeb578f9/task.md)
- **relates to** [RELAY-24-FU-SKIPPEDPEERADJACENCY](../RELAY-24-FU-SKIPPEDPEERADJACENCY--f7eaf64a/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-12](../RELAY-12--069f0607/task.md) — RELAY-12: agent-bus peer add\|list\|remove (done)
- [RELAY-19](../RELAY-19--24e0bd11/task.md) — RELAY-19: Forwarder writes and settles outbox records (part 2 of 2) (done)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-22](../RELAY-22--b4e45cda/task.md) — RELAY-22: Choose and wire the multi-principal relay abuse-control primitive (todo)
- [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (done)
- [RELAY-24-BLOCKER-PEERCERTFLAG](../RELAY-24-BLOCKER-PEERCERTFLAG--0e6b5a49/task.md) — RELAY-24-BLOCKER-PEERCERTFLAG: agent-bus peer add has no flag to bind a peer's inbound cl… (done)
- [RELAY-24-FU-RELAYHTTP-4XX](../RELAY-24-FU-RELAYHTTP-4XX--b2fb4b36/task.md) — RELAY-24-FU-RELAYHTTP-4XX: bus-path last-hop binding refusal answers a retryable 503 inst… (todo)
- [RELAY-24-FU-STOREMSGLOOKUP](../RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) — RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMess… (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-34](../RELAY-34--03fd8897/task.md) — RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can… (done)
- [RELAY-35](../RELAY-35--2bafb2a5/task.md) — RELAY-35: PeerStore composition-root precondition -- replay MUST run before the first wri… (todo)
- [SIGN-4](../../SIGN/SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side seque… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [55fe0e43-3cd1-40a6-8f18-73f0fbea69d3](../CONTRACTS-ONDISK.md-1294-1297-retire-the-composition-wit--55fe0e43/task.md) — CONTRACTS-ONDISK.md:1294-1297: retire the "composition with the forwarder remains RELAY-1… (todo)
- [INVITE-GATE-ENFORCE](../../INVITE/INVITE-GATE-ENFORCE--8297d7e2/task.md) — INVITE-GATE-ENFORCE: enforce invite-only enrolment (P0: anonymous roster exhaustion) (in_progress)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-16-FU-SEQUENCING](../RELAY-16-FU-SEQUENCING--83ef0b67/task.md) — RELAY-16-FU-SEQUENCING: RemoteRouter must not be wired before the durable outbox exists (todo)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-21-FU-DOCGAP4](../RELAY-21-FU-DOCGAP4--9972d0ed/task.md) — RELAY-21-FU-DOCGAP4: internal/relay/doc.go known-gaps item 4 falsely claims forward-only-… (todo)
- [RELAY-22](../RELAY-22--b4e45cda/task.md) — RELAY-22: Choose and wire the multi-principal relay abuse-control primitive (todo)
- [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (done)
- [RELAY-24-BLOCKER-EGRESS-ATTEST](../RELAY-24-BLOCKER-EGRESS-ATTEST--3334677e/task.md) — RELAY-24-BLOCKER-EGRESS-ATTEST: no bus can ISSUE an origin attestation for its own agents… (done)
- [RELAY-24-BLOCKER-HUBINGEST](../RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) — RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sen… (done)
- [RELAY-24-BLOCKER-PEERCERTFLAG](../RELAY-24-BLOCKER-PEERCERTFLAG--0e6b5a49/task.md) — RELAY-24-BLOCKER-PEERCERTFLAG: agent-bus peer add has no flag to bind a peer's inbound cl… (done)
- [RELAY-24-FU-AGENTSSORTONHANDSHAKE](../RELAY-24-FU-AGENTSSORTONHANDSHAKE--aeb578f9/task.md) — RELAY-24-FU-AGENTSSORTONHANDSHAKE: h.Agents() re-sorts the whole roster on every peer han… (todo)
- [RELAY-24-FU-PEERWITHDRAWNMSG](../RELAY-24-FU-PEERWITHDRAWNMSG--2a202eca/task.md) — RELAY-24-FU-PEERWITHDRAWNMSG: startup INFO log says "no peer records and no peer trust re… (todo)
- [RELAY-24-FU-RELAYHTTP-4XX](../RELAY-24-FU-RELAYHTTP-4XX--b2fb4b36/task.md) — RELAY-24-FU-RELAYHTTP-4XX: bus-path last-hop binding refusal answers a retryable 503 inst… (todo)
- [RELAY-24-FU-SKIPPEDPEERADJACENCY](../RELAY-24-FU-SKIPPEDPEERADJACENCY--f7eaf64a/task.md) — RELAY-24-FU-SKIPPEDPEERADJACENCY: skippedPeerRecords ERROR block is wedged inside the com… (todo)
- [RELAY-24-FU-STOREMSGLOOKUP](../RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) — RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMess… (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-25-FU-INBOUNDBIND](../RELAY-25-FU-INBOUNDBIND--336c3b76/task.md) — RELAY-25-FU-INBOUNDBIND: fed-smoke.sh never binds each peer's INBOUND client-certificate… (done)
- [RELAY-34](../RELAY-34--03fd8897/task.md) — RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can… (done)
- [RELAY-34-FU-DIRWIRING](../RELAY-34-FU-DIRWIRING--4b302011/task.md) — RELAY-34-FU-DIRWIRING: cmd/agent-bus/peer.go must pass PeerStoreOptions.Dir or every revo… (todo)
- [RELAY-35](../RELAY-35--2bafb2a5/task.md) — RELAY-35: PeerStore composition-root precondition -- replay MUST run before the first wri… (todo)
- [RELAY-37](../RELAY-37--a613ddc8/task.md) — RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64)… (cancelled)
- [RELAY-37](../RELAY-37--7a7e6e8b/task.md) — RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64)… (todo)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-45-FU-CLI](../RELAY-45-FU-CLI--b9d645be/task.md) — RELAY-45-FU-CLI: operator CLI surface for the inbound peer client-certificate binding (todo)
- [RELAY-FU-BUSPATH-OFFBYONE](../RELAY-FU-BUSPATH-OFFBYONE--97fc6038/task.md) — RELAY-FU-BUSPATH-OFFBYONE: bus-path off-by-one between internal/relay/path.go:128 and int… (done)
- [RELAY-FU-IDEM-METER-BY-PEER](../RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) — RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not t… (done)
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
