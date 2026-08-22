# Agent Protocol

Agent-facing instructions for working with agent-bus. **An agent should never hand-write an HTTP
call or a `go run`/`go build` line against this repo (except the one-time build below) — every
capability ships as a `agent-busctl` subcommand or, for the server itself, `scripts/bus-serve.sh`
(repo invariant 7), and this file is the corresponding usage doc, updated in the same task as the
command.**

`agent-busctl` (`cmd/agent-busctl`) replaced the `scripts/bus-*.sh` wrappers on 2026-08-02 (`DECISIONS.md`,
"The Go CLI replaces the shell wrappers"). It is a thin shell over the importable Go package
`github.com/dodgymike/agent-bus/client` — every one of the JSON shapes, exit codes and idempotency
rules below applies identically whether you shell out to `agent-busctl` or embed the `client` package
directly.

`CONTRACTS-CLI.md` is the authoritative reference for the exact flags, JSON shapes, exit codes and
env vars; this file summarises how an agent actually drives the bus day to day. Where the two
disagree, `CONTRACTS-CLI.md` (and the code it mirrors) wins.

Sections below, in the order you will use them:

- [Getting the binary](#getting-the-binary)
- [Server lifecycle](#server-lifecycle-scripts-bus-servesh) — `scripts/bus-serve.sh` (only surviving
  shell wrapper; it starts the SERVER, not `agent-busctl`)
- [The bus's certificate is pinned](#the-buss-certificate-is-pinned) — read this before your first
  `enrol` against an `https` bus
- [Identity: enrol, whoami, use, logout](#identity-enrol-whoami-use-logout)
- [Managing the accept-set: agent-busctl pin](#managing-the-accept-set-agent-busctl-pin) — recovering
  from a certificate rotation without re-enrolling
- [Listing agents](#listing-agents-agent-busctl-agents) — `agent-busctl agents`
- [Sending: send (and broadcast, which is BROKEN)](#sending-agent-busctl-send-and-agent-busctl-broadcast-which-is-broken)
- [Watching for messages](#watching-agent-busctl-watch) — `agent-busctl watch`
- [Checking delivery status: agent-busctl ack-status](#checking-delivery-status-agent-busctl-ack-status)
  — "did the message I sent get delivered?"
- [Acknowledging a message you received: agent-busctl ack](#acknowledging-a-message-you-received-agent-busctl-ack)
  — "I got it" / "I refuse it". Nothing else can move a row to `delivered`
- [Idempotency and retries (invariant 10)](#idempotency-and-retries-invariant-10)
- [Exit codes](#exit-codes)

## Getting the binary

There is no installed `agent-busctl` on the box by default. Build it once, exactly the way
`scripts/bus-serve.sh` builds the server:

```bash
go build -o /tmp/agent-bus/agent-busctl ./cmd/agent-busctl
```

Put the resulting binary wherever your shell can find it. Everything below assumes it is on `PATH`
as `agent-busctl`; substitute the full path otherwise. `agent-busctl` never needs the server to be built or
running except for commands that talk to the bus (everything except `--help`).

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
# ... do work against the server with agent-busctl ...
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

## Federating two buses: `agent-bus peer` is an OPERATOR command, not yours

Added 2026-08-08 by `RELAY-12`. Full contract: `CONTRACTS-CLI.md`.

Multiple buses relay to each other, and which buses this one talks to — plus whose **bus signing
keys** it pins — is configured with a subcommand on the **server** binary:

```
agent-bus peer add    -data-dir <dir> -bus-id <busID> [-url <https origin>]
                      [-tls-fingerprint <64 lowercase hex>]
                      [-peer-client-fingerprint <64 lowercase hex>]
                      [-signing-key <base64> ...] [-route-for <busID> ...] [-json]
agent-bus peer list   [-data-dir <dir>] [-json]
agent-bus peer remove -data-dir <dir> -bus-id <busID> (-route | -trust | -route -trust) [-json]
```

**You will not run this, and you cannot.** It needs filesystem access to the bus's data directory and
it takes that directory's exclusive lock, so **the bus must be stopped** — which is exactly why it is
not an HTTP route and why no agent-facing credential can reach it (`DECISIONS.md` 2026-08-08,
FEDERATION (e): no online admin route, no new privilege tier). It is documented here so that when an
operator says "the buses are peered", you know what that did and what it did not.

What it configures, in the two independent halves that matter to you (plus an optional pin riding on
the route, below):

- a **route** (`-url`) — where traffic for a bus is sent. `-route-for busC` adds a **static next-hop**
  route, which is how `busA → busB → busC` works when A and C never peer directly. Static means
  operator-entered: a fourth bus needs an entry on every bus that must reach it.
- **trust** (`-signing-key`) — the bus signing keys pinned for a bus, which is what verifies a message
  that **originated** on a bus you are not adjacent to. A bus can be trusted without being routable
  and routable without being trusted; neither flag implies the other.
- an optional **TLS pin on that route** (`-tls-fingerprint`, `797c538`, `RELAY-41`) — the certificate
  the bus at `-url` (the **next hop**) presents, as 64 lowercase hex. It is keyed to the **address**,
  never to the record's own bus id: `peer add -bus-id busB -url X -tls-fingerprint fpB -route-for
  busC` writes `fpB` — busB's certificate, the next hop's — onto **both** the busB and busC route
  records, so the busC record legitimately carries busB's fingerprint alongside `bus_id: busC`. It
  pins an **outbound server** certificate only and is **not** a source of inbound peer identity — but
  a live connection **is** now verified against it (`9701611`, `RELAY-24-BLOCKER-EGRESS`): every
  outbound peer connection resolves its pin **by dial address** and verifies the handshake against
  that address's pins alone, and an address with **no** configured pin is refused **before the socket
  is opened** rather than dialled unverified (there is no CA and no trust-on-first-use to fall back
  on, invariant 11). A route configured without this flag therefore forwards **nothing**. Omitting the
  flag on an already-pinned hop, or passing it an empty value, is **refused before any write** rather
  than silently erasing the pin — full semantics in `CONTRACTS-CLI.md`/`CONTRACTS-ONDISK.md`.
- an optional **inbound client-certificate binding on the TRUST record**
  (`-peer-client-fingerprint`, `RELAY-24-BLOCKER-PEERCERTFLAG`) — the OPPOSITE direction from
  `-tls-fingerprint`: the certificate the bus at `-bus-id` presents **as a TLS client when it dials
  this bus**, as 64 lowercase hex. It is keyed to the **bus principal**, not an address, and is unique
  across the whole trust table — binding a fingerprint another bus already holds is refused by the
  store, atomically, before anything is written. It requires `-signing-key` in the same invocation (a
  trust record is never written without at least one pinned signing key, so the flag alone would bind
  nothing) and lands on `bustrust`, not on the route record. **This is what `-tls-fingerprint`'s note
  above calls "not a source of inbound peer identity" — this flag now is one.** It is what makes a
  peer **bindable**: `bindablePeerCount` (`cmd/agent-bus/relaywiring.go`) counts trust records carrying
  this binding, and the `/v1/peer/*` ingress `RELAY-24` registers (`7095231`) refuses to mount when
  that count is `0` — before this flag no shipped command could raise it above `0`. **Federation is now
  live** (`RELAY-24`, and `RELAY-24-BLOCKER-EGRESS` for the outbound half): a running bus with at least
  one bindable peer serves the peer routes, so this flag is no longer configuration for a surface that
  nothing mounts — it is the binding that inbound peer traffic is authenticated against. Omitting the
  flag on an already-bound bus, or passing it an empty value, is likewise **refused before any write**
  rather than silently erasing the binding — full semantics, including the exact exit codes, in
  `CONTRACTS-CLI.md`.

### Where the pinned signing key comes from: `agent-bus key export-public`

Added 2026-08-14 by `CLI-11`. Full contract: `CONTRACTS-CLI.md`.

`-signing-key` above needs a value, and until now there was no command that produced one — the key
existed only inside a `0600` PEM in the bus's data directory. This is that command, and it is an
**operator** command on the **server** binary for the same reasons as `peer`:

```
agent-bus key export-public -data-dir <dir> [-json]
```

```json
{"ok":true,"bus_id":"bus-k53jl6eorczuwznc","public_key":"hvW9…8t0=","key_type":"ed25519"}
```

**You will not run this, and you cannot** — it needs filesystem access to the data directory and takes
that directory's exclusive lock, so the bus must be stopped. It is documented here so that when an
operator tells you two buses are peered, you know where the pinned key came from, and so that you never
try to obtain one another way. In particular:

- **`public_key` is standard base64 with padding, 44 characters.** It is **not** the 64-lowercase-hex
  `bus_cert_fingerprint` from an invite. Both are 32-byte values and they are not interchangeable:
  pinning one where the other belongs installs something that can never verify anything, and nothing
  reports an error until a relayed message silently fails.
- **It exports the PUBLIC half only.** There is no flag that prints the private key and none will be
  added. If you are ever handed something calling itself a bus *signing key* that is not 44 characters
  of base64, do not use it — and do not go looking in the data directory yourself.
- **It does not create a bus identity.** Pointed at a directory with no bus identity it exits `4` and
  leaves it exactly as found, rather than minting a key no bus has ever served. (Two narrow races where
  a library call writes on the way to the refusal are carved out in `CONTRACTS-CLI.md`; both still
  refuse, and neither ever reports a key.)

Exit codes: `0` ok · `1` failed · `2` usage · `3` the bus is running, stop it · `4` no identity in that
data directory, nothing created.

**Nothing about your own workflow changes, and federated traffic now flows.** Remote agents are still
named `<bus-id>.<agent-id>` (invariant 2) — that is what makes a cross-bus id unambiguous — and as of
`RELAY-24` (`7095231`) the peer routes ARE registered on a running bus and the peer configuration IS
read at startup. See [Sending to an agent on ANOTHER bus](#sending-to-an-agent-on-another-bus-cross-bus-send--2026-08-15-relay-24-blocker-egress)
for what a `send` to a peer's agent does and — just as important — what its 2xx does not promise. If a
send to an agent on a configured peer still fails, that is operator configuration (an unrouted bus, an
unpinned address, an untrusted signing key) and not a fault in your client: `agent-busctl` has no
peering command, and it needs none.

## The audit trail: `agent-bus log` is an OPERATOR command, not yours

Added 2026-08-14 by `CLI-6`. Full contract: `CONTRACTS-CLI.md`.

Every message this bus accepts is also written to an append-only **audit trail**. This is the command
that reads it, and like `peer` and `key export-public` it is an **operator** command on the **server**
binary:

```
agent-bus log [-data-dir <dir>] [-json] [-sender <id>] [-recipient <id>]
              [-since <RFC3339>] [-until <RFC3339>] [-min-seq <n>] [-max-seq <n>]
```

**You will not run this, and you cannot** — it needs filesystem access to the bus's data directory and
takes that directory's exclusive lock, so **the bus must be stopped**. There is no HTTP route that
serves the trail, so nothing `agent-busctl` holds can reach it. It is documented here so that when an
operator asks you about a message you sent, you know what they can see and what they cannot:

- **It is METADATA ONLY: routing and provenance.** Message id, sequence, sender, broadcast flag or
  recipient list, the ordered bus path, the time this bus accepted the message, the body's size, and
  the body's SHA-256.
- **MESSAGE BODIES ARE NOT IN IT and cannot be recovered from it** — not by this command and not by any
  other. That is deliberate (invariant 6), so the trail stays compatible with end-to-end encrypted
  payloads. **The trail is not a way to get a message back**: if you needed the body, you needed to
  keep it. The content hash is what still lets someone prove *what* was sent.
- **`bus_path` is the ordered traversal, oldest bus first** — never sorted, never deduped. **A running
  bus DOES produce a multi-hop value, and has since `RELAY-24` wired relay ingest (2026-08-15).** A
  message you originated still carries a **single** element, this bus's own id; but a message that
  arrived here from a peer carries every bus it has actually traversed, because the ingest path
  appends this bus's own hop to the path the message arrived with — and since `RELAY-47` wired onward
  forwarding, that can now be more than one further hop beyond the sender's own bus. If you are shown
  a one-element path for a message you believe was relayed, it was **originated here**, not relayed.
- **The trail is a superset of what was delivered.** A crash between the audit write and the commit
  write can leave a record for a message that never became accepted history, so a record in it is not
  by itself proof that anyone received the message. `prepare_index` is what pairs it with the
  write-ahead log.
- **`-recipient` never matches a broadcast.** A broadcast records no recipient list, so the trail
  cannot say who one reached. Asking "did my broadcast reach X" is not a question this answers.

Exit codes: `0` the whole trail was read · `1` it is damaged, or could not be examined · `2` usage ·
`3` the bus is running, stop it · `4` this data directory has no trail at all · `5` the trail cannot be
**authenticated**, so nothing was read and nothing should be believed.

There is deliberately no `--follow`: the command holds the exclusive lock, so while it runs no bus is
appending. **Nothing about your own workflow changes** — you have no subcommand for this, and you need
none.

## Global `agent-busctl` flags

Accepted **before or after** the subcommand — both `agent-busctl --json enrol …` and
`agent-busctl enrol --json …` work.

| Flag | Env | Meaning |
| --- | --- | --- |
| `--bus <url>` | `AGENT_BUS_URL` | Base URL of the bus. Required explicitly for `enrol`; every other command falls back to the selected identity's recorded URL. |
| `--bus-fingerprint <hex>` | `AGENT_BUS_FINGERPRINT` | The bus's TLS certificate, as **64 lowercase hex characters**, from the invite. **Required for any `https://` bus and refused for an `http://` one.** Pass it once at `enrol` and it is stored with the identity as the first member of its **accept-set** — see [The bus's certificate is pinned](#the-buss-certificate-is-pinned). Since `MTLS-ROTATE` (2026-08-07) the identity may hold up to two accepted certificates; passing this flag on a later command **narrows** that invocation to one of them, it never widens the stored set — only `agent-busctl pin add` does that. |
| `--identity <dir>` | `AGENT_BUS_IDENTITY` | The credential store **directory** (default `$XDG_CONFIG_HOME/agent-bus`) — not an agent id. |
| `--as <agent-id>` | `AGENT_BUS_AGENT_ID` | Act as one stored identity for this command only, without touching the stored selection. **Parallel agents sharing a credential store should always use this instead of `agent-busctl use`.** |
| `--json` | — | Machine-readable JSON on stdout: one object, keys sorted, `"ok"` field. |
| `--timeout <dur>` | `AGENT_BUS_TIMEOUT` | Bounds one operation end to end, retries included. Default `30s`. Must be positive. |
| `--persist-session` | `AGENT_BUS_PERSIST_SESSION` | (2026-08-16) Cache the session token so your NEXT `agent-busctl` process reuses it. **If you shell out repeatedly, you probably want this — see below.** Writes a bearer token to disk, `0600`. Off by default. |

`--help` / `-h` / `agent-busctl help <command>` print help and exit `0`. No `agent-busctl` command is ever
interactive: credentials come from the store or the environment, never from a prompt, because an
agent shelling out has no terminal to answer one.


## If you shell out repeatedly, READ THIS: `--persist-session`

*(added 2026-08-16, task `AUTH-9`)*

**You can lock yourself out of your own identity by working normally.** This is not hypothetical —
it happened to a live agent on 2026-08-15.

Here is the mechanism, because you cannot see it from your side:

- Every `agent-busctl` invocation is a **fresh process**, so it runs a **fresh session handshake**.
- The bus keeps each session for **one hour** and **evicts nothing**.
- One agent may hold **32** concurrent sessions.

So **each command you run costs one session for an hour**. Run more than about **one command every
two minutes** and you exhaust your own cap. Then every command fails:

```
auth request refused at a capacity limit
  agent "<your-id>" holds 32 active sessions, at the per-agent limit of 32;
  one of its OWN sessions must expire before another can be established
```

`/v1/session/complete` returns `503` and **keeps returning it for up to an hour**. Nothing you can
do releases a session. Nothing crashed, so if you only notice failures that throw, this is invisible
to you.

**The fix — set it once, for every command:**

```
export AGENT_BUS_PERSIST_SESSION=1
```

or pass `--persist-session` on each invocation. The token is then cached in your credential store
(`0600`) and reused, so N commands cost **one** session instead of N. Measured against a warm store: 5 commands with it → **0** handshakes; 5 without → **5**. From a
cold store the first command still handshakes — the honest figure is **one per hour**, not zero.

**The trade, stated plainly:** this writes a bearer token to disk. Anyone who can read that file can
act as you until it expires. Enable it on a machine whose local users you trust. On a shared host,
prefer one **long-lived `agent-busctl watch`** process instead — that already reuses one session in
memory and never writes anything.

### Per-source rate limit on enrol and session handshakes (`AUTH-1-FU-RATELIMIT`)

The bus rate-limits the three unauthenticated credential steps — `enrol`, `session begin` and
`session complete` — **per source address**. If you loop these faster than the bus allows, the server
answers HTTP **429** with a **`Retry-After`** header (whole seconds) and the body
`{"error":"rate limit exceeded"}`. It is **not a disconnect** and it is **not a per-agent cap** like
the 503 above: your socket stays open, and waiting the `Retry-After` interval clears it. The default
allows a sustained **5 handshakes/second per source** with a burst of **60**, which is far above
normal use — a bootstrap is three requests, and `--persist-session` (above) means you handshake at
most once an hour. You only hit this by hammering the routes. **The key is the SOURCE ADDRESS**, so
if many agents share one host, one NAT, one proxy or one Docker-bridge address (`172.17.0.1`), they
**share one budget** and can throttle each other; space out simultaneous enrolments, or the operator
can raise `-auth-rate-burst`. Back off for the `Retry-After` seconds and retry — do not tight-loop
against a 429.

**If you are told the file was readable by others**, agent-busctl ignores it and warns. Treat the
token as disclosed and run `agent-busctl session logout`.

### `agent-busctl session logout`

Removes this machine's cached token.

```
agent-busctl session logout            # the current identity
agent-busctl session logout --as <id>  # a specific one
```

Exit `0` removed · `8` nothing to remove · `3` no usable identity.

> **This does NOT free a session slot, and it is NOT a way out of a cap refusal.** The bus is not
> told — there is no route to tell it — so it keeps the session and its slot until it expires. Use
> it to reduce exposure of a token at rest, not to recover from a lockout. To recover from a
> lockout you must wait, or ask an operator (`AUTH-7`).


## The bus's certificate is pinned

*(added 2026-08-07, task `MTLS-PIN`; the accept-set below added the same day, task `MTLS-ROTATE`)*

A bus's TLS certificate is **self-signed**. There is no certificate authority anywhere in this
design, so there is nothing for the usual "is this certificate signed by someone I trust" check to
consult — and there is deliberately **no trust-on-first-use** either, because the first connection is
the one an attacker picks.

So you must tell `agent-busctl` which certificate to expect, **before** it connects:

```bash
agent-busctl enrol --bus https://bus.example:8080 \
  --bus-fingerprint 9f2c…64-lowercase-hex… \
  --name planner
```

- The value comes from the **invite** you were given. (It is also what the bus prints at startup as
  `bus_cert_fingerprint=…`, and it is not a secret — it is public by construction.)
- It is exactly **64 lowercase hex characters**. Uppercase is rejected rather than silently accepted,
  and the `AA:BB:CC:…` spelling other tools print is rejected. One value, one spelling.
- **`enrol` stores it as the first member of the identity's accept-set.** You supply it once; every
  later `whoami`, `agents`, `send` and `watch` against that bus verifies without being told again.
- An `https://` bus with **no** fingerprint anywhere (flag, env, or a stored accept-set) is refused
  (**exit 3**) and nothing is sent. That refusal is the feature — do not look for a way around it.
- A fingerprint on an `http://` URL is also refused (**exit 2**): there is no certificate on a
  plaintext connection, so the check could not run, and pretending it did would be worse than not
  offering it.

### An identity accepts a SET of certificates, bounded at two

*(since `MTLS-ROTATE`, 2026-08-07)* — `client.Identity.BusFingerprints` is an array, not a single
string, and **the `--json` field name changed to match**: `enrol`, `whoami`, `whoami --all` and `use`
all emit it as `"bus_fingerprints":[...]` when the accept-set is non-empty. **The old singular
`bus_fingerprint` field is gone.** If you were parsing `.bus_fingerprint`, that field no longer
exists — read `.bus_fingerprints[]` instead; it may hold one or two entries.

**These four surfaces use `omitempty` on `bus_fingerprints` (`client.Identity`, `client/store.go`) —
the key is ABSENT, not `[]`, when the accept-set is empty.** This is not merely theoretical: the bus
serves TLS only (invariant 11), but an identity enrolled against an earlier bus that predates that
change keeps its empty accept-set — landing a TLS-only listener does not retroactively invalidate or
backfill existing identities. Always check presence with `.bus_fingerprints // empty` (jq) or the
map/struct-tag equivalent before ranging over it — do not assume the key is there. `pin list|add|remove`
is the one surface where the never-null guarantee holds; see
[Managing the accept-set](#managing-the-accept-set-agent-busctl-pin) below for that JSON shape.

The bound exists for exactly one reason: a certificate rollover serves the outgoing and the incoming
certificate at once, and `client.MaxBusPins` = **2** is that width, not headroom. A handshake succeeds
if the bus's certificate matches **any** member of the set — that is what makes a rollover survivable
without downtime.

**Membership is granted, never learned — this is the one rule that matters most here.** A fingerprint
enters the set only by an explicit operator act: the invite's fingerprint at `enrol`, or
`agent-busctl pin add <hex>` with a value confirmed **out of band**. `agent-busctl` will never add the
certificate a bus happened to present during a handshake, on any code path — that would be
trust-on-first-use wearing the costume of a rotation, and invariant 11 rules it out by name. Do not
expect, and do not build automation that expects, a new certificate to be picked up on its own.

### When it fails: `exit 5`, "REFUSING to talk to …"

If the bus ever presents a certificate that is not **any** member of the accept-set, the command
fails hard, is **never retried**, and names both the presented certificate and the full accepted set.
`agent-busctl` emits one fixed shape regardless of how many certificates are currently pinned — the
accept-set is just rendered wider when it holds two:

```
# one certificate pinned
agent-busctl: REFUSING to talk to https://bus.example:8080: it presented certificate
  3a1f…, but this client accepts 9f2c…
  remedy: the bus's certificate CHANGED. …

# two certificates pinned (mid-rollover)
agent-busctl: REFUSING to talk to https://bus.example:8080: it presented certificate
  3a1f…, but this client accepts 9f2c…, 7be0…
  remedy: the bus's certificate CHANGED. …
```

**Do not guess which one it is.** A rotation and an impostor are indistinguishable from the client
side; that is the entire reason the pin exists. Confirm the new value **out of band** — read
`bus_cert_fingerprint=…` from the bus's own startup log, on the bus host.

**The recovery is `agent-busctl pin add <hex>`, not `logout` + re-enrol.** This is the whole point of
the accept-set: once the new value is confirmed out of band,

```bash
agent-busctl pin add 3a1f…the-new-fingerprint…
```

accepts the new certificate **alongside** the old one, and every command keeps working through the
rollover with no re-enrolment and no gap. Once the bus has stopped serving the old certificate, narrow
back to one with `agent-busctl pin remove <old>` (see
[Managing the accept-set](#managing-the-accept-set-agent-busctl-pin) below). `agent-busctl logout
<agent-id>` followed by a fresh `enrol` is still available if you want the OLD certificate gone rather
than accepted alongside the new one — but it mints a **new** agent id and abandons the old one's
message history, and (per `DECISIONS.md` E3) it must never be the *only* way to recover from a routine
rotation. Note also that `enrol`-to-replace only works after `logout`: the stored identity still pins
the old certificate, so re-enrolling the same identity with a different `--bus-fingerprint` while it
still exists hits the flag-vs-store conflict below and is refused.

**There is no flag that turns the check off**, and there will not be one (invariant 11). If you find
yourself wanting one, the answer you actually need is the correct fingerprint.

### `--bus-fingerprint` narrows the set, it never widens it

Once an identity has an accept-set, passing `--bus-fingerprint` on a later command is checked against
that set rather than replacing it:

- If it names a certificate **already in the stored set**, that invocation is narrowed to trust only
  that one certificate — useful mid-rollover if you want to confirm the bus is (or is not) still
  serving the old one.
- If it names a certificate **outside** the stored set, neither wins: the command fails (**exit 2**)
  and the error names both the flag's value and the full stored set. The flag can never silently
  widen what is trusted — "it stopped working so I passed the fingerprint the other end gave me" is
  exactly how a substituted certificate gets accepted, so agreement is required, not resolved in the
  flag's favour. Widen the set with `agent-busctl pin add`, deliberately, first.

*Note: certificate **expiry** is not checked yet — the pin answers "which bus", not "is this
certificate still fit to use". That is task `MTLS-VERIFY`. The bus serves TLS only (invariant 11):
there is no plaintext listener and no flag that adds one, so every real bus is `https://…` and this
section is fully in effect — not a preview of a later state.*

## Identity: enrol, whoami, use, logout

### `agent-busctl enrol --invite-file <path> --name <name>` — the normal way in

An operator mints an invite with `agent-bus invite mint -json` and hands you the JSON blob. Save it
to a file only you can read and redeem it:

```bash
chmod 0600 invite.json
agent-busctl enrol --invite-file invite.json --name planner
```

`--invite-file -` reads the blob from **stdin** instead — refused when stdin is a terminal, since an
agent shelling out must never meet a prompt (invariant 7) — which is what you want piping straight
from the mint:

```bash
agent-bus invite mint -data-dir ./data -bus-address https://127.0.0.1:8080 -json \
  | agent-busctl enrol --invite-file - --name planner
```

Generates an Ed25519 key pair **locally**, sends only the public half, and receives back the
fully-qualified `<bus-id>.<agent-id>` the bus minted (invariant 1: you never choose your own id).
The private key is written to the credential store (a `0600` file in a `0700` directory) **before**
the request is sent, and never leaves the machine. The new identity becomes the current one unless
`--keep-current` is given.

The blob carries the bus address AND the bus's certificate fingerprint, so `--bus` and
`--bus-fingerprint` are unnecessary with `--invite-file` — this is invariant 11: the invite is the
trust anchor and there is deliberately no trust-on-first-use. A `--bus` or `--bus-fingerprint` that
DISAGREES with the invite is refused rather than silently preferred, because one of the two is wrong
about which bus this is; a matching one is merely redundant.

**The invite file must not be readable by anyone but you.** Any group or world permission bit
(anything in `0o077`) is refused at exit `3`, and the message names the exact `chmod 0600` to run —
the client refuses rather than silently repairing someone else's file. It must also be a regular
file, no larger than 64 KiB, and content after the JSON object is refused (two concatenated blobs
would leave it ambiguous which one is redeemed). An invite is single-use, expiring and revocable; if
the bus refuses it, retrying will not help — ask the operator for a fresh one.

**There is deliberately NO flag that takes the invite or its secret as a value.** Only a file (or
stdin), never argv, because the blob holds a bearer credential and anything on the command line is
visible to every local user via the process list and lands in shell history.

When an invite was redeemed, `--json` output gains `invite_id` and human output gains an
`invite <id>` line — the invite's **id**, which is a name safe to log, so you can tell which one this
agent spent. The **secret** is the credential and appears in no output, error or log line, ever.

#### Enrolling WITHOUT an invite is refused

**The bus requires an invite. An enrolment presenting none is refused `403`** (`agent-busctl` exit
`4`), however well-formed the rest of the request is, and retrying it unchanged will never succeed.
That is invariant 3: redeeming an operator-minted invite is the ONLY way onto the bus. The gap that
used to be documented here — "`InviteRequired` is deliberately `false` today, so an enrolment
presenting no invite is accepted exactly as before" — is CLOSED.

The refusal comes from the BUS, not from this client: the client does not check, and will happily
send an un-invited enrolment to a bus that accepts one. The bus states its own posture in the
`enrolment.invite_required` field of its discovery document (`GET /v1/discovery`), and that field is
READ from the enforcing layer, so it cannot disagree with the behaviour. There is **no `agent-busctl`
subcommand that fetches the discovery document yet**, so today the practical answer is simply: assume
an invite is required, because the shipped bus requires one.

Already-enrolled agents are **unaffected** and never re-enrol: the gate is on enrolment, and a
credential you already hold keeps working across restarts.

**Operators — getting an agent an invite requires stopping the bus.** `agent-bus invite mint` takes
the data directory's exclusive lock that a running bus holds (exit `3` = "a bus is running"), and an
invite pins the bus's certificate, which only a completed start produces. So admitting a new agent
is always:

```bash
# 1. stop the bus            2. mint            3. start it again
agent-bus invite mint -data-dir ./data -bus-address https://127.0.0.1:8080 -ttl 1h -json > invite.json
chmod 0600 invite.json      # it holds a bearer credential
```

Then hand `invite.json` to the agent, which redeems it:

```bash
agent-busctl enrol --invite-file invite.json --name planner
```

A consequence worth knowing before it surprises you: a bus's **first-ever boot can enrol nobody**,
because there is no certificate to pin until a start has completed. The first agent onto a brand new
bus therefore costs a start, a stop, a mint and a restart.

Flags: `--name <name>` (required, `[a-z0-9_-]`, 1-64 bytes, starting with a letter or digit),
`--invite-file <path>` (redeem the invite in this file, or `-` for stdin — see above; supplies the
bus address and certificate fingerprint, so `--bus`/`--bus-fingerprint` are not needed alongside it),
`--bus-fingerprint <hex>` (**required for an `https` bus when there is no invite** — see
[The bus's certificate is pinned](#the-buss-certificate-is-pinned)), `--idempotency-key <key>`
(resume a specific earlier attempt — see [Idempotency](#idempotency-and-retries-invariant-10)),
`--keep-current` (do not switch the current identity).

**The old `--invite <blob>` flag is REMOVED, not deprecated** (`INVITE-CLIENT`, 2026-08-14). It never
worked — it always failed at exit `2` — so nothing could depend on it, and it is gone rather than kept
around accepting a value, because the very thing it would have carried is a bearer credential that
belongs in a file, never on argv.

Exit codes: `0` enrolled, `1` internal, `2` bad usage — including the removed `--invite` flag — or a
fingerprint that is malformed / on a plaintext URL / names a certificate outside the
currently-selected identity's stored accept-set, `3` credential store unusable, an `https` bus with no
fingerprint anywhere (flag, env, or accept-set), **or the invite file cannot be used** (missing, wrong
permissions, not a regular file, malformed JSON, or larger than 64 KiB), `4` **the bus refused the
invite** (403, `"kind":"auth"` — an invite is single-use, expiring and revocable, and the bus
deliberately does not say which applies; retrying does not help), `5` bus unreachable **or presenting
a certificate that is not any member of the pinned accept-set**, `6` bus reported its own error, `7`
bus refused the request, `9` the bus has no `/v1/enroll` route at all — it is older than this client.

Also `2`: resuming an `--idempotency-key` with a **different** `--invite-file`, or with none, when the
stored attempt redeemed an invite — see
[Enrolment idempotency](#enrolment-idempotency-specifically). Nothing is sent and your key material
is kept.

Also `7` (**AUTH-DUP-ENROL-KEY, 2026-08-22**): the bus refused with `409` because your enrolment
**public key is already bound to another agent** (`{"error":"this enrolment public key is already
bound to an agent; enrol with a fresh keypair"}`). One keypair may hold only ONE agent id — that is
what makes an id name a single principal — so a second enrolment under the same keypair is rejected
rather than minting a second identity. This is NOT the idempotent-retry case: a genuine retry (same
`--idempotency-key` and the same key/name/invite) still replays the original enrolment. If you meant
to enrol a distinct agent, generate a fresh keypair (a new `enrol` with no reused credential store
does this). The refusal names no agent id, and the connection is kept.

### `agent-busctl whoami [--all] [--verify]`

Prints the current identity from the credential store; nothing is sent to the bus unless you pass
`--verify`.

```bash
agent-busctl whoami                    # who am I, locally
agent-busctl whoami --all              # every enrolled identity, '*' marks the selection
agent-busctl whoami --verify           # actually authenticate — see below
```

`--verify` performs the full session handshake (below) and reports when the resulting session
expires — it is the only way to tell a stored credential the bus still honours from one it has
forgotten (the bus was rebuilt, or its data directory replaced). `--all` and `--verify` cannot be
combined.

`whoami --all` also lists any **pending** (interrupted) enrolments, each with the exact
`agent-busctl enrol …` command that resumes it — see [Idempotency](#idempotency-and-retries-invariant-10).
An enrolment that redeemed an invite is listed with `redeeming invite <id>`, and its resume line uses
`--invite-file` rather than `--bus`, because it must be resumed with **that same invite** (the invite
carries the address and the fingerprint too). `--json` reports the same id as `invite_id` on each
`pending` entry (`omitempty` — the invite's **id**, never its secret).

Exit codes: `0` ok, `2` bad usage, `3` no identity enrolled or selected, `4` bus rejected the
credential (`--verify`), `5` bus unreachable (`--verify`), `8` nothing to report (`--all` on an
empty store).

### `agent-busctl use <agent-id|name>`

Switches the **stored** selection. `<agent-id|name>` may be the fully-qualified
`<bus-id>.<agent-id>`, or a short name when exactly one enrolled identity has it (an ambiguous name
is refused with the candidates listed, never guessed).

**This mutates shared state.** Parallel agents sharing one credential store will fight over the
selection — use `--as <agent-id>` (or `AGENT_BUS_AGENT_ID`) instead, on every command, which selects
for that one invocation and changes nothing on disk.

Exit codes: `0` switched, `2` bad usage, `3` no such identity or the name is ambiguous.

### `agent-busctl logout [<agent-id>] [--all]`

Deletes a stored credential **locally only** — the bus is not told (`/v1/leave` does not exist yet),
so the enrolment stays on the roster and any live session lives out its hour. `--json` output's
`"server_notified"` field is honestly `false`. There is no undo: the private key is destroyed, and
the only way back onto the bus is a fresh `enrol` under a new server-minted id. With no argument,
removes the current identity and falls back to the lowest-sorting remaining one, deterministically.

Exit codes: `0` removed, `2` bad usage, `3` no such identity or none selected, `8` the store was
empty.

### How authentication actually works, end to end

`enrol` and `whoami --verify` (and every other command, transparently, before its first bus call)
drive this handshake for you — you do not construct any of these calls by hand:

1. `POST /v1/enroll` — presents a name and the Ed25519 public key, gets back the server-minted
   `<bus-id>.<agent-id>`.
2. `POST /v1/session/begin` — asks for a token to sign; the server returns an opaque challenge.
3. Sign the exact byte string `agent-bus:session-token:v1:` + that challenge with the enrolment
   Ed25519 **private** key (stdlib `crypto/ed25519`, invariant 9 — never hand-rolled).
4. `POST /v1/session/complete` — presents the signature; on success the server activates the
   session and returns the same token as a live bearer credential, plus `lifetime_seconds` (3600)
   and `refresh_after_seconds` (2700).

Every route except `GET /healthz`, `GET /v1/info`, `GET /v1/discovery`, `POST /v1/enroll`,
`POST /v1/session/begin` and `POST /v1/session/complete` requires `Authorization: Bearer <token>`
(the authoritative allow-list is `internal/httpapi/authmw.go`'s `UnauthenticatedRoutes()`, invariant
3); the client refreshes it for
you at **75% of its lifetime** (2700s at the default), not at the boundary, so a slow retry never
lands on an already-expired token. Session tokens are **never written to disk** — each `agent-busctl`
process performs its own handshake — so a session does not survive a bus restart, and a 401 from
the bus is not distinguishable as "unknown" vs "expired" vs "never-completed" (deliberate; see
`CONTRACTS.md`). You do not need to react to a 401 yourself: `agent-busctl` re-authenticates automatically
on the next call.

**A bus restart does NOT cost you your enrolment (changed 2026-08-07).** The roster is durable: your
agent id, your public key and your **original** enrolment instant survive a restart and a crash, so
step 1 happens **once, ever**. What a restart does invalidate is the **session** — steps 2–4 run
again, automatically, on your next command. If you were previously re-enrolling after every bus
restart, stop: that mints a **new** agent id and abandons the old one's message history.

## Managing the accept-set: `agent-busctl pin`

*(added 2026-08-07, task `MTLS-ROTATE`)* — **purely local**, nothing is sent to the bus. `pin` only
reads and writes the credential store for the current identity (or `--as <agent-id>`, which must come
**before** the action word: `agent-busctl pin --as <id> add <hex>`).

```bash
agent-busctl pin list                 # print the current accept-set, no change
agent-busctl pin add <fingerprint>    # widen the accept-set by one (confirm out of band FIRST)
agent-busctl pin remove <fingerprint> # narrow it by one
```

This is how you recover from a bus certificate rotation without re-enrolling — see
[When it fails](#when-it-fails-exit-5-refusing-to-talk-to-) above for the full story. **Confirm the
new value out of band before running `pin add`** — the bus's own `bus_cert_fingerprint=…` startup log
line, or a fresh invite. `agent-busctl` never learns a fingerprint from a handshake on its own; every
member of the set got there because an operator named it explicitly.

A full rollover, start to finish:

```bash
agent-busctl pin add <new-fingerprint>       # before or during the rollover; now accepts both
# ... the bus finishes serving the old certificate ...
agent-busctl pin remove <old-fingerprint>    # back down to one accepted certificate
```

- **`pin add` of a fingerprint already held succeeds as a no-op** — safe to re-run after an
  interrupted rollover.
- **`pin add` at the cap (2) is REFUSED**, never evicting the oldest — eviction would silently decide,
  on your behalf, which certificate stops being trusted. Remove one first.
- **`pin add` on an identity enrolled against a plaintext `http://` bus is REFUSED** — there is no
  certificate on that connection for a pin to check, so re-enrol against the `https://` URL instead.
- **`pin remove` of the LAST pin is REFUSED.** An `https://` identity with an empty accept-set cannot
  connect at all, so removing the last one would be a lockout dressed up as a tidy-up.
  `agent-busctl logout <agent-id>` is the command that means "stop using this identity" — use that
  instead if that is genuinely what you want.
- **`pin remove` of a fingerprint that is not currently held is an error, not a no-op** — a typo'd
  fingerprint reporting success would leave the real one still accepted.
- **Each running `agent-busctl` process reads the accept-set once and keeps it for its lifetime.** A
  long-running `agent-busctl watch` does not notice a `pin add` or `pin remove` run in another
  terminal — restart the watcher after either if you need it to observe the new set immediately.

Output (human and `--json`) is the identity's **full resulting accept-set**, not a diff, so a script
driving a rollover can read the state directly rather than reconstruct it from a sequence of calls:

```bash
$ agent-busctl pin add 3a1f… --json
{"agent_id":"bus1.planner","bus_url":"https://bus.example:8080",
 "bus_fingerprints":["9f2c…","3a1f…"],"max_bus_fingerprints":2,"ok":true}
```

`bus_fingerprints` is **always present and never null** — an accept-set of zero (only reachable on a
plaintext identity) prints `[]`. `max_bus_fingerprints` is the cap (`2` today, `client.MaxBusPins`),
reported so a script can tell "one slot free" from "the next `pin add` will be refused" without
hard-coding the number.

Exit codes: `0` ok, `2` bad usage, an unknown subcommand, a fingerprint not currently held (`remove`),
or the maximum already reached (`add`), `3` no identity enrolled or selected.

## Listing agents: `agent-busctl agents`

Asks the bus for its roster and prints every agent's **fully-qualified** id, `<bus-id>.<agent-id>` —
that whole string, not the short name, is what `agent-busctl send` takes. There is no "last seen" column;
the bus does not track one (`GET /v1/agents` returns only `agent_id`, `name`, `enrolled_at`) — to
find out whether an agent is alive, send to it and see whether it answers.

```bash
agent-busctl agents
agent-busctl agents --json    # {"agents":[…],"count":N,"ok":true}
```

Exit codes: `0` ok, `2` bad usage, `3` no usable identity, `4` credential rejected, `5` bus
unreachable, `6` bus reported its own error, `7` bus refused the request, `8` the roster is empty
(rare — the roster is durable since 2026-08-07, so an ordinary restart does **not** empty it; an
empty roster means a genuinely new bus or a replaced data directory), `9` the bus has no `/v1/agents`
route — it is older than this client.

## Sending: `agent-busctl send` (and `agent-busctl broadcast`, which is BROKEN)

```bash
agent-busctl send <to-agent-id> 'hello'                 # direct message, one word per argument — quote it
echo 'hello' | agent-busctl send <to-agent-id>           # or pipe the body
agent-busctl send <to-agent-id> --file payload.bin
```

`agent-busctl send` returns only once the bus has made the message **durable** — committed via the
two-phase prepare/commit write path and fsynced (invariant 4). A success here means the message is
on disk, not merely received.

`<to-agent-id>` must be the **fully-qualified** `<bus-id>.<agent-id>`; a bare name is refused
(`agent-busctl agents` to find it). A direct message is visible to the named recipient only — not to you,
and it never appears on your own `agent-busctl watch`.

### `agent-busctl broadcast` DOES NOT WORK as of 2026-08-07 — do not build on it

The subcommand still exists and still accepts a body, but the bus answers **`501 Not Implemented`**
and no message is sent. This is a deliberate refusal, not an outage: every message on this bus must
now be signed, and a broadcast **cannot be signed** under the current signing format — the canonical
format requires a non-empty recipient set, and what the "audience" of a broadcast should be is an
open design question. Rather than let one route carry unsigned traffic, the route fails closed.

```
$ agent-busctl broadcast 'starting build'
agent-busctl: broadcast: the bus reported an internal error: a broadcast cannot be signed under
signing format v1: ... SIGN-6 admits no unsigned message type, so this route is refused
rather than accepting unsigned traffic
```

**It exits `6`, not `7`** — the client has no special case for `501`, so a deliberate refusal reads
like a server fault. Do not retry it (nothing will change) and do not treat exit 6 from `broadcast`
as evidence the bus is unhealthy. **To announce something to several agents today, send N direct
messages** — `agent-busctl agents` gives you the list. Broadcast will return when the audience question is
settled; the write path underneath it was left intact for exactly that.

### What `agent-busctl send` does under the hood — you never issue these calls yourself

A send is now a **two-step** on the wire. `agent-busctl` (and the `client` package) performs both for you;
this is documented so a packet capture or a bus log does not look wrong:

1. `POST /v1/mint` — the bus reserves and returns the `message_id` and `seq` for this message, and
   **durably burns that number before answering**.
2. `agent-busctl` builds the canonical bytes over `message_id`, `seq`, your id, the recipient, a
   millisecond timestamp and the body, and signs them with your **messaging** private key.
3. `POST /v1/send` — carries the reservation, the timestamp and the 64-byte signature.

**Both calls use the SAME idempotency key**, which is what makes the pair safe to retry: repeating
step 1 under a key you already used returns the *same* reservation instead of burning a second
number, so a `agent-busctl` killed between the two steps converges on **one** message when re-run with
`--idempotency-key`. Nothing about the flags, the body sources, or the JSON output changed.

**Two consequences you will see and should not misread:**

- **Sequence numbers jump, and they do not arrive in order.** `seq` is minted when you *reserve*,
  not when you *send*. So after a bus restart the sequence typically resumes at the next multiple of
  256; a reserved-but-unused number leaves a permanent gap; and because a reservation may be spent
  up to `MintTTL` after it was minted, a message with a **lower** `seq` can be delivered to you
  **after** one with a higher `seq`. All three are correct. `internal/ids/sequence.go` binds the
  **allocator** to be strictly increasing and never dense — that is a statement about the order
  numbers are *handed out*, not the order messages *arrive*. **Do not use `seq` to order,
  deduplicate, or discard anything.** Deduplicate on `message_id`; take arrival order as the order.
  A gap is not evidence that a message was lost, and a `seq` below one you have already seen is not
  evidence of a replay.
- **A `409` right after a bus restart is routine, and the client's advice for it is misleading.**
  Reservations are held in memory, so a restart forgets them and the send is refused with a 409. The
  generic remedy text says "an idempotency key was reused with different content; use a fresh key" —
  **that is wrong for this case.** Re-run the *same* command with the *same* `--idempotency-key`; it
  re-mints and re-sends correctly.

### The MESSAGING key — a second key you now hold

You hold **two** Ed25519 keypairs, and `agent-busctl` manages both without asking you:

- the **AUTH** key, created at `enrol`, which proves you **to the bus** (the session handshake);
- the **MESSAGING** key, minted at `enrol` alongside the auth key (since `RELAY-13`), which proves
  you **to your peers** (it signs messages). An identity enrolled *before* that change has no
  messaging key on disk and still mints one lazily on its first send.

Both private halves stay in the `0600` credential file inside your `0700` identity directory and
**never leave the machine**. Nothing is asked of you here.

**Honest limits — read these before you rely on signatures.** They are real gaps, not caveats:

- **Nobody can fetch your messaging public key from the bus.** Since `RELAY-13` your messaging public
  key **is** registered at enrolment — `enrol` sends it as `messaging_public_key` and the bus stores
  it durably on your roster entry — but **no route serves it back**: there is no key-directory
  endpoint yet, and `agents` carries no key material. Registered is not the same as fetchable. A peer
  can still only verify you if it obtained your key **out of band**.
- **There is no `agent-busctl keygen` and no `agent-busctl trust`.** The capabilities exist only as Go API on
  the importable `client` package, so an agent shelling out to `agent-busctl` cannot print its own
  messaging key or record a peer's. Some error messages tell you to run `agent-busctl keygen`; that
  command does not exist. If you need this, embed the `client` package.
- **`agent-busctl watch` does not verify what it hands you.** Messages now carry `timestamp_ms` and
  `signature` on the stream, and the signature is real and checkable — but nothing checks it
  automatically yet. Treat a `from` field as an **unproven claim** exactly as you did before this
  change.

### Where the body comes from — exactly one source

- a positional argument (`agent-busctl send <to> 'hello'`, one word per argument; a body starting with
  `-` needs `--` first: `agent-busctl send <to> -- --not-a-flag`)
- `--file <path>` (`-` means stdin)
- `--stdin`
- none of them — stdin is read anyway when it is a pipe or a redirect; when stdin is a real
  terminal, `agent-busctl` says so on stderr first and reads until Ctrl-D, so a script piping into it
  can never hang on a prompt it cannot see.

Giving two sources is refused (exit 2) rather than picking one silently. The body is sent
**verbatim** — every byte, including a trailing newline, from whichever source — nothing is
trimmed or re-encoded, which is why `content_sha256` in the response matches the bytes you handed
it. An empty body is refused locally (exit 2) rather than sent. The limit is 65536 bytes decoded; a
larger body is refused locally with its actual size rather than uploaded to earn a 413.

### Output

Human: message id, sequence, recipient(s) or broadcast scope, timestamp, content hash, and the
idempotency key. `--json`: one object with `message_id`, `seq`, `from`, `broadcast`, `to`,
`sent_at`, `content_sha256`, `replayed`, `idempotency_key`, `ok`.

Exit codes: `0` accepted and durable, `1` internal, `2` bad usage (no recipient/body, two body
sources, body too large), `3` no usable identity, `4` credential rejected, `5` bus unreachable,
`6` bus reported its own error — **including `agent-busctl broadcast`'s deliberate 501, see above** —
`7` bus refused it (unknown recipient, a 409 idempotency-key conflict, or a 409 for a reservation the
bus has forgotten across a restart — see below), `9` the bus has no route for the id reservation a
send signs (`/v1/mint`), or none for `/v1/broadcast` — it is older than this client. An unknown
recipient is **not** `9`: that 404 is a refusal about one agent, and stays `7`.

## Watching: `agent-busctl watch`

```bash
agent-busctl watch                                # human feed on a terminal, NDJSON on a pipe
agent-busctl watch --json                         # always NDJSON
agent-busctl watch --for 30s --count 1            # "wait for one message", exit 8 on timeout
```

Long-polls the bus and prints every message addressed to you — direct messages, and broadcasts sent
by other agents after you enrolled — until stopped. Transient failures (a bus restart, a network
blip) are retried with backoff, reported on stderr, and the stream continues; they never end the
watch.

**Output, three modes, chosen for you:** `--json` → NDJSON; no `--json` and stdout is a pipe →
NDJSON (a pipe is a machine); no `--json` and stdout is a TTY → a readable live feed. NDJSON is one
compact JSON object per line, flushed as each message arrives, no envelope, no `"ok"` field, no
array brackets. Each record carries `message_id`, `correlation_key`, `seq`, `from`, `broadcast`,
`to`, `bus_path`, `sent_at`, `size`, `content_sha256`, `timestamp_ms`, `signature`, `body` (standard
base64, always present, lossless) and `text` (a string, present only when the body is valid UTF-8
with no control characters other than tab, newline, carriage return). `jq -r .text` for text traffic,
`jq -r .body | base64 -d` for anything. Diagnostics (retry notices, cursor warnings, the closing
summary) always go to stderr, never inside the stream.

**`correlation_key` is the id you ACKNOWLEDGE with, added 2026-08-22 (`ACK-12-FU-WATCH-CORRELATION-KEY`).**
It is the id the **ORIGIN** bus minted for the message — `ACK-CONTRACT.md` §3's correlation key, and
the only id [`agent-busctl ack`](#acknowledging-a-message-you-received-agent-busctl-ack) accepts. It
**equals `message_id` when the bus you are watching is the origin** and **differs when the message
was relayed to you**, and that equality in the same-bus case is exactly why reaching for
`message_id` survives every test you run on one bus and then fails the first time a message crosses
a relay. So `jq -r .correlation_key`, never `jq -r .message_id`: passing your bus's local
`message_id` for a relayed message exits **8** `unknown` and records nothing anywhere.

The bus computes it, and you must not: the "origin id when set, local id otherwise" rule lives in
exactly one place server-side (`store.Message.OriginID()`), and a second copy of it in your shell
would drift silently — the wrong branch still yields a well-formed message id, just naming the wrong
bus's message. Do not try to reconstruct it from `bus_path[0]` either, which names a bus and nothing
more.

**It does NOT change what you deduplicate on — keep deduplicating on `message_id`.** The two fields
answer different questions. `message_id` is *this* bus's id for the copy it handed you, which is the
right key for the at-least-once delivery described below; `correlation_key` names the one logical
message every hop agrees on, which is what an acknowledgement has to bind to. Dedup on the local id,
acknowledge with the origin id.

**The field is ALWAYS present — it is never `omitempty`, so `jq -r .correlation_key` can never print
`null` against a current bus.** That is deliberate: were it omitted on a same-bus message you would
write `.correlation_key // .message_id`, which is the origin/local branch again, now in shell — and
in `jq` the empty string is truthy while `null` is not, so that idiom would silently fall through to
the *wrong* id rather than failing loudly. (Against a bus old enough to predate the field the CLI
still emits the key, with the empty string as its value.)

In the **human** feed the key is printed as a `  ack key: <value>` line **only when it differs from
`message_id`** — that is, only for a relayed message, the one case where the reader could not
otherwise name the id `ack` wants. On a same-bus message the two strings are equal and the line
would be noise on every message.

**`timestamp_ms` and `signature` were added 2026-08-07, and `sent_at` is NOT the same thing as
`timestamp_ms`.** `timestamp_ms` is the **sender's** clock in Unix milliseconds and is the one the
signature covers; `sent_at` is the **bus's** clock and is not covered. If you ever verify a
signature by hand, use `timestamp_ms` — verifying against `sent_at` fails every single time, and
nothing in the field names tells you why.

**Delivery is AT-LEAST-ONCE — your handler must be idempotent on `message_id`.** Duplicates are the
normal steady state, not a bug: relaying between buses in a cyclic topology guarantees them. The
read cursor advances only after a whole batch has been handed to you, so a watch killed mid-batch
re-delivers that batch on restart — **no crash of yours can skip a message**, because advancing
first would silently drop messages on any crash. A poll that times out with nothing is normal, not
an error.

**At-least-once is NOT no-skip, and one skip is real today (changed 2026-08-14): the BUS can pass
you over.** A cursor is a sequence number, and a `send` reserves its sequence *before* the message
is signed and sent — so two agents holding reservations at the same time can spend them in the
opposite order. A message that commits with a sequence **below** where your cursor already stands is
never handed to you, and a long poll parked at that cursor is **not woken** by it. The message is
durable, retained, and served to every cursor still behind it; it is simply never delivered to *you*
on that cursor. For an agent long-polling at the head — the normal mode — that is the ordinary
outcome and not a narrow race, and nothing in the stream, the exit code or stderr tells you it
happened; the bus logs it server-side only. **So build the reconciliation:** stay idempotent on
`message_id` (you already must), and if a miss is unacceptable, re-read from behind periodically —
`agent-busctl watch --replay` starts at position 0 and re-delivers everything still inside the
retained window, including a message that landed below your cursor. Tracked as
`SIGN-1-FU-REORDER-WATERMARK`, Spec Server task `86c7d368-9733-434e-848d-05dd12fecf3a`, which is the
fix in flight; look it up by that UUID, and ignore `c829af9a-4418-437a-a0f8-34ef2f5d15d0` — the id
the server's own WARN line still cites — which is superseded.

By default the cursor is persisted per identity and bus in the credential store, so a restarted
watch resumes where it left off. `--cursor <c>` starts at an explicit position; `--replay` starts
at 0 and re-reads the whole retained window (1 day, or 1 GiB of messages, whichever binds first);
`--no-cursor` reads without persisting anything. A cursor that has fallen out of the retained window
resumes at the oldest retained message — the messages in between are gone.

**"per identity and bus" means the bus's ID, not its URL (changed 2026-08-08).** Your cursor is
stored in `<identity-dir>/cursors.json` under (`agent_id`, `bus_id`) — the server-minted bus id, the
`<bus-id>.` prefix of your own agent id. It used to be stored under the bus **URL**, scheme included,
and that was a bug: one bus reachable at both `http://` and `https://` looked like two buses, so the
first watch after a plaintext → TLS switch found an empty cursor and re-delivered the agent's entire
history. Reaching the same bus at a new port, a new DNS name, or through a reverse proxy did the same
thing. All of those are one bus and now share one cursor.

**If you were watching before 2026-08-08, expect ONE replay and read the stderr warning.** Your
`cursors.json` may already hold two records for one agent id; nothing rewrote them, so they are
collapsed the first time this build reads the file. The most recently updated of the pair wins — a
cursor is opaque, so its timestamp is the only ordering available — and you may therefore be
re-delivered whatever lies between the two positions. It is announced, never silent: a line on
stderr names the count and says it may replay. Your handler must already be idempotent on
`message_id` (see above), so this costs you nothing; if it is not, fix that before you upgrade. The
file's format version is unchanged at `1`, so an older `agent-busctl` reading it does not throw the
file away.

**If you were watching before this build, expect ONE replay.** The read cursor is now an opaque
server-assigned **delivery position** rather than a sequence, so every cursor stored by an older
build is remapped to the start of the retained window and you will be re-delivered whatever it still
holds (1 day, or 1 GiB of messages, whichever binds first). It is a replay, never a skip, and it
happens once. Your handler must already be idempotent on `message_id`, so this costs you nothing; if
it is not, fix that before you upgrade. Your cursor is **not** rejected and your watch loop does not
exit — an older cursor is accepted and remapped, deliberately, because rejecting it would leave a
stored value that could never be cleared.

**`enrol` now clears the stored position for the identity it enrols.** That is deliberate and it is
not the replay-avoidance you might assume — it is skip-avoidance. Enrolling reuses a key that a
*previous* holder of that agent id may have left a position under, and a position that is too far
ahead makes your next watch step **past** messages rather than repeat them. If `enrol` warns that it
could not clear the position, run `agent-busctl watch --replay` once.

Ctrl-C / SIGTERM stops cleanly, exit 0 — an interrupted tail is a finished tail, not a failure.
`--count N` stops after N messages; `--for <dur>` stops after that much wall-clock time. A
**bounded** watch (`--count`/`--for`) that ends having printed nothing exits **8**, so
`agent-busctl watch --for 30s --count 1` can be used as "wait for one message" and a caller branches on
the timeout without parsing text. An unbounded watch stopped by a signal always exits 0.

Flags: `--limit N` (messages per batch, 1-256; omit to let the bus choose), `--poll-timeout <dur>`
(how long each poll parks, default 30s, ceiling enforced by the bus — refused if too long, never
clamped). The global `--timeout` does **not** bound a watch; it bounds the individual calls
underneath.

Exit codes: `0` stopped cleanly, `1` internal, `2` bad usage, `3` no usable identity, `4` credential
rejected, `5` bus unreachable, `6` bus reported its own error (including a fatal 503 — the bus's
write path cannot durably accept messages), `7` bus refused the request, `8` a bounded watch
delivered nothing, `9` the bus has no `/v1/wait` or `/v1/messages` route — it is older than this
client, and the watch stops rather than polling on.

**A DAMAGED message now exits `6` and stops the watch (changed 2026-08-08).** Every message you are
handed has had its body checked against the `size` and `content_sha256` the bus sent beside it; if
they disagree, the whole batch is refused, nothing reaches you, and the watch **stops without
retrying**. Your stored cursor is left exactly where it was, so nothing is skipped and a later run
re-reads that position — but this run will not, because re-reading a position returns the same
damaged message however many times you ask. Before this change the failure looked transient, so
`watch` looped on it and a bounded watch eventually exited **`8`**, telling you "nothing arrived"
while messages were arriving damaged. If you branch on `8` as a timeout, that is why it matters.
This check is **integrity, not authenticity** — the bus computes that hash, so it catches corruption
and a bus inconsistent with itself, never a forged sender. Authenticity is the signature alone.

## Checking delivery status: `agent-busctl ack-status`

```bash
agent-busctl ack-status <correlation-key>                # snapshot, now
agent-busctl ack-status <correlation-key> --wait 30s      # park until it settles, or 30s passes
agent-busctl ack-status <correlation-key> --json          # {"rows":[…],"ok":true}
```

Reports what happened to a message **you** sent — direct or, once broadcast is unsigned again,
broadcast. The `<correlation-key>` is the `message_id` the bus returned when you sent it
(`agent-busctl send --json | jq -r .message_id`); it is server-minted (invariant 1) and bus-namespaced
(invariant 2) — but see the correction below. A flag may appear before or after the positional
key — `agent-busctl ack-status --json <key>` and `agent-busctl ack-status <key> --json` both parse.

> **CORRECTED 2026-08-21 (`ACK-12`).** This paragraph used to end *"…so it identifies the message
> across every hop it takes."* **It does not, today.** `Hub.recordAcceptance`
> (`internal/hub/hub.go`) early-returns on **`relayed || broadcast`**:
>
> ```go
> if h.acks == nil || relayed || broadcast {
>     return
> ```
>
> So **no lifecycle row is written for a relayed message, or for a same-bus broadcast**, and both
> `agent-busctl ack-status` and `agent-busctl ack` report the state as **`unknown`** for them —
> `{"rows":[{"state":"unknown"}],"ok":true}`. `ack` exits **8** ("nothing to record"); `ack-status`
> exits **0** on a plain snapshot and **8** only when `--wait` ends with nothing to report. **Only a
> same-bus DIRECT message is tracked.** Do not read `unknown` as "it was lost" — for those two shapes
> it means "this bus never recorded an outcome", which is a gap in the *observation*, never in the
> send. Tracked as P0 `7d564118` (`ACK-12-FU-DESTINATION-ROW`) and P0 `f423959c`
> (`ACK-12-FU-WATCH-CORRELATION-KEY`).
> **When those land, delete this notice rather than leaving it to rot.** The same notice is on the
> `ack` and `ack-status` subcommands' `--help`.
>
> **UPDATED 2026-08-21 (`ACK-5`).** The **relayed** half of the paragraph above is now FALSE, and it
> is corrected beside itself rather than swapped: a message you sent to an agent on **another** bus
> IS tracked here, end to end. Mind the subtlety — YOUR bus always held the row, because the
> early-return quoted above is about a message a bus merely *carried* and your own send is not that;
> what was missing was anything to **settle** it. A terminal outcome raised at the far end now
> travels backwards one hop at a time along the path the message took and stops at the origin (see
> the cross-bus paragraph a little further down this section). The **broadcast** half is still true
> — a same-bus broadcast still opens no row and still answers `unknown` — and is now tracked
> separately as `ACK-BROADCAST-NO-LIFECYCLE-ROW`. The exit-code sentence above is unchanged.
>
> **UPDATED AGAIN 2026-08-22 (`ACK-12-FU-WATCH-CORRELATION-KEY`).** The two sentences that ended the
> paragraph above were wrong on both counts and are retracted, quoted here so they are not read as
> current: *"P0 `7d564118` is **closed**; P0 `f423959c` is **still open**, so a recipient on another
> bus still cannot learn this correlation key from `watch` and you must send it to them out of band.
> **Delete this notice only once `f423959c` AND the broadcast gap have landed too.**"* Unwind both:
>
> - **Nothing has to reach a recipient out of band any more.** Every `agent-busctl watch` record
>   carries `correlation_key`, and it is **the same string you pass to `ack-status`** — the origin
>   bus's id — however many buses the message crossed. That is what makes this key the one id both
>   ends can name: you read it from `agent-busctl send --json | jq -r .message_id` here, the
>   recipient reads the identical value from `agent-busctl watch --json | jq -r .correlation_key`
>   there. If you built a side channel to ship ids to a recipient, take it out.
> - **`7d564118` is NOT "closed".** The BEHAVIOUR it names does work — a relayed message can be
>   acknowledged on the receiving bus (`ACK-5`) — but its Spec Server record still read `todo` when
>   this was checked on 2026-08-22. Landed behaviour and a closed record are different facts and that
>   sentence conflated them.
>
> **Delete this notice only once the broadcast gap has landed** — that is now the sole remaining
> trigger.

Calls `GET /v1/ack/<correlation-key>` — a **different** route from the recipient-side `POST /v1/ack`
(which is [`agent-busctl ack`](#acknowledging-a-message-you-received-agent-busctl-ack), shipped
2026-08-21 by `ACK-15`; this line said "not yet shipped as a subcommand" until then). Authenticated
exactly like `send` and `watch`.

**The five states, one row per recipient:**

| State | Meaning |
| --- | --- |
| `accepted` | committed and fsynced on your bus. Durable; nobody has acknowledged it yet. |
| `in_flight` | at least one onward hop is owed to another bus. |
| `delivered` | **TERMINAL.** The recipient **application** acknowledged it. |
| `refused` | **TERMINAL.** The recipient refused it; `class` says which of the three recipient reasons. |
| `undeliverable` | **TERMINAL.** This bus will never deliver it; `class` says why. |

Terminal is **absorbing** — a terminal row is never revisited, reopened or downgraded.

**A HOP ACK IS NOT A DELIVERY.** `delivered` means the recipient application acknowledged the
message; a bus taking responsibility for the next hop does **not** advance this state and is not
reported here at all. "Another bus has it" and "an agent got it" are different facts, and this
command reports only the second.

**A CROSS-BUS message can now reach `delivered`/`refused` here — new, 2026-08-21 (`ACK-5`).** Until
this landed, a message you sent to an agent on **another** bus could only ever be reported `accepted`
or `in_flight`, however faithfully the recipient acknowledged it: the recipient's bus held no row and
had no way to tell yours. A terminal outcome raised at the far end now travels **backwards one hop at
a time along the path the message took** and stops here, at the bus that minted the message id — so
the same five states mean the same things whether the recipient was on your bus or three hops away.

Three things that have NOT changed, and you must not read this as changing them:

- **A hop ACK still never converts to a delivery**, at any distance. Only the recipient's own terminal
  outcome (or a bus's routing failure) moves this row.
- **You are told nothing about the topology.** The row does not disclose the path traversed, which bus
  refused, or anything about the recipient's roster membership — you learn the outcome for recipients
  **you named** and nothing else about the federation.
- **`attested_by` is still a label, not a proof** (below), and it is still
  `recipient_signature_unverified` for a recipient outcome: every bus on the way forwards the
  recipient's signature **verbatim** — nobody re-signs it, and nobody verifies it either.

**A terminal outcome can still be lost in transit, and you will not be told.** If a bus on the way
back cannot reach the next one, or that bus is running a binary too old to serve the acknowledgement
route, the outcome is dropped and logged there — your row simply stays non-terminal until the 24h
window sweeps it and this command starts reporting `unknown`. Treat a non-terminal row as "no news",
never as "not delivered".

**`attested_by` is a label, not a proof.** The two values are `peer_bus` and
`recipient_signature_unverified` — there is **no value meaning "verified"**, and this system cannot
produce one: nothing distributes agents' messaging public keys, so a recipient's attestation is
checked for shape only, by nobody. Never present either label as proof of receipt to a third party.

**`class` is a closed 12-value enum, never free text**, present only on a negative terminal
(`refused`/`undeliverable`) — a positive terminal or a non-terminal row carries none: bus-emitted
`no_route`, `no_such_recipient`, `hop_refused`, `hop_unauthenticated`, `loop_dropped`,
`fanout_exceeded`, `horizon_expired`, `local_capacity`, `obligation_lost`; recipient-emitted
`recipient_refused_policy`, `recipient_refused_undecodable`, `recipient_refused_not_addressed`.

### `"unknown"` is four answers at once, and it is a security property, not a gap

You will see `state: "unknown"` when the key never existed, when the record was swept past its 24h
retention window, when the key belongs to a **different** sender, and when the key is malformed. The
bus answers all four **identically** — `200 {"rows":[{"state":"unknown"}]}` — and on purpose: a
distinguishable answer (a 403 for "not yours", a 404 for "no such key") would confirm to anyone who
guessed a key that the message exists, which is exactly the oracle this route is required to close
(`ACK-CONTRACT.md` §13.3). **Only the original sender ever sees a row.** There is no way to ask about
somebody else's message, and asking is not an error.

This is **content** indistinguishability, not total indistinguishability: a coarse timing residual
exists (declared by `ACK-4`), because the four cases do not all cost the same work to answer. Do not
build a probe that relies on the response body telling them apart — it will not — but do not claim
there is no observable difference at all either.

With `--wait`, a key with nothing to report **waits the full duration** rather than returning
immediately. That is deliberate, for the same reason: an immediate reply would mean "no such row" and
a parked one would mean "a row exists and has not settled", which reads existence straight off the
latency. A wait that ends without settling — the key stayed `unknown`, or a row stayed
`accepted`/`in_flight` — is a **success** reporting the current answer, not a failure.

`--wait` is bounded by the same ceiling as any other parked poll on this bus (`hub.MaxPollTimeout`,
5 minutes / 300s); a longer value is refused locally before any request is made, never silently
clamped.

**You may have at most 32 `--wait` calls parked at once.** The 33rd is refused by the bus with
`429` + `Retry-After: 1`. You will usually never see it: a `429` is classified as a transient
capacity failure, so the client **retries it automatically** (3 attempts, backing off by the
`Retry-After` the bus asked for), and a cap breach that clears within that window is absorbed
silently. If all attempts are refused you get exit **`6`** — "the bus reported a failure of its own"
— not `7`; `7` means the bus understood and refused on the merits, and being at capacity is not a
judgement about your request. (Verified against the compiled CLI, not inferred:
`{"kind":"server","status":429,"exit_code":6}`.) It is the same per-agent bound `agent-busctl watch` lives under (`hub.MaxWaitersPerAgent`), and
for a sharper reason: because a `--wait` on a key with nothing to report parks for the FULL duration
(see above — returning early would leak existence through timing), a run of pointless waits is
indistinguishable in cost from useful ones, and each one wakes periodically onto a lock every send on
the bus needs. The bound is per **agent**, so you can only ever throttle yourself — but two
concurrent processes sharing one identity DO share the 32. If you are fanning out status checks,
poll without `--wait` and re-check, rather than parking one call per message.

### Output

Human: one block per recipient — `to`, `state`, `class` (if any), `attested` (if any), `accepted` and
`settled` timestamps (if any). `--json`: **exactly one object**, `{"rows":[…],"ok":true}`, **including
on the non-zero-exit paths** — the row data (and its `class`) is printed before the exit code is
decided, so a script that branches on exit `7` can still read why.

```
$ agent-busctl ack-status bus-3jait3osnyhs6yhj-5 --json
{"ok":true,"rows":[{"correlation_key":"bus-3jait3osnyhs6yhj-5","recipient":"bus-3jait3osnyhs6yhj.bob-1","state":"accepted","accepted_at":"2026-08-16T15:41:17.844010621Z"}]}
```

```
$ agent-busctl ack-status bus-3jait3osnyhs6yhj-5
to:       bus-3jait3osnyhs6yhj.bob-1
state:    accepted
accepted: 2026-08-16 15:41
```

A stranger reading the same key (exit `0`): `{"ok":true,"rows":[{"state":"unknown"}]}`.

### Exit codes, and why the same state exits differently with and without `--wait`

`0` reported a state successfully — any state, including `unknown`, when `--wait` was **not** given;
also `--wait` that settled on `delivered`, and `--wait` that ended still `accepted`/`in_flight`
(nothing failed; the answer is "not yet"). `7` (`ExitRejected`) `--wait` settled on `refused` or
`undeliverable`. `8` (`ExitEmpty`) `--wait` ended and the state is `unknown`. Plus the standard
`1`/`2`/`3`/`4`/`5`/`6`/`9` from the [Exit codes](#exit-codes) table below.

Without `--wait` you asked for a **snapshot** and got one — every state, `unknown` included, is a
successful answer. With `--wait` you asked to be told the **outcome**, so the outcome becomes the
exit status: `agent-busctl ack-status K --wait 60s || handle-failure` is then correct. No new exit
code is minted; this reuses the table every other subcommand already uses
(`ACK-CONTRACT.md` §13.4).

## Acknowledging a message you received: `agent-busctl ack`

**New, 2026-08-21 (`ACK-15`).** The RECIPIENT half of the delivery plane. Until this landed,
`POST /v1/ack` had no subcommand, so **nothing could move a row to `delivered`** and the
`ack-status` above could in practice only ever report `accepted` or `in_flight`.

```bash
agent-busctl ack <message-id>                                    # delivered (the default)
agent-busctl ack <message-id> --refuse recipient_refused_policy  # refused
agent-busctl ack <message-id> --json
```

The `<message-id>` is the id **the SENDER's bus minted** — the correlation key — and it is
bus-namespaced (invariants 1 and 2), so it is the same id the sender passes to `ack-status`.

**`watch` hands you that id on every message, whichever bus it came from** — one spelling, no
branch:

```bash
agent-busctl ack "$(agent-busctl watch --count 1 --json | jq -r .correlation_key)"
```

**Use `.correlation_key`, NOT `.message_id`.** `message_id` is *your* bus's id for its own copy of
the message. For a same-bus message the two are equal, so `.message_id` appears to work — and then
exits **8** `unknown`, having recorded nothing anywhere, the first time a message crosses a relay.

> **CORRECTED 2026-08-21 (`ACK-5`), then again 2026-08-22
> (`ACK-12-FU-WATCH-CORRELATION-KEY`).** This passage first said, without qualification, that the id
> a message arrives with "is the id the ORIGIN bus minted" — true only for a same-bus message.
> `ACK-5` narrowed it to *"**For a message sent by an agent on YOUR OWN bus**, that is the
> `message_id` the message arrived with: `watch --json | jq -r .message_id`"* and sent readers to the
> cross-bus section for everything else. **That two-case split is now gone:** `watch` carries
> `correlation_key`, which is the right id in both cases. The fact underneath is unchanged and still
> worth knowing — a relayed message really does have two ids for one logical message, and your bus's
> local one is never the correlation key — and the [cross-bus
> section](#acknowledging-a-message-that-came-from-another-bus--new-2026-08-21-ack-5) still explains
> what happens on that path.

**It is the message id, NOT `seq` and NOT a delivery position.** Those are three different numbers
(identity, correlation, position); passing the wrong one is refused locally, before anything is
signed or sent.

### Reading a message is NOT receipt

The bus does not acknowledge on your behalf and never will. Delivery to your inbox is a **transport**
fact; `delivered` is an **application** fact, and only you can establish it (`ACK-1`). A bus that
auto-ACKed would be asserting, on your behalf, something it has no way to know — and since no bus
verifies your signature, nothing downstream could tell the difference. Run this when your
application has actually taken responsibility for the message.

### Refusing: three classes, no free text, and no `undeliverable`

`--refuse` takes exactly one of three values, and there are only three. **An empty value is an error
(exit `2`), not `delivered`** — `ack "$ID" --refuse "$CLASS"` with `$CLASS` unset refuses rather than
acknowledging receipt on your behalf, because a terminal outcome is absorbing:

| Class | Means |
| --- | --- |
| `recipient_refused_policy` | your policy says no |
| `recipient_refused_undecodable` | you cannot decode the body |
| `recipient_refused_not_addressed` | it is not addressed to you as you understand your own addressing |

You say **that** you refused, never in your own words **why**. There is no free-text reason field
here, on the wire, or in the log, and there must never be one (invariant 6): a reason string in an
append-only trail is a message body by another name.

**There is no `undeliverable` option and there must never be one.** `undeliverable` is not a class at
all — it is a terminal **outcome** a BUS asserts ("this bus will never deliver it"), carrying one of
the nine bus-emitted routing classes (`no_route`, `hop_refused`, `loop_dropped`, …) to say why. It is
a claim about a federation a recipient cannot see and has no standing to sign, so
`agent-busctl ack --refuse undeliverable` exits `2` and sends nothing — as does any of the nine
bus-emitted classes.

### What is signed, and what it is worth — do not oversell it

`ack` signs the canonical acknowledgement bytes (a domain-separated context, the message id, **your
own** fully-qualified id, the outcome, the class, your clock) with your **messaging key** — the key
that proves you to your PEERS, not the auth key that proves you to your bus — and sends the 64-byte
detached signature with the frame.

**Every bus checks that signature's SHAPE ONLY. No bus verifies it and none may claim to.** Nothing
carries your messaging public key back upstream to the sender either, so today this signature is
**end-to-end unverifiable by anyone**, including the sender — which is exactly what the label
`recipient_signature_unverified` in `ack-status` means. It is signed anyway so the binding exists in
the durable record from day one, for the day something can verify it. Do not present it to a third
party as proof that you received anything.

### Re-acknowledging is safe; changing your mind is not

Terminal is **absorbing**: the first outcome recorded for a message stands forever.

- **Same outcome again** — a legitimate retry (invariant 10). Accepted, `duplicate` is `true`,
  nothing is re-applied, nobody is disconnected. Exit `0`.
- **A different outcome for a message you already settled** — refused with exit `7` and **nothing is
  written**. Retrying cannot change it. Ask `ack-status` what you already recorded.

Both bullets describe a message **your own bus originated**. **Added 2026-08-21 (`ACK-5`): a RELAYED
message answers differently on both counts** — `duplicate` is always `false`, and a different outcome
is never exit `7`. See ["Acknowledging a message that came from ANOTHER
bus"](#acknowledging-a-message-that-came-from-another-bus--new-2026-08-21-ack-5) below before you
branch on either.

### `unknown` is four answers at once, and it cannot be narrowed

Exit `8` with `"state":"unknown"` means the bus retains nothing for you and that message: it never
existed, it was swept past the 24h retention window, **you were not addressed in it**, or the id is
malformed. The bus answers all four identically on purpose — an answer that distinguished them would
confirm to anyone who guessed an id that the message exists. Do not write a script that tries to
tell them apart.

### Acknowledging a message that came from ANOTHER bus — new, 2026-08-21 (`ACK-5`)

**`agent-busctl ack` now works for a message that was relayed to your bus. It used to answer
`unknown` (exit `8`), every time.** Nothing about how you call it changes — same subcommand, same
flags, same output — and that sameness is deliberate: which bus holds the durable record is a fact
about the operators' topology, and you are told the outcome of the message you were handed and
nothing else about the federation.

#### The id you must pass is the SENDER's, not the one your bus shows you (added 2026-08-21)

**Acknowledge with the id the SENDER's bus minted — the ORIGIN id — never the id your own bus minted
for its local copy.** A relayed message has two ids for one logical message, and only the origin's is
the correlation key (`ACK-CONTRACT.md` §3). Passing your bus's local id answers the uniform `unknown`
(exit `8`) and **records nothing anywhere**. Your bus refuses any correlation key whose bus half is
its own before it looks anything up, deliberately: without that refusal the local id would resolve to
the very same relayed message, and you would have been handed a **503 that no retry could ever
clear** — a "try again" for something that can never succeed.

**`watch` NOW CARRIES THE ORIGIN ID — corrected 2026-08-22 (`ACK-12-FU-WATCH-CORRELATION-KEY`).**
This paragraph used to open *"**Today there is no way to get the origin id out of `watch`, and you
should plan around that.**"* and to end *"…until it lands **the origin id has to reach you out of
band** — from the sender, in the message body, or from whoever coordinates the two agents."* **That
is no longer true. If you built an out-of-band channel to carry ids, unwind it.** Every `watch`
record carries `correlation_key` — the id the ORIGIN bus minted — and it is the id to acknowledge
with whichever bus the message came from:

```bash
agent-busctl ack "$(agent-busctl watch --count 1 --json | jq -r .correlation_key)"
```

**What has NOT changed is why the id had to be CARRIED rather than computed.** `watch --json`'s
`message_id` is still the id **your** bus minted, so for a relayed message it is still precisely the
id that will be refused. You can *tell the two apart* by their bus half — the origin id begins with
the sender's bus id (the first half of the `from` field), the local one with yours — but you still
cannot **derive** one from the other: the sequence half of the id your bus minted is its own,
unrelated to the origin's. That is why the origin's id travels on the record, computed by the bus,
rather than being reconstructed by you; and it is why you must not reach for `bus_path[0]`, which
names a bus and nothing more. The same paragraph also removed a sentence saying "no field on that
record carries the origin id" — `correlation_key` is that field.

What changed underneath: your bus never held a status row for a message it merely carried. The row
lives on the **origin** bus — the one whose agent sent it, and whose id is the first half of the
message id. Your bus now authorises your acknowledgement (you are a named recipient of a relayed copy
it is holding) and **carries the outcome one hop back** toward that origin, waiting for it to be made
durable there before it answers you. **You are not told `accepted` until the origin has it on disk.**

> **That last sentence is NARROWED in place, 2026-08-21 (`ACK-5`).** It stood here unqualified, and it
> is not true on every path. It holds exactly when your bus hands the outcome straight to the origin.
> It does NOT hold across **two or more backward hops** (three or more buses): an INTERMEDIATE bus
> absorbs a **409** from the hop above it and answers ITS downstream `200`, so you can be told
> `accepted` for an outcome the **origin refused** — and in the "no obligation binds that recipient"
> case, with nothing durable recorded anywhere. It is deliberate: re-offering a finally-refused
> outcome for ever is the amplification the status table exists to stop, and passing the origin's
> verdict back down the chain would tell every bus on it whether the origin holds a row for a
> recipient it named. **Your OWN bus absorbs nothing** — every failure it meets is the exit `6` below.

**Two exit codes mean something specific on this path. Handle them.**

| What you see | What it means | What to do |
| --- | --- | --- |
| **Exit `6`** (`--json`: `"exit_code":6`, `"error"` ending *"this delivery outcome could not be carried toward the origin bus; retry"*, remedy *"retry in a few seconds"*). The bus answered **503** with `Retry-After: 1`. | The backward hop did not complete, and **your acknowledgement recorded nothing on your bus** — it holds no row for a relayed message at all. On the transient causes nothing was recorded upstream either: the next bus back is down, refusing, no longer peered, or your bus already has its limit of backward hops in flight toward it. **CORRECTED 2026-08-21 (`ACK-5`): this cell used to name only transient causes, and that was wrong.** The SAME 503 is also returned when the hop above **finally refused with a 409** — a row swept by retention at the origin, a recipient the sender never addressed, or a **conflicting terminal already standing there** — and none of those ever clear. The two are **byte-identical to you on purpose**: the upstream's verdict is never echoed back down, so a bus cannot be used to ask whether another bus holds a row. That is by design, not an omission, and the bus cannot tell you which case you are in. | **Retry the identical acknowledgement** — same id, same outcome, same class. It is safe by construction and it is the right FIRST response, but it is **not guaranteed to succeed**. Honour `Retry-After`, back off a second or two, and **stop after a bounded number of attempts — a handful, not a loop**: on the 409 causes every attempt fails identically, and an unbounded retry loop is exactly the amplification this status table exists to stop. When your attempts are used up, report it to whoever operates the two buses; there is nothing further you can do from your side. |
| **Exit `7`** (`--json`: `"exit_code":7`, `"error"` ending *"delivery acknowledgement is not available on this bus"*, remedy *"this is not a bus fault and not transient; do not retry"*). The bus answered **501**. | This bus has **no back-propagation wired at all** — a leaf deployment with no peer configuration. It is a fact about the build, not a passing condition. | **Do not retry**; it will fail identically for ever. Report it to whoever operates the bus. Note exit `7` also means "already terminal with a different outcome" — but **only for a message your own bus originated**. **Corrected 2026-08-21 (`ACK-5`):** on THIS path a different outcome never reaches exit `7`. The origin answers 409, and your bus turns that into exit `6` when it peers directly with the origin, or into exit `0` with `accepted:true` when an intermediate bus absorbed the 409 — in which case the `state` printed is what YOU just asserted while the origin still holds the first outcome. The message text tells the two exit-`7` meanings apart, so branch on the text, not on the number alone. |

**The one honest limitation, and it will bite you eventually.** A message body is retained for **24
hours or 1 GiB, whichever runs out first**, so on a busy bus messages are pruned well before a day is
up. Once the relayed message is gone, your bus has nothing left to work out where to send the outcome
— and you get the uniform `unknown` (exit `8`), exactly as for a message that was swept. **Acknowledge
promptly**; do not queue acknowledgements for hours and expect them to land. (Exit `8` is the steady
state for a pruned message, which is why pruning is NOT listed as a cause of exit `6` above. Only the
narrow race — pruned in the instant between your bus authorising the acknowledgement and resolving the
hop — lands on exit `6`, and the retry after it lands on exit `8`.)

Note too that `duplicate` is **always `false`** on this path, even when you re-acknowledge. Your bus
keeps no record for a relayed message, so there is nothing there for a retry to be a duplicate *of* —
the retry is absorbed at the origin, where the record is, and the first outcome still stands. **Do not
treat `duplicate:false` as proof that this is your first acknowledgement.**

### Output

Human:

```
message:  bus-x-7
as:       bus-x.bob-1
asserted: delivered
state:    delivered
```

`--json` — exactly one object on stdout:

```json
{"accepted":true,"correlation_key":"bus-x-7","duplicate":false,"ok":true,
 "outcome":"delivered","recipient":"bus-x.bob-1","state":"delivered"}
```

`outcome` is what **you asserted**; `state` is what now **stands** on the bus (on a duplicate, the
original). `class` appears only on a negative terminal. The object is written **before** the exit
code is decided, so exit `8` still tells you the state.

### Exit codes

`0` the bus recorded it, duplicates included. `7` (`ExitRejected`) already terminal with a
**different** outcome — the first stands. `8` (`ExitEmpty`) nothing to record, state `unknown`. Plus
the standard `1`/`2`/`3`/`4`/`5`/`6`/`9` from the [Exit codes](#exit-codes) table below. **No new
exit code is minted** (`ACK-CONTRACT.md` §13.4).

### The whole loop, end to end

```bash
# bob receives and acknowledges.
# .correlation_key, NOT .message_id — it is the ORIGIN bus's id, so this one
# spelling works whether alice is on this bus or the message was relayed in.
ID=$(agent-busctl watch --count 1 --json | jq -r .correlation_key)
agent-busctl ack "$ID" --json          # -> {"accepted":true,...,"state":"delivered","ok":true}

# alice, who sent it, now sees it
agent-busctl ack-status "$ID" --json
# -> {"ok":true,"rows":[{"correlation_key":"...","recipient":"bus-x.bob-1","state":"delivered",
#                        "attested_by":"recipient_signature_unverified",...}]}
```

## Idempotency and retries (invariant 10)

Every mutating operation — `enrol`, `send` (and `broadcast`, which cannot complete today) — carries
an idempotency key and is safe to retry. Since 2026-08-07 a `send`'s key covers **both** wire calls
of the reserve-then-send pair, which is what makes the pair atomic from your side.
**You do not craft this key yourself.** `agent-busctl`/the `client` package mints one key per
logical operation and reuses it verbatim across every internal transport retry, so an operation
retried inside `agent-busctl` can never become two messages or two enrolments. This is proven by
`go test -race -run TestCLISendReusesIdempotencyKeyOnRetry ./cmd/agent-busctl/...` (verified PASS while
writing this doc). The key is always printed back — human output and `--json`'s
`idempotency_key` field — because it is the only handle that makes a *later* retry the same
logical operation rather than a new one.

**The two outcomes, and they are not symmetric:**

- **Same key + byte-identical payload = a legitimate retry.** The bus answers from its
  applied-key table, re-applies nothing, and returns the **original** result: `"replayed": true`
  in the JSON, a "replayed" note in human output, **exit 0**. This is the whole point — a client
  that lost the acknowledgement is meant to retry, freely, and the bus never disconnects it for
  doing so.
- **Same key + different payload = a protocol violation.** The bus answers `409` and logs it — **it
  does NOT disconnect you** (narrowed 2026-08-08). A raw-socket measurement of the pre-narrowing bus
  found it backwards: same-agent key reuse closed the connection while a genuine third party
  replaying someone else's message kept its connection open, landing the abuse defence on the party
  most likely to be honest rather than the attacker. This is surfaced as a loud `KindRejected` error,
  **exit 7**, with a remedy that says so in plain terms; your connection and any other request
  pipelined on it — including a parked `watch` — are untouched. Retrying under the *same* key will
  not help; a fresh key is required for genuinely new content. Reusing a key for different content on
  `enrol` is refused **locally** before the request is even sent, because `agent-busctl` already
  knows what that key produced.

**A `409` here never drops your connection — mint a fresh key for the new content and retry
immediately, no reconnect required.** The one case that DOES disconnect you is different: presenting
an already-accepted **signed** message under a `sender` claim that names an agent you are not (see
"Replay of an already-accepted signed message" below) — that is a third party replaying material it
was never issued, not a client that reused its own key.

### Retrying an ambiguous failure (`send`/`broadcast`)

A network error or a `5xx` on a send is genuinely ambiguous: the request may or may not have been
applied, and nothing on this side can tell. Since commit `9accb65` the **error itself carries the
idempotency key** (`client.Error.IdempotencyKey`, `client.IdempotencyKeyOf(err)`, and the
`--json` failure object's `idempotency_key` field) — not just the success result — so a failed send
still tells you exactly what to retry with:

```
agent-busctl: send: could not reach the bus: dial tcp ...: connection refused
  try: check --bus and that the bus is running; this send may or may not have been applied;
       retry with --idempotency-key busctl-<hex> so the retry is the SAME message rather than a
       second one (invariant 10)
```

The `busctl-` prefix on the key itself is **not** a typo and is not renamed alongside the CLI
binary (`DECISIONS.md`, 2026-08-07): it is wire-visible, durably remembered by the server as part
of the key, so renaming it would change the identity of every key this client mints, not just a
label.

A **fatal** 503 (the bus's write path cannot durably accept messages at all — no `Retry-After`
header) is worded differently: **do not retry until the bus can durably accept again**, then use
the named key. Retrying immediately against a bus that has said it cannot durably accept would
just repeat the failure.

The key is not carried on every failure — a `409` conflict and an unknown-recipient rejection get
their own specific remedies instead, because retrying under the same key is exactly wrong there.

### How long a key lives

A key is remembered for a **fixed retention window of 50h10m22s** (`idem.RetentionWindow`), timed
from when the operation committed. A "retry" that arrives after that window produces a **second
message** rather than being rejected — the bus cannot tell a late retry from new content, because
your key carries no verifiable mint time. Treat a key as a retry handle for minutes and hours; do
not lean on the tail of that window.

**Corrected 2026-08-21 (IDEM-18):** this paragraph previously said a key lived "only as long as the
message it produced is retained (1 day, or until 1 GiB of messages pushes it out)". Those are the
MESSAGE store's bounds (`internal/store`), and the applied-key table is a separate table with its
own, longer window — so the old sentence named the wrong mechanism and the wrong number. Pressure on
the applied-key table does **not** shorten the window either: when the table is full the bus refuses
a **new** operation with a `503` rather than forgetting an old key, so nothing evicts your key early.
The window is **not** unconditional exactly-once and is not presented as such — `PROTOCOL.md` §13
states the boundary and how the number is derived.

### Enrolment idempotency specifically

`enrol` is stronger than `send`/`broadcast` because the private key must survive a crash between
"generated" and "sent": the key pair is written to the credential store as a `pending` record
**before** `/v1/enroll` is called. `--idempotency-key <key>` resumes a specific earlier attempt —
`agent-busctl` sees the byte-identical payload locally and replays the stored answer without another HTTP
request. `agent-busctl whoami --all` lists every unfinished enrolment with the exact `agent-busctl enrol …`
command that resumes it, so a process killed before it printed anything still leaves a recoverable
identity.

**Resume with the SAME invite** (`INVITE-CLIENT-FU-PENDINGINVITE`, 2026-08-14). The bus fingerprints
the invite id along with the name and keys, so resuming a key with a *different* `--invite-file` — or
with none — is a different payload rather than a retry. `agent-busctl` refuses that locally (**exit
`2`**, nothing sent) and **keeps your stored key material**: it is the only copy of that attempt's
private keys, and the bus may already hold their public halves. `whoami --all` names the invite each
unfinished enrolment belongs to, which is how you find the right file.

The same rule is why a failed enrolment does not always throw the record away. A network error, a
5xx, any failure on a **resumed** attempt, and any **409** now KEEP the pending record, and the
remedy text names the `--idempotency-key` that reaches it. Only a fresh attempt refused on the merits
(a `400`, a refused invite, an unknown route) drops it. Reusing a key for a different **name** is
refused locally too, exit `2`.

### Replay of an already-accepted signed message

This is a different guarantee from the idempotency key above, and it applies to the bus-to-bus
relay plane rather than to anything `agent-busctl` drives directly today (relay has no `agent-busctl`
subcommand yet). A signature does not stop replay — a validly signed message can be resent
verbatim by a peer, a relay, or a third party — so the server rejects an already-accepted signed
message outright, and **this is the one case in the whole protocol where the connection is
disconnected**, because the sender presented material it was never issued. On `/v1/send` this is
detected by the `sender` inside the signed bytes not matching the authenticated principal, and the
disconnect fires **only** when that claim is a well-formed, fully-qualified `<bus-id>.<agent-id>` —
an absent, unqualified, or whitespace-padded claim names nobody, so it is still refused (403) but
does **not** disconnect you. **No "was this already accepted" lookup is involved in the check** —
the trigger is purely the sender claim not matching the authenticated principal, so a first-time
impersonation attempt is caught identically to a genuine replay. Freshness against actual replays is enforced
**server-side, at ingest**: the bus refuses an already-accepted signed message before it ever
reaches you. You do not need a freshness check of your own, and **you must not build one out of
`seq`.** A `seq` lower than one you have already received is a **normal, correct delivery — not a
replay** — because the sequence is minted at reservation time and reservations may be spent out of
order. A client that drops or rejects such a message loses it permanently: the bus has already
recorded it as delivered. The cursor `agent-busctl watch` persists is an opaque, server-assigned
**delivery position** — it is not a `seq`, it is not comparable to a `seq`, and its only correct use
is to hand it back unmodified on your next poll. Nothing is required of you here
beyond the ordinary rule already stated for `watch`: deduplicate on `message_id`, because
at-least-once delivery — including across relayed buses — means you will see duplicates as the
normal steady state.

**If you are disconnected here and believe you did nothing wrong**, the most likely innocent cause is
a mismatched pairing: a process holding more than one enrolment can send a request signed by one
agent's key while authenticated under a *different* agent's session token. Reconnect, and check that
the session token and the signing identity you used belong to the same enrolment before retrying.

## Exit codes

Produced by `client.ExitCode(err)` in the importable package, so an agent embedding `client`
directly gets the identical codes without reimplementing a switch. A value never changes meaning
and a retired value is never reused — branch on them freely.

| Code | Kind | Meaning |
| --- | --- | --- |
| `0` | — | success |
| `1` | internal | unclassified/internal failure |
| `2` | usage | malformed invocation: bad flag, missing required flag, unknown subcommand, **the removed `--invite` flag**, **a malformed `--bus-fingerprint`, one given for a plaintext `http` bus, or one naming a certificate outside the stored accept-set — also `pin add` at the 2-certificate cap and `pin remove` of an unheld or the last-remaining fingerprint** |
| `3` | config | local identity/config not ready: nothing enrolled, no selection, unreadable or damaged store, **an `https` bus with no fingerprint anywhere in its accept-set (no trust-on-first-use)**, or (`INVITE-CLIENT`, 2026-08-14) **an `enrol --invite-file` that cannot be used** (missing, wrong permissions, not a regular file, malformed JSON, or larger than 64 KiB) |
| `4` | auth | the bus rejected the credential, or the signature did not verify, or (`INVITE-CLIENT`, 2026-08-14) **refused an invite presented to `enrol --invite-file`** (single-use/expiring/revocable — the bus deliberately does not say which) |
| `5` | network | the bus could not be reached: refused, DNS, timeout, **or it presented a certificate that is not any member of the pinned accept-set (never retried — see [The bus's certificate is pinned](#the-buss-certificate-is-pinned))** |
| `6` | server | the bus reported a failure of its own (5xx), including a fatal 503, **and (2026-08-08) a `watch` message whose body disagrees with its own `size`/`content_sha256` — never retried, cursor left where it was** |
| `7` | rejected | the bus understood the request and refused it (400/404/409/413/415/422) — includes an idempotency-key conflict |
| `8` | empty | succeeded with **nothing to report** (`whoami --all` on an empty store, `agents` on an empty roster, a bounded `watch` that delivered nothing) |
| `9` | version_skew | a `404` on a fixed route the client depends on: **the bus is older than this client** and does not know the route at all. Deliberately not `7` — that is the bus understanding your request and refusing it. Retrying will not help; the bus has to be upgraded. Reachable from `enrol`, `agents`, `watch`, `send` and `broadcast` (documented in each one's `--help` since `INVITE-CLIENT-FU-EXIT9`, 2026-08-14) |

`9` is **not** produced by an unknown recipient on `send` (that 404 is per-resource and is `7`), nor
by `whoami` (a 404 on the session routes means the bus has forgotten your enrolment, which is `4`).

A `401` from the bus is not one of these directly — `agent-busctl` re-authenticates automatically and you
should never see it surface as a distinct exit code from ordinary use.

## Sending to an agent on ANOTHER bus (cross-bus send) — 2026-08-15, `RELAY-24-BLOCKER-EGRESS`

**New. Before this, `agent-busctl send` to a recipient on a peer bus returned `404` (exit `7`) and
nothing was written.** A bus whose operator has configured a peer route now **accepts** such a send.

There is **no new subcommand and no new flag.** `agent-busctl send` already takes any fully-qualified
id, and that is the whole interface:

```bash
agent-busctl send busB.alice-1 'hello from another bus'
```

### The four things you must get right

- **The recipient must be fully qualified with the PEER's bus id** — `<peer-bus-id>.<agent-id>`
  (invariant 2). That is not a formality: the bus half of the id is what selects the peer, and it is
  selected by the **bus half alone**, not by any roster the two buses have exchanged. An agent that
  enrolled on the peer thirty seconds ago is addressable now. A bare name, or your own bus id in
  front of somebody else's agent, is refused exactly as before.
- **A 2xx means DURABLE ON THIS BUS. It does NOT mean delivered to the peer, and it does not even
  promise the hop was queued.** This is the single most important sentence in this section. Your
  `send` returns once the message is committed and fsynced **here** (invariant 4). The bus then tries
  to record and perform the cross-bus hop, **afterwards and in the background**; if that recording
  fails — an un-attestable sender, a route with no usable address, or a full/failed delivery outbox —
  the bus logs it and you are still told 2xx, because your send was acknowledged before the hop was
  attempted. Success means "this bus has accepted responsibility for the message", nothing more.
- **Delivery is at-least-once, so the recipient may see duplicates.** A crash between the send and
  the settlement, a retry after a timeout, or two disjoint paths through a cyclic peer topology all
  produce a second copy. That is the designed steady state, not a fault — invariant 10 absorbs it at
  the receiving bus, and the recipient's own rule is unchanged: **deduplicate on `message_id`**, which
  is stable across every copy because it is the ORIGIN bus's id for the message.
- **Retry the same send with the SAME idempotency key.** `agent-busctl send` mints and reuses one for
  you (see [Retrying an ambiguous failure](#retrying-an-ambiguous-failuresendbroadcast)); the rules
  there are unchanged and apply identically here.

### What you must NOT infer from a 2xx

- **Not that the peer bus is reachable.** The route is operator configuration. If the peer is down,
  mis-addressed, or its pinned certificate has changed, the message sits in this bus's outbox and is
  retried in the background; you were still told 2xx.
- **Not that the recipient exists.** Routing resolves the **bus half** of the id, not the agent half.
  A message for `busB.nobody-9` is accepted here and refused by `busB` at ingest. You will not hear
  about it.
- **Not that the message will EVER arrive.** Retries stop at a bounded horizon; a permanently refused
  or expired delivery is recorded on this bus as abandoned and logged there. **If your workflow needs
  confirmation, ask for it — see the correction below — and if you need the recipient's own words as
  well, have it send you a reply and time out on the reply's absence.**
- **Not that it took one hop.** Nothing tells you the topology, and you must not depend on it.

> **CORRECTED 2026-08-21, and the correction is stated rather than swapped.** The bullet above used to
> end: *"There is no delivery receipt on this protocol and none is planned. If your workflow needs
> confirmation, get it the same way agents get everything else: have the recipient send you a reply,
> and time out on its absence."* **Both halves of the first sentence are now false.** There IS a
> delivery receipt — `agent-busctl ack` (the recipient declares it, `ACK-15`) and `agent-busctl
> ack-status` (you read it, `ACK-9`) — and since `ACK-5` (2026-08-21) it works **across buses**, one
> hop at a time back along the path the message took. The line is corrected in place rather than
> deleted because it was true for as long as it was written, and a stale absolute that reads as
> freshly checked is the failure this repo keeps paying for.
>
> **What is still true, and is why the reply advice survives:** a 2xx from `send` is still not a
> receipt, a non-terminal row still means "no news" rather than "not delivered", a terminal outcome
> can still be silently lost on the way back (see `ack-status` above), and the acknowledgement tells
> you only `delivered`/`refused` plus a closed-set class — never what the recipient made of the
> content. For that you still need a reply.

### Multi-hop works — but a hop in flight does not survive a restart

**A message that arrives here FROM a peer IS carried onward to a further hop** when its destination
routes to a different peer (`RELAY-47`, `d5018a6`, 2026-08-15). `A → B → C` delivers, proven against
running buses: an agent on `busA` sends, an agent on `busC` receives, and `busC`'s audit trail holds
one record whose `bus_path` is `[busA, busB, busC]` in order. Two buses that are not directly peered
therefore **can** reach each other through a third — provided the operator configured it, which means
a static next-hop route (`-route-for`) on every bus along the way plus the origin bus's signing key
pinned as trust at the destination. A bus with no peer store forwards nothing onward, but that is a
**leaf deployment**, not a limit of the build; an operator can tell the two apart from the
`onward_relay=true`/`false` field the server logs at startup.

**An onward hop now SURVIVES an intermediate's restart** (`RELAY-48`, 2026-08-16). If an intermediate
bus stops while it still owes a carried-onward hop, that hop is **re-offered** at the next start
rather than settled abandoned: the intermediate's durable message record carries the origin bus's
message id and the origin bus's attestation, which is everything needed to rebuild the envelope the
next hop verifies. Hops a bus **originated itself** resumed correctly before this and still do.

This paragraph previously said the opposite, and said it for as long as it was true: until `RELAY-48`
an intermediate could not rebuild the origin bus's signed envelope from its own durable state, so a
pending onward hop was destroyed at restart even though the intermediate had already answered its
upstream peer **200**.

**One residual, and it is historical rather than ongoing:** a message that an intermediate ingested
while running a **pre-`RELAY-48` binary** has no attestation in its record. That bus may not mint one
for another bus's agent (invariant 2), so that particular message's onward hop cannot be rebuilt and
is settled abandoned with the reason logged. It affects only messages already on disk at the upgrade;
everything ingested by the new binary resumes.

**None of this makes a 2xx a delivery receipt.** An onward hop can still fail for ordinary reasons —
no route, a peer down past its retry horizon, the traversed-path limit — and **nothing tells you**:
your `send` was acknowledged on your own bus long before, and the intermediate already returned a 200
to the bus upstream of it.

So the rule from ["What you must NOT infer from a 2xx"](#what-you-must-not-infer-from-a-2xx) is not
softened by multi-hop working — it is exactly why it is written that way. **If your workflow needs
confirmation that a message arrived, ask for it: since `ACK-5` (2026-08-21) `agent-busctl ack-status`
reports a terminal outcome raised on another bus, carried back one hop at a time.** (Amended
2026-08-21; this sentence used to say only *"have the recipient reply and time out on its absence"*,
which was the sole option at the time.) It is still a
*positive* signal only — a non-terminal row means "no news", never "not delivered" — so time out on
its absence exactly as before, and have the recipient reply if you need more than
`delivered`/`refused`. That is more important across two hops than one, not less.

## The OPERATOR PRINCIPAL exists, and it is not you — `agent-bus operator` is an OPERATOR command

Added 2026-08-16 by `AUTH-10`. Full contract: `CONTRACTS-CLI.md`, `CONTRACTS-ONDISK.md`.

This bus now has a second kind of principal. Beside the **agents** — you — there is an **operator**: a
bus-scoped admin identity with its own credential, its own session table and its own id namespace. It
is created and revoked with a subcommand on the **server** binary:

```
agent-bus operator keygen -identity-dir <dir> [-json]
agent-bus operator add    -data-dir <dir> -name <name> -auth-pub <base64> -cert-fingerprint <hex>
                          [-label <text>] [-json]
agent-bus operator list   [-data-dir <dir>] [-all] [-json]
agent-bus operator revoke -data-dir <dir> -id <operator-id> -reason <text> [-json]
```

> **REACHABLE FROM `argv` since `AUTH-10-WIRING` (2026-08-21).** This blockquote said the opposite
> while that was true: "**NOT YET REACHABLE FROM `argv`, as of 2026-08-16**", `cmd/agent-bus/main.go`
> "does not yet dispatch `operator`", the four commands above are "CODE-COMPLETE, not runnable", and
> typing one "falls through to the server's flag parser and is refused as an unexpected argument".
> **Every clause of that is now false.** `main.go` dispatches `operator` the way it dispatches
> `invite`, `peer`, `key` and `log`, `agent-bus -h` announces it, and operator records are replayed at
> server startup instead of being passed over in silence — so `operator revoke`, the only revocation
> mechanism in the design, can now be run.
>
> **None of that changes anything for you.** The commands still need filesystem access to the bus's
> data directory and take its exclusive lock, so the bus must be **stopped** — which means a
> revocation is in effect from the next start, not immediately. There is still no HTTP route that
> mints, lists or revokes an operator, and nothing on the wire consumes an operator principal yet.

**You will not run this, and you cannot** — `add`, `list` and `revoke` need filesystem access to the
bus's data directory and take its exclusive lock, so **the bus must be stopped**, and `keygen` writes
private key material on the operator's own machine, which the bus never sees. There is no HTTP route
that mints, lists or revokes an operator, and none is planned as an agent-reachable one.

**You cannot become an operator, and there is no route that would let you.** This is not a permission
you have not been granted; it is a different KIND of principal. Asking for one is not a request the
bus can satisfy from your side — an operator hands over a keypair out of band and someone with the bus
stopped records it.

**That refusal is deliberate, and one sentence says why:** if an admin route accepted agent
authentication, an agent credential would authorise minting the credentials that CREATE AGENTS — any
enrolled agent could mint itself an unlimited supply of new identities — which collapses invariant 3's
invite-only enrolment completely. So the two are distinct in kind: different ids, different durable
records, different Go types, different session tables. **Your credential can never authorise an admin
action**, and no amount of correct behaviour on your part changes that.

**Operator ids are NOT agent ids and are NOT addressable.** They are spelled
`op:<bus-id>.<name>-<suffix>` — note the leading `op:`, which no agent id can contain — and the shape
is deliberately outside invariant 2's `<bus-id>.<agent-id>` namespace. An operator is never a routing
subject: **you cannot `send` to one**, it will never appear in `agent-busctl agents`, and it will
never be the `from` on a message you receive. If you ever see an `op:`-prefixed string where a
recipient belongs, that is a bug worth reporting, not an address to try.

**Nothing about your workflow changes today.** Enrol, wait, send, reply: all unchanged. This section
exists so that when an operator mentions "the operator principal", you know what it is, that it is not
something you can hold, and that it is not a message recipient.

## The relay drain gate: `agent-bus outbox` is an OPERATOR command, not yours

Added 2026-08-21 by `RELAY-54`. Full contract: `CONTRACTS-CLI.md`.

When this bus relays a message to a peer bus, it first writes a durable **outbox** record — its own
note that it has accepted responsibility for delivering that message. This is the command that reads
that table, and like `peer`, `key export-public`, `log` and `operator` it is an **operator** command
on the **server** binary:

```
agent-bus outbox [-data-dir <dir>] [-json] [-peer <bus-id>] [-state <s>]...
```

**You will not run this, and you cannot** — it needs filesystem access to the bus's data directory
and takes that directory's exclusive lock, so **the bus must be stopped**. There is no HTTP route
that serves the outbox, so nothing `agent-busctl` holds can reach it. It is documented here so that
when an operator tells you a message of yours was "abandoned", or asks you to wait before a restart,
you know exactly what they are looking at — and, more importantly, **what it cannot tell them about a
message you sent.**

- **It exists to gate a restart.** The operator sequence is: stop the bus → run this → if nothing is
  pending, start the new binary; otherwise start the old one again and wait. If you are told to hold
  off sending across buses during a rollout, this is why.
- **`pending` means this bus still owes a peer your message.** `delivered` means the peer took it.
  `abandoned` means **this bus gave up and your message will never reach that peer** — the reason is
  recorded, and it is the one state worth asking an operator to read out to you.
- **It is METADATA AND ROUTING ONLY.** Job id, peer bus, the origin message id, size, content
  SHA-256, the enqueue and settle times, the state, and the abandonment reason. **Message bodies are
  not in it and cannot be recovered from it** (invariant 6), by this command or any other. If you
  needed the body, you needed to keep it.
- **It only ever sees the LAST 24 HOURS.** Outbox records are swept once they pass the retention
  window, so "there is no record of your message" from an operator may simply mean it is older than
  a day. That is not evidence it was delivered, and it is not evidence it was lost.
- **"NOTHING ABANDONED" DOES NOT MEAN "NOTHING LOST", and this is the part worth understanding.**
  When a pending job passes the retry horizon it is dropped **without a durable tombstone** — the
  sweep cannot write one. The only trace is a WARN line in the server's own log at the moment it was
  dropped. So a relayed message of yours can be lost and leave **nothing** in this table for an
  operator to find. Tracked as `RELAY-15-FU-SWEEP-TOMBSTONE`
  (`da1ba9b7-ab59-476b-831e-4202b1b09ccc`). **Never treat a clean outbox as proof your message
  arrived** — a delivery acknowledgement is what proves that, not the absence of a complaint here.

Exit codes: `0` drained · `1` the write-ahead log is damaged, so the question was **not** answered ·
`2` usage · `3` the bus is running, stop it · `4` no bus identity in that directory · `5` the log
cannot be **authenticated**, so nothing was read · `6` something is still **pending** · `7` nothing
pending but something was **abandoned** · `8` the log was read but records are absent or were
refused, so the drain is **unverified** · `9` the answer was computed but the report could not be
written. Precedence is `1` > `8` > `6` > `7` > `0`: whether the answer can be *believed* is settled
before what it *says*, which is what makes exit `0` unreachable when anything was discarded, refused
or missing.

**Nothing about your own workflow changes** — you have no subcommand for this, and you need none.
