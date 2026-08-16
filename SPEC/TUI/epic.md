# EPIC TUI — TUI: a user-facing TERMINAL interface — administrate, monitor, and communicate as a person

[← all epics](../../SPEC.md)

**6 open / 6 total.** Full records live in `SPEC/TUI/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (6)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| TUI-1 | TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or com… | todo | P2 | [task.md](TUI-1--3ea68265/task.md) | _not fetched_ | [TUI-3](TUI-3--140aadf7/task.md) [TUI-4](TUI-4--11898d9b/task.md) [TUI-5](TUI-5--b2a44ce9/task.md) [TUI-6](TUI-6--cb4e3fd7/task.md) [ADMIN-3](../ADMIN/ADMIN-3--76bfce36/task.md) [INVMINT-1](../INVMINT/INVMINT-1--1bed65a8/task.md) +5 more |
| TUI-2 | TUI-2: DECIDE the TUI rendering dependency — this would be the project's FIRST third-part… | todo | P2 | [task.md](TUI-2--4b669f76/task.md) | _not fetched_ | [TUI-1](TUI-1--3ea68265/task.md) [TUI-3](TUI-3--140aadf7/task.md) [DEPLOY-4](../DEPLOY/DEPLOY-4--48b5d5b4/task.md) |
| TUI-3 | TUI-3: GUARD — the TUI sits on the client package and cannot bypass it, and agent-busctl'… | todo | P2 | [task.md](TUI-3--140aadf7/task.md) | _not fetched_ | [TUI-1](TUI-1--3ea68265/task.md) [CLI-4](../CLI/CLI-4--137465b9/task.md) [TUI-4](TUI-4--11898d9b/task.md) [INVMINT-6](../INVMINT/INVMINT-6--cedb8d6f/task.md) |
| TUI-4 | TUI-4: the read-only MONITOR view — renders LIVE/ADMIN data, re-specifies none of it | todo | P3 | [task.md](TUI-4--11898d9b/task.md) | _not fetched_ | [TUI-1](TUI-1--3ea68265/task.md) [TUI-3](TUI-3--140aadf7/task.md) [LIVE-1](../LIVE/LIVE-1--354e378c/task.md) [LIVE-6](../LIVE/LIVE-6--5825cf57/task.md) [LIVE-8](../LIVE/LIVE-8--742dd0ec/task.md) [ADMIN-8](../ADMIN/ADMIN-8--7f550309/task.md) +5 more |
| TUI-5 | TUI-5: the human as a bus PARTICIPANT — read and send messages as a person (message bodie… | todo | P3 | [task.md](TUI-5--b2a44ce9/task.md) | _not fetched_ | [TUI-1](TUI-1--3ea68265/task.md) [TUI-3](TUI-3--140aadf7/task.md) [CLI-4](../CLI/CLI-4--137465b9/task.md) [CLI-3-FU-SAFETEXT](../CLI/CLI-3-FU-SAFETEXT--e4baf8c5/task.md) [TUI-4](TUI-4--11898d9b/task.md) [INVMINT-6](../INVMINT/INVMINT-6--cedb8d6f/task.md) |
| TUI-6 | TUI-6: "and other people" — multiple humans as distinct enrolled identities, DMs between… | todo | P3 | [task.md](TUI-6--cb4e3fd7/task.md) | _not fetched_ | [TUI-1](TUI-1--3ea68265/task.md) [TUI-3](TUI-3--140aadf7/task.md) [LIVE-7](../LIVE/LIVE-7--09bc72d0/task.md) [INVMINT-6](../INVMINT/INVMINT-6--cedb8d6f/task.md) |

## Closed tasks (0) — done, cancelled, superseded

_None._

## Epic description

A user-facing TERMINAL interface to administrate, monitor, and instruct/communicate with agents AND OTHER
PEOPLE. Operator request, verbatim, 2026-08-15: "a user-facing terminal interface to administrate, monitor,
instruct / communicate with agents and other people". FILED ONLY -- not started. Critical path is relay egress.

## EXTEND-vs-NEW RULING (spec-keeper, 2026-08-15): NEW EPIC, DELIBERATELY NARROW

This was checked against ADMIN, CLI, COMMS and LIVE before filing, by searching all 621 task files in the
`SPEC/` mirror (NOT the task-list API, which silently truncates to 200). Prior art for a terminal UI:
`grep -rliE '\bTUI\b|bubbletea|tcell|ncurses|curses|terminal user interface' SPEC/` returns exactly ONE
file, and it is a `proof-check.sh` recursion bug -- an incidental match. There is NO prior art. Likewise
`human participant|humans on the bus|person on the bus|chat` returns ZERO files.

Why NOT extend each candidate:

- **ADMIN is a BROWSER console, not a terminal one.** Its ruling D1 is explicit and was ruled, not
  defaulted: "plaintext HTTP on loopback + a per-process capability token, strict Origin / Sec-Fetch-Site
  checks, plus a 0600 unix socket for non-browser access... this is a console surface, not a bus surface, and
  TLS here would reintroduce the browser trust-store problem the architecture exists to eliminate." The epic
  ships `agent-busadm serve` and "serves a read-first UI over loopback so that no browser ever terminates a
  pinned connection". A terminal interface is a DIFFERENT SURFACE with different trust properties (no browser,
  no origin checks, no capability token in a URL).
- **ADMIN explicitly EXCLUDES the two things that make this request new.** Its "EXPLICITLY NOT COVERED" list
  names "message bodies" and "multi-user auth". The operator asked to "instruct / communicate with agents and
  other people" -- that IS message bodies, and "other people" IS more than one human. Those are outside
  ADMIN's charter by its own text, not by oversight.
- **CLI (`agent-busctl`) is NON-INTERACTIVE BY CONTRACT and must stay so.** CLI-4 (done) states the standing
  rule for all three audiences: "an AGENT SHELLING OUT (--json, stable exit codes, NO interactive prompts, NO
  TTY-dependent credential input)". CLI-4's word "interactive" means reading a message body from stdin, NOT a
  full-screen UI. A TUI cannot be bolted onto `agent-busctl` without endangering that guarantee -- see TUI-3.
- **LIVE and ADMIN-8 are the DATA SOURCE, not the UI.** "Monitor" is mostly what LIVE already specifies
  (liveness contract, status subscriptions, `GET /v1/status`). This epic DEPENDS on them and MUST NOT
  re-specify them. If a monitoring capability is missing, the task belongs in LIVE or ADMIN, not here.
- **COMMS is measurement, not a surface.** It measures inter-agent message quality against a corpus. No
  overlap beyond vocabulary.

So: NEW epic for the TERMINAL SURFACE and the HUMAN-PARTICIPANT semantics. Everything else is a dependency.

## THE FIRST QUESTION IS WHETHER THIS REPLACES ADMIN'S BROWSER CONSOLE (TUI-1)

ADMIN has 14 OPEN tasks and its console is unbuilt. The operator has now asked for a TERMINAL interface
covering "administrate, monitor" -- which is ADMIN's core. Building a browser console AND a TUI over the same
data is duplicated effort on 14 open tasks, and D1 was an explicit operator ruling that a NEW operator request
may or may not be intended to supersede. THIS IS NOT SPEC-KEEPER'S CALL TO MAKE SILENTLY. TUI-1 puts it to
the operator and blocks the rest of the epic. Three outcomes are all legitimate: the TUI REPLACES the browser
console (ADMIN-3/D1 superseded, several ADMIN tasks re-homed); the TUI COMPLEMENTS it (both ship, sharing the
client package); or the TUI is a THIN VIEW and ADMIN stays the console. Do not start TUI-3..6 before this
lands.

## INVARIANT 7 GOVERNS THIS EPIC ABSOLUTELY

The compiled Go CLI is THE client; nobody hand-writes HTTP. Three audiences are ALL requirements: a human
interactively; an agent shelling out (`--json` everywhere, stable documented exit codes, NEVER an interactive
prompt); and an agent embedding it -- which is why the client package CANNOT live under `internal/`.

**A TUI IS A FOURTH SURFACE AND MUST NOT BECOME A WAY TO BYPASS THAT.** It sits ON the `client/` package; it
does not reach past it to hand-built HTTP, and it does not grow a second, divergent implementation of enrol /
send / wait. `scripts/bus-*.sh` wrappers are RETIRED and adding one is FORBIDDEN (invariant 7); only
`bus-serve.sh` survives. This is mechanically enforceable -- see TUI-3.

## THE INTERACTIVITY TENSION, NAMED

A TUI is interactive by definition. `agent-busctl` is forbidden from ever prompting. These are not reconcilable
in one binary by good intentions; they are reconciled by SEPARATION (a different binary, or a mode that cannot
be reached non-interactively) plus a GUARD TEST that fails if the non-interactive guarantee regresses. The
guarantee that must survive: an agent shelling out to `agent-busctl` in a pipeline NEVER blocks on a prompt
and never depends on a TTY.

## MESSAGE BODIES: THE INVARIANT-6 DISTINCTION THAT MUST NOT BE FUMBLED

Invariant 6 says THE APPEND-ONLY LOG records metadata and routing ONLY, never message bodies. It does NOT say
a client may not see bodies -- every agent reads its own inbox bodies over `/v1/wait` today, and a human
participant doing the same is ORDINARY, not a violation. The violation would be sourcing other principals'
bodies from the LOG or the audit view. State which side of that line every view sits on. ADMIN-5 already
established the safe pattern: the console reads from its OWN long-poll and sees metadata.

## THIRD-PARTY DEPENDENCY: THIS WOULD BE THE PROJECT'S FIRST

`go.mod` today declares `module github.com/dodgymike/agent-bus` and `go 1.19` and NOTHING ELSE -- ZERO
third-party dependencies. A realistic TUI wants `bubbletea`/`tcell`/`termbox`, which would make this the first
external dependency in the entire project. Invariant 8 requires a `DECISIONS.md` justification for any
third-party dependency, and invariant 9's "never write your own crypto" does NOT extend a blanket licence
here. TUI-2 decides it explicitly, including the stdlib-only option. Do not let a library arrive as an
incidental import in a feature task.

## IN THIS EPIC vs STAYS ELSEWHERE

IN: the terminal surface; the client-package boundary guard; the human-as-participant semantics (reading and
sending message bodies as a person); multiple humans as distinct enrolled identities.
STAYS IN ADMIN: the browser console, telemetry leases, the allow-list authorisation model, the audit reader.
STAYS IN CLI: every non-interactive subcommand and the `--json` contract.
STAYS IN LIVE: the liveness contract, heartbeats, status subscriptions.
STAYS IN INVMINT: minting invites without stopping the bus.
OUT: any new bus route invented solely for the UI; any privilege tier inside the bus; remote/multi-user
console auth (ADMIN excluded it and this epic does not reopen it).

Reservations: epic key `TUI` = `epic-key` #6; task keys TUI-1..TUI-6 = `task-key-TUI` #1..#6 (fresh namespace,
deliberately unseeded). Filed by spec-keeper 2026-08-15, FILE ONLY.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
