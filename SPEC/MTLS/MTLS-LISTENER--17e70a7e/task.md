# MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is no plaintext listener

| Field | Value |
| --- | --- |
| Public id | `17e70a7e-2f29-453a-8ded-74cbd01c4274` |
| Key | MTLS-LISTENER |
| Epic | [MTLS](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:50.174921+00:00 |
| Updated | 2026-08-14T22:26:49.426047+00:00 |
| Completed | 2026-08-14T22:26:49.426031+00:00 |

## Proof command

```sh
go test -race -run 'TestServerServesTLSOnly|TestPlaintextClientIsRejected|TestRunRefusesToStartWithoutUsableCert|TestCmdHasNoPlaintextListener' ./cmd/agent-bus && test "$(grep -Fxc '**The server serves TLS and ONLY TLS (invariant 11, `MTLS-LISTENER`, landed 2026-08-07).** `-listen`' CONTRACTS-CLI.md)" -eq 1
```

## Status note

Gate list now satisfied/handled as of 2026-08-07: MTLS-VERIFY is landing in the SAME commit as this task (dispatched together to feature-runner, one commit, at user's explicit direction). MTLS-ROTATE landed 2026-08-07 (29cdafc, client pins a SET rather than a single cert). MTLS-EXPIRY is being run concurrently by another agent, touching client/pin.go. MTLS-CLIENTAUTH and MTLS-CLIENTCERT are DELIBERATELY out of scope for this landing -- the listener therefore does NOT require a client certificate (tls.ClientAuth=NoClientCert), since MTLS-CLIENTCERT (client keypair generation) has not shipped yet and demanding client certs would lock out every agent.

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN, MTLS-BUSCERT | BLOCKS: MTLS-CLIENTAUTH, MTLS-VERIFY

invariant 11. Today cmd/agent-bus/main.go:375 does net.Listen("tcp", cfg.Listen) and main.go:386 does srv.Serve(ln); http.Server at main.go:368-372 sets no TLSConfig and there is no TLS/x509 code anywhere in the tree. Attach via tls.NewListener or srv.ServeTLS. The server must exit non-zero with a remedial message naming the cert path rather than degrading. Config.validate() (main.go:128-152) is purely syntactic and has no data-dir knowledge, so the refusal belongs in run(), not flag parsing. New flags land in CONTRACTS-CLI.md. BREAKING -- escalated: this strands every plaintext client, including scripts/bus-serve.sh's health probe (fixed by MTLS-VERIFY) and CLI-2's recorded proof_cmd.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-2](../../CLI/CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [MTLS-BUSCERT](../MTLS-BUSCERT--93f0dc19/task.md) — MTLS-BUSCERT: generate/load the bus's self-signed certificate + private key in the data d… (done)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (done)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-EXPIRY](../MTLS-EXPIRY--3604af80/task.md) — MTLS-EXPIRY: the client never checks the pinned bus certificate validity period -- the 36… (done)
- [MTLS-ROTATE](../MTLS-ROTATE--c2e8df5b/task.md) — MTLS-ROTATE: a client accepts a SET of pinned bus certificates so a rotation does not for… (done)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: prove a RUNNING bus is TLS-only and enforces the current RequestClientCert p… (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [51710f76-ea92-42fd-bbc3-b86415fbc8e1](../../CLI/Latent-data-race-in-cmd-agent-busctl-enrol_test.go-serve--51710f76/task.md) — Latent data race in cmd/agent-busctl/enrol_test.go: server stderr buffer is read while os… (done)
- [88781750-0005-4c2f-8375-2d93dc1560b8](../../DOCS/DECISIONS.md-1302-cites-a-superseded-bus-serve.sh-line-f--88781750/task.md) — DECISIONS.md:1302 cites a superseded bus-serve.sh line for the plaintext-probe follow-on (todo)
- [INVITE-CLIENT](../../INVITE/INVITE-CLIENT--4123e25d/task.md) — INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… (done)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [MTLS-BUSCERT](../MTLS-BUSCERT--93f0dc19/task.md) — MTLS-BUSCERT: generate/load the bus's self-signed certificate + private key in the data d… (done)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-EXPIRY](../MTLS-EXPIRY--3604af80/task.md) — MTLS-EXPIRY: the client never checks the pinned bus certificate validity period -- the 36… (done)
- [MTLS-LISTENER-FU-CLIENTHTTP](../MTLS-LISTENER-FU-CLIENTHTTP--8d906b8b/task.md) — MTLS-LISTENER-FU-CLIENTHTTP: client/config.go still allows unpinned http:// to loopback,… (todo)
- [MTLS-LISTENER-FU-TLS13](../MTLS-LISTENER-FU-TLS13--c6ff9160/task.md) — MTLS-LISTENER-FU-TLS13: raise both ends of the TLS floor to 1.3 and drop the reachable CB… (todo)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [MTLS-ROTATE](../MTLS-ROTATE--c2e8df5b/task.md) — MTLS-ROTATE: a client accepts a SET of pinned bus certificates so a rotation does not for… (done)
- [MTLS-ROTATE-FU-SERVERSIDE](../MTLS-ROTATE-FU-SERVERSIDE--b624915b/task.md) — MTLS-ROTATE-FU-SERVERSIDE: the bus serves ONE certificate, so DECISIONS.md E3's two-certi… (todo)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: prove a RUNNING bus is TLS-only and enforces the current RequestClientCert p… (done)
- [MTLS-VERIFY-FU](../../AUTH/MTLS-VERIFY-FU--5f8e0cba/task.md) — MTLS-VERIFY-FU-DOCSCHEME (README/PROTOCOL half): main still documents the bus as plaintex… (todo)
- [MTLS-VERIFY-FU-DOCSCHEME](../../DOCS/MTLS-VERIFY-FU-DOCSCHEME--cb4fd330/task.md) — MTLS-VERIFY-FU-DOCSCHEME: README + AGENT_PROTOCOL still tell agents to dial http:// a bus… (todo)
- [ORCH-1](../../ORCH/ORCH-1--e22449ec/task.md) — ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose ratio… (todo)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
