# RATCHET-2: Threat model -- what Ed25519 signing defends against, and explicitly what it does not

| Field | Value |
| --- | --- |
| Public id | `ade31a62-0e26-46e5-80a3-8e23b6cc39e2` |
| Key | RATCHET-2 |
| Epic | [RATCHET](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | crypto |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:44:36.714224+00:00 |
| Updated | 2026-08-14T20:19:30.792380+00:00 |
| Completed | — |

## Proof command

```sh
grep -rqi 'no confidentiality' THREAT_MODEL.md PROTOCOL.md
```

## Description

RESCOPED 2026-08-02 per user instruction ("keep it simple, standard sign/verify; encryption later"): this is no longer a ratchet/PFS threat model, it is the threat model for a SIGN-ONLY design. Write down the adversary before further work lands. Who is the attacker -- a compromised bus, a compromised relay peer bus, a network observer, another enrolled agent, someone who later obtains the disk? WHAT SIGNING BUYS: message AUTHENTICITY (this body really was produced by the holder of this messaging private key) and INTEGRITY (this body was not modified in transit), verified by the RECIPIENT -- so a compromised or malicious bus cannot forge a message purporting to be from an agent it does not control, even though the bus relays every message. This is the whole security value of keeping the AUTH keypair (CRYPTO-1/AUTH-1, authenticates to the bus) and the MESSAGING keypair (CRYPTO-3, authenticates to peers) separate -- state that explicitly. WHAT SIGNING DOES NOT BUY, STATE THIS PLAINLY AND WITHOUT HEDGING: NO CONFIDENTIALITY. Without encryption, the bus and any relay peer on a multi-bus path (RELAY-2/3) CAN and WILL read every message body, in cleartext, always. This is now an ACCEPTED property of the system per direct user instruction, not an oversight to be apologized for -- but it must be legible to every future reader of PROTOCOL.md, not discovered by surprise. NO forward secrecy (a compromised messaging private key lets an attacker forge NEW messages as that agent going forward, and there is no ratchet to bound the blast radius -- key rotation via key_epoch, CRYPTO-4, is the only mitigation). NO replay defence from the signature alone (covered separately by SIGN-4, freshness enforced SERVER-SIDE AT INGEST -- never by a recipient-side sequence cursor; SIGN-4's original wording specified exactly that defect and was corrected 2026-08-14 by SIGN-1-FU-REORDER-WATERMARK -- reference SIGN-4, do not re-derive it here). State plainly which threats are OUT of scope for this rescoped epic (traffic analysis / metadata exposure, a fully compromised endpoint agent, a malicious bus dropping/reordering/duplicating messages -- signing does not stop any of these, only forging content undetected). Without this document the sign/verify choice is unfalsifiable and 'we signed it' becomes a slogan rather than a security property.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [CRYPTO-1](../../CRYPTO/CRYPTO-1--30570fb9/task.md) — CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DEC… (done)
- [CRYPTO-3](../../CRYPTO/CRYPTO-3--dd1066af/task.md) — CRYPTO-3: Enrolment mints and registers the second (messaging) keypair, bound to the serv… (todo)
- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [RELAY-2](../../RELAY/RELAY-2--654140d7/task.md) — RELAY-2: Message relay + ongoing roster sync across peers (done)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)
- [SIGN-4](../../SIGN/SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side seque… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CRYPTO-11](../../CRYPTO/CRYPTO-11--0047e5b7/task.md) — CRYPTO-11: Audit-log content hash for signed (cleartext) messages -- implements the invar… (todo)
- [SIGN-4](../../SIGN/SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side seque… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
