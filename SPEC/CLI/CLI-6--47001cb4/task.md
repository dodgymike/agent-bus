# CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper idea from the dissolved DUR-4-FU-TOOLING)

| Field | Value |
| --- | --- |
| Public id | `47001cb4-bc0f-44f8-929e-ac51bc6d0fb3` |
| Key | CLI-6 |
| Epic | [CLI](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | cli |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:44:34.429708+00:00 |
| Updated | 2026-08-14T11:08:58.659671+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestCLILog' ./client/... ./cmd/agent-bus-cli/... && go run ./cmd/agent-bus-cli log --help 2>&1 | grep -qi 'metadata only'
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

Read the audit log -- which under invariant 6 is **METADATA AND ROUTING INFO ONLY** (id, sequence,
sender, recipients, bus path, timestamp, size, content hash), NEVER bodies. That is a deliberate
2026-08-02 decision so the audit trail stays compatible with end-to-end encrypted, forward-secret
payloads. Support filtering by sender, recipient, time range and sequence range, and --follow to tail
it. **The CLI must not imply message bodies are retrievable from the log; make their absence EXPLICIT
in the output and in --help**, so an operator is never misled into thinking a body was lost. The proof
greps --help for that statement precisely because it is the kind of thing that quietly goes missing.

ABSORBED FROM THE DISSOLVED DUR-4-FU-TOOLING (superseded 2026-08-02 by the always-restart decision):
a read-only frame-level view of the WAL -- offset, record index, record type, length, MAC-ok, one line
per frame -- so an operator can see what is on disk without writing a throwaway Go program. It is now
an ORDINARY diagnostic rather than an emergency tool, because the bus always restarts. Ship it here or
under CLI-8 doctor, but ship it somewhere.

DEPENDS ON: DUR-5 (the audit log itself), CLI-1. PROOF fails today by construction.

RELAY-25 WIDENING (2026-08-14, owner direction): the audit/log CLI output must expose the recorded `bus_path` for every audit record and every relay hop, in both human and --json modes. `bus_path` is routing metadata, not a message body; preserve hop order and make absence/empty path explicit. This is required for fed-smoke.sh to verify three-bus traversal and blocks RELAY-25. Do not duplicate this requirement in another log-reader task.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-1](../CLI-1--0495d133/task.md) — CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single clien… (done)
- [CLI-8](../CLI-8--ae4caacc/task.md) — CLI-8: doctor -- diagnose a broken setup with a specific remedy per failure (todo)
- [DUR-4-FU-TOOLING](../../DUR/DUR-4-FU-TOOLING--26c2ce16/task.md) — DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start (superseded)
- [DUR-5](../../DUR/DUR-5--a7123e88/task.md) — DUR-5: Append-only message audit log (done)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-4-FU-TOOLING](../../DUR/DUR-4-FU-TOOLING--26c2ce16/task.md) — DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start (superseded)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
