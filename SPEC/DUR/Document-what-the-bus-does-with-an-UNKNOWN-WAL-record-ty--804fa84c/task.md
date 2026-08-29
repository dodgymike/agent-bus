# Document what the bus does with an UNKNOWN WAL record type -- the answer is now discard-and-continue, not refuse-to-start (REVERSED 2026-08-02)

| Field | Value |
| --- | --- |
| Public id | `804fa84c-e97b-4737-8866-801f87468da4` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:53:04.514887+00:00 |
| Updated | 2026-08-02T18:00:51.401752+00:00 |
| Completed | — |

## Proof command

```sh
test -f PROTOCOL.md && grep -q 'unknown record type' PROTOCOL.md
```

## Description

REVERSED 2026-08-02 BY USER DECISION. THE BEHAVIOUR THIS TASK WAS FILED TO DOCUMENT IS BEING REMOVED,
SO DO NOT DOCUMENT IT.

The task said: "DUR-3 introduced a new way for a bus to refuse to boot: a WAL containing a record this
code cannot interpret now FAILS STARTUP ... That direction is correct (silent data loss is worse than
a loud refusal to start)." The user decided the opposite (DECISIONS.md, 2026-08-02): *"always be able
to restart, prefer to discard messages and/or corruption, with logging"*. An unknown record type is a
DAMAGE class: discard it, log loudly and specifically, keep running. The decision reconciles the two
positions -- "The defect was never that data was discarded; it is that the discard was SILENT."

WHAT TO DOCUMENT INSTEAD (in PROTOCOL.md, plus the operator-facing notes):
 - What an unknown record type IS (a record written by a NEWER binary, or a damaged type field --
   the reader cannot tell them apart, which is exactly why refusing to start was a downgrade trap).
 - What the bus DOES: discards that record, logs a specific line naming offset, record index, the
   unrecognised type value and the byte count discarded, and CONTINUES to a serving state.
 - What the operator SEES and what it means -- in particular that a burst of unknown-type discards
   after a rollback is the signature of a DOWNGRADE, not of media failure, and the remedy is to run
   the newer binary again rather than to repair the log.
 - What is NOT affected: NON-DAMAGE errors still refuse to start (permission denied, I/O failure,
   data-directory lock held, missing/unwritable data dir).

RELATED, DO NOT DUPLICATE: DUR-4-FU-DOCS (now P0) owns the RepairTail/TailRepair API surface, the
narrowed invariants 4 and 6, and at-least-once delivery; bd3cc650 owns the stale CONTRACTS.md:55
record-type list; DOCS-2 owns CREATING PROTOCOL.md, WHICH DOES NOT EXIST YET (verified 2026-08-02 --
this task's original proof_cmd grepped a file that is not in the repo, so it could never have passed).
e875182a is the sibling forward-compat problem: internal/wal/log.go's decodePayload uses
DisallowUnknownFields, so an unknown FIELD is currently fatal for the same downgrade reason an unknown
TYPE was -- reconcile the two answers rather than documenting them differently.

SEQUENCING: after DUR-11, which is the task actually converting the refusals into discards.

PROOF. `test -f PROTOCOL.md && grep -q 'unknown record type' PROTOCOL.md` -- FAILS TODAY at clause 1,
correctly and non-vacuously, because PROTOCOL.md does not exist. The previous proof_cmd
(`grep -n "unknown record\|refuses to start\|startup failure" PROTOCOL.md`) named a nonexistent file
AND grepped for the retired policy's wording.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DOCS-2](../../DOCS/DOCS-2--41c52cfa/task.md) — DOCS-2: PROTOCOL.md -- wire protocol + on-disk format (todo)
- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-3](../DUR-3--d8a991ea/task.md) — DUR-3: Replay/recovery on start (done)
- [DUR-4-FU-DOCS](../DUR-4-FU-DOCS--0b6d5c11/task.md) — DUR-4-FU-DOCS: state invariants 4/6 as explicit NARROWINGS + document the WAL recovery AP… (todo)
- [bd3cc650-da3f-483d-a48d-321ab2a8d1dd](../CONTRACTS.md-55-is-stale-says-no-WAL-record-types-wire-v--bd3cc650/task.md) — CONTRACTS.md:55 is stale -- says no WAL record types/wire version exist yet, false as of… (todo)
- [e875182a-aa12-48ba-8b58-71a1821e0c4d](../Codec-forward-compat-comment-contradicts-the-code-pre-ex--e875182a/task.md) — Codec forward-compat comment contradicts the code (pre-existing) -- reconcile or fix (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DOCS-2](../../DOCS/DOCS-2--41c52cfa/task.md) — DOCS-2: PROTOCOL.md -- wire protocol + on-disk format (todo)
- [DUR-4-FU-DOCS](../DUR-4-FU-DOCS--0b6d5c11/task.md) — DUR-4-FU-DOCS: state invariants 4/6 as explicit NARROWINGS + document the WAL recovery AP… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
