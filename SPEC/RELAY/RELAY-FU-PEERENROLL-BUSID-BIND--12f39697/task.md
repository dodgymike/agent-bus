# internal/relay/doc.go gap 5 (inbound twin): peer B presenting its own valid certificate and claiming bus_id C replaces C's roster and resets its version

| Field | Value |
| --- | --- |
| Public id | `12f39697-93b0-432e-92c2-3cf185ea6e50` |
| Key | RELAY-FU-PEERENROLL-BUSID-BIND |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T18:06:27.766032+00:00 |
| Updated | 2026-08-14T18:06:27.766032+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestPeerEnrollBusIDBoundToConnection ./internal/relay
```

## Description

Filed 2026-08-14 by reading internal/relay/doc.go's known-gaps section directly at HEAD (gap 5, doc.go:347-361), not from a summary.

doc.go's own framing: gap 5 as originally written covers only the OUTBOUND half (Client.Enroll validates a dialled peer's claimed id for SHAPE only, and Registry.UpsertPeer installs whatever it claimed -- the gate must cross-check it against the pinned certificate or the invite). The security gate on RELAY-20 found the INBOUND twin missing from the list ENTIRELY, and doc.go's own text says it is "worth more than the item it sits under": PeerEnrollRequest.BusID -- the id a peer claims when it dials US -- is likewise validated for SHAPE only (handshake.go, ValidatePeerBusID), and AcceptPeer receives it UNBOUND to the connection. Once a wiring site routes AcceptPeer to Registry.UpsertPeer, peer B presenting its OWN valid certificate and claiming bus_id "C" REPLACES C's roster and RESETS its version.

SEVERITY: this is a roster-hijack primitive -- an attacker who can complete a legitimate mTLS handshake as themselves (peer B) can overwrite an unrelated peer's (C's) entire roster state by simply asserting C's bus_id in the enrollment payload, with no proof of controlling C. Filed P0 to match the severity doc.go's own text assigns it ("worth more than the item it sits under", and the item it sits under is itself a real gap).

THE FIX, per doc.go's own words: "The fix is the same one gaps 3 and 6 need and it is now available" -- httpapi.PeerBusIDFromContext(ctx) is reachable from every callback in this package (Config.AcceptPeer included, since RELAY-20's mount attaches the authenticated peer identity to the request context every callback receives). A wiring site compares peerBus := httpapi.PeerBusIDFromContext(ctx) against the claimed PeerEnrollRequest.BusID before routing to Registry.UpsertPeer.

STRUCTURAL CONSTRAINT ON WHERE THE FIX CAN LIVE (doc.go:393-396, confirmed by the coordinator): internal/relay must never import internal/httpapi, so this comparison can only live in the CALLBACK, at the wiring site -- not inside this package. Same shape as gaps 3 and 6 (RELAY-FU-ROSTER-VERSION-BOUND and RELAY-FU-BUSPATH-BIND-PEER respectively) -- all three share one fix mechanism at one wiring site. Real relates relations wired to both.

OUTBOUND HALF (the original gap 5 text, Client.Enroll/Registry.UpsertPeer validating a DIALLED peer's claimed id against the pinned cert/invite, not the connection identity) remains IN SCOPE of this same task unless split during implementation -- both halves share the doc.go paragraph and the same underlying UpsertPeer call.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-24](../RELAY-24--e303c624/task.md)
- **relates to** [RELAY-FU-BUSPATH-BIND-PEER](../RELAY-FU-BUSPATH-BIND-PEER--f6a9fad0/task.md)
- **relates to** [RELAY-FU-PEERBUSID-CROSSCHECK](../RELAY-FU-PEERBUSID-CROSSCHECK--b2c28232/task.md)
- **relates to** [RELAY-FU-ROSTER-VERSION-BOUND](../RELAY-FU-ROSTER-VERSION-BOUND--b361d9e2/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-FU-BUSPATH-BIND-PEER](../RELAY-FU-BUSPATH-BIND-PEER--f6a9fad0/task.md) — RELAY-FU-BUSPATH-BIND-PEER: Bind the arriving BusPath's last hop to the authenticated pee… (todo)
- [RELAY-FU-ROSTER-VERSION-BOUND](../RELAY-FU-ROSTER-VERSION-BOUND--b361d9e2/task.md) — internal/relay/doc.go gap 3: RosterUpdate.BusID is not bound to the authenticated connect… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-FU-PEERBUSID-CROSSCHECK](../RELAY-FU-PEERBUSID-CROSSCHECK--b2c28232/task.md) — RELAY-FU-PEERBUSID-CROSSCHECK: invariant 11's PEER cross-check is documented but unimplem… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
