# DUR-11-FU-CONTRACTS: CONTRACTS.md still documents the reverted refuse-to-start WAL policy verbatim

| Field | Value |
| --- | --- |
| Public id | `5b178dde-e83a-4bdb-90aa-951c44624c5f` |
| Key | _(null in the export)_ |
| Epic | [DOCS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | documentation |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T19:15:10.944103+00:00 |
| Updated | 2026-08-08T10:29:52.838745+00:00 |
| Completed | — |

## Proof command

```sh
grep -qF "RepairLog" CONTRACTS-ONDISK.md && grep -qF "bus.wal.corrupt-" CONTRACTS-ONDISK.md && grep -qF ".repair" CONTRACTS-ONDISK.md && grep -qF "Rewritten" CONTRACTS-ONDISK.md && grep -qF "Quarantined" CONTRACTS-ONDISK.md && grep -qF "DiscardCount" CONTRACTS-ONDISK.md && grep -qF "MissingRecords" CONTRACTS-ONDISK.md && grep -qF "Exhausted" CONTRACTS-ONDISK.md && ! grep -qE "provably torn tail|refuses to start and leaves the file byte-for-byte|RepairTail" CONTRACTS-ONDISK.md
```

## Status note

Proof strengthened 2026-08-02 to a conjunction requiring the eight missing terms PRESENT (RepairLog, bus.wal.corrupt-, .repair, Rewritten, Quarantined, DiscardCount, MissingRecords, Exhausted) AND the three stale phrases still ABSENT. Ran through proof-check.sh: verdict=FAIL class=file-assertion exit=1 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0 (RED, confirmed today, 2026-08-02) -- this is the correct RED state since the positive scope has not landed. Do NOT dispatch until DUR-12 (cbc9ab0c) lands; it owns internal/wal and the on-disk format right now.

## Description

Discovered during the DUR-11 orphaned-task reconciliation pass (2026-08-02). HALF OF THIS TASK IS ALREADY DONE: c7e017d removed the stale "provably torn tail" / "refuses to start and leaves the file byte-for-byte" / "RepairTail" phrasing (verified absent from CONTRACTS-ONDISK.md, the plane the WAL-repair section moved to in the CONTRACTS split, 360a2679). THE REMAINING, UNMET HALF: CONTRACTS-ONDISK.md has ZERO mention of RepairLog, the bus.wal.corrupt-<ts> quarantine-rename-aside artefact name, the .repair temp-file-during-rewrite artefact name, or the Repair/Recovered struct fields actually surfaced to callers (Rewritten, Quarantined, DiscardCount, MissingRecords, Exhausted) -- confirmed via grep, zero matches for every one of the eight terms (2026-08-02). Fix: document the SHIPPED RepairLog / quarantine / always-restart behaviour in CONTRACTS-ONDISK.md, naming the on-disk artefacts and enumerating the struct fields.

*** BLOCKING: DO NOT DISPATCH until DUR-12 (cbc9ab0c) lands. *** DUR-12 is rewriting the on-disk WAL format (CRC32C -> HMAC-SHA256 MAC, format version 2) right now and will change this exact plane -- documenting the WAL surface concurrently would be stale on arrival, same ordering constraint applied to e120153b and db350e39.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTRACTS-SPLIT](../CONTRACTS-SPLIT--360a2679/task.md) — CONTRACTS-SPLIT: split CONTRACTS.md into per-plane files (pure move) + retarget every pro… (done)
- [DUR-11](../../DUR/DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-12](../../DUR/DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (in_progress)
- [db350e39-3dde-4166-b241-b21fa4635359](../../DUR/Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) — Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable ind… (done)
- [e120153b-9d8a-4b6a-bd4e-89431954496b](../../DUR/Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) — Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a na… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [9160ba8d-09f8-4510-bd0c-dcf1b22b82a5](../../DUR/Startup-summary-silently-omits-whole-log-quarantine-quar--9160ba8d/task.md) — Startup summary silently omits whole-log quarantine (quarantined/discard_count/discarded_… (done)
- [CONTRACTS-SPLIT](../CONTRACTS-SPLIT--360a2679/task.md) — CONTRACTS-SPLIT: split CONTRACTS.md into per-plane files (pure move) + retarget every pro… (done)
- [f0ef1ed9-cbcb-4ddd-9dec-394e1800ae78](../Stale-CONTRACTS.md-pointers-after-the-CONTRACTS-SPLIT-RE--f0ef1ed9/task.md) — Stale CONTRACTS.md pointers after the CONTRACTS-SPLIT: README.md:88, AGENT_PROTOCOL.md:12… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
