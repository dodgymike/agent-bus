# MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED agent id -- the invite is what authorises the binding

| Field | Value |
| --- | --- |
| Public id | `b6378bda-20ed-4c55-8189-2c28054085e3` |
| Key | MTLS-BIND |
| Epic | [MTLS](../epic.md) |
| Status | in_progress |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:50.604087+00:00 |
| Updated | 2026-08-14T19:40:06.967131+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -count=1 -run 'TestEnrolBindsThePresentedCertificate|TestEnrolOverHTTPBindsTheConnectionCertificate|TestOnlyTheLeafIsConsidered|TestCertFingerprintOwnerAmbiguousArmRefuses|TestCertFingerprintOwnerRefusesTheZeroFingerprint|TestRefusedCertBindingBurnsNoAgentIDSuffix|TestEnrolRefusesWhenTheCertificateResolvesAmbiguously|TestCertificateDoesNotInfluenceTheMintedAgentID|TestExpiredClientCertificateIsIgnored|TestMemoryRosterRefusesAFingerprintBoundToAnotherAgent|TestWALRosterRefusesAFingerprintBoundToAnotherAgentBeforeWriting' ./internal/auth/ ./internal/httpapi/
```

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-CLIENTAUTH, ENROL-SHAPE, INVITE-GATE | BLOCKS: MTLS-CROSSCHECK, AUTH-3 (d53e3b21)

DECISIONS.md:1146 -- the invite authorises binding a new client certificate to a new agent id; the two happen together, not as two independent gates either of which alone would suffice. Populates the fingerprint field that INVITE-STORE and ENROL-SHAPE reserved, on auth.RosterEntry (internal/auth/roster.go:16-37). INVARIANT 1: the certificate supplies a fingerprint and NOTHING else -- it must not influence the agent id, the name, or the suffix, which are minted by ids.AgentIDMinter.Mint (internal/ids/agentmint.go:360). auth.Roster.Put already refuses a duplicate AgentID rather than overwriting (internal/auth/roster.go:105-107); the same refuse-never-overwrite rule must apply to a fingerprint already bound to a different agent. ORDERING: land before AUTH-3 (d53e3b21, durable roster) or AUTH-3 encodes a durable record that immediately needs migrating.

FORBIDDEN IMPLEMENTATION (security-testing finding, 2026-08-07): the binding here must stay an EXACT-MATCH comparison of the presented certificate's fingerprint (SHA-256 over the DER) against the fingerprint stored on auth.RosterEntry -- never chain verification against an x509.CertPool built from enrolled agents' certificates. client/clientcert.go (~line 550-620) explains why the client-cert template deliberately has IsCA:false and no KeyUsageCertSign: with those set, a CertPool entry would be a TRUSTED ROOT and any agent could mint a certificate for any name that chains to itself and validates, becoming a CA for the whole bus. This binding step is exactly the mechanism that makes chain verification unnecessary -- do not reach for a CertPool 'for consistency' with anything else in the codebase. See also MTLS-RELAYGUARD-FU-BUSCERTPOOL (c873482f) for the same trap on the bus's own dual-usage (ServerAuth + ClientAuth) certificate.

MIDDLEWARE CLAUSE, RELOCATED FROM MTLS-CLIENTAUTH 2026-08-14 (spec-keeper, per that task's reviewer close-out C1 -- this is real undelivered scope, not bookkeeping): internal/httpapi has zero transport knowledge today. This task must plumb the peer certificate from r.TLS through a middleware using the existing ctxKey pattern (internal/httpapi/middleware.go:31, authmw.go:86; next free value is 2 as of 2026-08-14, CONFIRM current value before landing since other in-flight tasks may have claimed it), turning MTLS-CLIENTAUTH's admitted-but-unauthorised certificate (buscert.FingerprintOf(r.TLS.PeerCertificates[0])) into the bound, per-route-checkable principal this task's title promises. MTLS-CLIENTAUTH itself (cc9558a8, done) ships only the transport-level ADMIT; it deliberately authorises nothing, per DECISIONS.md "2026-08-14 -- MTLS-CLIENTAUTH".

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (in_progress)
- [ENROL-SHAPE](../../INVITE/ENROL-SHAPE--8942c8c8/task.md) — ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… (done)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [INVITE-STORE](../../INVITE/INVITE-STORE--a9ef92de/task.md) — INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by… (done)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CROSSCHECK](../MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (in_progress)
- [MTLS-RELAYGUARD-FU-BUSCERTPOOL](../MTLS-RELAYGUARD-FU-BUSCERTPOOL--c873482f/task.md) — MTLS-RELAYGUARD-FU-BUSCERTPOOL: relay client-cert verification must not build a CertPool… (todo)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [7a197025-93f9-470b-a69b-bad494eeae94](../MTLS-re-bind-route-an-agent-renews-its-client-certificat--7a197025/task.md) — MTLS re-bind route: an agent renews its client certificate against its EXISTING agent id,… (todo)
- [ENROL-SHAPE](../../INVITE/ENROL-SHAPE--8942c8c8/task.md) — ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… (done)
- [INVITE-STORE](../../INVITE/INVITE-STORE--a9ef92de/task.md) — INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by… (done)
- [MTLS-BIND-FU-CROSSPLANE](../MTLS-BIND-FU-CROSSPLANE--f6782d5c/task.md) — MTLS-BIND-FU-CROSSPLANE: one client certificate can name BOTH a peer bus and an agent --… (todo)
- [MTLS-BIND-FU-DOCS](../MTLS-BIND-FU-DOCS--8c40ea26/task.md) — MTLS-BIND-FU-DOCS: document the enrolment certificate binding -- CONTRACTS-HTTP.md 409, D… (todo)
- [MTLS-BIND-FU-ZEROFPRECORD](../MTLS-BIND-FU-ZEROFPRECORD--b8a4ac17/task.md) — MTLS-BIND-FU-ZEROFPRECORD: validateRosterEntry accepts a CertBinding with a ZERO fingerpr… (todo)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CROSSCHECK](../MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (in_progress)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-MIGRATE](../MTLS-MIGRATE--59883178/task.md) — MTLS-MIGRATE: pin add cannot migrate a pre-TLS (http-enrolled) identity onto TLS; its own… (todo)
- [MTLS-RELAYGUARD-FU-BUSCERTPOOL](../MTLS-RELAYGUARD-FU-BUSCERTPOOL--c873482f/task.md) — MTLS-RELAYGUARD-FU-BUSCERTPOOL: relay client-cert verification must not build a CertPool… (todo)
- [RELAY-41](../../RELAY/RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-44](../../RELAY/RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../../RELAY/RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
