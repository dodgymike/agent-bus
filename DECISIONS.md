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

---

## 2026-08-07 — ENROL-SHAPE: the final `/v1/enroll` shape and `auth.RosterEntry` field set

Settles the P0 that blocked `AUTH-3`, `INVITE-STORE`, `INVITE-GATE`, `MTLS-BIND` and
`AUTH-1-FU-POPKEY`. **Deliverable is this entry only** — `CONTRACTS-HTTP.md` documents SHIPPED
behaviour and none of this has shipped.

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
`/mnt/sdb4/mike/mike` and the snap's confinement does not resolve through it. Going straight at the
already-running daemon over its Unix socket sidesteps that entirely — the CLI's per-user data
directory is only needed for `docker context`/config bookkeeping the socket path bypasses.

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

### 4. The scan runs on EVERY start, to cross-check a rewound floors file — and it RAISES, not just reports

**Decision.** The WAL derivation runs even when `agent-suffixes` exists. When the file exists it is
authoritative and no backfill happens, but a WAL suffix ABOVE the persisted floor for a name is a
detectable INTEGRITY FAILURE — it cannot happen on a healthy dir, because the floor is written ahead
of the suffix, so it means the floors file was rewound, restored from an older backup, or replaced.
That is logged at **ERROR**, naming the file, the name, the persisted floor and the suffix found.

**And the floor is RAISED, not merely reported.** Detection alone would leave the bus knowingly
re-minting an id it can see on disk, which is the one outcome this whole area exists to prevent.
`RaiseFloor` never lowers a floor, so folding the finding in cannot weaken the persisted authority,
and `Seal` writes the merged map back. The posture "the floors file is authoritative" is unchanged:
the WAL is never allowed to lower a floor, only to reveal that the file is missing one.

**The cost, stated honestly.** `wal.ScanAll` materialises every record in memory and the WAL never
compacts, so this is one extra sequential read plus a peak proportional to log size, on every start.
It is affordable now and it is not affordable forever; a streaming scan seam in `internal/wal` is
filed as a follow-up rather than left to be discovered under a large log.

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

## 2026-08-07 — SIGN-2/SIGN-6: the signing core lands; the mandatory-signature policy is BLOCKED

**Context.** SIGN-2 asked for the Ed25519 sign/verify primitive; SIGN-6 asked for the policy that
makes a message's signature mandatory rather than advisory. Landing `internal/signing` (pure
delegation to `crypto/ed25519`, per invariant 9) over `internal/signing.Canonicalize`'s bytes forced
four choices that the next agent must not have to re-derive from scratch.

**Decision 1 — a rejected send must not consume a sequence number; validation happens BEFORE
minting.** Consuming a sequence on rejection would make every recipient cursor gap-tolerant, and once
gaps are normal a recipient can no longer tell a DROPPED message from a REJECTED one — which silently
destroys the only end-to-end signal that a bus on the path is withholding traffic, the one thing
SIGN-1's option (a) was bought with a round trip to preserve. Gaps also cost SIGN-4 its simplest and
strongest rule (strictly-increasing). The existing code already agrees with this and says so:
`internal/hub/hub.go`'s `publish()` checks the idempotency admission BEFORE `h.seq.Next()` with the
comment "Checked BEFORE the sequence is minted: a sequence spent on a send that will be refused is a
sequence burned for nothing, and invariant 1 forbids reusing it." So the SIGN-6 checks (signature
present; exactly 64 bytes; claimed sender == the AUTHENTICATED caller) belong at the same place or
earlier — in `internal/httpapi`, before `hub.Send`/`hub.Broadcast` is called at all. Consequence,
stated plainly: a rejected send leaves NO WAL record, NO audit entry beyond a rejection event, NO
delivery, NO ack, and NO sequence — the mirror image of invariant 4. SIGN-4's cursor is therefore
strictly increasing with no gap tolerance required.

**Decision 2 — the poison-message wedge: the cursor ADVANCES past an unverifiable message; the body
is DISCARDED, never delivered; the event is RECORDED.** The alternative — blocking the cursor until
verification succeeds — hands anyone who can get one bad message into an agent's stream a PERMANENT
denial of service against that agent, for the price of a single message. The asymmetry that makes
this the only defensible choice: the message was already durably accepted and cannot be un-sent, so
refusing to move past it does not undo it, it only stops everything behind it. Fail-closed applies to
the BODY (it is never handed to the calling agent), not to the CURSOR. And the failure must be LOUD —
log the message id, the sender, and WHICH check failed — because a silently skipped message is
indistinguishable from one that never arrived. `internal/signing`'s distinct error sentinels exist
precisely so "which check failed" is answerable, and the security gate confirmed the taxonomy is not
an oracle (`ErrVerify` is one opaque verdict for every cryptographic failure — it does not, by itself,
tell an attacker which byte of a forgery was wrong).

**Decision 3 — which key verifies.** `Verify()` takes the public key as a free parameter and cannot
check the caller chose it correctly, so PROTOCOL.md §8.3's rule is binding on callers: the key MUST be
resolved from the roster using the fully-qualified sender field INSIDE the signed bytes, and nothing
else — never a key or key-hint carried beside the signature. Get it wrong and verification is
self-signed and worth nothing while every test still passes.

**Decision 4 — why SIGN-2 and SIGN-6 are not done, three blockers, recorded so nobody re-derives
them.**
(a) No messaging keypair exists. `internal/auth/service.go` leaves `RosterEntry.MessagingPublicKey`
ZERO (CRYPTO-3, todo) and there is no `agent-bus keygen` (SIGN-8, todo). Nothing can sign; nothing can
verify.
(b) The durable mint does not exist. SIGN-1 chose option (a) — the sender signs the ORIGIN's minted
message id and sequence — so the bus must hand out an id/seq BEFORE the send, and that hand-out must
be DURABLE. Today `internal/hub`'s `Open()` derives the restart floor as `NextIndex-1` from the WAL
high-water index, resting on an explicit counting argument that every sequence issued is <= the WAL
index of the prepare that carried it. A mint that RETURNS a sequence without writing a durable record
breaks that argument outright: mint, restart, and the floor resumes below numbers already handed
out — so two validly-signed messages would share one origin message id, which is undetectable
downstream. The good news for whoever fixes it: `wal.Log` already exposes
`Begin`/`Txn.Commit`/`Txn.Abort`, and `CONTRACTS-ONDISK.md` records that `Entry.Kind` is NOT a
reserved namespace, so a durable reservation can be built without minting a new reserved record-type
number.
(c) The signature cannot reach the durable record without `internal/hub`. The path is
`httpapi -> hub.SendRequest/BroadcastRequest -> hub.publish -> store.NewMessage -> WAL`, and the
middle of that is `internal/hub`, which was outside this agent's file-ownership boundary.

**Warning to the next agent.** Shipping SIGN-6's ingest check ALONE — requiring a signature the bus
never persists and never returns on `/v1/wait` — would be exactly the "theatre" SIGN-6 warns against,
since senders would pay the cost and recipients would still have nothing to verify. It must land
together with the durable carry and the receive-path field.

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
  sequence. Holes are legal and permanent (invariant 1 beats gap-freeness) — the same trade
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
after damage deeper than a torn tail" (line ~1541).** `DECISIONS.md` is append-only, so the
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
  sender's messaging public key **only OUT OF BAND**. `client/keyring.go`'s `DirKeyRing` is a local,
  **manually populated** trust store and is explicitly a stopgap. **No fallback may be invented** —
  no TOFU, no "trust the key the bus handed over", no verification-optional switch, no `--insecure`.
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
