# Three buses in Docker — build, run, federate

A start-to-finish runbook: build the image, run one bus with a bare `docker run`, then stand up
three federated buses (**A ↔ B ↔ C**) and get a message from an agent on **A** to an agent on **C**,
transiting **B**.

**Verification status.** Executed on 2026-08-15 against `d5018a6` plus the DEPLOY-6 `Dockerfile`
change, on Docker Engine 29.6.1 / API 1.55. Part 3's command blocks were **extracted from this file
and run verbatim** — not transcribed by hand — precisely so that a command documented here is a
command that has been executed as printed. The end state was observed: bus C's audit trail holds the
relayed message with `bus_path` = `[A, B, C]`. Where something is *not* verified, or is a known
defect, it says so in [Known gaps](#5-known-gaps) — nothing here is aspirational.

---

## 0. What you need

* Docker Engine (any version supporting multi-stage builds and `HEALTHCHECK`).
* `jq`, to read the `--json` output. Every command here is the compiled CLI (invariant 7); there is
  no `curl` anywhere in this document and there must not be — a hand-written HTTP call against this
  bus cannot verify its self-signed certificate without disabling verification, which is forbidden
  outright (invariant 11).
* Nothing else. **The image ships both binaries**: `agent-bus` (the server) and `agent-busctl` (THE
  client). You do not need a Go toolchain on the host.

```bash
git clone <this repo> && cd agent-bus
docker build -t agent-bus:local .
```

---

## 1. One bus, one command

```bash
docker run -t agent-bus:local
```

That starts a working bus. On first start it mints its own bus id, a self-signed TLS certificate, an
Ed25519 signing key and a WAL MAC key, then serves **https only** on `:8080` inside its container.

It is, however, a bus whose state dies with the container. For anything you intend to keep:

```bash
docker volume create bus-data
docker run -d --name bus --restart unless-stopped \
  -v bus-data:/data \
  -p 127.0.0.1:8080:8080 \
  agent-bus:local

docker inspect --format '{{.State.Health.Status}}' bus     # -> healthy
docker logs bus
```

### A data volume is not optional

`/data` holds the **bus id, the TLS certificate and its private key, the signing key, the WAL MAC
key, the append-only log, the invite table and the peer configuration**. Lose it and you have not
lost "some messages" — you have lost the bus's identity. There is no CA and no trust-on-first-use
(invariant 11): every agent pinned that certificate's fingerprint at enrolment, so a bus that comes
back with a new certificate is a bus none of its agents will talk to, and every one of them must be
re-invited. Back up `/data` as a unit; a backup missing any one of `bus-tls.key`, `bus-signing.key`
or `wal-mac.key` restores a bus that cannot do its job.

### About the port

The image's `CMD` binds `:8080` — all interfaces **inside the container's own network namespace**.
Publishing is an explicit, visible act on the `docker run` line:

| `docker run` flag | Who can reach the bus |
| --- | --- |
| *(none)* | other containers on the same docker network — **and any local user on this host**, see below |
| `-p 127.0.0.1:8080:8080` | the above, plus this host's loopback |
| `-p 8080:8080` | the above, plus **every interface of the host, i.e. the LAN** |

**"No `-p`" is NOT an isolation boundary on Linux, and it is easy to believe it is.** The host owns an
interface on the docker bridge and routes that subnet, so it can reach an unpublished container port
directly at the container's bridge address. Verified: with a bus started with no `-p` at all, a probe
from the host to `https://172.20.0.2:8080/healthz` completed its TLS handshake. What `-p` actually
controls is reachability from *off* this host; it does not hide the container from the host itself.
So on a shared machine, treat every local user as able to reach the bus regardless of `-p`.

The last one is a deliberate act. Docker programs its own iptables rules, so it bypasses a host
firewall that would otherwise stop it.

**What bounds the damage — and, more importantly, what does not.** It is not TLS: that is mandatory
and unconditional either way. It is not mTLS: the listener only *requests* a client certificate, so
one that is presented authenticates nobody by itself. It is the **enrolment gate** — enrolment is
invite-only, so an un-invited `POST /v1/enroll` is refused `403`.

**But that gate bounds who can BECOME an agent, and nothing else.** Read this before you type
`-p 8080:8080`, because a published port exposes the routes that necessarily cannot authenticate —
enrolment, session begin, session complete, `/healthz`, `/v1/info` and `/v1/discovery` (six routes;
`internal/httpapi/authmw.go`'s allow-list is the authority, and `/v1/discovery` carries `bus_id` and
`invite_required`) — and **there is no rate limiting on any of them** (tracked as
`AUTH-1-FU-RATELIMIT`). Two consequences are documented in the
code itself rather than hypothetical:

* **Session-table exhaustion.** An anonymous flooder can fill the session table to its maximum and
  deny session establishment to every legitimate agent until entries expire. `internal/auth/session.go`
  says so in its own comment, and says it must not be read as fixed.
* **An agent-enumeration oracle.** `handleSessionBegin` answers distinguishably for an unknown agent
  (`404`), a known one not yet certificate-bound (`200` with a live challenge) and a bound one
  (`403`). The bus id is public from `/v1/info`, so a caller who can reach the port can map which
  agents exist and which are not yet bound.

Neither is introduced by containerising the bus, but publishing the port is what makes them
reachable. **Treat `-p 8080:8080` as exposing the bus to anyone who can route to the host**, and
prefer `-p 127.0.0.1:8080:8080` plus an SSH tunnel, or no `-p` at all with agents on the same docker
network.

---

## 2. Initialisation — read this before Part 3

This is the least obvious part of the whole runbook, and a naive sequence will not work.

**Three facts that together force the shape of it:**

1. **Enrolment is invite-only.** No invite, no agents. Ever.
2. **The operator subcommands require the bus to be STOPPED.** `invite mint`, `peer add`,
   `key export-public` and `log` all take the data directory's exclusive dirlock, which a running bus
   is holding; against a running bus they refuse with exit `3` rather than degrading. (`healthcheck`
   is the exception — it takes no lock and is meant to run against a live bus.)
3. **A bus's first-ever boot can invite nobody.** An invite pins the bus's certificate fingerprint,
   and there is no certificate until a completed start has minted one.

**Therefore initialisation spans two container lifetimes:**

```
start once   →   stop   →   mint invites + peer add   →   start   →   agents enrol, indefinitely
(mints id,       (releases   (offline, one-shot            (serving)
 cert, keys)      the lock)   containers on the volume)
```

The offline steps run as **throwaway containers sharing the bus's volume**, which is why the volume,
not the container, is the bus:

```bash
docker run --rm -v bus-data:/data agent-bus:local invite mint -data-dir /data ...
```

### Minting a pool

**There is one invite per `mint` invocation — there is no `-count`.** Since minting needs the bus
down, mint a *pool* in the one offline window rather than taking an outage per agent. Invites are
single-use and expiring (default TTL 24h, max 168h), so size the pool to the window, not to
eternity:

```bash
# umask INSIDE the subshell, so the file is 0600 from the instant it is created.
# `... > invites.ndjson; chmod 0600` would leave ten bearer credentials
# world-readable for the length of the loop.
( umask 077
  for i in $(seq 1 10); do
    docker run --rm -v bus-data:/data agent-bus:local \
      invite mint -data-dir /data -bus-address https://127.0.0.1:8080 -label "agent-$i" -json 2>/dev/null
  done > invites.ndjson
)
```

### `-bus-address` is the address *the agent* will dial

It is baked into the invite blob and there is no default. Mint with the address that agent will
actually use:

* agent in a container on the same docker network → `https://bus-a:8080`
* agent on the host, port published → `https://127.0.0.1:18091`

One bus can have both kinds of invite outstanding; the same certificate serves both, because clients
verify by **fingerprint pin, not hostname**. But a single invite carries a single address, so mint
host-side invites separately.

### Invite files must be mode 0600 — or skip the file entirely

An invite blob contains `invite_secret`, a bearer credential. The CLI refuses to read one from a
file other local users can read, and a plain shell redirect (`> f.json`) creates `0664`:

```
{"ok":false,"error":"invite: the invite file /identity/invite.json is mode 0664, so other local
users can read it, and it holds a bearer credential","kind":"config",
"remedy":"chmod 0600 /identity/invite.json — ...","exit_code":3}
```

**The better answer is not to write the file at all.** `--invite-file -` reads the blob from stdin,
which keeps the secret out of `argv` *and* off disk:

```bash
printf '%s\n' "$invite_json" | agent-busctl --identity /identity --json enrol --invite-file - --name sender
```

Every enrolment in this runbook uses stdin. `printf` is a shell builtin, so no `invite_secret`
reaches any process's `argv` this way.

The runbook holds blobs in ordinary shell variables (`$inv_a`). That is fine — but **do not `export`
them**: an exported variable is inherited by every child process and readable from that process's
environment, and it will also surface under `set -x` and in your shell history. Keep them local to
the setup shell and let them die with it.

---

## 3. Three federated buses

### Topology

```
   A  <-->  B  <-->  C
```

**A and C are NOT peered.** A message from A to C transits B. That is the point of the exercise: it
exercises onward relay, not just a direct hop.

### 3.1 Network and volumes

```bash
docker network create busnet
for n in a b c; do docker volume create bus-$n-data; done
docker volume create id-sender
docker volume create id-recipient
```

A user-defined network gives you DNS: `bus-a`, `bus-b`, `bus-c` resolve container-to-container. Those
names are what go in `-bus-address` and `-url`. They do **not** need to appear in any certificate —
a bus started with this image's default `-listen=:8080` has a certificate naming only `localhost`,
`127.0.0.1` and `::1` — a wildcard bind contributes no SAN, though a specific one such as
`-listen 10.0.0.5:8080` would add `10.0.0.5`. Either way it is fine, because
both agents and peer buses verify by pinned fingerprint rather than by hostname.

Mount an agent's identity directory as a **named volume**, as above, not a host bind mount. The
image pre-creates `/identity` owned by the container's non-root user so a fresh named volume inherits
that ownership. A host bind mount has to satisfy both the daemon's filesystem visibility and the
container uid's access to the host file — on a snap-packaged Docker daemon, for example, `/tmp`
inside the daemon is *not* the host's `/tmp`, so `-v /tmp/x:/identity` mounts an **empty** directory
and the CLI reports "no invite file", for a file you can plainly see on the host.

### 3.2 First start — mint each bus's identity

```bash
for n in a b c; do
  docker run -d --name bus-$n --network busnet -v bus-$n-data:/data \
    --restart unless-stopped agent-bus:local
done

for n in a b c; do
  until [ "$(docker inspect --format '{{.State.Health.Status}}' bus-$n)" = healthy ]; do sleep 1; done
done
```

### 3.3 Stop — release the dirlock

```bash
docker stop bus-a bus-b bus-c
docker rm   bus-a bus-b bus-c      # the volumes, and therefore the buses, survive
```

### 3.4 Offline: read each bus's identity, and mint invites

You need four values per bus: its **bus id**, its **certificate fingerprint**, its **signing public
key**, and (for the two buses hosting agents) an **invite**.

```bash
offline() { n=$1; shift; docker run --rm -v bus-$n-data:/data agent-bus:local "$@"; }

inv_a=$(offline a invite mint -data-dir /data -bus-address https://bus-a:8080 -label sender    -json 2>/dev/null)
inv_c=$(offline c invite mint -data-dir /data -bus-address https://bus-c:8080 -label recipient -json 2>/dev/null)
inv_b=$(offline b invite mint -data-dir /data -bus-address https://bus-b:8080 -label metadata  -json 2>/dev/null)

bus_a=$(jq -r .bus_id <<<"$inv_a"); fp_a=$(jq -r .bus_cert_fingerprint <<<"$inv_a")
bus_b=$(jq -r .bus_id <<<"$inv_b"); fp_b=$(jq -r .bus_cert_fingerprint <<<"$inv_b")
bus_c=$(jq -r .bus_id <<<"$inv_c"); fp_c=$(jq -r .bus_cert_fingerprint <<<"$inv_c")

key_a=$(offline a key export-public --data-dir /data --json 2>/dev/null | jq -r .public_key)
key_b=$(offline b key export-public --data-dir /data --json 2>/dev/null | jq -r .public_key)
key_c=$(offline c key export-public --data-dir /data --json 2>/dev/null | jq -r .public_key)
```

Note **B gets an invite that no agent will ever redeem.** That is not a mistake and not a workaround
you can skip: `bus_cert_fingerprint` is a *public* value, but the only compiled command that emits it
today is `invite mint`. There is no read-only `cert show` — do not go looking for one, and do not
scrape `bus-tls.crt` out of the volume by hand. (The missing command is filed as
`RELAY-25-FU-CERTSHOW`.) The signing key, by contrast, *does* have a read-only command:
`key export-public`, used above.

B's invite then just sits there. **You cannot revoke it** — see [Known gaps](#5-known-gaps) — so mint it
with a short TTL (`-ttl 1h`) and let it expire on its own.

### 3.5 Offline: routes and trust

`agent-bus peer add` writes two independent kinds of record, and one invocation can write either or
both:

* **route** — `-url` (+ `-tls-fingerprint`): where we send traffic for a bus, and what certificate we
  expect back.
* **trust** — `-signing-key` (+ optionally `-peer-client-fingerprint`): whose messages we accept, and
  which client certificate that peer may present when it dials us.

#### The two fingerprints are opposite directions. Do not conflate them.

| Flag | Direction | Keyed to | Meaning |
| --- | --- | --- | --- |
| `-tls-fingerprint` | **OUTBOUND** | an **address** (`-url`) | the **server** certificate the bus at `-url` presents when **we dial it** |
| `-peer-client-fingerprint` | **INBOUND** | a **bus principal** (`-bus-id`) | the **client** certificate that bus presents when **it dials us** |

Here is the part that makes this genuinely confusing, so it is stated rather than left to be
discovered: **the value you pass to both flags is the same string.** A bus holds exactly one
certificate/key pair, minted with both `ServerAuth` and `ClientAuth` — "one identity, both
directions". So the same `bus_cert_fingerprint` is correct in each role. They remain *conceptually*
different pins, keyed to different things, and collapsing them in code or in your head is a refuted
design that appears to work and is not secure. `-peer-client-fingerprint` requires `-signing-key` in
the same invocation.

An **adjacent** bus — one that opens connections to us — gets a `-peer-client-fingerprint`. A
non-adjacent bus we merely trust the signatures of gets a signing key and no transport binding,
because binding one would assert an inbound connection this topology never makes.

```bash
# --- A: routes through B; reaches C VIA B; trusts B (adjacent) and C (signing only) ---
offline a peer add -data-dir /data -bus-id $bus_b -url https://bus-b:8080 -tls-fingerprint $fp_b -route-for $bus_c -json
offline a peer add -data-dir /data -bus-id $bus_b -signing-key $key_b -peer-client-fingerprint $fp_b -json
offline a peer add -data-dir /data -bus-id $bus_c -signing-key $key_c -json

# --- B: the transit bus. Both neighbours are adjacent. ---
offline b peer add -data-dir /data -bus-id $bus_a -url https://bus-a:8080 -tls-fingerprint $fp_a -json
offline b peer add -data-dir /data -bus-id $bus_a -signing-key $key_a -peer-client-fingerprint $fp_a -json
offline b peer add -data-dir /data -bus-id $bus_c -url https://bus-c:8080 -tls-fingerprint $fp_c -json
offline b peer add -data-dir /data -bus-id $bus_c -signing-key $key_c -peer-client-fingerprint $fp_c -json

# --- C: routes through B; trusts B (adjacent) and A (signing only, A never dials C) ---
offline c peer add -data-dir /data -bus-id $bus_b -url https://bus-b:8080 -tls-fingerprint $fp_b -json
offline c peer add -data-dir /data -bus-id $bus_b -signing-key $key_b -peer-client-fingerprint $fp_b -json
offline c peer add -data-dir /data -bus-id $bus_a -signing-key $key_a -json
```

#### `-route-for $bus_c` on A is load-bearing — the setup silently fails without it

It installs a second route record, keyed on **C**, pointing at **B's** URL: "to reach C, go via B".
Trusting C's signing key is not enough; trust says whose messages you accept, not where to send them.

Verified by removing exactly that one record and retrying the send:

```
{"ok":false,"error":"send: the bus refused the request: unknown recipient",
 "kind":"rejected","status":404,"exit_code":7}
```

Inspect what you wrote with `offline a peer list -data-dir /data -json | jq`.

### 3.6 Restart — downstream first

```bash
for n in c b a; do
  docker run -d --name bus-$n --network busnet -v bus-$n-data:/data \
    --restart unless-stopped agent-bus:local
  until [ "$(docker inspect --format '{{.State.Health.Status}}' bus-$n)" = healthy ]; do sleep 1; done
done
```

C, then B, then A. Starting the forwarder before its next hop is up is not fatal — the relay outbox
retries — but it produces avoidable error lines on a first run.

### 3.7 Enrol an agent on A and one on C

```bash
# $1 is the agent's identity VOLUME; everything after it is agent-busctl's own arguments.
# Note the ordering: docker's flags (-v) must precede the image name, and the image name must
# precede agent-busctl's flags. Getting that backwards passes `-v` to agent-busctl.
ctl() {
  vol=$1; shift
  docker run --rm -i --network busnet -v "$vol:/identity" \
    --entrypoint /usr/local/bin/agent-busctl agent-bus:local --identity /identity "$@"
}

sender=$(printf '%s\n' "$inv_a" | ctl id-sender \
  --json enrol --invite-file - --name sender | jq -r .agent_id)

recipient=$(printf '%s\n' "$inv_c" | ctl id-recipient \
  --json enrol --invite-file - --name recipient | jq -r .agent_id)
```

Agent ids come back **fully qualified**, `<bus-id>.<agent-id>` — e.g.
`bus-t4yr4qzepvv7zjd6.sender-1`. That qualification is what makes cross-bus routing work; never
shorten one.

### 3.8 Send A → C, and watch on C

```bash
ctl id-sender --as "$sender" --json send \
  --idempotency-key demo-a-to-c-v1 "$recipient" 'hello from bus A'

ctl id-recipient --as "$recipient" --json watch \
  --replay --no-cursor --for 20s --poll-timeout 1s
```

Observed on C:

```json
{"message_id":"bus-rupqkacueu6qce45-9","from":"bus-t4yr4qzepvv7zjd6.sender-1",
 "to":["bus-rupqkacueu6qce45.recipient-1"],
 "bus_path":["bus-t4yr4qzepvv7zjd6","bus-zbgy6z4jqores3hx","bus-rupqkacueu6qce45"],
 "text":"hello from bus A"}
```

`bus_path` is the proof: three entries, A then B then C. That is a message that transited a bus its
sender is not peered with.

### 3.9 Confirm from the durable audit trail

The audit log is metadata and routing only — it never records message bodies. **You must stop the
bus first**: `log` takes the same exclusive dirlock as `invite mint` and `peer add`, so against a
running bus it does not read a slightly-stale file, it refuses outright with exit `3`.

```bash
docker stop bus-a bus-b bus-c
offline c log --data-dir /data --json | jq -c 'select(.bus_path)'
offline b log --data-dir /data --json | jq -c 'select(.bus_path)'   # bus_path = [A, B]
offline a log --data-dir /data --json | jq -c 'select(.bus_path)'   # bus_path = [A]
```

Each bus records the path *as it saw it*, so the path grows by one hop per bus. That is the intended
shape.

---

## 4. Operating notes

**Restarts invalidate every session, and that is by design.** Sessions are in-memory, short-lived
bearer credentials; a restart drops them all and each agent must run the session handshake again.
Agents do **not** re-enrol — the roster is durable, and agent ids, public keys and enrolment instants
survive restarts and crashes. `agent-busctl` handles the re-handshake itself; you will see it in the
logs, not in your workflow.

**Adding an agent later costs an outage on that bus.** `invite mint` needs the dirlock. Mint a pool
while you are already down (§2) — but size it to the TTL, since an unused invite cannot be withdrawn
early and only expiry retires it.

**`docker compose down -v` destroys the certificate every agent has pinned.** So does
`docker volume rm`. There is no trust-on-first-use to re-learn it. Every agent must be re-invited.

**Do not run two containers against the same data volume.** The dirlock will stop the second one, but
do not rely on that as a design.

---

## 5. Known gaps

These are real, currently true, and stated here rather than discovered later.

* **`scripts/fed-smoke.sh` exits 1, and that is a defect in the test, not in the product.** It
  asserts one identical `message_id` across all three buses. Every bus is authoritative on its own
  ids (invariant 1) and mints its own, so the assertion cannot hold. Directly observed in the run
  above: the sender saw `bus-t4yr4qzepvv7zjd6-11` on A while the recipient saw
  `bus-rupqkacueu6qce45-9` on C — the same message, two ids. **Do not treat that script's exit status
  as a green check for your deployment.** Tracked as `RELAY-25-FU-CORRELATION`. There is no
  cross-bus correlation id today. `content_sha256` is the practical substitute — **but read the next
  bullet before using it.**
* **`content_sha256` is stable across buses, and differs between the two views that report it.** In
  one verified run the value was `9322…` in both the sender's `send` result and the recipient's
  `watch` record, and `fe31…` in the `agent-bus log` audit record on *all three* buses. So it
  correlates a message across A, B and C perfectly well **within one view**, and comparing a `send`
  result against an audit record will not match. Correlate audit-to-audit, or message-to-message —
  not across the two.
* **No read-only command emits `bus_cert_fingerprint`.** You must mint an invite to read a public
  value (§3.4). Tracked as `RELAY-25-FU-CERTSHOW`.
* **There is no way to revoke an invite.** `agent-bus invite` has exactly one subcommand, `mint`.
  Invariant 3 requires invites to be revocable and `internal/invite` supports it in the store, but
  the operator surface is unbuilt — the code itself refers to it as "(INVITE-REVOKE) once that
  surface exists". Until then a minted invite is redeemable until it expires, so **TTL is your only
  control**: mint short, mint what you will use. Plan a mint window rather than a standing pool.
* **The listener requests but does not require a client certificate.** A presented client certificate
  authenticates nobody by itself. The enrolment gate, not mTLS, is what stands between a reachable
  port and an unwanted agent.
* **Not covered here:** certificate rotation, buses across real hosts or NAT (this runbook is one
  Docker host and one bridge network), and any multi-bus Compose profile — `docker-compose.yml`
  remains the single-bus definition, and a peered profile is DEPLOY-3's job.
