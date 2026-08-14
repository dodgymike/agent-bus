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
| `-bus-id` | *(empty → placeholder `bus-local`)* | **TEST-ONLY.** Validated against `^[A-Za-z0-9_-]{1,64}$`; `.` rejected (qualification separator, invariant 2). Using it logs a runtime `WARN`. See `DECISIONS.md`. |

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

**This is the mint command's OUTPUT, not a settled wire shape.** There is deliberately no single
packed token — no base64 blob, no bespoke encoding — because the shape `client.EnrolOptions.Invite`
will carry is settled by `INVITE-CLIENT`, and inventing one here would be the same class of mistake as
hand-picking a record-type number.

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

### `agent-bus peer add|list|remove` — the operator's federation configuration

Added 2026-08-08 by `RELAY-12`. Source: `cmd/agent-bus/peer.go`. Durable records:
`internal/relay/peerstore.go` (see `CONTRACTS-ONDISK.md` for the `peer` / `bustrust` record shapes).

```
agent-bus peer add    -data-dir <dir> -bus-id <busID> [-url <https origin>]
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
| `-signing-key` | *(none)* | A pinned Ed25519 **bus signing** key, standard base64 (44 chars). **Repeatable, at most 2** (`relay.MaxPinnedBusSigningKeys`) — two means a **rollover window**, the outgoing key and the incoming one, not a general-purpose accept list. Repeating the flag **REPLACES** the pin set; it never adds to it. Order is preserved and is part of the record. |
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
| `1` | A change failed. Anything already durable is listed under `applied` in `--json` (and after `ALREADY DURABLE:` on stderr otherwise). | Read the message; `peer list`; retry. |
| `2` | Usage: bad flag, unknown subcommand, positional argument, malformed bus id, bad `-url`/`-signing-key`, a self-peer, or a combination that would do nothing (`add` with neither `-url` nor `-signing-key`; `remove` with neither `-route` nor `-trust`; `-route-for` without `-url`). Nothing is written. | `agent-bus peer -h` |
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
  {"kind":"route","bus_id":"busB","state":"active","base_url":"https://b.example:8443","config_seq":2,"updated_at":"…"},
  {"kind":"route","bus_id":"busC","state":"active","base_url":"https://b.example:8443","next_hop_bus_id":"busB","config_seq":3,"updated_at":"…"}]}

{"ok":true,"bus_id":"<this bus>","routes":[…],"trust":[…]}
```

`remove` may also carry `"not_found":["route"]` — the requested kinds this bus held nothing for,
reported rather than dropped so a partial withdrawal is visible. `kind` is `"route"` or `"trust"`;
`state` is `"active"` or `"removed"`. `unchanged: true` appears when
the store found that exact configuration already applied and therefore wrote **nothing** — `config_seq`
then names the **earlier** generation. `next_hop_bus_id` is **this command's knowledge, not a durable
field**. `list` reports ACTIVE records only, each sorted by bus id.

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
  yet served.
- **It does not fix `RELAY-36`.** `internal/relay/client.go`'s `peerURL` — the function that actually
  dials — still accepts a path. This command's `-url` check is **strictly tighter** than `peerURL`, so
  no value it writes can reach that gap, and `TestPeerAddURLRulesMatchTheDurableRecord` pins the CLI
  check against the durable record's own rule so the duplicate cannot drift in either direction.

**A `-route-for` record's address belongs to a DIFFERENT bus than its bus id.** Consequence for
`RELAY-20`/`RELAY-24`: anything that later keys a per-peer credential off the record's bus id — a TLS
certificate pin, a client certificate, a peer principal — would pin the *destination's* identity
against a connection that terminates at the *next hop*, and would break every non-adjacent hop. The
identity on the wire is the next hop's; the record's bus id is the destination.

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

### Subcommands (as of 2026-08-02)

