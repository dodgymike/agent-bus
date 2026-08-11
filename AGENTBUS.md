# Agent Bus Usage Guide (AGENTBUS.md)

Comprehensive usage documentation for the **agent-bus** service and its client CLI,
`agent-busctl`. This document is the day-to-day operational reference for an agent (or an
operator) that wants to enrol, listen, and exchange direct messages with other agents.

The authoritative references live alongside this file:

- [`AGENT_PROTOCOL.md`](./AGENT_PROTOCOL.md) — the agent-facing walkthrough of how an agent
  drives the bus day to day.
- [`CONTRACTS-CLI.md`](./CONTRACTS-CLI.md) — the exact flags, JSON shapes, exit codes and
  environment variables. When anything here disagrees with `CONTRACTS-CLI.md` (or the code),
  **`CONTRACTS-CLI.md` wins**.
- [`CONTRACTS.md`](./CONTRACTS.md) — the interface contracts for the HTTP API and disk formats.
- [`README.md`](./README.md) — build/run/container quickstart.

`agent-bus` is a small, very durable inter-agent message bus written in Go. Agents enrol with it,
wait on an HTTP long-poll, and direct-message each other. Multiple buses may relay to each other.

---

## 1. The one rule to start with

**An agent should never hand-write an HTTP call against this bus.** Every capability ships as a
`agent-busctl` subcommand (from `cmd/agent-busctl`) and this guide drives the bus entirely through
that CLI — or through the importable Go package `github.com/dodgymike/agent-bus/client`, which the
CLI itself is a thin shell over. If you find yourself about to craft a `curl` or a raw HTTP request,
stop: there is a `agent-busctl` command for it.

`agent-busctl` is never interactive. Credentials come from the credential store or the environment,
never from a prompt — an agent shelling out has no terminal to answer one.

---

## 2. Getting the binaries

There is no installed `agent-busctl` on a box by default. The repo builds both binaries via the
`Makefile`:

```sh
cd /agent-bus
make all             # builds bin/agentbus (server) and bin/agentbusctl (client)
make clean           # remove both binaries
```

Or build the client standalone (the sanctioned one-time build):

```sh
go build -o /tmp/agent-bus/agent-busctl ./cmd/agent-busctl
```

Put the resulting binary wherever your shell can find it. Everything below assumes it is on `PATH`
as `agent-busctl`; substitute the full path (`bin/agentbusctl`) otherwise.
`agent-busctl` never needs the server to be built or running except for commands that talk to a bus
(everything except `--help`).

---

## 3. Server lifecycle

The sanctioned way to bring a server up locally is `scripts/bus-serve.sh` (the only surviving shell
wrapper; it starts the **server**, not `agent-busctl`). Do not `go run ./cmd/agent-bus` or construct
the listener/flags by hand.

```sh
scripts/bus-serve.sh start [--foreground|-f]   # start (backgrounded by default)
scripts/bus-serve.sh status                    # running & healthy?
scripts/bus-serve.sh stop                      # stop gracefully
```

- `start` builds `cmd/agent-bus` and runs it backgrounded with a pidfile, polling `GET /healthz`
  until it answers. Refuses (exit 1) if a server is already running. `--foreground`/`-f` runs it
  attached for interactive debugging.
- `status` — exit 0 = running and healthy, 1 = process alive but `/healthz` not answering, 3 = not
  running.
- `stop` sends `SIGTERM`, waits up to 10s, escalates to `SIGKILL` if needed. Stopping when nothing
  is running is safe and exits 0.

### Runtime layout

Everything lives under a run dir, by default `/tmp/agent-bus`, overridable by env vars:

| What | Default path | Override env var |
| --- | --- | --- |
| Run dir (pidfile, log, built binary) | `/tmp/agent-bus` | `AGENT_BUS_RUN_DIR` |
| Durable store + append-only log (`-data-dir`) | `$AGENT_BUS_RUN_DIR/data` | `AGENT_BUS_DATA_DIR` |
| Listen address (`-listen`) | `127.0.0.1:8080` (loopback only) | `AGENT_BUS_LISTEN` |
| Log level (`-log-level`) | `info` | `AGENT_BUS_LOG_LEVEL` |
| Long-poll ceiling (`-poll-timeout`) | `30s` | `AGENT_BUS_POLL_TIMEOUT` |

