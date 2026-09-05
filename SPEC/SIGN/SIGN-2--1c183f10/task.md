# SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message)

| Field | Value |
| --- | --- |
| Public id | `1c183f10-f079-4c62-895e-93f286e050fb` |
| Key | SIGN-2 |
| Epic | [SIGN](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | crypto |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:58:29.534931+00:00 |
| Updated | 2026-08-02T13:56:51.526386+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestSendSigns ./internal/... ; scripts/bus-send.sh against a running throwaway bus produces a message whose signature verifies against the sender's registered messaging pubkey
```

## Description

GATED on SIGN-1 (canonical format) and CRYPTO-3 (messaging keypair minted at enrolment). Supersedes the encryption-specific CRYPTO-6 (Double Ratchet encrypt on the DM path), which is superseded outright -- there is no ratchet and no ciphertext. What this task implements instead: the sender signs SIGN-1's canonical serialisation of the outgoing message with its messaging Ed25519 PRIVATE key (crypto/ed25519.Sign -- stdlib, audited, high-level API; invariant 9 -- no custom signing code) and the resulting detached signature travels alongside the plaintext body in the envelope. The private key never leaves the agent's machine and is never sent to the bus. Because SIGN-1 may require the signature to cover the server-minted message id/sequence (see SIGN-1's open question), specify and implement the exact ordering this requires: either (a) the client obtains the id/sequence first (e.g. a reserve-then-send two-step) and then signs, or (b) the client signs everything it controls and the durable record binds the server-minted id/sequence to that signature non-repudiably without them being literally inside the signed bytes -- pick the option SIGN-1 settled on. Wire this into the SAME Go binary used by scripts/bus-send.sh (invariant 7 -- shell cannot do Ed25519, so add a subcommand, e.g. `agent-bus sign`, that the wrapper shells out to) -- ship the wrapper change and any AGENT_PROTOCOL.md update IN THIS SAME TASK. The bus stores and forwards the signature as opaque bytes; it MAY optionally check the signature is well-formed (right length) but verification is the RECIPIENT's job (SIGN-1's epic note on why -- a malicious bus must not be able to forge on behalf of a sender it does not control, and equally must not be trusted to police messages against senders it does not control either). No new key material beyond CRYPTO-3's existing messaging keypair. Test via scripts/bus-send.sh against a running throwaway bus, not hand-written curl.

ACCEPTANCE CRITERION ADDED 2026-08-02 (RATCHET-7 fallout, verified first-hand by backlog-triage by reading this box's own stdlib source at crypto/ed25519/ed25519.go under GOROOT): ed25519.Verify PANICS (does not return false) when len(publicKey) != ed25519.PublicKeySize -- a remote DoS trap, asymmetric with malformed-signature handling (a bad signature safely returns false, a malformed key does not). Any verification this task's send path performs or triggers downstream (including recipient-side verification against a sender's messaging public key, and any self-check before accepting a signature as well-formed) must length-check the public key against ed25519.PublicKeySize BEFORE calling ed25519.Verify, and must fail closed on mismatch rather than panic. REQUIRED TEST: a negative test feeding a wrong-size and a nil/empty public key through this path, proving no panic. See also the standalone cross-cutting task filed to track this trap across all Verify call sites (AUTH-1, CRYPTO-10, SIGN-2).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md)
- **blocked by** [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md)
- **blocked by** [SIGN-1](../SIGN-1--43fd21ae/task.md)
- **supersedes** [CRYPTO-6](../../CRYPTO/CRYPTO-6--260e6003/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [CRYPTO-6](../../CRYPTO/CRYPTO-6--260e6003/task.md) — CRYPTO-6: Double Ratchet encrypt/decrypt on the direct-message send path (deferred)
- [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md) — RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed255… (done)
- [SIGN-1](../SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4eb903f8-04cd-497c-ba4a-7eadceb65725](../SEC-ed25519.Verify-panics-on-wrong-size-public-key-remot--4eb903f8/task.md) — SEC: ed25519.Verify panics on wrong-size public key -- remote DoS across AUTH-1/CRYPTO-10… (todo)
- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-11](../../CRYPTO/CRYPTO-11--0047e5b7/task.md) — CRYPTO-11: Audit-log content hash for signed (cleartext) messages -- implements the invar… (todo)
- [CRYPTO-2](../../CRYPTO/CRYPTO-2--0ad37da2/task.md) — CRYPTO-2: Adopt the crypto primitive layer chosen by the spike (internal/cryptobox + go.m… (superseded)
- [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [CRYPTO-6](../../CRYPTO/CRYPTO-6--260e6003/task.md) — CRYPTO-6: Double Ratchet encrypt/decrypt on the direct-message send path (deferred)
- [ID-2-WIRING-OBSERVER](../../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [RATCHET-6](../../RATCHET/RATCHET-6--fd0f3ca3/task.md) — RATCHET-6: RFC 8032 Ed25519 known-answer tests wired into the sign/verify implementation (todo)
- [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md) — RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed255… (done)
- [SIGN-1](../SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-3](../SIGN-3--f2daa6bc/task.md) — SIGN-3: Broadcast signature covers the recipient set (prevents split-content broadcasts) (todo)
- [SIGN-5](../SIGN-5--5cedc580/task.md) — SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must (done)
- [SIGN-6](../SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
