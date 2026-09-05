# agent-bus operator list mints wal-mac.key as a side effect of a read-only command

| Field | Value |
| --- | --- |
| Public id | `b5089ddf-5a5a-41e0-8278-036f6a195e2a` |
| Key | _(null in the export)_ |
| Epic | [AUTH](../epic.md) |
| Status | done |
| Priority | P2 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T14:14:52.446319+00:00 |
| Updated | 2026-08-23T00:05:10.041491+00:00 |
| Completed | 2026-08-23T00:05:10.041474+00:00 |

## Proof command

```sh
go test -race -run 'TestOperatorListMACKeyGuard|TestOperatorListMACKeyFixtureMintsControl' ./cmd/agent-bus
```

## Description

FILED 2026-08-21 by spec-keeper, from RELAY-54's security gate. SIBLING TASK: the identical defect
at `agent-bus peer list` -- `8cfd52e7-6cbd-4d29-b34d-c3dee87e73e5` (`agent-bus peer list`).

# The defect

`openOperatorRegistry`'s READ-ONLY path (`writable=false`) calls `wal.Replay` at
`cmd/agent-bus/operator.go:1149` with NO MAC-key guard.

`macKeyFor` (`internal/wal/mackey.go:218`) MINTS `wal-mac.key` AS A SIDE EFFECT OF A READ --
`wal.Replay` reaches it via `resolveCodec` -> `codecFor` -> `macKeyFor`, and `macKeyFor` calls
`createMACKey` whenever `loadMACKey` returns `ErrMACKeyMissing` and `macKeyMayBeCreated` says yes
(zero-length, unknown magic, or a header-only version 2 file). `ScanAll`/`Replay` take no logger, so
wal's "generated a new MAC key" line is suppressed and the creation is SILENT.

# THIS ONE SHIPPED WITH THE GAP RATHER THAN PREDATING THE FIX

`agent-bus log`'s guard landed with CLI-6's security gate. The operator principal was wired LATER --
`AUTH-10-WIRING`, commit `dc04a95` -- so `operator list` is NEWER than the fix and reintroduced a
closed defect class. Worth a line in the eventual `AGENT_LOG.md` entry: the CLI-6 fix was never
generalised into a rule that a new offline reader has to satisfy, so the next one will do it again.

# REPRODUCED at HEAD 1cc881f

Built `./cmd/agent-bus`, made a throwaway dir holding a valid `bus-id` (`bus-a`) and a 3-byte
`bus.wal` (`\x41\x47\x4e`), with NO `wal-mac.key`, then ran
`agent-bus operator list -data-dir <dir>`:

    exit=1
    before: bus-id bus.wal
    after : bus-id bus.lock bus.wal wal-mac.key      <-- 65-byte wal-mac.key MINTED
    agent-bus operator list: replaying the write-ahead log: wal: <dir>/bus.wal: corrupt at offset 0:
      truncated file header: have 3 of 48 bytes: unexpected EOF

# Why it matters

Integrity in this project is a keyed MAC (invariant 6). A READ-ONLY command that mints the key
SILENTLY MANUFACTURES THE AUTHORITY TO AUTHENTICATE THE VERY FILE IT IS ABOUT TO JUDGE. Every
positive test passes either way, which is exactly why this survives review.

Concretely, on a directory whose `bus.wal` is INTACT but whose key was lost, one run of this
read-only command converts a RECOVERABLE `wal.ErrMACKeyMissing` -- remedy "restore the key" -- into
`wal.ErrMACKeyMismatch`, whose DOCUMENTED remedy is to move `bus.wal` ASIDE. A read-only inspection
tool turns "restore a 64-byte file" into "destroy the write-ahead log". It also poisons the
directory for the real bus: a key minted now verifies nothing written under the real one.

The blast radius here is arguably worse than `peer list`'s: the operator registry is the
authorisation plane, so the file whose authenticity is being fabricated is the one that says who may
administer the bus.

# The fix -- copy a shape that already exists in THIS file

`cmd/agent-bus/operator.go` ALREADY has exactly the right guard shape, for the OTHER thing an offline reader must
never mint: `the bus-id presence check on the pre-lock/post-lock pair around `ids.LoadOrCreateBusID` (`operator.go:1104-1106`)` is called PRE-lock and RE-checked UNDER the lock, which is what makes
`ids.LoadOrCreateBusID`'s "Create" half unreachable. Verified at HEAD 1cc881f: pointed at a
directory with a 3-byte `bus.wal` and NO `bus-id`, ``agent-bus operator list`` exits 4 and the directory gains
NOTHING -- not even a `bus.lock`.

So the fix is to add the SAME PAIR for `wal-mac.key`:
  - PRE-lock, so a refusal writes nothing at all -- not even the `bus.lock` that
    `dirlock.Acquire` creates (a lone `bus.lock` in a virgin directory makes the operator's very
    first `agent-bus` start refuse to boot; see the comment above `checkPeerBusIDPresent`).
  - RE-checked UNDER the lock, because the pre-lock check races a concurrent delete. That second
    check is the load-bearing one.
  - REUSE `checkMACKeyPresent` (`cmd/agent-bus/auditlog.go:635`), already in package `main`, rather
    than copying it -- two copies of a security check drift. `outbox.go` reuses it behind a thin
    `outboxMACKeyGuard` adapter (`:689`) that only re-wraps the error type; do the same here.

## Exit code -- DO NOT blind-copy 5

