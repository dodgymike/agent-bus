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
