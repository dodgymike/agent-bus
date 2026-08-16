# MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change -- never InsecureSkipVerify, and no flag that silently disables verification

| Field | Value |
| --- | --- |
| Public id | `8c46dc93-16d0-4eea-8ad3-ac51136551e2` |
| Key | MTLS-PIN |
| Epic | [MTLS](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:51.278808+00:00 |
| Updated | 2026-08-07T18:56:39.469507+00:00 |
| Completed | 2026-08-07T18:56:39.469490+00:00 |

## Proof command

```sh
go test -race -run 'TestClientPinsBusFingerprintAtEnrol|TestClientRefusesChangedBusFingerprint|TestClientHasNoInsecureVerificationFlag|TestNoInsecureSkipVerifyAnywhere' ./client/... && grep -q -- '--bus-fingerprint' CONTRACTS-CLI.md && grep -q -- '--bus-fingerprint' AGENT_PROTOCOL.md
```

## Status note

CODE COMPLETE, NOT COMMITTED. Both gates COMPLETED and PASSED against a frozen tree identified by md5sum client/*.go cmd/agent-busctl/{enrol,root,whoami}.go | md5sum = 4df6e4c572995867adfb087392a4a806; only markdown changed after that and the hash was re-verified unchanged. proof-check.sh verdict: PASS -- 18 test(s) ran (7 top-level), 7 passed, 0 skipped. Awaiting integrator commit; complete with the real commit_sha then. CODE-ONLY by nature: the bus does not serve TLS yet (MTLS-LISTENER), so no deployment exercises this and the test_summary must NOT imply live behaviour.

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-BUSCERT, MTLS-CLIENTCERT, CLI-1 (0495d133) | BLOCKS: MTLS-VERIFY

CLAUDE.md invariant 11: never disable certificate verification to make something work, and never ship a flag that does it silently -- a bus that looks secure and is not is worse than no TLS. Verification via tls.Config.VerifyPeerCertificate against the pinned SHA-256-of-DER; a changed fingerprint is a hard failure whose error names the remedy. Where the pin comes from is settled by MTLS-DESIGN (planner recommends: carried in the invite blob, which removes the TOFU window). "The trusted path must be the easy path" -- enrol against a fresh bus must work without hand-editing a trust store. DEPENDS ON MTLS-BUSCERT, MTLS-CLIENTCERT, CLI-1.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-1](../../CLI/CLI-1--0495d133/task.md) — CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single clien… (done)
- [MTLS-BUSCERT](../MTLS-BUSCERT--93f0dc19/task.md) — MTLS-BUSCERT: generate/load the bus's self-signed certificate + private key in the data d… (done)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (in_progress)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is… (in_progress)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2582f548-6493-439c-ba71-7f5cf73650fc](../../PROCESS/Spec-Server-export-both-format-markdown-and-format-json--2582f548/task.md) — Spec Server /export (both format=markdown and format=json) silently drops the commits\[\] a… (todo)
- [MTLS-BUSCERT](../MTLS-BUSCERT--93f0dc19/task.md) — MTLS-BUSCERT: generate/load the bus's self-signed certificate + private key in the data d… (done)
- [MTLS-CLIENTCERT](../MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (in_progress)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-EXPIRY](../MTLS-EXPIRY--3604af80/task.md) — MTLS-EXPIRY: the client never checks the pinned bus certificate validity period -- the 36… (done)
- [MTLS-PIN-FU-MIRRORSYNC](../MTLS-PIN-FU-MIRRORSYNC--15e87708/task.md) — MTLS-PIN-FU-MIRRORSYNC: an agreement test that client.BusFingerprint and internal/buscert… (todo)
- [MTLS-PIN-FU-SCHEMEGUARD](../MTLS-PIN-FU-SCHEMEGUARD--38b5b7c3/task.md) — MTLS-PIN-FU-SCHEMEGUARD: a direct test for client.transportSecurity, whose unknown-scheme… (todo)
- [MTLS-ROTATE](../MTLS-ROTATE--c2e8df5b/task.md) — MTLS-ROTATE: a client accepts a SET of pinned bus certificates so a rotation does not for… (done)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is… (in_progress)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
