# scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1

| Field | Value |
| --- | --- |
| Public id | `b2567ffd-190d-4aff-8cc2-f6a2eb2d613e` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T08:40:01.921843+00:00 |
| Updated | 2026-08-22T09:27:57.826564+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/change-tier_test.sh'
```

## Description

Create scripts/change-tier.sh plus scripts/change-tier_test.sh. This task lands the SKELETON and exactly one signal; the remaining signals are T-04..T-09 and must not be pre-implemented here.

Scope:
- Arg contract and diff basis per T-01 (`git diff HEAD -- <pathspec>` over the worktree + untracked files; refuse ambiguity loudly).
- Deterministic output: the computed floor tier, and the list of signals that fired with the paths that fired them. Machine-readable enough to paste into a kind=tier note.
- Documented exit codes.
- Signal 1 only: any non-test .go file changed.
- The change-detection must read `git diff --name-status`, including D (delete) and R (rename). A classifier that inspects only ADDED LINES cannot see a deleted guard file, and renaming a guard file to a non-guard name is the specific evasion this must not have.
- Test harness scripts/change-tier_test.sh, following the existing shape at scripts/proof-check_test.sh: flat numbered `# --- Case N:` blocks, `set -uo pipefail` (no -e; toggle `set +e` around commands whose exit code IS the assertion), a fail() that exits 1, mktemp -d fixtures with an immediate `trap ... EXIT`, fixtures created OUTSIDE the repo so they cannot consume the repo's own tests.
- Apply the hardening already established: single-quoted trap bodies (`trap 'rm -rf -- "${tmp:?}"' EXIT`), test mktemp's OUTPUT not just its exit status, `--` terminators on grep/sed.
- Documentation in the SAME task (project rule): add both script files to the two tables in CONTRACTS-AGENT.md (:22-28 agent-facing, :98-104 non-agent-facing) and fix the now-doubly-wrong "scripts/ holds exactly six files" claim at CONTRACTS-AGENT.md:19 -- the directory holds nine today and eleven after this task.
- Naming note: the repo has both proof-check_test.sh and proof-cmd-audit-test.sh. Pick change-tier_test.sh and say so; do not "fix" the other files.
- There is NO CI and NO Makefile in this repo. Tests are invoked as Spec Server proof_cmds. Do not add a CI file.

Each signal task in this epic must show its fixture RED before the signal is implemented, and the task's kind=report must quote the RED output. A proof never observed failing is not evidence (PITFALLS.md section 2).

BLOCKED BY T-01. BLOCKS T-04..T-09 and T-10.

---

## AMENDMENT B (2026-08-22, planner via orchestrator): the basis is `git status`, NOT `git diff`

