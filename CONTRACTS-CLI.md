# Contracts: server / CLI flags + env vars

Split out of `CONTRACTS.md` (2026-08-02) — see that file for the index of all contract planes and
the rest of the surface (HTTP, on-disk, agent-facing).

Two binaries live here:

- **`cmd/agent-bus`** — the SERVER.
- **`cmd/agent-busctl`** — the CLIENT, added 2026-08-02 by CLI-1/CLI-2. Per the amended invariant 7 it
  **replaces the `scripts/bus-*.sh` wrappers** as their subcommands land. Its flags, exit codes and
  JSON shapes are a contract with two consumers — a human and an agent shelling out — plus a third,
  an agent **embedding** the Go package it is a thin shell over.

---

## CLI flags (`cmd/agent-bus`)

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | TCP address to bind (loopback-only by default; use `:8080` for all interfaces) |
| `-data-dir` | `./data` | Directory for the durable store + append-only log; created `0700` if missing |
| `-poll-timeout` | `30s` | Ceiling on a single long-poll wait (not yet consumed by any handler) |
| `-log-level` | `info` | `debug`, `warn`, `info`, or `error` |
| `-auth-rate-limit` | `5` | Sustained per-SOURCE request rate (req/s) of the token bucket in front of the three unauthenticated credential routes `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` (`AUTH-1-FU-RATELIMIT`). A throttled source is answered **429 + `Retry-After`**, never disconnected (invariant 10). Must be `> 0` when `-auth-rate-burst > 0`. |
| `-auth-rate-burst` | `60` | Per-SOURCE burst capacity (bucket size) for those three routes. **`0` DISABLES rate limiting entirely** (historical unlimited behaviour). |
| `-bus-id` | *(empty → placeholder `bus-local`)* | **TEST-ONLY.** Validated against `^[A-Za-z0-9_-]{1,64}$`; `.` rejected (qualification separator, invariant 2). Using it logs a runtime `WARN`. See `DECISIONS.md`. |

**Per-source rate limit (`AUTH-1-FU-RATELIMIT`).** The three routes above cannot authenticate
(invariant 3 — they are how a credential is obtained) and every admission cap behind them is GLOBAL,
so without a per-source cap one anonymous caller can exhaust `MaxRosterEntries` (4096) with enrols or
`MaxSessions` (16384) with session/begins and deny the whole bus (security measured ~137 req/s from a
single source as enough). The limiter is a stdlib token bucket keyed on the TCP **peer address with
its port stripped** (`X-Forwarded-For` and other proxy headers are IGNORED — they are trivially
forged). **Honest limitation:** behind a shared NAT, a reverse proxy, an SSH tunnel or the Docker
bridge (every container appears as e.g. `172.17.0.1`) many distinct clients collapse to ONE key and
share ONE bucket, so they throttle each other; the burst default of 60 is sized to absorb ~20 agents
bootstrapping at once (enrol + begin + complete = 3 requests each) from one address before any is
throttled. It sits IN FRONT of the allow-list and does not change its membership (invariant 3). The
refusal is a `429` with `Retry-After` and is logged at Info (`request rate-limited: ...`); it is NEVER
a disconnect (invariant 10). Configuring `-auth-rate-burst > 0` with `-auth-rate-limit <= 0` is
refused at flag-parse time (a bucket that never refills would 429 forever once drained).

Exit codes: `2` on invalid flags/config (`parseFlags`/`validate` failure), `1` on a startup failure
(e.g. bind failure), `0` on a clean signal-driven shutdown.

**The server serves TLS and ONLY TLS (invariant 11, `MTLS-LISTENER`, landed 2026-08-07).** `-listen`
binds the one and only listener, and it is wrapped in `tls.NewListener` — built from the bus
certificate and key in `-data-dir` (`bus-tls.crt` + `bus-tls.key`, `internal/buscert`) — before
anything can `Serve` on it. There is no plaintext mode and **no flag that requests one**. Unusable key
material makes the process **refuse to start**, exit `1`, naming the offending path; it never degrades
to plaintext and never regenerates the material. TLS floor is **1.2** (matching `client/pin.go`'s
`pinnedTLSConfig`), ALPN is pinned to `http/1.1`, and **`ClientAuth: tls.RequestClientCert`** — see the
client-certificate policy below.
The `server started` log line now carries `scheme=https tls=true tls_min_version=1.2
client_auth=requested` alongside `addr` and `bus_cert_fingerprint=<64 hex>` — `tls=false` no longer
appears, and `client_auth` is DERIVED from the live `tls.Config` rather than written as a literal. A plaintext
request to the port never reaches a route: `crypto/tls` fails the handshake and `net/http` writes a
bare `HTTP/1.0 400 Bad Request` + `Client sent an HTTP request to an HTTPS server.` onto the socket
and closes it, before any handler or auth middleware runs — see `CONTRACTS-HTTP.md`'s `## Routes` for
what those handlers answer once TLS has completed.

**The listener REQUESTS a client certificate and never REQUIRES one** (`MTLS-CLIENTAUTH`, landed
2026-08-14). `ClientAuth: tls.RequestClientCert`, paired with a `VerifyPeerCertificate` callback
(`admitClientCertificate`, `cmd/agent-bus/tlslisten.go`). The contract, in the terms a caller needs:

| Client presents | Handshake | `r.TLS.PeerCertificates` | Meaning |
| --- | --- | --- | --- |
| nothing | **succeeds** | empty | No transport identity. The ordinary path for every agent today, for `agent-bus healthcheck`, and for any operator probe of `/healthz`. Nothing is refused for this reason. |
| a certificate | **succeeds** | 1+ entries, leaf first | Its holder proved possession of the leaf's private key (`CertificateVerify`, which `crypto/tls` checks in every mode — `RequestClientCert` gets exactly the same proof of possession as `RequireAndVerifyClientCert`). It is **not** authenticated as anybody: no principal has been resolved. |
| an unparseable leaf | **fails** | — | `bad certificate` alert, fails closed. Note this is `crypto/tls` refusing it *before* the callback runs; `admitClientCertificate` re-checks only so that it has no path returning `nil` without having judged something. |

**`requested` is not a step on the way to `required`; it is the policy.** The two neighbouring values
are both wrong and in opposite directions, and this is recorded so neither is "finished" later by
mistake:

