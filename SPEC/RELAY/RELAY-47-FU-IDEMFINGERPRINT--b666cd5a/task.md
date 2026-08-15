# RELAY-47-FU-IDEMFINGERPRINT: the ENFORCED idempotency fingerprint is not the one internal/relay documents -- a permuted recipient array makes an HONEST peer earn a 409 protocol-violation

| Field | Value |
| --- | --- |
| Public id | `b666cd5a-a11f-40c7-a36f-bc0c6475d044` |
| Key | RELAY-47-FU-IDEMFINGERPRINT |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | relay |
| Section | backlog |
| Tags | relay, idempotency, invariant-10, from-review, relay-47-followup, needs-reconfirmation |
| Created | 2026-08-15T12:55:49.988097+00:00 |
| Updated | 2026-08-15T12:55:49.988097+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestPublishFingerprintIsRecipientOrderIndependent ./internal/hub
```

## Description

Filed 2026-08-15 by spec-keeper on behalf of the RELAY-47 feature-runner. Found by the RELAY-47 review/security gates; out of RELAY-47's boundary (the enforcement site is internal/hub).

== READ THIS FIRST -- THE READING MUST BE RE-CONFIRMED ==

**`internal/hub/hub.go` and `internal/idem/store.go` were BEING REWRITTEN by a concurrent agent when this was found (2026-08-15).** Every line reference below is against the tree as it stood then. **Re-confirm the whole finding against COMMITTED code before acting on it** -- if the rewrite already sorts recipients, close this as obsolete rather than 'fixing' it twice.

== THE FINDING ==

`relay.RelayedMessage.Fingerprint` (internal/relay/message.go:723) is COMPUTED AND NEVER READ. What actually decides `OutcomeRetry` vs `OutcomeViolation` is `hub.publishFingerprint` (internal/hub/hub.go, approx. 2024-2035) = (op, count, **recipients IN WIRE ORDER**, body). No timestamp; not sorted.

`signing.Canonicalize` sorts a COPY of the recipient list, so a PERMUTED recipient array is still VALIDLY SIGNED. Therefore a malicious intermediate bus can reorder the recipients of a message it relays, arrive first, and cause an HONEST peer's byte-identical copy to earn a **409 idempotency VIOLATION** plus a WARN line attributing a protocol violation to that honest peer.

== SEVERITY -- WHAT IT IS AND IS NOT ==

- No disconnect fires, so **invariant 10 is respected** (this is the same-key-different-payload arm, which must reject and log but not disconnect).
- Termination is unaffected; there is no amplification.
- What it IS: **misattribution** (an honest peer is logged as a protocol violator, which poisons exactly the signal an operator would use to find a malicious peer) plus delivery noise.

Two defects, and the task should settle BOTH:
1. a computed-and-never-read field (`RelayedMessage.Fingerprint`) that documents a contract the system does not enforce -- either wire it or delete it, but do not leave a decoy;
2. an order-sensitive fingerprint over a set whose signature is order-INSENSITIVE. Those two must agree.

== SUGGESTED RESOLUTION ==

Make the enforced fingerprint order-independent over the recipient SET, matching `signing.Canonicalize` (sort a copy -- never mutate the caller's slice, which would change the bytes the signature covers). Record the choice in DECISIONS.md.

== PROOF ==

    go test -race -run TestPublishFingerprintIsRecipientOrderIndependent ./internal/hub

Two identical sends whose recipient arrays differ only in ORDER must produce the SAME fingerprint and therefore `OutcomeRetry` (a legitimate retry: return the original result, do not re-apply, do not disconnect) -- never `OutcomeViolation`. Confirm RED before the fix.

**If the task is resolved the other way** (the enforcement is deemed correct and it is `internal/relay`'s DOCS + the dead field that are wrong), this `proof_cmd` is wrong for the work and MUST be changed through spec-keeper to a doc proof that pins the specific corrected line -- do not complete this task against a proof that was never run.

RELATES: RELAY-47, IDEM-14, IDEM-12, SIGN-1, SIGN-7.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up of** [RELAY-47](../RELAY-47--dd69c4d3/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-12](../../IDEM/IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (todo)
- [IDEM-14](../../IDEM/IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [RELAY-47](../RELAY-47--dd69c4d3/task.md) — RELAY-47: ONWARD RELAY -- WIRE an intermediate bus to forward a relayed message to a THIR… (done)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
