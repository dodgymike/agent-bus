# proof-check.sh head_token word-splits the LITERAL text of a VAR=$(...) proof_cmd, mis-refusing valid commands as UNVERIFIABLE

| Field | Value |
| --- | --- |
| Public id | `017304e6-a088-40c9-b6c2-5cac4bc0fb66` |
| Key | _(null in the export)_ |
| Epic | [UNASSIGNED](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T11:28:10.958758+00:00 |
| Updated | 2026-08-21T11:31:34.092200+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'X=$(echo one two three); test -n "$X" && echo GOT_$X' 2>&1 | grep -q 'GOT_one two three' && echo PROOFCHECK_FIXED
```

## Description

# CORRECTION (2026-08-21) -- MECHANISM

The root-cause LOCATION below is correct: scripts/proof-check.sh:278, the unquoted
`set -- $seg` inside `head_token()` (defined at :271). The MECHANISM as originally filed is
WRONG and must not be built against.

FILED (incorrect, see ORIGINAL TEXT below): "bash both executes the embedded $(...) and then
word-splits its output."

ACTUAL: bash does NOT re-scan the result of an unquoted VARIABLE expansion for command
substitution -- expansion results are word-split and globbed only, never re-executed. So
`set -- $seg` splits the LITERAL TEXT of the proof command, not the output of anything it
contains. For `X=$(echo one two three)` the literal words are: `X=$(echo` / `one` / `two` /
`three)`. head_token skips `X=$(echo` as a recognised env-assignment prefix and lands on the
literal word `one`.

WHY THE WRONG MECHANISM LOOKED RIGHT: in that one repro, the literal text after the assignment
prefix ("one two three)") happens to be byte-identical to what the command would actually output
("one two three"). A perfectly misleading fixture -- the two hypotheses are indistinguishable on
that example alone.

DISAMBIGUATING EVIDENCE (run 2026-08-21 against HEAD 665971c):

    $ bash scripts/proof-check.sh 'X=$(printf %s%s A B); echo $X'
    -> UNVERIFIABLE -- segment 1 starts with '%s%s'
    Literal-splitting predicts %s%s (the literal text after the assignment prefix). Execution
    predicts AB (printf %s%s A B outputs AB). It reported %s%s: literal-splitting confirmed,
    execution ruled out.

    $ bash scripts/proof-check.sh 'Y=$(touch <scratch>/sideeffect.txt; echo hi); echo $Y'
    -> segment 1 named the PATH, and the file was NOT created.
    Execution predicts the word "hi" and a created file. Neither happened: no side effect, no
    execution.

TWO CONSEQUENCES:

1. THERE IS NO SIDE-EFFECT-DURING-CLASSIFICATION AND NO SECURITY DIMENSION. proof-check.sh's
   "refusing to run it" claim is TRUE -- nothing in the proof command executes during
   classification. SCOPE SANDBOXING AND SIDE-EFFECT CONTAINMENT OUT of this task; building those
   would be defending against a defect that does not exist.

2. The fix is a TEXT-CLASSIFICATION fix, not an evaluation-order fix: head_token must recognise
   an assignment whose value is a command substitution (or arithmetic expression) and skip to the
   end of the `$(...)` (or `$((...))`) rather than treating its interior words as separate
   commands/tokens. Note `set -f` at :277 shows the author already reasoned about globbing ("We
   want words, not matches") but not about `$(...)` interiors -- the same instinct, one step
   short.

The severity assessment below is UNCHANGED and remains correct: this fails SAFE (the wrong
verdict is UNVERIFIABLE, a refusal, never a false PASS), P2 is right, and the real risk stays
second-order -- an author whose valid proof is refused rewrites it until it parses, and a
contorted proof is a weaker proof. Keep the existing requirement that the fixer demonstrate BOTH
that a valid `VAR=$(...)`/arithmetic proof now runs AND that genuine prose is still refused.

# ORIGINAL TEXT (2026-08-21), MECHANISM CORRECTED ABOVE -- location claim (scripts/proof-check.sh:278,
# head_token) still stands; the "executes ... and word-splits" mechanism sentence below is WRONG,
# see the correction above.

THE DEFECT -- reproduced verbatim, HEAD 665971c
scripts/proof-check.sh mis-parses a top-level shell assignment whose value is a command
substitution containing spaces. The culprit is `head_token()` (defined scripts/proof-check.sh:271):
line 278 does `set -- $seg` with $seg UNQUOTED. Bash expands the unquoted variable, which both (a)
EXECUTES any embedded `$(...)` command substitution and (b) then WORD-SPLITS its output, so
`X=$(echo one two three)` becomes positional params `X=one`, `two`, `three` -- the loop consumes
the `X=` prefix as a recognised env-assignment and is left staring at `one`/`two`/`three` as if one
of them were the command name.

Repro (run from repo root):

    $ bash scripts/proof-check.sh 'X=$(echo one two three); test -n "$X" && echo GOT_$X'
    proof-check: UNVERIFIABLE -- segment 1 starts with 'one', which is not an executable command
    here (prose, or a wrapper that does not exist yet)
    proof-check: refusing to run it; this is NOT a claim that the work is broken.
    proof-check: verdict=UNVERIFIABLE class=unrunnable exit=- tests_run=0 top_level=0 skipped=0
    failed=0 empty_pkgs=0

Direct execution of the SAME inner command works fine (`GOT_one two three`, exit 0) -- the bug is
entirely in proof-check.sh's classifier, not in the proof itself. Also affects bare
`$((...))` arithmetic assignments with internal spaces for the analogous reason (word-splitting
of text that should be treated as one already-evaluated token, or is otherwise misread as separate
segments/tokens).

Already independently hit today by a spec-keeper writing the proof_cmd for task dd2cdc20
(reservation-counter-drift task) -- see that task's description, final paragraph ("NOTE for
whoever re-verifies"), which records the same symptom against `TMPD=$(mktemp -d)` (reported as
segment '-d)') and the workaround used (prefixing such assignments with `export`, and stripping
internal spaces from `$((...))` arithmetic). That note explicitly says the quirk "did not seem
worth a separate filed task on its own" -- this task is that separate filing, so the defect has a
home with its own proof_cmd instead of living only as a buried aside.

WHY IT MATTERS -- severity, precisely
proof-check.sh is the project's verification backbone: CLAUDE.md requires every task be completed
by RUNNING it and quoting its verdict, and forbids completing on VACUOUS.

It FAILS SAFE: the wrong verdict here is UNVERIFIABLE (a refusal to run), never a false PASS. This
is NOT a claim that proofs have been passing when they should not -- state that plainly when
working this task.

The real risk is second-order, and is the reason to fix it: an agent whose valid proof is refused
will rewrite it until it parses. Contorting a proof to satisfy a broken parser can quietly make it
WEAKER -- and a weakened proof is exactly what this repo keeps paying for (an incidental-match
`grep` proof already green-lit a bad close here once). A refusal that pushes authors toward
simpler, less rigorous proof commands is a correctness risk even though the refusal itself is safe.

SCOPE
Make the head_token/segment check recognise `VAR=$(...)`, `export VAR=$(...)` and `$((...))`
arithmetic without re-word-splitting the ALREADY-SUBSTITUTED output -- i.e. classify based on the
SYNTACTIC shape of the segment (is its head an env-assignment followed by a command-substitution or
arithmetic expression?) rather than by executing the segment during classification. Keep the
existing protection that genuinely refuses prose and non-existent wrappers -- do NOT loosen
`resolvable()`/UNVERIFIABLE broadly. The UNVERIFIABLE class exists specifically to stop proofs that
never run; over-correcting it (e.g. making the segment check permissive enough that arbitrary prose
slips through) reintroduces the exact failure it was built to prevent. Whoever fixes this must
demonstrate BOTH:
  1. the repro command above (and the dd2cdc20-style `TMPD=$(mktemp -d)` case) now classifies and
     RUNS correctly, and
  2. a genuine prose string (e.g. `this is not a runnable command at all`) is STILL refused as
     UNVERIFIABLE.

A regression test belongs in scripts/proof-check_test.sh (already exists as the guard test for this
script, pinning the caller-cwd fix from 535876c) -- do not write it as part of this task's scope
note, but the fix should add cases there.

PROOF_CMD -- proven to go RED today, HEAD 665971c
Run via `bash scripts/proof-check.sh '<cmd>'` (meta) AND directly via `bash -c '<cmd>'` (since the
tool under test is also the thing being used to test it):

Meta (bash scripts/proof-check.sh 'X=$(echo one two three); test -n "$X" && echo GOT_$X'):

    proof-check: proof: X=$(echo one two three); test -n "$X" && echo GOT_$X
    proof-check: class: unrunnable
    proof-check: warning: the proof joins commands with ';', which DISCARDS the exit status of
      everything before the last one. Prefer '&&' so a failure actually fails.
    proof-check: UNVERIFIABLE -- segment 1 starts with 'one', which is not an executable command
    here (prose, or a wrapper that does not exist yet)
    proof-check: refusing to run it; this is NOT a claim that the work is broken.
    proof-check: verdict=UNVERIFIABLE class=unrunnable exit=- tests_run=0 top_level=0 skipped=0
    failed=0 empty_pkgs=0
    exit=3

Direct (bash -c 'X=$(echo one two three); test -n "$X" && echo GOT_$X'):

    GOT_one two three
    exit=0

Control -- genuine prose must still be refused, both today and after the fix
(bash scripts/proof-check.sh 'this is not a runnable command at all'):

    proof-check: UNVERIFIABLE -- segment 1 starts with 'this', which is not an executable command
    here (prose, or a wrapper that does not exist yet)
    proof-check: verdict=UNVERIFIABLE class=unrunnable exit=- tests_run=0 top_level=0 skipped=0
    failed=0 empty_pkgs=0
    exit=3

Definition of done for this task: re-run the meta invocation above after the fix and it must
classify as runnable and actually execute (printing `GOT_one two three`, exit 0 from proof-check's
perspective, no UNVERIFIABLE), while the control prose command above must still print the same
UNVERIFIABLE verdict quoted here. Quote both verdicts verbatim in the completion note, not a bare
exit code.

CONSTRAINTS honoured while filing: read-only investigation, zero files edited, zero commits, zero
reservations POSTed (per operator instruction -- task-key counters are drifting, see dd2cdc20).
Repo HEAD 665971c untouched by this filing.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](../Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) — Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
