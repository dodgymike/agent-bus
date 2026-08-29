# IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, complementing (never replacing) RELAY-3 loop prevention

| Field | Value |
| --- | --- |
| Public id | `1c490a08-589b-4466-85be-c66b516ee463` |
| Key | IDEM-7 |
| Epic | [IDEM](../epic.md) |
| Status | superseded |
| Priority | P2 |
| Component | relay |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:14:15.548206+00:00 |
| Updated | 2026-08-02T13:17:03.684877+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestRelayAppliesOnce ./internal/relay -- a cyclic 3-bus topology delivers one message once
```

## Status note

DUPLICATE-EPIC MERGE, 2026-08-02. Two spec-keeper agents were dispatched to file the IDEM epic (invariant 10) concurrently and both did: IDEM-1..9 (this set, keys reserved 13:05) and IDEM-10..17 (created 13:10). The task-key RESERVATION namespace did its job -- no two tasks share a key -- but reservations cannot prevent two agents writing the same EPIC, and 17 overlapping claimable tasks would have had two agents independently implementing the same applied-key store. Resolved by superseding IDEM-1..9 and keeping IDEM-10..17, for three reasons: (a) IDEM-10..17 gate more precisely against existing keys (DUR-1/2/3/6, AUTH-1/4, MSG-2/3, RELAY-1/2/3); (b) they escalate the durable applied-key store and its crash-injection test to P0 with an argument that matches why DUR-1/DUR-2/DUR-6 are P0; (c) the alternative -- cancelling another agent's tasks -- was outside this pass's ownership, whereas IDEM-1..9 are this pass's own to withdraw. Nothing was lost: the unique content of this task is preserved either in its counterpart or as an append-only note on it. SUPERSEDED BY: IDEM-15 (relay duplicate suppression).

## Description

GATED on IDEM-2; lands with RELAY-2/RELAY-3. WHY THIS IS WHERE IDEMPOTENCY EARNS ITS KEEP (invariant 10, verbatim): "a cyclic peer topology plus at-least-once delivery means duplicates are not an edge case but the normal steady state." A bus with two peers that both peer with a third receives the same message twice as a matter of routine, not as a failure. (1) DEDUPE ON THE ORIGIN'S IDENTITY, NOT THE FORWARDING PEER'S: two different peers legitimately forward the SAME origin message, so keying on the sending peer's own idempotency key would treat them as two messages. The dedupe identity must be the origin bus's message identity -- which per invariant 2 is already globally unambiguous because it is namespaced by bus id -- carried unchanged across every hop. (2) IT MUST NOT BE FORGEABLE BY AN INTERMEDIATE: interacts directly with SIGN-7. If a lying peer can rewrite the dedupe identity, it can split one message into two deliveries (duplicate injection) or collide two messages into one (suppression). Prefer an identity that is inside, or verifiably derived from, SIGN-1's signed bytes, and say explicitly what an intermediate CAN still do -- the traversed bus path is metadata outside the signature (SIGN-7), so loop prevention is an availability mechanism, not a security one. (3) COMPLEMENT, NEVER SUBSTITUTE: RELAY-3's traversed-bus-path check stops a message CIRCULATING; this stops it being APPLIED twice. Neither replaces the other -- a message can arrive twice by two loop-free paths, and a buggy or malicious peer can strip the path. Do not let an implementer delete one because the other exists; state the argument in the code comment and in PROTOCOL.md. (4) The far bus mints its OWN local sequence for its own recipients (SIGN-7), so 'applied once' means one local delivery and one local sequence, not the origin's numbers. (5) RELAY-4's retry/backoff is the duplicate SOURCE this defends against, so test them together: a peer that acks late and retries must not produce a second delivery, including across a restart of the receiving bus.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [IDEM-2](../IDEM-2--1c6a5ef1/task.md)
- **relates to** [RELAY-3](../../RELAY/RELAY-3--e944edda/task.md)
- **relates to** [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [DUR-1](../../DUR/DUR-1--c51e1959/task.md) — DUR-1: WAL record framing + writer (done)
- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-15](../IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)
- [RELAY-2](../../RELAY/RELAY-2--654140d7/task.md) — RELAY-2: Message relay + ongoing roster sync across peers (done)
- [RELAY-3](../../RELAY/RELAY-3--e944edda/task.md) — RELAY-3: Loop prevention via traversed-bus path (done)
- [RELAY-4](../../RELAY/RELAY-4--5ac738b4/task.md) — RELAY-4: Peer-down retry/backoff (done)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-15](../IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
