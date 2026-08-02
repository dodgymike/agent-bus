# CRYPTO deep dive — Signal-style end-to-end crypto for agent-bus

**Task:** CRYPTO-1 (`public_id 30570fb9-c5dd-40cd-b0f7-858a250d23b7`, P1, project `agent-bus`)
**Date:** 2026-08-02 · **Author:** deep-diver (`claude-opus-5`) · **Status:** DESIGN SPIKE — investigation only, no production code
**Scope of this file:** the whole deliverable. Nothing was written to `internal/`, `cmd/`, `scripts/`,
`DECISIONS.md`, `CONTRACTS.md`, `SPEC.md` or the Spec Server task graph. All prototypes ran in
throwaway dirs (`/tmp/.../scratchpad/*` and `/mnt/sdb4/mike/tmp-crypto1-spike`, both outside the repo;
the root filesystem is 100% full with 427 MB free, which is why the large toolchain spike went to
`/mnt/sdb4`).

---

## 1. Executive summary (read this, then jump to §6 to decide)

1. **The literal ask is not achievable.** Signal's own library (`github.com/signalapp/libsignal`) is
   Rust with Java/Swift/TS bindings and **has no Go binding**. There is no "the ratchet library the
   Signal people made" for Go. Everything below is a closest-alternative.
2. **This box has full network access** (`proxy.golang.org` 200 in 0.13 s; `go list -m -versions
   golang.org/x/crypto` returned 20+ versions). Fetching modules is *not* a constraint. Good news —
   the premise that it might be is disproved.
3. **`go1.19.4` is the blocker, not the network.** Latest `golang.org/x/crypto` **cannot compile on
   go1.19.4**: `curve25519` now wraps `crypto/ecdh` (go1.20+) and `hkdf` wraps `crypto/hkdf`
   (go1.24+). Bisected: **v0.24.0 is the last version that builds on go1.19.4**; v0.25.0 fails.
4. **Pinning x/crypto at v0.24.0 freezes us ~2 years behind** (v0.24.0 = Jun 2024; today 2026-08-02)
   with two published advisories already fixed above it (GO-2024-3321 → 0.31.0, GO-2025-3487 → 0.35.0;
   both `x/crypto/ssh`-only, so not exploitable for us, but the module goes permanently stale).
5. **The toolchain bump is safe and was proved, not assumed.** I downloaded go1.25.12 into a throwaway
   dir (no sudo, no system install) and ran the *existing repo at HEAD* under it: `go build`, `go vet`,
   `gofmt -l` and **`go test -race ./...` are all green under both go1.19.4 and go1.25.12**.
6. **No Rust toolchain exists here** (`rustc`/`cargo`: command not found; `sudo` needs a password).
   CGO-against-libsignal is not viable and should be struck from the menu.
7. **The best available Go option is `go.mau.fi/libsignal`** (mautrix's maintained fork of the
   libsignal-protocol-go port; X3DH + Double Ratchet + Sender Keys + fingerprints). Proved: passes its
   own suite with `-race` on go1.25.12, **rejects replays**, bounds skipped keys at 2000. It requires
   go ≥ 1.23.
8. **The go1.19-compatible alternative is materially worse and I recommend against it.**
   `status-im/doubleratchet` (last release **2019-10-31**, self-described *beta*, no mutex anywhere)
   **accepts replayed ciphertexts by default** — I reproduced unlimited replay in four configurations.
9. **The durability tension has been mis-framed and this is the biggest finding.** In a *true* E2E
   design the **bus never holds ratchet state at all** — it holds opaque ciphertext. Ratchet state
   lives in the agent-side helper. Invariants 4/5/6 are therefore **not** weakened by ratchet
   mutability. `CRYPTO-7` as written assumes the opposite and must be split.
10. **Crypto is free; fsync is the budget.** Measured on this box: ratchet seal+open of 1 KiB = **46 µs**;
    one fsync = **1.12 ms**; two fsyncs = **2.49 ms**. The ratchet costs ~2% of a single fsync, so the
    only performance decision that matters is *"one fsync per message, not two"*.

**My four headline recommendations:** bump the toolchain to go1.25.x (§6.1 Option A) · adopt
`go.mau.fi/libsignal` behind a narrow `internal/cryptobox` (§6.1) · E2E-opaque audit log (§6.2
Option a) · server-attested bundles **plus** mandatory TOFU pinning with safety numbers (§6.5
Option c). Six things need explicit user consent before any CRYPTO-2..12 work starts — §9.

---

## 2. Premise checks (commands run, output observed)

Every claim in this document that could have been an assumption was executed. Bounds on coverage are
stated in §11.

### 2.1 Go toolchain on this box

```
$ go version
go version go1.19.4 linux/amd64
$ which -a go
/usr/local/bin/go            # -> GOROOT /usr/local/go ; the ONLY installed Go
$ ls -d /usr/lib/go* /usr/local/go* /opt/go*
/usr/local/go                # no second toolchain installed
```

**Can it be bumped?** Yes, three ways, one of them proved:

| path | evidence | needs sudo? |
|---|---|---|
| `apt install golang-1.22/1.23/1.24` | `apt-cache policy` shows candidates 1.22.2, 1.23.1, **1.24.4** | yes — `sudo -n true` → *"a password is required"*, so **not** non-interactively |
| tarball from go.dev/dl into a user dir | **PROVED**, see below | **no** |
| `GOTOOLCHAIN` auto-download | implicitly proved (the spike pulled `golang.org/toolchain@v0.0.1-go1.26.3` for a test run) | no |

```
$ curl -s 'https://go.dev/dl/?mode=json' | ...   -> ['go1.26.5', 'go1.25.12']
$ curl -sSL -o go.tgz https://go.dev/dl/go1.25.12.linux-amd64.tar.gz && tar -xzf go.tgz
$ /mnt/sdb4/mike/tmp-crypto1-spike/go/bin/go version
go version go1.25.12 linux/amd64
```

**And the existing repo is clean under it** (run on a pristine `git archive HEAD` copy at `bd2e338`,
so concurrent in-flight edits by the ID/DUR feature-runner could not confound the result):

```
go1.25.12:  gofmt -l . -> (empty)
go1.25.12:  go test -race ./...
  ok  cmd/agent-bus 1.021s | ok internal/httpapi 1.078s | ok internal/ids 1.034s
  ok  internal/logging 1.016s | ok internal/wal 1.300s
go1.19.4 (baseline, same copy):  all ok
```

> **Verdict:** the go1.19 pin is a *choice*, not a constraint. Bumping it costs nothing in existing
> code and is reversible (`go.mod` one-liner).

### 2.2 Network access for Go modules — YES

```
$ go list -m -versions golang.org/x/crypto      # in a /tmp scratch module
... v0.44.0 v0.45.0 ... v0.53.0 v0.54.0         # exit 0
$ curl -o /dev/null -w '%{http_code} %{time_total}' https://proxy.golang.org/golang.org/x/crypto/@v/list
200 0.128123s
github.com 200 · archive.ubuntu.com 200 · go.dev/dl 200 · sh.rustup.rs 200
```

The "modules may be unfetchable" premise is **disproved**. `go.mod` having zero dependencies is a
deliberate invariant-8 posture, not an environmental limit.

### 2.3 Rust / CGO toolchain — NO Rust, yes C

```
$ rustc --version   -> bash: rustc: command not found
$ cargo --version   -> bash: cargo: command not found
$ gcc --version     -> gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0
$ go env CGO_ENABLED -> 1
$ apt-cache policy rustc -> Candidate: 1.75.0+dfsg0ubuntu1-0ubuntu7.4  (needs sudo, password-gated)
```

CGO works, but there is no Rust. A user-local `rustup` install is *possible* (sh.rustup.rs is
reachable, no sudo needed), and the distro's 1.75 is likely below libsignal's MSRV anyway.
**Recommendation: strike CGO-against-libsignal from the menu** — it drags a Rust toolchain into every
build and CI runner, breaks `CGO_ENABLED=0` static cross-compilation, and buys us a library whose Go
FFI surface we would have to write and maintain ourselves.

### 2.4 What go1.19.4's stdlib actually gives you

```
$ ls $(go env GOROOT)/src/crypto/
aes  boring  cipher  des  dsa  ecdsa  ed25519  elliptic  hmac  internal  md5
rand  rc4  rsa  sha1  sha256  sha512  subtle  tls  x509
$ ls $(go env GOROOT)/src/crypto/ecdh   -> No such file or directory
$ ls $(go env GOROOT)/src/crypto/hkdf   -> No such file or directory
```

| primitive | go1.19.4 stdlib? | note |
|---|---|---|
| Ed25519 sign/verify | **YES** `crypto/ed25519` | key-bundle attestation, prekey signing |
| AES-256-GCM AEAD | **YES** `crypto/aes` + `crypto/cipher.NewGCM` | a perfectly good AEAD |
| HMAC-SHA-256 | **YES** `crypto/hmac` | |
| HKDF | **NO** — but ~30 LOC over `crypto/hmac` (RFC 5869) | `crypto/hkdf` is go1.24 |
| constant-time compare | **YES** `crypto/subtle` | |
| X25519 / Curve25519 | **NO** | `crypto/ecdh` is go1.20; `x/crypto/curve25519` is third-party |
| ChaCha20-Poly1305 | **NO** for user code | `$GOROOT/src/vendor/golang.org/x/crypto/chacha20poly1305` exists but is **stdlib-internal vendoring**, not importable |
| P-256 ECDH | **YES** via `crypto/elliptic.ScalarMult` | backed by `crypto/internal/nistec` (constant-time in go1.19); **not** marked `Deprecated` at 1.19, but is in later Go |

