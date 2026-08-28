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

> CORRECTED 2026-08-16: neither happened, and the decision below still stands exactly as written. `-bus-id` is
still registered on the server binary with the usage string `"TEST-ONLY: force the bus id"`
(`cmd/agent-bus/main.go:295`), alongside `-listen`, `-data-dir`, `-poll-timeout`, `-log-level` and
`-backfill-suffix-floors`. The real bus id does now come from `internal/ids`
(`ids.LoadOrCreateBusID`, `cmd/agent-bus/main.go:424`), so the flag is an override of a real value
rather than of a placeholder.

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

---

## 2026-08-02 — Ed25519 is Go stdlib `crypto/ed25519`; NOT a cgo libsodium binding (RATCHET-7)

**Context.** The entry above deliberately left one question open: *"Whether to use stdlib
`crypto/ed25519` or a cgo libsodium binding is left to the implementing task; both satisfy
invariant 9."* That open question gates AUTH-1/2/3, SIGN-1..8, CRYPTO-3, CRYPTO-4 and CRYPTO-10 —
roughly a dozen tasks, several of which already name `crypto/ed25519` as a presumption they have no
authority to make. RATCHET-7 settles it once, in writing, with the supply-chain review behind it.

The shortlist was **exactly two** options, and deliberately not widened. Under invariant 9 no option
that involves implementing a primitive ourselves was considered at all:

- **(a)** Go stdlib `crypto/ed25519`.
- **(b)** a cgo binding to libsodium — the option that matches the user's word *"libsodium"*
  literally.

**Decision.** **(a) Go stdlib `crypto/ed25519`.** All Ed25519 keygen, detached signing and
verification in agent-bus uses `crypto/ed25519` from the Go standard library. **No third-party crypto
module and no C crypto library is added to the build or the runtime image.**

This *confirms* the presumption the SIGN/AUTH tasks were already carrying, rather than overriding it
— but it is now a reviewed decision rather than an inherited assumption, and the review is recorded
below so it does not have to be re-litigated per task.

**This is not a departure from the user's instruction.** The controlling requirement in
*"just use standard message auth/integrity using libsodium"* was **standard, audited, high-level
sign/verify** — not a specific vendor. `crypto/ed25519` is the same Ed25519 primitive that
libsodium's `crypto_sign` implements, from the same ref10/RFC 8032 reference lineage, behind an
equally high-level `Sign`/`Verify` API. The wire format is identical: a 32-byte public key, a 64-byte
detached signature. A future peer built against libsodium interoperates byte-for-byte.

### The review (RATCHET-7's mandated supply-chain assessment)

Conducted by the `security` sub-agent (read-only, opus) with network access, verified live against
`proxy.golang.org`, `sum.golang.org`, `api.osv.dev`, `vuln.go.dev` and the GitHub API; the local
go1.19.4 facts were independently reproduced. Verdict: **PASS, recommend (a)** — rejecting (b) on
evidence, not on preference.

**1. Provenance and who can push a release.** For (a) the supply chain *is* the Go toolchain: the Go
team, a public proposal/review process, and releases distributed as signed, checksummed tarballs. For
(b) the trust chain gains an extra, weaker link — a *binding* maintainer sitting between us and
libsodium, on top of libsodium itself.

**2. There is no credibly maintained cgo libsodium binding for Go.** This is the fact that decides
it. Upstream `jedisct1/libsodium` is healthy (pushed 2026-07-31); **the rot is in the Go bindings,
not in libsodium**:

| candidate | latest release | last push | verdict |
|---|---|---|---|
| `github.com/jamesruan/sodium` | v1.0.14, 2022-01-02 | 2022-01-02 | ~4.6 years stale, single individual |
| `github.com/GoKillers/libsodium-go` | **no tags at all** (`v0.0.0-20171022220152`) | 2018-02-23 | ~8.4 years abandoned |
| `tink-crypto/tink-go/v2` | v2.7.0, 2026-06-10 | active | **not a libsodium binding** |

Tink was assessed and is a category error in this shortlist: its `signature/ed25519` and
`signature/subtle` packages `import "crypto/ed25519"` and call `ed25519.Sign`, and the module
contains no `.c`/`.h` files. **Choosing Tink *is* choosing option (a)** — plus five modules
(wycheproof, go-cmp, `golang.org/x/crypto`, protobuf, x/sys), a protobuf keyset layer, a `go 1.24.0`
toolchain floor, and a Tink output-prefix that is **not RFC 8032 detached-signature compatible**
unless `VariantNoPrefix` is used. Strictly worse on every axis that matters here, and an interop trap
aimed straight at SIGN-1.

Picking (b) would therefore mean depending on unmaintained code **for the security-critical path** —
which is the precise failure mode the entry above was written about: `status-im/doubleratchet`, a
2019 beta that was the only thing fitting the constraints and was quietly broken.

**3. `#cgo pkg-config: libsodium` pins nothing.** `jamesruan/sodium` vendors no C and links the
*system* libsodium, so the effective crypto version would be whatever the base image happens to ship
— the opposite of a pinned dependency. libsodium itself demonstrably accrues CVEs
(**CVE-2025-69277**, published 2025-12-31: `crypto_core_ed25519_is_valid_point` accepts points
outside the main group; carried as Debian **DSA-6094-1**, fixed in `1.0.18-1+deb12u1`). That CVE is
in point validation rather than `crypto_sign` detached verify, so it likely would not have hit us —
but note it **postdates the newest binding release by roughly four years**, and the binding has no
way to ship the fix.

By contrast `crypto/ed25519` has **zero advisories, ever**: OSV `ecosystem=Go, package=stdlib`
returns 159 advisories and none of them touch ed25519.

**4. cgo breaks the shipping model (DEPLOY-1).** Verified on this box:
- `CGO_ENABLED=0 go build` on a cgo package fails outright
  (`build constraints exclude all Go files`).
- With stdlib ed25519, `CGO_ENABLED=0 go build` produces `ELF 64-bit … statically linked` — it runs
  on a `scratch`/distroless runtime with nothing else in the image.
- `pkg-config --modversion libsodium` **fails on this workstation today**, so (b) would break the
  local dev build immediately as well as complicating the image.

Option (b) forces `CGO_ENABLED=1`, a dynamically linked binary, `libsodium.so` in the *runtime*
image, and `libsodium-dev` + `pkg-config` in the builder — directly against DEPLOY-1's "minimal
runtime image" requirement, and it complicates cross-compilation.

**5. Transitive dependency footprint.** (a): zero. `go.mod` stays dependency-free. (b): one Go module
plus one C library plus its distro packaging, none of it covered by the Go module checksum database.

**6. RFC 8032 conformance, verified locally on go1.19.4.** RFC 8032 §7.1 TEST 1 reproduces exactly —
seed `9d61b19d…7f60` yields public key `d75a9801…7511a` and signature `e5564300…7a100b`, and
`Verify` returns true. Sizes are 32 / 64 / 64 / 32 (public / private / signature / seed).

**Pinned version.** There is deliberately **no module pin, because there is no module** — the pin
*is* the builder image. Concretely:
- `go.mod` stays free of crypto dependencies. `crypto/ed25519` has been in the stdlib since Go 1.13
  and needs no toolchain bump; the current `go 1.19` pin is sufficient (see the go1.19 entry above).
- **DEPLOY-1** pins the Go builder image by exact tag **and digest**; **DEPLOY-4** owns bumping it.
  Those two tasks are the whole version-pinning story for this decision.
- Keep `GOFLAGS` free of `-mod=mod`, and `GONOSUMDB`/`GOPRIVATE`/`GONOSUMCHECK` unset, so
  `GOSUMDB=sum.golang.org` and `GOPROXY=proxy.golang.org` stay in force for everything else.

**How we learn about advisories** (named mechanisms, not a vague intention):
1. **`govulncheck`** as a step in **DEPLOY-5**'s container check. It covers the Go stdlib, so with
   option (a) our crypto is *inside* its coverage. **Note the asymmetry that reinforces the choice:
   `govulncheck` is structurally blind to a C library** — `vuln.go.dev/index/modules.json` tracks
   1344 modules, includes `stdlib`, and contains nothing matching "sodium". Under (b) our crypto
   would have sat in the one place our tooling cannot see.
2. **Subscribe `golang-announce`** — where Go security releases are published.
3. A **base-image CVE scan** (trivy/grype) in the same DEPLOY-5 target, since `govulncheck` never
   covers OS packages under either option.

**User consent.** **None required.** Option (a) adds no dependency, no runtime component, and no new
key material or rotation behaviour. Consent *would* have been required for (b), which would have put
a new C library into the shipped image.

### Consequences

- **The open question in the previous entry is now closed.** SIGN-1..8, AUTH-1/2/3, CRYPTO-3/4/10 use
  `crypto/ed25519` and must not re-open the choice. Reversing this needs a new dated entry here.
- **Wire compatibility is preserved**, so this is reversible in principle: raw 32-byte keys and
  64-byte detached signatures mean a later move to any RFC 8032 implementation is a build-time
  change, not an on-disk or wire-format break.
- **Residual risk: go1.19 is out of support**, so a stdlib CVE would not be backported to it. The
  mitigation is **DEPLOY-4** landing a supported Go, and **DEPLOY-5**'s `govulncheck` step is exactly
  what would surface the need. This is a real, accepted, *tracked* risk — not an unknown. (Go 1.24+
  additionally routes ed25519 through the FIPS 140-3 Go Cryptographic Module, a provenance
  improvement that favours a modern pin; noted as directional, validation status not verified here.)
- **Do NOT reach for `ed25519consensus`.** It exists for consensus systems needing bit-exact
  batch/cofactored verification semantics. Our threat model is detached signatures over
  server-minted message metadata, with replay handled separately (invariant 10: server-minted
  monotonic sequence + recipient-side cursor). Stdlib semantics are correct for us.

### Sharp edges the implementing tasks MUST handle (verified against the go1.19.4 stdlib source and reproduced at runtime)

These are the misuse-resistance gaps in an otherwise high-level API. They are recorded here because
invariant 9 asks for the library that wraps as much of the problem as possible, and these are the
parts `crypto/ed25519` does *not* wrap:

- **`ed25519.Verify` PANICS on a wrong-size public key** — `panic("ed25519: bad public key length: …")`
  at `crypto/ed25519/ed25519.go:197`, including on `nil` (reproduced: lengths 3 and 0 both panic).
  **This is a remote-panic/DoS vector in CRYPTO-10**, which verifies keys drawn from an
  attacker-influenceable contact list. Every call site MUST length-check against
  `ed25519.PublicKeySize` first, and MUST have a test proving it.
- **`ed25519.Sign` PANICS on a wrong-size private key** (`ed25519.go:153`), and
  **`PrivateKey.Public()` on a short key panics with a bare `slice bounds out of range`**
  (`ed25519.go:58`). Validate key material **at load time**, not at use.
- **A short or garbage *signature* is safe** — `Verify` returns `false` with no panic. That asymmetry
  between the key case and the signature case is exactly the trap: testing malformed signatures does
  not prove malformed keys are handled.
- **Never sign a digest.** The `crypto.Signer` path explicitly rejects pre-hashed input
  (`ed25519: cannot sign hashed message`). Ed25519 signs the message itself. SIGN-1 must not take the
  shortcut of "sign the content hash", and DUR-5's content hash and SIGN-2's signature must be
  computed over the *identical* canonical bytes SIGN-1 defines.
- **Classic malleability is already closed**: stdlib rejects non-canonical S (`SetCanonicalBytes`)
  and high-bit S (`sig[63]&224`), both confirmed at runtime. Go uses ref10 cofactorless verification
  and explicitly does not extend the Go 1 compatibility promise to low-order/non-canonical edge
  cases — irrelevant to our threat model, as noted above.
- **Always `ed25519.GenerateKey(nil)`** so key generation draws from `crypto/rand`. Never seed from a
  caller-supplied reader.

---

## 2026-08-02 — The data-directory lock is `internal/dirlock`, taken at process startup, NOT inside `wal.Open` (DUR-8)

**Context.** Nothing in code stopped two servers being started on one data directory.
`internal/wal/log.go`'s replay-vs-open offset agreement check says so in its own words: it "IS NOT A
LOCK", it only catches a change *inside the window between the two passes*, and two servers "can both
replay the same bytes, both agree, and then both append at the same offsets, which destroys the log".
The only protection was a convention line in `CLAUDE.md`, enforced by nothing. The failure mode is
silent and unrecoverable — it destroys the append-only audit trail that invariants 4, 5 and 6 all
rest on.

**Decision.** A new stdlib-only package `internal/dirlock` takes an exclusive advisory
`syscall.Flock(LOCK_EX|LOCK_NB)` on `<data-dir>/bus.lock`, and `cmd/agent-bus`'s `run()` acquires it
immediately after `os.MkdirAll(cfg.DataDir, 0o700)` and **before** `ids.LoadOrCreateBusID` — before
any read or write of the data dir whatsoever.

**This deviates from the task text**, which said "the lock goes in `internal/wal/log.go` Open".
Placing it at process startup instead is strictly stronger and was chosen deliberately:

- `wal.Open` is not the first thing to touch the data directory. `ids.LoadOrCreateBusID` already
  reads and writes it, and a lock inside `wal.Open` would leave that outside the lock.
- The guarantee we want is "one server per data directory", which is a property of the PROCESS, not
  of one file handle. A lock scoped to the WAL would have to be re-reasoned about for every future
  store that lands in the same directory.
- It keeps `internal/wal` free of a process-lifetime global, and the lock testable on its own.

Recorded because `CLAUDE.md` step 8 requires a deviation from the spec to be written down rather
than discovered later in a diff.

**Sub-decisions, each of which is a way this could have gone wrong:**

- **Non-blocking only.** `LOCK_NB`, no blocking variant. Every caller wants "refuse to start"; a bus
  that waits for another bus to exit is a confusing failure mode, not a feature. Fail fast, exit 1,
  name the directory and the probable holder pid.
- **`Release` NEVER unlinks the lock file.** Unlinking races: process B can still hold its descriptor
  (and its flock) on the now-unlinked inode while process C creates a *fresh* file at the same path
  and locks that — two holders on one data directory, which is the exact disaster the lock exists to
  prevent. Leaving the file in place means everyone always locks the same inode. The file persisting
  forever is correct and intended.
- **No stale-lock cleanup and no pid liveness probe.** The kernel drops an flock when the holding
  process dies, by any route including SIGKILL, so a crash leaves a lock FILE but no LOCK. Probing
  whether a recorded pid is alive and unlinking on "no" is how a locking scheme grows two holders —
  pids are recycled, and the probe-then-unlink is a race with every other starter. Asserted, not
  assumed, by `TestDirLockReleasedAfterSIGKILL`.
- **The recorded pid is advisory and never load-bearing.** It exists so a refusal can say "try
  `ps 4242`". It is read *after* our own lock failed, so it may be stale; it is never used for
  control flow, never signalled, and never used to decide whether to unlink.
- **`bus.lock`, deliberately not `*.log`.** `wal.log` is the WAL and `.gitignore` ignores `*.log`, so
  a `.log`-suffixed lock file would be one glob away from being read as log data, and invisible in
  `git status`. It is not durable state; replay never reads it.
- **`O_NOFOLLOW` on the lock file** (from the security pass). Acquire truncates the file once the
  lock is its own; without `O_NOFOLLOW` a symlink planted at `<dir>/bus.lock` turns that into a
  "truncate any file the bus user can write, and create it if missing" primitive whose damage lands
  outside the data dir. Planting it already requires being the bus user, so no privilege boundary is
  crossed — the point is to keep the blast radius of a compromised data dir inside that dir, for the
  cost of one flag.
- **No build tag on `dirlock.go`**, even though it uses `syscall.Flock` and is therefore Unix-only.
  A `//go:build unix` tag plus a no-op fallback would mean a non-Unix build silently ships a server
  with NO data-directory locking. A compile error is the honest outcome; the project targets Linux.

**Consequences.**

- Every future durable store added to the data directory is automatically inside the lock, provided
  it is opened from `run()` after the acquire. DUR-9 (`wal.Open` in `run()`) must keep that ordering;
  `TestRunRefusesALockedDataDir` guards it by asserting a refused `run()` leaves `bus.lock` as the
  *only* entry in the data dir.
- The lock is **advisory**: it excludes other processes that also flock the file — in practice other
  `agent-bus` servers — not `rm`, `cp`, an editor or a backup job. It is a guard against the
  realistic accident, not a mandatory-locking security control.
- **flock over NFS before Linux 2.6.12, and on some network filesystems, is unreliable.** An operator
  who puts a data directory on such a mount gets no protection. We deliberately do not try to solve
  that: the answer is "do not run the durable store on a filesystem whose locking you do not trust",
  not a cleverer lock (invariant 8).
- The returned `*Lock` must stay reachable for as long as the lock is wanted. `_, err :=
  dirlock.Acquire(dir)` is a silent bug: the `*os.File` finalizer would close the descriptor at the
  next GC and quietly hand the directory to a second server. Documented at `Acquire`.

---

## 2026-08-02 — DUR-9: the WAL is opened in `run()` with a NIL Applier, and exposed to the HTTP layer as a one-method interface

**Context.** DUR-1..DUR-4 built a complete two-phase durable write path in `internal/wal`, and
nothing in the server binary called it. DUR-9 wires it in.

**Decision 1 — the WAL is opened from `cmd/agent-bus`'s `run()`, after `dirlock.Acquire` and before
`srv.Serve`.** Not from inside `internal/httpapi`, and not lazily on the first write.

- After the lock, because replay READS the file and `RepairTail` may TRUNCATE its tail. A WAL opened
  before the flock is a WAL two processes can be repairing at once, which is precisely the corruption
  DUR-8 exists to prevent. The ordering is guarded, not merely commented:
  `TestRunRefusesALockedDataDir` asserts a start refused at the lock leaves `bus.lock` as the ONLY
  entry in the data dir, and the reviewer confirmed by mutation that swapping the two lines fails it.
- Before serving, because `wal.Open` does not return until replay has finished (invariant 5). The
  guarantee is stated carefully: nothing is ANSWERED before replay. It is NOT "the socket is unbound
  during replay" — a bound-but-unserved listener answers nothing, and a comment claiming the stronger
  property was found false by mutation and corrected.
- Not lazily, because "the durable store opens on the first write" means the first write is also the
  first time the operator learns the store is unusable, long after startup succeeded.

**Decision 2 — a failed open or replay is a FATAL startup error, never a degraded start.** Exit 1,
the message names the data dir, and no listener binds. The temptation to "start anyway and serve what
we can" is exactly how an acknowledged write disappears silently: an empty store is
indistinguishable, to every client, from a bus that never accepted anything. The ONLY self-repair is
the provably torn tail `RepairTail` already performs (bytes whose `Append` never returned, and so
were never acknowledged to anyone). Everything else refuses, and leaves the file byte-for-byte
unchanged so an operator can copy it and diagnose it — asserted by
`TestServerOpensWALOnStartRefusesACorruptLog`, not assumed.

**Decision 3 — the `Applier` passed to `wal.Open` is `nil`, and this is documented rather than
hidden.** There is no in-memory serving copy yet: `internal/store` is a `doc.go` stub. Replay today
verifies every frame, pairs commits with prepares, discards uncommitted prepares and establishes the
next-index high-water mark — and rebuilds no application state, because none exists.

- The alternative considered and REJECTED: invent a placeholder Applier now. A no-op Applier that
  swallows every recovered entry would make the code LOOK like it recovers state while recovering
  nothing, which is worse than an obvious `nil` — it is a claim a future reader would believe. When
  the store lands it is passed here, and the honesty comment goes with it.
- Consequence: nobody may read "replay ran" as "state was restored" until the store exists. The log
  line reports counts of what the DISK contained, not of what memory absorbed.

  > CORRECTED 2026-08-16: the Applier is NO LONGER NIL and `internal/store` is no longer a stub — it is a full
  package, and `cmd/agent-bus/main.go:626-630` passes `auth.NewMultiplexApplier(lg, appliers)` into
  `wal.Open`. Decision 3's REASONING is what survives and is worth keeping: never invent a no-op
  placeholder Applier, because it makes the code look like it recovers state while recovering nothing.
  Its factual premise is spent. Decisions 1, 2, 4 and 5 are all still true: `wal.Open` is called from
  `run()` (`main.go:630`) after `dirlock.Acquire` (`:405`), and `httpapi.DurableLog` is still exactly
  one method (`internal/httpapi/server.go:38-42`).

**Decision 4 — the HTTP layer holds the log as `httpapi.DurableLog`, a ONE-METHOD interface
(`Write(wal.Entry) (wal.Committed, error)`), not as a `*wal.Log`.**

- One method because `Write` is the whole of invariant 4 as a handler needs it: hand over an entry,
  get a `Committed` back only once it is fsynced. A handler has no business calling `Begin`, `Close`
  or `Recovered` — those belong to the process lifecycle `main` owns, and a handler that could
  `Close` the log is a handler that can take the bus down.
- An interface rather than the concrete type so this package's own tests never open a real log on
  disk, and so a future in-memory or relay-backed implementation is a substitution rather than a
  refactor. `internal/wal` is still imported for the wire types (`Entry`, `Committed`); duplicating
  those to avoid the import would be cleverness bought at the price of two definitions of one
  contract (invariant 8). There is no cycle: `wal` does not import `httpapi`.
- It may be nil, and NOTHING may panic on that. There is deliberately no default: a no-op stand-in
  write path is silent data loss, which is strictly worse than a nil the caller must check.
- No handler and no route uses it in this task. It is wired now so that MSG/AUTH/IDEM/SIGN reach for
  ONE write path instead of each minting its own.

**Decision 5 — the successful close logs a DEBUG line, on purpose.** `msg="write-ahead log closed"`
exists because the close-on-shutdown requirement was otherwise UNTESTABLE: the kernel closes the
descriptor at process exit, so `bus.wal` is byte-identical whether or not the deferred `Close` ran,
and the reviewer proved by mutation that deleting the whole defer left the suite green. The line is
the observable event; the test additionally requires it to precede
`msg="data directory lock released"`, which pins the LIFO close-then-unlock order. Deleting it as
"log noise" silently removes the only guard on scope item 4.

---

## 2026-08-02 — Auth: client signs a short-lived, server-provided session token (settles AUTH-1)

**Context.** AUTH-1 had been consent-gated on how an agent proves possession of its enrolment key:
sign every request (safe, but hostile to a shell client) or hold a bearer token indefinitely (simple,
but a stolen token is valid forever). AUTH-1 blocked AUTH-2/3, all of MSG, POLL and the agent-facing
surface — i.e. the entire path to a usable bus.

**Decision (user, verbatim).** *"I want the client to sign a new server-provided session token that
lasts at most an hour. The client library/interface is a compiled go cli that does all the heavy
lifting, including waiting/long-polling for new messages to arrive."*

So:

1. **Enrolment** mints the server-authoritative agent id and records the agent's Ed25519 **public**
   key (invariants 1 and 3; the auth keypair stays distinct from the messaging keypair).
2. **Session establishment**: the client asks for a session; the **server** provides the token value;
   the client **signs it** with its enrolment private key; the server verifies against the recorded
   public key and thereafter accepts that session.
3. **The session lasts at most one hour.** After that the client repeats step 2 for a fresh
   server-provided token.

**Why this shape.** It is a middle path, and each half earns its place:
- The token is **server-provided**, so the client never chooses the value it signs. A
  client-chosen challenge would allow pre-computation and would make the signature prove far less.
- Signing happens **once per session, not per request**, so the client stays simple and the hot path
  (long-poll, send) is a cheap credential check rather than a signature verification.
- The **one-hour cap** bounds the blast radius of a leaked token, which is the failure a
  never-expiring bearer token cannot survive.

**Consequences — things this commits us to.**

- **Revocation is now time-bounded, not immediate.** `POST /v1/leave` and any future revoke must
  invalidate *outstanding sessions*, not merely stop new ones. Without that, a revoked or compromised
  agent stays live for up to an hour. This must be explicit in AUTH-4 and tested.

  > CORRECTED 2026-08-16: `POST /v1/leave` STILL DOES NOT EXIST. No such route is registered anywhere in
  `internal/httpapi`, and both `client/client.go:675` and `cmd/agent-busctl/logout.go:22` say so in
  terms; `agent-busctl logout` is a purely LOCAL credential wipe. AUTH-4's revocation surface is
  unbuilt. What the design did buy is real but narrower: `authMiddleware` never caches an
  `Authenticate` result, so a session removed from the table stops working on the NEXT request
  (`internal/httpapi/authmw.go:237-241`).
- **Expiry needs a clock-skew policy.** Server-side expiry is authoritative; the client must refresh
  early rather than at the boundary, and the server must reject an expired token even if the client
  believes it is valid. Say what the tolerance is rather than leaving it implicit.
- **Session state must survive restart** (invariant 5) or every bus restart forcibly re-authenticates
  every agent. Decide deliberately which of those two we want; it is not obvious that persisting
  sessions is right, and expiring them on restart may be the safer default.
- **The compiled Go CLI becomes the agent interface**, doing key generation and storage, session
  refresh, long-poll with cursor management, reconnect/backoff, and verification of inbound messages.
  This is a change to **invariant 7**, which currently requires a `scripts/bus-*.sh` wrapper per
  capability: the requirement is unchanged in spirit (an agent never constructs an HTTP call) but the
  vehicle is now a CLI subcommand. The AGENTIF epic's eight shell-wrapper tasks need re-scoping, and
  the CLI epic (filed for a *human* client) should be reconciled with this — most likely one binary
  serving both, since the heavy lifting is identical.
- Signing a server-provided value is a standard challenge-response; per invariant 9 it must be built
  from the stdlib's `crypto/ed25519` sign/verify API, never assembled from primitives.

**Open, deliberately not decided here:** the token's format and whether it carries claims or is an
opaque server-side handle; whether sessions persist across restart; the exact skew tolerance; and
whether one binary serves both agents and humans.

> CORRECTED 2026-08-16: three of these four are settled elsewhere in this file and none is still open as
written. The token is an OPAQUE SERVER-SIDE HANDLE, not a claims token (2026-08-02, "Sixteen open
questions settled", §4–7; `auth.SessionLifetime = time.Hour`, `internal/auth/session.go:24`). The
skew rule is "server expiry is authoritative, the client refreshes at 75% of lifetime" (same entry).
One binary serves both audiences (2026-08-02, "The Go CLI replaces the shell wrappers";
`cmd/agent-busctl`). The session-persistence question is answered in its own dated entries and is
deliberately not restated here.

---

## 2026-08-02 — The Go CLI replaces the shell wrappers (amends invariant 7)

**Context.** Invariant 7 required every capability to ship a `scripts/bus-*.sh` wrapper, so that an
agent never constructs an HTTP call. The AUTH-1 decision introduced a compiled Go CLI as the agent
interface, leaving two competing delivery vehicles. Separately, a CLI epic had been filed for a
*human* client, which overlapped it almost entirely.

**Decision (user, verbatim).** *"the go cli should take the place of the .sh files and be easy to use
for a human + friendly for an agent to use or embed"*

So there is **one client**, a compiled Go binary, serving both audiences. The shell wrappers are
retired as their subcommands land. Invariant 7 is amended, not weakened: nobody hand-writes HTTP, and
a feature without its CLI subcommand is still not done.

**Consequences.**

- **"Embed" is the load-bearing word.** It means the CLI must be a thin shell over a **reusable Go
  client package**, and that package therefore cannot live under `internal/` — Go would forbid any
  other module importing it. Deciding this late would be expensive, so it is decided now: the client
  package is importable, and its exported surface is a public API subject to compatibility care.
- **Three audiences, three concrete requirements** (see invariant 7 for the full text): readable
  output and remedial errors for humans; `--json`, stable exit codes, no interactive prompts and
  no TTY-dependent credential input for agents shelling out; an importable package for agents
  embedding. The long-poll command streams newline-delimited JSON so it can be consumed
  incrementally rather than buffered to completion.
- **The AGENTIF epic must be re-scoped.** Its eight tasks are shell wrappers; they become CLI
  subcommands. `AGENTIF-1` (`bus-serve.sh`) already shipped and is the only wrapper that exists.
- **The CLI epic and AGENTIF now overlap and should be reconciled** — most likely merged, since the
  heavy lifting is identical and the only difference is output formatting.
- `AGENT_PROTOCOL.md` must be rewritten against CLI subcommands rather than shell scripts.
- The binary now has a second consumer with a compatibility expectation, so its flags, exit codes and
  JSON shapes belong in `CONTRACTS.md` like any other contract surface.

---

## 2026-08-02 — Sixteen open questions settled (durability policy, session/auth, delivery, process)

All answers are the user's, given together. Two of them **reverse behaviour already implemented**;
those are called out first because the code currently does the opposite.

### 1. Availability over retention: the bus ALWAYS restarts (REVERSES current behaviour)

> *"always be able to restart, prefer to discard messages and/or corruption, with logging"*

The shipped code refuses to start on several damage classes. That is now wrong. Recovery must always
reach a running server: discard damaged records, **log loudly and specifically what was discarded**,
and continue.

**This reconciles with answer (2) below rather than contradicting it.** The reviewer's finding — a
double fault silently drops an acknowledged COMMIT — was rated P0 and the user agreed. The defect
was never that data was discarded; it is that the discard was **SILENT**. Discarding is now the
sanctioned behaviour; doing it without a log record is the bug. Every discard must be observable.

Consequence for **invariant 4** ("nothing acknowledged before it is durable"): acknowledged data may
now be discarded when it is found corrupt. The guarantee is narrowed honestly — we do not lose
acknowledged data through *our own* write path, but we will not hold the bus hostage to damaged
media. The narrowing is deliberate and must be stated in `PROTOCOL.md`, not left implicit.

Consequence for **invariant 6**: truncation is no longer restricted to a verified-corrupt *tail*.
Damaged records anywhere may be discarded — with a log entry each.

This also removes the permanent-refuse-to-start DoS, and with it the need for the operator escape
hatch that was previously recommended: always-restart *is* the escape hatch.

### 2. The double-fault silent COMMIT loss is P0 — go with reviewer

A torn sector is one physical event, not two independent faults, so the case is realistic. Fix is per
(1): discard and log, never silently.

### 3. Replace CRC32C with a modern keyed construction (CHANGES THE ON-DISK FORMAT)

> *"don't use crc! use a hash/hmac/more modern approach. We're not optimising for efficiency, we're
> optimising for integrity and security"*

CRC32C is an error-detecting code, not an integrity primitive: it is unkeyed and GF(2)-linear, which
is precisely why security demonstrated that an ordinary remote client could craft a payload making a
torn tail look like a complete record. A keyed MAC **eliminates that attack by construction** — a
client cannot compute a MAC over a key it does not hold.

Per **invariant 9**, this must come from the stdlib's high-level API (`crypto/hmac` + `crypto/sha256`),
never a hand-rolled construction.

Consequences: this is an **on-disk format change** and needs a reserved format/record-type version.
Much of DUR-4's torn-tail heuristic exists to compensate for a weak checksum and should get
**simpler**, not more complex, once a strong MAC can distinguish damage from truth reliably. Key
management is a new question — where the MAC key lives, and that storing it beside the WAL defends
against a remote client but not against an attacker who already has data-directory write access.

### 4–7. Session and auth

- **Sessions do NOT survive restart.** Expire them; the CLI re-authenticates.
- **Revocation is immediate.** `/leave` invalidates outstanding sessions at once — not at the
  ≤1h expiry.

  > CORRECTED 2026-08-16: `/v1/leave` was never built (see the correction on the AUTH-1 entry above), so
  "revocation is immediate" is a property of the session TABLE, not of any operator-reachable route,
  and it is bounded rather than instant. `authMiddleware` resolves against live session state on every
  request with no cache (`internal/httpapi/authmw.go:237-241`) — but the check runs ONCE, at
  admission, so a long poll admitted the instant before a revoke runs to the end of its timeout, up to
  `hub.MaxPollTimeout` (5 minutes), and an agent may hold up to `hub.MaxWaitersPerAgent` (32) of them.
  Closing that is the still-open task `AUTH-2-FU-POLLEXPIRY` (`internal/httpapi/authmw.go:275-297`).
- **Tokens are opaque server-side handles**, not signed claims. This is what makes immediate
  revocation possible; stateless claims cannot be revoked.
- **Clock skew**: server expiry is authoritative; the client refreshes at 75% of lifetime.

### 8–11. Delivery and limits

- **Delivery is AT-LEAST-ONCE.** Duplicates are the normal steady state, which is what invariant 10's
  idempotency exists to absorb. Must be stated in `PROTOCOL.md` and `AGENT_PROTOCOL.md`.
- **Idempotency keys are retained for a bounded window and fail closed** — a retry arriving after the
  window is rejected, never silently re-applied.
- **Retention: 1 day or 1 GB**, whichever comes first.
- **The default listen address is localhost.**

### 12–14. Scope

- **Merge the CLI and AGENTIF epics** — one client, one epic.
- **Relay auth is bi-directional and uses the same scheme as clients.** A node is **either a client
  endpoint or a relay, never both** — that exclusivity is a routing and trust simplification and
  should be enforced, not merely documented.

  > CORRECTED 2026-08-16 — THE EXCLUSIVITY CLAUSE IS REFUTED, and it was never enforced. Every bus is BOTH a
  client endpoint and a relay: `internal/httpapi/peermount.go` mounts `/v1/peer/{enroll,relay,roster}`
  on the SAME mux that serves the agent routes, `cmd/agent-bus/main.go:1441-1442` wires `Peer` and
  `PeerPrincipals` on a shipped server, and the three-bus run verified in containers on 2026-08-15
  (DEPLOY-6) has an agent enrolled on bus A while A also forwards to B. Nor is the scheme the same: a
  peer authenticates by TLS client-certificate fingerprint ALONE and never by a session token
  (2026-08-14, RELAY-6 AMENDMENT ruling (i), which names that as a narrowing of invariant 11). The
  other half of the bullet — relay auth is bi-directional — is still OWED on the egress side
  (`MTLS-RELAYGUARD`): we authenticate peers that dial us, and peers cannot yet authenticate us when
  we dial them.
- **Encryption is revisited later, once the bus works.**

### 15–16. Process

- **A missing `proof_cmd` blocks completion**, at least as hard as a vacuous one.
- **`done` requires actually running `scripts/proof-check.sh` and quoting its verdict**, not storing
  a bare command.

---

## 2026-08-02 — Authentication is DEFAULT-DENY, and an anonymous caller gets 401 on an unknown path (AUTH-2, folding in AUTH-6)

**Context.** AUTH-2 added the bearer-token middleware. AUTH-6 had separately flagged that routes were
registered individually on the mux, so the first implementer to write `mux.HandleFunc("/v1/send", …)`
and forget the auth wrapper would ship a fully unauthenticated route on a message bus **with no
failing test**. The two were implemented together deliberately: bolting default-deny on afterwards
would have meant reviewing the enforcement change twice.

**Decision 1 — the middleware wraps the WHOLE mux, with an explicit allow-list.** `authMiddleware`
sits between `LoggingMiddleware` and the mux, so authentication is what happens *unless* a path is
on `unauthenticatedRoutes`. Forgetting now means **401, not open**. Opening a route up requires a
deliberate, visible, reviewable edit to a five-entry map — and a second edit to the golden list in
`TestEveryRouteRequiresAuth`, which is the point: two signatures to widen the anonymous surface.

> CORRECTED 2026-08-16: the map has SIX entries, not five — `/healthz`, `/v1/info`, `RouteDiscovery`
(`/v1/discovery`), `RouteEnroll`, `RouteSessionBegin`, `RouteSessionComplete`
(`internal/httpapi/authmw.go:76-83`). `/v1/discovery` was added by the 2026-08-07 DISCOVERY-DOC
decision below. The two-signatures property is intact: the golden list is still there
(`internal/httpapi/authmw_test.go:549,629-635`). Anyone enumerating invariant 3's five
unauthenticated routes from prose is one short — cite the allow-list in code, not the invariant's
wording.

**Decision 2 — matching is EXACT string equality on `r.URL.Path`.** No prefix match, no path
cleaning, no trailing-slash tolerance. `//healthz`, `/healthz/`, `/HEALTHZ` and `/v1/info/` are all
*not* on the list and are refused. The failure mode of strictness is a 401 on a misspelled probe;
the failure mode of leniency is a normalisation mismatch between the middleware and `http.ServeMux`,
which is exactly how allow-list bypasses are built. Security probed this over raw TCP with 27
request shapes (percent-encoded `%2f`/`%2e%2e`, dot-dot walks, `;`-params, absolute-form
request-URI, `CONNECT`) and every divergent spelling landed on the deny side. It holds *structurally*
— both the middleware and `ServeMux.Handler` key off the already-decoded `r.URL.Path`, and all five
allow-list entries are already `path.Clean`-canonical, so `cleanPath` is the identity on exactly the
paths that matter. **This strictness is load-bearing and must not be relaxed for convenience.**

**Decision 3 — BOTH session routes are on the allow-list, including `/v1/session/complete`.** The
obvious four are `/healthz`, `/v1/info`, `/v1/enroll` and `/v1/session/begin`. `session/complete` is
the one that looks skippable, because its caller *does* hold a token — but that token names a
**PENDING** session, and `auth.Service.Authenticate` rejects a pending session exactly like an
unknown one. A bearer requirement there would be **unsatisfiable, not strict**: the only credential
that could satisfy it is the one the call exists to create. That route is authenticated by the
Ed25519 signature over the server-chosen token, which `handleSessionComplete` verifies; the token in
its body is not a credential until that succeeds.

**Decision 4 — an anonymous request to an UNKNOWN path returns 401, not 404.** AUTH-6 explicitly
required this to be decided rather than fallen into. 401 wins on two grounds. First, it is the only
answer consistent with one rule: any attempt to 404-before-authenticating means checking route
existence outside the wrapper, which is itself an unauthenticated code path — precisely the shape
AUTH-6 exists to forbid. Second, it does not let an anonymous caller enumerate which paths this bus
serves by reading status codes. A caller holding a **valid token** still gets the honest 404 from the
mux; that asymmetry is the feature.

*Consequences.* `CORE-8` (JSON 404 catch-all) is now **constrained**: its catch-all must be
registered INSIDE the wrapper, or it becomes the one unauthenticated route that leaks the surface.
Two existing `TestHealthzInfo` cases changed from 404 to 401 in the same commit.

**Decision 5 — nil `Options.Auth` serves the allow-list and nothing else.** A server built without an
auth service can authenticate nobody, so every non-allow-listed path is 401. The `WWW-Authenticate`
challenge says `invalid_token`, not `invalid_request` — the caller's request may have been perfectly
well-formed, so `invalid_request` would be a lie, and it has the useful side effect of making a
no-auth-service build indistinguishable from a normal build handed a bad token. AUTH-1's documented
behaviour is preserved exactly: the three credential routes stay allow-listed, so they still reach
the mux and still 404 when unregistered.

**Decision 6 — unknown, pending and expired are BYTE-IDENTICAL to the client.** Status, body and
`WWW-Authenticate` are the same for all three. Distinguishing them is an enumeration oracle. The LOG
gets the precise wrapped reason from `internal/auth`; the client gets none of it. There is a test
whose only job is to fail if someone later makes the message "more helpful".

**Rejected: caching the `Authenticate` result.** Tokens are opaque server-side handles precisely so
revocation can be immediate (settled 2026-08-02). A per-request cache, however short, is a window in
which a revoked credential still works. Every request resolves against live session state. The cost
is one SHA-256 over ≤512 bytes plus an O(1) map lookup — negligible against HTTP parsing.

**Rejected: constant-time token comparison.** Argued for by reflex, wrong here. The session table is
keyed on `hex(SHA-256(token))`, so a near-miss guess produces an *uncorrelated* hash and a
non-constant-time bucket compare leaks nothing about the real token. The branches that do differ
measurably (expired does a map delete plus an RFC3339Nano format) are reachable only by a caller who
already holds that token. Adding it would be cargo cult, and this is recorded so it is not
"discovered" as a finding again.

**Known coverage boundary, accepted:** `OPTIONS * HTTP/1.1` never reaches the middleware. Go's
`net/http` answers it above the application handler with `globalOptionsHandler` (bare 200,
`Content-Length: 0`). It exposes no application data or state, and
`http.Server.DisableGeneralOptionsHandler` is go1.20+ while this module is pinned at go1.19. Recorded
in `authmw.go`'s doc comment rather than worked around.

**Follow-ups this decision creates** (filed separately, no key invented here): per-source rate
limiting on the unauthenticated routes and the 401 path; `auth.Service.Authenticate` now takes an
exclusive `sessMu` on every request while the *unauthenticated* `BeginSession` holds the same mutex
for an O(n) sweep, so an anonymous flood can contend with authenticated traffic in a way it could not
before; and authentication is evaluated ONCE at request entry, so a long-poll could outlive its
session — the POLL epic must cap the wait at `min(PollTimeout, time.Until(principal.ExpiresAt))` or
"revocation is immediate" is quietly false for a poll already in flight.

## 2026-08-02 — The per-agent pending-challenge cap is REMOVED, not rekeyed (AUTH-1-FU-PENDINGCAP)

**Decision.** `auth.Options.MaxPendingPerAgent` / `auth.DefaultMaxPendingPerAgent` (8) and the
eviction loop they drove in `Service.BeginSession` are deleted outright. The session table is bounded
by the global `MaxSessions` cap alone, drained by expiry — `ChallengeTTL` (2 minutes) for pending
challenges, `SessionLifetime` (1 hour) for active ones. The helpers `countPendingLocked` and
`oldestPendingLocked` go with it; nothing else used them.

**The defect.** The cap was keyed on `agentID`. `POST /v1/session/begin` is UNAUTHENTICATED by
necessity — it is one of the calls that ISSUES the credential — so `agentID` there is not a subject,
it is an **attacker-supplied victim identifier**. Anyone who merely knows a real agent's id makes
their `BeginSession` calls land in **that agent's** bucket, and eviction under the cap then deletes
the **victim's** own correctly-issued challenge. Nine anonymous requests per round were enough to
prevent a named agent from ever completing an authentication, on a bus whose entire purpose is that
agents can enrol and talk. The attack was also stealthy: the victim's `BeginSession` still *succeeds*,
and only its `CompleteSession` fails, which reads like a client bug.

**Why we did not simply flip eviction to refusal.** It is the identical lockout arriving by the other
door: at the cap it is then the victim's own `BeginSession` that is refused. The property that makes
this unfixable-in-place is not the eviction policy, it is the KEY. There is no ordering, no tie-break
and no policy for a bucket keyed on the victim that is not a lockout primitive.

**Why option (b) (drop it) and not option (a) (key the cap on the request SOURCE).** Two reasons, in
order of weight. First, `internal/auth` deliberately has no view of the HTTP request; "source" would
have to be plumbed down from `internal/httpapi`, and once it is there the thing being built is
per-source rate limiting — which is already its own task (**AUTH-1-FU-RATELIMIT**), applies to all
three unauthenticated routes rather than just this one, and needs its own design for what a "source"
even is behind a proxy. Doing a partial, auth-package-local version of it under this task would have
produced a second mechanism to reconcile later. Second, the memory argument for keeping *any*
per-agent cap is weak: a pending session is a handful of words, and `MaxSessions` (16384) plus a
two-minute `ChallengeTTL` already bound the table. The cap was buying no memory safety that the
global cap did not already provide, while costing a P0 denial of service.

**What this trade makes worse, stated plainly.** Removing the cap makes the *untargeted* flood
CHEAPER, not merely no worse: pending entries used to be bounded by cap × roster size, so filling the
table first required enrolling enough distinct ids, whereas it is now directly reachable with
`MaxSessions` begins naming one known agent — roughly 140 sustained requests per second to hold it.
We accept that: it trades a targeted, permanent, stealthy, 9-request lockout of a chosen victim for an
untargeted, unamplified, self-healing, high-traffic outage that is obvious in any request-rate metric.
It does raise the priority of AUTH-1-FU-RATELIMIT, which is the only mechanism that can charge the
flooder rather than the victim.

**What is now guaranteed, and the one place a per-agent cap would still be SAFE.** Nothing an
unauthenticated caller does can DESTROY a challenge already issued to another agent; a challenge
leaves the table only by expiring, by being completed, or by a failed completion attempt — and that
last route requires holding the 32-byte `crypto/rand` token, so the token's unguessability is now
load-bearing in a way it was not before (there is no TLS in this server, so an on-path observer is a
real threat model on any non-loopback listener). Separately, the security gate established that
ACTIVE sessions are uncapped per agent and are reclaimed only after `SessionLifetime`, which is a
cheaper and longer-lasting outage than the pending flood; that is PRE-EXISTING (the removed cap
counted only pending entries) and is filed as **AUTH-1-FU-ACTIVECAP**. The distinction worth
preserving: an ACTIVE-session cap keyed on agent id is safe precisely because an active session can
only be created by proving possession of that agent's private key. The key is a PROVEN identity, not
an attacker-supplied one. That, and only that, is what made the pending cap unsalvageable.

**Constraint this places on AUTH-1-FU-SESSIONSCALE.** That task plans to change the full-table policy
from refuse to "evict the globally-oldest PENDING session". That reintroduces cross-tenant challenge
destruction — far less severe (16384 begins inside a victim's round trip, versus 9 per round) but the
same class — and it will fail the `session_test.go` subtest asserting `ErrCapacity`. That failure is a
constraint to honour, not a test to update; a rider to this effect is recorded on the task.

---

## 2026-08-02 — TLS is the required transport (new invariant 11)

**Context.** The server had no TLS at all. A review pass flagged it while noting that, with the
per-agent session bound removed, the opacity of the session token had become load-bearing — and an
on-path observer could both read that token and kill a pending challenge by racing the single-attempt
rule.

**Decision (user).** *"add tls as the required transport"*. TLS on every HTTP surface — client and
bus-to-bus relay. **No plaintext listener exists**; the server refuses to start rather than degrade.

**Why this is not merely hardening.** The session token is a **bearer credential**: possession is
sufficient to act as the agent for up to an hour. The whole auth design — the client signing a
server-provided challenge, opaque revocable handles — establishes *who* holds the credential, and all
of it is undone if the credential itself crosses the wire in clear. TLS is what makes the rest of the
auth work worth anything.

The loopback default is unaffected and stays. It bounds exposure; it does not substitute for TLS, and
a bus deliberately exposed on a real interface needs both.

**Consequences, and the open questions this raises.**

- **Certificate provisioning decides whether this is usable at all.** This is a local developer tool.
  If running a bus requires obtaining a CA-issued certificate, people will run it with verification
  disabled — which is *worse* than no TLS, because it looks secure and is not. The design must make
  the trusted path the easy path.
- **A self-signed certificate plus fingerprint pinning fits what already exists.** Enrolment is
  already a trust-establishing moment, and the design already uses TOFU pinning for messaging keys.
  Binding the bus's certificate fingerprint at enrolment reuses that machinery rather than inventing
  a second trust model. NOT YET DECIDED — see the open questions.
- **Never disable certificate verification to make something work.** No `InsecureSkipVerify` on any
  path a user can reach by accident, and no flag that silently does it. Per invariant 9, stdlib
  `crypto/tls`, configured and never reimplemented.
- Relay is bus-to-bus and the peers authenticate bi-directionally; mutual TLS is the obvious fit but
  is not yet decided.
- This changes the CLI's job: it must handle trust establishment, not just HTTP.

**Open — needs a decision before implementation:**
1. How certificates are provisioned: self-signed generated on first start with fingerprint pinning at
   enrolment, operator-supplied cert/key paths, or both.
2. Whether plaintext is permitted *anywhere*, including tests and local development, or truly never.
3. Whether relay peers use mutual TLS in addition to the session-token scheme.

> CORRECTED 2026-08-16: all three are decided, and none of them here. (1) BOTH — self-signed on first start
with the fingerprint carried in the invite blob, and operator-supplied `-tls-cert`/`-tls-key`
(2026-08-02, E6 and E7). (2) TRULY NEVER — no flag, no env var, no build tag; tests use real TLS
against certificates minted at test time (2026-08-02, E7). (3) Not "in addition to": on the peer
routes the certificate ALONE authorises and no bearer token is consulted, which the 2026-08-14
RELAY-6 AMENDMENT ruling (i) records as a deliberate narrowing of invariant 11. The self-signed-
plus-pinning shape the second bullet above called "not yet decided" is what shipped.

---

## 2026-08-02 — Five decisions: MAC key, invite-only enrolment, no id reuse, self-signed mTLS

User answers to the five outstanding questions. Two of them change work that was in flight.

### 1. The HMAC key lives in the DATA DIR; a missing or wrong key is FATAL

Unblocks **DUR-12** (format version 2 already reserved). The key is a file in the data directory,
mode 0600.

Honest statement of what that does and does not buy: it **completely** defeats the attack that
motivated the change — an ordinary enrolled client crafting a payload whose CRC makes damage look
like a complete record — because a client cannot compute a MAC over a key it does not hold. It buys
**nothing** against an attacker who already has data-directory write access; such an attacker can
read the key and forge freely. That is an accepted limit, not an oversight, and belongs in
`PROTOCOL.md`.

**A missing or wrong key at startup is FATAL**, and this is a deliberate exception to the
always-restart rule (invariant 6). The reasoning: a wrong key makes *every* record fail
verification, so "discard the unverifiable" would discard the entire log. Always-restart exists to
stop *media damage* holding the bus hostage; a wrong key is *misconfiguration*, it is fixable in
seconds, and destroying the log over it would be the worst possible response. Fail loudly and name
the key path.

### 2. Enrolment is INVITE-ONLY — and `AUTH-1-FU-ACTIVECAP` is P0

The deeper fix. Every pre-auth attack found so far shares one root: **enrolment was
unauthenticated**, so an attacker could mint its own agents. From there it could exhaust the
16384-entry session table (ACTIVECAP), lock out a named agent (PENDINGCAP, already fixed), or
enumerate the roster. Capping table sizes patches the symptoms one at a time; requiring an invite
removes the capability.

Invites must be **single-use, expiring and revocable**, and redemption is the only route onto the
bus — including for peer buses. `AUTH-1-FU-ACTIVECAP` is raised to **P0**; note it is now
defence-in-depth behind the invite gate rather than the sole bound, so the safe cap keyed on a
*proven* agent id (an active session implies possession of that agent's private key) is still the
right shape.

### 3. NO id reuse — invariant 1 stands, and the salvage reissue is a DEFECT

Invariant 1 is reaffirmed **without narrowing**. Recovery may not reissue an index it has already
handed out, even for a record it discards: when recovery discards a record the sequence advances
past the hole, it never rewinds.

**This makes the current salvage behaviour a bug, not a documented narrowing.** `internal/wal`
reissues the index of a damaged tail record, and `DUR-11` was in flight implementing exactly that.
Contrast invariant 4, which the user *did* deliberately narrow — the difference is that a narrowing
is a choice recorded up front, while this was a behaviour discovered after the fact and is now
rejected.

### 4. Commit history: LEAVE IT

`5a352de` and `3318872` each span two or three tasks. No rewrite. The mis-titling is recorded in
`AGENT_LOG.md` and in the commits' own messages; that record is the remedy.

### 5. TLS: SELF-SIGNED certificates, MUTUAL TLS

No certificate authority anywhere. Both ends present a certificate and both verify. Trust is
established at **enrolment**, which is already the trust-establishing moment: the agent's client-cert
fingerprint is bound to its server-minted agent id, and the bus's cert fingerprint is pinned by the
client. This reuses the TOFU machinery the design already needed rather than inventing a second
trust model, and it means a bus runs on a laptop with no CA in the picture.

**mTLS does not replace the session token; both are required.** mTLS proves which key holder is on
the connection; the session token is the revocable, time-bounded application credential — and
revocability is exactly what a certificate does not give you without a CRL. They should be
cross-checked: a session token presented over a connection whose client certificate belongs to a
*different* agent must be rejected. That is a stronger property than either mechanism alone, and it
is free once both exist.

Invite-only enrolment and mTLS compose well: the invite is what authorises a new client certificate
to be bound to a new agent id in the first place.

---

## 2026-08-02 — Enrolment/mTLS design questions settled (E2–E6, E8)

Six of the seven questions the planning pass raised. **E7 is NOT answered — see the end.**

### E6 — The invite blob carries the bus certificate fingerprint. NO TOFU.

The most consequential of the six. The invite carries **bus id + address + bus-cert fingerprint +
invite secret**, so a client knows exactly which certificate to expect before it ever connects.
There is no trust-on-first-use window at all.

**This makes the invite blob the trust anchor, which moves the security requirement onto the channel
the invite travels over.** Whoever can substitute an invite can point an agent at a bus of their
choosing — and, because the fingerprint travels with it, do so without tripping any mismatch. That
is a genuine new requirement on invite distribution, not a footnote. It is still the right trade:
a TOFU window is exploitable by anyone on-path at exactly the moment a new agent joins, whereas
invite integrity is a problem the operator can reason about and control.

### E4 — The first invite is minted server-side

A server-side subcommand writing to the data dir. Authority is **filesystem access**, the same model
as `wal-mac.key`, and nothing new is exposed on the wire. Bootstrap therefore introduces no new
network-reachable privilege.

### E2 — `/v1/enroll` is settled ONCE in `ENROL-SHAPE`, and is UNSTABLE until all three land

Three separate changes rewrite the enrol wire shape: the invite field, the client-cert binding, and
`6e3083b0` (POPKEY). Settling the shape once and declaring the route explicitly unstable until all
three ship avoids three consecutive breaking changes to the same route, each stranding whatever was
built against the last.

### E3 — Rotation serves TWO certificates during rollover

The bus serves both the outgoing and incoming certificate during a rotation window, so clients
re-pin without downtime. **Rotation must never require every client to re-enrol** — that would make
routine key hygiene indistinguishable from a security incident, and would guarantee it is deferred
indefinitely.

### E5 — Revoking an invite does NOT affect an agent that already redeemed it

An invite is **spent at redemption**. Revocation prevents future use only. Cascading revocation —
"remove the agent this invite created" — is a different capability that needs AUTH-4's revocation
surface first, and conflating the two would give operators a false expectation of reach.

### E8 — Non-loopback binding is APPROVED, and must be CONFIGURABLE

Needed for the Docker Compose multi-bus relay target. The default stays loopback (invariant 11);
this is an explicit opt-in.

**Hard sequencing constraint, and the reason this is stated here rather than assumed:** until the
mTLS listener ships, **invite secrets cross the wire in cleartext**, bounded only by the loopback
default. **The bus must NOT be exposed on a non-loopback interface before mTLS lands.** The
configurability approved here creates exactly the flag that could violate that, so the ordering is a
requirement on the implementation, not a recommendation.

### E7 — STILL OPEN

Not answered: (a) is there genuinely no plaintext escape hatch for tests and local development, or
is one permitted; (b) may certificates be operator-supplied as well as self-generated? Invariant 11
currently says no plaintext anywhere; that stands until decided otherwise.

---

## 2026-08-02 — E7: no plaintext escape hatch; tests use REAL TLS; operator certs allowed

**Decision (user).** *"do the best thing for testing that keeps prod secure"* — delegated. Resolved
as follows.

### There is NO plaintext escape hatch. Not a flag, not an env var, not a build tag.

Invariant 11 stands unqualified. The reasoning is that every plausible hatch fails the same way:

- **A flag** (`-insecure`, `-no-tls`) is one typo or one copied command from being in production, and
  a bus started with it looks identical to one that was not.
- **An env var** is worse — it is invisible in the command line and inherited by children.
- **A build tag** seems safer but is not: a binary built with the wrong tag is
  indistinguishable from a correct one after the fact, and "which tags was this built with?" is
  exactly the question nobody can answer during an incident.

All three share the deeper flaw: **they make the tested path different from the production path.**
A test suite that runs without TLS does not exercise the TLS code, so the one configuration that
actually ships is the one least covered — and the failure surfaces in production, where certificate
and handshake bugs are hardest to diagnose.

### Tests use real TLS, with certificates minted at test time

Tests generate a genuine self-signed keypair into `t.TempDir()` (stdlib `crypto/x509` +
`tls.X509KeyPair`), start the real TLS listener, and connect with the real client pinning the real
fingerprint. The production trust path — fingerprint delivered in the invite, no TOFU — is what the
tests exercise, because it is the only path that exists.

**The accommodation that makes this stick is ergonomic, not a weakening:** ship a test helper that
mints a short-lived cert and returns a configured client in one call, so writing a TLS test is no
harder than writing a plaintext one. The reason people add insecure hatches is friction; removing the
friction removes the motive. If writing a TLS test is painful, that is a bug in the helper, not an
argument for a hatch.

**`InsecureSkipVerify` must appear nowhere in the tree**, including tests. Worth a grep in CI: it is
the single clearest signal that verification was disabled to make something pass.

> CORRECTED 2026-08-16 — SUPERSEDED by the 2026-08-07 MTLS-PIN entry §2 and by invariant 11 as it now stands.
The rule is not "nowhere" but EXACTLY ONE FILE, EXACTLY ONCE: `client/pin.go:260-261`, paired with
`VerifyPeerCertificate` in the SAME composite literal, enforced by an AST walk rather than a grep
(`client/guard_test.go`). A grep-based CI check would now be the WRONG check — it cannot see the
pairing, and the pairing is the part that carries the security property. Everything else in this
section is unchanged and current: no plaintext hatch anywhere, tests use real TLS, operator-supplied
certificates allowed.

### Operator-supplied certificates ARE allowed

`-tls-cert` / `-tls-key` flags, alongside the self-signed default. This is a legitimate need for
anyone with existing PKI, and it weakens nothing: self-signed-plus-pinning remains the default, and
the fingerprint-in-invite mechanism is indifferent to who issued the certificate — the client pins
whatever the bus actually serves.

### Known follow-on work

`scripts/bus-serve.sh:54` (`HEALTH_URL="http://${LISTEN}/healthz"`), `CLI-2`'s `proof_cmd` (enrols
over `http://127.0.0.1:8092`), and DEPLOY-1/DEPLOY-2 all assume plaintext and must be updated as the
mTLS epic lands.

> CORRECTED 2026-08-16: done. `scripts/bus-serve.sh:107` now reads `HEALTH_URL="https://${PROBE_ADDR}/healthz"`
and probes with `curl -fsS --cacert "$CERT_FILE"` (`:113`); the image and the compose file were
reworked by the 2026-08-15 DEPLOY-6 entries. `bus-serve.sh` is the only `scripts/bus-*.sh` file left.

---

## 2026-08-02 — DEPLOY-1/DEPLOY-2: container image base, digest pinning, and the loopback-only default

**Builder image pinned by TAG *and* DIGEST**, not `:latest` or a bare tag:
`golang:1.19.4-alpine@sha256:86d32cc0dfc04757fd8aeebb86308e6d1e3de60c73cb59e0f99c7b2ef77416b6`. A tag
alone can be re-pushed to point at different bytes; the digest is the only thing that actually pins
the toolchain the image builds with. Must be re-pinned when DEPLOY-4 bumps `go.mod`'s toolchain
version — this Dockerfile currently tracks `go 1.19` (go1.19.4, the ambient version on this box)
deliberately, per DEPLOY-1's description, and is NOT gated on the crypto/toolchain work.

**Runtime base: Alpine (`alpine:3.19.1`, also digest-pinned), not distroless.** Invariant 8
(stdlib-first / third-party deps need justification) is read here to cover base-image choice, not
just Go packages. `distroless/static` is smaller and shell-less, but DEPLOY-2 requires a working
`docker-compose.yml` healthcheck against the existing `/healthz` route, and distroless/static has
neither a shell nor an HTTP client to run one with — the alternative is shipping a second static
binary purpose-built to answer "is the server up." Alpine's busybox already provides `wget` (for the
healthcheck) and `adduser`/`addgroup` (for the non-root user), keeping both to a couple of `RUN`
lines. That is simple-beats-clever, not a shortcut around it: the image is ~19MB, a few MB over what
distroless/static would give, in exchange for meaningfully less machinery.

> CORRECTED 2026-08-16: the base-image CHOICE stands; the version and the healthcheck story have both moved.
The runtime stage is now `alpine:3.22.1@sha256:4bcff63911fcb…`, still digest-pinned
(`Dockerfile:102`); the builder is still `golang:1.19.4-alpine@sha256:86d32cc0…` (`Dockerfile:15`).
The busybox-`wget` justification is spent: the healthcheck is now the `agent-bus healthcheck`
subcommand on the server binary, because busybox `wget` cannot be told to trust one self-signed
certificate (2026-08-07, MTLS-LISTENER §5). The image also now ships `agent-busctl` and a
pre-created `/identity` (2026-08-15, DEPLOY-6 §3), and the builder copies `client/` as well as
`cmd/` and `internal/` (`Dockerfile:29`) — without which it did not build at all.

**Non-root user is a fixed, explicit UID/GID (`10001:10001`)**, not `adduser`'s next-available
default, so a data volume's on-disk ownership is stable and predictable across image rebuilds — an
operator inspecting the named volume from the host sees the same owner every time, and the ownership
is set on `/data` in the image *before* `VOLUME` is declared, which is what lets Docker seed a fresh
named volume with the right permissions on first use.

**The compose service is deliberately unreachable from outside its own container, by design, and
this is a real design decision, not an oversight left for later.** `docker-compose.yml`'s `command:`
repeats the binary's own loopback default (`-listen=127.0.0.1:8080`) explicitly rather than omitting
the flag, and no `ports:` section is declared. This is a direct reading of CLAUDE.md invariant 11 and
the explicit brief for this wave: *"The default listen address is 127.0.0.1:8080 (loopback), and the
bus MUST NOT be exposed on a non-loopback interface until mutual TLS ships."* Because each container
gets its own network namespace, a server that binds only its own loopback cannot be reached even by a
published port (Docker's port-publish path connects to the container's external interface, which
nothing here is listening on) — so the honest consequence, stated in the compose file itself, is that
no other container and no host process can talk to this bus as shipped. That is the intended trade for
this wave: DEPLOY-2's job is to prove the built image starts, reports healthy, and its data volume
survives a container replace (all three verified by execution — see AGENT_LOG.md), not to make the bus
reachable by other agents. Making it reachable is DEPLOY-3's job (a peered multi-bus profile), which
CLAUDE.md explicitly sequences *after* mutual TLS. The compose file documents an explicit,
loudly-commented opt-in override (`-listen=0.0.0.0:8080` plus a loopback-bound host-side `ports:`
entry) for an operator who wants to accept that risk locally, but that is not the default and must
never become the default before mTLS lands.

> CORRECTED 2026-08-16: still TRUE of `docker-compose.yml`, which keeps `-listen=127.0.0.1:8080` in its
`command:` and declares no `ports:` (`docker-compose.yml:116`; `restart: unless-stopped` at `:107`).
NO LONGER true of the IMAGE: the Dockerfile's `CMD` is `-listen=:8080`, changed deliberately on
2026-08-15 (DEPLOY-6 §2) because a loopback bind inside a container's OWN network namespace is
unreachable even through a published port — the bus started, passed its own healthcheck and was
reachable by nobody. The binary's `defaultListen` is unchanged at `127.0.0.1:8080`
(`cmd/agent-bus/main.go:41`). mTLS has since landed (2026-08-07, MTLS-LISTENER), so the sequencing
constraint this paragraph guards is discharged.

**Verified by execution, not just inspection** (both binaries invoked directly — see the AGENT_LOG.md
entry for why `docker` on PATH needed a workaround): `docker build` + `docker run -h` (proof-check
verdict PASS); `docker compose up -d` reaching `(healthy)` via `docker compose ps`; `/healthz`
answered from inside the container; the bus id surviving a real `docker compose down` (no `-v`)
followed by `up -d`, proving the named volume — not just the container — carries the durable state
across a replace, which is the whole point of invariants 4/5/6 reaching the deployment layer.

---

## 2026-08-02 — The client is `client/` + `cmd/busctl`: six decisions settling CLI-1/CLI-2

CLI-1 left exactly two questions open ("the exact package path and binary name") and CLI-2's
implementation forced four more. All six are recorded here because each one is cheap now and
expensive later.

### 1. The package is top-level `client/`; the binary is `cmd/busctl`

`github.com/dodgymike/agent-bus/client`, imported by `cmd/busctl`. The package is **not** under
`internal/` — that was already decided (invariant 7's third audience is an agent that EMBEDS the
client, and Go forbids another module importing an `internal/` path), and this only settles the
name. The binary is separate from `cmd/agent-bus` so the server image never ships the client, and
CLI-1's proof mechanically enforces the boundary:

```
! go list -deps ./cmd/busctl | grep -q 'agent-bus/internal/'
```

**Known collision, flagged not resolved:** `busctl` is also a systemd binary (the D-Bus
introspection tool) present on most Linux hosts, so `go install ./cmd/busctl` shadows it on `PATH`.
The directory name was assigned by the orchestrator for this wave and is kept; whether the INSTALLED
name should differ (`agent-busctl`, `abus`) is a naming question for the user, filed as a follow-up
rather than decided unilaterally. Nothing in the code depends on the binary's installed name.

### 2. Session tokens are NEVER written to disk

The Ed25519 private key is persisted (it is the identity). The session token is not.

It is a bearer credential, it lasts at most an hour, and it does not survive a bus restart, so
persisting it would trade "a stealable credential at rest, in a file that outlives the process" for
"two saved round trips per invocation". Each `busctl` process performs its own
begin/sign/complete handshake. `client.SessionInfo` therefore has **no token field at all** — not
even one tagged `json:"-"`, because a field that exists can still be reached by a reflection walk or
a struct copy into someone else's logging type.

### 4. Enrolment key material is persisted BEFORE the request, and idempotency records are scoped to (key, bus URL)

Found by smoke-testing the first implementation, which was wrong in a way no unit test of it would
have caught: `busctl enrol --idempotency-key K` run twice generated a **fresh key pair each time**,
so the second attempt sent the same key with a different `public_key`. Invariant 10 classifies that
as "same key + DIFFERENT payload" — a protocol violation for which the server returns 409 **and
disconnects the client**. The flag as originally shipped could only ever be used incorrectly.

Fixed by ordering the writes the way invariant 4 orders the server's:

- the seed is written to the store as a `pending` record **before** `/v1/enroll` is called, and
  promoted to a full credential in one locked read-modify-write when the bus answers;
- a retry with the same key **reuses that key material**, so the payload is byte-identical and the
  retry is legitimate;
- the accepted key is **kept on the credential**, so re-running a completed enrolment is answered
  from the store (`"replayed": true`) with no HTTP request at all;
- the same key with different content on the same bus is refused **locally**, because the bus's
  answer costs a connection and teaches the caller nothing the local message does not.

Records are scoped to **(idempotency key, bus URL)**, matching the server's own scoping: a second
bus has never seen the key, so presenting it there is a fresh enrolment, not a conflict.

This also closes a data-loss window that would otherwise have been documented and lived with: a
process killed between the bus minting an id and the local save used to lose the private key
permanently, leaving a roster slot that can never authenticate and cannot be cleaned up until
AUTH-4's revocation surface exists.

### 5. Exit codes are a contract, and the mapping lives in the PACKAGE

Nine codes (`0` ok, `1` internal, `2` usage, `3` config/identity, `4` auth, `5` network, `6` server,
`7` rejected, `8` nothing-to-report), enumerated in `CONTRACTS-CLI.md`.

> CORRECTED 2026-08-16: there are TEN. `ExitVersionSkew = 9` was added on 2026-08-08
(`client/errors.go:95-109`). The decision this section records is unchanged and still holds: the
mapping lives in the importable package (`client.ExitCode`), not in `cmd/`, and `client.Kind` is a
CLOSED set.

`2` is usage to match Go's `flag` package and `cmd/agent-bus`.

`client.ExitCode(err)` performs the mapping, in the importable package rather than in `cmd/`, so an
agent that embeds the client and re-exposes it as its own subprocess produces exactly the documented
codes without copying a switch statement that will drift. Same reasoning for `client.Kind`: the set
is CLOSED, and anything that does not fit is `internal` rather than a new member invented at a call
site.

### 6. The client transport sends nothing through a proxy

`http.Transport.Proxy` is `nil`, not `http.ProxyFromEnvironment`. Every request on this surface
carries either a bearer token or a signature over a server-chosen challenge, and a proxy terminates
the connection carrying it. Once certificates are pinned (invariant 11) a proxy would also present a
certificate the invite never named, so honouring `HTTP_PROXY` could only produce either a confusing
failure or a weakened check. A bus is reached directly.

`newHTTPClient` is the single seam where any transport or `tls.Config` is constructed, so invariant
11's pinning lands there and nowhere else. `InsecureSkipVerify` is not set, is not reachable through
`client.Config`, and a test asserts it appears in no `.go` file under `client/` or `cmd/busctl/`.

**Consequence for `Config.HTTPClient`:** supplying one bypasses that seam, and therefore bypasses
pinning. It is kept as the escape hatch for embedders who already own an instrumented transport, and
documented as such — it is explicitly not a supported way to relax verification, and no `Config`
field will ever be added that is.

## 2026-08-02 — MSG/POLL: the messaging core (four decisions, two of them narrowings)

The wave that turned a durable store into a message bus: `GET /v1/agents`, `POST /v1/broadcast`,
`POST /v1/send`, `GET /v1/messages`, `GET /v1/wait`. Three of the four items below exist because a
gate found something; they are recorded here rather than in a comment because two of them narrow a
standing invariant, which CLAUDE.md requires be dated and argued in this file.

### 1. Delivery is AT-LEAST-ONCE, over ONE ordered stream with a per-agent cursor

Not per-agent queues. A broadcast into N queues has to be copied N times, an agent that enrols later
cannot read back through the retention window, and "one broadcast wakes every eligible waiter exactly
once" then depends on N queues agreeing rather than on one order everyone reads with their own
cursor. One stream plus a cursor is the simpler construction (invariant 8) and it is what makes the
delivery guarantee statable at all.

The cursor is opaque, versioned, bound to the agent it was issued to — and **deliberately not
signed**. Forging one for yourself replays or skips your own messages, which at-least-once already
permits and which is self-inflicted either way; forging one for another agent gains nothing, because
visibility is filtered with the AUTHENTICATED PRINCIPAL and the filter never consults the cursor. A
MAC would protect a value whose integrity buys no security property, at the cost of a key to manage
and rotate. Invariants 8 and 9 both point away from adding it.

> CORRECTED 2026-08-16: the one-ordered-stream-plus-cursor shape stands, and so does every word about the
cursor being opaque, versioned, agent-bound and deliberately unsigned. What changed is WHAT the
cursor names. Since 2026-08-14 (SIGN-1-FU-REORDER-WATERMARK) it names a delivery POSITION — the WAL
commit index — not a sequence, and `cursorVersion` is `"v2"` (`internal/hub/cursor.go:53`); a `v1`
cursor is remapped to position 0 and never rejected (`:165-178`).

### 2. The ENROLMENT EPOCH — a NEW restriction, added because this wave opened a hole

**Decision: a message sent before an agent's own enrolment is never delivered to it.**

The security gate found the hole, and it is worth stating plainly because it is a case of one epic
invalidating another's written justification. `cmd/agent-bus/main.go` allocates agent-id suffixes
from a FRESH counter every start, justified by "nothing in this path writes an agent id to disk".
That was true when it was written. It stopped being true in this wave: `store.Record` persists
`sender` and `recipients` as fully-qualified agent ids, `hub.publish` writes them through the WAL,
`hub.Apply` replays them, and the WAL never compacts. So after a restart the counter restarts at 1,
and anyone who reaches the (still unauthenticated) `/v1/enroll` and guesses the name `alpha` is
minted `<bus>.alpha-1` — the id the previous alpha held — and would have read a full retention
window of that agent's direct messages.

The bus cannot tell those two agents apart BY ID, because an id is exactly what is being reused. It
can tell them apart BY TIME. No legitimate agent needs traffic that predates its own enrolment, and
after a restart every enrolment is newer than every recovered message, so the hole closes at no cost
to a correct client.

Three things about this were deliberate:

- **It is not a temporary patch to be undone.** Once AUTH-3 makes enrolment durable, the roster
  restores each agent's ORIGINAL enrolment instant, and a genuinely continuous agent keeps seeing
  everything sent since it enrolled. The rule is correct in both worlds.
- **It does not fix identity CONTINUITY, and is not claimed to.** The new holder of a reused id
  reads none of the old traffic, but its FUTURE messages are attributed to an id with a prior
  history. `hub.NoteEnrolment` logs that at ERROR — silence would be the defect — and
  `MSG-FU-SUFFIXFLOOR` carries the real fix.
- **The read paths FAIL CLOSED for an unknown agent** (403, not an empty batch). A zero epoch
  disables the check, so an unknown reader read with no epoch would be served EVERYTHING rather than
  nothing. The error and the permissive default must not be the same value.

Proved on a running server, not just in tests: seed a DM from alpha to beta, restart, enrol the name
`beta` with a different keypair, get the id `<bus>.beta-1` back, and read 0 messages — while the log
reports the message store rebuilt with that message still in it. Not delivered, not lost.

### 3. Idempotency-key retention is the MESSAGE retention window — a narrowing of item 9 above

Item 9 of the 2026-08-02 CLI decisions says keys "are retained for a bounded window and fail closed —
a retry arriving after the window is rejected, never silently re-applied". The messaging path honours
the half that carries the weight and narrows the other half, on purpose:

- **HONOURED: never evict under pressure.** At `hub.MaxIdempotencyEntries` the send is REFUSED (503),
  never accepted by making room. Evicting a remembered key turns the next retry of it into a second
  message, and a refused send is recoverable where a duplicated one is not. Same posture as
  `auth.recordIdempotent`.
- **NARROWED: a key expires with its message.** A retry arriving after the retention window (a day)
  is treated as a fresh send and produces a second message rather than being rejected. Rejecting it
  would mean remembering every key ever used, for ever — the unbounded growth the cap exists to
  prevent — since a key that has been forgotten is indistinguishable from one never seen.

The window is orders of magnitude beyond any plausible client retry, and tying key lifetime to
message lifetime means the two cannot drift apart. `IDEM-11` owns the cross-cutting layer and may
revisit this; until then `CONTRACTS-HTTP.md` states the narrowed behaviour explicitly rather than
letting the stricter sentence stand while the code does something else.

## 2026-08-02 — Addendum to the CLI decisions: four more, from the reviewer and security gates

The gates on CLI-1/CLI-2 returned CHANGES-REQUESTED with findings that forced four further
decisions and one correction. Recorded here rather than folded into the section above, which is
already dated and appended.

### A. Plaintext `http` is permitted ONLY to a loopback host

Invariant 11 says TLS is required and there is no plaintext listener. The bus does not serve TLS
yet, so a client that refused `http` outright would not work at all — but accepting it unrestricted
was worse than it looked: `/v1/session/begin` returns the session token **in the response body**,
and that token is a **bearer credential**. `busctl whoami --verify --bus http://host:8080` put a
live credential on the wire in clear, in both directions.

So `parseBusURL` rejects plaintext to a non-loopback host and permits it to `127.0.0.1`, `::1` and
`localhost`. That is the CLIENT-SIDE half of E8's sequencing constraint ("the bus must NOT be
exposed on a non-loopback interface before mTLS lands"), and the whole case is deleted when the TLS
listener ships. Loopback detection is deliberately narrow — literal addresses and the exact name
`localhost`, with no DNS resolution, because a security check that depends on a resolver is a check
an attacker can move.

### B. Redirects are never followed

`CheckRedirect` returns `http.ErrUseLastResponse`. Go's default policy copies the `Authorization`
header across a redirect whenever the target's canonical address matches — and `canonicalAddr`
includes the port, so `https://bus:8080` → `http://bus:8080` compares EQUAL and the bearer token is
forwarded in clear. This API never legitimately redirects, so refusing costs nothing and closes a
credential-downgrade path before there is a caller to walk into it.

### C. The bus is NOT trusted to produce safe text

Two different treatments, and the split is the decision:

- **Free text is SANITISED.** The bus's `{"error":"…"}` string was being rendered verbatim to a
  terminal. A hostile bus — and invariant 11's threat model explicitly includes being pointed at
  one, since whoever substitutes an invite chooses the bus — could emit unbounded ESC, CR and BEL:
  erase the line, print a fabricated `enrolled as bus1.admin`, set the window title, or write the
  clipboard where OSC 52 is enabled. `safeText` replaces C0, DEL and C1 controls and truncates on a
  rune boundary. JSON output was never affected (`encoding/json` escapes everything below 0x20), so
  this was specifically a human-terminal hazard — and a PERSISTENT one, because the same fields are
  stored and reprinted by every later command.
- **Id-like fields are REJECTED, not sanitised.** `agent_id`, `bus_id` and `name` are checked
  against `[A-Za-z0-9._-]{1,256}` and a violation fails the enrolment. Invariant 1 makes the server
  authoritative on ids; it does not make them unvalidated input. Rewriting an id would leave the
  local store disagreeing with the bus about who we are, which is worse than refusing.

  The pattern is deliberately BROADER than the real `<bus-id>.<name>-<n>` grammar. This is a safety
  check, not a second implementation of the server's id rules — a client that re-derived the grammar
  would reject a legitimate future format, and invariant 1 is precisely the instruction not to keep
  a competing copy of it.

### D. Correction to §4: "closes the data-loss window" was true only where something was printed

The claim as written was too strong. Persisting the seed before the request does keep the key
material on disk, but until this addendum the idempotency key that reaches it was revealed only in
the remedy of a *graceful* failure. A process killed between the write and the answer had printed
nothing, so the seed sat in the store permanently unreachable while the bus held a roster slot
nobody could authenticate as — safely stored, and useless.

Two changes make the claim true: `EnrolResult` now REPORTS the idempotency key, and
`whoami --all` lists every unfinished enrolment with the exact command that resumes it. Recorded
because "the key is on disk" and "the identity is recoverable" are different properties, and
conflating them is how a durability claim ends up overstated.

Also corrected in the same pass, all from the same principle — a promise the code did not keep:

- `logout --all` said it destroys the private keys but left `pending` records untouched, so a seed
  for an enrolment the bus may well have applied survived a wipe the operator believed complete.
- The 24h pending TTL was enforced only inside `AddPending`, so a store that never enrolled again
  kept key material forever. Pruning now runs on every write.
- Abandoned `identities.json.tmp-*` files — each a complete copy of every private key — were never
  swept.
- The store lock's stale-break could delete a LIVE holder's lock, losing a whole-file update and
  therefore a private key. Locks now carry an ownership token and both break and release are
  conditional on it.
- The check-then-act around the idempotency key was outside the lock: two concurrent enrolments
  under one key generated different key pairs, one seed was overwritten, and both sent conflicting
  payloads under the same key. `ClaimEnrolment` makes the decision in one locked read-modify-write.

### Known follow-up, not fixed here

The transport is constructed in `client.New`, before any credential is resolved. Invariant 11's
pinning needs a **per-identity client certificate** and a **per-bus fingerprint**, neither of which
is a function of `Config` alone — so the seam is in the right place but at the wrong TIME, and the
TLS task will need to build it lazily or key a small cache by `(agentID, busURL)`. Written down now
because the person doing that work should not have to rediscover it.

## 2026-08-07 — The applied-key table gets a per-agent fair share, enforced only under pressure (IDEM-11-FU-FAIRSHARE)

A security-gate P1 raised on the IDEM-11 wave: the applied-key table (`internal/idem`) was bounded
ONLY bus-wide — `idem.MaxEntries` = 65536, fail-closed, nothing ever evicted. One authenticated agent,
buggy or hostile, could fill it, after which EVERY other agent's mutating operations were refused with
`ErrCapacity` for up to the full `RetentionWindow` (50h10m22s) — even agents holding zero keys of their
own — because entries are evicted by age alone, never under pressure. Idempotency exists so a
well-behaved client can retry safely (invariant 10); a bound that lets one client revoke that safety
from everybody else defeats the invariant it was written to serve.

**The fix:** a per-agent fair share, enforced only once the table crosses `idem.PressureLine`
(`maxEntries/2` — the crossover where free space stops exceeding used space). Above the line, an
agent may hold at most `maxEntries/(agents+1)` records, where `agents` is the count of distinct agents
currently holding at least one retained record; at its share, an admission attempt is refused with
`idem.ErrAgentQuota` (`hub.ErrAgentQuota` at the hub layer). Below the line, nothing changes — a bus
that never approaches its cap sees no behaviour difference at all. See `internal/idem/retention.go`
for the full term-by-term derivation and `CONTRACTS-ONDISK.md`'s IDEM-11-FU-FAIRSHARE section for the
shipped contract.

**Why the divisor is `agents + 1` and not `agents`.** The `+1` is the agent that has not arrived yet,
and it is load-bearing, not a safety margin. With a divisor of `agents`, a lone agent's share is the
whole table (`maxEntries/1`), so the exact attack in the finding — one agent, acting alone, filling
everything before any victim holds a single record — passes straight through and the rule buys
nothing. The victim cannot be counted in the divisor precisely because it holds nothing, which is the
condition of being starved; a bucket that only counts agents already holding records is blind to the
agent being denied its first one. The phantom slot is what reserves room for it before it exists.

**Why it fails CLOSED and evicts nothing**, rather than reclaiming the oldest record to make room: the
same reason the bus-wide cap already takes this posture. Evicting a live key silently turns that key's
next legitimate retry into a SECOND effect — the double-apply invariant 10 exists to prevent — and it
does so quietly, to the client that is behaving correctly by retrying. A refused operation is
recoverable and loud; a duplicated one is neither recoverable nor visible to the client it happened to.
The alternative posture (evict-oldest) was considered and rejected on this basis alone.

**Why the bucket is keyed on the agent id, and why that is safe HERE specifically.** A `Record` exists
only because an authenticated, server-minted, fully-qualified `<bus-id>.<agent-id>` (invariant 2)
performed a mutating operation — the bucket key is therefore a PROVEN identity, not an attacker-chosen
label. A flooder cannot make its keys land in a victim's bucket; it can only fill its own, so a refusal
at the share is always self-inflicted. This project got the opposite wrong once already:
`auth.BeginSession`'s removed `MaxPendingPerAgent` cap was keyed on an `agentID` an UNAUTHENTICATED
caller supplies at enrolment time, which turned it into a targeted denial of service against any named
agent (see `internal/auth/session.go`'s "There is deliberately NO per-agent cap" note). The two places
this project got it right beforehand — `hub.Wait`'s `MaxWaitersPerAgent` and `auth.CompleteSession`'s
per-agent active-session cap, both keyed on ids proven by a live session or an Ed25519 signature — are
the model this rule follows, not a coincidence of style.

**Why the replay path is exempt.** The fair share is a LIVE ADMISSION policy — a decision about
whether to accept an operation that has not happened yet — never a property of a stored record.
`hub.Apply` calls `idem.Store.Recover`, which is `Store.Remember` minus exactly the per-agent check (it
still enforces the bus-wide cap and still validates the record). A record on disk is proof that
admission ALREADY succeeded; re-testing it at replay can only ever disagree with a decision already
acted on, and the disagreement is not a stricter bus, it is a LOST key whose next retry becomes a
second message — the exact failure this rule exists to prevent, reintroduced by the mechanism meant to
prevent it. Two concrete triggers, not a theoretical one: a backwards clock makes the replayed retained
set a SUPERSET of what was live (safe for expiry's own predicate, unsafe for admission), and a log
written BEFORE this change can legitimately hold one agent above the new share, so the first restart
after the upgrade would otherwise drop that agent's already-accepted keys. Covered by
`TestReplayNeverRefusesWhatTheLivePathAccepted`.

**The accepted cost.** A SOLE agent on a bus can now hold at most `maxEntries/2` = 32768 applied keys
instead of 65536, halving its sustained throughput ceiling (~0.36 -> ~0.18 accepted mutating ops/sec,
sustained over the retention window). That halving is the price of the guarantee and is judged worth
it: the alternative is one agent being able to deny idempotency to the whole bus for up to 50 hours.
The general sustained-ceiling concern is already tracked by IDEM-11-FU-THROUGHPUT.

**Two residual surfaces, recorded by the security gate as follow-ups and NOT fixed here:**

- The divisor counts every distinct agent holding a record, so many cheap identities shrink everyone's
  share — this rule mitigates one agent starving others, it does not bound how many agents can exist.
  The root fix is enrolment authentication (INVITE-GATE); enrolment is unauthenticated today.

  > CORRECTED 2026-08-16: enrolment is NO LONGER unauthenticated. `enrolmentInviteRequired = true`
  (`cmd/agent-bus/main.go:66`, shipped `3cedcb7`, 2026-08-15), so an un-invited `POST /v1/enroll` is
  refused 403 — the root fix this residual named has landed. A second bound landed on 2026-08-15
  (RELAY-FU-IDEM-METER-BY-PEER): a relaying peer controls the origin-agent label, so foreign agents are
  now charged to their BUS half rather than to a peer-chosen per-agent label, and the store requires
  its local bus id at construction (`internal/idem/store.go:186-235`). The constants are unchanged:
  `MaxEntries` 65536, `PressureLine` 32768 (`internal/idem/retention.go:152,240`), `ErrAgentQuota`
  (`internal/idem/errors.go:88`), replay exempt via `Store.Recover` (`internal/idem/store.go:358`).
- Below the pressure line, admission is first-come first-served with no reclamation: an agent that
  grew its holding during the free-growth phase keeps that outsized allocation even after the bus
  crosses into pressure, because the share only ever REFUSES new admissions, it never claws back what
  is already held.

Not deployed: this is a code-only change to `internal/idem` and `internal/hub`, uncommitted at the
time this decision was recorded.

---

## 2026-08-07 — ENROL-SHAPE: the final `/v1/enroll` shape and `auth.RosterEntry` field set

Settles the P0 that blocked `AUTH-3`, `INVITE-STORE`, `INVITE-GATE`, `MTLS-BIND` and
`AUTH-1-FU-POPKEY`. **Deliverable is this entry only** — `CONTRACTS-HTTP.md` documents SHIPPED
behaviour and none of this has shipped.

> CORRECTED 2026-08-16: it has ALL shipped, and shipped exactly as specified — which is the outcome this entry
was written to buy. `auth.RosterEntry` now carries `AgentID`, `Name`, `AuthPublicKey`,
`MessagingPublicKey`, `InviteID`, `Epoch`, `CertBindings []CertBinding` and `EnrolledAt`
(`internal/auth/roster.go:88-171`); `CertBinding` is `{Fingerprint [32]byte; BoundAt time.Time;
RetiredAt *time.Time}` (`:42-60`); the history is BOUNDED at `MaxCertBindings = 16` (`:32`, enforced
`:397`). The "no migration required if this lands before AUTH-3" window closed as intended: nothing
had to be migrated.

**Why one entry instead of three changes.** Three separately-filed tasks each rewrite `POST
/v1/enroll`'s request body — the invite field, the client-cert fingerprint binding, and
proof-of-possession. Landing them independently revises the same contract three times. Worse, the
roster, sessions and the idempotency table are ALL in-memory today (`MemoryRoster`), so there is
currently **nothing persisted to migrate** — and that window closes the moment `AUTH-3` makes the
roster durable. Deciding the whole shape now costs a document; deciding it after `AUTH-3` costs three
migrations and a forced re-enrolment of every agent.

### The final `auth.RosterEntry` field set

Today: `AgentID`, `Name`, `PublicKey`, `EnrolledAt`. The target adds four and renames one.

- **`AuthPublicKey`** — renamed from `PublicKey`. With a messaging key alongside it, `PublicKey` no
  longer says which key it is, and this codebase has repeatedly been bitten by names that quietly
  stopped meaning what they said. The rename is free today and a breaking change once anything
  persists it.
- **`MessagingPublicKey`** — the second Ed25519 key. `SIGN` needs it, and auth/messaging separation
  is already an invariant; storing them in one field would collapse a distinction the design depends
  on.
- **`InviteID`** — which invite this enrolment redeemed. This is provenance: it answers "who
  authorised this agent onto the bus", and without it revocation and audit have nothing to join on.
- **`Epoch`** — the enrolment epoch the hub already uses for the id-reuse fix. Currently derived;
  storing it means the durable record carries it rather than reconstructing it on every boot.
- **`CertBindings []CertBinding`** — a HISTORY, not a single fingerprint (see below).

### Certificate bindings are a bounded history

`CertBinding` = `{ Fingerprint [32]byte; BoundAt time.Time; RetiredAt *time.Time }`, where the
fingerprint is `sha256.Sum256(cert.Raw)` — named explicitly so nobody invents a different one.

A single current-fingerprint field would make a rotating agent look, for the duration of the
rotation, like a *different agent* — which is precisely what invariant 1 says an id must never
become. The bus already serves two certificates during its own rollover (2026-08-02); this is the
client-side mirror of that decision, and the two should behave the same way or rotation is only half
solved.

Three rules, because a history is a place where bugs hide:

1. **The history is BOUNDED.** An unbounded per-agent list is the same class of defect as the
   unbounded applied-key table, and it is reachable by an agent that rotates in a loop.
2. **Verification accepts any binding that is not retired.** Not "the newest" — during a rollover
   both are legitimately live.
3. **Retirement is EXPLICIT, never implicit-by-supersession.** An operator must be able to see when
   an old certificate stopped being accepted. A binding that silently ages out is indistinguishable
   from one that was revoked, and those need different responses.

### Ordering rule

**`AUTH-3` must encode this FULL field set, including fields nothing populates yet.** Writing the
durable record once with reserved-but-empty fields is far cheaper than migrating it three times, and
it is the whole reason `AUTH-3` was blocked rather than merely deprioritised.

### Migration

**None required, if this lands before `AUTH-3`.** Nothing is persisted today. That is the entire
value of settling it now, and it evaporates the moment durable enrolment ships.

---

## 2026-08-07 — DEPLOY-2-FU-CONTAINERNAME: the working docker invocation for agent shells on this box

**Decision.** Record the concrete workaround for the previously-filed environment defect (`637fca2f`,
"docker CLI unusable for agents") rather than leave it only in an agent's transcript, because every
future docker-based `proof_cmd` on this box — including all of `DEPLOY-3` — depends on it.

**The recipe:** use the real snap binary directly, not the broken PATH wrapper, and point it at the
daemon socket explicitly:
```
DOCKER_HOST=unix:///run/docker.sock /snap/docker/current/bin/docker ...
DOCKER_HOST=unix:///run/docker.sock /snap/docker/3505/usr/libexec/docker/cli-plugins/docker-compose ...
```
`/snap/bin/docker` (what's on PATH by default) fails with `cannot create user data directory:
/home/mike/snap/docker/3505: Not a directory`, because `$HOME` (`/home/mike`) is a symlink to
`/mnt/sdb4/mike/mike` and the snap's confinement does not resolve through it.

> UNVERIFIED 2026-08-16: PARTLY confirmed by inspection, NOT by execution — this pass is read-only
> and did not run `docker`. Confirmed: `/snap/docker/current/bin/docker` exists and is a real 42 MB
> binary, and `/snap/bin/docker` is a symlink to `/usr/bin/snap` rather than a docker binary, which
> is consistent with the failure described. NOT confirmed: that the failure still reproduces with
> that exact message. AND ONE DETAIL IS LIKELY STALE — the compose plugin path hard-codes snap
> revision `3505`, but **revision `3579` is now also installed** (`/snap/docker/3505/`,
> `/snap/docker/3579/`, `/snap/docker/current/`). The `docker` line uses the revision-independent
> `current` symlink; the `docker-compose` line does not, so it may point at a superseded revision.
> Prefer `/snap/docker/current/usr/libexec/docker/cli-plugins/docker-compose` and verify before
> quoting this recipe in a `proof_cmd`.

Going straight at the already-running daemon over its Unix socket sidesteps that entirely — the
CLI's per-user data directory is only needed for `docker context`/config bookkeeping the socket path
bypasses.

**Rationale for recording this as a decision, not just a log line:** `637fca2f` was filed 2026-08-02
as "NEEDS THE USER (environment change, outside an agent's remit)". This session (2026-08-07) found
that framing was too pessimistic — no environment change was needed, only the right binary + socket
path — and used it to bring up two `agent-bus` compose instances simultaneously and pass
`DEPLOY-2-FU-CONTAINERNAME`'s proof (see `AGENT_LOG.md`, same date). Any future task whose `proof_cmd`
shells out to `docker`/`docker compose` should use this recipe before reporting UNVERIFIABLE.

**Not decided here:** whether to bake this into `scripts/proof-check.sh` or a wrapper (e.g. a
`scripts/bus-docker.sh` shim that resolves the right binary/socket automatically) so agents don't have
to rediscover it by hand each time. Left as a candidate follow-up for whoever next touches
`637fca2f` or picks up `DEPLOY-3`.

---

## 2026-08-07 — MSG-FU-SUFFIXFLOOR: the per-name agent-id suffix floor is a dedicated write-ahead
file, not derived from WAL replay

**Decision.** The per-name agent-id suffix high-water mark (what the next `enrol "alpha"` on this bus
must mint above) is persisted in its own atomically-replaced, fsynced file
(`<data-dir>/agent-suffixes`, format version **3**, reserved through the Spec Server
`ondisk-format-version` namespace 2026-08-07 by feature-runner — values 1 and 2 are the WAL's, never
picked by eyeballing the list) rather than derived from folding WAL replay or the enrolled roster.
`ids.DurableNameSuffixes` (`internal/ids/suffixstore.go`) writes `floor[name] = n`, fsynced, BEFORE
`NextSuffix` returns `n` to any caller — so the floor is durable ahead of the suffix it authorises,
and no derivation step is needed to resume correctly.

**Rationale, from `ID2_WIRING_DEEPDIVE.md`.** A floor derived from **committed** WAL history is wrong
on its own: a suffix burned by a dangling PREPARE that never committed is invisible to a fold over
committed records, yet the number still reached disk and may already have been told to a client. A
floor derived from the **roster** is wrong for a different reason: a departed agent's burned suffix
disappears from the roster the moment it leaves, so the next agent to request that name would be
minted straight over a departed agent's old id. Both derivations are the "obvious wiring" `NameSuffixes`'
own doc (`internal/ids/agentmint.go`) already names as the trap; this decision sidesteps needing either
one to be right, because the floor is no longer *derived* from anything — it is *written first*.

The prepare-observer work that would let a derivation see dangling prepares
(`ID-2-WIRING-OBSERVER`) is **not implemented**, which is precisely why derivation was the wrong tool
here: there was no honest way to derive a correct floor from history alone without it, and this
decision removes the dependency rather than waiting on it.

**Because the floors are not in the WAL, no tail repair or log quarantine can ever rewind them.**
`wal.RepairTail`/`RepairLog` operate on the WAL only; the suffix floors live in a file that discipline
never touches, so no amount of WAL damage or the "always restart" recovery policy (§6,
`PROTOCOL.md`) can lower a floor that was never stored there in the first place. This makes the
"never rewind a floor" property structural rather than a rule the recovery path has to remember.

**Rejected alternatives.**

- **(a) Derive from `wal.Replay`'s committed fold.** Wrong per the rationale above — misses dangling
  prepares. This is the FIX paragraph the original MSG-FU-SUFFIXFLOOR task description prescribed,
  and it is deliberately NOT what shipped; see the residual and scope notes below for why.
- **(b) Put the high-water mark in the WAL prepare header itself.** Rejected because it forces a WAL
  format-version bump (the frame header in `PROTOCOL.md` §3.2 is not free to grow) and a downgrade
  break for every deployed data directory, for a property a dedicated file gets more cheaply and
  without touching the WAL's stable format at all.

**The residual, stated honestly rather than glossed.** For a data directory that predates this file —
one that already holds agent ids on disk (WAL message bodies, as senders and recipients) and has no
`agent-suffixes` file — `OpenNameSuffixes` returns an allocator with empty floors and `Existed() ==
false`. The caller MUST back-fill those floors through `RaiseFloor` before `Seal`, and that
back-fill derivation is **exactly** the derivation this decision otherwise avoids needing — which
still cannot see a suffix burned by a dangling prepare, because the prepare-observer work named above
is not implemented. So on a **legacy** data directory the guarantee this file can honestly make is
"no suffix that reached COMMITTED history is reissued", not the full "no suffix that ever reached
disk is reissued". That gap closes for good once a directory has been served by `DurableNameSuffixes`
from the start (no derivation, no gap — the floor is always written ahead) or once
`ID-2-WIRING-OBSERVER` lands and the backfill derivation can see dangling prepares too. It is a
migration-window limitation, not a permanent one.

**Scope of what shipped, and what did not.** This decision and the file/API it describes are
implemented entirely inside `internal/ids` (`OpenNameSuffixes`, `DurableNameSuffixes.RaiseFloor` /
`Seal` / `NextSuffix`, `ErrSuffixFileCorrupt` — see `CONTRACTS.md`, 2026-08-07 entry, for the full
Go surface). `cmd/agent-bus/main.go:327` still calls `ids.NewNameSuffixes()`, and there are **zero
production callers of `OpenNameSuffixes`** anywhere in the tree — a restarting bus therefore still
re-mints agent ids today. Wiring `main.go` to this allocator (deriving legacy-dir backfill floors,
calling `RaiseFloor` over them, then `Seal` exactly once, the same shape `internal/hub` already
follows for `Sequence`) is a separate, not-yet-completed piece of work. Do not read this decision as
evidence the restart-reuse bug is fixed in a running bus; see `AGENT_LOG.md` (2026-08-07) for the full
review-chain record of that gap.

> CORRECTED 2026-08-16: the wiring landed the same day — see "MSG-FU-SUFFIXFLOOR (wiring)" below. The gap this
paragraph describes is CLOSED: `ids.NewNameSuffixes` no longer appears in `cmd/` at all,
`cmd/agent-bus/suffixfloors.go:137-143` constructs through `ids.OpenNameSuffixes`, and
`TestNoFreshSuffixCounterInCmd` (`cmd/agent-bus/suffixfloors_test.go:685`) is an AST guard that keeps
it that way. A restarting bus does NOT re-mint agent ids.

The OTHER caveat in this entry is still true, and permanently so: the prepare-observer work
(`ID-2-WIRING-OBSERVER`) was never implemented — there is no `ReplayWithPrepares` in `internal/wal`,
only `wal.Replay(path, fn)` (`internal/wal/replay.go:109`) — so on a LEGACY data directory the
backfill still cannot see a suffix burned by a dangling prepare, and the honest guarantee there
remains "no suffix that reached COMMITTED history is reissued".

---

## 2026-08-07 — INVITE-STORE: the idempotency scope for enrolment is THE INVITE

Invariant 10 requires every mutating operation to carry a client idempotency key scoped to a durable
namespace of previously-applied keys. Redemption of an invite is a mutating operation — it mints an
agent id — and it happens on `POST /v1/enroll`, the one route that is by definition reached before
any credential exists. This entry records the scope `internal/invite` was built to, so nobody
re-litigates it when `INVITE-GATE` wires the route.

### The scope is `(invite id, client idempotency key)`

Neither of the two obvious alternatives works:

- **PER-AGENT is impossible.** Enrolment is what MINTS the identity. There is no authenticated agent
  id at the moment the idempotency key is presented — the whole point of the call is to produce one.
- **BUS-WIDE is unsafe on an unauthenticated route.** A prior review of `idem`'s own bus-wide enrol
  scope flagged the exact risk: any caller, holding no invite and no credential at all, could squat a
  key ahead of a legitimate retry, because nothing about the route ties a key to a caller who is
  entitled to use it.

**The invite is the right namespace** because it is server-minted, single-use, and gated by a bearer
secret — only the holder of that secret can write into it, so the namespace inherits the invite's own
admission control for free. And because an invite is single-use it holds **at most one** redemption
record, so this namespace has no table to exhaust and none of the capacity concerns a per-key table
(`idem.Store`'s 65536-entry, 64 MiB bound) carries: there is nothing here for `IDEM-11-FU-FAIRSHARE`'s
class of attack to target, because filling it costs exactly one invite.

### The triage this scope produces (invariant 10 — not collapsed)

`Store.Begin`'s ordering is the authoritative statement of this and is quoted here rather than
re-derived, because re-deriving it in a second place is how the two go on to disagree:

```
correct secret, same key, same fingerprint      -> OutcomeReplay: the ORIGINAL result, verbatim
correct secret, same key, DIFFERENT fingerprint -> ErrKeyReuse: reject, log, DISCONNECT
correct secret, different key                   -> ErrAlreadyRedeemed: single use is spent
wrong secret or unknown id                       -> ErrUnknownInvite
```

The secret is verified FIRST, before any of the above is reported, so a caller holding no secret
learns nothing about whether an invite id exists or what state it is in — an unknown id is compared
against a per-store dummy digest so the work is identical either way. Once a caller has proved it
holds the secret, `ErrAlreadyRedeemed` and `ErrKeyReuse` are kept as DISTINCT sentinels rather than
one collapsed error, because they demand opposite reactions: one says "you are too late", the other
says "you are misbehaving", and collapsing them would make a legitimate retry indistinguishable from
an attack in the server's own logs. (`errors.go` requires the HTTP layer to collapse these on the
WIRE regardless — a client must not learn which of the four cases it hit — but the server-side
sentinels stay separate so an operator still can.)

### Second decision: redemption is a TWO-PHASE PARTICIPANT, not a one-shot

Redemption must be ATOMIC with the effect it authorises — the roster write that creates the agent. A
`wal.Entry` is exactly one transaction, so "atomic" means the consumption record and the roster
record have to ride in the SAME entry, composed by `INVITE-GATE`/`AUTH-3`. `internal/invite` therefore
exposes `Store.Begin -> Redemption.Consume -> the caller's write -> Commit/Abort`, not a single
`Redeem` call. `Store.Redeem` (the standalone one-shot path) exists so the package is complete and
provable on its own today, and its own doc says plainly that `INVITE-GATE` must NOT use it: splitting
the consumption record from the roster write reopens exactly the window the participant API exists to
close — a crash between the two would leave either an agent enrolled against an invite that is still
open, or an invite spent on an enrolment that never happened.

**A reservation is NOT sweepable after `Consume`.** Before `Consume`, `Store.sweepLocked`'s
`ReservationTTL` (30s) reaper may reclaim an abandoned reservation and reopen the invite. After
`Consume`, the caller may already have a durable consumption record built (though not yet committed),
so reaping the reservation back to open would let a SECOND redemption in while the eventual commit
still says the invite is spent. From that point only `Commit` or `Abort` resolves it, and an
abandoned one stays locked until restart — fail-closed, on purpose.

**A reservation must NOT be aborted on `wal.ErrDiverged`.** `wal.Txn.Commit` returns `ErrDiverged`
AFTER the commit record has been appended and fsynced (`internal/wal/log.go`): by the time a caller
sees that error, the invite is already durably SPENT on disk, and the failure belongs to a
neighbouring applier, not to this write. `Store.Redeem` states the rule its callers must inherit
verbatim: releasing the reservation on `ErrDiverged` would leave memory saying OPEN while disk says
REDEEMED, and the next `Begin` would admit a SECOND redemption of a spent invite — the one outcome
this whole package exists to prevent. So on `ErrDiverged` the `Redemption` is ABANDONED, still
holding the invite, and a restart rebuilds the correct (spent) state from the durable log.
`INVITE-GATE` must apply this same rule when it composes its own entry, not just `Store.Redeem`.

---

## 2026-08-07 — SUPERSEDES two earlier passages: no sequence reuse, and no refuse-to-start

`DECISIONS.md` contained two passages that contradicted each other AND contradicted the user's later
ruling. A triage pass found both. Recording the resolution here rather than editing them, because
this file is append-only.

**The two superseded passages:**

> NOTE 2026-08-16: both target passages have now been REMOVED from this document rather than
left in place, under an explicit operator instruction authorising removal — see the terminal
"Removed on 2026-08-16" section. Passage 1 was the whole of the "Addendum to ID-2-WIRING-SCHEMA"
entry; passage 2 was §4 of the "MSG/POLL" entry. The line numbers quoted below are historical and no
longer resolve. This entry is KEPT because it is the record of WHY they went and is cited by task
records outside this file.

1. **~line 1184 (ID-2-WIRING-SCHEMA addendum)** — after a whole-log quarantine the bus *"must refuse
   to start rather than resume from zero… a caller that cannot prove its floor MUST refuse to start
   rather than guess."* **SUPERSEDED** by the always-restart decision (2026-08-02, invariant 6),
   which is newer and explicit: the bus must always reach a running server, discarding damaged
   records and logging loudly. It must never refuse to boot over corruption.
2. **~line 1541 (item 4)** — *"Message ids may repeat after a WAL QUARANTINE, and after damage deeper
   than a torn tail"*, which narrowed invariant 1 to permit exactly that. **SUPERSEDED** by the
   user's ruling of 2026-08-02: asked whether the salvage index reissue should be documented as a
   narrowing, they answered **"no reuse"**, and on 2026-08-07 instructed *"fix the sequence reuse
   bugs"*. Invariant 1 stands unnarrowed. Reuse is a DEFECT, not a documented limit.

**Why both could be written in good faith, and the trap to avoid repeating.** Each passage solves the
problem in isolation and they are individually reasonable. Together they are incoherent, because they
answer the same question — *what happens when recovery destroys the evidence of what was minted?* —
with opposite trade-offs. The lesson is that a decision recorded against a narrow task can silently
contradict a project-wide invariant, and nothing catches it until someone reads both.

**The resolution, which requires neither of them.** Both passages assume the high-water mark is
derived from the log, which is what makes quarantine destroy it and forces the choice between
refusing to start and reusing ids. **Persist the mark independently of the log** — a small fsynced
file in the data dir, exactly as `bus-id` and `wal-mac.key` already work. Then quarantine still
starts the bus (invariant 6) AND the sequence still resumes above everything ever minted
(invariant 1). The dilemma was an artefact of where the state was kept.

`internal/ids/suffixstore.go` (commit `61b7c9a`) already does exactly this for the per-name suffix
floor, arrived at independently: it writes the floor BEFORE issuing the suffix, into its own file, so
no tail repair and no quarantine can rewind it. The message-sequence high-water mark should follow
the same shape.

**Consequences.** `db350e39` inherits the superseded "must refuse" premise and needs re-scoping to
the persist-outside-the-log design; it also appears to be a near-duplicate of `MSG-FU-SEQHIGHWATER`
(`6ebe51be`) and the two should be collapsed. Any test or comment asserting that ids may repeat after
quarantine is asserting superseded behaviour and must be inverted, not preserved.

## 2026-08-07 — RELAY-2 / RELAY-3: three decisions the relay plane rests on

Recorded by `feature-runner` while landing RELAY-2 (message relay + ongoing roster sync) and RELAY-3
(loop prevention via the traversed-bus path). All code is in `internal/relay`, which registers no
route and is imported by nothing (`guards_test.go`); these decisions are therefore about shapes that
are settled but not yet served.

> CORRECTED 2026-08-16: `internal/relay` IS served now. `internal/httpapi/peermount.go` mounts
`/v1/peer/{enroll,relay,roster}` behind `RequirePeerPrincipal`, and `cmd/agent-bus/main.go:1441-1442`
wires `Peer` and `PeerPrincipals` on a shipped server. The old no-mount guard was RETIRED AND
REPLACED, not deleted, by `TestRelayPeerRoutesAreMountedOnlyByTheGatedMountFile`
(`internal/relay/guards_test.go:949`), which permits exactly one file outside the package to name
those paths. All three decisions below still hold in code: the fingerprint excludes `bus_path`
(`internal/relay/message.go:437-464`), the idempotency key must equal the envelope `message_id`
(`:599-604`), and a loop drop is `200 {"accepted":false,"dropped_reason":"loop"}`
(`internal/relay/relayhttp.go:195`).

### 1. The relay idempotency fingerprint EXCLUDES `bus_path`

**Decision.** `relayFingerprint` covers the message's identity-defining content — origin bus, origin
message id, sender, broadcast flag, size, content hash, origin timestamp, recipients — and
deliberately NOT the traversed bus path.

**Why, and why the opposite is a trap.** The obvious implementation covers the whole envelope. It is
wrong, and wrong in a way that only appears once the topology has more than two buses. In a mesh the
SAME message reaches a bus by several routes, and each copy carries a different `bus_path` — that is
the normal steady state, not an edge case. A path-covering fingerprint makes the second copy "same
idempotency key, DIFFERENT payload", which is `idem.OutcomeViolation`, and invariant 10 mandates that
a violation is rejected, logged **and the offending peer disconnected**. So covering the path would
have correct peers disconnect each other as the ordinary behaviour of a correct mesh: a self-inflicted
partition produced by the very mechanism meant to make retries safe. No two-node test would ever show
it.

**The rule that keeps it coherent:** the fingerprint covers CONTENT; the path is PER-COPY ROUTING
METADATA. Changing the content is a violation; arriving by another route is not.

`internal/relay/cycle_test.go` proves both halves — a full 3-bus mesh where each non-origin node
receives the message twice and reports `duplicate:true` with zero violations, plus an explicit
counterexample showing a hypothetical path-covering fingerprint *would* be adjudicated a violation.

### 2. The relay idempotency key IS the origin's message id

**Decision.** `ValidateRelayRequest` REFUSES any relay whose `Idempotency-Key` header is not
byte-identical to the envelope's `message_id`.

**Why it is a protocol rule and not a convention.** That identity is the only reason dedupe works in a
cycle at all: it is what makes two copies arriving by two disjoint paths, from two different peers, at
two different times, resolve to ONE `idem.Scope`. A peer free to mint a fresh key per hop would defeat
invariant 10 **silently** — every copy would look new, every copy would be delivered, and nothing in
the system would report an error. Silent failure is the reason this is enforced rather than
documented.

The shapes fit and the fit is executable rather than asserted (`relayKeyFitsIdemKey`, called from a
test): `ids.MaxMessageIDLen` (85) < `idem.MaxKeyLen` (128), and every byte a message id can contain is
a legal idempotency-key byte. A future widening of `ids.BusIDPattern` therefore fails a test here
instead of quietly making relay undeliverable.

### 3. A loop drop is HTTP 200, not an error status

**Decision.** A message that has already traversed this bus is answered `200` with
`{"accepted":false,"dropped_reason":"loop"}`.

**Why.** Three reasons; the third is the load-bearing one and was NOT the reason first written down
(the reviewer corrected it, and the correction is kept visible in the code comment rather than quietly
swapped):

1. A 5xx would have RELAY-4's retry/backoff re-deliver — forever — a message that can never be
   accepted: the loop-prevention control would become the traffic amplifier it exists to prevent.
   (This argument does not extend to a 4xx; a correct backoff policy does not retry those.)
2. A 4xx would blame the sender for something it cannot know and cannot fix, and would be
   indistinguishable from the malformed-envelope rejections that genuinely are its fault.
3. **Structurally:** `Client.Relay` collapses every non-200 into `ErrPeerRefused`, so
   `Forwarder.deliver` would count an ordinary cyclic drop as `Failed` and log it at Warn. A healthy
   mesh's steady state would then be indistinguishable from a failing link, and `DropLoop` — the one
   number that shows an operator the shape of their topology — would never be recorded at all.

### What loop prevention is NOT, restated because it will be misread

PROTOCOL.md §8.5 already settles it and this work does not reopen it: the traversed bus path is
outside the signature and can never be inside it (it grows on every hop), so a lying peer can rewrite
it, **including stripping us out of it, which defeats the check completely and is undetectable**.
RELAY-3 bounds the TRAFFIC a cycle produces. It does not and cannot bound what a hostile peer
delivers. Duplicate suppression rests on idempotency plus the origin identity; RELAY-3 COMPLEMENTS
that and never substitutes for it, which is what invariant 10 requires in as many words.

---

## 2026-08-07 — MSG-FU-SUFFIXFLOOR (wiring): startup constructs the DURABLE suffix allocator, and every
failure to prove the floors is FATAL

Companion to the 2026-08-07 decision above ("the per-name agent-id suffix floor is a dedicated
write-ahead file"). That decision shipped the allocator and explicitly recorded that `cmd/agent-bus`
still built `ids.NewNameSuffixes()`, so a restarting bus still re-minted agent ids. This is the other
half: the wiring, and the three judgement calls it forced.

### 1. There is NO fallback to `ids.NewNameSuffixes()`, on any path — including a fresh data dir

**Decision.** `cmd/agent-bus/main.go` constructs through `openSuffixAllocator`
(`cmd/agent-bus/suffixfloors.go`): `ids.OpenNameSuffixes(dataDir)` → fold any derived backfill floors
through `RaiseFloor` → `Seal()` **once**, error checked. Every failure returns an error from `run()`
and the process exits non-zero. `ids.NewNameSuffixes` no longer appears in `cmd/` at all, and
`TestNoFreshSuffixCounterInCmd` parses the package's AST on every run to keep it that way.

**Why no "just for a fresh dir" fallback.** `OpenNameSuffixes` already handles a fresh dir — it
reports `Existed() == false` and yields an empty floor map, which `Seal` then persists — so a
fallback buys nothing and costs everything: it silently restores the defect while every other test
stays green. That is the exact shape `ids.NewNameSuffixes`' own doc names as the one hole the seal
does not close ("a caller whose per-name derivation FAILED and which then falls back to
`NewNameSuffixes()` mints every name from 1, silently"). A loud, recoverable outage beats silent
identity reuse; point 7 of the `ids.NameSuffixes` doc requires refusing to start rather than guessing.

### 2. The legacy-dir backfill scans MESSAGE records, and a scan that cannot complete REFUSES TO BOOT

**Decision.** On a data dir with no `agent-suffixes` file, the floors are derived from the SENDER and
the RECIPIENTS of `store.RecordKind` records in the WAL (`walAgentIDFloors`). A derivation that
cannot complete returns an error and startup FAILS.

**Why those fields and nothing else.** They are fully-qualified agent ids (invariant 2), they are
server-derived, and the WAL never compacts, so they are the ids that really are durable on a legacy
dir. `auth.EnrolmentSuffixesInWAL` is deliberately NOT used: its own doc records that it scans only
enrolment records, which are EMPTY on every dir the shipped binary has written, so sealing its result
is indistinguishable from sealing an empty map on a bus with history. A generic walk folding every
string that parses as an agent id was also rejected, and the reason is a security one: the
`idempotency_key` is CLIENT-SUPPLIED and durable, so a client could set it to
`<bus>.alpha-18446744073709551615` and permanently exhaust the name `alpha` for that bus, across
every future restart. Only server-derived fields are read.

**Known and stated in the code, not glossed:** the derivation is a LOWER BOUND. A broadcast stores a
flag rather than a recipient list (deliberately — `store.Message.Broadcast`), so an agent that only
ever received broadcasts leaves no trace in any record. That is structural to a DERIVED floor and is
exactly what the written-ahead floor file removes; the backfill runs once per data dir and never
again.

### 3. The `wal.Open` ordering hazard: BOOT, log at ERROR, and say what is exposed

**The conflict, stated rather than picked.** `internal/auth/floors.go` documents an ordering hazard
with no clean fix at this layer, both halves of it reproduced: scanning BEFORE `wal.Open` reads an
unrepaired file, so `wal.ScanAll`'s strict framing fails on an ORDINARY torn tail — a routine power
loss would then make the derivation fail, and a failed derivation on a legacy dir refuses to boot.
Scanning AFTER `wal.Open` reads the repaired file, so anything recovery removed is invisible and a
floor can come out too low (reproduced: 5 → 2). Against that, DECISIONS.md 2026-08-02 ("Availability
over retention") and the 2026-08-07 supersession say the bus ALWAYS restarts.

**Decision.** Scan AFTER `wal.Open`, and distinguish two cases rather than collapsing them:

- **The scan COMPLETES but recovery had repaired/discarded/quarantined something.** BOOT.
  `openSuffixAllocator` consults `walLog.Recovered().Repaired` and logs at **ERROR**, naming the
  exposure and every field of it (`repaired`, `rewritten`, `quarantined`, `lost_unidentified`,
  `discard_count`, `discarded_bytes`). Refusing here would trade a real availability loss for a
  narrow one: recovery removes records rarely, the ids in them are usually named by surviving records
  too, and the alternative is a bus that will not start after an ordinary power loss.
- **The scan CANNOT COMPLETE, on a dir with no floors file.** REFUSE. There is nothing to prove the
  floors with, and sealing an empty map asserts "no suffix was ever written" — the precise false
  claim that re-mints every live id. This is a deliberate, NARROW exception to "the bus always
  restarts": it can only fire on the FIRST start against a legacy dir whose log cannot be read, never
  on an ongoing start, because the first successful start writes the floors file and the backfill
  never runs again.

A partial map is never sealed: failure is TOTAL (`walAgentIDFloors` returns `(nil, err)`), because a
derivation that got every floor it saw right but MISSED A NAME seals exactly as cleanly as a complete
one, and every missed name then mints from 1 over ids already on disk.

### What this does NOT do

It does not make enrolment durable — the roster and all sessions are still in memory only (AUTH-3).
The startup WARN that says so was NARROWED, not removed: its clause "and agent id suffixes restart
from 1 for every name" became FALSE with this change, and a false WARN in the startup log is its own
defect. `CONTRACTS-HTTP.md:330` quotes the old wording verbatim and is OUTSIDE this task's file
ownership; updating it is filed as a follow-up.

Acceptance criteria (c) and (d) of the task — flip `ids.NewNameSuffixes` to born-unsealed or delete
it, and add a repo-wide guard that no production package calls it — live in `internal/ids`, which
this task did not own. There are currently ZERO production callers of `ids.NewNameSuffixes` anywhere
in the tree; both are filed as follow-ups for that package's owner.

---

## 2026-08-07 — Cross-bus key trust: pin the origin bus key at peering, NO TOFU

**The hole this closes.** CRYPTO-4's messaging-key bundle is attested by the **local** bus. So when
bus B hands a recipient a key for bus A's agent, **B can simply substitute its own key** — and the
recipient's verification then succeeds against a key the attacker chose. Cross-bus signatures would
verify against whatever the nearest bus says, which is worth nothing at all. Every other guarantee in
the SIGN epic sits on top of this one.

**Decision (user).** *"pin the bus key at peering, no TOFU"*. Concretely:

- A relayed message's key bundle keeps **bus A's own attestation intact** — signed by **A's bus key**,
  not re-attested by any intermediate.
- **A's bus key is pinned at peering time.** A bus that has not been peered with cannot have its
  agents' signatures trusted.
- **No trust-on-first-use anywhere**, including as a fallback.

**Why, beyond consistency.** This matches the invite-blob decision (E6, 2026-08-02) where the
bus-cert fingerprint travels out of band precisely to eliminate the TOFU window — and peer enrolment
already uses that same invite mechanism, so the material to pin is already in the handshake. But the
stronger argument is specific to relay: TOFU's exposure window is *the moment of first contact*, and
for a relay that is exactly when a hostile intermediate is best placed to act. A TOFU fallback would
reintroduce the whole hole for any peer not yet seen, which is every peer, once.

**Consequences.**

- **Peering material must carry A's bus SIGNING key**, not merely its TLS certificate fingerprint.
  Those are different jobs — the TLS key authenticates the *connection*, the signing key attests
  *agent key bundles* — so pinning one does not give you the other.
- **OPEN, and deliberately not decided here:** whether the bus signing key and the bus TLS key are the
  same key. One key is simpler; two lets the connection key rotate on a different schedule from the
  attestation key, which matters because rotating an attestation key invalidates pins held by every
  peer. Recommendation: keep them separate, but this needs its own ruling before implementation.

  > CORRECTED 2026-08-16: settled the same day, in the entry immediately below — "The bus TLS key and the bus
  SIGNING key are SEPARATE". The recommendation was taken.
- **Rotation must follow the two-certificates rule** already decided for the bus's own TLS rollover
  (E3, 2026-08-02): a bus rotating its signing key must serve both during a rollover window, or every
  peering breaks at once and re-peering becomes indistinguishable from an attack.
- **An unpeered bus's messages cannot be verified**, by construction. That is the intended behaviour,
  not a gap — but it must be stated in `PROTOCOL.md` so nobody "fixes" it later with a TOFU fallback.

---

## 2026-08-07 — The bus TLS key and the bus SIGNING key are SEPARATE

**Decision (user).** *"separate keys"*. Settles the sub-question left open by the cross-bus key-trust
entry above.

A bus holds two long-lived keypairs, with different jobs, different lifetimes and independent
rotation:

- **The TLS key** authenticates the CONNECTION. Its fingerprint travels in the invite blob (E6) and is
  pinned by clients.
- **The SIGNING key** attests AGENT KEY BUNDLES. It is pinned by PEERS at peering time (the entry
  above) and is what makes a relayed signature verifiable at the far end.

**Why separate, given one key is simpler.** The rotations have incompatible blast radii. Rotating the
TLS key affects clients connecting to this bus, and the two-certificate rollover already decided (E3)
makes that non-disruptive. Rotating the SIGNING key invalidates pins held by **every peer bus**, which
is a federation-wide event. With one key, every routine TLS rotation would become a federation-wide
event too — so the operationally cheap rotation would inherit the cost of the expensive one, and the
predictable result is that neither gets rotated.

Separating them also keeps the failure domains apart: a compromised TLS key lets an attacker
impersonate the bus to clients, which is bad; a compromised signing key lets it forge attestations for
any agent on the bus, which is worse. One key means the lesser compromise automatically becomes the
greater one.

**Consequences.**

- Two key files in the data dir, alongside `wal-mac.key`, both mode 0600. That makes **three** secrets
  in the data dir now — a backup that omits any of them is a bus that cannot do its job, and the
  backup guidance in `PROTOCOL.md` must say so explicitly rather than naming `wal-mac.key` alone.
- A peer pins **two** things: the TLS certificate fingerprint (connection) and the bus signing key
  (attestations). They are obtained at different moments and must not be conflated in code or docs.
- Both are generated with stdlib per invariant 9 — `crypto/ed25519` for signing, `crypto/tls` +
  `crypto/x509` for the certificate. Never hand-rolled, never assembled from primitives.
- Rotation of each follows the two-key rollover rule independently.

---

## 2026-08-07 — ADDENDUM to MSG-FU-SUFFIXFLOOR (wiring): the suffix-floor WAL scan is GATED, and a
missing floors file is now LOUD

This corrects §4 of the "MSG-FU-SUFFIXFLOOR (wiring)" section above and adds one decision it did not
make. Written as an addendum rather than an edit because that section is already committed and
`DECISIONS.md` is append-only. **Where the two disagree, this one governs.**

### Correction to §4: the WAL scan runs ONLY when there is no floors file

§4 decided the derivation should run on EVERY start so a rewound floors file could be cross-checked.
The reviewer and security gates independently rejected that, on the same evidence, and they are
right.

`wal.ScanAll` accumulates every record **including full payloads** (`internal/wal/reader.go`), the
WAL never rotates or compacts, and enrolment is unauthenticated — so peak startup memory would be
proportional to the entire log, on every start, forever, with no compaction to recover with. That is
not a hypothetical: `internal/wal` already carries a measured incident where a per-record **index
list** — far smaller than the payloads — cost 1.76 MB on a 23.7 MB log and was described as "the
boot-time OOM the eviction was written to avoid" at 10 GiB. Recovery itself is streaming
(`wal.Replay` uses the `scanFrom` callback); the raw scan is not, because `scanFrom` is unexported,
so there is no third option inside this task's file boundary.

**Decision.** The scan is gated on `!alloc.Existed()` — at most once per data directory, for the
legacy backfill. The every-start cross-check is filed as
`MSG-FU-SUFFIXFLOOR-FU-STREAMSCAN` (`6f4c17ef-220c-465f-b8d8-a0f04aac1905`): export a streaming raw
scan from `internal/wal`, fold floors as records arrive, and reinstate the check at O(record) peak.

**What that costs, precisely.** A floors file that EXISTS but has been rewound to an
older-but-checksum-valid version is no longer detected. A **deleted** one still is (`Existed()` is
false, so the backfill runs and the case is logged — see below); a **corrupt** one still is
(`ids.ErrSuffixFileCorrupt`, fatal). Only rewound-but-valid is uncovered, and that is written into
the code at the gate rather than left to be discovered.

**Unchanged from §4, and endorsed by both gates:** when the cross-check does fire, it RAISES the
floor rather than merely reporting it, and it does not refuse to boot. Refuse-to-boot would hand
anyone with data-dir write access a permanent boot-denial primitive; `RaiseFloor` can never lower a
floor, so raising cannot weaken the persisted authority.

### New: a MISSING floors file is logged loudly, and the startup WARN no longer claims otherwise

The security gate reproduced a case §4 did not cover. Delete **only** `agent-suffixes` from a live
data dir, leaving `bus-id` and the log intact, and the bus re-mints ids it has already issued —
because an id that was issued but never reached a durable record leaves nothing for the backfill to
find. The floors file was the only thing that knew. It went out at INFO, indistinguishable from an
ordinary start, while the startup WARN two lines later asserted "Agent id SUFFIXES are durable and
never restart from 1" — actively false in exactly the case an operator most needs the truth.

**Decision, two parts.**

1. The seal line is graded by case: **INFO** when the floors file was present (the steady state);
   **WARN** when it was absent AND the data directory was EMPTY when the process started (a
   genuinely first start — one line, once per dir); **ERROR** otherwise, i.e. absent on a directory
   that already had content or whose log holds records. It is deliberately NOT a refusal to boot: a
   legacy dir looks identical, so refusing would block the very migration this code performs, and it
   would hand anyone with data-dir write access a permanent boot-denial primitive.

   **The discriminator is DIRECTORY EMPTINESS, not the record count, and that correction came from
   running it.** The security gate prescribed "ERROR when `walLog.Recovered().Records > 0`, WARN
   otherwise", which was implemented and then measured against a real binary: because enrolment is
   still memory-only, a bus can issue `alpha-1` and `alpha-2` and leave a COMPLETELY EMPTY log, so
   deleting `agent-suffixes` on a dir with real history reported as a routine first start at WARN and
   re-minted `alpha-1`. Emptiness of the data dir catches that; the record count does not. It is read
   in `run()` at the last instant it is knowable — after `MkdirAll`, before `dirlock.Acquire` writes
   `bus.lock` — and it is used for NOTHING except the log level. The record count is kept as a second
   trigger, because it stays correct once enrolment is durable and a non-empty log is history by any
   definition.
2. The suffix clause was **deleted** from the standing startup WARN rather than negated. No single
   unconditional sentence can be right in both cases, so the claim now lives entirely in the
   per-start "agent-id suffix floors" line, which knows which case it is in. The WARN says only what
   is unconditionally true (the roster and sessions are in memory only) and points at that line.

### Release ordering, recorded because it is a constraint and not an observation

The backfill folds `store.RecordKind` records only; enrolment (`kind=agent`) records are deliberately
NOT folded, and a test pins that. **MSG-FU-SUFFIXFLOOR must therefore ship before or with AUTH-3.** A
build that has durable enrolment but reaches a data dir with no floors file would leave those
enrolment suffixes invisible to the backfill.

---

## 2026-08-07 — MTLS-DESIGN: the consolidated certificate lifecycle (audit + the two genuine gaps)

`MTLS-DESIGN`'s task description is STALE — it says "BLOCKED ON USER DECISION" for five questions,
but the user has since answered all but two of them and the answers are already scattered across this
file. This entry is the single pointer for all five, and settles the two that were never actually
closed.

### The three already settled — pointers only, not restated

1. **How a client learns the bus cert fingerprint before first connection.** Settled: the invite blob
   carries bus id + address + bus-cert fingerprint + invite secret, eliminating TOFU entirely. See
   "E6 — The invite blob carries the bus certificate fingerprint. NO TOFU" (line 1198) and the
   cross-bus reaffirmation at line 2227 ("This matches the invite-blob decision (E6, 2026-08-02) where
   the bus-cert fingerprint travels out of band precisely to eliminate the TOFU window").
2. **Bus-key rotation invalidating every client's pin.** Settled: the two-certificates rule — the bus
   serves both the outgoing and incoming certificate during a rollover window, so no client is ever
   forced to re-enrol on routine rotation. See "E3 — Rotation serves TWO certificates during rollover"
   (line 1224) and its restatement for the *signing* key specifically at "The bus TLS key and the bus
   SIGNING key are SEPARATE" (line 2251), which is also the entry establishing that the TLS key and
   the signing key are two independent keypairs with independent rotation schedules — the lifecycle
   below treats them as such throughout.
3. **Plaintext escape hatch for tests/dev, and self-generated vs operator-supplied certs.** Settled,
   both in the same entry: NO plaintext hatch anywhere — not a flag, not an env var, not a build tag —
   tests use real TLS against certificates minted at test time via a dedicated helper, and
   `InsecureSkipVerify` must appear nowhere in the tree. Operator-supplied certificates ARE allowed
   (`-tls-cert` / `-tls-key`) alongside the self-signed default. See "E7: no plaintext escape hatch;
   tests use REAL TLS; operator certs allowed" (line 1256).

### The two genuine gaps — decided here

Confirmed by grep: `expir` and `validity` do not appear anywhere in `DECISIONS.md` before this entry
in the context of TLS certificates (only session-token expiry, lines 576–700, and unrelated hits).
`re-bind` / `re-enrol` in the mTLS context likewise appear only as open questions in task descriptions,
never as a decision. This is the real gap the task's own status note predicted.

**Decision — validity periods.** Both the bus TLS certificate (`MTLS-BUSCERT`) and the client TLS
certificate (`MTLS-CLIENTCERT`) default to **365 days** when self-generated. Reasoning:

- There is no CA and no browser trust store to satisfy, so there is no externally-imposed ceiling
  (unlike the ~398-day cap public CAs enforce) — the number is purely an internal operational choice.
  365 days is chosen because it is long enough that routine possession of a valid cert is not itself
  an operational burden, short enough that a cert leaked and never rotated does not stay valid
  indefinitely, and it gives operators one predictable, memorable cadence to plan around instead of
  two different ones for the two cert kinds.
- Symmetry between bus and client cert periods is deliberate: both are stdlib-generated
  (`crypto/tls` + `crypto/x509`, per invariant 9) long-lived identity keys with the same job — proving
  which key holder is on the connection — so there is no reason for them to drift onto separate
  schedules. This is distinct from, and does not have to match, the *signing* key's rotation cadence
  (line 2251), which is deliberately allowed to run on its own, longer schedule because its blast
  radius is federation-wide rather than per-connection.
- Operator-supplied certificates (E7, allowed) are exempt — their validity period is whatever the
  operator's own PKI issues, and the bus does not second-guess it.
- This does **not** double as a revocation mechanism, and is not meant to. Line 1139–1144 already
  drew that line: "mTLS proves which key holder is on the connection; the session token is the
  revocable, time-bounded application credential." A cert with no CRL has no cheap way to be revoked
  early; the session token (≤1h expiry, immediate revocation on `/leave`, line 695–700) and the
  planned agent-revocation surface (`AUTH-4`) carry the short-term, revocable half of the job. The
  cert's validity period is a leak-containment bound, not a substitute for revocation.

**Decision — renewal discipline.** Both certs are renewed **proactively, at 75% of lifetime**,
mirroring the session-token refresh rule already decided at line 700 ("the client refreshes at 75% of
lifetime") rather than inventing a second convention. For the bus cert this means the operator (or an
operator-triggered rotation, per E3) starts serving the successor certificate well before the
incumbent's `NotAfter`, during the two-certificate rollover window that already exists. For the client
cert this is the re-bind route decided below.

**Decision — what an agent does when its client cert expires: a re-bind route, not re-enrolment, for
the common case; re-enrolment only for the case a re-bind route cannot reach.**

Two distinct situations, with different answers, and the distinction is the point:

1. **The cert is approaching `NotAfter` but the agent still holds a valid session token and can still
   complete the current mTLS handshake.** The agent generates a new keypair/self-signed cert
   (`MTLS-CLIENTCERT`'s existing job) and calls a **re-bind** route, authenticated by its still-valid
   session token (and, once `AUTH-1-FU-POPKEY` lands, proof of possession of its existing AUTH key —
   never proof of possession of the TLS key alone, since that key is exactly what is being replaced),
   to have the NEW fingerprint bound to its EXISTING, unchanged agent id. No invite is spent; no new
   identity is minted. This is the client-side mirror of E3's rule for the bus's own cert: "Rotation
   must never require every client to re-enrol — that would make routine key hygiene indistinguishable
   from a security incident." The same principle applies in the other direction — an agent renewing
   its own routine credential must not be indistinguishable from a brand-new agent joining. Note this
   is a NEW route, not yet filed as its own task; `MTLS-BIND` as scoped only covers the FIRST binding
   at enrolment, and the re-bind path is intentionally NOT folded into it here so it is not quietly
   dropped the way the E-7 section (line 1256) warned `MTLS-CROSSCHECK` not to fold into `MTLS-BIND`.
2. **The cert has already lapsed past `NotAfter` with no prior renewal — the agent was offline for the
   whole validity period, or otherwise missed the window.** No route can rescue this: every
   authenticated route sits behind the mTLS handshake, and a client that cannot complete that
   handshake cannot reach a re-bind route to save itself. This collapses to the same case as a lost or
   compromised private key, and the answer already implied by `INVITE-GATE` and invariant 3 is the
   right one: the operator issues a **fresh invite**, and redemption mints a **new** agent id
   (line 1146: "the invite is what authorises a new client certificate to be bound to a **new** agent
   id"). The old id is not recovered — invariant 1 forbids reissuing an id, and there is no mechanism
   recorded anywhere that lets an invite rebind an EXISTING id, so inventing one here to preserve
   continuity would be exactly the kind of undocumented, un-reviewed departure invariant 1 exists to
   prevent. The loss of identity continuity is the accepted cost of a credential that was never
   renewed in time, not a defect to route around.

**OPEN, not decided here, and flagged loudly because it affects `MTLS-CLIENTAUTH`/`MTLS-CROSSCHECK`
directly.** `MTLS-CLIENTAUTH` is scoped as "`RequireAnyClientCert` plus application-layer policy,
never `InsecureSkipVerify`" (`SPEC.md`). `tls.RequireAnyClientCert` performs **no chain verification
at all**, which in Go's stdlib means the TLS handshake itself does not check a client certificate's
`NotAfter`/`NotBefore` — only application code that explicitly reads those fields after the handshake
would enforce expiry. Nothing in `DECISIONS.md`, `MTLS-BIND`, or `MTLS-CROSSCHECK`'s scoping commits to
that application-side check existing. This entry's validity-period and renewal decisions above are
therefore a POLICY, not yet a proven ENFORCEMENT path — whoever implements `MTLS-CROSSCHECK` must
either (a) read the presented cert's `NotAfter` at the application layer and reject a connection past
it, mirroring the session-token expiry check, or (b) explicitly decide expiry is advisory only and the
session-token/revocation layer is the sole enforcement, and record which. This was not resolved from
anything already on record and is called out here rather than silently assumed either way.

> CORRECTED 2026-08-16: decided on two of the three surfaces, and still open on the third — the split matters.
BUS certificate: the CLIENT enforces the window, via `crypto/x509` rather than a local date compare
(2026-08-07, MTLS-EXPIRY; `client/pin.go:515-534`). PEER surface: option (a) was taken, inside
`RequirePeerPrincipal` and BEFORE the durable binding is consulted (2026-08-14, RELAY-6 AMENDMENT
ruling (g)). AGENT surface: STILL OPEN. The listener is `tls.RequestClientCert`
(`cmd/agent-bus/tlslisten.go:152`), which chain-verifies nothing, so an expired agent certificate is
still admitted; task `ca356fde`, which must close in the same task as `MTLS-BIND`/`MTLS-CROSSCHECK`.
Note also that the constant this section chose is real: `buscert.CertValidity = 365 * 24 * time.Hour`
with `NotBefore` backdated by `clockSkewAllowance = 5 * time.Minute`
(`internal/buscert/buscert.go:72,78,633`).

### Key file locations in the data dir — three long-lived secrets after `MTLS-BUSCERT` lands

Confirmed by reading `internal/wal/mackey.go:35`: the existing secret is literally named
`wal-mac.key`, mode 0600 (`macKeyMode`, `mackey.go:44`), fatal if missing or wrong (line 1093–1098).
After `MTLS-BUSCERT` (`internal/buscert`, `SPEC.md` line 2266) lands, the data dir holds **three**
long-lived secrets, all mode 0600, all fatal-if-unusable on the same precedent as `wal-mac.key`:

1. **`wal-mac.key`** — the WAL integrity MAC key (existing, shipped).
2. **The bus TLS private key** — backs the certificate whose fingerprint is what E6's invite blob
   carries and what `MTLS-PIN` verifies against. Not yet landed; `MTLS-BUSCERT` does not commit to a
   literal filename in `SPEC.md`, so none is asserted here — only that it is a fourth (now second)
   file in the data dir governed by the same "0600, fatal if unusable" rule as `wal-mac.key`.
3. **The bus SIGNING private key** — the Ed25519 key peers pin at peering time (line 2211), separate
   from the TLS key per the 2026-08-07 separation decision (line 2251). Also not yet landed, also no
   literal filename asserted here for the same reason.

**Stated explicitly, per this task's brief:** a backup of the data dir that omits any one of these
three files produces a bus that cannot do its job — missing `wal-mac.key` means the WAL cannot be
verified or extended; missing the bus TLS key means the bus cannot serve TLS at all (`MTLS-LISTENER`
refuses to start without a usable cert/key, `SPEC.md` line 2246); missing the bus signing key means
every peer's pin (line 2211) goes stale and no relayed signature from this bus can be verified anywhere
in the federation. This is already noted in passing at line 2278–2280 ("a backup that omits any of
them is a bus that cannot do its job") for the two not-yet-landed keys; this entry adds `wal-mac.key`
to make the count of three explicit and complete, and restates it because `PROTOCOL.md`'s backup
guidance (owned by `documentation`, outside this task's file boundary) still needs to name all three
once the two new keys exist on disk — filed here as a pointer for whoever does that edit, not done in
this task.

---

## 2026-08-07 — The WAL record-index high-water mark is a dedicated write-ahead file, not derived from the log (e120153b, db350e39)

**The two defects, one root cause.** `e120153b` and `db350e39` were reported as separate Spec Server
defects — a discarded tail record's index handed straight back out, and a whole-log quarantine
resetting the index space to 1 — but they share a single cause: `NextIndex` was derived SOLELY from
what SURVIVES in the log, and both a torn-tail discard and a quarantine destroy the evidence that
would be needed to derive it correctly. `internal/hub` derives the message-sequence floor from
`wal.Recovered.NextIndex - 1`, so both defects reissued MESSAGE IDS as well as WAL record indices.
This entry is the follow-through on the design already sketched in this file's 2026-08-07
"SUPERSEDES two earlier passages" entry above (line ~1983): that entry proposed persisting the
high-water mark outside the log; this entry records what actually shipped, the two rejected
alternatives that entry did not consider, and the security bound the implementation needed that the
design sketch did not.

**How invariants 1 and 6 were reconciled — they only LOOKED irreconcilable.** Invariant 1: an index
(and, downstream, a message id) is never reissued, including across restarts. Invariant 6
(2026-08-02, "Availability over retention"): recovery must always reach a running server, preferring
to discard damaged or unreadable data over refusing to start. These two read as a straight
contradiction the moment the high-water mark is derived SOLELY from the log: a quarantine must
discard the log to satisfy invariant 6, and discarding the log destroys the only record of what was
already minted, which invariant 1 needs to avoid reissuing it. **Moving the mark OUTSIDE the log
dissolves the conflict rather than trading one invariant against the other.** A quarantine still
discards the unreadable log and starts a fresh one (invariant 6 fully intact — the log itself is
still disposable), while the index still resumes above everything this data directory ever
authorised, because that fact now lives in a file the quarantine never touches (invariant 1 fully
intact). **No narrowing of either invariant was needed or made.** The apparent conflict was an
artefact of where the state was kept, not a property of the two invariants themselves.

**`db350e39`'s "startup must refuse, not resume from 1" premise was deliberately NOT implemented,
and is SUPERSEDED.** The defect as filed argued: *"a caller that cannot prove its floor MUST refuse
to start rather than guess."* That premise is rejected here, for the same reason the "SUPERSEDES"
entry above already gives: the always-restart decision (2026-08-02, "Availability over retention")
is newer and explicit, and once the mark survives the quarantine a refusal buys nothing — the bus
can prove its floor (it reads the durable file) and can always restart, so there is no case left
where refusing would have been the only honest answer. **Also recorded here because it is the kind
of thing that silently reappears:** `db350e39`'s stored `proof_cmd` named a test
`TestRecover_WholeLogQuarantine_RefusesStartOnUnprovenSequenceFloor` that was **deliberately never
written**, because the test's NAME would have enshrined the superseded refuse-to-start policy as the
contract — this repo has already had exactly one case of a test name outliving the policy it
encoded (the reissue-is-accepted framing at line 1541, superseded below). The behaviour that DID
land is proved instead by `TestWALIndexFloorSurvivesAQuarantine` and
`TestWALIndexFloorCrashNeverReissuesAnIndex` (`internal/wal/indexfloor_test.go`,
`indexfloor_crash_test.go`), which assert the bus **starts** after a quarantine and the next index is
strictly above everything the quarantined run ever authorised — the opposite assertion from the one
the abandoned test name would have carried.

**Why a dedicated file, and the alternatives rejected:**
- **(a) Derive the mark from the log alone.** Rejected — impossible after a quarantine by
  construction; that derivation IS the bug being fixed, not an option still on the table.
- **(b) Refuse to start when the floor cannot be proven.** Rejected — contradicts invariant 6 and is
  the superseded `db350e39` premise above; also unnecessary once (c)/(d) below are rejected too and
  the dedicated-file design makes refusal moot.
- **(c) Put the mark in the WAL frame header itself.** Rejected — this forces a WAL on-disk format
  bump (a real, disruptive migration for every deployed data directory) and the mark still dies with
  the log on exactly the quarantine path it exists to survive; storing it inside the thing being
  discarded solves nothing.
- **(d) fsync the floor before EVERY index (a reservation block of 1).** Rejected as the DEFAULT,
  though it is the theoretically simplest and most conservative option. It is CORRECT — it never
  burns a hole — but it DOUBLES the fsync count on the send path, doubling the cost of invariant 4's
  guarantee to buy nothing invariant 1 does not already get from a larger block. Shipped instead:
  `indexReserveBlock = 256`, which amortises the floor write to roughly one extra fsync per 256 WAL
  appends and accepts that a crash may burn up to 255 unused indices as a permanent hole in the index
  sequence.

  > CORRECTED 2026-08-16: the constant is **64**, not 256 — `internal/wal/indexfloor.go:114`, whose own comment
  records that "the block came down from 256 to 64". So the real amortisation is one extra fsync per 64
  appends and a crash burns up to 63 unused indices. The 2026-08-07 CORRECTION entry below spotted this
  drift and handed it to spec-keeper; the code moved and this paragraph did not. Nothing else in the
  trade changes.

  Holes are legal and permanent (invariant 1 beats gap-freeness) — the same trade
  `internal/ids/suffixstore.go` already made for the per-name agent-id suffix floor, arrived at
  independently and now mirrored deliberately.

**The security bound on untrusted indices.** A discarded frame's index is read from a frame header
whose MAC did NOT verify — that is precisely why the frame is being discarded rather than trusted —
and on a WAL those header bytes are CLIENT-INFLUENCED (a message body reaches the frame a client
constructed the content of). A forged, wildly implausible index in such a header could otherwise push
the durable floor to near `math.MaxUint64` in one damaged frame, permanently exhausting the bus's
64-bit id space — a restart-proof denial of service from a single corrupted byte. The fix accepts a
discarded frame's declared index only when it is PLAUSIBLE for the file's size (`RepairLog`'s framing
pass; proved by `TestWALIndexFloorRejectsAnImplausibleForgedIndex`, which forges index `1<<62` and
asserts the durable ceiling after recovery stays within one reservation block of the file's honest
size, not anywhere near the forged value). The durable floor is the TRUSTED backstop either way: an
implausible index is simply treated the same as any other unidentifiable loss
(`Repair.LostUnidentified`), which sends `Open` to the floor rather than to the untrusted number.

**This SUBSUMES most of the open task `MSG-FU-SEQHIGHWATER` (`6ebe51be`).** That task asked for
exactly this artefact, but scoped to the message sequence rather than the WAL record index:
`internal/hub` derives its sequence floor from `wal.Recovered.NextIndex - 1`, so raising the value
`Recovered.NextIndex` reports closes the measured message-id regression this task's item 4 (line
1541 below) recorded, without `internal/hub` needing a floor file of its own. **The residual that
keeps `MSG-FU-SEQHIGHWATER` open:** the migration window on a data directory that predates this file
— until the first `Open` under the new binary writes `wal-index-floor`, a quarantine on that very
first start can still regress the mark, exactly as before. That gap is inherent to any first-run
migration and is not something a second, hub-level floor file would close any further; whoever
re-scopes `MSG-FU-SEQHIGHWATER` should read it as "confirm the residual is acceptable and close" or
"account for the migration window explicitly", not "build a second floor".

**This SUPERSEDES the 2026-08-02 section "### 4. Message ids may repeat after a WAL QUARANTINE, and
after damage deeper than a torn tail" (line ~1541 — that section was REMOVED on 2026-08-16;
see the terminal "Removed on 2026-08-16" section).** `DECISIONS.md` is append-only, so the
correction is recorded here rather than by editing those lines, which must be read as HISTORICAL —
they describe the pre-2026-08-07 behaviour and the narrowing that was, at the time, believed to be
the accepted trade. It is not: quoted in full, that section recorded *"This narrows invariant 1,
which says ids are never reused including across restarts, and the narrowing needs to be recorded
rather than implied"* and pointed at `MSG-FU-SEQHIGHWATER` as "the real fix". The real fix has now
landed, in the WAL layer rather than in `hub` directly (see the subsumption note above), and
invariant 1 is UNNARROWED as of this section: a WAL quarantine no longer regresses the sequence
floor, and neither does damage deeper than a torn tail, because both are bounded by the durable
index floor rather than by the log's own arithmetic. The nine test assertions that encoded the old,
accepted-reissue behaviour were inverted, not preserved (see `AGENT_LOG.md`, same date, for the
full list) — that inversion was the point of the task, not a side effect of it.

**The cost, stated plainly.** Roughly one extra small-file write+fsync per 256 WAL appends on the hot
path (the reservation block), plus one at `wal.Open` (recording the skip, if any, and creating the
file on a migrating data directory) and one at a clean `Close` (sealing the true high-water mark so
the next start burns no hole it does not have to). Against invariant 4's existing one-fsync-per-append
cost, this is an amortised ~0.4% addition, not a new class of cost.

**Chain and verification.** spec-keeper → implementer → test-engineer → reviewer → security →
documentation (this entry). Code-only as shipped: nothing here has been exercised against a running
`agent-bus` server, so no claim of LIVE behaviour is made by this entry — see `AGENT_LOG.md`, same
date, for the full proof verdicts (RED/GREEN via `scripts/proof-check.sh`, plus a real `kill -9`
crash-injection test) and for that same caveat restated.

---

## 2026-08-07 — SIGN-2/SIGN-6 SHIPPED: the mandatory-signature policy is UNBLOCKED. This SUPERSEDES "SIGN-2/SIGN-6: the signing core lands; the mandatory-signature policy is BLOCKED"

**This entry SUPERSEDES, by name, the earlier section on this same date titled "SIGN-2/SIGN-6: the
signing core lands; the mandatory-signature policy is BLOCKED".** `DECISIONS.md` is append-only, so
that section is not edited — but it must now be read as HISTORICAL, and specifically its **Decision
4 ("why SIGN-2 and SIGN-6 are not done, three blockers")** is obsolete in its entirety. Anyone
reading that section on its own will conclude a signature is optional on this bus. **It is not: a
signature is MANDATORY on `POST /v1/send` and there is no unsigned message type on the wire.**
Decisions 1, 2 and 3 of that section SURVIVE unchanged and are re-affirmed below.

### What unblocked it — the three blockers, answered

The earlier entry named three. Each was real; none needed the thing it appeared to need.

**(a) "No messaging keypair exists."** It does not need to exist ON THE SERVER. The realisation that
unblocked this is that **SIGN-6's ingest check is SHAPE-ONLY**: present, valid strict base64, exactly
64 bytes, sender matches the authenticated caller, message id is this bus's and agrees with seq,
timestamp is positive. **Not one of those needs the sender's public key.** The bus therefore needs no
messaging key at all to enforce the policy, and the blocker dissolved rather than being solved. The
CLIENT mints and holds its own messaging keypair locally (`messaging_key_seed`, minted on first use,
private half never leaving the machine), distinct from its AUTH enrolment keypair per invariant 3, so
signing works end to end today.

**(b) "The durable mint does not exist."** Built — see the next decision.

**(c) "The signature cannot reach the durable record without `internal/hub`."** That was a
file-ownership boundary on one agent, not a design problem. `store.Message` gains
`TimestampUnixMilli` and `Signature`, `store.Record` carries both, and the hub passes them through.

### Decision 1 — the bus enforces SHAPE; the RECIPIENT enforces AUTHENTICITY. The bus NEVER verifies a signature

**Why.** The bus does not hold the sender's messaging key, and it must not be trusted to police
messages for senders it does not control. The asymmetry is the whole argument: **a bus that could
verify could equally forge.** Giving the server the verification key would put the trust boundary on
the very component the signature exists to make untrustworthy. Adding a verify to the ingest path
would look like an improvement and would quietly move that boundary.

**What it costs.** The bus accepts a well-formed message it cannot itself attribute — a signature
over the right shape by the wrong key passes ingest and is stored. That is intended: attribution is
the recipient's job, and a stored-but-unverifiable message is loud at the recipient rather than
silent everywhere.

### Decision 2 — reserve-then-send: a send is a TWO-STEP through a new `POST /v1/mint`

SIGN-1 settled (option (a), `PROTOCOL.md` §8.4) that the signature covers the ORIGIN bus's minted
message id and sequence. Invariant 1 makes the server authoritative on those, so the client
**cannot sign until it has them**. `POST /v1/mint` hands them out; the client canonicalizes, signs,
and presents the reservation back on `POST /v1/send`, **under the same idempotency key**.

**Why the same key covers both calls.** A reservation is scoped by `(agent, op, idempotency_key)` —
the same scoping `internal/idem` uses, for the same reason. A repeat of step 1 returns the SAME
reservation (`Idempotency-Replayed: true`) and burns no second sequence, so a client that crashes
between the two steps repeats both and converges on ONE message. A fresh key on the retry would
produce a second reservation and, if the first send had landed, a second message.

**What it costs.** One extra round trip per send, permanently. That was priced in when SIGN-1 chose
option (a): the alternative leaves the id and sequence unsigned, and an unsigned sequence is exactly
the field the replay defence rests on.

### Decision 3 — the durable artefact is a SEQUENCE FLOOR written AHEAD, not a per-key assignment record. The counting argument is RETIRED

`hub.Open` used to derive its restart floor as `NextIndex - 1` on a **counting argument**: every
sequence issued is `<=` the WAL index of the prepare carrying it, which held because each message
consumes one sequence and at least two WAL indices. **Handing a sequence to a client BEFORE any
record is written breaks that argument outright** — mint, restart, and the floor resumes below
numbers already handed out, so two validly-signed messages could carry one origin message id,
undetectably. That is the precise failure option (a) exists to prevent.

**Decision.** A new WAL entry kind `"seqfloor"` (`hub.SeqFloorRecordKind`, body
`{"v":1,"floor":N}`) records that every sequence `<= N` is BURNED. It is fsynced AHEAD of the number
being handed out, in batches of `hub.MintBatchSize = 256`. `hub.Open` takes the **maximum** of
`NextIndex - 1`, the replayed floor, and the highest applied sequence; `NextIndex - 1` is kept purely
as defence in depth, since it can only raise the floor. The runtime check becomes the **direct**
assertion — *every sequence handed out is `<=` the durably-recorded floor* — which is strictly
stronger than the counting argument it retires, and which POISONS the hub on violation exactly as
before. `publish`'s old `committed.PrepareIndex < seq` check had to GO, not merely be relaxed: it
now fires legitimately (the first floor record burns seqs 1..256 while sitting at WAL index 1), and
a false poison is worse than no check at all.

**This is the same pattern MSG-FU-SUFFIXFLOOR established** for per-name agent-id suffixes — *write
the floor AHEAD, never derive it* — reached here independently and then deliberately mirrored.

**Decision 3a — the mint TABLE is in memory; only the NUMBER is durable.** What must survive a crash
is that a number can never be issued twice, and the floor record does exactly that. Which
`(agent, op, key)` holds which sequence does not need to survive. **This is a conscious narrowing of
`PROTOCOL.md` §8.4's wording** ("bound to the idempotency key that asked for it"), recorded in
`PROTOCOL.md` §8.4.1 as a divergence rather than left to be discovered.

**What it costs, stated honestly:**
- **A restart invalidates outstanding reservations.** The send answers `409` (`hub.ErrUnknownMint`);
  the client re-mints under the same key, gets a fresh sequence, re-signs, re-sends. Safe against
  double-apply: if the crash landed after durability, the same key and fingerprint make
  `internal/idem` answer `OutcomeRetry` with the ORIGINAL result.
- **Sequence numbers now advance in jumps** — a restart typically skips to the next multiple of 256,
  and an unspent mint leaves a permanent hole. **Correct, not corruption:**
  `internal/ids/sequence.go` already binds consumers to treat the sequence as strictly increasing,
  never as dense. Invariant 1 beats gap-freeness.
- **One extra fsync per 256 mints**, and zero on the send path itself. A block of 1 would never burn
  a hole and was rejected as the default for doubling the fsync cost of invariant 4's guarantee to
  buy nothing invariant 1 does not already get from a larger block.
- Bounds fail CLOSED (`MaxOutstandingMintsPerAgent = 64`, `MaxOutstandingMints = 8192`,
  `MintTTL = 15m`); another agent's reservation is **never** evicted to make room.

### Decision 4 — `POST /v1/broadcast` answers 501, and this is a REGRESSION we chose

A broadcast **cannot be signed** under signing format v1, for two reasons that are both deliberate
and neither of which the route may paper over: `signing.Canonicalize` REJECTS an empty recipient set
(an empty set signs an audience of nobody), and `store.Message` stores a broadcast as a **FLAG**, not
an expanded roster snapshot, so there is no recorded audience to canonicalize even if the format
allowed one. What the canonical audience of a broadcast should BE is SIGN-3's open question, and
answering it here by accident would settle it for everybody.

Two answers were available: leave the route accepting UNSIGNED messages — precisely the "strip the
signature and the epic is theatre" hole SIGN-6 exists to close, and it would be the easiest hole in
the bus to find — or **fail closed**. **We fail closed.** The refusal is made immediately after
authentication and BEFORE the body is decoded, so no broadcast payload is read, parsed, logged or
measured on a route that cannot accept one. An anonymous caller still gets 401 first: a route that
told an unauthenticated caller what it does and does not implement would be describing the messaging
surface to somebody with no business knowing it exists.

**What it costs.** A working, shipped, agent-facing feature stops working. `busctl broadcast` fails
(exit 6, because the client maps `>= 500` generically). This is a REGRESSION and must be reported as
one, not as a limitation. `hub.Broadcast`, `client.Broadcast` and the whole broadcast write path are
**deliberately left INTACT and tested**, so SIGN-3 re-opens the route by settling ONE question rather
than by re-plumbing the write path.

### Decision 5 — `store.RecordVersion` 1 → 2, a destructive BIDIRECTIONAL break, with NO migration

`store.Record` gains REQUIRED `timestamp_ms` and `signature`. `RecordVersion` becomes **2**, reserved
from the Spec Server `store-record-version` namespace on 2026-08-07 (value `1` seeded in the same
pass to cover the already-shipped v1 record) — not picked by eyeballing the constant.

**Why there is no migration, and why one must not be invented.** A pre-SIGN-6 message is unsigned.
There is nothing to migrate it TO. Synthesising a zero signature or a fabricated timestamp would
manufacture records that look signed and verify as nothing — the silent-failure mode invariant 9
exists to forbid. So `store.Decode` refuses any record whose `v` is not exactly `RecordVersion`, and
`Hub.Apply` discards refused records **loudly** (invariant 6: discard is sanctioned, SILENT discard
is the defect).

**What it costs — say this to operators before they upgrade.** Upgrading an existing bus **discards
its entire message history**. The break is **BIDIRECTIONAL**: rolling back to the old binary discards
the v2 records the same way, so rollback is a second discard, not a recovery. Copy the data directory
first if the history matters. **Enrolment (`"agent"`), invite and `"seqfloor"` records are NOT
affected** — `auth.RecordVersion` is a separate, independently-versioned number and stays at `1` — so
**no agent has to re-enrol because of this bump.**

### Re-affirmed from the superseded entry, and now actually shipped

- **A rejected send consumes no sequence and leaves no durable record.** Every SIGN-6 check runs in
  `internal/httpapi` BEFORE `hub.Send` is called: no WAL record, no delivery, no ack, no sequence
  consumed by that request. (The reservation was burned earlier at `/v1/mint` — a separate,
  deliberate, earlier act.) **A rejection is TERMINAL for its idempotency key**, carries no
  `Retry-After`, and must not be retry-looped: re-presented unchanged it fails identically for ever;
  re-presented repaired it is a different payload under a used key, which invariant 10 answers with
  409 and a disconnect.
- **The poison-message wedge policy stands** (cursor ADVANCES, body DISCARDED, event RECORDED
  LOUDLY). It is encoded in `client`'s `RejectionReason`/`RejectedMessage` types, and it is **not yet
  in force** — see the honest limits below.
- **Which key verifies** — resolved from the fully-qualified sender INSIDE the signed bytes, never
  from a key or key-hint carried beside the signature.

### What is still BLOCKED — do not read this entry as "signing is done"

- **No messaging public key is registered at enrolment.** `auth.Service.Enrol` leaves
  `RosterEntry.MessagingPublicKey` ZERO, `GET /v1/agents` carries no key material, and **CRYPTO-4
  (the server-attested key-bundle endpoint) does not exist.** A recipient can therefore obtain a
  sender's messaging public key **only OUT OF BAND**.

  > CORRECTED 2026-08-16 — HALF of this is now false and the half that matters is not. The key IS registered at
  enrolment: `EnrolRequestBody.MessagingPublicKey` is decoded at `internal/httpapi/auth.go:226-228`,
  passed at `:348`, validated at `internal/auth/service.go:662` and written into the roster entry at
  `:799`. It is OPTIONAL — an empty value stays acceptable (`:651`) — and that is what
  `RELAY-24-BLOCKER-EGRESS` decision 2 relies on when it declines to attest a keyless agent. What has
  NOT changed is the retrieval half: `GET /v1/agents` still carries no key material at all
  (`AgentInfo` is `{agent_id, name, enrolled_at}`, `internal/httpapi/messages.go:112-116`) and
  CRYPTO-4's server-attested key-bundle endpoint still does not exist (`client/keyring.go:20`). So a
  recipient still obtains a sender's messaging key ONLY OUT OF BAND, and the prohibition below — no
  TOFU, no "trust the key the bus handed over", no verification-optional switch — is untouched.

  `client/keyring.go`'s `DirKeyRing` is a local, **manually populated** trust store and is explicitly
  a stopgap. **No fallback may be invented** — no TOFU, no "trust the key the bus handed over", no
  verification-optional switch, no `--insecure`.
- **Recipient-side verification is NOT wired into `client.Read`.** Signing works end to end, the
  signature is carried on the wire and returned by the read path, and a client-made signature is
  proven to verify under `internal/signing.Verify` from the wire fields — but nothing verifies
  automatically on receive. `Batch.Rejected` is always empty. `client/messages.go`'s doc comment
  calling `Batch.Messages` "the VERIFIED messages" is **FALSE today** and is recorded as a code-doc
  defect.
- **`busctl keygen` and `busctl trust` DO NOT EXIST**, though error remedies in `client/store.go`,
  `client/client.go` and `client/keyring.go` instruct operators to run them. An agent that shells out
  to `busctl` therefore cannot publish its messaging key or trust a peer's; only an embedder can.
  Recorded in `CONTRACTS-AGENT.md` as an open invariant-7 item, not as a satisfied requirement.

---

## 2026-08-07 — AUTH-7: durable enrolment is WIRED, and the hub keeps NO roster of its own

**Decision.** `cmd/agent-bus` constructs `auth.NewWALRoster`, attaches it to the WAL, and injects it
into `auth.NewService` and — adapted through the new `cmd/agent-bus/hubroster.go` — into the hub.
`hub.NoteEnrolment` and the hub's private roster map are **DELETED**; the hub reads through to the
authoritative roster via a new `hub.RosterSource` interface, and `hub.Options.Roster` is **REQUIRED**
(nil is a hard error at `Open`).

**Why one roster, read through, and not a synchronised copy.** The hub used to keep a second roster
fed by the enrolment handler. After a restart that copy came back EMPTY while `auth`'s durable one
came back full — **a bus that authenticated everyone and served nobody.** One roster read through
cannot diverge from itself. A snapshot taken at startup would reintroduce the same bug with a
different cause: every agent enrolled after boot would authenticate and then be refused as an unknown
sender. So `hubRoster` reads through on every call and caches nothing.

**Why the adapter lives in `cmd/agent-bus` and not in either package.** `internal/hub` must not
import `internal/auth` — the hub would then need the enrolment authority, the id minter and the
session table to build a message store, and the dependency runs the wrong way round, since auth
issues the identity the hub consumes. `internal/auth` must not import `internal/hub` for the mirror
reason. The composition root is the one place that legitimately holds both, so the translation is
fifteen lines there rather than a cycle anywhere else. It wraps the INTERFACE `auth.Roster` rather
than `*auth.WALRoster`, so a test's `MemoryRoster` needs no second adapter and nothing in the hub can
reach a durable-write method.

**Why `Options.Roster` is a hard error rather than defaulting to an empty roster.** A hub with
nothing to read refuses every send, rejects every recipient and serves an empty agent list **while
looking healthy**. Failing at `Open` turns a silent, hours-later mystery into a startup error.

**What this means for an operator — the headline.** **Agents no longer re-enrol after a restart.**
Agent ids, public keys and each agent's **ORIGINAL** `enrolled_at` survive a restart and a `SIGKILL`,
fsynced through the two-phase write path and rebuilt by replay. The enrolment-epoch visibility rule
(`CONTRACTS-HTTP.md`) now behaves as designed: a genuinely continuous agent keeps seeing everything
sent since it first enrolled, across restarts. Verified end to end — enrol two agents, `SIGKILL`,
restart, both authenticate with their existing credentials and `/v1/agents` lists both.

**What it costs — SESSIONS are still memory-only, and that is permanent, not a follow-up.** Every
bearer token is invalidated by a restart, so each agent must redo the session handshake before its
first authenticated call. It must **not** re-enrol. Persisting sessions is deliberately NOT planned:
they are short-lived credentials, and writing live ones to disk would store **replayable material**
to buy exactly one saved round trip. The startup WARN was rewritten to say precisely this — the old
one claimed the roster was lost, which is now the opposite of the truth, and a false reassurance in
either direction on this line is worse than no line.

---

## 2026-08-07 — Four rulings: refuse-to-boot exception, format break, binary rename, redeploy

### 1. A missing floors file is FATAL — a named exception to invariant 6

A data directory with history but no `agent-suffixes` file **refuses to boot**. Remedy: restore the
file, or restart once with `-backfill-suffix-floors`.

This deviates from invariant 6 (always-restart) deliberately, and the carve-out is the same one
already accepted for `wal-mac.key`: **always-restart exists so MEDIA DAMAGE cannot hold the bus
hostage, not so MISCONFIGURATION is survived silently.** A deleted floors file is the latter — it is
operator error or an attack, it is recoverable in exactly one restart, and starting anyway would
resume every agent name from suffix 1 and re-mint ids that are live, violating invariant 1, which
was reaffirmed WITHOUT narrowing.

Note the asymmetry this fixes: a *tampered* floors file already failed closed, while a *deleted* one
failed open. Corruption hit an explicit error path; deletion hit a "backfill" path that looked
correct and quietly under-counted. Failing closed on both is the point.

**Consequence:** `CONTRACTS-ONDISK.md` still states unqualified *"Recovery ALWAYS reaches a running
server"*. That is now false and must name this exception, or an operator meeting the refusal has no
document to search.

> CORRECTED 2026-08-16: that exact unqualified sentence no longer appears in `CONTRACTS-ONDISK.md` (grepped
2026-08-16, zero matches). The exception itself still holds in code: a data directory with history
and no `agent-suffixes` file refuses to boot unless `-backfill-suffix-floors` is passed
(`cmd/agent-bus/suffixfloors.go:184-186`; flag at `cmd/agent-bus/main.go:300`).

### 2. The `store.RecordVersion` 1→2 break is ACCEPTED

Existing v1 message records are discarded at recovery, and a rollback discards v2 the same way. **No
migration is possible** rather than merely unwritten: signed messages have nothing to migrate *from*,
and accepting unsigned legacy records would reintroduce exactly the downgrade SIGN-6 exists to
forbid.

**Enrolments and invites are unaffected, so no agent re-enrols.** Only message history is lost. No
production data exists; the only affected directory is the local test deployment.

### 3. The CLI binary is renamed `busctl` → `agent-busctl`

`busctl` is systemd's D-Bus tool and would shadow it on `PATH`. Cheap now, breaking once anyone
scripts against it. The rename touches `cmd/`, every doc that names it, and `CONTRACTS-CLI.md`'s
contract surface.

### 4. The running deployment is redeployed fresh

The Compose deployment predates messaging, signing and the durable roster, so it cannot carry a
message. The format break above makes its volume unusable regardless, and its contents are throwaway
test state — so a fresh teardown including the volume, rebuilt from current HEAD.

---

## 2026-08-07 — CLI-1-FU-BINARYNAME shipped: `cmd/busctl` → `cmd/agent-busctl`, closing the SS1 open question

This closes the open question raised in the "2026-08-02 — The client is `client/` + `cmd/busctl`"
entry above ("Known collision, flagged not resolved") and confirmed as ruling #3 in the preceding
"Four rulings" entry: `busctl` is systemd's D-Bus introspection tool, present in the base install on
Debian/Ubuntu/Fedora/Arch, so `go install ./cmd/busctl` or dropping the built binary on `PATH`
SHADOWS the system tool. Cheap to fix before anyone scripts against the name; breaking to fix after.
The rename is `busctl` → `agent-busctl`, mechanical, no behaviour change.

**What moved.** `cmd/busctl/` → `cmd/agent-busctl/` via `git mv` (history preserved). Every literal
program-name string that prints to a user — `--help` text, usage lines, error `Op`/`Remedy` fields,
the `flag.NewFlagSet` name, the stderr `%s: ` prefix in `output.go`, the package doc comment — renamed
alongside it, in both `cmd/agent-busctl/**` and every doc/comment in `client/**` that names the
binary by example (`` `busctl send` ``, `` `busctl enrol --bus ...` ``, etc.). `client/transport.go`'s
`userAgent` constant also moved to `"agent-busctl"`: it is informational only (no server-side code or
test keys on its literal value), so it is not a wire-compatibility concern, unlike the next point.

**What deliberately did NOT move: the idempotency-key prefix.** `client/enrol.go`'s
`newIdempotencyKey` mints keys prefixed literally `"busctl-"` — e.g. `busctl-47938fc0bbbb90f8c25d92fcd2043362`
— and that prefix is **wire-visible**: it is part of the key value the server durably remembers under
invariant 10 (idempotency, everywhere). Renaming it would not relabel a display string, it would
change the identity of every key this client mints from this build forward — a client retrying a send
that straddled the rename would present a "new" key for what the server (and the operator) understand
to be the same logical retry. Left exactly `"busctl-"`, with a comment at the call site recording why.
The one documented example of the prefix (`AGENT_PROTOCOL.md`, "Retrying an ambiguous failure") is
likewise left as `busctl-<hex>`, with a note explaining the asymmetry to the reader.

**Docs updated to match:** `CONTRACTS-CLI.md`, `CONTRACTS-AGENT.md`, `AGENT_PROTOCOL.md`, `README.md`
— including the `go build -o .../agent-busctl ./cmd/agent-busctl` one-time-build instructions and
CLI-1's isolation proof clause (now `! go list -deps ./cmd/agent-busctl | grep -q
'agent-bus/internal/'`, reverified PASS). `Dockerfile` and `scripts/*.sh` were grepped and confirmed
to contain zero `busctl` references — no change needed there.

**Deliberately NOT touched (append-only / not owned):** `AGENT_LOG.md` and prior `DECISIONS.md`
entries are historical records of what was true at the time and keep saying `busctl`/`cmd/busctl`
where they described that state; `SPEC.md` and `CONTRACTS.md` are generated/owned elsewhere.

**Known stale reference, not fixed here (outside this task's ownership):** the recorded `proof_cmd`
on Spec Server task `f801d128-0317-4d38-a8bc-77588d44d63d` (DEPLOY-REDEPLOY) reads
`go build -o /tmp/busctl ./cmd/busctl`; the path is now stale. Flagged for spec-keeper to correct the
recorded proof command, not edited directly (task state is server-owned, never hand-edited). Also
noted, not fixed: `.gitignore`'s `/busctl` entry and a descriptive comment in
`internal/httpapi/composition_test.go` that names `busctl` — both outside this task's file-ownership
boundary.

> CORRECTED 2026-08-16: `.gitignore` now carries `/agent-busctl` and no bare `/busctl` entry. The one thing this
entry says must NOT move has not moved: `client/enrol.go:930-942`'s `newIdempotencyKey` still mints
the wire-visible prefix `"busctl-"`, with the comment recording why.

---

## 2026-08-07 — DISCOVERY-DOC: a separate `/v1/discovery` document, not a bigger `/v1/info`

**Context.** An agent that holds nothing but a bus URL has no way to learn how to enrol short of
reading source or being told out of band. The candidate fix was obvious and cheap-looking: fold a
protocol guide into `/v1/info`, which is already unauthenticated and already the first thing an agent
fetches.

**The case FOR extending `/v1/info`, stated honestly because it has real merit.** `/v1/info` is
already on the allow-list and already the first call a fresh agent makes; adding fields to it costs
no new unauthenticated route, no new entry on the allow-list, and no second thing for a client to
remember to fetch. Fewer routes is, all else equal, a smaller surface.

**Why it lost.** `/v1/info`'s exact field set is pinned by a security test *precisely because* that
endpoint's growth was already judged a risk (`healthz_info_test.go`, see the original 2026-08-02
entry this addends: "do not add data-dir, listen address, peer list, or agent roster here without
updating that test and recording the decision"). Folding a multi-kilobyte protocol guide into it
would force that pin to do one of two things, both bad: cover a large nested structure (steps,
endpoints, enrolment, session, client, limitations — ten fields including four objects and three
arrays), which buries the security-relevant assertion ("this unauthenticated endpoint leaks nothing
about bus state") under wording churn that has nothing to do with security; or go slack and stop
being exhaustive, which is the exact failure mode the pin exists to prevent. Every future wording
change to the protocol description — a clearer sentence in `steps`, a new `limitations` entry — would
then be an edit to a security-sensitive test, which is the wrong incentive: it either discourages
improving the wording or trains reviewers to rubber-stamp diffs to that test.

It also conflates two different response profiles that have nothing to do with each other.
`/v1/info` changes on every call (`uptime_seconds`) and is meant to be polled cheaply and often — a
liveness/version probe. The discovery document is static for the life of the process and cacheable
indefinitely by a well-behaved client. Bolting the second job onto the first would make a probe
response large and repetitive, or force a cache-control story that only one field of the response
actually wants.

**Decision.** Two endpoints, two jobs, two independent exact-field-set pins:

- `GET /v1/info` keeps its three original fields (`bus_id`, `version`, `uptime_seconds`) and gains
  exactly ONE new field, `discovery`, whose value is the compile-time constant
  `httpapi.RouteDiscovery` (`"/v1/discovery"`). It costs the field-set pin one entry, not a nested
  structure, and the entry is safe to pin forever because its value cannot change: it is a string
  literal, not a read of anything.
- `GET /v1/discovery` is a new, separate, unauthenticated, bounded (test ceiling 16 KiB, observed
  ~6.1 KB), STATIC document. It gets its own exhaustive field-set pin
  (`TestDiscoveryFieldSetIsPinned`, `discovery_test.go`), its own DoS/leak-bound test
  (`TestDiscoveryDocumentIsStatic`, byte-identity across differently-configured servers), and its own
  leak guard (`TestDiscoveryDocumentLeaksNoBusState`) — independent of `/v1/info`'s tests, so a wording
  change to one document's prose never touches the other's security assertions.

`/v1/info` is still reachable from every caller that only knows it: the new `discovery` pointer field
is proven to resolve (`TestDiscoveryPointerOnInfo` follows it end to end), so nothing that used to be
learnable from `/v1/info` alone becomes unreachable.

**Two supporting decisions, made in the same change:**

1. **The `endpoints` list inside the discovery document is STATIC, not derived from the mux.**
   `discovery.go`'s `discoveryEndpoints` is a package-level `[]DiscoveryEndpoint` literal, never a
   projection of `(*Server).Routes()` / `s.routes`. Deriving it from the mux would leak whether
   `Options.Hub` and `Options.Auth` are wired — exactly the configuration enumeration that
   `authMiddleware`'s 401-not-404 behaviour on unregistered paths already exists to withhold (see the
   AUTH-2 entry). A static list describes the protocol the software speaks, in every build, and
   reveals nothing about how this particular instance is configured. `TestDiscoveryDocumentIsStatic`
   proves this by byte-identity across five differently-wired servers — including one with the full
   messaging surface registered and one without — rather than by field-by-field inspection, because
   byte-identity is the only check that catches a field nobody thought to look for.
2. **The document returns relative paths and deliberately does not echo a self-URL.** The `Host`
   header on an inbound request is client-supplied, not server-verified; a document that reflected it
   back as "here is my base URL" would let an attacker on the network path point a reader at a bus of
   the attacker's choosing while still naming this bus's real protocol shape. `paths_are_relative_to`
   states this explicitly rather than leaving a reader to assume a self-URL exists.

**Consequences.**

- Two exact-field-set tests exist where one might have sufficed, and both must be maintained. That is
  accepted as the cost of keeping a security-sensitive pin narrow and a protocol document freely
  editable.
- The discovery document's `enrolment.invite_required` is `false` as of this build — enrolment is
  genuinely open, not invite-gated, and the document says so plainly rather than pre-announcing a
  control that does not exist yet (`TestDiscoveryEnrolmentIsHonest` pins this and records that the
  flip to `true` must land in the SAME task as the invite gate itself, never before it).

  > CORRECTED 2026-08-16: the flip happened, in the task that shipped the gate, exactly as this bullet required.
  `invite_required` is no longer a constant — it is computed from `auth.Service.InviteRequired()`
  (`internal/httpapi/discovery.go:111`) and is `true` on every shipped build
  (`enrolmentInviteRequired = true`, `cmd/agent-bus/main.go:66`, `3cedcb7`, 2026-08-15).
  `TestDiscoveryEnrolmentIsHonest` still exists but is now NARROWED to the no-auth-service test server;
  the true/false honesty property is pinned by `TestInviteGateAdvertisesInviteRequired`
  (`cmd/agent-bus/invitegate_enforce_test.go`). The two-field split survives: `invite_accepted` is
  separate (`discovery.go:116`), for the reason the 2026-08-14 INVITE-GATE entry §(d) gives.
- The endpoint list omits `POST /v1/broadcast` (it answers 501) and states that honestly in
  `limitations` rather than advertising a route that refuses everything.
- `auth.SessionSigningContext` is never served in the document, by design (see invariant 3 and the
  "Enrolment and sessions" section of `CONTRACTS-HTTP.md`): a client must pin the domain-separation
  prefix at compile time, not learn it from the server.

---

## 2026-08-07 — MTLS-PIN: the client pins the bus's certificate, and `InsecureSkipVerify` gets exactly one home

**Task:** `MTLS-PIN` (`8c46dc93-16d0-4eea-8ad3-ac51136551e2`).

**STATUS OF EVERYTHING BELOW — read before quoting it.** This records the DECISIONS taken by that
task and describes what its change DOES; it is not a claim that any of it is running. The change is
**CODE-ONLY**: the bus does not serve TLS at all yet (`MTLS-LISTENER`), so no agent-bus deployment
anywhere exercises a single line of it, and none can until the listener ships. Read "the client
refuses X" throughout as "the client, as written in `client/`, refuses X" — a statement about the
source, never about an observed production behaviour.

### 1. Pinning lands BEFORE the TLS listener, on purpose

This inverts the obvious order and the reason is a finding, not a preference. The security gate
showed that overwriting all three key files in an established data directory makes the bus start
**cleanly, with a different fingerprint, and no warning at all**. Key **loss** is loud — the bus
refuses to regenerate over a partial set. Key **substitution** is silent.

The fingerprint the bus mints (`MTLS-BUSCERT`, commit `16f54c9`) is therefore worth nothing as a
defence until something checks it. Shipping the listener first would mean a window in which the bus
serves TLS, publishes a fingerprint, and no client compares it against anything — which reads as
"we have mTLS" while providing the confidentiality of TLS and none of the identity. The checker
lands first; the listener may then land into a client population that already refuses an
unrecognised certificate.

### 2. `InsecureSkipVerify: true` is now permitted in EXACTLY ONE FILE — `client/pin.go`

This reverses the absolute form of the 2026-08-02 rule ("E7: no plaintext escape hatch"), which
banned the literal from `client/` and `cmd/agent-busctl/` outright, and it needs stating plainly
rather than being buried in a test.

**Why the absolute ban could not survive contact with the requirement.** Invariant 11 specifies
self-signed certificates, **no CA**, and **no trust-on-first-use**. Go's default chain verification
therefore cannot succeed and cannot be configured to: there is no root to chain to, and the client
holds a 32-byte fingerprint rather than the certificate, so it cannot construct an `x509.CertPool`
either. `crypto/tls` offers exactly one supported way to substitute a verification policy — disable
the default chain check and supply `VerifyPeerCertificate` — and the field's own documentation
describes that pairing. A ban with no exception would not have prevented the exception; it would
have pushed it into a package the guard does not scan, which is strictly worse than one loud,
reviewed occurrence.

**What replaces the ban is stricter, not looser**, and it is mechanical (`client/guard_test.go`):

- `TestNoInsecureSkipVerifyAnywhere` — the literal appears in **exactly one file and exactly once**.
  Not "rarely": once, counted. Naming it in prose in that file is a failure too, so the count stays
  a count.
- `TestPinnedSkipIsAlwaysPairedWithAPinCheck` — an **AST** walk, not a grep, so it can see structure:
  any composite literal setting it `true` must set `VerifyPeerCertificate` non-nil **in the same
  literal**; setting it by **assignment** is banned outright (an assignment can be conditional and
  far from the literal); and **at least one such paired literal must exist**, so the guard cannot
  pass on a tree where pinning was deleted.
- `TestClientHasNoInsecureVerificationFlag` — no flag, env var, `Config` field or constant may be
  NAMED for weakening verification. Invariant 11 forbids a flag that does it *silently*; we read
  that as forbidding the flag at all. A documented hole is not better than a hidden one, it is a
  hole with a manual.

The thing being guarded is one line wide and completely silent: a `tls.Config` with the callback
**deleted** still compiles, still completes handshakes, still returns working connections, and
verifies nothing. Every positive test passes either way. Only a negative test and an AST guard tell
them apart — hence `TestClientRefusesChangedBusFingerprint`, which swaps the certificate under a
fixed address and asserts the bus receives **zero** requests afterwards.

**What is given up** by disabling the default check, stated exactly: CA chain building (there is
none by design) and **hostname verification** — for which the pin substitutes and is strictly
stronger, since a name check asks "does this certificate claim this address" and the pin asks "is
this the exact certificate the invite named". **Certificate expiry and validity are NOT checked**,
and that is a real gap owned by `MTLS-VERIFY`, recorded here rather than left implicit.

### 3. No TOFU — an `https` bus with no pin is REFUSED, and `http` with a pin is refused too

Both directions fail closed. The first is the invariant. The second is less obvious and matters as
much: a caller who passed `--bus-fingerprint` believes it is being checked, and a plaintext
connection has no certificate to check it against, so honouring the flag silently would manufacture
exactly the false confidence this task exists to remove.

The pin is **never derived from the certificate the bus presented**. `enrol` records only the
fingerprint that was already in force for the connection that succeeded. Deriving it would be
trust-on-first-use wearing the costume of a stored pin.

### 4. A flag/store disagreement is a REFUSAL, not a precedence question

Everywhere else in this client the order is flag → env → stored identity → default. For the
fingerprint it is not: when an explicit `--bus-fingerprint` and the stored identity name **different**
certificates for the **same** bus, the command fails and prints both.

Applying precedence here would be the documented rule and the wrong answer. "It stopped working, so
I passed the fingerprint the other end gave me" is the exact sequence by which a substituted
certificate gets accepted, and precedence would convert a **detected** substitution into a
successful one. Recovery from a genuine rotation is deliberately manual and out of band: confirm
`bus_cert_fingerprint=…` on the bus host, then `logout` and re-enrol. Rolling two certificates during
rotation (invariant 11) is a separate, unwritten task.

### 5. `client.BusFingerprint` mirrors `internal/buscert.Fingerprint` rather than importing it

Invariant 7 forbids `client/` from importing `internal/` — an embeddable client that drags an
`internal/` path behind it is not embeddable. The construction (`sha256` over the **leaf's DER**,
lowercase hex, one spelling) is duplicated with a comment naming the server-side definition, under
the same rule as `SessionSigningContext` and `client/canonical.go`. Divergence fails **closed**: if
the two ever disagreed about how a certificate is hashed, no pin would match and every connection
would be refused. Nothing is accepted by accident.

### 6. The transport is now built lazily, per (bus, pin)

`CONTRACTS-CLI.md` previously carried this as a known defect: "the transport is built before the
identity is resolved… the seam is in the right place, at the wrong time". It had to be fixed here,
because the pin may come from the selected identity and so is not a function of `Config` alone.
`Client.endpoint()` resolves the URL and the pin **together** — an address without its pin is the
input to a TOFU connection, and separating them is how a caller ends up with one and not the other —
and `Client.doer()` builds and caches an `http.Client` keyed on the pin. `enrol`, `use` and `logout`
drop it, so no pooled connection verified under one identity's pin is ever reused under another's.

### 7. Earlier entries in THIS file that are now superseded

`DECISIONS.md` is append-only, so the following remain on the page and are no longer accurate as
written. Nothing above them has been edited; this is the pointer a future reader greping for
`InsecureSkipVerify` needs:

- **"`InsecureSkipVerify` must appear nowhere in the tree, including tests. Worth a grep in CI"**
  (2026-08-02, "E7") — superseded by §2 above. The correct rule is now: **exactly one occurrence, in
  `client/pin.go`, paired with `VerifyPeerCertificate` in the same literal**, enforced by AST rather
  than by grep. A grep-based CI check would now be the wrong check: it cannot see the pairing, which
  is the part that carries the security property.
- **"`InsecureSkipVerify` is not set, is not reachable through `Config`"** (2026-08-02, the transport
  seam note) — the second half still holds and is now guarded by name
  (`TestClientHasNoInsecureVerificationFlag`); the first half does not.

Unchanged and NOT weakened: there is still no flag, environment variable or `Config` field that
disables verification, and never disabling verification "to make something work" (CLAUDE.md
invariant 11) is exactly what §2 is: the default check is not removed to get past an error, it is
replaced by a stricter one because the default check is inapplicable by design.

**Note for `MTLS-CLIENTAUTH`:** its stored `proof_cmd` also names a `TestNoInsecureSkipVerifyAnywhere`,
in `./internal/httpapi ./cmd/agent-bus`. That is a **different test in a different package** from the
one amended here, and the server side has a different answer available to it — `tls.RequireAnyClientCert`
performs no chain verification without needing the field at all. Do not copy this exemption across.

### 8. Corrections and gaps the security gate found (same task, recorded here rather than quietly fixed)

The gate returned **PASS** (0 critical, 0 high). Three things it raised change what §2 above may honestly
claim, so they are recorded beside it rather than only in a backlog:

- **"The pin is STRICTLY STRONGER than hostname verification" is true only under an assumption.**
  The assumption is **one certificate per bus**. If a single certificate were ever served by two
  buses, a hostname check would distinguish them and a fingerprint would not. Nothing in this design
  does that, and rotation is the opposite case (two certificates, one bus) and is safe — but the
  claim is conditional and is now stated as conditional in `pinnedTLSConfig`'s doc comment.
- **Certificate EXPIRY is not checked, and that is a live gap in a recorded control, not merely
  future work.** This file already chose a 365-day certificate lifetime explicitly as *"a
  leak-containment bound"*. Only the client can enforce that bound on the BUS's certificate, and this
  change does not: the gate demonstrated that a certificate whose `NotAfter` is a day in the past is
  pinned, accepted, and enrolled against. It is not exploitable until `MTLS-LISTENER` — but that is a
  **sequencing requirement, not an excuse**: `MTLS-VERIFY` must land with or before the listener, or
  the 365-day lifetime is decoration. The gap is noted at the callback that would do the check, not
  only here.
- **The single-pin store contradicts E3's two-certificate rollover.** E3 says the bus serves two
  certificates during rotation *"so no client is ever forced to re-enrol on routine rotation"* — and
  the recovery path this task ships is exactly logout-and-re-enrol, because an identity stores one
  pin. That is acceptable while no bus serves TLS and while rotation has no implementation, and it is
  **not** acceptable at the point `MTLS-LISTENER` ships: the first real rotation would wedge a fleet,
  and a wedged fleet is how "just let the flag win" gets argued for. The fix is a SET of accepted
  pins, added only by a deliberate explicit action — never learned from a handshake. Filed as a
  tracked task gating `MTLS-LISTENER`, not left as a sentence.

  > CORRECTED 2026-08-16: the SET shipped, as `MTLS-ROTATE`. `client.BusPinSet` is the stored value, the field is
  `bus_fingerprints` (`client/store.go:128`), and it is capped at `client.MaxBusPins = 2` — the width
  of a rollover, not headroom (`client/pinset.go:29`). Pins are still never learned from a handshake.
  The cap is now load-bearing a second time: `cmd/agent-bus/relaydial.go` REFUSES a dial address whose
  configured next-hop pins exceed it, rather than truncating (2026-08-15, RELAY-24-BLOCKER-EGRESS gate
  findings).

One gate finding was **incorrect and is recorded as such** so it is not actioned later on faith: the
gate reported an `InsecureSkipVerify` in `internal/relay` that the guard does not scan. There is no
occurrence anywhere under `internal/` or `cmd/` — after this change the string exists in exactly two
files, `client/pin.go` (once, the field) and `client/guard_test.go` (the guard that counts it).
Widening the guard's roots to `internal/` is still worth doing, but as belt-and-braces, not as a fix
for something that is there.

## 2026-08-07 — The durable WAL index floor is authenticated with a KEYED MAC, and version 4 stays backward-compatible (db350e39, security P1-1 + P1-2)

The security gate on the uncommitted `internal/wal` index-floor work returned **CHANGES-REQUESTED**
with two P1 blockers. Both were proved executably rather than argued, and both are fixed here. Three
decisions came out of the fix that are not obvious from the diff, so they are recorded.

### 1. The floor file is HMAC-SHA256 under the data directory's own `wal-mac.key`

The floor carried an UNKEYED SHA-256, justified in a comment as "an attacker with directory write
access can read `wal-mac.key` anyway". **That justification defends forging log RECORDS and does not
defend this.** The gate demonstrated the attack with NO KEY AT ALL: flip `sealed 0` to `sealed 1`,
recompute the digest by hand, touch `bus.wal` not at all. Measured on our own re-run: **2268 of 2289
truncation offsets reissued an already-issued index; 0 refused to open.** Every frame the bus then
wrote carried a VALID MAC, because the server itself computes it — so the corruption is invisible to
everything downstream, which is the property that makes it worse than data loss.

CLAUDE.md invariant 6 is not ambiguous about this: integrity here is `crypto/hmac` + `crypto/sha256`,
**never a CRC or any other unkeyed or linear checksum**. A CRC was removed from this codebase once
already, after a remote client was shown able to forge one. The same argument applies verbatim, and
the seal made it load-bearing: `sealed` is now a first-class TRUST DECISION read from a file on disk.

**Construction (invariant 9 — never write our own crypto).** Nothing was invented. The tag is
`hmac.New(sha256.New, key)` with the covered bytes written to it — the SAME pattern
`internal/wal/format.go`'s `codec.mac` already uses for every frame — verified with `hmac.Equal`,
never `==` and never `bytes.Equal`, because a tag comparison that leaks timing is a forgery oracle.
**No domain-separation scheme was designed.** The tag covers `agent-bus-wal-index-floor v4\n` plus
the body — the whole file except the tag field — which binds it to the format version (a future v5
body cannot be replayed as v4) and gives structural separation for free: this input begins with ASCII
magic, while a frame MAC's input begins with a 4-byte big-endian payload length bounded by
`MaxPayloadSize` (1 MiB), so no frame the server will ever MAC can share the prefix.

### 2. Version 4 accepts the two shapes that already exist, and reads them CONSERVATIVELY

The `sealed` line was added to v4 without a version bump, on the stated premise that "v4 never
shipped and no data directory in the world legitimately carries a two-line body". **The premise was
false.** `f56c723` is in `main` and its `encodeIndexFloor` writes exactly a two-line v4 body. A
routine upgrade therefore hit `ErrIndexFloorCorrupt`, the bus refused to start, and the operator was
pointed at a remedy that reissues ids. That is a live-deployment break, not a theoretical one.

So v4 reads three shapes and writes one:

| shape | written by | read as |
| --- | --- | --- |
| `hmac-sha256=` + 3-line body | current | authenticated; `sealed` TRUSTED |
| `sha256=` + 3-line body | the same-day pre-HMAC revision | digest verified; `sealed` FORCED FALSE; WARN |
| `sha256=` + 2-line body | `f56c723`, in `main` | same (no `sealed` line exists to read) |

**The version number is deliberately NOT bumped**, and not only because these numbers are reserved
through the Spec Server (5 is already taken by `internal/hub/seqfloorfile.go`). The version field
defends exactly one thing — an older binary reading a newer layout into a LOWER floor — and neither
older shape can do that.

**Why a legacy file's `sealed` bit is DISCARDED rather than believed.** The bit's entire meaning is
"a run reached `Writer.Close`, so `written` is EXACT". An unkeyed digest is recomputable by anyone who
can write the file, so it cannot support that claim. The cost of discarding it is at most ONE burned
reservation block (63 indices) on the first start after upgrade, appearing as a legal, permanent hole.
The cost of believing it is invariant 1. `reserved` and `written` from such a file ARE still used,
and that asymmetry is deliberate: both are consumed only as a RAISE, so a forged value costs
availability — loudly — and never a reissued index.

### 3. An UNVERIFIABLE floor is read unauthenticated and logged at ERROR; it is not fatal

Keying the file creates a state that did not exist before: the key is gone, recovery mints a new one
(`macKeyFor` permits that only where the log provably holds no readable record), and **nothing the
previous identity wrote can verify — floor included**. That is a re-founded data directory, not a
damaged floor.

Refusing there would brick a bus over a lost key in a directory recovery has already decided may be
re-founded, which is what invariant 6 forbids. So the file is read WITHOUT authentication: the
numbers are kept (raise-only, so they can only make the start MORE conservative), `sealed` is
discarded, and it is logged at **ERROR**, naming the key path and saying to stop and restore the key.

**Is this a way in? The obvious attack is "delete `wal-mac.key` to force the unauthenticated path,
then supply a LOWERED `reserved`".** It does not widen anything, and the reason is worth writing
down rather than re-deriving: that path is only reachable when `macKeyFor` was willing to mint a key,
and `macKeyFor` refuses exactly when the log POSITIVELY IDENTIFIES ITSELF as format version 2 and is
longer than its own file header. So an attacker who deletes the key must ALSO destroy the log to
reach the unauthenticated read — and an attacker who can do that could simply delete the floor file
instead and get the same result with less work. The floor is never the weakest link in that scenario.

The honest limit, stated rather than buried: an attacker who can forge that file can equally DELETE
it, and no MAC can prevent deletion. **What the MAC buys is that neither is SILENT any more** — the
forgery that used to succeed quietly now either fails loudly under the directory's own key, or is
read unauthenticated with an ERROR line naming the key. That is the whole of the improvement and it
is not claimed to be more.

### 4. The floor is read AFTER the MAC key is settled, not before

`wal.Open` used to read the floor first, "before a byte of the log is examined". Once the file is
keyed that ordering is wrong and actively dangerous: a merely WRONG key made the floor fail its tag,
so the operator was told the FLOOR was corrupt and pointed at deleting it — when nothing was wrong
with the floor and the fix was to restore the key. Two failures with opposite remedies must not
collapse into the more destructive one.

The floor is now opened immediately after `repairLog`, by which point `macKeyFor` has ruled on a
missing key and `repairLog` has raised `ErrMACKeyMismatch` for a wrong one.

**The move NARROWS the misdiagnosis; it does not eliminate it, and both gates measured that.**
`ErrMACKeyMismatch` is raised only for a log that identifies as version 2 AND is longer than its own
file header. A wrong key over a log with no readable record — a bare 48-byte header, which is what a
fresh clean run *and a post-quarantine directory* both leave — still reaches the floor first. The
remaining gap is closed by the error TEXT rather than by ordering: `indexFloorCorrupt` now names
`wal-mac.key` as the FIRST thing to check, ahead of the remedy.

**The cost of the move, corrected after review:** a corrupt floor is now refused after `repairLog`
has run, and **`repairLog` DESTROYS BYTES** — `truncateAt` truncates permanently and the mid-file
rewrite renames a temp over the original; only a QUARANTINE preserves the file by renaming it aside.
An earlier draft of this entry claimed it "never deletes bytes without moving them aside", and a
reviewer measured that false (839 bytes before a refused `Open`, 789 after, nothing moved aside).
The decision stands on a narrower argument: the bytes `repairLog` removes are damage it has already
LOGGED before touching the file, and are bytes any successful start would have discarded anyway,
whereas the misdiagnosis cost an id space. The floor is also still opened BEFORE `replay`, so a
refusal can never leave the caller holding a partially rebuilt Applier.

### 5. The corrupt-floor error states the COST of its remedy

CLAUDE.md requires an error to name the remedy rather than the stack. The old text ended: *"delete
`<path>` and restart: the bus will then resume from the log's own high-water mark, which is correct
unless the log has ALSO been damaged or quarantined."*

**That caveat is unsound, not merely narrow** — and unsound for exactly the reason the `sealed` bit
exists. A truncation at a clean frame boundary is byte-for-byte identical to a legitimately shorter
log, so neither recovery nor an operator has any signal that would reveal it. The remedy asked the
operator to certify something nobody can know. Measured: floor deleted per that instruction, crash,
cut at a clean boundary — **2268 of 2289 truncation offsets reissued an index.**

A remedy that silently breaks the repository's most load-bearing invariant is worse than no remedy.
The text now names the remedy AND its cost: restore from a backup if you can; deleting the file
**FORFEITS INVARIANT 1 for that data directory** unless the previous run shut down CLEANLY, and if
you delete it, treat every WAL record index and message id the bus has issued as potentially
reissued. There is no "unless the log was damaged" escape clause.

### Also fixed here (P2s the gate raised, cheap and self-contained)

- **The temp-file reaper is no longer a glob.** `dir` came from `-data` and was interpolated
  UNESCAPED into a `filepath.Glob` PATTERN, so `-data /srv/bus[1]` reaped a SIBLING directory while
  missing its own, and `-data /srv/bus[` returned `ErrBadPattern` and disabled the reaper
  permanently. It is `os.ReadDir` + a prefix/suffix match derived from the same `os.CreateTemp`
  pattern `atomicReplaceFile` creates with, so the two cannot drift. A path is a path, not a pattern.
- **Severe discards are logged FIRST, within the same cap.** `logDiscards` capped at 16 in FILE
  ORDER, so sixteen trivial discards at the head of a file crowded out a dangling COMMIT further in —
  an acknowledged write that is now lost — and the whole recovery emitted no ERROR line at all.
  Invariant 6 permits discarding damage only because every discard is logged "loudly and
  specifically"; a cap that evicts the loudest message is the silent-discard defect (rated P0) in
  another hat. The cap itself stays, and the exact total is still reported.

### Deliberately NOT done, and filed instead

- `Recovered.addDiscard` still RETAINS only the first 64 discards, in file order, so a severe discard
  beyond the 64th is never even available to log. Fixing it needs an eviction policy, not a reorder.
- `persistLocked` checks monotonicity against IN-MEMORY state, not the bytes on disk. Two
  `*indexFloor` on one directory could lower a persisted `written`; guarded today by
  `internal/dirlock`.
- `reapStaleFloorTemps`' safety argument cites the data-directory flock, which `wal.Open` does not
  itself take (`cmd/agent-bus` takes it first).
- `parseIndexFloorLine` applies no plausibility bound to `reserved`, while `salvage.go` explicitly
  refuses an implausible index from an unverified frame header — an asymmetry worth closing.
  **The security gate sharpened this into the one place the "no worse than deletion" argument
  actually fails, so it is recorded precisely rather than as a tidiness item:** an *unauthenticated*
  legacy floor claiming `reserved = 2^64-2` needs no key; `log.go`'s end-of-index-space guard fires
  only at EXACTLY `MaxUint64`, so `Open` accepts it, and `begin` then **re-persists
  `reserved = MaxUint64` under a VALID HMAC**. Every later `Open` refuses for ever, and the file is
  now indistinguishable from a legitimate one, so the only remedy left is the one that forfeits
  invariant 1. Deletion cannot produce that state. The fix is a plausibility bound applied BEFORE
  an unverified value is re-signed.
- A wrong MAC key over a log with no readable record still surfaces as "corrupt floor" rather than
  as a key problem. Mitigated by the error text (which now names `wal-mac.key` first) but not
  structurally fixed.
- `readIndexFloorFile` reads the whole file with `os.ReadFile`, unbounded — a startup memory DoS on
  a hostile data directory. Pre-existing, and `clipFragment` already bounds what reaches the log.

---

## 2026-08-07 — The listener is TLS, and the guard that proved it wasn't is REPLACED, not deleted (MTLS-LISTENER + MTLS-VERIFY)

Two backlog tasks landed as ONE commit, at the user's explicit direction, so `main` is never left
with a bus that nothing can health-check. That pairing is the decision worth recording first: the
server-side switch (`MTLS-LISTENER`) and the probe rework (`MTLS-VERIFY`) are a single atomic change
in practice, because `scripts/bus-serve.sh`'s `http://` health probe is what every other task's
server-startup proof runs through. Landing the listener alone would have reported every healthy bus
as failed.

### 1. `tls.NewListener`, not `srv.ServeTLS`

`internal/buscert` has already loaded, parsed and validated the certificate and key by the time the
listener is built. `ServeTLS(ln, certFile, keyFile)` would re-read the same two files from disk — a
SECOND load of the same material, and a second chance for the two loads to disagree (over an
operator's mid-start `cp`, say). Wrapping the already-loaded `tls.Certificate` keeps exactly one
load, one fingerprint, and one thing that can fail, and it fails at startup rather than per
connection.

The TLS config is built BEFORE `net.Listen`. Unusable material must refuse the start without having
taken the port, so an operator restarting over a broken certificate does not also find the address
held by the corpse of the attempt.

### 2. TLS 1.2 floor, and it is deliberately the SAME number as the client's

`client/pin.go`'s `pinnedTLSConfig` sets `MinVersion: tls.VersionTLS12`. The server now sets the same
value. A server floor ABOVE the client's is a handshake failure with no useful message at either end,
and this repo's own client is not the only consumer — an operator's `curl --cacert` against
`/healthz` has to work too. 1.3 is negotiated whenever both ends offer it, which for every Go client
here is always.

ALPN is pinned to `http/1.1` because `tls.NewListener` + `Serve` does not configure HTTP/2 the way
`ServeTLS` does. Advertising only what is actually served is the honest option; the alternative
leaves a client offering `h2` to infer the answer from an empty ALPN result.

### 4. `TestCmdDoesNotServeTLS` is REPLACED by `TestCmdHasNoPlaintextListener`

The old guard failed the build if `ServeTLS|ListenAndServeTLS|tls.NewListener|TLSConfig` appeared
anywhere under `cmd/`. It was added on purpose during `MTLS-BUSCERT` to prove that commit did not
start serving TLS, and its own doc comment said to delete it in the task that legitimately made the
listener TLS. That task is this one.

It was NOT simply deleted. A deleted guard leaves the tree with strictly less protection than it had;
the temporary scaffold is instead converted into the invariant that is now permanently true. The
replacement asserts, by AST walk over the package's non-test sources, that:

- `InsecureSkipVerify` appears nowhere in `cmd/agent-bus` (it is permitted in exactly one file in
  this repo, `client/pin.go`, paired with `VerifyPeerCertificate` — narrowed 2026-08-07);
- TLS IS served, so deleting the `tls.NewListener` wrap fails the build rather than silently
  reverting the invariant; and
- **no registered flag could make TLS optional** — the guard collects every flag NAME registered on a
  `flag.FlagSet` in the package and rejects `tls`, `no-tls`, `insecure`, `plaintext`,
  `allow-plaintext`, `disable-tls`, `http` and friends. Invariant 11 says the server refuses to start
  rather than fall back; the fallback that would actually get written is a flag, so that is what the
  guard watches.

### 5. The container healthcheck moved INTO the server binary (`agent-bus healthcheck`)

The runtime image is Alpine with busybox `wget` and no curl — chosen in `DEPLOY-1` precisely so a
healthcheck would not need a second binary. busybox `wget` cannot be told to trust ONE self-signed
certificate. Its only relevant knob is `--no-check-certificate`, which does not verify differently,
it verifies not at all, and invariant 11 is explicit that certificate verification is never disabled
to make something work.

The three options were: add curl to the image (a dependency, and invariant 8 wants a justification
for one); drop the certificate check (forbidden); or put the probe in the binary that is already
there. The third costs nothing at runtime and gains something real — because it is a genuine x509
verification against the data directory's certificate as the sole root, it also enforces the
HOSTNAME and the VALIDITY PERIOD. `DECISIONS.md` chose a 365-day certificate lifetime as a
leak-containment bound; a probe that ignored expiry would let a bus no client can dial keep reporting
itself healthy for ever.

It is a subcommand on the SERVER binary rather than on `agent-busctl`, following the precedent set by
`invite mint` (E4): its input is FILESYSTEM ACCESS to the data directory, not a network privilege,
and no agent ever runs it. It differs from `invite mint` in one respect that matters — it takes no
lock and writes nothing, so it is safe and intended to run against a RUNNING bus.

`scripts/bus-serve.sh` keeps using `curl --cacert` rather than the subcommand, because it runs on a
workstation where curl is already a dependency of the script and the binary may not be built yet at
`status` time. Two consumers, two tools, each already present where it runs.

### 6. What this breaks, stated plainly

Every existing deployment is now reached at a DIFFERENT SCHEME. A plaintext request to the port is
not routed at all: crypto/tls fails the handshake and net/http writes a bare `400 Bad Request` +
"Client sent an HTTP request to an HTTPS server." onto the socket and closes it — no route, no
handler, no auth middleware. `AGENT_PROTOCOL.md`, `README.md` and `PROTOCOL.md` still show `http://127.0.0.1:8080`
examples — and `AGENT_PROTOCOL.md:252` states as FACT that "today every real bus is
`http://127.0.0.1:…` and no fingerprint is involved", which this change makes false. All three are
outside this change's file-ownership boundary and are NOT edited here. They are filed as
**`MTLS-VERIFY-FU-DOCSCHEME`**, rated **P0** by the security gate, whose reasoning is worth quoting
because it is the reason this is not a tidiness item: *"A documented command that fails with a
transport error is the single most reliable generator of 'just add an insecure flag' in the field,
and invariant 11 forbids exactly that flag."* It must land before this change is announced to agents.

**The reviewer gate caught this paragraph asserting that filing had already happened when it had
not**, which is the same defect class as the two comment corrections above: a written claim about
the world that nobody checked. The task now exists; this sentence names it.

> CORRECTED 2026-08-16: `MTLS-VERIFY-FU-DOCSCHEME` landed. `AGENT_PROTOCOL.md`, `README.md` and `PROTOCOL.md`
now contain ZERO occurrences of `http://127.0.0.1` (grepped 2026-08-16).

Nothing on disk changed format. No existing WAL, enrolment, invite or agent id is invalidated: the
certificate has been minted into the data directory since `MTLS-BUSCERT` (`16f54c9`), and this change
only starts serving with it. `docker compose down -v` is newly more destructive though — it now
destroys the certificate every enrolled agent has pinned, and there is no trust-on-first-use to
re-learn it.

## 2026-08-07 — IDEM-17: crash-injection evidence is indexed by the RETRY WINDOW, not by durable state

**Decision.** `internal/idem/crashinjection_test.go` deliberately duplicates neither the harness nor
the coverage of `internal/hub/idem_crash_test.go` (IDEM-11's evidence). The two index the SAME write
path along two different axes, and both are needed:

- `internal/hub/idem_crash_test.go` indexes by **the durable state recovery finds**: a committed
  entry, a dangling prepare, and a pre-IDEM-11 entry carrying no applied-key record. That is the
  right axis for asking "does recovery read what is on disk correctly".
- `internal/idem/crashinjection_test.go` indexes by **where the SIGKILL falls relative to the
  client's retry**: between the prepare and commit fsyncs, after the commit but before the ack,
  after the ack with an in-process retry already answered, and while a post-restart retry was itself
  being answered. That is the right axis for invariant 10, because a duplicate is something a
  CLIENT does, not something a disk does.

**Why the second axis earns its keep, concretely.** Two of its crash points have IDENTICAL durable
state (post-commit-pre-ack and post-ack differ only in what the client knows), so the state axis
cannot distinguish them — yet they are the two cases invariant 10's legitimate-retry carve-out is
written for. Indexing by durable state alone would have left the honest-client property untested,
and it was untested: the existing suite issued every post-restart retry through a helper that
RE-MINTED first, which silently masks the property. A real client holds a signed assignment and
replays it verbatim; the reservation table is in memory and does NOT survive a restart. If the mint
lookup ever moved ahead of the applied-key lookup in `hub.publish`, every honest retry across a
restart would be refused with `ErrUnknownMint` for a message sitting durable on disk. That defect
was reproduced by mutation and is now caught by
`TestIdemCrashInjectionRestartHonestRetryIsNeverPunished`.

**The corollary that shaped the suite.** Under SIGN-1's reserve-then-send, losing the applied-key
table does NOT present as a duplicate — it presents as a refusal, because the in-memory mint table
died with it. The duplicate appears one step later, when the client does exactly what
`ErrUnknownMint` documents: re-mint under the same key and re-send. A suite that only ever replayed
verbatim would have reported the wrong failure and never observed the second message.
`TestIdemCrashInjectionRestartRemintingClientStillGetsOneEffect` exists solely to model that client,
and it is the test that fails with "the operation has now been APPLIED TWICE" when the applied-key
table stops being recovered. Recorded because it is genuinely counter-intuitive: the obvious test for
a double-apply does not catch the double-apply.

**Placement, recorded as a boundary consequence rather than a preference.** A suite that drives
`internal/hub` lives under `internal/idem` because the authoring agent's file-ownership boundary was
`internal/idem/**` while `internal/hub/**` was another live agent's. It is legal — `package idem_test`
is an external test package, so it may import `internal/hub` even though `internal/hub` imports
`internal/idem` — but it has a real cost: the file contains no reference to package `idem`, so
`go test ./internal/hub/` does not run it and coverage is attributed to the wrong package. Filed as
`IDEM-17-FU-PLACEMENT` to be settled deliberately rather than left as an accident of scheduling.

---

## 2026-08-07 — CORRECTION: the WAL record-index floor does NOT subsume the message-sequence floor. The subsumption claim is DISPROVED, 247 of 248 truncation offsets (MSG-FU-SEQHIGHWATER, `6ebe51be`)

**This entry CORRECTS, by name, the paragraph "This SUBSUMES most of the open task
`MSG-FU-SEQHIGHWATER` (`6ebe51be`)" in the earlier 2026-08-07 section "The WAL record-index high-water
mark is a dedicated write-ahead file, not derived from the log (e120153b, db350e39)" (section heading
at line 2577; the paragraph itself around line 2656, added by `aad611c`).** `DECISIONS.md` is
append-only, so that paragraph is not edited — it must be read as SUPERSEDED. Nothing else in that
section is disturbed: the dedicated index file, the rejected alternatives (a)-(d), the
implausible-index bound and the reservation-block trade all stand. What is withdrawn is one inference
drawn at the end of it.

*(An earlier draft of this entry named the wrong section — "The durable WAL index floor is
authenticated with a KEYED MAC…", which is a different section 615 lines further down, added by
`cc6f63a`. The reviewer gate caught it. In an append-only file "by name" is the whole mechanism by
which a correction finds its target, so naming the wrong one silently orphans the correction.)*

**The claim, quoted.** *"`internal/hub` derives its sequence floor from `wal.Recovered.NextIndex - 1`,
so raising the value `Recovered.NextIndex` reports closes the measured message-id regression … without
`internal/hub` needing a floor file of its own"*, and the instruction that followed it — that whoever
re-scoped the task should read it as *"confirm the residual is acceptable and close" or "account for
the migration window explicitly"*, **not** *"build a second floor"*.

**It is false, and it was measured false rather than argued false.** Sweeping EVERY truncation offset
of a data directory's `bus.wal` — a pristine copy per offset, `wal-mac.key` and `wal-index-floor`
carried through intact, the resumed sequence read by performing a real `hub.Hub.Mint` (the in-process
call the `/v1/mint` route makes) rather than by consulting an accessor. An offset counts as a reissue
when the resumed sequence lands at or below the **durably burned floor** (256 after five mints), not
merely at or below the highest number actually handed out (5) — the burned range is what
`seqfloorfile.go` promises never to reissue, and measuring against the issued numbers alone would
accept any rewind inside the 252-number gap between them:

| data directory | offsets swept | offsets that REISSUED a durably burned sequence |
|---|---|---|
| `message-seq-floor` present | 248 | **0** |
| `message-seq-floor` removed | 248 | **247** |

The single survivor in the second row is the undamaged file, where the in-log `seqfloor` record still
replays. Every other cut — including cuts that leave the WAL's own durable index floor fully intact —
resumes the message sequence at or below a number a client already holds an Ed25519 signature over.

**The provenance of that table was itself a finding, and this entry is what closes it.** When the
numbers first appeared — in `CONTRACTS-ONDISK.md`, quoted from a throwaway sweep under `/tmp` — the
**security** gate filed them as a LOW *evidence-provenance* finding in its 20:29 note: *"the
Measured-evidence 248/247 table appears ONLY in `CONTRACTS-ONDISK.md`; grep across all `.go` and `.md`
finds no committed test producing those counts"*. It was right to. The **reviewer** gate reproduced
the table experimentally, and said so in its 20:21:29 verdict (*"including the 248/0/247 table which I
reproduced experimentally"*), so the numbers were never in doubt — but a measurement that lives only in a
decision record is a claim, not evidence, and it decays the moment the code moves. The table above is
now produced by a committed test on every run, which is the difference between the two.

**WHY the ratio inverts, which is the whole of the error.** The subsumption rests on a COUNTING
argument: every sequence issued is `<=` the WAL index of the prepare carrying it, because a message
consumes one sequence and at least two indices, so the indices outrun the sequences for ever. That
argument was true, and `SIGN-2`/`SIGN-6` retired it. `/v1/mint` now hands a client a sequence **before
any record carries it**, and burns `MintBatchSize = 256` numbers per floor record — so **five mints
consume five sequences against two WAL indices**. The counting does not merely weaken at the edge, it
runs backwards: sequences outpace indices by up to 128:1. Raising `Recovered.NextIndex` therefore
raises a counter that is no longer an upper bound on the one that matters.

The codebase already said so, in capitals, in two places the paragraph did not consult:
`internal/hub/hub.go` ("That argument is RETIRED") and `internal/hub/seqfloorfile.go` ("THAT IS THE
ARGUMENT THE BATCH BROKE … Do not reinstate any reasoning that ties a sequence to a WAL index").

**The right artefact ALREADY EXISTS, and nothing new was built.** `internal/hub/seqfloorfile.go`,
`<data-dir>/message-seq-floor`, on-disk format version 5 (RESERVED, not chosen), landed under
`aad611c`. It is the authoritative source; the in-log `seqfloor` record and the three log-derived
sources `hub.Open` maximises over are defence in depth. This correction adds a TEST and this text —
no second floor, no third. The earlier instruction "not build a second floor" was right for the wrong
reason: there must not be a second floor because there is already a correct first one, not because
the WAL's index floor covers it.

**Chronology, because it is the transferable lesson — and it is SHARPER than two earlier drafts of
this paragraph said.** Both of them claimed the mechanism landed at 16:23 in `aad611c` and the
rationale denying it was needed landed nearly three hours later in `cc6f63a` at 19:20. That is FALSE,
and the reviewer gate disproved it with one command after two gates had waved it through by checking
only the two commit *timestamps* and never which commit carried the paragraph:

```
$ git log --oneline -S "This SUBSUMES most of the open task" -- DECISIONS.md
aad611c Durable roster + signed sends: two agents survive a restart
$ git show cc6f63a -- DECISIONS.md | grep -c "SUBSUMES most of the open task"
0
```

**`aad611c` added BOTH.** One commit, at one moment, shipped `internal/hub/seqfloorfile.go` — a
dedicated durable message-sequence floor — AND a decision paragraph arguing that `internal/hub` needed
no floor file of its own. The contradiction was not a drift that opened over three hours; it was
INTERNAL TO A SINGLE DIFF.

That makes the lesson a different and more uncomfortable one. "Re-read the tree before you write the
rationale" would NOT have caught this, because the refuting code was in the author's own working tree
as they wrote it. What catches it is narrower: **when one commit contains both a mechanism and an
argument that the mechanism is unnecessary, one of the two is wrong — read your own diff as a whole
before writing its rationale.** And the operational guard stands either way: a claim of the form "X
subsumes Y" is a claim about behaviour, so it gets a measurement before it gets a paragraph. This
entry is the third attempt at this one paragraph, which is itself the evidence for that rule.

**The evidence, and how it stays evidence.** `TestSequenceHighWaterSurvivesDeepDamage`
(`internal/hub/seqhighwater_test.go`) is the sweep above, run on every invocation in ~8s. Its second
arm is a NEGATIVE CONTROL that removes `message-seq-floor` and then checks three things, not one: that
the sweep finds reissues at all, that it finds them at a LARGE MAJORITY of offsets, and that the
UNDAMAGED offset is NOT among them (with the whole log intact the in-log `seqfloor` record still
bounds the sequence). A bare "at least one reissue" would let a sweep degraded to a single offset pass
while the table above quietly stopped being true, and a test has to check what it is quoted for.

Both assertions were observed RED before being accepted GREEN — a proof never seen failing is not
evidence that it can fail. Disabling source (0) in `hub.Open` (a one-line `if false &&`, in a
throwaway copy of `HEAD`, never in the tree) turns the primary arm's 0/248 into 247/248 and fails it;
collapsing the offset range to the undamaged file alone fails the control. Verdict on the task's
registered proof: `verdict=PASS class=test exit=0 tests_run=3 top_level=1 skipped=0 failed=0
empty_pkgs=0`, independently re-run by both gates.

**READ THE TWO INTEGERS AS A MEASUREMENT, NOT AS A CONSTANT.** 248 and 247 are a function of the WAL
frame layout: the pristine log is 247 bytes, so the sweep has 248 offsets. Any change to the frame
format moves both — with `internal/wal`'s in-flight audit-record work in the tree the same test prints
**245 swept / 244 reissued**, and it is just as correct. What must not move is the SHAPE: zero
reissues with the floor file, a large majority without it, and the undamaged offset never among them.
The test asserts the shape and prints the integers; this entry quotes the integers as of `bus.wal` at
247 bytes. Do not "fix" a future 245 to match this paragraph.

**Adjacent, and NOT part of this correction.** The same superseded section states
`indexReserveBlock = 256`; `internal/wal/indexfloor.go:114` says `64`. That is a wal-layer doc drift
with no bearing on the subsumption question, and it is outside this change's file boundary. **It is
NOT yet filed** — it is handed to spec-keeper as a recommended follow-up, and this sentence says so
rather than claiming a task exists, because asserting that filing has already happened when it has
not is a defect this very file has now recorded twice.

**Chain, and what the gates changed.** spec-keeper (task state) → feature-runner (this test and this
entry) → reviewer → security. Neither gate rubber-stamped it:

- **Reviewer: CHANGES-REQUESTED, TWICE, and both rounds were about THIS TEXT rather than the test.**
  Round one: two unverified claims-about-the-world — a follow-up asserted as already filed when no such
  task existed, and the 248/247 reproduction attributed to the wrong gate. Round two, after those were
  fixed: the section named the WRONG earlier section as its target, and the whole chronology paragraph
  was false (see above). It also caught a false comment in the test — a "read-only" probe that in fact
  rewrites `wal-index-floor` through `indexFloor.begin`, harmless only because the fixture closes
  cleanly — and the weak negative control. **Three of those five findings are the same defect: an
  assertion about git, the backlog or another agent's notes, written from memory and never executed.**
  In a file whose whole purpose is to be believed later, that is the defect class to watch for, and it
  is why every factual claim in this section now has a command behind it.
- **Security: PASS**, with one MEDIUM that is now fixed: the original assertion measured against the
  highest sequence ISSUED (5) rather than the durably BURNED floor (256), leaving a 252-number blind
  band in which a rewind would have passed silently. That is why the bar above is the burned floor.
  Security also confirmed the primary arm's safety is STRUCTURAL, not merely tested — `hub.Open` reads
  source (0) first and every later source can only raise it — and recorded one pre-existing follow-up
  that this entry must not be read as blessing: `message-seq-floor` is integrity-protected by a bare
  SHA-256 while its sibling `wal-index-floor` is authenticated with HMAC under `wal-mac.key`. Endorsing
  `seqfloorfile.go` as "the right artefact" above is an endorsement of its PLACEMENT (outside the log),
  not a ruling on that asymmetry, which belongs in its own task. That task is **NOT yet filed either**
  — it is recommended to spec-keeper alongside the `indexReserveBlock` one, and the security gate's
  framing is to adopt: *reconcile the two positions deliberately*, not *fix a hole*, since the same
  gate has already ruled the unkeyed digest DEFENSIBLE. The substance to settle is that
  `seqfloorfile.go` justifies it on "an attacker with write access can read `wal-mac.key` anyway",
  which assumes read AND write — a write-only primitive breaks that equivalence, and
  `internal/wal/indexfloor.go:393` states the opposite position outright.

Code-only: this is a test and a decision record, so no live behaviour is claimed by either.

## 2026-08-07 — MTLS-EXPIRY: the client ENFORCES the pinned certificate's validity window, and `crypto/x509` makes the verdict

Supersedes the two places this file recorded the gap as open: the MTLS-PIN entry §2 ("**Certificate
expiry and validity are NOT checked**, and that is a real gap owned by `MTLS-VERIFY`") and §8's
second bullet ("**Certificate EXPIRY is not checked, and that is a live gap in a recorded control**").
Both are now CLOSED on the client side. Neither line is edited — this file is append-only — so read
them as history.

### 1. Why this was split out of `MTLS-VERIFY`

`MTLS-VERIFY` depended on `MTLS-LISTENER` and `MTLS-LISTENER` was gated on `MTLS-VERIFY`: a genuine
cycle, and the TLS chain had no runnable head. The expiry check is the part of `MTLS-VERIFY` that
needs no running TLS bus — it is a property of `client/pin.go` alone, provable against an in-memory
certificate — so splitting it out breaks the cycle without weakening either task.

### 2. `crypto/x509` decides; this package only reports

The tempting implementation is two lines: `at.Before(leaf.NotBefore) || at.After(leaf.NotAfter)`.
It was rejected. Invariant 9 says never write your own crypto and prefer the API that wraps as much
of the problem as possible, and certificate validity is exactly the kind of detail a library exists
to get right — interval endpoints, the zero time, a `NotAfter` earlier than `NotBefore`. A second
implementation of a question that must have one answer is how the two answers eventually disagree.

`checkBusCertificateValidity` therefore calls `leaf.Verify`, with **the leaf itself as its own root**:

```go
selfSigned := x509.NewCertPool()
selfSigned.AddCert(leaf)
leaf.Verify(x509.VerifyOptions{Roots: selfSigned, CurrentTime: at,
    KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
```

That is the stdlib's own supported way to say *"trust is already established, apply the remaining
checks"*. Traced through go1.19's `verify.go`: `isValid(leafCertificate, …)` performs the
`NotBefore`/`NotAfter` comparison, `Roots.contains(c)` then short-circuits chain building, and
`ExtKeyUsageAny` returns before any EKU filtering. An **ordinary** `Verify` could not be used: it
fails with `UnknownAuthorityError` for the no-CA reason long before it reaches the dates, which is
precisely why disabling the default chain check took the validity check away with it.

Each option is load-bearing. `Roots` is a fresh pool per call holding one certificate.
`CurrentTime` is the caller's, which is what makes the boundary instants testable without waiting.
`KeyUsages: ExtKeyUsageAny` **skips** EKU filtering deliberately — the default (`ServerAuth`) would
make this function reject a valid pinned certificate over an EKU bit and report it as a validity
problem it is not. `DNSName` is left empty: there is no hostname verification in this design, the
pin substitutes for it, and setting it here would resurrect the name check the invite blob replaced.

### 3. Identity BEFORE validity, and the two failures are never conflated

A matching fingerprint is now necessary but not sufficient. The order is fingerprint first, window
second, because the two demand opposite responses: a mismatch means *"you may be talking to the
wrong bus"* and warrants an investigation; an expiry means *"you are talking to the right bus and
its certificate is stale"* and warrants a rotation or a clock check. A certificate that is **both**
unpinned and expired is reported as UNPINNED — the expiry of a stranger's certificate is a detail,
and leading with it would bury the substitution. `ErrBusCertificateExpired` is a separate sentinel
from `ErrBusFingerprintMismatch` for the same reason, and a test asserts an expired certificate does
NOT match the mismatch sentinel.

### 4. There is NO client-side clock-skew allowance, on purpose

`internal/buscert` backdates `NotBefore` by five minutes when it MINTS a certificate. That is the
right place for an allowance: applied once, by the party that knows the certificate is fresh, and
**visible in the certificate itself**. A second, invisible allowance in the client would extend
every certificate's usable life beyond the `NotAfter` it states — silently weakening the
leak-containment bound this task exists to enforce, in a way no operator reading the certificate
could detect. A client with a wrong clock instead gets a refusal whose remedy names THE CLOCK FIRST,
before any re-pinning advice, because re-pinning cannot fix a clock and trying it wastes the one
diagnosis that would.

### 5. The unrecognised case FAILS CLOSED

`ErrBusCertificateUnusable` is a catch-all for any x509 verdict that is not "valid" and not the
validity window — today only an unhandled critical extension, plus unparseable DER, which a live
handshake cannot produce because `crypto/tls` parses the peer chain before the callback runs. It
exists so "expired" stays a precise claim rather than becoming the label on every refusal. The
important property is the direction: a default arm that returned `nil` would accept everything it
did not think of, which is the silent-accept hole the whole pinning design exists to prevent.

`isPinError` lists all four sentinels, which is what routes them through `pinError` (so the operator
gets the certificate remedy rather than "cannot reach the bus") and makes them non-retryable. A
sentinel added to `pin.go` and forgotten there would be reported as a transient network fault AND
retried — a certificate problem dressed as a flaky connection, which is the most effective way to
stop anyone noticing it.

### 6. The evidence, because "the code looks right" is not evidence for this class of bug

A `tls.Config` whose verification callback returns `nil` still compiles, still completes handshakes,
and still returns working connections. Every positive test in `client/` passes either way. So the
proof is negative and it was **mutation-tested**:

- Neutering `checkBusCertificateValidity` to `return nil` → BOTH new tests go red.
- Replacing it with a one-sided `at.After(leaf.NotAfter)` check → **only**
  `TestNotYetValidBusCertificateIsRejected` goes red, proving the two tests are not redundant and
  that both ends of the window are independently guarded.
- Both tests were confirmed RED before the fix: an expired certificate and a not-yet-valid one were
  each pinned, accepted and enrolled against.

### 7. What the gates found, and the one place they CONVERGED

Both gates returned **PASS** (security: no P0, no P1; reviewer: PASS, its two P1s being process/docs rather
than code). The security gate re-derived the correctness argument by brute force in an isolated copy
rather than by reading: 5 pin-sets × 8 chain shapes, zero `CurrentTime`, an inverted window
(`NotAfter` < `NotBefore`), zero-valued dates, unhandled critical extensions, and malformed DER at 1
byte, truncated, junk-suffixed and 4 MiB. It could not construct an input that is accepted when it
should be refused.

**The convergence is the finding worth recording.** Both gates independently arrived at
`ClientSessionCache` as the NEXT occurrence of this exact bug class, and neither was prompted to look
for it. `crypto/tls` **does not call `VerifyPeerCertificate` on a RESUMED handshake** — its own source
says "Resumptions currently don't reverify certificates". So adding a session cache for latency would
disable the pin check *and* this new expiry check on every resumed connection, silently, with every
positive test still green. It is absent today, which is the only reason the callback runs at all.
That is the same one-line, silent, every-positive-test-still-passes shape as deleting the callback —
arrived at from a performance argument instead of a correctness one, which is what makes it likely.

Treated as a control rather than a comment: `pinnedTLSConfig` carries a "DO NOT ADD A
ClientSessionCache HERE" section naming `VerifyConnection` as the only correct home if resumption is
ever wanted, **and** `TestPinnedSkipIsAlwaysPairedWithAPinCheck` now fails if the field appears in
that literal at all.

**The guard was then widened, because the first version of it was not enough.** On re-verification the
security gate REPRODUCED the bypass over live TLS 1.2 — with a cache attached, the second connection
resumed and was **accepted while the server served a completely unpinned certificate**, the callback
never running — and pointed out that a literal-only guard misses `tlsConfig.ClientSessionCache = …`
by ASSIGNMENT, in `transport.go` or two lines under the DO-NOT-ADD comment itself. The existing
`*ast.AssignStmt` arm (which already banned setting `InsecureSkipVerify` by assignment, for the same
reason: an assignment can be conditional and far from the literal) now bans the cache too.
Mutation-confirmed both ways: in the literal, and by assignment at `transport.go:105`.

**And then the guard itself had to be narrowed, which is the more interesting half.** The reviewer
gate — re-verifying, and explicitly WITHDRAWING its own earlier "non-brittle" assessment — found the
first version guilty of three things a guard must never do, all confirmed by injection:

- it **rejected the remedy it prescribes**. The message says "run the checks from `VerifyConnection`",
  and the check never looked for `VerifyConnection`, so taking the advice still failed. A guard that
  refuses its own fix gets deleted by the next person who follows it.
- it **false-positived on an explicit `ClientSessionCache: nil`**, which disables resumption exactly
  as omitting the field does and is if anything the clearer spelling.
- it fired **a second, false message** — "pinning was removed, or it moved somewhere this guard does
  not look" — about a file pairing them on the very next line, because it returned before the
  paired-literal counter incremented.

All three are now fixed and each is verified by injection rather than by reading: explicit `nil`
passes, cache-plus-`VerifyConnection` passes, cache alone fails with exactly ONE message, and the
assignment form fails.

**And the fix for the third of those introduced a fourth defect, which the gate then caught** — worth
recording because it is the most instructive step in the sequence. Accepting any literal that
mentioned `VerifyConnection` meant `ClientSessionCache: …` **plus `VerifyConnection: nil`** passed the
guard, and that configuration resumes with NO verification whatsoever: `crypto/tls` skips a nil
callback and never calls `VerifyPeerCertificate` on a resumption. It was the exact bypass the branch
exists to close, **wearing the remedy's name** — and the same function already spends a dedicated
branch rejecting `VerifyPeerCertificate: nil`, so the new arm was asymmetric with the file's own
standard. A nil `VerifyConnection` now resolves to "absent" and does not satisfy the remedy.

**The guard is SHAPE-ONLY, and its limit is deliberate rather than overlooked.** It resolves a literal
`nil` and nothing more, so `VerifyConnection: connHook` where `connHook` is a nil variable, or a
constructor returning a nil callback, both PASS. The reviewer gate probed exactly those and then
argued AGAINST tightening — and it is right: the only stricter rule available is "demand a function
literal", which would reject `VerifyConnection: c.verifyConn`, the likeliest spelling of a genuine
remedy, and a guard that rejects its own prescribed fix is the defect this branch already had once.
`InsecureSkipVerify` can be required to be a literal `true` because a bool is a constant; a callback
is not. The residual is recorded rather than closed, and the honest consequence is that **no
behavioural test asserts a resumed handshake re-verifies** — the guard alone holds it, exposure is
zero while no cache exists in-tree, and a live-TLS resumption test is filed as a follow-up.

Two general lessons, both earned here rather than asserted:

- **A guard is only as good as its false-positive behaviour.** One that fails correct work is one the
  next agent deletes, and then the property is unguarded and nobody notices.
- **Widening a guard to admit a remedy is itself a change that needs adversarial review.** Three of
  the four defects above were introduced by making the guard *more* permissive, each time for a good
  reason.

Also applied, each mutation-confirmed:

- **The fail-closed catch-all is now tested.** Both gates independently noted that the
  `ErrBusCertificateUnusable` arm — the one whose entire purpose is refusing what nobody thought of —
  was asserted in prose and nowhere else, and was the single line that could be mutated to `return
  nil` and redden nothing. It is live-reachable, not dead code: `crypto/tls` parses the peer chain but
  never calls `Verify` itself, so a certificate with a critical extension Go does not understand
  arrives intact, in date, with a matching fingerprint.
- **A zero clock is now REFUSED, not repaired.** x509 substitutes `time.Now()` for a zero
  `CurrentTime`, so the verdict was already right — but `Error()` compared the zero `At` for its
  wording, and the gate observed an EXPIRED certificate described as *"NOT VALID UNTIL … it is now
  0001-01-01"*. Removing the divergence beats documenting it: a caller with no clock has not judged
  anything.
- **The per-handshake clock is now guarded.** It had a seven-line rationale and no test; capturing it
  once at construction reddened nothing. It does now.
- **Three doc claims were narrowed to what the code actually does.** `checkBusCertificateValidity`
  AUTHENTICATES NOTHING on its own (an attacker-minted in-date self-signed certificate passes it
  cleanly — identity comes solely from the fingerprint comparison that runs first, and the
  self-signature is never verified on this path). "Per handshake" is NOT "per request" — the real
  bound on an expired certificate's usable life is the POOLED CONNECTION's lifetime, and an agent
  continuously long-polling `/v1/wait` holds one open across the expiry. And `x509.NewCertPool()`
  specifically — not nil, not the system pool — is what keeps the darwin/windows/ios PLATFORM
  verifier branch out of this path, which matters because this package is meant to be EMBEDDED.

## 2026-08-07 — `bus_fingerprint` → `bus_fingerprints` is a CLEAN BREAK: no deprecation alias (user ruling)

`MTLS-ROTATE` changed the stored/JSON field from a scalar `bus_fingerprint` to a set
`bus_fingerprints`. The question raised was whether to keep the old scalar as a read alias.

**Ruled: no alias.** Two reasons, and the first is the one that matters:

1. **A scalar alias for a set re-teaches the exact assumption `MTLS-ROTATE` removed.** The whole
   point of that work is that a bus mid-rollover legitimately has TWO certificates, so any code path
   that can still ask "what is *the* bus fingerprint" will eventually be written against, and it will
   be wrong precisely during the event the set exists to survive. A compatibility shim that preserves
   a broken mental model is not compatibility.
2. **Nothing consumes the field yet.** No bus serves TLS (`MTLS-LISTENER` is unlanded), so there are
   no enrolled identities in the wild carrying the old spelling. The break is free NOW and gets
   permanently more expensive after the first consumer — an alias is the kind of thing that is
   cheap to add and never removed.

Reversible if a real consumer appears before the listener ships; the point is to not pay for a
migration nobody needs.

> CORRECTED 2026-08-16: the ruling held for the WIRE and for every new write, but a one-way READ shim was added
and is still in the tree — `client/store.go:666` keeps a legacy scalar field `BusFingerprint`,
tagged `json:"bus_fingerprint"`, consumed only by `migrateLegacyBusFingerprints`.
That is a migration path, not the maintained alias this entry refused, and it does not re-teach the
scalar mental model to any live code path. It is recorded here because a reader grepping
`bus_fingerprint` WILL find it and must not read it as the scalar being supported.

<!-- ===== BEGIN 2026-08-07 feature-runner: data-directory permissions + seq-floor bounds (task be447589-6583-4d5c-a9d4-ec9d9fef0f1c) ===== -->

## 2026-08-07 — The data directory's PERMISSIONS are enforced at startup, and the message-seq floor is bounded at both ends

Three decisions, all forced by exploits that were reproduced against `9f2878a` before anything was
changed. They are recorded together because they are one argument: **the integrity of the data
directory is a property of the DIRECTORY, not of the files in it.**

### 1. Other-writable data directory: REFUSE. Group-writable: TIGHTEN and WARN.

`os.MkdirAll(dir, 0o700)` does nothing at all to a directory that already exists — no chmod, no
check, no warning — so a pre-created `0777` data dir survived a completely clean start. The live
data dir in this repo is `0775` today, a umask artefact nobody chose. Meanwhile `client/store.go`
and `client/clientcert.go` have stat-tighten-warned their credential directory since MTLS. **The
client protected its credentials and the server did not protect its own. That asymmetry was the
defect.**

Why this is a directory problem and not a per-file one: every identity file is written `0600`, and
that mode governs who may OPEN the file. It does not govern who may REPLACE it. Unlinking a file and
creating another in its place, or renaming over it, are permissions on the CONTAINING DIRECTORY. So
`0600` on `message-seq-floor` protects nothing when the directory is world-writable. Closing it at
the directory covers `bus-id`, `agent-suffixes`, `wal-mac.key`, `wal-index-floor` and
`message-seq-floor` in one place.

**Why the two outcomes differ**, which was the question actually asked:

- **Other-writable => refuse.** The trusted set is unbounded — every account on the box, and anything
  that gets code execution as any of them. There is no benign cause: a umask can only CLEAR bits, so
  a directory *we* created can never be other-writable, which means the bit was always set by
  something else. And a directory that has been world-writable may ALREADY have had a file
  substituted; adopting a forged one is silent and undetectable. Tightening would convert an attack
  into a warning line that scrolls past. The mode is deliberately left UNCHANGED so the operator can
  see it, and nothing is written into the directory before the check.
- **Group-writable => tighten to `0700` and WARN.** The trusted set is bounded and was chosen by an
  administrator, the dominant cause is an accident (`mkdir` under umask 002, or a deployment group)
  that the chmod fully removes, and refusing would brick working buses on upgrade over a condition we
  can simply fix. The WARN is not decoration: once the chmod has run it is the ONLY surviving
  evidence that the directory was ever exposed.

The sticky bit is deliberately NOT treated as mitigating. It stops another user unlinking files they
do not own, but it does not stop them CREATING a file the bus has not written yet — and every
identity file is absent on a first start.

**There is no flag to bypass either branch.** A flag that disables a security check ends up in
somebody's unit file; invariant 11 already states the posture.

### 2. An implausibly high `message-seq-floor` is refused, not adopted — REVERSING a prior decision

`internal/hub/mint_test.go` used to assert that a floor of `math.MaxUint64` must OPEN, on the stated
grounds that *"an exhausted id space is a legitimate state to recover, not corruption. Refusing to
start here would be indistinguishable from a damaged file and would send an operator after the wrong
problem."* **That is reversed.** New bound: `maxPlausibleSeqFloor = 1<<56`; above it the file is
refused as corrupt-or-tampered.

The old reasoning treated a physically unreachable state as legitimate — the test's own comment
conceded the fixture was "the only way to reach this state without issuing 1.8e19 sequences" — and in
doing so left the file's most damaging forgery indistinguishable from a normal start. The digest is
UNKEYED, so `floor = 2^64-1` with a valid SHA-256 is one line of Python for anyone with directory
write. The measured consequence: the bus boots **completely healthy** (`/healthz` ok, roster intact,
log replayed, zero warnings) and every `/v1/mint` returns 500 for ever, across every restart.

The comparison is not "refuse" versus "keep working". It is:

- **adopt** => a bus that serves, enrols and issues sessions, cannot deliver a single message, ever,
  and says nothing about why;
- **refuse** => a bus that stops and names the file, the value and a one-step remedy.

A genuinely exhausted bus is equally unable to mint either way, so refusing costs it nothing it still
had. **Refusing strictly dominates on the availability axis as well as the security one**, which is
what makes reversing the earlier call correct rather than merely different.

`1<<56` is deliberately generous: ~2,285 years at a sustained million minted sequences per second,
four orders of magnitude beyond a single-node bus. It also leaves >1.8e19 numbers before exhaustion,
so an attacker gains nothing by choosing the largest value that still passes. Only the READ is
bounded; `ensureSeqFloorLocked` still saturates to `MaxUint64` on true arithmetic overflow, and the
honest caveat is recorded at `maxPlausibleSeqFloor`.

### 3. Floor file ABSENT **and** the log lost records => REFUSE (invariant 1 over invariant 6)

> CORRECTED 2026-08-16 — READ THE 2026-08-08 CORRECTION FURTHER DOWN THIS FILE BEFORE THIS SECTION. Both
"durable" arms described below — the unaccounted-for-indices arm this section calls "the arm that
matters most", and the `MissingRecords` arm built to replace it — were REMOVED on 2026-08-08, because
each turned an ordinary unclean shutdown into a PERMANENT refusal of a healthy data directory. What
survives is the four transient `Repaired.*` signals plus a narrow emptied-log arm; the guard is
one-shot for truncation and interior loss, and refuses on EVERY start after a quarantine (2026-08-08
(b)). Decisions 1 and 2 of this entry are unaffected and current
(`cmd/agent-bus/datadirperm.go:88,105-134`; `maxPlausibleSeqFloor`).

A missing floor file is a SUPPORTED UPGRADE PATH — a data directory written by a binary that predates
it — and rebuilding the floor from the log is right when the log is INTACT. Combined with a damaged
log it is a fabrication. Measured: 300 sequences minted, handed out and signable; delete the floor
file, truncate the log; the bus starts happily and mints **25**, walking back up through 275 numbers a
client may hold a signature over. The harm is invisible and is WORST FOR THE MOST CORRECT CLIENTS:
our own docs require consumers to deduplicate on `message_id`, so a correctly implemented consumer
sees the repeat, concludes it is the duplicate it was told to expect, and silently DROPS the new
message.

The knowledge was already in the tree; only the guard was missing. `openSeqFloorFile`'s comment
already named "missing-file plus quarantine on the SAME start" as the one uncovered case, and
`seqFloorCorrupt`'s remedy already said the log fallback is *"correct ONLY if that log has not also
been damaged or quarantined"*. **The CORRUPT path refused and explained itself; the MISSING path
performed the identical unsafe fallback silently and then logged that it had "closed the window"** —
with a floor below numbers already issued. That warning is now qualified to say what actually makes
it true.

**On invariant 6.** This is a refuse-to-start path, and invariant 6 says recovery must always reach a
running server. It falls under the NARROW IDENTITY-FILE exception already granted for the MAC key,
the persisted bus id, the agent-suffix floors and the WAL index floor — not the log. The log still
always starts: a damaged log on a directory that HAS its floor file is still repaired or quarantined,
still logged loudly, and still serves. What refuses is the case where NOTHING on disk can prove the
id authority, and invariant 1 was reaffirmed WITHOUT narrowing on 2026-08-02 precisely for this.

**Two supporting decisions this required:**

- **`seqFloorFile.ensureExists()` writes the file at floor 0 on every start.** Before this, "the file
  is absent" meant two different things — a legacy directory, and a fresh directory that has simply
  never minted — and the guard has to tell them apart. Measured: a brand-new bus opened and then
  `kill -9`'d leaves `records=0, NextIndex=65`, because indices are reserved in blocks; that is
  indistinguishable from a log whose records were destroyed. Writing the file at Open collapses the
  ambiguity at the source and closes the migration window one step earlier, at the first START rather
  than the first MINT. This reverses a smaller assertion in `mint_test.go` whose stated worry ("every
  start would rewrite a file it has nothing new to say about") does not occur — the write happens
  only when the file is absent.
- **The predicate is "records were REMOVED FROM THE FILE", not "recovery had something to say".** It
  counts quarantine, truncation, mid-file rewrite, unidentified loss, and — the arm that matters
  most — **indices the file cannot account for**. It deliberately EXCLUDES `MissingRecords`, dangling
  prepares, `HeaderRepaired` and `Rebuilt`: those remove nothing from the file and are the ordinary
  signature of a clean crash, so counting them would refuse every legacy directory that had ever
  crashed.

**The unaccounted-for-indices arm was forced by measurement, and it is the reason a sampled sweep is
not acceptable evidence here.** Sweeping every byte offset of a 4491-byte log: 23 offsets — exactly
the RECORD BOUNDARIES — produce a file recovery calls perfectly clean (no truncation, no
unidentified loss, no quarantine, records present), because a log cut precisely on a boundary is
indistinguishable from a log that ended there. At one of them the bus started and minted 257 against
a pristine high-water mark of 300. What makes it detectable is the durable index floor, which lives
OUTSIDE the log and therefore survives: when the surviving records cannot account for the indices
this directory has authorised, records are gone whatever the file looks like.

**The accepted false positive, stated plainly:** indices are authorised in blocks, so an unclean
shutdown legitimately leaves authorised-but-unused indices and this arm fires on them. That cost can
only ever be paid by a directory with NO floor file — i.e. one written by a binary older than
`ensureExists` — once, with a documented remedy. The alternative is silently reissuing signed message
ids, and there is no third option: nothing on disk distinguishes "burned and unused" from "used and
then lost".

### What was deliberately NOT done

**`message-seq-floor` was NOT keyed with the WAL MAC, and keying it must not be recorded as the
answer to this finding.** Keying does not help against the attacker who can read `wal-mac.key`
sitting in the same directory, and it would leave every other file there creatable, deletable and
renameable by a directory-writer. The permission is the defect. Keying remains worth doing for
consistency with `wal-index-floor`, as a separate and honestly-labelled change.

The false justification has been corrected in place at `encodeSeqFloor`: it claimed the digest need
not be keyed because "an attacker with write access to the data directory can read the WAL MAC key
sitting next to it anyway". **Directory-write and file-read are independent permissions.** A local
user on a `0777` data directory has the first and not the second, so there really is an attacker who
can forge the unkeyed seq floor and cannot forge the keyed index floor.

<!-- ===== END 2026-08-07 feature-runner: data-directory permissions + seq-floor bounds ===== -->

## 2026-08-08 — Invariant 10's disconnect is NARROWED to the replay path (user decision)

**The change.** Same key + different payload no longer disconnects; it rejects and logs. Replay of
an already-accepted signed message by a third party now DOES disconnect, and did not before.
Implemented in `1c6c540`, contract text in `0dbb025`.

**Why this entry exists.** CLAUDE.md requires an explicit decision record for any change that
weakens a load-bearing invariant. `0dbb025` shipped the weakening without one — the integrator
flagged the gap in both commit messages rather than letting it pass silently. This closes it.

**What was measured, at the raw socket rather than from status codes.** An independent security
agent in another repo first reported that neither path disconnected. That was wrong, and the way it
was wrong is the useful part: `Connection: close` fires AFTER the response, and any pooled HTTP
client transparently redials, so a 200 on the next request is consistent with either outcome. Re-run
against a pinned connection:

- same-agent key reuse → `409`, `Connection: close` present, socket **closed by the server**
- third-party replay   → `403`, header **absent**, socket **still open**

So the control existed and fired — on the party most likely to be honest — while the actual
adversary kept its connection. Worse, the disconnect was **unreachable** by a third party at all:
both routes in (claim `sender=victim`, or present the victim's mint) are caught by an authorization
check that runs BEFORE the idempotency layer and exits through a non-disconnecting door.

**Why not simply implement the invariant as written.** A disconnect on same-agent key reuse lands on
a client that lost track of its own keys — keys are scoped per agent, so the key is always the
caller's own — and drops every other request pipelined on that socket, including its parked
long-poll. This project has now aimed an abuse defence at the wrong party four times; this is the
one place it was specified.

**The implementation reproduced the very bug it was fixing.** The first draft disconnected on ANY
sender mismatch, so an empty `sender`, a dropped bus prefix (`alpha-1`) and a trailing space each
dropped an honest client's socket. Caught by the reviewer, reproduced at the socket, then fixed by
gating on `ids.ParseAgentID(body.Sender)` succeeding: the 403 still fires for every mismatch, but
only a claim that NAMES an agent drops the connection. That is why invariant 10 now carries a
two-question test before anyone adds a disconnect.

**One ambiguity deliberately left un-disconnected.** `409 no-matching-reservation` is byte-identical
for a third party spending someone else's mint and for an agent re-presenting its OWN spent
reservation. The minting agent lives only in the `mints` map KEY, never the value, so a miss carries
no ownership information. Rather than guess, the case rejects without disconnecting, and
`TestCrossMintIsIndistinguishableFromAnHonestSpentReservation` asserts the indistinguishability so
it goes RED the day it becomes resolvable. Follow-up `10212db3`.

**Not yet reconciled, and it is the forward hazard.** `internal/relay/doc.go` still specifies
"OFFENDING PEER DISCONNECTED". Relay ingest inheriting the pre-narrowing rule would drop every agent
behind a peer bus at once — the same defect one scale up. Six agent-facing files also still assert
the removed disconnect; no code branches on them, which is exactly why the suite stays green over
stale security prose.

> CORRECTED 2026-08-16: reconciled. `internal/relay/doc.go` no longer contains "OFFENDING PEER DISCONNECTED";
the section now states the narrowed rule — same key + different payload is reject-and-log with the
connection KEPT, and the only disconnect anywhere in the codebase is a well-formed third-party replay
(`internal/relay/doc.go:482-506`). The narrowing itself is unchanged in code:
`internal/httpapi/messages.go:1016-1041` keeps the connection on a 409, and `:644-663` disconnects
only when `ids.ParseAgentID(body.Sender)` succeeds for a DIFFERENT agent.
`TestCrossMintIsIndistinguishableFromAnHonestSpentReservation` still exists
(`internal/httpapi/disconnect_socket_test.go:476`) and still asserts the deliberate
indistinguishability.

<!-- ===== BEGIN 2026-08-08 feature-runner: CORRECTION to the 2026-08-07 seq-floor entry (task be447589-6583-4d5c-a9d4-ec9d9fef0f1c) ===== -->

## 2026-08-08 — CORRECTION: both "durable" arms of the seq-floor guard were WRONG and were removed

The 2026-08-07 entry above describes an "unaccounted-for indices" arm as *"the arm that matters
most"*. **That arm, and a second one built to replace it, were both removed on 2026-08-08 because
each turned an ordinary unclean shutdown into a PERMANENT refusal of a perfectly healthy data
directory.** The entry above is left intact per the append-only rule; this section supersedes it.

**Attempt 1 — compare the file's reach against `rec.NextIndex`.** `NextIndex` from `Log.Recovered`
is the FLOOR-RAISED value, not the file's own high-water mark. Indices are authorised in BLOCKS, so
any unclean shutdown leaves authorised-but-unused indices, which the arm read as data loss.
Reproduced: five mints, `SIGKILL`, remove the floor file — exit 1 on starts #1, #2 and #3 with a
byte-identical, undamaged log.

**Attempt 2 — count `MissingRecords`.** It looked like the one signal that outlives a repair. But a
burned reservation starts at the END of the file and becomes an INTERIOR hole the moment the bus
writes past it, and then never clears. Measured on an undamaged log: crash once, run and stop
CLEANLY twice, and `MissingRecords` sits at 58 for ever.

**Both were reachable by following our own documentation.** `seqFloorCorrupt` and
`CONTRACTS-ONDISK.md` tell an operator to move a damaged floor file aside and restart; on a
directory whose last stop was unclean — i.e. the crash that plausibly damaged the file — that
lands in the refusal, permanently, with no automated way out.

**The lesson, recorded because it was learned twice in one session:** a refusal that "fails safe"
is only safe if its false-positive population has been MEASURED. Both arms were argued from first
principles, both arguments were wrong about how the durable index floor behaves, and the cost was a
brick on healthy directories — worse than the reissue each was closing.

**What remains** is the four transient `Repaired.*` signals plus a narrow emptied-log arm
(`Records == 0 && NextIndex > 1`), which cannot reproduce the false positive because any bus that
minted anything has records, and because `ensureExists` means any directory this binary has opened
carries a floor file and never reaches the predicate.

**Accepted consequence: the guard is now UNIFORMLY ONE-SHOT.** Truncation, quarantine and interior
loss all refuse on start #1 and come up on start #2 — and `docker-compose.yml` ships
`restart: unless-stopped`, so that second start is automatic and unattended. This is a KNOWN,
DOCUMENTED gap, pinned by `TestSeqFloorGuardSurvivesARestart`, which is written to FAIL the day it
is closed. It is still strictly better than what shipped before, which was silent adoption with no
refusal at all.

**Closing it honestly requires `internal/wal` to expose the highest index a record actually
CONSUMED** (its index floor already tracks `reserved`/`written` durably and logs the difference as
`indices_skipped`, but neither value is on `wal.Recovered`). That is outside this task's boundary
and is reported as a blocker rather than approximated a third time.

<!-- ===== END 2026-08-08 feature-runner: CORRECTION ===== -->

<!-- ===== BEGIN 2026-08-08b feature-runner: precision fix to the same-day CORRECTION (task be447589) ===== -->

## 2026-08-08 (b) — "UNIFORMLY one-shot" was imprecise: QUARANTINE is covered on every start

The CORRECTION above says the guard is "UNIFORMLY ONE-SHOT … truncation, quarantine and interior
loss all refuse on start #1 and come up on start #2". **The quarantine half is wrong**, found by the
reviewer gate and since measured and pinned. Corrected, per shape:

| damage shape   | start #1 | start #2 onwards | why |
| -------------- | -------- | ---------------- | --- |
| QUARANTINE     | refuses  | **refuses**      | leaves an EMPTY log; the emptied-log arm is a property of the FILE, not of this start |
| TRUNCATED tail | refuses  | starts           | only transient `Repaired.*` flags survive the repair |
| INTERIOR loss  | refuses  | starts           | same |

So the known gap is the two ONE-SHOT shapes, not all three. The distinction is worth stating
because it is the one structural hint about what a correct durable signal looks like: the arm that
works reads the state of the FILE, and both arms that had to be removed read what THIS START did to
it. `TestSeqFloorGuardSurvivesARestart` now carries a quarantine case pinning the covered shape
alongside the two gaps.

Also added, on the reviewer's point that nothing pinned the FALSE POSITIVE itself:
`TestUncleanShutdownWithNoFloorFileStillStarts` kills the bus, runs cleanly twice so the burned
index reservation becomes an interior hole, removes the floor file exactly as our own documentation
instructs, and requires the bus to COME UP — twice, because the defect it guards was permanent.
Every other test in that file uses SIGTERM, so without it a third rebuild of either removed arm
would pass the whole suite green.

<!-- ===== END 2026-08-08b feature-runner ===== -->

<!-- ===== BEGIN 2026-08-08c feature-runner: AMENDMENT to Decision 4 (broadcast 501) — DUR-5 audit wiring ===== -->

## 2026-08-08 (c) — AMENDMENT to Decision 4: the broadcast write path is no longer "INTACT and tested". Half of it is (operator ruling)

Decision 4 (`POST /v1/broadcast` answers 501) closed with this promise, and it is the sentence this
amendment corrects — the original stays exactly as written, above:

> `hub.Broadcast`, `client.Broadcast` and the whole broadcast write path are **deliberately left
> INTACT and tested**, so SIGN-3 re-opens the route by settling ONE question rather than by
> re-plumbing the write path.

**Which half is still true, and which is not.** The load-bearing half holds: SIGN-3 still re-opens
the route by settling ONE question, and no re-plumbing is waiting for it. The *plumbing* is intact —
`hub.Broadcast`, `hub.publish`, the mint, the idempotency scope, the fan-out and the wake-up are
unchanged and are the same single code path a directed send takes. What is **no longer true** is
"and tested": `hub.Broadcast` now **fails closed** before its durable write, and the ~31 tests that
exercised it are **skipped**, not passing.

**Why it fails closed.** DUR-5 landed the append-only message audit log (invariant 6), and
`PROTOCOL.md` §8.6 binds the record's `content_sha256` to `signing.CanonicalDigest` — SHA-256 over
the canonical *signing* bytes, the same bytes Ed25519 signs. `signing.Canonicalize` **rejects an
empty recipient set**, and `store.Message` stores a broadcast as a **FLAG** rather than an expanded
roster snapshot, so a broadcast has no canonical bytes and therefore no content hash. That is the
*same* unanswered question Decision 4 already identified as SIGN-3's: what the canonical audience of
a broadcast IS.

So `internal/hub/audit.go` refuses rather than substituting a value. **Any value chosen there would
settle SIGN-3 by accident** — in a file nobody would think to read when they came to settle it
properly — and would then be written into an **append-only trail that cannot be edited afterwards**.
That permanence is what makes this different from an ordinary interim shortcut: a wrong digest in a
durable audit log is not a thing a later commit can take back. The rejected alternative, auditing a
broadcast under a hash we invented, produces a trail that looks authoritative and proves nothing —
which is worse than refusing.

**The tests are SKIPPED, not REWRITTEN, and the distinction is the point.** Rewriting them to assert
"a broadcast is refused" would have been less code and a green suite. It was rejected because a
suite asserting the refusal reads as the **settled design**: the next person would find tests
documenting the interim posture as if it were the decision, and SIGN-3 would look answered. A skip
says the opposite — this is unresolved, and it names the question that resolves it. The skip is a
**single** check in `mintedBroadcast` keyed on `signing.ErrInvalid`, not ~31 hand-placed calls, for
two reasons: it is exact (a broadcast test failing for any *other* reason still fails, loudly,
rather than hiding behind a convenient explanation), and it is **self-healing** (the day SIGN-3
lands, every one of those tests comes back on its own — nobody has to find and delete thirty-one
skips, and none can be left behind).

> CORRECTED 2026-08-16: the mechanism is unchanged and still correct; only the COUNT has moved. `go test -v
./internal/hub/...` shows **33** leaf `--- SKIP:` lines today (`internal/hub/hub_test.go:113-169`;
fail-closed call site `internal/hub/hub.go:1753`, before `h.durable.Write` at `:1798`). Read "~31" as
"every broadcast test in the package" — the number moves whenever a broadcast test is added, which is
exactly the self-healing property this paragraph is arguing for. `POST /v1/broadcast` still answers
501 before the body is decoded (`internal/httpapi/messages.go:452-466`).

**Production impact is ZERO, and that is why this was acceptable as an interim posture rather than a
release blocker.** No broadcast can reach `hub.Broadcast` on a running bus:

- `POST /v1/broadcast` answers **501** before the body is decoded (`internal/httpapi/messages.go`,
  `handleBroadcast`) — unchanged by this amendment.
- The route is **deliberately absent from the discovery document** (`internal/httpapi/discovery.go`),
  so it is not advertised to agents.
- **Relayed broadcasts are refused on INGEST**, so a peer cannot introduce one either. The
  enforcement is `ValidateRelayRequest` in `internal/relay/message.go`, which rejects a relayed
  broadcast outright with `ErrUnsignable` — naming SIGN-3 for the same reason this amendment does,
  and noting that exempting broadcasts from the signature requirement would be an unauthenticated
  downgrade selectable from the wire. Pinned by `internal/relay/signed_test.go`. (`internal/relay/doc.go`
  describes the rule; `message.go` is where it is enforced.) The egress fan-out code exists but has
  nothing to fan out.

The refusal is therefore reachable only from tests, which is precisely the population that is
skipped.

**What resolves this.** SIGN-3, and nothing else. When it defines the canonical audience of a
broadcast, `auditContentHash` gets its digest, `hub.Broadcast` stops failing, the 31 skips stop
firing without being touched, and `/v1/broadcast` can return from 501 in the same change. Until
then, treat "broadcast works at hub level" as **false** and do not cite Decision 4's closing
sentence as evidence that it does.

**Recorded because the backlog would otherwise mislead:** DUR-5 is code-complete and its behaviour
IS live for directed sends — every `POST /v1/send` now writes a real audit record — but it is
**not** live for broadcasts, and no amount of test-suite green should be read as saying it is.

<!-- ===== END 2026-08-08c feature-runner ===== -->

## 2026-08-08 — FEDERATION (RELAY-6): deployment assumptions and what they defer

Recorded by feature-runner for the `RELAY` epic (`9b187d47`), phase "FEDERATION". The canonical
text for (a)/(b)/(d) below was drafted by spec-keeper this session as a task note on the epic and
is transferred here close to verbatim, carrying the user's own ruling; (c)/(e)/(f) are the
planner's rulings for the same wave-1 kickoff, recorded here because this task owns `DECISIONS.md`
exclusively for the wave. Six rulings, each with what is given up and what would mechanically
reverse it.

**THE ASSUMPTION.** The user is deploying three buses — laptop ↔ an internet-facing machine ↔ this
machine — with the middle box acting as a relay hop. The premise, stated on one line so it can be
grepped whole: every bus-to-bus link is an SSH tunnel; no bus process is ever a publicly listening
service. The user is the sole operator and sole local user of all three machines.

**(a) Topology: SSH tunnels only, operator-controlled end to end.**
No bus binds anything but loopback; every hop between buses is an SSH tunnel the operator set up.
- *Given up:* any deployment path where a bus is reachable without an operator-controlled tunnel in
  front of it.
- *Reverses when (mechanical):* any bus process binds an interface other than the loopback address
  its local tunnel terminates on, **or** a fourth machine joins the topology without an SSH tunnel
  the operator personally holds both ends of.

**(b) INVITE-GATE does not block the FEDERATION epic.** INVITE-GATE (P0, task `05a5216d`) exists
chiefly because an unauthenticated `POST /v1/enroll` lets anyone who reaches the port mint agents,
exhaust the session table, or brick the roster at the 4096 cap (finding `1c4d3dea`, roster-brick
DoS). With no reachable listener, that attacker does not exist — an SSH tunnel terminating on
loopback means the port is never exposed to anyone who has not already authenticated to the
underlying machine via SSH. This makes running before INVITE-GATE defensible in a way it is **not**
on an exposed bus. On the same basis, **all other security work is deferred until end-to-end relay
is running** — security must not sit on the FEDERATION critical path. Nothing already built is
weakened by this: invariant 9 (never write our own crypto) is untouched, since relay needs no new
crypto.
  - *What it does NOT buy, stated plainly so nobody over-reads it:* the tunnel authenticates the
    **machine**, not the bus process. Anything with a shell on the internet-facing machine reaches
    both forwarded ports — and because the bus binds loopback and the tunnel terminates on
    loopback, the bus cannot distinguish tunnel traffic from local traffic. Peer identity and
    certificate pinning (invariant 11, the MTLS-* epic) remain necessary regardless of this
    decision, because invariant 2's routing needs to know unambiguously which bus a message
    traversed — that is functionality, not hardening, and this decision does not touch it.
  - *Given up:* single-use/expiring/revocable peer admission, redemption audit.
  - *Reverses when (mechanical, any ONE of):* any bus is bound to a non-loopback interface, or a
    tunnel endpoint is shared with a non-operator, or a second local/operator user is added to any
    of the three machines, or a peer bus is admitted that the operator does not control. Any one of
    these makes INVITE-GATE a hard blocker again, immediately.
  - *Time-box:* this deferral is scoped to "until end-to-end relay is running", not indefinite. Once
    the FEDERATION epic is functioning end-to-end across the three-bus topology, the deferred tasks
    are due for re-triage against whatever the topology looks like at that point.

**(c) Peer-principal authentication is a REQUIREMENT (b) does not defer, and it is NOT yet built —
this ruling states what must be true before the relay handler is ever served, not the current
runtime behaviour.** Security review (2026-08-08) flagged that an earlier draft of this ruling read
as a present-tense claim; corrected here. Today `internal/relay.Handler` "performs NO
AUTHENTICATION OF THE PEER, and none is missing by accident" (`internal/relay/doc.go:7-9`), is
"reachable from nowhere" — it is deliberately never registered on any mux
(`internal/relay/doc.go:5,7`) — and two more specific gaps are open in the same file: roster
updates are not yet bound to the authenticated connection (`internal/relay/doc.go:154-158`), and
the last bus-path hop is not yet checked against the sending peer (`internal/relay/doc.go:172-175`).
Both close only once `INVITE-PEERGUARD` (task `f5d91dbe`) and `MTLS-RELAYGUARD` (task `8192c3c7`)
land — per `doc.go:9-19`, BOTH must land before the handler is served on a listener. What (b) defers
is INVITE-GATE's pre-auth-enrolment concern; it does NOT defer, and was never meant to defer,
peer-principal authentication on the relay path itself — that remains a hard precondition of
wiring the handler in at all, tracked by the two tasks above, not by this decision.
  - *Given up:* nothing — this ruling authorises no shortcut; the handler stays unregistered until
    both gating tasks land.
  - *Reverses when:* never by topology change. It resolves (not reverses) once `f5d91dbe` and
    `8192c3c7` both land and the handler is wired to a listener; only a future decision that
    explicitly narrows invariant 2 or 6 could touch it otherwise, and none is proposed here.

**(d) Local-attacker scenarios are out of scope, by operator ruling.** No second local user exists
on any of the three machines.
  - *Given up:* defence against a co-resident non-operator process on any of the three machines.
  - *Reverses when (mechanical):* a second local user account (human or service) is added to any of
    the three machines.

**(e) Peer configuration is an offline `agent-bus peer` subcommand under the dirlock, not a new
online admin route.** This follows the `invite mint` / D6 precedent: peers are configured by the
operator running the CLI against the on-disk store while the bus is (or can safely be) restarted,
never through a new privileged HTTP surface.
  - *Given up:* online re-peering — a topology change needs a restart.
  - *Reverses when (mechanical):* an operator requirement for zero-downtime re-peering is stated
    explicitly; that requires a new design (a new privilege tier and admin route), not a flag flip,
    and must be recorded as its own decision when it happens.

**(f) Static next-hop routing, not a routing protocol.** Each bus is configured with explicit,
operator-entered routes to its directly-known peers; there is no discovery or dynamic propagation of
reachability between non-adjacent buses.
  - *Given up:* topology discovery — a fourth bus needs an operator-entered route on every bus that
    must reach it. Right trade for a fixed three-node line.
  - *Reverses when (mechanical):* the topology grows past what the operator can hand-enter as static
    routes (in practice: more buses than the operator is willing to edit config for by hand), or two
    buses that are not directly peered need to route through an intermediary neither operator
    configured explicitly.

## 2026-08-09 — Shared-WAL authenticated checkpoint generations

The WAL, rather than relay or any individual application projection, owns checkpoint generation
publication, authentication, recovery selection, and kind routing. All registered projections are
snapshotted at one committed shared-log high-water mark; the only post-checkpoint durable stream is
the generation's bounded WAL tail. This keeps one globally ordered WAL history and avoids a
relay-only side store with a second recovery history and trust root. Checkpoint format
version 7 is independently reserved from the WAL frame version. Participant-set changes require an
explicit migration because recovery rejects missing, extra, duplicate, or differently-owned kinds.

`CURRENT` is an atomic publication hint, not recovery authority. Recovery scans immutable
generations newest-first and selects one complete authenticated unit; it never mixes snapshots and
tails across generations, and it never falls back to stale legacy `bus.wal` after any published
generation exists. Invalid newest material is rejected as a whole, persistently malformed or
interrupted material is quarantined loudly where safe, and an older complete generation is the
fallback. The durable record-index floor remains authoritative across that fallback, so rejected
later history can disappear from served state without making its indexes reusable.

Each generation receives a random `tail_id` authenticated by its manifest and used as domain
separation for every tail-frame MAC; a second MAC binds the tail header to the generation and shared
high-water. These bindings prevent a valid tail from being transplanted between generations. The
candidate scan stays read-only; only the selected tail enters the ordinary WAL repair path, keeping
discard evidence visible through `Recovered().Repaired` and operator logs. Legacy generation zero
remains supported until the first checkpoint, including the existing v1-to-v2 WAL migration.

> CORRECTED 2026-08-16 — THE CODE EXISTS AND IS NOT WIRED ON ANY SHIPPED BUS. Everything named above is real:
`internal/wal/checkpoint.go` carries `checkpointFormatVersion = 7` (`:110`),
`checkpointCurrent = "CURRENT"` (`:100`), `TailID` (`:126`), `MultiApplier` (`:35`),
`CheckpointParticipant` (`:25`), `selectCheckpoint` (`:466`) and `verifyGeneration` (`:624`). But
`cmd/agent-bus/main.go:630` opens the log with NO `wal.LogOptions.Checkpoints`, so
`Log.Checkpoint` returns `"wal: checkpoint requires a MultiApplier"` unconditionally
(`internal/wal/checkpoint.go:230-231`) and no generation is ever published. Read this entry as a
DESIGN record, not as running behaviour.

## 2026-08-09 — Relay outbox retention is reclaimed only by a successful checkpoint

The `relay-outbox` checkpoint participant owns the `outbox` kind and emits deterministic snapshot
version 1: one shared high-water plus unique, job-id-sorted canonical records. Retention expiry
makes a record non-serving immediately, but does not delete or uncharge it before publication. The
snapshot records the exact canonical identity of its omissions, and only a successful checkpoint
may reclaim records that are still expired and byte-identical to that immutable candidate. This
generation-scoped identity check prevents a checkpoint from deleting a concurrent settlement or a
record that expired after its snapshot.

Retained lifecycle capacity is separate from pending-work capacity and is bounded by global record
count, canonical-byte budget, and per-peer count. Enqueue reserves all three across fsync and charges
the worst canonical pending/delivered/abandoned form for the job. That deliberately makes settlement
capacity-unconditional: after enqueue is acknowledged, writing the larger terminal form never
depends on finding a new slot. A successful checkpoint may rebase an unchanged terminal record to
its exact published length; failure reclaims or rebases nothing.

Replay and snapshot restore admit acknowledged state even when it exceeds today's limits. The
overage is explicit legacy capacity debt: it is logged, retained, and blocks applicable new growth
until successful checkpoint reclamation clears it. Capacity configuration is prospective admission
policy, not a retrospective data-loss mechanism.

> CORRECTED 2026-08-16: the `relay-outbox` participant is real (`internal/relay/outbox.go:2101-2105`,
`outboxCheckpointVersion = 1`), but NO checkpoint can run on a shipped bus — see the correction on
the entry immediately above — so nothing is ever reclaimed by publication. That is not a theoretical
gap: taking "the log HAS a `Checkpoint` method" as "a checkpoint CAN run" wedged cross-bus egress
permanently, and the fix was to ask `wal.Log.CheckpointSupported()`
(`internal/wal/checkpoint.go:217`) and, when the answer is no, DROP and reclaim on sweep rather than
defer. See the 2026-08-15 "RELAY-24-BLOCKER-EGRESS: the reviewer and security gate findings" entry.

<!-- ===== BEGIN 2026-08-14 MTLS-CLIENTAUTH ===== -->

## 2026-08-14 — MTLS-CLIENTAUTH: the listener REQUESTS a client certificate, and never requires one

**Decision.** `cmd/agent-bus/tlslisten.go` sets `ClientAuth: tls.RequestClientCert` with a
`VerifyPeerCertificate` callback (`admitClientCertificate`). A client that has a certificate puts it on
the connection, where it is visible as `r.TLS.PeerCertificates`. A client that has none still completes
the handshake and is served exactly as before.

**This deviates from the task's own title, which named `tls.RequireAnyClientCert`**, so it is recorded
here rather than left as a silent substitution.

### Why not the two neighbouring values

Both were checked against this box's `crypto/tls` source and then EMPIRICALLY, by flipping the
production value and observing the failures.

- **`tls.RequireAnyClientCert`** refuses every certificate-less client at the handshake — before any
  route, log line or error message they could act on. That is every agent whose identity directory
  predates `MTLS-CLIENTCERT`, `agent-bus healthcheck` (which presents none, and is what Docker's
  `HEALTHCHECK` branches on), and every operator probe of `/healthz`. This repo has shipped
  server-side enforcement ahead of client-side capability once already — signature checking landed
  before the client could sign, and every send failed with curl exit 7 until it was reverted.
- **`tls.VerifyClientCertIfGiven`** (and `RequireAndVerifyClientCert`) sit at or above `crypto/tls`'s
  verification threshold in `processCertsFromClient`, so the stdlib chain-verifies against
  `ClientCAs`. **There is no CA in this design and `ClientCAs` is nil, which means the SYSTEM ROOTS.**
  Observed: `tls: failed to verify client certificate: x509: certificate signed by unknown authority`
  for both an agent certificate and a peer-bus certificate. It would admit every client *without* a
  certificate and reject every client *with* one — exactly backwards — and would additionally accept
  any certificate issued by any public CA carrying the ClientAuth EKU.

### Does this narrow invariant 11's "TLS is MUTUAL"?

**No — it sequences it.** Invariant 11's requirement is that both ends present a certificate and both
verify. Nothing here weakens that target; what changes is where the refusal lands. The bus will reach
"mutual" by binding certificate fingerprints to agent ids (`MTLS-BIND`) and then refusing unbound
principals PER ROUTE, which produces a legible 401/403 an agent can act on, rather than by slamming the
handshake shut on a fleet that cannot yet speak it. The handshake is the wrong place for that refusal
in a system whose enrolment route MUST accept a certificate it has never seen.

**The cost, stated plainly: nothing is authorised at the transport layer today.** A certificate on the
connection proves possession of its private key and nothing more. Until `MTLS-BIND` lands, the session
token remains the only credential, exactly as before.

### What `admitClientCertificate` deliberately does NOT do

It **admits**; it does not authorise. Its success case returns `nil` having decided nothing, which is
normally the shape of a silent-accept bug — so the asymmetry with `client/pin.go` is worth stating.
There, the pin IS the authorisation and the callback is where it lives. Here there is structurally
nothing to pin against at handshake time: enrolment must accept an unseen certificate (accepting it is
how the binding gets made, and the INVITE is what authorises it), and every other route needs
per-request state `crypto/tls` does not have. It guarantees one property only — that a certificate
reaching the application is a single parseable leaf with a derivable fingerprint.

Three things it must never grow into:

1. **A `CertPool` of enrolled agents' certificates, verified against.** A pool entry is a TRUSTED ROOT,
   so any agent in it could mint certificates for any name and become a CA for the whole bus.
   Verification here is a 32-byte fingerprint comparison, never chain building.
2. **An `IsCA` or `ExtKeyUsage` filter.** It reads like defence in depth and would break relay: the
   bus's own certificate is `IsCA` with both `ServerAuth` and `ClientAuth`, because a peer bus presents
   that same certificate when it dials. A test arm asserts an `IsCA` client certificate is admitted.
3. **Identity from `Subject`/`CN`/`SAN`/`Issuer`/`SerialNumber`,** or a SEARCH of
   `r.TLS.PeerCertificates` for a known fingerprint. Those fields are chosen by whoever minted the
   certificate; and the peer controls the whole chain while `CertificateVerify` proves possession of
   the LEAF key only, so searching the slice is spoofed by appending the victim's public certificate at
   index 1. Check non-empty, then index `[0]`, fingerprint only — empty is the MAJORITY case under
   `RequestClientCert`, so an unguarded `[0]` panics on almost every connection.

### One fingerprint construction, named here so nothing computes a second

`buscert.FingerprintOf(r.TLS.PeerCertificates[0])` — `sha256` over the leaf's DER exactly as it arrived
(`x509.Certificate.Raw`, never a re-marshalling), rendered as 64 LOWERCASE hex characters with no
prefix, colons or whitespace. The same construction the invite blob carries and `client/pin.go` pins
the bus with. `RELAY-41`'s peer-record field and `RELAY-20`'s lookup must use this exact helper: a
second implementation (SPKI instead of `Raw`, base64 instead of hex, uppercase) produces a well-formed
value that NEVER matches, and nothing anywhere reports the mismatch — it reads as a peering
configuration fault. Do not call such a value `peerFingerprint`; that name is taken at
`internal/relay/peer.go` for an idempotency digest of a roster payload.

### Accepted, open gap

**Client-certificate expiry is enforced nowhere on this side.** `RequestClientCert` does no chain
verification, so `NotAfter` is never checked and an expired agent certificate is admitted. Filed and
owned separately (Spec Server `ca356fde-0613-42cb-ac85-a629609d9c78`). It is harmless only while
nothing authorises on a client certificate, so it must close in the same task as
`MTLS-BIND`/`MTLS-CROSSCHECK` — not after.

<!-- ===== END 2026-08-14 MTLS-CLIENTAUTH ===== -->

## 2026-08-14 — RELAY-45: an inbound peer TLS client certificate is bound to a bus principal on the EXISTING trust record, fingerprint-first, refused with 403 and no challenge, and never without a signing pin

Four decisions, recorded together because each one only makes sense against the others. Code:
`internal/relay/peerstore.go` (`BusTrustRecord.PeerClientTLSCertFingerprint`, `ParsePeerClientTLSFingerprint`,
`PutTrust`, `InboundPeerPrincipal`) and `internal/httpapi/peerprincipal.go`
(`Options.PeerPrincipals`, `RequirePeerPrincipal`). Spec Server `4be32336` (`RELAY-45`).

### (a) The binding lives on the EXISTING `"bustrust"` record, not a new record kind

A fifth `wal.Entry.Kind`, a fresh table and a fresh number reservation were the alternative, and it was
rejected: `BusTrustRecord` is **already keyed by bus principal** (`bus_id`, exactly what this binding
needs to be keyed by — see the INBOUND-vs-OUTBOUND table in `CONTRACTS-ONDISK.md`), and it **already
has** the `RELAY-34` durable-withdrawal-floor revocation machinery a credential binding needs for free:
`RemoveTrust` fsyncs a withdrawal floor outside the log before the tombstone is written, so a discarded
WAL tail cannot resurrect a revoked binding. A new record kind would have had to rebuild that machinery
from nothing, duplicating a mechanism that already exists for the reason this binding needs it. The
cost is coupling — a trust record now carries two logically separate facts (a signing-key pin and a
transport credential) — and decision (d) is the direct consequence of accepting that cost rather than
hiding it.

### (b) The lookup is FINGERPRINT-FIRST — narrowing, not confirming, the task's own design inference

RELAY-45's filing carried an explicit **UNVERIFIED** design inference, offered for the implementer to
confirm or correct rather than as a settled decision: by analogy with `next_hop_tls_cert_sha256`'s
"address-first, outbound" rule, the inbound mirror "is very likely… CLAIMED-IDENTITY-FIRST, not
fingerprint-first" — take a bus id claimed some other way, look up ITS bound fingerprint, and compare
against what TLS presented, the same shape as the session-token/certificate cross-check.

That inference does not hold here, and the reason has to travel with the conclusion or the next reader
will re-derive the wrong caution from the right precedent. The outbound field is fingerprint-first-**forbidden**
specifically because **one fingerprint legitimately sits on N records with N different `bus_id`s**
(`-route-for` duplicates a next-hop pin across every destination reached through that hop) — the
ambiguity is structural, by design, and cannot be removed. The inbound binding has no such structural
ambiguity: `PutTrust` refuses at write time to bind one fingerprint to a second `bus_id`
(`ErrPeerClientCertAlreadyBound`), so on a healthy table the mapping is a true function. Fingerprint-first
is therefore sound here **only because uniqueness is enforced at write and ambiguity fails closed at
read** (`InboundPeerPrincipal` returns `ErrAmbiguousInboundPeerCert` rather than picking one, if a
hand-edited directory or a foreign binary ever produces two anyway) — it is sound BY ENFORCEMENT, not
because the direction of the arrow changed. A "claimed-identity-first" design was not built: there is
no protocol-level bus-id claim to cross-check against on this connection in the first place, since the
whole point of this task is establishing that identity from the certificate. Do not read this decision
as "fingerprint-first is fine here" without the enforcement clause; carry the reason, not the
conclusion — the same instruction the field's own doc comment repeats.

### (c) 403, not 401, and no `Bearer` challenge

A `401` must carry a `WWW-Authenticate` challenge (RFC 7235), and the only challenge this server speaks
is `Bearer`. Sending it on a peer-route refusal would tell a refused peer bus exactly what this gate
exists to keep it from doing: retry with a session token. That is the credential confusion invariant
11's cross-check rule targets — a credential meant for one principal class accepted as though it
authorised the other — and a `401 WWW-Authenticate: Bearer` response is an invitation to attempt
exactly that. The TLS client certificate is chosen when the connection is established and cannot be
supplied by retrying with a different header, so "forbidden for this connection" (`403`, no challenge)
is the honest answer, not merely the safer-sounding one. All six refusal causes share one fixed body
string for the enumeration-oracle reason `### Authentication` already states for session-token
failures; the log line, never the response, names which one it was.

### (d) An active trust record still requires at least one pinned signing key

Not relaxed by this task, and not an oversight: `BusTrustRecord.validate` already refused an active
record with zero `SigningKeys` before this field existed, and adding an optional transport binding does
not create a path around that. The reasoning is decision (a)'s cost made concrete — a bus adjacent
enough to open a TLS connection to us and be admitted as a peer is a bus whose relay-signed messages we
must be able to verify; a transport binding with no signing pin would describe a peer this bus admits
onto the wire and then cannot believe anything it says. A future record that wants a credential binding
with no signing requirement at all is a different record, deliberately, not a relaxation of this one.

### What this task does NOT establish, stated so nobody reads more into it than shipped

No route is mounted (`RELAY-20`), no running server constructs a `*relay.PeerStore` for
`Options.PeerPrincipals` to be wired to (`RELAY-24`), and no CLI flag writes this field — `agent-bus
peer add` lives in `cmd/agent-bus/peer.go`, untouched here. This is a durable record shape and an HTTP
middleware, both tested in isolation; nothing in this task makes either operator-reachable.

> CORRECTED 2026-08-16: ALL THREE are now false, and the surface IS operator-reachable. The routes are mounted
(`internal/httpapi/peermount.go`, RELAY-20 at `ed77bba`); `cmd/agent-bus/main.go:1441-1442` sets
`Peer` and `PeerPrincipals` on a shipped server (RELAY-24); and `agent-bus peer add
-peer-client-fingerprint <hex>` writes this exact field — flag at `cmd/agent-bus/peer.go:627`, parsed
at `:747`, passed as `PeerClientTLSCertFingerprint` into `relay.PutTrust` at `:891`
(`internal/relay/peerstore.go:2581`), i.e. `RELAY-45-FU-CLI` landed. The 2026-08-14 RELAY-6 AMENDMENT
already corrected the first of the three; the other two moved after it was written. Everything the
entry decides about the BINDING — record placement, fingerprint-first-by-enforcement, 403 with no
challenge, signing-pin requirement — is unchanged.

## 2026-08-14 — CLI-11: `key export-public` ships on the SERVER binary, against a task record and a deliverable that both said `agent-busctl`

**Decision.** `agent-bus key export-public -data-dir <dir> [-json]` is a subcommand on the **server**
binary (`cmd/agent-bus/key.go`), not on `cmd/agent-busctl`. Recorded because **every written artefact
said otherwise**, so without this entry the natural reading of the backlog is that the implementation
went to the wrong place, and someone will helpfully "fix" it back.

### What said `agent-busctl`

- The Spec Server task `CLI-11` (`bf966c07-5f99-4fe6-bb23-52868ed04c33`) described "a compiled
  **agent-busctl** operator subcommand".
- Its recorded `proof_cmd` built `./cmd/agent-busctl` and invoked `./agent-busctl key export-public`.
- `scripts/fed-smoke.sh`, the RELAY-25 deliverable, called `"$CTL" key export-public --data-dir … --json`
  where `CTL` is the `agent-busctl` binary.
- `CLI-11` sits in the `CLI-*` epic, every other member of which (`CLI-1`…`CLI-10`) is an
  `agent-busctl` subcommand.

That is four independent artefacts agreeing, which is exactly why the placement was escalated to the
owner rather than decided by the implementer.

### Why the server binary won anyway

**The authority this command needs is FILESYSTEM ACCESS to the data directory, not a network
privilege.** That is the same ruling as E4 (invite minting) and FEDERATION (e) (peer configuration),
and it puts `key export-public` in the company it belongs to: `invite mint`, `peer add|list|remove`
and `healthcheck` are all subcommands on the server binary for this reason. Three concrete facts
settled it:

1. **`agent-busctl` is a pure HTTP client.** No non-test file in `cmd/agent-busctl` imports anything
   under `internal/` — the imports are `github.com/dodgymike/agent-bus/client` and nothing else. It has
   **no data-directory concept and no `dirlock` plumbing at all**; every one of its subcommands is
   defined against a bus URL and a credential store.
2. **This command reads local key material under the data directory's EXCLUSIVE lock.** It opens
   `bus-signing.key` through `internal/buscert` and takes `internal/dirlock`. Teaching the network
   client to do that is not plumbing — it is giving the client filesystem authority and a lock it has
   never held, to satisfy a spelling.
3. **`fed-smoke.sh` was inconsistent with itself, and that was the tell.** It already routed every
   other data-directory operation to the server binary — `mint_invite` and both `add_route` and
   `add_trust` run `"$server" …` — and sent only this one command to `"$CTL"`. The odd one out was the
   spelling, not the architecture.

### What this does NOT mean, stated because invariant 7 is easy to over-read

Invariant 7 says the compiled Go CLI is THE client and that nobody hand-writes HTTP. It does **not**
say every capability lands on `agent-busctl`. The dividing line, now four commands deep, is:

- **`agent-busctl`** — anything an AGENT does, over the network, with a credential: enrol, send,
  broadcast, watch, agents, pin, whoami.
- **`agent-bus <subcommand>`** — anything an OPERATOR does, offline, with filesystem access to a data
  directory: `invite mint`, `peer add|list|remove`, `healthcheck`, and now `key export-public`.

Invariant 7's real requirement — that a capability ship with a compiled subcommand and its
`AGENT_PROTOCOL.md` entry in the same task, and never as a hand-written `curl` or a `scripts/bus-*.sh`
wrapper — is satisfied either way, and was satisfied here.

### Consequences, all landed

`scripts/fed-smoke.sh` was corrected by codex-1 in `1bc778a` and now calls
`"$server" key export-public --data-dir "$data_dir" --json` against each stopped, seeded bus, which is
exactly this command's shape. `CLI-11`'s `proof_cmd` was corrected by spec-keeper — the recorded one
also named a test that was never written, and would have exported from a data directory holding no key
material, which `buscert.LoadOrCreate` would have satisfied **by minting a fresh bus identity**.

### The cost, and what would reverse this

The cost is that an operator wanting the pin must be on the bus host with the bus **stopped**, because
the command takes the exclusive lock. `healthcheck` shows a no-lock read-only subcommand is possible,
so a future task could relax this — but not by moving the command to `agent-busctl`, which would still
need the data directory. **This decision would only be reversed if `agent-busctl` acquired legitimate
data-directory access for some other reason**; until then, a change that moves this command to the
client binary is reintroducing the mistake this entry exists to prevent.

<!-- ===== END 2026-08-14 CLI-11 ===== -->

## 2026-08-14 — INVITE-GATE: invite redemption is genuinely LIVE; the gate stays OFF; five decisions and a corrected residual risk (`documentation`)

> CORRECTED 2026-08-16 — THE TITLE IS NOW WRONG: THE GATE IS **ON**. `enrolmentInviteRequired = true`
(`cmd/agent-bus/main.go:66`) shipped in `3cedcb7` on 2026-08-15, and an un-invited `POST /v1/enroll`
is refused **403** (`ErrInviteRequired`, pinned by `TestInviteGateEnrolWithoutAnInviteIsRefused403`).
The heading is left as written because other entries and task records cite it by name. Section (c) —
the argument for shipping redemption WITHOUT flipping the gate — has been REMOVED; see the terminal
"Removed on 2026-08-16" section. Sections (a), (b), (d) and (e) below are unchanged and current.

**Context.** `internal/auth/inviteenrol.go` (composite record), `internal/invite/store.go`
(`Store.Begin`/`Consume`/`Commit`/`Abort`), `internal/httpapi/auth.go` (`handleEnroll`'s
`invite_id`/`invite_secret` handling) and `internal/httpapi/discovery.go` (`invite_accepted`) shipped
together. This entry records five decisions and corrects one now-stale risk statement rather than
leaving them implicit in code comments.

### (a) Why consumption and enrolment share ONE `wal.Entry`, not two writes

A `wal.Entry` is exactly one transaction (one prepare-fsync, one commit-fsync). Writing the invite
consumption record and the roster enrolment record as two SEPARATE entries — even back to back — opens
a crash window between them: a process killed after the first commits and before the second would leave
either an agent enrolled against an invite the log still calls open (redeemable a second time — the one
outcome single-use exists to prevent), or an invite marked spent with no agent to show for it. Composing
both halves into one entry (`internal/auth/inviteenrol.go`'s `EncodeEnrolWithInvite`/
`DecodeEnrolWithInvite`) closes that window structurally: there is no instant at which one half is
durable and the other is not. `internal/invite/store.go`'s own standalone `Store.Redeem` — which DOES
write its own, separate transaction — is explicitly documented as NOT to be used by this path for
exactly this reason, and is retained only because the invite package must be provable in isolation
before anything composes it.

### (b) Why a new free-form `Entry.Kind` (`"agent+invite"`), and NOT a reserved record-type number

`wal.Entry.Kind` is a free-form application-level discriminator inside the PREPARE payload; the
NUMBERED namespace the Spec Server reserves (`record-type`, `ondisk-format-version`) belongs to
`internal/wal/format.go`'s framing types (`TypePrepare`, `TypeCommit`, …), which this change does not
touch. `"agent"`, `"invite"` and `"seqfloor"` already established the precedent of adding a new `Kind`
value with no reservation; `"agent+invite"` follows it. Recorded explicitly, again, because this is the
second time in this file's history someone has had to write down "no, don't reserve a number for this"
— the first is `internal/invite/doc.go` section 3, which makes the identical argument for
`invite.RecordKind`.

### (d) Why `invite_accepted` is a SEPARATE field from `invite_required`

They answer different questions and collapsing them would make one of the two answers a lie by omission.
`invite_required` says whether an enrolment MUST carry an invite — `false`, and will stay `false` until
(c) above is separately decided. `invite_accepted` says whether an invite PRESENTED to `POST /v1/enroll`
is genuinely redeemed rather than ignored or answered `501` — `true` on every `cmd/agent-bus` build
today, because every one wires an invite store. A single boolean could not distinguish "invites work if
you send one, but none is required" from either "invites are mandatory" or "invites do nothing here";
this build is the first of those three, and only two fields can say so without a client discovering the
truth only by spending a single-use secret to find out.

### (e) Residual risk, corrected for what is actually true TODAY

The Spec Server's own stored description of this task says: *"until MTLS-LISTENER lands the invite
secret crosses the wire in CLEARTEXT."* **That sentence is STALE and must not be repeated as current.**
MTLS-LISTENER landed 2026-08-07 (see that dated entry above): the listener is `https`-only, wrapped in
`tls.NewListener` before it ever accepts a connection, and there is no plaintext fallback — invariant 11
is enforced here, and a plaintext request never reaches `/v1/enroll` at all (`net/http` writes a bare
400 onto the raw socket before any route is consulted). `invite_secret` therefore travels encrypted on
the wire in every build shipped today, not in cleartext.

**What actually remains is narrower, and it is a TRANSPORT-TRUST gap, not a plaintext one.** TLS on this
listener is still ONE-WAY IN EFFECT: `ClientAuth` is `tls.RequestClientCert`
(`cmd/agent-bus/tlslisten.go:152`), which REQUESTS a client certificate and accepts the handshake
whether or not one is presented, and never verifies one against anything — so the server proves
itself to the client and never the reverse. (Stated with the real constant: an earlier draft of this
paragraph said `tls.NoClientCert`, which named the right RISK with the wrong MECHANISM. The
distinction matters to anyone auditing this line later, because `RequestClientCert` means a
certificate may well be sitting in `r.TLS.PeerCertificates` — unvalidated, and trusted by nothing on
the enrolment path. Reading it as authentication is exactly the mistake this correction exists to
prevent.) The
session token, and now the invite secret, remain the only things proving who is on the other end. And
the certificate itself is self-signed, with NO certificate authority and deliberately NO
trust-on-first-use (`MTLS-PIN`): a caller that does not PIN the bus's certificate fingerprint from its
invite blob before connecting has verified nothing about who it is talking to. Concretely: an ACTIVE
on-path attacker who can terminate TLS with a certificate of its own — not a passive observer, who gets
nothing — can read `invite_secret` in full, on the very first request an unpinned client makes,
including one fetching `GET /v1/discovery` to learn how to enrol. `GET /v1/discovery`'s own
`limitations[0]` already states this exact gap for the session token; it now applies equally to the
invite secret, and this entry is the record that the risk moved rather than closed.

<!-- ===== END 2026-08-14 INVITE-GATE ===== -->

<!-- ===== BEGIN 2026-08-14 RELAY-24-BLOCKER-HUBINGEST: relayed audit content hash ===== -->

## 2026-08-14 — RELAY-24-BLOCKER-HUBINGEST: the relayed audit content hash is taken under the ORIGIN's assignment. This REVERSES a recorded position, and the reversal is stated first

### What this reverses, quoted, before anything else

The 2026-08-08 (c) amendment to Decision 4 — the SIGN-3 broadcast entry above — says this, and it
names the exact file this change makes substitute:

> So `internal/hub/audit.go` refuses rather than substituting a value. **Any value chosen there would
> settle SIGN-3 by accident** — in a file nobody would think to read when they came to settle it
> properly — and would then be written into an **append-only trail that cannot be edited afterwards**.

`internal/hub/audit.go` now DOES substitute a value on one path: for a message ingested from a peer
bus, the audit record's `content_sha256` is computed under the ORIGIN bus's message id and sequence
rather than the local record's. That is a reversal of the sentence above as a general rule, and it is
recorded here rather than in a code comment because a position taken in a dated entry cannot be
retired by a comment in the file it constrains. The 2026-08-08 (c) entry stays exactly as written.

### The fact that forces it

A relayed message's LOCAL record carries a message id THIS bus minted — a bus never adopts a peer's id
(invariant 1) — and a sender belonging to the ORIGIN bus (invariant 2). `signing.Canonicalize` refuses
that pair unconditionally: the origin binding in `internal/signing/canonical.go` compares the sender's
bus half against the bus that minted the id, EXACTLY and with no case fold, on the ground that a
message is signed by an agent of the bus that minted its id. A relayed record therefore has **no
canonical bytes of its own and cannot be given any**.

The bytes that DO exist are the origin's: the exact byte string the origin agent signed, which
`(relay.RelayedMessage).CanonicalBytes()` (`internal/relay/signed.go`) re-derives from the field
values this bus will route, deliver, attribute and log. The audit hash is taken over those. Exactly
two fields are substituted — the message id and the sequence — because they are exactly the two this
bus re-minted; the sender, the recipients, the sender's timestamp and the body are already the
origin's on both sides.

### Why this is not a second rule, and why PROTOCOL.md §8.6 is unchanged

§8.6's rule is *hash the bytes the signature covers*. That rule is unchanged and is not narrowed. What
is new is only the observation that, for a message signed on another bus, the bytes the signature
covers are the origin's — so applying the rule unchanged **requires** the origin's assignment.
`PROTOCOL.md` §8.6.1 states this normatively and says so in its first line.

It is also NOT the substitution §8.6's closing paragraph forbids. That paragraph governs the
out-of-band fields it names — the traversed bus path, the local delivery sequence, the byte size —
which are additional columns of the audit record and are never folded into the canonical bytes nor put
in their place. Neither happens here: the value is still SHA-256 over a canonical byte string produced
by `signing.Canonicalize`. (The reviewer gate explicitly RETRACTED that paragraph as the grounding for
this task, as imprecise, and it is recorded as retracted so nobody re-cites it.)

### Why the reversal is correct here, and why it does NOT reopen the broadcast question

The relayed case is the **opposite** of the broadcast case, on the one axis the 2026-08-08 (c) entry
turned on — whether correct bytes exist:

| | Relayed message | Broadcast |
|---|---|---|
| Do canonical bytes exist? | **YES** — the origin's, over which the origin agent's signature was made | **NO** — `Canonicalize` rejects an empty recipient set, and a broadcast is stored as a FLAG, not an expanded roster |
| Who computed them? | `(relay.RelayedMessage).CanonicalBytes()`, already, before the hub was asked to record anything | nobody; any value would be one we invented |
| Were they checked against a signature? | **YES** — `internal/relay/message.go` step 13, inside `ValidateRelayRequest`, verifies before it returns a `RelayedMessage` at all | there is no signature over a relayed broadcast and none can exist |
| Would a value settle SIGN-3? | No — SIGN-3 asks what a BROADCAST's signed audience is; a relayed message has an explicit recipient set | **Yes, by accident** — which is the whole reason for refusing |

The 2026-08-08 (c) position was never "never substitute"; it was "never INVENT a value, least of all in
an append-only trail, least of all in a file nobody would think to read." Selecting bytes that already
exist, were already computed, and were already verified against a signature is not inventing one. The
broadcast path is untouched and **still fails closed**: `hub.Broadcast` still refuses on
`signing.ErrInvalid`, and the ~31 broadcast tests are still SKIPPED rather than rewritten, for the
reasons the 2026-08-08 (c) entry gives at length. SIGN-3 still owns the question and this entry does
not answer any part of it.

### The one structural guarantee that keeps the two apart

The substitution is gated on the message being RELAYED — a single boolean on the internal publish
request — and **not** on the origin fields being populated. A LOCAL send that reaches the write path
carrying an origin assignment is refused as an internal error rather than honoured
(`internal/hub/hub.go`). Deriving the behaviour from the field alone would have meant a future local
caller that set it, for any reason, silently moved a local send's audit hash onto an id of its own
choosing. This is what makes "the relayed case is the exception" a property of the code rather than a
claim in this file.

### What a READER of the trail must know

A relayed record's content hash does **not** reproduce from that record's own `message_id` and `seq`.
It DOES reproduce from the ORIGIN's pair, and that pair is durably recorded: `IngestRelayed` passes the
origin message id as the idempotency key, so it is the message record's `idempotency_key`
(`store.Message.IdempotencyKey`, durable, `json:"idempotency_key"`), and the origin sequence parses out
of it. The MESSAGE log therefore carries everything needed.

**The AUDIT log alone does not** — `wal.AuditRecord` carries neither the origin message id nor the
sender's claimed timestamp. That is not a new limitation and not one this change introduces: it is
equally true of a LOCAL record, whose hash also needs the sender's timestamp from the message log.
Re-deriving any content hash from this trail has always required both files.

### The discriminator is the SENDER's bus half, and NEVER the bus path

A multi-hop bus path does NOT imply a relayed record, and anything that assumes it does will
misclassify records. `internal/hub/buspath_test.go`'s `TestAuditRecordsMultiHopBusPath` publishes a
three-hop path — `[busa, busb, testbus]` — with a LOCAL sender (`testbus.alice-1`), and that record's
hash IS locally reproducible from its own assignment. The structural test is whether the sender's bus
half is this bus's, which is exactly the condition `IngestRelayed` enforces and the condition the
substitution is gated on. This is stated because the bus path is the intuitive discriminator, the test
proving it wrong already exists, and a future fsck or trail-reading tool is where the mistake would
land.

### What remains forbidden, and the tests that catch it

Hashing the BARE BODY — `store.ContentHash(body)` — remains forbidden for a relayed record exactly as
for a local one. It fingerprints content while proving nothing about who sent it, to whom, or in what
order, and it decouples the audit record from the signature. It is also invisible to every structural
check: it is 64 lowercase hex characters too, so `wal.AuditRecord.validate` cannot reject it,
`DecodeAudit` cannot, and no assertion on shape ever will. The substitution has the same property.
Three value-pinning tests are the entire defence, and each rebuilds the expected digest independently
rather than by calling the producer, so a producer that changed what it hashes fails them instead of
moving them:

- `TestSendWritesItsAuditRecord` (`internal/hub/audit_roundtrip_test.go`) — the LOCAL digest by value,
  plus an explicit assertion that it is not the bare-body hash.
- `TestIngestRelayedAuditHashIsTakenUnderTheOriginAssignment`
  (`internal/hub/relayingest_relay24blocker_test.go`) — the ORIGIN digest by value, that it is not the
  bare-body hash, and that `signing.CanonicalDigest` REFUSES the local-id/foreign-sender pair, so the
  local derivation is asserted impossible rather than merely unused.
- `TestLocalSendAuditPayloadIsUnchanged` (`internal/hub/buspath_test.go`) — a whole-record golden
  payload, so a local send's trail entry cannot move by a byte.

### The residual, named rather than implied

`internal/hub` does NOT verify the origin signature and must not: verification needs the origin bus's
attested key and belongs to `internal/relay`, which does it at step 13 of `ValidateRelayRequest`
before any `RelayedMessage` is returned. On the wired ingress chain the bytes are therefore verified
before the hub sees the message. But `hub.IngestRelayed` is EXPORTED and `relay.Acceptor.Accept`
carries no `VerifyRelayed` call of its own, so nothing structurally enforces that a future caller did
the verification — the hub checks only the signature's LENGTH. That gap is filed as
`HUB-FU-INGEST-SIGNATURE-GUARD` (P3), which proposes the same AST-guard shape
`internal/relay/guards_test.go` already uses for `CrossBusTrust`. It is named here so this entry cannot
be read as claiming a guarantee the code does not yet enforce.

### What would reverse THIS decision

Only SIGN-3 landing a canonical audience for a broadcast, which would remove the asymmetry the table
above rests on and would let the broadcast path stop failing closed on its own terms. Nothing in this
entry may be cited to choose a broadcast content hash before then, and nothing in it licenses
substituting a value on any path where the correct bytes do not already exist and have not already
been verified.

<!-- ===== END 2026-08-14 RELAY-24-BLOCKER-HUBINGEST: relayed audit content hash ===== -->

<!-- ===== BEGIN 2026-08-14 RELAY-6 amendment (feature-runner) ===== -->

## 2026-08-14 — FEDERATION (RELAY-6), AMENDMENT: ruling (c) is un-gated from the wrong direction, and two premises are corrected

Amends the `## 2026-08-08 — FEDERATION (RELAY-6)` section above (landed at `77d2b73`,
`DECISIONS.md:4340-4431`). That section stands; nothing in it is deleted. This entry changes ruling
**(c)** and clarifies ruling **(b)**. Rulings (a), (d), (e) and (f) are untouched and remain exactly
as recorded.

**THIS IS AN AMENDMENT, NOT A REVERSAL, and a later reader must not read it as the 2026-08-08
security gate being overturned.** That gate's finding was CORRECT and REMAINS IN FORCE: an earlier
draft of (c) was written in the present tense and would have asserted a control that does not exist,
since, **as of `a8c367c` and until `ed77bba`**, `internal/relay.Handler` performed no authentication of
the peer and was deliberately never registered on any mux, enforced by the guard
`TestRelayPeerRoutesAreNotMountedYet`, which failed if any file outside the package so much as NAMED
`PeerEnrollPath`, `PeerRelayPath`, `PeerRosterPath` or a `"/v1/peer/"` literal. **That guard is now
RETIRED AND REPLACED, not deleted** (`internal/relay/guards_test.go:903`, RELAY-18 precedent): the
live guard is `TestRelayPeerRoutesAreMountedOnlyByTheGatedMountFile`
(`internal/relay/guards_test.go:947`), which permits exactly ONE file outside the package —
`internal/httpapi/peermount.go` — to name those paths, and bounds that exemption. The tense matters
here precisely because this section elsewhere records that the routes are now mounted; the historical
guard is cited as history, not as a live control. The requirement the gate imposed — **no
peer route is mounted without an authenticated peer identity** — is carried forward here unchanged
and unweakened. What changes is only the MECHANISM named as the gate.

**(c-AMENDED) The PEER PRINCIPAL is functionality and is gated on the INGRESS credential chain, not
on the two egress/admission hardening tasks the original ruling named.**

*The defect being fixed.* Ruling (c) as landed conflates two different things and gates the first on
the second:

- **(i) The peer principal** — resolving an authenticated peer identity from the connection, so that
  `RosterUpdate.BusID` can be bound to it (`internal/relay/doc.go` gap 3, `:303` at `ed77bba`) and a
  bus path's last hop can be checked against the sending peer (gap 6, `:343`; both summarised at
  `:173-174`). This is **FUNCTIONALITY**. Invariant 2's unambiguous cross-bus routing
  depends on knowing which bus a message traversed, and ruling (b) already says so in as many words:
  "that is functionality, not hardening, and this decision does not touch it"
  (`DECISIONS.md:4377-4378`). It is what RELAY-20 (`701dc54d`) was scoped to deliver.
- **(ii) Peer invite redemption** (`INVITE-PEERGUARD`, `f5d91dbe`) and **relay client certificates on
  the dialling side** (`MTLS-RELAYGUARD`, `8192c3c7`). This is **HARDENING**.

The landed text turns that into a hard gate of (i) on (ii): "the handler stays unregistered until
both gating tasks land" (`:4402-4403`) and "resolves ... once `f5d91dbe` and `8192c3c7` both land"
(`:4404-4405`). The epic plan and the recorded ruling therefore contradict each other, and the
contradiction is load-bearing: RELAY-20 was attempted on 2026-08-14, correctly wrote NO code, and
named this ruling as a blocker.

*Why (ii) never needed to gate (i) — this is a DIRECTION argument, and it is the whole of the
amendment.* On `/v1/peer/{enroll,relay,roster}` **we are the SERVER**: the peer dials us. The peer
principal on those routes therefore comes from the **INGRESS** direction — a client certificate
arriving on the connection TO us, which we resolve and verify. `MTLS-RELAYGUARD` (`8192c3c7`)
governs the **EGRESS** direction: our relay client presenting a certificate when we dial OUT, i.e.
the peer authenticating US. That is a real requirement and is still owed, but it authenticates the
wrong direction to be a precondition for mounting OUR routes. To head off an apparent contradiction:
`MTLS-RELAYGUARD`'s own task record correctly says every relay hop is *both* a certificate-verifying
TLS client and a TLS server. That is true, and nothing here narrows it — the point is only that the
INGRESS half of it is what `MTLS-CLIENTAUTH` and `RELAY-45` now supply, so `MTLS-RELAYGUARD` retains
the EGRESS half and is not a precondition of the mount. `INVITE-PEERGUARD` (`f5d91dbe`) is
admission control over peer enrolment — also real, also still owed, and also not the thing that
resolves an established connection to a principal. Gating (i) on either was a **category error, not
a security judgement**.

*What the amended ruling still protects — unchanged, and stated as prohibitions so it is checkable.*

- Peer paths **NEVER** appear in `unauthenticatedRoutes` (`internal/httpapi/authmw.go:23` documents
  the explicit allow-list, `:76` declares the map, and `:65-66` records that `mountPeerRoute` REFUSES
  to register a pattern appearing in it; a golden-list test makes any addition visible in review).
- The peer routes register **only** when the inbound peer-identity chain is present. A nil chain is
  a **404** — never a registered-503. A registered-503 is a mounted route that says "later", and the
  no-mount guard described above existed precisely because merely naming the path is the risk — and
  its replacement keeps that property, narrowed to a single permitted file rather than none.
- A session/agent credential is **never** accepted in place of a peer principal, **and a peer-bus
  credential is never accepted in place of an agent credential**. The confusion is forbidden in both
  directions; only one of them is the obvious one.

*THE GATE, AS AMENDED — and a correction of a refuted draft.* A 2026-08-14 amendment request against
this task proposed the replacement gate as `MTLS-CLIENTAUTH` (`cc9558a8`) **plus RELAY-41**
(`05253c80`), on the theory that RELAY-41's `NextHopTLSCertFingerprint` hands RELAY-20 the chain
`r.TLS.PeerCertificates[0]` → SHA-256 fingerprint → peer-store lookup → peer principal. **That chain
is REFUTED and is NOT recorded here as the gate.** RELAY-41 has since landed (`797c538`) and its own
field documentation states the refutation in terms, under the heading "WHICH CERTIFICATE, IN WHICH
DIRECTION — read this before consuming it" (`internal/relay/peerstore.go:464-481` as committed in
`ed77bba`; `:430-447` in the pre-RELAY-45 revision at `a8c367c`). The pin is
the certificate the hop at `BaseURL` presents **when this bus dials it** — an OUTBOUND, SERVER-side
certificate keyed to an ADDRESS — and is explicitly "NOT a source of INBOUND peer identity".
Inverting it is unsound **by construction**: next-hop keying deliberately puts ONE fingerprint on N
records with N DIFFERENT bus ids (`peer add -bus-id busB -url https://b:8443 -route-for busC`), so a
fingerprint-first lookup "would resolve an inbound busB connection to busC — a peer principal spoofed
out of entirely correct data read backwards". `BaseURL → bus id` is the same trap in the other field.
Recording that chain would have written a spoofing vulnerability into the very decision that
authorises the mount.

The gate, as amended, is:

1. **`MTLS-CLIENTAUTH` (`cc9558a8`, landed `a97f854`)** — which puts a peer's client certificate on
   the connection where the application layer can see it, and proves possession of its key; **and**
2. **`RELAY-45` (`4be32336`)**, which owns the **INBOUND** binding: one durable, operator-configured
   credential keyed by the **ADJACENT bus principal**, distinct from `NextHopTLSCertFingerprint` in
   Go name, JSON/on-disk shape, CLI flag and docs, where no lookup may read route records,
   `base_url`, route-for destinations, signing keys, certificate CN/SAN/Subject/Issuer/SerialNumber,
   or an attestation origin to infer the transport principal.

RELAY-41 remains a **necessary sibling** — it is what lets us verify the hop we DIAL — but it is not
part of the inbound identity chain and must never be read as if it were.

*SECOND CORRECTION OF RECORD — `MTLS-CLIENTAUTH` REQUESTS a client certificate and NEVER REQUIRES
one.* The same amendment request described it as `RequireAnyClientCert`. It is not: the listener
pins `ClientAuth: tls.RequestClientCert` (`cmd/agent-bus/tlslisten.go:152`, rationale at `:25-66`),
deliberately, so that agents whose identity directories predate `MTLS-CLIENTCERT`, `agent-bus
healthcheck` and operator `/healthz` probes are not refused at the handshake. The consequence is
load-bearing and must not be lost: **nothing is authorised at handshake time**, and a
certificate-less client still completes the handshake and is served. The peer routes therefore get
**no protection at all from the listener**; the PRESENCE of a client certificate must be enforced at
the **application layer** on those routes, and its absence must fail closed before any handler
executes. What the mode does buy — and it is the part the chain actually needs — is proof of
possession: a client that sends a certificate must also send a `CertificateVerify`, which
`crypto/tls` verifies in every mode. "A certificate on the connection proves its holder has the key;
it does not prove WHO they are" (`tlslisten.go:61-63`). RELAY-45 supplies the *who*.

*HOW THE CERTIFICATE MUST BE READ, because the chain is attacker-controlled.* Two rules already
recorded at `cmd/agent-bus/tlslisten.go:261-281` are hereby part of this gate, not merely advice:
the **fingerprint is the only identity** — never Subject, CN, SAN, Issuer or SerialNumber, every one
of which is chosen by whoever minted a self-signed certificate, i.e. by whoever presented it; and
**check the slice is non-empty, then index `[0]` ONLY, never iterate it**. The handshake's
`CertificateVerify` proves possession of the LEAF's private key and nothing else, so a consumer that
SEARCHED `PeerCertificates` for a known fingerprint would be spoofed by anyone who appended the
victim's (public) certificate at index 1. Under `RequestClientCert` the empty slice is the MAJORITY
case, not the exceptional one.

*THIS RULING NARROWS INVARIANT 3, ON THE PEER-BUS PLANE ONLY — said plainly so a reader grepping
"invariant 3" finds the departure.* Invariant 3 makes redeeming an invite "the ONLY way onto the bus
— including for peer buses". Admitting an adjacent bus on an operator-installed certificate
fingerprint instead of a redeemed invite is a narrowing of exactly that clause. It is scoped to the
peer-bus plane and does not touch agent enrolment, it costs precisely the properties listed under
*Given up* below, and it is restored in full when `INVITE-PEERGUARD` (`f5d91dbe`) lands. Invariant
11 is narrowed in the same breath and for the same reason: the invite blob is invariant 11's trust
anchor, and on this plane the operator's out-of-band channel takes its place.

*THE CONSTRAINT THE AMENDMENT MUST NOT LOSE.* The next-hop pin is keyed to **the record that carries
`-url`** — the next hop — and **never** to the record's bus id (`cmd/agent-bus/peer.go:68-75`;
`CONTRACTS-CLI.md:461-465`, section opened at `:354`, flag rows at `:377` and `:379`, worked
`-route-for` example at `:472-475`). The amendment request cited `CONTRACTS-CLI.md:392` for this,
which is the unrelated `config_seq` paragraph — corrected here. For a `-route-for` entry the address
belongs to a DIFFERENT bus than the record's bus id, so a destination-keyed pin would pin busC's
identity against a connection terminating at busB and would break every non-adjacent hop — the whole
A→B→C topology this section exists to serve. This is restated here not because the inbound chain uses
that field (it must not), but because it is the exact property that makes reuse unsound, and a reader
who loses it will reach for the pin again.

- *Given up:*
  - **Single-use, expiring, revocable PEER admission and its redemption audit.** An operator-
    installed fingerprint binding is durable configuration, not a redeemable credential: there is no
    expiry, no single-use property and **no online revocation of a peer link**. Withdrawing one will
    mean an operator edit plus a restart — *will*, not *does*: **no supported client can write
    `BusTrust.PeerClientTLSCertFingerprint` today.** RELAY-45 landed the durable record and the
    resolver at `ed77bba`, but not the operator surface; the only path to it at the time of writing
    is the internal `relay.PutTrust` Go API, and the CLI flag
    plus its `CONTRACTS-CLI.md` / `AGENT_PROTOCOL.md` entries are owed by `RELAY-45-FU-CLI`
    (`b9d645be-0849-4a62-9c50-3ab32e41fc8a`), which blocks RELAY-20. (RELAY-45 itself COMPLETED at
    `ed77bba`; the follow-up carries only the operator surface, not the record or the resolver.)
    Stated in the future tense on purpose: asserting an operator control that does not yet
    exist is the exact defect the 2026-08-08 security gate caught in the original (c), and it is not
    being repeated here. `INVITE-PEERGUARD` (`f5d91dbe`) still owes the redeemable-credential
    properties and is **DEFERRED, not satisfied**.

    > CORRECTED 2026-08-16: `RELAY-45-FU-CLI` HAS LANDED. `agent-bus peer add
    > -peer-client-fingerprint <hex>` writes the binding — flag at `cmd/agent-bus/peer.go:627`,
    > parsed at `:747`, into `relay.PutTrust` at `:891`. The rest of this bullet stands unchanged
    > and is the part that matters: the binding is durable operator CONFIGURATION with no expiry,
    > no single-use property and NO ONLINE REVOCATION — withdrawal is `RemoveTrust` under the
    > dirlock plus a restart.
  - **The invite blob as the peer trust anchor** (a narrowing of invariant 11 on this plane). The
    operator transfers the fingerprint out of band, so what anchors a peer link is the operator's own
    channel between machines they control, not invariant 11's invite-integrity property.
  - **Mutual authentication on the EGRESS dial.** Until `8192c3c7` lands we authenticate peers that
    dial us, and peers cannot authenticate us when we dial them. Under ruling (a) — SSH tunnels the
    operator holds both ends of — that asymmetry is bounded by the tunnel, which is exactly the
    ground (b) already stands on.
  - **Handshake-time rejection of an unknown peer.** Under `RequestClientCert` every refusal happens
    after a completed handshake, in the application layer, so an unknown or unbound peer still costs
    us a full TLS handshake before it is turned away.
- *Reverses when (mechanical, any ONE of):*
  - an inbound peer principal is resolved from `NextHopTLSCertFingerprint`, from `BaseURL`, from a
    route-for destination, from a bus signing key, from an attestation origin, or from certificate
    CN/SAN/Subject/Issuer/SerialNumber;
  - an inbound peer principal is resolved from any certificate other than `r.TLS.PeerCertificates[0]`,
    or by SEARCHING/iterating the presented chain for a known fingerprint;
  - the adjacent-principal credential is keyed off a record's DESTINATION bus id rather than the
    adjacent bus, or a record carrying `-url` loses next-hop keying;
  - any peer path appears in `unauthenticatedRoutes`, or a peer route registers with a nil registry,
    a nil `CrossBusTrust`, or a nil peer-identity binding — including as a registered-503;
  - the peer routes serve while the adjacent-principal binding table is **empty or unconfigured**
    (empty is the DEFAULT state until `RELAY-45-FU-CLI` ships an operator write path, so
    "unconfigured therefore unenforced" is the realistic bootstrap shortcut and is forbidden);
  - the peer routes serve a connection that presented no client certificate, or an agent session
    credential is accepted as a peer principal, or a peer-bus credential is accepted as an agent
    credential;
  - ruling (a) itself reverses — any bus bound to a non-loopback interface, or a tunnel endpoint
    shared with a non-operator.

  On any ONE of these, (c)'s ORIGINAL hard gate on `f5d91dbe` and `8192c3c7` is reinstated
  immediately and the peer routes come back down.

*COROLLARY OUTSIDE THIS FILE — now discharged, and recorded so the sequencing is legible.* When this
section was drafted, `internal/relay/doc.go` restated the ORIGINAL gate in prose ("RELAY-20 either
lands under those two tasks or amends the ruling in `DECISIONS.md` first"), and the worry was that
amending `DECISIONS.md` without touching `doc.go` would simply relocate the contradiction into the
code. RELAY-20 has since rewritten that passage (`internal/relay/doc.go:94-104`), which now reads
"RELAY-20 HAS MOUNTED THE ROUTES, AND THE RULING IT LANDED UNDER IS NOT YET WRITTEN DOWN" and names
MTLS-CLIENTAUTH + RELAY-45 as the gate it landed against. **This entry is the text that debt points
at**, so the two now agree rather than contradict. Note also that the original (c)'s citations
`doc.go:154-158` and `:172-175` had DRIFTED even before that rewrite, which is why this entry cites
the gaps by NUMBER and NAME as well as by line — line numbers in this file have now been wrong twice.

*LAND-ORDER NOTE — satisfied.* This section was drafted while RELAY-45 was uncommitted, so it cites
`peerstore.go` for both revisions; RELAY-45 has since landed at `ed77bba`, ahead of this text, so the
`:464-481` range is the one that is live. The quoted wording is identical either way, and the stable
anchor is the heading "WHICH CERTIFICATE, IN WHICH DIRECTION".

**(b-CLARIFIED) "Does not block FEDERATION" is not "deferred", and `INVITE-GATE` is not
deprioritised.**

Ruling (b) is a statement about the FEDERATION critical path ONLY: with no reachable listener, the
pre-auth attacker `INVITE-GATE` exists to stop cannot reach the port, so `INVITE-GATE` is not a
blocker of relay work while every bus sits behind an operator-held SSH tunnel. It is **NOT deferred
and NOT deprioritised (2026-08-08, user decision).** `INVITE-GATE` (`05a5216d`) remains **P0** and
**blocks `INVITE-HARDEN` (`d250d0dd`), `INVITE-REVOKE` (`d9def083`), `INVITE-CLIENT` (`4123e25d`) and
`INVITE-PEERGUARD` (`f5d91dbe`)** — a dependency chain recorded on each of those tasks, not implied.

Stated plainly because (b)'s own wording is in tension with it: the sentence "all other security work
is deferred until end-to-end relay is running" (`DECISIONS.md:4368-4369`) and the time-box at
`:4384-4386` govern **scheduling against the relay critical path**, and are never authority to lower
`INVITE-GATE`'s priority or to treat it as optional. This entry deliberately makes **no claim about
the current state of the enrol path**: `INVITE-GATE` is in flight as this is written, and a dated
append-only file is the wrong place to freeze a snapshot of code that is changing underneath it. Its
own task record carries that detail, and the 2026-08-14 finding there — that nothing downstream can
be satisfied by `INVITE-CLIENT` alone — is what establishes the dependency chain above.

- *Given up:* nothing. This clarification narrows no control and authorises no shortcut; it removes
  an ambiguity in (b)'s wording that could be read as licence to drop `INVITE-GATE`'s priority.
- *Reverses when:* never by topology change — this is a statement about the backlog, not the
  deployment. It is superseded when `INVITE-GATE` lands, or by an explicit, recorded user decision
  changing its priority.

### RELAY-20 mounted under this amended gate: three dispositions it forced

RELAY-20 (`701dc54d`) landed at `ed77bba` and mounts `/v1/peer/{enroll,relay,roster}` behind
RELAY-45's `RequirePeerPrincipal`. It is the first thing in this repo to authorise anything on a
client certificate, and that first-ness forced three decisions that exist today only in code
comments. Its reviewer blocked completion until they were recorded here, correctly: a decision that
lives in a package comment is not a decision the next task will find.

**Said first, because the backlog must not over-read the mount: the routes are mounted IN CODE and
are served by NO RUNNING BINARY.** `cmd/agent-bus/main.go` sets neither `Options.Peer` nor
`Options.PeerPrincipals`, so no shipped server registers this surface; wiring it is RELAY-24's.
Nothing below describes live production behaviour.

**(g) Client-certificate expiry is ENFORCED on the peer surface — `ca356fde` option (a), not (b).**
`ca356fde-0613-42cb-ac85-a629609d9c78` required an explicit choice between (a) reading the presented
certificate's validity window at the application layer and rejecting a connection outside it, and (b)
declaring expiry advisory. RELAY-20 chose **(a)**, inside `RequirePeerPrincipal` and **before** the
durable binding is consulted (`internal/httpapi/peermount.go:337-362`, `:415`;
`internal/httpapi/peerprincipal.go:272`). The verdict comes only from `crypto/x509.Certificate.Verify`
with the leaf as its own root — never a local `NotBefore`/`NotAfter` comparison — which is invariant 9
applied to date arithmetic, mirroring `client/pin.go`. A nil leaf and a zero clock both REFUSE rather
than pass. The reason it could not wait: `RequestClientCert` does no chain verification, so nothing on
this side had ever looked at a client certificate's validity window, and without this check an
operator-installed binding would **outlive the certificate it names** — a peer whose key material has
aged out would authenticate indefinitely, and expiry is the only automatic leak-containment bound a
never-revoked credential has.
  - *Supersedes IN PLACE, without editing it:* the "Accepted, open gap" paragraph at
    `DECISIONS.md:4560-4566`, which says "client-certificate expiry is enforced nowhere on this side".
    That is now false **for the peer surface only**. It remains TRUE for the agent surface, and
    `ca356fde` closes for agents only alongside `MTLS-BIND`/`MTLS-CROSSCHECK` — this ruling narrows
    that gap, it does not close it.
  - *Given up:* nothing on this surface. The cost is borne elsewhere: a peer bus whose certificate
    lapses is refused with no warning window and no grace period, so certificate rotation on a peer
    link is now an availability concern the operator must schedule.
  - *Reverses when (mechanical):* the validity check is moved after the binding lookup, is made
    advisory, is satisfied by a local date comparison rather than by `x509.Certificate.Verify`, or
    grows a skew/grace allowance that is not itself recorded as a decision.

**(h) Whether this bus federates is observable PRE-AUTH, accepted and bounded rather than closed.**
An anonymous `POST /v1/peer/relay` returns **403** on a federating build (the peer gate) and **401**
on a non-federating one (default-deny), so one unauthenticated request distinguishes the two
(`internal/httpapi/peermount.go:64-95`). This is a genuine NEW disclosure and is out of character for
this server, which elsewhere registers its catch-all THROUGH the same route helper so an anonymous
caller gets 401 for known and unknown paths alike, and whose discovery document deliberately does not
report which optional surfaces a build registered.
  - *Why it is not closed, since both cheap fixes are worse:* answering 401 here needs a
    `WWW-Authenticate` challenge (RFC 7235), and the only scheme this server speaks is `Bearer` —
    which would invite a refused peer to retry with a session token, advertising precisely the
    credential confusion this gate exists to prevent. Answering 401 only when NO certificate was
    presented closes the no-certificate probe and nothing else, since anyone can mint a self-signed
    certificate and probe again. Both trade a real property for a cosmetic one.
  - *Given up:* one bit — "this bus federates". No peer id, no roster, no count, and nothing that
    identifies a peer.
  - *What bounds it:* ruling **(a)** — every bus-to-bus link is an SSH tunnel and no bus listens
    publicly, so the prober must already have reached the loopback listener, which under (a)/(d)
    means it has already authenticated to a machine the operator controls. Stated that way on
    purpose: it is NOT true that "the pre-auth prober does not exist" — every enrolled agent on the
    loopback listener, and anything at the far end of the tunnel, can send this request. What is true
    is that the set of parties who can ask is bounded to parties the operator has already admitted to
    the machine, and what they learn is one bit. This is the same ground ruling (b) stands on.
  - *Reverses when (mechanical, any ONE of):* the triggers of ruling **(a)** — any bus bound to a
    non-loopback interface, a tunnel endpoint shared with a non-operator, or a second local user on
    any of the three machines; **or** the refusal stops being a single fixed 403 with no challenge,
    so that a probe distinguishes more than the one bit; **or** the discovery document begins
    reporting whether the peer surface is registered, which would disclose the same bit to any
    authenticated caller without even a probe.

**(i) ONE FACTOR authorises on the peer surface: the certificate alone. This narrows invariant 11.**
Invariant 11 requires mTLS and the session token BOTH, and requires them cross-checked. On the peer
routes the certificate alone authorises, and no bearer token is consulted. **This is a narrowing and
is named as one** — RELAY-45's own file explicitly delegated the decision to RELAY-20 rather than
letting a passing gate be read as a finished authorisation, so it is settled here rather than
inferred.
  - *Why it is not the weakening it looks like — and note this is NOT the "unsatisfiable" argument,
    which is false.* An earlier draft of this ruling said a peer bus holds no session token and has
    no route by which to obtain one, so a bearer requirement would be unsatisfiable. **That premise
    is wrong and is corrected here rather than shipped**: enrolment is open to a peer bus like any
    other client, and `internal/httpapi/peerprincipal.go:195-197` says so in terms — "a peer bus is
    also an enrolled principal on the buses it peers with, so a peer request may well carry a valid
    session token". A bearer requirement would therefore be **satisfiable but WRONG**, which is a
    different and stronger objection. The real argument is CONFLATION, and this section already makes
    it under (c-AMENDED): **a session token names an AGENT**, and an agent credential must never
    authorise a peer route. That is why the wrapper REMOVES the agent principal rather than merely
    ignoring it — leaving it in the context would let a peer handler pick up an agent identity and
    act on it as if it had authorised the peer request, which is exactly "a session credential
    accepted as a peer-bus credential".
    Invariant 11's cross-check clause ("a session token presented over a connection whose client
    certificate belongs to a different agent must be rejected") is answered here in its **strongest
    available form**: there is no pair to cross-check because a peer handler never sees an agent
    principal at all — the gate shadows it out and the auth middleware skips the bearer path — so an
    agent credential cannot open a peer route and a peer credential cannot present as an agent.
  - *Given up:* the revocable, time-bounded half of the credential pair, and more of it than the
    phrase suggests. A peer link is withdrawn by an OFFLINE operator action (`RemoveTrust`'s fsynced
    withdrawal floor, reached through the CLI under the dirlock per ruling (e)) — **not an online
    revocation; it needs a restart**, exactly as (c-AMENDED) records. So the only automatic bound on
    a peer's authority is its certificate's own expiry, which is why (g) had to land in the same
    task. **And that bound is weaker than it reads: nothing caps a peer's `NotAfter`.** The window is
    chosen by whoever minted the certificate — the credential holder — and `checkClientCertValidity`
    accepts any window `x509` accepts, so a peer may present a hundred-year certificate and (g) will
    pass it. Expiry is therefore an automatic bound only to the extent the peer chose to impose one
    on itself; a real bound needs `INVITE-PEERGUARD`'s expiring admission or an operator-side maximum
    lifetime, and neither exists today. Invariant 3's "invites are the ONLY way onto the
    bus, including for peer buses" is also deferred here to `INVITE-PEERGUARD` (`f5d91dbe`): the
    binding this gate reads is operator-installed, not invite-redeemed. That is the same narrowing
    named above under (c-AMENDED), restated at the point where it is actually spent.
  - *Reverses when (mechanical, any ONE of):* a **bus-scoped** bearer credential becomes obtainable —
    one that names the PEER BUS rather than an agent, at which point invariant 11's pair becomes
    constructible and applies unnarrowed (note the trigger is deliberately NOT "a peer bus can hold a
    session token", which is already true today and is the false premise corrected above); an
    agent principal becomes visible to any peer handler, or a peer principal to any agent handler; the
    bearer skip stops being derived from the same function that installs the certificate gate; or
    `INVITE-PEERGUARD` lands, which restores invariant 3 on this plane.

**Correction of record, made by supersession rather than by editing.** `DECISIONS.md:4639`
(RELAY-45's "What this task does NOT establish", heading at `:4637`) states "No route is mounted
(`RELAY-20`)". That was
true when written and became false at `ed77bba`. It is corrected here in place, in keeping with this
file's append-only rule; the rest of that paragraph still holds — **no running server constructs a
`*relay.PeerStore` for `Options.PeerPrincipals` (`RELAY-24`), and no CLI flag writes the binding
(`RELAY-45-FU-CLI`)**, so the surface remains operator-unreachable.

> CORRECTED 2026-08-16: the rest of that paragraph NO LONGER holds either. `cmd/agent-bus/main.go:1441-1442`
wires `Peer` and `PeerPrincipals`, and `cmd/agent-bus/peer.go:627` ships the
`-peer-client-fingerprint` flag. The peer surface is operator-reachable on a shipped build, and was
verified end to end in containers on 2026-08-15 (DEPLOY-6): three buses A↔B↔C with A and C not
peered, each recording `bus_path` `[A]` / `[A,B]` / `[A,B,C]`.

<!-- ===== END 2026-08-14 RELAY-6 amendment (feature-runner) ===== -->

<!-- ===== BEGIN 2026-08-14 SIGN-1-FU-OUTOFORDER-POISON ===== -->

## 2026-08-14 — SIGN-1-FU-OUTOFORDER-POISON: the store's strictly-increasing rule is retired. Two narrowings, stated before the justification

### What is narrowed

`internal/store/Append` no longer requires `m.Seq > head`. Two guarantees the old rule provided are
deliberately given up, and neither is a side effect:

1. **Duplicate DETECTION is now exact only within the RETAINED window.** Inside it, a re-applied
   sequence is caught and reported as `ErrDuplicateSequence`. Across the region retention has already
   dropped, `prunedHead` is a high-water mark, not a set: it PREVENTS the message being served a
   second time, but it cannot DISTINGUISH a genuine double-apply (an invariant 1 breach) from a
   merely very late first arrival, so the reissue is prevented but **not detected**. The old rule
   caught it. This one does not.
2. **An acknowledged, fsynced message may now be retained by nobody, and may never be delivered.** A
   sequence arriving at or below `prunedHead` returns `nil` and is dropped from the serving copy; a
   sequence arriving below the head is retained, but a reader whose cursor has already passed that
   position never receives it and its parked long poll is never woken. From that recipient's point of
   view the message was lost.

**Invariant 1 itself is NOT narrowed.** Ids are still server-minted and never reused, and the head
still never rewinds — it is assigned only under `m.Seq > s.head`. What is narrowed is the store's
defensive ENFORCEMENT POINT for invariant 1, not the invariant. The 2026-08-02
reaffirmed-without-narrowing ruling stands.

### The fact that forces it

SIGN-1 made a send two-step: `hub.Mint` allocates and durably BURNS a sequence so the CLIENT can sign
it, and only then does the client send. Reservations live for `hub.MintTTL`, so two agents holding
numbers at once and spending them in the other order is the ordinary shape of the protocol, not a
race. `hub.publish` calls `store.Append` AFTER the two-phase durable write has committed and fsynced
(invariant 4), so refusing the late arrival orphaned a record already on disk and set `h.poisoned`
permanently — and on every subsequent start recovery discarded it again, loudly, for ever. Any
enrolled agent could stop the bus with two mints and two sends. Reproduced live, twice, at P0.

Making the at-or-below-`prunedHead` case an ERROR would reopen a narrower version of exactly that
DoS: an agent holding a reservation across a byte-pressure prune would poison the bus. So it returns
`nil`.

### Why this is the right way round, and what is bought back

Invariant 6's ruling is that a discard is sanctioned and a SILENT discard is the defect. Both
narrowed paths are therefore made LOUD: the at-or-below-`prunedHead` branch logs a WARN that names
BOTH readings rather than reporting the benign one as fact, and the ordinary late insert logs a WARN
naming the delivery consequence and the follow-up. Both are pinned by tests that go RED when the call
is deleted.

Trading a whole-bus halt any enrolled agent can trigger at will for a missed delivery that is logged
is the right direction, and it is emphatically not the end state. The delivery gap is filed as
`SIGN-1-FU-REORDER-WATERMARK` and needs a reorder watermark — "no sequence <= W can still arrive" —
which this package cannot compute, because the answer lives in the hub's table of outstanding mints.
There is deliberately NO watermark API in `internal/store` for nothing to call. **Wiring relay ingest
into a served acceptor remains blocked on that task** (security's ruling, restated): `IngestRelayed`
lets a peer bus advance the local head with no reservation of its own, which widens the
message-suppression primitive from "any enrolled local agent" to "any peered bus".

Two smaller consequences are written up where they live rather than here: the age bound in
`pruneLocked` is now soft by at most `hub.MintTTL`, in the OVER-retention direction; and the two new
WARN lines are uncapped on the recovery path, filed with `SIGN-1-FU-STORE-LOGGER`.

### What must not drift back

Do not restore "strictly greater than the head". Do not turn the at-or-below-`prunedHead` branch into
an error. Do not sort the serving copy on `SentAt` — that destroys the sequence ordering `Since`
binary-searches. And do not let the P1 wording in `Append` drift back into claiming `prunedHead`
DETECTS a reissue; it does not.

<!-- ===== END 2026-08-14 SIGN-1-FU-OUTOFORDER-POISON ===== -->

<!-- ===== BEGIN 2026-08-14 SIGN-1-FU-REORDER-WATERMARK ===== -->

## 2026-08-14 — SIGN-1-FU-REORDER-WATERMARK: delivery order is the WAL commit index, not the sequence

Spec Server task `86c7d368-9733-434e-848d-05dd12fecf3a` (P0). Supersedes
`c829af9a-4418-437a-a0f8-34ef2f5d15d0`, whose note journal did not survive the recreate.

**This section SUPERSEDES the SIGN-4 freshness model wherever it is recorded in this file** —
including the entries at `:222`, `:371` and `:1005`, which each state that freshness comes from "the
server-minted monotonic sequence plus the recipient-side cursor". Those entries are dated and this
file is append-only, so they are **left exactly as they are**; this pointer exists because without it
a grep for the freshness rule lands on the superseded answer three times and the current one once.
The sequence is minted at reservation time, is therefore not monotone in delivery order, and is an
identity rather than a freshness token; freshness is enforced server-side at ingest.

### The defect

SIGN-1 made a send two-step: `hub.Mint` allocates and durably burns a sequence so the client can
sign it, and the send follows. So a message can be committed at a sequence BELOW a reader's cursor.
`store.Since` binary-searched for the first retained message strictly after the cursor and
`hub.notify` skipped any waiter with `m.Seq <= w.after`, so an actively long-polling reader — the
normal state of every agent on this bus — never received that message and was never woken. The
sender got a successful ack carrying a message id, and the record was durable and in the audit
trail. Security rated it HIGH: a deterministic targeted suppression and false-ack primitive, not
merely a lost message.

`SIGN-1-FU-OUTOFORDER-POISON` (commit `800fe25`) did not cause this and did not fix it. It removed
the HALT that was MASKING it: before that fix the bus stopped dead instead of skipping a message.

### What we rejected: the reorder watermark

The task as filed sketched a watermark `W` = (lowest outstanding mint) − 1, pushed down from the
hub, with the returned cursor clamped to `min(next, max(W, after))` so a reader re-reads the reorder
window. We rejected it, and the reason is worth recording because the shape is intuitive and will be
proposed again.

**It livelocks, and this was reproduced by execution rather than argued.** With a reader at
`after=3`, `W` pinned at 3 by one outstanding mint for sequence 4, and messages 5..200 present,
three consecutive polls returned the IDENTICAL batch 5..104 with `next=104, more=true`.
`DefaultBatchLimit` is 64, so the batch limit binds long before the window closes and the clamp puts
the cursor straight back. The client does not rescue itself: `client/watch.go` backs off only on an
EMPTY batch, and a clamped batch is never empty.

The cost to an attacker is one `/v1/mint` per `MintTTL`. Measured: one unspent reservation at
sequence 1 let the head reach 201, i.e. **200 durable, acknowledged messages withheld from every
reader on the bus** — so the clamp trades a targeted suppression of one message for a bus-wide
denial of delivery. Head-of-line blocking is the watermark's mechanism and not an incidental
side effect, so every bounded variant merely trades the livelock for a permanent tail suppression
plus a stall. Shortening `MintTTL` narrows the window and closes nothing.

### What we did instead: a delivery position, distinct from the signed sequence

The sequence was carrying two jobs that SIGN-1 pulled apart, and the fix is to stop conflating them:

- **`Seq` is IDENTITY.** Server-minted, signed by the client, never reused, never rewound. Unchanged.
- **`Pos` is DELIVERY ORDER.** Server-assigned at commit, monotone by construction. New.

`Pos` is the WAL commit index (`wal.Committed.CommitIndex`) — taken on the live path from the return
of `h.durable.Write`, which publish had been discarding, and on the recovery path from the
`wal.Committed` handed to `Hub.Apply`. Cursors, `store.Since`, `store.HasVisibleAfter` and
`hub.notify` all move onto `Pos`.

A late-arriving low `Seq` therefore gets a HIGH `Pos`, lands ABOVE every reader's cursor, is
delivered to every reader and wakes every parked waiter. There is no watermark, no reorder window,
no redelivery amplification and no starvation, and the delivery decision is O(1).

### Why the WAL index and not a counter of our own

A store-local `pos++` assigned per successful `Append` is NOT stable across a restart. `Hub.Apply`
skips records it cannot decode, and the set it skips can differ between runs — a schema bump skips
every message record. Any skip shifts every later position down by one, so a persisted client cursor
would silently point one message further along and SKIP a message. That is the very defect being
fixed, reintroduced through the recovery path. The WAL index is durable, monotone, never reused, and
replay is defined to run in commit order, so a position means the same thing before and after a
restart.

`Pos` is deliberately NOT persisted inside the message record. It is derivable from where the record
sits in the log, so this is **not** an on-disk format change: `store.RecordVersion` does not move and
no record-type number was reserved.

### Consequences accepted

- **Delivery order is no longer ascending `Seq`.** Inside the reorder window a reader sees a higher
  sequence before a lower one. This is not a regression to be fixed later: ANY fix that delivers a
  late arrival to a reader who has already passed that position must hand it over out of sequence
  order. The alternative is not "ordered delivery", it is "no delivery".
- **Every client-held cursor changes meaning.** `cursorVersion` moves to `v2`. A `v1` cursor is
  REMAPPED TO POSITION 0 — one replay of the retention window — and is NOT rejected. Rejecting it
  would be a permanent wedge rather than a one-off replay: a 400 surfaces as a rejection the watch
  loop does not retry, and nothing clears the stored cursor, so the same poisoned value is
  re-presented for ever. `AGENT_PROTOCOL.md` already documents at-least-once delivery and an
  idempotent-on-`message_id` handler, and there is precedent for exactly this announced one-off
  replay in the 2026-08-08 cursor-store change.
- **The retention age bound becomes exact.** It was documented as soft by up to `MintTTL` because
  the slice was ordered by `Seq` while expiry is judged on `SentAt`. Ordered by `Pos`, the slice is
  in commit order, which IS `SentAt` order, so the age loop no longer stops early.
- **The prune race dies structurally.** A late low sequence arriving after higher sequences were
  pruned now gets a high position and is delivered normally, instead of landing at-or-below
  `prunedHead` and being retained by nobody.

### NARROWING: pruned-region re-serve PREVENTION is traded for delivery

**This is a deliberate narrowing of a stated store-level property and it is called out separately
because the reviewer gate blocked the commit until it was written down.**

`store.Append` used to carry an at-or-below-`prunedHead` branch that refused to RETAIN a message
whose sequence was at or below the highest sequence retention had already dropped. Its documented
job was: across the already-pruned region, a re-arriving sequence is **PREVENTED from being served a
second time**, though it is **not DETECTED** — a high-water mark cannot distinguish a double-apply
from a genuinely very late first arrival, and the previous entry says so explicitly.

**That branch is deleted, so the PREVENTION is gone.** A sequence that was served, then pruned, then
re-arrives is now retained and served again.

Why that is the right trade, and not a regression:

- Under the new design that branch would drop **exactly the message this task exists to deliver**. A
  late low sequence arriving after higher sequences were pruned now gets the HIGHEST position, sits
  at the tail of the delivery order, and is deliverable to every reader. Refusing it would preserve
  the suppression in the one case an attacker can force — which is precisely the prune-race finding
  the security gate raised (roughly 1 GiB of max-size sends past a victim's outstanding mint made
  the victim's acknowledged message retained by nobody).
- What is actually lost is the ability to re-serve-proof a region we can no longer see. Within the
  RETAINED window nothing is lost: the new `bySeq` index detects a genuine double-apply exactly as
  before and still returns `ErrDuplicateSequence`, loudly.
- DETECTION across the pruned region was already absent before this change, so no detection was
  given up — only a prevention that was, by construction, indistinguishable from dropping a valid
  message.
- Re-serving is a DUPLICATE DELIVERY, which `AGENT_PROTOCOL.md` already requires every client to
  tolerate (at-least-once, deduplicate on `message_id`). Dropping is unrecoverable. Between an
  outcome the contract already covers and one it does not, this takes the former.

It is pinned by a test (`TestReorderWatermark…` in `internal/store/reorder_watermark_sign1fu_test.go`)
so the narrowing is a recorded decision rather than a silent behaviour change.

### What must not drift back

Do not restore the at-or-below-`prunedHead` refusal. It reads like a safety check and is now a
message-suppression bug.

Do not reintroduce a watermark, a reorder window, or any rule that holds a reader behind an
outstanding mint — that is the livelock above, and `MintTTL` is 15 minutes.

Do not derive the delivery position from anything but the WAL index — specifically not from a
per-`Append` counter, and not from `SentAt`.

Do not "restore ordering" by sorting the serving copy on `Seq` again. That re-opens the suppression
in full. The serving copy is ordered by `Pos`; `Seq` ordering is now maintained only where identity
needs it, by the duplicate-sequence index.

Do not make a `v1` cursor an error.

### Amendments to the immediately preceding section, recorded HERE

This file is append-only and a reversal is recorded **by** a later entry, never **in** the entry it
reverses. So the two sentences below are NOT edited where they stand in
`## 2026-08-14 — SIGN-1-FU-OUTOFORDER-POISON`; they are amended from here. Read that section with
these two substitutions applied.

**Amendment 1** — in that section's `### What must not drift back` list, the sentence *"Do not sort
the serving copy on `SentAt` — that destroys the sequence ordering `Since` binary-searches."* is
amended to read:

> Do not sort the serving copy on `SentAt`. ~~that destroys the sequence ordering `Since`
> binary-searches~~ — **AMENDED 2026-08-14 by SIGN-1-FU-REORDER-WATERMARK.** The prohibition on
> `SentAt` stands, but its stated REASON no longer does: `Since` no longer binary-searches on
> sequence. The serving copy is now ordered by the delivery position (the WAL commit index) and
> `Since` searches on that. Sorting on `SentAt` remains wrong because a wall-clock field is not a
> durable, monotone, never-reused ordering and cannot be recovered identically; use `Pos`.

The other three items in that list are intact, unaffected and still correct.

**Amendment 2** — in the same section, the sentence beginning *"Two smaller consequences are written
up where they live rather than here"* is **false in both halves** — `internal/store/store.go` now
states *"The age bound is EXACT again"*, and there are no longer "two new WARN lines" (this change
deleted the ordinary late-insert WARN entirely; the package now emits exactly ONE line, an ERROR on
the non-monotone-position fault). It is amended to read:

> Two smaller consequences were written up where they live rather than here. **Both were overtaken on
> 2026-08-14 by `SIGN-1-FU-REORDER-WATERMARK` and are recorded here only so the trail is followed
> rather than believed:** the `pruneLocked` age bound was soft by at most `hub.MintTTL` in the
> OVER-retention direction — it is **EXACT again** now that the serving copy is ordered by delivery
> position, which is commit order and therefore `SentAt` order; and the two uncapped recovery-path
> WARN lines (filed with `SIGN-1-FU-STORE-LOGGER`) **no longer exist** — the ordinary late-insert WARN
> was deleted, and `internal/store` now emits a single ERROR line, on the non-monotone-position fault.

<!-- ===== END 2026-08-14 SIGN-1-FU-REORDER-WATERMARK ===== -->

<!-- ===== BEGIN 2026-08-15 RELAY-24-FU-STOREMSGLOOKUP ===== -->

## 2026-08-15 — RELAY-24-FU-STOREMSGLOOKUP: no version bump for `origin_message_id`, and duplicate-origin-id resolution

Spec Server task `c6530638-7cca-4404-bc61-88ca6c2d30b9` (P1), documentation follow-up
`e02aa062-a0ec-48b6-9f39-eeee64801580`. `internal/store` gained a point lookup by local message id
(`Store.ByID`) and a correlation-key lookup by origin message id (`Store.ByOriginMessageID`), backing
the new `Message.OriginMessageID` / `Record.OriginMessageID` field so `relay.Forwarder` can resume a
relayed message after a restart. Full surface and on-disk shape are in `CONTRACTS-ONDISK.md`,
"`OriginMessageID` — the relay correlation key"; this entry records the two decisions the reviewer
and security ruled on, not the surface itself.

### Decision 1 — `store.RecordVersion` stays at 2; reserving a number would have been the destructive choice

`origin_message_id` is an additive, `omitempty` field on `store.Record`. The reviewer considered and
explicitly rejected bumping `RecordVersion` to 3 and reserving that number from the Spec Server
`ondisk-format-version` namespace. The reasoning, stated in the reviewer's own ruling and restated
here because a future maintainer will otherwise "fix" this destructively:

> `RecordVersion`'s own doc says an added OPTIONAL field does not move it, and `Record` decoding is
> non-strict about unknown fields. Bumping to 3 would have been **actively harmful**: `Record.Decode`
> does an EXACT version match, so it would discard all existing message history on upgrade.

**The meta-point worth recording on its own:** the standing rule in this repo (`CLAUDE.md`, "Parallel-
agent coordination") is that a record-type number or format version is **always reserved, never
hand-picked**, precisely so two agents working in parallel cannot collide on the same number. This is
a case where following that rule to the letter — reserving a number and bumping `RecordVersion` for
this field — would itself have been the destructive act, because `Decode`'s exact-version match turns
any bump into a silent history-discarding event for every existing data directory on upgrade. The
rule is about **collision avoidance for numbers that must move**; it was never a mandate that every
schema-adjacent change moves a number. Reserving one here would have "won" the coordination protocol
while losing the data it exists to protect. No reservation was taken, `RecordVersion` is unchanged at
2, and the reviewer's PASS explicitly ruled on this point (see the task's `kind=response` notes,
2026-08-14, question 4).

### Decision 2 — a duplicate `OriginMessageID` is resolved last-writer-wins, retained, never refused; it is peer-triggerable

Two distinct messages can arrive carrying the SAME `OriginMessageID` — concretely, one peer bus
presenting one origin message id under two different attested sender labels within its own
namespace, because the relay-ingest applied-key scope is the triple `(sender, idem.OpRelay, origin
message id)` (`idem.NewAgentScope`) and `hub.relayedOrigin` binds only the BUS halves of sender and
origin id, never the agent half — so the same origin id can be admitted twice under two agent labels
the same peer bus controls. `relay.PeerStore.AttestedSignerKey` bounds the blast radius to that
peer's OWN namespace; it cannot forge another origin bus's correlation.

**Resolution:** `store.Append` retains BOTH messages (refusing after the record is already fsynced
would orphan a committed record — invariant 4, and the same reasoning as
`SIGN-1-FU-OUTOFORDER-POISON`'s non-monotone-position branch), points the `byOrigin` index at the
NEWER message only, so the older copy becomes unresolvable by origin id (still resolvable by its own
local id via `Store.ByID`), logs the event, and unconditionally increments the exported counter
`Store.DuplicateOriginMessageIDs() uint64` — every occurrence is counted, whether or not it is logged.
Nothing is refused, nothing disconnects (invariant 10: this is reachable by a merely buggy or
adversarial-within-its-own-namespace peer, not a protocol violation against a shared connection).

**Known limitation, filed rather than fixed: `RELAY-24-FU-STOREMSGLOOKUP-THROTTLE`
(`cc7a463e-9804-41d4-8c5c-4d0e66efe2a0`, P3).** The operator-facing ERROR log line is throttled to
once per process (mirroring `hub.idemCapWarned`'s existing shape), but the throttle is
**unconditional** — it is not scoped per origin bus or per time window. A peer can therefore burn the
single log line early with one duplicate, after which a later, genuinely write-path- or
recovery-caused duplicate (the case this diagnostic exists to catch) produces no log line at all. The
counter (`DuplicateOriginMessageIDs`) still moves on every occurrence regardless of the log throttle,
so it remains the source of truth for operators correlating this condition; the log line is a
convenience, not the guarantee. This was accepted as a P3 rather than blocking, because the counter
is unaffected and the log-line loss is a diagnostics gap, not a data-loss or authorization gap.

Also filed from this task's security round, for completeness of the record: a pre-existing defect in
`store.copyMessage` (deep-copies `Body`, `Recipients` and `BusPath` but not `Signature`, also a
`[]byte`, on the live `Since` read path — the two new lookups widen an existing gap rather than
introduce it), `RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY` (`6e13a7d9-6ff0-49bb-a102-6ee1b69e9b51`, P1).

<!-- ===== END 2026-08-15 RELAY-24-FU-STOREMSGLOOKUP ===== -->

<!-- ===== BEGIN 2026-08-15 RELAY-24-BLOCKER-EGRESS (partial: seams only) ===== -->

## 2026-08-15 — RELAY-24-BLOCKER-EGRESS: the egress seams, and the one decision that is NOT mine

This task was to give the bus the ability to FORWARD a locally-published message to a peer, end to
end. Four of its five parts landed; the fifth is BLOCKED on a security decision recorded below, so
**nothing about cross-bus forwarding is live in this build** and no document in this repo may claim
otherwise until the blocker is settled.

### Decision 1 — `hub.Options.Egress`: the forward is called LAST, and it can never fail a send

`internal/hub` gains an OPTIONAL `Egress` interface (`Forward(store.Message)`, no return value) and
`Hub.publish` calls it at the very end, after the durable write, after `store.Append` and after
`h.notify(m)` — through `forwardOnward`, which recovers a panic and logs it at ERROR.

It has NO return value on purpose. There is no outcome the seam could report that `publish` would be
allowed to act on: the local send is acknowledged by its OWN durable write (invariant 4), so a peer's
queue, a peer's health and a peer's absence are separate best-effort-plus-outbox concerns. It is
called with `writeMu` held, which is safe only because `relay.Forwarder.Enqueue` is non-blocking BY
CONSTRUCTION (every queue send is a select with a default arm) — that structural property, not a
convention, is what keeps "a slow or dead peer never slows a local send" true from inside the write
lock. Nil `Egress` is behaviourally identical to the bus before the seam existed.

**The hole is named, not hidden.** The outbox record for a forward is written in a SECOND wal
transaction (inside `Forwarder.Enqueue`), not in the message's own `wal.Entry`. A crash between the
message commit and the outbox enqueue leaves the message durable and delivered locally with the
forward simply UN-OWED — no record, so no restart recovers it. That is a bounded AT-MOST-ONCE window
on the CROSS-BUS HOP ONLY; the local message is never at risk. Folding the outbox record into the
message's own entry would close it and would also change RELAY-15's one-record-one-job shape, so it
is deliberately NOT done here.

### Decision 2 — the origin attestation's `KeyEpoch` is the enrolment epoch in Unix MILLISECONDS

`attest.Attestation.KeyEpoch` is an unvalidated `uint64` whose only requirement is that the ORIGIN
bus assigns it. `auth.RosterEntry.Epoch` is a `time.Time` documented as bumpable on a future re-key.
`uint64(entry.Epoch.UnixMilli())` is therefore monotone per re-key by construction, needs no second
counter, no durable record of its own and no reconciliation after a restart.

A zero or pre-1970 epoch is recorded as **0**, not cast: `uint64` of a negative `int64` wraps to a
value near 2^64 — an epoch no later re-key could ever exceed, i.e. a monotonicity inversion produced
by a cast.

`NotAfter` is `issuedAt + relay.RetryHorizonCeiling` (which IS `idem.PeerOutageBudget`), because
`attest.Sign`'s own doc REQUIRES it be derived from the maximum relay retry window rather than a
plausible constant: an intermediate forwards verbatim and cannot re-mint. It contains **no** allowance
for clock skew between buses, and none was invented: `attest.Verify` applies its own
`ClockSkewAllowance` (5 minutes) on the verifying side, which is the only end that knows how far its
clock has drifted.

**An agent with NO messaging public key cannot be attested, and that is a legitimate state**
(`auth.RosterEntry.MessagingPublicKey` is optional at enrolment). Its message is not forwarded. No
key is fabricated, no unattested envelope is sent, and the LOCAL SEND IS NOT FAILED: the forward is
dropped, and logged at WARN naming the agent and the remedy (invariant 6).

### Decision 3 — the egress envelope's `BusPath` is EMPTY, and that is the value, not an omission

`relay.RelayedMessage.BusPath` is the path AS RECEIVED, NOT including this bus; `Forward(localBusID)`
appends our hop via `AppendHop`, whose doc names an empty input path as the ONE legal empty case,
meaning "this bus is the ORIGIN". `store.Message.BusPath` on a locally-originated message is
`store.LocalBusPath(busID)` — our own hop ALREADY in it — so copying it across would hand `AppendHop`
a path it is already on, and EVERY forward would return `ErrRelayLoop` and be dropped, on a bus whose
logs would say "loop" about a message that had never left.

Relatedly, the adapter forwards **only locally-originated** messages (`m.OriginMessageID == ""`).
`hub.publish` serves relay INGEST too, and rebuilding an ingested message here would claim OUR bus as
its origin, try to attest an agent in someone else's namespace (`attest.Sign` refuses that outright)
and erase the traversed path. Carrying an ingested message further is
`relay.AcceptOptions.Onward`'s job — a different seam — and `nil` there remains the documented LEAF
configuration.

### Decision 4 — the `"outbox"` WAL kind is registered as an applier (task item (d))

See `CONTRACTS-ONDISK.md`. It was in no applier map, so replay passed over it in silence. It is now
registered on the same conditional as the other federation kinds, with `unreplayedPeerRecords`
counting it on the path where no peer store could be built. `relay.Outbox.Attach` is new, modelled on
`invite.Store.Attach`.

## 2026-08-15 — RELAY-24-BLOCKER-EGRESS: the outbound-TLS blocker, resolved

**This section SUPERSEDES TWO NAMED PARTS of the section immediately above** — and they are named
exactly, because an earlier wording pointed at a "Decision 5 — BLOCKED" heading that does not exist
in this file, and then blessed "everything else" as unchanged while one of the parts it blessed was
false:

1. **"BLOCKED, and escalated rather than decided: the OUTBOUND peer TLS pin (invariant 11)"** — its
   closing heading. It correctly refused to write a second `InsecureSkipVerify`-shaped literal and
   listed two candidate resolutions; the first is taken here. Its closing paragraph is no longer
   true: the forwarder HAS production callers, the Registry is built at the composition root, peers
   ARE seeded into it, and the startup line no longer says the forwarder is unwired.
2. **The `RemoteRouter` paragraph under "Decision 1"**, which says `/v1/send` to a peer's agent
   "still answers `404 unknown recipient` on this build". It does not: the router is wired and such a
   send is **accepted (201)**. There is one live answer to that question and it is this one.

Everything else that section records — the `Egress` seam, `forwardOnward`, `Outbox.Attach`, the
applier registration, the envelope mapping, the `KeyEpoch` derivation, the empty `BusPath` — stands
unchanged and is still the reasoning for those pieces.

### Decision — export `client.PinnedTLSConfig` rather than write a second pinned dialler

`client/pin.go` gains one exported wrapper over the `pinnedTLSConfig` it already had:

```go
func PinnedTLSConfig(pins BusPinSet, clientCert *tls.Certificate) *tls.Config
```

**It adds ZERO new occurrences of the banned literal, and that is the entire point.** The literal
stays in one file, once, in one composite literal beside `VerifyPeerCertificate`, inside the scope
`client/guard_test.go` walks. Invariant 11's text remains literally true rather than "true in
spirit", and the AST guard keeps enforcing it mechanically — including the counting rule that fails
if the identifier is so much as NAMED in prose in that file, which it caught during this task.

The rejected alternative was a pinned dialler in `internal/relay`. It fails on invariant 11's own
words: `client/guard_test.go:22` walks only `.` and `../cmd/agent-busctl`, so that occurrence would
be BOTH a second one AND an unscanned one — "pushed into a package the guard does not scan", which
the invariant names as strictly worse than one loud, reviewed hole. Widening the guard to a third
root was possible but strictly more change for strictly less containment.

`client/` is deliberately NOT under `internal/` (invariant 7, so an agent can EMBED it), which is
exactly what makes this import legal from the server's composition root. That property was put there
for a different reason and paid for itself here.

**What the export does NOT do:** it takes no position on WHICH pins apply. It verifies against the
set it is handed, refuses an empty set inside the callback, and sets no `ClientSessionCache` — so
`VerifyPeerCertificate` runs on every connection. `crypto/tls` does not re-verify certificates on a
RESUMED handshake, so a caller that adds a cache to the returned config silently bypasses both the
pin check and the expiry check; the doc comment says so at the export, because the returned value is
an ordinary `*tls.Config` and nothing structurally prevents it.

### Decision — the outbound pin is resolved BY ADDRESS, at dial time, and an unpinned address FAILS CLOSED

`relay.Client` holds ONE `*http.Client` for ALL peers, while every peer route has its OWN
`NextHopTLSCertFingerprint`. The obvious wiring — put every peer's pin into one `BusPinSet` on one
`tls.Config` — is a **cross-peer confusion hole**: peer A's certificate would be accepted when
dialling peer B, out of entirely correct data combined wrongly, and every "does the peer connect?"
test would still pass. It was not done.

`NextHopTLSCertFingerprint` is the OUTBOUND, **address-keyed** pin — it names the certificate served
by whatever answers at the record's `BaseURL`, which for a non-adjacent destination is a DIFFERENT
bus from the record's `BusID`. (Its mirror image, `BusTrustRecord.PeerClientTLSCertFingerprint`, is
INBOUND and bus-principal-keyed. Conflating the two is a refuted design that appears to work.) So
the resolution follows the field: `cmd/agent-bus/relaydial.go` builds a map from **dial address** to
`client.BusPinSet` out of the durable peer routes, and `http.Transport.DialTLSContext` looks the
address up, builds `client.PinnedTLSConfig(thatSet, ourLeaf)`, and hands it to `tls.Client` +
`HandshakeContext`.

- **An address with NO configured pin is REFUSED before a socket is opened**, with an error naming
  the address and the remedy, and an ERROR line at startup naming every such address. There is no
  fall-through to an unpinned or default config: that fall-through is exactly how a pinning layer
  comes to be present in the code and absent on the wire.
- **The value is a SET, not one fingerprint**, for two legitimate reasons: rotation serves two
  certificates for one bus (invariant 11, which `BusPinSet` was built to model), and N route records
  with N different destinations can share ONE next hop and therefore one address. This is a
  deliberate, narrow departure from `PeerRecord`'s advice to "read each record's own pin rather than
  caching one per address": a consumer that knows which record it is acting for should follow that
  advice, and `DialTLSContext` structurally does not — it is given an address and nothing else.
  Divergent pins at one address are unioned and reported at **WARN**, not silently merged and not
  fatally refused: refusing would take a whole federation down over one stale record. The union
  never widens what is accepted at any OTHER address, which is the property that matters.
- **`ourLeaf` is the bus's own serving certificate.** `internal/buscert` mints ONE leaf carrying both
  `ServerAuth` and `ClientAuth`, and "one identity, both directions" is already decided in this file,
  so there is no second key and no second identity for a peer to bind.
- **No proxy.** A proxied https connection is established with `CONNECT` and bypasses
  `DialTLSContext` entirely, taking the pin with it. `Transport.Proxy` is nil, explicitly.

### Decision — `RemoteRouter` is wired NOW, and the precondition that gated it is met

`hub.RemoteRouter`'s "DO NOT INJECT A ROUTER EARLY" note makes it a precondition that no router may
be wired until the egress path carrying an admitted message onward exists and is DURABLE. It was
correctly left unwired while that was untrue. It is now true — the forwarder is constructed, its
outbox is the durable relay delivery table replayed by `wal.Open` and attached before the hub opens —
so the router is wired, and `POST /v1/send` to an agent on a seeded peer changes from an honest 404
into an accepted, durable, forwarded message.

The registry moved OUT of `newFederation` and into the composition root for a structural reason: the
same table is read by the egress half (hub admission, and the forwarder's address resolution), which
is wired BEFORE the hub, while the ingress is assembled after it. A registry built inside
`newFederation` would be a SECOND table — the handshake would populate one and the forwarder would
route on the other. `newFederation` keeps its validation posture: a nil registry is a construction
error naming what is missing.

Peers are seeded with `Agents: nil`, and an empty roster is the CORRECT value rather than a stub:
`Registry.Route` resolves by the BUS HALF of an id and never by roster membership, roster membership
is the separate `Knows` discovery convenience, and the address is operator configuration by design.
A peer whose roster we have never exchanged is routable, which is the documented design.

`Forwarder.Resume()` runs AFTER the seed and BEFORE the server serves — the third stage of the
mandated three-stage ordering. Seeding first is not cosmetic: `Resume` resolves every recovered job
through `PeerBaseURL`, so against an empty registry it takes the no-route arm for the entire
backlog. `Forwarder.Close` is registered so LIFO runs it BEFORE `walLog.Close` and before the
data-directory lock is released, because settlements are written THROUGH the WAL.

### What is still NOT true, stated so no document drifts into claiming it

**Multi-hop onward relay does not exist.** `relay.AcceptOptions.Onward` is still nil and the egress
adapter forwards only messages this bus ORIGINATED (`m.OriginMessageID == ""`). "This bus forwards to
a peer" and "this bus relays a message onward" are different claims; only the first is now true, and
the startup line reports them as separate fields (`egress_forwarder_wired`, `peers_seeded`,
`onward_relay`) precisely so an operator cannot read one as the other.

<!-- ===== END 2026-08-15 RELAY-24-BLOCKER-EGRESS (resolution) ===== -->

<!-- ===== BEGIN 2026-08-15 RELAY-24-BLOCKER-EGRESS (gate findings) ===== -->

## 2026-08-15 — RELAY-24-BLOCKER-EGRESS: the reviewer and security gate findings

The two sections above landed the egress path. Both gates returned CHANGES-REQUIRED against it. These
are the decisions taken to close them; the sections above stand except where named.

### Decision — a swept outbox record is reclaimed unless a checkpoint can ACTUALLY run

**This closes a wedge that disabled cross-bus egress permanently, and the shape of the mistake is the
reusable part: "has the method" is not "can do the thing".**

`Outbox.sweepLocked` decided whether to `del()` a record or merely mark it `expired` (charged, and
reclaimed later by a successful `Checkpoint`) with `_, ok := ob.durable.(outboxCheckpointer)`.
`*wal.Log` HAS `Checkpoint() error`, so that assertion succeeded the instant the composition root ran
`relayOutbox.Attach(walLog)` — but `main.go` opens the log with **no** `wal.LogOptions.Checkpoints`
(deliberately: the checkpoint dispatcher is not wired here), so `wal.Log.Checkpoint` returns
`"wal: checkpoint requires a MultiApplier"` unconditionally, and `Outbox.Checkpoint` has no
production caller to call it with anyway. Only `del()` decrements `retainedByPeer`, so the deferral
never resolved. Measured on the shipped wiring: after `MaxRetainedPerPeer` (256) lifecycle records to
one peer, **every** further `Enqueue` for that peer returned `ErrOutboxCapacity` for the life of the
process, with the clock 48 h past a 24 h retention window and every record still charged. Any
enrolled local agent could silently disable cross-bus egress to a peer.

**The fix is the smaller of the two candidates.** Passing `Checkpoints` to `wal.Open` was rejected:
it wires the checkpoint dispatcher for the whole server — every participant, snapshot, generation and
recovery path — to fix a capacity accounting bug, and `main.go` documents that dispatcher as
deliberately unwired. Instead `internal/wal` gains one read accessor,
`func (l *Log) CheckpointSupported() bool`, and the outbox asks THAT:

- the capability is a property of the OPEN (`LogOptions.Checkpoints`), fixed and immutable, so the
  answer is computed once in `NewOutbox`/`Attach` and cached;
- a log implementing `Checkpoint` but not `CheckpointSupported` is taken at its word (`true`) — the
  conservative direction, since it retains rather than drops, and it leaves existing test doubles
  unchanged;
- with no checkpointer the sweep drops and reclaims, which is exactly what the replay path already
  did (`durable` is nil during replay) and what a bus did before `Attach` existed.

**The answer is CACHED for a correctness reason, not a performance one, and this is the trap.** The
only caller is `sweepLocked`, which holds `ob.mu`; `wal.Log.CheckpointSupported` takes the LOG's
mutex; and `wal.Log.Write` holds that same mutex across the `Apply` that takes `ob.mu`. Asking the
question lazily is a lock-order inversion (`ob.mu -> log.mu` against `log.mu -> ob.mu`) and it
deadlocked a live forward against a concurrent sweep — observed, as a hung test, during this fix.
Nothing in `internal/relay/outbox.go` may call into the durable log while holding `mu`.

Proved by a crash-shaped capacity test on the production wiring
(`TestLocalMessageForPeerRecipientReachesForwarder/cross-bus egress is NOT wedged…`): `wal.Open`
verbatim from `main.go`, `Attach(walLog)`, fill a peer's retained share, assert the bound IS
enforced, advance the clock past retention, assert the share comes back. It is RED against the old
one-line predicate.

### Decision — the "do not re-forward an ingested message" gate reads the BUS PATH, not `OriginMessageID`

The stated control was `if m.OriginMessageID != "" { return }` in `relayEgress.Forward`. It was
**dead code**: nothing in this build sets `store.Message.OriginMessageID`
(`Message.WithOriginMessageID` has no non-test caller tree-wide, and the origin id rides on hub's
internal `publishRequest` for the audit content hash alone). Every relay-ingested message therefore
reached `envelope()` and `attest()`, and failed closed only by ACCIDENT — on the roster miss, with
`attest.Sign`'s `subjectBus != busID` refusal behind it. Deleting the gate left the whole suite green,
including the subtest whose own failure text named it.

The gate is now `m.BusPath[0]` not being this bus, which every message carries by construction:
`store.NewMessage` writes `LocalBusPath(busID)` for a local send, and `store.NewMessageWithBusPath`
writes the received path with our hop APPENDED for an ingest — where `hub.relayedBusPath` has already
refused an empty path and refused any path this bus already appears in. An empty path is
structurally unproducible and is DECLINED rather than assumed local, because forwarding it would
assert a provenance nobody recorded.

The accidental defences are KEPT as defence in depth and are now documented as the SECOND line, not
the first. The `OriginMessageID` check is kept as a belt and labelled as one — it cannot fire today
and the comment says so.

**Verified by deletion:** removing the bus-path gate from a scratch copy turns
`TestLocalMessageForPeerRecipientReachesForwarder/a message this bus INGESTED from a peer is never
re-forwarded` RED. The assertion that carries it is that the adapter LOGGED NOTHING — the only
observable difference between "declined at the gate" and "reached the attestation and fell over on a
roster miss".

### Decision — a conservative routing PRE-CHECK before minting the attestation, and why it is not a second routing authority

`relayEgress.Forward` built the envelope — `envelope()` → `attest()` → `attest.Sign` — BEFORE any
routing decision, so a purely LOCAL send with zero remote recipients still minted a full ed25519
attestation under the hub's global `writeMu`. Measured at 29.2 µs/op.

`relayEgress.routesToSomePeer` now gates the mint. It is deliberately a **strict superset** of
`relay.Forwarder.targets`, so a disagreement can only ever cost an unnecessary MINT, never a skipped
forward:

- it reads the **same `*relay.Registry` instance** the forwarder routes on (the composition root
  passes one table; the adapter now REQUIRES it, and refuses to construct without one);
- it **omits the split-horizon filter** (`NextHopAllowed`), which can only ever REMOVE targets.

`relayegress.go`'s "ONE CALL, NO SECOND ROUTING DECISION" comment argued against exactly this, and
has been reconciled rather than left contradicting the code: the argument holds for a pre-check that
could answer NO where the forwarder answers YES, which is the shape this one is built to exclude.
The forwarder remains the only routing authority — every message that passes is routed again, from
scratch, by `Enqueue`, and every drop is counted there.

### Decision — the outbound pin union is capped at `client.MaxBusPins`, and over that the address is REFUSED

`newPeerPinsByAddress` built its per-address accept-set with `client.NewBusPinSet`, which is
documented as explicitly **not** enforcing `MaxBusPins` ("construction is not the operator act; growth
is"). Feeding it N route records therefore bypassed the bound. `client/pinset.go` gives the reason the
bound exists: an unbounded, never-pruned accept-set degenerates into "accept every certificate this
bus has ever had", so a key compromised two rotations ago is honoured forever with nothing looking
wrong. This is reachable in the intended topology — N destinations routed through ONE adjacent hop
share one address — not theoretical. The previous defence recorded here ("the union never widens what
is accepted at any OTHER address") is true and sidesteps the bound rather than addressing it.

**The real bound, recorded:** at most `client.MaxBusPins` (**2**) distinct next-hop certificates may
be configured for one dial address. Two is the width of a rollover (invariant 11), not headroom. A
third is not a rollover, it is stale configuration, and the address is **refused** — it is not added
to the pin table, so nothing is forwarded through it and every dial to it fails closed, exactly as an
unpinned address does. It is **not truncated** to the first two: truncation would decide on the
operator's behalf, silently, which certificate stops being trusted, which is precisely what
`MaxBusPins`'s own doc refuses to do. The refusal is logged at ERROR with the address, the count and
the remedy, and it suppresses the "no pin configured" line for that address so the operator is not
sent the opposite way.

### Decision — the hub's documented lock order is CORRECTED, not restored

`internal/hub/hub.go`'s `writeMu` note said "nothing here may hold `writeMu` across a call into the
roster". `forwardOnward` now does: it runs at the end of `publish` with `writeMu` held, and the
injected `hub.Egress` reads the enrolment roster for the sender's messaging public key —
`auth.WALRoster.Get`, an exclusive `sync.Mutex`, the same object the hub sees through its injected
`RosterSource`.

The note is corrected rather than the code restructured. Moving the roster read off the lock would
mean either snapshotting the roster (which would attest a re-keyed agent under its OLD key for the
life of the process — the exact thing the live read exists to prevent) or deferring the whole forward
to a goroutine (which changes the seam's ordering guarantees relative to `notify`). Neither is worth
it for a latent inversion with no cycle: the roster's lock is held only across its own map
operations, `auth.WALRoster`'s durable writes go to the WAL, and no roster path calls into
`internal/hub` at all. So the order is total — `writeMu -> {waitMu, store lock, roster lock}` — with
nothing taking `writeMu` while holding any of the three.

**The rule that replaces the old prohibition:** an `Egress` implementation may take a lock that is a
LEAF with respect to the hub, and may not take one that anything reachable from the hub's own lock
could be waiting on.

### Correction — "never blocks" is a claim about PEERS, not about DISK

`relayegress.go` said `Forward` "NEVER BLOCKS", unqualified. `hub.forwardOnward` states the
defensible version — never *waits on a network peer*, structurally, because every queue send in
`relay.Forwarder.Enqueue` is a `select` with a default arm. The two wordings now match, and the
distinction is written down where it was missing: `Enqueue` writes a durable outbox record per
target, **two fsyncs each**, before it returns, under `writeMu`.

For a directed send that is one target. For a **broadcast** it is every configured peer —
`Registry.BroadcastTargets` returns them all regardless of recipients — so at `relay.MaxPeers` (64) a
single broadcast is up to **128 serial fsyncs** with the global write lock held, repeatable by any
local agent. That is LATENT today (`/v1/broadcast` answers 501, and a relayed broadcast is refused at
the far end with `ErrUnsignable`) and is deliberately **documented rather than fixed here**:
backpressure or batching on this path is a design change, not a tidy-up, and anyone enabling
broadcast must deal with it first. Filed as a follow-up.

### Correction — an unreplayed OUTBOX record is now COUNTED, and reported apart from configuration

`main.go` registers `unreplayedPeerRecords` for `relay.OutboxRecordKind` as well as the two
configuration kinds, but its `Apply` switch matched only the latter two — so on the no-peer-store
path an outbox record, a delivery this bus OWED a peer, was passed over counting **nothing**. That is
the silent discard invariant 6 rates as the actual defect, in the type written to prevent it, while
`main.go`, `CONTRACTS-ONDISK.md` and this file each asserted it WAS counted.

The switch now counts it, and the two halves are counted and reported **separately**, on their own
ERROR lines, because the remedies differ: peer-route and bus-trust records are configuration and
return intact once the store can be built; an outbox record names a delivery nothing in this run
owes, which no restart re-owes if its retry horizon has since passed.

### Correction — `cmd/agent-bus` is NOT an unscanned package (three comments)

`cmd/agent-bus/relaydial.go`, `cmd/agent-bus/relayegress.go` and `client/pin.go` all justified
exporting `client.PinnedTLSConfig` on the grounds that writing the pinned literal in `cmd/agent-bus`
would be an UNSCANNED second occurrence. The conclusion is right; the stated reason was wrong and
would mislead the next reader into thinking that directory is unguarded. `cmd/agent-bus` has a guard
of its own — `scanPlaintextListener` (`cmd/agent-bus/tlslisten_test.go`, driven by
`TestCmdHasNoPlaintextListener`) — which is STRICTER than `client/guard_test.go`: it parses every
non-test `.go` file there and bans the identifier OUTRIGHT, with no paired-`VerifyPeerCertificate`
exception. Injecting one into `relaydial.go` fails that test. The genuinely unscanned direction is
`internal/relay`, which is what the export avoids. All three comments now say so.

### Decision — `Enqueue` racing an unresumed outbox is ABSORBED, not refused, and this is a decision, not a discovery

The task's own description proposed the cheapest fix for a genuine hazard: a live `Enqueue` that runs
before `Forwarder.Resume` has re-offered a still-pending job from the last run can hand out a second
copy of that job, so a message already pending for a peer may be sent twice. The cheapest fix would
have made `Enqueue` refuse until `Resume` has run.

**That fix is deliberately NOT taken, and `internal/relay/forward.go` is UNMODIFIED by this task** —
the behaviour described here predates `RELAY-24-BLOCKER-EGRESS` (last touched by RELAY-19, `4113d24`)
and was previously recorded only in two code comments (`forward.go`'s "FORWARDING BEFORE Resume IS A
WIRING BUG" block above `Enqueue`, and `resumeJob`'s no-route arm), not in this file. It is written
down here because the reviewer gate on this task examined it and returned the same verdict: refusing
converts a startup-ordering MISTAKE into a TOTAL forwarding OUTAGE — no message from this bus reaches
any peer until an operator notices and re-runs `Resume` — whereas the duplicate a refusal would have
prevented is exactly what invariant 10's applied-key check absorbs at the RECEIVING bus. Recoverable
beats irreversible, the same trade `resumeJob`'s no-route arm already makes for a job whose peer is
unknown at Resume time.

What is not acceptable is the race being invisible: `Enqueue` emits a one-shot `Warn` — "relay
forwarding started BEFORE Resume: deliveries still owed from the last run have not been re-offered
yet, so a message already pending for a peer may be sent twice" — the first time it fires, rather than
per message, so a per-send line cannot bury it. The test that exercises this path in this codebase
therefore characterises PRE-EXISTING behaviour, not new behaviour introduced by this task.

<!-- ===== END 2026-08-15 RELAY-24-BLOCKER-EGRESS (gate findings) ===== -->

## 2026-08-15 — RELAY-FU-IDEM-METER-BY-PEER: foreign applied keys share a bus bucket

The applied-key table's fair-share denominator counts identity buckets. A relaying peer controls
the origin-agent label, so counting each foreign fully-qualified agent separately lets one
authenticated peer manufacture 32,766 buckets and reduce an honest local agent's share to two
while half the table remains free. This survives restart because the same labels are durable.

The store now requires its local BusID at the production composition root. It parses every
non-enrol agent ID and derives accounting as follows: a local agent retains its complete
fully-qualified ID, while every foreign agent is charged to the lower-cased bus half. The original
full agent ID remains the idempotency lookup scope; only fairness accounting is coalesced. Recovery
derives the same bucket from the persisted agent ID, so live and rebuilt denominators cannot drift.
Malformed IDs fail closed. This bound depends on relay validation continuing to pin the sender bus
to the authenticated peer's allowed origin bus; parsing alone proves syntax, not authority.

The deliberate fairness trade is per-bus rather than per-foreign-agent isolation. An honest peer
with one hundred agents receives the same foreign share as a peer with one agent, and one busy or
hostile agent behind that peer can temporarily consume its peers' shared allowance for the
retention window. That collateral effect is preferable to allowing a peer-controlled label set to
shrink every local agent's allowance without bound. Metering only the authenticated peer at the
HTTP wiring layer was rejected as insufficient: it can attribute resource use but does not change
the applied-key table's poisoned label denominator. Treating `OpRelay` as identity was also
rejected because an operation name authenticates nobody.

## 2026-08-15 — RELAY-47: onward relay wiring, and the seam deliberately left untouched

**`relay.AcceptOptions.Onward` is now the SAME `*relay.Forwarder` the egress half already builds**,
passed through `federationOptions.Onward` in `cmd/agent-bus/relaywiring.go`. `relay.Forwarder` already
satisfied the `OnwardForwarder` interface the acceptor calls through (compile-time assertion,
`internal/relay/accept.go`); the only wiring change is that the composition root now hands it over
instead of leaving the field `nil`. It is assigned through an **interface-typed** local rather than a
bare `*relay.Forwarder`, because a `nil` concrete pointer assigned straight into an interface value is
NOT a `nil` interface — the acceptor's `Onward != nil` check would pass and it would call through a nil
receiver. `nil` remains a legitimate LEAF configuration (a bus with no peer store has nothing to
forward with); the two states are distinguished at startup by `onward_relay=true`/`false`, not by
whether the field is set.

**This SUPERSEDES the "What is still NOT true" note in the 2026-08-15 RELAY-24-BLOCKER-EGRESS section
above** ("Multi-hop onward relay does not exist... `relay.AcceptOptions.Onward` is still nil"). That
was accurate when written and is not edited here — DECISIONS.md is append-only — but a reader relying
on it now would be wrong; this entry is the current word.

**The task's own brief asked for a second change that was NOT made, deliberately: relaxing
`cmd/agent-bus/relayegress.go`'s `BusPath[0]`-originated-here check.** That line guards `hub.Egress`,
which builds a NEW envelope claiming THIS bus as the origin. `hub.publish` calls it for relay-ingested
messages too (for the audit content hash), so relaxing the check would forward every ingested message
TWICE — once correctly through `AcceptOptions.Onward`, and once as a fabricated local origin, which
`attest.Sign` refuses outright on the sender-namespace check (invariant 2), buying nothing but a Warn
line per relayed message. The brief's own text partly agreed with this in a separate paragraph, so the
description was self-contradictory; the reviewer gate concurred. The check is unchanged, and its
comment now records why relaxing it would be wrong rather than merely that it is unchanged.

**`maxOnwardBusesPerMessage = 8` (`cmd/agent-bus/relaywiring.go`).** An onward hop is outbound work a
PEER can trigger on this bus — two durable fsyncs per destination before the ingest that requested it
even returns — so it is bounded the same way locally-triggered fan-out already is, not left uncapped.
Combined with the pre-existing per-peer in-flight ingest cap of 8 (`RELAY-22`), one authenticated peer
can hold at most 64 onward copies in flight, measured at ≤16 fsyncs and ≤8 outbound POSTs per inbound
relayed message. The count is taken from the envelope's **recipients** (distinct foreign bus halves),
not from the forwarder's resolved next hops, so it is an **upper bound on outbound copies, never an
under-count** — it can overcount today, on entirely correct traffic, whenever the egress split horizon
drops a destination already on the traversed path or the registry has no route for one; a first draft
of this reasoning claimed the two were exactly equal and was corrected during review before landing
(`RELAY-47-FU-FANOUT` tracks tightening the count to next hops instead of destinations).

**The 200 for `POST /v1/peer/relay` is UNCHANGED, by explicit operator ruling, and that is a decision,
not an oversight.** It was proposed that a message accepted and durably recorded but carried no
further should be reported differently, or refused at ingest instead of accepted. The operator
overruled that: durable-acceptance and delivered-to-recipient are different facts, the second will be
reported asynchronously once epic `ACK` (end-to-end delivery ACK/NACK) lands, and nothing here should
change ahead of that or block on it. `CONTRACTS-HTTP.md`'s "Onward relay" section states the 200's
meaning positively rather than as an open question.

**Known limitation, not fixed here: a pending onward hop does not survive a restart.** `store.Message`
carries no field for the origin bus's attestation, so an ingested-but-not-yet-forwarded envelope cannot
be rebuilt from durable state — at boot, `Forwarder.resumeJob` cannot resolve it and abandons the job
(logged at WARN; invariant 6 is satisfied, invariant 4 is not violated — the message genuinely is
durable on this bus, only the forwarding obligation is lost). Filed as `RELAY-48`. Loop prevention
(`relay.AppendHop`'s hop-count and repeat-visit refusals, `relay.NextHopAllowed`'s egress split
horizon, `relay.CheckIncomingPath`'s ingress backstop) and the fan-out bound above were proved by
mutation — stubbing each guard out independently turns a three-bus ring test RED — not by inspection
alone, per this project's rule that "the code looks right" is not evidence for a durability or
loop-safety claim.

## 2026-08-15 — DEPLOY-6: the container CMD binds `:8080`, and the image was unbuildable

Two deliverables: an image a bare `docker run -t` starts usefully, and `docs/THREE-BUS-DOCKER.md`,
an operator runbook for a federated A↔B↔C setup. Three decisions came out of it.

### 1. The image did not build at all, and had not for some time

Before any design question could be asked, `docker build .` failed at `d5018a6`:

```
cmd/agent-bus/relaydial.go:66:2: no required module provides package
  github.com/dodgymike/agent-bus/client
```

The builder stage copies `go.mod`, `cmd/` and `internal/` — and nothing else. That was complete when
DEPLOY-1 wrote it. It stopped being complete the moment `cmd/agent-bus` began importing
`github.com/dodgymike/agent-bus/client` for relay certificate pinning, because **`client/` cannot
live under `internal/`** — invariant 7 requires an agent to be able to embed it, so it sits at the
module root and is the one first-party tree that neither existing `COPY` line covers. The fix is one
`COPY client/ ./client/`, but the lesson is worth recording: **the Dockerfile enumerates the source
tree, so invariant 7's placement rule has a build-system consequence.** Any future package placed at
the module root for the same reason needs a `COPY` line, and nothing detects the omission except a
build. Nothing in CI builds this image today — filed as a follow-up.

### 2. `CMD` binds `:8080`; the BINARY's loopback default is untouched

The image's `CMD` was `-listen=127.0.0.1:8080`, repeating the binary's default. Inside a container
that is not a conservative choice, it is a broken one: the process binds the loopback interface of
its **own network namespace**, so `docker run -p 8080:8080 agent-bus` publishes a port that forwards
into the namespace and finds nothing listening. The bus starts, passes its own in-namespace
`HEALTHCHECK`, reports healthy, and is reachable by nobody. Observed directly — two otherwise
identical images, both published to host loopback:

```
old CMD (127.0.0.1:8080): ... read: connection reset by peer          -> probe exit 1
new CMD (:8080):          ok https://127.0.0.1:18087/healthz          -> probe exit 0
```

(The probe is `agent-bus healthcheck` against the container's own certificate — a real x509
verification, not `curl -k`. Invariant 11 forbids disabling verification to make something work, and
that applies to a diagnostic as much as to the product.)

**Why this does not narrow invariant 11.** Invariant 11 keeps `-listen 127.0.0.1:8080` because it
"bounds exposure, it does not replace TLS". That is a claim about **reachability**, expressed in the
only isolation primitive a bare process has: the interface it binds. A container has a stronger
primitive. The network namespace is the boundary, and `:8080` in an unpublished namespace is
reachable by strictly nobody outside it — the same property loopback buys on a host. So the change
is made **at the layer that knows it is in a namespace**, and `defaultListen` in
`cmd/agent-bus/main.go` is deliberately unchanged. A bare `agent-bus` on a host still binds loopback,
and `docker-compose.yml` still sets `-listen=127.0.0.1:8080` explicitly in its `command:`, so that
service is unaffected and stays deliberately unreachable.

**What actually moved is who owns the exposure decision**, and that is stated in the Dockerfile
rather than left implicit: no `-p` means container-network only; `-p 127.0.0.1:8080:8080` adds host
loopback; `-p 8080:8080` adds the LAN, through iptables rules Docker inserts itself, bypassing a host
firewall. The thing bounding a published port is **not** mTLS — the listener only
`RequestClientCert`s, so a presented certificate authenticates nobody — it is the **invite gate**
(invariant 3, `3cedcb7`): an un-invited `POST /v1/enroll` is refused 403. That is the honest
statement of the residual risk, and it is why this is defensible where it would not have been before
invite-only enrolment landed.

### 3. The image ships `agent-busctl` too, and pre-creates `/identity`

An image containing only the server ships a bus **no agent can enrol with** unless the operator also
has a Go toolchain — which contradicts both the ask ("just `docker run`") and invariant 7, under
which the compiled CLI *is* the client and an agent never hand-writes HTTP. So the builder produces
both binaries and the runtime stage carries both. Reached with
`--entrypoint /usr/local/bin/agent-busctl`.

`/identity` is pre-created `agentbus:agentbus 0700` for the same reason `/data` is: a named volume
mounted onto a path that does not exist in the image is created `root:root 0755` by Docker, and the
non-root user cannot write its Ed25519 private key there. It is deliberately **not** declared
`VOLUME` — the server never touches it, and a `VOLUME` line would make every plain
`docker run agent-bus` create a stray anonymous volume.

A named volume is also the portable answer to a trap found the hard way here: on a snap-packaged
Docker daemon, `/tmp` **inside the daemon is not the host's `/tmp`**, so `-v /tmp/x:/identity` mounts
an empty directory and the CLI reports "no invite file" for a file plainly visible on the host. The
runbook recommends named volumes, and `--invite-file -` (stdin) over an invite file entirely — which
also keeps the bearer secret out of `argv` and off disk.

### Verified end to end, in containers only

Three buses on a user-defined bridge network, A↔B↔C, A and C not peered. A message from an agent on A
reached an agent on C, and each bus's audit recorded the path as it saw it:

```
bus-a  bus_path=[A]
bus-b  bus_path=[A,B]
bus-c  bus_path=[A,B,C]
```

Two findings that corrected the brief this task was given:

- **`-route-for <C>` on A is load-bearing and was not in the brief.** Trust says whose messages you
  accept; it does not say where to send them. Withdrawing exactly that one route record and retrying
  the send returns `{"ok":false,"error":"send: the bus refused the request: unknown recipient",
  "status":404,"exit_code":7}`. A runbook omitting it produces a setup that looks configured and
  cannot deliver.
- **`agent-bus invite` has exactly one subcommand, `mint`. There is no revoke.** `internal/invite`
  supports revocation in the store and invariant 3 requires it, but the operator surface is unbuilt
  (the code says so itself: "revoke it (INVITE-REVOKE) once that surface exists"). Until it lands,
  **TTL is the only control over a minted invite**, which changes the advice on minting a pool: mint
  short and mint what you will use, rather than a standing pool nobody can withdraw.

The three-bus `message_id` correlation gap was also confirmed rather than taken on trust: the sender
saw `bus-t4yr4qzepvv7zjd6-11` on A and the recipient saw `bus-rupqkacueu6qce45-9` on C — the same
message, two ids, because every bus is authoritative on its own (invariant 1). `scripts/fed-smoke.sh`
asserts they are equal and therefore cannot pass; that is a defect in the test
(`RELAY-25-FU-CORRELATION`), and the runbook says so rather than presenting the script as a green
check. Correlate on `content_sha256` + `bus_path`, both stable end to end.

## 2026-08-15 — DEPLOY-6: security gate findings, and what changed because of them

The security gate returned **CHANGES-REQUESTED** on the entry above. It confirmed the invariant-11
reasoning in code rather than in prose — `defaultListen` at `cmd/agent-bus/main.go:41` unchanged with
an empty diff, `docker-compose.yml`'s explicit loopback `command:` unaffected, `InsecureSkipVerify`
still exactly once in `client/pin.go:260` paired with `VerifyPeerCertificate`, and the invite gate
genuinely wired (`enrolmentInviteRequired = true` → `ErrInviteRequired` → 403, covered by
`TestInviteGateEnrolWithoutAnInviteIsRefused403`). Four findings, all applied:

**The residual-risk statement was incomplete everywhere it appeared**, and this is the one worth
recording as a decision rather than a fix. All three copies said the thing bounding a published port
is the enrolment gate. That is true and it is not sufficient: **the gate bounds who can BECOME an
agent and bounds nothing else.** The routes that necessarily cannot authenticate — enrolment,
session begin, session complete, `/healthz`, `/v1/info` — have **no rate limiting at all**
(`AUTH-1-FU-RATELIMIT`, open). Two consequences are documented in our own code rather than
hypothesised: `internal/auth/session.go` states that an anonymous flooder can fill the session table
to `maxSessions` and deny session establishment to everyone until entries expire, and explicitly says
that must not be read as fixed; and `handleSessionBegin` answers distinguishably for an unknown agent
(404), a known-but-unbound one (200 + live challenge) and a bound one (403), which is an agent
enumeration oracle once the bus id is read from the public `/v1/info`.

Neither is introduced by containerising the bus. **But this is the change that makes them reachable,
and this is the document somebody reads before typing `-p 8080:8080`** — so understating it here
would be understating it at the exact moment it matters. Now stated in full in the Dockerfile's `CMD`
comment and in `docs/THREE-BUS-DOCKER.md` §1, which recommends `-p 127.0.0.1:8080:8080` plus a tunnel,
or no `-p` at all.

**The runbook's `ctl()` helper was broken**, found independently by the author and by the gate: it
placed `-v` after the image name, so the identity volume was never mounted and `agent-busctl` was
handed a flag it does not define (`flag provided but not defined: -v`, exit 2). The security tail is
worth keeping: the *obvious* wrong fix — deleting the `-v` — writes the agent's Ed25519 private key
into the container's writable layer instead of a named volume, which is harmless under `--rm` and
recoverable via `docker commit`/`export`/`cp` without it. Fixed by taking the volume as `$1` and
placing it before the image name, with a comment explaining the ordering. **Part 3 of the runbook is now
extracted from the markdown and executed verbatim** rather than transcribed by hand, which is what
caught it; a documented command that has never been run as written is not a verified command.

Two smaller fixes: the invite-pool example now runs `( umask 077; … > invites.ndjson )` instead of
`chmod 0600`-ing afterwards, which left ten bearer credentials world-readable for the length of the
loop; and the runbook now says explicitly not to `export` a variable holding an invite blob, since an
exported variable is readable from every child's environment and surfaces under `set -x`.

### Addendum — DEPLOY-6 gate round 2: both gates PASS, two late corrections

Reviewer and security both returned **PASS** on re-verification. The reviewer re-cleared the P1 with
its OWN extractor (independent of the author's), running Part 3's nine bash blocks verbatim three
times from a torn-down state and observing `bus_path` `[A]`/`[A,B]`/`[A,B,C]`. Security re-verified
the key-material claim concretely: `docker diff` on a non-`--rm` busctl container is **empty**, so
the agent's private key lands on the named volume and nothing reaches the writable layer.

Two late LOW findings were nevertheless real errors of fact in security-relevant advice, and are
fixed rather than deferred:

**"No `-p`" is not an isolation boundary on Linux.** All three copies said an unpublished container
port is reachable only from the same docker network. It is not: the host owns an interface on the
docker bridge and routes that subnet, so a local user can reach the container's bridge address
directly. Confirmed independently — a probe from the host to an unpublished bus at
`https://172.20.0.2:8080/healthz` **completed its TLS handshake**, failing only on hostname
verification (`certificate is valid for 127.0.0.1, ::1, not 172.20.0.2`), which is a name check, not
a reachability failure; an agent verifying by pinned fingerprint would have connected. This mattered
because "no `-p`" is the option the runbook *recommends*, and the DoS and enumeration surface it
honestly documents is therefore reachable by any local user on a shared host. `-p` governs
reachability from OFF the host; it does not hide the container from the host itself.

**The unauthenticated route list is six, not five.** All three copies enumerated invariant 3's list —
enrolment, session begin, session complete, `/healthz`, `/v1/info` — but `internal/httpapi/authmw.go`'s
`unauthenticatedRoutes` also contains `RouteDiscovery` (`/v1/discovery`), which is unauthenticated,
unrate-limited, and carries `bus_id` and `invite_required`. The doc now cites the allow-list in code
as the authority rather than restating the invariant's prose, which is the difference that would have
prevented the error.

Also corrected: this record previously said "the whole runbook is now extracted and executed
verbatim". Only **Part 3** is — §1's `docker run -t` is a foreground blocking command that cannot sit
in an extracted script. The operator-facing header was already scoped correctly; the rationale record
was not, and the reviewer flagged it as the same class of over-broad verification claim that produced
the original P1.

Finally, the follow-up filed for `CLAUDE.md`'s stale "enrolment is NOT invite-gated" claim is
**unnecessary**: it was already corrected at HEAD by `aade191`, and the round-1 finding was written
against a stale snapshot. `DOCS-4` should close as already-done rather than being worked.

---

## 2026-08-16 — AUTH-9: a session token MAY be written to disk, opt-in, default off

**This REVERSES the standing decision that a session token is NEVER persisted** (CLI §2, and the
"sixteen questions" §4-7 answer that sessions do not survive a restart — that second one is
UNCHANGED and still true; a persisted token is still destroyed by a bus restart, because the bus
forgets it).

**The reversal was requested by the operator, directly and explicitly, twice.** The second time, after
the security gate opened, in these words: *"I want this feature to write the creds to disk! so no
refusals on that, only on practical security / safety concerns."* Recorded verbatim because a
reversal of a security decision must be traceable to a person, not to a document that asserts its
own authority.

### What was wrong with the old decision

Nothing, on its own terms. Writing a bearer credential to disk is a real cost and the old entry
priced it correctly. What it never priced was the **other** side.

`agent-busctl` is one-shot. The client caches a session **in memory only**, so the cache dies with
the process. The bus holds each session for `SessionLifetime` = 1h against
`DefaultMaxActiveSessionsPerAgent` = 32 and **evicts nothing** — deliberately, so a stolen key cannot
be used to destroy a victim's live sessions. Under **invariant 7** an agent drives the bus by
SHELLING OUT. So every command costs one server-side session for an hour, and an agent working
faster than **one command every two minutes locks itself out of its own identity**, refused `503`
with no self-service recovery.

That is not hypothetical. 2026-08-15, live bus `bus-matv6xu7ronvdq7o`: `elastic-agent-1` took
12 × HTTP 200 then **32 × HTTP 503** on `/v1/session/complete`. The bus was restarted four times that
evening as the only available remedy — which punishes every agent on the bus to unstick one.

**The sizing assumption is the actual defect.** `internal/auth/service.go:46` justifies 32 as
"about sixteen times" a steady state of "TWO concurrent sessions". True for a **long-lived embedding
client**. False for the **shell-out** shape invariant 7 mandates. The healthy agents on that bus ran
one long-lived `watch`; the broken one shelled out per action. That was the whole discriminator.

### The decision

`--persist-session` / `AGENT_BUS_PERSIST_SESSION`, **default OFF**. When set, the token is written to
`<identity-dir>/session-<fq-agent-id>.json`, `0600`, `O_EXCL` at creation. `agent-busctl session
logout` discards it.

**Not** made the default, and not made automatic on hitting the cap: the safe default stays safe, and
the operator chooses per invocation.

### What this decision does NOT do

- It does **not** make sessions survive a bus restart. The bus forgets them; the file then holds a
  dead token, which is refused and re-handshaked.
- It does **not** free a session slot. `session logout` deletes the local copy only — there is no
  server-side end-session route. `AUTH-7` covers that.
- It does **not** change what a session authorises, or the session↔certificate cross-check.
- It does **not** narrow invariant 3. A session remains an opaque, revocable, server-side handle;
  persisting the handle does not turn it into a signed claim.

### The gate findings, recorded because the class matters more than the bugs

Both gates returned CHANGES-REQUESTED. Two are worth carrying forward as patterns:

**A binding check that compares two values from the same source is a tautology.**
`loadPersistedSession` compared `doc.BusURL` against `cred.BusURL` — both off the stored credential —
while `resolveBusURL` prefers `--bus`/`AGENT_BUS_URL`. The flag moved the CONNECTION without moving
the CHECK, so the token was presented to whatever `--bus` named. Proven leaking to a rogue loopback
listener, with a passing no-persist control that showed it was **new damage from persistence**, not a
pre-existing property of `--bus`. The comment above it asserted the opposite guarantee. Bind to the
value you will ACT on, never to a second copy of the value you already had.

**A cache that outlives the process silently redefines every "check the live state" command.**
`whoami --verify` calls `EnsureSession`, which is a cache lookup. Once the cache reached disk,
`--verify` returned exit `0` against an **unreachable bus** — failing at its one job, in exactly the
bus-restart case its own help text names. Fixed with `VerifySession`, which always reaches the
network. When adding a cache, audit every caller that promised freshness.

Also fixed in-task: `logout` orphaned a live token it could no longer delete; `session logout
--as/--json` exited 2; a fixed `.tmp` name raced across processes and was never swept; `os.Stat`
followed a planted symlink; the world-readable file was overwritten by the same command that warned
about it; and the "0 handshakes" figure in the docs was impossible from a cold store — the honest
number is one per hour.

## 2026-08-16 — Operator rulings: ADMIN D6 and D1 are OVERRULED; an operator principal is next

Three standing positions were changed by the operator on 2026-08-16, in response to a direct question
listing what was blocked and on whom. Recorded here as REVERSALS rather than left to contradict the
originals silently — each names what it overrules, so a reader who finds the old ruling first is
carried forward instead of acting on it.

### 1. ONLINE INVITE MINT IS NOW ALLOWED — overrules ADMIN **D6 "NO ONLINE INVITE MINT"**

D6 required the bus to be STOPPED to mint an invite, because minting appends to the write-ahead log
and takes the data directory's exclusive lock that a running bus holds. Operator ruling: *"I am
changing that ruling to allow online invite mint."* The `INVMINT` epic is unblocked.

**What this buys, concretely.** On 2026-08-15/16 the live bus was stopped **eight times** in one
session, purely to mint. Every stop invalidates every agent's bearer token and forces all of them
through the handshake again — the whole roster is disturbed to admit one member. The pre-mint-a-pool
workaround exists and works, but it trades a security property for convenience: spare invites sit on
disk unspent, and **there is no way to recall one**, because `invite revoke` is documented in three
places and implemented in none (`DOCS-11`).

**What this does NOT authorise.** An online mint route needs an OPERATOR PRINCIPAL to authorise it.
It must NOT ship as "any authenticated agent may mint an invite" — that would let any enrolled agent
admit arbitrary new agents, which is strictly worse than the bus-stop it replaces. See ruling 3;
that dependency is the reason it is being tackled first.

### 2. THE TUI IS WANTED — overrules ADMIN **D1**

D1 ruled that the browser console on loopback with no TLS was the deliberate operator surface.
Operator ruling: *"I want the TUI."* The `TUI` epic is unblocked.

Note the design tension this inherits rather than resolves: D1's console is unauthenticated because
it binds loopback only. A terminal UI that administrates, monitors and instructs agents is a far more
capable surface, and the same "loopback is the boundary" argument does NOT carry — that reasoning was
already found false on Linux once this session, when a probe to an unpublished container's
`172.20.0.2:8080` completed a TLS handshake. The TUI's authorisation model is an open question for
its epic, not something D1's reversal settles.

### 3. AN OPERATOR PRINCIPAL IS THE NEXT PIECE OF WORK

Operator ruling: *"Yes, that's what I want to tackle next."*

agent-bus today has NO admin/operator identity. Every authenticated caller is an enrolled agent, so
"operator-only" has nothing to authorise against. That single gap now blocks **four** things:

  - `AUTH-7` — an operator clearing one agent's sessions
  - `INVMINT` — the online mint above, which is otherwise a self-service enrolment hole
  - `INVMINT-2` — as recorded
  - the admin arm of `CONV` — "only the channel creator may change the recipient list, **or an
    admin**", which was answered by the operator and cannot be built

The recurring shape is worth naming: **every "an operator can do X" feature has been filed, designed
and then stalled at the same missing noun.** It is not four problems.

### Still OPEN, deliberately not decided here

`CONV` versus `COMMS-THREAD-FIELD`. The latter was filed `blocked` ON PURPOSE so a wire-level
`thread_id` could not be added un-measured; `CONV` is strictly heavier — it needs that field PLUS a
durable record type, a mint path, membership state, a recipient-change event and relay semantics for
all of it. Approving `CONV` would silently answer a question someone deliberately left open, so it
is left open here too, awaiting an explicit ruling.

## 2026-08-16 — RELAY-48: the origin attestation is carried on `store.Record`, not on the outbox record

**The decision:** a relay-ingested message's origin attestation is persisted as an OPTIONAL field on
`store.Record`, alongside the message it attests. It is NOT added to `relay.OutboxRecord`.

### Why the question exists at all

`RELAY-48` — a pending onward hop is durably abandoned at restart — looked like a one-line fix:
nothing calls `store.Message.WithOriginMessageID`, so `Store.byOrigin` is permanently empty, so
`Resume` cannot re-find a relay-ingested message. Add the writer, done.

**That fix is a trap, and the task record already said so.** With `OriginMessageID` set,
`ByOriginMessageID` starts HITTING — and control then reaches `cmd/agent-bus/main.go:1017`, which
refuses because this bus *cannot rebuild an origin envelope*: `store.Message` has no attestation
field (`grep Attestation internal/store/*.go` is empty), while `ValidateRelayRequest` REQUIRES one
(`internal/relay/message.go:674` → `internal/relay/signed.go:403`). The abandonment moves; it does
not go away. So the real blocker was never the writer — it was that a relayed-in envelope is
**unbuildable from durable state**.

### Why `store.Record` and not the outbox record

**One copy, next to the thing it is about.** The attestation attests THE MESSAGE. The outbox record
is per-HOP, so putting it there stores one copy per pending hop of a fact that belongs to the
message — a second, third and fourth copy free to disagree with each other and with the original.
That is the precise reasoning `internal/store/message.go:437` gives for refusing a `Pos` field on
`store.Record`: it *"would create a second copy free to disagree with the first"*. The `Seq` vs `Pos`
vs `OriginMessageID` conflation has caused **three** defects in this codebase for exactly this shape
of mistake. We are not adding a fourth.

**It is also the cheaper half of the trade, and that is a tiebreak, not the reason.** An optional
field on `store.Record` keeps `RecordVersion` at 2 under that type's own documented rule
(`message.go:461-471`), so no `POST /reservations` value is spent. Putting it on the outbox record is
on-disk surface and would need a reserved number. Had the correctness argument pointed the other way
we would have spent the number.

### What this does NOT decide

It does not decide the field's encoding, its size bound, or whether an absent attestation is legal on
an ORIGINATED message — it must be, since this bus mints no attestation for its own traffic, so the
field is optional by construction and its absence is meaningful rather than an error.

### The writer placement, recorded because it is a silent-no-op hazard

The write must happen between `store.NewMessageWithBusPath` and `m.Encode()` —
`internal/hub/hub.go:1757-1761` at `6d1cd8f`. `Message.Record()` is the only thing that carries the
value to disk and `Encode()`'s output becomes the WAL entry body, so **anywhere after `Encode()` is a
silent no-op**: `h.store.Append(m)` still populates `byOrigin` in the LIVE process, so a late writer
passes every non-restart test and fails only after a crash. Any test for this must restart, or it
cannot distinguish a correct fix from a broken one.

Placing it in that window moves neither hash: `SigningMessage()` omits `OriginMessageID`
(`message.go:728`), and `auditContentHash` derives from `SigningMessage` (`internal/hub/audit.go:169`).

### Correction to the task record

`RELAY-48` says the fix touches `internal/relay`. **It does not need to.** The attestation is already
in hand one layer up: `cmd/agent-bus/relaywiring.go:125` receives the full `relay.RelayedMessage`,
attestation included, and builds the hub request at `:126`. The pass-through is a `cmd/agent-bus`
change, so the real boundary is `internal/store` + `internal/hub` + `cmd/agent-bus` — and the epic's
`internal/relay` work does not collide with it.

## 2026-08-21 — ACK-6 and ACK-9: the delivery-acknowledgement plane reaches an agent

> **LANDED as `dc04a95`**, together with `ACK-15`, `ACK-16`, `836c9ff8` and `AUTH-10-WIRING`. This
> entry carried a "NOT LANDED" marker until then, because an earlier draft written in landed tense
> was REFUSED at the integrator gate as a false dated claim — correctly, and that is the 2026-08-07
> incident shape.
>
> One consequence of landing the whole wave, recorded because it is better than what this entry
> originally said: `ACK-6`'s invariant-7 blocker is **SATISFIED, not waived.** The reviewer's remedy
> was "a `DECISIONS.md` section **OR** land `836c9ff8` first"; the wave landed `836c9ff8` *and*
> `ACK-15`, so there is no deferral left to authorise. The authorisation text below was written for a
> deferral that no longer exists — kept because its reasoning about WHY the CLI could not be written
> earlier is still the record of that decision.

`ACK-6` adds `POST /v1/ack` (the RECIPIENT declares receipt) and `ACK-9` adds
`GET /v1/ack/<correlation-key>` plus `agent-busctl ack-status` (the SENDER reads it). They must land
together because `internal/httpapi/server.go` and `CONTRACTS-HTTP.md` interleave both halves.

### Inbox delivery is NOT receipt — an explicit application ACK is required

`ACK-1` ruled it and this implements it. The reasoning is not ergonomic, it is structural: **the bus
does not verify message signatures** (`internal/store/message.go:260-270`), so auto-ACKing on poll
would have the bus assert, on the recipient's behalf, a fact only the recipient can establish.
Supporting: the cursor is opaque and client-held, so inferring receipt would need strictly MORE
server state, and a replayed cursor would fire many receipts for one message.

### Authorization is STRUCTURAL, not a comparison — and the load-bearing line is not the obvious one

The recipient IS the authenticated principal AND is the second half of the `(correlation key,
recipient)` key, so an agent can only ever reach the row naming it. `internal/httpapi/ack.go`
compares the frame's `recipient` and then **discards** it.

Proven by mutation, and the result is worth recording because it inverts the intuition: deleting the
403 check while leaving `Recipient: recipient` at `internal/httpapi/ack.go:248` yields a third party
getting `200 unknown` with the victim's row untouched and **no row minted**. Deleting BOTH gives full
privilege escalation. **So `:248` is the security boundary and the 403 is loudness.** A future
reader tidying away "the redundant 403" would be removing a diagnostic; tidying away `:248` would be
removing the control.

### §13.3's uniform answer has NO owner in the record shape — it lives entirely in the handlers

Nothing in `internal/ack` enforces it. Malformed, swept, never-existed and someone-else's must all
return one byte-identical answer, or the route is an existence oracle. A review enumerated **10
shapes on `POST /v1/ack` and 9 on `GET /v1/ack/<key>`**: all negatives are byte-identical. The sole
deviation is a 413 at the 4 KiB frame cap, decided on caller-chosen bytes independently of bus
state — not an oracle.

Round 1 found a real one: a malformed or ABSENT `correlation_key` answered **500 with two unthrottled
ERROR lines** while an unknown key answered the uniform `unknown` — the fourth of the four facts,
reachable by omitting a field. The cause is a seam worth remembering:
`relay.ValidatePeerAckRequest` deliberately validates NO ids, because the PEER route validates them
inside `AuthorizePeerAck` — **and the agent route has no `AuthorizePeerAck`.** The validator was
inherited without its other half.

### What is NOT reachable yet, stated plainly

`POST /v1/ack` ships with **no CLI subcommand**, so **no row can reach `delivered` through the
supported client surface** and `ack-status` can in practice only report `accepted`/`in_flight`.

This is a deferral with a hard technical cause, not a scheduling excuse:
`relay.ValidateAckAttestation` (`internal/relay/ack.go:549-553`) requires a 64-byte signature on
every recipient-sourced outcome, and `internal/signing` has **no canonical-ACK-bytes function**,
which `ACK-CONTRACT.md §6.3` mandates. A CLI written before that encoding exists would sign
*something* and freeze an unspecified format onto the wire permanently. Sequenced as `836c9ff8`
(canonical bytes) → `ACK-15` (the subcommand).

### `internal/ack/store.go:479` — deliberately NOT "fixed", and why

The WAL error is wrapped without `ErrNotDurable`, so `errors.Is(err, ErrNotDurable)` is false on a
write failure. Two agents and a reviewer converged on the same answer: **`ErrNotDurable` means "no
log attached"**, and wrapping a FAILED WRITE in it would make `errors.Is` true for two conditions
with **opposite remedies**. Invariant 4 holds structurally — a non-nil error is returned, the memory
row stays `accepted`, nothing is acknowledged. What is wrong is only the status CLASS: 500 where 503
is right on a disk failure. If a caller ever needs that branch it wants a NEW sentinel, not this one
widened. Pre-existing; filed `c4dc6b6b`.

### A guard that could not fire, found by mutation after two gates passed it — AND IT IS A BLOCKER, NOT A NOTE

> **This was initially mis-triaged BY ME as a follow-up** (`ACK-16`) and written up here as an
> observation. That was wrong: the reviewer raised it as a BLOCKER with security impact, and
> recording a defect is not the remedy it prescribed. An integrator refused the commit over it and
> was right to. Corrected here rather than quietly, because the mis-triage is the more instructive
> half: a finding written into a decisions file can LOOK addressed while the code is unchanged.

`internal/httpapi/ackstatus.go:292` — changing the per-principal `s.ackWaiters.acquire(sender)` to a
GLOBAL bucket leaves the entire package green, even with the newly-added internal test present. One
agent could then lock every other agent out of `?wait=`, which `ackstatus.go:65-79` and
`CONTRACTS-HTTP.md:456` both claim is structurally impossible.

That is the **fourth** guard in this project written specifically to catch a defect that could not
fail, and the fourth found by mutation rather than review. Recorded here because the pattern is now
the most reliable defect-finder we have: **mutate every guard, including — especially — the ones
that look obviously correct.**

Related and worth carrying: "17/17 mutations RED" means 17/17 of the mutations CHOSEN. Removing only
the outer `ValidateCorrelationKey` stays green, and that is genuine defence in depth rather than a
vacuous guard — but a mutation count is a statement about the chooser, not about the code.

## 2026-08-21 — RELAY-51: the wire-version rollout is READERS-FIRST, and health is not a rollout gate

> The rehearsal and its two findings ARE landed (`14ed009`, `scripts/fed-smoke.sh` +
> `docs/THREE-BUS-DOCKER.md`). `RELAY-23` itself has NOT landed, so the hazard below is
> **prospective**: it was reproduced against simulated accept-only and emitting builds in `/tmp`,
> never against code in this repo.

### The hazard, reproduced rather than reasoned about

A strict decoder plus a non-retriable 4xx loses messages permanently on a partial deploy.
`internal/relay/handshake.go:334` uses `DisallowUnknownFields()`; `internal/relay/client.go:78-84`'s
`Retriable()` is true only for 408, 429 and 5xx. So a pre-`RELAY-23` peer answers a versioned frame
**400**, the sender treats 400 as FINAL, and the message is **abandoned, not retried**:

```
B: status=400 code=invalid_request  err="json: unknown field \"protocol_version\""
A: "relay forward failed" attempt=1 retriable=false
A: "an outbox job was ABANDONED; this message will never reach the peer"
```

`attempt=1` — the retry horizon is never entered. Seven configurations were run; the hazard and both
staging orders were each observed, not inferred.

Note `RELAY-23` has NOT landed, so this is PROSPECTIVE. The rollout was rehearsed against simulated
accept-only and emitting builds in `/tmp`, never in the repo.

### The decision: READERS-FIRST, two deploys, no downtime window

Stage 1 ships an **accept-only** build everywhere (the field exists, nothing assigns it, so an
`omitempty` field is simply absent from the wire). Stage 2 enables emission. Because the accept-only
build and an un-upgraded bus interoperate **in both directions**, stage 1 rolls one bus at a time in
any order — proven by `mid→old→old` passing.

**Drain-and-restart was REJECTED even for the dev set**, and the reason generalises: the drain must be
VERIFIED, and the only instrument that would verify it does not exist. The durable
`"state":"abandoned"` outbox record lives in the WAL and **no subcommand surfaces it** (`agent-bus
log` reads the audit file). An unverifiable drain is an assumption, and this task exists because of
one.

**`ACK-3` is already correctly positioned as stage 1**: its route shipped with **no production
caller**, which IS readers-first. `ACK-5` (emission) must not be enabled until every bus serves
`/v1/peer/ack`.

### Two things found while rehearsing that are worse than the hazard

**1. A transit bus's ORIGIN logs nothing.** In `A(old) → B(new,emits) → C(old)`, the origin logged
ZERO forward failures — only B logged it. **The loss is completely invisible from the bus that owes
the delivery.** Combined with there being no operator view of an abandoned job, a federation can be
dropping messages with nothing on the owing side to show it.

**2. `/healthz` IS NOT A ROLLOUT GATE.** `httpapi.mountPeerSurface` is all-or-nothing: an incomplete
`PeerSurface` registers NO peer route, so every `/v1/peer/` path — relay included — answers **404**,
equally non-retriable — **while the container healthcheck still reports `healthy`**. The only signal
is a startup `level=error` line, `FEDERATION IS NOT SERVED`.

So a bus can be healthy and silently deaf to the entire federation. **Any rollout gate must assert the
ABSENCE of that error line, not the presence of health.**

### The standing rule this confirms

A strict decoder defeats precisely the forward-compatibility a version field exists to provide.
Adding the version did not make the envelope forward-compatible; it made the NEXT change
**diagnosable**. That is worth having — but it is not tolerance, and the difference is paid for in
deploy discipline. The strict decoder stays: an unknown field on a federation surface is a real
signal.

## 2026-08-21 — ACK-5: a terminal outcome travels backwards, and no intermediate records it

> **NOT LANDED at the time of writing.** The code sits uncommitted in a worktree; the integrator
> commits. This entry is written in the present tense about the change, not about `main`, and the
> distinction matters — a dated claim that something IS in `main` when it is not is what the 2026-08-07
> integrator refusal exists to prevent.

`ACK-5` makes plane C reachable beyond one hop. A terminal outcome raised by a recipient at the far
end of `A→B→C` now travels backwards, one hop at a time along the traversed `bus_path`, and stops at
the origin bus — the only bus holding a durable sender-visible lifecycle row. Correlation is §3's key
(the origin's server-minted message id) and nothing else; no fourth identifier is minted (invariant
1). Three decisions in it are real, in the sense that a competent implementer would have chosen
otherwise on at least one.

Invariants read in full before writing: **2** (fully-qualified ids), **4** (nothing acknowledged
before durable), **6** (metadata and routing only; loud, specific discard), **10** (idempotency, and
the two questions before any disconnect), **11** (TLS, mutual, and the pinned peer client).

### 1. NO lifecycle row is written at an intermediate or terminal bus

**Decision.** Only the ORIGIN bus writes a sender-visible row. `hub.recordAcceptance` already returns
early for relayed ingest; `ACK-5` does not change that, writes no row of its own on any surface, and
adds no durable state whatsoever — no record type, no file, no index.

**The alternative considered: write rows at relayed ingest**, so every bus on the path has something
to settle and the existing peer-ack path works unchanged at every hop. It is the obvious design and
it was rejected for three reasons, in descending order of weight.

- **The row would be readable by NOBODY.** §13.3 authorises exactly one reader — the ORIGINAL SENDER,
  on the origin bus. An intermediate's row has no reader at all, so it is durable state that answers
  no question anyone is entitled to ask.
- **The cost lands on a PEER-DRIVEN path.** Each row is a separate two-phase transaction under the
  global write lock, so it is one fsync per recipient, up to `store.MaxRecipients` (64) per relayed
  message. `hub.recordAcceptance`'s own cost note already names this as the thing to re-check before
  any task gives a send several recipients — today the local loop runs exactly once, and relayed
  ingest is precisely the caller that passes several. Taking that cost for an unreadable row would
  make the hazard live, driven by a remote bus rather than by a local agent.
- **It would put SEVERAL rows behind one correlation key**, which `ACK-4-FU-RECIPIENT-BINDING` says
  must not happen before its fix lands.

**The cost accepted instead** is stated in decision 2: a transit acknowledgement depends on the
relayed MESSAGE still being retained.

### 2. The recipient acknowledgement boundary now consults the message store — on ONE arm

**Decision.** `internal/hub/ack.go`'s "THIS BOUNDARY NEVER CONSULTS THE MESSAGE STORE" is **narrowed,
in place and marked**. On `ack.ErrNoRecord` and only there, `hub.transitAck` asks
`store.RelayProvenanceByOriginMessageID` one routing question: does this bus hold a **relayed** copy
under this correlation key naming the authenticated principal as a recipient? If so the acknowledgement
is AUTHORISED and reported as transit; nothing is written.

**The original reasoning (ACK-6), which still governs every row.** The lifecycle row is the authority
and the message store is not consulted, because the two retention regimes differ — a message is kept
for 1 day or 1 GiB, whichever bites first, a row for `ack.Retention` (24h) from `accepted_at` — so
requiring the message to still be held would refuse an acknowledgement for a message the recipient
demonstrably received and is holding a copy of. And "two fields that must agree are two fields that
can disagree" applies to two TABLES with far more force.

**Why the narrowing was forced.** A relayed message has no row here, by decision 1. So without a
second authorisation path the recipient on bus C is told the uniform `unknown`, and §8.4's rule that a
hop receipt never converts to delivery leaves plane C unreachable beyond one hop. That is the defect,
not a nicety.

**Why the original reasons do not reach this arm.**

- **The two-tables-can-disagree hazard cannot arise, because the paths are DISJOINT BY
  CONSTRUCTION.** A message is either relayed or locally originated, never both, and only the
  locally-originated one ever has a row. `transitAck` refuses a locally-originated message outright —
  this bus IS its origin, so forwarding would send a terminal outcome to a bus that never owed us one
  and turn an expired row into an unsolicited network contact. No row is ever settled, reopened or
  overridden on the strength of a message lookup.
- **No oracle is added.** The principal comes from the request context and there is no field another
  agent's id could arrive in, so an agent can only ever ask whether IT was addressed. A non-member and
  a miss are byte-identical, and ids are compared with `==`, the same rule `store.Message.VisibleTo`
  used to decide whether the agent could ever have been HANDED the message — matching more loosely
  here would authorise acknowledging a message never shown. The accessor is body-free by construction
  (`internal/store/provenance.go`: recipients, `bus_path`, a `relayed` flag; no body, no sender, no
  signature, no timestamps), which is invariant 6's line drawn at the seam rather than trusted to a
  caller.

**The cost, accepted rather than argued away.** The retention objection is REAL on this arm: a transit
acknowledgement stops working once the relayed message is pruned, which the 1 GiB cap can make happen
well before 24h. The recipient is then told the uniform `unknown`. The alternative is decision 1's
unreadable rows, and this bus genuinely has nothing else to bind the frame to once the message is gone.
Two residuals are declared rather than hidden: a coarse TIMING difference between the transit arm
(which resolves a message and performs network I/O) and the uniform refusal, which is the same
residual `ACK-4` already declares for `GET /v1/ack/<key>`; and one transient body copy per call,
because the accessor projects over `ByOriginMessageID` rather than duplicating its subtle
`byOrigin`-then-local-id fallback.

### 3. Propagation is SYNCHRONOUS, with no durable queue and no retry

**Decision.** Each hop answers only after the next hop has answered. There is no spool, no queue and
no retry on this path.

**The alternative considered: a durable queue with retry** at each hop — accept locally, answer 200,
and deliver upstream in the background, which is exactly the shape the existing outbox has for
messages. Rejected because the outbox's shape solves a different problem: it exists because the
message must be delivered even if the peer is down for a day, and it pays for that with a record type,
a resume path and a retry horizon.

**Why synchronous is correct here.** Invariant 4 says nothing is acknowledged before it is durable,
and on this path the durable write happens at the ORIGIN, possibly several hops away. Answering only
after the next hop has answered makes **invariant 4 hold END TO END through the chain** rather than
through a local write — which is precisely what makes it correct for this path to add no durable state
of its own, and closes the loop with decision 1. A failure is answered **"not now"** (503 on both
surfaces, with `Retry-After: 1` on the agent surface): nothing was written on any bus, so the
identical retry is safe and is the correct remedy, and nothing is lost because nothing was
acknowledged. A 4xx is deliberately never used, because `PeerRefusedError.Retriable` treats every 4xx
but 408/429 as FINAL and the outcome would be **abandoned** rather than delayed.

**The cost.** A recipient's acknowledgement now has the latency of the whole chain and fails when any
bus on it is down. That is the honest exchange for a guarantee that holds end to end, and it is
bounded: nothing is lost, only delayed. **Retry, backoff and bounce remain `ACK-7`/`ACK-14`** and
belong there — once, beside the durable outbox that already survives a restart. A loop added here
would be a second retry policy with no durable record behind it: it would survive neither a restart
nor a crash and would race the real one the moment `ACK-7` lands.

### Two smaller calls, recorded because they look like bugs

- **A FINAL refusal from upstream is answered 200 downstream, not forwarded.** Re-offering a frame the
  origin has finally refused is the retry amplification §9.3 exists to stop; and forwarding the
  origin's 409 verbatim would turn the hop into an ORACLE, letting any bound peer ask whether the
  ORIGIN holds a row for a recipient it names — the uniform-answer property, leaked one hop back. The
  settlement really is dropped, so it is logged loudly and specifically (invariant 6) with the
  upstream status, which is what an operator needs to see it.
- **`duplicate` is always `false` on the transit path.** This bus keeps no record for a relayed
  message, so there is nothing here for a retry to be a duplicate OF; the duplicate is absorbed where
  the record is, at the origin, under §8.2 note 2. Labelling it otherwise would mean this bus
  asserting something about a table it does not hold. The consequence is documented rather than
  papered over: a recipient CAN infer the transit path from `duplicate:false` on a re-acknowledgement,
  which is a topology hint to a party that was handed the message, not a message-existence oracle.

**No disconnect is added anywhere** (invariant 10, §12). Both of invariant 10's questions were asked:
a merely BUGGY client reaches every refusal on these paths (a de-peered neighbour, an upstream bus
that is simply down, a message swept by retention), and a peer link carries an entire remote roster's
traffic rather than one principal's.

### Applied after review — the 200 arm is narrowed to 409, and invariant 4 is narrowed with it

The reviewer and security gates on `ACK-5` produced one behavioural correction, and it makes an
absolute stated five places in the new code slightly untrue. Both halves are recorded here.

**The correction.** The bullet above — "A FINAL refusal from upstream is answered 200 downstream" —
was implemented as `!refused.Retriable()`, and `PeerRefusedError.Retriable()` calls **every** 4xx
except 408/429 final. So **404, 403 and 400 all landed in the 200 arm**, and every one of them is an
upstream that decided *nothing about the frame*: `relay.Client.PeerAck`'s own rollout-ordering note
says an upstream running a pre-`ACK-5` binary does not serve the route and answers **404**; a **403**
is an ordinary de-peering; a **400** is `RELAY-51`'s live `DisallowUnknownFields` hazard. Traced end
to end, each produced `200 accepted:true` to the recipient with the outcome DROPPED and nothing
durable anywhere — §1.1's sentence, written by the code that exists to prevent it. The arm is now
`refused.StatusCode == http.StatusConflict` and nothing else; every other final status falls to the
existing 503. The earlier bullet is left standing rather than edited (this file is append-only) and
is **superseded by this paragraph**.

**Why 409 is still absorbed.** A 409 is the ONE final refusal meaning the upstream *understood the
frame and made a decision about it* — no obligation binds that recipient, or a conflicting terminal
already stands there. Re-offering it is the retry amplification §9.3 exists to stop, and forwarding
its verdict verbatim would tell any bound peer whether the ORIGIN holds a row for a recipient it
named: the uniform-answer property, leaked one hop back. A 404/403/400 is instead
OPERATOR-recoverable — upgrade the binary, re-peer the bus, fix the encoder — so "not now" is
truthful and a later identical offer can succeed. The anti-oracle property is unchanged either way:
the 503 is byte-identical to every other "not now" on that route, so a downstream peer still never
learns the upstream's verdict.

**THE NARROWING, stated plainly.** Five doc sites now assert, and asserted without qualification
before this change, that *nothing is answered `accepted` before the ORIGIN has fsynced the outcome*
— `cmd/agent-bus/ackback.go`'s header, `internal/hub/ack.go` (the `RecipientAckResult.Transit`
contract and the `AcknowledgeDelivery` check-order list), `internal/httpapi/server.go`
(`Options.AckTransit`) and `internal/httpapi/ack.go` (the `AckTransit` interface and `handleAck`
step 6). **That is true on every arm except one**, and all five now say so in place rather than
dropping the claim: an INTERMEDIATE bus absorbs a 409 and answers ITS downstream 200, so across two
or more backward hops a recipient can be told `accepted` for an outcome the origin refused with a
409 — and in the "no obligation binds that recipient" case nothing durable exists anywhere. No bus
does this to its OWN immediate caller: on the agent surface every `TransitAck` error, 409 included,
is a 503.

**The cost, accepted rather than argued away.** A recipient at the end of a multi-hop chain can be
told `accepted` for a settlement that was refused at the origin, and it has no way to tell that from
a recorded one — deliberately, since §13.3's posture is that a recipient learns the outcome of the
message it was handed and nothing about the federation. The alternative — forwarding the 409 — buys
the recipient a truthful answer at the price of a row-existence oracle for every bound peer, and is
the worse trade. It is bounded: it needs the origin to refuse a frame an intermediate was
legitimately bound to carry, which is the swept-row and never-addressed-recipient case, not the
steady state.

**AMENDED 2026-08-21 (`ACK-5` documentation pass); the sentence above is left standing beside this
correction.** "The swept-row and never-addressed-recipient case" **omits the third cause this entry
names two paragraphs earlier** — a **conflicting terminal already standing at the origin**. That one
is not exotic: it is what a recipient reaches by changing its mind about a message, and it is
permanent. Both of its observable shapes were traced through the code rather than assumed. With the
recipient's bus peered **directly** to the origin there is no intermediate to absorb anything, so
`handleAck`'s transit arm turns the 409 into a **503**, which the CLI reports as **exit 6** —
identically and for ever, with `Retry-After: 1` inviting a retry that can never clear. With an
**intermediate** in between, the recipient gets **exit 0, `accepted:true`, `duplicate:false`, and
`state` set to the outcome it has just asserted**, while the origin's FIRST outcome still stands. So
the bound is "not the steady state"; it is **not** "only where nothing was ever recorded anywhere".

**The documentation pass that found it.** `AGENT_PROTOCOL.md`'s exit-6 row called the identical retry
"safe by construction and … the correct remedy", named only transient causes, and set no bound on the
attempts — advice that becomes an endless loop on the 409 causes, which are permanent. It now says
retry with backoff, **bounded**, and states that a transient failure and a permanent refusal are
byte-identical to a recipient **by design**. The unqualified "not told `accepted` until the origin has it on disk" is narrowed in place
in `AGENT_PROTOCOL.md`, `PROTOCOL.md` §12, `CONTRACTS-HTTP.md`'s `POST /v1/ack` section and
`ACK-CONTRACT.md` §9.4.2 — the last of which the review did not name and which carried the same
unqualified claim. `cmd/agent-busctl/ack.go`'s help now separates the local case from the relayed one.
Two citations were corrected: `ACK-CONTRACT.md` cited `internal/relay/ackback.go:831` for
`AuthorizePeerAckVia`, which is a line inside its doc comment (the declaration is `:843`), and
`CONTRACTS-HTTP.md` cited `internal/httpapi/ack.go:459` for the "ONE arm that carries a Retry-After"
phrase, which is at `:501`. The same `:831` citation appears later in this entry, in the `ACK-12`
amendment, and is left as written because this file is append-only.

**One claim in those docs was REFUTED rather than reworded, and it is a real gap.** `UpstreamHop`'s
end-at-us rule was documented — in `internal/relay/ackback.go`, `cmd/agent-bus/ackback.go`,
`PROTOCOL.md` §12 and `ACK-CONTRACT.md` §9.4.2 — as stopping a peer from steering this bus's onward
contact. It does not. `hub.relayedBusPath` checks that the arriving path is non-empty, within
`store.MaxReceivedBusPath`, that every hop is a valid bus id and that this bus is not already on it,
then appends this bus; `hub.RelayedIngestRequest` has **no peer-principal field at all**, so nothing
binds `received[len-1]` to the authenticated peer and an authenticated peer can still name a
different bus it knows we peer with as our upstream. What the rule DOES buy is narrower and still
worth having: we contact only the hop adjacent to a position **we** wrote, and only if it is in our
own peer registry with a base URL — so a fabricated prefix can never name an address, a host or a
scheme, and an unpeered id means nothing is dialled. The residual is bounded at the far end by §6.2's
obligation binding. Tracked as **`ACK-5-FU-BUSPATH-SENDER`**, which is now named at each corrected
site; `cmd/agent-bus/ackback.go` still carries the refuted wording and is owned by another agent in
the same change.

**Two smaller corrections applied at the same time.** The backward hop is now bounded by
`ackTransitTimeout = relay.DefaultForwardTimeout` (referenced, not copied, so it cannot drift from
the forwarder's own per-attempt bound), derived from the INBOUND context so a caller that goes away
still cancels the outbound hop. It had no deadline at all: the pinned peer HTTP client carries no
`Timeout`, `ForwarderOptions.Timeout` does not cover this path, the server leaves
`ReadTimeout`/`WriteTimeout` unset for the long-poll, and `peerDialTimeout` bounds only connect and
handshake — while a stalled call holds one of eight per-peer in-flight slots **shared with relay
message ingest**, so an unbounded ACK hop is a denial of service against a neighbour's MESSAGE
delivery, not merely against its acknowledgements. And `federationOptions.AckTransit`'s doc no
longer calls a nil one "a LEGITIMATE configuration": `main.go` builds the back-propagator on the
`peerStore != nil` branch with every failure fatal, and `newFederation` is reached only from
`bindable > 0`, which implies that branch — so the nil arm is a fail-closed default reachable only
from a test or a future composition, kept because the alternative is a nil dereference on a
peer-facing path.

### Applied after the `ACK-12` harness — §6.2's obligation binding is WIDENED by ONE case

The three-bus acceptance harness proved the ACK plane still failed at the LAST hop back, and the
cause was in `ACK-CONTRACT.md` §6.2 itself rather than only in the code. **The contract is amended in
place and marked; this is the decision behind that amendment.**

**The defect, measured rather than hypothesised.** On A→B→C with a recipient on C and a sender on A,
A writes its outbox obligation as `DeriveJobID(C, K)` — `Forwarder.targets` keys the job on
`Registry.Route(recipient)`, which returns the recipient's HOME bus, not the next hop A dials
(`internal/relay/forward.go:1044`). But the acknowledgement comes back over A's mutual-TLS link with
**B**, so `AuthorizePeerAck` looked up `DeriveJobID(B, K)` — a job id nothing ever wrote. **The two
spellings coincide ONLY on a direct peer link**, which is why every single-hop test was green while A
answered 409, B's 409-absorbing arm turned that into a 200 for C, and the recipient read "accepted"
for an outcome nothing recorded anywhere — §1.1's sentence, reached on the happy path.
`cmd/agent-bus/peer.go`'s `-route-for` documentation predicts it in words: the identity on the wire is
the NEXT HOP's, the record's bus id is the DESTINATION, "they are not the same field".

**Decision: widen the BINDING RULE, additively, by one case** (`relay.AuthorizePeerAckVia`,
`internal/relay/ackback.go:831`). A frame from authenticated peer `P` for key `K` naming recipient `R`
binds if EITHER the DIRECT arm holds (unchanged, tried first: `DeriveJobID(P, K)` names a job we
wrote) OR an INDIRECT arm does: `D` is the bus half of `R` (invariant 2), `D` is neither `P`
(case-folded) nor us, **the address this bus would dial for `D` equals the address it would dial for
`P`** — both resolved, both non-empty — and `DeriveJobID(D, K)` names a job we wrote whose peer bus id
and origin message id are the two asked for.

**The third clause is the security core, and it is computed from THIS BUS's own peer configuration,
never from the frame.** `R` is peer-supplied, so it selects WHICH job we look for; it cannot conjure
one, and it cannot make an unrelated peer the next hop for a destination we route elsewhere. On a
`-route-for` topology a route record's base URL *is* the address of the next hop for that destination,
so `PeerBaseURL(D) == PeerBaseURL(P)` is exactly "is `P` the hop we route `D` through", asked of the
only party entitled to answer it: us.

**The alternative considered — re-key the outbox job on the NEXT HOP instead** (make
`Forwarder.targets` derive the job id from the peer it actually dials, so one spelling serves both
sides). Rejected, and not on taste:

- **The job id is a DURABLE id.** Changing its shape orphans every pending job across an upgrade —
  the records on disk derive and re-verify it (`OutboxRecord.validate`), so old jobs would neither
  match nor settle, on the one path whose whole purpose is not losing deliveries.
- **It moves split-horizon and recovery logic**, which live in `internal/relay/outbox.go` and
  `forward.go` — a far larger change, in a file another task owns, reached by a change whose entire
  purpose is the ACK plane.
- The chosen widening **adds no state, no record type and no wire field**, and is computed from state
  this bus already holds: its own routing table and its own outbox.

**What it costs and what it does not buy.** Two registry `RLock` lookups per otherwise-unbound frame,
and — only if they agree — a second `Outbox.Lookup` (exclusive mutex, O(n) sweep). The routing checks
run FIRST deliberately, so a peer can provoke that second sweep only for a destination this bus
already routes through it (§16 Q3). **Every indirect refusal returns the same `ErrAckNotBound`, by
identity**, so the uniform 409 acquires no new distinguishable case: the widening adds a way to be
BOUND, never a new way to be told why you were not. A nil routing resolver fails closed to the direct
arm's answer, byte-for-byte the pre-`ACK-5` behaviour.

**It does NOT close `ACK-4-FU-RECIPIENT-BINDING`, and the docs say so in those words.** The indirect
arm does bind the recipient's home bus to the acknowledging peer, which is adjacent to that task and
easy to mistake for it — but **the DIRECT arm still binds only (peer, key)**, so a peer legitimately
bound for `K` can still settle any recipient of `K` on a direct link, and the direct arm is the one
every single-hop delivery takes. That task stays open.

### And: a transit correlation key must name ANOTHER bus

`hub.transitAck` now refuses, before any lookup, a correlation key whose bus half is THIS bus
(`internal/hub/ack.go:728-730`, folded comparison). Without it the LOCAL id this bus minted resolves —
through `store.ByOriginMessageID`'s documented local-id fallback — to the same relayed message, the
principal is a member, and the route answered a **retriable 503 that no client could ever clear**,
because `relay.DisposeAck` then says `AckStopAtOrigin` for a key whose bus half is ours. The answer is
the uniform `unknown` instead. **It is reached by doing the obvious thing:** `agent-busctl watch`
prints the LOCAL `message_id` (`toWireMessage`, `internal/httpapi/messages.go:844`) and no route
exposes the origin id, so the id a recipient holds is exactly the one refused here — a real usability
gap, documented as such in `AGENT_PROTOCOL.md` and tracked separately (`f423959c`,
`ACK-12-FU-WATCH-CORRELATION-KEY`). Nothing here narrows §3: the correlation key was always the
ORIGIN's server-minted id, and this is that rule enforced at the boundary that was accepting a second
spelling.

## Removed on 2026-08-16 — superseded, refuted or overtaken

`DECISIONS.md` is described elsewhere in this repo as append-only. **That convention was SUSPENDED
for this pass by operator instruction, not silently contradicted.** The instruction, 2026-08-16,
verbatim: *"I want it to reflect the current state. Irelevant, refuted, changed, etc decisions should
be removed."* It is recorded in the commit message and in `AGENT_LOG.md` as well as here — cite those
rather than this sentence, because a document asserting its own authority is not evidence of it. The
condition attached was that nothing vanish without trace. Each line
below is one decision (or one named section of one) that was REMOVED from the live document above,
with its original date and title and the reason it went. The reasons were VERIFIED against the code
on 2026-08-16, not inferred from age — where a decision could not be verified either way it was KEPT
and marked `> UNVERIFIED 2026-08-16` in place instead.

If these one-liners are also unwanted, deleting this section is a second, trivial edit. Deleting the
entries without it would not have been.

- **2026-08-02 — The message sequence high-water mark lives in the WAL message body, read via a replay-time PREPARE observer (ID-2-WIRING-SCHEMA)** — DEAD. The mechanism it chose was never built: there is no `ReplayWithPrepares` and no prepare observer anywhere in `internal/wal` (only `wal.Replay(path, fn)`, `internal/wal/replay.go:109`). The high-water mark now comes from the dedicated `<data-dir>/message-seq-floor` file plus three log-derived sources maximised in `internal/hub/hub.go:608-746` — see *2026-08-07 — CORRECTION: the WAL record-index floor does NOT subsume the message-sequence floor*. The one surviving fact, that a `"message"` WAL body carries a top-level `"seq"`, is still true (`internal/store/message.go:448`) and lives in `CONTRACTS-ONDISK.md`.

- **2026-08-02 — Addendum to ID-2-WIRING-SCHEMA: the quarantine residual is a DEFECT, not a narrowing** — SUPERSEDED. Its operative requirement (refuse to start when the floor cannot be proven after a quarantine) was rejected by *2026-08-07 — SUPERSEDES two earlier passages* and by invariant 6; its surviving half (reuse is a DEFECT, not a narrowing) is restated there and in *2026-08-07 — The WAL record-index high-water mark is a dedicated write-ahead file*. The entry it addended is also removed above.

- **2026-08-07 — SIGN-2/SIGN-6: the signing core lands; the mandatory-signature policy is BLOCKED** — SUPERSEDED BY NAME by *2026-08-07 — SIGN-2/SIGN-6 SHIPPED: the mandatory-signature policy is UNBLOCKED*, which re-affirms its decisions 1–3 verbatim and retires its decision 4 (the three blockers) in full. Left in place it read as "a signature is optional on this bus", which is false: a signature is mandatory on `POST /v1/send` (`internal/httpapi/messages.go:564-662`).

- **2026-08-02 — The client is `client/` + `cmd/busctl`, §3 `--invite` is REJECTED locally, not guessed onto the wire** — REFUTED by what shipped. `ENROL-SHAPE` settled the wire shape and `INVITE-CLIENT` landed: `client/enrol.go:256-337` now SENDS the invite id and secret on the wire. The seam (`client.EnrolOptions.Invite`) is used as this decision intended; only the local refusal is gone. The other five decisions in that entry are untouched.

- **2026-08-02 — MSG/POLL, §4 Message ids may repeat after a WAL QUARANTINE, and after damage deeper than a torn tail** — SUPERSEDED BY NAME, twice: by *2026-08-07 — SUPERSEDES two earlier passages* and again by *2026-08-07 — The WAL record-index high-water mark is a dedicated write-ahead file*, which records that invariant 1 is UNNARROWED and that the nine test assertions encoding the old accepted-reissue behaviour were INVERTED. Keeping it left a narrowing of invariant 1 on the page that the code and the tests both contradict.

- **2026-08-07 — MSG-FU-SUFFIXFLOOR (wiring), §4 The scan runs on EVERY start** — SUPERSEDED BY NAME the same day by *2026-08-07 — ADDENDUM to MSG-FU-SUFFIXFLOOR (wiring)*, whose own words are "Correction to §4" and "Where the two disagree, this one governs". The code agrees with the ADDENDUM: the scan is gated on `!alloc.Existed()` (`cmd/agent-bus/suffixfloors.go:172,179-218`), i.e. at most once per data directory. The RAISE-don't-just-report rule survives and is restated in the ADDENDUM.

- **2026-08-07 — The listener is TLS …, §3 `ClientAuth: tls.NoClientCert`** — REFUTED by the code and superseded by *2026-08-14 — MTLS-CLIENTAUTH*. The listener sets `ClientAuth: tls.RequestClientCert` with an `admitClientCertificate` callback (`cmd/agent-bus/tlslisten.go:152,173,309`). The PRINCIPLE the section was written to defend — server-side enforcement does not precede client-side capability — is unchanged and is argued at length in the MTLS-CLIENTAUTH entry.

- **2026-08-14 — INVITE-GATE …, §(c) Why this ships WITHOUT flipping the gate** — REVERSED. The gate was flipped on 2026-08-15 in `3cedcb7`: `enrolmentInviteRequired = true` (`cmd/agent-bus/main.go:66`), and an un-invited `POST /v1/enroll` is refused 403. Both blockers it named were cleared — the CLI now sends an invite (`client/enrol.go:256-337`) and the live agents were re-invited. Sections (a), (b), (d) and (e) of that entry stand.

- **2026-08-15 — RELAY-24-BLOCKER-EGRESS: the egress seams …, §BLOCKED, and escalated rather than decided: the OUTBOUND peer TLS pin** — RESOLVED, and named as superseded by *2026-08-15 — RELAY-24-BLOCKER-EGRESS: the outbound-TLS blocker, resolved*, which took candidate resolution 1 (export `client.PinnedTLSConfig`). Its closing claims are all false now: the forwarder has production callers, the registry is built at the composition root, peers are seeded, and the startup line no longer reports the forwarder unwired. The refusal to add a second `InsecureSkipVerify` literal held — there is still exactly one, in `client/pin.go:260-261`.

- **2026-08-15 — RELAY-24-BLOCKER-EGRESS: the egress seams …, the `RemoteRouter` paragraph under Decision 1** — SUPERSEDED BY NAME the same day by *… the outbound-TLS blocker, resolved*. It said `/v1/send` to a peer's agent "still answers `404 unknown recipient`"; the router is wired (`cmd/agent-bus/main.go:1150`) and such a send is accepted **201**. It already carried an inline supersession banner; both are removed so there is one live answer to that question.

## 2026-08-21 — ACK-12: a Go acceptance harness at `tests/e2e/`, and why it asserts a BROKEN plane

Two decisions, both taken deliberately and both easy to mistake for mistakes later.

### 1. `ACK-CONTRACT.md` §15 says "do not build a parallel harness". This task built one.

§15 directs ACK-12 to reuse DEPLOY-3's Compose topology and `scripts/fed-smoke.sh`. **The premise
is false as of today, and was checked rather than assumed:**

- **DEPLOY-3 is `todo`.** The multi-bus Compose profile it would supply does not exist.
- **`docker-compose.yml` defines exactly ONE service.** There is no three-bus Compose topology to
  reuse.
- **The stored `proof_cmd` mandates a Go test:**
  `bash scripts/proof-check.sh 'go test -race -run ^TestThreeBusEndToEndAckNack$ ./tests/e2e'`.
  A shell script cannot satisfy it, and `tests/` did not exist.

So §15's instruction could not be followed as written. What §15 is actually protecting against —
a second, divergent bring-up recipe — is honoured a different way: the harness reuses the
SANCTIONED building blocks rather than reimplementing them. Server lifecycle goes through
`scripts/bus-serve.sh` and nothing else; every bus and client capability goes through a compiled
command (invariant 7); and the peering recipe is lifted in shape from `scripts/fed-smoke.sh:620-692`,
including the outbound `-tls-fingerprint` / inbound `-peer-client-fingerprint` split, which are
opposite directions and must never be collapsed.

**It is deliberately ADDITIVE, not a superset.** It does NOT re-assert what fed-smoke already
proves — send idempotency, exact `bus_path` equality beyond what the readiness gate needs. It
covers the ACK plane only. If DEPLOY-3 later lands a real Compose topology, this harness should be
re-pointed at it rather than duplicated again.

### 2. Two subtests assert that the product is BROKEN. That is the deliverable, not a defect.

`relayed_message_cannot_yet_be_acked_on_the_receiving_bus` and
`ack_does_not_yet_propagate_to_origin_bus` assert measured current behaviour: `state:"unknown"` /
exit 8 on the destination bus, and a sender row stuck at exactly `accepted`.

The alternative — asserting the ideal and leaving the test red, or skipping it — was rejected. A
red test is noise that gets muted; a skipped one scores VACUOUS under `proof-check.sh` and asserts
nothing. **An assertion pinned to today's exact values goes RED the moment the product moves**,
which is what forces the next agent to look.

**They must be INVERTED when ACK-5 lands, never deleted and never loosened.** That instruction is
written inside the `t.Fatalf` MESSAGE rather than in a header comment, because the failure message
is the only text guaranteed to be read by the agent who makes it go red.

**The risk being accepted, stated plainly:** a test asserting a bug entrenches it if nobody reads
the comment. Mitigated by two P0 follow-ups naming the exact lines — `7d564118`
(ACK-12-FU-DESTINATION-ROW) and `f423959c` (ACK-12-FU-WATCH-CORRELATION-KEY) — and by the fact that
ACK-5 is already `in_progress`, so the inversion will be exercised within days rather than years.

### The readiness gate is the observed relay, never `/healthz`

Recorded because it is the third time this has bitten the project. A bus reports healthy on
`/healthz` while every `/v1/peer/` path 404s, because `mountPeerSurface` is all-or-nothing and
`main.go` supplies a NIL PAIR when no peer has an inbound client-certificate binding. RELAY-51's
rollout gate passed on a completely deaf bus for exactly this reason. **This harness gates on an
actual A->B->C delivery being observed**, and that gate was proved fireable by sabotage — removing
`-peer-client-fingerprint` makes it fail with `DELIVERY GATE FAILED`, not pass quietly. When
RELAY-55 lands an authenticated `/v1/readyz`, this is a candidate to re-point.

## 2026-08-21 — ACK-13: `internal/ack` is the SINGLE home of the closed ACK vocabulary

**Decision.** The closed ACK vocabulary — the twelve NACK classes, the two attestation labels and
the five lifecycle states — is declared exactly once, in `internal/ack`. `internal/relay` declares
no vocabulary value at all: `AckClass`, `AckOutcome` and `AckAttestation` are true Go **type
aliases** (`=`) for `ack.Class`, `ack.State` and `ack.Attestation`, and all seventeen relay
constants are bound to `ack`'s by identity.

**Why `internal/ack` and not `internal/relay`.** Three reasons, in order of weight:

1. `internal/ack` owns the DURABLE spellings — the bytes that reach disk. Pinning the vocabulary
   where it is persisted is the stronger pin.
2. `internal/signing/ackvocab_external_test.go` already pins its FROZEN wire alphabet against
   `internal/ack`, and its own comment anticipated this task ("when ACK-13 collapses the two
   declarations into one, this guard follows the survivor").
3. `internal/relay` sits under `TestRelayImportedOnlyByWiringSites`, which restricts its IMPORTERS
   to `internal/httpapi` and `cmd/agent-bus` (RELAY-6 ruling (c), 2026-08-08). That guard does not
   restrict what relay itself imports, and `internal/ack` imports only `idem`, `ids`, `logging` and
   `wal`, so `relay -> ack` is acyclic. The reverse direction would have forced a third entry into
   `wiringSites`, which is an architectural change a de-dup has no business making.

**What was deliberately NOT collapsed.** Two further copies of these spellings survive, both
justified:

- `client/ack.go` keeps its own string constants because `client/` cannot import `internal/`
  (invariant 7 — an importable client package is the whole point of "embed").
- `internal/signing` keeps its FROZEN alphabet. Deferring to `ack.Class` would mean that renaming a
  constant there silently changes the SIGNED BYTES and invalidates every signature ever made, with
  nothing going red because signer and verifier would follow the rename together. The drift guard
  is the seam instead.

**Consequence that had to be designed, not assumed: `String()` must not echo.** Moving from a
bounded `uint8` to a string type turns un-echoable values into echoable ones — HEAD's
`AckClass(200)` could not reproduce attacker bytes, whereas a naive `string(c)` would put
peer-chosen bytes into operator logs and error text. `Class.String()` and `Attestation.String()`
therefore render a NON-MEMBER as `invalid-class(N bytes)` / `invalid-attestation(N bytes)` and echo
nothing. This is what makes the uint8->string move safe under invariant 6, and it leaves invariant 6
marginally STRONGER than before the change.

**Consequence two: `!outcome.Terminal()` replaces the numeric bound.** The alias makes `accepted`
and `in_flight` REPRESENTABLE as an `AckOutcome` for the first time — a three-member uint8 enum
excluded them structurally. `!outcome.Terminal()` is at least as strict (it admits the same three
and additionally refuses the two non-terminals) and it is now the only thing keeping a non-terminal
state off a wire frame and out of an absorbing durable terminal.

**And the defect that consequence caused, found by three gates independently.**
`internal/httpapi/ack.go`'s `ackRecordVocabulary` had inherited its terminality guarantee from the
old uint8's inability to spell a non-terminal; after the alias, `accepted` and `in_flight` parse
cleanly and it stopped refusing them — while its own doc comment went on claiming it did. Not
exploitable (`handleAck` feeds it terminal-only output), but it was unreachability by AGREEMENT
BETWEEN TWO PACKAGES rather than by construction. Fixed in the same task with an explicit
`!state.Terminal()` check mirroring its twin in `cmd/agent-bus/relaywiring.go`, plus the first
direct test that function has ever had.

**Rejected: changing anything about the vocabulary while moving it.** The task forbade it and the
prohibition earned its keep — `internal/ack/state.go` has a ZERO-BYTE diff and all nineteen wire
literals are byte-identical to HEAD, which is a structural proof rather than a reviewer's opinion.
A de-dup that also edited values would have made the diff unreviewable and the mutation proofs
unrepeatable.

## 2026-08-21 — ACK-7: a terminal negative does NOT cancel outstanding hops (ACK-CONTRACT.md §16 Q2)

**The question, assigned to ACK-7 by name.** §16 Q2 asks whether a terminal negative ACK should
cancel outstanding hops for that recipient — stop retrying a message the recipient has already
refused. The contract set a default of "do not cancel" and deferred the ruling here, with ACK-4
reviewing the denial-of-service angle.

**Ruling: DO NOT CANCEL. The default becomes the decision.**

**1. Nobody can verify a recipient attestation, so a cancel would act on an unverifiable claim.**
§16 Q1 is still open: no endpoint distributes agents' messaging public keys, so a recipient NACK is
attributable to a BUS and is end-to-end unverifiable by anybody — which is why `Attestation` has no
value meaning "verified". Cancelling on that claim converts an unverifiable assertion into
irreversible work-cancellation.

**2. That makes cancellation a denial-of-delivery vector — PROSPECTIVELY, and the tense matters.**
The obligation binding rule (§6.2) constrains WHO may speak about a correlation key, but in an
A->B->C chain B is legitimately bound to the key: `AuthorizePeerAck` binds **(peer, correlation key)
only** and never binds the RECIPIENT, and §6.2 deliberately declines a "bus half must equal the
acking peer" rule because that would break multi-hop. If a NACK cancelled outstanding hops, B could
suppress A's OTHER routes to C by refusing its own copy. The binding rule was never designed to
carry that weight; it answers "may this peer speak about this key", not "may this peer decide the
fate of every other route".

> **PRECONDITION, stated so a future reader does not reverse this ruling after failing to reproduce
> the attack.** As of 2026-08-21 the suppression is NOT reachable at HEAD, and the security gate
> confirmed it: directed routing picks exactly ONE next hop per recipient (`f.reg.Route`, deduped,
> `internal/relay/forward.go`), `Hub.recordAcceptance`'s recipient loop "runs exactly once" today,
> and broadcast answers 501. One job and one row per key means a cancel could only ever reach the
> acking peer's OWN job. **The attack goes live the moment multi-recipient send or broadcast lands**,
> because a peer bound to K by one recipient's job can then name a DIFFERENT recipient routed via a
> DIFFERENT peer. This is therefore a FORWARD-LOOKING rule adopted before the hazard exists, which is
> the cheap moment to adopt it — not a description of a live hole. Do not read the absence of a
> reproduction as evidence against it.

**3. A NACK from one route does not prove the other route's copy is unwanted.** Distinct hops may
reach distinct recipient instances, and §3.2 already keys the sender-visible row per
(key, recipient) precisely because outcomes are per-recipient facts.

**4. The saving is small, and smallest exactly when it is claimed to matter.** A hop that has been
forwarded is already settled `OutboxDelivered` at forward time (`internal/relay/forward.go`), so the
only jobs a cancel could reach are ones no peer has accepted — a dead peer, which is not consuming
meaningful work. Cancelling buys little and spends a security property.

**5. The sender loses nothing by not cancelling.** The sender-visible state reaches `refused`
immediately on the terminal NACK; cancellation would change only hop-level work, never what the
sender learns. Terminal is absorbing either way.

**What is given up, stated plainly.** A message the recipient refused on one route may still be
delivered on another, so a recipient can receive a copy it has already refused. That is at-least-once
delivery behaving as designed; the answer is recipient-side idempotency (invariant 10), not
cancellation.

**Status quo confirmed rather than assumed.** No cancellation mechanism exists today: there is no
`Cancel`/`dropJob` path anywhere in `internal/relay`, `internal/hub` or `cmd/agent-bus`; the only
production `Outbox.Settle` caller is the forwarder settling its own job (`internal/relay/forward.go`);
and `federation.settleAck`'s BODY touches only `f.log`, `f.acks` and `f.busID`. **That last clause is
deliberately about the METHOD, not the type**: `federation` does reach a forwarder one field-hop away
(`onward.next`), so this is a statement about what the settle path does, not a structural
impossibility. The pin against regression is the test, not the object graph. This decision RECORDS
that posture and pins it so it cannot drift in silently.

**Revisit condition.** If §16 Q1 is ever answered — a key-distribution endpoint that lets a SENDER
verify a recipient NACK end-to-end — cancellation becomes safe to reconsider, because the cancel
would then be authorized by the recipient itself rather than by a bus asserting on its behalf. Until
then this is not a close call.

## 2026-08-21 — ACK-7: a concurrent byte-identical retry is answered 503, and that is invariant 4 winning

**The apparent contradiction.** `ack.Store.Settle` reserves the `(correlation key, recipient)` pair
ACROSS the fsync (`Store.inflight`), so a second transition for the same pair that arrives while the
first is being made durable gets `ErrConcurrentTransition`, which `settleAck` lets fall through to
**503 `CodeUnavailable`, nothing written**. A byte-identical retry therefore receives an ERROR — and
invariant 10's first case says a legitimate retry must "return the ORIGINAL result, do not re-apply,
do not error".

**Ruling: 503 is CORRECT and must not be "fixed" to `200 {duplicate:true}`.**

When the loser is refused, the winner's fsync **may or may not have completed** — and the decisive
point is that **the reservation is one bit and cannot tell the two apart**. It is taken before
`durable.Write` and released after `foldIn`, so a refusal covers both the pre-fsync window (nothing
is durable yet) and the sliver after `Write` returns (it is). Answering `duplicate:true` would
therefore assert durability in a case where no one has confirmed it — a direct breach of invariant 4,
which is the stronger guarantee and the one the whole system is built on. Refusing is the only answer
that is safe in BOTH windows.

> An earlier draft of this entry said flatly that "the winner's fsync has not completed". That is
> false for the post-`Write` sliver. The conclusion is unchanged, but the reasoning was stated more
> confidently than it was true, which is the failure this file exists to avoid. Invariant 10's "do not error" is about not PUNISHING a retry; it is not a licence to
acknowledge before durability. A 503 is retriable, carries no penalty, drops no connection, and the
retry gets the correct absorbed answer once the write lands.

**This is recorded because the wrong fix looks like an improvement.** "A duplicate should not get a
503" is a natural code-review comment, the change is two lines, and every positive test would still
pass — while the bus would have begun acknowledging writes before they were durable. That is the
failure mode invariant 4 exists to prevent and it would be invisible until a crash.

**The distinction that must never be collapsed**, and is now pinned by
`TestAckTerminalExactlyOnceUnderRetry`:

| Sentinel | Meaning | Status | Retriable |
| --- | --- | --- | --- |
| `ack.ErrConcurrentTransition` | transient race; nothing written | **503** | yes |
| `ack.ErrTerminal` -> `relay.ErrAckOutcomeConflict` | permanent; a DIFFERENT terminal is recorded | **409** | no |

Conflating them in either direction is a defect. Reporting a race as a **conflict** tells an honest
retrying peer it committed a protocol violation; reporting a conflict as a **race** invites it to
retry a frame that can never be accepted. Neither disconnects (§12).

**Exactly-once is enforced in ONE place and it is not the reporting path.** `Settle`'s reservation is
the mechanism; `settleAck`'s advisory read exists only to label `duplicate` and is explicitly allowed
to lose a race. Mutation-proved: deleting the reservation's busy check produces **12 durable terminal
records for one pair** under 12 identical concurrent frames. The in-memory table is idempotent, so
the WAL write count is the ONLY place that second write is observable — which is why the test counts
writes rather than inspecting the table.

## 2026-08-21 — CONTEXT-DOCCHECK: an ambiguous heading FAILS, and re-entry is argv-only

Two decisions inside `scripts/doc-check.sh` that change what a proof can assert, recorded because
both look like over-strictness until you know what they replaced.

### A repeated heading is a hard failure, with no fallback to "the first one"

`section <file> <heading> <needle>` used to bind to the first heading that matched. On live docs that
produced BOTH errors at once: a needle from `AGENT_PROTOCOL.md`'s canonical `## Exit codes` table
FAILED because the range stopped at an earlier `### Exit codes`, and a needle from that earlier
section PASSED as if the canonical one had been checked. Either way the proof asserts something the
document does not say.

The decision: **count every match and fail when there is more than one**, naming the count and
quoting each spelling verbatim. Where the matches differ in level, the caller can pin one by passing
the full heading line; where they are spelled identically **no argument can pin one**, and the tool
says so rather than suggesting a remedy that cannot work. The rejected alternative was an occurrence
index (`heading#2`): it would let a proof keep passing while the document silently reordered, which
is the same class of defect one level down.

The cost is accepted deliberately: measured on 2026-08-21, 9 heading keys across `AGENT_PROTOCOL.md`,
`CONTRACTS-HTTP.md`, `DECISIONS.md` and `AGENT_LOG.md` are duplicated at the SAME level and are now
unassertable until the document makes them unique. `AGENT_LOG.md` repeats `### Proof` and
`### Verification` per entry by convention — that convention is fine, and it simply means per-entry
sections are not section-proof targets.

**No list of duplicates is kept in the script.** The one that was there had gone stale within five
days and was wrong in both directions — it named four headings that were still singular on the date
it claimed and omitted three that were already duplicated. A hand-maintained list of a moving
property is a stale claim with a fuse; what is recorded instead is the `awk` one-liner that measures
it on demand.

### The environment gets no say in what the selftest runs

Re-entry (the selftest re-invoking itself to test its own temp-dir handling) was keyed on an exported
`DOC_CHECK_SELFTEST_INNER`. Inheriting that variable dropped seven assertions — precisely the ones
proving the command-injection and `mktemp` fixes — while both runs printed an identical
`proof-check: verdict=PASS class=wrapper exit=0`, because `proof-check.sh` judges a wrapper proof by
exit status alone.

The decision: **an internal flag is a private argv token, never an environment variable**, and the
selftest carries a probe that spawns a child with the old names exported and asserts the child's
assertion COUNT is still the larger one. Comparing counts rather than exit status is the point — the
broken version exited 0 too.

The general rule this is an instance of: **a proof instrument must not have an off switch that the
ambient environment can reach.** If a knob has to exist, it belongs in argv where it is visible in
the command the proof records.

### A measurement that did not happen is never a pass

Two 2026-08-21 security findings closed the same gap from opposite ends, and the rule they leave
behind is worth stating once: **the tool must distinguish "measured and fine" from "could not
measure".** `sed` reading stdin because the file name looked like an option, and `[ "$actual" -gt
"$max" ]` returning 2 because `wc` produced nothing, both ended in a confident PASS about something
that was never examined. So `<file>` is passed after `--`, `wc -c` output goes through the same
`is_uint` gate as `max_bytes`, and a `.tsv` row that cannot be measured is a FAIL naming the file
rather than a silently uncounted row.

That also settles a deferral. Deleting a row from `docs/doc-budgets.tsv` still turns the check green
and remains `CONTEXT-BUDGET-WIRE`'s to close, and the argument for deferring it is that the deletion
is visible in the diff of a tracked three-row file. The unmeasured-size defect reached the same
outcome — the failing row stops being measured — **with no diff at all**, which is why it was fixed
here rather than deferred with it.

### The fact that forces both

`proof-check.sh` classifies a shell proof as `class=wrapper` and takes its exit status as the whole
check. Anything that can make this script exit 0 while asserting less is therefore invisible at the
gate, and 16 stored `proof_cmd`s invoke it — measured, not recalled: 16 tasks in a 783-task
enumeration on 2026-08-21, every one in the CONTEXT epic and every one still `todo`. (An earlier
draft said 29, which was never reachable: that epic holds 30 tasks in total.) That is why
"absence is never a pass" now also covers a
`.tsv` with no data rows, why the selftest's failure path is a literal `exit 1` that bypasses the
dispatcher, and why every guard added here was mutation-proved RED before being trusted.

## 2026-08-21 — DOC-REFACTOR: `PITFALLS.md` is a third companion tier, and `CLAUDE.md` gets the one-line rule only

Spec task `f4bd3c9f-3af8-4438-bcb0-18203b857255` (PROCESS, P2). Full audit and evidence in
`DOC-REFACTOR_DEEPDIVE.md`.

**The problem.** `CLAUDE.md` measured 31023 B in the WORKING TREE against the 28781 B ratchet in
`docs/doc-budgets.tsv` (`doc-check.sh budget` exit 1, 2242 B over). The COMMITTED file was 30063 B at
`85ed77f`, `b95d22d` and `2ed05c2`, i.e. over by 1282 B; the 960 B difference is a `## How to write`
section that was in the worktree and in no commit. Both readings are over the ceiling. Stated
explicitly because an earlier draft of the deep-dive treated the worktree figure as a commit figure
and wrongly called two other files stale on the strength of it. Measured cause: 8267 B — 27% of the file — was incident
NARRATIVE (dates, shas, exact output) for process and verification traps, and there was nowhere else
for it to go. `INVARIANTS.md` already carried the correct pattern for DESIGN reasoning and states it
at `INVARIANTS.md:6-8`, but it covers only the eleven invariants. Nothing covered traps, so every
newly learned trap was appended in full to the one file injected into every sub-agent spawn.

**Decision 1 — a third tier, not a bigger ceiling.** `CLAUDE.md` = rules, injected per spawn.
`INVARIANTS.md` = design reasoning. `PITFALLS.md` (new) = process and verification incidents.
Each rule in `CLAUDE.md` keeps its one-line form and points at exactly one companion section.
This extends the split `INVARIANTS.md` established rather than inventing a new arrangement.

**Decision 2 — a written destination for the NEXT warning.** `CLAUDE.md`'s "How to write" section
now states where a new trap goes: one-line rule in `CLAUDE.md`, incident in `PITFALLS.md`, design
reasoning in `INVARIANTS.md`, and never delete a warning to make room. Without this the refactor
buys a fixed number of bytes and the growth resumes. `CLAUDE.md` is 28213 B (27253 B without that foreign
section), 568 B under the ceiling; the ratchet going red on the next careless append is the intended behaviour, because the
correct response is now defined.

**Nothing was deleted.** A relocation audit checked 50 load-bearing phrases plus one negative
control across the three files with whitespace normalisation: 50/50 present, control correctly
absent. The `docs/doc-preserve.tsv` count is unchanged at 5 and all five phrases remain in
`CLAUDE.md` — the relocation was designed around keeping them there. `docs/doc-preserve.tsv` was
not edited.

**Decision 3 — `AGENTS.md` gets a CONTENT sync only; the MECHANISM is not decided here.** Task
`6a5ece85` owns that decision and is still `todo`/unowned, and `f4bd3c9f`'s own description forbids
deciding it twice. `AGENTS.md` is now byte-identical to `CLAUDE.md`. Two findings handed to
`6a5ece85`: (a) the drift was not only staleness — a hand `Claude`→`Codex` substitution had renamed
real paths, producing `.Codex/agents/` (does not exist) and
`/mnt/sdc/mike/Codex-scratch/spec-cloud-creds.env` (no such file), so any mechanism preserving a
substitution table must forbid substituting filesystem paths, directory names and model ids;
(b) `6a5ece85`'s stored `proof_cmd` (`diff … | grep -qx 0`) now PASSES because of this task's copy,
so it measures the symptom rather than the mechanism and must be strengthened before that task is
closed.

**A correction found while checking (b), unrelated to sizing.** Both `CLAUDE.md` and `AGENTS.md`
named the Spec Server creds file as `spec-cloud-creds.env`. `scripts/spec-cloud.sh:20` actually
defaults to `spec-cloud-creds-agent-bus.env`. Both files exist on disk, so the wrong one is readable
and stale rather than absent, which is why the error survived. `CLAUDE.md` now cites
`scripts/spec-cloud.sh:20` as the authority instead of copying a path — the same rule invariant 3
already applies to the unauthenticated allow-list: cite the enumeration, never a copy of it.

**Related invariant-3 note.** `CLAUDE.md`'s invariant 3 said "the **six** on the explicit
allow-list" above a list that reads as FIVE bullets, because `session begin/complete` is two routes
written as one phrase. That is the most likely mechanical reason the count was written as five three
separate times (`401f112`, `2828dcf`, `b95d22d`; `DOC-TRUTH_DEEPDIVE.md` row 25). The numeral is
removed; the enumeration is unchanged. Flagged for operator ratification because open task
`9a02d65a` reserves that wording to the operator.

**Append-only convention, decided explicitly because the task required a decision rather than a
silent reorganisation, and NEITHER file was reorganised.** `DECISIONS.md` keeps append-only: it is
grepped and range-read, never injected, and its value is that a dated decision cannot be rewritten;
`docs/doc-budgets.tsv` already exempts it and says why. `AGENT_LOG.md`'s convention has ALREADY been
ruled on by the user (2026-08-08, "freeze + one-line entries", recorded on task `116179c8`, with
`f39083ae` adding the mechanical guard) — both still `todo`. This task points at that ruling and does
not re-open it.

## 2026-08-22 — `ACK-12-FU-WATCH-CORRELATION-KEY` (`f423959c`): the correlation key is DERIVED SERVER-SIDE and always present on the read path

The ACK correlation key was unreachable from the CLI. `agent-busctl watch` emitted only
`message_id` — the id the LOCAL bus minted — and `agent-busctl ack` accepts only the ORIGIN bus's
id, so for a relayed message the one id a recipient held was precisely the one the ack route
refuses. The documented workaround was out of band: the sender captured its own `message_id` and
told the recipient. This change puts the key on the read path. Three decisions, and the rejected
alternative, recorded because each is the reason and not the mechanism.

**1. It is derived on the SERVER, not in the client.** `internal/httpapi.WireMessage` gained
`CorrelationKey`, set in `toWireMessage` from `store.Message.OriginID()`. The alternative was to
emit the raw `origin_message_id` (empty on a same-bus message) and let each consumer branch on it.
**Rejected.** `OriginID()` is the ONE place the "origin id when set, local id otherwise" rule is
written down, and its own doc forbids re-spelling that branch at a call site; `client/` cannot
import `internal/store`, so a wire carrying only the raw origin id would force a SECOND copy of the
rule into `client/` and a third into `cmd/agent-busctl`. The drift would be silent — the wrong
branch still returns a well-formed message id, it just names the wrong bus's message, so an
acknowledgement would resolve to nothing rather than fail visibly. Deriving once, server-side,
keeps the branch where it already lives.

**2. It is NEVER `omitempty`, on the wire or in the CLI record.** Omitting it on a same-bus message
(where it equals `message_id`) looks like a saving and is a trap: every consumer would then write
`.correlation_key // .message_id`, which is the same origin/local branch re-spelled in shell, in
every agent. Worse, `jq`'s `//` operator treats the EMPTY STRING as truthy and `null` as falsy, so
that idiom would fall through to the *wrong* id silently on exactly the case it was written for.
Always-present makes `jq -r .correlation_key` one instruction with no fallback and no branch.
`OriginID()` never returns empty — it falls back to `ID`, which is always set — so the field can
never be `null` against a current bus.

**3. It is named for its PURPOSE, not as an id.** `correlation_key` is the vocabulary the ack plane
already uses: the `correlation_key` field of the `POST /v1/ack` request body,
`client.AckOptions.CorrelationKey`, and `internal/relay`'s peer ack frame. `OriginID()`'s doc states
that the value is "a CORRELATION key, not an identity" that "must never be served as 'the message
id'" — invariant 1, this bus never adopts a peer's id. Calling the field `origin_message_id` on the
read path would have invited exactly that reading.

Nothing on disk changed: no record type, no record-type number, no wire protocol version. The value
is derived at render time from state already durable.

The human `watch` render gained one line, `  ack key: <key>`, printed **only when the key differs
from `message_id`** — i.e. only for a relayed message. On a same-bus message the two are the same
string and the line would be noise on every message.

**Docs corrected rather than supplemented, because a stale warning reads as freshly checked.** Four
places told agents to use `.message_id` or to pass the id out of band, and all four are retracted in
place with the old text quoted: `AGENT_PROTOCOL.md` (the `ack` id rule, the cross-bus "no way to get
the origin id out of `watch`" paragraph, the `ack-status` notice, and the end-to-end worked
example), `cmd/agent-busctl/ack.go` and `cmd/agent-busctl/ackstatus.go` `--help`, and
`CONTRACTS-HTTP.md`'s transit-ack bullet. The TRAP itself is unchanged and is kept everywhere: the
ack route still refuses a correlation key whose bus half is this bus, so passing `message_id` for a
relayed message still exits `8` `unknown` with nothing recorded. Only the workaround changed.

**Two findings recorded while checking, both verified rather than assumed.** (i) `7d564118` was
described as "closed" in `ackstatus.go` and in `AGENT_PROTOCOL.md`; its Spec Server record read
`todo` on 2026-08-22. The BEHAVIOUR landed with `ACK-5`; the record did not. Corrected in place.
(ii) `CONTRACTS-HTTP.md` said a recipient reconstructs the signed bytes from `message_id` and `seq`;
on a RELAYED message the preimage carries the ORIGIN's pair instead —
`relay.RelayedMessage.CanonicalBytes` passes `MessageID: m.OriginMessageID, Sequence: m.OriginSeq`
(`internal/relay/signed.go:262-263`), and `client.Message.signingMessage`
(`client/canonical.go:215-218`) still feeds the local pair. Nothing verifies signatures on the read
path today, so this breaks nothing now; it is stated in `CONTRACTS-HTTP.md` as a trap for whoever
wires verification on, and is REPORTED rather than fixed by this task.

### Information disclosure: the origin SEQ becomes visible to the recipient — assessed and accepted (added by the security gate, 2026-08-22)

Recorded here so a later audit does not have to re-derive it. `correlation_key` is
`<origin-bus-id>-<seq>`, so for a RELAYED message the recipient now learns the ORIGIN bus's
sequence number. That number is a **strictly dense monotone counter** (`internal/ids/sequence.go`,
`s.floor++`, one allocation per `Next()`, never rewound, durable across restart), so a recipient
holding two relayed messages from the same origin with seqs `s1 < s2` can infer that exactly
`s2-s1-1` other allocations happened on that bus in between — the aggregate send-reservation plus
relay-ingest volume of every agent there. The inference is real and exact. It reveals no sender, no
recipient, no body and no agent name.

It is accepted, for four reasons in descending strength:

1. **The identical leak already exists for the local bus.** `WireMessage.Seq` has always carried the
   same dense counter to every recipient of every message. This extends an already-accepted side
   channel from the local bus to the origin bus; it does not introduce a new class.
2. **It was already sanctioned and already instructed.** `AGENT_PROTOCOL.md` at `493450f` told the
   sender to hand the recipient this exact string out of band. The change moves the channel
   in-band; it does not create the disclosure.
3. **The ACK-5 design requires it.** A recipient cannot acknowledge a relayed message without this
   string, because `POST /v1/ack` refuses a correlation key whose bus half is the local bus.
4. **Nobody new sees it.** `toWireMessage` is reached only from `writeBatch`, only from
   `handleMessages`/`handleWait`. Neither route is on `httpapi.UnauthenticatedRoutes()` (the
   middleware is default-deny), and selection is gated by `store.Message.VisibleTo`.

**A relayed BROADCAST cannot exist, so no origin id is fanned out to every local agent.** Verified
rather than assumed: `internal/relay/accept.go` refuses a broadcast at ingest and
`internal/hub/relayingest.go` hardcodes `broadcast: false`, so `OriginMessageID` is never set on a
broadcast record and a broadcast's `correlation_key` always equals its `message_id`.

**One LOW finding was accepted as a follow-up rather than fixed here.** The human render writes
`  ack key: <key>` with the same two-space indent that multi-line body continuations get, so a
sender can put `ack key: <id>` on its own line in a multi-line body and produce byte-identical
output — and on a same-bus message, where the genuine line is deliberately suppressed, the forged
line is the only one present. Impact is bounded: `POST /v1/ack` answers the uniform `unknown` for any
key the caller was not a recipient of. The `--json` path carries `correlation_key` as a real field,
which is the agent-facing surface invariant 7 governs, so the human feed is not the path an agent
should parse. Rendering the key at column 0 would make it unforgeable; that is the follow-up.

---

## 2026-08-22 — `97a315af`: security is SKIPPED by default for docs-and-tests-only changes with no guard file and no control-plane file

**What changed.** `CLAUDE.md` and `AGENTS.md` ("Agent roster") said the chain
spec-keeper → implementer → reviewer → security is MANDATORY for ANY code change, and that skipping
any step needs a one-line `AGENT_LOG.md` justification. Reviewer and documentation are unchanged and
still mandatory. Only SECURITY's default flips: it is **SKIPPED for a change touching ONLY docs and
tests AND no GUARD file AND no CONTROL-PLANE file**, and RUNNING it is then what needs the reason.
The rule is stated here in its FINAL, narrowed form; the control-plane clause was added before this
entry was committed, and the section "Narrowed the same day" below records how it was arrived at.
A GUARD file is enumerated concretely so the boundary needs no judgment call — an AST guard, any
`*guard*_test.go`, and any test whose removal disables an invariant check. A CONTROL-PLANE file is
anything that decides WHAT is checked or performs the check: `CLAUDE.md`, `AGENTS.md`,
`INVARIANTS.md`, anything under `.claude/`, `scripts/doc-check.sh`, `scripts/proof-check.sh` and any
other check or gate script, `docs/doc-budgets.tsv`, `docs/doc-preserve.tsv`.
**And EVERY skip still needs its `AGENT_LOG.md` line — the carve-out security skip included** —
naming the skipped tier and the exact paths it covered; that entry is what the periodic sweep
(`ed6853d4`) scopes against, and `.claude/agents/integrator.md` REFUSES the commit without it.

**Who decided.** The user, on 2026-08-22, answering a proposal backed by measurement. The proposal
and the decision are on task `97a315af-70b3-4a64-8456-92335d8c9631` as the `kind=request` note by
`main` (2026-08-22T08:22:14Z).

**The measurement.** A sample of the 1000 most recent project notes held 60 `kind=response` notes
authored by `security`, spread across 35 distinct tasks: 33 PASS, 16 demanding changes (14
CHANGES-REQUESTED + 2 CHANGES-REQUIRED), the rest re-audits and addenda. So security demands changes
on 27% of its verdicts — it is load-bearing, not ceremony. The cost sits elsewhere: 18 of those 35
tasks (51%) needed two or more security passes, two needed three, one needed four, one needed five,
and most re-gates re-audit the whole change rather than the delta since the last verdict. **This
carve-out therefore ROUTES effort; it does not remove the gate.** The waste was in re-gating, not in
the gate. (Re-measured independently on 2026-08-22 at 08:34Z from the same endpoint: 60 security
`kind=response` notes, 35 tasks, 18 with two or more passes — identical. The changes-demanding count
reproduces as 16 or 17 of 60 (27–28%) depending on whether a note whose verdict token is a bare
`CHANGES` is counted with the CHANGES-REQUESTED class.)

**The alternative that was rejected: fold security into a single commit-time pass, like `integrator`.**
Refused. The integrator's checks are mechanical and context-free — does HEAD compile, is the deletions
column zero, is the pathspec scoped — and they lose nothing by running late. Security review needs
intent and threat model. That is cheap while the implementer is still live and expensive to
reconstruct afterwards, and the finding it produces can be architectural rather than local.

Evidence, from ACK-5. Security's `kind=response` of 2026-08-21T15:00:17Z returned CHANGES-REQUESTED
and promoted an unmetered outbound peer request to blocking: the synchronous backward acknowledgement
hop passed the inbound request context straight to `Propagate` with no deadline, the peer HTTP client
deliberately sets no `Timeout`, and a stalled upstream therefore held one of only 8 per-peer in-flight
admission slots — a bucket SHARED with relay message ingest, so the stall denied an honest downstream
peer its ingest too. The fix was structural: a bounded hop (`cmd/agent-bus/ackback.go:207`
`ackTransitTimeout`, applied at `:536`) plus a per-upstream in-flight cap taken BEFORE any address
resolution or dial (`enterUpstream`, `:359`, entered at `:494`), which the 2026-08-21T21:36:19Z
verdict then re-audited with a nine-mutation battery (7 of 9 KILLED). Found at commit time instead,
that would have meant shipping the hole or unwinding the work already built on top of it.

**Why the guard-file exception exists.** A test file can be security-relevant, so "docs and tests
only" alone would be an unsafe rule. The standing example is invariant 11: `client/pin.go` carries the
repo's single permitted `InsecureSkipVerify: true`, paired with `VerifyPeerCertificate` in the same
composite literal and enforced by an AST walk in `client/guard_test.go`. Deleting either the line or
the callback beside it silently disables certificate pinning while every positive test still passes —
a change that reads as tidying and removes a security property. The rule is therefore "docs and tests
only **AND** no guard file", never "docs and tests only".

**This is INTERIM.** A tiered chain (T0–T4, keyed off the invariant planes a change touches, with a
mechanically-computed floor that an implementer may raise but never lower) is being planned now, and
it will ABSORB this carve-out as its T0/T1 case. Do not read the carve-out as the final shape of the
rule. No backlog task for the tiering scheme existed when this entry was written (checked 2026-08-22
against a complete task listing, `X-Total-Count: 808`); the two sibling follow-ups filed alongside
this one do exist and are both `todo` — `727dc387` (security re-gates must be delta-scoped, citing
the prior verdict) and `ed6853d4` (a periodic repo-wide security sweep, additive to per-task review).

**Residual risk, accepted deliberately.** Low-tier changes now get less scrutiny, and that is a real
cost, not a rounding error: the classifier is filename-shaped, and a security-bearing assertion can
live in a `_test.go` file whose name contains no `guard`. The offset is partial — the periodic
repo-wide sweep (`ed6853d4`, still `todo` as of 2026-08-22, so the offset is PLANNED and not yet in
place) catches what a per-task gate skipped. A tiering scheme makes that sweep MORE necessary, not
less.

**Consumers updated in the same beat**, because the rule is inoperable without them.
`.claude/agents/integrator.md` step 1 required the report to state reviewer AND security COMPLETED and
to REFUSE otherwise; since `integrator` is the only agent permitted to commit, that would have refused
every legitimate carve-out commit. It now requires reviewer COMPLETED always, and security COMPLETED
unless the carve-out applies — and it **verifies the carve-out mechanically from the diff rather than
accepting the owning agent's assertion of it**, default-denying any path it cannot classify.
`.claude/agents/feature-runner.md` (the chain statement and the GATE STATUS report line) and
`.claude/agents/spec-keeper.md` (definition of done) were corrected to match.

**Narrowed the same day, BEFORE commit: CONTROL PLANE never qualifies.** As first written the
carve-out was "docs and tests only, and no guard file". The change that introduced it was seven
files, every one of them `.md`, so **it qualified for its own exemption** and would have been
committed with no security verdict. Caught reviewing the staged change against HEAD `231b769` on
2026-08-22:

```
$ git status --porcelain | sed 's/^...//' | grep -Ev '\.md$|_test\.go$'
            # no output — every path in the change passed the carve-out's own test
```

The category error was calling `CLAUDE.md`, `AGENTS.md` and `.claude/agents/*.md` documentation.
They are CONTROL PLANE: they determine which checks run at all. `scripts/doc-check.sh` and
`scripts/proof-check.sh` are the same class — they ARE the verification machinery, and security
review has already found two real defects in `doc-check.sh`: an injectable `TMPDIR` that ran an
attacker's command while the instrument printed `SELFTEST PASS: 27/27`, and a `sed` option-injection
where a file named `-n` was eaten as an option so `sed` read the needle from STDIN (both MEDIUM,
2026-08-21; guards at `scripts/doc-check.sh:1030-1097` and `:335-345`). `docs/doc-budgets.tsv` holds
the ceilings the budget check enforces, and `docs/doc-preserve.tsv` the phrases it protects.

**The rule, stated so it generalises past the list: a file that determines WHAT is checked, or that
PERFORMS the check, is control plane. Changing it can disable a check without touching a line of
product code, and that is exactly the change that needs review.** A `.md` extension does not make a
file documentation. The enumeration is a floor for the mechanical check
(`.claude/agents/integrator.md` step 1, check (c)); the principle is what an agent applies to a file
nobody anticipated, and "it is not on the list" is not an argument.

The carve-out itself stands as the user approved it on 2026-08-22 — this NARROWS it, it does not undo
it. Applied in all three places it is stated: `CLAUDE.md`/`AGENTS.md` "Agent roster",
`.claude/agents/integrator.md` step 1, and `.claude/agents/feature-runner.md` +
`.claude/agents/spec-keeper.md`. The incident, the table of control-plane paths and the reasoning are
in `PITFALLS.md` §8; `CLAUDE.md` carries the one-liner and the pointer, per its own where-a-warning-
goes rule. Paying for the `CLAUDE.md` addition inside a 5-byte budget meant tightened sentences, a
shorter `.claude/ORCHESTRATION.md` pointer, and moving an out-of-scope Spec Server pagination bullet
to the task that owns it (`SPEC-API-LIST-SILENT-TRUNCATION`, 301 B). The 14-name agent roster was
cut in a first pass and RESTORED once that 301 B came back: the roster is 235 B, it is what
`.claude/ORCHESTRATION.md:8` says `CLAUDE.md` keeps, and deleting a list to buy room for content
that then leaves is a bad trade. `doc-check.sh budget` PASSES: `CLAUDE.md` is 28779 B against its
unchanged 28781 B ceiling — 2 B of spare, tighter than the 5 B before the change — and `AGENTS.md`
is byte-identical (`cmp`).

The narrowing is self-demonstrating: check (c) prints five paths for THIS change, so the change that
narrows the carve-out does not qualify for the carve-out.

## 2026-08-22 — AUTH-DUP-ENROL-KEY (`ac4f9c2b`): a duplicate enrolment public key is REJECTED, not idempotently returned

**The defect.** `Service.Enrol` validated the AUTH public key's LENGTH but never checked whether that
key was already on the roster. Three enrolments with a byte-identical public key were all accepted and
minted three distinct agent ids bound to ONE keypair (found by the security gate on
AUTH-1-FU-ACTIVECAP, verified empirically). One private-key holder could then authenticate as any of
those ids — an impersonation and accountability hole — and it was the direct reason the per-agent
active-session cap raised an attacker's flood cost by only ~1.6%: the "distinct enrolments" the cap
counted were one keypair, not many identities the attacker had to obtain.

**The decision: REJECT the second enrolment with `auth.ErrAuthKeyBound` (409, connection KEPT), NOT
idempotently return the first agent's id.** Two behaviours were defensible; this one was chosen.

Why reject, not idempotent-return:

- **Idempotent-return conflates two different keys.** The enrolment IDEMPOTENCY key (a client-supplied
  retry token, `EnrolRequest.IdempotencyKey`) is a different concept from the enrolment PUBLIC key.
  Returning the existing agent id to whoever re-presents a public key would key identity resumption on
  a value that is PUBLIC (agent auth keys are listed on `GET /v1/agents`), so anyone who read another
  agent's key could "enrol" and be handed that agent's id. They still could not obtain a session
  without the private half, so no session is granted — but returning an identity a caller cannot use,
  and doing so off a public value, muddies invariant 1 for no benefit.
- **Idempotent-return would silently ignore a genuinely different request.** The same public key can
  arrive with a DIFFERENT requested name or invite. Returning the original agent id would apply
  neither, leaving the caller believing a name/invite was registered that was not — the same
  "worse than a missing check" failure the messaging-key idempotency comment already documents.
- **Consistency with the established, security-gate-approved pattern.** The codebase already keeps one
  CERTIFICATE from naming two agents (`ErrCertFingerprintBound`, MTLS-BIND). The auth public key is
  the same kind of identity-bearing credential, so it gets the same shape of rule: an authoritative
  refusal in `Roster.Put` (new rule 3, in the same critical section as the insert) and an advisory
  pre-mint read (`Roster.AgentIDForAuthKey`) in `Enrol` so the refusal burns no agent-id suffix.

The genuine idempotent RETRY is untouched: same idempotency key + same payload still replays the
original result, because the idempotency replay check runs BEFORE the auth-key read. Nothing here
disconnects — enrolment is not a signed-message replay, and `/v1/enroll` is unauthenticated so the
socket names no principal to punish (invariant 10's two questions).

**On the oracle concern.** Rejecting does tell an enrolling caller "this key is already enrolled". That
disclosure is bounded and accepted: enrolment is invite-gated on the shipped bus (invariant 3), so
each probe costs a single-use invite; the key is already public on `GET /v1/agents`; the reply names
NO agent id (only the server log does); and `ErrCertFingerprintBound` already makes exactly this trade
for the certificate axis.

**Why the check is BEFORE the mint.** A refusal placed only in `Roster.Put` (after `minter.Mint`)
would burn an agent-id suffix on every duplicate, and because a failed enrolment aborts and RELEASES
its invite reservation, a single invite could drive an unbounded suffix-burn loop with one keypair and
fresh names — the exact hazard MTLS-BIND records for the certificate check. The pre-mint advisory read
closes it; `Roster.Put`'s check under its own lock stays the authoritative one. `WALRoster.Apply` does
NOT run the write-side check (it replays already-durable records; invariant 6), so a damaged log can
present two agents under one key, and `AgentIDForAuthKey` fails closed with `ErrAuthKeyAmbiguous`
rather than picking one.

Files: `internal/auth/{errors.go,authkey.go,roster.go,walroster.go,service.go}`,
`internal/httpapi/auth.go` (the 409 mapping). Docs: `CONTRACTS-HTTP.md`, `AGENT_PROTOCOL.md`. Proven
by `internal/auth/authkey_test.go` (RED-before confirmed: the three enforcement tests accept the
duplicate with the checks removed).

## 2026-08-22 — Per-source rate limiting on the unauthenticated credential routes (AUTH-1-FU-RATELIMIT)

The three unauthenticated credential routes — `/v1/enroll`, `/v1/session/begin`,
`/v1/session/complete` — had no per-source rate limit. Every admission cap behind them is GLOBAL, so
one anonymous source could exhaust `MaxRosterEntries` (4096) with enrols or `MaxSessions` (16384)
with session/begins and deny the whole bus. Security measured ~137 req/s from a single source as
enough to sustain the session-table denial.

**Decision: a stdlib per-source token bucket in front of those three routes, refusing with 429 +
Retry-After, never a disconnect.**

- **Stdlib, not `golang.org/x/time/rate` (invariant 8).** A token bucket is ~40 lines; a dependency
  is not justified. `internal/httpapi/ratelimit.go`.
- **Keying: the TCP peer address with its port stripped (`remoteHost`), proxy headers IGNORED.**
  Same "source" identity `LoggingMiddleware` already records. `X-Forwarded-For` is trivially forged
  by the attacker this guards against, so it is not trusted. HONEST LIMITATION, documented and not
  papered over: clients behind one NAT / reverse proxy / SSH tunnel / Docker bridge (every container
  is `172.17.0.1`) collapse to ONE key and share ONE bucket, throttling each other. The burst
  default (60) is sized to absorb ~20 agents bootstrapping at once (3 requests each) from one address.
- **Refusal is a 429 with an integer `Retry-After`, logged at Info, NEVER a disconnect
  (invariant 10).** Too-fast is not replay; a merely-busy or merely-buggy client must keep its
  socket, and one anonymous socket may carry a legitimately busy client. It runs BEFORE any body
  parse or credential read, so a throttled request consumes no roster/session capacity and touches
  no token.
- **It sits IN FRONT of the allow-list and does not change its membership (invariant 3).**
  `unauthenticatedRoutes` in `authmw.go` is untouched; `rateLimitedRoutes` is a separate set derived
  from the three route constants. The middleware is innermost, inside `authMiddleware`.
- **Mechanism in `internal/httpapi` (`Options.AuthRateLimit`), policy in the composition root.**
  The zero value disables the limiter, so every embedder that does not opt in — and the entire
  existing test suite — is unchanged. `cmd/agent-bus` enables it by default (`-auth-rate-limit 5`,
  `-auth-rate-burst 60`; burst 0 disables). A positive burst with a non-positive rate is refused at
  flag-parse time: a bucket that never refills would 429 forever once drained.
- **Memory bound:** an opportunistic sweep drops buckets that have refilled to capacity — they hold
  no throttling state (a fresh bucket admits identically), so removal changes nothing and the map
  stays proportional to the sources currently being throttled, not to every source ever seen.

---

## 2026-08-22 — AUTH-4: `POST /v1/leave` and durable roster removal (the tombstone, sessions, self-vs-operator, suffix growth)

**Context.** The AUTH epic stalled on this: `WALRoster` had no durable remove path, so an agent
could not leave the bus and AUTH-5 could only prove token-recovery, not agent-revocation-recovery.
AUTH-4 adds a `POST /v1/leave` route, `auth.Service.Leave`, `Roster.Remove`, the `agent-busctl
leave` subcommand and `client.Leave`. Invariants read in full before writing: **1** (ids never
reused, incl. across restart — the departed id is not re-issued), **3** (roster is the authoritative
identity set; sessions are memory-only opaque handles; `/v1/leave` is AUTHENTICATED and is NOT on
`unauthenticatedRoutes`), **4/5/6** (durable two-phase write; recover to a prefix; append-only log,
metadata only), **10** (idempotency — leaving twice is a clean retry).

**Decision — removal is a TOMBSTONE, reusing the enrolment wal kind.** `Roster.Remove` APPENDS an
`auth.RecordKind` ("agent") record carrying the departing agent's own entry with a new `left_at`
field set. `WALRoster.Apply` deletes the agent when it decodes a record whose `LeftAt != nil`.
Recovery replays enrol-then-leave to "absent". Nothing is rewritten or truncated (invariant 6).

- **Why reuse the "agent" kind rather than a new one.** A new wal kind would need registration in
  `cmd/agent-bus/main.go`'s applier map, `cmd/agent-bus/operator.go`, a `MultiplexApplier` entry and
  — the day checkpoints are wired — a `CheckpointParticipant.Kinds()` entry. Reusing "agent" needs
  none of that: the roster already owns the kind. It is also STRICTLY BETTER for invariant 1 — the
  leave record carries the departed agent id, so `EnrolmentSuffixesInWAL` folds it exactly like an
  enrol record and the departed suffix stays in the derived floor even if the enrol record is later
  compacted.
- **`left_at` is optional and omitempty**, so a live enrolment record is byte-identical to a
  pre-AUTH-4 one and `RecordVersion` does NOT move (adding an optional field is the same
  no-bump precedent INVITE/MTLS set). `RosterEntry.LeftAt` is nil on every serving entry; it is only
  ever non-nil transiently in the tombstone body that `Apply` consumes to delete.

**Decision — sessions are DROPPED at leave; a restart drops them anyway.** Sessions are opaque,
memory-only handles (invariant 3). `Service.Leave` sweeps the session table for the departing agent
AFTER the durable removal, so its live tokens stop authenticating at once on the running bus; a
restart loses all sessions regardless, so "stays gone across a restart" is automatic.
`CompleteSession`/`BeginSession` already re-read the roster and refuse a departed id.

**Decision — SELF-LEAVE ONLY.** `/v1/leave` acts on the AUTHENTICATED principal
(`PrincipalFromContext`), never a body field, so there is no way to name a victim. Operator-initiated
revocation of ANOTHER agent is a separate concern (AUTH-7 / AUTH-ROSTER-RECLAIM) with a different
authority model and is deliberately NOT built here.

**Decision — undelivered direct messages to a departed agent are NOT erased.** The log is
append-only and bodies live in the hub, not the roster. They become undeliverable (the recipient id
is gone from the roster, so the hub's read paths fail closed for it), and a re-enrolment under the
same name is a NEW id (invariant 1) that does not inherit them.

**Decision — suffix-counter growth is bounded by INVITES, not by reclamation (the AUTH-4 acceptance
criterion).** The per-name suffix floors (`ids.NameSuffixes`, one entry per distinct name ever
enrolled) are NEVER reclaimed on leave — point 5 of that type's doc forbids forgetting a name, and
reclaiming would reuse an id (invariant 1). Before leave, distinct-name growth was bounded because
the roster never shrank and admission capped `roster.Len()`. Leave breaks that coupling, so the bound
moves to invariant 3's invite gate: on a gated bus every enrolment costs one operator-minted,
single-use invite, and the gate sits ABOVE the mint (`service.go`), so a refused enrolment burns no
suffix. Distinct-name growth is therefore bounded by invites redeemed — a controlled resource — not
by an anonymous enrol/leave loop. Eviction was rejected (invariant 1); an unbounded-but-slow argument
was rejected in favour of this concrete invite bound. `TestRosterLeaveDoesNotReclaimSuffixFloor`
pins that a re-enrolment gets a strictly higher suffix; the pre-AUTH-4 guard
`TestRosterDoSRosterInterfaceHasNoReclamationMethod` was rewritten (as its own doc required) into
`TestRosterReclamationIsLeaveOnly`, which now asserts exactly ONE sanctioned reclamation verb
(`Remove`) and forbids the AUTOMATIC ones (evict/expire/prune/compact) that would free a slot without
the holder acting.

**Consequences.**
- New durable behaviour: a `left_at` record can appear in any WAL. A binary older than AUTH-4 would
  fail to decode it (strict `DisallowUnknownFields`) and discard it — the agent would stay enrolled
  after a downgrade. Downgrade is unsupported (forward-only, one container), the same posture INVITE
  and MTLS took.
- `Roster.Remove` is on the interface, so every implementation (MemoryRoster, WALRoster, the three
  in-package test doubles) implements it.
- Operators: rebuild the binary and restart to serve `/v1/leave`. Existing logs and enrolments are
  unaffected — no format bump, no migration.
- Paired durability proof `TestLeaveRevocation` (crash-injection + idempotency + session-drop) and
  end-to-end `TestClientLeaveEndToEnd` (real client against real server) close
  AUTH-5-FU-REVOCATION's realizable half and unblock AUTH-7.

## 2026-08-23 — ACK-3 R4: the ACK wire frame version field, error code, and the both-frames obligation split

`ACK-3` (peer-hop ACK/NACK, `POST /v1/peer/ack`) shipped and its security gate passed, but the last
reviewer verdict was CHANGES-REQUESTED (2026-08-16), and the orchestrator (`main`) held the task open
on one item, R4: three rulings that the CODE already implements but that **supersede
`ACK-CONTRACT.md`** live only in the code comments and the `CONTRACTS-*.md` plane files, and
`DECISIONS.md` records none of them. Superseding a contract document is exactly what `DECISIONS.md`
is for. This entry records the three, each with the superseded contract statement and the file:line
where the shipped code implements it, so a later reader can check the decision against the code. main's
words for the three (task journal, 2026-08-16T14:53): "the protocol_version spelling, a distinct
unsupported_ack_version code, and the both-frames obligation split."

Invariants read IN FULL before writing this: **Invariant 1** (server-authoritative ids/sequence,
never reused — the version is a RESERVED value, spent not chosen) and **Invariant 10** (idempotency,
and specifically that an unrecognised version is REFUSED, never defaulted, because a terminal outcome
is absorbing and a frame mis-read under the wrong version could durably settle an outcome that can
never be revisited).

### Ruling 1 — the field is spelled `protocol_version`, not `wire_version`

`ACK-CONTRACT.md` §9.2 (the frame sketch, line 576) and §10 (the versioning ruling, line 730) both
name the field `wire_version`. The shipped frame names it `protocol_version` — JSON key
`protocol_version`, `omitempty` — at `internal/relay/ackframe.go:232`, with the supersession recorded
in the field comment at `ackframe.go:211-225`.

Rationale: the key `version` is already taken on a neighbouring peer envelope by `RosterUpdate`, where
it is a monotonic ROSTER EPOCH and **not** a protocol version; two meanings on one key is how a peer
ends up applying a roster epoch as a format number (`ackframe.go:211-215`). The two frames of one peer
protocol must not disagree about the name of their version field, and RELAY-23 pins the same
`protocol_version` spelling on the relay envelope, so that is the spelling that wins. This engages
invariant 1: a client-declared version is validated input, never a trusted identity.

### Ruling 2 — a distinct `unsupported_ack_version` code, not the existing `invalid_request`

`ACK-CONTRACT.md` §9.3 (status table, line 600) folds "unknown `wire_version`" into "**400**, existing
`CodeInvalidRequest`". The shipped code instead returns a distinct wire code
`CodeUnsupportedAckVersion = "unsupported_ack_version"` (`internal/relay/handshake.go:105`), backed by
its own sentinel `ErrUnsupportedAckVersion` (`internal/relay/ackframe.go:116`), mapped to the wire
code at `internal/relay/peer.go:411`.

Rationale (`ackframe.go:104-115`): an unsupported version and a malformed field are different OPERATOR
problems with different remedies, and only the stable code crosses the wire. Folded together, a peer's
operator reads `invalid_request` and hunts a malformed field that does not exist, when the real remedy
is to upgrade one of the two buses. It stays a **400, not a 503**: a retry cannot install a new binary
at either end, so the verdict is final. The code was also added to the sending side's
`peerErrorCode` allow-list (`internal/relay/client.go:331`) so a sending bus surfaces it verbatim
rather than reporting "unrecognised error code" — this was the security gate's low finding 3, fixed in
ACK-3.

### Ruling 3 — the both-frames versioning obligation is SPLIT: ACK-3 ships the ACK frame, RELAY-23 owns the relay envelope

`ACK-CONTRACT.md` §10 (line 730) ruled that `ACK-3` MUST add the version field to **BOTH** the
existing relay envelope AND the new ACK frame in the same change, spending the already-reserved
`relay-wire-version = 1`. §10 also stated the fallback (lines 758-760): "If the reviewer rules this
outside `ACK-3`'s file boundary, it becomes a separate task that `ACK-3` is blocked by. It does not
get skipped, and `ACK-3` does not ship an unversioned frame in the meantime."

The reviewer exercised exactly that fallback: the relay-envelope half was ruled outside ACK-3's file
boundary (a separate frame, and RELAY-23 holds the `relay-wire-version` reservation). So the both-frames
obligation is **split**:

- ACK-3 ships only the versioned ACK frame. The relay envelope's version field is deferred to
  RELAY-23, recorded as a `blocks` edge RELAY-23 -> ACK-3 in the Spec Server (created
  2026-08-16T13:50:48Z, per the spec-keeper journal note). RELAY-23 has NOT landed: verified at HEAD
  that `relay.WireVersion` / `RelayRequest.ProtocolVersion` are absent from
  `internal/relay/message.go`, and `RelayedMessage` still carries no version field.
- ACK-3 spends `relay-wire-version = 1` with NO second reservation. `AckWireVersion = 1`
  (`internal/relay/ackframe.go:92`) IS that reserved value, deliberately a separate constant only until
  RELAY-23 lands `relay.WireVersion` — a sequencing call, not a claim that two constants are
  acceptable (`ackframe.go:77-91`). The collapse onto `relay.WireVersion` is deferred to the follow-up
  `ACK-3-FU-COLLAPSE-WIREVERSION`.
- The version-READING rules ACK-3 owns hold whichever constant survives (invariant 10): an ABSENT
  version reads as 1 via a LITERAL that must never be respelled as the constant
  (`resolveAckWireVersion`, `ackframe.go:132-153`), and an UNRECOGNISED version is REFUSED, never
  defaulted (`ackframe.go:146-154`). Refusing rather than guessing matters here because the ACK frame
  carries a TERMINAL outcome and terminal is absorbing (§8.1) — a future-format frame read under
  version 1's rules could durably write a `delivered` or `refused` that can never be corrected.

Cross-references: `ACK-CONTRACT.md` §9.2, §9.3 and §10 are the superseded statements; the
`CONTRACTS-*.md` plane files already carried these three as built; RELAY-23 (the relay-envelope
version field) and `ACK-3-FU-COLLAPSE-WIREVERSION` (collapse the duplicated constant) are the open
follow-ups.

## 2026-08-23 — ACK-4-FU-RECIPIENT-BINDING: the direct arm binds the recipient's HOME bus to the peer

**Decision.** `relay.AuthorizePeerAck`'s DIRECT arm now requires, in addition to
`DeriveJobID(P, K)` naming an outbox job this bus durably wrote, that the recipient `R`'s home bus
equals the authenticated peer `P`, compared case-insensitively (`strings.EqualFold`). On a mismatch
it returns the SAME uniform `ErrAckNotBound`. No on-disk format changes; `DeriveJobID` is unchanged.

**The defect it closes.** The direct arm previously bound only `(peer, key)`. The outbox job is keyed
on the recipient's HOME bus (`Forwarder.targets` → `Registry.Route(recipient)`), so `DeriveJobID(P, K)`
is the job for recipients whose home bus is `P`. When a correlation key has more than one recipient
row — which a destination-side or broadcast-side lifecycle row (`ACK-12-FU-DESTINATION-ROW`, P0)
introduces, and the outbox already fans one key to several peers — a peer legitimately bound for its
OWN recipient of `K` could name any SIBLING recipient of `K` and be authorised on the `(peer, key)`
job alone; `SettleAck` then found that sibling's row and settled it. A terminal outcome is ABSORBING
(the first terminal stands, never revisited), so this was an uncorrectable cross-recipient /
cross-peer forgery, including burning a LOCAL recipient's terminal. Found by the security gate during
ACK-3 (2026-08-16); "must land before any task creates a second row for one correlation key."

**Keying — why home-bus and not a per-recipient job id.** The security property required is "a peer
may settle only the recipients it was routed." The peer→recipient routing fact this bus holds is the
outbox job, keyed on the recipient's home bus. Binding `EqualFold(homeBus(R), P)` on the direct arm
gets exactly that property (a peer whose obligation names bus `P` may settle only recipients on `P`)
with zero durable-format change. Re-keying `DeriveJobID` on the recipient was rejected: the job id is
durable, one job per destination bus is deliberately shared by all recipients on it, and a
per-recipient id would (a) multiply the peer-driven fsync cost per recipient, (b) orphan every
pending job across an upgrade, and (c) add no security the home-bus binding does not already give.
The recipient's home bus is readable because invariant 2 makes `R` fully qualified. `EqualFold`
(not `==`) because `DeriveJobID` is case-sensitive while bus ids are folded everywhere else, and it
matches the case-folded `D`-vs-`P` comparison the INDIRECT arm (`AuthorizePeerAckVia`) already makes.

**Locus — direct arm, not `SettleAck`.** The original filing suggested making "the second half"
(`SettleAck`'s `(key, recipient)` existence check) recipient-aware. That check lives in `ack.Store`,
which holds no peer/routing information, so it cannot answer "was this peer routed this recipient."
The routing knowledge is the outbox, in the relay layer, so the peer↔recipient binding belongs in
`AuthorizePeerAck(Via)`. `SettleAck`'s existence check remains the conjunctive complement.

**Invariant 10 — refuse, do not disconnect.** The mismatch is refused with `ErrAckNotBound` and NO
disconnect: an ACK frame is not a signed message and must never reach the one disconnect path; a
merely buggy or mis-routed peer can reach this line, and a peer link carries its whole roster's
traffic. The refusal is byte-identical to every other unbound cause, preserving the uniform-answer
oracle protection.

**Verification.** RED-first: `TestAckDirectArmBindsRecipientHomeBus/cross_recipient_forgery_*` and
`.../forgery_is_refused_end_to_end_through_AuthorizePeerAckVia` both FAILED against unmodified
`ack.go` (returned `nil`), then passed after the fix. The three-bus e2e ack test
(`TestThreeBusEndToEndAckNack`) still passes, confirming multi-hop back-propagation (recipient home
bus ≠ acking peer, but routed through it) is unaffected. `internal/httpapi`'s
`TestPeerAckBindsToTheCertificateResolvedBus` used a placeholder recipient on the LOCAL bus — never a
legitimate peer-ack target — and was updated to a recipient on the acking peer's bus.

## 2026-08-23 — MTLS-BIND: enrolment accepts the absence of a client certificate, and ignores an out-of-window presented certificate rather than binding it

This records the two MTLS-BIND decisions the task text required in `DECISIONS.md`, and it narrows the stale
`MTLS-CLIENTAUTH` open-gap text at `DECISIONS.md:4497-4501`. It is about the AGENT enrolment plane only.
`MTLS-CROSSCHECK` owns the later per-agent ENFORCEMENT step; this section records what `MTLS-BIND` itself does
before any cross-check exists.

**(a) Absence of a client certificate at enrolment is ACCEPTED.** `cmd/agent-bus/tlslisten.go` uses
`tls.RequestClientCert`, which requests a certificate and never requires one, so an empty
`r.TLS.PeerCertificates` is the ordinary case on this listener. `internal/httpapi/clientcert.go`
(`WithClientCertificate`, `presentedClientLeaf`) therefore admits the request and attaches nothing when no
certificate was presented, and `internal/httpapi/auth.go` reaches enrolment with `enrolCertFingerprint(r) == nil`.
The request is still served and may still return 201.
- Why this is the decision rather than an omission: refusing here would invent “a client certificate is mandatory”
  in HTTP middleware while the transport says it is optional, which would lock out `/healthz`, the container
  healthcheck, and every client identity directory that does not yet hold a client keypair. `MTLS-CLIENTCERT` is a
  separate task; MTLS-BIND could not make its absence fatal without stranding the clients it was supposed to migrate.
- Why this does not weaken invariant 11: a missing certificate authorises nothing. It produces no
  `ClientCertificate`, no durable `cert_bindings` entry, and no agent can satisfy invariant 11's pair with “no
  certificate”. The migration-safe target is the later per-agent rule: once an agent HAS a binding, requests using
  its session token must present it. That is `MTLS-CROSSCHECK`, not this task.

**(b) A presented certificate outside its own validity window is IGNORED and never durably bound.** The listener's
TLS handshake proves possession of the private key and does not check `NotBefore` or `NotAfter`, so an expired or
not-yet-valid certificate completes the handshake and arrives in HTTP exactly like a fresh one. `internal/httpapi/clientcert.go`
therefore calls `checkClientCertValidity(leaf, s.now())` before publishing the fingerprint to the request context. On
failure it logs at INFO with the fingerprint, serves the request anyway, and attaches nothing. Because enrolment reads
only `enrolCertFingerprint(r)`, a rejected certificate never reaches `auth.EnrolRequest.CertFingerprint` and therefore
never becomes a durable `cert_bindings` row.
- Why this is the decision rather than a local convenience check: expiry is the only automatic bound on a leaked client
  key. Binding an already-expired certificate would mint durable roster state for a credential the transport should no
  longer accept, and the agent plane would lose the only automatic lifetime bound it has. The check therefore sits at
  the transport edge, before any fingerprint is published, and it uses `crypto/x509`'s verdict through the shared
  `checkClientCertValidity` helper rather than a local date comparison.
- Why the request is still served: the invalid certificate names nobody once its validity window is outside `x509`'s
  verdict. Refusing the entire request would again make “a valid client certificate is mandatory for enrolment” the de
  facto rule, which this task was not authorised to do. The contract is narrower: the request continues WITHOUT a
  transport identity, so it can bind nothing and satisfy no cross-check.
- Why this must be distinguished from `MTLS-CROSSCHECK`: MTLS-BIND creates or omits the FACT on the roster.
  `MTLS-CROSSCHECK` is the later 403 gate that refuses a session token presented over the wrong certificate. Saying
  MTLS-BIND “enforces invariant 11” would still be false; what it enforces is the antecedent that only an in-date
  presented certificate may become the durable fact the cross-check reads.

Evidence at HEAD when this note was written:
- `internal/httpapi/clientcert.go`: `WithClientCertificate`, `presentedClientLeaf`, `enrolCertFingerprint`.
- `internal/httpapi/auth.go`: enrolment consumes only `enrolCertFingerprint(r)`; no second certificate read exists.
- `internal/httpapi/clientcert_mtlsbind_test.go`: `TestNoClientCertificateIsAdmittedWithNoIdentity`,
  `TestExpiredClientCertificateIsIgnored`, `TestNotYetValidClientCertificateIsIgnored`, and the enrolment guard that a
  presented expired certificate still yields 201 but binds nothing.
- `CONTRACTS-HTTP.md`, section `### The client certificate on an agent connection`: the shipped contract already says
  the same thing and now has its decision record here.

## 2026-08-23 — MTLS-MIGRATE: first client-certificate binding for a legacy HTTP identity uses the existing auth session plus an explicit bus pin

**Decision.** A pre-TLS identity that is already enrolled, still holds its auth private key, and still can complete the normal session handshake may migrate to mTLS without spending an invite and without minting a replacement agent id. The client must be given the HTTPS bus URL and bus certificate fingerprint out of band. It uses that pin in memory, connects over pinned TLS, presents its local client certificate, completes the normal auth-key session handshake, signs the bootstrap intent with the enrolled AUTH private key, and calls `POST /v1/client-cert/bootstrap`. The server binds the certificate fingerprint from TLS to the authenticated bearer principal's existing agent id. Only after the server accepts does the client durably record the HTTPS URL and first bus pin.

**Why this is sufficient authority.** The auth key is already the credential that proves control of the enrolled identity (invariant 3), and the bus certificate fingerprint is still explicit operator input, so the migration does not introduce trust-on-first-use (invariant 11). A live bearer token alone is not sufficient: the route verifies an Ed25519 signature over `agent-bus:client-cert-bootstrap:v1:` plus the active session token, idempotency key, and TLS-derived client certificate fingerprint. A stolen bearer presented with an attacker certificate therefore fails unless the attacker also holds the enrolled auth private key. The request body carries no agent id and no certificate fingerprint: the server obtains the agent id from `authMiddleware` and the certificate from `WithClientCertificate`, preserving server authority over ids (invariant 1) and avoiding a body-supplied certificate claim.

**Why no invite is used.** The invite admits a new identity. This operation changes neither the agent id nor the auth key; it adds the missing transport binding to an existing roster entry. Spending or requiring an invite here would make routine TLS migration look like re-enrolment and would contradict the rotation/no-re-enrolment direction already recorded in E3.

**On-disk shape.** No record kind, numeric type, or schema version is reserved. `cert_bindings` already exists in `auth.RecordVersion = 1`; the durable migration appends another `auth.RecordKind = "agent"` roster record with unchanged identity fields, exactly one additional live binding, and `cert_bootstrap_idem` set to the successful idempotency key. Recovery accepts only that duplicate-agent-id shape; all other duplicates still keep the first record. That idempotency key is deliberately durable: same key plus same presented certificate replays the original accepted body after restart, including `already_bound:false` for a first binding. After the first binding, same key plus a different presented certificate is refused by `authMiddleware`'s certificate/session cross-check as `403` before the bootstrap idempotency path, with no `Connection: close`.

## 2026-08-28 — ACK-12-FU-DESTINATION-ROW: the transit-vs-settle decision moves onto the correlation key's bus half; destination rows are non-settleable

**Context.** ACK-12-FU-DESTINATION-ROW makes `hub.recordAcceptance` write one ack lifecycle row per recipient at RELAYED ingest, so that transit-ack authorisation is bounded by `ack.Retention` (24h) rather than by the byte/age pruning that drops the relayed message body first. That change interacts with a load-bearing assumption in the ack-settle path that was true only while intermediates held no row.

**The signal that was erased.** Before this task, an intermediate or terminal bus held NO durable ack row for a relayed message — only the ORIGIN bus did. So `ack.Store.Settle(correlationKey, recipient, ...)` answering `ErrNoRecord` WAS the transit signal: a miss meant "this bus did not originate the key, carry the outcome one hop further back". Both the agent surface (`hub.AcknowledgeDelivery`) and the peer surface (`cmd/agent-bus/relaywiring.go settleAck`) discovered transit that way — settle first, and on `ErrNoRecord` divert to the forward path. ACK-12-FU-DESTINATION-ROW writes destination rows on intermediates too, so `Settle` now SUCCEEDS on an intermediate and would settle the outcome LOCALLY, stranding the origin's own row non-terminal and breaking back-propagation. The `ErrNoRecord` signal is gone.

**Decision.** The transit-vs-settle decision moves UP FRONT, off the correlation key's bus half, BEFORE `Settle` is ever called, on BOTH surfaces. A key whose bus half is NOT this bus (`ids.ParseMessageID(key)` origin bus, compared case-insensitively against `f.busID` / `h.busID`) is a FOREIGN-ORIGIN key: it is authorised — primarily off the destination lifecycle row for `(key, recipient)`, with retained relay provenance as a fallback — and FORWARDED one hop back toward the origin as a transit acknowledgement (`RecipientAckResult.Transit` on the agent surface; `disposeUnrecordedAck` on the peer surface). It is NEVER settled locally. A LOCAL-ORIGIN key (bus half == this bus) reaches `Settle` and settles the sender-visible row exactly as before, and never reaches the transit path. The two paths are DISJOINT BY CONSTRUCTION: a message is either relayed or locally originated, the bus half decides which, and no row is ever both settled locally and forwarded. See `internal/hub/ack.go:110-140` (agent surface, `AcknowledgeDelivery` reordering and `transitAck`) and `cmd/agent-bus/relaywiring.go settleAck` (peer surface divert at the foreign-origin check before the advisory read/`Settle`).

**Why destination rows are non-settleable.** A destination/intermediate row exists to bound transit-ack authorisation by `ack.Retention`, not to become a sender-visible terminal outcome — §13.3 authorises only the ORIGINAL sender to read a row, and the original sender's row lives on the origin bus. Settling the destination row would record a terminal state nobody can read and would consume the transit signal. Only the origin settles.

**Why not option (b) — "settle if a local row exists".** That was considered and rejected: intermediates now ALSO hold rows, so "a row exists for this key" no longer distinguishes an intermediate from the origin. The bus half is the only durable discriminator that does (invariants 1 and 2 — the server-minted origin bus id is embedded in the correlation key and is fully qualified).

**Invariant 4 is satisfied by the synchronous chain, not by a local write.** On the transit arm nothing durable changes on this bus. The recipient is not answered `accepted` until the forward hop has answered, and each hop answers only after ITS next hop, so the guarantee holds end to end through the chain to the origin's own fsync. This is why the transit arm needs no retry queue and no local spool. There is ONE documented arm that falsifies the end-to-end `accepted` reading: an intermediate ABSORBS a 409 from the hop above and answers its downstream 200, so on a chain of two or more backward hops a recipient can be told `accepted` for an outcome the origin FINALLY refused with 409 ("no obligation binds that recipient", nothing durable anywhere). That absorb is deliberate and was recorded for ACK-5 (2026-08-21): re-offering a finally-refused frame is the retry amplification §9.3 exists to stop, and forwarding the 409 verbatim would make the hop an oracle for whether the origin holds a row for a named recipient — the uniform-answer property `ErrAckNotBound` protects. Every OTHER final upstream status (404, 403, 400) means the upstream decided nothing and is answered "not now" (503). Nobody is disconnected on any arm (§12, invariant 10).

**Guards updated.** `TestSettleAckDisposition` and `TestSettleAckCorrelatesToTheDurableRecord` (`cmd/agent-bus/acktransit_test.go`, `cmd/agent-bus/ackwiring_ack3_test.go`) encoded the pre-ACK-12 `ErrNoRecord` semantics: a foreign-origin key with a row settled LOCALLY and was not forwarded. Those foreign-origin assertions are updated to the new truth (authorise + forward as transit, not settle-local). The safety property the guards protect — "a row is the sole authority; a settled row must NOT ALSO be forwarded (no settle-AND-forward double action), and the disposition must not move above the settle" — is PRESERVED by re-asserting it for a LOCAL-ORIGIN key, which still reaches `Settle` and must settle-locally-not-forward exactly as before.
