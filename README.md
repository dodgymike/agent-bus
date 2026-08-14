# agent-bus

A small, very durable inter-agent message bus, written in Go. Claude Code agents enrol with it,
wait on an HTTP long-poll, and DM each other. Multiple buses relay to each other.
Agents drive it entirely through the `agent-busctl` CLI (or the importable `client` package it shells
over) — **an agent should never have to construct an HTTP call.**

## Status (2026-08-07)

Enrolment, sessions, direct messaging and long-poll all work, and the client is `cmd/agent-busctl` — see
[`AGENT_PROTOCOL.md`](./AGENT_PROTOCOL.md) for the agent-facing walkthrough. Relay between buses is
written but **not registered on any listener**, so cross-bus messaging does not work yet.

Three things an operator must know before upgrading an existing bus:

- **Enrolment is durable.** Agent ids, public keys and each agent's original enrolment instant
  survive a restart and a crash. **Agents no longer re-enrol after a restart.** Sessions are
  memory-only by design, so each agent redoes the session handshake — but not the enrolment.
- **A signature is mandatory on every message.** `POST /v1/send` requires a sender-supplied
  Ed25519 signature over the bus-minted message id and sequence; `agent-busctl send` handles the whole
  two-step for you. **`agent-busctl broadcast` / `POST /v1/broadcast` now answer `501` and do not work** —
  a broadcast has no signable audience under the current signing format, and the route fails closed
  rather than carrying unsigned traffic. Send N direct messages instead until that is settled.
- **Upgrading DISCARDS the existing message history.** The durable message record moved from
  version 1 to version 2 to carry the signature, and there is no migration — an unsigned message has
  nothing to migrate to. The break runs both ways, so rolling back discards the v2 records too. Copy
  the data directory first if the history matters. Enrolment and invite records are unaffected.

## Design contract

The full set of invariants — server-authoritative ids, `<bus-id>.<agent-id>` qualification, signed
enrolment, durable two-phase writes, memory-as-serving-copy/disk-as-truth, the append-only audit
log, agent-facing wrappers, and "simple beats clever" — is defined once, in
[`CLAUDE.md`](./CLAUDE.md#what-this-project-is-the-standing-design-contract). Read it there; it is
not duplicated here.

## Requirements

- Go **1.19.4** (the toolchain pinned by `go.mod` — `go 1.19`). No language or stdlib features from
  later toolchains without an explicit bump, per invariant 8 and `CLAUDE.md`.

## Build

```sh
go build ./cmd/agent-bus
```

This produces an `agent-bus` binary in the working directory (already covered by `.gitignore`).

## Run

```sh
./agent-bus -listen 127.0.0.1:8080 -data-dir ./data -poll-timeout 30s -log-level info
```

Flags (all optional; see `CONTRACTS.md` for the authoritative table):

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | TCP address to bind (loopback-only by default; use `:8080` for all interfaces) |
| `-data-dir` | `./data` | Directory for the durable store + append-only log (created `0700`) |
| `-poll-timeout` | `30s` | Ceiling on a single long-poll wait |
| `-log-level` | `info` | `debug`, `warn`, `info`, or `error` |
| `-bus-id` | *(unset)* | **TEST-ONLY** override of the bus id — never use in production; see `DECISIONS.md` |

The process serves until `SIGINT`/`SIGTERM`, then stops accepting long-polls and drains in-flight
requests within a bounded grace period.

## Container / Docker Compose

agent-bus ships as a container and Docker Compose is the runtime target (see `CLAUDE.md`). From the
repo root:

```
docker compose up -d --build   # build and start a single bus
docker compose logs -f         # follow its logs
docker compose ps              # status, including the /healthz healthcheck
docker compose down            # stop; PRESERVES the named data volume
docker compose down -v         # stop and DESTROY the data volume -- the WAL, bus id and MAC key
```

The data directory is a named volume declared `VOLUME /data` in the image. That is deliberate: the
write-ahead log, the bus id, the directory lock and the MAC key all live there, so a data dir that
vanished on `down` would silently discard the durability guarantees the rest of this project exists
to provide. `down -v` is the only command that destroys it, and it is flagged in the compose file too.

**The bus is loopback-only as shipped, and is therefore not reachable from outside its own
container.** That is by construction, not an oversight: until mutual TLS lands (invariant 11),
enrolment and session material would cross the wire in cleartext, so the container does not publish a
port. `docker compose exec` reaches it for now. Non-loopback binding is approved and configurable,
but it is gated behind mTLS — the compose file documents the opt-in and the risk it carries.

## What works today

```sh
curl -s localhost:8080/healthz
# {"status":"ok"}

curl -s localhost:8080/v1/info
# {"bus_id":"bus-local","version":"dev","uptime_seconds":12.345}
```

Both routes are unauthenticated by design (liveness and pre-enrolment discovery — see
`DECISIONS.md`). Every other route (`GET`/`POST` other than these two paths, or a non-`GET` on
these) returns a JSON error, not the bare 200 above.

## Quickstart

```sh
go build -o /tmp/agent-bus/agent-busctl ./cmd/agent-busctl
scripts/bus-serve.sh start                       # start a local bus

agent-busctl --bus https://127.0.0.1:8080 --bus-fingerprint <64-hex-from-invite> enrol --name planner
agent-busctl --bus https://127.0.0.1:8080 --bus-fingerprint <64-hex-from-invite> enrol --name builder --keep-current
agent-busctl agents                                    # fully-qualified ids

agent-busctl --as <bus>.builder-1 watch &              # long-poll for messages
agent-busctl --as <bus>.planner-1 send <bus>.builder-1 'hello'
```

`agent-busctl broadcast` is deliberately refused by the bus — see the status note above.
[`AGENT_PROTOCOL.md`](./AGENT_PROTOCOL.md) is the full agent-facing doc (every flag, exit code and
idempotency rule); [`CONTRACTS-CLI.md`](./CONTRACTS-CLI.md) is the exact reference.

## Repository layout

See the "Repository layout" table in [`CLAUDE.md`](./CLAUDE.md#repository-layout) — not duplicated
here to avoid the two going out of sync.

## More docs

- [`CLAUDE.md`](./CLAUDE.md) — the development protocol and design invariants (read this first)
- [`DECISIONS.md`](./DECISIONS.md) — dated design decisions and their rationale
- [`CONTRACTS.md`](./CONTRACTS.md) — index of every route, flag, env var, and record type; the
  detail lives in `CONTRACTS-HTTP.md`, `CONTRACTS-ONDISK.md`, `CONTRACTS-CLI.md`, `CONTRACTS-AGENT.md`
- [`AGENT_PROTOCOL.md`](./AGENT_PROTOCOL.md) — agent-facing instructions (`agent-busctl`)
- [`PROTOCOL.md`](./PROTOCOL.md) — the on-disk format and the canonical signing format
- [`AGENT_LOG.md`](./AGENT_LOG.md) — per-task work log
- [`SPEC.md`](./SPEC.md) — generated epic INDEX of the Spec Server backlog; the task records live in
  [`SPEC/`](./SPEC) (`SPEC/<EPIC>/epic.md` per epic, `SPEC/<EPIC>/<task>/task.md` per task). Never
  hand-edit either — regenerate with `bash scripts/gen-spec-mirror.sh`
