# ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed history

| Field | Value |
| --- | --- |
| Public id | `838677e6-d424-45ed-8580-924cb2da28a6` |
| Key | _(null in the export)_ |
| Epic | [ID](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | id |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T14:02:19.994673+00:00 |
| Updated | 2026-09-05T12:03:59.944340+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestRunRefusesAnUnprovableSequenceFloor ./cmd/agent-bus
```

## Status note

Step 1 completed. Located the agent-bus source repo at /mnt/sdb4/mike/mike/source/agent-bus (fallback /home/mike/source/agent-bus also exists). The .worktrees/agent-bus-spec-keeper-worktree exists, is registered, on branch spec-keeper-phased-20260905-114702 at HEAD 6aaed56, and is clean — so it was adopted for reuse.

## Description

RE-SCOPED 2026-08-02 (spec-keeper) AFTER THE DEEP-DIVE. This task is now T4 of the deep-dive's own breakdown -- 'derive, prove and SEAL the sequence floor in main' -- and it is BLOCKED, not in progress.

WHY. The deep-diver (dispatched as DESIGN INVESTIGATION ONLY) produced ID2_WIRING_DEEPDIVE.md, committed at 2f89fc1, and its verdict is: THE PREMISE IS CONFIRMED BUT THE TASK AS ORIGINALLY FILED CANNOT BE IMPLEMENTED YET, AND IS NOT EXPLOITABLE TODAY. The sequence number lives in the caller-written PREPARE body, no message-body schema exists, and nothing in production mints a sequence at all -- so there is no code path to harden. Implementing as specced would either invent the MSG-epic body schema or change the prepare payload format, and the backlog settles neither. It becomes a genuine P0 the instant the first MSG write path lands.

VERIFIED FIRST-HAND BY SPEC-KEEPER before re-scoping: `git log --oneline -- ID2_WIRING_DEEPDIVE.md` -> 2f89fc1 ("ID2_WIRING_DEEPDIVE.md: the task as filed cannot be implemented yet"), and `git status --porcelain` is EMPTY -- so the INVESTIGATION is committed and NO production code was written, exactly as dispatched. The task's own code deliverable is therefore NOT delivered, which is why this is blocked rather than done.

IT ALSO HAD NO proof_cmd AT ALL, which under the 2026-08-02 process decision ("a missing proof_cmd blocks completion, at least as hard as a vacuous one") made it uncompletable by definition. One is now recorded -- see PROOF below.

THE WORK WAS SPLIT. Three sibling tasks now carry the separable parts; this task is the last of the four and depends on the other three:
  ID-2-WIRING-SEAL     P0 -- Sequence refuses to issue from an unsealed floor. internal/ids only. NO dependencies; startable NOW.
  ID-2-WIRING-SCHEMA   P0 -- DECIDE and record where the sequence high-water mark lives on disk. Docs only. THIS IS THE BLOCKER.
  ID-2-WIRING-OBSERVER P0 -- wal offers every prepare (incl. dangling) to an observer in the existing replay pass. Depends on SCHEMA choosing Option A'.

REMAINING SCOPE OF THIS TASK (T4). In cmd/agent-bus/main.go, after wal.Open, fold the observer over EVERY prepare, construct ids.Resume(floor), RaiseFloor from any other source, then Seal() -- and return a NON-NIL ERROR from run() on ANY failure: the scan errored, a message prepare's body had no seq or a zero seq, RaiseFloor returned non-nil, or Seal() returned non-nil. Log the derived floor at INFO beside the existing "write-ahead log opened" line.

THE LANDMINE THIS TASK MUST COVER: a scan that FAILED must not be indistinguishable from an EMPTY log. Floor 0 from a failed derivation must refuse to start, not resume as a fresh bus. Note this is a NON-DAMAGE error (a derivation we cannot prove), so it stays FATAL and is NOT touched by the 2026-08-02 always-restart decision, which sanctions discarding DAMAGED RECORDS -- not guessing at an id floor. Reissuing a burned id is silent corruption of the audit trail, not a discarded message.

--- ORIGINAL DESCRIPTION (still accurate as the statement of the hazard) ---
ids.Resume(highestOnDisk) requires the highest sequence EVER WRITTEN TO DISK -- committed, aborted AND dangling. The obvious wiring produces exactly the value that is forbidden: wal.Replay(path, fn) hands fn COMMITTED entries only, and wal.Recovered exposes no message-sequence high-water mark at all (Recovered.NextIndex is the WAL RECORD index, a different counter that also advances for commits and aborts). Concrete break: allocate seq 100, write the PREPARE, fsync it, crash before the COMMIT. 100 is burned and an audit record for it may exist, but replay never surfaces it, the floor comes back 99, and the next send is minted as <bus-id>-100 -- two different messages sharing one id in the append-only audit trail, and any dedup keyed on message id conflates them (invariants 1 and 10). An attacker able to induce crashes in the prepare->commit window chooses what lands on the reissued id.

Cross-reference: ID-2 (a3a5edc4-0a34-4691-b1a6-c1206218ac65, completed CODE-ONLY). internal/ids/sequence.go's doc comment already spells all of this out.

PROOF. `go test -race -run TestRunRefusesAnUnprovableSequenceFloor ./cmd/agent-bus` -- VACUOUS TODAY BY CONSTRUCTION (the test does not exist; it is this task's to write, modelled on cmd/agent-bus/wal_startup_test.go). MUST NOT BE COMPLETED ON A VACUOUS VERDICT: scripts/proof-check.sh must report PASS with tests_run > 0.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [ID-2-WIRING-OBSERVER](../ID-2-WIRING-OBSERVER--c31f6999/task.md)
- **blocked by** [ID-2-WIRING-SCHEMA](../ID-2-WIRING-SCHEMA--80b54ee4/task.md)
- **blocked by** [ID-2-WIRING-SEAL](../ID-2-WIRING-SEAL--8c9b6489/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ID-2](../ID-2--a3a5edc4/task.md) — ID-2: Monotonic sequence allocator (drives message ids) (done)
- [ID-2-WIRING-OBSERVER](../ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [ID-2-WIRING-SCHEMA](../ID-2-WIRING-SCHEMA--80b54ee4/task.md) — ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… (done)
- [ID-2-WIRING-SEAL](../ID-2-WIRING-SEAL--8c9b6489/task.md) — ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half impleme… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ID-2-WIRING-OBSERVER](../ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [ID-2-WIRING-SCHEMA](../ID-2-WIRING-SCHEMA--80b54ee4/task.md) — ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… (done)
- [ID-2-WIRING-SEAL](../ID-2-WIRING-SEAL--8c9b6489/task.md) — ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half impleme… (done)
- [ID-2-WIRING-SEAL-FU-CONTRACTS](../ID-2-WIRING-SEAL-FU-CONTRACTS--9c183c8e/task.md) — ID-2-WIRING-SEAL-FU-CONTRACTS: land the Sequence seal contract rows that the file-boundar… (todo)
- [db350e39-3dde-4166-b241-b21fa4635359](../../DUR/Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) — Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable ind… (done)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
