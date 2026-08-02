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
