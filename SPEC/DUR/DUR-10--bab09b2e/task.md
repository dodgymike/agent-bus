# DUR-10: Review the RepairTail truncation veto -- half is already in \`main\` UNREVIEWED (landed inside DUR-8's commit d06c704) and the rest has been rewritten since

| Field | Value |
| --- | --- |
| Public id | `bab09b2e-5e1b-4e2b-8ac8-0a3b06aca8b7` |
| Key | DUR-10 |
| Epic | [DUR](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T14:22:51.786582+00:00 |
| Updated | 2026-09-05T11:48:49.152073+00:00 |
| Completed | 2026-08-02T17:53:02.488646+00:00 |

## Proof command

```sh
go test -race -run 'TestCrashInjection|TestWALRepairTail' ./internal/wal
```

## Status note

Step 3 completed. Created a dedicated git worktree for the agent-bus repo at /mnt/sdb4/mike/mike/source/agent-bus/.worktrees/agent-bus-spec-keeper-worktree on branch spec-keeper-phased-20260905-114702 (HEAD 6aaed56ab8cceb8c5596ddb10b5f331d176b914d). Recorded the base branch (main, ahead 4, dirty) and verified the new worktree is clean.

## Description

VERDICT 2026-08-02 (spec-keeper): THE REVIEW DEBT IS PAID -- this task is (b) satisfied by the gates that actually ran, and it is being completed on that basis. It is NOT (a) an outstanding gate and NOT (c) obsolete: the gates ran, produced findings, and the findings were landed.

EVIDENCE, verified first-hand this pass:
- `git log --oneline -- internal/wal/recover.go` -> c362152, dad04aa, d06c704, 6f22a99. d06c704 is the never-gated half; dad04aa is the rewrite; c362152 ("WAL: correct comments that claimed a proof where the code has a heuristic (DUR-10)") is the comment-only landing of the reviewer's P0.
- REVIEWER GATE RAN (kind=response, 2026-08-02 15:06): CHANGES-REQUIRED, COMMENT-ONLY -- "the code is approved, it is strictly safer than what d06c704 left in main, and every finding is a comment or a scope/test-coverage nit, not a code defect". It re-probed DUR-11 finding (a) over 35 cases with zero silent losses, and mutation-tested rather than argued.
- SECURITY GATE RAN (kind=response, 2026-08-02 15:23): PASS-WITH-CONCERNS, byte-verified against dad04aa, ~345k probe cases. Its NEW MEDIUM finding (CRC32C is GF(2)-linear, so lengthOnlyDamage's completeness "proof" is forgeable by an ordinary remote client) is NOT dropped: it is the direct motivation for the 2026-08-02 decision to replace CRC32C with an HMAC-SHA256 keyed MAC, and it is carried by the MAC task, not by this one.
- IMPLEMENTER landed the reviewer's P0/P1 comment corrections at c362152 with ZERO executable lines changed (`git diff -U0` had no non-comment +/- lines).
- Every agent that touched this task posted kind=report + kind=model; reviewer and security also posted kind=response.

WHAT MOVED OUT OF SCOPE, and where it went. The description below still describes "recovery REFUSES TO START rather than cutting" as the designed failure mode. THAT POLICY IS NOW REVERSED by the user decision of 2026-08-02 ("Availability over retention: the bus ALWAYS restarts" -- DECISIONS.md). Converting every damage-class refusal into discard + specific log + continue is DUR-11's scope (884d3da4), in flight. Replacing the CRC32C checksum with a keyed MAC is the MAC task's scope. Neither is a reason to keep this review-debt task open: the debt was "this code reached main without a reviewer or a security gate", and that is now false.

--- ORIGINAL DESCRIPTION FOLLOWS (retained verbatim; read the reversal above before acting on any "refuse to start" language in it) ---

REVIEW CODE THAT IS ALREADY PARTLY IN `main` AND IS STILL MOVING. This task's premise has been
CORRECTED TWICE -- read this paragraph before anything else. It was originally filed (by spec-keeper on behalf of
backlog-triage, pass 4b) as "review-and-land an uncommitted fix". That framing is now WRONG on both halves: the code
is no longer uncommitted, and what is in the tree is no longer the code that was described. See the kind=response
note of 2026-08-02 for the correction and who is responsible for the original error.

WHAT IS ACTUALLY TRUE NOW (each fact verified first-hand by spec-keeper, commands quoted).

(1) HALF OF IT IS ALREADY IN `main`, COMMITTED WITHOUT ANY REVIEW, UNDER ANOTHER TASK'S TITLE.
`git show --stat d06c704` -- a commit titled "Exclusive lock on the bus data directory (DUR-8)" -- includes
internal/wal/recover.go (+152), internal/wal/format.go (+22), internal/wal/doc.go (+8),
internal/wal/crash_injection_test.go (+616) and internal/wal/recover_test.go (+102). None of that is DUR-8's work.
`git log --oneline -- internal/wal/recover.go` returns exactly two commits: 6f22a99 (DUR-4) and d06c704. So the
truncation change rode into main under an unrelated task's title. DUR-8's own agents said so: DUR-8's reviewer note
records verbatim "Deliberately ignored the in-flight internal/wal/** ... belonging to parallel agents", and DUR-8's
security audit lists only internal/dirlock files in its scope. THE PRODUCTION WAL CHANGE IN `main` HAS THEREFORE HAD
NO REVIEWER GATE AND NO SECURITY GATE. That -- not "landing a patch" -- is the debt this task exists to pay.

(2) IT HAS SINCE BEEN SUBSTANTIALLY REWRITTEN AGAIN, AND THAT REWRITE IS UNCOMMITTED.
`git diff --cached --stat internal/wal/` shows a further recover.go +311/-, recover_test.go +141, doc.go +13
(staged; `git diff` unstaged for internal/wal is empty, so the working tree == the staged version).
The function the original description told a reviewer to look at, `laterRecordInTail`, NO LONGER EXISTS. It has been
refactored into `inspectTail` (internal/wal/recover.go:347), with `tailHasRecordsAfterIt` now at :461 and
`truncatableTail` at :244; RepairTail is at :118 and calls inspectTail as the second gate at ~:150.
A REVIEWER MUST REVIEW THE CURRENT WORKING TREE, NOT MERELY THE DIFF AT d06c704. Reviewing d06c704 alone would
review code that has already been replaced.

THE BUG BEING FIXED (unchanged, P0 -- silent loss of acknowledged records on the append-only log).
truncatableTail decides from the damaged frame's OWN header. A single flipped bit in a NON-FINAL frame's length
field that overshoots EOF but stays <= MaxPayloadSize is byte-for-byte the same shape as a genuine torn tail, so
recovery started SUCCESSFULLY and silently deleted checksum-valid COMMIT records. Reproduced against the pre-fix
sources at 10 single-bit offsets [17 121 160 236 275 276 408 409 447 448]; at offset 17 recovery served 0 of 4
acknowledged entries (RepairTail Truncated:true At:16 Removed:573). Violates invariant 4 (nothing acknowledged is
ever lost) and invariant 6 (truncation only of a verified-corrupt tail).

THE SHAPE OF THE FIX AS IT NOW STANDS (describes `inspectTail`, the CURRENT code, not the superseded
laterRecordInTail). RepairTail applies inspectTail as a SECOND gate, only AFTER truncatableTail has already said
"tail-shaped". inspectTail reads the region [at, size) that the cut would discard and applies two proofs:
(a) lengthOnlyDamage -- recompute the damaged frame's checksum on the hypothesis that its true payload is the bytes
actually present; if it verifies, the record is COMPLETE and only its length field is corrupt, so it may have been
fsynced and acknowledged; (b) a forward search for any complete, checksum-verifying record inside the discard region
whose INDEX continues the file's sequence. Anchoring (b) on record index rather than on end-of-file is a DELIBERATE
change from the earlier version and the code comments say so. A candidate cap (maxTailCandidates=4096) bounds the
checksum work because the region is attacker-influenced. Any refusal is logged and returned as a fatal error:
recovery REFUSES TO START rather than cutting.

WHAT THIS TASK REQUIRES (unchanged in substance; the target has moved).
(1) REVIEWER GATE on the CURRENT internal/wal/recover.go working tree -- is the veto still strictly additive versus
6f22a99, is refusing-to-start the right failure mode versus truncating, and does the rewrite from laterRecordInTail
to inspectTail preserve the strict-subset property that justified landing it at all? The "purely additive, zero
removed lines in truncatableTail" argument was checked against the FIRST version and MUST BE RE-CHECKED against the
+311/- rewrite; do not carry it forward on trust.
(2) SECURITY GATE -- a remote-influenced WAL byte must not be able to turn recovery into either data loss OR a
permanent startup denial of service. The maxTailCandidates cap and the attacker-influenced-region reasoning in
inspectTail's comments are squarely in scope.
(3) ONE clean logical commit of the remaining uncommitted recover.go/recover_test.go/doc.go changes, with a message
that says plainly that the earlier half landed under DUR-8's title.
(4) CONTRACTS.md / PROTOCOL.md touch-up only if the described recovery contract moved.

CROSS-REFS. DUR-4 (done at 6f22a99) is the task this file was last completed against, and its reviewer verdict there
was CHANGES-REQUIRED, still unresolved. DUR-6 (done at e63ced5) owns the TESTS that ride with this code and
explicitly does NOT cover this production change. DUR-11 (884d3da4-bceb-4ac2-93a2-e147c77f9dca) carries two HIGH
findings this fix may or may not still leave open -- they were written against laterRecordInTail and must be
re-probed against inspectTail. Do not let DUR-10's reviewer re-litigate DUR-11's scope.

PROOF RE-VERIFIED against the CURRENT working tree by spec-keeper on 2026-08-02 after the rewrite:
scripts/proof-check.sh --quiet "go test -race -run 'TestCrashInjection|TestWALRepairTail' ./internal/wal" ->
verdict=PASS class=test exit=0 tests_run=42 top_level=14 skipped=1 failed=0 empty_pkgs=0. Not vacuous. The permanent
regression net is TestCrashInjectionSingleBitCorruptionSweep in internal/wal/crash_injection_test.go.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [DUR-6](../DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [DUR-8](../DUR-8--6f099429/task.md) — DUR-8: Exclusive lock on the bus data directory (stop two servers destroying one WAL) (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [DUR-12](../DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (in_progress)
- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [DUR-4-FU-TOOLING](../DUR-4-FU-TOOLING--26c2ce16/task.md) — DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start (superseded)
- [DUR-9](../DUR-9--8234db61/task.md) — DUR-9: Wire the WAL into server startup (open, replay, hold for process lifetime, expose… (done)
- [ID-3](../../ID/ID-3--745cce13/task.md) — ID-3: Agent id minting \`&lt;bus-id&gt;.&lt;name&gt;-&lt;n&gt;\` (done)
- [c3a27591-5b0c-44c0-ac68-94072f3c3fc2](../RESOLVED-2026-08-02-SUPERSEDED-CRC32C-tail-repair-proofs--c3a27591/task.md) — \[RESOLVED 2026-08-02 -- SUPERSEDED\] CRC32C tail-repair proofs are remotely forgeable =&gt; p… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
