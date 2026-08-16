# MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certificate (0600) and presents it on every connection

| Field | Value |
| --- | --- |
| Public id | `0bc7a2eb-c436-49ca-92d3-17be58fdd5bd` |
| Key | MTLS-CLIENTCERT |
| Epic | [MTLS](../epic.md) |
| Status | in_progress |
| Priority | P1 |
| Component | agentif |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:51.035037+00:00 |
| Updated | 2026-08-08T14:47:53.734436+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestClientGeneratesClientCert|TestClientTLSKeyIs0600' ./client/...
```

## Status note

Code-complete at 9418a48, gates COMPLETED (reviewer + security PASS on final state). Blocked on documentation only: invariant 7 requires the AGENT_PROTOCOL.md entry and CONTRACTS-CLI.md rows for the new subcommand/flags in the SAME task, and neither is written yet. Do not complete until docs land.

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN, CLI-1 (0495d133) | BLOCKS: MTLS-PIN

Client-side half of the mutual handshake, in the importable client/ package (NOT under internal/ -- CLI-1's non-negotiable). Key stored 0600 in the user's config dir, never in the repo, no interactive prompt, no TTY-dependent input. Stdlib crypto/tls + crypto/x509 only. DEPENDS ON CLI-1 (0495d133) -- no client package exists today.

=== AUDIT 2026-08-08 (spec-keeper): PARTIALLY SHIPPED -- the split, explicitly ===
MET (in main at 9418a48, an ancestor of HEAD efde70c): client/clientcert.go (802 lines),
cmd/agent-busctl/clientcert.go registering the `client-cert` subcommand, and BOTH tests the
proof_cmd names -- TestClientGeneratesClientCert and TestClientTLSKeyIs0600 -- exist at HEAD in
client/clientcert_test.go. The proof_cmd is USABLE.
NOT MET -- the documentation half that invariant 7 requires IN THE SAME TASK, and it is worse than
merely absent:
  (a) AGENT_PROTOCOL.md at HEAD contains ZERO occurrences of "client-cert", "clientcert" or "client
      certificate". The `agent-busctl client-cert` subcommand is undocumented for agents.
  (b) CONTRACTS-CLI.md at HEAD also names the subcommand ZERO times, and its seven MTLS-CLIENTCERT
      mentions are FORWARD REFERENCES asserting the OPPOSITE of HEAD -- line 988: "the **client
      certificate** half of mutual TLS is still to come (`MTLS-CLIENTCERT`)"; line 999:
      "certificate** still has no home, and `MTLS-CLIENTCERT` gives it one"; line 37: "before
      `MTLS-CLIENTCERT` teaches the client to present one". That is doc drift asserting UNSHIPPED
      what shipped at 9418a48.
The earlier status_note ("neither is written yet") was half right and is now stale in the other
direction: CONTRACTS-CLI.md HAS text about this task, and that text is WRONG. Fixing (b) is a
correction, not an addition.

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
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2ca053dd-1b63-42b5-a485-f57b623722ac](../../RELAY/internal-relay-guards_test.go-912-says-the-RELAY-6-subst--2ca053dd/task.md) — internal/relay/guards_test.go:912 says the RELAY-6 substitution 'IS NOT RECORDED IN DECIS… (todo)
- [4b51635d-336f-4f25-94c2-64c53578859d](../../AGENTIF/AGENT_PROTOCOL.md-is-missing-the-CLI-11-key-export-publi--4b51635d/task.md) — AGENT_PROTOCOL.md is missing the CLI-11 (key export-public) and CLI-6 (log) sections -- b… (todo)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-MIGRATE](../MTLS-MIGRATE--59883178/task.md) — MTLS-MIGRATE: pin add cannot migrate a pre-TLS (http-enrolled) identity onto TLS; its own… (todo)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is… (in_progress)
- [RELAY-FU-DOCGO-CROSSBUSTRUST-STALE](../../RELAY/RELAY-FU-DOCGO-CROSSBUSTRUST-STALE--4988156c/task.md) — internal/relay/doc.go asserts relay ingest is structurally blocked (no CrossBusTrust impl… (todo)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)
- [c716f8e7-ad9c-4af9-9fac-1bdb75c8f900](../../DOCS/PROTOCOL.md-1002-says-internal-relay-is-imported-by-noth--c716f8e7/task.md) — PROTOCOL.md:1002 says internal/relay is 'imported by nothing' -- false since ed77bba (int… (todo)
- [fbb16f9b-1b81-4fd0-a60f-5b2a76806bff](../../RELAY/internal-httpapi-peermount.go-pre-auth-prober-does-not-e--fbb16f9b/task.md) — internal/httpapi/peermount.go: 'pre-auth prober does not exist' overstates ruling (h), an… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
