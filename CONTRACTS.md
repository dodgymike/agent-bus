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
| `POST` | `/v1/enroll` | none (unauthenticated by necessity — this is how the credential is obtained; only registered when `Options.Auth != nil`, see AUTH-1 section below) | 201 | `{"agent_id":"...","bus_id":"...","name":"...","enrolled_at":"<RFC3339Nano UTC>"}` — the SAME body, byte for byte, on an idempotent replay (see `Idempotency-Replayed` header) |
| `POST` | `/v1/enroll` | none | 400 | invalid `name`; invalid `public_key` (not base64, or not exactly the 32-byte Ed25519 public key size); invalid `idempotency_key` (empty, over 128 bytes, or a byte outside `[A-Za-z0-9._-]`) |
| `POST` | `/v1/enroll` | none | 409 | `idempotency_key` reused with a **different** `name`/`public_key` than its first use — a protocol violation, not a retry (invariant 10); response carries `Connection: close` (see `## Headers`) |
| `POST` | `/v1/enroll` | none | 503 | the roster (default 4096 entries) or the idempotency table (default 16384 entries) is at capacity; `Retry-After: 5` |
| `POST` | `/v1/session/begin` | none (issues the challenge; only registered when `Options.Auth != nil`) | 200 | `{"agent_id":"...","token":"...","challenge_expires_at":"<RFC3339Nano UTC>"}` |
| `POST` | `/v1/session/begin` | none | 404 | `agent_id` is malformed **or** well-formed but not on this bus's roster — the two cases are deliberately indistinguishable to the caller |
| `POST` | `/v1/session/begin` | none | 503 | the session table (default 16384 entries, pending + active together) is at capacity; `Retry-After: 5` |
| `POST` | `/v1/session/complete` | none (activates the credential; only registered when `Options.Auth != nil`) | 200 | `{"agent_id":"...","expires_at":"<RFC3339Nano UTC>","lifetime_seconds":3600,"refresh_after_seconds":2700}` |
| `POST` | `/v1/session/complete` | none | 400 | `signature` is not valid base64; also returned if the roster holds a corrupt (wrong-length) public key for the agent (defence in depth — see `internal/auth/session.go`) |
| `POST` | `/v1/session/complete` | none | 401 | the signature does not verify against the agent's enrolled public key, or is not exactly the 64-byte Ed25519 signature size |
| `POST` | `/v1/session/complete` | none | 404 | `token` names no session (never existed, already expired, or was dropped after a prior failed verification), or a pending/active session has passed its deadline — again deliberately indistinguishable to the caller |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 400 | malformed JSON, an unrecognised field, or trailing content after the one JSON value the body must contain |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 405 | any method but `POST`; `Allow: POST` |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 413 | request body exceeds `httpapi.MaxAuthRequestBytes` (8 KiB) |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 415 | `Content-Type` is not `application/json` (a `charset` parameter is accepted) |
| any | any path off the five-entry allow-list (`/healthz`, `/v1/info`, `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete`) | `Authorization: Bearer <token>` required — see `## Authentication` below | 401 | `{"error":"authentication required"}` when no usable credential was presented at all (missing or duplicate `Authorization` header, a scheme other than `Bearer`, an empty/spaced/oversized/non-base64url token — `WWW-Authenticate: Bearer realm="agent-bus", error="invalid_request"`), or `{"error":"invalid or expired credential"}` when a well-formed token failed to authenticate (unknown, pending, or expired — deliberately indistinguishable, see `## Authentication` — `WWW-Authenticate: Bearer realm="agent-bus", error="invalid_token"`) |
| any | unregistered path, no credential (or one that does not authenticate) | — | 401 | `authMiddleware` wraps the whole mux and refuses before the mux is ever consulted, so an anonymous caller cannot enumerate which paths this bus serves by probing unknown ones; same body/header shape as the row above |
| any | unregistered path, valid bearer token | valid bearer token | 404 | `net/http.ServeMux`'s built-in `text/plain` "404 page not found" — **not** the JSON error envelope — because the middleware let the request through and the mux, honestly, has no route there. Known follow-up: CORE-8 (register a catch-all so unmatched paths get the same JSON envelope); that catch-all MUST be registered INSIDE the auth wrapper (through `(*Server).route`, so it is itself subject to `authMiddleware`) or it becomes the one unauthenticated route that leaks the surface. This 404 is also what `/v1/enroll`, `/v1/session/begin`, and `/v1/session/complete` return when the server was built with `Options.Auth == nil` — those three stay on the allow-list unconditionally (see the AUTH-1 section below), so they reach the mux with or without a credential and 404 there like any other unregistered path. |