**THIS REPLACES THE DIFF-BASIS WORDING ABOVE. The phrasing `git diff HEAD -- <pathspec>` (in the
first scope bullet, and in T-01's "diff-basis contract" bullet) is SUPERSEDED -- do not implement
it.** An implementer following the earlier line ships a classifier with a hole.

**CORRECTION (2026-08-22, coordinator via spec-keeper) -- THE RATIONALE PREVIOUSLY RECORDED HERE
WAS WRONG, and is named rather than deleted so nobody re-derives it.** This amendment originally
justified the `git status` basis by claiming that `git diff HEAD --name-only` omits a new untracked
`.go` file "**and a pathspec commit TAKES that file**". **That second clause is FALSE and is
withdrawn.** Measured 2026-08-22: `git commit -m x -- brandnew.go` against an untracked path errors
with `pathspec 'brandnew.go' did not match any file(s) known to git` and exits 1. A pathspec commit
does NOT take an untracked file.

**The CONCLUSION is UNCHANGED** -- the basis is `git status`, never `git diff`. Only the reason is
corrected. **Do not "re-correct" the conclusion after checking the old reason and finding it false;**
that is the specific mistake this paragraph exists to prevent.

**THE PRIMARY REASON, measured 2026-08-22: `git diff HEAD --name-only` prints ONLY THE NEW PATH FOR
A RENAME**, hiding the source half entirely:

```
$ git mv docs/policy.md .claude/agents/newreviewer.md
$ git mv client/pin.go client/pin_test.go

$ git status --porcelain
R  docs/policy.md -> .claude/agents/newreviewer.md
R  client/pin.go -> client/pin_test.go

$ git diff HEAD --name-only
.claude/agents/newreviewer.md
client/pin_test.go            # client/pin.go's DELETION is invisible
```

The `client/pin.go -> client/pin_test.go` case is the one to write down: `client/pin.go` is the
single file where `InsecureSkipVerify` is permitted (invariant 11), and under
`git diff --name-only` that change reads as **TESTS-ONLY**.

**THE SECONDARY REASON, still real and still worth stating:** `git diff HEAD --name-only` prints
nothing, exit 0, for a new untracked `.go` file, while `git status --porcelain` shows it as `??`. A
classifier that cannot see a brand-new non-test `.go` file is blind to the file most in need of
classification. What is NOT true is the old claim that a pathspec commit would then ship it.

**Hard requirement, not a preference:**

> `scripts/change-tier.sh` computes its file set from `git status --porcelain --no-renames` over the
> exact pathspec. (`--no-renames` is required by finding F1 below; without it a rename arrives as a
> single `R  old -> new` line and only one half of it gets classified.)

A classifier that cannot see new files has a hole in the same shape as the guards this project keeps
finding that cannot fire.

**Required fixture (in addition to those already listed):** a brand-new UNTRACKED non-test `.go`
file must fire signal 1. **That fixture must be shown RED first**, and the RED output quoted in the
task's `kind=report` -- a proof never observed failing is not evidence (PITFALLS.md section 2).

Note this does not retire the `--name-status` requirement above: `git status --porcelain` carries the
same D (delete) and R (rename) information in its two status columns, and the classifier must still
read deletes and renames from it. Both requirements stand; only the COMMAND changes.

**Implementation constraint on the script, applying to every check inside it:**
**`grep` exits 1 when it matches nothing**, so `... | grep -Ev '<pattern>' && echo OK` is INVERTED
and prints nothing on the failing case. Every check inside `change-tier.sh` -- and inside
`change-tier_test.sh` -- must be judged by **EMPTY OUTPUT, never by exit status**. PITFALLS.md
section 1 records the identical trap for `gofmt -l`, which exits 0 even when it lists files. The two
traps are mirror images and both are live in this repo's history.

---

## RULINGS 2026-08-22 (coordinator, via spec-keeper)

**Ruling 1 -- SIGNAL 1 IS RE-SCOPED BY THE TIER COLLAPSE** (T-01 / 4d990ef4, ruling 2: T1 removed,
four lanes T0/T2/T3/T4). Signal 1 is now the **T0/T2** boundary, not the T1/T2 one:

> **Any `.go` file changed -- test or not -- floors T2.**

The scope bullet above reading "Signal 1 only: any non-test .go file changed" is superseded on the
test/non-test point. The test/non-test distinction is **no longer a tier input**. It survives as an
input to the PLANE PARTITION (T-04): a test-only change inside a mapped plane package stays **T2**
unless the guard signal (T-05) or the test-removal signal (T-06) fires.

**Ruling 2 -- FOUR ACCEPTANCE FIXTURES, each asserting the EXPECTED TIER, each shown RED before its
signal exists. A classifier that cannot sort these four is not finished.**

> **SEQUENCING FIX 2026-08-22 -- READ RATIFICATION S1 AT THE END OF THIS DESCRIPTION BEFORE
> IMPLEMENTING THIS RULING. These four fixtures go in a SEPARATE FILE, `scripts/change-tier_acceptance.sh`,
> which is explicitly NOT part of this task's `proof_cmd`. As originally written, this ruling made
> THIS TASK'S OWN PROOF GUARANTEED RED. The requirement to author them RED is unchanged.**

| fixture | expected tier | why |
|---|---|---|
| `cmd/agent-bus/main.go:67` -- flip `enrolmentInviteRequired` from `true` to `false` | **T3+** | invariants 3 and 11. Measured **T2** under the original mapping. This is the headline miss. |
| an edit inside `internal/signing` that adds NO new import | **T3** | invariant 9. The original "any `crypto/*` import" rule never fires on it. |
| a `go.mod` change adding a dependency | **T4** | invariant 8, which mandates a `DECISIONS.md` entry. |
| `scripts/change-tier.sh` classifying ITSELF | **T3+** | control plane. Measured **T0** under the original design. |

These are acceptance criteria for the epic; the signals they exercise land in T-04..T-09. This task
lands the harness able to express them, and each must be RED at the point it is written.

**Ruling 3 -- COUNTING USES `git ls-files`, NEVER A FILESYSTEM WALK. Write this as a CONTRACT LINE
in the script and in `docs/CHANGE-TIERS.md`, not as a note.** Measured 2026-08-22: eight nested
checkouts under `.claude/worktrees/` and `.worktrees/` inflate a naive `grep -r` count from 5/16 to
**45/159**. Any signal that counts files is silently wrong without this.

**Ruling 4 -- CONFIRM THESE TWO ARE PRESENT; add them if not.** Both are in Amendment B above and
neither is optional:
- the **`git status --porcelain --no-renames`** basis, never `git diff --name-only`. PRIMARY
  reason: `git diff --name-only` prints only the NEW path for a rename, hiding the source half
  (`client/pin.go -> client/pin_test.go` reads as tests-only). SECONDARY reason: a new untracked
  `.go` file is absent from the diff (reproduced 2026-08-22: empty output, exit 0). `--no-renames`
  is required by F1 below; the withdrawn "a pathspec commit takes that file" rationale is corrected
  in Amendment B above;
- the **`grep` exit-status rule** -- judge every internal check by EMPTY OUTPUT; `grep` exits 1 on
  no match.

---

## SECURITY FINDINGS F1-F3 (2026-08-22) -- inherited by this classifier

A security gate returned **CHANGES-REQUESTED** on the interim docs-and-tests-only carve-out with
three findings. It agreed the PRINCIPLE was right and still found three bypasses in the mechanical
form. `scripts/change-tier.sh` classifies by path, so it inherits every one of them. All three are
MEASURED, not theorised.

**F1 -- RENAMES BYPASS PATH-BASED CLASSIFICATION.** `git status --porcelain` prints a rename as ONE
line: `R  old -> new`. A check anchored at `^` never sees the TARGET; a check testing the line END
never sees the SOURCE.

> **CONTRACT REQUIREMENT: the file set comes from `git status --porcelain --no-renames`**, which
> splits the rename into its two halves so BOTH are classified.

Verified 2026-08-22:

```
$ git status --porcelain --no-renames
A  .claude/agents/newreviewer.md
D  client/pin.go
A  client/pin_test.go
D  docs/policy.md
```

Named bypasses that classify as docs-only WITHOUT `--no-renames`:
- `docs/policy.md -> .claude/agents/newreviewer.md` -- CREATES A CONTROL-PLANE FILE (a new reviewer
  agent definition) out of a change that reads as a docs edit;
- `CLAUDE.md -> DOCS.md`;
- `client/pin.go -> client/pin_test.go` -- moves the one file where `InsecureSkipVerify` is
  permitted (invariant 11) into a name that reads as a test.

**F2 -- PATH MATCHING FAILS OPEN.** `git status --porcelain -- <pathspec matching nothing>` prints
nothing and exits **0** (verified 2026-08-22). A typo, the wrong case, the wrong cwd, or an unquoted
path containing a space all yield empty output -- which a naive reading treats as "no risky files".
Measured: an unreviewed `client/pin.go` edit, checked as `client/Pin.go`, classifies CLEAN.

> **REQUIREMENT: empty output must NEVER yield a low tier by default.** The exit codes this task
> defines must distinguish **"MEASURED T0"** from **"COULD NOT MEASURE"**. The latter is an ERROR
> EXIT, not a result.

Collapsing those two is the same failure in a different costume, and it is exactly the shape
`scripts/proof-check.sh` already guards against with its VACUOUS verdict: a check that ran nothing
is not a pass.

**F3 -- INHERITANCE, stated so no signal task can miss it.** Every signal that classifies by path
inherits F1 and F2: **T-04 (255bdc5a), T-05 (212e695b), T-06 (9921c55d), T-09 (4604ae4d), T-10
(3c9c28d9)**. Each of those must consume the `--no-renames` file set, must not treat an empty file
set as low-risk, and needs a RENAME FIXTURE of its own. The same line is recorded on each of those
tasks.

---

## RATIFICATION S1 2026-08-22 (coordinator, via spec-keeper): THE SEQUENCING DEFECT, AND ITS FIX

**The defect, stated plainly.** This task's `proof_cmd` is
`bash scripts/proof-check.sh 'bash scripts/change-tier_test.sh'` -- the WHOLE harness. Ruling 2
above requires FOUR acceptance fixtures written into that harness and **RED at the point they are
written**, because the signals they exercise land in T-04..T-09 and do not exist yet. Those two
requirements are in direct contradiction: **as written, this task can NEVER complete with a green
proof.** Its own proof is guaranteed RED by its own acceptance criteria.

**The fix, normative:**

1. **The four acceptance fixtures go in a SEPARATE FILE, `scripts/change-tier_acceptance.sh`.** That
   file is explicitly **NOT** part of this task's `proof_cmd`.
2. **This task's `proof_cmd` stays the UNIT HARNESS**, `scripts/change-tier_test.sh` -- the
   skeleton, the diff basis, the exit codes and signal 1. Those are all implemented here, so that
   proof can and must go GREEN here.
3. **This task LANDS `scripts/change-tier_acceptance.sh` with all four cases written**, each
   annotated in-file with the TASK that will turn it green (invite gate / `internal/signing` /
   `change-tier.sh` self-classification -> T-04 `255bdc5a-f36e-4cfb-a484-199fbd6d16ab`;
   `go.mod` -> T-04's sibling T-09 `4604ae4d-a8b3-4272-9226-67557de66de3`), and each **verified RED
   at authoring time**.
4. **That RED observation is recorded in THIS task's `kind=report`, quoting the actual output.**
   That record is what Ruling 2 actually wanted: evidence that each fixture CAN fail. It is
   satisfied by the report, not by the proof exit code.
5. **The gate that requires all four GREEN is a separate task: T-20
   (`461ebf5b-5d0e-4c3d-b95e-a1437e15b31f`)**, blocked by T-04 and T-09.

**Why they are split, said plainly so it is not "simplified" back together later:** a task whose
proof CANNOT PASS is not a strict task. It is a task that will be completed on a **vacuous or edited
proof** -- because at completion time the cheapest available move is to rewrite the assertion until
it fits. That is `PITFALLS.md` section 2, and it is the exact subject of task
`4faa6782-6b49-4507-9a23-bb2cf42e7d02` ("a proof that stays RED against a tree where the work IS
done"). Making a proof unpassable does not make a task stricter; it makes the proof the thing that
gets edited.

**Unchanged by this fix:** the RED-first discipline for every signal's own fixtures in
`scripts/change-tier_test.sh`, and Ruling 2's four cases with their expected tiers.

**Documentation consequence:** `scripts/change-tier_acceptance.sh` is a third new script from this
task. Add it to the CONTRACTS-AGENT.md tables alongside `change-tier.sh` and `change-tier_test.sh`,
and the stale "scripts/ holds exactly six files" claim at CONTRACTS-AGENT.md:19 becomes TWELVE after
this task, not eleven.

**BLOCKS T-20 (`461ebf5b-5d0e-4c3d-b95e-a1437e15b31f`) and T-19
(`4629eb94-5ddb-4acb-98a1-125230ca5afe`)**, in addition to T-04..T-09 and T-10 already recorded.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [212e695b-c11c-485b-aaa4-730d2f0ebd13](../change-tier.sh-guard-file-and-verification-infrastructur--212e695b/task.md) — change-tier.sh: guard-file and verification-infrastructure signal (todo)
- [255bdc5a-f36e-4cfb-a484-199fbd6d16ab](../change-tier.sh-package-to-invariant-plane-partition-with--255bdc5a/task.md) — change-tier.sh: package to invariant-plane partition, with DEFAULT-DENY for unmapped paths (todo)
- [3c9c28d9-a02e-465b-b13b-6f9d29056eb4](../Decide-and-implement-the-client-exported-API-signal-or-r--3c9c28d9/task.md) — Decide and implement the client/ exported-API signal, or replace it with a path floor (todo)
- [4604ae4d-a8b3-4272-9226-67557de66de3](../change-tier.sh-irreversible-surface-pinned-record-wire-v--4604ae4d/task.md) — change-tier.sh: irreversible surface -- pinned record/wire-version constants and go.mod d… (todo)
- [461ebf5b-5d0e-4c3d-b95e-a1437e15b31f](../Acceptance-gate-all-four-low-measuring-cases-sort-correc--461ebf5b/task.md) — Acceptance gate: all four low-measuring cases sort correctly (todo)
- [4629eb94-5ddb-4acb-98a1-125230ca5afe](../change-tier.sh-credential-bearing-files-floor-at-T3-inde--4629eb94/task.md) — change-tier.sh: credential-bearing files floor at T3, independent of every other signal (todo)
- [4d990ef4-23ee-4971-ab00-84eb5ec137ae](../Write-docs-CHANGE-TIERS.md-the-normative-tier-and-signal--4d990ef4/task.md) — Write docs/CHANGE-TIERS.md, the normative tier and signal specification (todo)
- [4faa6782-6b49-4507-9a23-bb2cf42e7d02](../PITFALLS.md-a-proof-that-stays-RED-against-a-tree-where--4faa6782/task.md) — PITFALLS.md: a proof that stays RED against a tree where the work IS done (todo)
- [9921c55d-d8a0-460c-ac5f-91a6bb6adcf2](../change-tier.sh-test-removal-signal-the-missing-signal--9921c55d/task.md) — change-tier.sh: test-removal signal (the missing signal) (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [212e695b-c11c-485b-aaa4-730d2f0ebd13](../change-tier.sh-guard-file-and-verification-infrastructur--212e695b/task.md) — change-tier.sh: guard-file and verification-infrastructure signal (todo)
- [255bdc5a-f36e-4cfb-a484-199fbd6d16ab](../change-tier.sh-package-to-invariant-plane-partition-with--255bdc5a/task.md) — change-tier.sh: package to invariant-plane partition, with DEFAULT-DENY for unmapped paths (todo)
- [3c9c28d9-a02e-465b-b13b-6f9d29056eb4](../Decide-and-implement-the-client-exported-API-signal-or-r--3c9c28d9/task.md) — Decide and implement the client/ exported-API signal, or replace it with a path floor (todo)
- [4604ae4d-a8b3-4272-9226-67557de66de3](../change-tier.sh-irreversible-surface-pinned-record-wire-v--4604ae4d/task.md) — change-tier.sh: irreversible surface -- pinned record/wire-version constants and go.mod d… (todo)
- [461ebf5b-5d0e-4c3d-b95e-a1437e15b31f](../Acceptance-gate-all-four-low-measuring-cases-sort-correc--461ebf5b/task.md) — Acceptance gate: all four low-measuring cases sort correctly (todo)
- [4629eb94-5ddb-4acb-98a1-125230ca5afe](../change-tier.sh-credential-bearing-files-floor-at-T3-inde--4629eb94/task.md) — change-tier.sh: credential-bearing files floor at T3, independent of every other signal (todo)
- [4d990ef4-23ee-4971-ab00-84eb5ec137ae](../Write-docs-CHANGE-TIERS.md-the-normative-tier-and-signal--4d990ef4/task.md) — Write docs/CHANGE-TIERS.md, the normative tier and signal specification (todo)
- [9921c55d-d8a0-460c-ac5f-91a6bb6adcf2](../change-tier.sh-test-removal-signal-the-missing-signal--9921c55d/task.md) — change-tier.sh: test-removal signal (the missing signal) (todo)
- [c65a5051-678c-487c-bdae-37183e01f049](../scripts-spec-cloud.sh-a-caller-supplied-w-breaks-status--c65a5051/task.md) — scripts/spec-cloud.sh: a caller-supplied -w breaks status detection and makes a 200 exit 5 (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
