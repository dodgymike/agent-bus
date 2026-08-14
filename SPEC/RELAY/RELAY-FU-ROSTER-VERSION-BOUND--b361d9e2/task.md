# internal/relay/doc.go gap 3: RosterUpdate.BusID is not bound to the authenticated connection -- one request permanently wedges a peer via Version=MaxUint64

| Field | Value |
| --- | --- |
| Public id | `b361d9e2-ed4c-4ebe-b725-11f408095127` |
| Key | RELAY-FU-ROSTER-VERSION-BOUND |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | relay |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T18:06:27.406346+00:00 |
| Updated | 2026-08-14T20:23:44.273780+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestRosterUpdateBusIDBoundToConnection ./internal/relay
```

## Status note

DOWNGRADED P0->P2 2026-08-14: re-verified internal/relay/registry.go:396 (ApplyRosterUpdate step 3) directly at HEAD -- it DOES enforce strict monotonicity (u.Version <= st.version is rejected). This task's own description already states the correct scoping: bounding Version alone narrows the blast radius of ONE exploitation shape but does not close the root cause, which is the missing BusID-to-authenticated-connection binding (the impersonation vector -- claiming a VICTIM peer's BusID with Version=MaxUint64). That root cause is the SAME class of gap already tracked at P0 by RELAY-FU-PEERBUSID-CROSSCHECK (b2c28232, blocks wiring) and shares its fix mechanism (httpapi.PeerBusIDFromContext cross-check at the wiring site) with the gap-5/gap-6 twins this task already cross-references. As SCOPED here -- a self-contained internal/relay Version-bound change with no BusID cross-check -- this is hardening that reduces a self-inflicted wedge's ceiling, not the P0 fix for third-party impersonation. Latent (requires an already-trusted peer to misbehave against itself, or the separately-tracked BusID-binding gap to also be exploited), not live on its own.

## Description

Filed 2026-08-14 by reading internal/relay/doc.go's known-gaps section directly at HEAD (gap 3, doc.go:323-327), not from a summary. Also independently verified by the security gate against the watermark task (86c7d368) as one of four P0s remaining on the RELAY-24 critical path.

THE GAP, doc.go's own words: "RosterUpdate.BusID MUST BE BOUND TO THE AUTHENTICATED CONNECTION. Nothing here checks that the peer on the wire IS the bus the update describes. One no-delta update claiming another peer's bus id with Version set to MaxUint64 wedges that peer PERMANENTLY: every genuine update it sends is then refused as stale, recoverable only by re-handshake. One request."

MECHANISM, verified directly at internal/relay/registry.go:440: applying a RosterUpdate does `st.version = u.Version` unconditionally (past the earlier MaxRosterAgents size cap) with no check that the CONNECTION presenting the update is actually authenticated AS the bus named in u.BusID, and no bound on u.Version itself. A peer connecting with its own valid certificate can send a no-delta RosterUpdate CLAIMING BusID=<victim>, Version=MaxUint64 -- the update installs against the VICTIM's roster state (impersonation via the unbound BusID claim), and because MaxUint64 can never be exceeded by a genuine subsequent update from the real victim bus, that bus is wedged permanently until a re-handshake. One request, no repetition needed.

THE FIX, per doc.go's own cross-reference note (doc.go:359-361, elaborated at :368-391 under gap 6, shared verbatim by gaps 3/5/6): the authenticated peer identity now EXISTS and is REACHABLE -- RELAY-20's mount attaches it to the request context, and every callback in this package (RosterConfig.Apply included) takes that same context.Context. A wiring site can call `peerBus := httpapi.PeerBusIDFromContext(ctx)` and compare it against the claimed RosterUpdate.BusID before applying the update. NOTHING IN THIS PACKAGE DOES THAT YET. Bounding Version alone (the registry.go:440 line) narrows the blast radius of ONE exploitation shape but does not close the root cause -- the BusID-to-connection binding is the actual gap; do not treat a Version cap as sufficient on its own.

STRUCTURAL CONSTRAINT ON WHERE THE FIX CAN LIVE, per doc.go:393-396 and confirmed by the coordinator: internal/relay must NEVER import internal/httpapi (TestRelayImportedOnlyByWiringSites governs the reverse), so the cross-check can only ever live in the CALLBACK, at the wiring site -- never in a handler inside this package. That places this fix's actual comparison logic at RELAY-24 (or wherever RosterConfig.Apply's real implementation is constructed), not inside internal/relay itself, even though the Version-bound half (registry.go:440) is a self-contained internal/relay change that CAN land independently.

SHARES ITS FIX MECHANISM WITH: gap 5's inbound twin (PeerEnrollRequest.BusID, filed separately) and gap 6 (BusPath's last hop, already filed as RELAY-FU-BUSPATH-BIND-PEER, f6a9fad0) -- all three compare the same httpapi.PeerBusIDFromContext(ctx) value against a different claimed field. Real relates relations wired to both.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-24](../RELAY-24--e303c624/task.md)
- **relates to** [RELAY-FU-BUSPATH-BIND-PEER](../RELAY-FU-BUSPATH-BIND-PEER--f6a9fad0/task.md)
- **relates to** [RELAY-FU-PEERBUSID-CROSSCHECK](../RELAY-FU-PEERBUSID-CROSSCHECK--b2c28232/task.md)
- **relates to** [RELAY-FU-PEERENROLL-BUSID-BIND](../RELAY-FU-PEERENROLL-BUSID-BIND--12f39697/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-FU-BUSPATH-BIND-PEER](../RELAY-FU-BUSPATH-BIND-PEER--f6a9fad0/task.md) — RELAY-FU-BUSPATH-BIND-PEER: Bind the arriving BusPath's last hop to the authenticated pee… (todo)
- [RELAY-FU-PEERBUSID-CROSSCHECK](../RELAY-FU-PEERBUSID-CROSSCHECK--b2c28232/task.md) — RELAY-FU-PEERBUSID-CROSSCHECK: invariant 11's PEER cross-check is documented but unimplem… (todo)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-FU-PEERBUSID-CROSSCHECK](../RELAY-FU-PEERBUSID-CROSSCHECK--b2c28232/task.md) — RELAY-FU-PEERBUSID-CROSSCHECK: invariant 11's PEER cross-check is documented but unimplem… (todo)
- [RELAY-FU-PEERENROLL-BUSID-BIND](../RELAY-FU-PEERENROLL-BUSID-BIND--12f39697/task.md) — internal/relay/doc.go gap 5 (inbound twin): peer B presenting its own valid certificate a… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
