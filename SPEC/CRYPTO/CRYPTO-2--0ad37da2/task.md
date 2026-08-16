# CRYPTO-2: Adopt the crypto primitive layer chosen by the spike (internal/cryptobox + go.mod/toolchain)

| Field | Value |
| --- | --- |
| Public id | `0ad37da2-e491-4efb-bbd9-0e7b22ea7a49` |
| Key | CRYPTO-2 |
| Epic | [CRYPTO](../epic.md) |
| Status | superseded |
| Priority | P2 |
| Component | crypto |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:41:19.789256+00:00 |
| Updated | 2026-08-02T12:59:50.434828+00:00 |
| Completed | — |

## Proof command

```sh
go build ./... && go vet ./... && go test -race ./internal/cryptobox
```

## Status note

Superseded by user instruction (2026-08-02): no ratchet library, no toolchain bump needed -- crypto/ed25519 is Go stdlib since 1.13 and needs no internal/cryptobox facade over X25519/HKDF/AEAD. See SIGN-1/SIGN-2.

## Description

GATED: do not start until CRYPTO-1 (design spike) is done and its DECISIONS.md entry is recorded -- the spike chooses the crypto library/primitives, the ratchet-state durability model, the broadcast/relay scheme and the key-trust model. (The audit-log-vs-PFS trade-off is CLOSED per user instruction 2026-08-02 -- see CRYPTO-11/DUR-5 -- and is no longer part of what CRYPTO-1 decides.) Implementing before the remaining decisions exist is guessing.

Land the dependency decision CRYPTO-1 recorded: add the chosen module(s) to go.mod, and bump the go directive/toolchain to whatever version the spike says is required.

RUNTIME TARGET (user instruction, 2026-08-02): agent-bus ships as a container under Docker Compose. The CONTAINER's builder image pins the Go toolchain, NOT this workstation's ambient go1.19.4 -- CORE-1's go1.19 pin was a dev-box artifact, not a permanent constraint (see CLAUDE.md's "Runtime target: Docker Compose" section). So a bump past go1.19 is no longer something to work around: choose the version the ratchet library actually needs (crypto/ecdh is go1.20+; a current libsignal-compatible stack may want newer) and state it plainly in DECISIONS.md. This relaxes the Go VERSION only -- invariant 8 (stdlib first, third-party deps need a DECISIONS.md justification) is UNCHANGED.

SEQUENCING: the actual go.mod/toolchain bump and the container builder image pin are owned by the DEPLOY epic's toolchain-bump task, which is explicitly sequenced to land AFTER the in-flight ID/DUR wave completes (that wave is building against go1.19 right now). Coordinate with spec-keeper on ordering rather than bumping go.mod unilaterally from this task -- if this task's dependency needs the newer toolchain to even compile its test vectors, block on the DEPLOY toolchain-bump task rather than bumping go.mod early.

Introduce internal/cryptobox as a NARROW interface over the primitives (keypair generation, X25519 agreement, HKDF, AEAD seal/open, constant-time compare). The point of the narrow interface is that the ratchet code above it does not care which of the spike's options (a)/(b)/(c) won, and swapping the implementation later is a one-package change. Include known-answer/test-vector tests for every primitive -- crypto without test vectors is unverified. NO protocol logic in this task: no X3DH, no ratchet, no wire format. Update DECISIONS.md if the adopted dependency differs in any way from what CRYPTO-1 recorded.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CORE-1](../../CORE/CORE-1--eea035e4/task.md) — CORE-1: Repo skeleton: go.mod, internal/ package layout, .gitignore (done)
- [CRYPTO-1](../CRYPTO-1--30570fb9/task.md) — CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DEC… (done)
- [CRYPTO-11](../CRYPTO-11--0047e5b7/task.md) — CRYPTO-11: Audit-log content hash for signed (cleartext) messages -- implements the invar… (todo)
- [DUR-5](../../DUR/DUR-5--a7123e88/task.md) — DUR-5: Append-only message audit log (done)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-2](../../SIGN/SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-1](../CRYPTO-1--30570fb9/task.md) — CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DEC… (done)
- [CRYPTO-5](../CRYPTO-5--9f3f8065/task.md) — CRYPTO-5: X3DH session establishment between two agents (deferred)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