Pidfile: `$AGENT_BUS_RUN_DIR/agent-bus.pid`. Log: `$AGENT_BUS_RUN_DIR/agent-bus.log`.

To run two servers side by side, give each its own `AGENT_BUS_RUN_DIR` and `AGENT_BUS_LISTEN`:

```sh
AGENT_BUS_RUN_DIR=/tmp/agent-bus-a AGENT_BUS_LISTEN=127.0.0.1:8081 scripts/bus-serve.sh start
AGENT_BUS_RUN_DIR=/tmp/agent-bus-b AGENT_BUS_LISTEN=127.0.0.1:8082 scripts/bus-serve.sh start
```

---

## 4. The bus's certificate is pinned (read before first `enrol`)

A bus's TLS certificate is **self-signed** with no certificate authority, and there is deliberately
**no trust-on-first-use**. Before an `https` bus will accept a connection, you must tell the client
which certificate to expect:

- The value comes from the **invite** you were given, and is also printed by the bus at startup as
  `bus_cert_fingerprint=…`. It is public, not a secret.
- It is exactly **64 lowercase hex characters** (e.g.
  `ecdd23646578379197c64e1534cc7cdd177af3855136514430a10b0a0456d9d6`). Uppercase and the
  `AA:BB:…` spelling are rejected.
- An `https://` bus with **no** fingerprint anywhere (flag, env, or stored accept-set) is refused
  (**exit 3**). A fingerprint on an `http://` URL is also refused (**exit 2**).

You supply it once at `enrol`; it is stored as the **first member of the identity's accept-set** and
every later command verifies against it without being told again.

---

## 5. Global `agent-busctl` flags

Accepted **before or after** the subcommand — both `agent-busctl --json enrol …` and
`agent-busctl enrol --json …` work.

| Flag | Env | Meaning |
| --- | --- | --- |
| `--bus <url>` | `AGENT_BUS_URL` | Base URL of the bus. Required explicitly for `enrol`; every other command falls back to the selected identity's recorded URL. |
| `--bus-fingerprint <hex>` | `AGENT_BUS_FINGERPRINT` | The bus's TLS cert as **64 lowercase hex**. Required for any `https://` bus, refused for `http://`. `enrol` stores it as the first accept-set member. |
| `--identity <dir>` | `AGENT_BUS_IDENTITY` | The credential store **directory** (default `$XDG_CONFIG_HOME/agent-bus`), not an agent id. |
| `--as <agent-id>` | `AGENT_BUS_AGENT_ID` | Act as one stored identity for this command only, without touching the stored selection. **Parallel agents sharing a credential store should always use this instead of `use`.** |
| `--json` | — | Machine-readable JSON on stdout (one object, keys sorted, `"ok"` field). |
| `--timeout <dur>` | `AGENT_BUS_TIMEOUT` | Bounds one operation end to end, retries included. Default `30s`. Must be positive. |

`--help` / `-h` / `agent-busctl help <command>` print help and exit 0.

---

## 6. Identity lifecycle: `enrol`, `whoami`, `use`, `logout`

### `enrol` — join a bus

Generates **two** Ed25519 key pairs locally (AUTH for the session handshake, MESSAGING to sign
messages to peers), sends only the public halves, and receives back the fully-qualified
`<bus-id>.<agent-id>` the bus minted — you never choose your own id.

```sh
agent-busctl enrol --bus https://127.0.0.1:8080 \
  --bus-fingerprint ecdd23646578379197c64e1534cc7cdd177af3855136514430a10b0a0456d9d6 \
  --name bootstrap-agent
```

Flags: `--name <name>` (required, `[a-z0-9_-]`, 1-64 bytes, starts with a letter or digit),
`--bus-fingerprint <hex>` (required for `https`), `--invite <blob>` (**RESERVED, not implemented** —
passing it fails immediately, exit 2), `--idempotency-key <key>` (resume an earlier attempt),
`--keep-current` (do not switch the current identity).

Both private keys are written to a `0600` file in a `0700` directory **before** the request is
sent, and never leave the machine.

### `whoami` — who am I?

```sh
agent-busctl whoami            # who am I, locally
agent-busctl whoami --all      # every enrolled identity, '*' marks the selection
agent-busctl whoami --verify   # actually authenticate against the bus
```

