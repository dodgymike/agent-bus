# Agent Protocol

Agent-facing instructions for working with agent-bus. **An agent should never hand-write an HTTP
call or a `go run`/`go build` line against this repo — every capability ships as a
`scripts/bus-*.sh` wrapper (repo invariant 7), and this file is the corresponding usage doc,
updated in the same task as the wrapper.**

`CONTRACTS.md` is the authoritative reference for the exact wire shape (routes, flags, env vars,
record types); this file summarises how an agent actually drives each wrapper day to day.

Sections below are added one per capability, in the order the capability ships:

- [Server lifecycle](#server-lifecycle-scripts-bus-servesh) — `scripts/bus-serve.sh` (this doc)
- [Authentication](#authentication-added-2026-08-02) — applies to every capability below; no wrapper
  or CLI subcommand ships for the handshake itself yet
- Enrol — `scripts/bus-enrol.sh` (not yet shipped)
- List agents — `scripts/bus-list.sh` (not yet shipped)
- Wait / long-poll — `scripts/bus-wait.sh` (not yet shipped)
- Send / broadcast / DM — `scripts/bus-send.sh` (not yet shipped)
- Relay (cross-bus) — `scripts/bus-relay.sh` (not yet shipped)

Do not invent usage for an unshipped wrapper here; add its section in the same task that adds the
script.

## Server lifecycle: `scripts/bus-serve.sh`

Starts, stops, and reports the status of a **local** agent-bus server. This is the only sanctioned
way to bring a server up locally — do not `go run ./cmd/agent-bus` or `go build` it yourself, and
never construct the listener/flags by hand.

```
scripts/bus-serve.sh start [--foreground|-f]
scripts/bus-serve.sh status
scripts/bus-serve.sh stop
```

- `start` builds `cmd/agent-bus` and runs it **backgrounded** with a pidfile by default, polling
  `GET /healthz` until the server answers before returning 0. Refuses (exit 1) if a server is
  already running per the pidfile. Pass `--foreground`/`-f` to run attached instead (useful for
  interactive debugging); this `exec`s the binary so signals reach it directly and the command
  blocks until the server exits.
- `status` reports whether the pidfile's process is alive and `/healthz` answers. Exit `0` =
  running and healthy, `1` = process alive but `/healthz` not answering, `3` = not running (no
  pidfile, or a stale pidfile whose process is gone — handled automatically, no manual cleanup
  needed).
- `stop` sends `SIGTERM`, waits up to 10s for a graceful exit (matching the server's own shutdown
  grace period), escalates to `SIGKILL` if it doesn't, and removes the pidfile. Calling `stop` when
  nothing is running is safe and exits `0`.

Typical use in an agent session:

```bash
scripts/bus-serve.sh start
scripts/bus-serve.sh status
# ... do work against the server ...
scripts/bus-serve.sh stop
```

### Where it puts things

Nothing lands in the repo tree. Everything lives under a run dir outside the repo, by default
`/tmp/agent-bus`:

| What | Default path | Override env var |
| --- | --- | --- |
| Run dir (pidfile, log, built binary) | `/tmp/agent-bus` | `AGENT_BUS_RUN_DIR` |
| Durable store + append-only log (`-data-dir`) | `$AGENT_BUS_RUN_DIR/data` | `AGENT_BUS_DATA_DIR` |
| Listen address (`-listen`) | `127.0.0.1:8080` (loopback only) | `AGENT_BUS_LISTEN` |
| Log level (`-log-level`) | `info` | `AGENT_BUS_LOG_LEVEL` |
| Long-poll ceiling (`-poll-timeout`) | `30s` | `AGENT_BUS_POLL_TIMEOUT` |

The **default listen address is loopback**, not the server binary's own `:8080` (all-interfaces)
default — the wrapper always passes `-listen` explicitly. Set `AGENT_BUS_LISTEN` if you deliberately
need the server reachable from outside loopback; the wrapper will never pick a non-loopback default
for you.

The tracked `./data` directory in this repo is a real (if currently empty) durable-store location
used by other tooling, not a test fixture — never point `AGENT_BUS_DATA_DIR` at it for a throwaway
run.

Pidfile: `$AGENT_BUS_RUN_DIR/agent-bus.pid`. Log: `$AGENT_BUS_RUN_DIR/agent-bus.log` (server's own
stderr/stdout, useful when `start` reports the server didn't become healthy in time).

### Running two servers side by side

Give each instance its own `AGENT_BUS_RUN_DIR` (which also relocates the default data dir and
therefore the pidfile) and its own `AGENT_BUS_LISTEN`, e.g.:

```bash
AGENT_BUS_RUN_DIR=/tmp/agent-bus-a AGENT_BUS_LISTEN=127.0.0.1:8081 scripts/bus-serve.sh start
AGENT_BUS_RUN_DIR=/tmp/agent-bus-b AGENT_BUS_LISTEN=127.0.0.1:8082 scripts/bus-serve.sh start
```

## Authentication (added 2026-08-02)

Every route this bus serves requires a credential **except** exactly five: `GET /healthz`,
`GET /v1/info`, `POST /v1/enroll`, `POST /v1/session/begin`, `POST /v1/session/complete`. Everything
else — every future capability this file will document as it ships (list, wait, send, relay) —
answers `401` without `Authorization: Bearer <token>`. This is not opt-in per route: the server
refuses by default and only the five paths above are carved out, so a new capability is authenticated
the day it lands whether or not its doc entry mentions the fact.

**How a token is obtained**, end to end:

1. `POST /v1/enroll` — presents a name and an Ed25519 public key, gets back a server-minted, fully
   qualified `<bus-id>.<agent-id>`.
2. `POST /v1/session/begin` — asks for a token to sign; the server returns an opaque challenge token.
3. Sign the exact byte string `agent-bus:session-token:v1:` + that challenge token with the
   enrolment Ed25519 **private** key.
4. `POST /v1/session/complete` — presents the signature; on success the server activates the session
   and returns the same token as a live bearer credential, plus `lifetime_seconds` (3600) and
   `refresh_after_seconds` (2700).

That bearer token then goes on every subsequent call as `Authorization: Bearer <token>`. A session
lasts **at most one hour** (`lifetime_seconds`); a well-behaved agent re-runs steps 2–4 at **75% of
that lifetime** (`refresh_after_seconds`, 2700s at the default), not at the boundary, so a slow retry
never lands on an already-expired token. Server-side expiry has no clock-skew grace — an agent's own
clock opinion is never consulted.

**A 401 means re-authenticate, not retry the same request.** The server never distinguishes "unknown
token" from "expired token" from "pending, never-completed session" in the response (that
indistinguishability is deliberate — see `CONTRACTS.md`, `## Authentication`), so an agent cannot
diagnose *which* of those it hit from the response alone; the correct reaction to any 401 on a
previously-working token is to run the enrol/session handshake again, not to resend the failing
request with the same credential.

**No wrapper or CLI subcommand ships for enrol or session yet.** Per the amended invariant 7
(`DECISIONS.md`, "The Go CLI replaces the shell wrappers"), the sanctioned vehicle for this handshake
is now a Go CLI subcommand, not a `scripts/bus-*.sh` script — and no such subcommand exists in this
repo as of this writing. There is nothing here an agent can shell out to today to enrol or establish a
session end to end. Do not treat a hand-written `curl` against `/v1/enroll` or `/v1/session/*` as the
sanctioned interface just because it would work; it is not documented as one, and it is not one.
