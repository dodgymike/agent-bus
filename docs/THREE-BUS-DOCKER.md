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

### Rolling out a wire change — readers first

**A bus-to-bus frame gains a field only in two deploys, readers before senders. One deploy loses
messages permanently.** This is a procedure, not advice, and every log line quoted below was
observed, not expected.

*Verified 2026-08-21 against `591355f`, on the loopback three-bus harness `scripts/fed-smoke.sh` —
**not** on the Docker set this runbook otherwise describes.* Seven runs: the hazard cases failed and
the readers-first cases passed. That harness proves the bus-to-bus wire behaviour, which is what this
section is about, and its own header disclaims the rest — SSH tunnel bring-up, flap recovery,
NAT/keepalive, latency against `RetryHorizonCeiling`, and pinning across a real tunnel. The
`docker` commands below follow from the same ordering but were not themselves rehearsed.

**Why one deploy destroys traffic.** The peer surface decodes with `DisallowUnknownFields`
(`internal/relay/handshake.go`), so a bus running yesterday's binary answers **400 invalid_request**
to any frame carrying a field it does not know. The sender then classifies that 400 with
`PeerRefusedError.Retriable` (`internal/relay/client.go`), which is true for **408, 429 and 5xx
only** — every other 4xx is a verdict on the message's content, and resending identical bytes cannot
change it. So the message is **abandoned on the first attempt**, not retried:

```
# on the OLD receiver
msg="peer request rejected" surface=peer-relay status=400 code=invalid_request \
  err="relay: invalid peer handshake payload: json: unknown field \"protocol_version\""

# on the NEW sender, same millisecond
msg="relay forward failed"          ... attempt=1 retriable=false
msg="an outbox job was ABANDONED; this message will never reach the peer"
msg="relay forward permanently refused; NOT retried, because resending identical bytes cannot change this answer" attempts=1
```

`attempt=1` is the whole problem. The retry horizon is never entered, so the loss is not a delay you
can wait out — and it is **total for the affected link**, because the first-hop path and the
restart-resume path both emit through the same `Forward`.

None of that is a defect to be fixed by loosening the decoder. An unknown field on a federation
surface is a real signal, and the strict decode is the standing posture on this surface. Which means
a version field does **not** buy forward compatibility here — it makes the *next* change diagnosable,
and that is a different and smaller thing. **The cost of the strictness is paid in deploy discipline,
which is what this section is.**

Two consequences that are easy to get wrong:

* **A transit bus is a sender too.** Upgrading only the middle bus breaks the second hop. Observed on
  `A(old) → B(new) → C(old)`: A→B was fine, B→C was refused, and **B** logged the abandonment.
* **The origin sees nothing.** In that same run bus A — which minted the message and owes the
  delivery — logged **zero** forward failures, because its own hop succeeded. The three lines above
  appear only on the bus that actually attempted the refused hop. There is also **no CLI subcommand
  that surfaces an abandoned outbox job**: `agent-bus log` reads the audit file, and the
  `"state":"abandoned"` outbox record lives in the WAL. So during a rollout, **the bus logs are your
  only instrument** — collect them from every bus, not just the origin.

**The procedure.** Build two images from the change: an **accept-only** build, which knows the field
in its decoder but never sets it on an outgoing frame, and the **emitting** build. In Go these differ
by one line, because a `json:"…,omitempty"` field that nothing assigns is absent from the wire — the
accept-only build is the full struct change with the sender's assignment left out.

