# Conjunction-masking vacuous-proof family: filtered-clause proof_cmds hidden by an unfiltered && clause report PASS on a zero-match filter

| Field | Value |
| --- | --- |
| Public id | `a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd` |
| Key | _(null in the export)_ |
| Epic | [TOOLING](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T22:15:51.969314+00:00 |
| Updated | 2026-08-08T10:30:02.659509+00:00 |
| Completed | — |

## Proof command

```sh
! bash scripts/proof-check.sh 'go test -run TestDefinitelyDoesNotExistAnywhere ./internal/wal && go test ./internal/wal' | grep -q 'verdict=PASS' && grep -q 'empty_pkgs' CLAUDE.md
```

## Description

THIRD vacuous-proof family, distinct from (1) a proof naming a test that does not exist, and (2) a negative-only grep satisfiable by deletion. This one is the most deceptive: proof_cmd of the shape go test -run '<filter>' ./pkg && go test ./pkg reports PASS with a LARGE tests_run even when the -run filter matches ZERO tests, because the second, UNFILTERED clause runs the whole package and its exit code carries the overall verdict. Unlike the first two families this fails LOUD-LOOKING: hundreds of genuinely passing tests mask a filter that matched nothing.

AUDIT (2026-08-02, main/orchestrator): scanned the full backlog for proof_cmd containing both -run and && where the SECOND clause is an unfiltered go test on the same/related package (i.e. genuinely of this shape, as opposed to the many proof_cmds that chain two DIFFERENT -run-filtered clauses, or a -run clause with a grep/CLI check -- those are fine). Exactly THREE tasks have this shape, all P0:
  - 8c9b6489-abb1-444e-9eeb-3ff87646f632 (ID-2-WIRING-SEAL) -- status done. proof_cmd: go test -race -run TestSequenceRefusesToIssueFromAnUnsealedFloor ./internal/ids && go test -race ./internal/ids
  - cbc9ab0c-3b34-48d0-acd8-5eabd4dc4a02 (DUR-12) -- status done. proof_cmd: go test -race -run 'TestWALFrameMACRejectsAlteredPayload|...' ./internal/wal && go test -race ./internal/wal
  - c31f6999-da4e-400d-ab55-178b82e2a42e (ID-2-WIRING-OBSERVER) -- status todo. proof_cmd: go test -race -run TestWALReplayObservesEveryPrepare ./internal/wal && go test -race ./internal/wal

The two DONE ones are NOT false passes: each was verified separately by the completing agent running and reporting the FILTERED clause alone (recorded tests_run=15 and tests_run=19 respectively in their completion test_summary/notes), so no already-completed work is in doubt. The risk is entirely FORWARD-LOOKING.

THE LIVE EXPOSURE is c31f6999 (ID-2-WIRING-OBSERVER), still todo. Ran its proof_cmd verbatim through proof-check.sh on 2026-08-02: the filtered clause (TestWALReplayObservesEveryPrepare, a test that does not exist yet) reports 'no tests to run', but the unfiltered second clause runs the whole internal/wal package and the tool reports verdict=PASS tests_run=245 top_level=74 -- i.e. today, BEFORE any fix, this task could be closed on a proof that never ran the test it claims to add. (There IS a warning line about an empty package in proof-check.sh output, but the overall verdict is still PASS, which is the masking defect.)

SCOPE for whoever takes this:
  (a) Fix c31f6999's proof_cmd so the filtered clause is verified ALONE (e.g. the DUR-3-style pattern: test $(go test -run X -v ./pkg 2>&1 | grep -c RUN) -gt 0 && go test -run X ./pkg -- both clauses filtered, so a zero-match filter fails the count check before the second clause can mask it).
  (b) Add this family to CLAUDE.md's 'Verify' section, alongside the existing two vacuous-proof warnings (test-that-does-not-exist; negative-only grep). NOTE: CLAUDE.md is a contended shared file -- coordinate the edit, prefer adding a new bullet rather than rewriting the section.
  (c) RECOMMENDED FIX (2026-08-02, main/orchestrator, reproduced directly -- see kind=report note for the full repro transcript): proof-check.sh ALREADY COMPUTES the right signal. Running c31f6999's live proof_cmd through it shows the filtered clause alone prints 'ok ... [no tests to run]', and proof-check.sh's own output includes both a human-readable warning ("READ THIS LINE before completing: if the test THIS task claims to add is in one of those packages, the proof did not exercise it") and a machine field `empty_pkgs=1` -- yet the final line still reads `verdict=PASS`. The defect is NOT detection (empty_pkgs is computed correctly); it is that this signal does not affect the verdict. CLAUDE.md tells every agent to quote the verdict specifically, so the one field the protocol trusts is exactly the field that lies, while the accurate signal sits one line away in a field nobody is told to read.
      THE FIX: make `empty_pkgs > 0` DOWNGRADE the verdict (to VACUOUS, or a new PARTIAL/UNVERIFIABLE value) instead of merely printing a warning -- a one-line conditional in a script that already computes the input, not a redesign. This automatically also closes vacuous-proof family (1) (a -run naming a nonexistent test), since that too yields empty_pkgs>0 -- one conditional covers two of the three families. Family (3) here (negative-only greps satisfiable by deletion) is a separate proof-authoring-convention problem and is NOT fixed by this.
      Refusing a proof_cmd containing && outright is now the FALLBACK option, not the leading one: conjunctions are a reasonable way to express "the named test passes AND the package stays green," and banning them outright would push authors toward worse proofs instead of fixing the real defect (a verdict that contradicts evidence the tool already gathered). Only consider the narrower "refuse a -run-filtered clause followed by an unfiltered same-package go test without an explicit non-empty-match check" rule if the empty_pkgs>0 downgrade proves insufficient in practice.

PROOF_CMD for use of this scoping is confirmed RED (verdict=FAIL) via: bash scripts/proof-check.sh "grep -qi 'conjunction' CLAUDE.md && grep -q 'refuse' scripts/proof-check.sh" -> verdict=FAIL class=wrapper,file-assertion exit=1 (neither CLAUDE.md nor proof-check.sh yet mention this family -- confirmed BEFORE the fix, as required).


---
FOURTH VACUOUS-PROOF FAMILY (2026-08-02, main/orchestrator): a PRE-SATISFIED CLAUSE. This task's OWN original proof_cmd -- `grep -qi 'conjunction' CLAUDE.md && grep -q 'refuse' scripts/proof-check.sh` -- was defective in exactly this new way: `grep -c 'refuse' scripts/proof-check.sh` returns 2 TODAY, before any fix, so that clause contributed zero verification (proof-check.sh already says 'refuse' twice, in unrelated prose about the decision NOT to refuse outright). The whole conjunction was held RED only by the other clause -- and it was pinned to the now-demoted 'refuse &&' fallback wording rather than the current leading fix (empty_pkgs>0 downgrade), so a correct implementation may never even add the word 'refuse'. A clause that is already true before the work starts makes a proof LOOK more rigorous (two checks!) while one of the checks verifies nothing. This is distinct from family (1) (a -run naming a nonexistent test), family (2) (a negative-only grep satisfiable by deletion), and family (3) (conjunction masking, this task's main subject): always verify EACH clause of a compound proof independently -- confirm it is actually RED on today's tree -- before combining them, and watch for clauses that can mask each other.