**The only genuinely missing piece at go1.19 is X25519.** I proved a complete zero-dependency crypto
stack runs on go1.19.4 (`/tmp/.../scratchpad/stdonly`, `go.mod` with **no** `require` lines):

```
P-256 ECDH agree: true shared len: 32
hkdf key len: 32
AES-256-GCM: err=<nil> open_err=<nil> pt="hello" overhead=16
ed25519 verify: true
constant-time cmp: true
$ go vet ./...   -> clean
```

### 2.5 `golang.org/x/crypto` vs go1.19.4 — bisected

```
require v0.14.0 -> BUILDS OK      require v0.22.0 -> BUILDS OK
require v0.17.0 -> BUILDS OK      require v0.23.0 -> BUILDS OK
require v0.21.0 -> BUILDS OK      require v0.24.0 -> BUILDS OK   <-- LAST GOOD
require v0.25.0 -> FAIL: curve25519/curve25519.go:13:8: package crypto/ecdh is not in GOROOT
require v0.54.0 -> FAIL: curve25519.go:16:8 crypto/ecdh + hkdf/hkdf.go:14:2 crypto/hkdf not in GOROOT
```

Advisories above the pin (queried from `vuln.go.dev`):

| ID | fixed in | affected package | exploitable for us? |
|---|---|---|---|
| GO-2024-3321 | v0.31.0 | `x/crypto/ssh` (`ServerConfig.PublicKeyCallback`, …) | **no** — we would not import `ssh` |
| GO-2025-3487 | v0.35.0 | `x/crypto/ssh` (large symbol list) | **no** — same |

So pinning v0.24.0 is not *currently* an exploitable hole. It is a **standing commitment to never take
another x/crypto fix**, in the one module where fixes matter most. That is the argument, and it is
about future risk, not present vulnerability.

---

## 3. What the literal ask maps to

> **User ask, verbatim (2026-08-02):** *"Add to the backlog to add a mechanism to validate messages in
> the agent script before accepting them. enrolment generates a pub/prv keypair for auth, and for
> messaging. Use the messaging ratchet library the signal people made for signal /whatsapp to ensure
> pfs and message integrity / authenticity between agents"*

| the ask | achievable literally? | closest honest alternative |
|---|---|---|
| "validate messages in the agent script before accepting them" | **YES**, exactly | a Go subcommand the wrapper shells out to; §6.6 |
| "enrolment generates a keypair for auth" | **YES** | AUTH-1 already does this |
| "…and for messaging" | **YES** | second, independent keypair; §6.5 |
| "PFS + integrity + authenticity between agents" | **YES** | Double Ratchet gives all three |
| **"the ratchet library the signal people made"** | **NO** | Signal ships Rust (`signalapp/libsignal`). **No Go binding exists.** Best Go option: `go.mau.fi/libsignal`, a *community* port (mautrix fork of `crossle/libsignal-protocol-go`), which is what actually talks to WhatsApp from Go today. It is **not** written or audited by Signal. |

**Say this plainly to the user:** we cannot use Signal's library. We can use a community Go
implementation *of Signal's published protocol*, or implement that published protocol ourselves. Both
are "the Signal protocol"; neither is "Signal's code".

---

## 4. Candidate libraries — measured, not read

### 4.1 `go.mau.fi/libsignal` v0.2.2 (mautrix / tulir fork)

```
$ curl -s https://proxy.golang.org/go.mau.fi/libsignal/@v/v0.2.1.info
{"Version":"v0.2.1","Time":"2025-10-04T17:31:10Z", "URL":"https://github.com/tulir/libsignal-protocol-go.git"}
go.mod: go 1.24.0 / toolchain go1.25.1
require ( filippo.io/edwards25519 v1.1.0 ; golang.org/x/crypto v0.42.0 ; google.golang.org/protobuf v1.36.10 )
```

Version history and Go floor (fetched `@v/*.mod` for each):

| version | date | `go` directive |
|---|---|---|
| v0.1.0 | 2023-01-01 | go 1.18 (x/crypto v0.4.0) |
| v0.1.1 | 2024-07-16 | go 1.18 (**x/crypto v0.25.0** — already past the go1.19 wall) |
| v0.1.2 | 2025-02-12 | go 1.23.0 / toolchain go1.24.0 |
| v0.2.0 | 2025-05-13 | go 1.23.0 |
| v0.2.1 / v0.2.2 | 2025-10-04 / later | go 1.24.0 |

**Under go1.19.4 it does not build, at any version**, because even v0.1.1's MVS floor of x/crypto
v0.25.0 needs `crypto/ecdh`:

```
$ GOROOT=/usr/local/go go build ./...        # requiring go.mau.fi/libsignal v0.1.1
.../golang.org/x/crypto@v0.25.0/curve25519/curve25519.go:13:8: package crypto/ecdh is not in GOROOT
```

**Under go1.25.12 it builds, and its own suite passes with `-race`:**

```
$ go test -race ./...      # go.mau.fi/libsignal v0.2.2, go1.25.12
ok  go.mau.fi/libsignal/tests   9.855s
(all other 28 packages: [no test files])
```

Properties that matter, verified in source and by execution:

- **Replay is rejected by the library.** `session/SessionCipher.go:337-338` does
  `HasMessageKeys(...)` → `RemoveMessageKeys(...)`, i.e. the message key is *consumed*. My test
  (`tests/zz_replay_test.go`, throwaway):
  ```
  delivery 1: pt="first message" err=<nil>
  REPLAY of the SAME ciphertext: pt="" err=failed to get or create message keys:
      received message with old counter (index: 1, count: 0)
  ```
- **Skipped keys are bounded:** `state/record/SessionState.go:16` `const maxMessageKeys int = 2000`,
  enforced at `SessionState.go:389`.
- **Sender Keys (group sessions) are present**: `groups/GroupCipher.go`, `groups/GroupSessionBuilder.go`.
- **Fingerprints/safety numbers are present**: `fingerprint/`.
- **Persistence hooks are interfaces we implement**: `state/store/{SessionStore,MessageKeyStore,
  PreKeyStore,SignedPreKeyStore,IdentityKeyStore,SignalProtocolStore}.go`.
- **Envelope overhead measured** (`tests/zz_size_test.go`, first message = `PreKeySignalMessage`,
  which carries the whole X3DH bundle so it is the worst case):
  ```
  plaintext=     16B -> envelope=    164B (overhead 148B)
  plaintext=   1024B -> envelope=   1174B (overhead 150B)
  plaintext=  65536B -> envelope=  65688B (overhead 152B)
  ```

**Weaknesses, stated honestly:** it is a fork of a fork of an abandoned port; it is **not audited**;
exactly **one** package (`tests/`) carries tests, covering session build, roundtrip, out-of-order,
group, prekey, fingerprint, serializer, saved-message-keys; and it drags `google.golang.org/protobuf`
(a heavy dependency) into a project that currently has **zero**.

### 4.2 `github.com/status-im/doubleratchet` v3.0.0+incompatible

Builds on **go1.19.4** with x/crypto pinned to v0.24.0 — I ran a full session. But:

```
$ curl -s .../@v/v3.0.0+incompatible.info -> {"Time":"2019-10-31T15:13:07Z"}
README: "The library is in beta version and ready for integration into production projects with care."
$ grep -rn 'sync\.' *.go | grep -v _test   -> (no output)   # NO locking anywhere; Session is not
                                                             # safe for concurrent use. In a project
                                                             # whose CLAUDE.md calls a data race P0.
```

**And it accepts replays.** Four configurations, all reproduced (`/tmp/.../scratchpad/dr19e`):

```
A1 first delivery : pt="one" err=<nil>
A2 REPLAY same msg: pt="one" err=<nil>          <-- replay ACCEPTED
B  replay of n1 after n2: pt="one" err=<nil>    <-- replay ACCEPTED
C  replay of q3 (head msg): pt="3" err=<nil>    <-- replay ACCEPTED
C3 REPLAY of q1 (skipped key, already consumed): pt="1" err=<nil>   <-- replay ACCEPTED
```

Root cause, quotable — `session.go:190-198` re-files the *just-used* key into the skipped-key store:

```go
	// Append current key, waiting for confirmation
	skippedKeys := append(skippedKeys1, skippedKeys2...)
	skippedKeys = append(skippedKeys, skippedKey{
		key: sc.DHr, nr: uint(m.Header.N), mk: mk, seq: sc.KeysCount,
	})
```

…and the skipped-key branch (`session.go:131-143`) never deletes after use. Replay protection is
**delegated to the caller** via `Session.DeleteMk`. It works when you call it:

```
first  : pt="one" err=<nil>
DeleteMk err=<nil>
replay after DeleteMk: err=can't skip current chain message keys: bad until: probably an
                            out-of-order message that was deleted     <-- BLOCKED
```

`MaxSkip` (default 1000) *is* enforced: skipping 1200 → `too many messages`.

This is a **landmine for any implementer**: the naive integration is silently replayable, and the
`DeleteMk` call has to be ordered correctly against a durable commit or a crash re-opens the window.
Its `DefaultCrypto` is AES-256-CTR + HMAC-SHA-256 encrypt-then-MAC (`default_crypto.go:108-146`),
which is spec-conformant; it only needs x/crypto for `hkdf` and `curve25519`. MIT licensed.

### 4.3 Ports checked and rejected

`github.com/tiabc/doubleratchet` (empty version list on the proxy), `RTradeLtd/libsignal-protocol-go`
and `signal-golang/libsignal-protocol-go` (`not found` from the proxy — repos gone or private),
`crossle/libsignal-protocol-go` (empty list; it is the upstream that mautrix forked).

### 4.4 Vendoring x/crypto instead of depending on it

