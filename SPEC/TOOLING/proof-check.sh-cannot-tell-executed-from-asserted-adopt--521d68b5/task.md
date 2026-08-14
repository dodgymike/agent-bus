# proof-check.sh cannot tell "executed" from "asserted" -- adopt a zero-probe guard convention

| Field | Value |
| --- | --- |
| Public id | `521d68b5-4181-4df6-b3c2-ef660ff5461d` |
| Key | _(null in the export)_ |
| Epic | [TOOLING](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T20:39:07.174241+00:00 |
| Updated | 2026-08-08T10:30:02.239156+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh "grep -A2 'zero-probe convention' CLAUDE.md | grep -q 'AFTER any filtering' && grep -q 'zero-probe convention' DECISIONS.md"
```

## Description

scripts/proof-check.sh classifies a proof PASS / FAIL / VACUOUS / UNVERIFIABLE, and it correctly catches the classic vacuity of `go test -run TestThatDoesNotExist ./pkg` (prints `ok ... [no tests to run]` and EXITS 0). But it only verifies tests EXECUTED -- not that they ASSERTED anything. Observed in this project (AGENT_LOG.md, 2026-08-02 AUTH-2 entry): TestEveryRouteRequiresAuth's headline loop passed with ZERO children -- every registered route was on the allow-list, so `continue` fired every iteration and the body never ran. The existing `len(routes)==0` guard did not catch it because the slice was non-empty; it was the FILTERED set that was empty. The test ran, exited 0, and proved nothing.

Proposed fix is a CONVENTION, not full mutation testing (out of proportion here): a test that loops over cases must count the probes it actually asserted (a `probed` counter) and `t.Fatalf` when that count is zero -- with the guard placed AFTER any filtering, since filtering is exactly what silently empties the set. Where zero is a legitimate expected outcome on the current build (as with TestEveryRouteRequiresAuth today, where every route IS on the allow-list), the convention must allow a documented exception (t.Logf with a named companion test that keeps the real assertion alive, per the existing pattern in internal/httpapi/authmw_internal_test.go's TestEveryRouteRequiresAuthOnASyntheticRoute) -- but that exception must be an explicit, reviewed choice, not silent.

Scope:
(a) Document the convention in CLAUDE.md's "Verify" section under a heading/phrase containing "zero-probe convention", including the AFTER-filtering placement rule and the documented-exception carve-out.
(b) Survey the repo for enumeration-shaped tests (loops with a filter/continue) and apply the convention -- audit internal/httpapi/authmw_test.go and internal/httpapi/authmw_internal_test.go (both already have partial `probed` counters -- confirm/align them with the finished convention) plus any other loop-shaped test found elsewhere in the tree.
(c) Decide, and record in DECISIONS.md under a section containing the phrase "zero-probe convention", whether proof-check.sh itself can detect the zero-probe case mechanically (e.g. via -v output inspection for `probed`/counter patterns) or whether the hand-written convention is the whole answer for now. Either way, write down the reasoning.

Cross-reference: CLAUDE.md's "Verify" section already warns that grep-based doc proofs are the MORE dangerous vacuous family, because a loose pattern can match an unrelated line -- not hypothetical: task c27f9439's proof passed over a still-broken CONTRACTS.md:51 by matching an unrelated line in README.md. A doc proof must pin the SPECIFIC line/phrase it claims to prove and must be CONFIRMED RED before the fix -- a proof never observed failing is not evidence it CAN fail.

proof_cmd was confirmed RED on 2026-08-02 (before this task's work) via:
  bash scripts/proof-check.sh "grep -A2 'zero-probe convention' CLAUDE.md | grep -q 'AFTER any filtering' && grep -q 'zero-probe convention' DECISIONS.md"
Verdict: FAIL (class=file-assertion, exit=1) -- neither CLAUDE.md nor DECISIONS.md yet contains the phrase, confirming the proof is not vacuous-by-accident (it can and does fail today).

Coordination note: CLAUDE.md and DECISIONS.md are shared files -- confirm no other agent is mid-edit on them before touching (per CLAUDE.md's parallel-agent-coordination rule: only one agent at a time, prefer adding a new dated section over editing existing lines). At time of filing (2026-08-02) a parallel loop had an agent editing CLAUDE.md/CONTRACTS.md/SPEC.md; do not clobber that work -- re-read the file immediately before editing.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [HANDOVER-BACKLOG-RECONCILE](../../HANDOVER/HANDOVER-BACKLOG-RECONCILE--43d14776/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1-FU-LISTENADDR](../../AUTH/AUTH-1-FU-LISTENADDR--c27f9439/task.md) — AUTH-1-FU-LISTENADDR: default listen address is :8080 (all interfaces) but DECISIONS.md s… (done)
- [AUTH-2](../../AUTH/AUTH-2--4b45a6d8/task.md) — AUTH-2: Token verification middleware (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [HANDOVER-BACKLOG-RECONCILE](../../HANDOVER/HANDOVER-BACKLOG-RECONCILE--43d14776/task.md) — HANDOVER-BACKLOG-RECONCILE: the inherited backlog stops lying about what is in flight (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
