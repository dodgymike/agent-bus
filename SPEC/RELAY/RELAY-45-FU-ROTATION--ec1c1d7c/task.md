# RELAY-45-FU-ROTATION: inbound peer client-certificate binding has no rollover overlap window (scalar field)

| Field | Value |
| --- | --- |
| Public id | `ec1c1d7c-1d18-4bc7-a692-e9bc08876a61` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | relay |
| Section | backlog |
| Tags | federation, mtls, rollover |
| Created | 2026-08-14T12:43:06.611978+00:00 |
| Updated | 2026-08-14T12:43:06.611978+00:00 |
| Completed | — |

## Description

RELAY-45 (4be32336-5a48-410e-a70c-62ea154a6196) added BusTrustRecord.PeerClientTLSCertFingerprint (JSON peer_client_tls_cert_sha256) as a SCALAR: exactly one pinned inbound client-certificate fingerprint per adjacent bus. Invariant 11 requires 'Rotation serves TWO certificates during rollover and must never require re-enrolment' for this bus's own listener certificate, and the same reasoning applies to a peer bus rotating the client certificate it presents to us: with a single pinned fingerprint, the moment the peer bus rotates its client certificate, every inbound request it makes is refused until the operator synchronously updates the binding -- a forced, coordinated outage on a routine certificate rotation, not a graceful rollover.

This is the SAME shape of gap RELAY-46 (eb5c3312-8b45-4f97-850c-bd9f77c553ce) tracks for PeerRecord.NextHopTLSCertFingerprint, but it is NOT the same field: RELAY-46 covers the OUTBOUND next-hop pin on route records (this bus dialling a peer, keyed by destination address); this task covers the INBOUND peer-certificate binding on bus trust records (a peer bus authenticating to us, keyed by the adjacent bus principal). RELAY-45's own description required the new field to be 'distinct in Go names, JSON/on-disk shape, CLI flags, and docs' from NextHopTLSCertFingerprint precisely so the two lookup directions are never conflated -- so the rollover fix for each belongs in its own atomic task rather than one task touching both record types. Checked RELAY-46 before filing this: its scope, PeerRecord/route-record specific, is not the right home for the inbound gap; a short cross-reference note has been left there instead of merging the two.

SCOPE: change BusTrustRecord.PeerClientTLSCertFingerprint from a scalar to a bounded list (mirror BusTrustRecord.SigningKeys / MaxPinnedBusSigningKeys's existing shape and its stated two-participant rollover-window rationale; confirm during implementation whether the same cap of 2 applies here or whether a different bound is warranted since this is per-adjacent-bus rather than per-route). Update relay.ParsePeerClientTLSFingerprint's call sites, PutTrust's encode/decode and its uniqueness-collision guard (ErrPeerClientCertAlreadyBound must still refuse binding either of the two rollover fingerprints to a SECOND bus id), PeerStore.InboundPeerPrincipal's resolution (any of the pinned fingerprints must resolve to the same principal), the CLI flag delivered by RELAY-45-FU-CLI (repeatable or otherwise expressing up to 2 values), and CONTRACTS-ONDISK.md/CONTRACTS-CLI.md documentation. Preserve the keyed-by-bus-principal, fingerprint-first-with-enforced-uniqueness discipline RELAY-45 established -- this task changes the CARDINALITY of the pin, not its keying direction.

DEPENDS ON RELAY-45-FU-CLI landing first (this changes the field/flag that task introduces) and is non-blocking hardening, not on the RELAY-41 -> RELAY-45 -> RELAY-20 -> RELAY-21 critical path: a scalar is a legitimate v1 for that path to land on; this task exists so the cardinality gap is tracked rather than forgotten, matching RELAY-46's own non-blocking framing for the outbound side.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [RELAY-46](../RELAY-46--eb5c3312/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-45-FU-CLI](../RELAY-45-FU-CLI--b9d645be/task.md) — RELAY-45-FU-CLI: operator CLI surface for the inbound peer client-certificate binding (done)
- [RELAY-46](../RELAY-46--eb5c3312/task.md) — RELAY-46: NextHopTLSCertFingerprint should be a bounded list, not a scalar, for peer-cert… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
