# DISCOVERY-DOC: self-describing unauthenticated discovery document so an agent with only a bus URL can bootstrap

| Field | Value |
| --- | --- |
| Public id | `2d7ce37b-0d58-42fd-99fd-573246af0fd2` |
| Key | _(null in the export)_ |
| Epic | [CORE](../epic.md) |
| Status | in_progress |
| Priority | P1 |
| Component | httpapi |
| Section | backlog |
| Tags | — |
| Created | 2026-08-07T17:32:33.347837+00:00 |
| Updated | 2026-08-08T10:29:47.918265+00:00 |
| Completed | — |

## Proof command

```sh
go test -race ./internal/httpapi/... && D=$(mktemp -d) && P=18173 && (go run ./cmd/agent-bus -listen 127.0.0.1:$P -data-dir "$D" &>"$D/log" & echo $! >"$D/pid") && for i in $(seq 1 30); do curl -sf http://127.0.0.1:$P/healthz >/dev/null 2>&1 && break; sleep 0.5; done && curl -sf http://127.0.0.1:$P/v1/discovery | jq . && kill "$(cat "$D/pid")" 2>/dev/null; rm -rf "$D"
```

## Status note

CODE COMPLETE, NOT COMMITTED. GET /v1/discovery implemented, tested and staged in internal/httpapi/** + CONTRACTS-HTTP.md + DECISIONS.md + AGENT_LOG.md, awaiting the orchestrator's coordinated commit (feature-runner never commits). Chain complete: spec-keeper -> implementer -> test-engineer -> reviewer -> security -> documentation. reviewer=CHANGES-REQUESTED on two doc deliverables (both since written) with three P2s applied; security=PASS-WITH-NITS with one MEDIUM (two-keypair conflation) and three LOWs, all applied. proof-check: verdict=PASS class=test exit=0 tests_run=83 top_level=14 skipped=0 failed=0 empty_pkgs=0. Verified against a RUNNING bus on a throwaway data dir: /v1/discovery returned 7107 bytes with no credential, /v1/info carried the new discovery field, POST returned 405 Allow: GET. The orchestrator should complete this task with the commit_sha once committed.

## Description

GET /v1/info returns only {bus_id, version, uptime_seconds}, which tells an agent nothing about HOW TO JOIN. An agent handed only a bus URL cannot enrol. Serves invariant 7 (nobody hand-writes HTTP; the compiled Go CLI is THE client) by making the bus self-describing.

Precedent: the Spec Server's own GET /api/v1/agent-enrollments — an unauthenticated, machine-readable document with a `service` name, an ordered `steps` array, exact URLs, an explicit token_source explanation, and what the caller must save.

Scope (server side ONLY, this task): internal/httpapi/** and CONTRACTS-HTTP.md. Add a bounded, static, unauthenticated discovery document describing: what the service is + bus id; the ORDERED enrolment steps with exact paths; whether enrolment is invite-only (describe what is TRUE today and flag what is imminent — INVITE-GATE is still `todo`); that the agent supplies an Ed25519 public key and receives a SERVER-MINTED fully-qualified <bus-id>.<agent-id> it does not choose (invariant 1); the session model (client signs a SERVER-PROVIDED token, max one hour, refresh at 75%); where to get the client and that an importable Go package exists at client/; and the HONEST LIMITS (no TLS yet so loopback only; messages are signed but NOT verified on receipt because key distribution (CRYPTO-4) does not exist).

SECURITY LINE — describe the PROTOCOL, never the ROSTER. No agent list, no agent count, no data-dir path, no peer list, no key material, no on-disk file paths. An unauthenticated caller learns HOW TO JOIN and nothing about WHO HAS JOINED. The response must be bounded and static — its size must NOT grow with bus state (that is both an information leak and a DoS surface). internal/httpapi has a test pinning /v1/info's EXACT field set; it must be updated deliberately, never weakened or deleted.

Design question to settle and record in DECISIONS.md: extend /v1/info vs add a separate endpoint.

Explicitly OUT OF SCOPE (follow-up): the CLI subcommand half, AGENT_PROTOCOL.md and CONTRACTS-CLI.md — cmd/** and client/** are owned by other agents right now.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [DISCOVERY-DOC-FU-CLI](../../CLI/DISCOVERY-DOC-FU-CLI--b123c098/task.md)
- **blocks** [DISCOVERY-DOC-FU-CLI](../../CLI/DISCOVERY-DOC-FU-CLI--b123c098/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DISCOVERY-DOC-FU-CLI](../../CLI/DISCOVERY-DOC-FU-CLI--b123c098/task.md) — DISCOVERY-DOC-FU-CLI: \`agent-busctl\` subcommand that fetches and renders the bus discover… (todo)
- [DISCOVERY-DOC-FU-GITIGNORE](../../TOOLING/DISCOVERY-DOC-FU-GITIGNORE--9047f6a7/task.md) — DISCOVERY-DOC-FU-GITIGNORE: stale untracked busctl binary at repo root is not gitignored (todo)
- [DISCOVERY-DOC-FU-README](../../DOCS/DISCOVERY-DOC-FU-README--be3c84f3/task.md) — DISCOVERY-DOC-FU-README: README.md still documents the old three-field /v1/info body (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
