# SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus can neither forge nor strip a signature

| Field | Value |
| --- | --- |
| Public id | `aeb90793-c0ac-43d8-b1d3-caa2e6f6a8c1` |
| Key | SIGN-7 |
| Epic | [SIGN](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | crypto |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:11:15.229994+00:00 |
| Updated | 2026-08-08T22:16:37.440318+00:00 |
| Completed | 2026-08-08T22:16:37.440301+00:00 |

## Proof command

```sh
go test -race -count=1 -run '^TestSign7' ./internal/relay
```

## Status note

CODE-COMPLETE at 7b383cf (ancestor of HEAD). This task is the signed-envelope relay primitive; its focused proof is the TestSign7 suite. It remains deliberately unregistered/unserved until the federation composition work. The live two-bus signed delivery acceptance belongs to DEPLOY-3, and the three-bus A->B->C exactly-once/path smoke belongs to RELAY-25; neither criterion is dropped.

## Description

GATED on SIGN-1; implementation lands with RELAY-2/RELAY-3. RAISED TO P1 DESPITE THE RELAY EPIC BEING P2 BECAUSE IT CHANGES A SIGN-1 DECISION: SIGN-1 must not be completed until the question below is answered, or the canonical format will have to be redesigned after code depends on it. THE COLLISION: SIGN-1 wants the server-minted message id and sequence INSIDE the signed bytes (so a malicious bus cannot reorder or misattribute messages undetected). But those are minted by the ORIGIN bus, while the receiving bus needs its own local sequence for its own recipients' cursors (SIGN-4) and, per invariant 1, does not accept ids minted by a client -- and a peer bus IS a client from its perspective. If the far bus re-mints and substitutes, EVERY relayed signature fails at the far end; if it adopts the origin's numbers wholesale, it has ceded id authority to a peer. RESOLVE IT EXPLICITLY. The likely answer -- state it or a better one, and make SIGN-1 match: the signed bytes carry the ORIGIN's fully-qualified sender id and the ORIGIN's message id, which per invariant 2 are already bus-namespaced and therefore globally unambiguous and not the far bus's to mint, while the receiving bus mints its own LOCAL delivery sequence OUTSIDE the signed bytes and binds it in its durable record. (2) NO FORGERY: an intermediate bus cannot forge a message because it does not hold the sender's messaging private key -- but ONLY if the recipient verifies against a key it trusts. CROSS-BUS KEY TRUST IS AN OPEN HOLE: CRYPTO-4's bundle is attested by the LOCAL bus, so bus B attesting a key for bus A's agent means bus B can simply lie and substitute its own key. Decide and document: relay A's attestation intact (bundle signed by A's bus key) and pin A's BUS key at peering time, or TOFU the agent's messaging key at first contact and alarm on change, or both. Without this, cross-bus signatures verify against whatever the nearest bus says, which is worth nothing. (3) NO STRIPPING: SIGN-6's mandatory-signature ingest rule applies to the relay ingest path EXACTLY as it applies to /v1/send. A relayed message arriving with no signature, or with a re-signed one, is rejected -- an unauthenticated downgrade must not be reachable through a peer. (4) NO MUTATION: the relay forwards the signed bytes verbatim. Any normalisation on the path (re-encoding JSON, reordering keys, trimming whitespace, re-framing the body) breaks verification at the far end -- which is a strong argument for SIGN-1 choosing a length-prefixed binary canonical form, or for the relay carrying the exact signed byte string as an opaque blob. Say which. (5) Complements RELAY-3 (traversed-bus-path loop prevention) and IDEM-15 (relay duplicate suppression -- exactly-once APPLICATION on the relay path; this gloss pointed at IDEM-7 until the 2026-08-02 duplicate-epic merge superseded IDEM-1..9 and folded IDEM-7's origin-identity dedupe and non-forgeability content into IDEM-15): the bus path is metadata OUTSIDE the signature and grows on every hop, so it can never be inside the signed bytes -- state that explicitly, since it means the path is unauthenticated and a lying peer can rewrite it (loop prevention is availability, not security). TESTS: signed on A, verifies for a recipient on B; strip the signature in transit -> rejected at B's ingest; mutate one byte of a signed field in transit -> the recipient's verification fails and the body is never delivered; the far bus's local sequence differs from the origin's without breaking verification.

