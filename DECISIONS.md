# Decisions

Design decisions and their rationale. **Append-only, newest entry LAST** (chronological order —
new decisions are appended at the bottom, matching the append-only-log spirit of the rest of this
project). Never edit or delete a past entry; if a decision is superseded, add a new dated entry that
says so and links back.

Each entry: a dated, stable heading; **Context** (what forced the decision); **Decision** (what was
chosen); **Consequences** (what that commits us to, including things to revisit later).

---

## 2026-08-02 — Package is `internal/httpapi`, not `internal/http`

**Context.** The HTTP server package needs a name. `internal/http` is the obvious one.

**Decision.** Named it `internal/httpapi`.

**Consequences.** `internal/http` would shadow stdlib `net/http` in every file that imports both —
a needless papercut repeated across the whole package. `httpapi` avoids it. No functional effect;
purely a naming call, recorded so nobody "fixes" it back to `http` later without knowing why.

---

## 2026-08-02 — No `log/slog`; a small internal structured logger over stdlib `log`

**Context.** The server needs structured, leveled logging. `log/slog` is stdlib's answer, but this
project's pinned toolchain is go1.19.4. Verified on this box:

```
$ go version
go version go1.19.4 linux/amd64
$ ls $(go env GOROOT)/src/log/
log.go  syslog/
$ go list std | grep log/slog
(empty)
```

`log/slog` landed in go1.21; it does not exist in this toolchain.

**Decision.** Wrote `internal/logging`, a small logfmt-style structured logger built on stdlib
`log.Logger`, rather than either backporting `slog` or pulling in a third-party logging library.

**Consequences.** Invariant 8 ("simple beats clever" — Go stdlib first, a third-party dependency
needs a `DECISIONS.md` justification) rules out reaching for e.g. `zerolog` or `zap` for what is a
few hundred lines of formatting. Revisit this decision if the toolchain is ever bumped to go1.21+:
at that point `log/slog` becomes a real option and `internal/logging` should be evaluated for
replacement rather than kept out of inertia.

---

## 2026-08-02 — Log format is logfmt `key=value`, not JSON

**Context.** Structured log output needs a wire format. JSON is the common default for log
shippers; logfmt is more human-readable for someone tailing a dev server.

**Decision.** `internal/logging` emits one logfmt line per event
(`ts=... level=info msg="request" method=GET path=/healthz status=200`), not JSON.