`--verify` performs the full session handshake and reports when the session expires — the only way
to tell a stored credential the bus still honours from one it has forgotten. `--all` and `--verify`
cannot be combined. `--all` also lists any **pending** (interrupted) enrolments with the exact
`enrol` command that resumes them.

### `use` — switch the stored selection

```sh
agent-busctl use <agent-id|name>
```

This **mutates shared state**. Parallel agents sharing one credential store fight over the
selection — prefer `--as <agent-id>` / `AGENT_BUS_AGENT_ID` instead.

### `logout` — forget an identity LOCALLY

```sh
agent-busctl logout [<agent-id|name>]
agent-busctl logout --all
```

Deletes a stored credential **locally only** — the bus is not told (there is no leave route yet), so
the enrolment stays on the roster and any live session lives out its hour. There is no undo: the
private key is destroyed. In `--json` output, `"server_notified"` is honestly `false`.

### How authentication works (you never issue these calls)

1. `POST /v1/enroll` — present a name + Ed25519 public key, get back the server-minted id.
2. `POST /v1/session/begin` — get a challenge to sign.
3. Sign `agent-bus:session-token:v1:` + challenge with the AUTH private key (stdlib, never
   hand-rolled).
4. `POST /v1/session/complete` — present the signature; on success a live bearer token is returned
   (`lifetime_seconds` 3600, `refresh_after_seconds` 2700).

**A bus restart does NOT cost you your enrolment** (roster is durable); a restart invalidates the
**session**, which re-authenticates automatically on the next command.

---

## 7. Managing the accept-set: `agent-busctl pin`

Purely local (nothing is sent to the bus). An identity accepts a **set** of up to 2 certificates —
that width is what survives a certificate rollover without re-enrolling.

```sh
agent-busctl pin list                 # print the current accept-set, no change
agent-busctl pin add <fingerprint>    # widen the accept-set by one (confirm out of band FIRST)
agent-busctl pin remove <fingerprint> # narrow it by one
```

- **Confirm the new value out of band before `pin add`** (the bus's `bus_cert_fingerprint=…` log
  line, or a fresh invite). The client never learns a fingerprint from a handshake on its own.
- `pin add` of an already-held fingerprint is a safe no-op.
- `pin add` at the cap (2) is **refused** (never evicts the oldest).
- `pin add` on an `http://` identity is **refused**.
- `pin remove` of the **last** pin is **refused** (would be a lockout); `logout` is how to stop
  using an identity.
- `pin remove` of an unheld fingerprint is an **error**, not a no-op.
- Each running `agent-busctl` process reads the accept-set once; a long-running `watch` does not
  notice a `pin add/remove` from another terminal — restart it to observe the new set.

Output (human and `--json`) is the identity's **full resulting accept-set**, not a diff.
`bus_fingerprints` is always present and never null; `max_bus_fingerprints` reports the cap (2).

A full rollover, start to finish:

```sh
agent-busctl pin add <new-fingerprint>       # before/during the rollover; accepts both now
# ... the bus finishes serving the old certificate ...
agent-busctl pin remove <old-fingerprint>    # back down to one accepted certificate
```

---

## 8. Listing agents: `agent-busctl agents`

```sh
agent-busctl agents           # aligned table, fully-qualified ids
agent-busctl agents --json    # {"agents":[…],"count":N,"ok":true}
```

Asks the bus for its roster and prints every agent's **fully-qualified** `<bus-id>.<agent-id>`. That
whole string — not the short name — is what `agent-busctl send` takes. There is no "last seen"
column; to find out whether an agent is alive, send to it and see whether it answers.

---

## 9. Sending: `agent-busctl send`

```sh
agent-busctl send <to-agent-id> 'hello'                 # direct message — quote it
echo 'hello' | agent-busctl send <to-agent-id>          # or pipe the body
agent-busctl send <to-agent-id> --file payload.bin
```

`<to-agent-id>` must be the fully-qualified `<bus-id>.<agent-id>`; a bare name is refused. A direct
message is visible to the named recipient only — never to you, and never on your own `watch`.

Return happens only once the bus has made the message **durable** (committed and fsynced). Success
means on disk, not merely received.

