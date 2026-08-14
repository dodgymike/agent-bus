# CLI-3: watch -- long-poll tail, human-readable for a person and NDJSON for a pipe (replaces bus-wait.sh)

| Field | Value |
| --- | --- |
| Public id | `6e70abe5-ab09-4625-a62a-4a6696ae0841` |
| Key | CLI-3 |
| Epic | [CLI](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | cli |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:44:33.115083+00:00 |
| Updated | 2026-08-07T18:08:45.641635+00:00 |
| Completed | 2026-08-07T12:07:08.316530+00:00 |

## Proof command

```sh
go test -race -run 'TestCLIWatch' ./client/... ./cmd/agent-busctl/... && go test -race -run TestCLIWatchStreamsNDJSONIncrementally ./cmd/agent-busctl/...
```

## Status note

DISPATCHED by triage-20260802-c3d-breadth-pass3. Boundaries: CLI-3/4/5 -> client,cmd/busctl,CONTRACTS-CLI.md | DEPLOY-2-FU-CONTAINERNAME -> docker-compose.yml | NAMESUFFIXES -> internal/ids | IDEM-11 -> internal/idem,internal/wal(additive),internal/hub,CONTRACTS-ONDISK.md. All DISJOINT. No agent writes DECISIONS.md/AGENT_LOG.md/SPEC.md this wave.

## Description

MERGED EPIC 2026-08-02. The CLI and AGENTIF epics are now ONE epic (user decision, DECISIONS.md
2026-08-02: "Merge the CLI and AGENTIF epics" / "the go cli should take the place of the .sh files and
be easy to use for a human + friendly for an agent to use or embed"). Invariant 7 is AMENDED, not
weakened: nobody hand-writes HTTP, but the vehicle is a CLI subcommand, not a scripts/bus-*.sh
wrapper. A feature without its CLI subcommand is still not done.

THREE AUDIENCES, and every subcommand serves all three: a HUMAN (readable output, remedial errors); an
AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO TTY-dependent credential
input); an AGENT EMBEDDING (the reusable client package, which therefore CANNOT live under internal/).

REPLACES AGENTIF-6 (`scripts/bus-wait.sh`), which is superseded.

The headline command. Drives the long-poll wait endpoint in a loop and renders messages as a readable
live feed (timestamp, sender, recipient/scope, body), advancing its cursor across reconnects. Handles
Ctrl-C cleanly, reconnects with backoff on transient failure, and never busy-loops.

**--json STREAMS NDJSON: one JSON object per line, FLUSHED AS IT ARRIVES.** This is the requirement
that makes the command usable by an embedding or shelling-out agent at all -- a long-poll that buffers
to completion is useless, because it never completes. The test named in the proof must assert
INCREMENTAL delivery (a reader sees line 1 before the stream ends), not merely that the output parses.

**DELIVERY IS AT-LEAST-ONCE** (decision, 2026-08-02). Duplicates are the NORMAL steady state, not an
edge case. The watch loop must not present a duplicate as an error, and the help text must say so, so
an agent author writes an idempotent handler instead of assuming exactly-once. Freshness comes from
the server-minted monotonic sequence plus the recipient-side cursor.

Session refresh (75% of lifetime) and transparent re-authentication after a server restart must be
invisible here -- a watch that dies when the bus restarts is a watch nobody can rely on.

DEPENDS ON: POLL epic, CLI-1, CLI-2.

PROOF. FAILS TODAY by construction. The second clause is deliberately a SEPARATE named test for the
incremental-streaming property, because a --json flag that buffers would pass a naive shape test.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [797fb15f-27d8-4671-8d27-c8bd38bfb1f6](../busctl-watch-help-documents-a-fatal-503-under-exit-5-but--797fb15f/task.md)
- **supersedes** [AGENTIF-6](../../AGENTIF/AGENTIF-6--31c1257c/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-6](../../AGENTIF/AGENTIF-6--31c1257c/task.md) — AGENTIF-6: scripts/bus-wait.sh + AGENT_PROTOCOL.md entry (superseded)
- [CLI-1](../CLI-1--0495d133/task.md) — CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single clien… (done)
- [CLI-2](../CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [DEPLOY-2-FU-CONTAINERNAME](../../DEPLOY/DEPLOY-2-FU-CONTAINERNAME--e9dd20b4/task.md) — DEPLOY-2-FU-CONTAINERNAME: docker-compose.yml hardcodes container_name, collides with any… (done)
- [IDEM-11](../../IDEM/IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-6](../../AGENTIF/AGENTIF-6--31c1257c/task.md) — AGENTIF-6: scripts/bus-wait.sh + AGENT_PROTOCOL.md entry (superseded)
- [CLI-1](../CLI-1--0495d133/task.md) — CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single clien… (done)
- [CLI-3-FU-HASHVERIFY](../CLI-3-FU-HASHVERIFY--4c48cbe1/task.md) — CLI-3-FU-HASHVERIFY: verify content_sha256 against the decoded body on the read path (done)
- [CLI-3-FU-SAFETEXT](../CLI-3-FU-SAFETEXT--e4baf8c5/task.md) — CLI-3-FU-SAFETEXT: export the terminal-safe renderer from client/ and delete busctl's copy (done)
- [CLI-3-FU-STOREDEDUP](../CLI-3-FU-STOREDEDUP--be8c763c/task.md) — CLI-3-FU-STOREDEDUP: collapse cursorstore.go's duplicated atomic-save and lock discipline… (done)
- [CLI-3-FU-URLKEY](../CLI-3-FU-URLKEY--6979c651/task.md) — CLI-3-FU-URLKEY: watch cursor is keyed by bus_url INCLUDING SCHEME, so a TLS flip replays… (done)
- [CLI-4](../CLI-4--137465b9/task.md) — CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadc… (done)
- [CLI-5](../CLI-5--86dea094/task.md) — CLI-5: agents -- roster listing (replaces bus-agents.sh) (done)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
