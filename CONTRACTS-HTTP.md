# Contracts: HTTP routes, headers, enrolment and authentication

Split out of `CONTRACTS.md` (2026-08-02) — see that file for the index of all contract planes and
the rest of the surface (CLI/env, on-disk, agent-facing). This is a pure content move: everything
below this header is unchanged from the prior single-file `CONTRACTS.md`, verbatim.

## Transport (`MTLS-LISTENER`, added 2026-08-07)

**Every route below is reachable over `https` ONLY.** Invariant 11: the server's single listener is
wrapped in `tls.NewListener` before it accepts a connection, TLS floor `1.2`, ALPN pinned to
`http/1.1`, and **`ClientAuth: tls.RequestClientCert`** — the listener **requests** a client
certificate and **never requires** one (`MTLS-CLIENTAUTH`, landed 2026-08-14, `a97f854`; this line
said `tls.NoClientCert` until 2026-08-14 and was false from that commit onward). **Do not read this
listener as mutual TLS**: a connection presenting nothing still completes the handshake and is the
ordinary case for every route below, and a connection presenting a certificate is **not** thereby
authenticated as anybody — resolving a presented certificate to a principal is application-layer work
(`### The client certificate on an agent connection`, below, and `## Peer-bus transport identity` for
peer buses). See `CONTRACTS-CLI.md`'s CLI-flags section for the full statement, the
`client_auth=requested` field on the `server started` log line, and why `requested` is the policy
rather than a step towards `required`.

**A plaintext request to the port never reaches any row in the table below.** `crypto/tls` fails the
handshake before `net/http` decodes a request line, and `net/http` itself writes a bare
`HTTP/1.0 400 Bad Request` + `Client sent an HTTP request to an HTTPS server.` onto the raw socket and
closes it — no route match, no `authMiddleware`, no handler. Read every "none"/"bearer" auth column
below as "once TLS has completed", not as a statement about what a plaintext caller sees; a plaintext
caller sees that one 400, identically, for every path including `/healthz`.

## Routes

