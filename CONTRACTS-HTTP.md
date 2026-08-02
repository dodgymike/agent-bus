# Contracts: HTTP routes, headers, enrolment and authentication

Split out of `CONTRACTS.md` (2026-08-02) — see that file for the index of all contract planes and
the rest of the surface (CLI/env, on-disk, agent-facing). This is a pure content move: everything
below this header is unchanged from the prior single-file `CONTRACTS.md`, verbatim.

## Routes

| Method | Path | Auth | Status | Response |
| --- | --- | --- | --- | --- |
| `GET` | `/healthz` | none | 200 | `{"status":"ok"}` |
| `GET` | `/v1/info` | none | 200 | `{"bus_id":"...","version":"...","uptime_seconds":0.0}` |
| other | `/healthz`, `/v1/info` | none | 405 | `{"error":"method not allowed"}`, `Allow: GET` |
| `POST` | `/v1/enroll` | none (unauthenticated by necessity — this is how the credential is obtained; only registered when `Options.Auth != nil`, see AUTH-1 section below) | 201 | `{"agent_id":"...","bus_id":"...","name":"...","enrolled_at":"<RFC3339Nano UTC>"}` — the SAME body, byte for byte, on an idempotent replay (see `Idempotency-Replayed` header) |
| `POST` | `/v1/enroll` | none | 400 | invalid `name`; invalid `public_key` (not base64, or not exactly the 32-byte Ed25519 public key size); invalid `idempotency_key` (empty, over 128 bytes, or a byte outside `[A-Za-z0-9._-]`) |
| `POST` | `/v1/enroll` | none | 409 | `idempotency_key` reused with a **different** `name`/`public_key` than its first use — a protocol violation, not a retry (invariant 10); response carries `Connection: close` (see `## Headers`) |
| `POST` | `/v1/enroll` | none | 503 | the roster (default 4096 entries) or the idempotency table (default 16384 entries) is at capacity; `Retry-After: 5` |
| `POST` | `/v1/session/begin` | none (issues the challenge; only registered when `Options.Auth != nil`) | 200 | `{"agent_id":"...","token":"...","challenge_expires_at":"<RFC3339Nano UTC>"}` |
| `POST` | `/v1/session/begin` | none | 404 | `agent_id` is malformed **or** well-formed but not on this bus's roster — the two cases are deliberately indistinguishable to the caller |
| `POST` | `/v1/session/begin` | none | 503 | the session table (default 16384 entries, pending + active together) is at capacity; `Retry-After: 5` |
| `POST` | `/v1/session/complete` | none (activates the credential; only registered when `Options.Auth != nil`) | 200 | `{"agent_id":"...","expires_at":"<RFC3339Nano UTC>","lifetime_seconds":3600,"refresh_after_seconds":2700}` |
| `POST` | `/v1/session/complete` | none | 400 | `signature` is not valid base64; also returned if the roster holds a corrupt (wrong-length) public key for the agent (defence in depth — see `internal/auth/session.go`) |
| `POST` | `/v1/session/complete` | none | 401 | the signature does not verify against the agent's enrolled public key, or is not exactly the 64-byte Ed25519 signature size |
| `POST` | `/v1/session/complete` | none | 404 | `token` names no session (never existed, already expired, or was dropped after a prior failed verification), or a pending/active session has passed its deadline — again deliberately indistinguishable to the caller |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 400 | malformed JSON, an unrecognised field, or trailing content after the one JSON value the body must contain |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 405 | any method but `POST`; `Allow: POST` |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 413 | request body exceeds `httpapi.MaxAuthRequestBytes` (8 KiB) |
| `POST` | `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete` | none | 415 | `Content-Type` is not `application/json` (a `charset` parameter is accepted) |
| any | any path off the five-entry allow-list (`/healthz`, `/v1/info`, `/v1/enroll`, `/v1/session/begin`, `/v1/session/complete`) | `Authorization: Bearer <token>` required — see `## Authentication` below | 401 | `{"error":"authentication required"}` when no usable credential was presented at all (missing or duplicate `Authorization` header, a scheme other than `Bearer`, an empty/spaced/oversized/non-base64url token — `WWW-Authenticate: Bearer realm="agent-bus", error="invalid_request"`), or `{"error":"invalid or expired credential"}` when a well-formed token failed to authenticate (unknown, pending, or expired — deliberately indistinguishable, see `## Authentication` — `WWW-Authenticate: Bearer realm="agent-bus", error="invalid_token"`) |
| any | unregistered path, no credential (or one that does not authenticate) | — | 401 | `authMiddleware` wraps the whole mux and refuses before the mux is ever consulted, so an anonymous caller cannot enumerate which paths this bus serves by probing unknown ones; same body/header shape as the row above |
| any | unregistered path, valid bearer token | valid bearer token | 404 | `net/http.ServeMux`'s built-in `text/plain` "404 page not found" — **not** the JSON error envelope — because the middleware let the request through and the mux, honestly, has no route there. Known follow-up: CORE-8 (register a catch-all so unmatched paths get the same JSON envelope); that catch-all MUST be registered INSIDE the auth wrapper (through `(*Server).route`, so it is itself subject to `authMiddleware`) or it becomes the one unauthenticated route that leaks the surface. This 404 is also what `/v1/enroll`, `/v1/session/begin`, and `/v1/session/complete` return when the server was built with `Options.Auth == nil` — those three stay on the allow-list unconditionally (see the AUTH-1 section below), so they reach the mux with or without a credential and 404 there like any other unregistered path. |