`HealthResponse` / `InfoResponse` / `ErrorResponse` types live in `internal/httpapi/server.go`. `EnrolRequestBody` / `EnrolResponseBody` / `SessionBeginRequestBody` / `SessionBeginResponseBody` / `SessionCompleteRequestBody` / `SessionCompleteResponseBody` live in `internal/httpapi/auth.go`.

`/v1/info`'s payload is deliberately minimal (see `DECISIONS.md`, 2026-08-02): `bus_id`, `version`,
`uptime_seconds` only. A test pins the exact field set — do not add data-dir, listen address, peer
list, or agent roster here without updating that test and recording the decision.

**Authentication is now default-deny across the whole mux** (AUTH-2, with AUTH-6's fail-open fix
folded into the same change). `authMiddleware` wraps `s.handler` before any route is dispatched, so a
route is authenticated the moment it is registered through `(*Server).route` — nobody has to remember
to protect it individually, which closes the exact risk AUTH-6 flagged (routes wired one at a time,
easy to forget on the next addition). The allow-list is exactly the five paths named in the routes
above; see `## Authentication` further down for the full contract.

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
| `Authorization` | in | Required on every route off the five-entry allow-list (`## Authentication`). Exactly one header, form `Bearer <token>` (scheme case-insensitive); `<token>` must be non-empty, contain no space, be no longer than `httpapi.MaxBearerTokenLen` (512), and consist only of the base64url alphabet `[A-Za-z0-9_-]`. Zero headers, more than one, a non-`Bearer` scheme, or a token failing any of those checks is treated as "no usable credential" (401, `error="invalid_request"`) — distinct from a syntactically fine token that simply does not authenticate (401, `error="invalid_token"`). Never logged, echoed, truncated or hashed into any response or log line — only the resulting agent id ever leaves `authMiddleware`. |
| `WWW-Authenticate` | out | On every 401: `Bearer realm="agent-bus", error="invalid_request"` when no usable credential was presented, or `Bearer realm="agent-bus", error="invalid_token"` when a well-formed token failed to authenticate (unknown, pending, or expired — the three are deliberately indistinguishable to the caller). |
| `Allow` | out | Set to `GET` on a 405 from `/healthz` or `/v1/info`. |
| `Content-Type` | out | `application/json; charset=utf-8` on every JSON response. |
| `X-Content-Type-Options` | out | `nosniff` on every JSON response. |
| `Idempotency-Replayed` | out | `true` on `POST /v1/enroll`'s 201 when the response was replayed from the idempotency table rather than freshly applied. The BODY is byte-identical to the original either way — the header is the only out-of-band signal that this call re-applied nothing. |
| `Connection` | out | `close` on `POST /v1/enroll`'s 409 (idempotency key reused with a different payload). Invariant 10: same key + different payload is a protocol violation, and the server disconnects the offending client. Contrast the same-key/same-payload case, which is a legitimate retry, returns the original 201 unchanged, and is never disconnected or otherwise punished. |
| `Retry-After` | out | `5` (seconds) on a 503 from any of the three auth routes (a roster, idempotency-table, or session-table capacity limit). Short deliberately: every cap in `internal/auth` is a live in-memory bound that a departing agent or an expiring session can relieve within seconds. |
| `Allow` | out | Also set to `POST` on a 405 from `/v1/enroll`, `/v1/session/begin`, or `/v1/session/complete`. |
| `Cache-Control` | out | `no-store` on `POST /v1/session/begin` only. That response body carries a LIVE credential (the session token); the other two auth responses carry none, so the header is deliberately not set on them and its presence stays meaningful. |

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

## Enrolment and sessions (added 2026-08-02)

