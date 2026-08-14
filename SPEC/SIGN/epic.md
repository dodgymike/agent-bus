# EPIC SIGN — SIGN: message authenticity & integrity (Ed25519 sign/verify, no encryption yet)

[← all epics](../../SPEC.md)

**8 open / 13 total.** Full records live in `SPEC/SIGN/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (8)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| SIGN-1-FU-REORDER-WATERMARK | SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… | todo | P0 | [task.md](SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) | blocks [RELAY-24](../RELAY/RELAY-24--e303c624/task.md)<br>supersedes [SIGN-1-FU-REORDER-WATERMARK](SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) | [SIGN-1-FU-OUTOFORDER-POISON](SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) [SIGN-1](SIGN-1--43fd21ae/task.md) [RELAY-24](../RELAY/RELAY-24--e303c624/task.md) [RELAY-25](../RELAY/RELAY-25--10491a01/task.md) [SIGN-1-FU-REORDER-WATERMARK](SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) [CONTEXT-KEY-IDENTITY](../CONTEXT/CONTEXT-KEY-IDENTITY--73dec684/task.md) |
| 4eb903f8-04cd-497c-ba4a-7eadceb65725 | SEC: ed25519.Verify panics on wrong-size public key -- remote DoS across AUTH-1/CRYPTO-10… | todo | P1 | [task.md](SEC-ed25519.Verify-panics-on-wrong-size-public-key-remot--4eb903f8/task.md) | — | [RATCHET-7](../RATCHET/RATCHET-7--aaa7cddc/task.md) [AUTH-1](../AUTH/AUTH-1--54fa94c0/task.md) [CRYPTO-10](../CRYPTO/CRYPTO-10--68ff679d/task.md) [SIGN-2](SIGN-2--1c183f10/task.md) |
| SIGN-1-FU-STORE-LOGGER | SIGN-1-FU-STORE-LOGGER: pass the hub's configured logger into store.New so the invariant-… | todo | P1 | [task.md](SIGN-1-FU-STORE-LOGGER--50081b3c/task.md) | — | [SIGN-1-FU-OUTOFORDER-POISON](SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) |
| SIGN-2 | SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) | todo | P1 | [task.md](SIGN-2--1c183f10/task.md) | blocked by [CRYPTO-3](../CRYPTO/CRYPTO-3--dd1066af/task.md)<br>blocked by [RATCHET-7](../RATCHET/RATCHET-7--aaa7cddc/task.md)<br>blocked by [SIGN-1](SIGN-1--43fd21ae/task.md)<br>supersedes [CRYPTO-6](../CRYPTO/CRYPTO-6--260e6003/task.md) | [SIGN-1](SIGN-1--43fd21ae/task.md) [CRYPTO-3](../CRYPTO/CRYPTO-3--dd1066af/task.md) [CRYPTO-6](../CRYPTO/CRYPTO-6--260e6003/task.md) [RATCHET-7](../RATCHET/RATCHET-7--aaa7cddc/task.md) [AUTH-1](../AUTH/AUTH-1--54fa94c0/task.md) [CRYPTO-10](../CRYPTO/CRYPTO-10--68ff679d/task.md) |
| SIGN-4 | SIGN-4: Replay/freshness -- server-minted monotonic sequence + recipient-side cursor | todo | P1 | [task.md](SIGN-4--33fa35d8/task.md) | relates to [IDEM-5](../IDEM/IDEM-5--9631dfcb/task.md) | [SIGN-1](SIGN-1--43fd21ae/task.md) [RATCHET-2](../RATCHET/RATCHET-2--ade31a62/task.md) |
| SIGN-6 | SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… | todo | P1 | [task.md](SIGN-6--c9e4aea1/task.md) | blocked by [SIGN-1](SIGN-1--43fd21ae/task.md)<br>blocks [RELAY-25](../RELAY/RELAY-25--10491a01/task.md)<br>relates to [IDEM-4](../IDEM/IDEM-4--d9c00d0d/task.md) | [SIGN-1](SIGN-1--43fd21ae/task.md) [SIGN-2](SIGN-2--1c183f10/task.md) [MSG-2](../MSG/MSG-2--50995c75/task.md) [MSG-3](../MSG/MSG-3--2655c6ae/task.md) [CRYPTO-4](../CRYPTO/CRYPTO-4--13f3947e/task.md) [SIGN-4](SIGN-4--33fa35d8/task.md) +4 more |
| SIGN-8 | SIGN-8: Agent-side messaging key material -- \`agent-bus keygen\`, key file location/permis… | todo | P1 | [task.md](SIGN-8--71ef73d5/task.md) | blocked by [CRYPTO-3](../CRYPTO/CRYPTO-3--dd1066af/task.md)<br>blocked by [SIGN-1](SIGN-1--43fd21ae/task.md) | [CRYPTO-3](../CRYPTO/CRYPTO-3--dd1066af/task.md) [AGENTIF-2](../AGENTIF/AGENTIF-2--15e4509c/task.md) [CRYPTO-10](../CRYPTO/CRYPTO-10--68ff679d/task.md) [CORE-10](../CORE/CORE-10--27ad23ef/task.md) [CRYPTO-4](../CRYPTO/CRYPTO-4--13f3947e/task.md) [AUTH-1](../AUTH/AUTH-1--54fa94c0/task.md) +1 more |
| SIGN-3 | SIGN-3: Broadcast signature covers the recipient set (prevents split-content broadcasts) | todo | P2 | [task.md](SIGN-3--f2daa6bc/task.md) | supersedes [CRYPTO-8](../CRYPTO/CRYPTO-8--2b1068eb/task.md)<br>supersedes [RATCHET-4](../RATCHET/RATCHET-4--58fd8bc3/task.md) | [SIGN-1](SIGN-1--43fd21ae/task.md) [SIGN-2](SIGN-2--1c183f10/task.md) [CRYPTO-8](../CRYPTO/CRYPTO-8--2b1068eb/task.md) [RATCHET-4](../RATCHET/RATCHET-4--58fd8bc3/task.md) [MSG-2](../MSG/MSG-2--50995c75/task.md) [AGENTIF-4](../AGENTIF/AGENTIF-4--715fc1b8/task.md) |

## Closed tasks (5) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| SIGN-1-FU-OUTOFORDER-POISON | SIGN-1-FU-OUTOFORDER-POISON: Reserve-then-send lets mints be spent out of order, which pe… | done | P0 | [task.md](SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) | blocks [RELAY-24](../RELAY/RELAY-24--e303c624/task.md)<br>blocks [RELAY-25](../RELAY/RELAY-25--10491a01/task.md) | [RELAY-24-BLOCKER-HUBINGEST](../RELAY/RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) [RELAY-24](../RELAY/RELAY-24--e303c624/task.md) [RELAY-25](../RELAY/RELAY-25--10491a01/task.md) [SIGN-1](SIGN-1--43fd21ae/task.md) [RELAY-17](../RELAY/RELAY-17--817649ce/task.md) |
| SIGN-1-FU-REORDER-WATERMARK | SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… | superseded | P0 | [task.md](SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) | superseded by SIGN-1-FU-REORDER-WATERMARK (unresolved) | [SIGN-1-FU-OUTOFORDER-POISON](SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) [SIGN-1](SIGN-1--43fd21ae/task.md) [RELAY-24](../RELAY/RELAY-24--e303c624/task.md) [RELAY-25](../RELAY/RELAY-25--10491a01/task.md) [SIGN-1-FU-REORDER-WATERMARK](SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) |
| SIGN-1 | SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) | done | P1 | [task.md](SIGN-1--43fd21ae/task.md) | blocks [SIGN-2](SIGN-2--1c183f10/task.md)<br>blocks [SIGN-6](SIGN-6--c9e4aea1/task.md)<br>blocks [SIGN-8](SIGN-8--71ef73d5/task.md)<br>relates to [RATCHET-2](../RATCHET/RATCHET-2--ade31a62/task.md)<br>relates to [RATCHET-6](../RATCHET/RATCHET-6--fd0f3ca3/task.md)<br>relates to [RATCHET-7](../RATCHET/RATCHET-7--aaa7cddc/task.md)<br>+1 more (see task.md) | [SIGN-2](SIGN-2--1c183f10/task.md) [SIGN-5](SIGN-5--5cedc580/task.md) [CRYPTO-10](../CRYPTO/CRYPTO-10--68ff679d/task.md) [CRYPTO-3](../CRYPTO/CRYPTO-3--dd1066af/task.md) [RATCHET-7](../RATCHET/RATCHET-7--aaa7cddc/task.md) [DUR-5](../DUR/DUR-5--a7123e88/task.md) |
| SIGN-5 | SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must | done | P1 | [task.md](SIGN-5--5cedc580/task.md) | blocks [CRYPTO-10](../CRYPTO/CRYPTO-10--68ff679d/task.md) | [SIGN-1](SIGN-1--43fd21ae/task.md) [SIGN-2](SIGN-2--1c183f10/task.md) [CRYPTO-10](../CRYPTO/CRYPTO-10--68ff679d/task.md) [SIGN-4](SIGN-4--33fa35d8/task.md) |
| SIGN-7 | SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… | done | P1 | [task.md](SIGN-7--aeb90793/task.md) | blocks [RELAY-17](../RELAY/RELAY-17--817649ce/task.md)<br>relates to [IDEM-7](../IDEM/IDEM-7--1c490a08/task.md)<br>relates to [SIGN-1](SIGN-1--43fd21ae/task.md) | [SIGN-1](SIGN-1--43fd21ae/task.md) [RELAY-2](../RELAY/RELAY-2--654140d7/task.md) [RELAY-3](../RELAY/RELAY-3--e944edda/task.md) [SIGN-4](SIGN-4--33fa35d8/task.md) [CRYPTO-4](../CRYPTO/CRYPTO-4--13f3947e/task.md) [SIGN-6](SIGN-6--c9e4aea1/task.md) +7 more |

## Epic description

RESCOPE, user instruction verbatim (2026-08-02): "ok, let's keep it simple and just use standard message auth/integrity using libsodium. encryption can come later." This SUPERSEDES the whole Signal/ratchet direction explored in CRYPTO-1's CRYPTO_DEEPDIVE.md: NO X3DH, NO Double Ratchet, NO forward secrecy, NO encryption of any kind for now. What ships is message AUTHENTICITY and INTEGRITY via Ed25519 detached signatures (crypto_sign in libsodium terms; Go's stdlib crypto/ed25519 implements the identical RFC 8032 primitive and needs no toolchain bump -- it has been in Go since 1.13 and works fine on go1.19.4).

GOVERNED BY INVARIANT 9 (never write your own crypto; always use a well-known, standard, audited library, preferring one that wraps as much of the problem as possible -- this OVERRIDES invariant 8 stdlib-first where they conflict). NO task in this epic may implement a primitive, a signature scheme, a hash, or any bespoke construction -- every task calls an audited library's high-level Sign/Verify API and nothing more. If no suitable library exists for some sub-problem, the required outcome is to change the requirement or escalate to the user -- never to write it ourselves.

WHY THIS IS THE SECURITY VALUE (say it explicitly): enrolment already mints TWO keypairs (AUTH-1: an AUTH keypair that authenticates HTTP calls TO the bus; CRYPTO-3: a MESSAGING keypair). The bus verifies AUTH keys, but a message SIGNATURE is verified by the RECIPIENT -- so a compromised or malicious bus cannot forge a message from agent A, even though it relays every message in cleartext. That asymmetry is the whole point of keeping the two keypairs separate.

WHAT SIGNATURES DO NOT PROTECT AGAINST (accepted, not an oversight -- see rescoped RATCHET-2): confidentiality. Without encryption, the bus and any relay peer on a multi-bus path can read every message body. This is now an accepted property of the system, to be stated plainly in PROTOCOL.md, not discovered later.

WHAT SIGNATURES DO NOT PROTECT AGAINST EITHER: replay. A validly signed message can be replayed verbatim by anyone who saw it once (including a malicious bus). Addressed via the server-minted monotonic sequence plus a recipient-side delivery cursor -- see SIGN-4.

TASK MAP -- this epic holds the NEW work; related coverage lives in rescoped CRYPTO/RATCHET tasks (not duplicated here):
  - SIGN-1: canonical signing format (blocks everything below)
  - SIGN-2: sign on the send path
  - SIGN-3: broadcast signature over the recipient set (replaces the superseded CRYPTO-8 / RATCHET-4 scope)
  - SIGN-4: replay/freshness via sequence + cursor
  - SIGN-5: mandatory negative-test suite (tampered body, swapped sender, replayed message, wrong key, truncated signature -- all REJECTED, proof for each; invariant 9 makes this MANDATORY because a verifier that accepts everything passes every positive test ever written)
  - Key generation + binding: CRYPTO-3 (rescoped, kept)
  - Key-bundle distribution + TOFU pinning: CRYPTO-4 (rescoped, kept)
  - Verification on the receive path + scripts/bus-*.sh validate-before-accept + AGENT_PROTOCOL.md: CRYPTO-10 (rescoped, kept)
  - RFC 8032 known-answer tests: RATCHET-6 (rescoped, kept) -- MANDATORY per invariant 9
  - Threat model (what signing does/does not defend against): RATCHET-2 (rescoped, kept)
  - Supply-chain confirmation that the chosen library is genuinely well-known/maintained/audited with a high-level misuse-resistant API: RATCHET-7 (rescoped, kept)
  - Audit-log content-hash pairs with the signature for non-repudiation: CRYPTO-11 (already settled, kept)

All numbered task keys reserved via POST /api/v1/projects/agent-bus/reservations, namespace task-key-SIGN -- never hand-picked.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
