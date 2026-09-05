# DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start

| Field | Value |
| --- | --- |
| Public id | `26c2ce16-2962-47a5-93f0-5f66e4e268f2` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | superseded |
| Priority | P2 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T15:00:04.786601+00:00 |
| Updated | 2026-08-02T17:58:44.014480+00:00 |
| Completed | — |

## Status note

Dissolved by the 2026-08-02 always-restart decision, which states in terms that it 'removes the permanent-refuse-to-start DoS, and with it the need for the operator escape hatch ... always-restart IS the escape hatch'. Residual value re-homed: WAL dumper -> CLI epic (CLI-6/CLI-8, not a shell wrapper -- invariant 7 amended); discard counters -> DUR-11 + CORE-5; failing-media signature -> CORE-5.

## Description

DISSOLVED 2026-08-02 BY USER DECISION -- ALWAYS-RESTART IS THE ESCAPE HATCH.

This task existed because "refuse to start" was the designed answer to several WAL damage classes and
an operator facing a refused boot had no runbook and no tooling. The user has decided the bus ALWAYS
restarts (DECISIONS.md, 2026-08-02: *"always be able to restart, prefer to discard messages and/or
corruption, with logging"*). The decision text says so explicitly: "This also removes the
permanent-refuse-to-start DoS, and with it the need for the operator escape hatch that was previously
recommended: always-restart *is* the escape hatch."

The premise is gone, so the task is superseded rather than done -- nothing was built.

WHAT WAS IN IT THAT IS STILL WANTED, AND WHERE IT WENT -- so this is not a silent loss of three good
ideas:
 - (1) A read-only WAL dumper (offset / index / record-type / length / MAC-ok per frame). Still
   useful, but as an ORDINARY diagnostic, not an emergency tool. It belongs in the merged CLI epic as
   a subcommand (see CLI-6 'log' and CLI-8 'doctor'), NOT as a scripts/bus-*.sh wrapper -- invariant 7
   was amended on 2026-08-02 and the Go CLI replaces the shell wrappers.
 - (2) Counters for tail-repaired / repair-refused / commit-records-discarded-by-repair. This is now
   MORE important, not less: under always-restart the discard is the normal path, and the whole point
   of the decision is that every discard must be OBSERVABLE. It is folded into DUR-11's added scope
   (discard + SPECIFIC log + continue) and into CORE-5 (metrics/inspect endpoint).
 - (3) "A bus that repairs its tail on EVERY boot is the signature of failing media." Still true and
   still worth alarming on; it rides on (2)'s counters and belongs with CORE-5.

DO NOT REVIVE THIS TASK. If the dumper is wanted, file it against the CLI epic.

--- ORIGINAL DESCRIPTION ---
"Refuse to start" is now the designed answer to several WAL damage classes (see internal/wal/recover.go, DUR-4/DUR-10/DUR-11) and there is no runbook or tooling to diagnose it. Needs: (1) a read-only WAL dumper (offset / index / record-type / length / CRC-ok, one line per frame); (2) metrics/log counters for tail-repaired, repair-refused, and commit-records-discarded-by-repair; (3) an alarm-worthy signature: a bus that repairs its tail on EVERY boot is the signature of failing media. Ship as a scripts/bus-*.sh wrapper per invariant 7. Depends on DUR-9.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (in_progress)
- [CLI-8](../../CLI/CLI-8--ae4caacc/task.md) — CLI-8: doctor -- diagnose a broken setup with a specific remedy per failure (todo)
- [CORE-5](../../CORE/CORE-5--06c5b1f5/task.md) — CORE-5: Observability: metrics/inspect endpoint (follow-up) (superseded)
- [DUR-10](../DUR-10--bab09b2e/task.md) — DUR-10: Review the RepairTail truncation veto -- half is already in \`main\` UNREVIEWED (la… (done)
- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [DUR-9](../DUR-9--8234db61/task.md) — DUR-9: Wire the WAL into server startup (open, replay, hold for process lifetime, expose… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (in_progress)
- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