AUTH-1 adds the three credential-issuing routes documented in `## Routes` and `## Headers` above:
`POST /v1/enroll`, `POST /v1/session/begin`, `POST /v1/session/complete`. This section is the prose
that does not fit a table row. No `scripts/bus-*.sh` wrapper and no `AGENT_PROTOCOL.md` entry are
added by this task — invariant 7 was amended so a Go CLI replaces the shell wrappers, and wiring
these routes to that agent-facing surface is a separate, later task. Do not infer a wrapper or CLI
subcommand exists for enrolment or sessions from this document.

**`Options.Auth` gates route registration, not route behaviour.** `internal/httpapi.Options.Auth`
(`*auth.Service`) has no default and is `nil` unless the caller supplies one. When it is `nil`, `New`
does not register `/v1/enroll`, `/v1/session/begin`, or `/v1/session/complete` on the mux at all —
they 404 through the same `net/http.ServeMux` catch-all as any other unknown path, not a 503. That is
deliberate: a route that exists and refuses is a claim that the capability is present, and a server
built without an auth service does not have it. `cmd/agent-bus`'s `run()` always constructs one
(`auth.NewService(auth.Options{Minter: minter})`), so the shipped binary always registers these three
routes; a `nil` `Options.Auth` is reachable only by a caller of the `httpapi` package directly (tests,
or a future build that intentionally omits the auth surface).

**The signing contract — load-bearing for any future client.** `POST /v1/session/complete`'s
`signature` field is an Ed25519 signature over the exact byte string:

```
auth.SessionSigningContext + token
```

where `SessionSigningContext = "agent-bus:session-token:v1:"` (quote it exactly — it is a Go string
constant in `internal/auth/session.go`, concatenated directly onto `token` with no separator) and
`token` is the literal string returned as `token` in the `/v1/session/begin` response. A future client
implementation **must pin `SessionSigningContext` as a compile-time constant** and must **not** learn
it from the wire: the `/v1/session/begin` response deliberately does not echo this prefix anywhere in
its body, precisely so a man-in-the-middle who could choose what gets signed cannot turn the agent's
key into a signing oracle for arbitrary bytes. `public_key` (on `/v1/enroll`) and `signature` (on
`/v1/session/complete`) are both `base64.StdEncoding` — the **standard, padded** alphabet, decoded
`Strict()` server-side so a value has exactly one valid spelling. (The `token` value itself uses a
different encoding, `base64.RawURLEncoding` — unpadded, URL-safe — since it is minted server-side and
only ever needs to round-trip, never to be independently re-encoded by a client.)

**Session lifetime constants** (`internal/auth/session.go`):

| Constant | Value | Meaning |
| --- | --- | --- |
| `SessionLifetime` | 1 hour | How long an ACTIVE session is valid, from the instant its challenge was completed. A **ceiling**, not a default to raise — the whole argument for a bearer token in place of per-request signing rests on a stolen token going stale soon. |
| `SessionRefreshFraction` | 0.75 | Where in a session's life a well-behaved client should begin its next challenge: 75% of `SessionLifetime`, leaving a quarter of the lifetime as slack. Surfaced on the wire as `refresh_after_seconds` (2700 at the default lifetime) in the `/v1/session/complete` response — advice only, not enforced. |
| `ChallengeTTL` | 2 minutes | How long an issued-but-unsigned token stays completable; surfaced as `challenge_expires_at` in the `/v1/session/begin` response. |
| `TokenRandBytes` | 32 | Bytes of `crypto/rand` entropy in a session token. |

