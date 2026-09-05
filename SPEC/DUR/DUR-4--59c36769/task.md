# DUR-4: Corrupt-tail detection & truncation

| Field | Value |
| --- | --- |
| Public id | `59c36769-d356-43e8-bcc0-9e1446a097c7` |
| Key | DUR-4 |
| Epic | [DUR](../epic.md) |
| Status | superseded |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:05:46.185927+00:00 |
| Updated | 2026-08-07T20:12:04.900853+00:00 |
| Completed | 2026-08-02T14:06:56.211375+00:00 |

## Proof command

```sh
go test -race -run TestWALRepairTail ./internal/wal
```

## Status note

CLOSED AS SUPERSEDED 2026-08-07 by spec-keeper. Blocking condition ("THIS TASK CLOSES WHEN DUR-11 CLOSES") is satisfied: DUR-11 (884d3da4, anchor tail veto on record INDEX + stop truncating a checksum-failing acknowledged record) is DONE, and DUR-12 (cbc9ab0c, CRC32C->HMAC-SHA256 keyed MAC, ondisk-format-version=2) is also DONE. This task carried no independent implementation of its own once those landed -- it was kept open only as the parent record over an unresolved reviewer/security gate, and both gates are now resolved (see this tasks own description/status_note history, and DECISIONS.md 2026-08-02 Five decisions). The founding sentence this task was filed on ("a corrupt record anywhere but the tail is a fatal startup error") is WRONG under the reversed always-restart policy (DECISIONS.md 2026-08-02 Availability over retention; CLAUDE.md invariant 6) and must not be built by anyone reading this task historically. Remaining, still-open work: DUR-4-FU-DOCS (0b6d5c11, PROTOCOL.md/CONTRACTS.md writeup of the narrowed invariants 4+6) and DUR-4-FU-DECISIONS (180f11f8, record the shipped damage-class taxonomy in DECISIONS.md) -- both todo, both docs-only, neither blocks anything else. DUR-4-FU-TOOLING was already superseded separately. No new follow-up filed; existing FUs cover the remainder.

## Description

POLICY REVERSED 2026-08-02 BY USER DECISION -- READ THIS BEFORE ACTING ON ANY OLDER TEXT HERE.

THE SENTENCE THIS TASK WAS FILED ON IS NOW WRONG. It said: "A corrupt record anywhere but the tail is
a fatal startup error, not a truncation." The user has decided the opposite (DECISIONS.md, 2026-08-02,
"Availability over retention: the bus ALWAYS restarts"): *"always be able to restart, prefer to
discard messages and/or corruption, with logging"*. Recovery must ALWAYS reach a running server.
Damaged records ANYWHERE may be discarded -- each with its own specific log entry. Invariant 6 is
narrowed: truncation is no longer restricted to a verified-corrupt TAIL. Invariant 4 is narrowed:
acknowledged data may be discarded when found corrupt (we still never lose it through our OWN write
path). The defect was never that data was discarded -- it is that the discard was SILENT. Every
discard must be OBSERVABLE.

ANYONE IMPLEMENTING FROM THIS TASK MUST NOT BUILD THE OLD POLICY. The line that still holds:
NON-DAMAGE errors -- permission denied, I/O failure, the data-directory lock already held -- stay
FATAL. Do not turn an unreadable disk into a silently empty bus.

WHERE THE REMAINING WORK LIVES. This task's own code shipped at 6f22a99 and has been rewritten twice
since (d06c704, dad04aa, c362152). It is kept open only because it was completed over an unresolved
reviewer CHANGES-REQUIRED and a security PASS-WITH-CONCERNS. Both of those are now resolved or
re-homed:
  - The reviewer P0 was landed as comment-only corrections at c362152 under DUR-10, which is now DONE
    (reviewer and security gates both ran; that was the whole point of DUR-10).
  - Security's two HIGH findings are DUR-11 (884d3da4), IN FLIGHT, re-scoped to the always-restart
    policy: finding (a) (index-anchored search -- one damaged record must never mass-delete later
    INTACT records) stands as a real bug; finding (b) is no longer an invariant-4 violation, and its
    residual is the SILENCE plus the false "provably never fsynced" doc comments.
  - Security's later MEDIUM (CRC32C is GF(2)-linear, so the completeness "proof" is forgeable by an
    ordinary remote client) is DUR-12, the CRC32C -> HMAC-SHA256 keyed MAC change, holding reserved
    ondisk-format-version=2 and BLOCKED on where the MAC key lives.
  - The "no operator override exists" escalation (c3a27591) is DISSOLVED: always-restart IS the
    escape hatch.

THIS TASK CLOSES WHEN DUR-11 CLOSES. It carries no independent implementation work any more; it is
the parent record. Do not dispatch an implementer here -- dispatch to DUR-11.

--- ORIGINAL DESCRIPTION, retained for the record. Its last sentence is REVERSED, see above. ---
During replay, a checksum mismatch or short read at the END of the WAL (the torn record a crash mid-write leaves behind) is detected, logged, and the file is truncated at the last verified-good record boundary -- the ONLY truncation ever permitted (invariant 6). A corrupt record anywhere but the tail is a fatal startup error, not a truncation.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-10](../DUR-10--bab09b2e/task.md) — DUR-10: Review the RepairTail truncation veto -- half is already in \`main\` UNREVIEWED (la… (done)
- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-12](../DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (in_progress)
- [DUR-4-FU-DECISIONS](../DUR-4-FU-DECISIONS--180f11f8/task.md) — DUR-4-FU-DECISIONS: record the SHIPPED damage-class taxonomy in DECISIONS.md -- which cla… (todo)
- [DUR-4-FU-DOCS](../DUR-4-FU-DOCS--0b6d5c11/task.md) — DUR-4-FU-DOCS: state invariants 4/6 as explicit NARROWINGS + document the WAL recovery AP… (todo)
- [DUR-4-FU-TOOLING](../DUR-4-FU-TOOLING--26c2ce16/task.md) — DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start (superseded)
- [c3a27591-5b0c-44c0-ac68-94072f3c3fc2](../RESOLVED-2026-08-02-SUPERSEDED-CRC32C-tail-repair-proofs--c3a27591/task.md) — \[RESOLVED 2026-08-02 -- SUPERSEDED\] CRC32C tail-repair proofs are remotely forgeable =&gt; p… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2a961fcc-426d-4c98-bc63-eb236367fd85](../Startup-scans-the-WAL-twice-soon-three-times-bound-the-c--2a961fcc/task.md) — Startup scans the WAL twice (soon three times) -- bound the cost (todo)
- [84b76d5e-fe02-4651-9828-caba3d82606b](../../TOOLING/Proof-command-guard-a-run-pattern-that-matches-no-test-m--84b76d5e/task.md) — Proof-command guard: a \`-run\` pattern that matches no test must FAIL, not pass vacuously (done)
- [DUR-10](../DUR-10--bab09b2e/task.md) — DUR-10: Review the RepairTail truncation veto -- half is already in \`main\` UNREVIEWED (la… (done)
- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-12](../DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (in_progress)
- [DUR-4-FU-TOOLING](../DUR-4-FU-TOOLING--26c2ce16/task.md) — DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start (superseded)
- [DUR-8](../DUR-8--6f099429/task.md) — DUR-8: Exclusive lock on the bus data directory (stop two servers destroying one WAL) (done)
- [DUR-9](../DUR-9--8234db61/task.md) — DUR-9: Wire the WAL into server startup (open, replay, hold for process lifetime, expose… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
