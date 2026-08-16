# CONV-RECORD: the durable conversation record -- wal.Entry.Kind "conversation" (reservation wal-entry-kind=3)

| Field | Value |
| --- | --- |
| Public id | `cd3524c2-3c65-4b53-8584-9f6dd7fa91b7` |
| Key | CONV-RECORD |
| Epic | [CONV](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | conv |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:54:46.835127+00:00 |
| Updated | 2026-08-15T08:54:46.835127+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestConversationRecord' ./internal/store
```

## Description

Define and implement the durable conversation record: server-minted id, creator, user-supplied
name/tags, recipient list, routing metadata, timestamps.

**BLOCKED BY CONV-ID-SHAPE (the id shape) AND CONV-NAME-INV6 (whether/how the name may be persisted
at all, and its length+charset bound). Do not fix the format before both rule.**

RESERVED, DO NOT HAND-PICK ANYTHING:
  - `wal-entry-kind` value **3** = the string `"conversation"` (reserved 2026-08-15, spec-keeper).
    Use that exact string as `wal.Entry.Kind`.
  - `wal-entry-kind` value **4** = the string `"convmember"`, reserved for CONV-MEMBER-CHANGE.
  - **NO numeric `record-type` was reserved, and reserving one would be WRONG.** By precedent, a
    business record rides inside the EXISTING `TypePrepare`/`TypeCommit` WAL frames and consumes no
    `record-type` number: `store.RecordKind = "message"` (internal/store/message.go:18) and
    `hub.SeqFloorRecordKind = "seqfloor"` (internal/hub/mint.go:85) both do exactly this. The
    `record-type` namespace holds exactly the four WAL FRAME types (1=Prepare, 2=Commit, 3=Abort,
    4=AuditMessage).
  - **IF** the design turns out to need a new WAL FRAME type, a new standalone on-disk FILE, or a
    relay wire change, reserve from `record-type` / `ondisk-format-version` / `relay-wire-version`
    **at that point, via the reservations API** -- never by eyeballing the ledger.

INVARIANTS 4 AND 5: the record must be DURABLE BEFORE ACKNOWLEDGEMENT (two-phase prepare->commit,
fsynced -- never traded for latency) and recovery must reach a PREFIX of accepted history. Follow
`store.Record`'s existing shape: it is deliberately built so an OPTIONAL added field needs no
version bump (see the RecordVersion doc at internal/store/message.go:461).

**DO NOT ADD A `Pos` FIELD.** internal/store/message.go:437 -- "Message.Pos is DELIBERATELY ABSENT,
and adding it would be a mistake ... would create a second copy free to disagree with the first."
The same reasoning binds the conversation record.

BOUND EVERYTHING DECODED FROM DISK. `store.Record`'s bounds exist because Decode reads whatever the
FILE holds, not whatever a handler validated (see the MaxRecipients doc at
internal/store/message.go:65). The recipient list, the name, and the tag set each need a bound
enforced on the DISK-decode path as well as the handler path.

Definition of done:
  1. The record type, its bounds, and its encode/decode, with the reserved Kind string.
  2. Bounds enforced on BOTH the handler path and the disk-decode path, each with a test.
  3. CONTRACTS-ONDISK.md updated, including the `wal-entry-kind` namespace table (this task
     introduces that namespace to the doc -- state that it coordinates the STRING discriminator and
     was seeded retrospectively with 1=message, 2=seqfloor).
  4. Crash-injection coverage is CONV-CRASH's job, not this task's, but this task must not make it
     impossible.

Parallel-safety: touches internal/store; coordinate with DUR work in flight.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-CRASH](../CONV-CRASH--3078ad4e/task.md) — CONV-CRASH: crash-injection proof that conversation create + membership change recover to… (todo)
- [CONV-ID-SHAPE](../CONV-ID-SHAPE--8914a5d8/task.md) — CONV-ID-SHAPE: decide the conversation id shape -- bare UUID vs &lt;bus-id&gt;.&lt;conv-id&gt; (ATTRI… (todo)
- [CONV-MEMBER-CHANGE](../CONV-MEMBER-CHANGE--03ebeed2/task.md) — CONV-MEMBER-CHANGE: the change event -- a REMOVED participant gets exactly ONE final mess… (todo)
- [CONV-NAME-INV6](../CONV-NAME-INV6--a11d59cd/task.md) — CONV-NAME-INV6: is a user-supplied conversation NAME metadata, or a body wearing metadata… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-CREATE-CLI](../CONV-CREATE-CLI--627d20e0/task.md) — CONV-CREATE-CLI: mint a conversation -- HTTP route + agent-busctl subcommand + AGENT_PROT… (todo)
- [CONV-ID-SHAPE](../CONV-ID-SHAPE--8914a5d8/task.md) — CONV-ID-SHAPE: decide the conversation id shape -- bare UUID vs &lt;bus-id&gt;.&lt;conv-id&gt; (ATTRI… (todo)
- [CONV-MEMBER-CHANGE](../CONV-MEMBER-CHANGE--03ebeed2/task.md) — CONV-MEMBER-CHANGE: the change event -- a REMOVED participant gets exactly ONE final mess… (todo)
- [CONV-NAME-INV6](../CONV-NAME-INV6--a11d59cd/task.md) — CONV-NAME-INV6: is a user-supplied conversation NAME metadata, or a body wearing metadata… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
