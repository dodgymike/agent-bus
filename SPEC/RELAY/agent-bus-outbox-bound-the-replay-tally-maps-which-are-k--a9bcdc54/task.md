# agent-bus outbox: bound the replay tally maps, which are keyed off attacker-influenced file content (+3 cosmetic reviewer NICE-TO-HAVEs)

| Field | Value |
| --- | --- |
| Public id | `a9bcdc54-fe1c-4497-9294-13efe2fca8fc` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T15:10:18.740468+00:00 |
| Updated | 2026-08-21T15:10:18.740468+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestOutboxCommandReplayTallyIsBoundedAndDegradesToUnverified' ./cmd/agent-bus
```

## Description

Filed from RELAY-54's REVIEW GATE (911841af-83d7-445f-bf46-9097eeb0661d). Carries the reviewer's
remaining NICE-TO-HAVE findings from the 2nd-pass `kind=response` (PASS, 0 new MUST-FIX) on
`agent-bus outbox`. Both gates (reviewer, security) ended PASS; none of this is blocking, and none
of it was a reason to hold RELAY-54.

CONTEXT — what RELAY-54 shipped
`agent-bus outbox` is an OFFLINE, READ-ONLY drain gate: it points at a bus data directory that no
server is running against, replays `bus.wal` through `wal.Replay` into a `relay.Outbox`, and answers
"is this bus drained enough to restart onto a new binary?". Exit codes 0-9 with precedence
1 > 8 > 6 > 7 > 0 (trust outranks verdict). The load-bearing property is that exit 0 is
STRUCTURALLY UNREACHABLE when anything was discarded, missing or refused — an operator is being
trained to trust a 0 absolutely, so every change here must preserve that.

===========================================================================================
LEAD ITEM (the only non-cosmetic one) — BOUND THE REPLAY TALLY MAPS
===========================================================================================
Reviewer, VERBATIM:
  "(1) outbox.go:914/949-960 the replay tally's pending/settled maps are UNBOUNDED and keyed off
   file content, in a codebase that bounds this deliberately everywhere else (relay
   MaxOutboxRetainedJobs, wal maxOpenPrepares/maxOpenPrepareBytes, which exist because 'the bytes
   being walked are ATTACKER-INFLUENCED'); cap them and print 'further refusals not named
   individually', mirroring maxDiscardsLogged."

Re-verified at HEAD (line numbers current as of 2026-08-21):
  - cmd/agent-bus/outbox.go:913-915 — `tally := &outboxReplayTally{pending: map[string]relay.OutboxRecord{},
    settled: map[string]struct{}{}}` then `wal.Replay(walPath, tally.apply(ob))`.
  - cmd/agent-bus/outbox.go:949-960 — the struct: `pending map[string]relay.OutboxRecord` (a WHOLE
    record per distinct job id, not a bare id) and `settled map[string]struct{}`.
  - cmd/agent-bus/outbox.go:967-978 — `apply` inserts on every `relay.OutboxRecordKind` record it
    decodes, keyed by `r.JobID`, with no cap of any kind.
  - cmd/agent-bus/outbox.go:1022-1046 — the consumer builds a third unbounded map (`present`) plus a
    `missing` slice and logs one ERROR per missing job.

THIS IS A MEMORY CHANNEL, NOT TIDY-UP. The map keys come OUT OF THE FILE BEING READ, and this is an
offline reader an operator points at ARBITRARY directories — copied, restored, forensic, or handed
over by someone else. The tally exists precisely because `relay.Outbox.Apply` swallows its own
refusals (logs at ERROR, returns nil), so a refusal is otherwise invisible; that is a good reason for
the tally to exist and no reason for it to be unbounded. The rest of this codebase bounds exactly
this shape ON PURPOSE:
  - internal/wal/replay.go:252-267 evicts open prepares to stay inside `maxOpenPrepares` (=1024,
    replay.go:427-430) and `maxOpenPrepareBytes`, with a discard reason that says so;
  - internal/wal/salvage.go:139-143 caps retained discard DETAIL at `maxDiscardsRetained` (=64)
    while the COUNT stays exact;
  - internal/wal/recover.go:628-654 caps per-discard log lines at `maxDiscardsLogged` (=16) in FILE
    ORDER;
  - internal/relay/outbox.go:117-124 caps the table itself at `MaxOutboxRetainedJobs` /
    `MaxOutboxRetainedBytes`.
Note the table's cap does NOT bound the tally: the tally walks the whole HISTORICAL log, so the
number of DISTINCT job ids it meets is bounded by the log's lifetime of relay traffic, not by the
live table's retention.

REQUIRED SHAPE OF THE FIX (all three, or it is not fixed):
  1. A DOCUMENTED CAP on the tally's memory — entries and/or bytes, in the style of
     `maxOpenPrepares`/`maxOpenPrepareBytes`, with the constant carrying the reasoning in its
     comment, not just a number.
  2. A LOUD, SPECIFIC REPORT WHEN THE CAP IS HIT — invariant 6: the defect is the SILENT discard,
     not the discard. Mirror `maxDiscardsLogged`: name what can be named, then say plainly that
     there are more and that they are NOT named individually (reviewer's wording: "further refusals
     not named individually"). Both the human report and `--json` must carry it.
  3. IT MUST DEGRADE TOWARD "UNVERIFIED", NEVER TOWARD CLEAN. Hitting the cap means the run can no
     longer prove no record was refused, so the result must become untrustworthy — exit 8
     (`exitOutboxUnverified`), never 0. Do NOT let a capped run fall into the `default` branch of
     `writeOutboxHuman` (outbox.go:1324-1327) or leave `outboxIntegrity.Trustworthy` true
     (outbox.go:1048). The whole property RELAY-54 shipped is that exit 0 is unreachable when
     anything was refused or missing; a cap that quietly forgets refusals would reintroduce exactly
     the false-clean-drain the P0 MUST-FIX removed.

===========================================================================================
THE OTHER NICE-TO-HAVEs, VERBATIM (all cosmetic; all re-verified present at HEAD)
===========================================================================================
Reviewer (3), VERBATIM:
  "the evicted-prepare discard (replay.go:264, Length 0) is labelled 'a record index is ABSENT' and
   counted in index_gaps - trust-safe (still 8, never 0) but the wording mis-describes it."
  -> cmd/agent-bus/outbox.go:1000-1008 (the `d.Length == 0 && !d.Severe && d.Stage != "framing"`
     arm: `out.IndexGaps++` plus the `lg.Warn("a record index is ABSENT ...")` line).
     internal/wal/replay.go:252-267 is the evicting site whose discard it mis-labels.

Reviewer (4), VERBATIM:
  ":1305-1311 is the only verdict branch with no untrusted-read qualifier, unlike the abandoned
   branch at :1314-1319; it prints 'start the old binary again ... run this command again' under a
   trust code."
  -> now cmd/agent-bus/outbox.go:1304-1311 (`case out.Counts.Pending > 0`), against the abandoned
     branch's `if out.Integrity.Trustworthy` qualifier at outbox.go:1312-1319. NOT a correctness
     bug — exit precedence already returns 1/8 over 6 — only the prose is unqualified.

Reviewer (5), VERBATIM:
  "no test for exit 9 (exitOutboxReportFailed) or for outboxWindowProse's non-whole-hour branch -
   both trivially unit-testable."
  -> `exitOutboxReportFailed = 9` at cmd/agent-bus/outbox.go:245-255, raised at outbox.go:765;
     `outboxWindowProse` at outbox.go:258-287, non-whole-hour fallback at outbox.go:272-278.
     Confirmed: `grep -n 'outboxWindowProse\|exitOutboxReportFailed' cmd/agent-bus/outbox_test.go`
     returns NOTHING.

===========================================================================================
DELIBERATELY EXCLUDED — reviewer's NICE-TO-HAVE (2) IS ALREADY FIXED. DO NOT REDO IT.
===========================================================================================
The reviewer wrote: "(2) writeOutboxIntegrity returns at :1419 before the dangling count, so a
clean human run with dangling>0 reads 'the log replayed with nothing discarded' and never learns a
prepare was dropped ... --json is fine." That was fixed after the re-review, inside
`writeOutboxIntegrity`'s trustworthy branch: cmd/agent-bus/outbox.go:1424-1436 now prints the
dangling-prepare count on the CLEAN path too, with the invariant-4 explanation that a dangling
prepare was never committed and so nothing there was ever acknowledged, and that the verdict still
stands. Verified in the working tree before filing this task. It is NOT part of this task.

Also NOT part of this task: the FIVE NICE-TO-HAVEs from the reviewer's FIRST-pass
CHANGES-REQUESTED note (the 0-5 alignment overclaim, hardcoded "24 HOURS", the
OutboxSettledRetention/retryHorizon comment, the compound `if` at outbox_test.go:743, and the
JSON-encode failure path returning exit 1). The re-review states all five are FIXED.

===========================================================================================
SEPARATE, DO NOT MERGE
===========================================================================================
Two follow-ups already filed from RELAY-54's SECURITY gate cover an unrelated defect (a read-only
command minting `wal-mac.key` as a side effect) and must stay separate:
  - 8cfd52e7-6cbd-4d29-b34d-c3dee87e73e5 — "agent-bus peer list mints wal-mac.key as a side effect
    of a read-only command"
  - b5089ddf-5a5a-41e0-8278-036f6a195e2a — "agent-bus operator list mints wal-mac.key as a side
    effect of a read-only command"

===========================================================================================
PRIORITY REASONING (P2, not P1) — recorded so it can be challenged rather than re-litigated
===========================================================================================
A remote party genuinely INFLUENCES the map keys: job ids are derived from relayed traffic a peer
drives, and the historical log is not bounded by the live table's retention. But the allocation
happens in a SHORT-LIVED OFFLINE TOOL the operator explicitly invoked against a directory of their
choosing — no remote request causes a RUNNING bus to allocate here, and the serving path is
untouched. The failure mode is also fail-safe today: an OOM kill exits non-zero, so it cannot
manufacture a false `DRAINED`/exit 0; the harm is that the drain gate itself becomes unanswerable
(which does block a rollout, hence P2 rather than P3). Raise to P1 only if someone demonstrates a
path where a remote peer causes this allocation in a running server.

SCOPE / INVARIANTS TO READ IN FULL FIRST: 4 (nothing acknowledged before durable, incl. the
2026-08-02 narrowing), 5 (memory serves, disk is truth), 6 (metadata-only log, EVERY discard logged
loudly and specifically — the silent discard is the defect), 7 (the compiled CLI is THE client; any
change to exit codes or `--json` shape needs its CONTRACTS-CLI.md entry IN THE SAME TASK).

DONE WHEN: the cap exists and is documented; hitting it produces a loud, specific, bounded report in
both human and `--json` output; a capped run is UNTRUSTWORTHY (exit 8, never 0) and has a test
proving exit 0 is unreachable; the three cosmetic items above are fixed; tests exist for exit 9 and
for `outboxWindowProse`'s non-whole-hour branch; and CONTRACTS-CLI.md reflects any change to the
`--json` integrity block.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [8cfd52e7-6cbd-4d29-b34d-c3dee87e73e5](../agent-bus-peer-list-mints-wal-mac.key-as-a-side-effect-o--8cfd52e7/task.md) — agent-bus peer list mints wal-mac.key as a side effect of a read-only command (todo)
- [RELAY-54](../RELAY-54--911841af/task.md) — RELAY-54: an abandoned outbox job is invisible to every subcommand -- the drain a rollout… (done)
- [b5089ddf-5a5a-41e0-8278-036f6a195e2a](../../AUTH/agent-bus-operator-list-mints-wal-mac.key-as-a-side-effe--b5089ddf/task.md) — agent-bus operator list mints wal-mac.key as a side effect of a read-only command (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