If the goal is "no module dependency but modern primitives", the three packages can be copied into
`internal/`. Measured at v0.24.0:

| package | Go LOC | asm LOC |
|---|---|---|
| `curve25519` (+ `internal/field`) | 942 | 0 |
| `hkdf` | 95 | 0 |
| `chacha20poly1305` (+ `chacha20`, `internal/poly1305`, `internal/alias`) | 1533 | 4479 |

`curve25519` + `hkdf` alone = **~1 kLOC of pure Go, no asm, no `x/sys` dependency** (grepped: neither
imports `golang.org/x/sys`). Since go1.19 stdlib already has AES-GCM, that is the entire missing
piece. **This is a real, cheap option** and it is the one that preserves invariant 8 while still using
audited upstream code — see §6.1 Option D′.

---

## 5. Performance budget — measured on this box

`Intel i7-6700 @ 3.40 GHz`, ext4 on `/dev/mapper/ubuntu--vg-ubuntu--lv`:

```
BenchmarkSeal1KiB-8                     17496 ns/op    (0.017 ms)
BenchmarkSealOpenRoundTrip1KiB-8        46042 ns/op    (0.046 ms)
BenchmarkOneFsync-8                   1117966 ns/op    (1.12 ms)
BenchmarkTwoFsyncsTwoFiles-8          2485318 ns/op    (2.49 ms)
```

**Conclusions that should drive the design, not vibes:**

- A full ratchet round trip is **2.4% of one fsync**. Crypto cost is irrelevant to throughput here.
- Splitting ratchet-state persistence into its own file/fsync costs **+123%** (1.12 → 2.49 ms). One
  record, one fsync.
- A 50-way broadcast fan-out = 50 × 17.5 µs = **0.87 ms of sealing** — still less than a single
  fsync. Pairwise fan-out is *not* the expensive part of a broadcast; the N durable writes are.
- Envelope overhead ≈ 150 B fixed (§4.1) — negligible against a 1 KiB message, 10× against a 16 B one.

---

## 6. The six tensions — decision menus and recommendations

### 6.1 Tension 1 — dependency + toolchain (invariant 8: stdlib first)

**Every option breaks "stdlib only" or breaks the go1.19 pin, or both.** There is no free choice. The
question is which cost the user wants to pay.

| | **A — `go.mau.fi/libsignal` + go1.25.x** | **B — `status-im/doubleratchet` + x/crypto@v0.24.0, stay go1.19.4** | **C — go1.25.x + x/crypto only, implement X3DH+DR ourselves** | **D′ — stay go1.19.4, vendor `curve25519`+`hkdf`, implement X3DH+DR ourselves** |
|---|---|---|---|---|
| new modules | 5 (`libsignal`, `x/crypto`, `x/sys`, `edwards25519`, **`protobuf`**) | 2 (`doubleratchet`, `x/crypto` pinned stale) | 2 (`x/crypto`, `x/sys`) | **0** (≈1 kLOC vendored under `internal/`) |
| toolchain bump | **YES → ≥1.23; recommend 1.25.x** | no | **YES** | no |
| we write the ratchet state machine | no | no (but must add replay defence) | **yes** | **yes** |
| replay defence | **in the library (proved)** | **absent by default (proved)** | ours | ours |
| skipped-key bound | 2000, in-library | 1000, in-library | ours | ours |
| group / Sender Keys | present | **absent** | ours if ever needed | ours if ever needed |
| safety numbers / fingerprints | present | **absent** | ours | ours |
| concurrency-safe | store-interface driven; we own locking | **no mutex anywhere** | ours | ours |
| upstream health | maintained (v0.2.2, Oct 2025); unaudited fork-of-fork; 1 test package | **last release 2019-10-31; self-described beta** | Go team (x/crypto) | Go team code, frozen copy |
| curve | X25519 (Signal-conformant) | X25519 | X25519 | X25519 |
| invariant weakened | **8 (hard)** + the go1.19 pin | **8** (and ships a known replay hole) | **8 (moderate)** + the pin | **8 (soft — vendored, not a module)** |
| security-patch story | x/crypto current, forever | **frozen at v0.24.0 forever** | x/crypto current | manual re-vendor, but curve25519/hkdf are stable code |

**Rejected outright:** CGO against `signalapp/libsignal` — no Rust on this box, `sudo` is
password-gated, and it would put a Rust toolchain in the build path of a Go-stdlib-first project.
**Rejected outright:** Option B — a 2019 beta with no locking that accepts replays is *worse than code
we would write ourselves*, which destroys the entire "don't roll your own" argument for using it.

> ### RECOMMENDATION: **Option A** — bump to go1.25.x and adopt `go.mau.fi/libsignal` behind a narrow `internal/cryptobox` / `internal/ratchet` facade.
>
> **One-line reason:** it is the only option that ships a *demonstrably correct* ratchet (replay
> rejected, skipped keys bounded, out-of-order handled, sender keys and fingerprints included) while
> keeping primitives patchable — and getting the ratchet subtly wrong is the failure mode that is both
> catastrophic and silent.
>
> **The one thing that could disqualify it, and the exact test that decides.** Option A's store
> interfaces (`state/store/*.go`) are called by the library at points we do not control. If the
> library will not let us commit *the ratchet-state advance and the message* in **one** fsynced record
> (§6.3), we would be paying 2.49 ms/msg instead of 1.12 ms *and* opening a crash window. **Run a
> 200-line throwaway spike against the store interfaces first** (proposed task `CRYPTO-2-STORESPIKE`,
> §8). If it fails, **fall back to Option C**, where we own the whole state machine and can make that
> record atomic by construction.
>
> **If the user refuses the toolchain bump**, the answer is **D′, not B.** Vendor `curve25519` +
> `hkdf` (~1 kLOC, no asm, no `x/sys`) and build X3DH + Double Ratchet over stdlib AES-256-GCM /
> Ed25519 / HMAC. It keeps `go.mod` at zero dependencies, keeps go1.19.4, and is strictly safer than
> adopting a beta library with a known replay hole.

**Position on rolling our own crypto.** We do **not** roll primitives — curve arithmetic, AEAD, HKDF
and signatures come from the Go team's code in every option. What is at stake is the *protocol state
machine*, which is a published spec with published test vectors. And note the asymmetry that makes
this less scary than it sounds: **in every option we must write the persistence layer ourselves**
(the libraries expose storage as interfaces). The catastrophic risk in this project — key/nonce reuse
via state rollback — is **ours in all four options**. Choosing a library does not outsource it.

### 6.2 Tension 2 — audit log vs forward secrecy (invariant 6)

| | **(a) E2E-opaque audit** | **(b) bus-visible / transport-only** | **(c) auditor-as-extra-recipient (escrow)** |
|---|---|---|---|
| what the WAL holds | ciphertext + envelope metadata | **plaintext** | ciphertext + an escrow copy |
| satisfies the user's ask | **yes** | **no** — the bus reads everything | partially |
| PFS against a stolen disk | **total** | none | **none, ever**, for anyone holding the escrow key |
| invariant 6 | *satisfied in the letter* (every message IS logged), weakened in utility (content unreadable) | satisfied | satisfied |
| new standing risk | none | the bus becomes the juiciest target in the system | a permanent skeleton key that must never leak and can never be rotated retroactively |

> ### RECOMMENDATION: **(a) E2E-opaque audit**, with (c) available only as an explicitly-off-by-default per-bus operator flag.
>
> **One-line reason:** (b) does not do what the user asked, and (c) re-creates the exact attacker prize
> that E2E exists to remove — so escrow must be an opt-in that a human turns on knowingly and that the
> server shouts about at startup.

**Invariant 6 is restated, not broken.** Proposed wording for the standing contract: *"Every message is
written to an append-only log. Where a message is end-to-end encrypted, the log records the ciphertext
and envelope metadata, not the plaintext: the audit trail proves existence, authorship, addressing,
ordering and delivery — it does not prove content."*

**What DUR-5's audit record must carry**, per message:

```
msg_id · bus-minted seq · sender FQ id (<bus-id>.<agent-id>) · recipient FQ id(s)
server timestamp · ciphertext length · SHA-256(ciphertext) · the ciphertext bytes
ratchet header (sender ratchet pubkey, PN, N) · envelope AAD
traversed-bus path (RELAY-3) · crypto_scheme tag · sender key_epoch
delivery/ack state transitions
NOT: plaintext. NOT: any message key. NOT: any ratchet secret.
```

**How a human debugs a flow they cannot read** — this must ship with the feature, not after it:

1. **Metadata tracing at the bus:** a `bus-trace.sh <msg-id>` wrapper renders the full chain — who → whom,
   seq, timestamps, sizes, hops, retries, ack state. Nearly every real routing/delivery bug is visible
   here; content is rarely what you need.
2. **Plaintext lives at the endpoint, where it belongs.** `agent-bus open --debug-log <dir>` writes the
   *decrypted* message to the **agent's own** key dir (0600). The receiving agent already has the
   plaintext; making it inspectable there costs nothing cryptographically and never puts plaintext on
   the bus.
3. **Escrow (opt-in, off by default):** `-audit-escrow-key <path>` adds an auditor identity as an
   additional pairwise recipient of every message. Cost, stated up front: **PFS is void against whoever
   holds that key, for every message ever sent while it was enabled** — retroactively unfixable.
   Enabling it must log `WARN` at startup and be surfaced by `/v1/info` so agents can see they are
   being escrowed. Recommend: do **not** build this in v1; file it as a separate P3 task so it is a
   deliberate later conversation.

### 6.3 Tension 3 — ratchet state vs durability (invariants 4 and 5)

