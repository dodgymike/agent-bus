# SIGN-1: Canonical signing format for messages (Ed25519 detached signatures)

| Field | Value |
| --- | --- |
| Public id | `43fd21ae-e2d4-45e6-aede-4023df29299d` |
| Key | SIGN-1 |
| Epic | [SIGN](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | crypto |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:58:11.375102+00:00 |
| Updated | 2026-08-02T23:43:41.682717+00:00 |
| Completed | 2026-08-02T23:43:41.682701+00:00 |

## Proof command

```sh
go test -race -run TestCanonicalize ./internal/...
```

## Status note

DISPATCHED by triage-20260802-b1f-breadth (breadth-first feature pass, user instruction). Boundary is a SINGLE package, disjoint from the in-flight MSG/POLL (hub,store,httpapi) and CLI (client,cmd/busctl) waves.

## Description

RESCOPE (supersedes the Signal/ratchet direction, user instruction 2026-08-02: "ok, let's keep it simple and just use standard message auth/integrity using libsodium. encryption can come later"). GOVERNED BY INVARIANT 9 (never write your own crypto; always use a well-known, standard, audited library that wraps as much of the problem as possible -- this OVERRIDES invariant 8 where they conflict). This task specifies the EXACT bytes a sender signs and a recipient verifies -- the sharp edge of the whole epic: if sender and verifier serialise differently, verification fails intermittently or, worse, a field outside the signed bytes becomes silently forgeable. Deliverable: a written spec (in PROTOCOL.md or a dedicated section) plus a canonicalize() function pinned by test vectors, naming EXACTLY which fields are covered and in what order/encoding -- at minimum: message id (server-minted), sequence (server-minted, monotonic), fully-qualified sender id (<bus-id>.<agent-id>), fully-qualified recipient id(s) (sorted, for determinism), timestamp, and the message body. State explicitly which fields are server-minted vs sender-supplied, since a server-minted field being outside the signed bytes would let a malicious bus reorder/misattribute messages undetected -- so the id and sequence MUST be inside the signed bytes even though the sender does not choose them (the sender signs the server's assignment as part of the accept flow, OR the design places signing before minting and the signature covers only sender-known fields with the id/seq bound separately by the durable record -- DECIDE and document which, do not leave it ambiguous). We do NOT invent a signing construction: canonical bytes are handed to the library's Sign/Verify API (Go stdlib crypto/ed25519 -- crypto/ed25519.Sign / crypto/ed25519.Verify, the audited, high-level, misuse-resistant sign/verify API for RFC 8032 Ed25519) and NOTHING else -- no custom padding, no hand-rolled length framing beyond a documented fixed field order, no bespoke hashing construction assembled ourselves. Include a table of the exact byte layout (fixed-order concatenation with length-prefixed variable fields, or a documented canonical JSON form -- pick ONE, deterministic, and say why) and a handful of worked test vectors (input struct -> exact signed bytes -> hex) that SIGN-2/SIGN-5 and CRYPTO-10 depend on. BLOCKS every other SIGN task and the CRYPTO-3/4/10 rescopes' implementation.

CONSTRAINT ADDED 2026-08-02 (RATCHET-7 fallout): Ed25519 signs the message itself, never a digest -- crypto/ed25519's Sign/Verify API rejects pre-hashed input for Ed25519 (there is no PureEdDSA-over-a-hash mode exposed; feeding it a hash instead of the message is a misuse of the API, not a supported shortcut). Because DUR-5 defines an audit-log content hash and SIGN-2 defines the signature, and the two are specced in separate epics, they will drift apart unless pinned together here: SIGN-1's canonicalize() output -- the exact canonical byte sequence -- MUST be the single shared input that (a) SIGN-2 passes to ed25519.Sign/ed25519.Verify UNHASHED, and (b) DUR-5 hashes for its audit-log content hash. Do not let DUR-5 hash a differently-serialised or differently-ordered view of the same logical message; if DUR-5's audit record needs additional fields beyond what SIGN-1 signs, those extra fields must be clearly out-of-band from (not silently substituted for) the canonical signed bytes. State this explicitly in the PROTOCOL.md deliverable and cross-reference DUR-5 by name so the two epics do not drift.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [SIGN-2](../SIGN-2--1c183f10/task.md)
- **blocks** [SIGN-6](../SIGN-6--c9e4aea1/task.md)
- **blocks** [SIGN-8](../SIGN-8--71ef73d5/task.md)
- **relates to** [RATCHET-2](../../RATCHET/RATCHET-2--ade31a62/task.md)
- **relates to** [RATCHET-6](../../RATCHET/RATCHET-6--fd0f3ca3/task.md)
- **relates to** [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md)
- **relates to** [SIGN-7](../SIGN-7--aeb90793/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [DUR-5](../../DUR/DUR-5--a7123e88/task.md) — DUR-5: Append-only message audit log (done)
- [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md) — RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed255… (done)
- [SIGN-2](../SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)
- [SIGN-5](../SIGN-5--5cedc580/task.md) — SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-KEY-IDENTITY](../../CONTEXT/CONTEXT-KEY-IDENTITY--73dec684/task.md) — CONTEXT-KEY-IDENTITY: Standardize task identity (public_id vs key) before SPEC/&lt;epic&gt;/&lt;ta… (todo)
- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-11](../../CRYPTO/CRYPTO-11--0047e5b7/task.md) — CRYPTO-11: Audit-log content hash for signed (cleartext) messages -- implements the invar… (todo)
- [CRYPTO-12](../../CRYPTO/CRYPTO-12--eb1827ff/task.md) — CRYPTO-12: PROTOCOL.md wire format + CONTRACTS.md for the crypto surface (todo)
- [CRYPTO-2](../../CRYPTO/CRYPTO-2--0ad37da2/task.md) — CRYPTO-2: Adopt the crypto primitive layer chosen by the spike (internal/cryptobox + go.m… (superseded)
- [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [IDEM-1](../../IDEM/IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-10](../../IDEM/IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-15](../../IDEM/IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [IDEM-7](../../IDEM/IDEM-7--1c490a08/task.md) — IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, co… (superseded)
- [RATCHET-6](../../RATCHET/RATCHET-6--fd0f3ca3/task.md) — RATCHET-6: RFC 8032 Ed25519 known-answer tests wired into the sign/verify implementation (todo)
- [RATCHET-7](../../RATCHET/RATCHET-7--aaa7cddc/task.md) — RATCHET-7: Choose and supply-chain-review the Ed25519 implementation (stdlib crypto/ed255… (done)
- [SIGN-1-FU-OUTOFORDER-POISON](../SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) — SIGN-1-FU-OUTOFORDER-POISON: Reserve-then-send lets mints be spent out of order, which pe… (done)
- [SIGN-1-FU-REORDER-WATERMARK](../SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)
- [SIGN-2](../SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)
- [SIGN-3](../SIGN-3--f2daa6bc/task.md) — SIGN-3: Broadcast signature covers the recipient set (prevents split-content broadcasts) (todo)
- [SIGN-4](../SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- server-minted monotonic sequence + recipient-side cursor (todo)
- [SIGN-5](../SIGN-5--5cedc580/task.md) — SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must (done)
- [SIGN-6](../SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)
- [SIGN-7](../SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
