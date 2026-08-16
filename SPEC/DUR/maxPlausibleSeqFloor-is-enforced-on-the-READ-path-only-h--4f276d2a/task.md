# maxPlausibleSeqFloor is enforced on the READ path only -- hub can persist a floor its own reader then refuses, and the documented remedy loops

| Field | Value |
| --- | --- |
| Public id | `4f276d2a-88d5-45fd-90e1-810429b3fb78` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | deferred |
| Priority | P0 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T15:32:07.121973+00:00 |
| Updated | 2026-08-08T15:44:58.094109+00:00 |
| Completed | — |

## Proof command

```sh
sed -n '/^func (f \*seqFloorFile) persistLocked/,/^}/p' internal/hub/seqfloorfile.go | grep -q maxPlausibleSeqFloor && go test -race -count=1 -run TestSeqFloorPersistRefusesAnImplausibleFloor ./internal/hub
```

## Status note

DEFERRED 2026-08-08 (user decision, recorded by spec-keeper -- see full text on RELAY epic notes / pending DECISIONS.md transfer): three-bus laptop<->internet-machine<->this-machine topology, every inter-bus link an SSH tunnel, no bus ever publicly listening, user is sole operator/local user of all three machines. Local-attacker scenario is explicitly OUT OF SCOPE while that holds; this finding requires local write access to the data directory to exploit. Deferral is TIME-BOXED to 'until end-to-end relay is running', not indefinite, and REVERSES immediately if any bus is exposed on a real interface, a second local user is added to any machine, or an uncontrolled peer bus is admitted.

## Description

SECURITY GATE FINDING (HIGH) against the seq-floor bound shipped under be447589-6583-4d5c-a9d4-ec9d9fef0f1c, committed at 217a3c0. PROVED BY RUNNING CODE (a throwaway test), not by reading.

MECHANISM. The bound is checked in ONE place, on the READ path: internal/hub/seqfloorfile.go:538 (`if n > maxPlausibleSeqFloor`), inside the parse. The WRITE path bounds nothing: `persistLocked` (seqfloorfile.go:365) refuses only a DECREASE and then writes whatever it was handed. Re-verified at HEAD 16da89f by spec-keeper: the whole persistLocked body is 11 lines and `sed -n '/^func (f \*seqFloorFile) persistLocked/,/^}/p' ... | grep -c maxPlausibleSeqFloor` returns 0.

WHY THAT IS REACHABLE. `hub.Open` derives the floor from three LOG-derived sources and persists it through `raise()` (seqfloorfile.go:311) -> `persistLocked`. So the value the bound exists to reject can arrive from the log rather than from the floor file, and never passes `parseSeqFloorLine` at all.

MEASURED. `raise(math.MaxUint64)` is ACCEPTED and fsynced, and the next `openSeqFloorFile` REFUSES the file this package itself just wrote. The documented remedy -- move the floor file aside -- re-derives from the poisoned log and LOOPS. The same test proved a 256-value window in which a floor at the bound plus one `MintBatchSize` bricks the next start.

COMPOUNDING (do not treat as separate). WAL v1 is accepted with an UNKEYED CRC32 and laundered into a MAC'd v2 log (tracked as DUR-12-FU-V1LAUNDER, daf18983-fb58-47cd-8e1b-b9dc50a36f08), so a directory-write attacker reaches the floor VIA THE LOG and never touches parseSeqFloorLine -- i.e. the read-path bound is not merely incomplete, it is on the wrong side of the actual attack path.

MINIMAL FIX (given by the gate). Move the bound into `persistLocked`, the last point before bytes are written. That is the single choke point all four inputs (the three log-derived sources plus the file) pass through, so it fires at the source and covers them at once. Note persistLocked already carries the monotonicity refusal for exactly this reason -- its own comment says a bad value here "is caught at the last point before the bytes are written" -- so this is completing an argument the code already makes, not adding a new one.

SIBLING, NOT A DUPLICATE: 259b7033-2191-423f-bb7b-cff8c6b59dc1 bounds the wal-index-floor reserved value in internal/wal. This task is internal/hub only.

PROOF STATE OBSERVED (spec-keeper, HEAD 16da89f, not assumed): `bash scripts/proof-check.sh '<proof_cmd>'` -> verdict=FAIL, exit 1. RED, and RED FOR THE RIGHT REASON (the sed/grep pin returns 0 matches inside persistLocked, so the && short-circuits) -- RED today rather than VACUOUS. The test half must ALSO be observed RED before the fix.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [259b7033-2191-423f-bb7b-cff8c6b59dc1](../Bound-the-wal-index-floor-reserved-value-the-same-way-as--259b7033/task.md) — Bound the wal-index-floor reserved value the same way as the message-seq floor (todo)
- [DUR-12-FU-V1LAUNDER](../DUR-12-FU-V1LAUNDER--daf18983/task.md) — DUR-12-FU-V1LAUNDER: v1-format WAL laundering re-signs forged CRC32C records with the rea… (todo)
- [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../../CORE/Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) — Enforce data-directory permissions at startup, and bound the message-seq floor (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
