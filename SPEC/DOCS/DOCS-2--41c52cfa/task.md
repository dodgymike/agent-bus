# DOCS-2: PROTOCOL.md -- wire protocol + on-disk format

| Field | Value |
| --- | --- |
| Public id | `41c52cfa-39b5-425c-ab6e-92872ec5876a` |
| Key | DOCS-2 |
| Epic | [DOCS](../epic.md) |
| Status | todo |
| Priority | P0 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:05:58.765698+00:00 |
| Updated | 2026-08-02T18:07:13.547699+00:00 |
| Completed | — |

## Proof command

```sh
test -s PROTOCOL.md && grep -q 'at-least-once' PROTOCOL.md && grep -q 'metadata' PROTOCOL.md
```

## Description

RAISED P1 -> P0 2026-08-02. **PROTOCOL.md DOES NOT EXIST.** Verified first-hand this pass: CLAUDE.md's
repository-layout section lists it as a tracked contract document ("PROTOCOL.md -- the wire protocol +
on-disk format") and there is no such file in the repo. THREE OTHER TASKS ARE WRITTEN AS THOUGH IT
DOES and grep it in their proof commands -- DUR-4-FU-DOCS (0b6d5c11, now P0), the unknown-record-type
docs task (804fa84c), and CLI/CONTRACTS work -- so its absence is now BLOCKING, not merely a gap.
This task OWNS CREATING THE FILE; those tasks own sections within it.

MANDATED CONTENT ADDED BY THE 2026-08-02 DECISIONS -- the user's decision text says these MUST be
stated in PROTOCOL.md, so they are not discretionary:
 - **AT-LEAST-ONCE DELIVERY.** "Duplicates are the normal steady state, which is what invariant 10's
   idempotency exists to absorb. Must be stated in PROTOCOL.md and AGENT_PROTOCOL.md."
 - **THE NARROWED INVARIANT 4.** Acknowledged data may be discarded when found corrupt: "The
   narrowing is deliberate and must be stated in PROTOCOL.md, not left implicit." Likewise the
   narrowed invariant 6 (truncation no longer restricted to a verified-corrupt tail).
 - **THE AUDIT LOG IS METADATA AND ROUTING INFO ONLY** -- id, sequence, sender, recipients, bus path,
   timestamp, size, content hash. NEVER bodies. A deliberate 2026-08-02 decision so the trail stays
   compatible with E2E-encrypted, forward-secret payloads.
 - Retention: 1 day or 1 GB, whichever comes first. Default listen address: localhost.
 - Sessions: server-provided token, client-signed, <=1h, opaque handle, DO NOT survive restart;
   revocation via /leave is IMMEDIATE.
 - On-disk format: FormatVersion 1 today; ondisk-format-version=2 is RESERVED for DUR-12's
   CRC32C -> HMAC-SHA256 change. Say what is current and what is reserved.

PROOF. `test -s PROTOCOL.md && grep -q 'at-least-once' PROTOCOL.md && grep -q 'metadata' PROTOCOL.md`
-- FAILS TODAY at clause 1 (the file does not exist), correctly and non-vacuously. The previous
proof (`test -s PROTOCOL.md`) was fine but did not pin the two mandated statements.

--- ORIGINAL DESCRIPTION ---
Every HTTP route (method, path, auth requirement, request/response shape) and the on-disk format (WAL record framing, audit log format, roster/counter file layouts) -- maintainer-facing, kept current as routes land.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [804fa84c-e97b-4737-8866-801f87468da4](../../DUR/Document-what-the-bus-does-with-an-UNKNOWN-WAL-record-ty--804fa84c/task.md) — Document what the bus does with an UNKNOWN WAL record type -- the answer is now discard-a… (todo)
- [DUR-12](../../DUR/DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (done)
- [DUR-4-FU-DOCS](../../DUR/DUR-4-FU-DOCS--0b6d5c11/task.md) — DUR-4-FU-DOCS: state invariants 4/6 as explicit NARROWINGS + document the WAL recovery AP… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [804fa84c-e97b-4737-8866-801f87468da4](../../DUR/Document-what-the-bus-does-with-an-UNKNOWN-WAL-record-ty--804fa84c/task.md) — Document what the bus does with an UNKNOWN WAL record type -- the answer is now discard-a… (todo)
- [CRYPTO-12](../../CRYPTO/CRYPTO-12--eb1827ff/task.md) — CRYPTO-12: PROTOCOL.md wire format + CONTRACTS.md for the crypto surface (todo)
- [DUR-4-FU-DOCS](../../DUR/DUR-4-FU-DOCS--0b6d5c11/task.md) — DUR-4-FU-DOCS: state invariants 4/6 as explicit NARROWINGS + document the WAL recovery AP… (todo)
- [HANDOVER-CHECK](../../HANDOVER/HANDOVER-CHECK--0f909b6c/task.md) — HANDOVER-CHECK: one command that tells you the health of this repo, plus its recorded out… (todo)
- [a695f85f-0c69-42a8-a653-deed4960a610](../PROTOCOL.md-8-cites-Spec-Server-task-id-INVITE-PEERGUARD--a695f85f/task.md) — PROTOCOL.md §8 cites Spec Server task id INVITE-PEERGUARD (f5d91dbe) as if it were a comm… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