| Command | Purpose | Network? |
| --- | --- | --- |
| `enrol --name <name>` | Generate an Ed25519 key pair, send only the public half, receive the server-minted `<bus-id>.<agent-id>`, store the credential **and the bus's pinned certificate fingerprint** | yes — `POST /v1/enroll` |
| `whoami [--all] [--verify]` | Show the identity commands act as; `--all` lists them; `--verify` performs a real session handshake | only with `--verify` |
| `use <agent-id\|name>` | Change the stored selection | no |
| `logout [<agent-id>] [--all]` | Delete a credential **locally** | no |
| `pin list \| add <fingerprint> \| remove <fingerprint>` | (added 2026-08-07, `MTLS-ROTATE`) List, widen or narrow the bus certificates an identity accepts — see "Certificate pinning" below | **no — purely local**, reads/writes the credential store only |
| `agents` | List every agent enrolled on the bus, fully-qualified id first | yes — `GET /v1/agents` |
| `send <to-agent-id> [body]` | Send one direct message, **signed**, durable before it returns (invariant 4) | yes — `POST /v1/mint` **then** `POST /v1/send` (two calls, one idempotency key — see "Signed sends" below) |
| `broadcast [body]` | **BROKEN as of 2026-08-07.** The subcommand is still registered and still builds a request; the bus answers **501** because a broadcast has no canonical audience under signing format v1. Surfaces as **exit 6** — see below. | yes — `POST /v1/mint` then `POST /v1/broadcast`, which refuses |
| `watch` | Long-poll and stream messages addressed to you until stopped | yes — `GET /v1/messages`, `GET /v1/wait` |

**There is no `agent-busctl keygen` and no `agent-busctl trust` subcommand**, and the registry in
`cmd/agent-busctl/root.go` is exactly the nine rows above. This matters because several error remedies in
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

#### `agent-busctl pin` — list / add / remove accepted certificates

```
agent-busctl pin list
agent-busctl pin add <fingerprint>
agent-busctl pin remove <fingerprint>
```

**Purely LOCAL** — nothing is sent to the bus. `pin` only reads and writes the credential store
(`client.Store.AddBusPin` / `Store.RemoveBusPin`, via `Client.AddBusPin` / `Client.RemoveBusPin`). It
takes no flags of its own beyond the globals (`--as` / `--identity` / `--json`).

- `pin list` — prints the current accept-set; makes no change.
- `pin add <fingerprint>` — confirm the value **out of band** first (the bus's
  `bus_cert_fingerprint=…` startup log line, or the invite), then widen the accept-set by one.
  Re-adding a fingerprint already held succeeds as a no-op — the obvious retry after an interrupted
  rollover. Refused at `MaxBusPins`, and refused on an identity enrolled against an `http` bus (a
  plaintext connection presents no certificate, so a pin there would be a check that never runs —
  re-enrol against the `https` URL instead). The gate is the **scheme, not an empty accept-set**: an
  `https` identity that holds *no* pin — which a downgrade can produce, see below — may gain one, and
  `pin add` is its recovery rather than a full re-enrolment.
- `pin remove <fingerprint>` — retire one certificate. Refused if it is the last one held, and refused
  (not a silent no-op) if the fingerprint given is not currently held, so a mistyped value cannot be
  reported as success while the real one stays accepted.

All three forms answer with the identity's **full resulting accept-set**, never a diff, so a script
driving a rollover reads the resulting state instead of reconstructing it:

```json
{"agent_id":"bus-abc.planner-1","bus_url":"https://127.0.0.1:8443",
 "bus_fingerprints":["<old-64-hex>","<new-64-hex>"],"max_bus_fingerprints":2}
```

`bus_fingerprints` is **always present and never null** (an empty accept-set prints `[]`), and
`max_bus_fingerprints` is the cap (`client.MaxBusPins`), so a caller can tell "one slot free" from
"add will be refused" without hard-coding the number.

Exit codes: `0` ok; `2` `usage` — unknown subcommand, wrong argument count, a malformed fingerprint,
`pin add` at the cap, or `pin remove` of the last pin; `3` `config` — no identity enrolled or selected.

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

`enrol` flags: `--name` (required), `--invite` (**reserved, currently rejected** — see below),
`--idempotency-key` (resume an earlier attempt), `--keep-current` (do not switch the selection).

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

**`logout` is LOCAL ONLY.** `/v1/leave` does not exist yet, so nothing is revoked: the enrolment
stays on the roster and any live session lives out its hour. The JSON field `server_notified` reports
this honestly and is `false` today.

### Exit codes — CONTRACT

