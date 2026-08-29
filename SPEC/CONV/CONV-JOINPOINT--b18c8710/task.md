# CONV-JOINPOINT: NO BACKFILL -- define the join point as a durable POSITION, atomic with the membership change

| Field | Value |
| --- | --- |
| Public id | `b18c8710-128b-4035-ad1e-bb6b971b4dc2` |
| Key | CONV-JOINPOINT |
| Epic | [CONV](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | conv |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:56:20.154260+00:00 |
| Updated | 2026-08-15T08:56:20.154260+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestConversationJoinPointNoBackfill' ./internal/hub ./internal/store
```

## Description

**OPERATOR CONSTRAINT (2026-08-15): "no backfill".** A newly-added participant sees NOTHING sent
before they joined. This is a CONSTRAINT, not a decision to revisit.

**RECORD THE RATIONALE, not just the rule, so nobody "improves" it later by adding history sync:**
no-backfill removes the retroactive-exposure hazard ENTIRELY -- adding someone to a conversation can
never expose history they were not party to. It is both the cheaper and the safer answer. A later
history-sync feature would reintroduce a confidentiality hazard the operator has already declined.

**THE REAL WORK IS DEFINING THE JOIN POINT PRECISELY, AND THIS REPO HAS TWO SPECIFIC TRAPS HERE.**

TRAP 1 -- THE CONFLATION THAT ALREADY CAUSED TWO P0s. Three distinct notions:
  - `Seq` -- the pre-assigned, SIGNED IDENTITY. **NOT a position.**
  - `store.Message.Pos` -- the DELIVERY POSITION, stamped from `wal.Committed.CommitIndex`
    (internal/store/message.go:143).
  - `OriginMessageID` -- a CORRELATION key, **INERT in every ordering/cursor/retention decision**
    (landed `88c43b3`, documented at internal/store/message.go:167-194).
A join point is a POSITION question, so it is **`Pos`, NEVER `Seq`**. Using `Seq` looks right and is
wrong: sequence identity and delivery order are not the same relation, and that is precisely the
conflation the two P0s came from.

TRAP 2 -- THE NAIVE FIX FOR "MAKE IT DURABLE" WALKS STRAIGHT INTO A DOCUMENTED PROHIBITION.
`store.Record` has **NO `Pos` field, deliberately.** internal/store/message.go:437:
"Message.Pos is DELIBERATELY ABSENT, and adding it would be a mistake ... writing it INTO that entry
would record a fact the entry's own location already states -- and would create a second copy free
to disagree with the first." `Decode` returns `Pos == 0`; `Hub.Apply` stamps it from
`wal.Committed.CommitIndex`, which is what makes the position identical on the live and recovery
paths without an on-disk format change.
**SO: DO NOT ADD A `join_pos` FIELD TO THE MEMBERSHIP RECORD.** The join point IS the commit index
of the membership-change record itself -- durable BY LOCATION, derived on replay exactly as
`Message.Pos` is. If you believe you need to persist a position, re-read that doc comment first.

ATOMICITY: the join point must be established ATOMICALLY with the membership change. A gap or
overlap between "added" and "starts receiving" either **leaks one message backwards** (a
confidentiality failure -- the exact thing no-backfill exists to prevent) or **silently drops one
forwards** (a delivery failure). **That is a crash-injection test, not a code reading** -- see
CONV-CRASH case 3, which is where the atomicity claim is PROVED.

ASYMMETRY, one line, state it in the code: **joining is EXCLUSIVE of prior history; leaving is
INCLUSIVE of exactly one subsequent message** (CONV-MEMBER-CHANGE). Both are off-by-one hazards in
opposite directions and a naive membership check gets each one wrong.

Definition of done:
  1. The join point defined as a delivery POSITION, derived by location, surviving restart.
  2. A test that an agent added at position P receives NOTHING committed before P -- including the
     boundary message AT P-1.
  3. A test that they DO receive the first message at/after P (no silent drop forwards).
  4. A comment naming the Seq/Pos/OriginMessageID conflation explicitly so the next reader does not
     re-make it.
  5. CONTRACTS-ONDISK.md / CONTRACTS-HTTP.md updated with the no-backfill rule AND its rationale.

Blocked by CONV-MEMBER-CHANGE.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [CONV-MEMBER-CHANGE](../CONV-MEMBER-CHANGE--03ebeed2/task.md)
- **relates to** [CONV-CRASH](../CONV-CRASH--3078ad4e/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-CRASH](../CONV-CRASH--3078ad4e/task.md) — CONV-CRASH: crash-injection proof that conversation create + membership change recover to… (todo)
- [CONV-MEMBER-CHANGE](../CONV-MEMBER-CHANGE--03ebeed2/task.md) — CONV-MEMBER-CHANGE: the change event -- a REMOVED participant gets exactly ONE final mess… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-CRASH](../CONV-CRASH--3078ad4e/task.md) — CONV-CRASH: crash-injection proof that conversation create + membership change recover to… (todo)
- [CONV-MEMBER-CHANGE](../CONV-MEMBER-CHANGE--03ebeed2/task.md) — CONV-MEMBER-CHANGE: the change event -- a REMOVED participant gets exactly ONE final mess… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
