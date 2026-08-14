# MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is TLS-only and mutually authenticated (committed is not running)

| Field | Value |
| --- | --- |
| Public id | `9dab7303-02eb-40ca-9ac4-508d3a315389` |
| Key | MTLS-VERIFY |
| Epic | [MTLS](../epic.md) |
| Status | in_progress |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:51.494963+00:00 |
| Updated | 2026-08-08T14:47:54.155127+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestLiveBusServeWrapperOverTLS' ./cmd/agent-bus && ! grep -q 'HEALTH_URL="http://' scripts/bus-serve.sh
```

## Status note

SEQUENCING (from the MTLS-PIN security gate, 2026-08-07, MED-1): MTLS-VERIFY must land WITH OR BEFORE MTLS-LISTENER. MTLS-PIN's client pins sha256-of-DER but does NOT check the certificate's validity period -- disabling the default chain check disables expiry with it. The gate demonstrated empirically that a certificate whose NotAfter is a day in the past is pinned, accepted, and enrolled against. DECISIONS.md chose a 365-day certificate lifetime explicitly as a leak-containment bound, and only the client can enforce that bound on the BUS's certificate, so until this lands that lifetime decision is decoration. Minimal fix named by the gate: after the pin matches in client/pin.go's verifyPinnedBusCertificate, x509.ParseCertificate(rawCerts[0]) and reject outside NotBefore..NotAfter.

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

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-1](../../AGENTIF/AGENTIF-1--5bc152d6/task.md) — AGENTIF-1: scripts/bus-serve.sh + AGENT_PROTOCOL.md entry (done)
- [DEPLOY-1](../../DEPLOY/DEPLOY-1--fa0c5a4e/task.md) — DEPLOY-1: Dockerfile -- multi-stage build, pinned Go builder, minimal runtime image (done)
- [DEPLOY-2](../../DEPLOY/DEPLOY-2--14f8ec3b/task.md) — DEPLOY-2: docker-compose.yml -- single bus, named volume, healthcheck (done)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (in_progress)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (in_progress)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [88781750-0005-4c2f-8375-2d93dc1560b8](../../DOCS/DECISIONS.md-1302-cites-a-superseded-bus-serve.sh-line-f--88781750/task.md) — DECISIONS.md:1302 cites a superseded bus-serve.sh line for the plaintext-probe follow-on (todo)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CROSSCHECK](../MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (in_progress)
- [MTLS-EXPIRY](../MTLS-EXPIRY--3604af80/task.md) — MTLS-EXPIRY: the client never checks the pinned bus certificate validity period -- the 36… (done)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (in_progress)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [TRIAGE-LOCK](../../PROCESS/TRIAGE-LOCK--25f0eac6/task.md) — TRIAGE-LOCK: backlog-triage mutex (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
