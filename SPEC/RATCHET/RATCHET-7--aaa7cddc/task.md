# RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed25519 vs a libsodium binding)

| Field | Value |
| --- | --- |
| Public id | `aaa7cddc-e941-41e6-b952-b2fc4ab55423` |
| Key | RATCHET-7 |
| Epic | [RATCHET](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:44:38.913562+00:00 |
| Updated | 2026-08-02T14:08:05.791651+00:00 |
| Completed | 2026-08-02T14:08:05.791633+00:00 |

## Proof command

```sh
grep -q 'RATCHET-7' DECISIONS.md && grep -q '2026-08-02 .* Ed25519 is Go stdlib' DECISIONS.md && test "$(go list -m all)" = 'github.com/dodgymike/agent-bus'
```

## Status note

RATCHET-7 DECIDED 2026-08-02 by feature-runner: option (a) Go stdlib crypto/ed25519; cgo libsodium binding REJECTED. Deliverable (dated DECISIONS.md entry incl. supply-chain review, pinned-version story, advisory-monitoring mechanism, and implementer sharp edges) is written and staged but AWAITING COORDINATED COMMIT by the orchestrator. security sub-agent verdict: PASS / recommend (a). No user consent required (no new dependency, no runtime component, no key material). Complete this task with the commit_sha once the wave is committed. UNBLOCKS AUTH-1/2/3, SIGN-1..8, CRYPTO-3, CRYPTO-4, CRYPTO-10 -- they may now proceed naming crypto/ed25519 as a settled decision.

## Description

RESCOPED 2026-08-02 (sign-only). This is the LAST undecided crypto question in the epic and it GATES the implementation of SIGN-1/SIGN-2/CRYPTO-10, all of which currently name Go's crypto/ed25519 as the presumptive answer -- this task confirms or overrides that, once, in writing. DECISIONS.md deliberately left it open: "whether to use stdlib crypto/ed25519 or a cgo libsodium binding is left to the implementing task; both satisfy invariant 9". DECIDE BETWEEN EXACTLY TWO OPTIONS -- do not open a wider search, and under invariant 9 do not consider any option that involves implementing a primitive ourselves: (a) Go stdlib crypto/ed25519 -- zero new modules, no cgo, works on the box's go1.19.4, is the RFC 8032 reference-implementation lineage upstreamed into the stdlib, and is a high-level Sign/Verify API (exactly the 'wraps as much of the problem as possible' invariant 9 asks for); its supply chain IS the Go toolchain, so the review becomes 'how is the builder image's Go version pinned and how do we learn about Go security releases' (ties to DEPLOY-1). (b) a cgo libsodium binding -- matches the user's word 'libsodium' literally, but adds a C library to the runtime image, cgo to the build, and a binding maintainer to the trust chain. REVIEW BOTH ON: provenance and who can push a release, release signing / checksum verification, transitive dependency footprint, cgo and native build requirements against the multi-stage Docker image (DEPLOY-1's minimal runtime -- a cgo binary is not static and will not run on a scratch/distroless base without care), CVE history, and our exposure if it is abandoned. DELIVERABLES: the choice, the exact pinned version, how we learn about advisories (name the mechanism -- e.g. govulncheck in the DEPLOY-5 container check, GitHub advisory watch), and a dated DECISIONS.md entry containing all of it. Invariant 8 requires a justification for any third-party dependency; a crypto dependency requires this stronger form. NOTE the honest asymmetry when weighing: 'it is what the user said' is a reason to take libsodium seriously, but the user's controlling requirement was standard, audited, high-level sign/verify -- not a specific vendor -- so either option satisfies the instruction as long as the reasoning is recorded.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [SIGN-2](../../SIGN/SIGN-2--1c183f10/task.md)
- **relates to** [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [DEPLOY-1](../../DEPLOY/DEPLOY-1--fa0c5a4e/task.md) — DEPLOY-1: Dockerfile -- multi-stage build, pinned Go builder, minimal runtime image (done)
- [DEPLOY-5](../../DEPLOY/DEPLOY-5--259a6a55/task.md) — DEPLOY-5: container build/test check (CI or make/script target) (todo)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-2](../../SIGN/SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4eb903f8-04cd-497c-ba4a-7eadceb65725](../../SIGN/SEC-ed25519.Verify-panics-on-wrong-size-public-key-remot--4eb903f8/task.md) — SEC: ed25519.Verify panics on wrong-size public key -- remote DoS across AUTH-1/CRYPTO-10… (todo)
- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [DEPLOY-5](../../DEPLOY/DEPLOY-5--259a6a55/task.md) — DEPLOY-5: container build/test check (CI or make/script target) (todo)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-2](../../SIGN/SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