> **The framing in CRYPTO-1/CRYPTO-7 is wrong, and fixing it dissolves most of the tension.**
> If encryption is genuinely end-to-end, **the bus holds no ratchet state at all.** It stores and
> forwards opaque ciphertext. Ratchet state — root key, chain keys, `Ns`/`Nr`/`PN`, skipped message
> keys — lives **on the agent side**, in the `agent-bus` helper's key dir. Therefore the bus's WAL
> never contains mutable ratchet state, and **invariants 4, 5 and 6 are not weakened by ratchet
> mutability**. `CRYPTO-7` must be split accordingly (§8).

The durability problem does not vanish; it **moves and splits in two**:

**(3a) Agent side — the ratchet state store.** A small, per-agent, WAL-structured file family under the
agent's key dir. Same discipline as the bus (append-only checkpoints, CRC per record, fsync,
prefix-consistent recovery), but a separate, much simpler store — **not** part of the bus's WAL and
**not** using the bus's `record-type` namespace.

**(3b) Bus side — one-time-prekey single issuance.** The bus *does* own one durable, monotonic,
must-never-roll-back fact: **a one-time prekey is handed out at most once.** That is precisely the same
shape as id minting (invariant 1) and belongs in the bus WAL with a reserved record type, re-derived on
replay without re-issuing. If replay re-issues a consumed prekey, two initiators can derive the same
X3DH secret — a real break.

#### The ordering rules (these are the whole answer)

- **SEND: advance → fsync → transmit.** Never transmit-then-persist. If we crash after fsync but before
  transmit, the message is simply never sent and the recipient sees a gap in `N` — harmless, the
  skipped-key machinery covers it. If we crashed the other way round, recovery would reuse `N` →
  **key and nonce reuse** → two ciphertexts under one key → plaintext leak. *Losing a message is
  recoverable; reusing a nonce is not.*
- **RECEIVE: one record, one fsync, then deliver.** The record is
  `{consumed-key marker, advanced receive state, the decrypted plaintext}` written and fsynced **as a
  unit**, and only then handed to the calling agent. This is simultaneously replay-safe (the key is
  durably consumed before anyone sees the plaintext) and crash-safe (the plaintext is durable, so a
  crash between fsync and delivery loses nothing — redelivery reads the stored plaintext instead of
  re-decrypting). The alternative orderings both lose: *deliver-then-persist* double-delivers on crash
  (a duplicated instruction to an autonomous agent is worse than a missing one); *consume-then-deliver
  without storing plaintext* destroys the message on crash.
  **Consequence requiring user sign-off:** decrypted plaintext is written to the agent's local disk
  (0600). That is a deliberate trade, not an accident — call it out (§9).
- **NEVER REWIND. FAIL CLOSED.** Recovery loads the **last fully-committed checkpoint**; it does not
  re-apply deltas. A monotonic guard — same shape as ID-4's counter test — refuses to load a checkpoint
  whose `(session_id, Ns, Nr, ratchet_epoch)` is *lower* than any previously observed. On a detected
  rollback, or a torn/CRC-failed checkpoint that may already have been used, the session is marked dead
  and a **fresh X3DH handshake is forced**. It must never "continue from the older state".