- `tls.RequireAnyClientCert` locks out, at the handshake and before any route or log line they could
  act on, every agent whose identity directory predates `MTLS-CLIENTCERT`, plus `agent-bus healthcheck`
  (which is what Docker's `HEALTHCHECK` branches on) and every operator `/healthz` probe.
- `tls.VerifyClientCertIfGiven` (and `RequireAndVerifyClientCert`) sit at or above `crypto/tls`'s
  verification threshold, so the stdlib chain-verifies against `ClientCAs`. **There is no CA in this
  design and `ClientCAs` is nil, which means the system roots** — a self-signed agent or peer-bus
  certificate chains to nothing there. It would admit every client *without* a certificate and reject
  every client *with* one: exactly backwards.

**Verification is by fingerprint, never by chain.** `admitClientCertificate` deliberately authorises
nothing: it guarantees only that a certificate which reached the application is a single parseable
leaf with a derivable fingerprint. Resolving that fingerprint to a principal is application-layer work
(`MTLS-BIND`, `MTLS-CROSSCHECK`, and `RELAY-20` for peer buses). **The fingerprint of a presented
certificate has exactly ONE spelling — `buscert.FingerprintOf(r.TLS.PeerCertificates[0])`: `sha256`
over the leaf's DER exactly as it arrived (`x509.Certificate.Raw`, never a re-marshalling), rendered as
64 LOWERCASE hex characters, no prefix, no colons, no whitespace.** It is the same construction the
invite blob carries and `client/pin.go` pins the bus with. Call the helper; a second implementation
(SPKI instead of `Raw`, base64 instead of hex, uppercase) produces a well-formed value that never
matches, and nothing reports the mismatch.

**Two rules bind every future consumer of `r.TLS.PeerCertificates`**, both from the security gate:
the **fingerprint is the only identity** — never `Subject`/`CN`/`SAN`/`Issuer`/`SerialNumber`, which are
chosen by whoever minted the certificate, i.e. by whoever presented it; and **check the slice is
non-empty, then index `[0]` only, never iterate it**. Empty is the *majority* case (every ordinary agent
connects without a certificate), so an unguarded `PeerCertificates[0]` panics on almost every
connection; and the peer controls the whole chain while `CertificateVerify` proves possession of the
*leaf* key alone, so a consumer that searched the slice for a known fingerprint would be spoofed by
anyone appending the victim's public certificate at index 1.

Three further consequences. An `IsCA`/`ExtKeyUsage` filter must **not** be added — the bus's own
certificate is `IsCA` with both `ServerAuth` and `ClientAuth`, because a peer bus presents that same
certificate when it dials, so such a filter would refuse exactly the relay connection this enables.
**Client-certificate expiry is enforced nowhere on this side**: `RequestClientCert` does no chain
verification, so `NotAfter` is unchecked and an expired agent certificate is admitted. That gap is
filed and owned separately (Spec Server `ca356fde-0613-42cb-ac85-a629609d9c78`), not closed here; it is
harmless only for as long as nothing authorises anything on a client certificate, so it must close in
the same task as `MTLS-BIND`/`MTLS-CROSSCHECK`. And there are two **non-zero costs**: on **TLS 1.2 only**,
the client Certificate message is unencrypted, so a passive observer gains a stable per-agent
correlatable identifier (the leaf's public key) that did not exist under `NoClientCert` — TLS 1.3
encrypts it and no `MaxVersion` is set, so the residue is TLS-1.2-only peers, and the CN is a fixed
descriptive string so no agent *name* leaks; and an unauthenticated peer can now push a certificate
chain (bounded by `crypto/tls`'s 64 KiB handshake-message cap) that is retained for the connection's
lifetime, bounded in practice by the loopback default listen address.

Nothing on disk changed, no wire format moved, and **no existing client is affected**: a client that
presents no certificate behaves exactly as before. Operators must rebuild the binary and restart the
bus for the listener to start asking. This is **code-only** — no deploy has been performed, and no
running bus has been observed doing any of the above.

**Every existing deployment now dials a different scheme.** `http://<bus>` gets that bare 400, never a
route. An operator must rebuild the binary, restart the bus, and every client must dial `https://` with
the bus's certificate fingerprint (there is no trust-on-first-use). Nothing on disk changed format and
no existing WAL, enrolment or invite is invalidated — the certificate has been minted into the data dir
since `MTLS-BUSCERT`; this change only starts serving with it.

The binary now takes exactly ONE of TWO subcommands, dispatched before flag parsing on the first
non-flag argument. Everything else reaches the server flag set unchanged.

---

### `agent-bus invite mint` — the operator's invite-minting surface

Added 2026-08-07 by `INVITE-MINT`. Source: `cmd/agent-bus/invite.go`.

```
agent-bus invite mint -data-dir <dir> -bus-address <url> [-ttl <dur>] [-label <text>] [-json]
```

**It is a subcommand on the SERVER binary, not an HTTP route.** Minting authority is **filesystem
access** to the data directory — the same model as `wal-mac.key` and the bus's private keys
(`DECISIONS.md` E4, 2026-08-02). Nothing new is exposed on the wire, so bootstrapping invite-only
enrolment adds no network-reachable privilege.

**THE BUS MUST BE STOPPED.** Minting appends an invite record through the two-phase WAL path and takes
the data directory's exclusive `dirlock`, which a running bus holds. This is structural, not a
limitation to be worked around: two processes appending to one log destroys it.

**SCOPE: mint only.** `POST /v1/enroll` still accepts callers with no invite — that is `INVITE-GATE`.
A record written here is durable and is replayed by the bus (both existing WAL appliers ignore an
unrecognised `Entry.Kind`), but nothing consumes it yet.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data-dir` | `./data` | The bus's data directory. **Never created, and neither is any part of the bus identity in it** — it must already hold BOTH the `bus-id` file and `bus-tls.crt` (exit `4` otherwise). A typo that minted a second bus identity would yield an invite pinning a certificate no running bus serves; a regenerated `bus-id` would rename the bus away from its own certificate. |
| `-bus-address` | *(none — REQUIRED)* | The base URL an agent dials, e.g. `https://bus.example:8443`. **No default**, deliberately: a guessed address produces an invite pointing somewhere the operator did not mean. Validated and canonicalised by the same rule as `agent-busctl --bus`: scheme `http` or `https`; **`http` ONLY to a loopback host** (`localhost`, or an IP literal `net.IP.IsLoopback` accepts); userinfo, query and fragment rejected. Canonicalised identically to `client.parseBusURL` — scheme and host lower-cased, a default port (`:80`/`:443`) dropped, one trailing `/` trimmed — because the client uses this string as an idempotency **scope key**, so two spellings of one bus would be two scopes. **One deliberate divergence:** an IPv6 literal is re-bracketed when a default port is dropped (`https://[::1]:443` → `https://[::1]`, not `https://::1`), because the unbracketed form is not a parseable URL host and would put an undiallable address in the trust anchor. `client.canonicalHost` still has that bug and fails closed; filed as a follow-up against `client/`. |
| `-ttl` | `24h` (`invite.DefaultTTL`) | How long the invite stays redeemable. Maximum `168h` (`invite.MaxTTL`); an over-long value is **REFUSED, not clamped** (exit `1`). |
| `-label` | *(empty)* | An operator note recorded on the invite, ≤ 128 bytes (`invite.MaxLabelLen`). **Never shown to whoever redeems the invite** — it is echoed only in this command's own output. |
| `-json` | off | Emit the invite blob as one JSON object on **stdout**. |
| `-log-level` | `warn` | Severity floor for recovery/durability lines on stderr (`debug`, `info`, `warn`, `error`). |

**There is no `-invite-id` and no `-invite-secret` flag, and none may be added.** Invariant 1: the
server mints both from `crypto/rand`. `invite.MintRequest` has no field for either, which makes this
structural rather than a rule this command has to remember;
`TestInviteMintRejectsClientSuppliedSecret` pins both halves.

#### Exit codes (`agent-bus invite`)

| Code | Meaning | Remedy |
| --- | --- | --- |
| `0` | The invite was minted and is durable. | — |
| `1` | The mint failed and **no invite was returned** (WAL, I/O, capacity, an over-long `-ttl`, a failed log close). | Read the message; retry. |
| `2` | Usage: bad flag, unknown subcommand, positional argument, or a bad `-bus-address`. | `agent-bus invite mint -h` |
| `3` | The data directory is **locked** — a bus is running. | Stop the bus, mint, start it again. |
| `4` | The data directory does not hold a usable bus identity: missing, not a directory, no `bus-id` file, no `bus-tls.crt`, or a certificate whose CommonName names a **different bus** than the `bus-id` file. | Start the bus once if it has **never run**; **restore the missing file from backup** if it has. |

`1` almost always means nothing was written, but the contract does not claim that: if the record was
written and then the log **Close** failed, a durable OPEN invite remains whose secret was discarded.
It is unusable, but it holds a slot until it expires, and the error names its id so it can be revoked.

**Every exit-`4` refusal writes nothing whatsoever**, and both halves of that are load-bearing:

- It is checked **before the `dirlock` is taken**, because `dirlock.Acquire` creates `bus.lock`, and
  the server decides whether a data directory "has history" by whether it was EMPTY at startup. A lone
  `bus.lock` left by a premature mint made the operator's *first* `agent-bus` start refuse to boot and
  demand `-backfill-suffix-floors`.
- It checks the **`bus-id` file as well as the certificate**, because `ids.LoadOrCreateBusID` *creates*
  a bus id when the file is absent. Checking only the certificate meant a directory whose `bus-id` had
  been lost got a freshly minted one persisted **before** the CommonName cross-check refused — leaving
  the bus permanently renamed away from its own certificate, with every agent id it had ever issued
  (`<bus-id>.<agent-id>`, invariant 2) naming a bus that no longer exists.

Both are pinned by tests:
`TestInviteMintSubcommand/a_refusal_on_a_virgin_data_directory_leaves_it_COMPLETELY_untouched` and
`TestInviteMintSubcommand/a_lost_bus-id_file_is_REFUSED,_never_regenerated`.

`-h` / `--help` print to **stdout** and exit `0`, at both `agent-bus invite -h` and
`agent-bus invite mint -h`; only errors go to stderr.

`3` and `4` are distinct from `1` because their remedies are opposite — one says *stop* the bus, the
other says *start* it — and a caller that cannot tell them apart has to parse English.

#### The invite blob (`--json` success shape)

This is the **TRUST ANCHOR** of `DECISIONS.md` E6. The four load-bearing fields are `bus_id`,
`bus_address`, `bus_cert_fingerprint` and `invite_secret`: together they let an agent find the bus and
verify it **before its first connection**, with no trust-on-first-use window. Whoever can substitute
this blob can point an agent at a bus of their choosing, so the channel it travels over is
load-bearing.

```json
{
  "ok": true,
  "invite_id": "inv-abcdefghijklmnop",
  "bus_id": "bus-…",
  "bus_address": "https://bus.example:8443",
  "bus_cert_fingerprint": "<64 lowercase hex>",
  "invite_secret": "<43-char base64url>",
  "created_at": "2026-08-07T12:00:00Z",
  "expires_at": "2026-08-08T12:00:00Z",
  "label": "for the deploy runner",
  "transport_insecure": true
}
```

- `bus_cert_fingerprint` is `sha256.Sum256(cert.Raw)` of the bus's leaf certificate, rendered as
  **exactly 64 LOWERCASE hex characters** — byte-identical to what `--bus-fingerprint` /
  `client.ParseBusFingerprint` accepts, and to the bus's `bus_cert_fingerprint=…` startup log line.
  Uppercase is not produced and would not be accepted.
- `invite_secret` is 32 bytes of `crypto/rand` in `base64.RawURLEncoding`. It is **printed exactly
  once and is not recoverable**: only its SHA-256 digest is durable, and the plaintext is never in the
  WAL, a log line or an error. A lost secret means revoke and re-mint.
- `label` is omitted when empty. In the **human** output it is rendered with `%q`, so control/ANSI
  bytes in an operator-supplied label cannot reach the terminal raw; `--json` needs no equivalent
  because `encoding/json` escapes everything below `0x20`, and it round-trips the label verbatim.
- `transport_insecure` is **omitted unless true**, so it is only ever a positive assertion of risk. It
  is `true` when `bus_address` is plaintext `http`, meaning the invite secret will cross the wire in
  cleartext at redemption and `bus_cert_fingerprint` pins **nothing** over that connection (invariant
  11) — this is now a choice the operator made in `-bus-address` rather than a gap forced by an
  unlanded listener (`MTLS-LISTENER` landed 2026-08-07; see above). It exists because the matching
  stderr `WARN` is invisible to an agent that discarded stderr — invariant 7's second audience needs a
  field it can branch on.

**This is the mint command's OUTPUT.** There is deliberately no single packed token — no base64 blob,
no bespoke encoding — just this JSON object, saved to a file. **Settled by `INVITE-CLIENT`
(2026-08-14):** `client.Invite` (`client/invite.go`) is the consumer, and its json tags mirror this
struct EXACTLY, field for field — `agent-busctl enrol --invite-file <path>` reads this JSON verbatim.
See "Invite redemption" below.

Failure shape, also on **stdout** so an agent that redirected stderr still gets a parseable answer:

```json
{ "ok": false, "error": "…", "remedy": "…", "exit_code": 3 }
```

`ok` is present and first in both shapes, so a caller branches on one field.

**Residual risk, now that `MTLS-LISTENER` has landed (2026-08-07):** `-bus-address` still accepts
`http://` for a loopback host, unchanged, for local testing. That is now the operator's choice, not a
gap awaiting the listener — the bus CAN serve `https`, so an `-bus-address` pointed at a real
deployment should be `https://`. An `http://` address still logs a `WARN`, the invite secret still
crosses the wire **in cleartext** at redemption over that address, and the fingerprint in the blob
still pins nothing over a plaintext connection (there is no certificate to pin). Exposure is bounded
only by the `-listen 127.0.0.1:8080` loopback default; do not point `-bus-address` at a non-loopback
`http://` deployment.

---

### `agent-bus healthcheck` — TLS-aware liveness probe

Added 2026-08-07 by `MTLS-VERIFY`. Source: `cmd/agent-bus/healthcheck.go`.

```
agent-bus healthcheck [-data-dir <dir>] [-addr <host:port>] [-timeout <dur>]
```

**Why it exists at all.** `MTLS-LISTENER` makes the bus serve `https` and nothing else, which breaks
any plaintext liveness probe pointed at it. The runtime image is Alpine with busybox `wget` and no
`curl`, and busybox `wget` cannot be told to trust ONE self-signed certificate — its only relevant
knob is `--no-check-certificate`, which verifies nothing, and invariant 11 forbids disabling
verification to make something work. So the probe is a subcommand on the binary that already ships in
the image, rather than a second artefact or a weakened check.

**It is a subcommand on the SERVER binary, like `invite mint`, and for the same reason** (`DECISIONS.md`
E4): its input is filesystem access to the data directory, not a network privilege. It differs from
`invite mint` in one respect that matters — it takes **no lock and writes nothing**, so unlike minting
it is safe, and expected, to run against a **running** bus holding the dirlock. It is deliberately
**not** part of `agent-busctl`: an agent never runs it, since it needs no session, no enrolment and no
identity, and it reads the bus's own data directory, which no agent has.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data-dir` | `./data` | Reads **only** `<data-dir>/bus-tls.crt`; never created, never locked. |
| `-addr` | `127.0.0.1:8080` | Must match the bus's `-listen`. A wildcard/empty host (`:8080`, `0.0.0.0:8080`, `[::]:8080`) is rewritten to `127.0.0.1`, because the certificate deliberately does not name a wildcard bind (`internal/buscert`). |
| `-timeout` | `2s` | Bounds connect + TLS handshake + request + response. |

Probes `GET https://<addr>/healthz`, trusting **that one certificate as the sole x509 root** — a full
verification, not a bare pin, so it also enforces the **hostname** (against the certificate's SANs) and
the **validity period**: an expired bus certificate reports unhealthy. There is no
`InsecureSkipVerify` and none may be added — it is permitted in exactly one file in this repo
(`client/pin.go`, paired with `VerifyPeerCertificate`), and this is not it.

#### Exit codes (`agent-bus healthcheck`)

| Code | Meaning |
| --- | --- |
| `0` | Healthy: `GET /healthz` answered `200` over a verified TLS connection. Prints `ok <url>` on stdout. |
| `1` | Unhealthy: unreachable, an unusable/untrusted certificate, a TLS failure, a timeout, or a non-`200` status — **one code**, deliberately, so a `HEALTHCHECK` line cannot be written to treat some failure modes as healthy. |
| `2` | Bad usage (malformed flags). Distinct from `1` so a typo in a `HEALTHCHECK` line is not indistinguishable from a dead bus. Docker documents exit `2` as **reserved**; it is only reachable from a malformed probe invocation here, never from an unhealthy bus. |

**Container healthchecks now invoke this subcommand, not `wget`.** Both `Dockerfile`'s `HEALTHCHECK`
and `docker-compose.yml`'s `healthcheck.test` changed from
`wget -q ... http://127.0.0.1:8080/healthz` to
`["/usr/local/bin/agent-bus","healthcheck","-data-dir=/data","-addr=127.0.0.1:8080","-timeout=2s"]`,
for the same reason stated above: busybox `wget` cannot trust one self-signed certificate without
disabling verification outright.

---

### Container image (`Dockerfile`) — CONTRACT (`DEPLOY-6`, 2026-08-15)

The image is a deployment surface with its own defaults, and **one of them deliberately differs from
the binary's**. Operator runbook: `docs/THREE-BUS-DOCKER.md`.

| Image property | Value | Notes |
| --- | --- | --- |
| `ENTRYPOINT` | `/usr/local/bin/agent-bus` | the server. Append flags on the `docker run` line to override `CMD`. |
| `CMD` | `["-listen=:8080","-data-dir=/data","-log-level=info"]` | **`-listen` is `:8080`, NOT the binary's `127.0.0.1:8080`.** See below. |
| binaries shipped | `/usr/local/bin/agent-bus` **and** `/usr/local/bin/agent-busctl` | invariant 7 — an image with no client ships a bus no agent can enrol with. Reach the client with `--entrypoint /usr/local/bin/agent-busctl`. |
| `/data` | `agentbus:agentbus`, `0700`, declared `VOLUME` | bus id, TLS cert + keys, signing key, WAL + MAC key, invite table, peer config. |
| `/identity` | `agentbus:agentbus`, `0700`, **not** a declared `VOLUME` | mount point for an *agent's* identity dir (`agent-busctl --identity`). Pre-created so a fresh named volume inherits non-root ownership; not declared `VOLUME` so a plain server run does not create a stray anonymous volume. |
| user | `USER agentbus:agentbus` — the account is created with a fixed `uid`/`gid` of `10001` | the NAMES are what the `USER` line and the image config record; the numbers are pinned at `adduser`/`addgroup` time so a volume's owner is stable and predictable across rebuilds. |
| `EXPOSE` | `8080` | documentation only; publishing is `docker run -p` / compose `ports:`. |

**`-listen=:8080` in the image is not a narrowing of invariant 11's loopback default.** The default in
`cmd/agent-bus/main.go` is unchanged and stays `127.0.0.1:8080`; a bare `agent-bus` on a host still
binds loopback. What differs is one image's `CMD`, because a container has a stronger isolation
primitive than the interface a process binds: the **network namespace**. `:8080` inside an
unpublished namespace is not reachable from off the host. It IS reachable from the host itself, at
the container's bridge address — `-p` governs off-host reach, not host-to-container reach — so on a
shared machine treat local users as able to reach the bus regardless of `-p`. The previous value made
`docker run -p 8080:8080` publish a port that forwarded into the namespace and found nothing
listening — the bus started, reported itself healthy to its own in-namespace probe, and was
reachable by no one. Full reasoning: `DECISIONS.md` 2026-08-15 (DEPLOY-6), and the comment block
above the `CMD` line itself.

**`docker-compose.yml` is unaffected**: its `command:` sets `-listen=127.0.0.1:8080` explicitly, so
that service stays deliberately unreachable from outside its container, as its own header documents.

---

### `agent-bus peer add|list|remove` — the operator's federation configuration

Added 2026-08-08 by `RELAY-12`. Source: `cmd/agent-bus/peer.go`. Durable records:
`internal/relay/peerstore.go` (see `CONTRACTS-ONDISK.md` for the `peer` / `bustrust` record shapes).

```
agent-bus peer add    -data-dir <dir> -bus-id <busID> [-url <https origin>]
                      [-tls-fingerprint <64 lowercase hex>]
                      [-peer-client-fingerprint <64 lowercase hex>]
                      [-signing-key <base64> ...] [-route-for <busID> ...] [-json]
agent-bus peer list   [-data-dir <dir>] [-json]
agent-bus peer remove -data-dir <dir> -bus-id <busID> (-route | -trust | -route -trust) [-json]
```

**It is a subcommand on the SERVER binary, not an HTTP route, and it adds NO new privilege tier.**
`DECISIONS.md`, 2026-08-08 FEDERATION **(e)**: peer configuration is offline under the dirlock,
following the `invite mint` / E4 precedent. What that costs is recorded in the same ruling — **online
re-peering is given up; a topology change needs a restart.**

**THE BUS MUST BE STOPPED — for `list` too.** `add` and `remove` append through the two-phase WAL path
and take the data directory's exclusive `dirlock`, which a running bus holds. `list` writes nothing
(it uses `wal.Replay`, the package's read-only fsck: no repair, no truncation, no file created) but
takes the same lock anyway, because a read racing an append can see a half-written tail record and
would then either report a peer that is not yet durable or fail with a corruption error against a
perfectly healthy bus.

#### A ROUTE and a TRUST PIN are independent records, and the flags keep them that way

This is the load-bearing property of the surface, and it comes from the topology the FEDERATION epic
exists for — `laptop(A) ↔ internet(B) ↔ this machine(C)`, where **C never peers with A** but must
still pin A's bus signing key, because a message *originating* at A is verified by C against that pin
and B is not allowed to vouch for it (`internal/relay/signed.go`: "presentation is not attestation").

| Invocation | Writes | Does **not** write |
| --- | --- | --- |
| `peer add -bus-id busA -signing-key <b64>` | one `bustrust` record — **trust with NO route** | any route |
| `peer add -bus-id busB -url https://b:8443` | one `peer` record — **a route with NO trust** | any pin |
| both flags together | **two** records, trust first | — |

`-url` never implies a pin and `-signing-key` never implies a route. A surface that required them
together would foreclose the case the epic turns on, and the mistake would surface only at `RELAY-17`.
`TestPeerAddListRemove/a_TRUST_entry_survives_with_NO_route` and `/a_ROUTE_survives_with_NO_trust`
assert both **on disk**.

**Write order is a safety property, not a style choice.** `add` writes **trust first**, then the
peer's own route, then the `-route-for` routes; `remove` withdraws the **route first**, then trust.
Each record is durable on its own, so a failure part-way leaves the earlier ones on disk — and of the
two possible half-states, "pinned but not routed" is inert while "routed but unverifiable" is not.
`TestPeerAddListRemove/both_together_write_TWO_independent_records` pins the ordering by `config_seq`.

#### `-route-for` — STATIC next-hop routing, and what it looks like on disk

`DECISIONS.md` FEDERATION **(f)**: static routes, not a routing protocol. **Given up: topology
discovery — a fourth bus needs its own `-route-for` entry on every bus that must reach it.**

The encoding is worth stating because nothing in the record says it: **a route record's bus id is the
DESTINATION, and its base URL is the address to DIAL to reach that destination.** For a directly
peered bus those are the same machine; for a non-adjacent one they are not.

```
agent-bus peer add -bus-id busB -url https://b.example:8443 -route-for busC
```

writes **two** `peer` records: `busB → https://b.example:8443` and `busC → https://b.example:8443`,
i.e. "traffic for busC leaves via the peer at that address". **The durable record does not remember
that the next hop is busB — only the address.** `peer add --json` reports `next_hop_bus_id` because it
knows it from the same command line; `peer list` does not, and says so in its output.

| Flag (`add`) | Default | Meaning |
| --- | --- | --- |
| `-data-dir` | `./data` | **Never created, and neither is the `bus-id` file in it** (exit `4` otherwise). Unlike `invite mint` the **certificate is NOT required**: peer configuration pins no certificate of ours. |
| `-bus-id` | *(REQUIRED)* | The peer bus this entry is about. Validated with `ids.ValidateBusID` behind a length guard (`relay.MaxPeerBusIDLen` = 64) so an oversized value cannot size the diagnostic. **May not be this bus's own id** (invariant 2) — refused before anything is written. |
| `-url` | *(none)* | A **BARE https origin**: scheme, host, optional port, and nothing else. No path, query (including a bare `?`), fragment, userinfo or opaque form; `https` only (invariant 11); ≤ `relay.MaxPeerBaseURLLen` (512). One trailing `/` is trimmed; **nothing else is rewritten**, so the address an operator typed is the address the bus will dial. |
| `-tls-fingerprint` | *(none)* | Pins the TLS certificate of the bus **at `-url`** — the **NEXT HOP** — as exactly **64 LOWERCASE hex** characters: `sha256` over the leaf certificate's DER, the value `buscert.FingerprintOf` returns (no `sha256:` prefix, no colons, no internal spaces; surrounding whitespace is trimmed as for `-signing-key`; uppercase is **refused**, not normalised, so one fingerprint has exactly one spelling). **Requires `-url`** — a pin belongs to an address, and a bus you only `-signing-key` is never dialled. **Written onto every route this invocation gives that address to, `-route-for` records included** (see the next-hop paragraph below). All-zero is refused, and so is an **empty value** (`-tls-fingerprint "$FP"` with `FP` unset is a *lost* value, not an absent flag — presence is detected with `fs.Visit`): both would otherwise store an unpinned hop while reporting success. Not repeatable — one address, one certificate. **It is a transport pin, not a signing key and not an idempotency fingerprint.** |
| `-signing-key` | *(none)* | A pinned Ed25519 **bus signing** key, standard base64 (44 chars). **Repeatable, at most 2** (`relay.MaxPinnedBusSigningKeys`) — two means a **rollover window**, the outgoing key and the incoming one, not a general-purpose accept list. Repeating the flag **REPLACES** the pin set; it never adds to it. Order is preserved and is part of the record. |
| `-peer-client-fingerprint` | *(none)* | Binds the certificate the bus **at `-bus-id`** PRESENTS AS A TLS CLIENT WHEN IT DIALS THIS BUS — the **INBOUND** transport credential — as exactly **64 LOWERCASE hex** characters: `sha256` over the leaf certificate's DER, the value `buscert.FingerprintOf` returns (no `sha256:` prefix, no colons, no internal spaces; surrounding whitespace trimmed; uppercase **refused**, not normalised). Parsed by `relay.ParsePeerClientTLSFingerprint` — the same validator the durable decode path uses. Written to `relay.BusTrustRecord.PeerClientTLSCertFingerprint` on the **trust** record, JSON key `peer_client_tls_cert_sha256`, and it is what lets this bus resolve an incoming peer connection to a bus principal (`PeerStore.InboundPeerPrincipal`). **Requires `-signing-key`** — an active trust record always carries at least one pinned signing key, and no trust record is written without one, so the flag alone would report success and bind nothing; the coupling is deliberate: a bus adjacent enough to open a TLS connection to us is a bus whose relay signatures we must be able to verify. All-zero is refused, and so is an **empty value** (`-peer-client-fingerprint "$FP"` with `FP` unset is a *lost* value, not an absent flag; presence is detected with `fs.Visit`, exactly as for `-tls-fingerprint`). **Keyed to a BUS PRINCIPAL and UNIQUE across the trust table** — the opposite of `-tls-fingerprint`, which is keyed to an address and deliberately duplicated. Binding a fingerprint another bus's trust record already holds is refused by the store with `relay.ErrPeerClientCertAlreadyBound` (exit `1`, atomically, nothing written — it cannot be decided from the command line). **A trust record is written WHOLE, so an omitted binding is an ERASED binding**: re-adding a trust record for an already-bound bus (e.g. a plain `-signing-key` rotation) without re-stating this flag is refused at exit `2` before any write, not silently unbound. See the contrast subsection below. |
| `-route-for` | *(none)* | Repeatable. Installs a static next-hop route for another bus through `-url`. **Requires `-url`**; may not name this bus, may not name `-bus-id` (that route is what `-url` installs), and may not repeat a destination (two spellings differing only by ASCII case are the same routing key). |
| `-json` | off | One JSON object on **stdout**. |
| `-log-level` | `warn` | Severity floor for recovery/durability lines on stderr. |

`remove` takes `-data-dir`, `-bus-id`, `-json`, `-log-level` plus `-route` and `-trust`. **At least
one of `-route`/`-trust` is REQUIRED and neither is implied by the other.** The two mistakes are not
symmetric: withdrawing a route you meant to keep breaks federation loudly and is repaired by re-adding
it, while leaving a key pinned that you meant to **revoke** fails silently and looks exactly like a
working bus. A default either way would make one of those easy, so the command will not guess.
Removal leaves a **tombstone** rather than deleting (that is what stops a duplicated older record
resurrecting a withdrawn configuration); `peer list` does not show tombstones.

**There is no flag that supplies a `config_seq`, and none may be added** (invariant 1): the store
mints it from its own replayed high-water mark. Operator input names *which* bus and *where* it lives
and never influences a minted number.

#### Exit codes (`agent-bus peer`)

`0`–`4` are deliberately **the same numbers with the same meanings as `agent-bus invite`**.

| Code | Meaning | Remedy |
| --- | --- | --- |
| `0` | Every requested change is durable — or was already the configuration on disk, which writes nothing and reports `"unchanged": true`. | — |
| `1` | A change failed. Anything already durable is listed under `applied` in `--json` (and after `ALREADY DURABLE:` on stderr otherwise). **Also the already-bound refusal**: `-peer-client-fingerprint` names a certificate another bus's trust record already binds (`relay.ErrPeerClientCertAlreadyBound`) — undecidable from the command line, so it is not a usage error. | Read the message; `peer list`; retry. |
| `2` | Usage: bad flag, unknown subcommand, positional argument, malformed bus id, bad `-url`/`-signing-key`/`-tls-fingerprint`/`-peer-client-fingerprint`, a self-peer, or a combination that would do nothing (`add` with neither `-url` nor `-signing-key`; `remove` with neither `-route` nor `-trust`; `-route-for` without `-url`; `-tls-fingerprint` without `-url` or with an empty value; `-peer-client-fingerprint` without `-signing-key` or with an empty value). **Also the pin-consistency refusals** — an `add` that would erase an existing next-hop TLS pin, leave one address with two pins, or erase an existing `-peer-client-fingerprint` binding on a trust record (e.g. a plain `-signing-key` rotation re-run without re-stating the binding); those are decided under the lock but **before any write**. Nothing is written. | `agent-bus peer -h` |
| `3` | The data directory is **locked** — a bus is running. | Stop the bus, configure peering, start it again. |
| `4` | The data directory is missing, is not a directory, or holds no `bus-id` file. **Nothing is written, not even `bus.lock`.** | Start the bus once if it has **never run**; restore `bus-id` from backup if it has. |
| `5` | `remove` found **none** of the record kinds it was asked to withdraw, so **nothing is written**. If one of `-route`/`-trust` existed and the other did not, the one that existed **is** withdrawn, the command exits `0`, and the absent kind is named in `not_found`. | `agent-bus peer list` and check the spelling — a bus id differing only by ASCII case is a **different** bus. |

`5` is separate from `1` because a provisioning script that removes-then-adds must be able to tell
"there was nothing to remove" (fine) from "the removal failed" (not fine). **It fires only when
nothing at all was withdrawn**, and that matters: an earlier version returned on the first absent
record, so `peer remove -bus-id busA -route -trust` against a bus that is pinned but not routed —
exactly the non-adjacent case above — aborted on the missing route and **left the trust anchor
pinned** while exiting with the code a script is told it may ignore. Both gates reproduced it. `-h`/`--help` print to
**stdout** and exit `0`; only errors go to stderr, and an unknown subcommand is **not echoed back**.

#### JSON shapes — CONTRACT

`add` / `remove` success, and `list`:

```json
{"ok":true,"bus_id":"<this bus>","changes":[
  {"kind":"trust","bus_id":"busA","state":"active","signing_keys":["<b64>"],"config_seq":1,"updated_at":"…"},
  {"kind":"route","bus_id":"busB","state":"active","base_url":"https://b.example:8443","next_hop_tls_cert_sha256":"<64 hex>","config_seq":2,"updated_at":"…"},
  {"kind":"route","bus_id":"busC","state":"active","base_url":"https://b.example:8443","next_hop_tls_cert_sha256":"<64 hex — busB's>","next_hop_bus_id":"busB","config_seq":3,"updated_at":"…"}]}

{"ok":true,"bus_id":"<this bus>","routes":[…],"trust":[…]}
```

`remove` may also carry `"not_found":["route"]` — the requested kinds this bus held nothing for,
reported rather than dropped so a partial withdrawal is visible. `kind` is `"route"` or `"trust"`;
`state` is `"active"` or `"removed"`. `unchanged: true` appears when
the store found that exact configuration already applied and therefore wrote **nothing** — `config_seq`
then names the **earlier** generation. `next_hop_bus_id` is **this command's knowledge, not a durable
field**; `next_hop_tls_cert_sha256` **is** durable and is the same key the on-disk record uses, so
`--json` and the record read alike. It is absent when the hop is unpinned — never 64 zeros, which
would read as a pin nobody set. **On a `-route-for` line it is the certificate of the bus at
`base_url`, NOT of `bus_id`**, and those are different buses. `list` reports ACTIVE records only, each
sorted by bus id, and its human output prints the pin (or `no certificate pinned for that address`)
indented under the address it belongs to.

Failure (`--json`, on **stdout**, so a caller that discarded stderr still gets a parseable answer):

```json
{"ok":false,"error":"…","remedy":"…","exit_code":2,"applied":[…]}
```

`applied` lists what became **durable before** the failure — a configuration change is several records,
and a partial failure leaves the earlier ones on disk.

#### Two things this does NOT do

- **Nothing serves this yet.** Records written here are durable and are replayed, but no running bus
  reads them: `relay.Handler` is registered on no listener and `relay.PeerStore` is not yet wired into
  server startup (`RELAY-24` is the composition root). This command configures a topology that is not
  yet served. **This is why `-peer-client-fingerprint` (below) matters even though nothing consumes it
  yet**: `bindablePeerCount` (`cmd/agent-bus/relaywiring.go`) counts ACTIVE trust records carrying that
  field, and the `/v1/peer/*` ingress that `RELAY-24` will register refuses to mount when the count is `0` —
  a registered-and-refusing route would advertise federation while serving nobody. Before this flag, no
  shipped command wrote the field, so the count was `0` for every operator-reachable configuration; this
  flag makes a bindable peer producible, which is **code-complete, not live** — nothing serves the peer
  routes until `RELAY-24`'s composition root is wired into a running server.
- **It does not fix `RELAY-36`.** `internal/relay/client.go`'s `peerURL` — the function that actually
  dials — still accepts a path. This command's `-url` check is **strictly tighter** than `peerURL`, so
  no value it writes can reach that gap, and `TestPeerAddURLRulesMatchTheDurableRecord` pins the CLI
  check against the durable record's own rule so the duplicate cannot drift in either direction.

**A `-route-for` record's address belongs to a DIFFERENT bus than its bus id.** Consequence for
`RELAY-20`/`RELAY-24`: anything that later keys a per-peer credential off the record's bus id — a TLS
certificate pin, a client certificate, a peer principal — would pin the *destination's* identity
against a connection that terminates at the *next hop*, and would break every non-adjacent hop. The
identity on the wire is the next hop's; the record's bus id is the destination.

> **RELAY-41 IS THE RESOLUTION OF THAT WARNING for the first such credential.** `-tls-fingerprint`
> and `PeerRecord.NextHopTLSCertFingerprint` are keyed to the record that carries `-url` — the next
> hop — and never to the record's bus id. Worked example:
>
> ```
> agent-bus peer add -bus-id busB -url https://b.example:8443 -tls-fingerprint <fpB> -route-for busC
> ```
>
> writes `fpB` — **busB's, the next hop's** — onto **both** records, so the `busC` route record
> carries `bus_id: busC` **and** `next_hop_tls_cert_sha256: <fpB>`. That mismatch between the bus id
> and the fingerprint is **correct, not a mix-up**: the handshake that record describes terminates at
> busB. Nothing on the command line can key a pin to a destination — the flag is refused without
> `-url`, and the value is written onto whatever records receive that address.
> `TestPeerAddTLSFingerprintRoundTripsOnDisk` (`cmd/agent-bus`) and
> `TestPeerRecordTLSFingerprintIsKeyedToTheNextHop` (`internal/relay`) are the anti-regression tests;
> both use two **different real certificates**, because with one certificate every assertion would
> pass under either keying.
>
> The warning above still stands **unresolved for the credentials RELAY-41 did not add** — a client
> certificate, a peer principal. Each must be keyed the same way.
>
> **WHICH CERTIFICATE, IN WHICH DIRECTION — and DO NOT INVERT THE PIN.** This pins the certificate
> presented **by the hop at `-url` when this bus DIALS it**: an **outbound, server-side** certificate
> keyed to an **address**. It is **not a source of inbound peer identity** — `RELAY-20`'s
> `r.TLS.PeerCertificates[0]` is the peer's **client** certificate on a connection *to* us, and
> nothing in this task or in `MTLS-CLIENTAUTH` establishes that the two certificates are the same.
> That binding needs its own record.
> Next-hop keying also puts **one fingerprint on several records with different bus ids** (`fpB` is
> on busB's route *and* on busC's, above), so a `fingerprint -> bus id` index is **ambiguous by
> construction** and would resolve an inbound busB connection to **busC** — a peer-principal spoof
> produced by reading correct data backwards. `base_url -> bus id` is the same trap in the other
> field. Resolve **address first, outbound only**, and read **each record's own pin** rather than
> caching one per address.
>
> **Still configuration only.** No connection is verified against this pin yet. Whatever eventually
> compares it must compute it with `buscert.FingerprintOf`, byte for byte — a mismatched pin does not
> report a mismatch, it reports an unknown peer.

> **`-peer-client-fingerprint` IS THE OTHER HALF the warning above named as still unresolved, and it
> pins in the OPPOSITE DIRECTION from `-tls-fingerprint`. Do not confuse the two; they name different
> certificates in the general case, and neither substitutes for the other.**
>
> | flag | direction | pins whose certificate | keyed to | lands on |
> | --- | --- | --- | --- | --- |
> | `-tls-fingerprint` | OUTBOUND | the bus at `-url` — the NEXT HOP — serving TLS to us when WE dial IT | an ADDRESS; deliberately duplicated across every route sharing it | the `peer` (route) record, `next_hop_tls_cert_sha256` |
> | `-peer-client-fingerprint` | INBOUND | the bus at `-bus-id`, acting as a TLS CLIENT, when IT dials US | a BUS PRINCIPAL; unique across the table | the `bustrust` record, `peer_client_tls_cert_sha256` |
>
> **Requires `-signing-key`, not `-url`** — the opposite requirement from `-tls-fingerprint`, and for
> the same reason stated on the flag row above: it lands on the trust record, and no trust record is
> written without at least one pinned signing key. **One certificate names exactly one bus**: binding a
> fingerprint another bus's trust record already holds is refused by the store with
> `relay.ErrPeerClientCertAlreadyBound` — exit `1`, atomically, nothing written, because it cannot be
> decided from the command line alone; it depends on what another record holds. `peer list --json`
> reports it on the trust entry under `peer_client_tls_cert_sha256`; the human `peer list` prints
> `inbound client certificate bound: <hex>` or, when there is none, states the absence explicitly
> (`no inbound client certificate bound`) — the same "state the absence, do not leave it blank"
> convention `-tls-fingerprint`'s route listing already uses.
>
> **This is inbound-only.** It says nothing about who this bus's own client certificate is presented as
> when IT dials out — that direction has no flag here at all, because `-tls-fingerprint` pins the far
> end's server certificate, not our own client identity.

**A route record is written WHOLE, so an omitted pin is an ERASED pin — `add` refuses two shapes of
that, before writing anything** (both found by RELAY-41's security gate; both exit `2`):

- **Re-adding a pinned route without `-tls-fingerprint`.** `peer add -bus-id busB -url X` against a
  busB that is already pinned would replace the record with an unpinned one and exit `0`. The
  realistic path is a colleague following a runbook written before the flag existed. Remedy: re-state
  the pin (`peer list` shows it), or — if you really mean to stop pinning that hop — `peer remove
  -bus-id busB -route` first, which withdraws the **route** entirely and leaves a tombstone, so you
  must then re-add it unpinned.
- **Leaving one address half-pinned.** Routes through one hop are separate records, so
  `-bus-id busB -url X -tls-fingerprint <new>` alone would update busB and leave every `-route-for`
  destination through busB on the OLD certificate (or unpinned) — a rotation reported as successful
  while half the routing table trusts the old key. Remedy: name every destination through that hop in
  the SAME invocation.

**A trust record is written WHOLE too, and gets the same refusal for the same reason.** Re-running
`peer add -bus-id busB -signing-key <b64>` — e.g. a plain key rotation — against a busB that already
has a `-peer-client-fingerprint` bound is refused at exit `2` before any write, rather than silently
unbinding busB while reporting `unchanged`. That was the live bug `RELAY-45-FU-CLI`; this task fixes it.
Remedy: re-state the binding (`peer list` shows it), or withdraw the trust record first with
`peer remove -bus-id busB -trust`. A **route-only** add (`-url` with no `-signing-key`) writes no trust
record and is **not** refused — there is nothing on that record to erase. Also fixed in this task:
`unchanged` reporting now compares the binding as well as the pinned keys, so re-binding a **rotated**
client certificate at an otherwise-unchanged key set is no longer reported as "already configured;
nothing written".

**What that costs, stated plainly: adopting pinning on an EXISTING federation means re-stating that
hop's `-route-for` destinations once**, in the invocation that first pins it — otherwise the
already-unpinned siblings trip the second refusal. An add that pins **nothing** is unaffected, so a
federation that never uses `-tls-fingerprint`/`-peer-client-fingerprint` behaves exactly as before.

**The scope of "one address, one certificate": it is a CLI check over the stored `base_url` STRING,
compared case-insensitively.** It resolves nothing, so all of these are *different addresses* as far
as it is concerned and may end up carrying divergent pins: `https://h` versus `https://h:443`, a
trailing-dot FQDN, and two DNS names for one machine. It is also **not enforced by the record** — a
hand-edited or externally-generated log can still hold a divergence. So a consumer must read **each
record's own pin** and never cache one pin per address.

**Dependency on `RELAY-34`.** `openPeerStore` passes `relay.PeerStoreOptions.Dir = <data-dir>` on
**both** the writable and the read-only path. Withdrawals are refused by the store without it (a
withdrawal recorded only in the log can be un-said by a discarded tail, and for the trust table that
means a revoked pinned key comes back), and `peer list` needs it so a revocation an operator just
made is not displayed as still pinned. **RELAY-12 therefore must not be committed before RELAY-34.**

**The replay precondition, and how this command satisfies it.**
`relay.PeerStoreOptions.Durable` requires the log to be replayed into the store **before the first
write**: `config_seq` rebuilds only from the records `Apply` sees, so an un-replayed store mints
`config_seq=1` over a log already holding `1..N` and the **superseded generation wins** on the next
replay (reproduced by a security gate during `RELAY-10`). The package cannot enforce it. This command
does so **structurally**: the store is constructed around `deferredLog` (`cmd/agent-bus/invite.go`),
whose `Write` **errors** while the log is nil, and the log is handed over only after `wal.Open` — which
replays into the `Applier` before returning — has succeeded. A write before replay is therefore
unreachable, and would fail loudly rather than silently mint `1`.
`TestPeerAddReplaysTheLogBeforeTheFirstWrite` asserts the `config_seq` sequence on disk across three
separate invocations and that a full replay reconstructs the **latest** configured address; it was
confirmed RED against a build with the `Applier` removed.

---

### `agent-bus key export-public` — export this bus's SIGNING PUBLIC KEY

Added 2026-08-14 by `CLI-11`. Source: `cmd/agent-bus/key.go`.

```
agent-bus key export-public -data-dir <dir> [-json]
```

It prints the **public half** of this bus's Ed25519 signing key — the value a **peer** bus pins with
`agent-bus peer add -signing-key`, and the thing that lets that peer verify a message which
**originated** here. Before this existed there was no compiled way to obtain the value at all: the key
lived only inside a `0600` PKCS#8 PEM in the data directory, so the federation smoke test
(`scripts/fed-smoke.sh`) could not take its first step, and the workaround anyone would otherwise
reach for — scraping the PEM with `openssl` — is exactly what invariant 7 forbids.

**It is a subcommand on the SERVER binary, not on `agent-busctl`,** and this was a deliberate move: the
task was specified against `agent-busctl`. The authority it needs is **filesystem access** to the data
directory (`DECISIONS.md` E4), not a network privilege, so it belongs beside `invite mint` and
`peer add`. `agent-busctl` is a pure HTTP client — it imports only `client/`, touches nothing under
`internal/`, and has no data-directory or `dirlock` plumbing; giving the network client filesystem and
lock access to satisfy a spelling would have been a real architectural change justified by nothing.

**THE BUS MUST BE STOPPED.** It takes the data directory's exclusive `dirlock`. Note the lock is *not*
protecting a write of ours — this command writes no key material, only the `bus.lock` the lock itself
creates. Holding it is what stops a bus **starting** against a virgin directory from minting key
material between the presence check and the load.
`healthcheck` is the deliberate contrast: it takes no lock, because it reads one file and asserts
nothing about the directory's shape.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data-dir` | `./data` | The bus's data directory. **Never created, and neither is any part of the identity in it.** All four of `bus-id`, `bus-tls.crt`, `bus-tls.key` and `bus-signing.key` must already be present (exit `4` otherwise). |
| `-json` | off | Emit one JSON object on **stdout**. |

Both flag spellings work (`-json` and `--json`): Go's `flag` package treats them identically, so the
`--data-dir … --json` form `scripts/fed-smoke.sh` uses is accepted as-is.

**There is no flag that exports the private key, and none may be added.** The private half has exactly
one legitimate location — its own `0600` file — and a backup of that file is the only supported way to
copy it. A convenience flag would put it in a shell history and a CI log the first time anyone used it.

#### It NEVER mints an identity — the load-bearing property

`buscert.LoadOrCreate` **generates** a certificate and two private keys when the data directory holds
**none** of the three, and there is no load-only entry point in `internal/buscert`. An *export* command
that quietly minted would be a **federation-wide identity event triggered by a read**: the operator
would copy a signing key no bus has ever served, pin it on a peer, and discover the fault only when a
relayed message failed to verify — which at the peer is indistinguishable from the substitution the pin
exists to detect. It would also leave a half-built directory that the real first start then refuses.

So the material is checked **present** before it is loaded, **twice**, exactly as `invite mint` does it
and for the same two reasons: the **pre-lock** check keeps a refusal from writing so much as a
`bus.lock` into a mistyped directory, and the **post-lock** check is the one load-bearing for
correctness, because between the two a concurrent process could remove a file and turn the load into a
mint. `material.Generated()` is then refused as a last resort.

All four files are required, not just `bus-signing.key`: `ids.LoadOrCreateBusID` *creates* a bus id
when absent, and requiring the other three present is what makes `buscert`'s mint branch structurally
unreachable rather than merely unlikely.

`TestKeyExportRefusesADirectoryWithNoKeyMaterial` asserts the directory is still **empty** afterwards —
not merely that the exit code was nonzero, because a command that minted and then failed for some later
reason satisfies an exit-code-only test while having just created a bus identity nobody asked for.

#### The encoding is MATCHED, not chosen

**Standard base64 with padding — 44 characters for the 32-byte key.** That is precisely what
`agent-bus peer add -signing-key` parses (`base64.StdEncoding.DecodeString`, `cmd/agent-bus/peer.go`)
and what `internal/relay` writes into a `BusTrustRecord`, so the printed value pastes straight into the
command that consumes it. **It is NOT the 64-lowercase-hex encoding used for the TLS certificate
fingerprint.** Two 32-byte values live in this workflow and are distinguishable only by their encoding;
confusing them installs a pin that can never verify anything and reports no error until a relayed
message fails. `TestKeyExportBusSigningPublicKey/the_key_is_NOT_the_TLS_fingerprint_encoding` pins them
apart. No encoding, hash or KDF is implemented here (invariant 9) — this is stdlib base64 over a key
`crypto/ed25519` derived.

#### `--json` success shape

```json
{"ok":true,"bus_id":"bus-k53jl6eorczuwznc","public_key":"hvW9…8t0=","key_type":"ed25519"}
```

`bus_id` is reported **with** the key because a peer pins the two as a pair (`peer add -bus-id X
-signing-key Y`); a bare key invites pinning it against the wrong bus. `key_type` is `ed25519`, the only
value today, so a consumer never infers the algorithm from the length. **There is no field for the
private key** and the struct has none.

The failure shape is `{"ok":false,"error":…,"remedy":…,"exit_code":…}` on **stdout**, so an agent that
redirected stderr away still gets a parseable answer.

#### Exit codes (`agent-bus key`)

| Code | Meaning | Remedy |
| --- | --- | --- |
| `0` | The public key was printed. | — |
| `1` | The export failed — unreadable file, corrupt certificate, **or an expired one**. Nothing was written. | The bus refuses to start on the same error; fix it there. |
| `2` | Usage: bad flag, unknown subcommand, or a positional argument. | `agent-bus key export-public -h` |
| `3` | The data directory is **locked** — a bus is running. | Stop the bus, export, start it again. The signing key does not change across a restart. |
| `4` | No data directory, or it does not hold all four identity files, or the certificate's CommonName names a **different bus** than the `bus-id` file. **Nothing was created** — with the one carve-out below. | Start the bus once if it has **never run**; **restore the missing file from backup** if it has. |

**Two carve-outs on "nothing was created",** both being races this command lost under the lock, where a
library call wrote on the way to the refusal:

- The **`material.Generated()`** backstop exits `4` precisely *because* `buscert` minted. It does **not**
  delete what was minted (deleting key material is never this command's call), and `Generated()` is
  **false** on the next load — so a re-run would pass every check and export the freshly minted key with
  **exit 0 and no warning**. The operator would then pin, on a peer, a key no bus has ever served: at
  that peer, indistinguishable from the substitution the pin exists to detect. The remedy on that branch
  therefore says to **delete the three files by hand** before anything else.
- The **CommonName cross-check** exits `4` when `ids.LoadOrCreateBusID` minted a bus id in the same kind
  of window. That refusal is **persistent** — the new id disagrees with the certificate on every
  subsequent run — so unlike the first it cannot decay into a silent success, and its remedy is to
  **restore**, not to delete.

Every other path to `4` wrote nothing at all.

An unreadable-but-present **identity file** (EACCES, EIO) is exit **`1`**, not `4`, and its remedy says
*do not restore over it until you have looked* — "I could not look at the file" is not "the file is not
there", and telling an operator to restore a `bus-id` that is present and fine would rename the bus away
from its own certificate. **The data directory itself is the exception:** any `stat` failure on
`-data-dir` is exit `4`, whose message is "cannot read the data directory" and whose remedy tells nobody
to restore anything. Folded into `CLI-11-FU-STATERR`.

`-h` / `--help` print to **stdout** and exit `0`, at both `agent-bus key -h` and
`agent-bus key export-public -h`; only errors go to stderr. An unknown subcommand and an unexpected
positional argument are **not echoed back** — they are unvalidated argv on the way to a terminal.

**Known wart, recorded rather than hidden:** an **expired TLS certificate** makes this command exit `1`,
even though the signing key it would print is independent of the certificate. `buscert.LoadOrCreate`
validates the certificate's date window on the way to loading the signing key, and there is no
load-only accessor for the signing half — so an operator whose certificate has expired cannot export
the key a peer needs in order to keep verifying messages that originated here, which is exactly when
they need it. Fixing it means adding a load-only accessor to `internal/buscert`, which was outside this
task's file boundary: filed as **`CLI-11-FU-LOADONLY`**, together with the matching `internal/ids` gap —
`ids.LoadOrCreateBusID` has no `Generated()`-equivalent, so a `bus-id` file removed in the window
between the post-lock check and that call is **minted and persisted**. The export is still refused (the
CommonName cross-check catches it and no key is reported), but a command documented as read-only will
have written. That is why the usage text claims only that it does not create a bus *identity*.

**Private key material never reaches either stream.** There is no public-only file on disk — the public
half is *derived* from the private key — so this command necessarily loads the secret in order to print
the derived value. `Material.SigningPublicKey()` is the only accessor used; `SigningPrivateKey()` is
never called in `key.go`; nothing logs the material; and every failure path names **paths**, never
contents. `TestKeyExportNeverPrintsPrivateKeyMaterial` asserts it on **both** streams across every flag
combination including the failure paths, searching for the seed and the full 64-byte key in base64/hex
and for the on-disk PEM body lines. It is a test rather than a comment because a leak of this kind is
silent.

---

### `agent-bus log` — read the append-only MESSAGE AUDIT TRAIL (metadata only)

Added 2026-08-14 by `CLI-6`. Source: `cmd/agent-bus/auditlog.go`.

```
agent-bus log [-data-dir <dir>] [-json] [-sender <id>] [-recipient <id>]
              [-since <RFC3339>] [-until <RFC3339>] [-min-seq <n>] [-max-seq <n>]
```

An **offline, read-only** reader for `bus.audit`. It prints **metadata and routing only** — message
id, sequence, sender, broadcast flag or recipient list, the ordered bus path traversed, the time this
bus accepted the message, the body's size and its SHA-256. **Message bodies are not in the file and
cannot be recovered from it** by this command or any other (invariant 6); the content hash is what
preserves the ability to prove *what* was sent without retaining it. The `--json` record struct has no
body field, no `payload` and no catch-all, and the raw frame is never marshalled.

**It is a subcommand on the SERVER binary, not on `agent-busctl`,** for `invite mint`'s reason
(`DECISIONS.md` E4), restated by `peer add` and `key export-public`: the authority it needs is
**filesystem access** to the data directory, not a network privilege. **There is no HTTP route that
serves the audit trail and this command does not add one.**

**Known mismatch, recorded rather than hidden:** `scripts/fed-smoke.sh`'s `read_audit` (line 191)
invokes `"$CTL" log --data-dir … --json`, and `CTL` is `bin/agent-busctl` (line 55) — but the
subcommand landed on `agent-bus`, and `agent-busctl` registers no `log` command. That call therefore
takes its `die "BLOCKED: CLI-6 agent-busctl log is unavailable"` branch. This is a **static reading of
both files at `a8c367c`, not an observed run** — the smoke test asserts a three-hop `bus_path` that
nothing produces yet (below), so it does not reach that step regardless. The `jq` selectors match the
output shape documented here; only the binary name is wrong.

**THE BUS MUST BE STOPPED.** It takes the data directory's exclusive `dirlock`. It writes nothing and
repairs nothing — the lock is what stops a read from seeing a half-written tail record and then either
reporting a message that is not yet durable or reporting a healthy bus as damaged.

**There is deliberately no `--follow`.** While this command holds the exclusive lock no bus is running,
so nothing is appending; tailing a file nobody writes to is not a capability. A tail mode needs a
lock-free consistent read first, which is its own decision — deferred to **`CLI-6-FU-FOLLOW`**, not
omitted by oversight. Do not re-file it.

The trail is a **superset of committed history**: a crash between the audit write and the commit write
leaves a record for a message that never became accepted history. `prepare_index` names the WAL
transaction, so an audit record can be paired with the WAL entry that (may have) committed it.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data-dir` | `./data` | The bus's data directory. It must already hold `bus.audit`; **this command never creates one** (exit `4` otherwise). Empty is a usage error. |
| `-json` | off | Emit **NDJSON** on stdout: one object per record, one per line. |
| `-sender <id>` | — | Only records whose sender is **exactly** this fully-qualified `<bus-id>.<agent-id>`. |
| `-recipient <id>` | — | Only records whose recipient list **contains** this id. **A broadcast never matches** — it records no recipient list, so matching one would be a guess about a roster the trail deliberately does not hold. |
| `-since <RFC3339>` | — | Only records with `sent_at` **>=** this instant (**inclusive**). |
| `-until <RFC3339>` | — | Only records with `sent_at` **<** this instant (**exclusive**), so consecutive windows tile the timeline with no gap and no double-count. |
| `-min-seq <n>` | `0` | Only records with `seq >= n` (**inclusive**). `0` means no bound. |
| `-max-seq <n>` | `0` | Only records with `seq <= n` (**inclusive**). `0` means no bound; sequences start at 1, so `0` can never exclude a real record. |

Both flag spellings work (`-json` and `--json`): Go's `flag` package treats them identically.

Filter flags are validated **before anything is opened and before the lock is taken**, so a mistyped
timestamp costs no I/O. An unparseable `-since`/`-until`, `-min-seq` above `-max-seq` (both non-zero),
or a `-until` that is not strictly after `-since` are each exit `2`.

**`bus_path` is the ORDERED traversal, oldest bus first.** Nothing sorts, dedupes or reorders it — the
reader displays exactly what was recorded. **A running bus DOES produce a multi-hop value, and has
since `RELAY-24` wired relay ingest (2026-08-15).** A locally-originated send or broadcast
still carries a single element — `hub.publish`'s `Send`/`Broadcast` callers set no path, so `publish`
substitutes `store.LocalBusPath(h.busID)` — but `hub.IngestRelayed` appends this bus's own hop to the
path the message arrived with (`internal/hub/relayingest.go`), so any relayed record's `bus_path`
carries every bus it has actually traversed, and — since `RELAY-47` wired onward forwarding — that can
now be more than one further hop beyond the sender's own bus. `internal/hub/buspath_test.go` covers
the shape directly.

**Damage is always reported, and filters never hide it.** A frame that is not a `TypeAuditMessage`, a
frame `wal.DecodeAudit` refuses, and a scan that stops early are each named on **stderr** with the
path, byte offset and reason; the remaining records are still read and printed; and the command exits
`1` — **even when a filter excluded every record**. Silence from this command therefore means the
trail really is intact. It uses `wal.ScanAll` (strict, read-only) rather than `RepairLog` or `Replay`:
it repairs nothing and truncates nothing.

#### Exit codes (`agent-bus log`)

| Code | Meaning | Remedy |
| --- | --- | --- |
| `0` | The **whole** trail was read and every record decoded. Still `0` when a filter matched nothing — an empty result is an answer. | — |
| `1` | The trail is **damaged**, or could not be examined. Every readable record was still printed and every discard was named on stderr. Also covers a **zero-length** `bus.audit` (`wal` reports `file is empty: it has no <N>-byte file header`) and a `bus.audit` that is present but cannot be `stat`ed (EACCES, EIO). | Keep the file; do not truncate it by hand. For the un-`stat`able case the message says explicitly that this is **not** evidence the trail is missing or damaged — check permissions and ownership. |
| `2` | Usage: bad flag, an unexpected positional argument, an empty `-data-dir`, or an unparseable/contradictory `-since`/`-until`/`-min-seq`/`-max-seq`. Nothing was read. | `agent-bus log -h` |
| `3` | The data directory is **locked** by a live process, almost certainly the bus. | Stop the bus, run this, start it again. The trail is append-only, so what you see after a restart is a superset of what you would have seen now. |
| `4` | This data directory holds **no `bus.audit`** at all — so any messages this bus routed have **no provenance record**. Also the code for a `-data-dir` that cannot be `stat`ed or is not a directory. | Expected if the bus has never accepted a message: start it, send one, stop it, retry. If it **has**, the trail is lost and must be restored from backup. This command will not create one — an empty trail written now would look exactly like a bus that never carried anything. |
| `5` | **The trail cannot be AUTHENTICATED. Nothing was read and nothing should be believed.** A refusal *before* the scan, so **no record is printed on this path**. | Below. |

`4` and `5` are deliberately distinct from `1`: "there is no trail", "the trail is broken" and "the
readable part carries no authority in the first place" must never be reported as the same thing. `-h` /
`--help` print the usage text to **stdout** and exit `0`; only errors go to stderr. An unexpected
positional argument is **not echoed back** — it is unvalidated argv on its way to a terminal.

**Exit `5` fires for four states, all saying the same thing — *this reader cannot vouch for these
bytes*:**

- **No `wal-mac.key` in the data directory.** Integrity here is a keyed MAC (invariant 6), so with no
  key not one record can be authenticated. It is also a **safety** check, and this is the bug the
  task's security gate found: `wal.ScanAll` resolves a codec, which resolves a MAC key, and `wal`'s
  `macKeyMayBeCreated` permits **creating** one for exactly the shapes a reader is most likely to be
  pointed at — silently, because `ScanAll` takes no logger. On a directory whose `bus.wal` is intact
  but whose key was lost, one run of this read-only command minted a key and thereby converted
  `wal.ErrMACKeyMissing` (remedy: restore a 64-byte file) into `wal.ErrMACKeyMismatch`, **whose
  documented remedy is to move `bus.wal` aside**. Requiring the key to exist closes the whole class:
  `macKeyFor` only reaches `createMACKey` when `loadMACKey` returns `ErrMACKeyMissing`.
- **`bus.audit` does not declare on-disk format version `2`.** Version-1 frames are authenticated by an
  **unkeyed CRC32C anyone can compute**, and `wal` will happily read a version-1 file, so records an
  attacker authored would print under a header promising provenance, with exit `0` and an empty stderr.
  The security gate did exactly that. Audit records have **only ever been written at version 2**
  (`internal/wal/audit.go`), so such a file was never written by this bus.
- **`bus.audit` does not begin with the audit magic** (`AGNTBUSA`) — not a trail this bus wrote.
- **`bus.audit` is non-empty but shorter than the 12-byte magic+version prefix** — it claims to be
  something and cannot be checked. (A **zero-length** file is deliberately *not* refused here: it
  carries no header to judge and no record to misbelieve, and `wal` already reports it loudly as
  damage — exit `1`.)

A `bus.audit` that **cannot be opened at all** — `EACCES`, `EIO`, a bad mount — is deliberately **not**
in this list: it is exit `1`, "could NOT BE EXAMINED", because exit `5` presupposes we could see the
bytes and judge them. That split is load-bearing and must not be collapsed; see the block comment on
`checkAuditFormatVersion` in `cmd/agent-bus/auditlog.go`.

The remedy on the last three is the same: keep the file, do **not** let the bus append to it, inspect it
out of band, and do not treat its contents as evidence.

#### `--json` shape — NDJSON, one object per line

One record object per line, so it streams and so a consumer can count:

```json
{"audit_index":1,"offset":12,"message_id":"bus-k53jl6eorczuwznc-42","seq":42,"sender":"bus-k53jl6eorczuwznc.agent-alpha","broadcast":false,"recipients":["bus-k53jl6eorczuwznc.agent-beta"],"bus_path":["bus-k53jl6eorczuwznc"],"sent_at":"2026-08-14T09:00:00.123456789Z","size":42,"content_sha256":"<hex>","prepare_index":7}
```

Keys, exactly: `audit_index`, `offset`, `message_id`, `seq`, `sender`, `broadcast`, `recipients`,
`bus_path`, `sent_at`, `size`, `content_sha256`, `prepare_index`. The payload field names mirror `wal`'s
`auditPayload` JSON tags, plus two frame-level locators the payload cannot carry: `audit_index` and
`offset`.

- `sent_at` is **RFC3339Nano, UTC**.
- **`recipients` and `bus_path` are ALWAYS present and are NEVER `null`** — a nil slice is normalised to
  `[]`, so a broadcast emits `"recipients":[]`. A missing key would read as "not recorded" when the
  truth is "recorded, and empty".
- `audit_index` is a **frame locator, not an identity**: a quarantined audit log restarts at `1`, so
  audit indices are not unique across the lifetime of a data directory. **Join on `message_id` or
  `seq`.**
- `offset` is the byte offset of the record's frame header, so a damage report and a record line up
  against the same file.

**CONTRACT: no object other than a record ever carries a `message_id` key.** `scripts/fed-smoke.sh`
(`assert_audit_path`) selects on `.message_id` and `.bus_path` and requires **exactly one** match per
message, so a consumer counting records by `message_id` must not be corruptible by a non-record line.
The two non-record shapes therefore omit it:

```json
{"damaged":true,"path":"./data/bus.audit","audit_index":3,"offset":512,"reason":"…","remedy":"…"}
{"ok":false,"error":"…","remedy":"…","exit_code":5}
```

`{"damaged":true,…}` is emitted **in addition to** the stderr report, never instead of it — a human
watching the terminal and a script parsing stdout must both learn about a discard. `audit_index` is
omitted when the framing scan itself found the damage and there is no record to name; `remedy` is
omitted when empty. `{"ok":false,…}` is the pre-read failure shape and goes to **stdout**, so an agent
that redirected stderr away still gets a parseable answer.

#### Human output

A header naming the file and stating that bodies are not in it, then two-to-four lines per record, then
a count. **Every client-derived string is printed `%q`-quoted** — sender, message id, each recipient and
each bus-path element — and each element of a list is quoted *individually*, never the joined string:
`wal` bounds these fields on emptiness and length only and imposes **no character restriction**, so a
newline in a sender would otherwise forge a whole record line, and an ANSI escape would reach the
terminal. The security gate produced both. An empty `bus_path` renders as `(none)` rather than as a
blank, and a broadcast renders as `broadcast (no recipient list is recorded)`. The footer repeats that
the trail is damaged when it is, and notes that the count covers only the records that could be read.

---

### `agent-bus operator keygen|add|list|revoke` — the OPERATOR/ADMIN principal (`AUTH-10`, 2026-08-16)

An **operator** is a bus-scoped identity that is **not an enrolled agent**. It exists so that
operator-only capabilities can be authorised against something an AGENT credential can never satisfy:
if an admin route reused agent authentication, an agent credential would authorise minting the
credentials that CREATE AGENTS, and any enrolled agent could mint itself an unlimited supply of new
identities — invariant 3, collapsed. The principal is therefore distinct **in KIND**: its own id
namespace, its own durable record, its own session table and its own Go principal type
(`auth.OperatorPrincipal`, which an `auth.Principal` cannot satisfy at compile time).

```
agent-bus operator keygen -identity-dir <dir> [-json]
agent-bus operator add    -data-dir <dir> -name <name> -auth-pub <base64>
                          -cert-fingerprint <64 hex> [-label <text>] [-json] [-log-level <lvl>]
agent-bus operator list   [-data-dir <dir>] [-all] [-json] [-log-level <lvl>]
agent-bus operator revoke -data-dir <dir> -id <operator-id> -reason <text> [-json] [-log-level <lvl>]
```

It is on the **server** binary, not `agent-busctl` — the minting authority is filesystem access to the
data directory (`DECISIONS.md` E4, the same model as `invite mint` and `peer`), so no new
network-reachable privilege is introduced, and an admin capability does not go on the agent surface.

**`add`, `list` and `revoke` require the bus to be STOPPED** — they take the data directory's exclusive
`dirlock`. `keygen` touches no data directory at all.

#### `keygen` and `add` are separate on purpose

`keygen` runs on the **operator's** machine. It writes a TLS client certificate through
`client.LoadOrCreateClientCertificate` (`<identity-dir>/client-tls/`) and an Ed25519 session-signing
keypair at `<identity-dir>/operator-auth-key.pem` (PKCS#8 PEM, **0600**, in a **0700** directory), and
prints only the two PUBLIC values `add` consumes: the base64 auth public key and the certificate
fingerprint. **The private keys never leave that machine and the bus never generates them.** Existing
material is **loaded, never overwritten** — regenerating either half would silently invalidate an
operator record the bus already holds while looking like a no-op. A damaged file is an error, not a
regeneration.

#### `-cert-fingerprint` is MANDATORY, unlike an agent's certificate binding

`RosterEntry.CertBindings` may legitimately be empty; an operator's fingerprint may not. Invariant 11's
session/certificate cross-check can only be applied **unnarrowed** if there is always a pair to
cross-check, and an operator with no fingerprint would silently degrade to a bearer token alone. `add`
also **refuses a fingerprint already live-bound to an enrolled AGENT** (and to another operator): one
certificate must never name both, or the collapse above is reachable through the transport instead of
through a permission flag.

#### There is no `-operator-id` flag, and none may be added

The server is authoritative on every id (invariant 1). Ids are `op:<bus-id>.<name>-<suffix>`, where the
suffix is 16 characters of lowercase base32 over 10 bytes of `crypto/rand` — the same construction
`ids.GenerateBusID` and `invite.GenerateInviteID` use. **Revoking does not free an id**; adding an
operator under a previously-used *name* mints a new suffix and is a **different principal**.

#### Revocation

`revoke` **appends** a record (invariant 6: no in-place edits, no deletions) and requires `-reason`
(invariant 6: an operator action must be attributable). **Re-revoking is a legitimate retry**
(invariant 10): it returns the ORIGINAL record, writes nothing, and reports `"unchanged":true`.
`list` hides revoked operators; `list -all` shows them with the instant and the reason.

> **Revocation reaches a RUNNING bus only after a restart, TODAY** — a property of this build, not of
> revocation. There is no online admin route yet, so this offline command is the only writer. *Inside* a
> running server a revocation is refused at the very next request with no restart, because
> `OperatorService.Authenticate` re-reads the registry on every call and `OperatorRegistry.Revoke` drops
> the operator's live sessions synchronously.

#### Flags — CONTRACT

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data-dir` | `./data` | The bus's data directory, for `add`/`list`/`revoke`. **Never created**, and neither is any part of the bus identity in it — it must already hold the `bus-id` file (exit `4` otherwise, with nothing written, not even `bus.lock`). Takes the directory's exclusive `dirlock`, so **the bus must be stopped**. |
| `-identity-dir` | *(none — REQUIRED)* | `keygen` only, and the **operator's own machine**, not the bus host. Receives `client-tls/` (via `client.LoadOrCreateClientCertificate`) and `operator-auth-key.pem`. Created `0700`; an existing directory looser than that is **tightened to `0700` and the change is warned about**, naming both modes. Existing material is LOADED, never overwritten. |
| `-name` | *(none — REQUIRED)* | `add` only. Matches `^[a-z0-9][a-z0-9_-]{0,63}$`. It is the human half of the minted id, **not** the id: the same name added twice yields two different principals. |
| `-auth-pub` | *(none — REQUIRED)* | `add` only. The operator's Ed25519 **session-signing public key**, standard base64, exactly 32 bytes decoded. Printed by `keygen`. Distinct from the TLS key — see the two-keys note above. |
| `-cert-fingerprint` | *(none — REQUIRED)* | `add` only. 64 lowercase hex = SHA-256 over the operator's client-certificate DER. **Mandatory**, unlike an agent's binding; refused if already live on an enrolled agent or another operator. |
| `-id` | *(none — REQUIRED)* | `revoke` only. The full `op:<bus-id>.<name>-<suffix>`. An agent id is refused — the namespaces are structurally disjoint. |
| `-reason` | *(none — REQUIRED)* | `revoke` only. **Required, not optional**: invariant 6 wants an operator action attributable, and an unattributed revocation is the one an incident review cannot reconstruct. Durable, and rendered with `%q` in human output so a control-sequence-bearing reason cannot reach a terminal. |
| `-label` | *(empty)* | `add` only. An operator note recorded on the principal. Shown by `list`; it authorises nothing. |
| `-all` | off | `list` only. Include REVOKED operators, with the revocation instant and reason. |
| `-json` | off | *(all four)* Emit one JSON object on **stdout** — for success **and** failure, so a caller that discarded stderr still gets a parseable answer. |
| `-log-level` | `warn` | *(`add`, `list`, `revoke` — NOT `keygen`)* Severity floor for recovery/durability lines on stderr (`debug`, `info`, `warn`, `error`). `keygen` opens no log and has neither this nor `-data-dir`. |

**There is no `-operator-id` flag and none may be added** — see above; `MintOperatorID` has no
parameter for one, which makes it structural rather than a rule this file has to remember.

#### Exit codes (`agent-bus operator`) — CONTRACT

> **REACHABLE FROM `argv` since `AUTH-10-WIRING` (2026-08-21) — this caveat has been REVERSED.**
> It read "**NOT REACHABLE FROM `argv` TODAY (2026-08-16)**", said `cmd/agent-bus/main.go` "does not
> dispatch `os.Args[1] == "operator"`" so **every** surface in this section "falls through to the
> server's flag parser and is refused as an unexpected argument", and warned that the codes below
> "are not yet the codes a command run at a prompt returns, because the command does not run".
> **Every clause of that is now false.** `main.go` dispatches `operator` beside `invite`, `peer`,
> `key` and `log`, `agent-bus -h` announces the subcommand, and the table below IS what a command run
> at a prompt returns — including `operator revoke`, the only revocation mechanism in the design.
> `TestOperatorSubcommandIsReachableFromArgv` (`cmd/agent-bus/operatorwiring_test.go`) proves it by
> driving the **compiled binary** through `add`, `list` and `revoke`. `cmd/agent-bus/operator_test.go`
> calls `runOperatorCommand` directly and stayed green for the whole period the command could not be
> typed, which is exactly why it could not have caught this.
>
> **Not softened by any of that:** `add`, `list` and `revoke` take the data directory's exclusive
> lock, so **the bus must be stopped** (exit `3` otherwise). A revocation is therefore in effect from
> the next start rather than on a running bus, and no HTTP route consumes an operator principal yet.

Numbered to **match** `invite` and `peer`, so an operator scripting against all three needs one table.

| Code | Meaning | Remedy |
|---|---|---|
| `0` | Success. | — |
| `1` | The command failed. | Read the message. |
| `2` | Usage: bad flag, unknown subcommand, positional argument, invalid `-name`/`-auth-pub`/`-cert-fingerprint`/`-id`, or a missing required flag (including `-reason` on `revoke`). Nothing is written. | `agent-bus operator -h` |
| `3` | The data directory is locked — a bus is running. | Stop the bus, run the command, start it again. |
| `4` | The data directory does not hold a usable bus identity (missing, not a directory, or no `bus-id` file). **Nothing is written**, including no `bus.lock`. | Start the bus once if it has never run; restore `bus-id` from backup if it has. |
| `5` | The named operator is not registered. | `agent-bus operator list -all` |
| `6` | (`list` only) The data directory holds no `wal-mac.key`, so the write-ahead log cannot be authenticated (invariant 6); the operator registry was **NOT read** and **no key was created**. **Nothing is written**, including no `bus.lock` — the refusal is pre-lock. Deliberately not `5`: `log`/`outbox` use `5` for "unverifiable", but `5` is already "operator not registered" here, and one code with two meanings breaks a scripted caller. | Restore `wal-mac.key` from backup; do not start the bus or let anything mint a key here first (a fresh key turns a recoverable "missing" into an unrecoverable "mismatch"). |

#### `--json` shapes — CONTRACT

Success and failure **both** go to **stdout**, so an agent that redirected stderr away still gets a
parseable answer, and `ok` is the one field to branch on.

```json
{"ok":true,"identity_dir":"…","cert_path":"…","cert_key_path":"…","auth_key_path":"…",
 "auth_pub":"<base64 std>","cert_fingerprint":"<64 hex>","created_cert":true,"created_auth_key":true,
 "warnings":["…"]}

{"ok":true,"bus_id":"bus-…","operators":[
  {"operator_id":"op:bus-….ops-<16 base32>","name":"ops","auth_pub":"<base64 std>",
   "cert_fingerprint":"<64 hex>","label":"…","created_at":"<RFC3339Nano>",
   "revoked_at":"<RFC3339Nano>","revoked_reason":"…"}],"unchanged":false,"warnings":["…"]}

{"ok":false,"error":"…","remedy":"…","exit_code":3}
```

`label`, `revoked_at`, `revoked_reason`, `unchanged` and `warnings` are omitted when empty/false. **No
secret is ever printed by any of these** — every value crossing the boundary is a public key or a digest.

**`warnings` is a REQUIRED read, not decoration**, and it is in the JSON document rather than only on
stderr so an agent running with `2>/dev/null` still sees it (the rule `inviteBlob.TransportInsecure`
follows). What it carries today:

| Command | Warning |
|---|---|
| `keygen` | The identity directory was **loose** (any bit in `0o077`): it is tightened to `0700` and the mode it *was* is named, because directory **write** permission is enough for another local user to REPLACE `operator-auth-key.pem` — the file's `0600` protects its contents, not which key is there. If the chmod fails, the warning says so and the command still succeeds. |
| `keygen` | **Existing material was REUSED** (`created_cert:false`): nothing was regenerated. Check the directory belongs to an operator and to nobody else — pointing `-identity-dir` at an **agent's** directory binds one certificate to both planes, which `add` then refuses. |
| `keygen` | The certificate is outside its validity window. |
| `add` | The fingerprint was held by a **REVOKED** operator. A revoked binding constrains nothing, so the add SUCCEEDS and the new operator is live on the same certificate — right after an administrative revocation, wrong if the laptop was stolen. The warning names the revoked operator id, instant and reason. |

#### Wiring status — BOTH GAPS ARE CLOSED (`AUTH-10-WIRING`, 2026-08-21)

**This section is a REVERSAL and is written as one on purpose.** It was headed "READ THIS BEFORE
ASSUMING ANY OF THE ABOVE RUNS" and listed "**two separate gaps, and the second makes the whole
section above unreachable**". Both are closed; what each one claimed is quoted below so a reader who
remembers it can tell a correction from a deletion.

1. **`main.go` DOES dispatch the subcommand.** The old gap 1 read "`cmd/agent-bus/main.go` does not
   test `os.Args[1] == "operator"` the way it does for `invite` and `peer`", concluded that everything
   in this section was "CODE-COMPLETE AND NOT REACHABLE FROM `argv`", and warned that "a bus with a
   live operator has **no shipped way to revoke it from a shell**". `main.go` now dispatches
   `operator` beside its five sibling subcommands and `parseFlags`' usage announces it, so `keygen`,
   `add`, `list`, `revoke`, the flags, the exit codes and the `--json` shapes above **are** an
   operational contract.
2. **`main.go` DOES register `auth.OperatorRecordKind`** in the applier map it hands `wal.Open`, and
   `Attach`es the registry once `Open` has returned. The old gap 2 read "`main.go` does not register
   `auth.OperatorRecordKind` … so an operator record in the WAL is passed over at **server** replay
   without a word" — the silent-discard shape invariant 6 rates as the defect, and fail-**open** for a
   revocation. The server now reports the outcome at INFO on every start:
   `msg="operator registry recovered from the append-only log…" operators_recovered=<n>
   live_operators=<n>`. A log holding two adds and one revoke reads `operators_recovered=2
   live_operators=1`; those counts reading `0` over a data directory that holds operator records is
   how the defect would be seen to return.

**Unchanged by all of that** — two things to keep straight:

- The `operator` subcommands were never affected by gap 2: they open the log with their own applier
  map, which registers the operator registry, the enrolment roster **and** the invite store (the last
  so a composite `agent+invite` record's rider has an applier — without it every gated enrolment made
  the multiplexer log a **false** "invite may be REDEEMABLE AGAIN" at ERROR on a command that writes
  nothing to the invite plane).
- **Nothing on the wire consumes an `OperatorPrincipal`.** `auth.NewOperatorService` has no non-test
  caller and no HTTP route authenticates an operator; `AUTH-7`, `INVMINT` and `CONV-AUTHZ-ADMIN` are
  the consumers and are unstarted. The principal now EXISTS and REPLAYS — it is not yet USED — and
  admitting or revoking one is still an **offline** action under the data directory's exclusive lock,
  so a revocation needs the bus restarted before it is in effect.

### `agent-bus outbox` — the RELAY DRAIN GATE: what this bus still owes, and what it gave up on (`RELAY-54`, 2026-08-21)

Source: `cmd/agent-bus/outbox.go`. Query method: `relay.Outbox.Jobs(states ...relay.OutboxState)`.

```
agent-bus outbox [-data-dir <dir>] [-json] [-peer <bus-id>] [-state <s>]...
```

An **offline, read-only** view of the durable relay outbox — the table of messages this bus accepted
responsibility for delivering to a peer. It answers two questions an operator could not previously
ask at all: **is anything still pending, and has anything been abandoned — and for which peer.**

**Why it exists.** `RELAY-51` REJECTED drain-and-restart as a rollout order, because a drain must be
VERIFIED before you restart and the instrument that would verify it did not exist. `agent-bus log`
reads `bus.audit`, which is a **different artefact** and holds no outbox record. An abandoned outbox
job — a message this bus will never deliver — was durable in `bus.wal` and reachable from no
subcommand.

**The rollout sequence it is for:** stop bus A → run this → if nothing is pending, start the NEW
binary; otherwise start the OLD binary again and wait. **A stopped bus does not defeat the gate** —
the stop is the restart's first half anyway, and a running bus is still enqueueing, so a drain cannot
be verified against one.

**It is a subcommand on the SERVER binary, not on `agent-busctl`,** for `invite mint`'s reason
(`DECISIONS.md` E4), restated by `peer`, `key export-public`, `log` and `operator`: the authority it
needs is **filesystem access** to the data directory, not a network privilege. `agent-busctl` imports
only `client/`, `internal/buscert` and `internal/signing` and has no data-directory, WAL or `dirlock`
plumbing. **There is no HTTP route that serves the outbox and this command does not add one** — the
operator principal (`42fad3c`) is offline-only.

**THE BUS MUST BE STOPPED.** It takes the data directory's exclusive `dirlock` (exit `3` otherwise).

**It is STRUCTURALLY read-only, not merely careful:** the `relay.Outbox` is constructed with
`Durable: nil`, so every mutating call on it fails with `relay.ErrOutboxNotDurable`, and the log is
read with `wal.Replay`, which repairs nothing and truncates nothing.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data-dir` | `./data` | The bus's data directory. It must already hold a bus identity; **this command never creates one** (exit `4` otherwise). |
| `-json` | off | Emit **ONE JSON object** with `ok` first — **not** NDJSON (unlike `agent-bus log`). Carries `integrity`, `filter`, `counts`, both per-peer breakdowns, the selected `jobs`, `limits` and `exit_code`. |
| `-peer <bus-id>` | — | Print only jobs owed to this peer bus (exact match). |
| `-state <s>` | — | Print only jobs in this state: `pending`, `delivered` or `abandoned`. Repeatable; an unrecognised value is exit `2`. |

**FILTERS NEVER CHANGE THE EXIT CODE.** The verdict, `counts` and both per-peer breakdowns are
computed over the **WHOLE** outbox *before* `-peer` and `-state` are consulted; a filter changes only
the printed `jobs` list. This mirrors `agent-bus log`'s "filters never suppress damage" rule and
exists for the same reason: a filter that could turn a `6` into a `0` would make this command's
silence meaningless, and its silence is the entire product.

#### Exit codes (`agent-bus outbox`) — CONTRACT

| Code | Meaning |
| --- | --- |
| `0` | Read cleanly; **nothing pending and nothing abandoned** — the outbox is drained. |
| `1` | The write-ahead log is **DAMAGED**: it could not be read, or the replay **threw bytes away**. The question was **NOT ANSWERED** — this must never be read as "drained". |
| `2` | Usage: bad flag, unexpected positional argument, unparseable filter value. Nothing was read. |
| `3` | The data directory is **locked** by a live process, almost certainly the bus. Stop it and retry. |
| `4` | The data directory holds **no bus identity** (`bus-id`), so it is not a bus's data directory. Also covers a `-data-dir` that is absent or is not a directory. |
| `5` | **UNVERIFIABLE:** `wal-mac.key` is absent, so nothing in `bus.wal` can be authenticated. **Nothing was read, nothing was printed, and NO KEY WAS CREATED.** |
| `6` | Read cleanly; **at least one job is PENDING** — the drain is **NOT** complete. Do not restart onto a new binary. |
| `7` | Read cleanly; nothing pending, but **at least one job is ABANDONED** inside the retention window — messages this bus accepted will never reach their peer. |
| `8` | The log was read and **no bytes were lost**, but record indices are **absent** from it, or an outbox record was **REFUSED** as it was applied, so **THE DRAIN IS UNVERIFIED**. This is **NOT** a claim of damage (see below). Treat it as a stop. |
| `9` | The outbox was read and the verdict computed, but the **REPORT could not be written** (closed pipe, failing disk). Nothing is implied about the log. |

**PRECEDENCE: `1` > `8` > `6` > `7` > `0`.** `1` and `8` say whether the answer can be **believed**;
`6`, `7` and `0` say what it **is**, and trust is decided first. **Exit `0` is therefore
STRUCTURALLY UNREACHABLE whenever anything was discarded, refused or missing** — a quiet instrument
must not be able to spell itself the same way as a quiet outbox. `9` is neither: it means the answer
never reached you.

That property is the point of the task. Before it, `wal.Replay` returning `err == nil` for
**record-level** discards meant a damaged log printed `VERDICT: DRAINED … safe to start the new
binary` with **exit 0 and empty stderr** — a gate reporting green over damage, which is the exact
`RELAY-51` shape.

**Why `8` is separate from `1`, and not more `1`s.** A `wal.Discard` with `Length > 0`, or `Severe`,
or `Stage == "framing"` means bytes were actually thrown away → `1`. A discard with `Length == 0` is
a **hole in the index sequence** → `8`, because `internal/wal/replay.go` says a hole may be a record
lost from the media, a record an earlier recovery correctly discarded without renumbering the
survivors, **or an index range BURNED BY A RESERVATION A CRASH NEVER USED** — the ordinary
post-crash signature, since the durable index floor authorises indices in blocks and an authorised
index is never authorised again (invariant 1). `Recovered.MissingRecords` is documented as an
**UPPER BOUND ON LOSS, NOT A COUNT OF IT**. Reporting "damaged" on every bus that ever crashed would
be a channel that cries wolf; reporting "drained" would be worse. `DiscardCount > len(Discarded)`
is also `1`: wal caps retained detail, and a loss nobody can inspect reads as damage.

**Dangling prepares are deliberately NOT a trust signal.** A prepare that reached neither commit nor
abort is the ordinary crash-between-the-two-fsyncs signature, and **nothing about it was ever
acknowledged** (invariant 4), so it does not make the answer doubtful. It is still **reported** on
the trustworthy path, because `wal.Replay` has no logger and this read path would otherwise be
quieter than a normal `wal.Open` start.

#### `--json`: the `integrity` and `filter` blocks

Both are **always present and never null**.

```json
"integrity": {"trustworthy":true,"wal_discards":0,"missing_records":0,"index_gaps":0,
              "outbox_records_refused":0,"dangling_prepares":0,"discarded":[]},
"filter":    {"peer":"","states":[],
              "note":"jobs is filtered; counts, the per-peer breakdowns and exit_code are computed over the whole outbox"}
```

`discarded[]` entries carry `stage`, `index`, `offset`, `length`, `reason`. **`discarded` is CAPPED**
(wal's `maxDiscardsRetained`) while **`wal_discards` is EXACT** — never read `len(discarded)` as the
total. `pending_by_peer`, `abandoned_by_peer`, `jobs`, `limits` and `discarded` are `[]` when empty,
never `null`.

A job object carries **routing and accounting only** — `job_id`, `peer_bus_id`, `origin_message_id`,
`state`, `enqueued_at`, `settled_at`, `reason`, `size`, `content_sha256`. **There is no body,
payload, raw or catch-all field** (invariant 6), and the raw WAL frame is never marshalled.

**Untrusted text is quoted on the human output** (`strconv.Quote`, the house standard from
`internal/logging`): the stored abandonment `reason`, the wal discard `reason`, `-data-dir` and
`-peer`. A stored reason is bounded and UTF-8-validated but **control characters survive**, and a
security gate demonstrated a reason carrying `ESC[2J ESC[H` repainting a **fake `VERDICT: DRAINED`**
over the real one — on the one command whose product is a restart decision.

#### TWO LIMITS THIS COMMAND STATES IN ITS OWN OUTPUT — and they belong here too

1. **It can only ever answer about the LAST 24 HOURS.** Retention is `relay.OutboxSettledRetention`
   ( = `RetryHorizonCeiling` = `idem.PeerOutageBudget` = 24h). Anything older has been swept out of
   the table. The prose in `-h` and in `limits[]` is **derived from the constant**, not typed beside
   it, so it cannot rot the day the constant moves; `retention_window_seconds` reports it exactly.
2. **"NOTHING ABANDONED" DOES NOT MEAN "NOTHING LOST".** When a pending job passes the retry horizon
   the sweep drops it **without a durable tombstone** — it cannot write one, because it runs holding
   a lock this package never holds across a durable write. The only trace is a WARN line in the
   **server's** log at the moment it was dropped. So a message can be lost and leave nothing for this
   command to find. Tracked as **`RELAY-15-FU-SWEEP-TOMBSTONE`** (`da1ba9b7-ab59-476b-831e-4202b1b09ccc`);
   this command references the gap and does not fix it.

Both limits appear in the human output, in `-h`, and in the JSON `limits` array — a caveat only
humans can see is a caveat that gets dropped.

#### It refuses to mint the key that would authenticate the file it is judging

`wal.macKeyFor` **creates `wal-mac.key` as a side effect of a read** whenever the log is non-empty and
does not positively identify itself as format version 2 (garbage magic, or a header too short to
read). A reader without a guard therefore *manufactures the authority to verify the very bytes it is
about to judge* — and every positive test passes either way. This command reuses `auditlog.go`'s
`checkMACKeyPresent` and calls it **before `dirlock.Acquire`** (so a refusal writes nothing at all,
not even `bus.lock`) **and again under the lock** (the pre-lock check races a concurrent delete).
That pre-lock + post-lock pair is **stricter than `agent-bus log`**, whose MAC-key check is post-lock
only. The same guard makes `ids.LoadOrCreateBusID`'s *create* half unreachable, so no bus id is ever
minted by a read (invariants 1 and 2).

---


## `cmd/agent-busctl` — the client

Binary directory `cmd/agent-busctl`; the importable package it shells over is
`github.com/dodgymike/agent-bus/client` (top-level, deliberately **not** under `internal/` — see
"The client package" below).

```
agent-busctl [flags] <command> [flags]
```

Global flags are accepted **before or after** the subcommand, so both `agent-busctl --json enrol …` and
`agent-busctl enrol --json …` work.

### Global flags

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `--bus <url>` | `AGENT_BUS_URL` | *(the selected identity's recorded URL; `enrol` requires it explicitly)* | Base URL of the bus. `https` anywhere; **`http` ONLY to a loopback host** (`127.0.0.1`, `::1`, `localhost`) — see "In flight" below. A path prefix is allowed; **userinfo, query and fragment are rejected**. Canonicalised: host lower-cased, default port dropped, trailing `/` trimmed. |
| `--bus-fingerprint <hex>` | `AGENT_BUS_FINGERPRINT` | *(the selected identity's recorded accept-set)* | (added 2026-08-07, `MTLS-PIN`) SHA-256 of the bus certificate's DER, as **exactly 64 LOWERCASE hex characters** — no `0x`, no colons, no whitespace. **REQUIRED for any `https` bus and REFUSED for an `http` one.** Since `MTLS-ROTATE` (2026-08-07) the stored identity may hold **up to two** accepted certificates; this flag **narrows** the connection to one of them. See "Certificate pinning" below. |
| `--identity <dir>` | `AGENT_BUS_IDENTITY` | `$XDG_CONFIG_HOME/agent-bus` (`os.UserConfigDir()` + `/agent-bus`) | The credential store **DIRECTORY** — not an agent id. |
| `--as <agent-id>` | `AGENT_BUS_AGENT_ID` | *(the stored selection)* | Act as one stored identity for this command only, changing nothing on disk. **Parallel agents sharing a store should use this, not `use`.** |
| `--json` | — | off | Machine-readable JSON on stdout. |
| `--timeout <dur>` | `AGENT_BUS_TIMEOUT` | `30s` | Bounds ONE operation end to end, retries included. Any `time.ParseDuration` value; must be positive. |
| `--persist-session` | `AGENT_BUS_PERSIST_SESSION` | **off** | (added 2026-08-16, `AUTH-9`) Cache the session token in the credential store so the NEXT process reuses it instead of handshaking. **Writes a bearer token to disk.** The env var is a CLOSED set — `1`, `true`, `yes`, `on` (case-insensitive, trimmed) enable it; **anything else, including `0` and `false`, leaves it OFF**. See "Persisted sessions" below. |


### Persisted sessions — `--persist-session`, and `agent-busctl session logout`

*(added 2026-08-16, task `AUTH-9`. This REVERSES the earlier position that a session
is never written to disk; the default is unchanged and still safe.)*

**Default: OFF. Nothing is written.** With the flag set, the session token is written to
`<identity-dir>/session-<fully-qualified-agent-id>.json`, mode **`0600`**, created `O_EXCL` at that
mode — never written then `chmod`ed, so there is no instant at which a bearer token exists under a
looser mode.

**Why it exists.** The bus caps one agent at `DefaultMaxActiveSessionsPerAgent` = **32** concurrent
sessions, holds each for `SessionLifetime` = **1 hour**, and **evicts nothing**. Under invariant 7 an
agent drives the bus by shelling out, and each process is a fresh handshake whose in-memory cache
dies with it — so **every command costs one server-side session for an hour**. Above roughly one
command every two minutes an agent exhausts its OWN cap and is refused `503` on
`/v1/session/complete` for up to an hour, with no self-service recovery. Observed in production
2026-08-15. Measured after the fix, against a store that already held a session: **5 commands with the flag →
0 handshakes; the same 5 without → 5 handshakes.** From a COLD store the first command still
handshakes, so the honest steady-state figure is **one handshake per session lifetime (1 hour)**,
not zero.

**On-disk document** (field names are contract surface):

| Field | Meaning |
| --- | --- |
| `version` | `1`. An unrecognised version is IGNORED, not an error — a session is a cache, so the useful behaviour is a silent re-handshake. |
| `agent_id` | The fully-qualified id this token authenticates. |
| `bus_url` | The bus it was issued by. |
| `token` | **The bearer token. SECRET.** |
| `expires_at`, `refresh_at` | As the bus reported them. |
| `lifetime_seconds` | The issued lifetime, so a reloaded session reports what it was issued with. |

**What is refused on load, each a silent miss:** a mismatched `agent_id` or `bus_url` (presenting a
token to the wrong bus would LEAK it to that bus), an unknown `version`, an empty token, and anything
past its refresh point — the disk path uses **the same** usability rule as the in-memory path, never
a laxer one. A **forced** refresh (after a `401`) deliberately SKIPS the file: it holds the token the
bus just refused, so reading it would turn one `401` into a loop.

**A file readable by other users is IGNORED and WARNED about, and deliberately left in place** —
removing it would destroy the evidence that a bearer token was readable.

**`agent-busctl session logout`** removes this machine's copy. Exit `0` removed, `8` there was
nothing to remove, `3` no usable identity, `2` bad usage. `--json`:
`{"agent_id":…,"ok":true,"removed":true,"server_notified":false}`.

> **`session logout` does NOT free a session slot on the bus.** There is no server-side end-session
> route, so the bus keeps the session and its slot against the per-agent cap until it expires. The
> command reduces exposure of a token at rest and does nothing for the session count.
> `server_notified` reports that honestly and stays `false` until a real route exists — filed as
> `AUTH-7`.


**Resolution order, deterministic:** explicit flag → environment variable → the selected identity's
recorded value (`--bus` and `--bus-fingerprint` only) → built-in default.

**`--bus-fingerprint` is the ONE documented exception to that order**, and it is deliberate. Since
`MTLS-ROTATE` (2026-08-07) the stored identity may accept a **set** of up to two certificates (see
"Certificate pinning" below). When the explicit fingerprint names a certificate **already in that
set**, it **narrows** the connection to that one certificate for this invocation. When it names a
certificate **outside** the set, neither wins: the command fails with exit `2` and names both the
flag's value and the full stored set. Letting the flag win would be the precedence rule and the wrong
answer — "it stopped working so I passed the fingerprint the other end gave me" is exactly how a
substituted certificate gets accepted, and it would turn a detected substitution into a successful
one. Agreement is not a conflict. **The refusal itself is unchanged from `MTLS-PIN`; only the remedy
is new** — confirm the presented value out of band, then `agent-busctl pin add <fingerprint>` to
widen the stored set. The flag itself never widens anything.

`--help` / `-h` / `agent-busctl help <command>` print help and exit `0`.

### The agent's own TLS certificate — `agent-busctl client-cert`

```
agent-busctl client-cert [--identity <dir>] [--json]
```

**Purely local. No HTTP at all.** This command never contacts the bus and does not need a session.
It reads or mints the certificate this identity PRESENTS to the bus through
`client.pinnedTLSConfig`'s `GetClientCertificate` callback: `<identity-dir>/client-tls/cert.pem`
plus `key.pem`, both **0600**, inside a **0700** `client-tls/` directory. The CLI path is a thin
shell over `client.ClientCertificate()` / `LoadOrCreateClientCertificate()`, so an embedder sees the
same material and semantics.

**Idempotent and non-destructive by contract.** The first successful call mints a fresh Ed25519 key
and self-signed certificate; every later call loads the same material and reports `created=false`.
Existing material is NEVER overwritten. A shared identity directory therefore means one shared client
certificate, and a concurrent loser reports the winner's certificate rather than silently replacing
it.

**If the directory is damaged, it refuses instead of "repairing" it.** One file without the other,
garbage PEM, a non-regular file, unreadable material, or a vanished directory entry is exit `3`
(`config`) with a remedy. The missing half is never regenerated in place, because doing so would
change the fingerprint the bus binds while looking like a fix.

**If the certificate is expired, it is REPORTED and still returned.** `expired=true` means the
validity window excludes `time.Now()`, not that the file was refused. Automatic renewal does not
exist yet; the current deliberate replacement is "move the whole `client-tls/` directory aside and
re-enrol".

Human output is the fingerprint first, then the cert path, key path, and validity window. `minted`
appears only when `created=true`; `EXPIRED` appears only when `expired=true`. The key path line ends
with `"(never leaves this machine)"`, which is a contract statement, not decoration.

`--json` emits **ONE object**:

```json
{
  "ok": true,
  "fingerprint": "64-lowercase-hex",
  "cert_path": "/path/to/client-tls/cert.pem",
  "key_path": "/path/to/client-tls/key.pem",
  "not_before": "2026-08-07T12:00:00Z",
  "not_after": "2027-08-07T12:00:00Z",
  "created": true,
  "expired": false
}
```

Field contract:

| Field | Meaning |
| --- | --- |
| `ok` | Always `true` on success. The CLI's success JSON shape leads with it, so a caller can branch on one stable field before reading command-specific payload. |
| `fingerprint` | SHA-256 of the certificate DER, **64 lowercase hex** — the same construction and spelling the bus uses for certificate bindings. |
| `cert_path`, `key_path` | The on-disk files under `<identity-dir>/client-tls/`. |
| `not_before`, `not_after` | RFC3339 UTC validity bounds taken from the parsed leaf certificate. |
| `created` | `true` only when THIS invocation installed the material. Losing a concurrent create race reports `false`. |
| `expired` | `true` when the certificate is outside its validity window at command time. Report-only, not a refusal. |

Exit codes: `0` ok · `2` bad usage · `3` local client-certificate material is missing, unreadable or
damaged.

### Subcommands (as of 2026-08-02)

| Command | Purpose | Network? |
| --- | --- | --- |
| `enrol --name <name>` | Generate an Ed25519 key pair, send only the public half, receive the server-minted `<bus-id>.<agent-id>`, store the credential **and the bus's pinned certificate fingerprint** | yes — `POST /v1/enroll` |
| `whoami [--all] [--verify]` | Show the identity commands act as; `--all` lists them; `--verify` performs a real session handshake | only with `--verify` |
| `use <agent-id\|name>` | Change the stored selection | no |
| `logout [<agent-id>] [--all]` | Delete a credential **locally** | no |
| `leave` | (added 2026-08-22, `AUTH-4`) Tell the bus to durably remove the CURRENT identity from its roster, then delete the credential locally — the server-side counterpart to `logout` | yes — `POST /v1/leave` |
| `pin list \| add <fingerprint> \| bootstrap <fingerprint> \| remove <fingerprint>` | (added 2026-08-07, `MTLS-ROTATE`; `bootstrap` added 2026-08-23, `MTLS-MIGRATE`) List, widen, bootstrap or narrow the bus certificates an identity accepts — see "Certificate pinning" below | `list`/`add`/`remove`: no, local store only. `bootstrap`: yes — session handshake plus `POST /v1/client-cert/bootstrap` |
| `client-cert` | (added 2026-08-07, `MTLS-CLIENTCERT`) Report the TLS certificate this agent PRESENTS to the bus, minting it on first use | **no — purely local**, reads/writes `<identity-dir>/client-tls/` only |
| `agents` | List every agent enrolled on the bus, fully-qualified id first | yes — `GET /v1/agents` |
| `send <to-agent-id> [body]` | Send one direct message, **signed**, durable before it returns (invariant 4) | yes — `POST /v1/mint` **then** `POST /v1/send` (two calls, one idempotency key — see "Signed sends" below) |
| `broadcast [body]` | **BROKEN as of 2026-08-07.** The subcommand is still registered and still builds a request; the bus answers **501** because a broadcast has no canonical audience under signing format v1. Surfaces as **exit 6** — see below. | yes — `POST /v1/mint` then `POST /v1/broadcast`, which refuses |
| `watch` | Long-poll and stream messages addressed to you until stopped | yes — `GET /v1/messages`, `GET /v1/wait` |
| `ack-status <correlation-key>` | (`ACK-9`, added 2026-08-16) Report the sender-visible delivery status of a message you sent — one row per recipient, `--wait <dur>` to park until it settles | yes — `GET /v1/ack/<correlation-key>` |
| `ack <message-id>` | (`ACK-15`, added 2026-08-21) Acknowledge a message you RECEIVED: `delivered` by default, `--refuse <class>` for one of the three recipient classes. Signs the canonical acknowledgement bytes with the agent's **messaging** key. **Nothing else can move a row to `delivered`.** | yes — `POST /v1/ack` |
| `conversation create --recipient <id> [--recipient <id> ...] [--name <label>] [--idempotency-key <key>]` | (`CONV-CREATE-CLI`, added 2026-08-30) Mint a server-tracked, multi-party conversation: the bus mints the id and records the CREATOR as the authenticated caller (invariant 1), never a value the client supplies. `--recipient` is repeatable, 1..64, each a fully-qualified `<bus-id>.<agent-id>` (invariant 2). Idempotent (invariant 10) — see `AGENT_PROTOCOL.md`'s conversations section for the retry contract. | yes — `POST /v1/conversations` |
| `conversation send <conversation-id> [--body <text>] [body] [--file <path>] [--stdin] [--idempotency-key <key>]` | (`CONV-SEND-BY-ID`, added 2026-08-31) Send ONE message to a conversation by its id; the bus resolves the membership server-side, so the client never enumerates participants. Only a MEMBER (creator or a recipient) may send — a non-member and a nonexistent conversation both `404` (leak-less). Body from exactly one of `--body`/positional/`--file`/`--stdin`; at most 64 members; the sender is excluded from its own copy. Two-step signed send under the hood (resolve→sign→send), idempotent (invariant 10). See `AGENT_PROTOCOL.md`'s "Send to a conversation" section. | yes — `POST /v1/conversations/mint` + `POST /v1/conversations/send` |

**There is no `agent-busctl keygen` and no `agent-busctl trust` subcommand.** `cmd/agent-busctl/root.go`
registers **fifteen** commands: the fourteen rows above (eleven before `ack-status`, `ACK-9`,
2026-08-16; twelve before `ack`, `ACK-15`, 2026-08-21; thirteen after `leave`, `AUTH-4`, 2026-08-22;
fourteen after `conversation`, `CONV-CREATE-CLI`, 2026-08-30) plus `session`, which has its own
section. The `conversation send` row (`CONV-SEND-BY-ID`, 2026-08-31) adds a SUBCOMMAND, not a new
command — `conversation` is one registered command with two subcommands (`create`, `send`), so the
count is unchanged. **The count above previously said the
table WAS the registry, which it never was** — corrected 2026-08-21 (`ACK-15` reviewer,
minor/pre-existing). What matters is the claim the paragraph is actually making: `keygen` and `trust`
are in NEITHER list. This matters because several error remedies in
`client/store.go`, `client/client.go` and `client/keyring.go` tell the operator to "run
`agent-busctl keygen`" or to add a key with `agent-busctl trust` — **those commands do not exist**. The
capabilities exist only as Go API (`Client.MessagingPublicKey()`, `Client.TrustPeer()`,
`Client.TrustedKeys()`), so today they are reachable by an agent EMBEDDING the client and not by one
shelling out. Recorded as an open item, not as a satisfied requirement; see `CONTRACTS-AGENT.md`.

### Certificate pinning — CONTRACT (`MTLS-PIN`, 2026-08-07; rotation amended by `MTLS-ROTATE`, 2026-08-07)

Bus certificates are **self-signed**, there is **no certificate authority**, and there is **no
trust-on-first-use** (invariant 11). The fingerprint is therefore the only thing that says which bus
is on the other end, and the client must be told it **before** its first connection.

| Situation | Behaviour | Kind / exit |
| --- | --- | --- |
| `https` bus, fingerprint known (flag, env, or a member of the stored accept-set) | The bus's leaf certificate is verified against the pin on every handshake — matching **any** accepted member succeeds, which is what makes a rollover survivable | — |
| `https` bus, **no** fingerprint anywhere | **REFUSED before any connection is made.** No TOFU: nothing is sent, nothing is remembered | `config` / **3** |
| `http` bus **with** a fingerprint | **REFUSED.** A plaintext connection has no certificate, so the pin cannot be checked, and silently ignoring it would fake a check that never ran | `usage` / **2** |
| Fingerprint present but malformed | Refused at client construction, before any I/O | `usage` / **2** |
| `--bus-fingerprint` names a certificate **outside** the stored accept-set | Refused; the error names the flag's value and the full stored set | `usage` / **2** |
| Bus presents a certificate that is not **any** member of the accept-set | **Hard failure. Never retried.** The error names the accepted set and the presented fingerprint and the remedy | `network` / **5** |
| Stored accept-set holds more than `MaxBusPins` (2) certificates (only reachable by hand-editing the store) | Refused **at connect time**, not at load — `pin list`/`pin remove` still work to repair it | `config` / **3** |

- **The pin is `sha256(cert.Raw)`** — the DER of the **leaf**, exactly as it arrived on the wire.
  Only `rawCerts[0]` is considered. This is the same construction the server uses
  (`internal/buscert.Fingerprint`), mirrored in `client/pin.go` because the client package may not
  import `internal/` (invariant 7). Divergence would fail closed: no pin would ever match.
- **Textual form: 64 lowercase hex characters, and nothing else.** Uppercase is rejected rather than
  folded, and the colon-separated spelling other tools print is rejected. One value, one spelling.
- **`enrol` records the fingerprint as the first (and usually only) member of the identity's
  accept-set** (`bus_fingerprints` in `identities.json` — see "The accept-set is a bounded SET, since
  `MTLS-ROTATE`" below), so it is supplied **once**, from the invite, and every later command against
  that bus verifies without being told again — "the trusted path must be the easy path". A member of
  the set is **never** derived from a certificate the bus presented, because that would be TOFU
  wearing the costume of a pin. The stored set is used only for the bus URL it was stored against.
- **There is no flag, environment variable or `Config` field that disables verification**, silently
  or otherwise, and three tests in `client/guard_test.go` enforce that structurally (see "The client
  package" below). Nothing added by `MTLS-ROTATE` changes this.

#### The accept-set is a bounded SET, since `MTLS-ROTATE` (2026-08-07)

An identity no longer pins exactly one certificate — it accepts a **set**, bounded at
`client.MaxBusPins` = **2**: the outgoing certificate and the incoming one, which is exactly the width
of the two-certificate rollover `DECISIONS.md` E3 describes.

- **Membership is granted, never learned.** A fingerprint enters the set only by an explicit operator
  act — the invite's fingerprint at `enrol`, or `agent-busctl pin add` with a value confirmed **out of
  band**. Nothing ever adds the certificate a bus happened to present during a handshake; that would
  be trust-on-first-use, which invariant 11 rules out by name.
- **`pin add` at the cap is REFUSED, never evicting the oldest** — eviction would silently decide,
  on the operator's behalf, which certificate stops being trusted.
- **`pin remove` of the LAST pin is REFUSED.** An `https` identity with an empty accept-set cannot
  connect at all, so removing the last one would be a lockout dressed as a tidy-up.
  `agent-busctl logout <agent-id>` is the command that means "stop using this identity".
- A hand-edited store holding more than `MaxBusPins` is refused **at connect time, not at load**, so
  `pin list` and `pin remove` still work to fix it.
- **`--bus-fingerprint` / `AGENT_BUS_FINGERPRINT` still only NARROWS**: it may name a certificate
  already in the stored set (the connection then accepts only that one), and a fingerprint **outside**
  the set is still a hard refusal, not a precedence question. Widening is only ever `pin add`.

#### `agent-busctl pin` — list / add / bootstrap / remove accepted certificates

```
agent-busctl pin list
agent-busctl pin add <fingerprint>
agent-busctl --bus https://127.0.0.1:18090 pin bootstrap <fingerprint>
agent-busctl pin remove <fingerprint>
```

`pin list`, `pin add` and `pin remove` are **purely LOCAL** — nothing is sent to the bus. They read and write the credential store (`client.Store.AddBusPin` / `Store.RemoveBusPin`, via `Client.AddBusPin` / `Client.RemoveBusPin`). `pin bootstrap` is different: it uses the supplied fingerprint as an in-memory trust anchor, authenticates over pinned TLS, signs the bootstrap intent with the enrolled AUTH key, calls `POST /v1/client-cert/bootstrap`, and only then writes the explicit TLS URL and first pin locally. It takes no flags of its own beyond the globals (`--bus`, `--as`, `--identity`, `--json`).

- `pin list` — prints the current accept-set; makes no change.
- `pin add <fingerprint>` — confirm the value **out of band** first (the bus's
  `bus_cert_fingerprint=…` startup log line, or the invite), then widen the accept-set by one.
  Re-adding a fingerprint already held succeeds as a no-op — the obvious retry after an interrupted
  rollover. Refused at `MaxBusPins`, and refused on an identity enrolled against an `http` bus (a
  plaintext connection presents no certificate, so a pin there would be a check that never runs). The
  gate is the **scheme, not an empty accept-set**: an `https` identity that holds *no* pin — which a
  downgrade can produce, see below — may gain one, and `pin add` is its recovery rather than a full
  re-enrolment.
- `pin bootstrap <fingerprint>` — for a pre-TLS identity whose stored bus URL is `http://` and whose
  roster entry has no live certificate binding. Requires global `--bus https://...`; the fingerprint
  is still operator-supplied and parsed before any network I/O. The server call authenticates with
  the existing auth key/session, requires a fresh AUTH-key signature over the session token,
  idempotency key and TLS-derived client certificate fingerprint, derives the agent id from that
  bearer principal, derives the client certificate fingerprint from TLS, and appends the first
  binding on the existing roster entry. Only after that 200 does the local store change the existing
  identity's `bus_url` to the HTTPS URL and write the first pin; it never creates a new credential.
  No invite is presented or spent.
- `pin remove <fingerprint>` — retire one certificate. Refused if it is the last one held, and refused
  (not a silent no-op) if the fingerprint given is not currently held, so a mistyped value cannot be
  reported as success while the real one stays accepted.

All four forms answer with the identity's **full resulting accept-set**, never a diff, so a script
driving a rollover reads the resulting state instead of reconstructing it:

```json
{"agent_id":"bus-abc.planner-1","bus_url":"https://127.0.0.1:8443",
 "bus_fingerprints":["<old-64-hex>","<new-64-hex>"],"max_bus_fingerprints":2}
```

`pin bootstrap` adds `client_cert_fingerprint`, `bound_at`, `already_bound` and `idempotency_key` to that JSON object. A true retry with the same idempotency key and same certificate replays the original response (for a first binding, `already_bound:false`) and carries `Idempotency-Replayed: true` on the HTTP hop. `already_bound:true` means the bus already held the same live certificate binding for this agent under a different or older successful attempt, so this call appended nothing.

`bus_fingerprints` is **always present and never null** (an empty accept-set prints `[]`), and
`max_bus_fingerprints` is the cap (`client.MaxBusPins`), so a caller can tell "one slot free" from
"add will be refused" without hard-coding the number.

Exit codes: `0` ok; `2` `usage` — unknown subcommand, wrong argument count, a malformed fingerprint,
`pin add` at the cap, `pin remove` of the last pin, or `pin bootstrap` without an explicit `https:// --bus`; `3` `config` — no identity enrolled or selected. `pin bootstrap` can also return the normal network/auth/server/version exit codes because it performs a session handshake and an authenticated server write.

**Recovery from a genuine rotation**, in order:

1. Confirm the new fingerprint **out of band** — the bus logs `bus_cert_fingerprint=…` at startup.
2. `agent-busctl pin add <new>` — the identity now accepts both the outgoing and the incoming
   certificate.
3. Once the bus has stopped serving the old certificate, `agent-busctl pin remove <old>` to narrow
   back to one.

`agent-busctl logout <agent-id>` followed by re-enrolling with `--bus-fingerprint <new>` remains the
heavier alternative, and is still the right answer when the goal is to drop the OLD certificate
outright rather than accept it alongside the new one for a window. (Enrolling straight over an
existing identity without the `logout` is refused — the stored identity still pins the old
certificate.)

- **Certificate expiry on the CLIENT side IS checked, since `MTLS-EXPIRY` (2026-08-07, commit
  `9f2878a`).** A matching fingerprint is now NECESSARY BUT NOT SUFFICIENT:
  `client/pin.go`'s `verifyPinnedBusCertificate` runs the **identity** check first (the leaf's
  `sha256(DER)` must be a member of the accept-set) and then `checkBusCertificateValidity`, so a bus
  certificate outside its validity window is REFUSED even when the pin matches. The order is
  deliberate — a certificate that is both unpinned *and* expired is reported as UNPINNED, because the
  substitution is what an operator must see first.

  The mechanism, since it is contract:
  - The verdict comes from `x509.Certificate.Verify` and nowhere else (invariant 9: no hand-rolled
    date arithmetic). The leaf is added to a **fresh `x509.NewCertPool()` as its own root** — the
    stdlib's supported way to say "trust is already established, apply the remaining checks", and
    specifically a fresh pool rather than nil/system, which on darwin and windows would hand off to
    the platform verifier and change the code path per OS.
  - No chain is built and **the self-signature is not verified here**. It does not need to be: the pin
    covers the whole DER, and the handshake separately proves the peer holds the matching private
    key. `checkBusCertificateValidity` authenticates nothing on its own — an attacker's in-date
    self-signed certificate passes it cleanly — which is why it is only ever reached after the
    fingerprint comparison.
  - `KeyUsages` is `ExtKeyUsageAny` (no EKU filtering) and `DNSName` is empty (there is no hostname
    verification in this design; the pin replaces it).
  - Failure surfaces as `*BusCertificateExpiredError`, carrying `Fingerprint`, `NotBefore`, `NotAfter`
    and the time judged against, and unwrapping to the sentinel `ErrBusCertificateExpired`. Every
    other x509 verdict fails closed as `ErrBusCertificateUnusable`, as does being handed the zero time
    instead of a clock.
  - **There is no client-side clock-skew allowance, on purpose.** `internal/buscert` backdates
    `NotBefore` by five minutes when it MINTS a certificate; a second, invisible allowance here would
    extend every certificate's usable life beyond the `NotAfter` it states.
  - **The clock is read per HANDSHAKE, not per request.** `pinVerifier` takes `now func() time.Time`
    and calls it on each handshake, so a long-lived transport cannot go on approving a certificate
    that expired hours ago — but an established connection is reused *without* a new handshake, so the
    real bound is how long the pooled connection survives, which for an agent continuously
    long-polling `/v1/wait` can be a long time. TLS session resumption is disabled (no
    `ClientSessionCache`), which is what keeps `VerifyPeerCertificate` running on every new
    connection; adding a cache without also moving these checks into `VerifyConnection` would
    silently disable both of them, with every positive test still passing.

  **This bullet has now been wrong in BOTH directions, and both corrections are recorded rather than
  quietly reverted** — the pattern is the point:
  1. A 2026-08-07 revision claimed the check "has been in place since `MTLS-PIN` … commit `61e6067`".
     It was not: that revision was reading `MTLS-EXPIRY`'s *uncommitted* work in the same worktree and
     documenting it as shipped. `61e6067` genuinely does not contain it — the proof below is RED
     against that commit, which is what makes the proof worth anything.
  2. The bullet that replaced it said expiry is "NOT checked yet" and that `MTLS-EXPIRY` is "in
     flight, not in `main`", citing a proof that `git show HEAD:client/pin.go` matched no `NotAfter`,
     `ErrBusCertificateExpired` or `ParseCertificate`. That was true when written and became FALSE the
     same day, when `9f2878a` landed — at which point the paragraph's own stored proof matched all
     three and disproved the paragraph. **A claim anchored on a moving `HEAD` rots silently**; cite a
     commit.

  Proof — RED at `61e6067`, GREEN at `9f2878a`. Read with `git show`, never from the worktree, which
  is exactly how correction 1 happened:

  ```
  for s in 'var ErrBusCertificateExpired = errors.New(' \
           'return checkBusCertificateValidity(rawCerts[0], at)' \
           'leaf, err := x509.ParseCertificate(der)' \
           'CurrentTime: at,' \
           'invalid.Reason == x509.Expired'; do
    git show 9f2878a:client/pin.go | grep -qF -- "$s" || echo "MISSING: $s"
  done
  ```

  What `MTLS-VERIFY` (2026-08-07) separately added is the **server-side operator** check, which is a
  different surface and does not substitute for the client one: `agent-bus healthcheck` performs a
  real x509 verification against the bus's own certificate, so it enforces the validity period *for
  the operator's probe*. That makes an expired certificate show up as an unhealthy container. It does
  nothing for an agent connecting from elsewhere.
- **`client.Config.HTTPClient` bypasses all of this — both halves.** An embedder that supplies its
  own transport bypasses the fingerprint check *and* the refusal to speak `https` to a bus with no
  pin at all, because `Client.doer` returns the supplied transport before it consults
  `transportSecurity`. That is the correct trade for an embedder who owns its own verification, and
  the exported `BusFingerprint` / `ParseBusFingerprint` exist so such an embedder can reuse this
  construction rather than invent a second one. **Nothing in this package or the CLI ever sets it**,
  and no flag or env var reaches it.
- **Split with `MTLS-LISTENER`, corrected 2026-08-07:** `MTLS-ROTATE` is the **client** half only —
  the accept-set, `pin add`/`pin remove`, and matching any set member during the handshake are
  implemented and enforced by `client/`. `MTLS-LISTENER` has now landed and the bus **does** serve
  `https` — see above — but it serves exactly ONE certificate, loaded once at startup
  (`internal/buscert`, which still has "no rotation machinery yet" per its own doc comment). A bus
  actually **serving two certificates through a rollover window** is a separate, still-unbuilt server
  capability that this repo has not yet named a task for. So `agent-busctl pin add`/`pin remove` are
  now exercised against a REAL, TLS-serving bus (single-certificate case), but the two-certificate
  rollover half of the rotation story described above has still never been exercised end to end.

### Signed sends: `agent-busctl send` makes TWO calls (SIGN-2/SIGN-6, 2026-08-07)

`client.Send` reserves, signs, then sends. The whole two-step is invisible from the command line —
the flags, the positional body and the JSON output shape are all unchanged — but it is visible in a
packet capture and in the bus's logs, so it is documented rather than hidden:

1. `POST /v1/mint` → `{message_id, seq, sender, op, expires_at}`.
2. The client canonicalizes and signs with its **MESSAGING** key (see "Credential storage" below).
3. `POST /v1/send` carrying the reservation, `timestamp_ms` and the base64 signature.

**Both calls use the SAME idempotency key**, and that is what makes the two-step retryable: a
reservation is scoped by `(agent, op, key)`, so repeating step 1 with the same key returns the SAME
id and sequence rather than burning a second one. A client that crashes between the two steps repeats
both under the same key and converges on ONE message. Minting a fresh key on the retry would produce
a second reservation and, if the first send had landed, a second message.

**A 409 on step 3 is not always a conflict.** After a bus restart the reservation table (memory-only
by design) is empty, so `/v1/send` answers 409 `ErrUnknownMint`. That is ROUTINE, and the correct
response is to re-mint under the same key, re-sign and re-send — not to mint a new key. Note the
client's generic remedy text for a 409 currently says "an idempotency key was reused with different
content; use a fresh key for new content", which is **wrong advice for this case**
(`client/transport.go`'s `statusError` has no `ErrUnknownMint` branch). Reported, not fixed here.

**`agent-busctl broadcast` exits 6, not 7.** `client/transport.go` maps any status `>= 500` that is not
429/503 to `KindServer`, and 501 falls there, so a refused broadcast is reported as "the bus reported
an internal error" with the bus's own explanation appended. It is **not retried** (`isRetryable`
retries `KindServer` only on 429/503), so there is no retry loop — but the exit code and wording do
not say "this route is deliberately unimplemented", and an agent branching on exit codes will read a
deliberate refusal as a server fault. Recorded as a known rough edge.

`enrol` flags: `--name` (required), `--invite-file <path>` (redeem an operator-minted invite; `-`
reads it from stdin — see "Invite redemption" below), `--idempotency-key` (resume an earlier
attempt), `--keep-current` (do not switch the selection). **`--invite` (the old, never-working flag
that took the blob as a value) is REMOVED** (`INVITE-CLIENT`, 2026-08-14), not merely reserved — see
below.

`agents` flags: none beyond the globals.

`send`/`broadcast` flags: `--file <path>` (`-` means stdin), `--stdin`, `--idempotency-key <key>`
(retry a specific earlier send/broadcast — see "Send/broadcast idempotency" below). The body itself
is a **positional argument** — `send`'s second (after `<to-agent-id>`), `broadcast`'s first — not a
flag. Both commands **permute flags and positionals** (`parseWithPositionals` in
`cmd/agent-busctl/send.go`) so `agent-busctl send <to> --json` parses as intended: Go's plain
`flag.FlagSet.Parse` stops at the first non-flag argument and hands everything after it back as
positionals, so before that helper existed `agent-busctl send <to> --json` read `"--json"` itself as the
message body and **delivered it as the message**, silently. Any future command that adds a
positional needs the same helper, or it will reproduce that exact bug.

`watch` flags: `--replay`, `--cursor <c>`, `--limit N`, `--poll-timeout <dur>`, `--count N`,
`--for <dur>`, `--no-cursor` — see "`watch`: output modes and the cursor contract" below.

`ack-status` flags: `--wait <dur>` (park on the bus until every row is terminal, or this duration
elapses — ceiling `client.MaxPollTimeout`, 5 minutes / 300s, refused locally if exceeded, never
clamped; `0`/omitted answers immediately with a snapshot). The correlation key is a **positional**
argument, and the flag may appear before or after it — `ack-status` parses in passes for exactly this
reason (`cmd/agent-busctl/ackstatus.go`), the same accommodation `send`'s `parseWithPositionals` makes
above. A negative `--wait` is refused locally (exit `2`), as is a correlation key containing
whitespace or more than one positional argument. At most **32** `--wait` calls may be parked
per agent at once (`maxParkedAckStatusPerAgent`, `internal/httpapi/ackstatus.go` — the ack-status
twin of `hub.MaxWaitersPerAgent`); the bus refuses the 33rd with `429` + `Retry-After: 1`. That is
a **transient** capacity failure, so `client.retryable` retries it automatically (3 attempts,
honouring `Retry-After`); only if every attempt is refused does it surface, as **exit `6`**
(`KindServer`) — **not** exit `7`, because being at capacity is not the bus refusing the request on
its merits. No new exit code is minted.

`ack` flags: `--refuse <class>` (refuse the message instead of accepting it; one of
`recipient_refused_policy`, `recipient_refused_undecodable`, `recipient_refused_not_addressed`, and
**only** those three). OMITTED, the outcome is `delivered` — that is the ONLY default. **`--refuse`
PRESENT WITH AN EMPTY VALUE IS EXIT `2`, NOT THE DEFAULT** (`ACK-15` reviewer finding C1): `ack "$ID"
--refuse "$CLASS"` with `$CLASS` unset is the idiom an agent shelling out writes, and defaulting it
to `delivered` would assert receipt for a caller that was trying to refuse — a terminal outcome is
ABSORBING and can never be revisited. `cmd/agent-busctl/ack.go` distinguishes absent from
present-but-empty with `fs.Visit`. There is deliberately **no `--outcome` flag**, which is what makes
`undeliverable` unspellable through the CLI: `undeliverable` is not a class at all but a terminal
OUTCOME a BUS asserts (`ACK-CONTRACT.md` §8.1), carrying one of the nine bus-emitted routing classes
to say why, and a recipient has no standing to assert either (§5.2, §6.3). `--refuse undeliverable`
is refused locally with its own message, exit `2`, and nothing is signed or sent — as is any of the
nine bus-emitted classes. The message id is a **positional** argument and the flags
may appear before or after it (`parseWithPositionals`, `cmd/agent-busctl/send.go`). A key that is
not a well-formed message id, one containing whitespace, one over `client.MaxCorrelationKeyLen`
(512), and any argument count other than one are all refused locally (exit `2`) before a request is
built. The frame carries **no idempotency key** (`ACK-CONTRACT.md` §4): an ACK's idempotency is the
durable `(correlation key, recipient)` row and the absorbing-terminal rule over it, so the request is
retryable by construction — every attempt asserts the same outcome for the same pair, which the bus
answers as `duplicate` rather than as a conflict.

**`logout` is LOCAL ONLY, and stays that way — `leave` is the server-side counterpart.** `logout`
does not tell the bus: the enrolment stays on the roster and any live session lives out its hour. The
JSON field `server_notified` reports this honestly and is `false` on every `logout`, always. **As of
`AUTH-4` (2026-08-22), `POST /v1/leave` exists and `agent-busctl leave` durably removes the identity
from the bus AND deletes the local credential — its `server_notified` is `true`.** Use `logout` to
stop this machine from acting as an identity while leaving the enrolment standing; use `leave` when
the identity itself is done for good. See the `leave` row in the subcommands table above and
`AGENT_PROTOCOL.md`.

### Exit codes — CONTRACT

An agent branches on these, so a value never changes meaning and a retired value is never reused.
They are produced by `client.ExitCode(err)` in the importable package, so an embedder gets the same
codes without copying a switch.

| Code | Kind | Meaning |
| --- | --- | --- |
| `0` | — | Success |
| `1` | `internal` | Unclassified/internal failure |
| `2` | `usage` | Malformed invocation: bad flag, missing `--name`, unknown subcommand, the removed `--invite` flag |
| `3` | `config` | Local identity/config not ready: nothing enrolled, no selection, unreadable or damaged store, or (`INVITE-CLIENT`, 2026-08-14) an `enrol --invite-file` that cannot be used — missing, wrong permissions, not a regular file, malformed JSON, or larger than `client.MaxInviteFileBytes` (64 KiB) |
| `4` | `auth` | The bus rejected the credential (401/403), or the signature did not verify, or (`INVITE-CLIENT`, 2026-08-14) refused an invite presented to `enrol --invite-file` (403, `"kind":"auth"` — single-use/expiring/revocable, and the bus deliberately does not say which; retrying does not help) |
| `5` | `network` | The bus could not be reached: refused, DNS, timeout, or a certificate that does not verify |
| `6` | `server` | The bus reported a failure of its own (5xx), or a capacity refusal that survived retries |
| `7` | `rejected` | The bus understood the request and refused it (400/404/409/413/415/422) |
| `8` | `empty` | Succeeded with **nothing to report** (e.g. `whoami --all` on an empty store) |
| `9` | `version_skew` | A `404` on a fixed route this client depends on: the bus does not know the route at all, so it is **older than this client** (`client.ExitVersionSkew`, `client.KindVersionSkew`). Deliberately NOT `7` — that is the bus understanding the request and refusing it, which is the opposite claim |

`2` is usage rather than `1` to match Go's `flag` package and `cmd/agent-bus`.

**`9` was reachable and documented NOWHERE until `INVITE-CLIENT-FU-EXIT9` (2026-08-14).** It is
produced by the single `KindVersionSkew` assignment in `client/transport.go`, and the subcommands
that can return it are `enrol` (`POST /v1/enroll`), `agents` (`GET /v1/agents`), `watch`
(`POST /v1/wait`, `GET /v1/messages`), `send` (`POST /v1/mint` — the id reservation it signs),
`broadcast` (`POST /v1/broadcast`) and — **added `AUTH-4`, 2026-08-22** — `leave`
(`POST /v1/leave`, a fixed non-session call, so a bus too old to serve it 404s and that 404 is version
skew like every other fixed route); each of their `--help` EXIT CODES tables now carries the row, and
`TestEveryVersionSkewCommandDocumentsExitNine` (`cmd/agent-busctl/cli_test.go`,
`versionSkewCommands`) fails if one drops it. **Deliberately absent: `send`'s own `/v1/send` 404**,
which is a per-resource "unknown recipient" carved out to `KindRejected`/`7`; and `whoami`, whose only
remote calls are the session routes, where `annotateSessionError` overrides a 404 to `KindAuth`/`4`.

No code changes meaning; some commands give one a more specific sense:

- `8` — `agents` on an empty roster, and a **bounded** `watch` (`--count`/`--for`) that delivered
  nothing before it finished. An unbounded `watch` stopped by a signal is always `0`, however many
  messages it saw.
- `7` — a 409 idempotency-key conflict on `send`/`broadcast` (same key, different payload — the bus
  **rejects and logs it and does NOT disconnect**; invariant 10, narrowed 2026-08-08), and an unknown
  recipient on `send`.
- `6` — a fatal 503 (the bus's write path cannot durably accept messages, signalled by **no**
  `Retry-After` header) is `KindServer`, so it is exit **6**, not `5`. `5` stays reserved for the bus
  being unreachable at all — refused, DNS, timeout, TLS. See "The 503 split" below. **Also `6`
  (2026-08-07): `agent-busctl broadcast`, because the bus's deliberate `501` falls into the generic
  `>= 500` branch.** Not retried, but not distinguishable from a real server fault by exit code alone
  — read the message text, which carries the bus's own explanation.
- `6` — **and, since `CLI-3-FU-HASHVERIFY` (2026-08-08), `watch` on a BODY-INTEGRITY failure: a
  message whose body disagrees with the `size` or `content_sha256` the bus sent beside it.** The
  whole batch fails, nothing reaches the caller, and the watch **STOPS — it does not retry.** Read
  the distinction precisely, because the two halves are separately load-bearing: the **cursor is left
  exactly where it was**, so nothing is skipped and a later run re-reads that position; but this run
  does not re-read it, because the bus is deterministic about what lives at a position and no number
  of attempts turns a body that disagrees with its digest into one that agrees.

  **This exit code REPLACES a previous exit `8`, and that is the point of the task.** Before the fix
  the error was an ordinary `KindServer`, which both the transport retry loop and `watchShouldRetry`
  treat as transient, so `watch` re-read the same cursor, got the same damaged message, looped until
  `--for` expired and exited **8** — "a bounded watch delivered nothing" — while messages were in
  fact arriving damaged. A caller branching on `8` was being told the opposite of the truth.

  What the check proves and does not: it proves the bytes and the metadata beside them AGREE, which
  catches corruption in transit, a proxy that mangled a body, and a bus internally inconsistent with
  itself. It is **not authenticity** — the BUS computes that hash, so a bus that wants to lie hashes
  the body it invented. Sender authenticity is the signature and nothing else. An **absent** field is
  not verified: `size` `0` or an omitted `content_sha256` is an older bus, and refusing to read from
  it would turn version skew into an outage for a check that is not an authenticity control anyway.

- `7`/`8` — **`ack-status` (`ACK-9`, added 2026-08-16) reuses both codes, and mints no new one**
  (`ACK-CONTRACT.md` §13.4). `--wait` that settles on `refused` or `undeliverable` is `7`
  (`ExitRejected`); `--wait` that ends with the state still `unknown` is `8` (`ExitEmpty`). Without
  `--wait`, EVERY state — `unknown` included — is a successful snapshot and exits `0`; a `--wait` that
  ends still `accepted`/`in_flight` is also `0` (nothing failed, the answer is "not yet"). The same
  reported state therefore exits differently depending on whether `--wait` was given: without it the
  caller asked for a snapshot and got one, with it the caller asked to be told the outcome, so the
  outcome becomes the exit status. The row data (and its `class`) is always printed **before** the
  exit code is decided, including under `--json`, so a script branching on `7` still has the one
  field — `class` — it needs.

- `7`/`8` — **`ack` (`ACK-15`, added 2026-08-21) reuses both codes too, and mints no new one**
  (`ACK-CONTRACT.md` §13.4). `7` (`ExitRejected`) is a `409`: the `(message, recipient)` pair is
  already terminal with a DIFFERENT outcome, the first terminal stands and NOTHING was written —
  the client replaces the transport's generic 409 remedy, which talks about idempotency keys this
  frame does not carry. `8` (`ExitEmpty`) is the uniform `accepted:false, state:"unknown"` answer,
  byte-identical for a message that never existed, one swept past retention, one this agent was not
  addressed in, and a malformed id (§13.3) — it is NOT an error and NOT a permission failure. A
  duplicate of the SAME outcome is exit `0` (invariant 10's legitimate retry). The result object is
  printed BEFORE the exit code is decided, including under `--json`.

- `7` — **`conversation create` (`CONV-CREATE-CLI`, added 2026-08-30) mints NO new code.** A `409`
  (the idempotency key was already used for a DIFFERENT recipient list or name) is `ExitRejected`;
  the bus does not disconnect (invariant 10), so the failure is ordinary and retryable under a fresh
  key. A retry under the SAME key with the SAME payload is a replayed success and exits `0` — the
  output says `replayed` and prints the ORIGINAL conversation, nothing is minted twice. There is no
  `8`: the route always either mints or replays, never returns an empty result.

### JSON shapes — CONTRACT

**Success** — exactly ONE JSON object on stdout, keys sorted, plus `"ok": true`:

```json
{"agent_id":"bus-abc.planner-1","bus_id":"bus-abc","bus_url":"https://127.0.0.1:8080",
 "enrolled_at":"2026-08-02T22:10:24.217971827Z","name":"planner","ok":true,
 "public_key":"KouoAWExNgv14Dh4sg/h/AnDXw/tn583vbvCCyO01Rs=","replayed":false,
 "store_path":"/home/u/.config/agent-bus/identities.json","stored":true}
```

| Command | Fields |
| --- | --- |
| `enrol` | `agent_id`, `bus_id`, `name`, `bus_url`, `bus_fingerprints` (array, **`omitempty`** — present only when at least one certificate was pinned, i.e. never for a plaintext loopback bus), `public_key`, `enrolled_at`, `replayed`, `idempotency_key`, `stored`, `store_path`, `invite_id` (**`omitempty`**, added `INVITE-CLIENT` 2026-08-14 — present only when `--invite-file` redeemed an invite; the invite's **id**, never its secret) |
| `whoami` | the identity fields above, plus `is_current` (bool), and `session` (`agent_id`, `expires_at`, `refresh_at`, `lifetime_seconds`) with `--verify` |
| `whoami --all` | `identities` (array), `current_agent_id` (string), and `pending` (array of `idempotency_key`/`name`/`bus_url`/`invite_id` (**`omitempty`**, added `INVITE-CLIENT-FU-PENDINGINVITE` 2026-08-14 — the invite's **id**, never its secret; absent when the attempt presented no invite AND when the record predates the field)/`created_at`) when any enrolment is unfinished |
| `use` | the identity fields, plus `is_current` (bool) |
| `logout` | `removed` (array of agent ids), `current_agent_id` (string), `server_notified` |
| `leave` | (added 2026-08-22, `AUTH-4`) `agent_id`, `server_notified` (always `true` — the opposite of `logout`'s), `already_left`, `sessions_dropped`, `locally_removed` (array of agent ids), `current_agent_id` |
| `pin` (`list`/`add`/`remove`) | `agent_id`, `bus_url`, `bus_fingerprints` (array, **never null** — an empty accept-set prints `[]`), `max_bus_fingerprints` (int, `client.MaxBusPins`). See "Certificate pinning" above. |
| `pin bootstrap` | Same fields as `pin`, plus `client_cert_fingerprint`, `bound_at`, `already_bound`, `idempotency_key`. |
| `agents` | `agents` (array of `agent_id`/`bus_id`/`name`/`enrolled_at`), `count`, `ok` |
| `send`, `broadcast` | `message_id`, `seq`, `from`, `broadcast`, `to`, `sent_at`, `content_sha256`, `replayed`, `idempotency_key`, `ok` |

**BREAKING JSON CHANGE (`MTLS-ROTATE`, 2026-08-07): `Identity` no longer carries `bus_fingerprint`
(a single string).** It now carries `bus_fingerprints` — an array of strings, `omitempty` — and that
is the shape everywhere an `Identity` is emitted: `enrol`, `whoami`, `whoami --all`, `use`, and the new
`pin`. A consumer reading `.bus_fingerprint` must move to `.bus_fingerprints[]` (e.g. `.bus_fingerprints[0]`
for "the one I enrolled with", or iterate the array to check every accepted certificate). This surface
shipped the same day as `bus_fingerprint` itself (`MTLS-PIN`, commit `61e6067`), and **no bus served
TLS yet at that point in the day** (`MTLS-LISTENER` landed later the same day), so there was no
deployed consumer of the old field — which is the justification for changing the shape outright rather
than keeping both.

**`is_current` is a bool; `current_agent_id` is a string.** They are deliberately different keys: one
name that is a bool in one subcommand and a string in another makes `jq .current` unpredictable.

**Failure** — one JSON object on **stdout** in `--json` mode (so a consumer parses one stream), or
two human lines on **stderr** otherwise:

```json
{"ok":false,"error":"enrol: cannot reach the bus at http://127.0.0.1:8080","kind":"network",
 "remedy":"check --bus / AGENT_BUS_URL and that the bus is running","exit_code":5}
```

`status` is added when the failure carried an HTTP status. `idempotency_key` (`omitempty`) is added
when the failed operation was a mutating one that had already minted a key — `send`/`broadcast` — and
is **omitted** when the failure never had one (a local usage error caught before a key existed, e.g.
a missing recipient). It matters because a network error or a 5xx on a send is genuinely ambiguous —
the message may or may not have been applied — and the key is the only handle that makes a later
retry the SAME logical send rather than a second message (invariant 10). In human mode the same key
is named on stderr alongside the `--idempotency-key` flag that resumes it.

**NDJSON — the streaming convention, now landed with `watch`.** A streaming subcommand writes **one
compact JSON object per line, flushed as it arrives**, with **no envelope, no `ok` field and no array
brackets**, so a consumer can act on each record incrementally instead of buffering to completion.
Diagnostics never go to stdout.

### `watch`: output modes and the NDJSON record shape

`watch` picks its output form for you, from stdout alone — there is no flag that forces the human
feed:

| Condition | Output |
| --- | --- |
| `--json` | NDJSON |
| no `--json`, stdout is **not** a terminal (a pipe or redirect) | NDJSON — a pipe is a machine |
| no `--json`, stdout **is** a terminal | a readable live feed, one line (or indented block) per message |

One NDJSON record per message, field by field:

| Field | Meaning |
| --- | --- |
| `message_id` | the server-minted id, the key to deduplicate on. **This bus's** id for the copy it handed you — for a relayed message it is NOT the id `ack` takes; see `correlation_key` |
| `correlation_key` | (added 2026-08-22) the **ORIGIN** bus's server-minted id — `ACK-CONTRACT.md` §3's correlation key, and the only id `agent-busctl ack` accepts. **Equal to `message_id` when this bus is the origin, DIFFERENT when the message was relayed in**, which is why acknowledging with `message_id` passes every same-bus test and then exits `8` `unknown` the first time a message crosses a relay. Computed by the BUS (`store.Message.OriginID()`), carried verbatim, never re-derived client-side; bus-namespaced (invariant 2) and not reconstructible from `bus_path[0]`. **Deduplicate on `message_id`; acknowledge with this.** Always present — see below |
| `seq` | the server-minted sequence — a unique, never-reused IDENTITY. Monotone in **allocation** order, NOT in delivery order: it is minted when a client *reserves*, so a message with a lower `seq` can arrive after one with a higher `seq`. Do not order, deduplicate or discard on it; key on `message_id`. |
| `from` | the fully-qualified sender id |
| `broadcast` | whether this went to every agent except the sender |
| `to` | the recipient list — one entry for a direct message, empty for a broadcast |
| `bus_path` | bus ids traversed, oldest first |
| `sent_at` | the **bus's** timestamp, verbatim. **NOT covered by the signature** |
| `size` | body length in bytes, as the bus recorded it |
| `content_sha256` | hex SHA-256 of the decoded body |
| `timestamp_ms` | (added 2026-08-07) `int64` Unix milliseconds UTC — the **SENDER's** clock, and the one that **IS** covered by the signature |
| `signature` | (added 2026-08-07) the sender's detached Ed25519 signature, standard base64 of 64 bytes |
| `body` | the decoded body |
| `text` | the body as a string, present only under the conditions below |

**`sent_at` and `timestamp_ms` are different facts and are both on the stream on purpose.** Verifying
a signature against `sent_at` fails every time. The signed bytes are reconstructed from
`message_id`, `seq`, `from`, `to`, `timestamp_ms` and `body`; `bus_path` is deliberately not covered.

`correlation_key` is **always present and never `omitempty`**, so `jq -r .correlation_key` can never
print `null` against a current bus. That is the point of it: were it omitted on a same-bus message
every consumer would write `.correlation_key // .message_id`, re-spelling the bus's own origin/local
branch in shell — and in `jq` the empty string is truthy while `null` is not, so that idiom would
fall through to the **wrong** id silently instead of failing loudly. Against a bus old enough to
predate the field the CLI still emits the key, with the empty string as its value; treat an empty
`correlation_key` as "this stream cannot tell me what to acknowledge with", never as a key to hand
to `ack`.

In the **human** feed (no `--json`, stdout a terminal) it is printed on its own line after the body,
`  ack key: <value>`, and **only when it differs from `message_id`** — that is, only for a relayed
message, the one case where the reader could not otherwise name the id `ack` wants. For a same-bus
message the two strings are equal and the line would be noise on every message. Like every other
bus-supplied string on that render it goes through `client.TerminalSafe` with `keepNewlines=false`
first, so a line break inside an id cannot forge a second line of output.

`body` is **always present**, standard base64 — the authoritative, lossless form, true for any bytes
at all. `text` is present **only** when the body is valid UTF-8, free of control characters other
than tab/newline/CR, and free of the Unicode bidi and zero-width characters that can reorder or hide
what a terminal renders (`isBidiOrInvisible` in `cmd/agent-busctl/watch.go` — the same forgery class as an
ANSI escape, spelled in Unicode). It is **omitted, never rewritten**, otherwise: a lossily-rewritten
body would be worse than no field at all, since a consumer would have no way to tell what it read is
not what was sent.

So: `jq -r .text` for text traffic, `jq -r .body | base64 -d` for anything (binary or otherwise
disqualified). Running diagnostics — retry notices, cursor-store warnings, the closing summary — go
to stderr and never appear inside the stream. The one exception: under `--json`, the FINAL failure
object (including the exit-8 "nothing arrived" outcome of a bounded watch) is emitted as the last
line of the stream on stdout, in the same shape every other subcommand's failure uses — branch on the
presence of an `"ok"` field, which a failure object always has and a message record never does.

### `watch`: the cursor contract

This is the load-bearing part of `watch`, and it applies whether the output is human or NDJSON:

- the read position (the "cursor") is **persisted by default**, per (identity, bus), in the
  credential store directory;
- the cursor **advances only after a whole batch has been handed to the caller** — poll, hand every
  message in the batch to the caller, only then adopt and (if persisting) write the new cursor. A
  process killed mid-batch **re-delivers that whole batch** on the next run; it never advances past
  messages the caller was never given, and it never skips;
- delivery is **at-least-once**: duplicates are the normal steady state (a cyclic relay topology with
  at-least-once forwarding guarantees them, not just a crash), and a handler must be **idempotent on
  `message_id`**;
- a poll that times out with nothing is a `200` and a **normal** outcome, not an error — on a quiet
  bus it is the steady state;
- `--no-cursor` does **not persist** anything (a throwaway tail), but it still **starts from the
  stored position** — the run's own `--help` says so plainly, and this doc does not soften it: the
  next (persisting) run resumes wherever the stored cursor already was, unaffected by the throwaway
  run;
- `--replay` and `--cursor <c>` are **both start positions**; giving both is a usage error (exit `2`)
  rather than one silently winning over the other — the same "refuse an ambiguous instruction rather
  than guess" rule `send`/`broadcast` apply to a body given twice.

### Credential storage

| Path | Mode | Contents |
| --- | --- | --- |
| `<identity-dir>/` | `0700` | The store. Tightened on open if it already exists looser. |
| `<identity-dir>/identities.json` | `0600` | Format version `1` (**not bumped by `MTLS-ROTATE`** — see below). Enrolled identities **including TWO Ed25519 private-key seeds each** (`private_key_seed` and, since 2026-08-07, `messaging_key_seed`), the pinned `bus_fingerprints` **array** (up to `MaxBusPins` = 2, `omitempty`; a single-pin `MTLS-PIN` store is migrated on load — see below), the current selection, and in-flight (`pending`) enrolments. |
| `<identity-dir>/identities.lock` | `0600` | Exclusive lock for read-modify-write; treated as abandoned after 30s. |
| `<identity-dir>/trusted-keys/` | `0700` | (added 2026-08-07) The local trust store — `client.TrustedKeysDirName`. One `0600` file per peer, **named `<fully-qualified-agent-id>.pub`**, holding the standard base64 of that peer's 32-byte Ed25519 **messaging** public key. Deliberately the dullest format that works: one key, one file, no index, so an operator can inspect/add/remove with `cat`/`cp`/`rm` during an incident and a damaged file costs trust in one peer rather than all. A file over `4 KiB` is refused unread. The `0600`/`0700` modes protect **INTEGRITY, not secrecy** — these are public keys; whoever can write this directory decides whose signatures this agent accepts. |
| `<identity-dir>/cursors.json` | `0600` | Format version `1` — **unchanged, and deliberately so, even though every cursor it stores changed meaning on 2026-08-14** (`SIGN-1-FU-REORDER-WATERMARK`): the cursor is an OPAQUE server token, so the file's own schema is untouched and an older `agent-busctl` reading this file does not throw it away. The token inside moved from a sequence to a delivery position and its internal version moved `v1` → `v2`, so **every cursor written by an older build is remapped by the bus to the start of the retained window and replays once** — accepted and remapped, never rejected, because a rejected cursor would never be cleared and would be re-presented for ever. One `watch` read position per (`agent_id`, `bus_id`) pair — **`bus_id`, the server-minted one, NOT the bus URL, since `CLI-3-FU-URLKEY` (2026-08-08)**; see below. No key material. Capped at 256 records, and 512 bytes per stored cursor, so a bus cannot grow the file without bound. |
| `<identity-dir>/cursors.lock` | `0600` | A **separate** exclusive lock from `identities.lock` — a cursor advances far more often than a credential changes, and sharing one lock would put `watch` in needless contention with `enrol`/`use`/`logout`. |

**The `identities.json` accept-set field is migrated, one-way, on load (`MTLS-ROTATE`, 2026-08-07).**
A store written by the single-pin `MTLS-PIN` build carries a scalar `bus_fingerprint` per identity;
`(*Store).load` folds that into the new `bus_fingerprints` array the first time the file is read, and
the store format version (`1`) is **deliberately not bumped** for this — an additive field migration,
same reasoning as `messaging_key_seed`. The migration only happens **in memory**: nothing is written
back until something else writes the store for another reason, but from that point on the legacy
`bus_fingerprint` key is gone and only `bus_fingerprints` remains. This is intentionally **one-way**:
a **downgrade** to a single-pin binary reading a store that now only has `bus_fingerprints` sees **no**
pin for that identity and therefore **refuses** to speak `https` to the bus (`transportSecurity`)
rather than connecting unverified. That fail-closed direction — a downgrade loses a pin and stops,
never loses a pin and proceeds — is what makes the one-way migration acceptable.

A downgrade that **writes** the store has a second consequence, stated here because stating only the
read half reads as reassurance: the older binary re-marshals the credentials without a
`bus_fingerprints` field it does not know, and the accept-set is **permanently lost from the file**.
Trust is not silently downgraded — the identity is simply unpinned and `https` is refused — but it is
unrecoverable from disk, so the operator must re-pin. That is exactly why `pin add` is gated on the
URL **scheme** rather than on the set being empty: the recovery is `agent-busctl pin add <hex>`, not a
re-enrolment.

**`cursors.json` fails OPEN, unlike `identities.json`.** A damaged file, one that fails to parse, or
one written by an unknown format version is **not fatal**: it is ignored, a warning is printed to
stderr, and `watch` replays from the start of the retained window — the same outcome as an agent that
had simply never watched before. This is the deliberate opposite of `identities.json`, which refuses
outright on an unknown version (see `(*Store).load`): a credential misread is unrecoverable and
dangerous (a private key misparsed as public fails silently), so refusing to guess is the only safe
move there, while a cursor is a **position hint, not a credential** — losing it re-delivers messages,
which at-least-once delivery already permits and which a correct handler already tolerates by
deduplicating on `message_id`. Refusing to run because a position hint was damaged would trade a
harmless replay for an outage.

Written by atomic replace: an `O_EXCL` `0600` temp file in the same directory, fsynced, renamed, then
the directory fsynced. Abandoned temp files are swept on the next write — each is a complete copy of
every private key. **Never inside the repository.**

The lock carries an ownership token (pid + 16 random bytes); a stale break removes only the exact
file it observed as stale, and a release removes only a lock that is still its own. Without that,
two processes breaking one abandoned lock could both believe they held it and one whole-file
update — one private key — would be lost.

A store directory or credential file found at looser permissions is TIGHTENED **and a warning is
printed to stderr**: a key file that was ever readable by another local user must be assumed
compromised, and silently fixing the mode destroys the only evidence.

**Session tokens are never written to disk.** They are bearer credentials with at most an hour of
life that do not survive a bus restart, so persisting them would trade a stealable token at rest for
two saved round trips. Each `agent-busctl` process performs its own handshake.

### The cursor key is the BUS ID, not the bus URL (`CLI-3-FU-URLKEY`, 2026-08-08)

A `cursors.json` record is keyed on (`agent_id`, `bus_id`) — the **server-minted** bus id (invariants
1 and 2), read from the `<bus-id>.` prefix of the agent's own fully-qualified id. It was keyed on
`bus_url`, **scheme included**, until this change.

**This is a fix, not a preference.** A real agent migrated across a plaintext → TLS switch and ended
up with two records for one agent id:

```
{agent_id: …mic-array-1, bus_url: http://127.0.0.1:18080,  cursor: …|266}
{agent_id: …mic-array-1, bus_url: https://127.0.0.1:18080, cursor: …|266}
```

The `https` record started empty, so the first `watch` after the flip re-received the agent's entire
history. At-least-once delivery permits that and `message_id` dedup absorbs it; the SCOPE is what
made it worth fixing. It fires for **every agent on the bus at once**, the moment TLS is required,
and any handler that acts per-message rather than deduplicating re-acts on its whole history
simultaneously. A URL is not an identity for a bus — one bus is reachable at `http` and `https`
during a migration, and also across a port move, a DNS change or a reverse proxy appearing.

`bus_url` is deliberately **not** retained as a second field. A non-key field that looks like a key
is how this happened once.

#### BREAKING FOR EMBEDDERS — same Go signature, different meaning

`Store.Cursor`, `Store.SetCursor` and `Store.ClearCursor` all keep the signature they had. Their
**second parameter changed meaning**, from the bus URL to the bus id:

```go
// before CLI-3-FU-URLKEY
c, err := st.Cursor(agentID, "https://127.0.0.1:8080")  // bus URL
// after
c, err := st.Cursor(agentID, "bus-abc")                 // server-minted bus id
```

Both are `string`, so **an embedder that keeps passing a URL recompiles cleanly and silently
mis-keys every cursor**: the lookup matches nothing, `Cursor` returns `""` (it does not error on a
miss — see below), and the caller replays from the start of the retained window while `SetCursor`
writes records under a key nothing will ever read back. That is the worst shape a breaking change
takes, so it is called out here rather than left to a release note. The fix is to derive the id the
same way the client does — the prefix of `Credential.AgentID` up to the first `.` (invariant 2) — and
**not** from `Credential.BusURL`. The empty string is refused: all three return a `KindInternal`
error when either key is empty, so a caller that has no bus id gets an error rather than a wrong
answer.

`Credential.BusID` is used as a **cross-check**, not as the source. The client derives the bus id
from the agent id (checked against the bus-id grammar) and refuses when `Credential.BusID` is present
and disagrees, rather than picking a winner: the two disagreeing means the stored identity is
self-contradictory about whose sequence space the cursor belongs to.

#### Migration — REQUIRED READING even now the key is fixed

Existing installs already have the split records; the fix stops new ones appearing, it does not
retroactively unsplit what a flip already wrote. Three properties, all contract:

- **The format version stays `1`, deliberately.** No bump. The bus id is recoverable from the record
  itself (the `agent_id` prefix), so nothing is lost and nothing needs guessing — and `loadCursors`
  **refuses an unknown version outright**, so bumping would make an older binary discard the whole
  file and *guarantee* the full replay this change exists to prevent.
- **Migration happens on READ, in memory** (`migrateCursorRecords`), so a read never takes the write
  lock. The migrated shape reaches disk the next time anything calls `SetCursor`/`ClearCursor`, which
  on an active watch is every batch.
- **Mixing builds is safe in both directions.** An older build reading a new file finds no `bus_url`,
  matches nothing and replays. An older build *writing* strips `bus_id` from every record — but
  `agent_id`, which the id was derived FROM, is untouched, so the next new-build load re-derives them
  all and collapses whatever duplicates the old build appended. The file degrades to the old shape
  and is losslessly re-migrated; it never loses a position.

What the one-time migration does to the split pair, and both are ANNOUNCED on stderr rather than
done silently:

| Case | Outcome |
| --- | --- |
| Two records collapse onto one key (the `http`/`https` pair) | The **most recently updated** wins — a cursor is opaque, so the timestamp is the only ordering available, and choosing the newest replays at most the gap between the two rather than the whole history. A warning names the count and says it may replay the messages in between. |
| A record whose `agent_id` carries no bus prefix | **Dropped** — it can never be keyed or matched, and would otherwise hold one of the 256 slots for ever. A warning names the count. |

#### `enrol` now writes `cursors.json`

`Store.PromotePending` — the last step of a successful `enrol` — calls `ClearCursor` for the new
credential's (agent id, bus id), **unconditionally**, whether or not it replaced an existing
identity. Two consequences worth stating:

- `enrol` now touches a **second** file and takes a **second** lock (`cursors.lock`, after the
  identities lock is released — never nested).
- The direction of the risk is the opposite of the obvious guess. Re-keying removed the accidental
  separation the URL key gave, so an id enrolled by a *new* holder could inherit the *previous*
  holder's read position — and the bus's cursor is not bus-scoped, so a position from elsewhere is
  accepted and only later messages are returned. That is a **SKIP** — silent loss — where every other
  failure mode in this client is a replay. It is unconditional because `logout` removes a credential
  without touching `cursors.json`, so a logout-then-enrol lands on the append path with the old
  position still sitting under the identical key. A genuinely fresh identity has no record, so the
  clear is a no-op.
- A **failed** clear is a warning, not a failure: the credential is already durable, so failing the
  enrolment would report a success as a failure. The warning names `--replay` as the remedy, which is
  the one thing that reliably steps around a position that is too far ahead.

### The MESSAGING keypair — a second key, distinct from the AUTH key (added 2026-08-07)

An identity now holds **two** Ed25519 keypairs, and they are not interchangeable:

| Key | Store field | Proves | Minted |
| --- | --- | --- | --- |
| **AUTH** | `private_key_seed` | this agent **to the bus** — it signs `agent-bus:session-token:v1:<challenge>` at `POST /v1/session/complete` (invariant 3) | at `enrol`, before the request is sent |
| **MESSAGING** | `messaging_key_seed` | this agent **to its PEERS** — it signs the canonical bytes of every outgoing message | **at `enrol`, before the request is sent** (`RELAY-13`, 2026-08-08) — minted into the `pending` record alongside the AUTH key and promoted with it. **Legacy only:** a credential enrolled before `RELAY-13`, or resumed from a `pending` record written before the field existed, has no seed and still mints one **on first use**, lazily, under the store lock (`Store.EnsureMessagingKey`) |

Both private halves live in the same `0600` `identities.json` inside the `0700` store directory, and
**neither ever leaves the machine**. `Credential.String()` redacts both.

Splitting them is invariant 3's separation of concerns, not bookkeeping: the bus must be able to
authenticate an agent without being able to speak as it. **Both public keys are now registered with
the bus at enrolment** (`RELAY-13`, 2026-08-08) — only the private halves stay local — and the
remaining gap is that nothing serves the MESSAGING one back.

**Which field goes where.** The seed and the wire field are different values and are easy to
conflate:

| Name | Where it lives | What it is |
| --- | --- | --- |
| `messaging_key_seed` | `identities.json`, on the **`pending`** record (`pendingEnrolment.MessagingKeySeed`) | the PRIVATE 32-byte seed, minted before `/v1/enroll` is called so an interrupted enrolment cannot lose it |
| `messaging_key_seed` | `identities.json`, on the **promoted credential** (`Credential.MessagingKeySeed`) | the same seed, carried across when the pending record becomes a credential |
| `messaging_public_key` | the **wire**, in the `POST /v1/enroll` request body (client side `client.enrolRequestBody`, unexported; server side `httpapi.EnrolRequestBody`) | the base64 PUBLIC half, **derived** from the seed above via `Credential.MessagingPublicKey()` — never a second, independently generated value |

**A resumed pre-`RELAY-13` `pending` record sends NO messaging key**, deliberately: minting one at
that point would re-present the original idempotency key with different content, which the bus
answers with `409` (invariant 10), turning the retry of an interrupted enrolment into a permanent
failure. Such an identity keeps a locally-minted messaging key its bus cannot attest.

**KNOWN GAP — a messaging public key can be published to the bus, but not FETCHED from it.**
Enrolment registers it (`auth.RosterEntry.MessagingPublicKey`, durable as `msg_pub`), but no route
serves it to anyone: `GET /v1/agents` carries no key material, and CRYPTO-4 (the server-attested key
bundle) does not exist. `trusted-keys/` is therefore a **manually populated stopgap**: a peer's key reaches it out of
band, by a human or a deployment system. There is deliberately **no TOFU, no "trust the key the bus
handed over", no verification-optional switch and no `--insecure`** — each would let a bus that can
choose the verification key forge any message from any sender, which is the exact property the
messaging key exists to deny it.

**Verification is NOT yet performed on receive.** Signing works end to end and the signature is
carried on the wire and returned by the read path, but `client.Read` does not verify: it decodes the
batch and returns it. `Batch.Rejected` is declared and documented but nothing populates it, and the
doc comment on `Batch.Messages` — "the VERIFIED messages" — is **FALSE today**. A recipient that
wants verification must do it itself, against a key it obtained out of band; that path is proven to
work (a client-made signature verifies under `internal/signing.Verify` from the wire fields). Do not
read the presence of `RejectionReason`, `RejectedMessage` or `KeyRing` as evidence that the read path
enforces anything yet.

### Invite redemption: `enrol --invite-file` (added 2026-08-14, `INVITE-CLIENT`)

`agent-busctl enrol` now redeems an operator-minted invite. Source: `cmd/agent-busctl/enrol.go`
(`loadInvite`), `client/invite.go` (`Invite`, `LoadInviteFile`, `ParseInvite`, `Validate`,
`MaxInviteFileBytes`), `client/enrol.go` (`EnrolOptions.Invite`, `EnrolResult.InviteID`).

```bash
agent-busctl enrol --invite-file invite.json --name planner
```

`--invite-file -` reads the blob from **stdin**; this is REFUSED (`KindUsage`, exit `2`) when stdin
is a terminal — invariant 7, never an interactive prompt.

**`--invite <blob>` is REMOVED, not deprecated.** It never worked — it always failed at exit `2` — so
nothing depends on it, and it is gone rather than kept accepting a value, because the value it would
have carried is a bearer credential: argv is world-readable via `/proc/*/cmdline` and is recorded in
shell history. **There is deliberately no flag of any name that takes the invite or its secret as a
value.**

**File requirements, checked against the OPEN file (`f.Stat`), not the path — no window between the
check and the read:**

- must be a regular file;
- must carry no group or world permission bit (any bit in `0o077` is refused, exit `3`, naming the
  exact `chmod 0600 <path>` to run) — the client refuses rather than silently repairing a file it does
  not own;
- at most `client.MaxInviteFileBytes` (64 KiB) — two orders of magnitude of headroom over a real
  blob (a few hundred bytes), refused rather than truncated;
- decodes as exactly one JSON object; content after it is refused (`ParseInvite`), because a file
  holding two concatenated blobs is ambiguous about which one is redeemed;
- unknown/extra JSON keys are **ignored on purpose** (`json.Decoder` without
  `DisallowUnknownFields`) — forward compatibility with `agent-bus invite mint -json`, which already
  emits operator-facing keys (`ok`, `created_at`, `label`, `transport_insecure`) this client does not
  need and may add more.

Any of the above failing is `KindConfig`, exit `3`. Malformed invite JSON is reported by offset and
type only — **no error from this path ever quotes the file's content**, because the content is a
bearer credential and errors are printed to terminals, piped into logs and pasted into tickets.

**The blob supplies the bus address AND the certificate fingerprint, so `--bus` and
`--bus-fingerprint` are unnecessary with `--invite-file`.** This is invariant 11: the invite is the
trust anchor, and there is no trust-on-first-use. A `--bus` or `--bus-fingerprint` that **disagrees**
with the invite is refused (`KindUsage`, exit `2`) rather than resolved by precedence — one of the two
is wrong about which bus this is, and the client will not silently prefer either. A value that
**agrees** is accepted as merely redundant.

**The bus refusing the invite** (already-redeemed, expired, revoked, or simply unknown — the bus
deliberately answers all four with the same terse `403`, so a caller cannot use the response to probe
which invites exist) surfaces as `KindAuth`, exit `4`. Retrying does not help; the remedy is a fresh
invite from the operator (`agent-bus invite mint`).

**Enrolling with NO invite at all still works, unchanged.** The bus's `httpapi.DiscoveryEnrolment.InviteRequired`
is deliberately `false` today (invariant 3 says that must eventually flip; it has not). Do not
document or build on invite-only enrolment as live.

**Output.** `--json` gains `invite_id` (`omitempty` — present only when an invite was redeemed); human
output gains an `invite <id>` line. The invite **id** is a name, safe to log and to quote in a ticket.
The invite **secret** is the credential and appears in **no** output, error or log line anywhere in
this path — not in `EnrolResult`, not in an `*Error`, not in the on-disk `pending` enrolment record,
and not in `Invite.String()`/`GoString()`, both of which redact it even under `%#v`.

**The `pending` record DOES carry the invite ID** since `INVITE-CLIENT-FU-PENDINGINVITE` (2026-08-14):
`pendingEnrolment.InviteID` (`json:"invite_id,omitempty"`), one optional additive field in
`identities.json`. The ID ONLY — the secret is not a field of that struct at all, which is a stronger
guarantee than redacting it would be. **Correcting the earlier wording here, which was wrong in both
directions:** it called the record "not carrying the invite at all" a KNOWN GAP, in a sentence whose
subject was the SECRET — but the secret's absence was never a gap, it is the guarantee, and the
record already carried the invite's *address* as `bus_url` on this path (`Enrol` resolves it via
`inviteEndpoint`). The real gap was the missing **id**, and that is what is now closed; see
"Enrolment idempotency" below for what the id is for.

### Enrolment idempotency (invariant 10)

The key pair is written to the store as a `pending` record **before** `/v1/enroll` is called, so a
process killed after the bus minted an id does not lose the private key. Records are scoped to
**(idempotency key, bus URL)** — the same scoping the server uses — so:

- re-running an enrolment with the same `--idempotency-key` and name is answered **from the store**,
  with `"replayed": true` and **no HTTP request**;
- the same key with a different name on the same bus is refused **locally**, exit `2`, because the
  bus's answer to that is a 409 it rejects and logs (invariant 10, narrowed 2026-08-08 — it does
  **not** disconnect), and the round trip would spend a redemption attempt against a single-use
  invite while teaching the caller nothing the local refusal does not;
- the same key with a **different invite** — or with none — is refused **locally** too, exit `2`
  (`inviteConflict`, `KindUsage`; `INVITE-CLIENT-FU-PENDINGINVITE`, 2026-08-14). **Nothing is sent and
  the stored key material is KEPT** — see the paragraph below, which is the point of the change.
  For a DIFFERENT invite the server would agree: `POST /v1/enroll`'s invited path fingerprints
  `(name, public_key, messaging_public_key, invite_id)` (`idem.ComputeFingerprint`,
  `internal/httpapi/auth.go`) and answers `409` on a mismatch, rejecting and logging it with the
  connection explicitly KEPT. For **no invite at all** the request would take the *un-invited* path,
  which never computes that fingerprint — so the bus's answer there is not the same 409, and this
  document does not claim to know which answer it is. The client refuses it either way: the stored
  attempt asserted an invite and this one does not, so it is not the same payload;
- the same key against a different bus is a fresh enrolment;
- a network failure keeps the record and the error names the exact `--idempotency-key <key>` that
  resumes it; `whoami --all` lists every unfinished enrolment with the command that resumes it — and,
  for an attempt that redeemed an invite, a `redeeming invite <id>` line plus a resume line built
  around `--invite-file` rather than `--bus` (the invite carries the address and the pin), so a
  process killed before it printed anything still leaves a recoverable identity;
- pending records are pruned 24h after creation, on the next store write, and are destroyed
  outright by `logout --all`.

**When a failed enrolment KEEPS its key material, and when it drops it** (`client.enrolFailed`,
tightened by `INVITE-CLIENT-FU-PENDINGINVITE`). A `KindNetwork`/`KindServer` failure has always kept
the record. It now also keeps it whenever the attempt was **RESUMED** (the seeds belong to an earlier
request that may already have reached the bus) and whenever the bus answered **409** even on a fresh
attempt (a 409 says a request under this key arrived — an in-call transport retry can achieve that
with no pending record having pre-existed). Both errors' remedies say so, naming the
`--idempotency-key` that reaches the material. What still drops: a fresh attempt refused on the
merits — a 400, a 403 on an invite, an unknown route. This matters because the record is the ONLY
copy of the attempt's two private key seeds, and the bus may already hold their public halves; the
pre-fix path routed the 409 through `KindRejected` and dropped them, making a minted identity
permanently unrecoverable.

**Reading an `invite_id` back is deliberately lenient in ONE direction.** A stored id that
*disagrees* with the presented one is refused; an *absent* stored id never is. Empty is ambiguous —
a genuinely un-invited attempt, or a record written by a build older than this field — and refusing
it would strand exactly the interrupted enrolment the record exists to rescue.

The claim decision — already applied / resume / start new — is made in ONE locked read-modify-write.
Two concurrent enrolments under one key would otherwise both generate a key pair, and one private key
would be lost while both sent conflicting payloads under the same key.

### Send/broadcast idempotency (invariant 10)

The idempotency key for `send`/`broadcast` is minted **once per invocation** — before the payload is
marshalled — and reused across every internal transport retry, so a send retried inside `agent-busctl` can
never become two messages. Omit `--idempotency-key` and one is minted for you; it is always printed
back (human output and `--json`'s `idempotency_key` field), because it is the only handle that makes
a *later* retry the same logical send rather than a second message.

- **Same key + byte-identical body** is a legitimate retry. The bus answers from its applied-key
  table, re-applies nothing, and returns the ORIGINAL result — `"replayed": true`, exit `0`.
- **Same key + different content** is a protocol violation. The bus answers `409`, **rejecting and
  logging it — it does NOT drop the connection** (invariant 10, narrowed 2026-08-08), so anything
  else in flight on that connection survives. Surfaced as its own loud `KindRejected` error, exit
  `7`. Retrying will not help; use a fresh key for new content.
- A key is remembered only as long as the message it produced is **retained** (1 day, or until 1 GiB
  of messages push it out). A "retry" that arrives after that produces a **second message** rather
  than being rejected — a key is a retry handle for minutes and hours, not for days.

### In flight — what will change

- ~~**`--invite` is RESERVED and currently rejected** (exit `2`) rather than guessed.~~ **PARTLY
  CLOSED by `INVITE-CLIENT` (2026-08-14):** `--invite` (the flag that took the blob as a value) is now
  REMOVED outright rather than reserved, and `enrol --invite-file <path>` redeems an invite for real —
  see "Invite redemption" above. The wire shape this bullet was waiting on (`invite_id`/`invite_secret`
  on `POST /v1/enroll`, `httpapi.EnrolRequestBody`) is the settled `ENROL-SHAPE` shape and is what this
  path sends. **Still open:** enrolment is not invite-only — `httpapi.DiscoveryEnrolment.InviteRequired`
  is still `false` (invariant 3 says that must eventually change) — and `AUTH-1-FU-POPKEY` (proof of
  possession of the AUTH key) has not landed; do not read this bullet's closure as either of those
  being done.
- **TLS** (invariant 11): **the bus now serves `https`, and only `https`** (`MTLS-LISTENER`, landed
  2026-08-07 — see the top of this file). `http://` is still accepted by the client, unchanged, but
  **only to a loopback host**; that allowance is a client-side affordance for local testing, not a
  restriction forced by an unlanded listener — it has **not** been deleted from `client/` by
  `MTLS-LISTENER` landing, and doing so (if it happens) is a separate follow-up, not automatic
  fallout of the server change. Plaintext to anything else is refused, because `/v1/session/begin`
  returns the session token — a bearer credential — in a response body. Redirects are never followed,
  because Go's default policy would forward the `Authorization` header across an `https`→`http`
  downgrade on the same port.
  **The client-side half of pinning is IMPLEMENTED AND ENFORCED BY THE CLIENT** (`MTLS-PIN`,
  2026-08-07, **rotation support added by `MTLS-ROTATE`, same day**) — it shipped deliberately ahead of
  the listener, because a fingerprint nobody checks defends nothing, and it is what a real TLS-serving
  bus is now verified against. **Corrected 2026-08-07:** a bus now DOES serve TLS
  (`MTLS-LISTENER`), so the earlier claim here — "no bus serves TLS yet, nothing exercises this end to
  end" — no longer holds for the basic single-certificate case: `agent-busctl enrol
  --bus-fingerprint` and the rest of the "Certificate pinning" behaviour above can now be run against a
  real, TLS-serving `agent-bus` started via `scripts/bus-serve.sh` (see `CONTRACTS-AGENT.md`). What is
  **still** unverified end to end is the **two-certificate rollover** specifically: the client can
  accept a set of two and `pin add`/`pin remove` manage it (`MTLS-ROTATE`, done), but the bus itself
  still serves exactly ONE certificate — `internal/buscert` has "no rotation machinery yet" — so no
  running deployment has ever presented a client with a second, incoming certificate to roll onto. The
  **client certificate** half of mutual TLS belongs in **`client.pinnedTLSConfig`** — *not* in
  `newHTTPClient`'s unpinned fallback literal, which the pinned branch replaces wholesale, so a client
  certificate put there would be silently dropped on every pinned (i.e. every real) connection.
  **Updated 2026-08-14 (`MTLS-CLIENTAUTH`).** This bullet previously said "`tls.Config.Certificates` is
  unset today"; that is **stale** — `client/pin.go` sets `GetClientCertificate` (deliberately not
  `Certificates`, which `crypto/tls` filters against the server's acceptable-CA list) in
  `pinnedTLSConfig`, fed with real material via `client/client.go`. Combined with the bus's `ClientAuth`
  now being `tls.RequestClientCert`, a client that HAS certificate material presents it and the bus can
  see it. Requested, never required, so a client with none is unaffected. See the client-certificate
  policy paragraph at the top of this file.
- ~~**The transport is built before the identity is resolved**~~ — **fixed by `MTLS-PIN`**
  (2026-08-07). The transport is now built **lazily**, on the first request, once the bus URL and its
  pin have been resolved together (`Client.endpoint` → `Client.doer`), and is rebuilt when the pin
  changes. `enrol`, `use` and `logout` drop it, so no connection verified under one identity's pin is
  reused under another's. The remaining half no longer stands: `MTLS-CLIENTCERT` gave the per-identity
  **client certificate** its home in `client/clientcert.go`, exposed it through
  `Client.ClientCertificate()`, and wired it into `client.pinnedTLSConfig`'s
  `GetClientCertificate` callback. `agent-busctl client-cert` is the local inspection surface.

### The client package (`github.com/dodgymike/agent-bus/client`)

Top-level, **not** under `internal/`: invariant 7's third audience is an agent that EMBEDS the
client, and Go forbids another module from importing an `internal/` path, so the requirement would be
silently foreclosed by a directory name. Its exported surface is a public API subject to
compatibility care, and it must **not** import anything under `internal/` — mechanically enforced by
CLI-1's proof clause `! go list -deps ./cmd/agent-busctl | grep -q 'agent-bus/internal/'`.

Constants shared with the server are **pinned literals** with a comment naming the server-side
definition they mirror (`client.SessionSigningContext`, `client.AgentNamePattern`, the route paths).
Divergence fails closed — a signature simply does not verify.

Exported surface as of 2026-08-02:

| Symbol | Purpose |
| --- | --- |
| `Config`, `DefaultConfig`, `Config.ApplyEnv`, `DefaultIdentityDir`, `RetryPolicy`, `HTTPDoer` | Configuration and the transport escape hatch |
| `EnvBusURL`, `EnvIdentityDir`, `EnvAgentID`, `EnvTimeout`, `EnvBusFingerprint` | The env var names above |
| `BusFingerprint`, `BusFingerprintSize`, `ParseBusFingerprint`, `BusFingerprintError`, `ErrBusFingerprintMismatch`, `ErrBusPresentedNoCertificate`, `Config.BusFingerprint` | (2026-08-07, `MTLS-PIN`) One certificate fingerprint. `BusFingerprint` is a comparable `[32]byte`, a **pinned mirror** of `internal/buscert.Fingerprint` under the same no-`internal/`-import rule as `SessionSigningContext`. `errors.Is(err, ErrBusFingerprintMismatch)` is how an embedder branches on "this is not a certificate I accept" without parsing a message; `BusFingerprintError` carries both the accepted set and the presented fingerprint. There is **no** exported (or unexported) way to turn the check off. |
| `BusPinSet`, `NewBusPinSet`, `ParseBusPinSet`, `MaxBusPins`, `Identity.BusFingerprints`, `Client.AddBusPin`, `Client.RemoveBusPin`, `Client.BootstrapClientCertificate` | (2026-08-07, `MTLS-ROTATE`; bootstrap added 2026-08-23, `MTLS-MIGRATE`) The accept-**set**. `BusPinSet` replaces a bare `BusFingerprint` wherever an identity's accepted certificates are resolved or verified against (`Client.doer`, `pinnedTLSConfig`); it is bounded at `MaxBusPins` = 2 and every membership change goes through `With`/`Without`, never direct mutation. **`Identity.BusFingerprint` (singular) no longer exists** — see the BREAKING JSON CHANGE note above. `Client.AddBusPin`/`RemoveBusPin` are the Go API the `pin add`/`pin remove` subcommands are a thin shell over. `Client.BootstrapClientCertificate` is the Go API for migrating a legacy HTTP identity to a pinned HTTPS bus and binding its first client certificate without changing agent id. |
| `ClientCertificate`, `LoadOrCreateClientCertificate`, `Client.ClientCertificate`, `ClientTLSDirName`, `ClientCertFileName`, `ClientKeyFileName`, `ClientCertValidity`, `ErrClientCertIncomplete` | (2026-08-07, `MTLS-CLIENTCERT`) The agent's own TLS client-certificate surface. `LoadOrCreateClientCertificate` is **idempotent and non-destructive**: it mints `<identity-dir>/client-tls/{cert.pem,key.pem}` once (both `0600` inside `0700`), later loads the same material, and refuses damaged or half-populated state instead of minting over it. `ClientCertificate.Created` reports whether THIS call installed the material; `Fingerprint()` is SHA-256 over the leaf DER in **64 lowercase hex**; `IsExpired()` reports without refusing. `Client.ClientCertificate()` is the cached wrapper the CLI and embedders call. |
| `DefaultTimeout`, `DefaultRetryAttempts`, `DefaultRetryBaseDelay`, `DefaultRetryMaxDelay` | Defaults |
| `New`, `Client` | The client; `Config()`, `Store()`, `Identity()`, `Identities()`, `Use()`, `Logout()`, `LogoutAll()`, `Leave()`, `Enrol()`, `EnsureSession()`, `Send()`, `Broadcast()`, `Agents()`, `Read()`, `Watch()`, plus (2026-08-07) `MessagingPublicKey()`, `TrustPeer()`, `TrustedKeys()`, and (2026-08-07, `MTLS-ROTATE`) `AddBusPin()`, `RemoveBusPin()` |
| `Identity`, `Credential`, `PendingEnrolment`, `Store` (`OpenStore`, `Dir`, `Path`, `Warnings`, `List`, `ListPending`, `Resolve`, `SetCurrent`, `Remove`, `RemoveAll`, `FindApplied`, `PromotePending`, `Cursor`, `SetCursor`, `ClearCursor`, `CursorPath`) | Credential storage, plus `watch`'s persisted read position (`cursors.json` — see above). The in-flight-enrolment methods that take the unexported record type (`ClaimEnrolment`, `FindPending`, `DropPending`) are effectively package-internal and are NOT part of the embeddable surface. |
| `EnrolOptions`, `EnrolResult`, `SessionInfo`, `LogoutResult`, `LeaveResult` | Operation inputs and results. `LeaveResult` (added 2026-08-22, `AUTH-4`) is `client.Leave`'s result — see the `leave` JSON shape above; it is the server-notified counterpart to `LogoutResult`. |
| `SendOptions`, `BroadcastOptions`, `SendResult`, `AgentSummary`, `AgentList`, `ReadOptions`, `Batch`, `Message`, `WatchOptions`, `WatchStats` | Messaging inputs, results and the wire-faithful `Message`/`Batch` types |
| `Error`, `Kind` (+ the `Kind*` constants), `KindOf`, `ExitCode`, `ErrorPayload`, `NewErrorPayload`, the `Exit*` constants, `IsFatalUnavailable`, `IdempotencyKeyOf` | Errors and the exit-code contract |
| `SessionSigningContext`, `AgentNamePattern` | Pinned protocol constants |
| `MessageSigningContext` (`"agent-bus/msg-sig/1"`), `MessageSigningFormatVersion` (`1`), `BusIDPattern` | (2026-08-07) The message-signing constants, **pinned literals mirroring `internal/signing/canonical.go` byte-for-byte in behaviour** — `client/` may not import `internal/`, so the canonical encoder is duplicated in `client/canonical.go` under the same rule as `SessionSigningContext` and the route paths. **Divergence FAILS CLOSED**: a signature simply does not verify. |
| `KeyRing`, `DirKeyRing` (`NewDirKeyRing`, `MessagingKey`, `Trust`, `List`), `TrustedKey`, `TrustedKeysDirName`, `ErrNoTrustedKey`, `Config.KeyRing` | (2026-08-07) The local trust store. A `nil` `Config.KeyRing` means a `DirKeyRing` under `<identity-dir>/trusted-keys`; it is **not** a way to turn verification off — a `KeyRing` holding nothing means "this agent trusts nobody" and every message is unverifiable. Fail closed. |
| `RejectionReason` (+ `RejectedNoTrustedKey`, `RejectedMalformedKey`, `RejectedNoSignature`, `RejectedSignatureEncoded`, `RejectedSignatureLength`, `RejectedNotCanonical`, `RejectedSignatureInvalid`), `RejectedMessage`, `Batch.Rejected` | (2026-08-07) The verification-failure vocabulary. **Declared, json-tagged and stable — but NOT YET PRODUCED**: `Client.Read` does not verify, so `Batch.Rejected` is always empty today. The settled policy these encode, for when the wiring lands: on failure the **cursor ADVANCES**, the **body is DISCARDED** and never handed to the caller, and the event is **recorded loudly** (message id, sender, which check failed). Fail-closed applies to the BODY, not the CURSOR — blocking the cursor would hand anyone who can inject one bad message a permanent DoS against that agent. |
| `MaxBodyBytes`, `MaxBatchLimit`, `DefaultBatchLimit`, `MaxPollTimeout`, `DefaultPollTimeout` | Protocol limits, pinned literals mirroring the server's own (see `client/messages.go`) |
| `TerminalSafe(s string, keepNewlines bool) string`, `IsBidiOrInvisible(r rune) bool` | (2026-08-08, `CLI-3-FU-SAFETEXT`) The terminal-output neutralisers, **newly exported** — see below |

`Identity` is the redacted public half and `Credential` is `Identity` plus the secret seed; the split
is structural, so no rendering path can marshal a private key by forgetting a redaction step.
`Credential.String()` redacts. `SessionInfo` has **no token field at all**, not even a `json:"-"` one.

**`TerminalSafe` and `IsBidiOrInvisible` — two functions, two jobs** (`CLI-3-FU-SAFETEXT`,
2026-08-08). Both are in `client/sanitize.go`, both are now part of the compatibility surface.

```go
func TerminalSafe(s string, keepNewlines bool) string
func IsBidiOrInvisible(r rune) bool
```

`TerminalSafe` **REWRITES**, which is right when something must be printed: every C0 control and
`DEL`, every **C1** control (`0x80`–`0x9f` — a lone `0x9b` is `CSI` on some terminals, so these are
as dangerous as `ESC [` and are not merely "high bytes"), and every bidi/zero-width codepoint becomes
a **space**; invalid UTF-8 becomes `U+FFFD`. It does **not** truncate — a bound belongs to the
caller, since a message body is legitimately long. Replacement rather than deletion is contract, not
taste: dropping a codepoint splices the text either side into one convincing token, turning
`adm\x1bin` into `admin`.

`IsBidiOrInvisible` is the **predicate**, so a caller can **DISQUALIFY** instead of rewrite. That is
the right move when a value is being handed to a consumer that will print it *later* — `watch`'s
NDJSON `text` field is omitted rather than rewritten on this test, because `jq -r .text` strips the
JSON escaping and pipes the result straight at a terminal, and a lossily-rewritten body would be
worse than no field at all. Its set: `U+200B`–`U+200F`, `U+202A`–`U+202E`, `U+2066`–`U+2069`,
`U+FEFF`. **None of these is a control character**, so every ordinary control check misses all of
them.

`keepNewlines` is for a message **BODY**, where a newline is content. It must **never** be set for an
id, a timestamp or an error detail, where a line break is an attempt to forge a second line of
output — and a caller that keeps newlines takes on the job of making a continuation line
unmistakable (`cmd/agent-busctl` indents them, so a multi-line body cannot read as several messages).

They are exported because invariant 7's third audience — an agent EMBEDDING this package — needs
exactly this when rendering another agent's message body, and its only alternatives were reaching
into the package (impossible) or writing a second copy. `cmd/agent-busctl` had written that second
copy; this change **deleted it**. A security-relevant neutraliser that exists twice decays silently:
the two agreed the day they were written, and nothing would have failed on the day they stopped.
None of this applies to `--json` output, where `encoding/json` already escapes every byte below
`0x20`.

**The 503 split.** A `503` with a `Retry-After` header is a transient capacity refusal and is retried
with jittered backoff (`Watch`, and the transport's own retry loop, both honour it). A `503` with
**no** `Retry-After` means the bus's write path cannot durably accept messages at all — not
transient — and is **not** retried: it stops a `Watch` outright and is reported through
`client.IsFatalUnavailable`, which every long-running caller (a supervisor, a `watch`) must check and
stop on rather than back off forever, or an operator-visible fault becomes a silent one.

**`IsFatalUnavailable` now reports TWO conditions, not one** (`CLI-3-FU-HASHVERIFY`, 2026-08-08).
Read it as "fatal, bus-side"; the NAME is narrower than the condition and is kept unchanged for
compatibility. Both conditions are `KindServer`, so both are exit `6`, and both must stop a loop:

| Condition | Why retrying cannot help |
| --- | --- |
| A `503` with **no** `Retry-After` (`hub.ErrNotDurable` / `hub.ErrPoisoned`) | Every capacity refusal on every route carries the header, so its absence is the bus stating this is not transient — invariant 4, refusing rather than acknowledging what it cannot make durable. |
| A message whose body disagrees with the `size` or `content_sha256` beside it (`verifyMessageBody`) | A retry re-reads the same cursor and gets the same damaged message. |

The bit is unexported and read only through this function, deliberately: `Kind` is a **closed**
vocabulary a caller branches on and the CLI maps to an exit code, so this stays `KindServer` to
everything that switches on `Kind`, with one extra bit for the two places that must not loop on it —
the transport's retry loop and a long-running watch. `watchShouldRetry` therefore checks
`IsFatalUnavailable` **first and separately**, before its `KindNetwork, KindServer` retry branch,
which would otherwise sweep both conditions straight back into retrying.

---

## Env vars

| Var | Consumed by | Meaning |
| --- | --- | --- |
| `AGENT_BUS_URL` | `agent-busctl` | Bus base URL (`--bus`) |
| `AGENT_BUS_IDENTITY` | `agent-busctl` | Credential store directory (`--identity`) |
| `AGENT_BUS_AGENT_ID` | `agent-busctl` | Act as this stored identity (`--as`) |
| `AGENT_BUS_TIMEOUT` | `agent-busctl` | Per-operation timeout (`--timeout`) |
| `AGENT_BUS_PERSIST_SESSION` | `agent-busctl` | Cache the session token on disk (`--persist-session`). **Closed set**: `1`/`true`/`yes`/`on` enable; everything else, including `0` and `false` and any unrecognised value, leaves it off. It is deliberately NOT "non-empty means true" — an operator setting `=0` to DISABLE it must not thereby enable writing a bearer token to disk. |
| `AGENT_BUS_FINGERPRINT` | `agent-busctl` | The bus certificate to accept, 64 lowercase hex (`--bus-fingerprint`). Surrounding whitespace is trimmed, as for `AGENT_BUS_TIMEOUT`; nothing else about the value is repaired. **Not a secret** — a certificate fingerprint is published in the bus's startup log and derivable from any handshake — so an env var is a fit carrier for it, unlike a key. |
| `COLUMNS` | `agent-busctl agents` | (2026-08-08) Terminal width budget for the human-readable roster table. **The only env var here that is not `AGENT_BUS_`-prefixed**, because it is not ours — it is the conventional one, read rather than defined. See below. |

**`COLUMNS` — `agent-busctl agents` only, and `--json` is unaffected.** When the table would not fit
the budget, `agents` drops a **COLUMN** (`ENROLLED` first, then `BUS`, which is only the id's own
prefix restated) rather than cutting an id — a truncated fully-qualified id is not a thing a reader
can act on.

The contract, which is deliberately dull:

- The budget is `$COLUMNS`, falling back to **100** (`maxAgentTableWidth`) when it is unset.
- **The fallback is the common case, not a consolation prize.** `COLUMNS` is usually unset in a
  non-interactive shell — which is exactly when the output is piped into something that does not care
  about width, and exactly when an agent is reading it. "Unset" therefore behaves precisely as it did
  before this existed.
- **A value that is not a positive integer is NOT BELIEVED** and falls back to 100. `COLUMNS` is
  ordinary environment, so `0`, `-1`, `""` and `wide` are all reachable, and each would otherwise
  collapse the table to its narrowest form for a reason invisible to the reader. There is
  deliberately **no upper clamp**: an implausibly large value only ever means "never drop a column".
- It is read from the environment, not probed. An `ioctl(TIOCGWINSZ)` would give a sharper answer
  than a table that only ever drops two columns needs, at the cost of a dependency or per-platform
  `unsafe`/`syscall` plumbing (invariant 8, stdlib first).

`cmd/agent-bus` still reads no environment variables; every server knob is a flag.
(`scripts/bus-serve.sh` has its own `AGENT_BUS_RUN_DIR` / `AGENT_BUS_DATA_DIR` / `AGENT_BUS_LISTEN`
/ `AGENT_BUS_LOG_LEVEL` / `AGENT_BUS_POLL_TIMEOUT` — those are the wrapper's, not the binary's, and
are documented in `CONTRACTS-AGENT.md`.)