| Method | Path | Auth | Status | Response |
| --- | --- | --- | --- | --- |
| `GET` | `/healthz` | none | 200 | `{"status":"ok"}` |
| `GET` | `/v1/info` | none | 200 | `{"bus_id":"...","version":"...","uptime_seconds":0.0,"discovery":"/v1/discovery"}` |
| `GET` | `/v1/discovery` | none | 200 | **NEW 2026-08-07 (DISCOVERY-DOC).** The bounded, STATIC protocol-discovery document — observed ~6.1 KB in practice (varies only with the length of `bus_id`), well under the 16 KiB ceiling `discovery_test.go` enforces. See `### Discovery document` below for the full shape. |
| `HEAD` | `/healthz`, `/v1/info`, `/v1/discovery` | none | 200 | **CHANGED 2026-08-08 (CORE-7): HEAD is now ACCEPTED on every GET route** and was a 405 before. Same status as the corresponding `GET`, and the same `Content-Type` / `X-Content-Type-Options`, with **no body** — `writeJSON`/`writePreformattedJSON` suppress it. **`Content-Length` is absent** (measured: `GET /healthz` sends `Content-Length: 16`, `HEAD /healthz` sends none), because net/http computes it from bytes written and the handler writes none. Legal under RFC 9110 §8.6, which permits omitting it; do not read "same headers" more strongly than this row states. Probes (load balancers, container healthchecks, uptime monitors) commonly issue `HEAD /healthz`; that used to be a false alarm from the one route whose job is to report liveness honestly. |
| other | `/healthz`, `/v1/info`, `/v1/discovery` | none | 405 | `{"error":"method not allowed"}`, `Allow: GET, HEAD` — the `Allow` value changed with CORE-7 and now names every method the route serves |
| `POST` | `/v1/enroll` | none (unauthenticated by necessity — this is how the credential is obtained; only registered when `Options.Auth != nil`, see AUTH-1 section below) | 201 | `{"agent_id":"...","bus_id":"...","name":"...","enrolled_at":"<RFC3339Nano UTC>"}` — the SAME body, byte for byte, on an idempotent replay (see `Idempotency-Replayed` header) |
| `POST` | `/v1/enroll` | none | 400 | invalid `name`; invalid `public_key` (not base64, or not exactly the 32-byte Ed25519 public key size); invalid `messaging_public_key` **when present** (three checks: standard base64, exactly 32 bytes, and not equal to `public_key` — see the request-body section below); invalid `idempotency_key` (empty, over 128 bytes, or a byte outside `[A-Za-z0-9._-]`) |
| `POST` | `/v1/enroll` | none | 409 | `idempotency_key` reused with a **different** `name`/`public_key`/`messaging_public_key` than its first use — a protocol violation, not a retry (invariant 10). Rejected and logged; **the connection is KEPT** — narrowed 2026-08-08, this row carried `Connection: close` until then (see `## Headers`) |
| `POST` | `/v1/enroll` | none | 409 | **NEW (MTLS-BIND, 2026-08-14, `818207d`).** `{"error":"this client certificate is already bound to an agent; enrol with a fresh client keypair"}` — the client certificate presented on THIS CONNECTION is already a live binding on a **different** agent id (`auth.ErrCertFingerprintBound`). Rejected and logged at WARN; **the connection is KEPT** (invariant 10: a merely buggy client reaches this by re-enrolling without regenerating its keypair, and this route is unauthenticated so the socket identifies no principal to punish). **The body names no agent** — naming the holder would make enrolment an oracle mapping a certificate an anonymous caller possesses to an agent id on this bus; the server LOG names it. Nothing is written and **no agent-id suffix is burned** — the refusal is read BEFORE the mint and under the same `enrolMu` as the write, so it never reaches the never-reclaimed suffix floors (`Roster.Put`'s own check, after the mint, stays the authoritative one). See `### The client certificate on an agent connection` below. |
| `POST` | `/v1/enroll` | none | 409 | **NEW (AUTH-DUP-ENROL-KEY, 2026-08-22).** `{"error":"this enrolment public key is already bound to an agent; enrol with a fresh keypair"}` — the AUTH `public_key` in this request is already held by a **different** agent id (`auth.ErrAuthKeyBound`). Rejected and logged at WARN; **the connection is KEPT** (invariant 10: a merely buggy client reaches this by re-enrolling with a keypair it already enrolled, and this route is unauthenticated so the socket identifies no principal to punish). **The body names no agent** — naming the holder would make enrolment an oracle mapping a (public) key to an agent id on this bus; the server LOG names it. Nothing is written and **no agent-id suffix is burned** — the refusal is read BEFORE the mint and under the same `enrolMu` as the write (`Roster.Put`'s own check, after the mint, stays the authoritative one). This is the AUTH-KEY mirror of the client-certificate 409 above, and closes the hole where one keypair could hold multiple agent ids (an impersonation/accountability hole; it was also why the per-agent active-session cap bought almost nothing). The DECISION to REJECT rather than idempotently return the existing id is recorded in `DECISIONS.md` (2026-08-22, AUTH-DUP-ENROL-KEY). |
| `POST` | `/v1/enroll` | none | 503 | the roster (default 4096 entries) or the idempotency table (default 16384 entries) is at capacity; `Retry-After: 5` |
| `POST` | `/v1/enroll` | none | 201 | **NEW (INVITE-GATE, 2026-08-14).** Presenting a valid, unspent `invite_id`+`invite_secret` alongside a fresh enrolment redeems the invite ATOMICALLY, in the SAME `wal.Entry` as the enrolment record — one transaction, two fsyncs (see `CONTRACTS-ONDISK.md`, kind `"agent+invite"`). The 201 body is the same `EnrolResponseBody` shape as above. A legitimate retry of the invite redemption (same invite id + same idempotency key + same payload) replays the ORIGINAL 201 body verbatim with `Idempotency-Replayed: true` — sourced from the invite's own stored consumption record, not the roster's applied-key table (see `## Headers` below). **Presenting no invite at all is still accepted and still 201** — enrolment is NOT gated by this change; see the `enrolment` row of the discovery document and the "Known gaps" note below. |
| `POST` | `/v1/enroll` | none | 400 | **NEW (INVITE-GATE, 2026-08-14).** `invite_id` and `invite_secret` presented but not both — they must arrive TOGETHER; omitting both is accepted (no invite), sending exactly one is refused rather than silently treated as "no invite", because that would leave a client believing a credential it half-sent was spent. Neither value is echoed. |
| `POST` | `/v1/enroll` | none | 403 | **NEW (INVITE-GATE, 2026-08-14).** `{"error":"invite not accepted"}` — the deliberately COLLAPSED answer for unknown id, expired, revoked, already-redeemed, or malformed invite id (`internal/invite/errors.go`), so this route is not an oracle for which invite ids exist or are live. The specific reason is logged server-side only. |
| `POST` | `/v1/enroll` | none | 409 | **NEW (INVITE-GATE, 2026-08-14).** `{"error":"idempotency key already used with a different payload"}` — the invite's OWN (invite id, idempotency key) pair was reused with a DIFFERENT payload fingerprint; a protocol violation, not a retry (invariant 10). Rejected and logged, **connection KEPT**. This is a SEPARATE idempotency scope from the roster-level 409 above — see `internal/invite/doc.go` section 4 for why the invite, not the agent or the bus, is the right namespace. |
| `POST` | `/v1/enroll` | none | 409 | **NEW (INVITE-GATE, 2026-08-14).** `{"error":"another redemption of this invite is in flight; retry"}` — another lifecycle transition for this invite (a concurrent redemption or a revocation mid-write) is already in progress, including one presenting the SAME key. Safe to distinguish from the row above: `Begin` only reaches this check after the presented secret has already verified, so it tells a non-holder nothing. |
| `POST` | `/v1/enroll` | none | 501 | **NEW (INVITE-GATE, 2026-08-14).** `{"error":"this bus does not redeem invites"}` — an invite was presented but this bus was built with no invite store (`httpapi.Options.Invites == nil`). Never a silent success: a client must never walk away believing its single-use credential was spent when it was not. |
| `POST` | `/v1/enroll` | none | 503 | **NEW (INVITE-GATE, 2026-08-14).** the invite table (default 8192 entries, `invite.MaxInvites`) is at capacity; `Retry-After: 5`. A SEPARATE capacity bound from the roster/idempotency 503 above — an invite-table refusal does not touch the roster or idempotency tables and vice versa. |
| `POST` | `/v1/session/begin` | none (issues the challenge; only registered when `Options.Auth != nil`) | 200 | `{"agent_id":"...","token":"...","challenge_expires_at":"<RFC3339Nano UTC>"}` |
| `POST` | `/v1/session/begin` | none | 404 | `agent_id` is malformed **or** well-formed but not on this bus's roster — the two cases are deliberately indistinguishable to the caller. **NARROWED 2026-08-14 (MTLS-CROSSCHECK): an EMPTY `agent_id` no longer reaches this row** and is now the 403 below; every other malformed value still 404s here |
| `POST` | `/v1/session/begin` | none | 403 | **NEW (MTLS-CROSSCHECK, 2026-08-14, `2ea7dfb`).** `{"error":"this credential was not presented over the client certificate it is bound to"}` — invariant 11's cross-check, run **before `BeginSession` mints a challenge**, so a mismatched connection creates no server state at all (a challenge is server state with a lifetime, and an unauthenticated caller must not be able to create it for an agent whose certificate it does not hold). Four causes, deliberately indistinguishable in the response: the named agent holds ≥1 live certificate binding and this connection presented no matching in-date certificate; the connection's certificate is a live binding on a **different** agent; that certificate is live on **two** agents at once (it names nobody until an operator retires all but one); or `agent_id` is **empty**, which names no agent and so cannot be checked against anything. **NO `WWW-Authenticate` header** (the wrong half of the pair is the connection's certificate, chosen at handshake time — resending a header cannot help) and **never `Connection: close`** (invariant 10: a merely buggy client — one that regenerated its client keypair without re-enrolling — reaches this on every request, and the socket may carry other principals' traffic). `body.AgentID` is untrusted here and is never logged raw. See `### Invariant 11's cross-check on the agent plane` below, including the enumeration oracle this row deliberately leaves standing. |
| `POST` | `/v1/session/begin` | none | 503 | the session table (default 16384 entries, pending + active together) is at capacity; `Retry-After: 5` |
| `POST` | `/v1/session/complete` | none (activates the credential; only registered when `Options.Auth != nil`) | 200 | `{"agent_id":"...","expires_at":"<RFC3339Nano UTC>","lifetime_seconds":3600,"refresh_after_seconds":2700}` |
| `POST` | `/v1/session/complete` | none | 400 | `signature` is not valid base64; also returned if the roster holds a corrupt (wrong-length) public key for the agent (defence in depth — see `internal/auth/session.go`) |
| `POST` | `/v1/session/complete` | none | 401 | the signature does not verify against the agent's enrolled public key, or is not exactly the 64-byte Ed25519 signature size |
| `POST` | `/v1/session/complete` | none | 404 | `token` names no session (never existed, already expired, or was dropped after a prior failed verification), or a pending/active session has passed its deadline — again deliberately indistinguishable to the caller |
| `POST` | `/v1/session/complete` | none | 403 | **NEW (MTLS-CROSSCHECK, 2026-08-14, `2ea7dfb`).** The same body and the same four causes as the `/v1/session/begin` 403 above, checked against the **server-recorded `sess.AgentID`** — never a body field, and `SessionCompleteRequestBody` carries no agent id for one to be taken from. It necessarily runs **after** `CompleteSession`, because the agent id is not knowable until the token resolves, so **the session is left ACTIVATED and then refused**. That residue authorises nothing: every authenticated route applies the same check to the same token over any connection, so an activated-but-refused handle is useless until it expires on its own. No `WWW-Authenticate`, no disconnect. |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 400 | malformed JSON, an unrecognised field, or trailing content after the one JSON value the body must contain |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 405 | any method but `POST`; `Allow: POST` |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 413 | request body exceeds `httpapi.MaxAuthRequestBytes` (8 KiB) |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 415 | `Content-Type` is not `application/json` (a `charset` parameter is accepted) |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 429 | **NEW (`AUTH-1-FU-RATELIMIT`).** `{"error":"rate limit exceeded"}` with **`Retry-After: <whole seconds>`** — the PER-SOURCE token bucket in front of these three routes is empty for this source. Keyed on the TCP **peer address, port stripped** (proxy headers ignored — trivially forged). Enabled by default (`cmd/agent-bus` `-auth-rate-limit 5` / `-auth-rate-burst 60`; set burst `0` to disable). It sits in front of the allow-list and does NOT change its membership (invariant 3). **The connection is KEPT — never a disconnect** (invariant 10: too-fast is not replay, and one anonymous socket may carry a legitimately-busy client). Runs BEFORE any body parse or credential read, so a throttled request consumes no roster/session capacity. Logged at Info (`request rate-limited: per-source cap on an unauthenticated credential route`). **Honest limitation:** clients behind one NAT/proxy/Docker-bridge address (`172.17.0.1`) share one bucket and throttle each other — see `CONTRACTS-CLI.md`. The limiter is OFF unless `httpapi.Options.AuthRateLimit` is configured, so every build that does not opt in (and the whole test suite) is unchanged. |
| `POST` | `/v1/leave` | bearer — **AUTHENTICATED**, and deliberately **NOT** on `unauthenticatedRoutes`; only registered when `Options.Auth != nil` | 200 | **NEW (AUTH-4).** `{"agent_id":"...","left":true,"already_left":false,"sessions_dropped":N}` — self-leave only: the subject is always the AUTHENTICATED principal (`PrincipalFromContext`), never a request-body field, and the route reads no body at all. Durably removes the caller's own agent from the roster — a tombstone through the two-phase prepare→commit write path (invariants 4, 6) — and drops every one of its live sessions, pending and active, at once. `left` is always `true` on a 200. |
| `POST` | `/v1/leave` | bearer | 200 | A repeat call after the agent has already left answers the identical shape with `already_left:true` and `sessions_dropped:0` — no new tombstone is written and nothing is re-applied (invariant 10). Leaving twice is a legitimate retry, never a 409 and never a disconnect. |
| `POST` | `/v1/leave` | bearer | 500 | An unexpected roster-write failure. `ErrUnknownAgent` (a malformed id) is unreachable on this route — `agent_id` is the server-minted authenticated principal, never client input. |
| `POST` | `/v1/leave` | bearer | 405 | any method but `POST`; `Allow: POST`. No 400/413/415 on this route — it reads no request body, so there is nothing to reject the shape, size or `Content-Type` of. |
| any | any path off the six-entry allow-list (`/healthz`, `/v1/info`, `/v1/discovery`, `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete`) | `Authorization: Bearer <token>` required — see `## Authentication` below | 401 | `{"error":"authentication required"}` when no usable credential was presented at all (missing or duplicate `Authorization` header, a scheme other than `Bearer`, an empty/spaced/oversized/non-base64url token — `WWW-Authenticate: Bearer realm="agent-bus", error="invalid_request"`), or `{"error":"invalid or expired credential"}` when a well-formed token failed to authenticate (unknown, pending, or expired — deliberately indistinguishable, see `## Authentication` — `WWW-Authenticate: Bearer realm="agent-bus", error="invalid_token"`) |
| any | any path off the six-entry allow-list | `Authorization: Bearer <token>` that DID authenticate | 403 | **NEW (MTLS-CROSSCHECK, 2026-08-14, `2ea7dfb`).** `{"error":"this credential was not presented over the client certificate it is bound to"}` — invariant 11's cross-check, applied in `authMiddleware` **after the token authenticates but before the principal is attached to the context**, so no handler can ever see a principal the cross-check did not accept. Same four causes and same fixed body as the two session rows above; the agent id here is server-minted (out of the roster via `Authenticate`), never client-supplied. **No `WWW-Authenticate`** — unlike the 401 rows either side of this one, the caller already authenticated and re-presenting a bearer token cannot help. **Never `Connection: close`.** The acknowledged trade: 403 here versus those 401s does let a caller separate "valid token, wrong certificate" from "invalid token", which is accepted because reaching this row at all requires already holding a valid session token, and collapsing it would cost an honest client its only signal to re-enrol. |
| any | unregistered path, no credential (or one that does not authenticate) | — | 401 | `authMiddleware` wraps the whole mux and refuses before the mux is ever consulted, so an anonymous caller cannot enumerate which paths this bus serves by probing unknown ones; same body/header shape as the row above |
| any | unregistered path, valid bearer token | valid bearer token **that also passed the cross-check** (MTLS-CROSSCHECK, 2026-08-14 — `authMiddleware` applies the 403 row above before the mux is consulted, so a mismatched certificate never reaches this 404) | 404 | **CHANGED 2026-08-08 (CORE-8): now `{"error":"not found"}` with `Content-Type: application/json; charset=utf-8` and `X-Content-Type-Options: nosniff`.** It was `net/http.ServeMux`'s built-in `text/plain` "404 page not found", which broke the JSON error contract every other route honours — a client, or a wrapper piping through a JSON parser, got a parse error exactly when something was already wrong. Served by a catch-all registered at `RouteCatchAll` (`"/"`) **through `(*Server).route`, i.e. INSIDE the auth wrapper** — see the row above for why that placement is load-bearing. **Every method gets 404, never 405**: 405 would assert the resource exists but not via that verb, which is false here and lets a caller separate "path exists, wrong method" from "path does not exist" by method-probing. The body never echoes the requested path. This 404 is also what `/v1/enroll`, `/v1/session/begin`, and `/v1/session/complete` return when the server was built with `Options.Auth == nil` — those three stay on the allow-list unconditionally (see the AUTH-1 section below), so they reach the mux with or without a credential and 404 there like any other unregistered path. |

### Discovery document (added 2026-08-07, DISCOVERY-DOC)

`GET /v1/discovery` is an unauthenticated, bounded, STATIC document so a caller holding nothing but a
bus URL can learn how to enrol without first needing a credential. `internal/httpapi/discovery.go` is
the source of truth for the exact wording; this section pins the shape. The body is exactly ten
top-level fields, in this order:

| Field | Type | Contents |
| --- | --- | --- |
| `service` | string | Constant: `"agent-bus"`. |
| `description` | string | One-paragraph, constant description of what the bus does. |
| `bus_id` | string | The **one** bus-specific value in the whole document — the same `bus_id` `/v1/info` already serves to the same anonymous caller. |
| `paths_are_relative_to` | string | States that every `path` below is relative to the base URL the caller already fetched this document from, and explains why the document does NOT echo a self-URL (the `Host` header is client-supplied; a reflected URL could point a reader at an attacker's bus). |
| `steps` | array of 8 strings | The ordered enrolment recipe, from fetching this document through generating a keypair, `POST /v1/enroll`, the session handshake, and `GET /v1/wait`/`POST /v1/mint`+`POST /v1/send`. |
| `endpoints` | array of 11 objects | `{name, method, path, auth, purpose}` per entry; `auth` is `"none"` or `"bearer"`. |
| `enrolment` | object | `{invite_required (bool), invite_accepted (bool), invite_note, you_supply, you_receive}`. **`invite_accepted` is NEW (INVITE-GATE, 2026-08-14)** — see the note immediately below the table. |
| `session` | object | `{model, lifetime_seconds, refresh_after_seconds, authorization_header, signing_context}`. |
| `client` | object | `{binary, build, go_package, note}` — points at the compiled `agent-busctl` CLI and the importable `client` Go package (invariant 7). |
| `limitations` | array of 5 strings | Blunt, verified-true-of-this-build negative claims — see below. |

**`invite_required` and `invite_accepted` are two DIFFERENT questions (INVITE-GATE, 2026-08-14) and
must never be collapsed.** `invite_required` reports whether an enrolment MUST carry an invite — it is
`false` in this build (verified: `internal/httpapi/discovery.go`'s `InviteRequired: false`, and
`cmd/agent-bus/main.go` logs `enrolment_invite_required=false` at startup), and that is the truth, not
a placeholder: enrolment WITHOUT an invite is still accepted. `invite_accepted` reports whether an
invite PRESENTED to `POST /v1/enroll` is genuinely redeemed — `true` on any build with an invite store
wired (every `cmd/agent-bus` binary today), `false` only for a `httpapi` build that omits one (a
`501` on presenting one, never a silent ignore). Do not read `invite_accepted: true` as "invites are
mandatory" — it answers only "a presented one works", not "one is required".

**Invariants that govern this document, and must not be relaxed by a future edit:**

- **It describes the PROTOCOL, never the ROSTER.** No agent list, no agent count, no data-dir or
  on-disk path, no listen address, no peer list, no key material, no uptime, no config value. The
  only bus-specific value anywhere in the document is `bus_id`.
- **The `endpoints` list is a STATIC, compile-time-constant list — it is NOT a projection of the
  registered routes (`s.routes`/`Routes()`).** `authMiddleware` deliberately answers 401 rather than
  404 on an unknown path so an anonymous caller cannot enumerate which routes this build serves (the
  messaging and credential routes are registered only when `Options.Hub`/`Options.Auth` are non-nil).
  A mux-derived endpoint list would hand out exactly the configuration that 401-not-404 choice exists
  to withhold. `/v1/broadcast` is **deliberately absent** from `endpoints` for the same class of
  reason stated differently: it is registered and authenticates, then answers 501 (see "Signed sends"
  above), and advertising a route that refuses everything is worse than not advertising it — it is
  instead named honestly in `limitations` entry 4.
- **The document is built ONCE, in `httpapi.New`, and cannot grow with bus state.** Its only input is
  `Identity.BusID()`, which is stable for the process lifetime; the handler (`handleDiscovery`) writes
  the value stored on `*Server` at construction and computes nothing per request — no route
  enumeration, no state read, no clock.
- **`auth.SessionSigningContext` is deliberately NOT served.** `session.signing_context` says so and
  points at the compiled client, which pins the prefix instead. It is documented as a value the
  client must PIN (see "The signing contract" above): a client that learned the domain-separation
  prefix from the server would sign whatever a man-in-the-middle chose to put in front of the token.

The `limitations` array is blunt on purpose and must be restated exactly, never softened, if quoted
elsewhere: (1) **TLS is on, mutual TLS is not, and TLS alone protects against a PASSIVE observer
only** (rewritten 2026-08-07 by `MTLS-LISTENER` — it previously said "no transport security,
plaintext HTTP", which the running bus made false): https only, no plaintext listener, a plaintext
request never reaches a route; but the certificate is self-signed with **no CA and no
trust-on-first-use**, so the caller **must pin the fingerprint** it got out of band, and **until it
does, an ACTIVE on-path attacker can terminate TLS with its own certificate and read the session
token — including on this document, which is unauthenticated and would be served by that same
attacker**. The order of those clauses is deliberate and was set by the security gate: the
reassurance must not precede the condition that makes it true. Separately, the bus does **not**
request a **client** certificate, so TLS authenticates the BUS to the caller and never the caller to
the bus — the session token remains the only thing proving who the caller is; (2) messages are
signed but the bus checks the signature's SHAPE only and does **not** verify it against the sender's
key — **by design, not a gap awaiting a release** ("the bus enforces shape, the recipient enforces
authenticity"; a bus that verified would move the trust boundary onto itself), and the recipient
cannot verify either today because nothing distributes messaging public keys, so every message must
be treated as UNAUTHENTICATED; (3) no end-to-end encryption — bodies are held, **persisted to disk
unencrypted** and served in the clear, so anyone who can read the data directory can read every
stored body (only the append-only audit log omits bodies; the message store does not);
(4) `POST /v1/broadcast` answers 501; (5) single bus only — cross-bus relay is not served yet, a
recipient on another bus is a 404.

`steps` distinguishes the **two separate Ed25519 keypairs**, which is the one thing a reader is most
likely to get wrong: step 3's AUTH key is the only key the bus ever learns and is what authenticates
you *to the bus*, while a message signature (step 8) is made with a **second, separate MESSAGING
key** that is never sent to the bus. Since no endpoint distributes messaging public keys, no
recipient can currently verify a message signature — which is why step 8 points at limitation 2.

Both `session.lifetime_seconds` and `session.refresh_after_seconds` are **derived at construction**
from `auth.SessionLifetime` and `auth.RefreshAfter()` rather than hand-copied, so the document cannot
drift from the rule the server enforces; `TestDiscoverySessionConstantsMatchAuth` pins the equality.

### Messaging routes (added 2026-08-02 — MSG-1…5, POLL-1…3)

Registered **only when the server has a hub** (`Options.Hub != nil`, or one that `httpapi.New` built
for itself — see `## Messaging` below). When there is no hub they are not registered at all and 404
like any other path this build does not serve, exactly as the three auth routes do without
`Options.Auth`. **Every one of them authenticates**: none is on the allow-list, and none may ever be
added to it.

| Method | Path | Auth | Status | Response |
| --- | --- | --- | --- | --- |
| `GET` | `/v1/agents` | bearer | 200 | `{"agents":[{"agent_id":"<bus>.<name>-<n>","name":"...","enrolled_at":"<RFC3339Nano UTC>"}],"count":N}` — sorted by `agent_id`. Carries **no key material** — in particular NOT the messaging public key, which nothing registers (see "Signed sends" below). |
| `POST` | `/v1/mint` | bearer | 201 | **NEW 2026-08-07 (SIGN-2).** `{"message_id":"<bus-id>-<seq>","seq":N,"sender":"<authenticated principal>","op":"send","expires_at":"<RFC3339Nano UTC>"}`. Request: `{"op":"send"\|"broadcast","idempotency_key":"..."}` — **there is no `sender` field in the request and there never may be** (invariant 1); the response echoes the AUTHENTICATED principal. Reserves the id and sequence the caller must sign. See "Signed sends" below. |
| `POST` | `/v1/mint` | bearer | 201 + `Idempotency-Replayed: true` | A repeat of the same `(agent, op, idempotency_key)` returns the SAME reservation, body **byte-identical including `expires_at`** (so a client cannot extend a reservation by asking again), and allocates **nothing** — no second sequence is burned. |
| `POST` | `/v1/mint` | bearer | 400 / 403 / 405 / 413 / 415 | `op` not `send`/`broadcast`, or `idempotency_key` empty, over 128 bytes, or containing a byte outside `[A-Za-z0-9._-]`; sender not on the roster (403); any method but `POST` (`Allow: POST`); body over `MaxMessageRequestBytes`; `Content-Type` not `application/json` |
| `POST` | `/v1/mint` | bearer | 503 | `hub.MaxOutstandingMintsPerAgent` (64) or `hub.MaxOutstandingMints` (8192) reached — `Retry-After: 5`, **fails closed and evicts no other agent's reservation**; **or** the hub cannot durably accept (`hub.ErrNotDurable` / `hub.ErrPoisoned`) — **no** `Retry-After` |
| `POST` | `/v1/broadcast` | bearer | **501** | **CHANGED 2026-08-07 (SIGN-6) — this is a REGRESSION of a working feature, deliberately.** Answered immediately after authentication and **before the body is decoded**. Body: `{"error":"a broadcast cannot be signed under signing format v1: the canonical format requires a non-empty recipient set and the canonical audience of a broadcast is SIGN-3's undecided question; SIGN-6 admits no unsigned message type, so this route is refused rather than accepting unsigned traffic"}` — pinned as the constant `httpapi`'s `broadcastUnsignableReason`. `hub.Broadcast` and the whole broadcast write path are INTACT and tested; only the ROUTE refuses. SIGN-3 re-opens it. |
| `POST` | `/v1/send` | bearer | 201 | `{"message_id":"<bus-id>-<seq>","seq":N,"from":"<authenticated sender>","broadcast":false,"to":["<recipient>"],"sent_at":"<RFC3339Nano UTC>","content_sha256":"<hex>"}` — `SendResponseBody` is UNCHANGED, and is returned **only after the message is committed and fsynced** (invariant 4). **Request is BREAKING as of 2026-08-07 (SIGN-6)**: `{"to":"<bus>.<agent>","body":"<standard base64>","idempotency_key":"<the SAME key the mint used>","sender":"<bus-id>.<agent-id>","message_id":"<bus-id>-<seq>","seq":N,"timestamp_ms":1754570000000,"signature":"<standard base64 of exactly 64 bytes>"}`. The last five fields are **REQUIRED**; a pre-SIGN-6 client is rejected. |
| `POST` | `/v1/send` | bearer | 400 | `body` missing/empty, not standard base64, or over `store.MaxBodyBytes` (64 KiB decoded); `idempotency_key` empty, over 128 bytes, or containing a byte outside `[A-Za-z0-9._-]`; `to` not a well-formed fully-qualified `<bus-id>.<agent-id>`. **Plus the SIGN-6 shape checks:** `signature` absent/empty (`"a signature is required"`); `signature` not valid **strict** standard base64 (`"signature is not valid base64"`); decoded `signature` not **exactly** 64 bytes — 63 and 65 are both refused, there is no tolerance and no truncation (`"signature must be exactly 64 bytes"`); `message_id` malformed, minted by ANOTHER bus, or disagreeing with `seq`, or `seq == 0` (`"invalid message id"`); `timestamp_ms <= 0` (`"timestamp_ms is required"`) |
| `POST` | `/v1/send` | bearer | 403 | `{"error":"sender is not enrolled on this bus"}` — authenticated, but not on the roster; **or** `{"error":"sender does not match the authenticated caller"}` (SIGN-6) — the `sender` field is INPUT TO VALIDATE, never an identity. 403 rather than 400 because the request is well formed and re-sending it will not help. **The sender-mismatch case carries `Connection: close` and the server closes the socket** (invariant 10's replay clause, added 2026-08-08) — but ONLY when the claimed `sender` PARSES as a well-formed, fully-qualified `<bus-id>.<agent-id>` (invariant 2). The `sender` is inside the signed bytes, so naming a real agent you did not authenticate as is how a replayed third-party message presents. A claim that is ABSENT, unqualified, or carries stray whitespace names nobody: it is still 403, but it does **not** disconnect, because those are the shapes an honest single-identity client reaches by mis-filling the field. The check runs BEFORE `hub.Send`, so a disconnected caller consumed no idempotency key, wrote no WAL record and delivered nothing. The roster-miss case does **not** disconnect. |
| `POST` | `/v1/send` | bearer | 404 | `{"error":"unknown recipient"}` — `to` is well-formed but not enrolled on this bus **and not routable to a configured peer**. Nothing is written. **CHANGED 2026-08-15 (`RELAY-24-BLOCKER-EGRESS`):** a recipient whose bus half names a peer this bus has seeded into its routing table is no longer 404 — it is **accepted (201)** and forwarded; see [Cross-bus send](#cross-bus-send-relay-24-blocker-egress-2026-08-15). A recipient on an UNCONFIGURED bus, or on a bus with no peer store, is still 404 exactly as before. |
| `POST` | `/v1/send` | bearer | 409 | `idempotency_key` reused with a **different** payload — a protocol violation, not a retry (invariant 10). Rejected and logged; **the connection is KEPT** — narrowed 2026-08-08, this row carried `Connection: close` until then. **Or (SIGN-6, and these do NOT disconnect either):** `hub.ErrUnknownMint` — no outstanding reservation for this key, which is ROUTINE after a bus restart because the mint table is memory-only; the client re-mints under the same key, re-signs and re-sends. **Or** `hub.ErrMintMismatch` — the `message_id`/`seq` presented are not the ones minted for this key; never routine. |
| `POST` | `/v1/send` | bearer | 405 / 413 / 415 | any method but `POST` (`Allow: POST`); body over `httpapi.MaxMessageRequestBytes` (128 KiB); `Content-Type` not `application/json` |
| `POST` | `/v1/send` | bearer | 503 | applied-key table at `hub.MaxIdempotencyEntries` (65536) — `Retry-After: 5`; **or** the hub cannot durably accept messages (`hub.ErrNotDurable` / `hub.ErrPoisoned`) — **no** `Retry-After`, because that is not transient |
| `GET` | `/v1/messages` | bearer | 200 | `{"messages":[<message>...],"cursor":"<opaque>","more":false,"timed_out":false}` — history from a cursor; never parks. Query: `?cursor=<opaque>&limit=<1..256>` |
| `GET` | `/v1/wait` | bearer | 200 | Same body. Parks until a visible message arrives or the deadline passes. Query: `?cursor=<opaque>&limit=<1..256>&timeout=<1..300 seconds>` |
| `GET` | `/v1/messages`, `/v1/wait` | bearer | 200 | **A long-poll timeout is a 200**, with `"messages":[]`, `"timed_out":true` and the **same `cursor` that was sent**. It is never an error status: a quiet bus is the steady state. |
| `GET` | `/v1/messages`, `/v1/wait` | bearer | 400 | `cursor` malformed, not base64url, or **bound to a different agent**; an **unknown cursor version is NOT a 400** — it is accepted and remapped to the start of the retained window (see below); `limit` not a positive integer or over 256; `timeout` not a positive whole number of seconds or over 300 |
| `GET` | `/v1/messages`, `/v1/wait` | bearer | 403 | `{"error":"sender is not enrolled on this bus"}` — authenticated, but not on this bus's roster. The read paths **fail closed** rather than returning an empty batch; see the enrolment epoch below for why an unknown reader must never be read with no epoch. |
| `GET` | `/v1/wait` | bearer | 409 | **CHANGED (POLL-CONCURRENT-WAITERS, 2026-09-01).** `{"error":"another long poll is already active for this agent; only one /v1/wait may be active per agent at a time — retry once the other poll returns"}` — message delivery is now **single-active per agent id** (`hub.ErrPollActive`): the FIRST poll holds the delivery slot and a SECOND concurrent poll for the SAME authenticated id is REFUSED, not parked. This closes a real defect where two parked polls on one identity SPLIT delivery — a DM meant for an interactive session could be woken on a background monitor on the same id. Rejected and logged at Debug; **the connection is KEPT** and there is deliberately **no `Retry-After`** (invariant 10: a buggy client running two pollers must not be dropped, and a 409 — not the 503 this row returned until 2026-09-01 — keeps the CLI from looping the refusal). The slot is released on every exit path of the holding poll (return, timeout, cancel, disconnect), so the agent is never locked out of its own inbox. |
| `HEAD` | `/v1/agents`, `/v1/messages`, `/v1/wait` | bearer | 200 | **Added 2026-08-08 (CORE-7).** Accepted exactly as on the unauthenticated GET routes: same status, same headers, no body. Authentication is unchanged — `HEAD` goes through the same default-deny `authMiddleware` as `GET`, so an anonymous `HEAD` is 401. Safe because every `requireGET` route is a pure read: the cursor is the client-supplied `after`/`cursor` parameter, so a `HEAD` consumes and advances nothing a later `GET` needed. |
| `GET` | `/v1/messages`, `/v1/wait` | bearer | 405 | any method but `GET` or `HEAD`; `Allow: GET, HEAD` (was `GET` before CORE-7) |
| `GET` | `/v1/wait` | bearer | (none) | A **cancelled request context** (client hung up, or server shutting down) writes no response at all — there is nobody to write to. Distinct from a timeout, which is a 200. |
| `GET` | `/v1/ack/<correlation-key>` | bearer | 200 | **NEW (`ACK-9`, 2026-08-16).** Sender-visible delivery status; `{"rows":[...]}`, never empty/null. **200 whatever the key is** — never existed, swept, somebody else's and malformed are one byte-identical answer, and there is no 400/403/404 branch **on the key**. (The 400 and 429 rows below are judgements about the caller's own `wait` parameter and its own parked-request count, not about the key.) See `## The SENDER-visible ack-status route` near the end of this file for the full oracle rule, the `?wait=` long-poll and the response shape. Registered only when `Options.AckStatus` is set; otherwise this path 404s through the catch-all. |
| `GET` | `/v1/ack/<correlation-key>` | bearer | 400 | `wait` present but not a positive whole number of seconds, or over `hub.MaxPollTimeout` (300s) — refused, never clamped |
| `GET` | `/v1/ack/<correlation-key>` | bearer | 429 | `wait` requested while this **principal** already has `maxParkedAckStatusPerAgent` (32) requests parked on this route. `Retry-After: 1`. Decided from the caller's own parked count and **nothing about the key**, so an unknown key and a live non-terminal one are refused identically. See the limits table below. |

A `<message>` on the read path is (`timestamp_ms` and `signature` **added 2026-08-07, SIGN-6**;
`correlation_key` **added 2026-08-22, `ACK-12-FU-WATCH-CORRELATION-KEY`**):

```json
{"message_id":"<bus-id>-<seq>","correlation_key":"<origin-bus-id>-<seq>","seq":42,
 "from":"<bus>.<agent>","broadcast":false,
 "to":["<bus>.<agent>"],"bus_path":["<bus-id>"],"sent_at":"<RFC3339Nano UTC>","size":11,
 "content_sha256":"<hex sha256 of the decoded body>",
 "timestamp_ms":1754570000000,"signature":"<standard base64 of 64 bytes>",
 "body":"<standard base64>"}
```

**`correlation_key` and `message_id` are two different facts and the read path carries both.**
`message_id` is **this** bus's server-minted id for the copy it is serving. `correlation_key` is the
**ORIGIN** bus's server-minted id — `ACK-CONTRACT.md` §3's correlation key — and it is the only id
`POST /v1/ack` accepts, because that route binds a **(correlation key, recipient)** row. The two are
**equal when this bus is the origin** and **differ when the message was relayed here**, which is
precisely why the field exists: a recipient holding only `message_id` could name the right row for a
local message and never for a relayed one.

It is derived **server-side**, in `toWireMessage` (`internal/httpapi/messages.go`), from
`store.Message.OriginID()` — never from `Message.OriginMessageID` directly, which is empty on a
message this bus minted. `OriginID()` is the one place the "origin id when set, local id otherwise"
rule is written down, and its own doc forbids re-spelling that branch at a call site: the wrong
branch still returns a well-formed message id, just naming the wrong bus's message. Deriving it here
is what keeps the branch out of `client/` and out of `cmd/agent-busctl`, neither of which can import
`internal/store`. It is the origin's id **carried**, never re-minted or adopted as an identity
(invariant 1), and it is bus-namespaced (invariant 2), so it is not derivable from `bus_path[0]`.

**It is NEVER `omitempty`.** The server always knows the value — `OriginID()` falls back to `ID`,
which is always set — so it always sends it, and `jq -r .correlation_key` can never yield `null`
against a current bus. Omitting it on a same-bus message would push `.correlation_key //
.message_id` into every consumer, which is that same origin/local branch re-spelled one layer out;
and in `jq` the empty string is truthy while `null` is not, so that idiom would fall through to the
wrong id silently rather than failing loudly.

The name is its **purpose**, not an identity claim. It is the same field name the ack plane already
uses — the `correlation_key` of the `POST /v1/ack` request body and of `internal/relay`'s peer ack
frame — and `OriginID()`'s doc is explicit that the value is "a CORRELATION key, not an identity"
that "must never be served as 'the message id'".

**`sent_at` and `timestamp_ms` are two different facts and MUST NOT be conflated.** `timestamp_ms`
is an `int64` of Unix milliseconds UTC, it is the **SENDER's** clock, and it **IS covered by the
signature**. `sent_at` is unchanged: it is **this bus's** clock and is **NOT covered**. A recipient
verifying against `sent_at` fails every time, and the reason is not obvious from the field names —
which is exactly why it is stated here. A recipient reconstructs the signed bytes from
`message_id`, `seq`, `from`, `to`, `timestamp_ms` and `body`; `bus_path` is deliberately NOT covered
(settled in SIGN-1, `PROTOCOL.md` §8.3).

**On a RELAYED message the first two of those are the ORIGIN's — i.e. `correlation_key` and the
sequence half parsed out of it, not the `message_id`/`seq` beside them** (noted 2026-08-22 while
adding `correlation_key`; the sentence above is stated for the bus that minted the message and was
never narrowed). The signature was made by the sender on the origin bus before any hop existed, so
the preimage can only carry the origin's pair: `RelayedMessage.CanonicalBytes`
(`internal/relay/signed.go`) passes `MessageID: m.OriginMessageID, Sequence: m.OriginSeq`. On the
origin bus the two ids are the same string and the distinction is invisible, which is how a verifier
passes every same-bus test and then fails on the first relayed message — and it fails EARLIER than a
mismatched signature: `signing.Canonicalize` requires the sender's bus half and the message id's bus
half to agree (`internal/signing/canonical.go`), and on a relayed message `from` names the origin bus
while the local `message_id` names this one, so canonicalization is refused before any signature is
compared. `sent_at` and `bus_path` are unaffected and remain uncovered. **Nothing verifies signatures
on the read path today** (see the enforcement note in `INVARIANTS.md`), so this is a trap for whoever
wires verification on, not a live break — and `client.Message.signingMessage`
(`client/canonical.go:215-218`) currently feeds the LOCAL pair, which must be settled before that
happens. **Reported, not fixed, by this task.**

`HealthResponse` / `InfoResponse` / `ErrorResponse` types live in `internal/httpapi/server.go`. `EnrolRequestBody` / `EnrolResponseBody` / `SessionBeginRequestBody` / `SessionBeginResponseBody` / `SessionCompleteRequestBody` / `SessionCompleteResponseBody` live in `internal/httpapi/auth.go`. `AgentsResponseBody` / `MintRequestBody` / `MintResponseBody` / `BroadcastRequestBody` / `SendRequestBody` / `SendResponseBody` / `WireMessage` / `BatchResponseBody` live in `internal/httpapi/messages.go`. (`BroadcastRequestBody` is still declared but is **never decoded** — the route refuses before reading the body.)

**`POST /v1/ack`** (`ACK-6`, 2026-08-16) is a messaging route too and is registered in this same
block, so it authenticates identically. It is documented in full under
`## The RECIPIENT ack route` at the end of this file rather than as a row here, because its answer
is deliberately NOT expressible as one status per situation: the same **200** carries a
recorded acknowledgement, §13.3's uniform `unknown` refusal, and — since `ACK-5` (2026-08-21) — a
**transit** acknowledgement carried one hop back toward the origin bus; and compressing that into a
table row is how somebody comes to read the status line instead of `accepted`. (This sentence said
"both … and" while there were two.)

**`GET /v1/ack/<correlation-key>`** (`ACK-9`, 2026-08-16) is the **SENDER**-visible counterpart and a
**separate route** — `http.ServeMux` resolves the bare `/v1/ack` path above and this subtree
independently, so the two coexist without colliding. It is registered OUTSIDE this block (see the
routes table above and `## The SENDER-visible ack-status route` near the end of this file), because it
reads a durable table rather than the messaging surface.

### Cross-bus send (`RELAY-24-BLOCKER-EGRESS`, 2026-08-15)

**`POST /v1/send` now ACCEPTS a recipient on a peer bus. It used to answer 404.**

The change is entirely in what the route ADMITS; the request shape, the response shape, every status
code and every header are unchanged, and there is no new route, header or field.

| | |
| --- | --- |
| What decides it | The **bus half** of `to`. `relay.Registry.Route` resolves `<bus-id>` against the peers seeded at startup from the durable peer-route records; it does **not** consult any exchanged roster (invariant 2 — the id names its own owner). An agent that enrolled on the peer since the last roster sync is routable. |
| When it is still 404 | The bus half is this bus (then it is an ordinary local lookup), or it names no seeded peer, or the peer store failed to build, or the server has no peer configuration at all. Nothing is written in any of those cases. |
| What a 201 means | **DURABLE ON THIS BUS, AND NOTHING MORE.** The message is committed and fsynced here (invariant 4). It does **NOT** mean the peer received it, and it does **not** promise an outbox record exists either: three paths return 201 with no `pending` record written — the sender cannot be attested (no messaging public key on the roster), the route has no usable base URL (`internal/relay/forward.go`), or the outbox enqueue itself fails (capacity, or the durable write). Each is counted and logged on this bus; none of them changes the status code, because the local send was already acknowledged before the hop was attempted. |
| Response body | Unchanged (`SendResponseBody`). It carries **no** peer, route, hop or delivery field, deliberately: the hop's outcome is not known when the response is written and adding a field that could only ever say "unknown" would invite clients to branch on it. |
| Delivery semantics | **At-least-once.** The peer may see duplicates (crash between send and settlement, retry after timeout, two paths in a cyclic topology). Invariant 10 absorbs them at the receiving bus; the recipient deduplicates on `message_id`, which is the ORIGIN bus's id and is stable across copies. |
| Failure of the hop | Invisible to the caller. Retried in the background to a bounded horizon, then settled `abandoned` and logged on this bus. There is no delivery receipt on this protocol. |
| Onward relay | **Implemented as of `RELAY-47` (2026-08-15).** See the section below — this row said "still not implemented" and was true until that task. |

### Onward relay: an intermediate bus carries a peer's message to a THIRD bus (`RELAY-47`, 2026-08-15)

**`POST /v1/peer/relay` may now cause OUTBOUND work.** A relayed message naming a recipient on a bus
that is neither this one nor the sender's is carried to the next hop instead of stopping here. There
is **no new route, no new header, no new field and no change to any status code** — the wire surface
is identical, and only the behaviour behind it moved. A→B→C works; before this, B was a leaf.

| | |
| --- | --- |
| What a 200 still means | **DURABLE ON THIS BUS, AND NOTHING MORE** — unchanged, and deliberately so (operator ruling, 2026-08-15). It does **not** mean the further hop was taken, and there is still no delivery receipt on this protocol. Asynchronous delivery notification is epic `ACK`, and nothing here depends on it. |
| What is forwarded | The envelope **verbatim**: the ORIGIN bus's attestation, the SENDER's signature, the body, the recipient set and the signed timestamp. An intermediate re-signs nothing and re-attests nothing (invariant 2), and the destination verifies against the **origin** bus's pinned signing key, not against ours. The **only** field that changes on a hop is `bus_path`, which gains exactly this bus's id (`relay.AppendHop`). |
| When it is forwarded | **Only on a NEW acceptance.** A duplicate (same origin message id, same content) is answered with the original local id and forwarded **nowhere** — that is what terminates traffic in a cyclic federation (invariant 10). |
| Where it can go | Only a peer this operator configured: the destination bus half is resolved through `relay.Registry`, seeded from durable peer-route records, and the address is dialled with the same address-keyed TLS pin as any other hop. **Nothing in a peer-supplied envelope names an address, host or scheme.** |
| Loop control | The egress split horizon (`relay.NextHopAllowed`) drops a next hop already on the path before a byte leaves the process; `relay.AppendHop` refuses to stamp a second visit and enforces the 64-hop `store.MaxBusPath` limit; and `relay.CheckIncomingPath` re-derives the decision at ingest, because a peer's path is untrusted input (`PROTOCOL.md` §8.5). None of these is sufficient alone, and none substitutes for idempotency. |
| Fan-out bound | **At most 8 distinct foreign destination buses per relayed message** (`maxOnwardBusesPerMessage`, `cmd/agent-bus/relaywiring.go`). An onward hop is outbound work a peer asks this bus to do, and each destination costs two fsyncs before the ingest returns. Combined with the per-peer in-flight ingest cap of 8, one authenticated peer can hold at most 64 onward copies in flight. A message over the bound is accepted, carried **nowhere**, and logged individually. The count is taken from the envelope's recipients rather than from the resolved next hops (`RELAY-47-FU-FANOUT`). |
| When it is still carried no further | No route to the destination bus, the next hop is already on the traversed path, the path is at the hop limit, or the fan-out bound was exceeded. A message that reaches **no** peer at all is logged individually at WARN, naming `origin_message_id`, with a remedy (invariant 6). **A message that reaches SOME of its destinations and not others is NOT individually logged** — `relay.Forwarder.targets` counts an unroutable recipient without a line, and "queued fewer copies than destinations" is not a sound detector because the split horizon legitimately drops a destination already on the traversed path. Gap recorded, not papered over: `RELAY-50`. |
| Crash-safe (since `RELAY-48`) | A pending onward hop **is** re-offered after a restart. The durable outbox record survives AND the intermediate now retains the origin's attestation on `store.Record`, so `Resume` can rebuild the envelope. **This row previously said the opposite** — until `RELAY-48` the job settled as abandoned (logged at WARN) after this bus had already answered the upstream peer 200. Locally-originated hops were always unaffected and DO resume. Note the behaviour is not live until a rebuilt binary runs: a relayed message already on disk carries no attestation, so its onward hop stays unrecoverable. |
| Startup | The `FEDERATION is served` line reports `onward_relay=true` on a bus with a forwarder. A bus with no peer configuration has no forwarder, reports `onward_relay=false`, and is a leaf — which is a supported configuration, not a fault. |

The outbound link is mutually-authenticated TLS pinned **by the address dialled**, against the route
record's `next_hop_tls_cert_sha256` (`CONTRACTS-ONDISK.md`). An address with **no** configured pin is
**refused before a socket is opened** — there is no CA and no trust-on-first-use (invariant 11), so an
unpinned peer is never dialled. The certificate this bus presents as a client is its own serving leaf
("one identity, both directions").

### Signed sends: reserve-then-send, and what the bus does and does not check (SIGN-2/SIGN-6, added 2026-08-07)

**A send is now a TWO-STEP.** SIGN-1 settled that the sender's signature covers the ORIGIN bus's
minted message id and sequence (`PROTOCOL.md` §8.4, option (a)), and invariant 1 makes the server
authoritative on those, so a client cannot sign until the bus has given them to it:

1. `POST /v1/mint` with `{"op":"send","idempotency_key":"<k>"}` → `{message_id, seq, sender, op, expires_at}`.
   The number is **durably burned before it leaves the process** (`CONTRACTS-ONDISK.md`, the
   `"seqfloor"` record).
2. The client canonicalizes `{message_id, seq, sender, recipients, timestamp_ms, body}` and signs
   with its **MESSAGING** Ed25519 private key — a key distinct from its **AUTH** enrolment key.
3. `POST /v1/send` with the reservation, the sender's `timestamp_ms`, and the 64-byte detached
   signature, **under the SAME idempotency key `k`**.

**The bus enforces SHAPE. The recipient enforces AUTHENTICITY.** The bus does **NOT** verify the
signature and must never be given the ability to: it does not hold the sender's messaging key, and a
bus that could verify could equally forge. Every check in the 400/403 rows above is a
well-formedness check; none of them is a cryptographic one.

**Every SIGN-6 check runs before `hub.Send` is called.** A rejection therefore leaves **no WAL
record, no delivery, no ack, and no sequence consumed by that request**. (The reservation from step 1
was burned earlier — a separate, deliberate, earlier act; an unspent mint is a permanent hole in the
sequence and that is correct, see `CONTRACTS-ONDISK.md`.)

**A SIGN-6 rejection is TERMINAL for its idempotency key, not transient.** There is deliberately no
`Retry-After` on any of them. The same key re-presented with the same malformed request is rejected
identically for ever; re-presented with a REPAIRED one it is a different payload under a used key,
which invariant 10 already answers with 409 (rejected and logged; the connection is kept — see
`## Headers`). Dressing a permanent refusal as
retryable is how a client ends up in a loop that can never succeed.

**KNOWN GAP — a recipient cannot obtain a sender's messaging public key from this bus.** Nothing
registers a messaging public key at enrolment (`auth.Service.Enrol` leaves
`RosterEntry.MessagingPublicKey` ZERO), `GET /v1/agents` carries no key material, and CRYPTO-4 (the
server-attested key-bundle endpoint) does not exist. So verification is possible **only** against a
key obtained **out of band**. That is the honest state of the world; there is no TOFU fallback, no
"trust the key the bus handed over", and none may be added.

`/v1/info`'s payload is deliberately minimal (see `DECISIONS.md`, 2026-08-02, and its 2026-08-07
addendum on `/v1/discovery`): `bus_id`, `version`, `uptime_seconds`, and (added 2026-08-07,
DISCOVERY-DOC) `discovery`. That fourth field is safe precisely because it adds no information: its
value is the compile-time constant `httpapi.RouteDiscovery` (`"/v1/discovery"`), identical in every
build and independent of this bus's identity, state and configuration — it exists only so a caller
that already knows `/v1/info` can find the protocol-discovery document (`GET /v1/discovery`, see
`### Discovery document` below) instead of guessing the path. A test pins the exact field set — do
not add data-dir, listen address, peer list, or agent roster here without updating that test and
recording the decision.

**Authentication is now default-deny across the whole mux** (AUTH-2, with AUTH-6's fail-open fix
folded into the same change). `authMiddleware` wraps `s.handler` before any route is dispatched, so a
route is authenticated the moment it is registered through `(*Server).route` — nobody has to remember
to protect it individually, which closes the exact risk AUTH-6 flagged (routes wired one at a time,
easy to forget on the next addition). The allow-list is exactly the six paths named in the routes
above (added 2026-08-07: `/v1/discovery`); see `## Authentication` further down for the full
contract.

## Messaging: delivery guarantee, cursors, retention (added 2026-08-02)

### Delivery is AT-LEAST-ONCE. It is not exactly-once and must not be described as such.

A message may be delivered to a recipient **more than once** — after a client retry, a reconnect with
a stale cursor, or (once the RELAY epic lands) a cyclic peer topology. What the bus guarantees is:

- **No acknowledged message is lost through our own write path.** `POST /v1/send` returns 201 only
  after the message is committed through the two-phase prepare→commit path and fsynced (invariant 4).
  A crash before the 201 may leave the message absent; a crash after it may not. (`POST /v1/broadcast`
  used to carry the same guarantee and no longer answers at all — it is 501 since 2026-08-07. The
  write path beneath it is unchanged; only the route refuses.)
- **Every message is delivered whole or not at all.** Recovery never serves a torn record: a
  message that survives carries its original sender, recipients, body and content hash, and the hash
  is re-verified on the way back off disk.
- **The order is total, stable and server-assigned — but it is NOT sequence order.** Every message
  carries a server-minted `seq` (invariant 1) that is unique and never reused, and is read back in
  ascending **delivery-position** order — a second server-minted number, stamped from the WAL commit
  index. Because a sequence is minted at *reservation* time and reservations may be spent out of
  order, **the delivered `seq` stream is not ascending.** The order is total and stable in the sense
  that matters: every reader traverses the same order, that order never changes, and a message can
  never land behind a cursor that has already passed it. `seq` is an identity; the cursor is a
  position; they are different numbers and are not comparable.

Duplicates are absorbed by invariant 10 on the WRITE side (idempotency keys) and are expected to be
tolerated by the reader. A client that must not act twice on one message should key on `message_id`.

### The cursor

Opaque, versioned, base64url. It encodes a **position and nothing else**, and it is **bound to the
agent it was issued to**: presenting agent A's cursor as agent B is a 400.

**The cursor is an opaque, agent-bound delivery position, and its format is versioned.** Clients
must treat it as bytes: pass back exactly what you were given. The current format version is `v2`
(RESERVED from the Spec Server `cursor-format-version` namespace, not chosen by hand). A cursor
carrying a version this build does not issue is **accepted and remapped to position 0** — one replay
of the retained window — rather than rejected.

That asymmetry is deliberate and is a correctness requirement, not a convenience. A 400 here is not
a recoverable error for a real client: it surfaces as a rejection the watch loop does not retry, and
nothing in the client clears the stored cursor, so the same rejected value is re-read from disk and
re-presented on every restart, for ever. Remapping costs one duplicate-delivery burst, which
at-least-once delivery already requires every client to tolerate. Rejecting costs the agent its
entire message stream, permanently.

**The agent binding is still enforced for an old-version cursor.** A `v1` cursor issued to a
DIFFERENT agent is still a 400 — the version remap happens only after the agent-id check, never
before it.

- An **absent or empty** `cursor` means position 0 — "I have seen nothing" — so a fresh agent reads
  back through the whole retained window, paginated.
- A **non-empty batch** returns the **delivery position** of its last message as the next cursor —
  NOT its sequence. The two are different server-minted numbers (see the delivery-order bullet
  above); a cursor is never comparable to a `seq`.
- An **empty batch returns the cursor unchanged**, byte for byte. This is what makes a long-poll
  timeout resumable, and it is the safe direction: a cursor is never advanced past messages the
  caller was not handed.
- The cursor is **not signed**, deliberately. Forging one for yourself replays or skips your own
  messages (self-inflicted, and at-least-once already permits the replay); forging one for another
  agent gains nothing, because **visibility is filtered with the authenticated principal, never with
  the cursor**. A MAC would protect a value whose integrity buys no security property (invariants 8
  and 9).

### Visibility

| Message | Visible to |
| --- | --- |
| broadcast | every agent **except the sender** — the rule still governs any broadcast already on disk, but **no new broadcast can be created**: `POST /v1/broadcast` answers 501 since 2026-08-07 |
| direct (`/v1/send`) | the named recipient only — **not** the sender, **not** anyone else |
| any message sent **before** the reader's own enrolment | **nobody** — see the enrolment epoch below |

The sender is excluded from its own message on purpose: an agent polling its own bus does not want
its traffic echoed back into the loop, and it already holds the `message_id` from the 201.

#### The enrolment epoch: you do not receive mail sent before you existed (added 2026-08-02)

**A message whose `sent_at` precedes the reader's own `enrolled_at` is never delivered**, whatever it
is addressed to. This closes a hole the messaging epic itself opened, found by the security gate:

Message records are durable and they name agent ids. **The paragraph that used to stand here said
"enrolment is not durable yet (AUTH-3)"; that is FALSE as of 2026-08-07 — AUTH-7 wired the durable
roster (see "Durability of the roster and sessions" below).** The hazard it described was real at the
time: with a memory-only roster the per-name suffix counter restarted at 1 on every boot, so anyone
who reached the unauthenticated `/v1/enroll` after a restart and guessed the name `alpha` was minted
`<bus>.alpha-1` — the id the *previous* alpha held — and would otherwise have read a full retention
window of that agent's direct messages. The bus could not tell the two apart **by id**, because an id
was exactly what was being reused. It could tell them apart **by time**, and no legitimate agent
needs traffic that predates its own enrolment.

**The rule is unchanged and still enforced**, and it now behaves the way it was designed to: a
durable roster restores each agent's **ORIGINAL** enrolment instant, so a genuinely continuous agent
keeps seeing everything sent since it first enrolled, across restarts. Nothing here had to be undone.

Consequences a client must know:
- An agent that enrols after a broadcast does not receive that broadcast. Join, then listen.
- An agent not on this bus's roster gets **403** from `/v1/messages` and `/v1/wait`, not an empty
  batch. Failing closed matters: reading with no epoch would disable the filter entirely.
- **Superseded:** "after a restart, no agent receives pre-restart history" was a consequence of the
  memory-only roster and is no longer true. An agent whose durable enrolment predates a message
  still sees that message after a restart.

Identity *continuity* (a new keypair inheriting an id with a prior history, and future messages
attributed to it) is **not** fixed by the epoch; it is logged at ERROR by the hub.

### Retention: 1 day or 1 GiB, whichever comes first

`store.DefaultMaxAge` = 24h, `store.DefaultMaxBytes` = 1 GiB (bodies only). Both bounds are enforced
together and the tighter wins; retention drops whole messages from the **oldest end only**, never
from the middle. A cursor that has fallen behind the retained window resumes at the **oldest
retained message** — the messages in between are gone. That is what a retention window means; it is
stated here rather than hidden, and it is the one case where at-least-once becomes at-most-once.

The **applied-key table follows message retention**: a key is forgotten when the message it produced
ages out. `hub.MaxIdempotencyEntries` (65536) is a memory backstop above that, and it **fails closed**
(503) rather than evicting — evicting a remembered key under pressure silently turns the next retry
of it into a second message, which is the double-apply invariant 10 forbids.

Be precise about what that does and does not promise, because `DECISIONS.md` item 9 (2026-08-02) is
stricter on one axis: a retry arriving **after the retention window** — more than a day after its
send — is treated as a **fresh send and produces a second message**. It is not rejected. That is a
deliberate, dated narrowing recorded in `DECISIONS.md` under the MSG/POLL wave: fail-closed is
honoured on the axis that matters (**never evict under pressure**), while the *window* is set to
message retention, which is orders of magnitude beyond any plausible client retry. Rejecting a
day-old key would require remembering every key ever used, for ever, which is the unbounded growth
the cap exists to prevent. `IDEM-11` owns the cross-cutting layer and may revisit it.

### Long polling

| Symbol | Value | Meaning |
| --- | --- | --- |
| `hub.DefaultPollTimeout` | 30s | Used when the client sends no `timeout` and the server was configured with none. |
| `hub.MaxPollTimeout` | 5m | Hard ceiling. A `timeout` above it is **refused with 400**, not silently clamped — a client that asked for an hour and got five minutes would conclude the request was dropped. |
| `hub.DefaultBatchLimit` | 64 | Batch size when `limit` is absent. |
| `hub.MaxBatchLimit` | 256 | Ceiling on `limit`; above it is a 400. |
| `store.MaxBatchBytes` | 1 MiB | Ceiling on one batch in **body bytes**, enforced alongside `limit`. Count alone is the wrong unit: 256 × 64 KiB is 16 MiB of body, which is then base64-encoded and marshalled, so one request would cost ~45 MiB of live allocation. **At least one message is always returned** even if it alone exceeds the budget, so a large message can never become undeliverable to a client that pages politely. Hitting it sets `more: true`. |
| `hub.MaxWaitersPerAgent` | 1 | **CHANGED to 1 (POLL-CONCURRENT-WAITERS, 2026-09-01); was 32.** Message delivery is **single-active per agent id**: one parked `/v1/wait` poll per agent. A SECOND concurrent poll for the same id gets **409** (`hub.ErrPollActive`), **no `Retry-After`**, connection KEPT. Fails closed, evicts nothing. This is a **correctness** bound, not a resource one: allowing several parked polls for one identity meant the wake loop SPLIT a message among them, so a DM meant for an interactive session could be delivered to a background monitor on the same id and never reach the session. Keyed on the agent id, which is safe here for the same reason `auth.MaxActiveSessionsPerAgent` is: this route is authenticated, so the key is a proven, fully-qualified identity and one identity can only refuse itself. The slot is released on every exit path of the holding poll. |
| `maxParkedAckStatusPerAgent` | 32 | (`ACK-9`) Concurrent parked `GET /v1/ack/<key>?wait=` requests per agent. A 33rd gets **429** with `Retry-After: 1`. Fails closed, evicts nothing, keyed on the authenticated principal. **It exists because this route parks even when there is nothing to report**: returning early on an unknown key would leak existence through latency, so a probe needs no valid key and is guaranteed to hold a connection for the full ceiling, waking every 200 ms onto `ack.Store`'s single global mutex — the same mutex `Accept` takes inside `Hub.publish`, so the cost lands on every **writer** on the bus. This is a pure **resource** bound (32 parked probes), and since POLL-CONCURRENT-WAITERS it is a DIFFERENT limit for a DIFFERENT reason from `hub.MaxWaitersPerAgent` above, which is now `1` for a CORRECTNESS reason (single-active message delivery) — do NOT re-tune this cap to match it. The status is **429 + `Retry-After: 1`** because the service is healthy and it is *this caller* that is over a quota, and the short retry matches a bound that frees as soon as any one parked request finishes. Self-starvation between two connections of the SAME agent is possible and accepted; cross-agent starvation is structurally impossible because the key is a proven identity. |

A parked poll holds **no goroutine of its own** — it parks the request's own goroutine on a select
over the request context, the deadline, and its wake channel. A client that vanishes mid-wait
releases within one scheduling turn, and `(*hub.Hub).WaiterCount()` returns to zero.

**The wake happens only after the commit is durable and applied** (POLL-2). A waiter is woken only
for a message it is entitled to see, and N messages arriving before it runs coalesce into ONE wake,
after which it re-reads the store and returns them all in one batch — so one broadcast wakes every
eligible waiter exactly once, with no duplicates and no misses.

### Where the hub comes from (transitional — 2026-08-02)

`httpapi.Options.Hub` is the intended wiring. Until `cmd/agent-bus` sets it, `httpapi.New` builds one
itself whenever `Options.Durable` also satisfies `Path() string` + `Recovered() wal.Recovered`
(i.e. it is a real `*wal.Log`). Two honest costs of that arrangement, both to be removed when main
passes the hub as the WAL's `Applier`:

- the durable log is **replayed twice** at startup (once as an fsck by `wal.Open`, once read-only to
  rebuild the store);
- a rebuild **failure cannot be fatal**, because `New` returns no error — it is logged at ERROR and
  the messaging routes are simply not registered, rather than serving a store disk does not justify.

## Conversation route: `POST /v1/conversations` — mint a durable, multi-party object (`CONV-CREATE-CLI`, added 2026-08-30)

Registered in `internal/httpapi/conversations.go`, **only when the composition root wires a
`ConversationCreator`** (`Options.Conversations`) — in `cmd/agent-bus` that is
`*store.ConversationStore`, the same table `wal.Open` replays and attaches, so a create and a
recovery can never disagree about a conversation (one table, not a serving copy of one). When no
creator is wired the route is not registered at all and 404s like any other unserved path, the same
convention the messaging and ack routes follow.

**It authenticates like every other route: it is NOT on the allow-list**
(`httpapi.UnauthenticatedRoutes()` — enrolment, session begin/complete, `/healthz`, `/v1/info`,
`/v1/discovery` only), so it is reachable only after `authMiddleware`'s default-deny resolves a live
session (invariant 3).

| Method | Path | Auth | Status | Response |
| --- | --- | --- | --- | --- |
| `POST` | `/v1/conversations` | bearer | 201 | `{"conversation_id":"<bus-id>.<uuid-v4>","creator":"<bus>.<agent>","name":"...","recipients":["<bus>.<agent>",...],"created_at":"<RFC3339Nano UTC>"}`. The server MINTS `conversation_id` and derives `creator` from the authenticated session (invariant 1) — request body: `{"recipients":["<bus>.<agent>",...],"name":"...","idempotency_key":"..."}`. **There is no `creator` field in the request and there never may be one** — an unknown field is rejected by the strict decoder (400), the same rule `SendRequestBody`'s absent `sender`-as-identity field follows. |
| `POST` | `/v1/conversations` | bearer | 201 + `Idempotency-Replayed: true` | A repeat of the same `(creator, idempotency_key)` with the SAME `recipients`/`name` returns the ORIGINAL record from the applied-key table, body byte-for-byte identical, and mints nothing (invariant 10). |
| `POST` | `/v1/conversations` | bearer | 400 | `store.ErrInvalidConversation` — a recipient id that is not a well-formed fully-qualified `<bus-id>.<agent-id>` (invariant 2), an empty or duplicate recipient list, more than `store.MaxConversationRecipients` (64) recipients, or a `name` over `store.MaxConversationNameBytes` (128) bytes, carrying a newline/control codepoint, or otherwise not a single-line printable label — REFUSED, never truncated (`CONV-NAME-INV6`). Also 400: no `idempotency_key` (`idem.ErrMissingKey`), or a key that is empty, over 128 bytes, or outside `[A-Za-z0-9._-]` (`idem.ErrInvalidKey`/`idem.ErrInvalidAgent`). |
| `POST` | `/v1/conversations` | bearer | 401 | No authenticated principal — the same `authMiddleware` default-deny every other messaging route gets, since this path is not on the allow-list. |
| `POST` | `/v1/conversations` | bearer | 409 | `store.ErrConversationKeyReused` — the SAME `idempotency_key` presented with a DIFFERENT `recipients`/`name` payload: a protocol violation, not a retry. Rejected and logged; **the connection is KEPT** (invariant 10 — the key is the caller's own, so this is overwhelmingly a buggy client, not an attacker). |
| `POST` | `/v1/conversations` | bearer | 503 | `idem.ErrCapacity` / `idem.ErrAgentQuota` / `store.ErrConversationCapacity` (the store's hard cap, `store.MaxConversations` = 65536, live create only — replay of an already-durable record is never refused) — `Retry-After` set, retryable; **or** `store.ErrConversationNotDurable` — this bus cannot durably record a conversation (the store exists but is not yet `Attach`ed to a WAL), refused rather than acknowledged undurably (invariant 4), **no** `Retry-After` because that is not transient. |

`ConversationCreateRequestBody` / `ConversationCreateResponseBody` live in
`internal/httpapi/conversations.go`. The route body is capped at
`httpapi.MaxConversationRequestBytes` (32 KiB) before it reaches the decoder — comfortable headroom
over the largest legitimate request (64 recipients × up to 150 bytes each, a 128-byte name, a
128-byte idempotency key, JSON overhead) and still finite; the STORE enforces the real bounds
(recipient count, id shape, name).

CLI (invariant 7): `agent-busctl conversation create` — see `CONTRACTS-CLI.md`'s Subcommands table
and `AGENT_PROTOCOL.md`'s conversations section.

## Conversation send routes: `POST /v1/conversations/mint` + `POST /v1/conversations/send` — address a message by conversation id (`CONV-SEND-BY-ID`, added 2026-08-31)

Registered in `internal/httpapi/conversationsend.go`, **only when the composition root wires a
`ConversationLookup` AND a `Hub`** (`Options.ConversationLookup`, `Options.Hub`) — in `cmd/agent-bus`
the lookup is the same `*store.ConversationStore` as `Options.Conversations`, so a create and a send
can never disagree about a conversation's membership. When either is absent the two routes are not
registered and 404 like any other unserved path.

Send-to-a-conversation is a **two-step, exactly like `/v1/mint` + `/v1/send`**, and for the same
reason: SIGN-6 requires every message to carry a signature, the signature covers the recipient SET,
and the bus never verifies it — so the client must SIGN the membership, which means the bus must
first TELL it the membership. `mint` resolves the conversation, checks the caller is a member, and
returns the reservation **plus the resolved member list to sign**; `send` re-resolves the membership
server-authoritatively at send time and publishes one directed multi-recipient message through the
hub's existing fan-out (`hub.SendConversation`, a thin wrapper over the same `publish` path `Send`
uses — there is no second delivery mechanism).

The **member set** is the conversation's creator plus its recipients, de-duplicated. The durable
message record stores that **expanded set** as its `recipients`, frozen at send time — the deliberate
OPPOSITE of a broadcast (which stores a FLAG to avoid freezing the roster), because for a conversation
the membership at send time is exactly what authorised delivery (`DECISIONS.md`, `CONV-SEND-BY-ID`).
The sender is excluded from its own copy by `store.Message.VisibleTo`, so every OTHER member receives
it. The set is bounded by `store.MaxRecipients` (**64**), not `signing.MaxRecipients`.

**Both routes authenticate**: neither is on the allow-list (`httpapi.UnauthenticatedRoutes()`), so
both are reachable only after `authMiddleware`'s default-deny resolves a live session (invariant 3).

| Method | Path | Auth | Status | Response |
| --- | --- | --- | --- | --- |
| `POST` | `/v1/conversations/mint` | bearer | 201 | `{"message_id":"<bus>-<seq>","seq":<n>,"sender":"<bus>.<agent>","op":"send","conversation_id":"<bus>.<uuid>","recipients":["<bus>.<agent>",...],"expires_at":"<RFC3339Nano UTC>"}`. Resolves the conversation, checks the caller is a MEMBER, reserves the id+sequence (`hub.Mint`, op `send`), and returns the member SET to sign. Request body: `{"conversation_id":"<bus>.<uuid>","idempotency_key":"..."}` — **no sender field** (invariant 1). |
| `POST` | `/v1/conversations/mint` | bearer | 201 + `Idempotency-Replayed: true` | A re-mint under the same `(sender, op, idempotency_key)` returns the SAME reservation, body byte-identical, allocating nothing (invariant 10). |
| `POST` | `/v1/conversations/send` | bearer | 201 | `{"message_id":...,"seq":...,"from":...,"broadcast":false,"to":[<the member set>],"sent_at":...,"content_sha256":...}` — the same `SendResponseBody` `/v1/send` returns. Request body: `{"conversation_id","body"(base64),"idempotency_key","sender","message_id","seq","timestamp_ms","signature"}` — the same signed fields as `/v1/send` with `to` replaced by `conversation_id`; **there is no `recipients` field** — the bus resolves the member set from the conversation (invariant 1). |
| `POST` | `/v1/conversations/send` | bearer | 201 + `Idempotency-Replayed: true` | Same key + byte-identical body is a legitimate retry: the ORIGINAL result from the applied-key table, nothing re-applied (invariant 10). |
| both | `/v1/conversations/{mint,send}` | bearer | 404 | The conversation does not exist, **or** the caller is not a member. The two are **byte-identical** on purpose: 404 leaks less than 403, so a non-member cannot probe for conversation ids it is not in (invariants 1–3; the distinction is logged server-side at Debug). |
| `POST` | `/v1/conversations/send` | bearer | 400 | The membership exceeds `store.MaxRecipients` (64) — a 64-recipient conversation whose creator is a 65th member cannot be delivered as one message; **or** the usual signed-send shape failures (`checkSignedMint`): signature not base64, not 64 bytes, absent; `message_id`/`seq` malformed or from another bus; `timestamp_ms` ≤ 0. |
| `POST` | `/v1/conversations/send` | bearer | 403 | `checkSignedMint` check (d): the `sender` field is not the authenticated caller. As on `/v1/send`, a well-formed id for a DIFFERENT agent also drops the connection (invariant 10's replay clause); a malformed claim keeps it. |
| `POST` | `/v1/conversations/send` | bearer | 404/409/503 | The recipient/idempotency/durability outcomes of the underlying send (`writeHubError`): `ErrUnknownRecipient` (a member left the roster) → 404; `ErrIdempotencyKeyReused`/`ErrUnknownMint`/`ErrMintMismatch` → 409; capacity/`ErrNotDurable` → 503. |

The mint body is capped at `httpapi.MaxConversationRequestBytes` (32 KiB) and the send body at
`httpapi.MaxMessageRequestBytes` (128 KiB) before the decoder. `ConversationMintRequestBody` /
`ConversationMintResponseBody` / `ConversationSendRequestBody` live in
`internal/httpapi/conversationsend.go`.

CLI (invariant 7): `agent-busctl conversation send` — see `CONTRACTS-CLI.md`'s Subcommands table and
`AGENT_PROTOCOL.md`'s "Send to a conversation" section.

## Headers

| Header | Direction | Rule |
| --- | --- | --- |
| `X-Request-Id` | in/out | Inbound value accepted only if it matches `[A-Za-z0-9._-]{1,64}` (`httpapi.MaxRequestIDLen = 64`); otherwise replaced with a server-generated id (`crypto/rand` 16 hex chars, falling back to a `seq-<n>` counter). Always echoed on the response. |
| `Authorization` | in | Required on every route off the six-entry allow-list (`## Authentication`). Exactly one header, form `Bearer <token>` (scheme case-insensitive); `<token>` must be non-empty, contain no space, be no longer than `httpapi.MaxBearerTokenLen` (512), and consist only of the base64url alphabet `[A-Za-z0-9_-]`. Zero headers, more than one, a non-`Bearer` scheme, or a token failing any of those checks is treated as "no usable credential" (401, `error="invalid_request"`) — distinct from a syntactically fine token that simply does not authenticate (401, `error="invalid_token"`). Never logged, echoed, truncated or hashed into any response or log line — only the resulting agent id ever leaves `authMiddleware`. |
| `WWW-Authenticate` | out | On every 401: `Bearer realm="agent-bus", error="invalid_request"` when no usable credential was presented, or `Bearer realm="agent-bus", error="invalid_token"` when a well-formed token failed to authenticate (unknown, pending, or expired — the three are deliberately indistinguishable to the caller). |
| `Allow` | out | Set to `GET, HEAD` (the exported constant `httpapi.AllowGET`) on a 405 from `/healthz`, `/v1/info`, `/v1/discovery`, `/v1/agents`, `/v1/messages` or `/v1/wait`. **CHANGED 2026-08-08 (CORE-7)** — it was `GET`, and `HEAD` was 405'd; now `HEAD` is served, so `Allow` names it. An `Allow` that omitted a method the route serves would be the same inconsistency CORE-7 fixed, one layer out. |
| `Content-Type` | out | `application/json; charset=utf-8` on every JSON response. |
| `X-Content-Type-Options` | out | `nosniff` on every JSON response. |
| `Idempotency-Replayed` | out | `true` on `POST /v1/enroll`'s 201, on `POST /v1/send`'s 201, and (added 2026-08-07) on `POST /v1/mint`'s 201, when the response was replayed rather than freshly applied — from the applied-key table for `enroll`/`send`, from the outstanding-reservation table for `mint`. The BODY is byte-identical to the original either way — the header is the only out-of-band signal that this call re-applied (and, for `mint`, allocated) nothing. Not reachable on `/v1/broadcast`, which answers 501. |
| `Idempotency-Replayed` | out | **NEW (INVITE-GATE, 2026-08-14).** Also `true` on `POST /v1/enroll`'s 201 when the INVITE REDEMPTION itself (not the roster-level enrolment) was a legitimate retry — same invite id, same idempotency key, same payload — sourced from the invite's own stored consumption record (`internal/invite`'s `Record.Result`), a DIFFERENT table from the roster's applied-key table the row above reads. The body is still byte-identical to the original either way. |
| `Connection` | out | `close` on **exactly one** response: the **403** from `POST /v1/send` when `sender` is a well-formed fully-qualified agent id naming an agent that is not the authenticated caller (a malformed or absent `sender` is 403 WITHOUT a disconnect — it names nobody and is a client bug). That is invariant 10's REPLAY clause — an accepted signed message can be resent verbatim by anyone who has seen it and the bytes still verify, and what identifies the replayer is that the sender inside those bytes is not the agent on the session. The server closes the socket after the response. **NARROWED 2026-08-08:** this header was previously sent on the 409 key-reuse conflicts from `POST /v1/enroll` and `POST /v1/send`, and is not any more — those keys are the caller's OWN (keys are scoped per agent), so the conflict is a client bug rather than an attack, and dropping the socket destroyed every other request the client had pipelined on it, including its long poll. Those paths now reject and log. The SIGN-6 409s (`ErrUnknownMint`, `ErrMintMismatch`) do **not** disconnect either — see the `hub.ErrUnknownMint` row in the sentinel table for why the hostile and honest cases are currently indistinguishable there. |
| `Retry-After` | out | `5` (seconds) on a 503 from any of the three auth routes (a roster, idempotency-table, or session-table capacity limit; **added INVITE-GATE 2026-08-14: also the invite table's own 8192-entry capacity limit, `invite.MaxInvites`, on `POST /v1/enroll` when an invite is presented**), on a 503 from `/v1/send` caused by the applied-key table being at capacity, and on a 503 from `/v1/mint` caused by the outstanding-reservation bounds. Short deliberately: every cap here is a live in-memory bound that a departing agent, an expiring session, an expiring reservation, or a message ageing out of the retention window relieves within seconds. It is deliberately **absent** from the 503 a poisoned or non-durable hub returns — that one is not transient and dressing it up as retryable would be a lie. It is also deliberately **absent from every SIGN-6 4xx**, which are terminal for their idempotency key. |
| `Allow` | out | Also set to `POST` on a 405 from `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete`, `/v1/client-cert/bootstrap`, `/v1/mint`, `/v1/broadcast` or `/v1/send`. |
| `Cache-Control` | out | `no-store` on `POST /v1/session/begin` only. That response body carries a LIVE credential (the session token); the other two auth responses carry none, so the header is deliberately not set on them and its presence stays meaningful. |

## Enrolment and sessions (added 2026-08-02)

AUTH-1 added the three credential-issuing routes documented in `## Routes` and `## Headers` above:
`POST /v1/enroll`, `POST /v1/session/begin`, `POST /v1/session/complete`. MTLS-MIGRATE adds the authenticated `POST /v1/client-cert/bootstrap` route in the same auth-service registration block. This section is the prose that does not fit a table row.

**`Options.Auth` gates route registration, not route behaviour.** `internal/httpapi.Options.Auth`
(`*auth.Service`) has no default and is `nil` unless the caller supplies one. When it is `nil`, `New`
does not register `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete`, or `/v1/client-cert/bootstrap` on the mux at all —
they 404 through the same `net/http.ServeMux` catch-all as any other unknown path, not a 503. That is
deliberate: a route that exists and refuses is a claim that the capability is present, and a server
built without an auth service does not have it. `cmd/agent-bus`'s `run()` always constructs one
(`auth.NewService(auth.Options{Minter: minter})`), so the shipped binary always registers these auth routes; a `nil` `Options.Auth` is reachable only by a caller of the `httpapi` package directly (tests, or a future build that intentionally omits the auth surface).

### `POST /v1/enroll` request body (RELAY-13, 2026-08-08)

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `name` | string | yes | The short name being requested. The server mints the id; this is only the human-chosen half. |
| `public_key` | base64 std, padded | yes | The agent's Ed25519 **AUTH** public key, exactly 32 bytes. Proves the agent to **this bus**. |
| `messaging_public_key` | base64 std, padded | **no — see below** | **NEW 2026-08-08 (RELAY-13).** The agent's Ed25519 **MESSAGING** public key, exactly 32 bytes, and it may **not** be the same value as `public_key` (400 if it is). Stored on the roster entry as `MessagingPublicKey` and written to the durable enrolment record as `msg_pub` (`CONTRACTS-ONDISK.md`), so it survives a restart by WAL replay. It is client-supplied **material, not identity**: it is validated as input and has **no influence on the minted agent id** (invariant 1). |
| `idempotency_key` | string | yes | 1–128 bytes of `[A-Za-z0-9._-]`. Makes the enrolment safe to retry (invariant 10). |
| `invite_id` | string | **no — see below** | **NEW (INVITE-GATE, 2026-08-14).** The invite being redeemed, `^inv-[a-z2-7]{16,32}$` (`invite.InviteIDPattern`). Must be presented TOGETHER with `invite_secret` — one without the other is 400. Omitting both is accepted and unchanged from before this task: enrolment with no invite is still accepted (`invite_required` is `false`, `GET /v1/discovery`). |
| `invite_secret` | string | **no — see below** | **NEW (INVITE-GATE, 2026-08-14).** The invite's plaintext bearer secret, exactly as the operator handed it out (`invite.Minted.Secret`). NEVER logged, echoed, or present in an error, on any path — the same discipline a session token gets. Whoever holds it can enrol an agent onto this bus. |

**The table above is the WHOLE body, and the certificate binding is deliberately not in it (MTLS-BIND,
2026-08-14).** The client-certificate fingerprint enrolment records is a **TRANSPORT FACT**, taken
from `r.TLS` on the connection the request arrived over — there is no `client_cert`, no
`cert_fingerprint` and no header a caller can set, and an unknown field is a 400 anyway. That is the
point of it: a body-supplied fingerprint would be a claim anyone could make about anyone's
certificate, so binding one would durably record a fact the handshake never established (invariants 1
and 11). It is also **not part of the idempotency payload** — the same-key-different-payload check
compares `name`, both public keys and `invite_id`, and does **not** compare the certificate, so a
retry arriving over a different certificate replays the ORIGINAL 201 and creates NO binding for the
certificate it presented (a replay applies nothing; there is no path by which a retry forges a
binding). Comparing it would turn an honest client that regenerated its keypair between two attempts
into a 409, which invariant 10 is explicit is the wrong answer for a merely buggy client.

**Why two keys, with the right control named.** It is *not* that the bus could forge with the auth
key — the bus holds only the **public** half of both keys and can forge with neither. The hazard is
that **the bus chooses the bytes the auth key signs**: the session handshake has the server issue a
token and the client sign it (invariant 3), so one key serving both roles would put a server-chosen
input under the key peers verify with.

What actually prevents a session signature being read as a message signature is **domain
separation**, and it already does: a session challenge always begins with the `a` of
`auth.SessionSigningContext`, a canonical message always begins with the `0x00` of a uint32 length
(see `internal/signing/canonical.go`, which makes this argument for exactly this key pair). **Do not
read the same-key refusal as the control that closes a signing oracle** — it is not. It makes the
separation *structural* rather than contingent on every future signing domain staying disjoint, it
bounds a compromised key to one role, and it lets the two keys rotate independently. It is also
**per-request**: it does not stop an enroller registering some *other* agent's public key as its
messaging key, because no proof of possession of the messaging private key is taken at enrolment —
that is a separate, reported gap.

A client built before `messaging_public_key` existed keeps working because the field is **optional**,
not because of the decoder. Separately, unknown fields are rejected (400), so a client that
**misspells** the field is told so rather than silently enrolling without a key.

**`messaging_public_key` is OPTIONAL today, and that is a STAGING state.** Omitting it (or sending
`""`) is accepted and the roster entry carries no messaging key — the same reserved/empty state every
agent enrolled before the field existed already has on disk, which is why `auth.Decode` and
`auth.validateRosterEntryKeys` treat an absent key as valid and a **present-but-wrong-length** key as
a hard error. Requiring it on **new** enrolments is the intended end state (an agent whose key the bus
never received cannot be attested to a peer bus, so it cannot participate in relay); **read this row,
not the code comments, for which of the two this build enforces.** When the flip lands, a body with
no `messaging_public_key` becomes a 400 and this paragraph changes with it. The flip is reported as a
follow-up in RELAY-13's completion report — at the time of writing **no follow-up task has been filed
for it**, and this sentence must be updated with the task key when one is, rather than left implying
work is scheduled that is not.

**The 400 bodies are of two different granularities, and a client sending both keys cannot always
tell which one it got wrong.** A bad *encoding* is field-specific — `{"error":"messaging_public_key
must be standard base64"}` — because the encoding check happens per field in the HTTP layer. A bad
*length*, and the same-key-twice refusal, both come back as `{"error":"invalid public key"}`,
byte-identical to the message for a bad `public_key`, because `internal/auth` maps them to the one
`ErrInvalidPublicKey` sentinel. The server log distinguishes them; the unauthenticated response
deliberately says little. Reported as a follow-up in RELAY-13's completion report and **not yet filed
as a task**; documented here so it is not mistaken for a bug in the client.

**It is part of the idempotency payload.** Invariant 10's "same key + same payload = a retry" now
includes this field: presenting one idempotency key with two different messaging keys is a 409
(rejected and logged, connection KEPT). It has to be counted — if it were not, the second call would
be answered as a replay, the roster would keep the **first** key, and the client would leave believing
the second was registered. Every message it signed would then fail to verify for every peer, with
nothing pointing at the cause.

**The invite has its OWN idempotency scope, separate from the roster-level one above (INVITE-GATE,
2026-08-14).** When `invite_id`/`invite_secret` are presented, the fingerprint `internal/invite`
compares a retry against is computed over exactly `name`, the DECODED `public_key` bytes, the DECODED
`messaging_public_key` bytes (empty if absent), and `invite_id` — in that order
(`idem.ComputeFingerprint`, called at `internal/httpapi/auth.go`'s `handleEnroll`). The invite secret
itself is deliberately NOT in that list: it is a bearer credential already proved by `invite.Store.Begin`,
and hashing it would put a credential-derived value into a durable record. This scope is keyed to
`(invite id, client idempotency key)`, not to the agent (there is no authenticated agent id yet) and not
bus-wide (an unauthenticated caller could otherwise squat a key ahead of a legitimate retry) — see
`internal/invite/doc.go` section 4 for the full argument.

**The signing contract — load-bearing for any future client.** `POST /v1/session/complete`'s
`signature` field is an Ed25519 signature over the exact byte string:

```
auth.SessionSigningContext + token
```

where `SessionSigningContext = "agent-bus:session-token:v1:"` (quote it exactly — it is a Go string
constant in `internal/auth/session.go`, concatenated directly onto `token` with no separator) and
`token` is the literal string returned as `token` in the `/v1/session/begin` response. A future client
implementation **must pin `SessionSigningContext` as a compile-time constant** and must **not** learn
it from the wire: the `/v1/session/begin` response deliberately does not echo this prefix anywhere in
its body, precisely so a man-in-the-middle who could choose what gets signed cannot turn the agent's
key into a signing oracle for arbitrary bytes. `public_key` and `messaging_public_key` (on
`/v1/enroll`) and `signature` (on
`/v1/session/complete`) are all `base64.StdEncoding` — the **standard, padded** alphabet, decoded
`Strict()` server-side so a value has exactly one valid spelling. (The `token` value itself uses a
different encoding, `base64.RawURLEncoding` — unpadded, URL-safe — since it is minted server-side and
only ever needs to round-trip, never to be independently re-encoded by a client.)

**Session lifetime constants** (`internal/auth/session.go`):

| Constant | Value | Meaning |
| --- | --- | --- |
| `SessionLifetime` | 1 hour | How long an ACTIVE session is valid, from the instant its challenge was completed. A **ceiling**, not a default to raise — the whole argument for a bearer token in place of per-request signing rests on a stolen token going stale soon. |
| `SessionRefreshFraction` | 0.75 | Where in a session's life a well-behaved client should begin its next challenge: 75% of `SessionLifetime`, leaving a quarter of the lifetime as slack. Surfaced on the wire as `refresh_after_seconds` (2700 at the default lifetime) in the `/v1/session/complete` response — advice only, not enforced. |
| `ChallengeTTL` | 2 minutes | How long an issued-but-unsigned token stays completable; surfaced as `challenge_expires_at` in the `/v1/session/begin` response. |
| `TokenRandBytes` | 32 | Bytes of `crypto/rand` entropy in a session token. |

**Server-side expiry is authoritative, with NO clock-skew grace.** `auth.Service.Authenticate` (the
seam AUTH-2's middleware will wrap; nothing enforces it on any route yet) checks `ExpiresAt` against
the server's own clock on every call — a client's opinion of the time never enters into it, because a
grace window is just a longer lifetime with a less honest name. `ExpiresAt` is set exactly **once**,
at the first successful `POST /v1/session/complete`, and is **never** extended by re-completing an
already-active session: a repeat completion re-verifies the signature and returns the identical
`expires_at`, so a client cannot hold one session open indefinitely off a single signature.

### `POST /v1/client-cert/bootstrap` — first certificate binding for a legacy identity (`MTLS-MIGRATE`, 2026-08-23)

This route migrates a pre-TLS identity that already exists in the roster but has no live client-certificate binding. It is **authenticated** by the ordinary bearer middleware and is **not** on `UnauthenticatedRoutes()`, but the bearer is not sufficient authority by itself: the request must also carry a fresh Ed25519 signature by the enrolled AUTH key over the bootstrap intent. The server reads no agent id or certificate fingerprint from the body: the agent id is the `auth.Principal` attached by `authMiddleware`, and the certificate fingerprint is the `ClientCertFingerprintFromContext` value attached by `WithClientCertificate` from the TLS connection.

Request body:

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `idempotency_key` | string | yes | 1-128 bytes of `[A-Za-z0-9._-]`. A successful key is stored durably on the roster entry with the first binding. Same key + same presented certificate replays the original 200 body with `Idempotency-Replayed: true`; for the first successful binding that original body has `already_bound:false`. After that binding, presenting a different certificate with the same session is refused earlier by `authMiddleware`'s certificate/session cross-check as `403` with no `Connection: close`, so bootstrap idempotency handling is not reached. |
| `signature` | string | yes | Standard-base64 Ed25519 signature by the enrolled AUTH private key over `auth.ClientCertificateBootstrapSigningBytes(sessionToken, idempotency_key, tls_client_cert_fingerprint)`, whose pinned context is `agent-bus:client-cert-bootstrap:v1:`. This is what makes stolen bearer + attacker certificate fail. |

Response body, 200:

| Field | Meaning |
| --- | --- |
| `agent_id` | The existing fully-qualified agent id from the authenticated session. No new id is minted. |
| `client_cert_fingerprint` | The SHA-256 fingerprint of the TLS client certificate presented on this request. |
| `bound_at` | Server timestamp for the binding. |
| `already_bound` | `true` when this call presents the already-live certificate but is not the stored idempotent replay. A same-key replay returns the original body, so a first-binding replay keeps `already_bound:false` and uses `Idempotency-Replayed: true` for the replay signal. |

Status codes specific to this route: `403 {"error":"client certificate required"}` when the request carries no usable in-date client certificate; `403 {"error":"bootstrap signature does not verify"}` when the bearer session is valid but the AUTH-key proof is absent or wrong; `403 {"error":"this credential was not presented over the client certificate it is bound to"}` when a later request presents a different certificate after the first binding. That last refusal is returned by `authMiddleware` before the bootstrap handler, keeps the connection open, and performs no bootstrap idempotency lookup. `409` means the certificate is already live on another agent, or this agent already has a different live binding and must use a future rotation path. `400` covers an invalid `idempotency_key` or malformed base64 signature; `404` is only for an impossible authenticated principal that no longer exists. It spends no invite and never calls enrolment.


**Admission-control caps** (`internal/auth/service.go` `Options`, all overridable, `0` means "use the
default", there is no "unlimited"):

| Cap | Default | Behaviour at the cap |
| --- | --- | --- |
| `MaxRosterEntries` | 4096 | **Fails closed**: `POST /v1/enroll` returns 503 (`ErrCapacity`, `Retry-After: 5`). Never evicts a roster entry — evicting one would let an already-enrolled agent's id be re-minted out from under it. |
| `MaxIdempotencyEntries` | 16384 | **Fails closed**: 503, same as above. Never evicts — evicting a remembered key would silently turn the next legitimate retry into a fresh (duplicate) application, exactly what invariant 10 forbids. |
| `MaxSessions` | 16384 | **Fails closed**: `POST /v1/session/begin` returns 503 (`ErrCapacity`, `Retry-After: 5`). Counts pending and active sessions together. This is now the ONLY bound on unauthenticated session-table growth — there is deliberately no per-agent cap (see the note below the table) — and expiry is what drains it. A refusal leaves the table exactly as it found it, so an error path never destroys anyone's earlier challenge. The residual risk is untargeted: a flooder can fill the table to this limit and deny NEW session establishment to EVERYONE; already-ACTIVE sessions keep authenticating. **How long that outage lasts depends on which state the table is full of, and the two differ by 30x:** pending challenges drain after `ChallengeTTL` (2 minutes), but ACTIVE sessions are reclaimed only after `SessionLifetime` (1 hour), and nothing caps active sessions per agent while enrolment is itself unauthenticated — so an attacker that enrols its own agent can hold the outage for an hour past the flood at far less traffic. That gap pre-dates this row and is filed as AUTH-1-FU-ACTIVECAP. Mitigation for the flood itself is per-source rate limiting, **not implemented** — task AUTH-1-FU-RATELIMIT. |

**There is deliberately no per-agent pending-challenge cap** (removed in AUTH-1-FU-PENDINGCAP,
2026-08-02; formerly `MaxPendingPerAgent`, default 8, evicting the oldest pending challenge for that
agent). It was removed rather than retuned: on the unauthenticated `POST /v1/session/begin` route,
`agent_id` is an attacker-supplied *victim* identifier, so a bucket keyed on it always lands an
anonymous flooder's requests in the *victim's* own bucket. Evicting drops the victim's
correctly-issued challenge; refusing denies the victim its next one — either behaviour at the cap is
a lockout of a named agent by anyone who merely knows its id, achievable in single-digit anonymous
requests. There is no ordering of a victim-keyed bucket that is not a lockout primitive, so do not
re-add one; per-source rate limiting (AUTH-1-FU-RATELIMIT, not implemented) is the correct fix for
the flooding this cap never actually addressed.

Be precise about the trade, because removing the cap made the *untargeted* flood **cheaper**, not
merely no worse: pending entries used to be bounded by cap × roster size, so exhausting the table
first meant enrolling enough distinct ids, whereas it is now directly reachable with `MaxSessions`
begins naming one known agent. That is still clearly the right trade — roughly
`MaxSessions`/`ChallengeTTL` ≈ 140 sustained requests per second buys an untargeted, unamplified,
self-healing outage, against nine requests per round for a targeted, permanent, stealthy one — but it
does raise the priority of AUTH-1-FU-RATELIMIT.

What IS guaranteed without the cap: nothing an unauthenticated caller does can destroy a challenge
already issued to another agent. A challenge leaves the session table by exactly three routes, and
the third requires the token — it expires (`ChallengeTTL`), it is completed, or a completion attempt
against it fails verification (`CompleteSession`'s single-attempt-per-pending-challenge rule). The
token is 32 bytes of `crypto/rand` and the table is keyed on its SHA-256, so that third route is
reachable only by whoever holds the token: the agent itself, or someone who observed it in flight.
**Corrected 2026-08-07 (`MTLS-LISTENER`):** the server now serves TLS ONLY (see "## Transport" above),
so plainly sitting on the wire no longer suffices — the observer must also hold or forge the bus's
pinned certificate, which is precisely what a caller who follows invariant 11's no-TOFU pinning refuses
to accept from anyone else. Two things this does NOT close: any TCP peer that can complete the TLS
handshake — not just the enrolled agent — can still attempt this route, and `MTLS-CLIENTAUTH` landing
2026-08-14 did **not** change that (**this sentence claimed `ClientAuth` was still `tls.NoClientCert`
and was false from `a97f854` onward**), because `tls.RequestClientCert` requests a certificate and
never requires one, and nothing on this route reads one; and a caller that skips certificate verification (there
is no such flag in this repo's own client, `client/pin.go`, but a hand-rolled one could) is back to the
pre-TLS threat model. The token's unguessability therefore stays load-bearing against both of those,
and against the fact that there is still no per-agent or per-source rate limiting on this route.

### `POST /v1/leave` — agent self-leave (AUTH-4)

`internal/httpapi/leave.go`. Registered in the same `if s.auth != nil` block as the three routes
above, but it is **not** on `unauthenticatedRoutes` — leaving the bus is an authenticated action, and
adding it to the allow-list would let an anonymous caller name and remove any agent id it likes.

**Self-leave only, structurally.** The route reads no request body at all — there is no field to name
a victim — and the agent removed is always `PrincipalFromContext(r.Context())`, the same principal
`authMiddleware` attached before the handler ran. Operator-initiated revocation of a **different**
agent is a separate, not-yet-built capability (AUTH-7 / AUTH-ROSTER-RECLAIM) with a different
authority model; this route does not provide it and must not be read as a step toward it without a
new decision.

**What one call does, in order:** a durable tombstone append to the roster (`Roster.Remove`, under the
same `enrolMu` `Enrol` holds, two fsyncs — invariants 4, 6), THEN the agent's live sessions (pending
and active) are dropped from the in-memory table. The order is load-bearing: a session dropped ahead
of a departure that failed to commit would be a credential revoked with no durable record behind it.

**Idempotent, never a disconnect (invariant 10).** A second `POST /v1/leave` for an already-departed
agent returns the same 200 shape with `already_left:true`, `sessions_dropped:0`, and writes nothing.
A retry whose first attempt already succeeded has, by then, lost its session — it meets
`authMiddleware`'s ordinary 401 on the next call, a clean refusal, never a dropped socket.

**The id is never re-issued (invariant 1).** A leave reuses the enrolment's own WAL kind
(`auth.RecordKind = "agent"`) with `left_at` set rather than minting a new record kind; `Apply` folds
it as a removal, and the departed agent's burned name-suffix floor stays visible so a later enrolment
under the same name gets a fresh server-minted id — see `CONTRACTS-ONDISK.md` and `PROTOCOL.md` §9.

**Undelivered direct messages are not erased.** The append-only log never rewrites (invariant 6) and
message bodies live in the hub, not the roster; a departed agent's undelivered DMs simply become
undeliverable — the recipient id is gone from the roster, so the read paths fail closed for it, and a
re-enrolment under the same name is a new id (invariant 1) that inherits nothing.

`LeaveResponseBody` lives in `internal/httpapi/leave.go` alongside `HealthResponse` / `InfoResponse` /
`ErrorResponse` in `internal/httpapi/server.go` and the other typed bodies named at the end of the
messaging-routes section above.

### Durability of the roster and sessions (CORRECTED 2026-08-07 by AUTH-7)

**The passage that stood here — "Nothing here is durable — do not claim otherwise" — is now FALSE
for the ROSTER and is corrected in place.** It was accurate until AUTH-7 wired
`auth.NewWALRoster` into `cmd/agent-bus`. What holds today:

| State | Durable? | What a restart does to it |
| --- | --- | --- |
| The enrolment **roster** (`auth.WALRoster`) | **YES** | Nothing. Agent ids, public keys and each agent's **ORIGINAL** `enrolled_at` survive a restart and a `SIGKILL`, fsynced through the two-phase write path and rebuilt by replay. **An agent does NOT re-enrol after a restart.** |
| Per-name agent-id **suffix floors** | **YES** | Resume above every suffix ever issued (`<data-dir>/agent-suffixes`; `CONTRACTS-ONDISK.md`). They no longer restart at 1. |
| **Sessions** (bearer tokens) | **NO, deliberately, and not a gap to close** | Every token is invalidated. Each agent must redo the `session/begin` → `session/complete` handshake before its first authenticated call — but must **not** re-enrol. |
| The **idempotency-key** table | YES (IDEM-11, `<data-dir>` applied-key store) | Survives; see the applied-key sections in `CONTRACTS-ONDISK.md`. |
| Outstanding `/v1/mint` **reservations** | **NO, deliberately** | Invalidated. The next `/v1/send` gets 409 `ErrUnknownMint`; the client re-mints under the same key. Only the burned NUMBER is durable, not the table. |

**Sessions are memory-only on purpose and that is settled.** They are short-lived bearer
credentials; writing live ones to disk would store replayable material to save exactly one round
trip. Persisting them is not planned.

`cmd/agent-bus`'s `run()` says exactly this at `WARN` on every start — the old warning, which
claimed the roster was lost, has been replaced:

```
msg="SESSIONS are in-memory only: every bearer token is invalidated by this start, and each enrolled agent must run the session handshake again before its first authenticated call. It does NOT have to re-enrol -- the roster IS durable ..." 
```

The agent-id-suffix question is answered SEPARATELY, per data directory, by the "agent-id suffix
floors" line emitted just above that WARN: INFO on an ordinary start, raised to WARN or ERROR
precisely when there is something to act on.

**Hub wiring consequence (AUTH-7).** `hub.NoteEnrolment` and the hub's private roster map are
**DELETED**. The hub now reads through to the authoritative roster via the new `hub.RosterSource`
interface, and `hub.Options.Roster` is **REQUIRED** — `hub.Open` returns a hard error on nil rather
than serving an empty roster while looking healthy. The adapter from `internal/auth` to
`hub.RosterSource` lives in `cmd/agent-bus/hubroster.go`, the one place that legitimately holds both
packages, and it is a **live view, never a snapshot**: a snapshot taken at startup would reintroduce
the same bug with a different cause (every agent enrolled after boot would authenticate and then be
refused as an unknown sender).

No on-disk record type, WAL frame, or wire protocol version was introduced by AUTH-1 itself. AUTH-7's
wiring introduced none either — `auth.RecordKind = "agent"` / `auth.RecordVersion = 1` were already
defined; see `CONTRACTS-ONDISK.md`.

**Known gaps in this surface (recorded so nobody assumes a protection that is absent).** All three
routes above are UNAUTHENTICATED by necessity — they are the calls that ISSUE the credential — and:

- **There is NO per-source rate limiting.** The caps are all GLOBAL, so an anonymous caller can deny
  enrolment bus-wide with `MaxRosterEntries` requests, and deny session establishment with
  `MaxSessions` begins. The caps bound memory; they do not bound an attacker.
- **There is no per-agent pending-challenge cap**, and deliberately so: one existed briefly
  (`MaxPendingPerAgent`) and was removed (AUTH-1-FU-PENDINGCAP) once analysis showed any such cap is
  itself a lockout primitive on this unauthenticated route — see the note under the admission-control
  table above. Nothing an unauthenticated caller does can destroy a challenge already issued to
  another agent.
- **Enrolment does not prove possession of the private key.** A caller may bind any public key —
  including someone else's published one — to a fresh, server-minted agent id. The binding that this
  surface does guarantee still holds: an id can never later present a *different* key, and an id
  cannot be used without a signature from the key recorded against it.
- **Every route off the allow-list now enforces a session token** (AUTH-2 — see `## Authentication`
  below). `auth.Service.Authenticate` is the seam `authMiddleware` calls on every request; it is no
  longer unwired.
- **PARTIALLY CLOSED (AUTH-4, this task): `POST /v1/leave` gives an agent SELF-service revocation.**
  An agent that leaves has its live sessions (pending and active) dropped at once, rather than left to
  expire. **What is still absent: OPERATOR-initiated revocation of a DIFFERENT agent** — there is no
  route that lets anyone but the agent itself end its own session or remove its own enrolment early; a
  session belonging to an agent that has not called `/v1/leave` is still valid until it expires, at
  most one hour (filed as AUTH-7 / AUTH-ROSTER-RECLAIM). Do not read `/v1/leave` as closing this gap in
  general — it closes exactly the self-leave case.
- **SHARPENED, NOT CLOSED (INVITE-GATE, 2026-08-14): `POST /v1/enroll` is STILL unauthenticated, and
  that is the point — it is how a credential is obtained at all, and invariant 3 requires it stay
  reachable with nothing in hand.** What changed is that a presented invite is no longer decorative: an
  `invite_id`+`invite_secret` pair, when sent, is genuinely single-use and REDEEMED atomically with the
  enrolment (see the routes table above and `CONTRACTS-ONDISK.md`'s `"agent+invite"` entry). **This does
  NOT gate the route.** An enrolment presenting NO invite is still accepted exactly as before this task
  — `GET /v1/discovery`'s `enrolment.invite_required` is `false` and says so, and every gap in this list
  still applies in full to the no-invite path, which remains the common case. Requiring an invite
  (invariant 3's end state) is a separate, not-yet-filed change. Nor does a CLI exist to exercise the
  invite half of this route: `agent-busctl enrol --invite` is refused locally (`client/enrol.go`), so an
  agent cannot redeem an invite today even though the HTTP surface accepts one — that gap is task
  `INVITE-CLIENT`.

## Authentication (added 2026-08-02)

AUTH-2 wires `internal/httpapi/authmw.go`'s `authMiddleware` around the WHOLE mux — since MTLS-BIND
(2026-08-14) the chain is
`s.handler = LoggingMiddleware(s.log, s.WithClientCertificate(s.authMiddleware(mux)))`, and it read
`LoggingMiddleware(s.log, s.authMiddleware(mux))` before that — folding in **AUTH-6**'s fail-open fix
into the same change rather than as a later retrofit. The middleware is DEFAULT-DENY: every request is
refused 401 unless its **exact** `r.URL.Path` is on the allow-list, so a route added tomorrow is
authenticated the instant it is registered through `(*Server).route` — nobody has to remember to wrap
it, and forgetting is no longer possible for the surface `TestEveryRouteRequiresAuth` can see (below).

**The allow-list is exactly six paths, matched by exact string equality** (no prefix match, no path
cleaning, no trailing-slash tolerance — `/healthz/`, `//healthz`, `/HEALTHZ` are NOT allow-listed and
get 401; the cost of being this strict is a 401 on a misspelled-but-harmless probe, the cost of being
lenient is a normalisation mismatch between this check and the mux, which is how allow-list bypasses
get built):

- `/healthz` — liveness; a load balancer or orchestrator probe calls it before any agent exists, and it
  returns no state.
- `/v1/info` — pre-enrolment discovery; an agent needs the bus id and version to decide whether to
  enrol at all.
- `/v1/discovery` — **added 2026-08-07 (DISCOVERY-DOC).** How a caller holding nothing but this bus's
  URL learns to enrol at all — requiring the credential it explains would make it unreachable by
  everyone who needs it, the same circularity that puts `/v1/enroll` and the two session routes on
  this list. It is safe to leave open because it reveals nothing about this bus's contents or
  configuration: the document is a STATIC, compile-time-constant description of the PROTOCOL, never
  the ROSTER, plus the one bus-specific value (`bus_id`) that `/v1/info` already serves to the same
  anonymous caller. See `### Discovery document` below.
- `/v1/enroll` — this is where an identity is created; there is by definition no credential yet.
- `/v1/session/begin` — called with NO session at all; it is the request that asks the server for a
  token to sign.
- `/v1/session/complete` — the one that looks skippable. The caller does hold a token here, but it is
  PENDING, and `auth.Service.Authenticate` rejects a pending session exactly like an unknown one — a
  bearer requirement on this route would be unsatisfiable, not strict, since it could only ever be
  satisfied by the very credential the call exists to create. Authentication on this route is the
  Ed25519 signature over `auth.SessionSigningContext + token` (see "Enrolment and sessions" above),
  which `handleSessionComplete` verifies directly; the token in the body is not a credential until that
  succeeds.

**Every other route requires `Authorization: Bearer <token>`**, where `<token>` is the opaque handle
returned by a COMPLETED `/v1/session/complete` — not a signed claim, so every request re-checks live
session state, which is what makes revocation and expiry immediate; nothing here is cached. The 401
body is always the standard `{"error":"..."}` envelope and is one of exactly two strings, deliberately
never anything more specific:

- `{"error":"authentication required"}` — no usable credential presented: missing `Authorization`
  header, more than one `Authorization` header (rejected on ambiguity even when both carry the same
  valid token — a duplicate could be a proxy artefact, and choosing which of two to honour is the
  ambiguity an attacker exploits to make front and back disagree), a scheme other than `Bearer`
  (scheme itself case-insensitive per RFC 7235), an empty token, a token containing a space, a token
  over `MaxBearerTokenLen` (512 bytes), or a token with a byte outside the base64url alphabet
  `[A-Za-z0-9_-]`. Carries `WWW-Authenticate: Bearer realm="agent-bus", error="invalid_request"`.
- `{"error":"invalid or expired credential"}` — a syntactically well-formed token that did not
  authenticate: unknown (never issued), PENDING (challenge never completed), or EXPIRED. These three
  are deliberately BYTE-IDENTICAL in the response — distinguishing them is an enumeration oracle that
  would let a caller probe which session handles exist; the LOG line (not the response) names which of
  the three it was. Carries `WWW-Authenticate: Bearer realm="agent-bus", error="invalid_token"`.

On success the middleware attaches the verified `auth.Principal` to the request context; no principal
is attached on an allow-listed route. Downstream handlers read it via `httpapi.PrincipalFromContext` /
`httpapi.AgentIDFromContext` and MUST NOT take an identity from a header, query parameter or body —
those are client-supplied claims (invariant 1: the server is authoritative on every id).

**Exported Go surface** (`internal/httpapi/authmw.go`, `internal/httpapi/server.go`):

| Symbol | What |
| --- | --- |
| `MaxBearerTokenLen` | `512` — the length cap above; two orders of magnitude of headroom over a real 43-character token, and still finite. |
| `UnauthenticatedRoutes() []string` | The allow-list, sorted, returned as a COPY — the real map is the security boundary of this server and is not exported, so no caller can get a handle that mutates it. |
| `IsUnauthenticatedRoute(path string) bool` | Exact-match check against the allow-list; what `authMiddleware` itself calls. |
| `RouteDiscovery` | `"/v1/discovery"` — added 2026-08-07 (DISCOVERY-DOC). On the allow-list. `DiscoveryResponse` and its nested types (`DiscoveryEndpoint`, `DiscoveryEnrolment`, `DiscoverySession`, `DiscoveryClient`) live in `internal/httpapi/discovery.go`; see `### Discovery document` above for the shape. |
| `PrincipalFromContext(ctx) (auth.Principal, bool)` | The authenticated identity, or `ok == false` on an allow-listed route (not an error condition — it is the definition of an unauthenticated route). |
| `AgentIDFromContext(ctx) string` | The fully-qualified `<bus-id>.<agent-id>` (invariant 2) of the caller, or `""` when no principal is attached. |
| `(*Server).Routes() []string` | Every pattern registered through `(*Server).route`, sorted. This is the real surface `TestEveryRouteRequiresAuth` walks, because Go 1.19's `http.ServeMux` cannot otherwise be enumerated. **Since 2026-08-08 (CORE-8) it includes `"/"`.** |
| `AllowGET` | `"GET, HEAD"` — added 2026-08-08 (CORE-7). The `Allow` header value every `requireGET` route sends with its 405. |
| `RouteCatchAll` | `"/"` — added 2026-08-08 (CORE-8). The pattern the JSON-404 handler is registered at. **Never** on the allow-list; `IsUnauthenticatedRoute("/")` is false and a test asserts it, because a catch-all outside the auth wrapper would turn the whole server into a route oracle. |
| `PanickedField` / `PanicAfterWriteField` / `HijackedField` | `"panicked"` / `"panic_after_write"` / `"hijacked"` — added 2026-08-08 (CORE-14). Log keys, not HTTP; see `### Panic log records` below. |
| `RouteClientCertBootstrap` | `"/v1/client-cert/bootstrap"` — authenticated first-binding route for `MTLS-MIGRATE`; registered only when `Options.Auth` is non-nil and never on the allow-list. Requires an AUTH-key signature in addition to the bearer session. |
| `RouteAgents` / `RouteMint` / `RouteBroadcast` / `RouteSend` / `RouteMessages` / `RouteWait` | `/v1/agents`, `/v1/mint`, `/v1/broadcast`, `/v1/send`, `/v1/messages`, `/v1/wait` — the messaging surface. `RouteMint` added 2026-08-07. All are registered only when the server has a hub; **never** on the allow-list. `/v1/broadcast` is registered and authenticates, and then answers 501. |
| `MaxMessageRequestBytes` | `128 << 10` — the request-body cap on `/v1/mint`, `/v1/broadcast` and `/v1/send`. The real payload limit is `store.MaxBodyBytes` (64 KiB decoded); this one only stops an unbounded stream reaching the decoder. |
| `hub.SeqFloorRecordKind` / `hub.MintBatchSize` | `"seqfloor"` / `256` — the durable sequence floor that makes the mint safe across a restart. See `CONTRACTS-ONDISK.md`. |
| `hub.MaxOutstandingMintsPerAgent` / `hub.MaxOutstandingMints` / `hub.MintTTL` | `64` / `8192` / `15m` — bounds on the in-memory outstanding-reservation table. Both bounds **fail closed** and never evict another agent's reservation to make room. |
| `hub.ErrUnknownMint` / `hub.ErrMintMismatch` | The two 409s SIGN-6 added on `/v1/send`. Neither disconnects. **`ErrUnknownMint` is deliberately NOT disconnected even though one of the two ways to reach it is hostile:** it is raised by the `h.mints[{agent,op,key}]` miss in `internal/hub/hub.go`, both when a third party spends another agent's reservation and when the SAME agent re-presents its own already-spent reservation under a fresh key. The miss carries no information about who — if anyone — the presented `message_id` was minted for; the hub keeps no message-id→minting-agent index and `internal/store` has no lookup by message id. Disconnecting on the sentinel alone would punish the honest client. Resolving it needs a distinct hub sentinel raised only when the presented `(message_id, seq)` matches an OUTSTANDING reservation held by a different agent. |
| `(*Server).Hub() *hub.Hub` | The messaging hub, or `nil` when the messaging routes are not registered. |
| `hub.EncodeCursor(agentID, after) string` / `hub.DecodeCursor(agentID, cursor) (uint64, error)` | The cursor codec. `DecodeCursor` rejects a cursor bound to a different agent with `hub.ErrInvalidCursor`. `MaxCursorLen` is 512. |
| `store.Message.VisibleTo(agentID string, enrolledAt time.Time) bool` | The **one** authorization boundary of the read path. Applied on all four read paths (history, the long-poll fast path, its post-registration re-read, and its wake re-read) and by the wake filter itself, always with the AUTHENTICATED principal and that agent's roster entry — never with anything taken from a cursor. A zero `enrolledAt` disables the epoch check and exists only for roster-less callers (an audit tool); it must never be reached from a request path. |
| `hub.Result` / `hub.Batch` | What a send returns and what a read returns; see `internal/hub/hub.go` and `internal/hub/wait.go`. |
| `ClientCertificate{Fingerprint, Leaf}` | **NEW (MTLS-BIND, 2026-08-14).** The client certificate an ORDINARY AGENT connection presented, reduced to what may be acted on. **It is a TRANSPORT FACT and authorises NOTHING** — an unenrolled stranger with a self-signed certificate reaches it exactly as an enrolled agent does. `Fingerprint` is `buscert.FingerprintOf(leaf)`; `Leaf` exists for a log line and must never yield an identity (`Subject`/`SAN`/`EKU` are chosen by whoever presented it). |
| `ClientCertificateFromContext(ctx) (ClientCertificate, bool)` / `ClientCertFingerprintFromContext(ctx) (buscert.Fingerprint, bool)` | **NEW (MTLS-BIND, 2026-08-14).** `ok == false` means one of: not TLS, no certificate presented, or the leaf was outside its validity window — **deliberately not distinguished**, because all three mean "there is no transport identity to check against". A caller that requires a certificate must treat `false` as a REFUSAL, never as "no constraint applies". |
| `(*Server).WithClientCertificate(next) http.Handler` | **NEW (MTLS-BIND, 2026-08-14).** The middleware that puts the above in the context, at **`ctxKey` 3** (0 request id, 1 agent principal, 2 peer principal — the value was taken from the tree, since these keys are compared by VALUE and a collision silently shadows rather than failing to compile). **It admits every request and is NOT a gate**; see `### The client certificate on an agent connection` below. |
| `store.RecordKind` / `store.RecordVersion` | `"message"` / **`2`** (was `1`; bumped 2026-08-07 by SIGN-6, reserved from the Spec Server `store-record-version` namespace) — the `wal.Entry.Kind` discriminator and the schema version of the durable message payload. v2 adds REQUIRED `timestamp_ms` and `signature`, and **refusing v1 records at recovery is a destructive, bidirectional break** — see `CONTRACTS-ONDISK.md`. **DUR-5 consumes `store.Record`**: every field invariant 6 names is a top-level field and the only one the audit log must drop is `body`. |

### The client certificate on an agent connection (MTLS-BIND, 2026-08-14, `818207d`)

The listener requests a client certificate (`## Transport` above). `(*Server).WithClientCertificate`
is what makes the presented one visible to a handler, and `POST /v1/enroll` is the one route that
acts on it — it records the fingerprint on the agent's durable roster entry as `cert_bindings`
(`CONTRACTS-ONDISK.md`), which is the fact invariant 11's cross-check needs and never had.

**The middleware ADMITS EVERY REQUEST. It is not a gate**, and that is the decision, not an omission:
the listener never requires a certificate, so a connection carrying none is the ordinary case —
`/healthz`, `/v1/info`, the container's own healthcheck, and every client that has not grown a keypair
yet. Refusing here would enforce "a certificate is mandatory" in the middleware while the transport
says it is optional, and would take the bus's own health probe down with it.

**The order of its checks is the contract** (`internal/httpapi/clientcert.go`):

1. TLS or nothing (`r.TLS != nil`).
2. A certificate must have been presented; an EMPTY `PeerCertificates` is the ordinary case.
3. **`PeerCertificates[0]` only — never iterate the chain.** The client controls every certificate it
   sends while `CertificateVerify` proves possession of the LEAF's key alone, so searching the chain
   would be spoofed by anyone appending the victim's PUBLIC certificate at index 1 — and that single
   mistake would hand an attacker the victim's agent id. Extra entries authorise nothing.
4. **The leaf must be IN DATE, checked before the fingerprint is published.** `crypto/tls` proves
   possession and does **not** check dates, so an expired certificate completes the handshake exactly
   like a fresh one. It is `crypto/x509`'s verdict via the same helper `RELAY-20` uses on the peer
   plane, never a local date comparison (invariant 9). A leaf failing this is logged at INFO with its
   fingerprint and the request continues **without** a transport identity.

A leaf that fails any step attaches **NOTHING**, rather than something marked invalid — an
invalid-but-present value is the shape that gets read past.

**It is mounted OUTSIDE `authMiddleware` on purpose**: enrolment is unauthenticated by necessity and
is the route that CREATES the binding, so a certificate that only became visible after authentication
could never be bound to anything (invariant 3). It is INSIDE `LoggingMiddleware` so its own lines
carry the request id. It does not interact with `RequirePeerPrincipal` and does not need to — this
value is not a principal, so on a peer route it merely describes the certificate that gate already
authorised.

**What enrolment does with it.** A presented, in-date certificate becomes exactly one LIVE binding on
the new agent's roster entry; an absent one leaves the entry with none, and **that is accepted** —
requiring a certificate here would lock out, with no migration path, every identity directory that
holds no client keypair (`agent-busctl client-cert` generates one, and `MTLS-CLIENTCERT` is
`in_progress`, not done). A fingerprint already live on a **different**
agent is the 409 in `## Routes` above: refused before the mint, connection KEPT, body naming no agent.
One certificate must never name two agents — that would let one key holder authenticate as either, at
which point the fingerprint names nobody.

> **SUPERSEDED 2026-08-14 by `2ea7dfb` (MTLS-CROSSCHECK).** Between `818207d` and `2ea7dfb` this
> spot carried a block declaring invariant 11's cross-check designed but not enforced, and concluding
> that a stolen session token was replayable from any machine with nothing detecting it on any route.
> **`2ea7dfb` made both of those statements false**, and the section immediately below replaces them.
> The pointer is kept rather than the text deleted outright because the old claim was TRUE for the two
> days MTLS-BIND stood alone, and a reader who acted on it needs to know what changed: MTLS-BIND
> supplied only the antecedent — the stored cert→agent fact — and `MTLS-CROSSCHECK` supplied the check
> that reads it. (The original wording is not reproduced here verbatim; `git show 818207d` has it.)

### Invariant 11's cross-check on the agent plane (MTLS-CROSSCHECK, 2026-08-14, `2ea7dfb`)

Invariant 11 requires that **a session token presented over a connection whose client certificate
belongs to a DIFFERENT agent be rejected** — a stronger property than either mTLS or the session
token gives alone. `internal/httpapi/crosscheck.go`'s `(*Server).enforceCertBinding` is that check on
the **agent** plane, and it is LIVE — this section is about that plane only.

The peer plane is separate and is **not** the same claim. Its machinery landed at `ed77bba`
(RELAY-45/RELAY-20) but **`cmd/agent-bus` mounts no peer route at this commit**, so nothing below
`## Peer-bus transport identity` is reachable on a running bus; `s.isPeerRoute` is false for every
path today. When a peer route is mounted, `authMiddleware` returns on it before this gate runs and
the request is governed by `RequirePeerPrincipal` instead — on that surface the certificate alone
authorises and there is no PAIR to cross-check, which is recorded as a **named narrowing** of
invariant 11 (`DECISIONS.md`, `## 2026-08-14 — FEDERATION (RELAY-6), AMENDMENT`, ruling **(i)**), not
as compliance with it.

**Three call sites, all of them enforcing:**

| Where | When it runs | Which agent id it checks |
| --- | --- | --- |
| `authMiddleware` — every authenticated route | after the bearer token authenticates, **before** the principal is attached to the request context | `principal.AgentID`, server-minted out of the roster |
| `POST /v1/session/begin` | **before** `BeginSession` mints a challenge | `body.AgentID` — client-supplied and unvalidated at that point, deliberately (the check asks whether a credential for the agent this request NAMES may be issued over THIS connection, not whether the caller is that agent) |
| `POST /v1/session/complete` | **after** `CompleteSession`, which is the earliest the agent id is knowable | `sess.AgentID`, recorded by the server when the challenge was issued |

**The rule, stated once:**

- An agent holding **≥1 live certificate binding** must present a matching, **in-date** client
  certificate on the connection. Any live binding satisfies it, not the newest — rotation
  legitimately serves two certificates at once (invariant 11), so requiring the latest would refuse
  the outgoing certificate mid-rollover. A **retired** binding neither satisfies nor requires.
- An agent holding **no live binding is unconstrained on the agent side.** This is the deliberate
  **migration allowance**: bindings only started being written by MTLS-BIND (2026-08-14), so every
  agent enrolled before it has none, and refusing them all would be a flag day rather than a
  migration. **For such an agent a stolen session token is still replayable from a connection
  presenting no certificate** — read that plainly, it is the one part of the old paragraph above that
  survives, narrowed to unbound agents. It closes agent by agent as they either re-enrol under
  `MTLS-CLIENTCERT` or run the authenticated `POST /v1/client-cert/bootstrap` migration.
- A presented certificate that is a live binding on a **different** agent is refused **regardless** of
  whether the named agent has a binding of its own, and so is one that is live on **two** agents at
  once (reachable from a damaged or hand-edited durable record; it names nobody until an operator
  retires all but one). An **unbound** certificate on the connection is not itself a refusal — that is
  the ordinary case for a client that grew a keypair before its bus recorded bindings.

**Refusal shape, fixed for all four causes:** `403` with
`{"error":"this credential was not presented over the client certificate it is bound to"}`, **no
`WWW-Authenticate` header**, and **never `Connection: close`, on any path** (invariant 10 — a merely
buggy client reaches these lines trivially, and the connection does not carry only one principal's
traffic). The single string hides **which guard fired**, so a caller cannot separate "your
certificate belongs to someone else" from "this agent requires one and you presented none" from "your
certificate is live on two agents"; the server LOG names the guard, the agent id and the fingerprint.

**BEHAVIOUR CHANGE: an empty `agent_id` at `POST /v1/session/begin` moved 404 → 403.** An empty id is
not "an agent that happens to hold no binding", it is no agent at all, so the gate refuses it before
`BeginSession`'s 404 can see it. Rebuild and restart required; no on-disk format or wire protocol
change.

**Four routes are NOT cross-checked and still work with no client certificate**: `/healthz`,
`/v1/info`, `/v1/discovery` and `/v1/enroll`. They reach no principal, so there is no agent id to
check a certificate against, and `authMiddleware` returns before the gate runs. This is what keeps
the container healthcheck and pre-enrolment discovery working; `/v1/enroll` is where a binding is
CREATED, so requiring one there would be circular.

**Two consequences that are accepted rather than fixed, recorded here so nobody rediscovers them as
bugs:**

1. **An enumeration oracle at the unauthenticated `POST /v1/session/begin`, left standing
   deliberately.** Measured, for an anonymous caller presenting no certificate: an **unknown** agent
   → `404`; a **known, not-yet-bound** agent → `200` with a live challenge token; a **known, bound**
   agent → `403`. So a 403 discloses precisely that an agent holds a live certificate binding, and
   sweeping guessable ids maps which agents are **not** yet bound. Closing it costs an honest client
   the only signal telling it to re-enrol rather than retry forever, and moving the gate after
   `BeginSession` to equalise the shapes would recreate MTLS-BIND's mint-then-refuse defect, which
   permanently burned an agent-id suffix per refusal. What leaks is bounded — that an agent is not yet
   bound, never what any certificate is nor whose — and it shrinks as agents re-enrol. The security
   gate measured the three responses itself and accepted it.
2. **A deliberate REGRESSION, now live: a bound agent whose certificate EXPIRES is locked out
   everywhere, including the route it would use to recover.** `WithClientCertificate` attaches nothing
   for an out-of-date leaf, so the agent-side guard refuses every route — `POST /v1/session/begin`
   included, so the agent cannot even obtain a session to ask for help. Client certificates are minted
   for 365 days (`client.ClientCertValidity`), renewal is not automatic, **no code path retires a
   binding** (`CertBinding.RetiredAt` is only ever populated by decoding a durable record that already
   carries it — `internal/auth/record.go`), and there is no rebind or rotate route, so the only remedy
   today is **re-enrolment, which mints a NEW agent
   id** (invariant 1 — ids are never reused), losing the identity and its mailbox; an operator can
   otherwise repair the roster record directly. The check was **not** softened, because expiry is the
   only automatic bound on a leaked client key. Filed as `MTLS-CROSSCHECK-FU-CERTEXPIRY` (P1), which
   must land before any deployment binds certificates it cannot rotate.

**The check is a gate at request admission, not a supervisor.** A long poll admitted the instant
before a binding is retired runs to the end of its poll timeout — bounded by `hub.MaxPollTimeout`
(5 minutes) — exactly as it outlives a revoked session. Closing that is owned by the task titled
`AUTH-2-FU-POLLEXPIRY` (id `03d7ca66-110e-4560-803e-1a7825d1accc`; it has no short key), which must
now re-evaluate **both** the session and this cross-check before delivering, not the session alone.

**Enrolment is still NOT invite-gated** — nothing here changes that; see `invite_required` in the
discovery-document section above.

## Peer-bus transport identity (RELAY-45, added 2026-08-14) — NOT YET MOUNTED

`internal/httpapi/peerprincipal.go` answers exactly one question, for the routes a peer bus will
eventually call: **which single ADJACENT BUS is at the other end of THIS TLS connection?** It is a
**TRANSPORT** principal, structurally distinct from the **agent** principal `authMiddleware` attaches
above — different context keys, different Go types, and `RequirePeerPrincipal` never reads a token or
a header:

| | agent principal (`### Authentication`, above) | peer principal (this section) |
| --- | --- | --- |
| names | a fully-qualified `<bus-id>.<agent-id>` (invariant 2) | a bare bus id — `ids.ValidateBusID` refuses the `.` that would make it one |
| credential | a session token (opaque server-side handle), obtained by enrolling and completing a session (invariant 3) | the TLS **client certificate** presented on the connection, bound to a bus id in the durable trust table (`relay.BusTrustRecord.PeerClientTLSCertFingerprint` — see `CONTRACTS-ONDISK.md`) |
| context accessor | `PrincipalFromContext` / `AgentIDFromContext` | `PeerPrincipalFromContext` / `PeerBusIDFromContext` |

**Neither is ever accepted as the other.** `RequirePeerPrincipal` **removes** any agent principal
already attached to the request context rather than leaving it to coexist — a peer bus is very often
also an enrolled agent on the buses it peers with, and a peer request may carry a valid session token
as well as a client certificate. Picking it up as though it authorised the peer request would be
exactly the credential confusion invariant 11's cross-check exists to prevent, so the context handed
downstream carries a value under the agent-principal key that no assertion can succeed against
(`noAgentPrincipal{}`) — a peer route sees exactly one principal, the transport one.

**`Options.PeerPrincipals InboundPeerPrincipals`** (`internal/httpapi/server.go`) is the resolver,
satisfied by `*relay.PeerStore`. **It has NO DEFAULT.** A `nil` value is not a degraded mode to be
filled in with something permissive — it is the fail-closed state, and `RequirePeerPrincipal` refuses
every request behind it while it is unset, logging `errNoPeerResolver` rather than admitting anyone.

**`(*Server).RequirePeerPrincipal(next http.Handler) http.Handler` — the order of its checks is the
contract:**

1. A resolver must be configured (`Options.PeerPrincipals != nil`), or every request is refused.
2. The connection must have arrived over TLS (`r.TLS != nil` — never true on the plaintext listener
   this server refuses to serve, invariant 11, but reachable from a test harness or future in-process
   listener).
3. A client certificate must have been presented. The listener only **requests** one
   (`tls.RequestClientCert`), so an empty `r.TLS.PeerCertificates` is the ORDINARY case for every
   ordinary agent connection, not an exotic one.
4. **`r.TLS.PeerCertificates[0]` only — this handler never iterates the chain.** The peer controls
   every certificate after the leaf, while the handshake's `CertificateVerify` proves possession of
   the leaf's private key alone; a gate that searched the chain would be spoofed by appending the
   victim's public certificate at index 1.
5. The leaf resolves through `InboundPeerPrincipal`, or the request is refused. No fallback, no second
   lookup, no principal on any error path.

**Refusal is `403`, with NO `WWW-Authenticate` challenge, and ONE fixed reason
(`peerPrincipalRefusal = "this connection is not an authorised peer bus"`) for all six causes** — no
resolver configured, no TLS, no certificate, an unknown fingerprint, a withdrawn binding, an ambiguous
binding. This deliberately mirrors the two-string collapse `### Authentication` above already applies
to session-token failures, for the same enumeration-oracle reason, and it is `403` rather than `401`
because the only challenge this server speaks is `Bearer` — offering it here would invite a refused
peer to retry with a session token, i.e. advertise exactly the credential confusion this gate exists to
prevent (see `DECISIONS.md` "2026-08-14 — RELAY-45"). The LOG line (not the response) names which of
the six it was, and — since a certificate is public once presented — includes the fingerprint on every
resolver-failure path so an operator can see exactly what needs to be bound.

**`PeerPrincipal{BusID, CertFingerprint}`** is what a handler downstream receives:
`CertFingerprint` is carried so a handler or an operator can name the credential in a log line without
recomputing it; it is public data, derivable by anyone who completes the handshake. **A handler that
also has a claimed peer identity in the request body MUST cross-check it against this value, never use
it instead** — the same invariant-11 rule the session-token cross-check enforces, applied at bus scope.

**THE MOUNT NOW EXISTS, BUT NO SHIPPED BINARY FEDERATES (corrected 2026-08-14 —
`RELAY-20`, `ed77bba`).** This paragraph previously read "no route uses this yet"; that is no longer
true. `internal/httpapi/peermount.go` registers `/v1/peer/enroll`, `/v1/peer/relay` and
`/v1/peer/roster` behind `RequirePeerPrincipal`, and `mountPeerRoute` couples registration to
wrapping in one function so a path can never be recorded as a peer route without the certificate gate
around it.

**Registration is conditional, and all-or-nothing.** Routes appear only when `Options.Peer` is a
**complete** `PeerSurface` (`Enroll`, `Relay`, `Roster`, `Ack`, `Registry` and `Trust` all non-nil —
`Ack` was added by `ACK-3` on 2026-08-16, and a build supplying the other five registers **nothing**)
**and** `Options.PeerPrincipals` is set. A partial surface, or a complete one with no resolver, registers
**nothing** and logs an `Error` — every `/v1/peer/` path then answers as an unregistered path, because
a registered-but-refusing surface would advertise federation while serving nobody.

**Nothing in `cmd/agent-bus` constructs a `PeerSurface` or a `*relay.PeerStore` today** (`RELAY-24` is
the composition root and is still open). So a peer bus cannot reach these routes on a server built
from this repo's `main` — the conclusion an operator should draw is unchanged, even though the reason
has moved from "not mounted" to "not composed".

### Relay ingress: the unknown-recipient refusal (RELAY-21, `14eafd9`, added 2026-08-14)

**Not reachable on a server built from this repo's `main`** — see the paragraph directly above. This
row specifies what `POST /v1/peer/relay` answers once `RELAY-24` composes the surface; it is a
wire-visible contract a peer bus's client already classifies (`relay.peerErrorCode`), so it is pinned
here rather than left to the code.

| Method | Path | Auth | Status | Response |
| --- | --- | --- | --- | --- |
| `POST` | `/v1/peer/relay` | peer principal (TLS client certificate — see above) | 404 | `{"error":"unknown_recipient"}` — the relayed message names an agent in **this bus's** namespace that this bus's roster does not hold. **Nothing is written**, and the answer is **FINAL**. `14eafd9` added both the check and this code; before it there was no production `AcceptRelay` callback, and any unclassified callback failure fell into the generic 503 bucket — so this is a **new** classification, not a changed wire answer (no build ever served the 503). |
| `POST` | `/v1/peer/relay` | peer principal (TLS client certificate — see above) | 400 | `{"error":"unsupported_relay_version"}` — the envelope's `protocol_version` is a relay wire-protocol version this bus does not implement (`RELAY-23`). Resolved as **check 0** of `ValidateRelayRequest`, before any field it governs — an **absent** value reads as 1, an unrecognised one is **REFUSED, never defaulted** (invariant 10). **FINAL** (a retry installs no new binary) and the peer is **not** disconnected. Distinct from the ACK frame's `unsupported_ack_version` on purpose (`RELAY-53`): two codes let an operator read *which* frame the far end could not parse. |

**`Nothing is written` is a durability claim.** `relay.Acceptor.Accept` asks the roster *before* the
durable write, so a name nobody holds costs this bus nothing permanent — an id admitted by anything
other than the roster would burn that name for ever, since ids are never reused, including across
restarts (invariant 1).

**404 rather than 503, deliberately.** A 503 would drive the sending peer's retry machinery for its
whole retry horizon, letting a peer aim traffic at names that do not exist here and have our own
control retry each one. Every 4xx but 408 and 429 is FINAL — `(*relay.PeerRefusedError).Retriable`
decides from the **status**, not the error string — so the sending bus stops and its operator gets a
code whose remedy is its own roster. The sentinel is `relay.ErrUnknownLocalRecipient`; the code
constant is `relay.CodeUnknownRecipient`. It is deliberately **not** `invalid_relay`: the envelope is
well formed and its signature verified, so a peer told "invalid" would hunt a malformed field it does
not have. It leaks no roster membership to anyone who could not already ask — only a peered bus
reaches this handler, and peers exchange full rosters over the roster-sync surface by design.

### Panic log records (added 2026-08-08 — CORE-14, CORE-6)

`LoggingMiddleware` emits one `msg=request` record per request. Two fields are added to it, **and
only ever when the handler panicked**, so an ordinary line is unchanged and a log query can select on
their presence:

| Field | Value | Meaning |
| --- | --- | --- |
| `panicked` | `true` | The handler panicked. **This, not `status`, is the field an error-rate metric must key on.** |
| `panic_after_write` | `true` / `false` | `false`: recovery answered 500 and `status` is the truth. `true`: the response had already begun under a status that promised success — very often `200` — and HTTP gives no way to retract it. |

One further field, emitted independently of a panic:

| Field | Value | Meaning |
| --- | --- | --- |
| `hijacked` | `true` | The handler took the raw connection over via `http.Hijacker`. `status` is then **`0`** — "not known here": from that point the handler writes its own status line straight to the socket and the middleware never sees it, and inventing `200` would be the same fabricated success this section is about. **Exception:** if the handler wrote a real status *before* hijacking (an upgrade sends `101` first), that status is kept, because it genuinely is what went out. A `Write` *after* the hijack cannot change it. |

**Why this exists (CORE-14).** A handler that wrote a response and *then* panicked used to log
`status=200` and nothing else. The response itself is unfixable once bytes are on the wire — that is
HTTP — but the LOG was reporting a failed request as a success, so anyone reading the logs, or any
error-rate metric built from them, saw green. That is the same defect class as a control that reports
success while the thing it controls has failed, and it is a correctness bug in the audit trail, not
cosmetics. **Measured against a real `net/http` server, what the client receives is not visibly
broken** — a correctly framed `200` with no read error (`Content-Length` set if the handler never
flushed, a properly terminated `Transfer-Encoding: chunked` body if it did) — which is exactly why
the log has to say otherwise; it is the only place the failure is visible at all. `status` still reports what
the client actually received, which is the honest answer; the markers say when that number cannot be
taken at face value. The `level=error` `msg="panic serving request"` record that precedes it carries
`panic_after_write` too, since that is the line an operator reads first.

**The hijack case, found by the security gate 2026-08-08 and fixed in the same task.** A hijacking
handler bypasses the recorder entirely, so `wrote` stays false. Without the `hijacked` flag, a
handler that hijacked, served a response on the raw socket and then panicked logged
`panicked=true panic_after_write=false status=500` — claiming recovery had answered cleanly while the
client had already been sent something else. That is a **false negative in the control itself**, and
it is reachable precisely because CORE-13 (below) made `Hijack` work through the wrapper.
`panic_after_write` is therefore computed from "the response has begun" — `wrote || hijacked` — and
recovery does **not** attempt its 500 on a hijacked connection. No handler hijacks today; the POLL
epic is the likely first.

**Stack traces are no longer truncated (CORE-6).** `internal/logging` caps every field value at
`maxValueLen` = 1024 bytes so an attacker-controlled value (a header, a request id, an error string
built from client input) cannot turn one record into a multi-kilobyte payload. The `stack` key is now
**exempt**, with its own larger bound:

| Constant (`internal/logging`) | Value | Note |
| --- | --- | --- |
| `StackKey` | `"stack"` | The one exempt field key. |
| `MaxStackValueLen` | `8192` | The exempt cap. A bound, **not** "unlimited". |

Every other field, and `msg`, keep the 1024-byte cap. The exemption is narrow because the reasoning
behind the cap is untouched for every other field: a stack is produced by `runtime/debug.Stack`, so
its length is the *server's*, not the caller's. It mattered because a stack's **tail** is its useful
half — the deepest frames are where the panic happened — so a 1024-byte cut discarded exactly what an
operator needs. Measured: a real `net/http` request path renders 1238 bytes, while the pre-existing
test drove the handler through `httptest`, whose shorter call stack rendered 962 bytes, stayed under
the cap and passed. **The test now searches for a recursion depth that provably exceeds the old cap**
rather than hardcoding one (a frame's rendered size depends on the absolute source path, so a fixed
depth is not portable), which is what stops the blind spot returning the next time the constant is
tuned.

### Response-writer capabilities through the middleware (added 2026-08-08 — CORE-13)

`LoggingMiddleware` hands the handler a wrapper, and that wrapper advertises `http.Flusher`,
`http.Hijacker` and `io.ReaderFrom` **if and only if the underlying `http.ResponseWriter` does**. It
previously declared `Flush()` and `Hijack()` unconditionally, so `w.(http.Flusher)` and
`w.(http.Hijacker)` always succeeded — feature detection, which is the correct pattern for optional
interfaces, was being told a lie, and a handler took the streaming or upgrade path only to find out
at call time. `io.ReaderFrom` was dropped entirely, costing `net/http`'s sendfile fast path for large
responses; it is now forwarded, and the bytes it copies are counted in the record's `bytes` field.

`http.CloseNotifier` and `http.Pusher` are deliberately **not** forwarded (`CloseNotifier` is
deprecated in favour of `Request.Context`; `Pusher` is HTTP/2 server push, which this bus does not
serve). Neither was advertised before, so nothing regresses. Relevant to the POLL epic, which may
want `Flush`.

**Rule for every future route: register it through `(*Server).route`, never `mux.HandleFunc`
directly.** A route registered the wrong way is still authenticated — the middleware wraps the whole
mux regardless of how a pattern got onto it — but it is invisible to `Routes()` and therefore to
`TestEveryRouteRequiresAuth`'s enumeration, which is a testing gap, not a security hole; do not create
that gap when a five-minute fix (using the helper) avoids it.

**Caveat from the security audit: `OPTIONS * HTTP/1.1` never reaches this middleware.** Go 1.19's
`net/http` answers a server-wide `OPTIONS *` request with its own `globalOptionsHandler` (a bare `200`,
`Content-Length: 0`) ABOVE the application handler entirely — `authMiddleware` and the mux never see
it. It exposes no application data or state, so this is not a hole in the credential model, but it is a
real place a blanket "every request is authenticated" claim would be wrong, so it is recorded here
rather than left for someone to discover by testing it. `net/http.Server.DisableGeneralOptionsHandler`
would route it through the mux like everything else, but it is go1.20+ and this module is pinned to
go1.19 (see `CLAUDE.md`, "Runtime target") — not fixable here without a version bump recorded in
`DECISIONS.md` first.

## A FOURTH peer route: `POST /v1/peer/ack` — peer-hop delivery ACK/NACK (`ACK-3`, added 2026-08-16)

The peer surface is now **four** routes, not three. `internal/httpapi/peermount.go` registers
`/v1/peer/ack` through the same `mountPeerRoute` that registers the other three, so it is recorded as
a peer route and wrapped in `RequirePeerPrincipal` in one function and cannot become one without the
other. `httpapi.PeerSurface` gained a required `Ack *relay.AckHandler` field; a surface missing it
registers **nothing at all**, exactly like a surface missing `Relay`.

The route carries ONE terminal delivery outcome for ONE `(correlation key, recipient)` pair. It is
specified by `ACK-CONTRACT.md` §9; the paragraphs below are the wire contract.

### The frame

`Content-Type: application/json`. There is **no idempotency-key header** on this route, unlike
`/v1/peer/relay`: an ACK creates no applied-key entry, and its idempotency is the durable ACK record's
own `(correlation key, recipient)` row plus the absorbing-terminal rule over it.

```json
{ "protocol_version": 1,
  "correlation_key":  "<origin-bus>-<seq>",
  "recipient":        "<bus-id>.<agent-id>",
  "outcome":          "delivered" | "refused" | "undeliverable",
  "class":            "<one of the twelve closed classes>",
  "emitted_at":       1700000000000,
  "attestation":      { "signature": "<base64, exactly 64 bytes>" } }
```

**`protocol_version` spends the ALREADY-RESERVED `relay-wire-version = 1` and reserves nothing new.**
See `CONTRACTS-ONDISK.md`, "Record types / wire protocol versions". The JSON key is
`protocol_version` and **must never be `version`** — `RosterUpdate` already owns `version` on a
neighbouring peer envelope and that one is a monotonic *roster epoch*, not a format number.
`ACK-CONTRACT.md` §9.2 sketches the field as `wire_version`; **the sketch is superseded**, because a
single reserved version spelled two different ways on two frames of one protocol is how a future
negotiation task ends up writing two parsers.

**Reading rules.** A **missing** `protocol_version` reads as **1** — the only backward-compatible
read, and exact, since version 1 *is* this format. An **unrecognised** version is **REFUSED, never
defaulted**: `400 {"error":"unsupported_ack_version"}`. The stakes are higher than for an outbox row
because this frame carries a TERMINAL outcome and terminal is absorbing, so a frame read under the
wrong rules could durably settle a message in a way that can never be revisited.

**`class` is present IFF `outcome` is a negative terminal, and the two halves of the closed set are
enforced in BOTH directions:** `refused` takes one of the three *recipient-emitted* classes,
`undeliverable` one of the nine *bus-emitted* ones, and `delivered` takes none. That is an
anti-forgery check, not tidiness — without it a peer sends `outcome=refused, class=no_route` and this
bus records its own routing failure as the recipient's decision.

**`attestation` is present IFF the outcome is recipient-sourced** (`delivered`, `refused`) and
forbidden on `undeliverable`. It is checked for **SHAPE ONLY** — present, and exactly
`signing.SignatureSize` (64) bytes. **No bus verifies it and no bus may claim to**; nothing
distributes agents' messaging public keys, so it is end-to-end unverifiable by anybody today,
including the sender. An `{"attestation":{"signature":""}}` is refused rather than read as absent.

**`emitted_at` is required and positive, and is NEVER persisted.** It is provenance for the operator
log. The durable record's `accepted_at`/`settled_at` are this bus's own clock, and `emitted_at` takes
part in no comparison — two acknowledgements are compared on outcome and class alone, so a peer
re-sending the same settlement a second later is a retry rather than a violation.

**There is NO free-text field, and no field whose length a remote party chooses.** The request cap is
therefore derived exactly rather than guessed: **`MaxAckBytes` = 4 KiB**, against a widest legal frame
of ~560 bytes. It is deliberately three orders of magnitude below `MaxRelayBytes` (256 KiB), because
an ACK has no body and no recipient list and sharing the relay's cap would let an ACK-shaped stream
cost far more than an ACK can legally cost.

**THERE IS NO FIELD NAMING A BUS, AND ONE MUST NEVER BE ADDED.** See "Where the peer bus id comes
from" below. The decoder sets `DisallowUnknownFields`, so a peer that invents one is refused 400.

### Status codes

| Situation | Answer |
| --- | --- |
| Accepted | **200** `{"accepted":true,"duplicate":false}` |
| Idempotent replay — same pair, SAME outcome and class | **200** `{"accepted":true,"duplicate":true}`. The original result stands, **nothing is re-applied**, nobody is disconnected. |
| No obligation binds this peer to this key — a two-armed test since 2026-08-21, see [The binding rule was WIDENED by ONE case](#the-binding-rule-was-widened-by-one-case--2026-08-21-ack-5) — **or** — **AT THE ORIGIN BUS ONLY, amended 2026-08-21 (`ACK-5`)** — no ACK row exists for that `(key, recipient)` | **409** `{"error":"idempotency_violation"}` |
| A DIFFERENT terminal outcome is already recorded | **409** `{"error":"idempotency_violation"}` |
| **NEW (`ACK-5`, 2026-08-21).** No ACK row exists **and this bus is not the origin** of the key, and the outcome was carried one hop further back and accepted there | **200** `{"accepted":true,"duplicate":false}` — see [Back-propagation](#back-propagation-on-the-peer-surface-ack-5-2026-08-21) |
| **NEW (`ACK-5`).** Same, but the upstream hop failed **retriably** (unreachable, 5xx, 408/429, or the upstream bus is not in this bus's peer registry) | **503** `{"error":"unavailable"}` — nothing was written anywhere; re-offer the identical frame |
| **NEW (`ACK-5`).** Same, but the upstream hop answered **409** — the ONE final refusal that means it DECIDED something about the frame (no obligation binds that recipient there, or a conflicting terminal already stands) | **200** `{"accepted":true,"duplicate":false}` — deliberately NOT the upstream's verdict; the outcome is DROPPED and the drop is logged at WARN |
| **NEW (`ACK-5`).** Same, but the upstream hop answered any OTHER final status — **404** (a peer that does not serve the route yet), **403** (a peer that will not talk to us) or **400** (a frame it could not parse) | **503** `{"error":"unavailable"}`. **CORRECTED IN PLACE 2026-08-21 AFTER REVIEW; this row previously read "refused finally (any other 4xx, including a 404 …) → 200" and swept all four statuses together, which was a defect.** None of these three decided anything about the frame, all are OPERATOR-recoverable, and a 200 would tell the recipient its outcome was accepted when nothing anywhere recorded it. The arm now tests `refused.StatusCode == http.StatusConflict` and nothing else (`cmd/agent-bus/relaywiring.go:2078`); the separate 404/403/400 log line is at `:2098` |
| **NEW (`ACK-5`).** Same, but this build has **no back-propagation seam** (no peer store, so no registry and no propagator) | **409** `{"error":"idempotency_violation"}` — the uniform refusal, logged at WARN because from an operator's side the outcome stops here |
| Malformed frame, unrecognised outcome or class, wrong class half, wrong attestation shape, missing `emitted_at`, malformed id | **400** `{"error":"invalid_request"}` |
| Unrecognised `protocol_version` | **400** `{"error":"unsupported_ack_version"}` |
| This peer is at its in-flight limit on this bus | **503** `{"error":"unavailable"}` |
| The durable write failed, or the lifecycle table has no log attached | **503** `{"error":"unavailable"}` |
| Not an authorised peer bus | **403**, from `RequirePeerPrincipal`'s one fixed refusal |
| Not `POST` / not JSON / over `MaxAckBytes` | **405** / **415** / **413** |

**The two 409 rows share one code DELIBERATELY, and that must not be "fixed".** "No obligation binds
you to this key", "we owe it to a different peer", "the key names a third bus", "the row was swept"
and "there is no ACK row for that recipient" are **byte-identical on the wire**. Distinguishing them
would hand any peered bus an oracle for *"did bus A send message K to bus B"*, and by extension for
which agents exist and are being written to. It is the deliberate analogue of the
`409 no-matching-reservation` indistinguishability invariant 10 preserves. The causes are told apart
**only in the operator log**, through one redaction point that elides every id a remote party chose
the bytes of.

> **NARROWED IN PLACE 2026-08-21 (`ACK-5`): the paragraph above is true AT THE ORIGIN BUS, and the
> phrase "there is no ACK row for that recipient" no longer implies 409 anywhere else.** A bus that
> did not mint the correlation key never held a row for it, by design, so a missing row there is the
> ordinary shape of a **transit acknowledgement** rather than a refusal, and the outcome is carried
> one hop further back instead (`cmd/agent-bus/relaywiring.go:1995`, `disposeUnrecordedAck`). The
> origin test is `relay.DisposeAck(busID, key, nil)` — the one spelling of "are we the origin" — and
> **at the origin `relay.ErrAckNotBound` is returned UNCHANGED**, so the uniform 409 is byte-identical
> to what it was before this task. The three causes it collapses at the origin are unchanged too.

**A metered refusal is 503 and NOT 4xx, and that choice is load-bearing.**
`relay.PeerRefusedError.Retriable` treats every 4xx except 408/429 as **FINAL**, so a throttled
acknowledgement answered 4xx would be **abandoned** by the sender and the recipient's decision would
never reach the origin. Nothing durable is written for a metered refusal, so retrying is correct.

**NO refusal on this route closes a connection** (`ACK-CONTRACT.md` §12, invariant 10). A peer link
multiplexes an entire remote bus's roster, and a merely buggy peer reaches every refusal here
trivially.

### Where the peer bus id comes from — the one thing to get right

`relay.AuthorizePeerAck` authorises `DeriveJobID(peerBusID, correlationKey)`: it settles the frame
against an outbox job **this bus durably wrote to that peer**. So whoever controls `peerBusID`
controls whose obligations they may settle.

**It is `httpapi.PeerPrincipal.BusID` — the bus id `RequirePeerPrincipal` resolved from the TLS
CLIENT CERTIFICATE — and it is never read from the request.** That is structural rather than
documented:

1. the frame declares **no field** a bus id could be read from, and the decoder rejects an invented
   one;
2. `*relay.AckHandler` is deliberately **not an `http.Handler`** — it exposes
   `ServeAuthenticated(w, r, peerBusID)` — so it cannot be handed to a mux at all, and "forgot the
   principal" is a compile error rather than a silent forgery hole;
3. the only adapter that supplies the parameter is `internal/httpapi/peermount.go`'s `servePeerAck`,
   where the sole source of a bus id in scope is `PeerPrincipalFromContext`.

The binding does **not** require the correlation key's bus half to equal the acknowledging peer. In an
A→B→C chain, C's outcome reaches A **via B** and the key's bus half is A's; a "bus half must equal the
peer" rule would be wrong and would break multi-hop.

### The binding rule was WIDENED by ONE case — 2026-08-21 (`ACK-5`)

**The route now calls `relay.AuthorizePeerAckVia` (`internal/relay/ackback.go:831`) from
`internal/relay/ackhttp.go:398`, not `AuthorizePeerAck` directly.** The rule above is the DIRECT arm
and is unchanged; a second, INDIRECT arm was added because the direct one alone made the **last hop**
of every multi-hop path fail. On A→B→C, A keys its outbox job on `DeriveJobID(C, K)` —
`Forwarder.targets` routes on `Registry.Route(recipient)`, the recipient's HOME bus
(`internal/relay/forward.go:1044`) — while the acknowledgement arrives over A's mutual-TLS link with
**B**, so the direct lookup asked for `DeriveJobID(B, K)`, which nothing ever wrote. The two spellings
coincide only on a direct peer link. `ACK-CONTRACT.md` §6.2 carries the full amendment.

A frame from AUTHENTICATED peer `P` for key `K` naming recipient `R` binds if **EITHER**:

| Arm | Test |
| --- | --- |
| **DIRECT** (tried FIRST) | `DeriveJobID(P, K)` names an outbox job this bus durably wrote **AND the recipient `R`'s home bus equals `P`** (case-folded). The recipient conjunct is `ACK-4-FU-RECIPIENT-BINDING` (2026-08-23): the outbox job is keyed on the recipient's home bus, so `DeriveJobID(P, K)` covers only recipients on `P`; a frame naming a recipient on a DIFFERENT bus is refused (falls through to INDIRECT). Without it, once a key has more than one recipient row a peer bound for one recipient could settle any sibling recipient of `K`, uncorrectably (terminal is absorbing). |
| **INDIRECT** (new) | Let `D` be the **bus half of `R`** (invariant 2). ALL of: `D` is not `P` (case-folded) and `D` is not this bus; **the address this bus would dial for `D` equals the address it would dial for `P`**, both resolved and both non-empty; and `DeriveJobID(D, K)` names an outbox job this bus durably wrote whose peer bus id and origin message id are the two we asked for. |

**The address comparison is the security core, and it is computed from THIS BUS's own peer
configuration — never from the frame.** The only peer-supplied value that reaches it is a bus id
parsed out of an already-validated agent id; the answer comes from this bus's routing table
(`Registry.PeerBaseURL`, wired at `cmd/agent-bus/relaywiring.go:1396`). `R` therefore selects WHICH
job we look for — it cannot conjure one, and it cannot make an unrelated peer the next hop for a
destination we route elsewhere. On a `-route-for` topology a route record's bus id is the DESTINATION
and its base URL is the address to dial to reach it, so `PeerBaseURL(D) == PeerBaseURL(P)` is exactly
the question *is `P` the hop we route `D` through*.

**No new answer appears on the wire.** Every indirect refusal returns the same `relay.ErrAckNotBound`
by identity, so the 409 above is byte-identical whichever arm refused: the widening adds a way to be
BOUND, never a new way to be told why you were not. A malformed id still answers 400, because
`AuthorizePeerAck` owns that validation and any non-`ErrAckNotBound` error from it is returned
unchanged with the indirect arm never reached. A build that passes no routing resolver
(`AckConfig.NextHopAddress` nil, `internal/relay/ackhttp.go:176`) **fails closed to the direct arm's
answer** — byte-for-byte the pre-`ACK-5` behaviour.

**`ACK-4-FU-RECIPIENT-BINDING` is CLOSED (2026-08-23).** Both arms now bind the recipient's home bus
to the acknowledging peer: the INDIRECT arm always did (`P` must be the hop we route `R`'s bus
through), and the DIRECT arm now requires `EqualFold(homeBus(R), P)` (the table above). A peer bound
for `K` can therefore settle only recipients whose home bus is the one its obligation names — not any
sibling recipient of `K`, which mattered the moment a key gained a second recipient row
(`ACK-12-FU-DESTINATION-ROW`). The still-separate second conjunct is that a row exists for a recipient
the SENDER named ("no ACK row for that pair" refusal at the origin); the two are conjunctive.

**Cost.** The indirect arm adds two registry lookups (`RLock`) and, only if they agree, a **second**
`Outbox.Lookup` — the exclusive-mutex O(n) sweep the rate-limiting note below is about. The routing
checks run first deliberately, so a peer can provoke that second sweep only for a destination this bus
already routes through it.

### Rate limiting, and the decision that is NOT taken here

`relay.AuthorizePeerAck` reaches `Outbox.Lookup`, which takes the outbox's **exclusive mutex** and
runs an O(n) sweep — the same mutex `Enqueue` and `Settle` need. The route therefore meters **before
the body is read** and long before that call, keyed on the authenticated peer.

**It reuses the meter that already exists** — `cmd/agent-bus`'s `peerAdmission.enter`, the
per-authenticated-peer in-flight bound `RELAY-22` built for relay ingest — through an
interface-shaped `AckConfig.Admit` seam. `ACK-CONTRACT.md` §16 Q3 asks whether this surface needs its
own limit and defers the answer to the open task `48223968` ("Choose the abuse-control primitive for a
MULTI-PRINCIPAL relay link"). **This route does not answer it**; when that task rules, the ruling
lands in one place and both peer routes inherit it. The *concurrency* half is used and the *quota*
half is not: the quota counts applied-key entries and an ACK creates none.

### Back-propagation on the peer surface (`ACK-5`, 2026-08-21)

**A bus that is not the origin of a correlation key now carries the outcome one hop further back
instead of refusing it.** Before this task the peer surface was a pure sink: `ack.Store.Settle`
answering `ErrNoRecord` was the uniform 409 wherever it happened. That made `A→B→C` unreachable for
plane C, because `B` holds no lifecycle row for a message it merely relayed.

| Question | Answer |
| --- | --- |
| Which bus holds the durable row? | **Only the origin** — the bus that minted the correlation key. `hub.recordAcceptance` returns early for relayed ingest (`internal/hub/hub.go:2198`), so an intermediate or terminal bus writes none and none is written by this path either. |
| What decides "am I the origin"? | `relay.DisposeAck(localBusID, correlationKey, nil)`. With a nil path it answers `AckStopAtOrigin` iff the key's bus half is ours, compared with `strings.EqualFold`. It is the ONE spelling of that question. |
| Where does the next hop come from? | `relay.UpstreamHop` over **this bus's own stored `bus_path`** (`store.RelayProvenance.BusPath`), at index `len-2`. The path must **end at this bus** or the hop is refused — never searched for. |
| Where does its ADDRESS come from? | `relay.Registry.PeerBaseURL`, this bus's peer registry, re-resolved per emission. **Nothing in the frame names an address, host, scheme or destination bus**, and there is no field one could arrive in. A bus never contacts a bus it is not peered with. |
| What is forwarded? | The frame rebuilt from the **validated** value by `relay.AckFrameFrom`: outcome, class, `emitted_at` and the recipient's 64-byte attestation reproduced **exactly**. An intermediate re-signs nothing, re-classifies nothing and re-times nothing. `protocol_version` is (re)stamped by `Client.PeerAck` with the version **this** bus speaks. |
| Is anything durable written here? | **No.** Not a row, not a queue entry, not a record type. |
| Then how is invariant 4 kept? | **The hop is synchronous.** This bus does not answer its downstream peer until the upstream hop has answered, and that hop does not answer until *its* next hop has. The guarantee holds **end to end through the chain** rather than through a local write. |

**Why an upstream 409 — and ONLY a 409 — is answered 200 downstream, and not forwarded.**
**NARROWED IN PLACE 2026-08-21 AFTER REVIEW: this paragraph read "Why a FINAL upstream refusal is
answered 200" and applied to every final status, which was a defect.** The dividing question is not
"was it final" but **"did the upstream DECIDE anything about this frame"**, because
`PeerRefusedError.Retriable()` treats every 4xx except 408/429 as final. A **409** is the one final
refusal that means the upstream understood the frame and made a decision about it, and it is absorbed
for two load-bearing reasons: re-offering a frame the upstream has *finally* refused is exactly the
retry amplification `ACK-CONTRACT.md` §9.3 exists to stop, and forwarding the origin's 409 verbatim
would make this hop an **oracle** — any bound peer could learn whether the ORIGIN holds a row for a
recipient it named, the uniform-answer property leaked one hop back. The settlement really is dropped,
so it is logged loudly and specifically (invariant 6) with the local bus, the peer bus, the elided
correlation key and recipient, the outcome and the upstream status. A **404, 403 or 400 is answered
503** instead: each decided nothing, each is OPERATOR-recoverable (upgrade the binary, re-peer the
bus, fix the encoder), so "not now" is truthful and a later identical offer can succeed.

**There is no retry queue behind this seam and there must not be one.** A retriable failure is a 503
and the downstream peer re-offers the identical frame; nothing was written anywhere, so nothing is
lost. Retry, backoff and bounce are `ACK-7`/`ACK-14`, once, beside the durable outbox that survives a
restart. **NOBODY IS DISCONNECTED on any arm** (§12, invariant 10): a de-peered neighbour, an upstream
bus that is simply down, a message swept by retention and a merely buggy peer all reach these lines.

### Not reachable without federation

Like the other three, this route exists only on a build that composes a complete `PeerSurface` with an
inbound principal resolver. `cmd/agent-bus/relaywiring.go` now also requires a `*relay.Outbox` and a
`*ack.Store`; a federating build missing either **fails at startup** rather than serving a route that
could bind nothing or record nothing.

The back-propagator (`ACK-5`) is built on **the same peer-store branch**, for the same reason: it
dials peers, so it needs the pinned mutual-TLS peer client (invariant 11) and the one routing table.
A build with neither has nowhere to send an acknowledgement, and `federationOptions.AckTransit` is
then nil, whose cost is stated rather than hidden (see the no-seam status-code row above).

> **CORRECTED IN PLACE 2026-08-21 (`ACK-5`).** This paragraph called a nil `AckTransit` "a
> **legitimate** configuration for a leaf bus". The field's own doc was corrected against `main.go`
> and now says the composition root cannot produce one: the back-propagator is built on the
> `peerStore != nil` branch with every failure FATAL, and `newFederation` is reached only from
> `bindable > 0`, which implies that branch (`cmd/agent-bus/relaywiring.go:1133-1143`). **The nil arm
> is a fail-closed default reachable only from a test or a future composition**, kept because the
> alternative — assuming non-nil on a peer-facing path — is a nil dereference served to a remote
> party.

### Rollout ordering — receivers before senders

`POST /v1/peer/ack` is a **new route**. A peer running a binary from before this change answers
**404**, and `PeerRefusedError.Retriable` treats 404 as **FINAL** — so an acknowledgement sent to a
not-yet-upgraded peer is **abandoned, not retried**. Upgrade every bus that might be acknowledged
before any bus starts emitting. This is the same hazard family as
`RELAY-51`, which is **not** fixed here and is not made worse: the frame ships complete and versioned,
so the first task needing a new field has a version to bump.

> **AMENDED 2026-08-21 (`ACK-5`) — the ordering is now LIVE, not merely satisfiable.** This paragraph
> used to end *"Nothing in this build emits one yet (`ACK-5` owns emission and lands after), so the
> ordering is satisfiable rather than merely stated."* **That is now false**: `ACK-5` wired the
> emitting half (`relay.BackPropagator`, built at `cmd/agent-bus/main.go` on the peer-store branch),
> so a federating build **does** emit `POST /v1/peer/ack`. The remedy is unchanged and is now
> load-bearing rather than advisory: **upgrade receivers before senders.** An acknowledgement offered
> to a pre-`ACK-3` peer gets a 404 and `PeerRefusedError.Retriable` reads 404 as FINAL.
>
> **CORRECTED IN PLACE 2026-08-21 AFTER REVIEW.** This paragraph ended *"and the intermediate answers
> its downstream peer **200** while dropping the outcome (logged at WARN) — so the loss is diagnosable
> in the operator log and nowhere on the wire."* **That is no longer true, and it was the defect the
> narrowing above fixed:** a 404 now falls to the **503** arm, so the downstream peer is told "not
> now" and its identical re-offer succeeds once the upstream is upgraded. Only a 409 is absorbed into
> a 200. The 404 case is still logged at WARN, with the remedy named
> (`cmd/agent-bus/relaywiring.go:2098`), because nothing about it resolves on its own.

## The RECIPIENT ack route: `POST /v1/ack` — an explicit application acknowledgement (`ACK-6`, added 2026-08-16)

The **agent-surface** half of the delivery acknowledgement plane, and the ONLY way a message becomes
`delivered` or `refused` on this bus. `ACK-CONTRACT.md` §4 is the ruling it implements:

> **Delivery to an inbox or a poll is NOT recipient receipt. An EXPLICIT application ACK is
> required.** Plane C is reached only by the recipient calling this route; it is NEVER inferred from
> a cursor advancing.

Three reasons, and none of them is a preference:

1. **The bus cannot know what the recipient knows.** It carries the sender's signature as opaque
   bytes and never verifies it (`internal/store/message.go`, "the BUS enforces SHAPE and the
   RECIPIENT enforces AUTHENTICITY"). A bus that auto-ACKed on poll would assert, on the recipient's
   behalf, a fact only the recipient can establish.
2. **There is no server-side per-recipient delivery state to derive it from**, and adding one is
   strictly MORE state than an explicit ACK. The cursor is opaque and client-held.
3. **A poll is replayable.** Delivery is at-least-once and an unrecognised cursor is remapped to
   position 0 — one message would produce many "receipts".

There is therefore **no `polled` state and one must not be added.** A message that has been polled
and not acknowledged stays `accepted` / `in_flight`, and an agent that never acknowledges leaves its
sender's row non-terminal until the 24h retention window sweeps it, after which the status route
answers `unknown`. That cost is real, is the honest answer, and is why `unknown` is a first-class
value rather than an error.

### The route

Registered inside the `Options.Hub != nil` block, through `(*Server).route`, so it is authenticated
by **being registered**: `authMiddleware` is default-deny and this path is not on the allow-list. It
therefore requires a bearer session, and is subject to invariant 11's session/certificate
cross-check **on the same terms as every other authenticated route — which is weaker than "mTLS is
enforced" and must not be read as that**. The listener is `ClientAuth: tls.RequestClientCert`
(`cmd/agent-bus/tlslisten.go:152`; line 27 is only the comment that explains it), so the cross-check bites only for an agent that HAS a live
certificate binding; **for an agent with none, this route is bearer-token-only.** That is inherited
from the platform, neither narrowed nor widened here.

> **CORRECTED 2026-08-21 (`ACK-6` reviewer finding D2).** This sentence previously read *"requires a
> bearer session **and** passes invariant 11's mTLS cross-check"*. `internal/httpapi/ack.go:24-30`
> was written specifically to deny that reading — *"it is written out so nobody reads this file as
> evidence that mutual TLS is enforced"* — so a contract file was asserting the very thing the
> handler's own comment exists to prevent. Overstating an authentication property is the dangerous
> direction: a reader plans against a guarantee the build does not make.

It is a **bare path with no trailing slash**, so it does not collide with
`GET /v1/ack/<correlation-key>` (`ACK-9`), which `http.ServeMux` resolves as a separate subtree.

`POST /v1/peer/ack` is the *other* half of this plane and is a **different surface** with a
**different gate** (a peer certificate through `RequirePeerPrincipal`, no session). §6.1's documented
one-factor narrowing on the peer surface is **inherited and not widened**: an ACK frame is never
accepted on the agent surface on behalf of a peer bus, and a session token is never consulted on the
peer surface. The concrete expression of that is `relay.AckSurfaceAgent`, a compile-time constant
supplied by the mount site and **never read from the frame**.

### The frame

Identical to `POST /v1/peer/ack`'s (`relay.PeerAckRequest`, bounded by `relay.MaxAckBytes` = 4 KiB) —
deliberately one type for both surfaces, so the closed vocabulary has one spelling and the two
surfaces cannot drift into accepting different things.

```json
{ "protocol_version": 1,
  "correlation_key": "<origin-bus>-<seq>",
  "recipient":       "<bus-id>.<agent-id>",
  "outcome":         "delivered" | "refused",
  "class":           "recipient_refused_policy" | "recipient_refused_undecodable" | "recipient_refused_not_addressed",
  "emitted_at":      1755000000000,
  "attestation":     { "signature": "<base64, exactly 64 bytes>" } }
```

- **`recipient` MUST equal the authenticated principal.** It is a CLAIM that is compared and then
  **discarded**; the row is looked up with the context principal, exactly as `/v1/send` attributes a
  message to the context principal and discards `body.sender` (invariant 1). There is no path by
  which a frame-supplied value reaches the lookup key.
- **`outcome` is `delivered` or `refused` ONLY.** `undeliverable` is a claim about the federation's
  routing, asserted by a bus about its own failure to deliver; a recipient application has no
  standing to make it and the frame is refused before the hub is reached.
- **`class` is REQUIRED on `refused`, forbidden on `delivered`, and must come from the three
  RECIPIENT-emitted members** of the closed twelve (`ACK-CONTRACT.md` §5.2). A recipient sending
  `refused` + `horizon_expired` would have this bus record ITS OWN routing failure as THE RECIPIENT'S
  DECISION. **There is no free-text reason field on this frame and one must not be added** (invariant
  6): a recipient-chosen string in an append-only trail is a body by another name. Recipient and
  sender already have an end-to-end message channel for prose.
- **`attestation` is REQUIRED** and is checked for **SHAPE ONLY** — present, exactly
  `signing.SignatureSize` (64) bytes — byte-for-byte the posture already taken for message
  signatures. **No bus verifies it and no bus may claim to**: nothing distributes agents' messaging
  public keys, so it is end-to-end unverifiable by anybody today, **including the sender** (§16 Q1).
  The durable record therefore records `attested_by: recipient_signature_unverified`, and there is
  deliberately **no value meaning "verified"**. The signature bytes themselves are **not persisted** —
  the record (`CONTRACTS-ONDISK.md`, kind `"ack"`) has no field for them.
- **`emitted_at` is REQUIRED and positive.** It is PROVENANCE FOR THE LOG ONLY, is never persisted
  and never compared: the durable `accepted_at`/`settled_at` are this bus's clock.
- An **absent** `protocol_version` reads as **1**; an unrecognised one is **rejected, never
  defaulted**.

### Status codes

| Situation | Answer |
| --- | --- |
| Recorded, or an idempotent replay | **200** `{"accepted":true,"duplicate":<bool>,"state":"delivered"\|"refused","class":"<enum>"}` |
| **The uniform answer** — no retained row for (correlation key, authenticated principal) | **200** `{"accepted":false,"duplicate":false,"state":"unknown"}` |
| `recipient` is not the authenticated principal | **403** `{"error":"recipient does not match the authenticated caller"}` |
| Malformed frame, unknown class/outcome/version, bad attestation shape | **400** `{"error":"invalid acknowledgement"}` |
| Already terminal with a **different** outcome | **409** — the FIRST terminal stands, nothing is written. **No `Retry-After`**: re-sending can never succeed. |
| Another transition for the SAME pair is being fsynced right now | **503** + `Retry-After: 1`. A transient refusal: the retry lands on the row the in-flight transition wrote and is absorbed as a duplicate. It must never be a 5xx-with-no-remedy or a 500 — invariant 10's first case says a same-key/same-payload retry returns the original result and **does not error**, and an eager client retrying its own acknowledgement is the ordinary way to reach it. (**Amended 2026-08-21:** this row used to say "the ONE transient refusal here". The `ACK-5` row below is a second one. `internal/httpapi/ack.go:501` still calls it the one arm — true within `writeAckError`'s switch, which the transit arm does not go through. **Citation corrected 2026-08-21 (`ACK-5`): this line cited `:459`, which is `writeAckError`'s own doc comment, not the arm.**) |
| **NEW (`ACK-5`, 2026-08-21).** The message was **RELAYED** here, so no row exists on this bus, and the outcome was carried one hop back toward the origin and **accepted by the hop above** | **200** — the SAME body as the recorded case: `{"accepted":true,"duplicate":false,"state":"delivered"\|"refused","class":"<enum>"}`. **Wording corrected 2026-08-21 (`ACK-5`): this row said "and accepted there", i.e. at the ORIGIN, which overclaims.** This bus learns only that its NEXT hop accepted; across two or more backward hops an intermediate may have absorbed a **409** from the origin and answered this bus 200 (see the invariant-4 narrowing below). |
| **NEW (`ACK-5`).** Same, but the backward hop could not be completed (upstream unreachable or refusing, upstream not peered, this bus is already at `maxConcurrentAckTransitsPerUpstream` hops in flight toward that upstream, or the upstream refused **finally with a status other than 409** — 404/403/400) | **503** + `Retry-After: 1` `{"error":"this delivery outcome could not be carried toward the origin bus; retry"}`. **This acknowledgement recorded nothing anywhere**, so the identical retry is safe — but it is not guaranteed to succeed: a **409** from the hop above (swept row, recipient never addressed, conflicting terminal) reaches a local recipient as this same 503, and never clears. **Amended 2026-08-21 (`ACK-5`): this row also listed "this bus no longer retains the relayed message". That is the exit-`8` `unknown` row above in the steady state** — `hub.transitAck` returns false for a pruned message and the transit arm is never entered; the unresolved 503 needs the message to be pruned between that check and `TransitAck`'s own provenance lookup. |
| **NEW (`ACK-5`).** Same, but this build has **no back-propagation seam** (non-federating build) | **501** `{"error":"delivery acknowledgement is not available on this bus"}` — the same wording as the no-lifecycle-table row below, and **no `Retry-After`**: it is a fact about this build, not a transient condition. |
| This build has no delivery lifecycle table | **501** `{"error":"delivery acknowledgement is not available on this bus"}` |
| Any method but `POST` | **405**, `Allow: POST` |

**Why a refusal can be a 200.** `accepted:false, state:"unknown"` is §13.3's UNIFORM ANSWER and is
returned **identically for four different facts**: the key was never accepted here, the key names a
message this agent was not addressed in, the row was swept by retention, or the key is malformed.
They MUST stay indistinguishable — a 403 for "not yours" beside a 404 for "no such key" is a
**message-existence oracle**, letting any authenticated agent enumerate what this bus is carrying and
for whom. So the status line carries the SHAPE of the request and the body carries the OUTCOME; a
client reads `accepted`, not the status. This is the same reasoning `handleBroadcast` applies when it
authenticates before answering 501.

> **ONE CARVE-OUT ADDED 2026-08-21 (`ACK-5`), and it does not widen the four.** The four facts above
> still produce one indistinguishable answer. What changed is that a fifth case no longer falls into
> them: a message **relayed** here that names the authenticated principal as a recipient is not "a
> message this agent was not addressed in" — it is a **transit acknowledgement**, answered `200
> accepted:true` after a synchronous backward hop. A miss and a non-membership remain
> indistinguishable from each other. See [Transit
> acknowledgements](#transit-acknowledgements-a-relayed-message-has-its-row-somewhere-else-ack-5-2026-08-21).

**`state` is never `unknown` on disk.** It is a REPORTING value only; `ack.ParseState` refuses the
spelling by name, so an "I don't know" cannot round-trip through the log and overwrite a real
terminal outcome with ignorance.

### The three cases invariant 10 never collapses, on this route

| Case | Answer |
| --- | --- |
| Same pair, **same** outcome | **200 `duplicate:true`.** The ORIGINAL result stands, nothing is re-applied, **no second WAL record is written**, and **nobody is disconnected**. This is the normal answer to a duplicate DELIVERY: delivery is at-least-once and both copies carry the same correlation key. |
| Same pair, **different** terminal outcome | **409.** Rejected AND logged at ERROR by `internal/ack` with both outcomes named. The first terminal stands. **No disconnect, and no `Retry-After`** — re-sending can never succeed, and dressing a permanent refusal as transient puts a client in an endless retry loop. |
| Replay of an already-accepted **signed message** | Untouched by this route. **An ACK frame is not a message and never reaches that path** — which is the only disconnect on the bus. |

**NO NEW DISCONNECT EXISTS ANYWHERE ON THE ACK PLANE** (§12). Invariant 10's two questions were
answered on the record before the route was written: a merely BUGGY client reaches every refusal here
(a mistyped correlation key, a re-acknowledgement after its own restart, a retry that crossed with a
200 it never saw), and a connection does not carry a single principal's traffic. The 403 above looks
like `/v1/send`'s sender-mismatch, which DOES drop the socket, and is deliberately not the same
thing: there are no signed bytes to replay here, and an agent embedding `client/` under two
enrolments reaches it honestly.

### Authorization is structural, not a comparison

The lifecycle record is keyed on **(correlation key, recipient)** from day one, and the recipient
half is the authenticated principal. So "was I addressed?" and "may I settle this?" are ONE question
answered by ONE map lookup that cannot be talked out of it — an agent can only ever reach the row
that names it. `ack.Store.Settle`'s own doc records why this must live at the route: it copies the
sender forward from the row it found, so its internal sender guard **cannot** fire from this path.

**The route never creates a row.** A refusal writes nothing, so an authenticated agent cannot mint
durable rows for correlation keys it invented, and a swept row is never resurrected.

### What is NOT consulted: the message store — except on ONE arm (`ACK-5` narrowed this, 2026-08-21)

> **AMENDED IN PLACE 2026-08-21.** This heading read *"What is NOT consulted: the message store"* and
> the sentence below it read *"The lifecycle row is the sole authority; `store.Store` is not read at
> all."* **The absolute is no longer true.** On `ack.ErrNoRecord` — and on that arm ONLY —
> `hub.transitAck` (`internal/hub/ack.go:627`) asks `store.Store` one routing question: *does this bus
> hold a **relayed** copy under this correlation key that names the authenticated principal as a
> recipient?* Everything below still governs every case where a row EXISTS, which is why the original
> text is reproduced rather than rewritten. See [Transit
> acknowledgements](#transit-acknowledgements-a-relayed-message-has-its-row-somewhere-else-ack-5-2026-08-21).

Wherever a lifecycle row exists, that row is the sole authority and `store.Store` is not read at all:
no settle, no `duplicate` label and no refusal is conditioned on whether the MESSAGE is still held.
The two retention regimes
differ — a message body is kept for **1 day or 1 GiB, whichever bites first**, while a lifecycle row
is kept for **24h from `accepted_at`** — so a busy bus prunes bodies long before rows expire.
Requiring the body to still be held would refuse an acknowledgement for a message the recipient
demonstrably received and is holding a copy of, and would strand the sender's row non-terminal for
the rest of the window for no reason the recipient could act on.

"Expired message" therefore has two meanings and they answer differently:

| Case | Answer |
| --- | --- |
| The **body** was pruned, the **row** is retained | The acknowledgement is **accepted normally**. |
| The **row** was swept (24h) | The **uniform answer**, and **no row is created**. |

**A MALFORMED OR ABSENT `correlation_key` IS THE UNIFORM ANSWER TOO, and that is a fix rather than
a nicety.** It is the fourth of the four facts above, and it was found by this task's security gate
answering **500** with an unthrottled ERROR log line while an unknown key answered `unknown`. The
cause is worth recording because it is a seam and not a slip: `relay.ValidatePeerAckRequest`
deliberately validates **no ids**, because the PEER route validates them inside `AuthorizePeerAck`,
in the same call that binds them — and this route has no `AuthorizePeerAck`. The agent route
inherited the validator without inheriting that half. The id check now lives at the hub boundary
(`ack.ValidateCorrelationKey`), and `ack.ErrInvalidRecord` is additionally mapped onto the uniform
refusal so the inner check cannot reintroduce a fifth answer. A client that omits the field is
merely buggy, and §12 names that caller honest.

### Transit acknowledgements: a RELAYED message has its row somewhere else (`ACK-5`, 2026-08-21)

**A recipient of a message that was relayed to this bus used to get the uniform `unknown`. It now
succeeds.** That was not a bug in this route — it is a consequence of where the row lives — and it is
the defect `ACK-5` exists to fix, because it left plane C unreachable beyond one hop.

- **A relayed message has no lifecycle row here and never gets one.** `hub.recordAcceptance` returns
  early for relayed ingest (`internal/hub/hub.go:2198`). The row is readable by exactly one party —
  the ORIGINAL SENDER on the ORIGIN bus (`ACK-CONTRACT.md` §13.3) — so a row on any other bus is
  readable by nobody. See `DECISIONS.md`, 2026-08-21, for the full cost argument.
- **The extra authorization is a MEMBERSHIP test and nothing more.** On `ack.ErrNoRecord` only,
  `hub.transitAck` reads `store.RelayProvenanceByOriginMessageID` — a **body-free** accessor
  (`internal/store/provenance.go`) returning recipients, `bus_path` and a `relayed` flag, no body, no
  sender, no signature, no timestamps — and answers true iff the message is **relayed** AND the
  **authenticated principal** appears in the recipient list, compared with `==` exactly as
  `store.Message.VisibleTo` compares it. `recipient` is the context principal, never a request field,
  so an agent can only ever ask about mail addressed to itself.
- **A non-member and a miss are indistinguishable.** Both fall through to the uniform answer, so this
  adds **no message-existence oracle**. A **broadcast** reaches the test with an empty recipient list
  and is therefore refused, which is the right answer: a broadcast has no canonical audience under
  signing format v1, so there is no `(message, recipient)` pair to acknowledge.
- **THE CORRELATION KEY MUST NAME ANOTHER BUS, and this is the check that decides it** (added
  2026-08-21, `internal/hub/ack.go:728-730`). Before anything else, `hub.transitAck` parses the key and
  refuses — folded — any key whose **bus half is THIS bus**. §3 rules that the correlation key is the
  ORIGIN bus's server-minted id, so for a message this bus merely relayed the origin is by
  construction some other bus and a key with our bus half is a **LOCAL id**. The trap is that a local
  id nevertheless RESOLVES: `store.ByOriginMessageID` falls back to treating an unmatched key as a
  local id — a documented and correct property of that method — so the id THIS bus minted and served
  to the recipient resolves to the very same relayed message, `relayed` is true and the principal is a
  member. Without this check the route would authorise it, ask `relay.DisposeAck` where to send it, be
  told `AckStopAtOrigin` because the key's bus half is ours, and answer a **retriable 503 that no
  client could ever clear**. The answer is the uniform `unknown` instead. **It is reached by doing the
  obvious thing:** `agent-busctl watch` prints the LOCAL `message_id` (`toWireMessage` sets
  `MessageID: m.ID`), so the id a recipient reaches for first is precisely the one refused here.
  **CORRECTED 2026-08-22 (`ACK-12-FU-WATCH-CORRELATION-KEY`):** this bullet used to continue *"and no
  route exposes the origin id, so the id a recipient holds is precisely the one refused here. That
  gap is tracked separately (`f423959c`, `ACK-12-FU-WATCH-CORRELATION-KEY`)"*, and **the read path
  now DOES expose the origin id** — the same `toWireMessage` sets `correlation_key` from
  `store.Message.OriginID()`, and `agent-busctl watch` carries it. The check described here is
  unchanged and still load-bearing (a client that keeps reaching for `message_id` still lands on it),
  but the origin id is no longer unobtainable. It also cited
  `internal/httpapi/messages.go:844` for the `MessageID: m.ID` line; the line number moved with this
  change and the citation is now to the function.
- **A locally-originated message is refused outright** even though the store holds it. This bus IS its
  origin, so forwarding would send a terminal outcome to a bus that never owed it one — an expired
  row would become an unsolicited network contact. **The bus-half check above is what actually
  enforces this** — the `!prov.Relayed` test beside it is a stated post-condition, not a second
  independent discriminator, and `internal/hub/ack.go:661-688` says so after a mutation proved the
  package stays green without it.
- **Nothing durable is written on this bus, and the backward hop is synchronous**: this handler does
  not answer 200 until its next hop has answered, and it absorbs nothing — every `TransitAck` error,
  409 included, becomes the 503 above. **CORRECTED 2026-08-21 (`ACK-5`): this bullet read "the 200 is
  not an exception to invariant 4 … the recipient is not told 'accepted' until the origin has
  fsynced", and that is exactly the claim the 409-absorbing arm makes false.** There is exactly one
  exception and it is not this handler's: an INTERMEDIATE bus absorbs a **409** from the hop above it
  and answers ITS downstream 200 (`cmd/agent-bus/relaywiring.go`, `disposeUnrecordedAck`), so on a
  chain of **two or more backward hops** a recipient can be told `accepted` for an outcome the origin
  **refused** — and in the "no obligation binds that recipient" case with nothing durable anywhere.
  The 200 this handler writes therefore cannot promise more than the chain gave it. Recorded in
  `DECISIONS.md`, 2026-08-21 (`ACK-5`); the peer-surface half of this file states the same narrowing.
  See [Back-propagation on the peer
  surface](#back-propagation-on-the-peer-surface-ack-5-2026-08-21) for the traversal and address
  rules — in particular that the destination comes from this bus's own stored `bus_path` and its
  address from this bus's own peer registry, **never from the frame**.

**Two anti-oracle choices, stated so they are not "tidied".**

1. **The success body is the recorded case's body.** Same 200, same fields. Which bus holds the
   durable row is a fact about the federation's topology, and §13.3's posture is that a recipient
   learns the outcome of the message it was handed and nothing else about the federation.
   **One honest exception:** `duplicate` is **always `false`** on the transit path, because this bus
   keeps no record for a relayed message and there is nothing here for a retry to be a duplicate *of*
   — the duplicate is absorbed **where the record is**, at the origin, under §8.2 note 2. So a
   recipient that re-acknowledges CAN infer the transit path from `duplicate:false` where the local
   path would have said `true`. That is a topology hint to a party that was handed the message, not a
   message-existence oracle, and labelling it otherwise would mean this bus asserting something about
   a table it does not hold.
2. **A failed hop is 503, never 4xx.** A 4xx is FINAL under `relay.PeerRefusedError.Retriable`, so a
   4xx here would make the recipient **abandon** an acknowledgement that nothing recorded — the
   outcome would be lost outright rather than delayed. The upstream's own verdict is **not echoed**:
   the recipient is told "not now" and learns nothing about which bus refused or whether a row exists
   anywhere else in the federation.

**The limitation, accepted as a cost rather than argued away.** A transit acknowledgement **stops
working once the relayed MESSAGE is pruned** — 1 day or 1 GiB, whichever bites first, so on a busy bus
the byte cap can bite well before `ack.Retention`'s 24h would have expired a row. The recipient is
then told the uniform `unknown`, exactly as for a swept row. This bus genuinely has nothing else to
bind the frame to once the message is gone: the destination is derived from the stored `bus_path`, and
§9.4 forbids taking it from anywhere else. The alternative — writing unreadable rows on every bus — is
the cost `DECISIONS.md` refuses.

### The agent-facing half is `ACK-9`'s

Invariant 7 requires a CLI subcommand and an `AGENT_PROTOCOL.md` entry for every capability.
**INVARIANT 7 IS MET FOR THIS ROUTE as of `ACK-15` (2026-08-21).** The recipient CLI is
`agent-busctl ack <message-id> [--refuse <class>] [--json]`. Exit codes reuse the existing stable
set — `unknown` maps to `ExitEmpty` (8), an already-terminal conflict to `ExitRejected` (7) — and
mint no new code.

> **CORRECTED 2026-08-21, and the correction is stated rather than swapped.** Everything below this
> line used to read: *"Until `ACK-9` lands, this route has no subcommand"*, followed by an
> *"Update, 2026-08-16: only HALF of `ACK-9` has landed … **this route (`POST /v1/ack`) still has no
> subcommand***". **Both are now false**, and the attribution was wrong too: the recipient CLI landed
> as **`ACK-15`**, not `ACK-9`. `ACK-CONTRACT.md` §13.4 did assign it to `ACK-9`; that assignment was
> re-scoped when `ACK-9` shipped the sender half alone.
>
> This mattered more than a stale line usually does: **a CONTRACTS file telling an agent a route has
> no subcommand is telling it to hand-write HTTP**, which invariant 7 exists to prevent. The
> "Update, 2026-08-16" heading made it read as freshly checked.

**What is still true, and must not be over-corrected away:** the recipient CLI signs with the agent's
**messaging** key, not its auth key, and **every bus checks the signature's SHAPE only** — no bus
verifies it, and no route carries a recipient's messaging key back to a sender (`ACK-CONTRACT.md`
§16 Q1). So `attested_by` reports `recipient_signature_unverified` and an ACK remains **evidence, not
proof**. `agent-busctl ack` deliberately offers no way to spell `undeliverable`: that is a bus's
routing claim, and `signing.CanonicalizeAck` refuses it.

## The SENDER-visible ack-status route: `GET /v1/ack/<correlation-key>` — delivery status (`ACK-9`, added 2026-08-16)

The **sender-facing** half of the delivery acknowledgement plane (`internal/httpapi/ackstatus.go`),
answering "what happened to the message I sent". `httpapi.RouteAckStatus = "/v1/ack/"` is registered
as a **SUBTREE** — note the trailing slash — because this build targets go1.19, whose `http.ServeMux`
has no path wildcards; the correlation key is whatever remains of the path after the prefix, taken
with `strings.TrimPrefix` and treated as **UNTRUSTED INPUT** (a client-supplied string shaped like a
server-minted id, never an identity — invariant 1). It is a route **distinct** from `POST /v1/ack`
(`ACK-6`, above): `http.ServeMux` resolves the bare path and the subtree independently, so the two
coexist without colliding.

**Registered only when the server is built with `Options.AckStatus` (a `*ack.Store`).** A build with
no lifecycle table does not mount this route at all, and the path falls through to the catch-all
404 — there is no dedicated 501 here the way there is for `/v1/broadcast`. It is mounted **outside**
the `Options.Hub != nil` block: it reads a durable table, not the messaging surface, so a bus whose
hub is unavailable can still answer for messages it already accepted.

**Authenticated exactly like every other route.** `/v1/ack/` is not on `unauthenticatedRoutes`
(`internal/httpapi/authmw.go`), so `authMiddleware`'s default-deny applies: an anonymous caller gets
401, never a peek at delivery status.

### Query parameter

`?wait=<whole seconds>`, optional. Absent or empty means "answer with a snapshot now". A value that
is not a positive whole number of seconds, or that exceeds `hub.MaxPollTimeout` (300s / 5 minutes —
the same ceiling `GET /v1/wait` enforces), is **REFUSED with 400**, never silently clamped:
`{"error":"wait must be a positive whole number of seconds"}` or `{"error":"wait must be at most 300
seconds"}`.

With `?wait=`, the request **parks** (re-reading the table roughly every 200ms —
`ackStatusPollInterval`, a deliberate poll rather than a wake-on-write registry, invariant 8) until
every visible row is terminal, the deadline passes, or the client hangs up. **It parks even when
there is nothing to report for the key** — see the oracle rule below; returning early on an unknown
key would leak the key's existence through response latency alone. A deadline reached with nothing
settled is a 200 with the current (possibly `unknown`) rows, exactly as a timed-out `GET /v1/wait`
is a 200 — never an error.

### Response

**200 whatever the key is** — there is no 400/403/404 branch on the key itself (see the oracle rule
below). The route does answer two non-200 statuses, and **neither is about the key**: a **400** when
`wait` is not a positive whole number of seconds at most `hub.MaxPollTimeout`, and a **429** when
this principal is already at its parked-request cap. Both judge the caller's own request, so neither
tells anybody whether a message exists. Body of the 200:

```json
{"rows": [
  {"correlation_key": "<bus-id>-<seq>", "recipient": "<bus-id>.<agent-id>",
   "state": "accepted"|"in_flight"|"delivered"|"refused"|"undeliverable"|"unknown",
   "class": "<one of the closed 12-value enum>",
   "attested_by": "peer_bus"|"recipient_signature_unverified",
   "accepted_at": "<RFC3339Nano UTC>", "settled_at": "<RFC3339Nano UTC>"}
]}
```

`rows` is **never empty and never null** — every field but `state` is `omitempty`; `state` is always
present. `class` is present **only** on a negative terminal (`refused`/`undeliverable`); a positive
terminal or a non-terminal row carries none. When nothing is visible for the caller, the body is
**exactly** `{"rows":[{"state":"unknown"}]}` — the single smallest shape this response can take.

### The status-oracle rule (`ACK-CONTRACT.md` §13.3) — read this before building anything on this route

> **Only the ORIGINAL SENDER sees a row.** A key that never existed, a key swept past the 24h
> retention window (see "What is NOT consulted" above), a key belonging to a **different** sender,
> and a **malformed** key all return the identical `200 {"rows":[{"state":"unknown"}]}`.

There is **no 403 and no 404** for any of those four cases — a distinguishable answer would confirm
that a message exists, which is the existence oracle `ACK-4` is required to close. The filter is
applied against the **authenticated principal from the request context** (never a header, query
parameter or path value), through `AckStatusSource.StatusRows(correlationKey, sender string)` — an
interface that takes no recipient and returns no error, so a handler that could iterate candidate
recipients, or tell "wrong sender" apart from "unknown key" by an error value, cannot be written by
accident.

**This is CONTENT indistinguishability, not TOTAL indistinguishability.** A coarse timing residual
exists — declared by `ACK-4`, not introduced here — because the four cases do not all cost the same
amount of work to answer. The claim this route makes is about the response BODY; do not read it as a
claim that every observable (timing included) is identical.

### Two facts this route is misread on

- **A HOP ACK IS NOT A DELIVERY.** `delivered` means the recipient **application** acknowledged the
  message (`POST /v1/ack`, `ACK-6`, above). Another bus taking responsibility for the next hop
  (`POST /v1/peer/ack`, `ACK-3`, above) does **not** advance this state and is **not reported** here —
  the transport layer's "another bus has it" and the application layer's "an agent got it" are
  different facts, and this route reports only the second.
- **`attested_by` is a LABEL, not a proof.** The two values are `peer_bus` and
  `recipient_signature_unverified`. There is **no value meaning "verified"**, and nothing in this
  system can produce one: no route distributes agents' messaging public keys, so an ACK's attestation
  is checked for shape only, by nobody, ever (see the `POST /v1/ack` and `POST /v1/peer/ack` sections
  above).

`class` is a **closed 12-value enum**, never free text: bus-emitted `no_route`, `no_such_recipient`,
`hop_refused`, `hop_unauthenticated`, `loop_dropped`, `fanout_exceeded`, `horizon_expired`,
`local_capacity`, `obligation_lost`; recipient-emitted `recipient_refused_policy`,
`recipient_refused_undecodable`, `recipient_refused_not_addressed`.

### CLI (invariant 7)

`agent-busctl ack-status <correlation-key> [--wait <dur>] [--json]` — see `AGENT_PROTOCOL.md`,
"Checking delivery status", and `CONTRACTS-CLI.md`'s subcommand table and flags list. Exit codes
reuse the existing stable set, `ACK-CONTRACT.md` §13.4 — **no new code is minted**: `0` for any
reported state without `--wait` (including `unknown`), `0` for `--wait` that settles `delivered` or
that ends still `accepted`/`in_flight`, `7` (`ExitRejected`) for `--wait` that settles
`refused`/`undeliverable`, `8` (`ExitEmpty`) for `--wait` that ends `unknown`. As with the recipient
route above, this is the ONLY status branch this route has after authentication — the row data, not
the HTTP status, carries the outcome.
