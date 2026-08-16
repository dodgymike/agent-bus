# seq-floor guard predicate keys on discard, not on accounting -- boundary-exact truncation escapes it at every record boundary

| Field | Value |
| --- | --- |
| Public id | `18eac796-d1fd-4619-94cb-1164bf989634` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T13:34:27.862278+00:00 |
| Updated | 2026-08-08T13:34:27.862278+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestSeqFloorGuardCatchesBoundaryExactTruncation ./internal/hub
```

## Description

MECHANISM: internal/hub/hub.go:702 guards with `!h.seqFloorFile.existedAtOpen() && o.LogRepaired != ""`. `LogRepaired` answers "did recovery PHYSICALLY REMOVE records" -- a proxy for "the log is complete". A truncation landing exactly on a record boundary removes nothing during recovery (replay reads cleanly to EOF, nothing is torn), so LogRepaired == "" and the guard does not fire even though records are missing -- they were removed by the CUT, not by recovery. This is a DIFFERENT bug from the ones already fixed under e120153b/db350e39 (which were about the floor's own bookkeeping); this one is that the REFUSAL PREDICATE reads the wrong signal.

MEASURED (see 9fd58deb's notes for the full test-oracle writeup): on a purpose-built specimen (7 delivered messages, seqs 1,2,3 and 257-260, floor written=22, 8900-byte WAL), two exhaustive byte-by-byte sweeps covering the WHOLE file (1-4439 and 4440-8900, 8900/8900 offsets) found the escape set is EXACTLY the record-boundary set: 22 of 22 record boundaries escape the guard (offsets 360, 427, 738, 805, 937, 1004, 2016, 2083, 3095, 3162, 4172, 4240, 4372, 4440, 5487, 5555, 6602, 6670, 7717, 7785, 8832, 8900). 13 of those 22 reissue a sequence already delivered; the other 9 are harmless only because the derived floor had already stepped past the delivered high-water by that point in THIS specimen's history -- not a property of the bug, so a differently-shaped directory would have more harmful boundaries. The refusal path itself is measured GOOD (8878/8900 = 99.75% loud refusals, ZERO silent) -- this task is about the remaining escape set, not about weakening or removing the existing refusal.

WHY A DISCARD-KEYED PREDICATE CANNOT SEE THIS: a paired measurement showed a boundary-exact cut and a mid-record cut at the SAME offset derive IDENTICAL floors (checked at 1004, 2016, 4240, 4440, 5487, 6602, 7785) -- floor derivation is one monotonic step function, the same on both recovery paths, and its steps land exactly on record boundaries. The two paths cannot disagree about the FLOOR; they only disagree about whether LogRepaired gets set. LogRepaired answers "was something torn", not the question that matters: does the surviving log account for every index durably authorised.

FIX REQUIRED: replace (or supplement) the o.LogRepaired-keyed predicate with one keyed on wal.Recovered's highest-CONSUMED-index field, once 9fd58deb exposes it -- i.e. refuse when the floor file is absent AND the log's highest consumed index is provably below what this run's replay can account for, rather than only when recovery physically discarded something. BLOCKED ON 9fd58deb: the field this predicate needs does not exist yet on wal.Recovered.

SCOPE: internal/hub/hub.go (the predicate at :702) plus the seq-floor guard tests (cmd/agent-bus/seqfloorrestart_test.go, seqfloormissing_test.go, internal/hub/mint_test.go). Do NOT weaken or remove the existing o.LogRepaired refusal path -- it is independently correct and measured good; this task ADDS coverage for the boundary-exact case, it does not replace working coverage.

ORACLE FOR THE FIX: must refuse at all 22 record-boundary offsets on the reference specimen (see 9fd58deb notes for the full list and the reasoning for why 22, not 13, is the right target), not just the 13 that happen to be harmful on that one specimen's history.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [9fd58deb-6fb8-4d4e-8bf1-6df01329c3b2](../Expose-on-wal.Recovered-the-highest-index-a-record-actua--9fd58deb/task.md) — Expose on wal.Recovered the highest index a record actually CONSUMED (todo)
- [db350e39-3dde-4166-b241-b21fa4635359](../Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) — Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable ind… (done)
- [e120153b-9d8a-4b6a-bd4e-89431954496b](../Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) — Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a na… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2a38cdec-528f-47ef-8f38-7f83465b0213](../CONTRACTS-ONDISK.md-and-four-sibling-Go-comments-oversta--2a38cdec/task.md) — CONTRACTS-ONDISK.md and four sibling Go comments overstate the seq-floor migration guard:… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
