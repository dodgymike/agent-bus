# RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal

| Field | Value |
| --- | --- |
| Public id | `4be32336-5a48-410e-a70c-62ea154a6196` |
| Key | RELAY-45 |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | critical-path, federation, mtls, security |
| Created | 2026-08-14T11:21:59.378533+00:00 |
| Updated | 2026-08-14T11:42:38.615541+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh \"go test -race -run \\"TestInboundPeerPrincipalBinding|TestInboundPeerPrincipalRejectsWrongAndUnboundCert|TestInboundPeerPrincipalRouteForIsolation\\" ./internal/httpapi ./internal/relay ./cmd/agent-bus\"
```

## Status note

SPURIOUS EDGE FLAGGED 2026-08-14 (spec-keeper, per coordinator's priority-inversion check during RELAY-25 closure computation): a real `blocks` relation exists with MTLS-RELAYGUARD as source and RELAY-45 as target (created 2026-08-14T11:26:04 by codex-1). This produces a P2-blocks-P0 priority inversion feeding the P0 epic deliverable RELAY-25, which is a real smell -- but on inspection THE EDGE ITSELF IS WRONG, not the P2 priority. Evidence, from both tasks' own descriptions, neither of which I authored: (1) MTLS-RELAYGUARD's own description states "BLOCKS: RELAY-1 (9bc9d6c4), RELAY-2 (654140d7)" -- not RELAY-45 -- and explicitly disclaims the overlap: "This task covers the outbound client credential and transport guard. It does NOT establish the inbound fingerprint -> adjacent bus principal mapping; that distinct record/lookup... is owned by RELAY-45 (4be32336), which blocks RELAY-20." (2) RELAY-45's own DEPENDENCIES section lists only MTLS-CLIENTAUTH and RELAY-41 as what it depends on -- not MTLS-RELAYGUARD. Both tasks' own prose describe them as adjacent/parallel work in the same certificate area, not a blocking dependency. CONCLUSION: the P2 priority on MTLS-RELAYGUARD is correct for its actual scope (RELAY-1/RELAY-2); the edge to RELAY-45 is the defect. There is no DELETE on the relations API, so the edge is permanent -- do not treat it as load-bearing. Anyone computing RELAY-25's critical path should disregard the MTLS-RELAYGUARD->RELAY-45 edge specifically; RELAY-45's real blockers are MTLS-CLIENTAUTH (done) and RELAY-41 (done).

## Description

Security blocker discovered during RELAY-25 preflight (2026-08-14): RELAY-20 must resolve an authenticated inbound peer-bus client certificate to exactly one adjacent bus principal, but no existing task owns that binding. MTLS-BIND binds an agent client certificate to an agent id and RELAY-41 stores an OUTBOUND server-certificate pin on route records; neither establishes inbound peer-bus identity. The existing `NextHopTLSCertFingerprint` MUST NOT be reused for this purpose: `-route-for` intentionally duplicates one next-hop fingerprint across records for different destination bus_ids, so reading those records as an inbound principal map is ambiguous and can authorize busB as busC.

SCOPE / ACCEPTANCE:
1. Define and implement one durable, operator-configured credential field/record keyed by the ADJACENT bus principal (for example a client-certificate fingerprint on the bus trust record keyed by bus_id, or an equivalent dedicated principal record). It must be distinct in Go names, JSON/on-disk shape, CLI flags, and docs from `NextHopTLSCertFingerprint`; no lookup may read route records, base_url, route-for destinations, signing keys, CN/SAN/Subject, or an attestation origin to infer the transport principal.
2. Wire the compiled CLI/operator configuration surface (`--json` and stable errors) and AGENT_PROTOCOL/CONTRACTS documentation. Validate exact lowercase SHA-256-over-leaf-DER spelling, reject empty/all-zero/malformed values before durable write, reject duplicate fingerprint bindings to different adjacent bus ids, and make replay/restart preserve the binding. Removed/tombstoned principals must carry no live credential.
3. Wire authenticated inbound peer routes so the presented `r.TLS.PeerCertificates[0]` leaf fingerprint (after the existing TLS parse/proof-of-possession path) resolves through the dedicated adjacent-principal binding and yields the fully-qualified peer principal. Missing certificate, unknown fingerprint, stale/revoked/removed binding, malformed/extra-chain misuse, and a valid certificate configured for a different bus must all fail closed without route handler execution or principal fallback. A session/agent credential must not be accepted as a peer-bus credential, and a peer credential must not be accepted as an agent credential.
4. Add race-safe tests and a live/compiled-CLI proof covering A<-B admission, unknown/wrong/revoked/no-cert refusals, duplicate-fingerprint collision refusal, restart persistence, and the route-for ambiguity regression: B and C destination records may legitimately share B's outbound next-hop pin, but an inbound B client certificate must resolve only to B. Prove that changing `NextHopTLSCertFingerprint` or adding a route-for record cannot alter inbound principal resolution.
5. Record the boundary in CONTRACTS-CLI.md, CONTRACTS-HTTP.md, CONTRACTS-ONDISK.md, AGENT_PROTOCOL.md, and DECISIONS.md as required by the owning documentation/integration tasks. Do not broaden ClientAuth policy here; consume MTLS-CLIENTAUTH's requested certificate and preserve invariant 11's no-CA/fingerprint-only policy.

SECURITY NEGATIVES (must be explicit tests): fingerprint-first lookup over route records; base_url-first lookup; bus signing-key match treated as TLS identity; CN/SAN/Subject/Issuer/SerialNumber identity; client certificate accepted without exact binding; one fingerprint silently rebound; absent/unqualified/agent ids accepted as bus principals; a non-adjacent origin/attestation allowed to impersonate the adjacent TLS hop; or a route-for destination changing the principal.

DEPENDENCIES: depends on MTLS-CLIENTAUTH (cc9558a8) for a presented client leaf and on RELAY-41 (05253c80) only for the separate outbound pin semantics/anti-confusion contract. This task BLOCKS RELAY-20 (701dc54d), and therefore transitively blocks RELAY-21, RELAY-24, and RELAY-25's real federation proof. It is related to MTLS-BIND (b6378bda) but is not a substitute for agent certificate binding or MTLS-CROSSCHECK. No implementation is authorized by this filing; the normal planner -> spec-keeper -> implementer -> test -> reviewer -> security -> docs -> integrator chain applies.

--- MERGED FROM RELAY-44 (cec27a90-c561-4836-abd5-f27310ee25b6), reconciled 2026-08-14 ---
RELAY-44 described this same gap, filed independently 39 seconds after this task (confirmed via
the reservations event log: spec-keeper reserved task-key-RELAY=44 at 11:20:44, codex-1 reserved
task-key-RELAY=45 at 11:20:45 -- two agents, not one retrying). RELAY-44 is now `superseded` in
favour of this task (earlier filing, and this task's scope is not narrower). Content unique to
RELAY-44, preserved here:

PROVENANCE: this gap was independently corroborated by the RELAY-41 review, which refuted a premise
written into both RELAY-41's and RELAY-20's task records -- that RELAY-41's NextHopTLSCertFingerprint
field could supply RELAY-20's inbound peer identity. It cannot: CONTRACTS-ONDISK.md:1504-1527 (the
"WHICH CERTIFICATE, IN WHICH DIRECTION -- and DO NOT INVERT IT" block) states plainly that field pins
the certificate presented by the hop THIS bus DIALS (outbound, server-side, keyed to an address) and
is explicitly NOT a source of inbound peer identity -- using it that way is exactly the peer-principal
spoof the doc forbids (fingerprint->bus_id is ambiguous by construction: one fingerprint legitimately
sits on N records with N different bus_ids via -route-for). Both RELAY-41 and RELAY-20 have had their
descriptions/status_notes corrected to point at this task (see their own notes) rather than at RELAY-41
or at superseded RELAY-44.

MTLS-BIND FORBIDDEN-IMPLEMENTATION PRECEDENT (elaborating point 1's "distinct in Go names/JSON/CLI"
requirement above): MTLS-BIND (b6378bda) is the equivalent binding at AGENT/enrolment scope
(client-cert fingerprint -> server-minted AgentID on auth.RosterEntry) and its own description spells
out WHY chain/CertPool verification is forbidden there, not just exact-match preferred: client/clientcert.go
(~line 550-620) sets the client-cert template's IsCA:false and no KeyUsageCertSign deliberately -- a
CertPool entry would be a TRUSTED ROOT, and any agent could mint a certificate for any name that chains
to itself and validates, becoming a CA for the whole bus. The same trap applies unchanged at bus scope
for this task (see also MTLS-RELAYGUARD-FU-BUSCERTPOOL, c873482f, for the bus's own dual-usage
certificate version of it) -- exact SHA-256-over-leaf-DER match via buscert.FingerprintOf only, never a
CertPool, matching this task's own point 2 ("exact lowercase SHA-256-over-leaf-DER spelling").

DESIGN INFERENCE, UNVERIFIED -- offered for the implementer/reviewer to confirm or correct, not a
settled decision: CONTRACTS-ONDISK.md:1514-1521 states the sound direction for the OUTBOUND next-hop
table is address-first ("I am dialling this address -- does the certificate I was served match this
record's pin?"), forbidding fingerprint-first lookup there. The inbound analogue this task needs is
very likely the same shape rotated 180 degrees -- CLAIMED-IDENTITY-FIRST, not fingerprint-first: the
peer's bus_id is claimed some other way (an attested envelope field or protocol-level identity
assertion, NOT the TLS certificate itself, which would be circular), and the lookup takes THAT claimed
bus_id to its bound fingerprint and compares against what TLS actually presented -- the same shape as
invariant 11's existing rule that a session token's connection must cross-check against the client
certificate's owner, applied at bus-to-bus scope instead of agent scope. This task's own acceptance
criteria already forbid the concrete bad inputs (route records, base_url, signing keys, CN/SAN/Subject,
attestation origin); this paragraph names the POSITIVE pattern those prohibitions are protecting, for
whoever designs the actual lookup.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md)
- **blocked by** [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md)
- **blocked by** [RELAY-41](../RELAY-41--05253c80/task.md)
- **blocks** [RELAY-20](../RELAY-20--701dc54d/task.md)
- **supersedes** [RELAY-44](../RELAY-44--cec27a90/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-BIND](../../MTLS/MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (todo)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CROSSCHECK](../../MTLS/MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (todo)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [MTLS-RELAYGUARD-FU-BUSCERTPOOL](../../MTLS/MTLS-RELAYGUARD-FU-BUSCERTPOOL--c873482f/task.md) — MTLS-RELAYGUARD-FU-BUSCERTPOOL: relay client-cert verification must not build a CertPool… (todo)
- [RELAY-1](../RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)
- [RELAY-2](../RELAY-2--654140d7/task.md) — RELAY-2: Message relay + ongoing roster sync across peers (done)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (todo)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (todo)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [73b29060-f595-4f4d-90a9-3f13d231b909](../../CONTEXT/Spec-Server-warn-on-likely-duplicate-task-titles-at-crea--73b29060/task.md) — Spec Server: warn on likely-duplicate task titles at create/claim-next time (todo)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (todo)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-46](../RELAY-46--eb5c3312/task.md) — RELAY-46: NextHopTLSCertFingerprint should be a bounded list, not a scalar, for peer-cert… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