**Server-side expiry is authoritative, with NO clock-skew grace.** `auth.Service.Authenticate` (the
seam AUTH-2's middleware will wrap; nothing enforces it on any route yet) checks `ExpiresAt` against
the server's own clock on every call — a client's opinion of the time never enters into it, because a
grace window is just a longer lifetime with a less honest name. `ExpiresAt` is set exactly **once**,
at the first successful `POST /v1/session/complete`, and is **never** extended by re-completing an
already-active session: a repeat completion re-verifies the signature and returns the identical
`expires_at`, so a client cannot hold one session open indefinitely off a single signature.

**Admission-control caps** (`internal/auth/service.go` `Options`, all overridable, `0` means "use the
default", there is no "unlimited"):

| Cap | Default | Behaviour at the cap |
| --- | --- | --- |
| `MaxRosterEntries` | 4096 | **Fails closed**: `POST /v1/enroll` returns 503 (`ErrCapacity`, `Retry-After: 5`). Never evicts a roster entry — evicting one would let an already-enrolled agent's id be re-minted out from under it. |
| `MaxIdempotencyEntries` | 16384 | **Fails closed**: 503, same as above. Never evicts — evicting a remembered key would silently turn the next legitimate retry into a fresh (duplicate) application, exactly what invariant 10 forbids. |
| `MaxSessions` | 16384 | **Fails closed**: `POST /v1/session/begin` returns 503 (`ErrCapacity`, `Retry-After: 5`). Counts pending and active sessions together. This is now the ONLY bound on unauthenticated session-table growth — there is deliberately no per-agent cap (see the note below the table) — and expiry is what drains it. A refusal leaves the table exactly as it found it, so an error path never destroys anyone's earlier challenge. The residual risk is untargeted: a flooder can fill the table to this limit and deny NEW session establishment to EVERYONE; already-ACTIVE sessions keep authenticating. **How long that outage lasts depends on which state the table is full of, and the two differ by 30x:** pending challenges drain after `ChallengeTTL` (2 minutes), but ACTIVE sessions are reclaimed only after `SessionLifetime` (1 hour), and nothing caps active sessions per agent while enrolment is itself unauthenticated — so an attacker that enrols its own agent can hold the outage for an hour past the flood at far less traffic. That gap pre-dates this row and is filed as AUTH-1-FU-ACTIVECAP. Mitigation for the flood itself is per-source rate limiting, **not implemented** — task AUTH-1-FU-RATELIMIT. |

**There is deliberately no per-agent pending-challenge cap** (removed in AUTH-1-FU-PENDINGCAP,
2026-08-02; formerly `MaxPendingPerAgent`, default 8, evicting the oldest pending challenge for that
agent). It was removed rather than retuned: on the unauthenticated `POST /v1/session/begin` route,
`agent_id` is an attacker-supplied *victim* identifier, so a bucket keyed on it always lands an
anonymous flooder's requests in the *victim's* own bucket. Evicting drops the victim's
correctly-issued challenge; refusing denies the victim its next one — either behaviour at the cap is
a lockout of a named agent by anyone who merely knows its id, achievable in single-digit anonymous
requests. There is no ordering of a victim-keyed bucket that is not a lockout primitive, so do not
re-add one; per-source rate limiting (AUTH-1-FU-RATELIMIT, not implemented) is the correct fix for
the flooding this cap never actually addressed.

Be precise about the trade, because removing the cap made the *untargeted* flood **cheaper**, not
merely no worse: pending entries used to be bounded by cap × roster size, so exhausting the table
first meant enrolling enough distinct ids, whereas it is now directly reachable with `MaxSessions`
begins naming one known agent. That is still clearly the right trade — roughly
`MaxSessions`/`ChallengeTTL` ≈ 140 sustained requests per second buys an untargeted, unamplified,
self-healing outage, against nine requests per round for a targeted, permanent, stealthy one — but it
does raise the priority of AUTH-1-FU-RATELIMIT.

What IS guaranteed without the cap: nothing an unauthenticated caller does can destroy a challenge
already issued to another agent. A challenge leaves the session table by exactly three routes, and
the third requires the token — it expires (`ChallengeTTL`), it is completed, or a completion attempt
against it fails verification (`CompleteSession`'s single-attempt-per-pending-challenge rule). The
token is 32 bytes of `crypto/rand` and the table is keyed on its SHA-256, so that third route is
reachable only by whoever holds the token: the agent itself, or someone who observed it in flight.
**There is no TLS in this server**, so that observer is a real threat model on any non-loopback
listener, and the token's unguessability is load-bearing now that no other per-agent bound exists.

**Nothing here is durable — do not claim otherwise.** The roster (`auth.MemoryRoster`), the
idempotency-key table, the session table, and the per-name agent-id suffix counters
(`ids.NewNameSuffixes()`, wired fresh in `cmd/agent-bus`) are **all in-memory only**. Enrolment is
**NOT crash-safe**: every enrolled agent, every remembered idempotency key, and every session is lost
on process restart, and suffixes restart at 1 for every name until AUTH-3 lands durable enrolment and
recovery through the WAL. This is a known, deliberately-scoped gap, not an oversight — see the doc
comments on `auth.Service.Enrol` and `auth.Roster`. Sessions specifically are **not** a durability gap
to close later: not surviving a restart is a settled design decision (a lost session costs one
challenge/response round trip), independent of AUTH-3. `cmd/agent-bus`'s `run()` logs this at `WARN`
on every start:

```
msg="enrolment and sessions are IN-MEMORY ONLY: they are NOT crash-safe, the roster and all sessions are LOST on restart, and agent id suffixes restart from 1 for every name. Do not treat an accepted enrolment as durable until AUTH-3 lands durable enrolment and recovery" bus_id=<id> follow_up=AUTH-3
```

No on-disk record type, WAL frame, or wire protocol version was introduced by this change — the
`## Record types / wire protocol versions` section above remains the complete index.

**Known gaps in this surface (recorded so nobody assumes a protection that is absent).** All three
routes above are UNAUTHENTICATED by necessity — they are the calls that ISSUE the credential — and:

- **There is NO per-source rate limiting.** The caps are all GLOBAL, so an anonymous caller can deny
  enrolment bus-wide with `MaxRosterEntries` requests, and deny session establishment with
  `MaxSessions` begins. The caps bound memory; they do not bound an attacker.
- **There is no per-agent pending-challenge cap**, and deliberately so: one existed briefly
  (`MaxPendingPerAgent`) and was removed (AUTH-1-FU-PENDINGCAP) once analysis showed any such cap is
  itself a lockout primitive on this unauthenticated route — see the note under the admission-control
  table above. Nothing an unauthenticated caller does can destroy a challenge already issued to
  another agent.
- **Enrolment does not prove possession of the private key.** A caller may bind any public key —
  including someone else's published one — to a fresh, server-minted agent id. The binding that this
  surface does guarantee still holds: an id can never later present a *different* key, and an id
  cannot be used without a signature from the key recorded against it.
- **Every route off the allow-list now enforces a session token** (AUTH-2 — see `## Authentication`
  below). `auth.Service.Authenticate` is the seam `authMiddleware` calls on every request; it is no
  longer unwired.
- **There is no revocation** (AUTH-4). A session is valid until it expires, at most one hour.

## Authentication (added 2026-08-02)

AUTH-2 wires `internal/httpapi/authmw.go`'s `authMiddleware` around the WHOLE mux —
`s.handler = LoggingMiddleware(s.log, s.authMiddleware(mux))` — folding in **AUTH-6**'s fail-open fix
into the same change rather than as a later retrofit. The middleware is DEFAULT-DENY: every request is
refused 401 unless its **exact** `r.URL.Path` is on the allow-list, so a route added tomorrow is
authenticated the instant it is registered through `(*Server).route` — nobody has to remember to wrap
it, and forgetting is no longer possible for the surface `TestEveryRouteRequiresAuth` can see (below).

**The allow-list is exactly five paths, matched by exact string equality** (no prefix match, no path
cleaning, no trailing-slash tolerance — `/healthz/`, `//healthz`, `/HEALTHZ` are NOT allow-listed and
get 401; the cost of being this strict is a 401 on a misspelled-but-harmless probe, the cost of being
lenient is a normalisation mismatch between this check and the mux, which is how allow-list bypasses
get built):

- `/healthz` — liveness; a load balancer or orchestrator probe calls it before any agent exists, and it
  returns no state.
- `/v1/info` — pre-enrolment discovery; an agent needs the bus id and version to decide whether to
  enrol at all.
- `/v1/enroll` — this is where an identity is created; there is by definition no credential yet.
- `/v1/session/begin` — called with NO session at all; it is the request that asks the server for a
  token to sign.
- `/v1/session/complete` — the one that looks skippable. The caller does hold a token here, but it is
  PENDING, and `auth.Service.Authenticate` rejects a pending session exactly like an unknown one — a
  bearer requirement on this route would be unsatisfiable, not strict, since it could only ever be
  satisfied by the very credential the call exists to create. Authentication on this route is the
  Ed25519 signature over `auth.SessionSigningContext + token` (see "Enrolment and sessions" above),
  which `handleSessionComplete` verifies directly; the token in the body is not a credential until that
  succeeds.

**Every other route requires `Authorization: Bearer <token>`**, where `<token>` is the opaque handle
returned by a COMPLETED `/v1/session/complete` — not a signed claim, so every request re-checks live
session state, which is what makes revocation and expiry immediate; nothing here is cached. The 401
body is always the standard `{"error":"..."}` envelope and is one of exactly two strings, deliberately
never anything more specific:

- `{"error":"authentication required"}` — no usable credential presented: missing `Authorization`
  header, more than one `Authorization` header (rejected on ambiguity even when both carry the same
  valid token — a duplicate could be a proxy artefact, and choosing which of two to honour is the
  ambiguity an attacker exploits to make front and back disagree), a scheme other than `Bearer`
  (scheme itself case-insensitive per RFC 7235), an empty token, a token containing a space, a token
  over `MaxBearerTokenLen` (512 bytes), or a token with a byte outside the base64url alphabet
  `[A-Za-z0-9_-]`. Carries `WWW-Authenticate: Bearer realm="agent-bus", error="invalid_request"`.
- `{"error":"invalid or expired credential"}` — a syntactically well-formed token that did not
  authenticate: unknown (never issued), PENDING (challenge never completed), or EXPIRED. These three
  are deliberately BYTE-IDENTICAL in the response — distinguishing them is an enumeration oracle that
  would let a caller probe which session handles exist; the LOG line (not the response) names which of
  the three it was. Carries `WWW-Authenticate: Bearer realm="agent-bus", error="invalid_token"`.

On success the middleware attaches the verified `auth.Principal` to the request context; no principal
is attached on an allow-listed route. Downstream handlers read it via `httpapi.PrincipalFromContext` /
`httpapi.AgentIDFromContext` and MUST NOT take an identity from a header, query parameter or body —
those are client-supplied claims (invariant 1: the server is authoritative on every id).

**Exported Go surface** (`internal/httpapi/authmw.go`, `internal/httpapi/server.go`):

| Symbol | What |
| --- | --- |
| `MaxBearerTokenLen` | `512` — the length cap above; two orders of magnitude of headroom over a real 43-character token, and still finite. |
| `UnauthenticatedRoutes() []string` | The allow-list, sorted, returned as a COPY — the real map is the security boundary of this server and is not exported, so no caller can get a handle that mutates it. |
| `IsUnauthenticatedRoute(path string) bool` | Exact-match check against the allow-list; what `authMiddleware` itself calls. |
| `PrincipalFromContext(ctx) (auth.Principal, bool)` | The authenticated identity, or `ok == false` on an allow-listed route (not an error condition — it is the definition of an unauthenticated route). |
| `AgentIDFromContext(ctx) string` | The fully-qualified `<bus-id>.<agent-id>` (invariant 2) of the caller, or `""` when no principal is attached. |
| `(*Server).Routes() []string` | Every pattern registered through `(*Server).route`, sorted. This is the real surface `TestEveryRouteRequiresAuth` walks, because Go 1.19's `http.ServeMux` cannot otherwise be enumerated. |

**Rule for every future route: register it through `(*Server).route`, never `mux.HandleFunc`
directly.** A route registered the wrong way is still authenticated — the middleware wraps the whole
mux regardless of how a pattern got onto it — but it is invisible to `Routes()` and therefore to
`TestEveryRouteRequiresAuth`'s enumeration, which is a testing gap, not a security hole; do not create
that gap when a five-minute fix (using the helper) avoids it.

**Caveat from the security audit: `OPTIONS * HTTP/1.1` never reaches this middleware.** Go 1.19's
`net/http` answers a server-wide `OPTIONS *` request with its own `globalOptionsHandler` (a bare `200`,
`Content-Length: 0`) ABOVE the application handler entirely — `authMiddleware` and the mux never see
it. It exposes no application data or state, so this is not a hole in the credential model, but it is a
real place a blanket "every request is authenticated" claim would be wrong, so it is recorded here
rather than left for someone to discover by testing it. `net/http.Server.DisableGeneralOptionsHandler`
would route it through the mux like everything else, but it is go1.20+ and this module is pinned to
go1.19 (see `CLAUDE.md`, "Runtime target") — not fixable here without a version bump recorded in
`DECISIONS.md` first.