```bash
# roll IMAGE — replace one bus, and REFUSE to destroy it for an image that is
# not there. `docker rm -f` before a `docker run` that cannot succeed leaves you
# with no bus at all, and the health loop then spins forever, because
# `docker inspect` on an absent container prints nothing and nothing is never
# "healthy". Check the image first, and bound the wait.
roll() {
  local n="$1" image="$2" i=0
  docker image inspect "$image" >/dev/null || { echo "no such image: $image" >&2; return 1; }
  docker rm -f bus-$n
  docker run -d --name bus-$n --network busnet -v bus-$n-data:/data \
    --restart unless-stopped "$image"
  until [ "$(docker inspect --format '{{.State.Health.Status}}' bus-$n 2>/dev/null)" = healthy ]; do
    i=$((i + 1)); [ "$i" -lt 60 ] || { echo "bus-$n never became healthy" >&2; return 1; }
    sleep 1
  done
}

# gate IMAGE — assert, do not eyeball. Every bus must be on IMAGE *and* actually
# serving the peer surface. This is the step the whole procedure rests on, so it
# has to be able to go red; a list of image names printed for a human to compare
# is not a check.
gate() {
  local image="$1" n ok=0
  for n in a b c; do
    [ "$(docker inspect --format '{{.Config.Image}}' bus-$n)" = "$image" ] ||
      { echo "bus-$n is NOT on $image" >&2; ok=1; }
    # FIVE literals covering SEVEN call sites, because there are seven ways to be
    # deaf and they are worded differently -- including one at INFO, in lower
    # case. Each alternative is a literal you can `git grep` in the server source.
    # NEVER NARROW this alternation; matching only the first of them was a gate
    # that could not fire -- see the 2026-08-21 correction below. grep is
    # case-SENSITIVE here on purpose: the healthy line is "FEDERATION is served".
    docker logs bus-$n 2>&1 | grep -qE 'FEDERATION IS NOT SERVED|FEDERATION INGRESS IS NOT SERVED|FEDERATION IS DISABLED FOR THIS RUN|federation is not configured|REFUSING to mount a peer route' &&
      { echo "bus-$n is not serving the peer surface" >&2; ok=1; }
  done
  # NOT "and federating". This is an ABSENCE-of-announcement test, and claiming
  # more than it measured is the same overclaim the correction note below exists
  # to undo -- in executable form this time.
  [ "$ok" -eq 0 ] &&
    echo "GATE OK: all three buses on $image; none announced a refusal to serve the peer surface"
  return $ok
}

# Stage 1 — every bus learns to READ the field. No frame carries it yet, so
# an upgraded bus and a not-yet-upgraded bus interoperate in both directions
# and the buses may be rolled one at a time, in any order, with no window.
for n in c b a; do roll $n agent-bus:accept-only || break; done
gate agent-bus:accept-only || echo "STOP — do not start stage 2"

# Stage 2 — buses start EMITTING the field. Safe now, and again one at a time,
# because every peer has been able to read it since stage 1.
for n in c b a; do roll $n agent-bus:emitting || break; done
gate agent-bus:emitting
```

Downstream-first (`c b a`) within each stage for the reason §3.6 gives. Neither stage needs a
drain or a maintenance window; that is the point of splitting them.

**"Healthy" does not mean "federating", and this is the trap the gate above exists to catch.**
Peer-route registration is all-or-nothing at the surface level: when the peer surface is absent or
incomplete the server registers **no peer route at all** and *every* `/v1/peer/` path — relay
included — answers **404**. (Two per-route guard-rails in `mountPeerRoute` can also skip a *single*
route, rows 6–7 below, which is the one way a bus can end up serving a *partial* peer surface.)
The bus still starts, and its container healthcheck still reports `healthy`, because serving
federation is not what `/healthz` measures. Meanwhile 404 is non-retriable just like the 400 above,
so every peer sending to that bus **abandons** its traffic.

**There is no single signal, and that is the whole difficulty.** Seven distinct startup lines mean
"this bus is not serving the peer surface", they are worded differently, and one of them is at
`level=info` and in lower case. Five literals cover all seven, and the gate above matches them all:

| # | Emitted by | Reachable in the shipped `agent-bus` binary? | The line |
|---|---|---|---|
| 1 | `cmd/agent-bus/main.go` | **Yes** — the peer configuration store could not be built | `FEDERATION IS DISABLED FOR THIS RUN: the peer configuration store could not be built, …` |
| 2 | `cmd/agent-bus/main.go` | **Yes** — peering is configured, but no adjacent bus has an inbound client certificate bound to it | `FEDERATION INGRESS IS NOT SERVED although peering is configured: …` |
| 3 | `internal/httpapi/peermount.go` | **Not today** — `main.go` supplies `Peer` and `PeerPrincipals` as a **nil pair**, which takes `mountPeerSurface`'s *silent* exit | `FEDERATION IS NOT SERVED: a PeerSurface was supplied but is incomplete, …` |
| 4 | `internal/httpapi/peermount.go` | **Not today** — same nil-pair reason | `FEDERATION IS NOT SERVED: a complete PeerSurface was supplied but no inbound peer principal resolver was, …` |
| 5 | `cmd/agent-bus/main.go` | **Yes** — this bus has no peer records at all. `level=info`, **lower case** | `federation is not configured: this bus has no peer records and no peer trust records, …` |
| 6 | `internal/httpapi/peermount.go` | **Not today** — a compile-time guard-rail; a test asserts the route constants and the unauthenticated allow-list are disjoint | `REFUSING to mount a peer route: it is on the unauthenticated allow-list, …` |
| 7 | `internal/httpapi/peermount.go` | **Not today** — `missingParts` already refused a nil handler | `REFUSING to mount a peer route with no handler` |

