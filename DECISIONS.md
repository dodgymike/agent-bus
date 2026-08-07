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

---

## 2026-08-02 — The message sequence high-water mark lives in the WAL message body, read via a replay-time PREPARE observer (ID-2-WIRING-SCHEMA)

**Context.** `ids.Resume(floor)` requires the highest sequence EVER WRITTEN TO DISK — committed,
aborted **and dangling** — because a fsynced PREPARE burns its number forever even if the process
dies before the COMMIT (`internal/ids/sequence.go:87-122`). Nothing in the durability layer can
supply that value today:

- the sequence lives inside the caller-written `wal.Entry.Body`, and `wal` deliberately does not
  interpret `Kind` or `Body` (`internal/wal/log.go:72-73`);
- `wal.Replay` hands its callback **committed** entries only;
- `wal.Recovered` carries no message-sequence high-water mark at all. `Recovered.NextIndex` is the
  WAL **record** index (`replay.go:399-403`) — incremented by commits and aborts too, shared by every
  record type. It is a different counter and the two are not interchangeable.

So the floor could not be derived until it was decided WHERE the number lives. That is this decision.
`ID2_WIRING_DEEPDIVE.md` §3.5/§4.2/§4.4 ranked the options and prototyped each; this entry settles
the fork it left open.

**Decision.** **Option A′.**

1. **The message sequence is a top-level, cleartext field of the WAL message body**:
   for `wal.Entry{Kind: "message"}`, `Body` is a JSON **object** carrying `"seq": <uint64, non-zero>`
   at the top level, readable by the server without any key. This is the ONLY field ID-2-WIRING
   commits the MSG epic to; everything else about the message body — sender, recipients, timestamp,
   content hash, idempotency key, signature, any future envelope — stays open for MSG/SIGN/IDEM to
   design.
2. **The WAL offers every PREPARE to an observer during the replay pass that already happens**, and
   the **application** decodes `seq` out of the body. `wal` still does not interpret `Body`; the
   decode lives in the caller's callback, so `log.go:72-73`'s promise survives intact.
3. **No on-disk format change. No `ondisk-format-version` value is consumed by this decision.**

**Why A′ — the evidence, not the aesthetics.**

- **`Replay` already decodes every PREPARE.** `internal/wal/replay.go:129-131` calls `DecodePrepare`
  on every PREPARE record — committed, aborted and dangling alike — because a prepare payload that
  does not decode means the file no longer says what it recorded. The decoded `Entry` is then thrown
  away unless the entry later commits. A′ hands that already-materialised value to the application
  instead of discarding it. The floor therefore costs **zero additional passes and zero additional
  decodes**.
- **It removes a third startup scan before it is ever added.** Option A (a caller-side `ScanAll` +
  body decode) yields the identical number but adds a full third scan of the WAL at startup, directly
  aggravating the already-filed `2a961fcc` — *"Startup scans the WAL twice (soon three times) — bound
  the cost"*. That task's "(soon three times)" anticipated exactly this. A′ makes it moot.
- **The deepdive proved it yields the right floor from a dangling prepare in one pass**
  (`ID2_WIRING_DEEPDIVE.md` §4.2, `one-pass floor = 100 (records=100 applied=99 dangling=1)`,
  `proof-check: verdict=PASS`), at a measured cost of ~16 lines in `replay.go` with
  `./internal/wal` staying fully green.

**The §4.4 disproof test — RUN, and it does NOT fire.**

The deepdive made its recommendation conditional and named the test that would overturn it:

> *"if a CRYPTO task specifies that `wal.Entry.Body` for `Kind=="message"` is a bare opaque blob
> rather than an envelope, A′ is disproven and B wins."*

The deep-diver explicitly did not read the CRYPTO epic task bodies and flagged that as a bounded gap
(§0). This task read all of them. **No such task exists, and the live ones say the opposite:**

- Every task that would have made the body opaque — CRYPTO-5 (X3DH), CRYPTO-6 (Double Ratchet on the
  DM path), CRYPTO-7 (ratchet durability), CRYPTO-8 (broadcast AEAD), CRYPTO-9 (encrypted relay) — is
  **`deferred`**.
- **CRYPTO-11** (`todo`): *"there is no ciphertext — bodies travel in cleartext with a detached
  Ed25519 signature"*, and the plaintext-vs-ciphertext question *"is now moot and must not be
  re-opened"*.
- **CRYPTO-12** (`todo`): *"State PLAINLY in PROTOCOL.md that message bodies are NOT encrypted — any
  relaying/intermediate bus and any party with WAL/disk access can read every message body; this is a
  deliberate, user-approved property, not an oversight."*