`log` and `outbox` both use exit **5** for "unverifiable". `cmd/agent-bus/operator.go` ALREADY uses 5 for
``exitOperatorUnknown` (`operator.go:138`) -- "the named operator is not registered"`, so reusing it would give one code two documented meanings and break a caller that
scripts against it. Allocate the next free code (`exitOperatorUnverifiable = 6`) in that file's block, document it in
`CONTRACTS-CLI.md` and in the command's `-h` text, and say in the constant's comment why it is not 5.

# Test guidance -- LOAD-BEARING, read before writing the test

The fixture MUST use a **minting shape**. Verified empirically at HEAD by driving the compiled
binary: an ABSENT or ZERO-LENGTH `bus.wal` does NOT mint (`wal.Replay` takes its early empty-log
exit and never reaches `resolveCodec`). A NON-EMPTY GARBAGE-MAGIC log and a 3-BYTE TRUNCATED HEADER
both DO. A guard test built on an absent or zero-length `bus.wal` **cannot fail** -- it passes
identically with the guard deleted. This repo already carries four guard tests written against a
defect that could never fire; do not add a fifth.

Write TWO tests:
  1. THE GUARD TEST: fixture = valid `bus-id` + 3-byte `bus.wal` + NO `wal-mac.key`. Assert the
     command refuses with the new exit code, AND that after the run the directory contains NO
     `wal-mac.key` AND NO `bus.lock`.
  2. THE CONTROL TEST, which is what makes (1) meaningful: run an UNGUARDED `wal.Replay` over an
     IDENTICAL fixture and assert the key IS created. If the control ever goes green-without-minting
     the fixture has stopped exercising the defect and test (1) has silently become vacuous.

Confirm test (1) is RED before the fix. A proof never observed failing is not evidence.

# The other three offline readers

Four commands read a bus data directory offline. State at HEAD 1cc881f:
  - `agent-bus log`     -- **DONE** (CLI-6 security gate). `checkMACKeyPresent`, `auditlog.go:635`.
  - `agent-bus outbox`  -- **DONE** (RELAY-54). `outboxMACKeyGuard`, `outbox.go:689`, pre-lock at
    `:566` and post-lock at `:606`. NOTE: at the time of filing `cmd/agent-bus/outbox.go` is still
    UNTRACKED in the worktree and `agent-bus outbox` is not yet wired into the dispatcher, so this
    is "correct as written", not yet "correct as shipped" -- RELAY-54 is `in_progress`.
  - `agent-bus peer list`     -- **MISSING** (this defect class).
  - `agent-bus operator list` -- **MISSING** (this defect class).

These two are the LAST of the four to close.

# Provenance

Parent: RELAY-54 (`911841af-83d7-445f-bf46-9097eeb0661d`, `agent-bus outbox`), which is `in_progress` and NOT changed by this filing.
Sibling: `8cfd52e7-6cbd-4d29-b34d-c3dee87e73e5` -- agent-bus peer list, the same defect at the other call site. `relates` edges recorded both ways.

Filed from RELAY-54's security gate, which confirmed the class BY EXPERIMENT and correctly judged it
OUT OF RELAY-54's boundary. Re-reproduced independently at HEAD 1cc881f before filing.

# One correction to the brief this was filed from

The brief described the CLI-6 precedent as "`checkMACKeyPresent` called PRE-lock at
`auditlog.go:504`". That is NOT what `auditlog.go` does at HEAD. What runs pre-lock (`:505`) is
`checkAuditTrailPresent` -- a PRESENCE check on the artefact being read. `checkMACKeyPresent` is
called ONLY at `:547`, deliberately AFTER `dirlock.Acquire` (`:511`), with the reasoning given at
`:540-546`: the auth checks READ FILE CONTENTS so they must be serialised against a writer, and the
mistyped-directory case is already caught by the pre-lock presence check. CONFIRMED by experiment:
`agent-bus log` against a keyless directory exits 5, mints no key -- and DOES leave a `bus.lock`.
`outbox.go`'s pre-lock-AND-post-lock pair is the STRICTER shape and is the one to copy here.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [8cfd52e7-6cbd-4d29-b34d-c3dee87e73e5](../../RELAY/agent-bus-peer-list-mints-wal-mac.key-as-a-side-effect-o--8cfd52e7/task.md)
- **relates to** [RELAY-54](../../RELAY/RELAY-54--911841af/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [8cfd52e7-6cbd-4d29-b34d-c3dee87e73e5](../../RELAY/agent-bus-peer-list-mints-wal-mac.key-as-a-side-effect-o--8cfd52e7/task.md) — agent-bus peer list mints wal-mac.key as a side effect of a read-only command (todo)
- [AUTH-10-WIRING](../AUTH-10-WIRING--b11ef24c/task.md) — AUTH-10-WIRING: wire the operator principal into cmd/agent-bus/main.go — until this lands… (done)
- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (in_progress)
- [RELAY-54](../../RELAY/RELAY-54--911841af/task.md) — RELAY-54: an abandoned outbox job is invisible to every subcommand -- the drain a rollout… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [8cfd52e7-6cbd-4d29-b34d-c3dee87e73e5](../../RELAY/agent-bus-peer-list-mints-wal-mac.key-as-a-side-effect-o--8cfd52e7/task.md) — agent-bus peer list mints wal-mac.key as a side effect of a read-only command (todo)
- [a9bcdc54-fe1c-4497-9294-13efe2fca8fc](../../RELAY/agent-bus-outbox-bound-the-replay-tally-maps-which-are-k--a9bcdc54/task.md) — agent-bus outbox: bound the replay tally maps, which are keyed off attacker-influenced fi… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
