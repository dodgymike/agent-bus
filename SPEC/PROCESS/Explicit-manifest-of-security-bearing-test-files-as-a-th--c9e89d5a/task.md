# Explicit manifest of security-bearing test files, as a third guard check alongside the two carve-out patterns

| Field | Value |
| --- | --- |
| Public id | `c9e89d5a-6f6f-475e-8c8e-24f663a060bc` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | cancelled |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T09:20:54.746687+00:00 |
| Updated | 2026-08-22T09:52:01.517468+00:00 |
| Completed | — |

## Proof command

```sh
test -f docs/security-test-manifest.tsv && grep -qx "internal/httpapi/authmw_test.go" docs/security-test-manifest.tsv && grep -qx "client/guard_test.go" docs/security-test-manifest.tsv && grep -qx "cmd/agent-bus/tlslisten_test.go" docs/security-test-manifest.tsv && grep -qx "client/pinrotate_test.go" docs/security-test-manifest.tsv && grep -q "docs/security-test-manifest.tsv" CLAUDE.md && bash scripts/doc-check.sh section CLAUDE.md "## Agent roster (\`.claude/agents/\`)" "docs/security-test-manifest.tsv" && go test -run TestSecurityManifestEntriesExist ./... 2>&1 | grep -q "^ok"
```

## Status note

DUPLICATE of 212e695b-c11c-485b-aaa4-730d2f0ebd13 ("change-tier.sh: guard-file and verification-infrastructure signal", T-05), WHICH IS THE SINGLE OWNER OF THE SECURITY-TEST MANIFEST. Filed ~40 minutes after T-05 by a concurrent run that did not see it. T-05 already mandated "patterns UNIONED WITH the checked-in manifest" and already named all four files cited here, including internal/httpapi/authmw_test.go:549.

NOTHING IS LOST: this task's three genuinely new contributions were folded into T-05's description FIRST, and verified present on the server BEFORE this cancellation (T-05 v4 -> v5, description 9139 -> 15110 chars, section "MERGED IN FROM c9e89d5a"):
  1. the corrected SCALE - 22 of 235 tracked _test.go files covered by the two existing patterns, so the manifest is ~60 entries, not 4, with the ~15 uncovered files enumerated by invariant;
  2. OMISSION DRIFT recorded as UNSOLVED - nothing detects an entry SILENTLY ABSENT from the manifest, which is distinct from an entry DELETED or RENAMED (T-05 already catches that), and it must not be presented as covered by the existence check;
  3. the SAME-BASENAME PAIR internal/auth/crosscheck_mtlscrosscheck_test.go vs internal/httpapi/crosscheck_mtlscrosscheck_test.go - one covered, one not - which breaks any keying by basename and forces EXACT-PATH matching.

REASON FOR MERGING RATHER THAN RUNNING BOTH: one owner for the manifest. Otherwise the classifier and the TSV become two definitions of one guard set, free to disagree - and a guard set that disagrees with itself FAILS OPEN. Same failure shape as the two classifiers T-18 exists to collapse.

Linked to T-05 with a RELATES edge, deliberately NOT supersedes: a supersedes edge auto-flips the target's status, which is the behaviour filed as afc2fe3f. Work the manifest under T-05.

## Description

The security carve-out (CLAUDE.md "Agent roster", added for task 97a315af) identifies GUARD files two ways, both PATTERNS: by path (grep -Ei 'guard') and by content (go/ast|go/parser|InsecureSkipVerify|VerifyPeerCertificate inside a touched _test.go). Measured, both miss files that matter.

internal/httpapi/authmw_test.go contains NONE of the content patterns (grep -c returns 0) and has no "guard" in its path. It holds TestEveryRouteRequiresAuth at line 549 (the AUTH-6 deliverable), which pins invariant 3's unauthenticatedRoutes allow-list -- the enforcement for the security boundary CLAUDE.md describes as "trust the allow-list, never the prose". Under the carve-out as written, a change deleting or weakening that test is currently classified as a docs-and-tests-only change eligible to SKIP security review.

