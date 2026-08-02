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

**Why it exists.** `go test -race -run TestThatDoesNotExist ./internal/wal` prints
`ok … [no tests to run]` and **exits 0**. ~70% of this backlog's `proof_cmd` values have that shape,
so a task could be flipped to `done` behind a proof whose named test was never written. A second
variant is worse because it leaves no marker at all: a test body that is just `t.Skip()` exits 0
with no `[no tests to run]` text, so grepping for that string does not catch it. This script closes
both.

```
scripts/proof-check.sh [--task <id>] [--classify] [--strict] [--quiet] '<proof command>'
```

| Option | Meaning |
| --- | --- |
| `--task <id>` | Fetch `proof_cmd` from the Spec Server (task key or `public_id`) via `scripts/spec-cloud.sh`, then check it. Requires `jq`. |
| `--classify` | Static classification only — **runs nothing**. Safe on untrusted input. |
| `--strict` | Additionally require *every* package listed in a `go test` invocation to contribute ≥1 test. Opt-in. |
| `--quiet` | Suppress the proof's own output; print only the verdict. |

| Env var | Default | Meaning |
| --- | --- | --- |
| `PROOF_CHECK_PROJECT` | `agent-bus` | Spec Server project slug used by `--task`. |

**Exit codes** (distinct on purpose — "I cannot check this" must never read as "this is broken"):

| Code | Verdict | Meaning |
| --- | --- | --- |
| `0` | `PASS` | Ran, exited 0, and if it ran Go tests then ≥1 test really ran and not all skipped |
| `1` | `FAIL` | Ran and exited non-zero |
| `2` | — | Usage error |
| `3` | `UNVERIFIABLE` | Cannot be checked: `n/a`, unfilled `<placeholder>`, invalid shell, or a segment whose command does not exist (prose, or a wrapper not built yet). **Not** a claim the work is broken. |
| `4` | `VACUOUS` | Exited 0 but proved nothing: zero tests ran, or every test that ran skipped |

Stdout carries one machine-readable line; human output goes to stderr:

```
proof-check: verdict=<PASS|FAIL|VACUOUS|UNVERIFIABLE> class=<tags> exit=<n> tests_run=<n> top_level=<n> skipped=<n> empty_pkgs=<n>
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
privileges, in the repo root. With `--task` the string comes from the Spec Server, so anyone who can
edit that backlog can choose a command that runs on your machine. Use `--classify` to inspect
statically without executing.

**Known limitation.** To count tests, `go test` invocations are re-run through a `go` shim that
injects `-v` and merges stderr into stdout (`go build`/`go vet`/`go list` pass through untouched). A
proof that parses non-verbose `go test` output, or redirects the two streams separately, will see
different text than it would standalone. Nothing in the current backlog does.

**Policy (recommendation, not yet enforced):** completion should *require running* `proof-check.sh
--task <id>` and quoting its verdict line in `test_summary`, while `proof_cmd` stays stored as the
bare command — a proof that only runs inside our harness is a worse artifact than one anyone can
paste into a shell. Nothing in the tool can enforce this; its value is an auditable verdict line.
Full rationale and tradeoffs are in the comment block at the top of the script.
