# INVMINT-4: the CLI subcommand for online invite minting + its AGENT_PROTOCOL.md entry (invariant 7)

| Field | Value |
| --- | --- |
| Public id | `ea948fb0-4b9c-43ba-84cf-803abefc0542` |
| Key | INVMINT-4 |
| Epic | [INVMINT](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | CLI |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T07:46:06.114378+00:00 |
| Updated | 2026-08-15T07:46:06.114378+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestInviteMintOnline' ./cmd/agent-busctl ./cmd/agent-bus && grep -q 'mint an invite without stopping the bus' AGENT_PROTOCOL.md && echo INVMINT4_OK
```

## Description

Ship the operator-facing command that drives INVMINT-3's route, plus its documentation. BLOCKED ON
INVMINT-3.

INVARIANT 7: nobody hand-writes HTTP -- the compiled Go CLI is THE client. Every capability ships with its
subcommand and its `AGENT_PROTOCOL.md` entry IN THE SAME TASK. A capability with no subcommand is the
missing half of the task, not a follow-up. `scripts/bus-*.sh` wrappers are RETIRED and only
`bus-serve.sh` survives -- DO NOT add one.

## Which binary?

Decide and justify in the code comment. The existing split is real and load-bearing:
- `agent-bus invite mint` is on the SERVER binary because its input is filesystem access (E4);
- `agent-bus healthcheck` is on the SERVER binary and is "deliberately not part of `agent-busctl`: an agent
  never runs it, since it needs no session, no enrolment and no identity" (`CONTRACTS-CLI.md:271-276`);
- `agent-busctl` holds the agent-facing surface and speaks pinned mTLS through the `client/` package.

An ONLINE mint needs a network client and the operator principal's credential, not filesystem access -- so
it does not obviously belong with the offline `invite mint`. Whichever you choose, the OFFLINE
`agent-bus invite mint` MUST KEEP WORKING UNCHANGED: it is the bootstrap path (there is no operator
credential before the bus has an identity) and it is the documented fallback.

## Requirements

- `--json` output, stable documented exit codes, never an interactive prompt (all three audiences:
  human interactive, agent shelling out, agent embedding -- which is why client code cannot live under
  `internal/`).
- The SECRET must not be printed unless explicitly requested, must never be logged, and the human output
  should name the invite's ID (safe to log) not its secret. Follow the existing `invite_id` precedent.
- If the command writes the invite blob to a file, it creates it `0600` -- do not make the operator
  discover the mode check the hard way (see INVMINT-7).
- Exit codes documented in `CONTRACTS-CLI.md`, alongside the existing `agent-bus invite mint` table.
- `AGENT_PROTOCOL.md` gains the entry. `CONTRACTS-CLI.md` gains the flags/exit codes.
  `CONTRACTS-HTTP.md` was updated by INVMINT-3.

## Proof

Two halves: the CLI test AND a doc line. The doc half pins the literal phrase
`mint an invite without stopping the bus` in `AGENT_PROTOCOL.md` -- a phrase chosen because it cannot match
incidentally. Baseline observed RED by spec-keeper at filing (2026-08-15):
`grep -c 'mint an invite without stopping the bus' AGENT_PROTOCOL.md` = 0. A grep proof that passes on an
incidental match elsewhere in the file is the more dangerous proof family; confirm RED before the fix.

PROOF STATUS -- READ THIS BEFORE COMPLETING. The test named in `proof_cmd` DOES NOT EXIST YET; writing it is
part of this task's deliverable, not a pre-existing artefact. `scripts/proof-check.sh` will report VACUOUS
until it is written, and THAT VACUOUS IS THE RED OBSERVATION -- record it before you start. If the design
lands under a different test or command name, have spec-keeper UPDATE this task's `proof_cmd` to the real
name; DO NOT complete the task behind a proof naming a test nobody wrote. That mechanism has produced 88
broken proofs in this backlog and closed 2 tasks on targets that never existed.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-3](../INVMINT-3--8555e659/task.md) — INVMINT-3: the invite-mint HTTP route on the running bus (NOT /v1/mint, which is message-… (todo)
- [INVMINT-7](../INVMINT-7--174c7ba9/task.md) — INVMINT-7: document the pre-minted-invite pool recipe, with the 0600 mode built in (quick… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-3](../INVMINT-3--8555e659/task.md) — INVMINT-3: the invite-mint HTTP route on the running bus (NOT /v1/mint, which is message-… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
