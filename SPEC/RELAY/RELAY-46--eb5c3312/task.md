# RELAY-46: NextHopTLSCertFingerprint should be a bounded list, not a scalar, for peer-certificate rollover

| Field | Value |
| --- | --- |
| Public id | `eb5c3312-8b45-4f97-850c-bd9f77c553ce` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | relay |
| Section | backlog |
| Tags | federation, mtls, rotation, non-blocking |
| Created | 2026-08-14T11:29:06.173907+00:00 |
| Updated | 2026-08-14T11:29:39.318689+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestPeerRecordNextHopFingerprintSurvivesRolloverWindow ./internal/relay
```

## Status note

OPERATOR-VISIBLE NOTE (spec-keeper, self-reported error, 2026-08-14): same mistake as RELAY-44 (cec27a90) -- title carries RELAY-46 and reservation task-key-RELAY value 46 was minted for it, but the `key` field was again omitted from the creation payload and is now null and unfixable (no PATCH path for key). Resolve this task by public_id (eb5c3312-8b45-4f97-850c-bd9f77c553ce), not by key. Do not create a second RELAY-46.

## Description

Non-blocking follow-up from the RELAY-41 review, filed 2026-08-14.

PeerRecord's next_hop_tls_cert_sha256 (CONTRACTS-ONDISK.md ~line 1399) is currently a SCALAR: one fingerprint per route record. CONTRACTS-ONDISK.md:1402-1408 already establishes the precedent this should follow, for BusTrustRecord.bus_signing_keys: 'a LIST, not a scalar, and that is load-bearing rather than generous... MORE THAN ONE KEY IS RETURNED ONLY DURING A SIGNING-KEY ROLLOVER WINDOW... A scalar would force a federation-wide outage on every signing-key rotation. MaxPinnedBusSigningKeys = 2 is derived from that sentence: a rollover has exactly two participants, the outgoing key and the incoming one.'

The same reasoning applies unchanged to the next-hop TLS certificate fingerprint: invariant 11 already requires 'Rotation serves TWO certificates during rollover and must never require re-enrolment' for the bus's own listener certificate. A peer bus rotating ITS TLS certificate hits the exact same problem on the DIALLING side that a scalar bus_signing_keys would have hit on the verification side: with a single pinned fingerprint, the operator must synchronously update every OTHER bus's route record the instant the peer's certificate rotates, or every non-adjacent hop through it starts refusing the new certificate -- a coordinated federation-wide cutover on every routine TLS rotation, which is precisely the outage MaxPinnedBusSigningKeys=2 exists to avoid for signing keys.

SCOPE: change next_hop_tls_cert_sha256 from a scalar to a bounded list (mirroring MaxPinnedBusSigningKeys's shape and its stated rollover-window rationale -- confirm during implementation whether the same cap of 2 applies, or whether next-hop fingerprints warrant a different bound given they are per-route rather than per-bus). Update PeerRecord's encode/decode (internal/relay/peerstore.go), `agent-bus peer add`'s flag (repeatable, matching the existing repeatable-or-single flag RELAY-41 added), `agent-bus peer list` output, and CONTRACTS-ONDISK.md/CONTRACTS-CLI.md documentation. Preserve RELAY-41's next-hop-keyed (never destination-keyed) discipline and the address-first (never fingerprint-first) lookup direction -- this task changes the CARDINALITY of the pin, not its KEYING.

NON-BLOCKING: this is a hardening/correctness follow-up, not on the critical path RELAY-41 -> RELAY-45 -> RELAY-20 -> RELAY-21 (RELAY-45 reconciled from a RELAY-44/RELAY-45 duplicate filing, 2026-08-14). A scalar field is a legitimate v1 for that path to land on; this task exists so the cardinality gap is tracked rather than forgotten.

DEPENDS ON RELAY-41 (05253c80) landing first -- this changes the field RELAY-41 introduces.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [RELAY-45-FU-ROTATION](../RELAY-45-FU-ROTATION--ec1c1d7c/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [73b29060-f595-4f4d-90a9-3f13d231b909](../../CONTEXT/Spec-Server-warn-on-likely-duplicate-task-titles-at-crea--73b29060/task.md) — Spec Server: warn on likely-duplicate task titles at create/claim-next time (todo)
- [RELAY-21-FU-DOCGAP4](../RELAY-21-FU-DOCGAP4--9972d0ed/task.md) — RELAY-21-FU-DOCGAP4: internal/relay/doc.go known-gaps item 4 falsely claims forward-only-… (todo)
- [RELAY-45-FU-ROTATION](../RELAY-45-FU-ROTATION--ec1c1d7c/task.md) — RELAY-45-FU-ROTATION: inbound peer client-certificate binding has no rollover overlap win… (todo)
- [SPEC-API-LIST-SILENT-TRUNCATION](../../UNASSIGNED/SPEC-API-LIST-SILENT-TRUNCATION--82f35b73/task.md) — Task-list API silently truncates at 200 with no total, no next and no working pagination… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