- **Bound everything.** Skipped keys per chain ≤ 2000 (Option A's built-in) *and* a total cap per
  session *and* a cap on live sessions. A peer claiming `N = 2^31` must be rejected **before** any
  allocation — proved reachable in both libraries (`too many messages` / `maxMessageKeys`), but the
  per-session and per-agent totals are ours to enforce.
- **One fsync, not two** (§5): the state advance and its message must share a record.

#### Crash-injection tests that would prove it (name them in CRYPTO-7a/7b)

| test | inject at | must yield |
|---|---|---|
| `TestRatchetCrashAfterAdvanceBeforeSend` | after state fsync, before transmit | state does **not** rewind; next send uses `N+1`; recipient tolerates the gap |
| `TestRatchetCrashAfterSendBeforeAck` | after transmit, before bus ack | same state on recovery; **no** `N` reused |
| `TestRatchetTornCheckpoint` | mid-fsync of the checkpoint | CRC rejects the tail; session marked dead; **rekey forced**, not resumed |
| `TestRatchetRollbackRefused` | hand-craft an older valid checkpoint | load **fails**; no send permitted |
| `TestConsumedKeyDurableAcrossRestart` | kill after receive-record fsync | replay of the same ciphertext is rejected after restart |
| `TestReceiveCrashBetweenFsyncAndDeliver` | kill after fsync, before stdout | plaintext is recoverable from the durable record; delivered exactly once |
| `TestSkippedKeyStoreBounded` | peer claims `N=10^9` | rejected **without** allocating; memory flat |
| `TestOneTimePreKeyIssuedAtMostOnceAcrossCrash` | kill between issue and commit | replay never re-issues a consumed prekey |
| `TestConcurrentSendersOneSession` (`-race`) | two goroutines sealing | serialized; no duplicate `N`; no race |

### 6.4 Tension 4 — broadcast and relay

**Broadcast.** The ratchet is strictly pairwise; `MSG-2` broadcasts to N.

| | **pairwise fan-out (N ciphertexts)** | **Sender Keys (one ciphertext + per-member distribution msg)** |
|---|---|---|
| PFS | **full ratchet PFS per recipient** | **weaker** — a chain key shared with the whole group; a member who leaves can decrypt until rekey |
| cost | N seals (**17.5 µs each**; N=50 → 0.87 ms, *less than one fsync*) + N stored ciphertexts | 1 seal, 1 ciphertext |
| membership change | nothing — sessions are already per-peer | **forces a full rekey + N distribution messages** on every join/leave (AUTH-4) |
| new state machine | none | a whole second one (group sessions, distribution, rekey) |
| storage | N × (payload + ~150 B) | 1 × (payload + overhead) |

> ### RECOMMENDATION: **pairwise fan-out for v1.** Defer Sender Keys.
>
> **One-line reason:** agent-bus broadcasts to a handful of agents, not a 1000-member group, and at
> that size the fan-out costs less than one fsync while keeping full PFS and per-recipient
> authenticity — Sender Keys would buy a rounding error of CPU in exchange for weaker PFS and an
> entire rekey protocol.

**Add one thing that pairwise fan-out does not give you for free:** a sender-signed digest over
`(broadcast_id, SHA-256(plaintext), the sorted recipient FQ-id set)` using the sender's messaging
identity key, included in every recipient's envelope. Without it a malicious *sender* can put
**different content** in each of the N ciphertexts under one broadcast id, and no recipient can tell.
One Ed25519 signature, ~50 µs, and it makes "we all got the same broadcast" verifiable.

**`MaxPayloadSize` landmine:** `internal/wal/format.go:32` caps a payload at **1 MiB**. Do **not** write
a broadcast's N ciphertexts as one aggregate audit record — a 64 KiB message to 16 recipients already
exceeds the cap. **One audit record per recipient ciphertext**, tied together by a `broadcast_id` in a
small fan-out group record.

**Relay — exactly what an intermediate bus can and cannot see:**

| an intermediate/relaying bus CAN see | CANNOT |
|---|---|
| message id, bus-minted seq | **plaintext — now or ever** (PFS: no key exists, including the sender's long-term key) |
| fully-qualified sender and recipient ids | forge a message that a recipient will accept (it has no ratchet key) |
| server timestamps, ciphertext size | undetectably tamper (AEAD fails → helper exit 10) |
| traversed-bus path (RELAY-3), retry/ack state | learn a past message's content from the WAL, ever |
| the ratchet header (sender ratchet pubkey, `PN`, `N`) — needed for routing/ordering | |

It **can** drop, delay, reorder and duplicate. Mitigations, all endpoint-side: duplicates die on the
consumed-key check (§6.3); gaps and reordering are visible to the recipient as `N` gaps; drops surface
via bus-level ack/cursor state (MSG-4). **Metadata is not protected** — the social graph of which agent
talks to which, when, and how much is fully visible to every bus on the path. Say so in `PROTOCOL.md`;
do not let a reader assume otherwise.

**Cross-bus key trust:** for a message from `<bus-A>.alice` to `<bus-B>.bob`, bus A fetches bob's bundle
from bus B, which attests it. **Bus B is a trusted introducer for its own agents.** A compromised bus B
can MITM first contact with any of its agents. The only real mitigation is §6.5's TOFU pin + safety
number, which turns that from "silent, permanent" into "works once, then gets caught, and can never
touch an established session".

### 6.5 Tension 5 — ids, enrolment and key trust (invariants 1, 2, 3)

**Two keypairs, never conflated:**

| | **AUTH keypair** (exists: AUTH-1) | **MESSAGING identity keypair** (new) |
|---|---|---|
| purpose | authenticate the agent **to the bus** | authenticate the agent **to peers**; sign its own prekeys |
| who generates | agent presents key at enrolment; **server signs** it → bearer credential | **agent, locally** (`agent-bus keygen`) |
| private half | agent's, never sent | agent's, **never leaves the box, never sent to the bus** |
| bus sees | public half + issues the token | public half **only** |
| lifetime / rotation | re-enrolment | `key_epoch` bump; breaks all sessions by design |
| algorithm | AUTH epic's choice (Ed25519 recommended — stdlib at go1.19) | Ed25519 identity (signing) + X25519 for the ratchet DH |

**Binding (this is the invariant-1 hook).** The agent's messaging public key is *input to be validated*;
the identity it binds to is the **server-minted fully-qualified `<bus-id>.<agent-id>`**. The server
writes a durable binding record `{FQ id, messaging identity pubkey, key_epoch, enrolment ref}` and that
record — never a client assertion — is what a key-bundle fetch returns. A client can never assert its
own id, and can never register a messaging key against someone else's id.

**Key trust — the crux:**

| | **(a) server-attested bundles only** | **(b) TOFU + safety numbers only** | **(c) BOTH** |
|---|---|---|---|
| how it works | the bus signs `{FQ id, identity pubkey, signed prekey, one-time prekey, key_epoch, issued_at}` with a bus signing key | agent pins a peer's identity key on first use; change = hard error | attested bundle bootstraps, **and** the agent pins it and enforces the pin thereafter |
| agent state needed | none | a pin file per peer | a pin file per peer |
| malicious bus at **first** contact | **full silent MITM** | MITM possible, but a safety-number comparison catches it | MITM possible once, caught on comparison |
| malicious bus on an **established** session | **full silent MITM** (it can just serve a new key) | impossible | **impossible** |
| ceremony required of the agent | none | out-of-band fingerprint compare | none by default; compare available |
| extra cost | a bus signing key to generate/store/rotate | one file per peer | both — small |

> ### RECOMMENDATION: **(c) — server-attested bundles for bootstrap, PLUS mandatory TOFU pinning and a computable safety number.**
>
> **One-line reason:** attestation alone leaves the bus able to MITM *established* sessions forever,
> and pinning removes exactly that — for the price of one 0600 file per peer and one comparison.

Rules that make (c) real: a peer's identity key changing on an **established** session is a **hard
failure** (`agent-bus open` exit **15**), never an auto-accept and never a silent re-pin; re-pinning
requires an explicit `agent-bus trust --peer <fq-id> --fingerprint <sn>` with the new safety number
supplied by the human; `key_epoch` is bumped by the server on `AUTH-4` leave/revocation and
invalidates outstanding bundles.

**Residual threat model, plainly:**

- **Compromised BUS** — gets the **full metadata graph** (who talks to whom, when, how often, how much),
  can drop/delay/reorder/duplicate, and can MITM a **first** contact between two agents that have never
  pinned each other. Cannot read established sessions, cannot forge into them, cannot decrypt anything
  later. *This is the security win: a total bus compromise no longer means a total content compromise.*
- **Compromised PEER agent** — reads what is sent to it (unavoidable), can impersonate itself, holds its
  own ratchet state, and holds any plaintext it durably stored (§6.3). PFS bounds it to the current
  chain, not to history whose keys it already deleted.
- **Offline attacker with the bus's WAL and disk** — metadata + ciphertext. **Nothing readable, now or
  ever, even with the bus's own signing key**, because the bus never held a message key. This is the
  headline property and it should be the sentence in the README.
- **Offline attacker with an AGENT's key dir** — that agent's ratchet state and stored plaintexts. Hence
  key dir `0700`, files `0600`, and a helper that **refuses to run** on wider permissions (ssh-style).

**AUTH tasks needing amendment (descriptions only — spec-keeper's call, not mine):**

- **AUTH-1** — enrolment must additionally accept and durably bind the messaging identity pubkey + the
  initial prekey bundle, in the **same** round trip. Two separate enrolments would leave a window where
  an agent is authenticated but unaddressable for E2E.
- **AUTH-3** — roster persistence must carry the messaging key binding and `key_epoch`, and recover them.
- **AUTH-4** — leave/revocation must bump `key_epoch`, invalidate outstanding attested bundles and mark
  peer sessions dead.
- **AUTH-6** — the fail-open allow-list work must explicitly confirm the key-bundle route is **not** in
  the unauthenticated set. An unauthenticated bundle endpoint hands an attacker the whole roster's key
  material and the social graph.

### 6.6 Tension 6 — agent-side validation in the wrapper (invariant 7)

Shell cannot do X25519/AEAD. Add subcommands to the **same** `agent-bus` binary — no second artifact,
no second version to skew.

**`agent-bus open`** (receive: verify + decrypt) and **`agent-bus seal`** (send: encrypt + sign).
CRYPTO-10 currently only names the receive side; **it must cover both**, because `bus-send.sh` cannot
encrypt in shell either.

**Contract for `agent-bus open`:**

- **stdin:** the envelope JSON exactly as `bus-wait.sh` received it from the bus.
- **stdout:** on **success only**, the verified plaintext bytes — nothing else, ever. On **any** failure
  stdout is **empty** and every diagnostic goes to stderr. This is the property that makes a naive
  wrapper safe: a wrapper that pipes stdout onward cannot leak unverified content, because there is
  none to leak.
- **exit codes** (distinct per failure mode — this is precisely what the user asked for):

| code | meaning |
|---|---|
| 0 | verified and decrypted; plaintext on stdout |
| 1 | usage / internal error |
| 10 | **bad MAC** — AEAD open failed (tampered, or wrong key) |
| 11 | **unknown sender** — no such FQ id, or no key binding for it |
| 12 | **no session** — X3DH handshake required first |
| 13 | **out-of-order beyond the skipped-key bound** |
| 14 | **replayed message** — message key already consumed |
| 15 | **sender identity key CHANGED since pinned** — TOFU alarm; never auto-accept |
| 16 | **bundle attestation invalid** — bus signature failed |
| 17 | **ratchet state unreadable / rollback detected** — fail closed, rekey required |

- **Key dir:** `${AGENT_BUS_HOME:-$HOME/.agent-bus}/<bus-id>/`, dir `0700`; `identity.key` `0600`;
  `ratchet/<peer-fq>.state` `0600`; `peers/<peer-fq>.pin` `0600`; `prekeys/` `0700`. The helper
  **refuses to run** if any is wider. **No key material in argv, ever** — same bug class as the
  existing `scripts/spec-cloud.sh` argv-password finding already on the backlog.
- **Wrapper rule (the load-bearing line):** `bus-wait.sh` must capture to a temp file, check the exit
  code, and only then emit. `set -o pipefail` is necessary but **not** sufficient. Concretely the
  wrapper must never be written as `bus-wait | agent-bus open || true`, and must never emit anything on
  a non-zero exit. Ship the `AGENT_PROTOCOL.md` entry documenting all ten exit codes **in the same
  task** (invariant 7).

---

## 7. Reservations to make later — ENUMERATED, NOT RESERVED

None of these were reserved. Nobody may hand-pick them; each must come from
`POST /api/v1/projects/agent-bus/reservations`.

**Namespace `record-type`** (bus WAL; currently 1–4 used: prepare, commit, abort, audit_message — see
`internal/wal/format.go:48-61`). Needs **8** new values:

| # | record |
|---|---|
| 1 | messaging-key binding — FQ id ↔ messaging identity pubkey, `key_epoch`, enrolment ref |
| 2 | signed-prekey publication |
| 3 | one-time-prekey batch publication |
| 4 | **one-time-prekey issuance (consumption)** — the at-most-once fact (§6.3b) |
| 5 | key-epoch bump / messaging-key revocation |
| 6 | **E2E ciphertext audit record** — deliberately distinct from `TypeAuditMessage=4` so no reader ever mistakes ciphertext for plaintext |
| 7 | broadcast fan-out group record — ties N per-recipient ciphertexts to one `broadcast_id` + sender signature |
| 8 | bus bundle-attestation signing-key record + its rotation |

**New namespace `agent-store-record-type`** (agent-side helper store — a *different* file family; it
must not consume bus record-type numbers). Needs **4**: ratchet session checkpoint · consumed-message-key
record · peer identity pin (TOFU) · delivered-plaintext record.

**Namespace `ondisk-format-version`** (currently `FormatVersion = 1`, `format.go:24`).
**No bump needed for adding record types** — `reader.go`/`scanFrom` deliberately tolerates unknown types
whose CRC verifies (`format.go:78-86`). A new *file kind* (`KindAgentStore`, magic `AGNTBUSK`) is a new
magic, not a version bump either. **But see the landmine in §10.1 — that tolerance is one-sided.**

**Namespace `wire-protocol-version`** — `PROTOCOL.md` does not exist yet and no version has been
published. The crypto surface adds routes (bundle fetch, prekey upload/replenish, `key_epoch` query) and
**changes the shape of every message body** from plaintext to `{ciphertext, ratchet_header, …}`. Reserve
**one** value. See §9.3 for why the timing of this matters more than the number.

---

## 8. SPEC-ready task breakdown (proposed — for spec-keeper, NOT applied here)

I did **not** create, edit or reprioritise anything on the Spec Server. This is a proposal.
**Task keys must be reserved** via `POST /api/v1/projects/agent-bus/reservations` with
`{"namespace":"task-key-CRYPTO", …}` — the namespace is already at 12, so new numbered keys start at 13.
Derived keys (`CRYPTO-2-STORESPIKE`) need no reservation but must be unique.

### 8.1 Blocked on the user (§9) — nothing below starts until these are answered

- **`CRYPTO-1-DECIDE`** *(no code)* — user chooses: toolchain path (§6.1), audit trade-off (§6.2), key
  trust model (§6.5); consents to §9's six breaking/consent items; spec-keeper records the resulting
  `DECISIONS.md` entries (drafts ready in §12).

### 8.2 Corrections to existing CRYPTO tasks

| task | proposed change | why |
|---|---|---|
| **CRYPTO-2** | **SPLIT** into **`CRYPTO-2A`** (toolchain bump only: `go.mod` `go` directive, supersede the go1.19-pin DECISIONS entry, CI/doc refs; **no crypto code**) and **`CRYPTO-2B`** (add the chosen module(s) + `internal/cryptobox` facade) | a toolchain bump must be its own reviewable, revertable commit, never a side effect of a feature |
| **`CRYPTO-2-STORESPIKE`** | **NEW**, blocks CRYPTO-2B | prove the ratchet-state advance can be committed **atomically with the message in one fsync** against `go.mau.fi/libsignal`'s `state/store` interfaces. **This is the Option A / Option C decider** (§6.1). Throwaway code, `/tmp` only |
| **CRYPTO-5** | **CONDITIONAL** — if Option A wins, shrink to *"wire the library's session builder + verify the attested bundle + bind AAD to both FQ ids"*; if C/D′ wins, keep as written | under Option A, X3DH comes from the library; specifying a from-scratch X3DH would be duplicated work |
| **CRYPTO-7** | **SPLIT** into **`CRYPTO-7A`** (agent-side ratchet-state durability + recovery + the crash-injection matrix in §6.3) and **`CRYPTO-7B`** (bus-side one-time-prekey at-most-once issuance across replay) | §6.3: the bus holds **no** ratchet state. The current description assumes it does, which would send an implementer down a wrong and dangerous path |
| **CRYPTO-8** | **AMEND** — pin the decision to pairwise fan-out; add the sender-signed digest over `(broadcast_id, SHA-256(plaintext), recipient set)`; require **one audit record per recipient ciphertext** (`MaxPayloadSize`, §6.4) | prevents divergent-content broadcasts and a 1 MiB payload overflow |
| **CRYPTO-10** | **AMEND** — cover **both** `agent-bus open` **and** `agent-bus seal`; enumerate all ten exit codes; key-dir permission refusal; the "capture, check exit, then emit" wrapper rule; `AGENT_PROTOCOL.md` in the same task | shell cannot encrypt either; send-side was missing |
| **CRYPTO-11** | **AMEND** — adopt §6.2's exact field list; add the `bus-trace.sh` metadata-debugging wrapper as an explicit deliverable | "how do I debug what I can't read" must ship with the feature |
| **CRYPTO-9 / CRYPTO-12** | keep; CRYPTO-12 additionally documents that **metadata is not protected** and lists the reserved numbers from §7 | |

### 8.3 New tasks

| proposed title | epic / gate | note |
|---|---|---|
| **`DUR-*` (reserve from `task-key-DUR`): `wal.Type.Known()` must be an explicit set, not a range** | **DUR epic, blocks every new record type** | §10.1 — a genuine latent landmine, and it is **not** a CRYPTO task |
| TOFU pinning + safety numbers + `agent-bus trust` | CRYPTO, after CRYPTO-4 | currently implicit inside CRYPTO-4; it is the entire defence against a malicious bus and deserves its own task and tests |
| Broadcast sender-signature over the recipient set | CRYPTO, with CRYPTO-8 | may fold into CRYPTO-8 |
| Prekey replenishment + exhaustion policy | CRYPTO, after CRYPTO-4 | what happens when one-time prekeys run out — fall back to signed-prekey-only X3DH (weaker) or refuse? **Decide deliberately.** |
| Key-dir bootstrap: `agent-bus keygen` + permission enforcement | CRYPTO, before CRYPTO-10 | |
| Audit escrow / auditor recipient (**P3, opt-in, off by default**) | CRYPTO | §6.2(c); separate so it is never enabled by accident |
| `AUTH-1/3/4/6` description amendments | AUTH | §6.5 |

### 8.4 Ordering

```
CRYPTO-1-DECIDE
  └─ CRYPTO-2A (toolchain)  ──┐
     CRYPTO-2-STORESPIKE    ──┴─ CRYPTO-2B (cryptobox) ─ CRYPTO-3 (enrolment keys)
        └─ CRYPTO-4 (bundles) ─ TOFU/safety-numbers ─ CRYPTO-5 (X3DH wiring)
              └─ CRYPTO-6 (DM ratchet) ─ CRYPTO-7A (agent state) ─ CRYPTO-7B (prekey once)
                    └─ CRYPTO-8 (broadcast) ─ CRYPTO-10 (helper+wrappers) ─ CRYPTO-11 (audit)
                          └─ CRYPTO-9 (relay) ─ CRYPTO-12 (docs)
DUR-* (Type.Known set) must land before ANY new record type is written.
```

Note the epic still sits behind CORE/ID/DUR/AUTH/MSG, **none of which are complete** — `internal/store`,
`internal/auth`, `internal/hub` and `internal/relay` are still `doc.go` stubs (9–10 lines each).
CRYPTO-2A and CRYPTO-2-STORESPIKE are the only items that can start immediately after the user decides.

---

## 9. BREAKING CHANGES AND NEW KEY MATERIAL — explicit user consent required

**None of the following may be actioned by any downstream agent without the user saying yes.**

1. **TOOLCHAIN BUMP: go1.19 → go1.25.x.** Changes `go.mod`'s `go` directive, contradicts CORE-1's pin,
   and **supersedes** the existing `2026-08-02 — go1.19 pin` entry in `DECISIONS.md`. *Proved
   non-breaking for existing code* (§2.1) and trivially revertable, but it is user-visible. Note it also
   re-opens the `log/slog` question that the `internal/logging` decision explicitly deferred to "if the
   toolchain is ever bumped" — that is a **separate** decision, not an automatic follow-on.
2. **FIRST THIRD-PARTY DEPENDENCIES** in a deliberately zero-dependency project — directly weakens
   invariant 8. Option A adds five modules including `google.golang.org/protobuf`. Requires a
   `DECISIONS.md` justification per CLAUDE.md; draft in §12.
3. **WIRE-BREAKING: the message body becomes ciphertext.** Every client that reads `body` as text
   breaks. **Timing is the whole issue:** MSG-1..5 are not built, so doing this *before* `PROTOCOL.md`
   v1 is published costs **nothing**. Shipping plaintext messaging first and encrypting later makes it a
   hard v1→v2 break with a migration. **Recommend deciding this now, while it is still free.**
4. **THE AUDIT LOG STOPS CONTAINING PLAINTEXT** — and because of PFS this is **irreversible** for every
   message sent under it. No future key, including the bus's own, will ever decrypt it. Anyone who
   expects to grep the audit log for message content must be told now, not after the first incident.
5. **NEW LONG-TERM KEY MATERIAL, AND A ROTATION STORY THAT DOES NOT EXIST YET.** Per agent: a messaging
   identity keypair + a signed prekey + a pool of one-time prekeys. Per bus: a **bundle-attestation
   signing key** — new long-term secret material that must be generated, stored `0600`, backed up, and
   rotated; **rotating it invalidates every outstanding attested bundle.** There is currently no
   backup/rotation procedure for any of it.
6. **PLAINTEXT AT REST ON THE AGENT'S DISK.** §6.3's receive ordering durably stores the decrypted
   message in the agent's key dir (`0600`) so a crash between fsync and delivery is not data loss. This
   is a deliberate confidentiality/durability trade and the user should say yes to it explicitly.

Also worth a yes/no, though not breaking: **`agent-bus` stops being server-only** — it gains
`keygen`/`seal`/`open`/`trust` subcommands and starts being invoked as a client-side tool by the
wrappers.

---

## 10. Latent landmines found along the way

### 10.1 `wal.Type.Known()` is a RANGE check — this will silently admit unreserved record types

`internal/wal/format.go:83-86`:

```go
func (t Type) Known() bool {
	return t >= TypePrepare && t <= TypeAuditMessage
}
```

and `internal/wal/writer.go:152` gates every append on `if !t.Known()`.

The reservations API allocates a **unique monotonic** value — and with several epics reserving from the
same `record-type` namespace concurrently, **CRYPTO's values will not be contiguous with 1–4**. The
moment someone reserves, say, 9 and extends the bound to `t <= 9`, values **5, 6, 7, 8 — which belong to
another epic, or to nobody — become writable**. That is exactly the class of bug the reservations
discipline exists to prevent, reintroduced one layer down.

**Fix (small, and it belongs to DUR, not CRYPTO):** make `Known()` an explicit `switch`/set over the
reserved values. Do it **before** any new record type lands. `format_test.go:176` already table-tests
`Known()`, so the change is cheap and well-covered.

*(Noted only — I own exactly one file in this repo and did not touch `internal/`.)*

### 10.2 `MaxPayloadSize` = 1 MiB vs broadcast fan-out

`format.go:32`. N ciphertexts aggregated into one record overflows at modest N (64 KiB × 16 already
exceeds it). §6.4: one audit record per recipient ciphertext.

### 10.3 Durable commit must not use `r.Context()` — already true, now doubly so

`DECISIONS.md` ("Shutdown cancels the root context BEFORE `http.Server.Shutdown`") already flags this
for DUR/MSG. With crypto it gets worse: an abandoned mid-write commit that leaves the **ratchet state
advanced but the message not durable** is not a lost message, it is a **reuse hazard**. §6.3's
advance→fsync→transmit ordering is what makes that safe; a request-scoped context in the commit path
would break it.

### 10.4 `-bus-id` is test-only, and FQ ids are now cryptographic identities

`DECISIONS.md` records `-bus-id` as a test-only override. Once the messaging pubkey is **bound** to
`<bus-id>.<agent-id>` and that binding is what peers trust, a mutable bus id becomes an **identity
substitution primitive**: run a bus with someone else's `-bus-id` and every attested bundle it emits
claims their namespace. When CRYPTO-3 lands, `-bus-id` must either be removed or hard-refused whenever
messaging keys are enabled.

### 10.5 The go1.19 pin has a second, quieter cost

`crypto/elliptic.ScalarMult` — the only stdlib route to ECDH at go1.19 (§2.4) — is **not** marked
deprecated in go1.19's source, but **is** in later Go (superseded by `crypto/ecdh`). Option D′ therefore
writes code that starts life on a deprecation path. Not disqualifying (D′ uses X25519 from vendored
`curve25519`, not P-256), but worth knowing before anyone reaches for the pure-stdlib P-256 shortcut.

### 10.6 Root filesystem is 100% full

`/` has **427 MB free of 218 GB**. Unrelated to crypto, but a WAL-and-fsync project on a full disk will
fail in confusing ways, and the module cache (`/home/mike/go/pkg/mod`) lives there. Worth someone's
attention independently of this task.

---

## 11. What this spike did NOT cover (stated so nobody assumes it did)

- **No security audit of `go.mau.fi/libsignal`.** I ran its tests, read the replay and skipped-key
  paths, and measured envelope sizes. I did **not** review its X3DH, its protobuf parsing, or its ECC
  code. It is **unaudited** and my recommendation does not change that.
- **Replay was tested empirically in `status-im/doubleratchet` across four configurations, and in
  `go.mau.fi/libsignal` for the first-message (`PreKeySignalMessage`) case only.** The follow-up
  `SignalMessage` replay assertion panicked on a test-harness type assertion (the second message was
  also a `PreKeySignalMessage` because the session was not yet acknowledged) — the *code path* is
  `SessionCipher.go:337-338` and reads correct, but I did not execute that specific case.
- **No crash-injection was performed.** §6.3's tests are *specified*, not run — the code does not exist.
- **Benchmarks are single-box, single-run, `-benchtime=300x`** on one ext4 volume. The 1.12 ms fsync is
  this disk; the *ratio* (crypto ≈ 2% of an fsync) is the durable conclusion, not the absolute number.
- **I did not install any toolchain system-wide, did not modify `go.mod`, and did not run anything
  against the tracked `data/` dir.** The go1.25.12 test used a `git archive HEAD` copy in a scratch dir.
- **I did not reserve any number** and **did not create, edit or reprioritise any Spec Server task**.
- **Module-proxy probing was bounded** to six candidate ratchet ports (§4.3); there may be others.
- `SPEC.md` showed as modified in `git status` during this work. **That was another agent's mirror
  refresh, not mine** — I touched exactly one file, `CRYPTO_DEEPDIVE.md`.

---

## 12. PROPOSED DECISIONS.md ENTRIES (NOT YET APPLIED — AWAITING USER DECISION)

These are **drafts**. They were deliberately **not** written to `DECISIONS.md`: that file is being
appended to by the in-flight ID/DUR wave, and more importantly **these decisions are the user's to make,
not mine**. Once the user chooses, spec-keeper appends them verbatim (newest last, per that file's own
convention). Each is dated **2026-08-02**.

---

### Draft entry 1

```markdown
---

## 2026-08-02 — Toolchain bumped go1.19 → go1.25.x (supersedes the go1.19 pin)

**Context.** CRYPTO-1 (Signal-style E2E crypto) needs X25519, HKDF and a modern AEAD. Verified on this
box:

    $ go version                          -> go1.19.4 linux/amd64
    $ ls $(go env GOROOT)/src/crypto/ecdh -> No such file or directory   (crypto/ecdh is go1.20)
    $ ls $(go env GOROOT)/src/crypto/hkdf -> No such file or directory   (crypto/hkdf is go1.24)

`golang.org/x/crypto` was bisected against go1.19.4: **v0.24.0 is the last version that compiles**;
v0.25.0 onwards fail with `package crypto/ecdh is not in GOROOT`. Staying on go1.19 therefore means
pinning x/crypto at a June-2024 release permanently, with no path to future fixes. Every credible Go
implementation of the Signal protocol (`go.mau.fi/libsignal` v0.1.2+) declares `go >= 1.23`.

The bump was proved, not assumed: go1.25.12 was fetched to a throwaway dir (no sudo, no system install)
and the repo at HEAD was built and tested under it on a pristine `git archive` copy —
`go build`, `go vet`, `gofmt -l` and `go test -race ./...` all green under **both** go1.19.4 and
go1.25.12.

**Decision.** Bumped the `go` directive in `go.mod` from `1.19` to `1.25`, in its own commit, separate
from any crypto code. This **supersedes** the `2026-08-02 — go1.19 pin` entry above.

**Consequences.**
- `crypto/ecdh`, `crypto/hkdf`, `errors.Join`, `slices`/`maps` and `log/slog` all become available.
  Availability is not adoption: **`internal/logging` is NOT being replaced by `log/slog` as a side
  effect of this bump.** That earlier entry invited a re-evaluation "if the toolchain is ever bumped";
  that re-evaluation is its own task and its own decision.
- CI/dev boxes must have go1.25.x. `GOTOOLCHAIN` auto-download works here and covers the gap.
- The bump is trivially revertable (one line) as long as no go1.20+ feature has been adopted; the first
  such adoption makes it permanent, and should say so where it lands.
```

### Draft entry 2

```markdown
---

## 2026-08-02 — Signal protocol via go.mau.fi/libsignal; NOT Signal's own library (invariant 8 waiver)

**Context.** The user asked for "the messaging ratchet library the signal people made for
signal/whatsapp". **That library does not exist for Go.** `github.com/signalapp/libsignal` is Rust with
Java/Swift/TypeScript bindings and has no Go binding. Options assessed on this box:

- **CGO against libsignal** — rejected. `rustc`/`cargo` are absent (`command not found`) and `sudo`
  is password-gated, so a Rust toolchain cannot even be installed non-interactively. It would put Rust
  in every build and break `CGO_ENABLED=0` static builds.
- **`github.com/status-im/doubleratchet`** — rejected. Last release **2019-10-31**, README
  self-describes as *beta*, contains **no mutex anywhere** (a data race is P0 in this project), has no
  X3DH/sender-keys/fingerprints, and **accepts replayed ciphertexts by default**: reproduced in four
  configurations; `session.go:190-198` re-files the just-used message key into the skipped-key store
  and never deletes it, delegating replay defence to a `DeleteMk` call the caller must remember.
- **`go.mau.fi/libsignal` v0.2.2** — a maintained community Go port (mautrix's fork of
  libsignal-protocol-go), the code that speaks to WhatsApp from Go today. Verified: passes its own suite
  under `go test -race` on go1.25.12; **rejects replay** (`SessionCipher.go:337-338` consumes the
  message key — observed `received message with old counter`); bounds skipped keys at
  `maxMessageKeys = 2000`; includes X3DH, Sender Keys and fingerprints; exposes persistence as
  interfaces we implement.

**Decision.** Adopted `go.mau.fi/libsignal` behind a narrow in-house `internal/cryptobox` /
`internal/ratchet` facade, accepting the transitive modules `golang.org/x/crypto`, `golang.org/x/sys`,
`filippo.io/edwards25519` and `google.golang.org/protobuf`. This is an explicit **waiver of invariant 8**
("Go stdlib first; a third-party dependency needs a justification here") for the crypto core only.

**Consequences.**
- The project's zero-dependency posture ends here. Every one of these modules is now our supply chain;
  `protobuf` in particular is heavy for a project that had none. The facade exists so the *rest* of the
  codebase never imports them directly and a future swap touches one package.
- **The library is NOT audited, and it is a fork of a fork of an abandoned port.** We are trusting a
  community implementation of a published spec. Say so in `PROTOCOL.md`; do not let a reader infer
  Signal endorsement.
- **The persistence layer is ours regardless of this choice.** The library exposes storage as
  interfaces, so the catastrophic risk here — ratchet-state rollback causing key/nonce reuse — is
  ours to prevent in any option. Choosing a library did not outsource it.
- **This decision is contingent on `CRYPTO-2-STORESPIKE` passing**: if the ratchet-state advance cannot
  be committed atomically with the message in a single fsync against the library's store interfaces,
  fall back to implementing X3DH + Double Ratchet over `golang.org/x/crypto` alone, and supersede this
  entry.
```

### Draft entry 3

```markdown
---

## 2026-08-02 — The audit log records ciphertext, not plaintext (invariant 6 restated)

**Context.** Invariant 6 says every message is written to an append-only audit log. End-to-end
encryption with forward secrecy means **the bus cannot read its own audit log**, and PFS means it can
**never** be decrypted later — not with the sender's long-term key, not with the bus's own key. The
three options were: log plaintext (bus-visible "E2E" — does not do what the user asked), log ciphertext,
or add an escrow recipient (a permanent skeleton key that recreates exactly the attacker prize E2E
removes).

**Decision.** The audit log records **ciphertext plus envelope metadata**: message id, bus-minted
sequence, fully-qualified sender and recipient ids, server timestamps, ciphertext length,
SHA-256(ciphertext), the ciphertext bytes, the ratchet header (sender ratchet pubkey, PN, N), the
envelope AAD, the traversed-bus path, a crypto-scheme tag and the sender's key epoch. **Never**
plaintext, message keys, or ratchet secrets. Escrow is **not** built; if it ever is, it is a per-bus
flag that is OFF by default, logs WARN at startup, and is surfaced on `/v1/info` so agents can see they
are being escrowed.

Invariant 6 is restated as: *"Every message is written to an append-only log. Where a message is
end-to-end encrypted, the log records the ciphertext and envelope metadata, not the plaintext: the
audit trail proves existence, authorship, addressing, ordering and delivery — it does not prove
content."*

**Consequences.**
- **Irreversible.** No future key recovers the content of any message sent under this scheme. Anyone
  expecting to grep the audit log for message text must be told before the first incident, not during
  it.
- Debugging a flow you cannot read must ship **with** the feature: a `bus-trace.sh <msg-id>` wrapper
  rendering the metadata chain (who → whom, seq, timestamps, sizes, hops, acks), plus
  `agent-bus open --debug-log <dir>` which writes decrypted content to the **agent's own** key dir —
  plaintext belongs at the endpoint, never in the middle.
- **Metadata is not protected.** Every bus on the path sees the full social graph: who talks to whom,
  when, how often, how much. This must be stated in `PROTOCOL.md`, not left to inference.
- The upside, and it is the point: a total compromise of the bus (live process, disk, WAL, or its own
  signing key) no longer yields any message content, past or future.
```

### Draft entry 4

```markdown
---

## 2026-08-02 — Ratchet state lives on the AGENT, not on the bus

**Context.** CRYPTO-1 was specced on the assumption that Double Ratchet state — mutable, per-session,
advancing with every message — would have to live on top of the bus's append-only, replay-on-recovery
store, and that a replay or rollback would cause key/nonce reuse. That framing is wrong for a genuinely
end-to-end design: **if the bus can hold ratchet state, the bus can read the messages.**

**Decision.** The bus stores and forwards **opaque ciphertext** and holds **no** ratchet state. Ratchet
state lives in the agent-side `agent-bus` helper's key dir. The durability problem splits in two:

- **Agent side** — a small WAL-structured checkpoint store under the agent's key dir (own record-type
  namespace, `agent-store-record-type`; not the bus's). Ordering rules, which are the whole answer:
  **SEND = advance → fsync → transmit** (never transmit-then-persist: a lost message is recoverable, a
  reused nonce is not); **RECEIVE = one record `{consumed-key marker, advanced receive state,
  plaintext}`, one fsync, then deliver** (replay-safe *and* crash-safe, and it is what makes redelivery
  idempotent). Recovery loads the **last fully-committed checkpoint** and never re-applies deltas; a
  monotonic guard on `(session_id, Ns, Nr, ratchet_epoch)` refuses any checkpoint that would rewind, and
  a detected rollback or torn checkpoint **fails closed** and forces a fresh X3DH handshake.
- **Bus side** — exactly one durable crypto fact: **a one-time prekey is issued at most once.** Same
  shape as id minting (invariant 1); it gets a reserved record type and must be re-derived on replay
  without re-issuing.

**Consequences.**
- **Invariants 4, 5 and 6 are NOT weakened by ratchet mutability** — the bus's WAL never contains it.
- Measured on this box: ratchet seal+open of 1 KiB = 46 µs; one fsync = 1.12 ms; two fsyncs to two
  files = 2.49 ms. **Crypto is 2.4% of a single fsync**, so the only performance rule that matters is
  *one record, one fsync* — splitting state persistence from the message commit costs +123% and opens a
  crash window for nothing.
- **Decrypted plaintext is durably written to the agent's local disk (0600)** before delivery. That is
  a deliberate confidentiality-for-durability trade, taken so a crash between fsync and delivery is not
  data loss.
- Skipped message keys are bounded per chain (2000), per session, and per agent; a peer claiming a huge
  counter jump is rejected **before** allocation.
- `CRYPTO-7` must be split into `CRYPTO-7A` (agent-side state) and `CRYPTO-7B` (prekey single issuance);
  its current description sends an implementer down the wrong path.
```

### Draft entry 5

```markdown
---

## 2026-08-02 — Broadcast is pairwise fan-out; Sender Keys deferred

**Context.** The Double Ratchet is strictly pairwise; agent-bus broadcasts to N agents (MSG-2). Signal
solves groups with Sender Keys, which have deliberately weaker forward secrecy — a chain key shared
across the group, so a departed member can decrypt until a rekey — and which require a full rekey plus
N distribution messages on every membership change (AUTH-4).

**Decision.** Broadcast is **pairwise fan-out**: N ciphertexts, one per recipient session, each with
full ratchet PFS and per-recipient authenticity. Additionally, every recipient's envelope carries a
**sender-signed digest over `(broadcast_id, SHA-256(plaintext), sorted recipient FQ-id set)`** under the
sender's messaging identity key. Sender Keys are **not** built; the trade-off is recorded so a future
large-group requirement can revisit it rather than rediscover it.

**Consequences.**
- Measured: sealing 1 KiB costs 17.5 µs, so a 50-way fan-out is 0.87 ms of crypto — **less than one
  fsync (1.12 ms)**. The fan-out was always going to be N durable writes; the crypto is not the cost.
- Storage is N × (payload + ~150 B envelope) rather than one copy.
- The sender signature closes a real hole that pairwise fan-out otherwise leaves open: without it a
  malicious *sender* can place different content in each of the N ciphertexts under one broadcast id and
  no recipient can detect it.
- `internal/wal/format.go` caps a payload at 1 MiB, so a broadcast must be written as **one audit record
  per recipient ciphertext**, tied together by a fan-out group record — never as one aggregate record,
  which overflows at modest N (64 KiB × 16 already exceeds the cap).
- A sender with no session to a given recipient must run X3DH before it can broadcast to them.
```

### Draft entry 6

```markdown
---

## 2026-08-02 — Key trust: server-attested bundles PLUS mandatory TOFU pinning

**Context.** An agent must obtain another agent's messaging public key before it can start a session.
Server-attested bundles alone make the bus a trusted introducer: a compromised bus can silently MITM
**any** session, including established ones, simply by serving a different key. Trust-on-first-use alone
still trusts the bus at first contact and needs an out-of-band comparison to mean anything.

**Decision.** Both. The bus signs a key bundle `{fully-qualified id, messaging identity pubkey, signed
prekey, one-time prekey, key_epoch, issued_at}` with a bus attestation key, **and** the agent-side
helper pins the peer's identity key on first use and enforces that pin thereafter. A peer's identity key
changing on an established session is a **hard failure** (`agent-bus open` exit code 15), never an
auto-accept and never a silent re-pin; re-pinning requires an explicit
`agent-bus trust --peer <fq-id> --fingerprint <safety-number>` with the number supplied by a human.
`AUTH-4` leave/revocation bumps `key_epoch` and invalidates outstanding bundles.

The messaging keypair is generated **locally by the agent** and only its public half is registered at
enrolment; the private half never reaches the bus. It is a **second, separate** keypair from the AUTH
keypair — the auth key authenticates the agent to the bus, the messaging identity key authenticates it
to peers and signs its prekeys. They are never conflated. Per invariant 1, the server **binds** the
registered messaging pubkey to the server-minted fully-qualified `<bus-id>.<agent-id>`: the key is input
to be validated, the identity is the server's to mint.

**Consequences (residual threat model, stated plainly).**
- **Compromised bus:** gets the full metadata graph (who, when, how often, how much); can drop, delay,
  reorder and duplicate; can MITM a **first** contact between two agents that have never pinned each
  other. **Cannot** read or forge into an established session, and cannot decrypt anything later.
- **Compromised peer agent:** reads what is sent to it, holds its own ratchet state and any plaintext it
  durably stored. PFS bounds it to the current chain, not to history whose keys it deleted.
- **Offline attacker with the bus's disk/WAL:** metadata and ciphertext, and **nothing readable, ever**,
  even with the bus's own attestation key — the bus never held a message key.
- **Offline attacker with an agent's key dir:** that agent's sessions and stored plaintexts. Hence key
  dir 0700, files 0600, a helper that refuses to run on wider permissions, and no key material in argv.
- `-bus-id` (recorded above as a test-only affordance) becomes an **identity-substitution primitive**
  once messaging keys are bound to `<bus-id>.<agent-id>`: it must be removed or hard-refused whenever
  messaging keys are enabled.
- New long-term secret material exists that has no backup or rotation procedure yet: per-agent messaging
  identity keys and per-bus attestation keys. Rotating the bus attestation key invalidates every
  outstanding bundle.
```

### Draft entry 7

```markdown
---

## 2026-08-02 — Agent-side validation: `agent-bus open`/`seal`, and stdout is empty unless verified

**Context.** The user asked for "a mechanism to validate messages in the agent script before accepting
them" (invariant 7: agents never hand-write HTTP; every capability ships its `scripts/bus-*.sh` wrapper
and its `AGENT_PROTOCOL.md` entry in the same task). Shell cannot do X25519 or AEAD, in either
direction — it can neither verify a received message nor encrypt one to send.

**Decision.** The same `agent-bus` binary gains `keygen`, `seal` (encrypt+sign for send) and `open`
(verify+decrypt for receive) subcommands, which the wrappers shell out to. `open` reads the envelope
JSON on stdin and writes the verified plaintext to stdout **only on success** — on any failure stdout is
**empty**, all diagnostics go to stderr, and the exit code is non-zero and specific:
0 ok · 1 usage/internal · 10 bad MAC · 11 unknown sender · 12 no session · 13 out-of-order beyond the
skipped-key bound · 14 replayed message · 15 sender identity key changed since pinned · 16 bundle
attestation invalid · 17 ratchet state unreadable or rollback detected.

Keys live under `${AGENT_BUS_HOME:-$HOME/.agent-bus}/<bus-id>/` with the directory 0700 and every key
file 0600; the helper **refuses to run** if any is wider. No key material is ever passed in argv.

**Consequences.**
- "stdout is empty unless verified" is the load-bearing property: a wrapper that pipes stdout onward
  cannot leak unverified content, because there is none to leak. The wrapper must still capture to a
  temp file, check the exit code, and only then emit — `set -o pipefail` is necessary but not
  sufficient, and `bus-wait | agent-bus open || true` is forbidden.
- Codes 15 and 17 **fail closed** by design: a changed peer key and an unreadable/rolled-back ratchet
  state are both refusals that require a human, never an automatic retry or re-pin.
- `CRYPTO-10` must cover **both** directions; its current description names only the receive side, and
  `bus-send.sh` cannot encrypt in shell either.
- The `AGENT_PROTOCOL.md` entry documenting all ten exit codes ships in the **same** task as the code
  (invariant 7). The `agent-bus` binary is no longer server-only.
```

---

*End of CRYPTO_DEEPDIVE.md — CRYPTO-1 remains `in_progress`; its output is an escalation to the user,
not a completion.*