=== AUDIT 2026-08-08 (spec-keeper): PARTIALLY SHIPPED -- the split, explicitly ===
MET (in main): the signed-envelope preservation code and its tests landed at commit 7b383cf
("SIGN-7: relay preserves the signed envelope, by RE-DERIVATION not a blob"), verified an ancestor
of HEAD efde70c. internal/relay/signed_test.go carries nine TestSign7* tests, including
SignedOnAVerifiesForARecipientOnB, StrippedSignatureIsRejectedAtIngest,
MutatedFieldNeverReachesDelivery, LocalDeliverySequenceIsOutsideTheSignedBytes and
ForwardIsVerbatimAcrossTwoHops. The old status_note "CODE-COMPLETE, awaiting the orchestrator's
commit" is STALE: it IS committed.
NOT MET: the proof_cmd's second clause -- "a message signed on bus A verifies unmodified for a
recipient on bus B using the DEPLOY-3 two-bus Compose profile". At HEAD nothing outside
internal/relay imports internal/relay (the only cross-package mentions are comments at
cmd/agent-bus/suffixfloors.go:84 and internal/httpapi/messages.go:97), so the surface is registered
on no mux and no running bus can exhibit this. Gated behind INVITE-PEERGUARD (f5d91dbe, todo) and
MTLS-RELAYGUARD (8192c3c7, todo).
PROOF_CMD IS VACUOUS ON ITS FIRST CLAUSE TOO: TestRelayPreservesSignature does not exist anywhere at
HEAD. Retarget to `go test -race -run TestSign7 ./internal/relay` before anyone attempts to close
this, and keep the live cross-bus clause as the thing that holds it open.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-17](../../RELAY/RELAY-17--817649ce/task.md)
- **relates to** [IDEM-7](../../IDEM/IDEM-7--1c490a08/task.md)
- **relates to** [SIGN-1](../SIGN-1--43fd21ae/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [DEPLOY-3](../../DEPLOY/DEPLOY-3--9eaf2d19/task.md) — DEPLOY-3: multi-bus Compose profile (2+ peered buses) for RELAY end-to-end testing (todo)
- [IDEM-1](../../IDEM/IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-15](../../IDEM/IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [IDEM-7](../../IDEM/IDEM-7--1c490a08/task.md) — IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, co… (superseded)
- [INVITE-PEERGUARD](../../INVITE/INVITE-PEERGUARD--f5d91dbe/task.md) — INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate t… (todo)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [RELAY-2](../../RELAY/RELAY-2--654140d7/task.md) — RELAY-2: Message relay + ongoing roster sync across peers (done)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-3](../../RELAY/RELAY-3--e944edda/task.md) — RELAY-3: Loop prevention via traversed-bus path (done)
- [SIGN-1](../SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-4](../SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- server-minted monotonic sequence + recipient-side cursor (todo)
- [SIGN-6](../SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-CONTRACTS-PARKING](../../CONTEXT/CONTEXT-CONTRACTS-PARKING--881dae01/task.md) — CONTEXT-CONTRACTS-PARKING: CONTRACTS.md admits, in its own text, that it is 90% parking l… (todo)
- [CRYPTO-9](../../CRYPTO/CRYPTO-9--0a4562fc/task.md) — CRYPTO-9: Cross-bus relay of encrypted messages -- what an intermediate bus can and canno… (deferred)
- [IDEM-15](../../IDEM/IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [IDEM-7](../../IDEM/IDEM-7--1c490a08/task.md) — IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, co… (superseded)
- [RELAY-17](../../RELAY/RELAY-17--817649ce/task.md) — RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope (done)
- [SIGN-6](../SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)
- [c716f8e7-ad9c-4af9-9fac-1bdb75c8f900](../../DOCS/PROTOCOL.md-1002-says-internal-relay-is-imported-by-noth--c716f8e7/task.md) — PROTOCOL.md:1002 says internal/relay is 'imported by nothing' -- false since ed77bba (int… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
