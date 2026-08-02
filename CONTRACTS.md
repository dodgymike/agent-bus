# Contracts

Every route, CLI flag, env var, header, and durable record type agent-bus exposes. **Update this
file in the same commit that changes any of the surfaces below** (`CLAUDE.md` step 9). This is the
authoritative reference; `README.md` and `AGENT_PROTOCOL.md` summarise for humans/agents but this
file is where the exact shape lives.

## Routes

| Method | Path | Auth | Status | Response |
| --- | --- | --- | --- | --- |
| `GET` | `/healthz` | none | 200 | `{"status":"ok"}` |
| `GET` | `/v1/info` | none | 200 | `{"bus_id":"...","version":"...","uptime_seconds":0.0}` |
| other | `/healthz`, `/v1/info` | none | 405 | `{"error":"method not allowed"}`, `Allow: GET` |
| any | unregistered path | — | 404 | `net/http.ServeMux`'s built-in `text/plain` "404 page not found" — **not** the JSON error envelope. Known follow-up: CORE-8 (register a catch-all so unmatched paths get the same JSON envelope; must be decided together with AUTH-6's unauthenticated allow-list — whether an unauthenticated request to an unknown path should read 401 or 404). |

`HealthResponse` / `InfoResponse` / `ErrorResponse` types live in `internal/httpapi/server.go`.

`/v1/info`'s payload is deliberately minimal (see `DECISIONS.md`, 2026-08-02): `bus_id`, `version`,
`uptime_seconds` only. A test pins the exact field set — do not add data-dir, listen address, peer
list, or agent roster here without updating that test and recording the decision.

No route currently requires authentication because no authenticated route exists yet. **AUTH-6**
(filed as a follow-up) flags that routes are registered individually on the mux today, which is a
fail-open design once authenticated routes are added — read it before adding the next route.

## CLI flags (`cmd/agent-bus`)

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `:8080` | TCP address to bind, e.g. `:8080` or `127.0.0.1:8080` |
| `-data-dir` | `./data` | Directory for the durable store + append-only log; created `0700` if missing |
| `-poll-timeout` | `30s` | Ceiling on a single long-poll wait (not yet consumed by any handler) |
| `-log-level` | `info` | `debug`, `warn`, `info`, or `error` |
| `-bus-id` | *(empty → placeholder `bus-local`)* | **TEST-ONLY.** Validated against `^[A-Za-z0-9_-]{1,64}$`; `.` rejected (qualification separator, invariant 2). Using it logs a runtime `WARN`. See `DECISIONS.md`. |

Exit codes: `2` on invalid flags/config (`parseFlags`/`validate` failure), `1` on a startup failure
(e.g. bind failure), `0` on a clean signal-driven shutdown.

## Headers

| Header | Direction | Rule |
| --- | --- | --- |
| `X-Request-Id` | in/out | Inbound value accepted only if it matches `[A-Za-z0-9._-]{1,64}` (`httpapi.MaxRequestIDLen = 64`); otherwise replaced with a server-generated id (`crypto/rand` 16 hex chars, falling back to a `seq-<n>` counter). Always echoed on the response. |
| `Allow` | out | Set to `GET` on a 405 from `/healthz` or `/v1/info`. |
| `Content-Type` | out | `application/json; charset=utf-8` on every JSON response. |
| `X-Content-Type-Options` | out | `nosniff` on every JSON response. |

## Env vars

None yet. Every configuration knob today is a CLI flag; there is no env var surface to document.

## Record types / wire protocol versions

None yet — no durable store, no WAL record types, no wire protocol version exists in this wave.
When one is introduced: **reserve its number via
`POST /api/v1/projects/agent-bus/reservations`, never hand-pick it** — that is the standing rule
(`CLAUDE.md`, "Parallel-agent coordination") for record-type numbers, wire protocol versions, and
epic task keys alike, so two agents working in parallel can never collide on the same number.

## Agent-facing wrappers (`scripts/bus-*.sh`) and `AGENT_PROTOCOL.md`