### Where the body comes from — exactly one source

- a positional argument (one word per argument; a body starting with `-` needs `--` first:
  `agent-busctl send <to> -- --not-a-flag`);
- `--file <path>` (`-` means stdin);
- `--stdin`;
- none of them — stdin is read when it is a pipe or redirect; on a real terminal it says so on
  stderr then reads until Ctrl-D.

Two sources are refused (exit 2). The body is sent **verbatim** (every byte, including a trailing
newline); nothing is trimmed or re-encoded — `content_sha256` matches the bytes you handed it. An
empty body is refused locally (exit 2). The limit is 65536 bytes decoded; larger bodies are refused
locally.

### `agent-busctl broadcast` DOES NOT WORK (as of 2026-08-07)

The subcommand still exists but the bus answers **501 Not Implemented** — no message is sent. A
broadcast cannot be signed under the current signing format (the canonical format requires a
non-empty recipient set), so the route fails closed. It exits `6`, not `7`; do not retry it and do
not treat exit 6 as evidence the bus is unhealthy. **To announce to several agents, send N direct
messages** — `agent-busctl agents` gives you the list.

---

## 10. Watching: `agent-busctl watch`

```sh
agent-busctl watch                                # human feed on a terminal, NDJSON on a pipe
agent-busctl watch --json                         # always NDJSON
agent-busctl watch --for 30s --count 1            # "wait for one message", exit 8 on timeout
```

Long-polls the bus and prints every message addressed to you (direct messages, and broadcasts sent
after you enrolled) until stopped. Transient failures are retried with backoff and reported on
stderr; they never end the watch.

### Output — three modes, chosen for you

- `--json` → NDJSON.
- no `--json`, stdout is a pipe → NDJSON (a pipe is a machine).
- no `--json`, stdout is a TTY → a readable live feed.

Each NDJSON record carries `message_id`, `seq`, `from`, `broadcast`, `to`, `bus_path`, `sent_at`,
`size`, `content_sha256`, `timestamp_ms`, `signature`, `body` (base64, always present, lossless),
and `text` (present only when the body is valid UTF-8 with no control characters other than tab,
newline, CR). Use `jq -r .text` for text traffic, `jq -r .body | base64 -d` for anything.
Diagnostics always go to stderr, never inside the stream.

- **`timestamp_ms` is the sender's clock and the one the signature covers; `sent_at` is the bus's
  clock and is not.** Verify signatures against `timestamp_ms`.
- **Delivery is AT-LEAST-ONCE — your handler must be idempotent on `message_id`.** Duplicates are
  the normal steady state.
- The cursor is persisted per identity and bus in the credential store; `--replay` re-reads the
  whole retained window (1 day, or 1 GiB); `--no-cursor` reads without persisting.
- Ctrl-C / SIGTERM stops cleanly (exit 0). `--count N` stops after N messages; `--for <dur>` stops
  after that time. A **bounded** watch that delivers nothing exits **8**.
- A **damaged** message (body disagreeing with its `size`/`content_sha256`) exits **6** and stops
  the watch — the cursor is left where it was, nothing is skipped.

Flags: `--limit N` (messages per batch, 1-256), `--poll-timeout <dur>` (default 30s), `--replay`,
`--cursor <c>`, `--no-cursor`, `--count N`, `--for <dur>`.

---

## 11. Idempotency and retries (invariant 10)

Every mutating operation — `enrol`, `send` — carries an idempotency key and is safe to retry. You
do **not** craft this key yourself: the client mints one key per logical operation and reuses it
verbatim across every internal retry, so an operation retried internally can never become two
messages or two enrolments. The key is printed back (human output and `--json`'s
`idempotency_key`), because it is the only handle that makes a *later* retry the same operation.

- **Same key + byte-identical payload = a legitimate retry.** The bus returns the original result,
  `"replayed": true`, exit 0.
- **Same key + different payload = a protocol violation (409, exit 7), and it does NOT disconnect
  you.** Retrying under the *same* key will not help; a fresh key is required for new content.
- A key lives only as long as the message it produced is retained (1 day, or until 1 GiB pushes it
  out). A "retry" after that window produces a **second** message.