- This is not merely a backlog state, it is **already a recorded decision**: *"2026-08-02 — Message
  auth/integrity only (libsodium-style signatures); encryption deferred"* above, on direct user
  instruction — *"No encryption, no X3DH, no Double Ratchet, no forward secrecy for now. The ratchet
  direction in `CRYPTO_DEEPDIVE.md` is superseded — its recommendation must not be actioned."*

**And the test does not fire even in the world where encryption returns.** Three independent reasons,
which is what makes A′ safe rather than merely currently-convenient:

1. **Even the superseded maximal-encryption design kept an envelope.** `CRYPTO_DEEPDIVE.md:725`
   describes the change as *"changes the shape of every message body from plaintext to
   `{ciphertext, ratchet_header, …}`"* — a JSON object with named fields, with the ciphertext confined
   to one of them. That is an envelope, not a bare blob.
2. **A bare opaque blob is not a representable value at the WAL layer.** `Entry.Body` MUST be valid
   JSON — `log.go:80-87` documents it, `canonicalBody` (`log.go:555-573`) enforces it via
   `json.Compact`, and `Write` rejects invalid JSON with `ErrInvalidBody`. Ciphertext can only ever
   reach the WAL as a *field inside a JSON object*, which is precisely the shape A′ needs.
3. **Two other epics independently require the server to read `seq` out of the body.** DUR-5's audit
   record is *"message id, **sequence**, sender, recipient(s), bus path traversed, timestamp, size,
   and a content hash"* — the server cannot write that record without reading the sequence. SIGN-4's
   recipient cursor is built on the same server-minted monotonic sequence. And per invariant 1 the
   server MINTS the sequence in the first place. A world in which the bus cannot read `seq` out of its
   own durable record is self-contradictory: DUR-5 and SIGN-4 would break in it too. **The sequence is
   routing metadata, not content** — the same line invariant 6 already draws for the audit log.

**Options rejected, and why.**

- **Option B — promote the sequence to a WAL-level field** (`Entry.Seq` / `preparePayload.Seq`,
  `Recovered.HighestSequence`). Architecturally the cleanest: `wal` would compute the floor itself and
  no caller could get it wrong. **Rejected on price, not on principle.** It is an **on-disk format
  change** and the deepdive proved the break rather than assuming it: `log.go:671` calls
  `dec.DisallowUnknownFields()`, so an older binary meeting a newer log fails with
  `prepare payload does not decode: json: unknown field "seq"` and **refuses to start**. Fail-safe,
  but a hard downgrade break — and the on-disk `FormatVersion` would still say `1`, so the operator
  gets `corrupt at offset 16` instead of an honest version mismatch. B therefore also requires a
  `FormatVersion` bump, which makes **every existing WAL unreadable** (`format.go:328-329` refuses a
  mismatch outright). That buys a reserved version number, a migration story and a downgrade break in
  exchange for moving a correctness obligation out of a reviewed ~10-line callback — an obligation
  that is *already* visible and enforced by the seal gate (ID-2-WIRING-SEAL), which makes the floor
  claim a greppable `seq.Seal()` line at exactly the point a reviewer must check the derivation.
  **B also acquires a layering change** — `wal` would gain knowledge of a message sequence — which is
  the thing `log.go:72-73` was written to prevent.
- **Option A — caller-side `ScanAll` + decode the body.** Correct, and proven to yield the same floor,
  but it is the most expensive way to get an answer that a pass we already run is holding in a local
  variable. Rejected as strictly dominated by A′.
- **Option C — make the floor an acceptance criterion of the first MSG task that writes a message.**
  Not a mechanism, a schedule; kept as a *complement* to A′, not a substitute. On its own it is a
  promise that gets lost, and the MSG-2 implementer then writes the natural, wrong, `go vet`-clean
  `Replay`-fold. With the seal gate landed it becomes a mechanism, because minting an id without
  writing `seq.Seal()` is impossible.

**Ordering note — no version number was taken.** `ondisk-format-version` currently holds `1` (the
shipped format) and `2` (**already reserved by DUR-12**, the CRC32C → HMAC-SHA256 keyed MAC). A′
consumes neither. If encryption is ever revived and B becomes necessary, it must **reserve its own
value at that time** — format changes are ORDERED and two changes must never share one version
number. Never hand-pick it.

**Consequences.**