`HealthResponse` / `InfoResponse` / `ErrorResponse` types live in `internal/httpapi/server.go`. `EnrolRequestBody` / `EnrolResponseBody` / `SessionBeginRequestBody` / `SessionBeginResponseBody` / `SessionCompleteRequestBody` / `SessionCompleteResponseBody` live in `internal/httpapi/auth.go`.

`/v1/info`'s payload is deliberately minimal (see `DECISIONS.md`, 2026-08-02): `bus_id`, `version`,
`uptime_seconds` only. A test pins the exact field set — do not add data-dir, listen address, peer
list, or agent roster here without updating that test and recording the decision.

**Authentication is now default-deny across the whole mux** (AUTH-2, with AUTH-6's fail-open fix
folded into the same change). `authMiddleware` wraps `s.handler` before any route is dispatched, so a
route is authenticated the moment it is registered through `(*Server).route` — nobody has to remember
to protect it individually, which closes the exact risk AUTH-6 flagged (routes wired one at a time,
easy to forget on the next addition). The allow-list is exactly the five paths named in the routes
above; see `## Authentication` further down for the full contract.

## Headers

| Header | Direction | Rule |
| --- | --- | --- |
| `X-Request-Id` | in/out | Inbound value accepted only if it matches `[A-Za-z0-9._-]{1,64}` (`httpapi.MaxRequestIDLen = 64`); otherwise replaced with a server-generated id (`crypto/rand` 16 hex chars, falling back to a `seq-<n>` counter). Always echoed on the response. |
| `Authorization` | in | Required on every route off the five-entry allow-list (`## Authentication`). Exactly one header, form `Bearer <token>` (scheme case-insensitive); `<token>` must be non-empty, contain no space, be no longer than `httpapi.MaxBearerTokenLen` (512), and consist only of the base64url alphabet `[A-Za-z0-9_-]`. Zero headers, more than one, a non-`Bearer` scheme, or a token failing any of those checks is treated as "no usable credential" (401, `error="invalid_request"`) — distinct from a syntactically fine token that simply does not authenticate (401, `error="invalid_token"`). Never logged, echoed, truncated or hashed into any response or log line — only the resulting agent id ever leaves `authMiddleware`. |
| `WWW-Authenticate` | out | On every 401: `Bearer realm="agent-bus", error="invalid_request"` when no usable credential was presented, or `Bearer realm="agent-bus", error="invalid_token"` when a well-formed token failed to authenticate (unknown, pending, or expired — the three are deliberately indistinguishable to the caller). |
| `Allow` | out | Set to `GET` on a 405 from `/healthz` or `/v1/info`. |
| `Content-Type` | out | `application/json; charset=utf-8` on every JSON response. |
| `X-Content-Type-Options` | out | `nosniff` on every JSON response. |
| `Idempotency-Replayed` | out | `true` on `POST /v1/enroll`'s 201 when the response was replayed from the idempotency table rather than freshly applied. The BODY is byte-identical to the original either way — the header is the only out-of-band signal that this call re-applied nothing. |
| `Connection` | out | `close` on `POST /v1/enroll`'s 409 (idempotency key reused with a different payload). Invariant 10: same key + different payload is a protocol violation, and the server disconnects the offending client. Contrast the same-key/same-payload case, which is a legitimate retry, returns the original 201 unchanged, and is never disconnected or otherwise punished. |
| `Retry-After` | out | `5` (seconds) on a 503 from any of the three auth routes (a roster, idempotency-table, or session-table capacity limit). Short deliberately: every cap in `internal/auth` is a live in-memory bound that a departing agent or an expiring session can relieve within seconds. |
| `Allow` | out | Also set to `POST` on a 405 from `/v1/enroll`, `/v1/session/begin`, or `/v1/session/complete`. |
| `Cache-Control` | out | `no-store` on `POST /v1/session/begin` only. That response body carries a LIVE credential (the session token); the other two auth responses carry none, so the header is deliberately not set on them and its presence stays meaningful. |

## Enrolment and sessions (added 2026-08-02)

