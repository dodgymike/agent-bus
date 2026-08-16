# MSG-FU-SEQHIGHWATER: persist the message-sequence high-water mark so deep log damage cannot regress it

| Field | Value |
| --- | --- |
| Public id | `6ebe51be-2486-4ab9-a25d-675b627675f6` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T22:31:37.370825+00:00 |
| Updated | 2026-08-08T10:29:43.119686+00:00 |
| Completed | 2026-08-08T10:18:14.541125+00:00 |

## Proof command

```sh
go test -race -run TestSequenceHighWaterSurvivesDeepDamage ./internal/hub
```

## Description

THE DEFECT (general, MEASURED, plus the whole-log-quarantine instance folded in from db350e39, now superseded): the hub derives its sequence floor from wal.Recovered.NextIndex-1, raised to the highest replayed sequence. Each message burns 1 sequence and 2 WAL indices, so a truncation removing MORE records than the bus issued sequences leaves the floor at or below a sequence already issued, and the bus reissues message ids (invariant 1). Measured over a 585-offset truncation sweep of a 2523-byte WAL: 70 offsets regressed, all at n <= 1449 of 2523 -- every cut losing more than half the records. Inside the genuine crash window (a tear between two fsyncs, n >= 2038) the strong property HOLDS and is asserted by TestMessagingCrashRecovery; deep cuts are media damage and the information needed to reconstruct the mark is gone with the bytes.

STRICTLY WORSE INSTANCE, folded in from db350e39 (SUPERSEDED, collapsed into this task 2026-08-07 by spec-keeper): internal/wal/recover.go (pre-fix, ~lines 252-262) -- when the ENTIRE WAL log is quarantined as corrupt, recovery started a FRESH log at index 1. No PREPARE record survived anywhere in the log. The derived high-water mark was therefore 0, and a bus that then minted sequences from 1 reissued EVERY sequence number it had EVER used -- not one index at a damaged tail, but the buss entire history. Nothing downstream could detect this; it was silent.

REMEDY (unambiguous, per user ruling recorded in commit 888f6c6, 2026-08-07 -- supersedes any earlier framing): persist the message-sequence high-water mark OUTSIDE the log, in its own small fsynced file, WRITTEN AHEAD of the sequence number it authorises, read by recovery even when the WAL is damaged or quarantined -- exactly the shape internal/ids/suffixstore.go (commit 61b7c9a) already uses for the per-name agent-id suffix floor. Do NOT frame post-quarantine id reuse as an accepted, bounded risk anywhere in new work -- that framing is SUPERSEDED (DECISIONS.md 2026-08-07, "SUPERSEDES two earlier passages"); invariant 1 stands unnarrowed, reuse is a DEFECT.

IMPLEMENTATION STATUS (found by spec-keeper triage 2026-08-07, code exists but is UNCOMMITTED -- verify before writing anything new): an implementer working task db350e39 has ALREADY BUILT this exact mechanism at the WAL layer: internal/wal/indexfloor.go (NEW) plus companion changes to internal/wal/{doc,log,recover,replay,salvage,writer}.go. It persists a durable WAL record-index floor (<data-dir>/wal-index-floor, on-disk format version 4) written ahead of each reservation. hub.go derives its sequence floor from o.NextIndex = wal.Recovered.NextIndex, so this WAL-layer fix APPEARS to close this tasks hub-level hazard too, transitively, via the existing derivation path -- without needing a separate hub-owned persisted file. NOT YET VERIFIED, and this task should not be marked done until it is: (1) 9 internal/wal tests are currently failing pending a test-engineer pass fixing assertions that encoded the old reissue-permitted behaviour; (2) internal/hub/hub.go:137-140 and :383-394 still contain a comment and an ERROR log message asserting ids "may repeat" after quarantine -- STALE once this lands, tracked separately as 68c1788f-6043-4c2d-b409-887f71507d69; (3) no test yet proves the hub-level guarantee end-to-end (this tasks own proof_cmd, TestSequenceHighWaterSurvivesDeepDamage, does not exist yet -- do not complete on a VACUOUS proof). CHECK whether internal/wal/indexfloor.go already satisfies this task before writing new code.

ON-DISK RECORD TYPE: needs a RESERVED format/record-type number -- do NOT pick one by eyeballing the list. One already exists that appears to be the SAME mechanism this task needs: ondisk-format-version value=4, reserved by feature-runner 2026-08-07T12:06:32Z, note "WAL record-index floor file (internal/wal/indexfloor.go, <data-dir>/wal-index-floor) -- the durable ceiling that stops recovery reissuing indices after a tail discard or a whole-log quarantine (tasks e120153b, db350e39)". Confirm this covers the message-sequence case (it appears to, since hub derives sequence purely from wal.Recovered.NextIndex) before reserving a NEW number for what may be a redundant record type.

CROSS-REFERENCES: db350e39 (SUPERSEDED 2026-08-07, collapsed into this task -- quarantine-specific evidence folded in above); e120153b (separate task, tail-discard-specific instance of the same invariant, NOT a duplicate of this one, also apparently closed by the same indexfloor.go work per its own implementer note -- do not touch e120153b from this task); 68c1788f-6043-4c2d-b409-887f71507d69 (hub.go stale-assertion inversion, filed by spec-keeper 2026-08-07, blocked on this task landing).

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [68c1788f-6043-4c2d-b409-887f71507d69](../Invert-stale-internal-hub-hub.go-quarantine-reissue-perm--68c1788f/task.md) — Invert stale internal/hub/hub.go quarantine reissue-permitted assertions once MSG-FU-SEQH… (todo)
- [db350e39-3dde-4166-b241-b21fa4635359](../Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) — Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable ind… (done)
- [e120153b-9d8a-4b6a-bd4e-89431954496b](../Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) — Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a na… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [68c1788f-6043-4c2d-b409-887f71507d69](../Invert-stale-internal-hub-hub.go-quarantine-reissue-perm--68c1788f/task.md) — Invert stale internal/hub/hub.go quarantine reissue-permitted assertions once MSG-FU-SEQH… (todo)
- [ID-2-WIRING](../../ID/ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (superseded)
- [ID-2-WIRING-OBSERVER](../../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [b7ac3580-d4ff-44f0-9d10-a734ef4a6043](../internal-hub-hub.go-590-s-no-floor-file-quarantine-ERROR--b7ac3580/task.md) — internal/hub/hub.go:590's no-floor-file quarantine ERROR promises the file will be writte… (todo)
- [db350e39-3dde-4166-b241-b21fa4635359](../Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) — Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable ind… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