Rows 1, 2 and 5 are the ones a deployment actually hits, and they fail for genuinely different
reasons: 1 is "no peer configuration could be loaded at all", 2 is "configuration loaded, but
nothing can authenticate inbound", 5 is "there was no peer configuration to load". An operator
needs all three, so the gate matches all three.

**Row 5 is the by-design case for an unpeered bus, and it is gated here anyway — deliberately.**
`level=info` and lower case make it look benign, and for a single standalone bus it is. But in
*this* runbook all three buses are peered, so a bus reporting it has lost its peer configuration:
a mis-mounted or recreated `bus-$n-data` volume produces exactly this line, and the bus is then as
deaf as in rows 1–4 while looking entirely healthy. If you copy this gate to a deployment where
some buses legitimately do not federate, drop the `federation is not configured` alternative — that
one by name, and only that one. **Never** drop row 6's `REFUSING to mount a peer route`: that line
means the federation ingress was about to be served to an anonymous caller.

Rows 3, 4, 6 and 7 are matched **deliberately even though nothing emits them today.** They are the
embedder-and-bug path: any binary importing `internal/httpapi` directly can reach rows 3 and 4, and
so can `cmd/agent-bus` the moment it supplies a non-nil but incomplete pair. (Rows 3 and 4 are
unreachable from this binary for **two** independent reasons, not one: the nil pair above, and
`cmd/agent-bus/relaywiring.go`, which fills `httpapi.PeerSurface` in a single literal with no
conditional field — so a non-nil surface from this binary is always complete. Either reason alone
would do it, which is why neither is a licence to stop matching them.) Rows 6 and 7 are
guard-rails that exist precisely to fail loudly if a future edit makes them reachable — row 6 would
mean the federation ingress had been put on the unauthenticated allow-list, which is the worst
outcome on this surface and must never be gated *out*. Matching all four costs two alternatives in
a regex and nothing at runtime, whereas dropping them would make the gate go quiet again on
precisely the class of change that broke it before.

So gate each stage on the **absence of all seven**, not on the healthcheck. A bus can be on the right
image, healthy, and silently deaf to the entire federation.

**Three families — five lines — are deliberately NOT matched, so that the next reader does not
re-derive the set and think they found a hole.**

* **Co-emitted consequences.** `FEDERATION CONFIGURATION IN THE LOG WAS NOT RESTORED` and its
  owed-delivery twin `CROSS-BUS DELIVERIES THIS BUS OWED A PEER WERE NOT RESTORED` are both guarded
  by `skippedPeerRecords != nil`, which is assigned only inside the same branch that emits row 1 on
  the same start. They are consequences, never independent signals: matching them would add
  alternatives that can only fire when row 1 already has.
* **Egress.** The two `level=error` lines about a peer that could not be seeded into the routing
  table mean this bus cannot *send* to that peer, not that it cannot *serve* the peer surface. This
  section is scoped to ingress, which is what a wire-version rollout breaks; egress faults need
  their own check.
* **Request-time, not startup.** `REFUSING a peer acknowledgement: …` (`peermount.go`) fires per
  request, not at boot, and means the surface *is* served but this caller carried no peer principal
  — served-but-unauthenticated, not not-served. It sits two grep hits from row 6, so it is named
  here precisely because a reader re-deriving the set will land on it.

**What this gate does NOT prove.** It is an absence-of-error test, and absence is weaker than a
positive readiness signal — which is the whole reason `RELAY-55` exists. Three limits worth knowing
before you trust it: `docker inspect --format '{{.Config.Image}}'` succeeds on an **exited**
container, so `gate` does not by itself establish that a bus is running — that is `roll`'s
`.State.Health.Status` loop, and `roll` failing only `break`s the stage loop; `docker logs` is
**cumulative across restarts** of one container, so a stale error line from an earlier boot can
redden a bus that is fine now (fail-safe, but it will confuse you); and nothing here proves a peer
route actually answers — only that the server did not announce that it had refused to mount one.

**And it depends on `docker logs` actually returning the log, which is a FAIL-OPEN dependency.**
Under a logging driver the CLI cannot read back — `gelf`, `fluentd`, `syslog`, `none` — `docker logs`
returns an error and no lines, so the gate matches nothing and reports success; under `json-file`,
rotation can evict a startup line from a long-lived container for the same effect. Both fail OPEN,
unlike the cumulative-logs case above, which fails safe. The `docker run` in this runbook sets no
`--log-driver`, so the daemon default governs — check it before trusting this gate, and treat an
empty `docker logs` as a RED, not a pass.

