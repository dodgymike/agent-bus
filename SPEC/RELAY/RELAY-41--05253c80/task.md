# RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-bus peer add\`

| Field | Value |
| --- | --- |
| Public id | `05253c80-88e0-4416-9f5c-f3accff9bea8` |
| Key | RELAY-41 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | critical-path, vacuous-today, federation |
| Created | 2026-08-14T09:33:37.523155+00:00 |
| Updated | 2026-08-14T11:35:14.070617+00:00 |
| Completed | 2026-08-14T11:35:14.070598+00:00 |

## Proof command

```sh
go test -race -run 'TestPeerRecordTLSFingerprintIsKeyedToTheNextHop|TestPeerAddTLSFingerprintRoundTripsOnDisk' ./internal/relay ./cmd/agent-bus
```

## Status note

REFUTED PREMISE, corrected 2026-08-14: this task's description previously asserted it blocks RELAY-20 because RELAY-20 would resolve r.TLS.PeerCertificates[0] -> NextHopTLSCertFingerprint -> peer store -> PeerPrincipal. The RELAY-41 review refuted that -- CONTRACTS-ONDISK.md:1504-1527 now states this field pins the OUTBOUND next-hop certificate for dialling, and is explicitly NOT a source of inbound peer identity; using it that way is the exact peer-principal spoof the doc forbids. RELAY-20 needs a separate inbound peer-certificate binding record -- filed as RELAY-44 (cec27a90-c561-4836-abd5-f27310ee25b6). This task's description DEPENDENCIES section has been corrected to match; it now blocks RELAY-44, not RELAY-20 directly. See RELAY-44's own description for the corrected chain.

## Description

Operator decision, 2026-08-14: BUILD THE CREDENTIAL. Sequence is MTLS-CLIENTAUTH (cc9558a8) -> THIS TASK -> RELAY-20 mounts as originally scoped. RELAY-20 was attempted on 2026-08-14 and the agent correctly wrote NO CODE, because there is no peer credential on the wire at all: cmd/agent-bus/tlslisten.go:109 pins ClientAuth: tls.NoClientCert, internal/relay/client.go:200-204 presents no credential when dialling, and neither PeerRecord {bus_id, config_seq, state, base_url} nor BusTrustRecord (pinned bus SIGNING keys only) carries anything presentable-and-verifiable on a CONNECTION. A pinned signing key authenticates an origin attestation INSIDE an envelope; it never authenticates the hop. This task adds the missing durable half of the hop credential.

