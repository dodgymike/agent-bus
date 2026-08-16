# internal/hub/hub.go:590's no-floor-file quarantine ERROR promises the file will be written this start, but the LogRepaired guard now refuses Open before that write happens

| Field | Value |
| --- | --- |
| Public id | `b7ac3580-d4ff-44f0-9d10-a734ef4a6043` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T13:58:31.975949+00:00 |
| Updated | 2026-08-08T13:58:31.975949+00:00 |
| Completed | — |

## Proof command

```sh
grep -n "so the next one is covered" internal/hub/hub.go; test $? -ne 0
```

## Description

internal/hub/hub.go's quarantine branch (currently around line 591-611, the `switch` inside `if o.Quarantined != ""`) has a `default:` case whose h.log.Error(...) call ends: "This is the one-start migration window for a data directory written before " + SeqFloorFileName + " existed; the file is written on this start, so the next one is covered".

That promise is now FALSE, and not merely optimistic -- it is directly CONTRADICTED by the guard a few dozen lines below it in the same function. cmd/agent-bus/logrepair.go's describeLogRepair sets a non-empty LogRepaired string whenever rec.Repaired.Quarantined != "" -- i.e. on every quarantine, unconditionally. hub.go's guard (currently ~line 732): `if h.seqFloorFile != nil && !h.seqFloorFile.existedAtOpen() && o.LogRepaired != "" { return nil, fmt.Errorf(...ErrSeqFloorUnprovable...) }` -- fires on EXACTLY the population the quarantine ERROR at line ~606 is printed for (no seq-floor file on disk, log just repaired/quarantined) and REFUSES to open the hub at all. Open returns an error before it ever reaches the `h.seqFloorFile.raise(floor)` call (~line 745) that would write the file.

So the sequence on that start, in order, is: (1) the quarantine block logs the ERROR at line ~606 promising the file is written this start; (2) a few lines later in the SAME Open call, the guard at ~732 refuses and Open returns an error; (3) main.go's caller treats that as fatal (`opening the messaging hub: %w`) and the server does not start. The file is never written. The very next line an operator reads after the reassurance is the refusal that falsifies it.

This is the same reassurance SHAPE already corrected in the migration WARN under Spec Server task 9fd58deb (and its still-open follow-up task for the sibling comments/docs) -- a sentence claiming a check or a write closes something that the code, read a few lines further, does not actually let happen on this path.

FIX: rewrite the tail of the no-floor-file quarantine ERROR (hub.go, currently ~line 606) to stop promising the file is written this start. State plainly that whether the file gets written on this start depends on the guard below (o.LogRepaired / existedAtOpen()): when DataDir is configured (h.seqFloorFile != nil) this exact condition (no floor file + a repaired/quarantined log) makes Open REFUSE outright rather than write the file and continue -- so for a DataDir-backed bus this ERROR is typically followed immediately by a fatal refusal on the SAME start, not by a covered next one. If there is a real path where the file DOES get written after this ERROR (e.g. no DataDir configured, or seqFloorFile is nil for some other reason), name that path explicitly instead of a blanket claim. Add or extend a hub_test.go/mint_test.go case that starts a hub with a quarantine, no floor file, and a non-empty LogRepaired, and asserts BOTH that this ERROR is logged AND that Open returns ErrSeqFloorUnprovable on the same call -- pinning the contradiction so the wording cannot regress silently.

Origin: reviewer flagged this while reviewing the seq-floor migration WARN rewrite (Spec Server task 9fd58deb / its sibling follow-up). Checked the current backlog (search by q= for the exact phrase and for line 590/606, plus a grep of SPEC.md) before filing -- no existing task covers this call site or this specific contradiction; the nearby task 'Invert stale internal/hub/hub.go quarantine reissue-permitted assertions once MSG-FU-SEQHIGHWATER lands' (SPEC.md, no numbered key) covers the OTHER quarantine case (the seqFloorFile-existed-and-survived branch's 'message ids may repeat' language, hub.go ~137-140/383-394) and is explicitly blocked pending 6ebe51be -- this is a different branch, a different defect (promise vs contradiction, not staleness), and is NOT blocked.

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
- [MSG-FU-SEQHIGHWATER](../MSG-FU-SEQHIGHWATER--6ebe51be/task.md) — MSG-FU-SEQHIGHWATER: persist the message-sequence high-water mark so deep log damage cann… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