AUTH-1 adds the three credential-issuing routes documented in `## Routes` and `## Headers` above:
`POST /v1/enroll`, `POST /v1/session/begin`, `POST /v1/session/complete`. This section is the prose
that does not fit a table row. No `scripts/bus-*.sh` wrapper and no `AGENT_PROTOCOL.md` entry are
added by this task — invariant 7 was amended so a Go CLI replaces the shell wrappers, and wiring
these routes to that agent-facing surface is a separate, later task. Do not infer a wrapper or CLI
subcommand exists for enrolment or sessions from this document.

**`Options.Auth` gates route registration, not route behaviour.** `internal/httpapi.Options.Auth`
(`*auth.Service`) has no default and is `nil` unless the caller supplies one. When it is `nil`, `New`
does not register `/v1/enroll`, `/v1/session/begin`, or `/v1/session/complete` on the mux at all —
they 404 through the same `net/http.ServeMux` catch-all as any other unknown path, not a 503. That is
deliberate: a route that exists and refuses is a claim that the capability is present, and a server
built without an auth service does not have it. `cmd/agent-bus`'s `run()` always constructs one
(`auth.NewService(auth.Options{Minter: minter})`), so the shipped binary always registers these three
routes; a `nil` `Options.Auth` is reachable only by a caller of the `httpapi` package directly (tests,
or a future build that intentionally omits the auth surface).

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
key into a signing oracle for arbitrary bytes. `public_key` (on `/v1/enroll`) and `signature` (on
`/v1/session/complete`) are both `base64.StdEncoding` — the **standard, padded** alphabet, decoded
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
**There is no TLS in this server**, so that observer is a real threat model on any non-loopback
listener, and the token's unguessability is load-bearing now that no other per-agent bound exists.

**Nothing here is durable — do not claim otherwise.** The roster (`auth.MemoryRoster`), the
idempotency-key table, the session table, and the per-name agent-id suffix counters
(`ids.NewNameSuffixes()`, wired fresh in `cmd/agent-bus`) are **all in-memory only**. Enrolment is
**NOT crash-safe**: every enrolled agent, every remembered idempotency key, and every session is lost
on process restart, and suffixes restart at 1 for every name until AUTH-3 lands durable enrolment and
recovery through the WAL. This is a known, deliberately-scoped gap, not an oversight — see the doc
comments on `auth.Service.Enrol` and `auth.Roster`. Sessions specifically are **not** a durability gap
to close later: not surviving a restart is a settled design decision (a lost session costs one
challenge/response round trip), independent of AUTH-3. `cmd/agent-bus`'s `run()` logs this at `WARN`
on every start:

```
msg="enrolment and sessions are IN-MEMORY ONLY: they are NOT crash-safe, the roster and all sessions are LOST on restart, and agent id suffixes restart from 1 for every name. Do not treat an accepted enrolment as durable until AUTH-3 lands durable enrolment and recovery" bus_id=<id> follow_up=AUTH-3
```

No on-disk record type, WAL frame, or wire protocol version was introduced by this change — the
`## Record types / wire protocol versions` section above remains the complete index.

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
- **There is no revocation** (AUTH-4). A session is valid until it expires, at most one hour.

## Authentication (added 2026-08-02)

AUTH-2 wires `internal/httpapi/authmw.go`'s `authMiddleware` around the WHOLE mux —
`s.handler = LoggingMiddleware(s.log, s.authMiddleware(mux))` — folding in **AUTH-6**'s fail-open fix
into the same change rather than as a later retrofit. The middleware is DEFAULT-DENY: every request is
refused 401 unless its **exact** `r.URL.Path` is on the allow-list, so a route added tomorrow is
authenticated the instant it is registered through `(*Server).route` — nobody has to remember to wrap
it, and forgetting is no longer possible for the surface `TestEveryRouteRequiresAuth` can see (below).

**The allow-list is exactly five paths, matched by exact string equality** (no prefix match, no path
cleaning, no trailing-slash tolerance — `/healthz/`, `//healthz`, `/HEALTHZ` are NOT allow-listed and
get 401; the cost of being this strict is a 401 on a misspelled-but-harmless probe, the cost of being
lenient is a normalisation mismatch between this check and the mux, which is how allow-list bypasses
get built):

- `/healthz` — liveness; a load balancer or orchestrator probe calls it before any agent exists, and it
  returns no state.
- `/v1/info` — pre-enrolment discovery; an agent needs the bus id and version to decide whether to
  enrol at all.
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
| `PrincipalFromContext(ctx) (auth.Principal, bool)` | The authenticated identity, or `ok == false` on an allow-listed route (not an error condition — it is the definition of an unauthenticated route). |
| `AgentIDFromContext(ctx) string` | The fully-qualified `<bus-id>.<agent-id>` (invariant 2) of the caller, or `""` when no principal is attached. |
| `(*Server).Routes() []string` | Every pattern registered through `(*Server).route`, sorted. This is the real surface `TestEveryRouteRequiresAuth` walks, because Go 1.19's `http.ServeMux` cannot otherwise be enumerated. |

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