WHAT TO BUILD. A TLS certificate fingerprint (SHA-256 over the peer's certificate DER, matching the MTLS-BIND/MTLS-CLIENTAUTH fingerprint discipline -- exact match, never chain/pool verification, see the FORBIDDEN IMPLEMENTATION paragraph on cc9558a8) stored on the route record next to BaseURL, and set by a new repeatable-or-single flag on `agent-bus peer add` alongside -url.

THE CRITICAL DESIGN CONSTRAINT, AND IT IS THE WHOLE TASK. Key the fingerprint off THE RECORD THAT CARRIES -url -- the NEXT HOP -- and NEVER off the record's bus id. cmd/agent-bus/peer.go:68-75 and CONTRACTS-CLI.md:392 both record why: for a -route-for entry THE ADDRESS BELONGS TO A DIFFERENT BUS THAN THE RECORD'S BUS ID (`peer add -bus-id busB -url https://b:8443 -route-for busC` writes a record whose bus id is busC and whose address is busB's). A destination-keyed pin would pin busC's identity against a connection that terminates at busB, and WOULD BREAK EVERY NON-ADJACENT HOP -- which is the entire A->B->C topology this epic exists to serve. The identity on the wire is the NEXT HOP's; the record's bus id is the DESTINATION. They are not the same field and must not be collapsed into one.

NAMING TRAP: `peerFingerprint` / `idem.Fingerprint` already exist in internal/relay/peer.go and mean the IDEMPOTENCY fingerprint of a roster payload. The new field must be unmistakably distinct (e.g. TLSCertFingerprint / -tls-fingerprint), or a future reader will conflate a transport pin with a replay-protection digest.

DEFINITION OF DONE
1. PeerRecord (internal/relay/peerstore.go) carries a TLS certificate fingerprint field, set if and only if BaseURL is set -- validate() enforces that in BOTH directions (encode-side and decode-side), following the existing State/BaseURL precedent at peerstore.go:375-377.
2. peerRecordJSON gains the field; the on-disk shape round-trips through Encode/Decode and survives replay. A record without the field stays decodable (the field is additive and optional) OR, if the reviewer concludes the record version must bump, RESERVE the number from namespace `ondisk-format-version` -- never pick one by eyeballing.
3. A test PROVES the pin is next-hop-keyed, not destination-keyed: write `peer add -bus-id busB -url https://b:8443 -tls-fingerprint <fpB> -route-for busC`, then assert the busC ROUTE record carries fpB (busB's, the next hop's) and NOT a fingerprint keyed to busC. This is the anti-regression test for the constraint above and it is not optional.
4. `agent-bus peer add` accepts the flag, refuses it without -url (there is no hop to pin, mirroring the existing -route-for-without-url refusal at cmd/agent-bus/peer.go:616-619), validates the encoding/length before any durable write, and elides untrusted text in errors per the file's elidePeerText discipline.
5. `agent-bus peer list` surfaces the fingerprint (both human and -json output).
6. CONTRACTS-CLI.md documents the flag and, at the :392 warning, states that this task is the resolution of it. CONTRACTS-ONDISK.md documents the new record field.

SCOPE AND BOUNDARY. Files owned: internal/relay/peerstore.go, internal/relay/peerstore_test.go (or a NEW test file named for this task, per the epic's per-file ownership rule), cmd/agent-bus/peer.go, cmd/agent-bus/peer_test.go, CONTRACTS-CLI.md, CONTRACTS-ONDISK.md. Do NOT touch internal/httpapi (that is RELAY-20), do NOT touch cmd/agent-bus/tlslisten.go (that is MTLS-CLIENTAUTH), do NOT edit DECISIONS.md (owned by RELAY-6 this wave).

DEPENDENCIES -- CORRECTED 2026-08-14, see status_note and this task's notes for the full refutation. The claim that used to sit here ("BLOCKS RELAY-20 -- RELAY-20 resolves r.TLS.PeerCertificates[0] -> fingerprint -> peer store -> PeerPrincipal, and cannot without this record") is REFUTED by the RELAY-41 review and by CONTRACTS-ONDISK.md:1504-1527: NextHopTLSCertFingerprint pins the OUTBOUND certificate of the next hop THIS bus DIALS -- it is explicitly NOT a source of inbound peer identity, and using it that way would be a peer-principal spoof (fingerprint->bus_id is ambiguous by construction: one fingerprint legitimately sits on N records with N different bus_ids via -route-for). RELAY-20 needs the peer's INBOUND CLIENT certificate (r.TLS.PeerCertificates[0] on a connection made TO us) bound to a peer principal via its OWN record -- that is RELAY-45 (4be32336-5a48-410e-a70c-62ea154a6196, filed 2026-08-14; note: this was independently also filed as RELAY-44, cec27a90-c561-4836-abd5-f27310ee25b6, which is now superseded by RELAY-45 -- see RELAY-45's own description for the reconciliation), not this task. This task still DEPENDS ON MTLS-CLIENTAUTH (cc9558a8) for the fingerprint to be CHECKABLE at runtime -- until ClientAuth flips off NoClientCert there is no certificate on the connection to compare against. THE RECORD CHANGE ITSELF IS INDEPENDENT and can land first: it is durable configuration, and nothing serves it yet either way. This task now BLOCKS RELAY-45 (the sibling on-disk-encoding precedent, survivor of the RELAY-44/RELAY-45 reconciliation), not RELAY-20 directly.

PROOF STATE AT FILING. `bash scripts/proof-check.sh` reports verdict=VACUOUS class=test exit=0 tests_run=0 empty_pkgs=2 -- both named tests are unwritten. VACUOUS, not RED: both packages COMPILE and their existing suites are green, so this is 'the test does not exist yet', which is the correct state for an unstarted task. It must be observed PASSING (tests_run>0) before completion.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md)
- **blocks** [RELAY-20](../RELAY-20--701dc54d/task.md)
- **blocks** [RELAY-45](../RELAY-45--4be32336/task.md)
- **blocks** [RELAY-44](../RELAY-44--cec27a90/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-BIND](../../MTLS/MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (done)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-6](../RELAY-6--0f7275b9/task.md) — RELAY-6: Record the FEDERATION deployment assumptions (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-KEY-IDENTITY](../../CONTEXT/CONTEXT-KEY-IDENTITY--73dec684/task.md) — CONTEXT-KEY-IDENTITY: Standardize task identity (public_id vs key) before SPEC/&lt;epic&gt;/&lt;ta… (todo)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-45-FU-ROTATION](../RELAY-45-FU-ROTATION--ec1c1d7c/task.md) — RELAY-45-FU-ROTATION: inbound peer client-certificate binding has no rollover overlap win… (todo)
- [RELAY-46](../RELAY-46--eb5c3312/task.md) — RELAY-46: NextHopTLSCertFingerprint should be a bounded list, not a scalar, for peer-cert… (todo)
- [RELAY-6](../RELAY-6--0f7275b9/task.md) — RELAY-6: Record the FEDERATION deployment assumptions (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