**Consequences.**
- Values are `strconv.Quote`d whenever they contain whitespace, `"`, `=`, `\`, or any byte `>= 0x7f`
  — this is the log-injection defence: an attacker-controlled header value can never contain a raw
  newline (or a raw U+2028/U+2029 that some post-processors treat as a line terminator) and forge a
  second log record.
- Values truncate at 1024 bytes (`maxValueLen`) — a log record is not a transport for arbitrary
  payloads.
- Keys are sanitised to `[A-Za-z0-9._-]` — keys come from code, not the wire, so anything else in a
  key is a bug worth surfacing, not hiding.
- Still mechanically parseable by a shell wrapper or a log shipper, just not JSON-native; revisit if
  a downstream consumer needs JSON specifically.

---

## 2026-08-02 — go1.19 pin

**Context.** `go.mod` needs a `go` directive. The box currently has go1.19.4 installed.

**Decision.** Pinned `go 1.19` in `go.mod`. No go1.20+ or go1.21+ language or stdlib features
without an explicit, stated bump (per `CLAUDE.md`'s Go conventions section).

**Consequences.** Notably unavailable at go1.19: `crypto/ecdh` (go1.20), `errors.Join` (go1.20),
the `slices`/`maps` stdlib packages (go1.21), and `log/slog` (go1.21, see above). Any task that
wants one of these must bump the toolchain pin first and record that bump here — it must never be a
side effect of an unrelated change.

---

## 2026-08-02 — `-bus-id` is a TEST-ONLY affordance

**Context.** Invariant 1 makes the server authoritative on every id: a client- or operator-supplied
id is input to validate, never an identity to trust. Tests still need a deterministic bus id to
assert against, and the real id-minting machinery (`internal/ids`) doesn't exist yet.

**Decision.** Added a `-bus-id` flag that overrides the placeholder bus id, explicitly documented as
test-only in its usage string and in code comments. It is validated against
`^[A-Za-z0-9_-]{1,64}$`; `.` is rejected because `.` is the `<bus-id>.<agent-id>` qualification
separator (invariant 2) and would make that qualification ambiguous. Using the flag at all logs a
runtime `WARN`, not just a doc comment, so a production misuse is visible in the log stream.

**Consequences.** When the ID epic lands, the real bus id comes from `internal/ids`, and `-bus-id`
either goes away or is explicitly re-scoped as a documented production override with its own
decision entry — it must not silently keep meaning "test-only" while being used in production.

---

## 2026-08-02 — Inbound `X-Request-Id` is untrusted

**Context.** `X-Request-Id` is a caller-supplied correlation id, echoed on the response and written
into every log line for the request. An attacker-controlled value reaching the log stream unfiltered
is a log-injection vector.

**Decision.** An inbound `X-Request-Id` is accepted only if it matches `[A-Za-z0-9._-]{1,64}`
(`MaxRequestIDLen = 64`). Anything else is **rejected outright and replaced**, not escaped-and-kept:
the server mints a fresh id via `crypto/rand` (16 hex chars), falling back to a monotonic
`seq-<n>` counter if the entropy source errors, so ids stay unique even then.

**Consequences.** A caller cannot smuggle arbitrary bytes into the log stream via this header, at
the cost of the caller's own correlation id being silently dropped if it doesn't fit the pattern —
an acceptable trade since the id is advisory, not authoritative.

---

## 2026-08-02 — Shutdown cancels the root context BEFORE `http.Server.Shutdown`

**Context.** `http.Server.Shutdown` waits for all active handlers to return before it returns. A
future long-poll handler that parks on a channel with no way to be woken would block `Shutdown`
indefinitely — there is nothing else in `net/http` that would release it.

**Decision.** The server-lifetime context (`rootCtx`) is wired into `http.Server.BaseContext`, so
every request context descends from it. On a shutdown signal, `main.waitAndShutdown` cancels
`rootCtx` **first**, then calls `srv.Shutdown`. A regression test
(`TestShutdownReleasesLongPoll` in `cmd/agent-bus`) pins this ordering.

**Consequences.** A future POLL handler only has to `select` on `r.Context().Done()` alongside its
own timeout to be released cleanly on shutdown — the plumbing already exists.

**Forward-looking trap for the DUR/MSG epics, stated plainly so it isn't rediscovered the hard
way:** the commit step of the two-phase (prepare → commit) durable write path must **not** use
`r.Context()`. `r.Context()` is cancelled the instant shutdown begins draining, and invariant 4
("nothing is acknowledged before it is durable") means a commit in flight when that happens must
still complete and fsync — it must not be abandoned mid-write because the request context died.
Durable writes need their own context, independent of the request's.

---

## 2026-08-02 — `/healthz` and `/v1/info` are deliberately unauthenticated

**Context.** Every other route authenticates (invariant 3). These two don't: `/healthz` is a
liveness probe, `/v1/info` is pre-enrolment discovery — a caller needs bus info before it has a
credential to authenticate with.

**Decision.** Both routes are served with no auth check. `/v1/info`'s response body is deliberately
minimal: `{bus_id, version, uptime_seconds}` only.

**Consequences.** No data dir, listen address, peer list, or agent roster belongs in `/v1/info` —
any of those would hand an unauthenticated caller information it has no business having. A test
asserts the exact field set on `InfoResponse`, so adding a field there fails the test rather than
silently expanding what an unauthenticated caller learns; a deliberate expansion must update the
test and be justified here.

---

## 2026-08-02 — NEVER write our own crypto; always use a standard library that wraps the problem

**Context.** The project was heading toward Signal-style end-to-end encryption. `CRYPTO_DEEPDIVE.md`
established that Signal's own libsignal has no Go binding, that the best available Go option
(`go.mau.fi/libsignal`) needs a toolchain bump, and — the finding that mattered — that the
go1.19-compatible alternative, `status-im/doubleratchet`, **accepts replayed ciphertexts by default**
(reproduced in four configurations). A 2019 beta library with no mutexes was the only thing that fit
the constraints, and it was quietly broken.

**Decision.** Two standing rules, promoted to **invariant 9** in `CLAUDE.md`:

1. **Never write our own crypto.** No implementing or "adapting" a cipher, hash, MAC, KDF, signature
   scheme, key exchange, or ratchet; no hand-rolled padding, nonce, or IV schemes; no bespoke
   constructions assembled from otherwise-good primitives.
2. **Always use a well-known, standard, audited library, and prefer the one that wraps as much of
   the problem as possible** — a high-level, misuse-resistant sign/verify or sealed-box API over
   assembling primitives ourselves.

This **overrides invariant 8** (stdlib-first, justify dependencies). Where the two conflict, take the
audited crypto dependency.

**Consequences.** The reason this outranks our other preferences is that broken crypto fails
*silently*: it still encrypts, it still verifies, and it provides none of the protection it appears
to. No ordinary test suite detects it, so "our tests pass" is never evidence that a crypto change is
correct — which is why known-answer tests against published vectors (RFC 8032 for Ed25519) and
explicit negative tests (tampered body, swapped sender, replayed message, wrong key, truncated
signature) are mandatory rather than nice-to-have. A verifier that accepts everything passes every
positive test ever written.

When no suitable library exists, the answer is to change the requirement or stop and ask the user —
never to write it ourselves.

---

## 2026-08-02 — Message auth/integrity only (libsodium-style signatures); encryption deferred

**Context.** Full Signal semantics (X3DH + Double Ratchet + PFS) carried a large complexity and
supply-chain cost for a bus whose immediate need is "can I trust this message came from that agent,
unmodified". Complexity is itself a security risk here.

**Decision.** User instruction, verbatim: *"ok, let's keep it simple and just use standard message
auth/integrity using libsodium. encryption can come later"*. So:

- **No encryption, no X3DH, no Double Ratchet, no forward secrecy** for now. The ratchet direction in
  `CRYPTO_DEEPDIVE.md` is **superseded** — its recommendation must not be actioned.
- Messages carry an **Ed25519 detached signature** (libsodium's `crypto_sign`) over a canonical
  serialisation of the message, made with the sender's **messaging** private key.
- The **auth** keypair and the **messaging** keypair stay separate. This matters more without
  encryption, not less: the bus verifies auth keys, but a message signature is verified by the
  **recipient**, so a compromised or malicious bus still cannot forge a message from agent A.
- Verification happens on the receive path **and** in the `scripts/bus-*.sh` wrapper, so an agent
  never acts on an unverified message.

**Consequences.**

- **Every message body is readable by the bus and by any relay peer it traverses.** That is now an
  accepted property of the system, not an oversight. It must be stated plainly in `AGENT_PROTOCOL.md`
  and `PROTOCOL.md` so nobody assumes confidentiality the bus does not provide.
- **A signature alone does not stop replay.** A valid signed message can be resent verbatim; freshness
  comes from the server-minted monotonic sequence plus recipient-side cursor, and needs its own test.
- **Canonical serialisation is the sharp edge.** If signer and verifier serialise differently,
  verification fails intermittently, or worse, a field outside the signed bytes becomes silently
  forgeable. The exact signed bytes and their order are specified as their own task.
- The audit log stays metadata-and-routing-only (invariant 6); its content hash now pairs with the
  signature to make a message non-repudiable.
- **No toolchain bump is needed for this.** `crypto/ed25519` — the same Ed25519 primitive libsodium's
  `crypto_sign` implements, behind an equally high-level API — has been in the Go stdlib since 1.13
  and works on go1.19.4. Whether to use stdlib `crypto/ed25519` or a cgo libsodium binding is left to
  the implementing task; both satisfy invariant 9, and the cgo binding's cost is a C library in the
  container image.
- Encryption is deferred, not cancelled. Revisit it as a fresh decision rather than reviving the
  superseded ratchet plan.
