# scripts/proof-check.sh: head_token mis-parses an assignment from a command substitution containing a space

| Field | Value |
| --- | --- |
| Public id | `5960dd68-e457-4f3f-a1f2-47d1380963c2` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T09:50:15.693289+00:00 |
| Updated | 2026-08-22T09:50:15.693289+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/proof-check.sh "D=\$(mktemp -d) && test -d \"\$D\" && rm -rf -- \"\$D\"" 2>&1 | grep -q "verdict=PASS" && bash scripts/proof-check.sh "R=\$(git rev-parse --short HEAD) && test -n \"\$R\"" 2>&1 | grep -q "verdict=PASS" && bash scripts/proof-check.sh "the manifest should list every guard file" 2>&1 | grep -q "verdict=UNVERIFIABLE" && grep -q "rev-parse" scripts/proof-check_test.sh && bash scripts/proof-check_test.sh >/dev/null 2>&1'
```

## Description

`scripts/proof-check.sh` refuses a valid proof whose FIRST segment assigns from a command
substitution containing whitespace. Reproduced independently by the coordinator and re-reproduced
by spec-keeper 2026-08-22:

    $ bash scripts/proof-check.sh 'D=$(mktemp -d) && echo hi'
    proof-check: UNVERIFIABLE - segment 1 starts with '-d)', which is not an executable command here
    proof-check: verdict=UNVERIFIABLE class=unrunnable exit=3

ROOT CAUSE - `head_token`, NOT the splitter.
`split_segments` is correct: it tracks `$( )` nesting depth explicitly (scripts/proof-check.sh:15-21
of that function's awk body - `if (c == "$" && substr(s,i+1,1) == "(") { depth++ ... }`) and keeps
the substitution intact inside the segment. The defect is in `head_token`
(scripts/proof-check.sh:271-290), which then word-splits that segment with a bare unquoted
`set -- $seg` (:278) and has NO notion of `$( )` grouping. So the interior words of the substitution
enter the token stream as separate positional parameters. The assignment-prefix skip at :284
(`*=*)` with an identifier-shaped LHS) shifts past `D=$(mktemp` and lands on `-d)`, which is then
tested by `resolvable` and rejected at :371.

TWO FAILURE DIRECTIONS, and the second is the more dangerous one.

(1) FALSE UNVERIFIABLE - the interior word is not a command, so a runnable proof is refused.
    Measured 2026-08-22:
      `D=$(mktemp -d) && test -d "$D" && rm -rf -- "$D"`        -> UNVERIFIABLE, exit 3 (token `-d)`)
      `R=$(git rev-parse --short HEAD) && test -n "$R"`         -> UNVERIFIABLE, exit 3 (token `rev-parse`)

(2) FALSE ACCEPT ON THE WRONG TOKEN - if the interior word happens to name a real executable, the
    proof is admitted, but the token that validated it is NOT the command that runs. Measured
    2026-08-22:
      `G=$(go env GOROOT) && test -d "$G"`                      -> PASS
    That PASS is luck: `head_token` returned `env` (a real PATH binary), not `go`. The runnability
    check therefore validated an unrelated program. This is the "an assertion can pass for the wrong
    reason" shape in PITFALLS.md section 2/6 - it is a silent wrong-reason accept inside the very
    instrument that exists to catch wrong-reason passes. A fix that only stops the false
    UNVERIFIABLE and leaves `head_token` resolving to an interior word has fixed the visible half
    and left this half in place.

WHY P1, NOT COSMETIC.
`scripts/proof-check.sh` is the instrument every task's evidence passes through, and it is CONTROL
PLANE under the principle ratified in PITFALLS.md section 8.2 - it performs the check. A false
UNVERIFIABLE is not a harmless refusal:
  - it pushes authors toward proofs simple enough to survive the parser, which is selection pressure
    toward WEAKER evidence;
  - it is indistinguishable at a glance from a proof that genuinely is prose, so the signal that is
    supposed to separate the two is degraded;
  - it fires on `mktemp -d`, the exact idiom this repo's own script-test convention mandates for
    fixture directories.
Consequence (2) above additionally means the unrunnable class can accept on a token nobody intended.

REQUIRED REGRESSION CASES in `scripts/proof-check_test.sh` - all three, not a subset:
  a. assignment from a command substitution with an argument: `mktemp -d` - must become runnable;
  b. assignment from a FLAG-BEARING git substitution: `git rev-parse --short HEAD` - must become
     runnable (this one is why `rev-parse` is pinned in proof_cmd: the token is currently absent from
     the test file, `grep -c rev-parse scripts/proof-check_test.sh` = 0 on 2026-08-22, so the pin is
     RED today and cannot pass on an incidental match);
  c. a CONTROL that IS genuine prose and MUST STILL BE REFUSED as `unrunnable`.
CASE (c) IS THE POINT OF THE TASK, not decoration. The value of this guard is refusing prose. A
careless fix - widening `resolvable`, or accepting any token containing `$(`, or short-circuiting the
unrunnable class whenever an assignment is present - would make every proof runnable and DISABLE the
unrunnable class entirely, converting a false refusal into a universal false accept. The control is
what makes that impossible to ship green.
Do NOT pin case (a) with `grep -q 'mktemp -d' scripts/proof-check_test.sh`: that string ALREADY
appears once in the file (fixture setup, measured 2026-08-22), so such a pin passes on an incidental
match and proves nothing - the exact grep-proof trap in PITFALLS.md section 2.

SUGGESTED SHAPE OF THE FIX (not binding): make `head_token` aware of `$( )` / backtick grouping the
way `split_segments` already is, so that the token it yields for `D=$(mktemp -d) && ...` is the
command actually invoked (`mktemp`), or the assignment is recognised as an assignment and the check
moves to the next real segment. Whatever the shape, it must satisfy (c) and must not leave direction
(2) resolving to an interior word.

DEFINITION OF DONE:
1. The three regression cases above are in `scripts/proof-check_test.sh` and pass.
2. `head_token` no longer yields an interior word of a command substitution; direction (2) is fixed
   as well as direction (1) - state in the kind=report which token `head_token` now returns for
   `G=$(go env GOROOT) && test -d "$G"`.
3. proof_cmd shown RED before the fix and GREEN after. RED evidence is already recorded below - do
   not re-derive it as if unknown, but DO re-run it before claiming green.
4. `scripts/proof-check.sh` is CONTROL PLANE (PITFALLS.md 8.2): the security gate is REQUIRED on this
   change and may not take the docs-and-tests-only carve-out. Reviewer runs unconditionally.
5. No behaviour change to any verdict other than the misparse: a full `bash scripts/proof-check_test.sh`
   stays green, and the existing VACUOUS / FAIL / UNVERIFIABLE classifications are unchanged for
   every case already covered.

RED EVIDENCE RECORDED 2026-08-22 (spec-keeper, before filing).
The stored proof_cmd was RUN before storage. Verdict:
    proof-check: FAIL - proof command exited 1
    proof-check: verdict=FAIL class=wrapper,file-assertion exit=1 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0
So it is RED today, as required.

HOW TO READ THIS proof_cmd (it nests proof-check inside proof-check - deliberate).
The OUTER `proof-check.sh` was verified NOT to be confused by the nesting: the outer segment starts
with `bash`, which is resolvable, so the outer parser classifies it `wrapper,file-assertion` and runs
it. The outer verdict is therefore a genuine PASS/FAIL of the whole clause chain, not a parse
artefact. The INNER invocations are the subject under test; their verdicts are read by grepping their
own `verdict=` summary lines. The chain asserts, in order:
  1. inner `D=$(mktemp -d) ...`            -> must print `verdict=PASS`   (RED today: UNVERIFIABLE)
  2. inner `R=$(git rev-parse --short HEAD)...` -> must print `verdict=PASS`   (RED today: UNVERIFIABLE)
  3. inner genuine prose                    -> must print `verdict=UNVERIFIABLE` (GREEN today; the
     anti-"accept everything" control - if this clause ever goes red the fix disabled the class)
  4. `rev-parse` present in proof-check_test.sh (RED today: 0 occurrences)
  5. full `bash scripts/proof-check_test.sh` green (GREEN today; must stay green)
Clauses 1, 2 and 4 are independently RED today, so the FAIL is not resting on a single clause.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
