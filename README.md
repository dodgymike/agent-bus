# agent-bus

A small, very durable inter-agent message bus, written in Go. Claude Code agents enrol with it,
wait on an HTTP long-poll, and broadcast or DM each other. Multiple buses relay to each other.
Agents drive it entirely through shell wrappers — **an agent should never have to construct an
HTTP call.**

## Status: early scaffold

This is the CORE wave. The server binary starts, binds, logs, and answers two unauthenticated
routes (`/healthz`, `/v1/info`). **Enrolment, sending, long-poll, and relay are not built yet.**
There is no `scripts/bus-*.sh` wrapper yet, because there is no agent-facing capability yet —
invariant 7 (every capability ships with its wrapper) doesn't apply until one exists. Do not expect
the quickstart below to do anything beyond start a server and query it; the enrol/send/wait steps
are placeholders that will fill in as the ID, AUTH, MSG and POLL epics land.

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

## Quickstart (placeholder — fills in as the wrappers land)

Once the ID, AUTH, MSG and POLL epics ship, this section becomes a real walkthrough: build one bus,
enrol two agents through `scripts/bus-enrol.sh`, have one wait on `scripts/bus-wait.sh` and the
other send through `scripts/bus-send.sh`. Until then, showing those commands here would document
commands that fail — see `AGENT_PROTOCOL.md` (not yet written; it lands with the ENROL epic) for the
agent-facing instructions once they exist.

## Repository layout

See the "Repository layout" table in [`CLAUDE.md`](./CLAUDE.md#repository-layout) — not duplicated
here to avoid the two going out of sync.

## More docs

- [`CLAUDE.md`](./CLAUDE.md) — the development protocol and design invariants (read this first)
- [`DECISIONS.md`](./DECISIONS.md) — dated design decisions and their rationale
- [`CONTRACTS.md`](./CONTRACTS.md) — every route, flag, env var, and record type
- [`AGENT_LOG.md`](./AGENT_LOG.md) — per-task work log
- [`SPEC.md`](./SPEC.md) — generated mirror of the Spec Server backlog (never hand-edit)