No wrapper is due this wave. `/healthz` and `/v1/info` are operator/discovery surfaces, not agent
capabilities — invariant 7 ("every capability ships with a `scripts/bus-*.sh` wrapper and an
`AGENT_PROTOCOL.md` entry in the same task") is not yet triggered because no agent-facing capability
(enrol, send, wait, relay) exists yet. `AGENT_PROTOCOL.md` does not exist in the repo and should not
be created until the ENROL epic lands the first real agent-facing route.

## Repo tooling scripts (`scripts/*.sh`, NOT agent-facing)

These are maintainer/process tools, not bus capabilities. They are deliberately **not** `bus-*.sh`
and deliberately **not** in `AGENT_PROTOCOL.md`: invariant 7 governs the agent-facing bus surface
(enrol, send, wait, relay), and naming a process tool `bus-*.sh` would wrongly imply an agent calls
it to talk to a bus.

| Script | Purpose |
| --- | --- |
| `scripts/spec-cloud.sh` | Authed `curl` shim for the Spec Server (task state). See `CLAUDE.md`. |
| `scripts/proof-check.sh` | Runs a task's `proof_cmd` and refuses to call it a pass unless it demonstrated something. |

### `scripts/proof-check.sh` (added 2026-08-02)

**Why it exists.** Three ways a proof command exits 0 while proving nothing:

1. `go test -race -run TestThatDoesNotExist ./internal/wal` prints `ok … [no tests to run]` and
   **exits 0**. ~70% of this backlog's `proof_cmd` values have that shape, so a task could be
   flipped to `done` behind a proof whose named test was never written.
2. A test body that is just `t.Skip()` exits 0 with **no** `[no tests to run]` text at all, so
   grepping for that string does not catch it.
3. `A ; B` exits with `B`'s status and `|| true` swallows failure outright — so a **red** suite can
   sit behind a green exit code.

The script closes all three: it counts what actually ran rather than trusting the exit status.

```
scripts/proof-check.sh [--task <id>] [--classify] [--strict] [--quiet] '<proof command>'
```

| Option | Meaning |
| --- | --- |
| `--task <id>` | Fetch `proof_cmd` from the Spec Server (task key or `public_id`) via `scripts/spec-cloud.sh`, then check it. Requires `jq`. Id is validated against `^[A-Za-z0-9._-]+$` — it is interpolated into a URL that carries a bearer token. Note this *does* make a network call before classifying; it just never executes the proof. |
| `--classify` | Static classification only — **executes no part of the proof command**. |
| `--strict` | Additionally require *every* package listed in a `go test` invocation to contribute ≥1 test. Opt-in; reports `VACUOUS`/exit 4. |
| `--quiet` | Suppress the proof's own output; print only the verdict. |

| Env var | Default | Meaning |
| --- | --- | --- |
| `PROOF_CHECK_PROJECT` | `agent-bus` | Spec Server project slug used by `--task`. |

**Exit codes** (distinct on purpose — "I cannot check this" must never read as "this is broken"):

| Code | Verdict | Meaning |
| --- | --- | --- |
| `0` | `PASS` | Ran, exited 0, and if Go tests ran then ≥1 really ran, none failed, not all skipped |
| `1` | `FAIL` | Ran and exited non-zero, **or** ≥1 test failed behind an exit code that masked it |
| `2` | — | Usage error |
| `3` | `UNVERIFIABLE` | Cannot be checked: `n/a`, unfilled `<placeholder>`, invalid shell, a segment whose command does not exist (prose, or a wrapper not built yet), or a proof naming `go test` whose test output was never captured (absolute-path `go`, scrubbed `PATH`). **Not** a claim the work is broken. |
| `4` | `VACUOUS` | Exited 0 but proved nothing: zero tests ran, or every test that ran skipped |

Stdout carries **only** the machine-readable verdict line. All human output *and the proof's own
output* go to stderr, so a proof cannot print a convincing forgery of the verdict onto stdout. The
exit code, not the text, is authoritative.

```
proof-check: verdict=<PASS|FAIL|VACUOUS|UNVERIFIABLE> class=<tags> exit=<n> tests_run=<n> top_level=<n> skipped=<n> failed=<n> empty_pkgs=<n>
```

**Non-Go proofs are first-class.** `test -s PROTOCOL.md`, `grep -q … FILE`, `scripts/bus-*.sh …`,
`docker compose …` are legitimate proofs and are judged **purely on exit status** — they are never
forced through a test-count check.

**Decided: multi-package `-run` misses are a PASS.** `go test -race -run TestX ./internal/auth
./internal/httpapi`, where the pattern matches in one package and not the other, passes with a
warning naming the empty packages. `./internal/...` expands to a dozen packages of which two ever
match, so requiring all of them would fail almost every legitimate proof here — the false-negative
mode that gets guards switched off. The trap being closed is *zero tests anywhere*. `--strict`
opts into the stricter rule per-proof.

**Trust boundary.** A `proof_cmd` is executable input: the script runs it verbatim, with your
privileges and your full environment, in the repo root. With `--task` the string comes from the Spec
Server, so anyone who can edit that backlog can choose a command that runs on your machine. Use
`--classify` to inspect statically without executing. The echoed `proof:` line has non-printable
bytes replaced with `?`, so ANSI escapes cannot repaint it to hide what will run.

**Known limitations.**

- To count tests, `go test` runs through a `go` shim (installed on `PATH` for every run, so
  indirectly-invoked tests are still counted) that injects `-v` and merges stderr into stdout;
  `go build`/`go vet`/`go list` pass through untouched. A proof that parses non-verbose `go test`
  output, or redirects the two streams separately, will see different text than standalone. Nothing
  in the current backlog does.
- The empty-package warning is per output line, not per listed package, and the multi-package
  allowance generalises from one invocation to the whole proof: `go test -run TestOld ./a &&
  go test -run TestNew ./b` PASSES with a warning even when `TestNew` does not exist. **The
  completing agent must read the warning line, not just the verdict.**

**Policy (recommendation, not yet enforced):** completion should *require running* `proof-check.sh
--task <id>` and quoting its verdict line in `test_summary`, while `proof_cmd` stays stored as the
bare command — a proof that only runs inside our harness is a worse artifact than one anyone can
paste into a shell. Nothing in the tool can enforce this; its value is an auditable verdict line.
Full rationale and tradeoffs are in the comment block at the top of the script.

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
wording is set by `internal/wal/format.go`'s `corruptf`, not by `cmd/agent-bus`). The only damage
`wal.Open` repairs automatically is a provably torn tail — a frame whose checksum failed because a
crash landed mid-append (`RepairTail`, truncating back to the last verified-good record and
fsyncing that). Anything else — a bad magic, a wrong format version, a commit naming no open
prepare, a payload that will not decode — refuses to start and leaves the file byte-for-byte
unchanged; there is no silent empty start, and no second-guessing the repair decision at the
`cmd/agent-bus` layer.

**New INFO log line, asserted on by tests — treat its shape as part of this contract.** After a
successful open, `run()` logs one line naming what recovery found:
```
msg="write-ahead log opened" data_dir=<dir> path=<dir>/bus.wal records_replayed=<n> applied=<n> aborted=<n> dangling=<n> next_index=<n> repaired=<bool> repaired_bytes=<n>
```
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
