# CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles

| Field | Value |
| --- | --- |
| Public id | `13f3947e-6959-4c57-9a42-f3786cc57d6f` |
| Key | CRYPTO-4 |
| Epic | [CRYPTO](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:41:20.267201+00:00 |
| Updated | 2026-08-02T13:08:02.887021+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestKeyBundle ./internal/httpapi
```

## Description

RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): this is now a bundle of SIGNING material, not X3DH session-establishment material. GOVERNED BY INVARIANT 9 -- the bus attests bundles by signing them with its own Ed25519 signing key (crypto/ed25519, stdlib, audited); no custom attestation construction. Add the authenticated route that lets an enrolled agent fetch another agent's messaging (signing) key bundle: {fully-qualified <bus-id>.<agent-id>, messaging public key, key_epoch, issued_at}, signed by a bus signing key so the caller can verify the bus is vouching for this binding. Route is keyed by the fully-qualified id (invariant 2). Requires auth (invariant 3): an unenrolled caller gets 401; consider whether roster enumeration via this route needs rate-limiting or scoping. PLUS mandatory TOFU pinning: a recipient pins a peer's messaging public key on first use, in a local pin file; if the bus later serves a DIFFERENT key for a peer whose key is already pinned, that is a hard failure (never an auto-accept, never a silent re-pin) -- this is the actual defence against a malicious bus MITM-ing an established relationship, since attestation alone only protects first contact. Re-pinning requires an explicit human-driven trust command with an out-of-band comparison. key_epoch is bumped by the server on AUTH-4 leave/revocation and invalidates outstanding bundles. Any on-disk record type number or wire protocol version this needs MUST be reserved via POST /api/v1/projects/agent-bus/reservations -- never hand-picked. NOT NEEDED under this rescope (drop if present in any earlier draft): signed prekeys, one-time prekeys, prekey replenishment/exhaustion policy -- those were X3DH-specific and there is no X3DH; this bundle carries exactly one long-lived signing public key per agent.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-4](../../AUTH/AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-10](../CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-12](../CRYPTO-12--eb1827ff/task.md) — CRYPTO-12: PROTOCOL.md wire format + CONTRACTS.md for the crypto surface (todo)
- [CRYPTO-3](../CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [CRYPTO-5](../CRYPTO-5--9f3f8065/task.md) — CRYPTO-5: X3DH session establishment between two agents (deferred)
- [DISCOVERY-DOC](../../CORE/DISCOVERY-DOC--2d7ce37b/task.md) — DISCOVERY-DOC: self-describing unauthenticated discovery document so an agent with only a… (in_progress)
- [IDEM-13](../../IDEM/IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-6](../../IDEM/IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)
- [RATCHET-2](../../RATCHET/RATCHET-2--ade31a62/task.md) — RATCHET-2: Threat model -- what Ed25519 signing defends against, and explicitly what it d… (todo)
- [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md) — RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed255… (done)
- [RELAY-13-FU-DOCS](../../RELAY/RELAY-13-FU-DOCS--7f3a4b80/task.md) — RELAY-13-FU-DOCS: three docs/comments assert the opposite of shipped RELAY-13 behaviour -… (done)
- [RELAY-13-FU-MSGKEYPOP](../../RELAY/RELAY-13-FU-MSGKEYPOP--59db5455/task.md) — RELAY-13-FU-MSGKEYPOP: no proof-of-possession of the messaging private key at enrolment,… (todo)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)
- [SIGN-8](../../SIGN/SIGN-8--71ef73d5/task.md) — SIGN-8: Agent-side messaging key material -- \`agent-bus keygen\`, key file location/permis… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
