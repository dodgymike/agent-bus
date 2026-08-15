# RELAY-24-BLOCKER-EGRESS-HANDSHAKE: this bus never DIALS a peer, so its relay Registry never learns a peer's roster -- federated agent discovery/listing cannot resolve

| Field | Value |
| --- | --- |
| Public id | `0ab31d26-4a45-420d-930b-69f77346e4dd` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | relay |
| Section | backlog |
| Tags | blocker, invariant-11 |
| Created | 2026-08-15T07:52:46.461264+00:00 |
| Updated | 2026-08-15T11:24:51.209606+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestCompositionRootDialsConfiguredPeersAndPopulatesRegistry ./internal/relay/... ./cmd/agent-bus/...
```

## Description

CORRECTED 2026-08-15 by spec-keeper, on the reviewer's finding while working RELAY-24-BLOCKER-EGRESS: the original filing was WRONG on two counts, both about what this task actually gates. It does NOT gate directed cross-bus sends, and it does NOT gate broadcast fan-out either. It gates roster DISCOVERY AND LISTING ONLY.

WHY: relay.Registry.Route (internal/relay/registry.go:495-510) resolves purely by the BUS HALF of a fully-qualified recipient id and never consults st.roster. Knows (:519-532) is explicit in its own comment that it "is NOT the routing predicate -- Route is." The composition root now seeds the Registry from the operator's durable peer configuration with an EMPTY roster, and Route works from that alone -- a directed send to an agent behind a configured peer resolves and forwards with no handshake required. Registry.BroadcastTargets (:543-555) is the same shape: it iterates r.peers and filters only on NextHopAllowed; rosters are never consulted there either, so broadcast fan-out to peers is likewise unblocked by an empty roster.

What an empty roster DOES gate: relay.Registry.Knows and any federated/cross-bus agent LISTING that reads the roster -- an agent behind a peer bus cannot be discovered or enumerated until this bus has actually handshaken with that peer and learned its roster. That is the real, narrower scope of this task.

NEEDED (unchanged from before): an outbound peer-handshake driver in the composition root (cmd/agent-bus) that dials each configured peer -- address and pin taken from relay.PeerConfig -- populates the Registry's roster, and refreshes it. Get the fingerprint DIRECTION right: NextHopTLSCertFingerprint is the OUTBOUND, address-keyed fingerprint (the certificate the peer at -url presents to US when WE dial IT) -- this is the one the dialer must pin. It is NOT BusTrustRecord.PeerClientTLSCertFingerprint, which is the INBOUND, bus-principal-keyed binding used for the RECEIVING side (see RELAY-25-FU-INBOUNDBIND, 336c3b76, now done). Conflating the two is a refuted design -- do not repeat it in the dialer.

INVARIANT 11 APPLIES: mutual TLS, pinned via the certificate fingerprint above, no trust-on-first-use, and never a flag or code path that disables certificate verification, not even a documented one.

NOT a blocker on RELAY-24-BLOCKER-EGRESS or on the three-bus directed-send deliverable -- lowered from P0 to P2 accordingly (roster discovery/listing is a real gap, but it is not on the critical path for directed messages or broadcast fan-out to already-work). Relates to RELAY-25 (10491a01), whose smoke test remains the eventual end-to-end proof of federated listing once this lands.

ALSO NOTE (from the reviewer, while confirming the above): a relayed BROADCAST is refused at the receiving end TODAY with ErrUnsignable (internal/relay/message.go:698-700 -- the canonical signing format refuses an empty recipient set, so no signature over a broadcast audience can exist yet; SIGN-3 must define one first), and POST /v1/broadcast itself is 501 on this build, so `Broadcast: m.Broadcast` in the egress envelope is presently a LATENT trap, not a live one -- nothing can reach it end to end yet. A related but not identical task already exists (RELAY-2-FU-BROADCAST-FANOUT, 8b5319e1, P3) describing the old Forwarder.targets fan-out-to-a-guaranteed-400 shape; the ErrUnsignable mechanism and the current 501-on-send framing are a more precise, current restatement of the same underlying gap, tracked separately (see relates edge) rather than merged into this task's already-corrected scope.

PROOF: names a Go test to ADD (it does not exist today, since the outbound handshake driver does not exist yet) -- prefer TestCompositionRootDialsConfiguredPeersAndPopulatesRegistry, asserting that after the composition root's startup sequence runs against a configured relay.PeerConfig, relay.Registry.Knows resolves true for an agent behind the configured peer (NOT Route, which does not need this), using the OUTBOUND NextHopTLSCertFingerprint (not the inbound trust binding) for the dial's pin. Currently unimplementable-as-a-test because the dialer does not exist; MUST be observed RED first and GREEN once the outbound handshake driver is wired and mutually authenticated per invariant 11.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md)
- **relates to** [RELAY-25](../RELAY-25--10491a01/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-2-FU-BROADCAST-FANOUT](../RELAY-2-FU-BROADCAST-FANOUT--8b5319e1/task.md) — RELAY-2-FU-BROADCAST-FANOUT: Forwarder.targets fans broadcasts out to peers that always 4… (todo)
- [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-25-FU-INBOUNDBIND](../RELAY-25-FU-INBOUNDBIND--336c3b76/task.md) — RELAY-25-FU-INBOUNDBIND: fed-smoke.sh never binds each peer's INBOUND client-certificate… (done)
- [SIGN-3](../../SIGN/SIGN-3--f2daa6bc/task.md) — SIGN-3: Broadcast signature covers the recipient set (prevents split-content broadcasts) (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0f8c5332-1236-4e22-a249-72119401003f](../../PROCESS/Spec-Server-API-gap-no-relation-delete-endpoint-wrong-bl--0f8c5332/task.md) — Spec Server API gap: no relation-delete endpoint -- wrong blocks/relates/supersedes/follo… (todo)
- [6f82180f-8f57-473d-bd87-30f6d9d9695d](../PROTOCOL.md-599-cites-a-deleted-test-TestHandshakeHandle--6f82180f/task.md) — PROTOCOL.md:599 cites a deleted test (TestHandshakeHandlerIsNotWiredIntoAnyMux) as a live… (todo)
- [RELAY-24-BLOCKER-EGRESS-ATTEST](../RELAY-24-BLOCKER-EGRESS-ATTEST--3334677e/task.md) — RELAY-24-BLOCKER-EGRESS-ATTEST: no bus can ISSUE an origin attestation for its own agents… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
