# CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single client that replaces the shell wrappers

| Field | Value |
| --- | --- |
| Public id | `0495d133-8818-4657-b5f2-af81809ea922` |
| Key | CLI-1 |
| Epic | [CLI](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | cli |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:44:32.110685+00:00 |
| Updated | 2026-08-07T18:08:44.640707+00:00 |
| Completed | 2026-08-02T23:32:54.679389+00:00 |

## Proof command

```sh
go build ./... && go test -race ./client/... && go vet ./... && go run ./cmd/agent-busctl --help 2>&1 | grep -q 'enrol' && ! go list -deps ./cmd/agent-busctl | grep -q 'agent-bus/internal/'
```

## Description

MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
wrapper. A feature without its CLI subcommand is still not done.

THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).

THE DECISION THAT IS ALREADY MADE AND MUST NOT BE RE-LITIGATED: **"embed" is the load-bearing word.**
The CLI is a THIN SHELL over a REUSABLE GO CLIENT PACKAGE, and that package **CANNOT LIVE UNDER
`internal/`** -- Go would forbid any other module importing it, which defeats the entire requirement.
Decided 2026-08-02 precisely because deciding it late would be expensive. Put it at a top-level
importable path (e.g. `client/`), and treat its exported surface as a PUBLIC API subject to
compatibility care. The binary is a separate `cmd/` (e.g. `cmd/agent-bus-cli`) so the server image
never accidentally ships the client, and the client package must NOT import anything under
`internal/`.

STILL TO DECIDE AND RECORD IN DECISIONS.md (the original CLI-1 question, narrowed): the exact package
path and binary name. NOT still open: whether the package is importable (it is), and whether one
binary serves both humans and agents (it does).

SCOPE.
 - The client package: transport, base URL, timeouts, retry/backoff, credential handling, cursor
   management, and typed errors. NO business logic beyond what later CLI tasks need.
 - Subcommand skeleton and global flags: --bus URL, --identity path, --json, --timeout.
   Config/env resolution order, documented, deterministic.
 - EXIT-CODE CONVENTIONS, fixed now and treated as contract: distinct codes for usage error,
   auth/credential failure, network/unreachable, server-side error, and "nothing to report" so an
   agent can branch without parsing text. Put them in CONTRACTS.md.
 - **THE LONG-POLL SUBCOMMAND STREAMS NEWLINE-DELIMITED JSON (NDJSON)** -- one JSON object per line,
   flushed as it arrives, so a consumer can process incrementally rather than buffering to
   completion. Establish that convention here even though CLI-3 implements the command.
 - NO interactive prompts anywhere, and no TTY-dependent credential input. An agent shelling out has
   no TTY.
 - CONTRACTS.md gains the CLI's flags, exit codes and JSON shapes -- the binary now has a second
   consumer with a compatibility expectation.

NOT IN SCOPE: any actual endpoint call (CLI-2..CLI-8 own those), and rewriting AGENT_PROTOCOL.md
against subcommands (its own task).

PROOF. `go build ./... && go test -race ./client/... && go vet ./... && go run ./cmd/agent-bus-cli --help 2>&1 | grep -q 'enrol' && ! go list -deps ./cmd/agent-bus-cli | grep -q 'agent-bus/internal/'`
The last clause is the load-bearing one: it MECHANICALLY ENFORCES that the client binary (and hence
the client package) does not depend on `internal/`, which is the requirement most likely to be broken
by accident. FAILS TODAY by construction -- neither the package nor the binary exists. Adjust the
paths to whatever DECISIONS.md settles, but KEEP the internal/-dependency clause.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-2](../CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [CLI-3](../CLI-3--6e70abe5/task.md) — CLI-3: watch -- long-poll tail, human-readable for a person and NDJSON for a pipe (replac… (done)
- [CLI-8](../CLI-8--ae4caacc/task.md) — CLI-8: doctor -- diagnose a broken setup with a specific remedy per failure (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-2](../CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [CLI-2-FU-GITIGNORE](../CLI-2-FU-GITIGNORE--6fb7f295/task.md) — CLI-2-FU-GITIGNORE: Add the credential store to .gitignore (done)
- [CLI-2-FU-TLSSEAM](../CLI-2-FU-TLSSEAM--e4d60d97/task.md) — CLI-2-FU-TLSSEAM: The client transport is built before the identity is resolved (todo)
- [CLI-3](../CLI-3--6e70abe5/task.md) — CLI-3: watch -- long-poll tail, human-readable for a person and NDJSON for a pipe (replac… (done)
- [CLI-4](../CLI-4--137465b9/task.md) — CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadc… (done)
- [CLI-5](../CLI-5--86dea094/task.md) — CLI-5: agents -- roster listing (replaces bus-agents.sh) (done)
- [CLI-6](../CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (done)
- [CLI-7](../CLI-7--e600bde6/task.md) — CLI-7: peers -- relay topology and health (replaces bus-peer.sh) (todo)
- [CLI-8](../CLI-8--ae4caacc/task.md) — CLI-8: doctor -- diagnose a broken setup with a specific remedy per failure (todo)
- [CLI-9](../CLI-9--93973755/task.md) — CLI-9: shell completion + man/usage polish (todo)
- [CLI-BUSCTL-IMAGE](../CLI-BUSCTL-IMAGE--9be2105d/task.md) — CLI-BUSCTL-IMAGE: Ship the busctl binary in the container image (todo)
- [INVITE-CLIENT](../../INVITE/INVITE-CLIENT--4123e25d/task.md) — INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… (done)
- [MTLS-CLIENTCERT](../../MTLS/MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (in_progress)
- [MTLS-PIN](../../MTLS/MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
