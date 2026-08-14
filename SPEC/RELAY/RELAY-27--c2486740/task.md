# RELAY-27: relay error taxonomy collapses ALL FIVE attest sentinels to ErrNoSignerKey/bad_signature -- Go 1.19 blocks the naive multi-%w fix

| Field | Value |
| --- | --- |
| Public id | `c2486740-6f89-4f07-b500-c390b46ffbe4` |
| Key | RELAY-27 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T17:17:54.370211+00:00 |
| Updated | 2026-08-08T19:50:04.816866+00:00 |
| Completed | 2026-08-08T19:50:04.816851+00:00 |

## Proof command

```sh
go test -race -run TestSignedRelayPreservesAttestSentinels ./internal/relay
```

## Status note

code-complete at HEAD (feature-runner), reviewer+security PASS re-verified twice; awaiting integrator commit before completion

## Description

internal/relay/signed.go:306 wraps ONLY the inner error with %v, not %w -- but the re-verification found this is WIDER and HARDER than that single line.

WIDER: probed through the real VerifyRelayed with a trust returning each attest sentinel, ALL FIVE collapse, not just expiry: ErrExpired, ErrUnpinned, ErrAgentIDMismatch, ErrInvalid, ErrVerify -- every one yields errors.Is(err, sentinel)=false, errors.Is(err, ErrNoSignerKey)=true, ErrorCode(err)="bad_signature". A peer is told "forgery" in ALL FIVE cases. Two matter especially: ErrUnpinned is meant to become ErrUnpeeredBus/CodeUnpeeredBus, the only diagnosis with an operator remedy; ErrInvalid is meant to be a 400, not a 403.

HARDER -- must not be lost: the toolchain is Go 1.19.4 in BOTH the dev box and the digest-pinned builder (Dockerfile:15). Go 1.19 does NOT support two %w verbs in one fmt.Errorf: it wraps NEITHER, producing "%!w(*errors.errorString=...)", and errors.Is then fails for BOTH operands. So an implementer making the obvious %v-to-%w change would SILENTLY break errors.Is(err, ErrNoSignerKey) too, which peerErrorCode depends on -- and no positive test catches it (verified: two %w verbs on go1.19.4 wrap neither).

The flattening is DELIBERATE and DOCUMENTED today at signed.go:206-207 ("VerifyRelayed turns any error ... into ErrNoSignerKey"), so undoing it is a CrossBusTrust interface-contract change plus a wire-code decision touching the peerErrorCode allow-list RELAY-9 just tightened at 1e94b2f -- not a one-line fix.

Fix: restructure the error taxonomy at this seam (e.g. a wrapped-error type, or errors.Join if the Go version allows it, or an explicit sentinel-to-sentinel remap) so callers can distinguish all five attest sentinels via errors.Is/ErrorCode, verified on go1.19.4 specifically (not assumed from a newer local toolchain). Add regression tests asserting errors.Is for EACH of the five sentinels, plus one asserting errors.Is(err, ErrNoSignerKey) still holds for the genuine no-signer-key case.

SEQUENCING (per the security gate): this MUST be fixed BEFORE RELAY-17 lands, as its OWN task -- not folded into the keystone, because that is how it gets lost. Blocks RELAY-17 (see relations). A note is posted on RELAY-17 so its implementer sees this before starting.

Originally flagged by the RELAY-14 security gate (P2-4); widened and re-confirmed by the RELAY-14 security RE-VERIFICATION 2026-08-08.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-17](../RELAY-17--817649ce/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-14](../RELAY-14--7db695ee/task.md) — RELAY-14: internal/attest: bus-signed agent-key attestations (done)
- [RELAY-17](../RELAY-17--817649ce/task.md) — RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope (done)
- [RELAY-9](../RELAY-9--06f5e347/task.md) — RELAY-9: Peer error-code allow-list admits the three SIGN-7 codes (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [71cdaef8-c757-4ba9-a693-a8f744070d08](../../TOOLING/proof-check.sh-runs-the-proof-against-its-OWN-script-dir--71cdaef8/task.md) — proof-check.sh runs the proof against its OWN script directory repo root, not the callers… (in_progress)
- [RELAY-27-FU-EXPIRED](../RELAY-27-FU-EXPIRED--e65d1ca5/task.md) — RELAY-27-FU-EXPIRED: attest.ErrExpired and attest.ErrNoClock still answer a peer bad_sign… (todo)
- [RELAY-38](../RELAY-38--4b4beaab/task.md) — RELAY-38: signed-relay-ingest comments and docs are silent on the CodeInvalidRelay path R… (todo)
- [RELAY-39](../RELAY-39--5c0d9653/task.md) — RELAY-39: AST guard pinning TestErrorCodeIsStable's premise -- every sentinel relayhttp.g… (todo)
- [RELAY-40](../RELAY-40--930231f8/task.md) — RELAY-40: accepted residual -- a stateful CrossBusTrust error Is() can evade the RELAY-27… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