> **Correction, 2026-08-21 (RELAY-51).** From `14ed009` — which shipped this rollout section into a
> runbook created by `9938eb2` — until this change, the gate above read `grep -q 'FEDERATION IS NOT SERVED'` and this section claimed that
> line was **"the only signal"**. *Both claims were FALSE.* The shipped server binary never emits
> that literal at all: only rows 1 and 2 reach a real deployment, and **neither contains the
> substring**. Row 2 is not even a partial match, because the word `INGRESS` intervenes —
> `printf %s 'FEDERATION INGRESS IS NOT SERVED although peering is configured' | grep -q 'FEDERATION IS NOT SERVED'`
> finds nothing. The gate could therefore **never fire**: it reported
> `GATE OK: all three buses on $image and federating` for every bus, including one deaf to the
> entire federation, and it did so most confidently at exactly the moment it was needed. This is
> recorded rather than quietly swapped because a flipped claim reads as freshly checked whichever
> way it points, and that is how this class of defect survives review.

> **This log gate is INTERIM — `RELAY-55` replaces it.** Scraping `docker logs` for an error string
> is a guard whose correctness depends on prose in a source file that nothing mechanically links to
> this runbook, which is exactly how it broke. **`RELAY-55`** is filed (P1) to make a bus that cannot
> serve the peer surface distinguishable from a healthy one **without parsing logs**. Its mechanism
> is deliberately still open — a readiness endpoint separate from liveness, a field on
> `GET /v1/info`, or a refusal to start — so do not assume one here. When it lands, one of its
> acceptance criteria is that **this document names the chosen gate explicitly** — at which point
> RE-POINT this section at the new signal and keep the log alternation only as the fallback for
> buses not yet on that build. Until then the instruction in the gate stands: add a literal if you
> find one that is missing, never remove one.

**A new peer ROUTE is the same hazard with a different status code.** A bus that does not serve the
route answers **404**, which `Retriable` also treats as final. `POST /v1/peer/ack`
(`internal/relay/ackhttp.go`) is the live example: the route shipped with no caller precisely so that
every bus can serve it before anything emits to it. Roll the receiving half first, confirm it
everywhere, then enable emission — the same two stages.

**Rehearse it before you do it.** `scripts/fed-smoke.sh` runs the three-bus A→B→C proof on one build
by default, and takes a per-bus override so you can put a *different build* on each bus and rehearse
the mixed state a rollout actually passes through:

```bash
# The hazard, on purpose: a bus that emits, talking to one that cannot read.
# This run FAILS, and the artifacts it preserves under /tmp/fed-smoke-{a,b,c}
# contain the log lines quoted above.
FED_SMOKE_SERVE_A=/path/to/emitting-checkout/scripts/bus-serve.sh \
  bash scripts/fed-smoke.sh

# Stage 2 as it will really run: an emitting sender into accept-only peers.
FED_SMOKE_SERVE_A=/path/to/emitting-checkout/scripts/bus-serve.sh \
FED_SMOKE_SERVE_B=/path/to/accept-only-checkout/scripts/bus-serve.sh \
FED_SMOKE_SERVE_C=/path/to/accept-only-checkout/scripts/bus-serve.sh \
  bash scripts/fed-smoke.sh
```

Each variable names the `scripts/bus-serve.sh` of another checkout, and `bus-serve.sh` builds the
server from the tree it lives in — that is what puts a different binary on that bus. Unset, all three
default to this checkout and the run is exactly the single-build smoke test it has always been.
**Rehearse the failing direction too, and confirm it fails**: a rollout plan whose hazard was never
reproduced is a plan nobody has tested.

---

## 5. Known gaps

These are real, currently true, and stated here rather than discovered later.

* **There is still no cross-bus correlation id.** Every bus is authoritative on its own ids
  (invariant 1) and mints its own, so one logical message is `bus-t4yr4qzepvv7zjd6-11` on A and
  `bus-rupqkacueu6qce45-9` on C. `content_sha256` is the practical substitute — **but read the next
  bullet before using it.** (`scripts/fed-smoke.sh` used to assert one identical `message_id` across
  all three buses, which invariant 1 makes unsatisfiable, and so could never pass. That assertion was
  removed under `RELAY-25-FU-CORRELATION`; the script now correlates on the audit digest and **exits
  0**. This entry previously said it exits 1 — verified 2026-08-21 that it does not.)
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