Task: add an explicit MANIFEST of security-bearing test files, matched by EXACT PATH, as a third check alongside the two pattern checks in the carve-out logic (wherever that logic is applied -- currently prose in CLAUDE.md's "Agent roster" section, read by the security/reviewer agents at gate time). A manifest closes mechanically what a pattern cannot: it fails safe -- a listed file is always treated as a guard regardless of its name or content, so a file that legitimately has neither "guard" in its path nor the content markers is still caught.

== CORRECTION 2026-08-22 (coordinator, via security re-gate) -- SCALE, not just the one seed file ==

The task was originally sized around one example file. A security re-gate measured the real number: of 235 tracked _test.go files, the two existing carve-out patterns (path grep -Ei 'guard', content grep -E 'go/ast|go/parser|InsecureSkipVerify|VerifyPeerCertificate') cover only 22 -- re-verified 2026-08-22 by spec-keeper: the content pattern alone already matches those same 22 (it is a superset of the 5 path-pattern matches at this snapshot), so union = 22. **The manifest must be sized for roughly 60 files, not one.**

The uncovered set that matters (verified present via `git ls-files --error-unmatch`, and verified 2026-08-22 that each has 0 content-pattern matches and no "guard" in its path, by spec-keeper):

- Invariant 11 (mTLS / pinning / no-TOFU):
  - internal/auth/crosscheck_mtlscrosscheck_test.go
  - internal/httpapi/clientcert_mtlsbind_test.go
  NOTE: internal/httpapi/crosscheck_mtlscrosscheck_test.go (a DIFFERENT file, same basename, different package) IS already covered by the content pattern -- confirmed by spec-keeper. This pair is a concrete illustration of why the manifest must match by EXACT PATH: two files sharing a basename land on opposite sides of the existing patterns.
- Invariant 3 (invite-gated enrolment / allow-list):
  - internal/httpapi/invitegate_enforce_test.go
  - internal/auth/invitegate_crash_test.go
  - internal/auth/invitegate_enforce_test.go
  - internal/auth/invitegate_service_test.go
  - internal/auth/invitegate_test.go
- Invariants 4/5/6 (durability / recovery / crash-injection):
  - internal/wal/crash_injection_test.go
  - internal/wal/replay_crash_test.go
  - internal/wal/indexfloor_crash_test.go
  - internal/wal/indexfloor_auth_test.go
- Invariant 10 (idempotency / duplicate detection):
  - internal/idem/crashinjection_test.go
- Invariant 1 (server-authoritative ids / sequence never rewinds):
  - cmd/agent-bus/seqfloorforge_test.go
  - cmd/agent-bus/seqfloormissing_test.go
  - cmd/agent-bus/seqfloorrestart_test.go

This list (~15 named above, on top of the original 4-file seed, against a target scale of ~60) is a STARTING POINT for the seed, not the complete set -- whoever implements this must do a full pass across all 235 tracked _test.go files against every invariant (INVARIANTS.md 1-11), not just extend this list by inspection.

TWO CONSEQUENCES OF THE CORRECTED SCALE:

1. A ~60-ENTRY HAND-MAINTAINED MANIFEST WILL DRIFT BY OMISSION, AND THAT DIRECTION IS UNSOLVED HERE. The "every entry exists" test asked for below is necessary but not sufficient: it catches an entry that was DELETED or RENAMED (the file named in the manifest no longer exists), but it does nothing for a NEW security-bearing test that nobody adds to the manifest -- that omission is invisible to an existence check, because the manifest that omits it still looks internally consistent. Do NOT pick a fix here; record that this is open, with the options on the table for whoever picks it up next:
   - a periodic audit that diffs the current crash-injection-test and AST-guard sets (or a broader per-invariant sweep) against the manifest and flags anything present in the sweep but absent from the manifest;
   - requiring any NEW _test.go file added under an invariant-bearing package (internal/auth, internal/wal, internal/idem, internal/httpapi, cmd/agent-bus, client, and similar) to be either added to the manifest or carry an explicit not-a-guard annotation, enforced at review time.
   A manifest that only guards against deletion looks complete while quietly rotting as new tests are added uncatalogued.

2. THE COST ESTIMATE CHANGES WITH THE SCALE. Seeding ~60 entries with a per-entry justification (which invariant, which check the entry enforces) is materially more implementation work than seeding 4 entries. If this changes the task's priority relative to the rest of the PROCESS epic, that should be decided and recorded explicitly by whoever picks it up (e.g. via spec-keeper), rather than silently absorbed into the existing P1/estimate.

MEASUREMENT-METHOD WARNING: the naive-walk file counts underlying this class of analysis move between measurements and are NOT reliable as a fixed number -- the count of "test files inflated by nested worktrees" has been reported as 45/159, then 148/61, then 114/45 across different measurement passes, purely because a naive walk counts files inside the 8 transient nested checkouts (7 under .claude/worktrees/, 1 under .worktrees/), and those checkouts churn. Any manifest-building or manifest-auditing work in this task MUST enumerate with `git ls-files` (never a filesystem walk) and MUST record the METHOD used (the exact command), not just a snapshot figure -- a count written in prose here can go stale the moment a worktree is added or removed, but "we used git ls-files" does not.

DEFINITION OF DONE:
1. The manifest lives in a tracked file (e.g. docs/security-test-manifest.tsv or similar) and is itself CONTROL PLANE per CLAUDE.md's existing CONTROL-PLANE definition ("anything that decides WHAT is checked or performs the check") -- so a change to the manifest itself requires security review. A manifest anyone can quietly shorten without review is not a control.
2. CLAUDE.md's carve-out text is updated to name the manifest file as the third check, and AGENTS.md is re-synced (PITFALLS.md section 5 -- the two have drifted before).
3. Seeded at the corrected scale (~60 entries, not 4) -- the four originally named plus the ~15 named above are a floor, not the full set; a full pass across all 235 tracked _test.go files against INVARIANTS.md 1-11 is required to reach the real total. Each entry should record which invariant/check it enforces.
4. A Go test asserting every manifest entry EXISTS on disk (via git ls-files, see point 5) -- a manifest naming a moved or renamed file silently protects nothing, which is this repo's recurring failure shape (stale references after a rename). This is necessary but NOT sufficient (see consequence 1 above) -- it does not catch omission of a new security-bearing test, and that gap must be recorded as open, not silently treated as solved by this test.
5. Enumeration/verification uses `git ls-files`, never a filesystem walk (`find`/`grep -r`): this repo carries 8 nested checkouts (7 under .claude/worktrees/, 1 under .worktrees/) that inflate a naive walk and whose counts move between measurements (see MEASUREMENT-METHOD WARNING above) -- record the exact `git ls-files` invocation used, not a bare number. A manifest-existence check built on `find`/`grep -r` will silently pass on stale files that only exist in an abandoned worktree copy, or miscount entirely.
6. proof_cmd must be shown RED before the fix (no manifest file exists yet, so a proof asserting its presence and the CLAUDE.md cross-reference fails today) -- do not accept a bare grep; pin the manifest's specific path and the specific carve-out section heading (`scripts/doc-check.sh section`).

ACCEPTED AS A KNOWN GAP for the 97a315af carve-out commit rather than a blocker on it: this gap is to be documented in PITFALLS.md section 8 alongside the carve-out's own incident record, the reviewer chain still runs unconditionally on every change regardless of the security-skip decision, and security judged this an ENABLING step (a gap in classification that could let a guard-weakening change dodge review) rather than a direct route to an ungated endpoint -- hence P1 rather than P0.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [212e695b-c11c-485b-aaa4-730d2f0ebd13](../change-tier.sh-guard-file-and-verification-infrastructur--212e695b/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [212e695b-c11c-485b-aaa4-730d2f0ebd13](../change-tier.sh-guard-file-and-verification-infrastructur--212e695b/task.md) — change-tier.sh: guard-file and verification-infrastructure signal (todo)
- [97a315af-70b3-4a64-8456-92335d8c9631](../Make-security-skip-the-default-for-docs-and-tests-only-c--97a315af/task.md) — Make security skip the default for docs-and-tests-only changes, with a guard-file carve-o… (done)
- [AUTH-6](../../AUTH/AUTH-6--1640e0b4/task.md) — AUTH-6: Auth FAIL-OPEN risk -- wrap the mux with auth + an explicit unauthenticated allow… (superseded)
- [afc2fe3f-848f-4804-b031-92e4ffbb015e](../Spec-Server-creating-a-supersedes-edge-silently-flips-th--afc2fe3f/task.md) — Spec Server: creating a supersedes edge silently flips the target task to status=supersed… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [212e695b-c11c-485b-aaa4-730d2f0ebd13](../change-tier.sh-guard-file-and-verification-infrastructur--212e695b/task.md) — change-tier.sh: guard-file and verification-infrastructure signal (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
