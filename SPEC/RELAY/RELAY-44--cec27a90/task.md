# RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate to a peer principal

| Field | Value |
| --- | --- |
| Public id | `cec27a90-c561-4836-abd5-f27310ee25b6` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | superseded |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | federation, mtls, critical-path |
| Created | 2026-08-14T11:22:38.387458+00:00 |
| Updated | 2026-08-14T11:27:24.222538+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestInboundPeerCertBindingIsClaimedIdentityFirstNotFingerprintFirst ./internal/relay
```

## Status note

SUPERSEDED 2026-08-14 by RELAY-45 (4be32336-5a48-410e-a70c-62ea154a6196). This task and RELAY-45 were filed independently within 39 seconds of each other (this task by spec-keeper acting on a coordinator brief; RELAY-45 by agent codex-1 during RELAY-25 preflight -- confirmed via the reservations event log, not a retry). RELAY-45 was preferred as the survivor per coordinator decision (earlier filing, and its scope is not narrower -- it has a fuller acceptance-criteria/security-negatives list). RELAY-45's description has been merged with this task's unique content (the RELAY-41-review refuted-premise provenance and CONTRACTS-ONDISK.md:1504-1527 citation). CAUTION FOR ANY RENDERER: this task's own `blocks`/`blocked-by` relations (to RELAY-20, RELAY-41, MTLS-CLIENTAUTH) are now STALE -- there is no DELETE on the relations API, so they could not be removed. Do not treat them as live; the current edges are on RELAY-45, not here.

## Description

FILED 2026-08-14 because this record does not exist and RELAY-20 genuinely needs it. It exists on the backlog because the RELAY-41 review REFUTED the assumption -- written into both RELAY-41's and RELAY-20's task records -- that RELAY-41's NextHopTLSCertFingerprint field could serve this purpose. See CONTRACTS-ONDISK.md:1504-1527 (the "WHICH CERTIFICATE, IN WHICH DIRECTION -- and DO NOT INVERT IT" block): that field pins the certificate presented by the hop THIS bus DIALS (outbound, server-side, keyed to an address) and is explicitly NOT a source of inbound peer identity. RELAY-20 needs the mirror-image record: on a connection made TO us, bind the peer's presented CLIENT certificate (r.TLS.PeerCertificates[0], available once MTLS-CLIENTAUTH lands) to a peer principal (bus_id). Nothing in RELAY-41 or MTLS-CLIENTAUTH establishes that binding -- this task is the missing half.

CRITICAL DESIGN CONSTRAINT, carried over from the RELAY-41 review and NOT yet re-verified for this direction -- confirm it explicitly during design, do not assume: CONTRACTS-ONDISK.md:1514-1521 forbids a FINGERPRINT-FIRST lookup ("fingerprint -> bus id is forbidden") for the outbound next-hop table, because one fingerprint can legitimately sit on N records with N different bus_ids (a -route-for entry). The stated sound direction there is address-first: "I am dialling this address -- does the certificate I was served match this record's pin?" The inbound analogue is almost certainly the same shape rotated 180 degrees -- CLAIMED-IDENTITY-FIRST, not fingerprint-first: the peer's bus_id is claimed some other way (an attested envelope field, a protocol-level identity assertion -- NOT the TLS certificate itself, which would be circular), and the server looks up THAT claimed bus_id's bound fingerprint and compares it against what TLS actually presented on this connection -- the same shape as invariant 11's existing rule that a session token's connection must cross-check against the client certificate's owner, applied at bus-to-bus scope instead of agent scope. Do NOT implement a global fingerprint->bus_id map to identify an inbound peer from its certificate alone -- that reintroduces exactly the ambiguity CONTRACTS-ONDISK.md just spent a paragraph forbidding, one level up the stack. This paragraph is spec-keeper's inference from the existing outbound rule, not yet a reviewed decision -- the implementer and reviewer must confirm or correct it as part of this task's design, not treat it as settled.

PRECEDENT TO FOLLOW: MTLS-BIND (b6378bda) does the equivalent binding at AGENT/enrolment scope (client-cert fingerprint -> server-minted AgentID on auth.RosterEntry, exact-match only, refuse rather than overwrite a fingerprint already bound to a different identity, chain/CertPool verification explicitly FORBIDDEN per client/clientcert.go's IsCA:false design -- see MTLS-BIND's own description for the full FORBIDDEN IMPLEMENTATION rationale, which applies here unchanged: this is the same trap at bus scope, see also MTLS-RELAYGUARD-FU-BUSCERTPOOL, c873482f). RELAY-44 is the same discipline at BUS/peer-principal scope: exact SHA-256-over-DER match via buscert.FingerprintOf (never a second hashing construction -- CONTRACTS-ONDISK.md:1529-1535), refuse rather than silently overwrite a fingerprint already bound to a different peer, never CertPool/chain verification.

DEFINITION OF DONE (to be refined during design/implementation -- this is the floor, not the ceiling):
1. A durable record binds an inbound peer's expected client-certificate fingerprint to its peer principal (bus_id), set via `agent-bus peer add` (or the appropriate existing command) alongside the existing NextHopTLSCertFingerprint flag from RELAY-41 -- probably the SAME PeerRecord, as a second field, since both concern the same directly-adjacent-peer relationship the operator is already describing in one `peer add` call. Confirm this placement during design; do not assume without checking peerstore.go's existing shape.
2. A test PROVES the lookup direction is claimed-identity-first (or whatever direction design settles on), not fingerprint-first -- mirroring RELAY-41's TestPeerRecordTLSFingerprintIsKeyedToTheNextHop discipline: construct a case where one fingerprint would be ambiguous under the forbidden direction and assert the implementation does not take it.
3. RELAY-20 can resolve r.TLS.PeerCertificates[0] -> this binding record -> PeerPrincipal, and its own DoD/status_note is updated to reflect that this is the record it consumes (not RELAY-41's field).
4. CONTRACTS-ONDISK.md documents the new record/field with the same rigor as the NextHopTLSCertFingerprint section it sits beside -- including, explicitly, which direction the lookup runs and why, so a future reader gets the same anti-inversion warning this task exists because of.
5. CONTRACTS-CLI.md documents any new flag.

DEPENDENCIES (real relations wired via POST .../relations, not prose -- see this task's notes for what was wired and when): DEPENDS ON RELAY-41 (05253c80) and MTLS-CLIENTAUTH (cc9558a8) -- RELAY-41 for the sibling on-disk-encoding precedent this task should reuse on the same PeerRecord type, MTLS-CLIENTAUTH for there to be a client certificate on the wire to bind in the first place. BLOCKS RELAY-20 (701dc54d) -- RELAY-20 cannot resolve an inbound peer principal without this record; its own status_note and description must be corrected to point here instead of at RELAY-41 (see RELAY-20's own task notes for that correction).

OWNERSHIP NOTE: this task's exact file/record placement is NOT pre-decided (unlike RELAY-41, which pinned exact file ownership) -- the design constraint above (claimed-identity-first, not fingerprint-first) is the one thing that must not be re-litigated without cause; where the field physically lives is for design.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-BIND](../../MTLS/MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-RELAYGUARD-FU-BUSCERTPOOL](../../MTLS/MTLS-RELAYGUARD-FU-BUSCERTPOOL--c873482f/task.md) — MTLS-RELAYGUARD-FU-BUSCERTPOOL: relay client-cert verification must not build a CertPool… (todo)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [73b29060-f595-4f4d-90a9-3f13d231b909](../../CONTEXT/Spec-Server-warn-on-likely-duplicate-task-titles-at-crea--73b29060/task.md) — Spec Server: warn on likely-duplicate task titles at create/claim-next time (todo)
- [RELAY-21-FU-DOCGAP4](../RELAY-21-FU-DOCGAP4--9972d0ed/task.md) — RELAY-21-FU-DOCGAP4: internal/relay/doc.go known-gaps item 4 falsely claims forward-only-… (todo)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-46](../RELAY-46--eb5c3312/task.md) — RELAY-46: NextHopTLSCertFingerprint should be a bounded list, not a scalar, for peer-cert… (todo)
- [SPEC-API-LIST-SILENT-TRUNCATION](../../UNASSIGNED/SPEC-API-LIST-SILENT-TRUNCATION--82f35b73/task.md) — Task-list API silently truncates at 200 with no total, no next and no working pagination… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
