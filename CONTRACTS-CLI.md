# Contracts: server / CLI flags + env vars

Split out of `CONTRACTS.md` (2026-08-02) — see that file for the index of all contract planes and
the rest of the surface (HTTP, on-disk, agent-facing). This is a pure content move: everything below
this header is unchanged from the prior single-file `CONTRACTS.md`, verbatim.

## CLI flags (`cmd/agent-bus`)

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | TCP address to bind (loopback-only by default; use `:8080` for all interfaces) |
| `-data-dir` | `./data` | Directory for the durable store + append-only log; created `0700` if missing |
| `-poll-timeout` | `30s` | Ceiling on a single long-poll wait (not yet consumed by any handler) |
| `-log-level` | `info` | `debug`, `warn`, `info`, or `error` |
| `-bus-id` | *(empty → placeholder `bus-local`)* | **TEST-ONLY.** Validated against `^[A-Za-z0-9_-]{1,64}$`; `.` rejected (qualification separator, invariant 2). Using it logs a runtime `WARN`. See `DECISIONS.md`. |

Exit codes: `2` on invalid flags/config (`parseFlags`/`validate` failure), `1` on a startup failure
(e.g. bind failure), `0` on a clean signal-driven shutdown.

## Env vars

None yet. Every configuration knob today is a CLI flag; there is no env var surface to document.
