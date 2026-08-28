# CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PROTOCOL.md

| Field | Value |
| --- | --- |
| Public id | `68ff679d-a4e9-4545-afb5-398ca5633a0f` |
| Key | CRYPTO-10 |
| Epic | [CRYPTO](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | agentif |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:41:21.702506+00:00 |
| Updated | 2026-08-14T20:19:10.112639+00:00 |
| Completed | — |

## Proof command

```sh
scripts/bus-wait.sh against a running throwaway bus rejects a tampered message with non-zero exit and empty stdout
```

## Description

RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): this is now VERIFY-ONLY, no decryption. GOVERNED BY INVARIANT 9 -- calls crypto/ed25519.Verify (stdlib, audited, high-level, misuse-resistant) and nothing else; no custom verification logic. THIS IS THE TASK THE USER ACTUALLY ASKED FOR: "a mechanism to validate messages in the agent script before accepting them". Shell cannot do Ed25519, so add a subcommand to the same Go binary (e.g. `agent-bus verify`) that the wrapper shells out to, and wire it into the receive path of the agent-facing wrappers (bus-wait.sh, and bus-agents/bus-send as applicable -- AGENTIF-6/AGENTIF-5) so a message is VERIFIED (per SIGN-1's canonical format, against the sender's messaging public key from CRYPTO-4's bundle/TOFU pin) BEFORE it is handed to the calling agent. Contract: defined stdin/stdout shape; on ANY verification failure exit non-zero and print NOTHING to stdout, so a naive `msg=$(...)` cannot accidentally pass unverified content through. Distinct exit codes per failure mode, at minimum: bad signature (tampered or wrong key), unknown sender (no key binding), duplicate/already-accepted message (message_id-based client-side defense-in-depth only -- freshness itself is enforced SERVER-SIDE AT INGEST by SIGN-4, never by a recipient-side sequence cursor; SIGN-4's prior wording specified exactly that defect and has been corrected -- do not resurrect a sequence-cursor check here), sender identity key CHANGED since pinned (CRYPTO-4's TOFU alarm -- must be loud, never silent), and bundle attestation invalid (bus signature failed). Define where the agent's private key lives and with what file permissions, and refuse to run on world-readable key files. Per invariant 7 the wrapper AND its AGENT_PROTOCOL.md entry ship IN THIS SAME TASK -- a feature without its wrapper is not done. Verify the way an agent would: through scripts/bus-*.sh against a running throwaway bus (own data dir under /tmp), not hand-written curl. NOT NEEDED under this rescope (drop if present in any earlier draft): decrypt/AEAD-open, X3DH session state (no session/handshake required -- verification is stateless given the sender's pinned public key), out-of-order/skipped-key ratchet handling.

ACCEPTANCE CRITERION ADDED 2026-08-02 (RATCHET-7 fallout, verified first-hand by backlog-triage by reading this box's own stdlib source at crypto/ed25519/ed25519.go under GOROOT): ed25519.Verify PANICS (does not return false) when len(publicKey) != ed25519.PublicKeySize -- a remote DoS trap, asymmetric with malformed-signature handling (a bad signature safely returns false, a malformed key does not). This is directly relevant here because CRYPTO-10 verifies attacker-influenceable contact-list/sender public keys, including keys loaded from the roster ON DISK after a restart -- that reload path is also untrusted input and needs the same guard. REQUIRED: the `agent-bus verify` subcommand and its wrapper must length-check every public key against ed25519.PublicKeySize BEFORE calling ed25519.Verify, failing closed (non-zero exit, empty stdout, per this task's existing fail-closed contract) rather than panicking/crashing the process on a malformed key. REQUIRED TEST: a negative test feeding a wrong-size public key and a nil/empty public key (both a freshly-received one and one reloaded from the on-disk roster) through the verify path, asserting a clean non-zero-exit rejection, never a panic. See also the standalone cross-cutting task filed to track this trap across all Verify call sites (AUTH-1, CRYPTO-10, SIGN-2).


RELAY-25 SECURITY AMENDMENT (2026-08-14): the existing `client/wedge_test.go` proves only the verification seam; `client.Client.Read` / `agent-busctl watch` still delivers bodies after `validateBatch` without invoking `verifySignedMessage`, as pinned by `TestReadDoesNotYetVerifyReceivedMessages`. Extend this task's compiled helper/client integration so watch/read validates signatures and trusted sender keys before stdout or an embedding callback, with fail-closed rejection and cursor advancement per SIGN-6. A `fed-smoke.sh` success that observes unsigned/unverified bodies is not evidence of signed federation. This task BLOCKS RELAY-25 (10491a01) for the signature-verification acceptance and must coordinate with SIGN-5/SIGN-6 rather than duplicating their negative cases.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-5](../../AGENTIF/AGENTIF-5--8109ab88/task.md) — AGENTIF-5: scripts/bus-send.sh + AGENT_PROTOCOL.md entry (superseded)
- [AGENTIF-6](../../AGENTIF/AGENTIF-6--31c1257c/task.md) — AGENTIF-6: scripts/bus-wait.sh + AGENT_PROTOCOL.md entry (superseded)
- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CRYPTO-4](../CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md) — RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed255… (done)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (done)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-2](../../SIGN/SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)
- [SIGN-4](../../SIGN/SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side seque… (todo)
- [SIGN-5](../../SIGN/SIGN-5--5cedc580/task.md) — SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must (done)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4eb903f8-04cd-497c-ba4a-7eadceb65725](../../SIGN/SEC-ed25519.Verify-panics-on-wrong-size-public-key-remot--4eb903f8/task.md) — SEC: ed25519.Verify panics on wrong-size public key -- remote DoS across AUTH-1/CRYPTO-10… (todo)
- [AGENTIF-9](../../AGENTIF/AGENTIF-9--4f78ecb1/task.md) — AGENTIF-9: Envelope/schema validation in scripts/bus-*.sh before accepting a server respo… (cancelled)
- [AGENTIF-9](../../CLI/AGENTIF-9--b890e3d6/task.md) — CLI-VALIDATE: envelope/schema validation in the CLIENT before a message is handed to the… (todo)
- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CRYPTO-12](../CRYPTO-12--eb1827ff/task.md) — CRYPTO-12: PROTOCOL.md wire format + CONTRACTS.md for the crypto surface (todo)
- [RATCHET-6](../../RATCHET/RATCHET-6--fd0f3ca3/task.md) — RATCHET-6: RFC 8032 Ed25519 known-answer tests wired into the sign/verify implementation (todo)
- [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md) — RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed255… (done)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (done)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-2](../../SIGN/SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)
- [SIGN-5](../../SIGN/SIGN-5--5cedc580/task.md) — SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must (done)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)
- [SIGN-8](../../SIGN/SIGN-8--71ef73d5/task.md) — SIGN-8: Agent-side messaging key material -- \`agent-bus keygen\`, key file location/permis… (todo)
- [SWEEP-TWO-PASS-DISCIPLINE](../../PROCESS/SWEEP-TWO-PASS-DISCIPLINE--268a0c73/task.md) — SWEEP-TWO-PASS-DISCIPLINE: a contract change needs a MECHANISM sweep and a PROSE sweep, n… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