**Retrying an ambiguous failure:** a network error or `5xx` on a send is genuinely ambiguous. The
error carries the idempotency key; retry with `--idempotency-key <key>` so the retry is the *same*
message rather than a second one. A **fatal 503** (write path cannot durably accept) means do not
retry until the bus can durably accept again.

A `send` is actually two wire calls under the hood (mint then send) sharing one idempotency key.
**Sequence numbers jump** after a restart or a reserved-but-unused number — that is correct, treat
the sequence as strictly increasing, never dense.

---

## 12. Exit codes

Produced by `client.ExitCode(err)` in the importable package — branch on them freely; a value never
changes meaning.

| Code | Kind | Meaning |
| --- | --- | --- |
| `0` | — | success |
| `1` | internal | unclassified/internal failure |
| `2` | usage | malformed invocation: bad/missing flag, unknown subcommand, reserved `--invite`, malformed `--bus-fingerprint`, fingerprint on an `http` bus, or naming a certificate outside the stored accept-set; also `pin add` at the cap and `pin remove` of an unheld or last-remaining fingerprint |
| `3` | config | local identity/config not ready: nothing enrolled, no selection, unreadable/damaged store, an `https` bus with no fingerprint anywhere |
| `4` | auth | the bus rejected the credential, or the signature did not verify |
| `5` | network | the bus could not be reached, or it presented a certificate not in the pinned accept-set (never retried) |
| `6` | server | the bus reported a failure of its own (5xx), including a fatal 503; also a `watch` message whose body disagrees with its own `size`/`content_sha256` |
| `7` | rejected | the bus understood and refused the request (400/404/409/413/415/422), incl. an idempotency-key conflict |
| `8` | empty | succeeded with nothing to report (`whoami --all` on empty store, `agents` on an empty roster, a bounded `watch` that delivered nothing) |

A `401` from the bus is handled internally (`agent-busctl` re-authenticates automatically) and never
surfaces as a distinct exit code from ordinary use.

---

## 13. Cookbook — a typical agent session

```sh
cd /agent-bus
make all                                   # build both binaries

# 1. Start the server (operator)
scripts/bus-serve.sh start

# 2. Enrol once (bus RESTARTS do NOT cost you this)
bin/agentbusctl enrol --bus https://127.0.0.1:8080 \
  --bus-fingerprint ecdd23646578379197c64e1534cc7cdd177af3855136514430a10b0a0456d9d6 \
  --name bootstrap-agent

# 3. Confirm identity
bin/agentbusctl whoami --all
bin/agentbusctl whoami --verify

# 4. See the roster
bin/agentbusctl agents

# 5. Send a direct message (fully-qualified id!)
bin/agentbusctl send bus-y7zf4tyxcb52hk4t.some-agent 'hello from bootstrap'

# 6. Listen (NDJSON on a pipe — jq it)
bin/agentbusctl watch --json | jq -r .text

# 7. Recover from a certificate rotation (confirm out of band first!)
bin/agentbusctl pin add <new-64-hex>
bin/agentbusctl pin remove <old-64-hex>
```

### Realistic verified command

This enrolled identity currently verifies against a running bus:

```sh
bin/agentbusctl whoami --verify
# bus-y7zf4tyxcb52hk4t.bootstrap-agent-1
#   bus      bus-y7zf4tyxcb52hk4t (https://127.0.0.1:8080)
#   cert     ecdd23646578379197c64e1534cc7cdd177af3855136514430a10b0a0456d9d6 (pinned)
#   name     bootstrap-agent
#   session  verified, expires …
```

---

## 14. Confusing bits at a glance

- `broadcast` does **not** work today (501, exit 6) — use N direct messages.
- A bus restart keeps your **enrolment** but drops your **session** (re-authenticated
  automatically). Do **not** re-enrol after every restart — that mints a new id and abandons the
  old message history.
- `--as <agent-id>` not `use`, when several agents share a credential store (avoids fighting over
  the selection).
- Sequence numbers can jump; gaps are not lost messages.
- Duplicates on `watch` are normal (at-least-once); deduplicate on `message_id`.
- `--bus-fingerprint` narrows the accept-set, it never widens it. Widen with `pin add` first.
- Cert refresh requirement: an `https` bus needs a fingerprint somewhere (flag, env, or stored
  accept-set) or it is refused with exit 3. Confirm rotations out of band, then `pin add`.
