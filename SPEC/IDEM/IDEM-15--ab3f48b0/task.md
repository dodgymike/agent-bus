# IDEM-15: Relay duplicate suppression via idempotency keys

| Field | Value |
| --- | --- |
| Public id | `ab3f48b0-4e34-4b80-834c-5e7464063c18` |
| Key | IDEM-15 |
| Epic | [IDEM](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | relay |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:10:35.566363+00:00 |
| Updated | 2026-08-02T13:24:06.805764+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestRelayIdempotentSuppression ./internal/relay/...
```

## Description

GATED on IDEM-10, IDEM-11, RELAY-2 (message relay across peers) and RELAY-3 (loop prevention via traversed-bus path). Relay is where idempotency earns its keep: a cyclic peer topology combined with at-least-once delivery (invariant 4's guarantee, extended across the relay plane) means a relayed message can legitimately arrive at a bus by two different paths, or be resent by a peer retrying after a lost ack -- duplicates are the NORMAL steady state here, not an edge case. Apply the same applied-key check IDEM-12 uses to inbound relayed messages: a relayed message carries (or is assigned, at the originating bus) an idempotency key, and a receiving bus that has already applied that key suppresses the duplicate exactly as a duplicate direct send is suppressed -- no second delivery to local agents, no second audit record. STATE THIS EXPLICITLY, because RELAY-3 alone reads as sufficient and it is NOT: RELAY-3's traversed-bus-path loop prevention COMPLEMENTS this and is NEVER a substitute for it. RELAY-3 stops a message from being re-relayed back through a bus it has already visited (a topology-shape defence); it does nothing about a message that legitimately reaches the same bus via two DIFFERENT paths that never revisit any bus, which only idempotency catches. A relay implementation with RELAY-3 but without this task will silently double-deliver in exactly that topology. Priority is P2, matching the RELAY epic's own priority band, since it cannot land before RELAY-2/RELAY-3 exist. Test alongside RELAY-5's crash/loop integration test in IDEM-17, not as a replacement for it.

--- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-7, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) DEDUPE ON THE ORIGIN'S IDENTITY, NOT THE FORWARDING PEER'S. Two different peers legitimately forward the SAME origin message; keying suppression on the sending peer's own idempotency key treats those as two distinct messages and delivers both. The dedupe identity must be the ORIGIN bus's message identity -- already globally unambiguous per invariant 2 because it is <bus-id>-namespaced -- carried UNCHANGED across every hop. This is the single most important sentence in this task and it was absent before the merge. (b) IT MUST NOT BE FORGEABLE BY AN INTERMEDIATE (see SIGN-7): if a lying peer can rewrite the dedupe identity, it can split one message into two deliveries (duplicate injection) or collide two distinct messages into one (silent suppression). Prefer an identity that is inside, or verifiably derived from, SIGN-1's signed bytes -- and state explicitly what an intermediate CAN still do: the traversed-bus path is metadata OUTSIDE the signature, so RELAY-3's loop prevention is an availability mechanism, not a security one. (c) 'APPLIED ONCE' MEANS ONCE LOCALLY: the receiving bus mints its OWN local delivery sequence for its own recipients (SIGN-7), so the assertion is one local delivery and one local sequence consumed -- not that the origin's numbers are reused. (d) RELAY-4's RETRY/BACKOFF IS THE DUPLICATE SOURCE this defends against, so test them together: a peer that acks late and retries must not produce a second delivery, INCLUDING across a restart of the receiving bus -- which is where the durability of the relay-side applied-key record (IDEM-11) is actually exercised. (e) Put the complement-never-substitute argument in the CODE COMMENT and in PROTOCOL.md, not only in this task, so a later implementer does not delete one defence because the other exists. CROSS-REFERENCE: SIGN-7 point (5) now points at THIS task (it referenced the withdrawn IDEM-7 until the merge).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (in_progress)
- [IDEM-17](../IDEM-17--8b1e85fd/task.md) — IDEM-17: Crash-injection test -- restart mid-retry-window still yields exactly one effect (done)
- [IDEM-7](../IDEM-7--1c490a08/task.md) — IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, co… (superseded)
- [RELAY-2](../../RELAY/RELAY-2--654140d7/task.md) — RELAY-2: Message relay + ongoing roster sync across peers (done)
- [RELAY-3](../../RELAY/RELAY-3--e944edda/task.md) — RELAY-3: Loop prevention via traversed-bus path (done)
- [RELAY-4](../../RELAY/RELAY-4--5ac738b4/task.md) — RELAY-4: Peer-down retry/backoff (done)
- [RELAY-5](../../RELAY/RELAY-5--f3a31e10/task.md) — RELAY-5: Relay crash/loop integration test (done)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-7](../IDEM-7--1c490a08/task.md) — IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, co… (superseded)
- [RELAY-5](../../RELAY/RELAY-5--f3a31e10/task.md) — RELAY-5: Relay crash/loop integration test (done)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
