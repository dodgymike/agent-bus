# ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed history

| Field | Value |
| --- | --- |
| Public id | `838677e6-d424-45ed-8580-924cb2da28a6` |
| Key | _(null in the export)_ |
| Epic | [ID](../epic.md) |
| Status | superseded |
| Priority | P0 |
| Component | id |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T14:02:19.994673+00:00 |
| Updated | 2026-08-07T20:39:13.002014+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestRunRefusesAnUnprovableSequenceFloor ./cmd/agent-bus
```

## Status note

CORRECTED 2026-08-07 (spec-keeper). Disposition UNCHANGED (still SUPERSEDED, not reopened) but the REASON recorded a few hours earlier in this same field was WRONG and is replaced here, because a later reader would otherwise inherit a disproved argument.

WHAT WAS WRONG. The 20:12 note closing this task argued the hazard was closed transitively: "internal/hub derives its sequence floor from wal.Recovered.NextIndex-1 ... the WAL record index ... already bounds the sequence transitively (each message burns 1 sequence + 2 indices, so an index floor bounds the sequence floor)." That is the counting argument, and it is FALSE the moment a sequence can be handed out before any WAL record carries it.

THE DISPROOF, MEASURED (2026-08-07, after this task was closed). SIGN-2/SIGN-6 shipped `POST /v1/mint`: a client now receives a message id+sequence BEFORE any record is written, and the mint burns a batch of `hub.MintBatchSize = 256` sequences per durable claim. Sweeping every truncation offset of a WAL and reading the resumed sequence via a REAL mint call (internal/hub/seqhighwater_test.go, run first-hand: `go test -race -run TestSequenceHighWaterSurvivesDeepDamage ./internal/hub` -> PASS, 3 subtests):
  - with the durable `<data-dir>/message-seq-floor` file PRESENT -> 0 of 248 offsets reissued a sequence.
  - with it REMOVED, relying on wal-index-floor alone (exactly the mechanism the 20:12 note credited) -> 247 of 248 reissued.
Reproduced independently by the security gate per the brief that prompted this correction. The codebase already said so in capitals before this task was ever closed, at internal/hub/hub.go:477-491 ("That argument is RETIRED... wal's own durable index floor no longer covers this one transitively either") and internal/hub/seqfloorfile.go:93-98 ("Before the batch existed, every sequence was <= the WAL index... THAT IS THE ARGUMENT THE BATCH BROKE"). The 20:12 closing note did not check this before writing the transitive-bound claim.

THE ACTUAL, CORRECT REASON THIS TASK IS SUPERSEDED. Not wal-index-floor. It is internal/hub/seqfloorfile.go -- a DEDICATED, atomically-replaced, fsynced file (`<data-dir>/message-seq-floor`, on-disk format version 5, reserved through the Spec Server `ondisk-format-version` namespace) that records the message-sequence high-water mark OUTSIDE the log, written AHEAD of any sequence it authorises, exactly the shape ids.DurableNameSuffixes already uses for agent-id suffixes. It landed under commit aad611c (2026-08-07T16:23:26Z) and is genuinely wired into hub.Open (hub.go:513-517, :524-525, :620-633) -- not merely present in the tree. This is a DIFFERENT structural fix than the one this task specified: this task's own scope (T4 in ID2_WIRING_DEEPDIVE.md) was to derive the floor by folding an observer over every WAL prepare body (Option A'), decoding the sequence out of message content. The floor file makes that unnecessary for the authoritative path -- the sequence is durable BEFORE any message body exists at all, so there is nothing left in a message record for an observer to derive it from. Nothing in ID2_WIRING_DEEPDIVE.md's analysis survives as an open steady-state gap: its root cause (a dangling PREPARE burns a number that committed-only replay cannot see) is closed structurally, not by a better derivation.

RESIDUAL, UNCHANGED BY THIS CORRECTION. MSG-FU-SEQHIGHWATER (6ebe51be, still todo) tracks the one acknowledged open gap: the first-run migration window for a data directory written before message-seq-floor existed (hub.go:537-544 logs this loudly and is NOT silent, per invariant 6) -- and, as of this correction, that task's own proof test now exists in the working tree (internal/hub/seqhighwater_test.go, uncommitted) and is the same test used to measure the 247/0 result above. See that task's own notes for its current state; this correction does not complete it and does not change its disposition.

ID-2-WIRING-OBSERVER (c31f6999) -- separately corrected in its own notes/description by this same pass: still open, still P0, but re-scoped -- it is NOT needed for the primary derivation any more (nothing derives the message sequence from prepare bodies now), but IS still the only honest way to close the legacy-data-directory migration-window gap for both this task's residual and AUTH-3/MSG-FU-SUFFIXFLOOR's analogous suffix-floor residual. See its own record for the corrected scope.

Filed per an explicit request to correct reasoning that had been measurably disproved, not to re-litigate the disposition: SUPERSEDED stands, for the right reason now.

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


- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [ID-2](../ID-2--a3a5edc4/task.md) — ID-2: Monotonic sequence allocator (drives message ids) (done)
- [ID-2-WIRING-OBSERVER](../ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [ID-2-WIRING-SCHEMA](../ID-2-WIRING-SCHEMA--80b54ee4/task.md) — ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… (done)
- [ID-2-WIRING-SEAL](../ID-2-WIRING-SEAL--8c9b6489/task.md) — ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half impleme… (done)
- [MSG-FU-SEQHIGHWATER](../../DUR/MSG-FU-SEQHIGHWATER--6ebe51be/task.md) — MSG-FU-SEQHIGHWATER: persist the message-sequence high-water mark so deep log damage cann… (done)
- [MSG-FU-SUFFIXFLOOR](../MSG-FU-SUFFIXFLOOR--94159d93/task.md) — MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now… (in_progress)
- [SIGN-2](../../SIGN/SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)

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
