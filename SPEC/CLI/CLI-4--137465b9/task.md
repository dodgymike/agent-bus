# CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadcast.sh)

| Field | Value |
| --- | --- |
| Public id | `137465b9-c75c-4b43-88be-cd0cc13495c4` |
| Key | CLI-4 |
| Epic | [CLI](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | cli |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:44:33.529676+00:00 |
| Updated | 2026-08-07T18:08:46.100553+00:00 |
| Completed | 2026-08-07T12:07:08.617484+00:00 |

## Proof command

```sh
go test -race -run 'TestCLISend|TestCLIBroadcast' ./client/... ./cmd/agent-busctl/... && go test -race -run TestCLISendReusesIdempotencyKeyOnRetry ./cmd/agent-busctl/...
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

REPLACES AGENTIF-4 (`scripts/bus-broadcast.sh`) and AGENTIF-5 (`scripts/bus-send.sh`), both superseded.

Send a DM to a fully-qualified agent id, or broadcast to the roster. Body from an argument, from a
file, or piped on stdin (so it composes with other tools). Refuse ambiguous or empty sends with a
clear error rather than sending nothing.

**IDEMPOTENCY IS THIS COMMAND'S HARD REQUIREMENT (invariant 10).** The client generates the
idempotency key ONCE and REUSES IT ON EVERY RETRY of the same logical send. Generating a fresh key per
attempt turns the retry that idempotency exists to make safe into a duplicate message. The named test
in the proof exists specifically to pin that. Note the two cases must not be collapsed: same key +
same payload is a LEGITIMATE RETRY (server returns the original result); same key + DIFFERENT payload
is a PROTOCOL VIOLATION (server rejects and disconnects) -- the CLI must never produce the second by
mutating a body between attempts. Idempotency keys are retained for a BOUNDED window and FAIL CLOSED,
so a retry arriving after the window is rejected rather than silently re-applied: surface that as a
specific, actionable error, not a generic failure.

DEPENDS ON: MSG epic, IDEM epic, CLI-1, CLI-2.

PROOF. FAILS TODAY by construction. See IDEM-18 (wrappers generate the key once) -- that task is
re-scoped to this client.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [2b4ecf0b-7f01-436b-8135-811ff4963a0e](../busctl-send-broadcast-lose-the-minted-idempotency-key-on--2b4ecf0b/task.md)
- **relates to** [797fb15f-27d8-4671-8d27-c8bd38bfb1f6](../busctl-watch-help-documents-a-fatal-503-under-exit-5-but--797fb15f/task.md)
- **supersedes** [AGENTIF-4](../../AGENTIF/AGENTIF-4--715fc1b8/task.md)
- **supersedes** [AGENTIF-5](../../AGENTIF/AGENTIF-5--8109ab88/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-4](../../AGENTIF/AGENTIF-4--715fc1b8/task.md) — AGENTIF-4: scripts/bus-broadcast.sh + AGENT_PROTOCOL.md entry (superseded)
- [AGENTIF-5](../../AGENTIF/AGENTIF-5--8109ab88/task.md) — AGENTIF-5: scripts/bus-send.sh + AGENT_PROTOCOL.md entry (superseded)
- [CLI-1](../CLI-1--0495d133/task.md) — CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single clien… (done)
- [CLI-2](../CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [CLI-3](../CLI-3--6e70abe5/task.md) — CLI-3: watch -- long-poll tail, human-readable for a person and NDJSON for a pipe (replac… (done)
- [DEPLOY-2-FU-CONTAINERNAME](../../DEPLOY/DEPLOY-2-FU-CONTAINERNAME--e9dd20b4/task.md) — DEPLOY-2-FU-CONTAINERNAME: docker-compose.yml hardcodes container_name, collides with any… (done)
- [IDEM-11](../../IDEM/IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (todo)
- [IDEM-18](../../IDEM/IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-4](../../AGENTIF/AGENTIF-4--715fc1b8/task.md) — AGENTIF-4: scripts/bus-broadcast.sh + AGENT_PROTOCOL.md entry (superseded)
- [AGENTIF-5](../../AGENTIF/AGENTIF-5--8109ab88/task.md) — AGENTIF-5: scripts/bus-send.sh + AGENT_PROTOCOL.md entry (superseded)
- [IDEM-18](../../IDEM/IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (todo)
- [TUI-3](../../TUI/TUI-3--140aadf7/task.md) — TUI-3: GUARD — the TUI sits on the client package and cannot bypass it, and agent-busctl'… (todo)
- [TUI-5](../../TUI/TUI-5--b2a44ce9/task.md) — TUI-5: the human as a bus PARTICIPANT — read and send messages as a person (message bodie… (todo)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
