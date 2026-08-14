# proof-check.sh: subtest SKIP/PASS lines invisible to plain-text counter -- parent-PASS/all-children-SKIP certifies PASS instead of VACUOUS

| Field | Value |
| --- | --- |
| Public id | `cea09b96-72db-40f1-84b4-c2e227eae1cf` |
| Key | _(null in the export)_ |
| Epic | [TOOLING](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T14:38:07.492809+00:00 |
| Updated | 2026-08-14T14:29:53.868362+00:00 |
| Completed | 2026-08-14T14:29:40.467697+00:00 |

## Proof command

```sh
bash scripts/proof-check_test.sh
```

## Status note

DONE 2026-08-14 at 3d9955afa8ebdcf0d7cc1a2fc09deabc1feada78. Fresh original-shape proof is correctly VACUOUS (checker rc=4; tests_run=4, top_level=1, skipped=3); stable scripts/proof-check_test.sh proof PASS. All mandated gates completed. Deliberately adversarial TestMain/stdout authentication remains separate nonblocking P2 fe0d9030-f95f-49b9-ab3b-68c96860df8a.

## Description

proof-check.sh is the tool CLAUDE.md mandates specifically to stop a proof that runs nothing from being certified as passing. It has a blind spot of exactly that shape.

In the plain-text (non -json) code path (scripts/proof-check.sh, around the PASSED/FAILED/SKIPPED counters), the counters are computed with column-0-anchored patterns:
  PASSED=$(grep -cE '^--- PASS:' "$GOTEST_LOG")
  FAILED=$(grep -cE '^--- FAIL:' "$GOTEST_LOG")
  SKIPPED=$(grep -cE '^--- SKIP:' "$GOTEST_LOG")
Go indents subtest result lines with leading whitespace (e.g. '    --- SKIP: Test/Subtest'), so these patterns only ever match TOP-LEVEL result lines. A test whose parent PASSes while every one of its table-driven/t.Run children SKIPs is therefore counted as PASSED=1, SKIPPED=0, and sails through the existing 'PASSED==0 && SKIPPED>0 => VACUOUS' guard untouched, because that guard also only sees the parent.

Note there IS a second, JSON code path (go test -json Action:pass/skip with a Test field) that counts subtests correctly regardless of nesting, because JSON events aren't indentation-sensitive. The bug is specific to the plain-text (-v, not -json) branch, which is what every proof_cmd in this repo actually uses.

MEASURED LIVE (2026-08-08), RED before any fix:
  $ bash scripts/proof-check.sh "go test -run TestEnrolmentEpoch ./internal/hub"
  proof-check: verdict=PASS class=test exit=0 tests_run=4 top_level=1 skipped=0 failed=0
while the verbose output underneath shows:
  --- PASS: TestEnrolmentEpoch (0.18s)
      --- SKIP: TestEnrolmentEpoch/HistoryRefusesTrafficThatPredatesTheReader
      --- SKIP: TestEnrolmentEpoch/AParkedPollIsNotWokenByTrafficThatPredatesTheReader
      --- SKIP: TestEnrolmentEpoch/AReusedAgentIDAfterARestartInheritsNoTraffic
TestEnrolmentEpoch guards the P0 enrolment-epoch security fix from the 2026-08-02 audit and currently asserts nothing; a task closed on that exact proof_cmd would be certified PASS.

Distinct from the other tracked vacuous-proof families: (1) a -run pattern matching zero tests (task 84b76d5e, fixed), (2) negative-only greps satisfiable by deletion, (3) conjunction-masking where an unfiltered && clause carries the verdict (task a9a433dd, open), (4) a pre-satisfied clause in a compound proof. This is a FIFTH family: correctly-shaped single-clause proofs where the counting itself under-reports skips because of indentation, not shell composition.

SCOPE (definition of done -- the TOOL's report, not making TestEnrolmentEpoch itself pass, which is SIGN-3/gated work):
  (a) Count subtest PASS/FAIL/SKIP lines regardless of indentation in the plain-text path -- e.g. match on the trailing '--- (PASS|FAIL|SKIP):' token rather than anchoring '^' to a literal '-', or switch the shim to always request -json output (which does not have this defect) and drop the plain-text counting path entirely if that is simpler and does not regress the human-readable proof output CLAUDE.md relies on.
  (b) Specifically classify the parent-PASS/all-children-SKIP shape as VACUOUS: a test where every child subtest skipped but the parent itself reports PASS (because t.Run failures/skips don't fail the parent unless the parent itself calls Fail/Skip) must not read as an unqualified pass.
  (c) Add a regression case to whatever test suite covers proof-check.sh itself (or a scripted fixture under scripts/ if none exists) asserting this exact TestEnrolmentEpoch-shaped log is classified VACUOUS.
  (d) Note the finding in CLAUDE.md's 'Verify' section alongside the other vacuous-proof families, and in DECISIONS.md if the fix changes counting semantics materially.

RELATED / DO NOT DUPLICATE: SIGN-3 (f2daa6bc-53ee-4788-935c-ab73693c5e75) is the reason TestEnrolmentEpoch and a large cascade of tests are currently skipped -- TestBroadcastSend, TestMessageHistoryCurser, TestWaiterWakeup, TestPollConcurrency, TestLongPollWait, TestAppliedKeyStoreSurvivesRestart, TestMessagingCrashRecovery and more (42 SKIP results were observed in a full verbose run), all gated behind SIGN-3 landing. This task is NOT about closing SIGN-3 or making those tests pass -- it is about proof-check.sh correctly reporting the vacuity while they remain skipped.

proof_cmd confirmed RED on 2026-08-08 (before any fix):
  bash scripts/proof-check.sh "go test -run TestEnrolmentEpoch ./internal/hub" | grep -q 'verdict=VACUOUS'
  -> exit 1 (grep found no match; live verdict was 'verdict=PASS ... skipped=0', confirming the tool does NOT today classify this as vacuous).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [HANDOVER-CHECK](../../HANDOVER/HANDOVER-CHECK--0f909b6c/task.md)
- **relates to** [71cdaef8-c757-4ba9-a693-a8f744070d08](../proof-check.sh-runs-the-proof-against-its-OWN-script-dir--71cdaef8/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [84b76d5e-fe02-4651-9828-caba3d82606b](../Proof-command-guard-a-run-pattern-that-matches-no-test-m--84b76d5e/task.md) — Proof-command guard: a \`-run\` pattern that matches no test must FAIL, not pass vacuously (done)
- [SIGN-3](../../SIGN/SIGN-3--f2daa6bc/task.md) — SIGN-3: Broadcast signature covers the recipient set (prevents split-content broadcasts) (todo)
- [a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd](../Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) — Conjunction-masking vacuous-proof family: filtered-clause proof_cmds hidden by an unfilte… (todo)
- [fe0d9030-f95f-49b9-ab3b-68c96860df8a](../proof-check.sh-cannot-authenticate-go-test-evidence-agai--fe0d9030/task.md) — proof-check.sh cannot authenticate go test evidence against adversarial TestMain output (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [71cdaef8-c757-4ba9-a693-a8f744070d08](../proof-check.sh-runs-the-proof-against-its-OWN-script-dir--71cdaef8/task.md) — proof-check.sh runs the proof against its OWN script directory repo root, not the callers… (in_progress)
- [932fe938-0e42-42d8-802d-ff018cb6c955](../../PROCESS/Audit-stored-proof_cmds-for-the-subtest-skip-vacuous-sha--932fe938/task.md) — Audit stored proof_cmds for the subtest-skip vacuous shape (parent-PASS/hidden-child-SKIP… (todo)
- [HANDOVER-CHECK](../../HANDOVER/HANDOVER-CHECK--0f909b6c/task.md) — HANDOVER-CHECK: one command that tells you the health of this repo, plus its recorded out… (todo)
- [fe0d9030-f95f-49b9-ab3b-68c96860df8a](../proof-check.sh-cannot-authenticate-go-test-evidence-agai--fe0d9030/task.md) — proof-check.sh cannot authenticate go test evidence against adversarial TestMain output (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
