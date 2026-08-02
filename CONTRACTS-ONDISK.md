# Contracts: on-disk formats and record types

Split out of `CONTRACTS.md` (2026-08-02) — see that file for the index of all contract planes and
the rest of the surface (CLI/env, HTTP, agent-facing). This is a pure content move: everything below
this header is unchanged from the prior single-file `CONTRACTS.md`, verbatim.

**This is the plane most in flux** (DUR / on-disk format version 2 work is active) — that volatility
is the reason it got its own file rather than sharing one with a more stable plane.

## Record types / wire protocol versions

None yet — no durable store, no WAL record types, no wire protocol version exists in this wave.
When one is introduced: **reserve its number via
`POST /api/v1/projects/agent-bus/reservations`, never hand-pick it** — that is the standing rule
(`CLAUDE.md`, "Parallel-agent coordination") for record-type numbers, wire protocol versions, and
epic task keys alike, so two agents working in parallel can never collide on the same number.

## On-disk files in the data directory (added 2026-08-02)

`<data-dir>/bus.lock` (mode `0o600`, inside the `0o700` `-data-dir`) — an exclusive advisory lock
(`syscall.Flock(LOCK_EX|LOCK_NB)`, `internal/dirlock`) taken by `cmd/agent-bus`'s `run()` immediately
after `os.MkdirAll(cfg.DataDir, 0o700)` and BEFORE `ids.LoadOrCreateBusID` or anything else reads or
writes the data dir — so a WAL replay always happens inside the lock. Held for the process's
lifetime, released on clean shutdown (`Lock.Release`, deferred) and by the KERNEL on any death
(SIGKILL, panic, OOM kill) — the flock lives on the open file description, not on the path. Named
`bus.lock`, deliberately **not** `*.log`: `wal.log` is the WAL and `.gitignore` ignores `*.log`, so a
`.log`-suffixed lock file would be one typo/glob away from being mistaken for log data. It is NOT
durable state and NOT a record store — replay never reads it. Its only contents are a single
`<pid>\n` line, written (and fsynced) only AFTER the lock is held, so a refusal can name a probable
holder.

**Operator-facing failure mode:** a second `agent-bus` on the same `-data-dir` fails FAST — never
blocks, never proceeds — with exit code `1` and:
```
agent-bus: locking the data directory: dirlock: data directory "<dir>" is locked by another agent-bus process (pid N, best-effort: read from <dir>/bus.lock after the lock failed, so it may be stale); refusing to start — two servers on one data directory destroy the write-ahead log
```
`pid N` is best-effort/advisory only — read from the lock file *after* our own flock failed, so the
named process may already be gone. Treat it as a hint for `ps`, never as proof of a live holder.

**Stale locks: there are none.** A crash leaves the lock FILE but no LOCK (the kernel drops the
flock when the process dies), so the next start acquires it normally and simply overwrites the pid
line. `Release`/the package deliberately NEVER unlinks `bus.lock` — unlinking would let a starter
lock a fresh inode at the same path while another process still holds the old one, i.e. two
holders on one data directory, the exact failure this file exists to prevent. Operators must never
manually delete `bus.lock`, and never need to when no server is running against that directory.

**Limits:** advisory only — it excludes other processes that `flock` the same file (in practice,
other `agent-bus` servers), not `rm`, `cp`, an editor, or a backup job. Unreliable on NFS before
Linux 2.6.12 and on some network filesystems; a data dir on such a mount gets NO protection from
this lock.

`.gitignore` already ignores the default `./data/` dir wholesale, so `bus.lock` there is never
committed; a data dir at a non-default, non-ignored path is the operator's own responsibility.

No new route, CLI flag, env var, or header was introduced by this change — see the sections above,
which remain the complete index.

## The write-ahead log at startup (added 2026-08-02)

`cmd/agent-bus`'s `run()` now opens `internal/wal` with
`wal.Open(wal.LogOptions{Dir: cfg.DataDir, Logger: lg})`, creating `<data-dir>/bus.wal` (mode
`0o600`, a 16-byte file header) on first start. This is wiring only: the on-disk WAL format itself
is unchanged (see `PROTOCOL.md`) — this task connects the already-existing library to the server
binary, it does not add a record type or bump a format version.

**Startup order, which is the contract:** `os.MkdirAll(-data-dir, 0o700)` → `dirlock.Acquire`
(`bus.lock`, see above) → `ids.LoadOrCreateBusID` (`bus-id`) → `wal.Open` (which REPLAYS the file
before returning) → `net.Listen` → serve. `wal.Open` must run after the lock, because replay reads
the file and a torn-tail repair truncates bytes a second server could otherwise be appending to —
opening the log before locking would defeat the lock entirely. It must run before the listener
binds, because `wal.Open` does not return until replay has finished, so no request is ever served
from an unreplayed store (invariant 5: disk is the truth, memory is only the serving copy). Read that
guarantee precisely: what is enforced (and asserted) is that nothing is ever **answered** before
replay — `srv.Serve` starts after `wal.Open` returns. Nothing promises the socket is unbound during
replay; a listener that is bound but not yet served answers nothing.

