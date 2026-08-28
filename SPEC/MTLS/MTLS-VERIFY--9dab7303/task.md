# MTLS-VERIFY: prove a RUNNING bus is TLS-only and enforces the current RequestClientCert plus application-layer cert/session binding design

| Field | Value |
| --- | --- |
| Public id | `9dab7303-02eb-40ca-9ac4-508d3a315389` |
| Key | MTLS-VERIFY |
| Epic | [MTLS](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:51.494963+00:00 |
| Updated | 2026-08-23T19:26:12.237657+00:00 |
| Completed | 2026-08-23T19:26:12.237640+00:00 |

## Proof command

```sh
go test -race -run "TestLiveBusServeWrapperOverTLS|TestClientCertificateIsRequestedNotRequired" ./cmd/agent-bus && go test -race -run "TestCrossCheckUnauthenticatedRoutesStillServeWithoutACertificate|TestCrossCheckGatesAnAuthenticatedRoute|TestCrossCheckABoundAgentPresentingItsOwnCertificateIsAdmitted" ./internal/httpapi && go test -race -run "TestCLIEnrolEndToEnd" ./cmd/agent-busctl && ! grep -q 'HEALTH_URL="http://' scripts/bus-serve.sh
```

## Status note

2026-08-23 spec-keeper reconciliation: the old handshake-level 'TLS client with NO client certificate is refused' acceptance text is stale and conflicts with invariant 11's current ratified design. The server uses tls.RequestClientCert so no-cert connections can reach only allow-listed anonymous routes, while authenticated agent routes require the matching session/certificate binding at application admission. Completion proof must cover TLS-only listener, no-cert allow-list reachability, protected-route refusal without matching cert/session, and the correct pinned TLS/client-cert path.

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-LISTENER, MTLS-CLIENTAUTH, MTLS-PIN | BLOCKS: none

Paired committed-vs-running verification per CLAUDE.md. scripts/bus-serve.sh:54 sets HEALTH_URL="http://${LISTEN}/healthz" and curls it at :80 and :161; that is the only surviving bus-*.sh wrapper (AGENTIF-1, done) and it BREAKS the moment MTLS-LISTENER lands, taking every other task's server-startup proof with it. Live assertions required: a plaintext client is refused; a TLS client with NO client certificate is refused; a TLS client with a client certificate and the correct pin reaches /healthz. ALSO FLAG (planner was boundary-blocked from editing them): DEPLOY-1 (fa0c5a4e) and DEPLOY-2 (14f8ec3b) both assume a plaintext listener, and a Compose healthcheck cannot curl plaintext against a TLS-only bus.

=== AUDIT 2026-08-08 (spec-keeper): PARTIALLY SHIPPED -- the split, explicitly ===
MET (in main at 9f2878a, an ancestor of HEAD efde70c):
  - The plaintext health-probe defect is FIXED. scripts/bus-serve.sh:107 at HEAD reads
    HEALTH_URL="https://${PROBE_ADDR}/healthz" and line 113 curls it with --cacert "$CERT_FILE".
    The proof_cmd's second clause (! grep -q 'HEALTH_URL="http://' scripts/bus-serve.sh) PASSES at
    HEAD.
  - TestLiveBusServeWrapperOverTLS exists at HEAD in cmd/agent-bus/busservewrapper_test.go, so the
    proof_cmd's first clause is NON-VACUOUS.
NOT MET -- the "mutually authenticated" half named in this task's own title, and it cannot be met
today. The required live assertion "a TLS client with NO client certificate is refused" is FALSE by
construction: cmd/agent-bus/tlslisten.go:109 at HEAD pins `ClientAuth: tls.NoClientCert`
DELIBERATELY (its comment at lines 26-31 says moving it is MTLS-CLIENTAUTH's job, and
MTLS-CLIENTAUTH may not precede MTLS-CLIENTCERT). MTLS-CLIENTAUTH has NOT shipped, so a TLS client
presenting no client certificate is currently ACCEPTED.
KEEP OPEN until MTLS-CLIENTAUTH lands, then run the three live assertions together. Do NOT close on
the two clauses that currently pass -- the title's "mutually authenticated" claim would be false,
which is exactly the committed-vs-running trap this task was filed to prevent.


=== AMENDMENT 2026-08-23 (spec-keeper) ===
The prior acceptance sentence requiring a TLS handshake refusal when no client certificate is presented is superseded by the ratified invariant-11 design now in HEAD: cmd/agent-bus/tlslisten.go uses tls.RequestClientCert. That means /healthz, /v1/info, /v1/discovery and the credential bootstrap routes remain reachable after TLS without a client certificate, while authenticated routes apply the mTLS/session cross-check at the HTTP admission layer. Do not complete this task on the plaintext-probe half alone. The required proof is now the composed proof in proof_cmd: wrapper-started TLS-only listener; RequestClientCert admits no-cert health/discovery but records presented certs; cross-check refuses a protected route without the required matching certificate/session and admits a bound agent presenting its own certificate; agent-busctl enrol/whoami --verify succeeds over pinned TLS with the client certificate path.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-1](../../AGENTIF/AGENTIF-1--5bc152d6/task.md) — AGENTIF-1: scripts/bus-serve.sh + AGENT_PROTOCOL.md entry (done)
- [DEPLOY-1](../../DEPLOY/DEPLOY-1--fa0c5a4e/task.md) — DEPLOY-1: Dockerfile -- multi-stage build, pinned Go builder, minimal runtime image (done)
- [DEPLOY-2](../../DEPLOY/DEPLOY-2--14f8ec3b/task.md) — DEPLOY-2: docker-compose.yml -- single bus, named volume, healthcheck (done)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (done)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [7befde72-488e-4cf4-a05b-b16e2c2ffd15](../../PROCESS/Integrator-flips-the-task-to-done-atomically-after-a-suc--7befde72/task.md) — Integrator flips the task to done atomically after a successful commit -- close the commi… (todo)
- [88781750-0005-4c2f-8375-2d93dc1560b8](../../DOCS/DECISIONS.md-1302-cites-a-superseded-bus-serve.sh-line-f--88781750/task.md) — DECISIONS.md:1302 cites a superseded bus-serve.sh line for the plaintext-probe follow-on (todo)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CROSSCHECK](../MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (done)
- [MTLS-EXPIRY](../MTLS-EXPIRY--3604af80/task.md) — MTLS-EXPIRY: the client never checks the pinned bus certificate validity period -- the 36… (done)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
