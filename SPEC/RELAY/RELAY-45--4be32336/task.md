# RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal

| Field | Value |
| --- | --- |
| Public id | `4be32336-5a48-410e-a70c-62ea154a6196` |
| Key | RELAY-45 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | critical-path, federation, mtls, security |
| Created | 2026-08-14T11:21:59.378533+00:00 |
| Updated | 2026-08-14T16:09:29.464465+00:00 |
| Completed | 2026-08-14T16:09:29.464449+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh 'go test -race -run "TestInboundPeerPrincipal|TestPeerClientCertBinding" ./internal/httpapi ./internal/relay ./cmd/agent-bus'
```

## Status note

CODE-COMPLETE within this agent's file-ownership boundary (internal/relay/peerstore.go, internal/httpapi/peerprincipal.go, the Options.PeerPrincipals/Server.peerPrincipals wiring in internal/httpapi/server.go) and fully gated: security PASS; reviewer CHANGES-REQUIRED round 1, re-verified round 2 and confirmed complete within that boundary (the one blocking item, a false gate-status claim in AGENT_LOG.md, has since been fixed). What is NOT delivered is acceptance criteria 2 and 5's operator/CLI half: the CLI surface for this capability lives in cmd/agent-bus/peer.go, which was outside this task's file-ownership boundary, so no flag anywhere writes BusTrust.PeerClientTLSCertFingerprint and no CONTRACTS-CLI.md/AGENT_PROTOCOL.md entry exists for it. Invariant 7 requires a capability's CLI subcommand and AGENT_PROTOCOL.md entry to ship in the SAME task, so RELAY-45 cannot be marked done until that follow-up lands. Say plainly: NO HTTP route is mounted for this gate anywhere in the server (that is RELAY-20's job), NO server constructs a PeerStore for the HTTP layer (that is RELAY-24's job), and NO operator can currently write the inbound peer-certificate binding through any supported client -- the only path to it today is the internal relay.PutTrust Go API. Follow-up filed as RELAY-45-FU-CLI, which blocks this task's completion (and also blocks RELAY-20).

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
- [MTLS-CROSSCHECK](../../MTLS/MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (in_progress)
- [MTLS-RELAYGUARD-FU-BUSCERTPOOL](../../MTLS/MTLS-RELAYGUARD-FU-BUSCERTPOOL--c873482f/task.md) — MTLS-RELAYGUARD-FU-BUSCERTPOOL: relay client-cert verification must not build a CertPool… (todo)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45-FU-CLI](../RELAY-45-FU-CLI--b9d645be/task.md) — RELAY-45-FU-CLI: operator CLI surface for the inbound peer client-certificate binding (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [73b29060-f595-4f4d-90a9-3f13d231b909](../../CONTEXT/Spec-Server-warn-on-likely-duplicate-task-titles-at-crea--73b29060/task.md) — Spec Server: warn on likely-duplicate task titles at create/claim-next time (todo)
- [MTLS-CROSSCHECK](../../MTLS/MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (in_progress)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC](../RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC--7126f08b/task.md) — RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC: Record the relayed audit content-hash decisi… (done)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45-FU-CLI](../RELAY-45-FU-CLI--b9d645be/task.md) — RELAY-45-FU-CLI: operator CLI surface for the inbound peer client-certificate binding (todo)
- [RELAY-45-FU-ROTATION](../RELAY-45-FU-ROTATION--ec1c1d7c/task.md) — RELAY-45-FU-ROTATION: inbound peer client-certificate binding has no rollover overlap win… (todo)
- [RELAY-46](../RELAY-46--eb5c3312/task.md) — RELAY-46: NextHopTLSCertFingerprint should be a bounded list, not a scalar, for peer-cert… (todo)
- [de0fc1df-a948-4b44-95a4-4b9d01cab267](../../TOOLING/DECISIONS.md-HTML-comment-section-fences-are-imbalanced--de0fc1df/task.md) — DECISIONS.md HTML-comment section fences are imbalanced (6 BEGIN / 8 END) -- introduced b… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