- **`wal` gains an observer hook, not a schema.** ID-2-WIRING-OBSERVER implements it; the shape the
  deepdive prototyped and this decision assumes is: `Replay` keeps its exact signature and delegates
  to a new `ReplayWithPrepares(path, fn func(Committed) error, onPrepare func(Entry) error)`, where
  `onPrepare` is called once for **every** PREPARE record in file order regardless of how it later
  resolves, a nil `onPrepare` reproduces today's behaviour exactly, and an error from `onPrepare`
  fails the replay the same way an error from `fn` does. `wal` passes the `Entry` through without
  interpreting it.
- **The derivation must ERROR, never return 0, on failure.** `ids.Resume(0)` is documented as exactly
  equivalent to `NewSequence` (`sequence.go:159-160`), so a floor of `0` derived from an *empty* log
  and a floor of `0` derived from a *failed* decode are indistinguishable at the type level. `Seal()`
  does not fix that. The startup derivation must refuse to start on a scan or decode error rather than
  hand `Resume` a zero — this is a required behaviour of ID-2-WIRING-STARTUP, not a nicety.
- **A non-numeric, missing or zero `"seq"` in a message-kind prepare body is a startup failure**, for
  the same reason: silently skipping an undecodable body would lower the floor exactly when it must
  not move.
- **Contract text this implies** (owned by whoever holds `CONTRACTS*.md` / `PROTOCOL.md`, filed
  separately — this task wrote no contract file): *the WAL body of a `"message"`-kind entry is a JSON
  object carrying a top-level `"seq"` (uint64, non-zero), the server-minted message sequence; it is
  cleartext and readable by the bus; the message-sequence high-water mark is derived at startup from
  every PREPARE in the log, committed, aborted and dangling alike.*
- **Residual, shared by A′ and B alike, and therefore not a differentiator:** a whole-log
  **quarantine** renames the unreadable log aside and starts a fresh one at index 1
  (`recover.go:252-262`), and a repaired tail discards records declared never-durable. Sequences
  burned in bytes that are no longer replayed cannot be recovered from the WAL by *any* option here —
  B would compute its `HighestSequence` in the same pass over the same surviving bytes. This is the
  message-sequence face of the invariant-1 narrowing already filed for the WAL record index
  (`e120153b`), and it belongs on that task's docket rather than in a second one.
- **This decision commits the MSG epic to exactly one field and nothing else.** It is deliberately the
  smallest schema commitment that unblocks the floor derivation.


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

## 2026-08-02 — Addendum to ID-2-WIRING-SCHEMA: the quarantine residual is a DEFECT, not a narrowing

**Why this is a separate section.** The ID-2-WIRING-SCHEMA entry above and the "Five decisions" entry
below it were written independently and landed in the same commit (`4110946`). Decision 3 there —
*"NO id reuse — invariant 1 stands, and the salvage reissue is a DEFECT"* — supersedes the FRAMING of
one paragraph above. `DECISIONS.md` is append-only, so the correction is recorded here rather than by
editing a committed line.

**What changes.** The A′ entry's residual paragraph describes the quarantine/discard case as *"the
message-sequence face of the invariant-1 narrowing already filed for the WAL record index
(`e120153b`)"*, and parks it on that docket. That is now wrong in kind. The user reaffirmed invariant 1
**without narrowing**: *"Recovery may not reissue an index it has already handed out, even for a record
it discards: when recovery discards a record the sequence advances past the hole, it never rewinds"*,
and *"this makes the current salvage behaviour a bug, not a documented narrowing."* So it is a defect
to be fixed, not a limitation to be documented and accepted.

**What does NOT change: the choice.** Option A′ still stands, for exactly the reasons given above. The
residual was explicitly recorded as shared by A′ and B alike and therefore not a differentiator —
Option B would compute its `Recovered.HighestSequence` in the same pass over the same surviving bytes
and would lose precisely the same sequences. Reclassifying it from "narrowing" to "defect" changes who
must fix it and how urgently; it does not change where the number lives.

**What this sharpens, and it is worse for the sequence than for the record index.** Whole-log
quarantine renames the unreadable log aside and starts a **fresh log at index 1**
(`internal/wal/recover.go:252-262`). Under "no id reuse" that is not a subtle reissue of one damaged
tail record — it re-hands-out the entire index space from 1. The message-sequence face is the same and
is not fixed by anything in the A′ entry: after a quarantine there is no surviving PREPARE for the
observer to see, so the derived floor is `0`, and a bus that then starts minting sequences from 1 would
reissue every sequence it has ever used.

