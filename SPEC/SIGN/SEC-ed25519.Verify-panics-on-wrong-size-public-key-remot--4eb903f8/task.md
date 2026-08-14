# SEC: ed25519.Verify panics on wrong-size public key -- remote DoS across AUTH-1/CRYPTO-10/SIGN-2 call sites

| Field | Value |
| --- | --- |
| Public id | `4eb903f8-04cd-497c-ba4a-7eadceb65725` |
| Key | _(null in the export)_ |
| Epic | [SIGN](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:57:11.037231+00:00 |
| Updated | 2026-08-08T10:29:56.958880+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestSafeVerify ./... ; go test -race -run TestEnroll_MalformedPublicKey ./internal/auth ; go test -race -run TestVerify_MalformedPublicKey ./internal/... -- all pass with no panic on wrong-size/nil public keys
```

## Description

Cross-cutting security gap surfaced by RATCHET-7 and VERIFIED FIRST-HAND by backlog-triage by reading this box's own stdlib source at crypto/ed25519/ed25519.go (Go 1.19 GOROOT): ed25519.Verify PANICS -- it does not return false -- when len(publicKey) != ed25519.PublicKeySize. This is a remote DoS trap because it is ASYMMETRIC with malformed-signature handling: a bad/tampered signature safely returns false, but a wrong-size (or nil) public key crashes the process. A call site that validates the signature but not the key length therefore looks correct in review and is remotely crashable in production.

This matters immediately because at least three call sites accept or load attacker-influenceable public keys and will call ed25519.Verify on them:
- AUTH-1 (POST /v1/enroll): the public key is client-supplied at enrolment -- untrusted input by definition (invariant 1: a client-supplied value is input to be validated, never an identity to be trusted).
- CRYPTO-10 (`agent-bus verify` + wrapper validate-before-accept): verifies contact-list/sender public keys, including keys reloaded from the on-disk roster after a restart.
- SIGN-2 (sign on the send path) and any downstream recipient-side verification against a sender's messaging public key.

SCOPE OF THIS TASK: own the fix and its verification ACROSS all of the above call sites (do not let each task independently reinvent the guard -- provide or point to one shared, tested helper, e.g. a `safeVerify(pub, msg, sig []byte) bool` that length-checks before delegating to ed25519.Verify) plus any other Verify call site discovered during implementation, including ed25519.PublicKey values loaded from the roster on disk after a restart (DUR/recovery path).

ACCEPTANCE CRITERIA:
1. Every ed25519.Verify call site in the codebase length-checks the public key against ed25519.PublicKeySize before calling Verify, and returns/propagates a normal validation error on mismatch -- never a panic.
2. A shared helper exists (not copy-pasted per-call-site logic) so future call sites get the guard by construction.
3. Each affected call site (AUTH-1's enrolment path, CRYPTO-10's verify subcommand, SIGN-2's send/verify path, and the roster-reload-from-disk path) carries a negative test that feeds a wrong-size public key AND a nil/empty public key, asserting a clean rejection with no panic/crash (run with -race per project convention for anything touching concurrent paths).
4. Documented in CONTRACTS.md or DECISIONS.md as a standing invariant so it is not silently reintroduced by a later call site.

This task should land alongside (or ahead of) AUTH-1/CRYPTO-10/SIGN-2's implementation since it is a prerequisite acceptance criterion on each of them, but is filed separately because it is a security trap spanning multiple call sites, not a single-task scope.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md) — RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed255… (done)
- [SIGN-2](../SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
