# SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side sequence ordering

| Field | Value |
| --- | --- |
| Public id | `33fa35d8-2a1e-44ce-ae80-cf460f8e6eca` |
| Key | SIGN-4 |
| Epic | [SIGN](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | crypto |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:59:07.533867+00:00 |
| Updated | 2026-08-14T20:10:54.327876+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestReplayRejectedByCursor ./internal/...
```

## Description

GATED on SIGN-1. A signature alone does NOT provide a freshness/replay defence: a validly-signed message can be replayed VERBATIM by anyone who saw it once (including a malicious bus), and Ed25519 verification of a replayed message succeeds every time because nothing about the signature changes. Do not let an implementer assume signing solves this -- it does not, and the SIGN epic description says so explicitly. This task specifies and implements the defence: AMENDED 2026-08-14 by SIGN-1-FU-REORDER-WATERMARK -- the original wording specified the defect that task removed, and must not be built. Freshness is enforced SERVER-SIDE, AT INGEST: the bus refuses an already-accepted signed message before it is ever served. The recipient performs NO sequence-based freshness check. It deduplicates on message_id and treats its read cursor as an OPAQUE server-assigned DELIVERY POSITION, handed back unmodified. THE SEQUENCE IS AN IDENTITY, NOT AN ORDERING OR FRESHNESS TOKEN: since SIGN-1 it is minted when a client RESERVES, not when it sends, so a message with a LOWER sequence arriving AFTER a higher one is a normal, correct delivery. A recipient that rejects, reorders or discards on sequence re-implements SIGN-1-FU-REORDER-WATERMARK client-side and permanently loses messages the bus has already acknowledged -- in every client rather than in one server. Do NOT specify a recipient-side cursor that "MUST only advance, never rewind" over sequences, and do NOT reject a message whose sequence is <= any recipient-side high-water mark. State plainly what this does and does not cover: it defeats verbatim replay of a message already delivered; it does NOT provide encryption or hide metadata (accepted per RATCHET-2's rescope). Tests: replaying the exact same signed envelope after successful delivery is rejected AT THE BUS, before it is served; a message whose sequence is BELOW one the recipient has already accepted is DELIVERED to the calling agent unchanged (this is the regression test for SIGN-1-FU-REORDER-WATERMARK -- it must be impossible to write a conforming recipient that drops it); deduplication is keyed on message_id and is proven to survive a recipient-side restart.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [IDEM-5](../../IDEM/IDEM-5--9631dfcb/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RATCHET-2](../../RATCHET/RATCHET-2--ade31a62/task.md) — RATCHET-2: Threat model -- what Ed25519 signing defends against, and explicitly what it d… (todo)
- [SIGN-1](../SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-1-FU-REORDER-WATERMARK](../SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-10](../../CRYPTO/CRYPTO-10--68ff679d/task.md) — CRYPTO-10: \`agent-bus verify\` helper + scripts/bus-*.sh validate-before-accept + AGENT_PR… (todo)
- [CRYPTO-12](../../CRYPTO/CRYPTO-12--eb1827ff/task.md) — CRYPTO-12: PROTOCOL.md wire format + CONTRACTS.md for the crypto surface (todo)
- [CRYPTO-7](../../CRYPTO/CRYPTO-7--f90d7889/task.md) — CRYPTO-7: Ratchet-state durability and recovery (CRASH-INJECTION TEST REQUIRED) (deferred)
- [IDEM-14](../../IDEM/IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-5](../../IDEM/IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [RATCHET-2](../../RATCHET/RATCHET-2--ade31a62/task.md) — RATCHET-2: Threat model -- what Ed25519 signing defends against, and explicitly what it d… (todo)
- [RATCHET-5](../../RATCHET/RATCHET-5--e376433d/task.md) — RATCHET-5: Ratchet state durability vs invariants 4/5 -- the key-reuse trap (superseded)
- [SIGN-5](../SIGN-5--5cedc580/task.md) — SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must (done)
- [SIGN-6](../SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)
- [SIGN-7](../SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