**Therefore, a required behaviour of ID-2-WIRING-STARTUP** (stated here so it is not rediscovered
later): a quarantine event must not be allowed to silently yield floor `0`. The floor derivation must
distinguish "the log is legitimately empty because this bus is new" from "the log was quarantined and
its high-water mark is unknown", and in the second case it must refuse to start rather than resume from
zero — the same fail-closed rule the entry above already imposes for a scan or decode error, and the
same rule `internal/ids/sequence.go:119-122` states: *a caller that cannot prove its floor MUST refuse
to start rather than guess.* Where the surviving high-water mark is recorded so a quarantined bus can
still prove its floor is a durability-plane question that belongs with the quarantine defect, not with
this schema decision.


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

### Operator-supplied certificates ARE allowed

`-tls-cert` / `-tls-key` flags, alongside the self-signed default. This is a legitimate need for
anyone with existing PKI, and it weakens nothing: self-signed-plus-pinning remains the default, and
the fingerprint-in-invite mechanism is indifferent to who issued the certificate — the client pins
whatever the bus actually serves.

### Known follow-on work

`scripts/bus-serve.sh:54` (`HEALTH_URL="http://${LISTEN}/healthz"`), `CLI-2`'s `proof_cmd` (enrols
over `http://127.0.0.1:8092`), and DEPLOY-1/DEPLOY-2 all assume plaintext and must be updated as the
mTLS epic lands.

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

### 3. `--invite` is REJECTED locally, not guessed onto the wire

Enrolment is becoming invite-only (invariant 3) and the CLI needs the seam now. But the invite's
wire shape is settled by task `ENROL-SHAPE`, and `/v1/enroll` is explicitly UNSTABLE until that,
certificate binding and POPKEY all land.

So `busctl enrol --invite <blob>` fails immediately, locally, with a remedial message naming
`ENROL-SHAPE` — it does **not** invent a JSON field called `invite` and send it. Choosing a wire
field name ahead of the task that owns it is the same mistake as hand-picking an on-disk
record-type number: **the shape is reserved, not chosen.** The seam is `client.EnrolOptions.Invite`
plus one construction site in `Enrol`; when ENROL-SHAPE settles, the field name lands in one place.

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
`7` rejected, `8` nothing-to-report), enumerated in `CONTRACTS-CLI.md`. `2` is usage to match Go's
`flag` package and `cmd/agent-bus`.

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

### 4. Message ids may repeat after a WAL QUARANTINE, and after damage deeper than a torn tail

The sequence floor is derived from the durable log's own high-water index
(`wal.Recovered.NextIndex - 1`). The argument that this bounds every sequence is by counting: each
message burns ONE sequence and at least TWO WAL indices, so the indices outrun the sequences and the
gap only widens. `hub.publish` ASSERTS it per message (`PrepareIndex >= seq`) and poisons the hub if
it ever fails, rather than trusting the counting argument to survive future edits.

The argument depends on the index high-water mark surviving, and there are two cases where it does
not:

- **Quarantine.** Recovery moves an unreadable log aside and starts a FRESH one whose index restarts
  near 1, while the quarantined file holds sequences far above it.
- **Damage deeper than a torn tail.** Measured, not assumed: over a 585-offset truncation sweep of a
  2523-byte WAL, 70 offsets regressed the sequence — every cut losing more than half the records.
  Inside the genuine crash window (a tear between two fsyncs) the property HOLDS and is asserted.

This narrows invariant 1, which says ids are never reused including across restarts, and the
narrowing needs to be recorded rather than implied by invariant 6's availability decision — that
decision is about DISCARDING RECORDS and says nothing about REUSING IDS. The trade is the same one
invariant 6 made: a bus held hostage by one bad sector is worse than a bus that has lost something
and said so. So the bus starts, and `hub.Open` reports the exposure at ERROR naming the quarantine
path and the resumed floor. **Silence would be the defect; the discard is not.**

The real fix is a separately-persisted, fsynced sequence high-water mark written ahead of the
sequence it authorises — `MSG-FU-SEQHIGHWATER`, which also needs a RESERVED on-disk record-type
number. Until it lands, an operator who sees the quarantine line should expect repeated message ids
and treat the quarantined file as the only record of what came before.

---

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
- Below the pressure line, admission is first-come first-served with no reclamation: an agent that
  grew its holding during the free-growth phase keeps that outsized allocation even after the bus
  crosses into pressure, because the share only ever REFUSES new admissions, it never claws back what
  is already held.

Not deployed: this is a code-only change to `internal/idem` and `internal/hub`, uncommitted at the
time this decision was recorded.
