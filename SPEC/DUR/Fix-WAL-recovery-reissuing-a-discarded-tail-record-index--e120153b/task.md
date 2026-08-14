# Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a narrowing) -- BLOCKED on DUR-12

| Field | Value |
| --- | --- |
| Public id | `e120153b-9d8a-4b6a-bd4e-89431954496b` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | in_progress |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T18:41:30.363906+00:00 |
| Updated | 2026-08-07T13:01:28.086292+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestWALRepairDoesNotReissueDiscardedIndex ./internal/wal
```

## Status note

DUR-12 block CLEARED 2026-08-07: DUR-12 (cbc9ab0c-3b34-48d0-acd8-5eabd4dc4a02, the CRC32C->HMAC-SHA256 MAC swap, ondisk-format-version=2) is done, so the internal/wal/recover.go rewrite it owned has landed and the concurrent-rewrite hazard this task was blocked on no longer applies. Reopened in_progress under feature-runner: code-complete via the durable wal-index-floor mechanism (internal/wal/indexfloor.go, ondisk-format-version=4), reviewer and security gates running.

## Description

internal/wal/recover.go (RepairLog, Repair.NextIndex comment ~lines 56-91) documents and internal/wal/recover_test.go:440 and crash_injection_test.go (e.g. ~line 960) currently ASSERT AS CORRECT that a damaged TAIL record has its index REISSUED on the next successful write: "That record is discarded and its index is reissued. If it had been acknowledged, an id a client saw is handed out again." CLAUDE.md invariant 1 ("ids are never reused, including across restarts") was reaffirmed by the user on 2026-08-02 WITHOUT NARROWING (DECISIONS.md, "Five decisions" #3, and the addendum to ID-2-WIRING-SCHEMA): "Recovery may not reissue an index it has already handed out, even for a record it discards... when recovery discards a record the sequence advances past the hole, it never rewinds." THIS IS THEREFORE A DEFECT TO FIX, NOT A NARROWING TO DOCUMENT. The question this task was originally filed to raise -- "is the quarantined-tail index reissue observable, should we narrow invariant 1?" -- is CLOSED; invariant 1 stands unmodified.

Contrast deliberately drawn by the user: invariant 4 (durability) WAS narrowed on 2026-08-02 -- that narrowing was a choice made up front and recorded as such. This reissue behaviour was discovered AFTER THE FACT (by reviewer and security, on DUR-11) and is REJECTED, not accepted.

FIX REQUIRED in internal/wal: when RepairLog discards a damaged tail record (whether length-only damage, a torn frame, or bit rot indistinguishable from an interrupted write), the sequence/index counter must advance PAST the hole and never be handed to the next Append -- i.e. NextIndex must be one past the highest index EVER OBSERVED IN THE FILE, including a discarded record, not one past the highest SURVIVING record. This likely touches: Repair.NextIndex computation, the length-field-repair and torn-frame-truncation paths in recover.go, and every existing test that currently asserts reissue as wanted behaviour (recover_test.go:440, crash_injection_test.go ~671/838/960/982, replay_crash_test.go ~345/512) -- those tests encode the REJECTED behaviour and must be flipped to assert no-reissue, not left in place.

PRIORITY RAISED TO P0 by triage on CONSEQUENCE, not by explicit user P0 label -- flagging this so the user can overrule: a shipped violation of a load-bearing identity invariant is exactly what invariant 1 exists to prevent (ids repeating), and the code is live today, not merely planned.

BLOCKED ON DUR-12: this task is in internal/wal, the same package/recovery loop DUR-12 (HMAC-SHA256 MAC replacing CRC32C, ondisk-format-version=2) is actively rewriting per the 2026-08-02 decision, and DUR-12s own description says it should SIMPLIFY the torn-tail heuristic under a strong MAC. Do NOT dispatch this task until DUR-12 lands, to avoid two agents rewriting internal/wal/recover.go concurrently.

RELATED: DUR-11 (884d3da4, commits 0c122fa/6bb9f6c) implemented exactly the reissue behaviour this task now rejects; see DUR-11s notes for the reconciliation.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [9fd58deb-6fb8-4d4e-8bf1-6df01329c3b2](../Expose-on-wal.Recovered-the-highest-index-a-record-actua--9fd58deb/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-12](../DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (done)
- [ID-2-WIRING-SCHEMA](../../ID/ID-2-WIRING-SCHEMA--80b54ee4/task.md) — ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [18eac796-d1fd-4619-94cb-1164bf989634](../seq-floor-guard-predicate-keys-on-discard-not-on-account--18eac796/task.md) — seq-floor guard predicate keys on discard, not on accounting -- boundary-exact truncation… (todo)
- [9fd58deb-6fb8-4d4e-8bf1-6df01329c3b2](../Expose-on-wal.Recovered-the-highest-index-a-record-actua--9fd58deb/task.md) — Expose on wal.Recovered the highest index a record actually CONSUMED (todo)
- [DUR-11-FU-CONTRACTS](../../DOCS/DUR-11-FU-CONTRACTS--5b178dde/task.md) — DUR-11-FU-CONTRACTS: CONTRACTS.md still documents the reverted refuse-to-start WAL policy… (todo)
- [HANDOVER-CHECK](../../HANDOVER/HANDOVER-CHECK--0f909b6c/task.md) — HANDOVER-CHECK: one command that tells you the health of this repo, plus its recorded out… (todo)
- [MSG-FU-SEQHIGHWATER](../MSG-FU-SEQHIGHWATER--6ebe51be/task.md) — MSG-FU-SEQHIGHWATER: persist the message-sequence high-water mark so deep log damage cann… (done)
- [a695f85f-0c69-42a8-a653-deed4960a610](../../DOCS/PROTOCOL.md-8-cites-Spec-Server-task-id-INVITE-PEERGUARD--a695f85f/task.md) — PROTOCOL.md §8 cites Spec Server task id INVITE-PEERGUARD (f5d91dbe) as if it were a comm… (todo)
- [db350e39-3dde-4166-b241-b21fa4635359](../Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) — Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable ind… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