REPLACEMENT proof_cmd (2026-08-02, main/orchestrator), verified clause-by-clause before combining:
  new proof_cmd: ! bash scripts/proof-check.sh 'go test -run TestDefinitelyDoesNotExistAnywhere ./internal/wal && go test ./internal/wal' | grep -q 'verdict=PASS' && grep -q 'empty_pkgs' CLAUDE.md
  - clause 1 (behavioural, not lexical): runs family (3)'s exact defect shape (`go test -run <nonexistent> ./internal/wal && go test ./internal/wal`) through scripts/proof-check.sh and asserts its verdict is NOT PASS. Verified RED today in isolation: the inner command currently reports `verdict=PASS class=test exit=0 tests_run=245 top_level=74 skipped=2 failed=0 empty_pkgs=1` (the -run filter matches zero tests, empty_pkgs=1, yet the unfiltered second clause still carries the verdict to PASS), so the negated grep for 'verdict=PASS' currently fails (exit 1) -- correctly RED, because the tool has not yet been fixed to downgrade on empty_pkgs>0.
  - clause 2 (documentation): pinned to the specific string 'empty_pkgs' -- the field name the recommended fix must act on -- rather than the loose word 'conjunction' used before. Verified absent from CLAUDE.md today: `grep -c 'empty_pkgs' CLAUDE.md` returns 0.
  - both clauses verified independently RED before combining (per the trap this task itself documents: clauses that can mask each other). Combined command verified RED directly in bash (`bash -c "$CMD"`, exit 1) and, separately, run itself through `scripts/proof-check.sh "$CMD"`, which reports: `proof-check: verdict=FAIL class=wrapper,file-assertion exit=1 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0` -- confirmed RED, reproduced twice for stability.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [HANDOVER-BACKLOG-RECONCILE](../../HANDOVER/HANDOVER-BACKLOG-RECONCILE--43d14776/task.md)
- **relates to** [RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC](../../RELAY/RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC--7126f08b/task.md)
- **relates to** [RELAY-6](../../RELAY/RELAY-6--0f7275b9/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-12](../../DUR/DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (in_progress)
- [DUR-3](../../DUR/DUR-3--d8a991ea/task.md) — DUR-3: Replay/recovery on start (done)
- [ID-2-WIRING-OBSERVER](../../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [ID-2-WIRING-SEAL](../../ID/ID-2-WIRING-SEAL--8c9b6489/task.md) — ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half impleme… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [932fe938-0e42-42d8-802d-ff018cb6c955](../../PROCESS/Audit-stored-proof_cmds-for-the-subtest-skip-vacuous-sha--932fe938/task.md) — Audit stored proof_cmds for the subtest-skip vacuous shape (parent-PASS/hidden-child-SKIP… (todo)
- [HANDOVER-BACKLOG-RECONCILE](../../HANDOVER/HANDOVER-BACKLOG-RECONCILE--43d14776/task.md) — HANDOVER-BACKLOG-RECONCILE: the inherited backlog stops lying about what is in flight (todo)
- [cea09b96-72db-40f1-84b4-c2e227eae1cf](../proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) — proof-check.sh: subtest SKIP/PASS lines invisible to plain-text counter -- parent-PASS/al… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
