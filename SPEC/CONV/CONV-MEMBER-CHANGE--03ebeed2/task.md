# CONV-MEMBER-CHANGE: the change event -- a REMOVED participant gets exactly ONE final message, then nothing

| Field | Value |
| --- | --- |
| Public id | `03ebeed2-082f-4985-9057-a4a987ac3c95` |
| Key | CONV-MEMBER-CHANGE |
| Epic | [CONV](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | conv |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:56:19.917567+00:00 |
| Updated | 2026-08-15T08:56:19.917567+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestConversationMembershipChangeEvent' ./internal/hub ./internal/httpapi
```

## Description

The operator's rule, verbatim: "The receipeient list can change and a special change event message
should be sent to all current receipients, **including those that have been removed**."

**THE FAILURE MODE, NAMED SO IT CANNOT BE SHRUGGED OFF:** a removed participant receives EXACTLY ONE
final message AFTER removal, and NOTHING EVER AFTER. That is a deliberate one-message grace. **The
natural implementation gets it BACKWARDS:** the obvious order is "apply the membership change, then
look up current members, then deliver" -- which delivers the removal notice to everyone EXCEPT the
person who most needs it, and the bug is invisible in any test that only checks the remaining
members received it. A test that asserts only "the two survivors got the event" PASSES on the broken
implementation. The REQUIRED assertion is that the REMOVED agent got it, exactly once, and that a
subsequent conversation message does NOT reach them.

MEMBERSHIP HAS AN ASYMMETRY, and both edges are off-by-one hazards in OPPOSITE directions:
**joining is EXCLUSIVE of prior history (CONV-JOINPOINT / no backfill); leaving is INCLUSIVE of
exactly one subsequent message (this task).** A naive membership check gets EACH ONE WRONG. State
this in the code comment, not just the task.

The change event is DURABLE STATE (invariants 4 and 5): the membership change must be durable before
it is acknowledged, and recovery must not replay the change event as a second change (see
CONV-IDEM case 3, and CONV-CRASH case 3).

RESERVED: `wal-entry-kind` value **4** = the string `"convmember"` (reserved 2026-08-15,
spec-keeper) for the membership-change record. **Do not hand-pick a Kind string.** If the design
folds membership into the conversation record instead, value 4 is simply left unspent -- say so;
an unspent reservation is cheap, a collision is not. No numeric `record-type` is needed (see
CONV-RECORD for why).

Definition of done:
  1. The change event delivered to all CURRENT recipients AND to each REMOVED one, exactly once.
  2. A test asserting the REMOVED agent received it (the assertion that catches the natural bug).
  3. A test asserting the removed agent receives NOTHING after that one event -- including that a
     retry/duplicate change event does not produce a second delivery.
  4. Durable before ack; the change is not replayed as a second change after restart.

INVARIANT 7 (same task, non-negotiable): ship the `cmd/agent-busctl` subcommand AND the
AGENT_PROTOCOL.md entry IN THIS TASK. `--json` everywhere, stable documented exit codes, no
interactive prompt. `scripts/bus-*.sh` wrappers are RETIRED; adding one is FORBIDDEN. The client
package cannot live under `internal/`. Verify through the COMPILED CLI against a running server --
never hand-written curl.

Blocked by CONV-AUTHZ-CREATOR (who may make the change) and CONV-RECORD.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [CONV-AUTHZ-CREATOR](../CONV-AUTHZ-CREATOR--4abd8589/task.md)
- **blocked by** [CONV-RECORD](../CONV-RECORD--cd3524c2/task.md)
- **blocks** [CONV-JOINPOINT](../CONV-JOINPOINT--b18c8710/task.md)
- **relates to** [CONV-IDEM](../CONV-IDEM--aae5f71e/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-AUTHZ-CREATOR](../CONV-AUTHZ-CREATOR--4abd8589/task.md) — CONV-AUTHZ-CREATOR: only the creator may change the recipient list -- the arm that can sh… (todo)
- [CONV-CRASH](../CONV-CRASH--3078ad4e/task.md) — CONV-CRASH: crash-injection proof that conversation create + membership change recover to… (todo)
- [CONV-IDEM](../CONV-IDEM--aae5f71e/task.md) — CONV-IDEM: conversation create + membership change idempotency -- three cases, NOT collap… (todo)
- [CONV-JOINPOINT](../CONV-JOINPOINT--b18c8710/task.md) — CONV-JOINPOINT: NO BACKFILL -- define the join point as a durable POSITION, atomic with t… (todo)
- [CONV-RECORD](../CONV-RECORD--cd3524c2/task.md) — CONV-RECORD: the durable conversation record -- wal.Entry.Kind "conversation" (reservatio… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-JOINPOINT](../CONV-JOINPOINT--b18c8710/task.md) — CONV-JOINPOINT: NO BACKFILL -- define the join point as a durable POSITION, atomic with t… (todo)
- [CONV-RECORD](../CONV-RECORD--cd3524c2/task.md) — CONV-RECORD: the durable conversation record -- wal.Entry.Kind "conversation" (reservatio… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
