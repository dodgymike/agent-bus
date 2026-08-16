# CONTRACTS-ONDISK.md and four sibling Go comments overstate the seq-floor migration guard: it says the window CLOSES, but boundary-exact truncation escapes it

| Field | Value |
| --- | --- |
| Public id | `2a38cdec-528f-47ef-8f38-7f83465b0213` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T13:34:58.902048+00:00 |
| Updated | 2026-08-08T13:56:26.161450+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'grep -q "boundary-exact truncation" CONTRACTS-ONDISK.md && ! grep -rn "CLOSES the window immediately\|the one uncovered case\|window is closed by the very start" internal/hub/seqfloorfile.go internal/hub/hub.go internal/hub/mint_test.go'
```

## Description

CONTRACTS-ONDISK.md:326 (message-seq-floor section) currently reads: "hub.Open falls back to sources (1)-(3), logs at WARN when the directory is not otherwise fresh, and -- when the derived floor is non-zero -- CLOSES the window immediately by persisting it." That claim is the same false-all-clear shape already found and fixed in the internal/hub/hub.go WARN log line's own wording (see 9fd58deb's notes and be447589): the guard this sentence describes is keyed on o.LogRepaired (did recovery physically discard something), which is a proxy for log completeness with one hole -- a truncation landing exactly on a record boundary tears nothing, so the guard does not fire even though records are missing. Measured: the escape set is exactly the record-boundary set (22 of 22 record boundaries escape on the reference specimen; see 9fd58deb's notes for the full figures). CONTRACTS-ONDISK.md does not mention this at all; an operator reading it is told the window closes on a successful start with a non-zero derived floor, which is not true at every record-boundary-exact truncation.

FIX: qualify the "CLOSES the window" sentence (and the neighbouring "Migration residual" / "Be precise about when the window actually closes" paragraphs a few lines below, which have the same gap) to say plainly: the guard covers DISCARD-DETECTABLE damage (recovery found something torn) and NOT boundary-exact truncation (a cut that lands exactly on a record boundary, which recovery cannot distinguish from a log that legitimately ended there). Cross-reference the tracking task (9fd58deb, and its blocked follow-up 18eac796-d1fd-4619-94cb-1164bf989634) so a reader knows this is tracked, not merely disclosed once and forgotten.

SCOPE: CONTRACTS-ONDISK.md only (the message-seq-floor section, roughly lines 299-350). Do not touch internal/hub/hub.go -- its comment/WARN text is being handled under a separate, already-in-flight dispatch; this task is the operator-facing PLANE FILE, which is a different audience and currently has NO version of this caveat at all (checked: grep -n boundary-exact CONTRACTS-ONDISK.md currently finds nothing).
WIDENED 2026-08-08 (spec-keeper). The reviewer that PASSed the hub.go/mint_test.go rewrite (owned
separately, see Spec Server task 9fd58deb and the new task filed for that specific rewrite) flagged
that the SAME false-all-clear claim ("closes/closed the window", "the one uncovered case") survives
in FOUR sibling Go comments this task did not originally cover -- and one of them is the very source
the new honest hub.go block cites, so fixing only the docs plane leaves the origin uncorrected:

  - internal/hub/seqfloorfile.go:231 -- "Open logs it at WARN when the data directory is not
    otherwise fresh, and CLOSES the window immediately by persisting the derived floor."
  - internal/hub/seqfloorfile.go:241 -- "Missing-file plus quarantine on the SAME start is the one
    uncovered case" -- stated as the ONLY gap, when boundary-exact truncation on a NON-quarantine
    start is also uncovered and is the one the new hub.go WARN and 9fd58deb now document.
  - internal/hub/hub.go:716 -- repeats "the one uncovered case" (quoting seqfloorfile.go's framing)
    about 40 lines above the new honest block added under 9fd58deb's rewrite, which directly refutes
    it in the same file.
  - internal/hub/mint_test.go:455 -- "The window is closed by the very start that finds it open: Open
    writes the derived floor before it serves." -- same shape, in a test's doc comment this time.

FIX for all four: same correction as the hub.go WARN -- state plainly that the guard covers
DISCARD-DETECTABLE damage only (o.LogRepaired / recovery physically removed something) and NOT a
truncation landing exactly on a record boundary, which recovery cannot distinguish from a log that
legitimately ended there. Do not claim the window is closed or that the uncovered case is limited to
missing-file-plus-quarantine; boundary-exact truncation on ANY start (quarantine or not) is a second,
now-documented uncovered case. Cross-reference 9fd58deb.

ALSO WIDENED to two more CONTRACTS-ONDISK.md locations beyond the original 325-327 sentence, both in
the message-seq-floor section:

  - CONTRACTS-ONDISK.md:334-337 ("Migration residual, stated plainly...") and :339-346 ("Be precise
    about when the window actually closes...") -- both currently frame the ONLY residual as the
    missing-file-plus-quarantine-on-first-start case. Neither mentions boundary-exact truncation at
    all, which is a second, independent way the "window" stays open past a successful start with a
    non-zero derived floor. Add it alongside the existing residual, do not let the new caveat read as
    though it replaces or narrows the one already documented there.

  - CONTRACTS-ONDISK.md:269-273 is SEPARATELY STALE (security finding, distinct from the false-
    all-clear shape above): it documents a valid-digest `floor 18446744073709551615` as *adopted*
    ("seals the allocator at MaxUint64, every subsequent mint fails permanently"). At HEAD this is no
    longer true: internal/hub/seqfloorfile.go:539 has a plausibility bound that REFUSES an
    implausibly-high floor outright (ErrSeqFloorFileCorrupt, naming both the file and the remedy:
    "move message-seq-floor aside and restart") rather than adopting it and bricking the allocator.
    Update the bullet to describe the refusal, not the adoption -- the DoS-shaped conclusion
    ("denial of service, not an id reissue") may no longer be the right framing once the value is
    refused rather than adopted; the implementer should re-derive whatever the current behaviour's
    actual failure mode is (refusal is FATAL and not regenerated, per the CORRUPT-file paragraph a
    few lines below) and write that, not assume the old conclusion still holds.

SCOPE stays CONTRACTS-ONDISK.md plus the four Go comment locations listed above. Still do not touch
the hub.go WARN log line itself or its guard predicate -- that is 9fd58deb's tracking task and the
separately-filed task for the WARN-wording rewrite; this task's job is every OTHER place the same
claim was written down.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [18eac796-d1fd-4619-94cb-1164bf989634](../seq-floor-guard-predicate-keys-on-discard-not-on-account--18eac796/task.md) — seq-floor guard predicate keys on discard, not on accounting -- boundary-exact truncation… (todo)
- [9fd58deb-6fb8-4d4e-8bf1-6df01329c3b2](../Expose-on-wal.Recovered-the-highest-index-a-record-actua--9fd58deb/task.md) — Expose on wal.Recovered the highest index a record actually CONSUMED (todo)
- [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../../CORE/Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) — Enforce data-directory permissions at startup, and bound the message-seq floor (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [HANDOVER-REGISTER](../../HANDOVER/HANDOVER-REGISTER--7fddae9d/task.md) — HANDOVER-REGISTER: KNOWN_ISSUES.md, the known-defect register (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