An agent branches on these, so a value never changes meaning and a retired value is never reused.
They are produced by `client.ExitCode(err)` in the importable package, so an embedder gets the same
codes without copying a switch.

| Code | Kind | Meaning |
| --- | --- | --- |
| `0` | — | Success |
| `1` | `internal` | Unclassified/internal failure |
| `2` | `usage` | Malformed invocation: bad flag, missing `--name`, unknown subcommand, reserved `--invite` |
| `3` | `config` | Local identity/config not ready: nothing enrolled, no selection, unreadable or damaged store |
| `4` | `auth` | The bus rejected the credential (401/403), or the signature did not verify |
| `5` | `network` | The bus could not be reached: refused, DNS, timeout, or a certificate that does not verify |
| `6` | `server` | The bus reported a failure of its own (5xx), or a capacity refusal that survived retries |
| `7` | `rejected` | The bus understood the request and refused it (400/404/409/413/415/422) |
| `8` | `empty` | Succeeded with **nothing to report** (e.g. `whoami --all` on an empty store) |

`2` is usage rather than `1` to match Go's `flag` package and `cmd/agent-bus`.

No code changes meaning; some commands give one a more specific sense:

- `8` — `agents` on an empty roster, and a **bounded** `watch` (`--count`/`--for`) that delivered
  nothing before it finished. An unbounded `watch` stopped by a signal is always `0`, however many
  messages it saw.
- `7` — a 409 idempotency-key conflict on `send`/`broadcast` (same key, different payload — the bus
  disconnects), and an unknown recipient on `send`.
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
| `enrol` | `agent_id`, `bus_id`, `name`, `bus_url`, `bus_fingerprints` (array, **`omitempty`** — present only when at least one certificate was pinned, i.e. never for a plaintext loopback bus), `public_key`, `enrolled_at`, `replayed`, `idempotency_key`, `stored`, `store_path` |
| `whoami` | the identity fields above, plus `is_current` (bool), and `session` (`agent_id`, `expires_at`, `refresh_at`, `lifetime_seconds`) with `--verify` |
| `whoami --all` | `identities` (array), `current_agent_id` (string), and `pending` (array of `idempotency_key`/`name`/`bus_url`/`created_at`) when any enrolment is unfinished |
| `use` | the identity fields, plus `is_current` (bool) |
| `logout` | `removed` (array of agent ids), `current_agent_id` (string), `server_notified` |
| `pin` (`list`/`add`/`remove`) | `agent_id`, `bus_url`, `bus_fingerprints` (array, **never null** — an empty accept-set prints `[]`), `max_bus_fingerprints` (int, `client.MaxBusPins`). See "Certificate pinning" above. |
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
| `message_id` | the server-minted id, the key to deduplicate on |
| `seq` | the server-minted monotonic sequence |
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
| `<identity-dir>/cursors.json` | `0600` | Format version `1`. One `watch` read position per (`agent_id`, `bus_id`) pair — **`bus_id`, the server-minted one, NOT the bus URL, since `CLI-3-FU-URLKEY` (2026-08-08)**; see below. No key material. Capped at 256 records, and 512 bytes per stored cursor, so a bus cannot grow the file without bound. |
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
| **MESSAGING** | `messaging_key_seed` | this agent **to its PEERS** — it signs the canonical bytes of every outgoing message | **on first use**, lazily, under the store lock (`Store.EnsureMessagingKey`) |

Both private halves live in the same `0600` `identities.json` inside the `0700` store directory, and
**neither ever leaves the machine**. `Credential.String()` redacts both.

Splitting them is invariant 3's separation of concerns, not bookkeeping: the bus must be able to
authenticate an agent without being able to speak as it. Only the AUTH public key is registered with
the bus (at enrolment); the MESSAGING public key is registered **nowhere**, and that is the gap
below.

**KNOWN GAP — there is no way to publish or fetch a messaging public key through the bus.** Nothing
registers one at enrolment (the server leaves `auth.RosterEntry.MessagingPublicKey` zero),
`GET /v1/agents` carries no key material, and CRYPTO-4 (the server-attested key bundle) does not
exist. `trusted-keys/` is therefore a **manually populated stopgap**: a peer's key reaches it out of
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

### Enrolment idempotency (invariant 10)

