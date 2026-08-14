# MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.RequestClientCert plus admitClientCertificate (transport ADMITS, never AUTHORISES), never InsecureSkipVerify

| Field | Value |
| --- | --- |
| Public id | `cc9558a8-309e-4458-ab91-d9a28517ed53` |
| Key | MTLS-CLIENTAUTH |
| Epic | [MTLS](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:50.414957+00:00 |
| Updated | 2026-08-14T11:39:10.871517+00:00 |
| Completed | 2026-08-14T11:39:10.871502+00:00 |

## Proof command

```sh
go test -race -run 'TestClientCertificateIsRequestedNotRequired|TestAdmitClientCertificate|TestBusTLSConfig' ./cmd/agent-bus
```

## Status note

Dispatched and IN FLIGHT 2026-08-14. Now on the RELAY critical path, not only MTLS-epic work: it is the ingress half of the peer credential (MTLS-CLIENTAUTH -> RELAY-41 -> RELAY-20 -> RELAY-21 -> RELAY-24 -> RELAY-25). Moved off todo so claim-next cannot hand it to a second agent. Scope itself unchanged.

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN, MTLS-LISTENER | BLOCKS: MTLS-BIND, MTLS-CROSSCHECK, MTLS-VERIFY, MTLS-RELAYGUARD

AMENDED 2026-08-14 (spec-keeper, per reviewer close-out C1/C2 on this task's own notes -- see DECISIONS.md "2026-08-14 -- MTLS-CLIENTAUTH: the listener REQUESTS a client certificate, and never requires one" for the full empirical record). LANDED at commit a97f854: tls.RequestClientCert, NOT tls.RequireAnyClientCert as this task originally specified -- the deviation is deliberate and empirically justified, not a shortfall. With no CA, tls.RequireAndVerifyClientCert/VerifyClientCertIfGiven are unusable (chain-verify against nil ClientCAs = system roots, admits every client WITHOUT a certificate and rejects every client WITH one); tls.RequireAnyClientCert locks out every pre-MTLS-CLIENTCERT agent and Docker's HEALTHCHECK at the handshake, before any route or log line. RequestClientCert authorises NOTHING at handshake time -- the transport layer only ADMITS a certificate (admitClientCertificate: fingerprint-derivable, single parseable leaf, nothing more); the policy decision moves to the application layer, per-route, via a bound fingerprint. That produces a deliberate asymmetry: the enrolment route MUST accept a cert it has never seen (accepting it is how binding happens), while every other route requires a cert already bound to an agent. Also ships a permanent guard test that no InsecureSkipVerify exists on any reachable path (TestNoInsecureSkipVerifyAnywhere's replacement coverage -- see proof_cmd).

STILL OWED, MOVED to MTLS-BIND (b6378bda, todo) -- this is real undelivered scope, not bookkeeping, and closing this task without relocating it would silently lose it: internal/httpapi has zero transport knowledge today, so the peer cert must be plumbed from r.TLS through a middleware using the existing ctxKey pattern (internal/httpapi/middleware.go:31, authmw.go:86; next free value is 2), and THAT middleware is what turns an admitted-but-unauthorised certificate into a bound, per-route-checkable principal. MTLS-BIND's own description has been updated to carry this clause.

FORBIDDEN IMPLEMENTATION (security-testing finding, 2026-08-07): do NOT verify client certificates by collecting enrolled agents' certificates into one x509.CertPool and calling Verify against it. client/clientcert.go (~line 550-620) documents why in detail: the certificate template deliberately omits IsCA and KeyUsageCertSign for exactly this reason -- with those fields set, a CertPool entry is a TRUSTED ROOT, so any agent could mint a certificate for any name that chains to itself and validates, becoming a CA for the whole bus. Verification here must be fingerprint-based (SHA-256 over the DER, exact match against the fingerprint MTLS-BIND binds at enrolment), never chain/pool verification. See also MTLS-RELAYGUARD-FU-BUSCERTPOOL (c873482f) for the larger-blast-radius version: the bus's own certificate carries both ServerAuth and ClientAuth, so a pool-based scheme would also have to reason about a bus cert arriving on a client-auth connection during relay.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-41](../../RELAY/RELAY-41--05253c80/task.md)
- **blocks** [RELAY-45](../../RELAY/RELAY-45--4be32336/task.md)
- **blocks** [RELAY-44](../../RELAY/RELAY-44--cec27a90/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-BIND](../MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (in_progress)
- [MTLS-CROSSCHECK](../MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (in_progress)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-RELAYGUARD](../MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [MTLS-RELAYGUARD-FU-BUSCERTPOOL](../MTLS-RELAYGUARD-FU-BUSCERTPOOL--c873482f/task.md) — MTLS-RELAYGUARD-FU-BUSCERTPOOL: relay client-cert verification must not build a CertPool… (todo)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is… (in_progress)
- [RELAY-20](../../RELAY/RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-21](../../RELAY/RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-24](../../RELAY/RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-41](../../RELAY/RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [HANDOVER-RUNBOOK-DOC](../../HANDOVER/HANDOVER-RUNBOOK-DOC--a0e009e1/task.md) — HANDOVER-RUNBOOK-DOC: RUNBOOK.md narrates exactly what the smoke script does (todo)
- [MTLS-BIND](../MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-RELAYGUARD](../MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [MTLS-RELAYGUARD-FU-BUSCERTPOOL](../MTLS-RELAYGUARD-FU-BUSCERTPOOL--c873482f/task.md) — MTLS-RELAYGUARD-FU-BUSCERTPOOL: relay client-cert verification must not build a CertPool… (todo)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is… (in_progress)
- [RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC](../../RELAY/RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC--7126f08b/task.md) — RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC: Record the relayed audit content-hash decisi… (done)
- [RELAY-41](../../RELAY/RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-44](../../RELAY/RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../../RELAY/RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-6](../../RELAY/RELAY-6--0f7275b9/task.md) — RELAY-6: Record the FEDERATION deployment assumptions (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)
- [de0fc1df-a948-4b44-95a4-4b9d01cab267](../../TOOLING/DECISIONS.md-HTML-comment-section-fences-are-imbalanced--de0fc1df/task.md) — DECISIONS.md HTML-comment section fences are imbalanced (6 BEGIN / 8 END) -- introduced b… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