**Honest limit of what "replay" means right now:** the `Applier` passed to `wal.Open` is `nil`.
There is no in-memory serving copy yet — `internal/store` is still a stub — so there is nothing for
a committed entry to be applied to. Replay today is a durability fsck: it verifies every frame,
resolves each prepare against its commit or discards it, and establishes the next-index high-water
mark, but it rebuilds no application state, because none exists. When the store lands it is passed
here as the `Applier`, and this line changes with it.

The opened `*wal.Log` is held for the process lifetime and passed to the HTTP layer as
`httpapi.Options.Durable` (new field; interface `httpapi.DurableLog`, one method,
`Write(wal.Entry) (wal.Committed, error)`; accessor `func (s *Server) Durable() DurableLog`, which
may return `nil`). **No handler and no route reads it yet** — `/healthz` and `/v1/info` are
unaffected — it is wired through now so the epics that add writing handlers have exactly one write
path to reach for (invariant 4), rather than each minting its own.

On shutdown the log is `Close()`d via a `defer` registered *after* the lock's own deferred release,
so Go's LIFO ordering closes the WAL (flushing and releasing its file handle) while the data
directory is still locked, and only releases the lock afterward — the reverse order would open a
window where a second `agent-bus` could acquire the directory while this process still held the WAL
open. A `Close` error does not change the process exit code but is logged at `ERROR` with the
`data_dir`, `path`, and the error, since it is a durability signal an operator should see. A
SUCCESSFUL close logs, at `DEBUG`, `msg="write-ahead log closed" data_dir=<dir> path=<dir>/bus.wal`.
That line is not decoration and must not be deleted as noise: it is the only observable proof the
close ran at all (the kernel closes the descriptor at process exit, so `bus.wal` is byte-identical
either way), and the tests assert it appears BEFORE `msg="data directory lock released"`, which is
what pins the close-then-unlock order described above.

**Failure mode: any open-or-replay failure is FATAL.** `run()` returns a non-nil error, `main()`
prints it to stderr prefixed `agent-bus: ` and exits `1`, and nothing binds a listener — the same
"fail fast, never degrade to an empty store" shape as the `bus.lock` failure above. The message is:
```
agent-bus: opening the write-ahead log in "<data-dir>": <wal error>
```
where `<wal error>` is whatever `internal/wal` reports — for example a corrupt file header reads
`wal: <data-dir>/bus.wal: corrupt at offset 0: bad magic "XXXXXXXX", want "AGNTBUSW"` (the exact
wording is set by `internal/wal/format.go`'s `corruptf`, not by `cmd/agent-bus`). **Recovery ALWAYS reaches a running server** (decision of 2026-08-02, invariant 6).
Damaged records are repaired in place where possible, and otherwise QUARANTINED — the unusable log
is moved aside with its bytes preserved on disk, and the bus starts. A bad magic, a wrong format
version, a commit naming no open prepare, or a payload that will not decode no longer refuse to
start; they are discarded, loudly.

This supersedes the previous wording, which said those cases "refuse to start". The absolute
requirement that replaced it is that **every discard is logged** — a silent discard is the actual
defect (rated P0), not the discard itself, because a server quietly serving an empty bus after
eating a log is indistinguishable to an operator from one that had nothing to serve. Reaching the
fatal path above now means recovery could not complete at all (an unreadable file, a failed
quarantine), not merely that the log was damaged.

**New INFO log line, asserted on by tests — treat its shape as part of this contract.** After a
successful open, `run()` logs one line naming what recovery found:
```
msg="write-ahead log opened" data_dir=<dir> path=<dir>/bus.wal records_replayed=<n> applied=<n> aborted=<n> dangling=<n> next_index=<n> repaired=<bool> repaired_bytes=<n> quarantined=<bool> discard_count=<n> discarded_bytes=<n>
```
The last three fields are load-bearing, not decoration: without them a whole-log QUARANTINE prints
`repaired=false next_index=1`, which is byte-identical to a brand-new empty bus — an operator could
not tell "your log was eaten" from "you have not sent anything yet". That was a P0. Any change
that drops them reintroduces silent data loss at the outermost layer.

This fires even for a brand-new, empty log (all-zero fields, `next_index=1`), so its presence is
proof a replay ran before the process served anything. `wal.Open` itself additionally emits its own
`msg="wal replayed"` line (only when the file held ≥1 record) plus a `WARN` per discarded dangling
prepare (a prepare that was fsynced but never committed — the signature of a crash between the two
phases) — see `internal/wal/log.go`. Both log lines are internal library output, not routes or
headers, but an operator relying on this file to confirm "the WAL loaded" should look for the
`cmd/agent-bus` line above.

**Test-only env vars, not supported configuration:** `cmd/agent-bus/wal_startup_test.go` reads
`AGENT_BUS_TEST_RUN_SERVER`, `AGENT_BUS_TEST_DATA_DIR`, `AGENT_BUS_TEST_LISTEN`, and
`AGENT_BUS_TEST_LOG_LEVEL` in its own `TestMain`, to re-exec the test binary as a real server for a
startup/crash test. The server binary (`cmd/agent-bus/main.go`) does not read any of them. This
does not add an entry to the "Env vars" section above, which remains empty.

No new HTTP route, CLI flag, production env var, header, or on-disk record type was introduced by
this change.
