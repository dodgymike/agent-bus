# TUI-3: GUARD — the TUI sits on the client package and cannot bypass it, and agent-busctl's never-prompt guarantee survives

| Field | Value |
| --- | --- |
| Public id | `140aadf7-00d3-4fac-9aa0-3d064e29d1a5` |
| Key | TUI-3 |
| Epic | [TUI](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | CLI |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:00:49.192341+00:00 |
| Updated | 2026-08-15T08:00:49.192341+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestTUIUsesClientPackageOnly|TestBusctlNeverPrompts' ./client/... ./cmd/... && echo TUI3_GUARDS_OK
```

## Description

Make the two structural properties of this epic MECHANICALLY ENFORCED, before any UI is built on top of them.
BLOCKED ON TUI-1 (which decides the binary layout the guard must police).

This is the highest-value task in the epic and the one with a genuinely provable proof. Do it FIRST among the
code tasks.

## Property 1 — the TUI does not reach past `client/`

Invariant 7: nobody hand-writes HTTP; the compiled Go CLI is THE client; the client package cannot live under
`internal/` precisely so it can be embedded. A TUI is a FOURTH surface (after human CLI, agent shelling out,
agent embedding) and must not become the place where a second, divergent implementation of enrol/send/wait
grows. Assert that the TUI package imports no `net/http` request construction against the bus and no
`internal/httpapi` route constants -- it goes through `client/`.

## Property 2 — `agent-busctl` never prompts

CLI-4 (done) states the contract for all three audiences: "an AGENT SHELLING OUT (--json, stable exit codes,
NO interactive prompts, NO TTY-dependent credential input)". A TUI is interactive by definition. **These are
not reconcilable by good intentions in one binary; they are reconciled by SEPARATION plus a guard that fails
if the guarantee regresses.** Assert that no non-TUI command path reads from a terminal, blocks on stdin for a
credential, or changes behaviour based on `IsTerminal`.

## USE THE EXISTING AST-GUARD PRECEDENT — DO NOT INVENT A NEW MECHANISM

`client/guard_test.go` already implements exactly this pattern for invariant 11 (`TestNoInsecureSkipVerify
Anywhere`, policing `InsecureSkipVerify` outside `client/pin.go`). Read it first and copy its discipline,
especially these two properties, which are what make a guard trustworthy rather than decorative:
- **it walks the guard roots and FAILS IF IT VISITED NO FILES** -- "a guard that inspected nothing passes" is
  the failure mode, and that file names it explicitly;
- **the guard file itself is skipped** (it must name the strings it bans in order to ban them) while EVERY
  other file, including every other test file, is still scanned and counted.
A grep-based guard is NOT acceptable here: it cannot distinguish an import from a comment, and this repo has
already been bitten by grep proofs passing on incidental matches.

## Proof

Both guards in one command. This proof is REAL: it fails today because neither test exists, and it will fail
again the day someone regresses either property -- which is the whole point of a guard. Prefer this shape over
any proof that asserts a terminal renders correctly (see TUI-4).

## PROOF STATUS -- READ BEFORE COMPLETING

The test named in `proof_cmd` DOES NOT EXIST YET; writing it is part of this task's deliverable, not a
pre-existing artefact. `scripts/proof-check.sh` reports VACUOUS until it is written, and THAT VACUOUS IS THE
RED OBSERVATION -- record it before starting. If the design lands under a different name, have spec-keeper
UPDATE this `proof_cmd`; never complete behind a proof naming a test nobody wrote (88 broken proofs in this
backlog, 2 tasks closed on targets that never existed). Do NOT put `<angle brackets>` in a proof_cmd --
proof-check.sh classifies them as an unfilled template and REFUSES TO RUN IT (caught on INVMINT-6).

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-4](../../CLI/CLI-4--137465b9/task.md) — CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadc… (done)
- [INVMINT-6](../../INVMINT/INVMINT-6--cedb8d6f/task.md) — INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… (todo)
- [TUI-1](../TUI-1--3ea68265/task.md) — TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or com… (todo)
- [TUI-4](../TUI-4--11898d9b/task.md) — TUI-4: the read-only MONITOR view — renders LIVE/ADMIN data, re-specifies none of it (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [TUI-1](../TUI-1--3ea68265/task.md) — TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or com… (todo)
- [TUI-2](../TUI-2--4b669f76/task.md) — TUI-2: DECIDE the TUI rendering dependency — this would be the project's FIRST third-part… (todo)
- [TUI-4](../TUI-4--11898d9b/task.md) — TUI-4: the read-only MONITOR view — renders LIVE/ADMIN data, re-specifies none of it (todo)
- [TUI-5](../TUI-5--b2a44ce9/task.md) — TUI-5: the human as a bus PARTICIPANT — read and send messages as a person (message bodie… (todo)
- [TUI-6](../TUI-6--cb4e3fd7/task.md) — TUI-6: "and other people" — multiple humans as distinct enrolled identities, DMs between… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