The key pair is written to the store as a `pending` record **before** `/v1/enroll` is called, so a
process killed after the bus minted an id does not lose the private key. Records are scoped to
**(idempotency key, bus URL)** — the same scoping the server uses — so:

- re-running an enrolment with the same `--idempotency-key` and name is answered **from the store**,
  with `"replayed": true` and **no HTTP request**;
- the same key with a different name on the same bus is refused **locally**, exit `2`, because the
  bus's answer to that is a 409 **and a disconnection**;
- the same key against a different bus is a fresh enrolment;
- a network failure keeps the record and the error names the exact `--idempotency-key <key>` that
  resumes it; `whoami --all` lists every unfinished enrolment with the command that resumes it, so a
  process killed before it printed anything still leaves a recoverable identity;
- pending records are pruned 24h after creation, on the next store write, and are destroyed
  outright by `logout --all`.

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
- **Same key + different content** is a protocol violation. The bus answers `409` **and disconnects**
  — surfaced as its own loud `KindRejected` error, exit `7`. Retrying will not help; use a fresh key
  for new content.
- A key is remembered only as long as the message it produced is **retained** (1 day, or until 1 GiB
  of messages push it out). A "retry" that arrives after that produces a **second message** rather
  than being rejected — a key is a retry handle for minutes and hours, not for days.

### In flight — what will change

- **`--invite` is RESERVED and currently rejected** (exit `2`) rather than guessed. Enrolment is
  becoming invite-only (invariant 3) and the blob will carry bus id, address, **bus-certificate
  fingerprint** and invite secret — but the wire shape is settled by task `ENROL-SHAPE`, and
  `/v1/enroll` is explicitly UNSTABLE until it, certificate binding and POPKEY all land. Inventing a
  field name here would be the same mistake as hand-picking a record-type number.
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
  reused under another's. The remaining half of the original note stands: the per-identity **client
  certificate** still has no home, and `MTLS-CLIENTCERT` gives it one.

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
| `BusPinSet`, `NewBusPinSet`, `ParseBusPinSet`, `MaxBusPins`, `Identity.BusFingerprints`, `Client.AddBusPin`, `Client.RemoveBusPin` | (2026-08-07, `MTLS-ROTATE`) The accept-**set**. `BusPinSet` replaces a bare `BusFingerprint` wherever an identity's accepted certificates are resolved or verified against (`Client.doer`, `pinnedTLSConfig`); it is bounded at `MaxBusPins` = 2 and every membership change goes through `With`/`Without`, never direct mutation. **`Identity.BusFingerprint` (singular) no longer exists** — see the BREAKING JSON CHANGE note above. `Client.AddBusPin`/`RemoveBusPin` are the Go API the `pin add`/`pin remove` subcommands are a thin shell over, so an embedding agent can survive a rotation without shelling out. |
| `DefaultTimeout`, `DefaultRetryAttempts`, `DefaultRetryBaseDelay`, `DefaultRetryMaxDelay` | Defaults |
| `New`, `Client` | The client; `Config()`, `Store()`, `Identity()`, `Identities()`, `Use()`, `Logout()`, `LogoutAll()`, `Enrol()`, `EnsureSession()`, `Send()`, `Broadcast()`, `Agents()`, `Read()`, `Watch()`, plus (2026-08-07) `MessagingPublicKey()`, `TrustPeer()`, `TrustedKeys()`, and (2026-08-07, `MTLS-ROTATE`) `AddBusPin()`, `RemoveBusPin()` |
| `Identity`, `Credential`, `PendingEnrolment`, `Store` (`OpenStore`, `Dir`, `Path`, `Warnings`, `List`, `ListPending`, `Resolve`, `SetCurrent`, `Remove`, `RemoveAll`, `FindApplied`, `PromotePending`, `Cursor`, `SetCursor`, `ClearCursor`, `CursorPath`) | Credential storage, plus `watch`'s persisted read position (`cursors.json` — see above). The in-flight-enrolment methods that take the unexported record type (`ClaimEnrolment`, `FindPending`, `DropPending`) are effectively package-internal and are NOT part of the embeddable surface. |
| `EnrolOptions`, `EnrolResult`, `SessionInfo`, `LogoutResult` | Operation inputs and results |
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
