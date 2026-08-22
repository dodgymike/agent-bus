# PITFALLS — the traps, with the incident behind each

Each entry below is a way a check can report success while proving nothing, or a way a commit can
ship something its author did not write. Every one of them has already happened in this repository.

**This file holds the RULE and the INCIDENT. `CLAUDE.md` holds the one-line rule only.** The split
is the same one `INVARIANTS.md` uses, and for the same reason: `CLAUDE.md` is injected into every
sub-agent on every spawn, so its bytes are paid per dispatch, while this file is read when a task
actually reaches the trap it describes. `CLAUDE.md` links to each section here by name.

**Do not shorten the incidents out of this file.** The short version of every rule below was already
present when the rule was broken. The dates, commit shas, file names and exact quoted output are the
part that makes an agent check rather than assume. If you are about to compress a paragraph here,
you are probably about to reintroduce the defect it records.

**Do not delete an entry to make a byte budget.** `docs/doc-preserve.tsv` exists because compressing
`CLAUDE.md` has already been caught three times removing something load-bearing. Relocating a warning
and leaving a pointer is correct; deleting one is a regression.

---

## 1. Formatting checks that pass without running

### 1.1 Bare `gofmt` is not on PATH, and its absence reads as success

`gofmt` is NOT on PATH on this box — only `$(go env GOROOT)/bin/gofmt` is. The idiomatic check is
therefore self-defeating:

```
test -z "$(gofmt -l .)"      # WRONG: passes when gofmt exits 127
```

A command that fails to launch prints nothing to stdout, so the command substitution is empty and
`test -z` succeeds. Every "gofmt clean" ever recorded from a bare call is a false pass. Use one of:

```
go fmt ./...                      # reformats in place; prints the files it changed
"$(go env GOROOT)/bin/gofmt" -l . # lists unformatted files; empty output = clean
```

### 1.2 `gofmt -l` exits 0 even when it lists files

It reports by printing, not by exit status. So `gofmt -l . && echo CLEAN` prints `CLEAN` over a list
of unformatted files. This is a second false pass and the 127 case above does not cover it.

Observed 2026-08-07: a verification chain echoed `GOFMT_CLEAN` while `gofmt -l` had, in the same
command, just named `client/messages_test.go`.

Judge it by whether the OUTPUT is empty, never by exit status:

```
test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"   # correct: tests the output
```

---

## 2. Proofs that prove nothing

Run every proof command through `bash scripts/proof-check.sh '<cmd>'`, which reports
PASS / FAIL / VACUOUS / UNVERIFIABLE, and quote its verdict rather than a bare exit code. A task must
never be completed on a VACUOUS proof, and **a task with NO `proof_cmd` may not be completed at
all** — a missing proof is worse than a vacuous one, because it leaves no record of what would even
count as evidence. Completing a task requires RUNNING `proof-check.sh` and quoting its verdict, not
storing a command nobody executed.

### 2.1 A `-run` filter naming a test that does not exist

`go test -run TestThatDoesNotExist ./pkg` prints `ok ... [no tests to run]` and EXITS 0. A proof
command naming a test that was never written is indistinguishable, by exit status, from a passing
one.

### 2.2 A passing parent test does not rescue skipped children

Go reports a parent as PASS when every leaf subtest called `t.Skip`. That shape exercised no
assertion, and `proof-check.sh` therefore reports VACUOUS. Its plain-text and JSON parsers judge leaf
results, so an indented child `--- SKIP:` cannot be hidden by the unindented parent `--- PASS:` line.
Results stay scoped to their package, and a package's `[no tests to run]` summary overrides
marker-shaped output printed by `TestMain`.

### 2.3 An unquoted `-run` regex is re-parsed by the inner shell

`proof-check.sh` runs the proof through an inner `bash -c`. An unquoted `-run` regex containing
parens or a pipe has those characters re-parsed as shell metacharacters, so the command that runs is
not the command that was stored. The verdict is `verdict=UNVERIFIABLE` (exit 3) — such a proof could
never have passed.

Recorded sites: `ACK-3` and `ACK-4` carried `proof_cmd`s that were UNVERIFIABLE BY CONSTRUCTION for
this reason (`ACK-CONTRACT.md:786`, task `0fb4d032`); a RELAY task's stored proof had an unquoted `|`
re-parsed as a shell pipe and was corrected to the double-quoted `-run` form shortly before its
commit (`AGENT_LOG.md:3904`). Double-quote the `-run` argument.

### 2.4 `grep`-based doc proofs pass on an incidental match

This is the MORE dangerous family, and `CLAUDE.md` previously warned only about tests. A doc proof
like

```
grep -n '8080' README.md CONTRACTS.md | grep -qi localhost && echo DOCS_OK
```

passes on an incidental match somewhere else in the file. In the real case that was a pre-existing
`curl -s localhost:8080/healthz` line in README, and the proof would have green-lit closing a task
over the exact file two reviewers had blocked on.

A doc proof must pin the specific line it claims to prove — the table row, the field name, the
artefact name — and you must confirm it is **RED before the fix**. A proof that was never observed
failing is not evidence that it can fail. `scripts/doc-check.sh section` exists to scope a doc
assertion to one heading; prefer it to a bare `grep`.

### 2.5 Quote the proof's own number, not a wider one

A task's proof result is the number that proof produced. A full-suite figure relayed in its place
(`tests_run=2386 top_level=598 skipped=40` where the proof's own count was 17) reads as much stronger
evidence than the task actually has, and the narrow number is the one that can be falsified. Caught
by the integrator re-running the stored `proof_cmd` (`AGENT_LOG.md:3896`).

### 2.6 If a test fails, you are not done

Diagnose whether YOUR change caused it or it is pre-existing, name the exact failing test, and report
the verdict. Never hand-wave "pre-existing failures" to declare success.

### 2.7 A backtick inside a doc-check heading is command-substituted by the inner shell

Same family as 2.3, different construct. `proof-check.sh` runs the stored command through a shell, so
a backtick in a `doc-check.sh section` argument is COMMAND SUBSTITUTION, not literal text. Measured
2026-08-22 while proving the CLAUDE.md carve-out:

```
# stored proof (single-quoted here, but the inner shell still sees bare backticks)
bash scripts/doc-check.sh section CLAUDE.md "## Agent roster (`.claude/agents/`)" "<needle>"
  -> proof-check: verdict=FAIL exit=1        # heading became "## Agent roster ()"
# the same assertion, backticks escaped as \`
  -> proof-check: verdict=PASS exit=0
# and run directly, with no shell re-parse:
  -> doc-check: PASS: CLAUDE.md — 1/1 needles inside "## Agent roster (`.claude/agents/`)"
```

The failure direction here was safe (a heading that no longer exists FAILs, per `section`'s
absence-is-failure rule), but the reverse is available: substitute a backtick pair out of a heading
that still resolves and the proof scopes a section the author never named. Escape every backtick in a
stored `proof_cmd`, or choose a heading that has none.

---

## 3. Verify in a clean overlay of HEAD, not in your working tree

A working tree that builds proves nothing about what is COMMITTED. A definition you consume may be
sitting uncommitted beside you, so a consumer can land before its definition and break `main`. That
has happened here.

Extract HEAD, copy in ONLY the files you own, `cd` in, and run the check from there:

```
T=$(mktemp -d); git archive HEAD | tar -x -C "$T"
cp <the paths you own> "$T"/<same paths>     # ONLY your files — nothing else uncommitted
(cd "$T" && go build ./... && bash scripts/proof-check.sh '<cmd>')
```

**Call `proof-check.sh` — and every path in your proof command — by a RELATIVE path from inside
`$T`, never by an absolute path into the live worktree.** `git archive` already places
`scripts/proof-check.sh` in the overlay, so there is nothing to copy: do NOT `cp` the live script
over it, or the one file deciding PASS/FAIL becomes the only uncommitted code in the overlay. The
point is that the verifier's logic comes from HEAD too.

Its *cwd* handling is no longer the hazard — `535876c` made it run proofs in the caller's cwd — but
an absolute path still reaches a script that MAY be uncommitted, and any other absolute path in the
proof reaches uncommitted files.

---

## 4. Commits that ship someone else's work

The five entries below are the same trap approached from different directions. The tree is normally
busy with parallel agents, so every one of them is reachable on an ordinary day.

### 4.1 `git add` does not scope a later commit

ALWAYS commit with an explicit pathspec: `git commit -m '…' -- <paths>`. A bare `git commit` takes
the WHOLE index, including anything a concurrently-running agent has staged.

This has produced four mis-titled commits in this repo, one of which left `main` un-compilable for
several commits because half of a change was swept into an unrelated docs commit while the other half
stayed in the working tree. The working tree looked green throughout, which is why nobody noticed.
Never `git add` then bare-`git commit` while any other agent is running.

### 4.2 A pathspec commit takes the WORKTREE, not the index

This is the other half of 4.1 and it fails in the opposite direction, so `git add` does not protect
you either. `git commit -- <path>` commits that path's WORKING-TREE content, silently discarding
whatever you staged for it.

On a file showing `MM` in `git status --porcelain` — index clean, worktree dirty — carefully staging
only your own text and then committing by pathspec ships the OTHER agent's unstaged edits under YOUR
commit title.

Caught 2026-08-07 by the integrator on `DECISIONS.md`: the index held only the DISCOVERY-DOC section,
while the worktree had gained a full `## 2026-08-07 — MTLS-PIN` section from a concurrent agent —
text asserting that `client/pin.go` had landed, when that file was untracked and its test was red. It
refused the commit rather than putting a false dated claim in `main`.

Before any pathspec commit, check `git status --porcelain -- <paths>` for an `MM`, and diff the
worktree (`git diff HEAD -- …`), never just the index (`git diff --cached -- …`). This applies
hardest to the shared append-only files — `DECISIONS.md`, `AGENT_LOG.md` — which several agents
append to at once by design, and to the `CONTRACTS-*.md` plane files, which the 2026-08-02 split
(commit `0439836`) made concurrently writable ON PURPOSE. Being allowed to own a plane file for one
pass is not permission to skip the worktree diff.

### 4.3 `MM` catches only ONE direction; a clean ` M` hides the other

Index clean over a contaminated worktree trips no status check, and the pathspec commit still takes
the lot. On 2026-08-14 `client/client.go` sat at ` M` carrying one in-scope doc comment plus
`endpointWith` and `resolvePinsWith` from another agent's live, ungated task.

Status is never sufficient — read `git diff HEAD -- <path>` and confirm every hunk is yours.

### 4.4 Do NOT commit work no agent has reported

A package appearing in the tree and passing its tests is not a signal that it is finished — it may be
mid-review, or mid-edit. Wait for the owning agent's report with gates COMPLETED. Committing on "it
is green and it is there" has shipped ungated code three times (`518e71b`, `2451b4a`, `f56c723`),
each time discovered only when the agent later reported findings against code already in `main`.

### 4.5 A green tree is not a GATED tree

Do not commit an agent's work until it reports its reviewer AND security gates as COMPLETED, not
merely dispatched. Committing mid-review has shipped two real security holes here — a relay SSRF and
an unbounded input — both caught by gates that were still running when the commit landed.

Since 2026-08-22 the security gate alone may legitimately be SKIPPED rather than COMPLETED, under the
docs-and-tests carve-out (§8). That is the ONLY relaxation: reviewer is still always required, and a
skip is only valid when it is recorded in `AGENT_LOG.md` naming the skipped tier and the exact paths.
`CLAUDE.md` and this section still say "reviewer AND security … COMPLETED"; that reading is stricter
than the carve-out, never looser, so following it verbatim is always safe.

---

## 5. Documents that read as freshly checked

A count or a status written in prose can go out of date while the thing it describes moves. Two recorded
cases:

- **The enrolment gate.** `CLAUDE.md` claimed enrolment was "not yet invite-gated" for several hours
  AFTER the gate shipped in `3cedcb7` (2026-08-15). A stale "not yet implemented" note is more
  dangerous than no note, because it reads as freshly checked. See `INVARIANTS.md` invariant 3 for
  the current statement and the forge evidence behind it.
- **The unauthenticated-route count.** Three counts were live at once — the code's, `CLAUDE.md`'s and
  one in an `internal/httpapi` test's failure message — and all three read as freshly checked, which
  is why they lasted. Reconciled 2026-08-21 against `httpapi.UnauthenticatedRoutes()`
  (`401f112`, `2828dcf`, `b95d22d`).

The rule both cases produce: cite the ENUMERATION, not a number derived from it. Trust the
allow-list, never the prose.

`AGENTS.md` is the same hazard at file scale — a second copy of this protocol for a different agent
runtime, drifting against the first. See `CLAUDE.md`'s "Repository layout" entry for its current
status.

---

## 6. Hardening that disables a guard

This is the shape invariant 11 warns about in `client/pin.go`: a change that reads as tightening,
silently removes protection, and leaves every positive test passing. It is not specific to crypto.
Recorded 2026-08-21 from a security re-gate on `scripts/doc-check.sh` (`e2c9cd0`).

### 6.1 `unset -f` closes one vector of three, and hides a vacuous assertion

`scripts/doc-check.sh` calls `wc`, `grep`, `sed` and `awk` as plain commands, so an **exported shell
function** in the caller's environment replaces them. Reproduced — a 100-byte file against a 10-byte
ceiling, honest run then hijacked run:

```
$ DOC_CHECK_BUDGETS=b.tsv DOC_CHECK_PRESERVE=p.tsv bash scripts/doc-check.sh budget
doc-check: FAIL: big.md is 100 B, over its 10 B ceiling by 90 B          exit 1

$ wc() { printf '1\n'; }; export -f wc
$ DOC_CHECK_BUDGETS=b.tsv DOC_CHECK_PRESERVE=p.tsv bash scripts/doc-check.sh budget
doc-check: PASS: budget — 1 file(s) within ceiling, 1 preserved phrase(s) present   exit 0
```

**This is not a licence to turn a red budget green.** It is recorded so the fix is chosen with its
cost known. If `budget` is red, the file is over its ceiling: relocate content per `CLAUDE.md`'s
"Where a new warning goes".

A `PATH` shim and `BASH_ENV` reach the same result by other means. `unset -f` at the top of the
script looks like the fix. Two measured reasons it is not.

**It closes one vector of the three.** With `unset -f` in place the exported-function hijack is dead
and the `PATH` shim still returns `PASS ... exit 0`. Only `command -p` defeats all three, and that
would also defeat the selftest's own stubs, which would then need a bespoke environment indirection
— a test backdoor inside a proof instrument. That trade is the thing worth knowing.

**It breaks the selftest's guards, and the selftest says so.** The stubs at `scripts/doc-check.sh:903`
and `:915` (`export -f wc`) and `:1125`/`:1131` (`export -f mktemp`) are how the selftest proves an
unmeasurable or non-numeric size is a FAIL rather than "within ceiling". Insert
`unset -f wc grep sed awk mktemp` after `set -uo pipefail` — all five stubbed commands — and it goes
red:

```
doc-check: SELFTEST FAIL: an unmeasurable file must be named as such, got:
doc-check: SELFTEST FAIL: 6 of 96 assertions did not hold                exit 1
```

**Name every stubbed command, or the count will not match.** Measured in a clean `git archive HEAD`
extract at `2ed05c2`:

| inserted after `set -uo pipefail` | rc | selftest |
|---|---|---|
| *(nothing — control)* | 0 | `96/96 assertions held` |
| `unset -f wc grep sed awk` | 1 | 5 of 96 |
| `unset -f wc` | 1 | 5 of 96 |
| `unset -f mktemp` | 1 | 4 of 96 |
| **`unset -f wc grep sed awk mktemp`** | 1 | **6 of 96** |

The four-name form gives **5**, not 6: `mktemp` is stubbed too, at `:1125`/`:1131`, and omitting it
from the command while quoting the six-count is exactly the error §6.2 is about. An earlier draft of
this section did precisely that, and an integrator refused the commit over it.

**Only `wc` and `mktemp` are ever stubbed. `grep`, `sed` and `awk` are not.** The stub sites are
`:903` and `:915` for `wc` and `:1125` and `:1131` for `mktemp`; there is no `grep()`, `sed()` or
`awk()` definition anywhere in the file. Three of the five names in the command above are therefore
**inert**, and rows 2 and 3 of the table are not merely equal in count — their output is
**byte-identical** (`md5 95bb42a07d55` for both; `cmp` reports no difference).

That is the mechanism behind every defect this subsection has produced. **Nothing in the output
identifies the command you typed.** What it identifies is which STUBBED commands were removed —
`wc`, `mktemp`, or both — and the messages and the count do that equally well: both partition the
five rows as `{control} / {2,3} / {4} / {5}`. Neither is finer-grained than the other, and neither
separates rows 2 from 3, because by construction nothing can.

The `an unmeasurable file must be named as such` line tracks the `wc` stub: 6, 6 and 8 occurrences
in the three rows that remove it, 0 in the `unset -f mktemp` row. That row's four failures are
mktemp-CAUSED — the control is 96/96 — but not all are mktemp-worded. In full: `a silent empty
mktemp must not produce a passing run`, `an empty mktemp result must abort at mktemp`, `exported env
vars suppressed assertions (84 with them vs 84 for a token-suppressed child)`, and `a run with
re-entry env vars exported must still pass`. Only the first two name mktemp, and the last appears in
**all four** failing rows, so it distinguishes nothing. (That third one is a defect in its own right
— a failure message asserting a difference while quoting two equal counts — filed separately as
`5cf1edd0`; it is not caused by this change.)

This paragraph took **three** rounds of integrator refusal, each for the same error in a different
place: asserting a property across all rows after measuring two of them. Draft 1 paired a four-name
command with the five-name count. Draft 2 said the quoted line appeared in every failing row, which
its own table disproved. Draft 3 said the message set identified the stub set more finely than the
count, which byte-identical rows 2 and 3 disprove. The evidence that would have stopped all three was
already in the table each time. If you are about to write "only X identifies Y" here, check whether
the thing you are distinguishing can differ at all.

**The denominator does not move, and cannot.** `checks=$((checks + 1))` at `:904`, `:909`, `:916`
and `:923` is unconditional and sits outside every stub-dependent branch. An earlier draft of this
section said the count dropped silently to `92/92` and told you to compare the COUNT rather than the
verdict. `92/92` was never measured, and the advice was backwards: the count is the number that
holds still and the verdict is the one that moves. Two review gates caught it, three lines above
§6.2, which says not to restate a figure you cannot re-derive.

**The trap is one assertion below.** Of the two assertions guarding that stub, only one is
load-bearing:

- `:905` checks the EXIT CODE — `expected exit 1`. With the stub gone it still gets exit 1, because
  the fixture (`ten.md`, 10 B against a 5 B ceiling) is over its ceiling anyway. It **passes for the
  wrong reason**.
- `:909-913` checks the MESSAGE for `could not measure`. With the stub gone the message reads
  `over its 5 B ceiling`, and this is the assertion that actually fails.

An assertion that checks only an exit code can be satisfied by an unrelated path. When a fixture is
engineered to fail, assert on WHY it failed; otherwise the assertion proves nothing about the guard
it was written for.

### 6.2 An unsourceable number inside a proof instrument

`scripts/doc-check.sh:343-344` states: *"NOTE the awk call above deliberately does NOT get `--`: awk
stops option parsing at the program text, and adding it there breaks 20 assertions."*

That awk call is at `scripts/doc-check.sh:219` and is the ONLY `awk` invocation in the file, so the
comment can refer to nothing else. Measured three variants of it against the unmutated control:

| variant at `:219` | selftest |
|---|---|
| `DOC_CHECK_WANT="$heading" awk -- '` — `--` added, env kept | **96/96 PASS** (also 84/84, 94/94 at the other re-entry levels) |
| `awk -- '` — `--` added, env assignment dropped | 23 of 96 FAIL |
| `awk '` — env assignment dropped, no `--` | 23 of 96 FAIL |
| control, unmodified | 96/96 PASS |

**Adding `--` breaks nothing.** The last two rows are the control that isolates it: dropping
`DOC_CHECK_WANT` breaks 23 assertions with or without `--`, so `--` contributes zero. The heading
reaches awk through `ENVIRON` (deliberately, so a backslash is not interpreted), and losing that
assignment is what breaks the run.

So the comment's "20", and a security re-gate's independently reported 19/21/23, are both consistent
with having measured a mutation that dropped the env assignment — a different change from the one
the comment describes. The `--` omission may still be correct for other reasons; the stated evidence
for it does not hold.

If you cannot re-derive a number, do not restate it: say what you actually ran. §6.1 above records
this file failing its own rule while documenting it.

### 6.3 A stated guarantee that exceeds the delivered one

`CONTRACTS-AGENT.md:299-300` claims: *"`<file>` is passed to `sed` after `--`, so a file named `-n`,
`-s`, `-z` or `--debug` is read as a FILE."* True for those four. **Not true for a lone `-`:**

```
$ printf 'FROM-STDIN\n' | sed -n -e '1,3p' -- -
FROM-STDIN                    # GNU sed reads STDIN even after --
```

With a file literally named `-` in the directory, `path_is_contained` passes and `[ -f - ]` is TRUE,
so both guards admit it and the argument still does not name that file. The command does end in
`FAIL … 1 of 1 needles absent` (exit 1) — but by accident of composition, not because anything
rejected the argument. The two readers are sequential, not racing: `awk` runs first and drains stdin
to EOF, so the later `sed` reads an exhausted stream and the section body is empty. The outcome is
fail-closed; the contract sentence claims the argument is read as a file, which is a different and
stronger property that does not hold.

When you write "X is always Y" in a contract, test the degenerate argument — `-`, the empty string,
a name that is only whitespace — before the sentence goes in.

---

## 8. A gate rule that exempts the change which weakens it

### 8.1 The docs-and-tests security carve-out qualified for itself

Measured 2026-08-22 against HEAD `231b769`, while the carve-out recorded in `DECISIONS.md`
(2026-08-22, `97a315af`) was staged and NOT yet committed. As first written it skipped security
review for a change touching ONLY docs and tests with no guard file. Its own introduction was seven
files, every one of them `.md`:

```
$ git status --porcelain | sed 's/^...//' | grep -Ev '\.md$|_test\.go$'
            # no output: every path in the change qualified for the carve-out
```

So the change that WEAKENED the security gate passed the gate's own test for "not security-bearing",
and would have been committed with no security verdict. Caught in review before the commit, and
narrowed in the same change that introduced it.

### 8.2 The principle: a `.md` extension does not make a file documentation

**A file that determines WHAT is checked, or that PERFORMS the check, is CONTROL PLANE.** Editing one
can disable a check without touching a line of product code, and that is exactly the change that
needs review. Control plane never qualifies for the carve-out, whatever its extension:

| path | why it is control plane |
|---|---|
| `CLAUDE.md`, `AGENTS.md` | injected into every spawn; they state which gates run at all |
| `INVARIANTS.md` | it states what a reviewer and a security pass measure a change AGAINST |
| `.claude/**` | agent definitions, `ORCHESTRATION.md`, settings — the gates as implemented |
| `scripts/doc-check.sh`, `scripts/proof-check.sh` | they ARE the verification machinery |
| `docs/doc-budgets.tsv`, `docs/doc-preserve.tsv` | the ceilings and phrases `doc-check.sh budget` enforces |
| any script that implements a check or gate | same reason, whatever it is named |

`INVARIANTS.md` was missed by the first version of this list and added 2026-08-22: it matched neither
the `.md` test nor check (c), so a solo edit narrowing an invariant would have been classified
docs-only and committed with no security verdict — while every later review then measures the code
against the narrowed text.

**`PITFALLS.md` is NOT control plane — a stated decision, 2026-08-22, not an omission.** It records
incidents and reasoning; no gate consults it to decide what to check, so a solo edit here is
docs-only. Revisit that if it ever becomes the only place a check's scope is defined (the table above
mirrors `.claude/agents/integrator.md` check (c), which is the mechanical copy).

The list is illustrative and it will go stale. The PRINCIPLE is what you apply to a file nobody
anticipated: "it is not on the list" is not an argument — ask whether editing the file could make a
check stop checking, or stop checking the thing it claims to.

The check scripts are not a hypothetical attack surface. Security review has already found two real
defects in `scripts/doc-check.sh`: an injectable `TMPDIR`, where a hostile value ran an attacker's
command AND the instrument still printed `SELFTEST PASS: 27/27` (`scripts/doc-check.sh:1030-1045`,
regression guards to `:1097`), and a `sed` option-injection, where a file NAMED `-n` was eaten as an
option so `sed` read the needle from STDIN and the verdict line looked perfect for a file that did
not contain it (fixed with `--` at `:345`, reasoning at `:335-344`, fixture at `:644`). Both are
`.sh` files that only ever run inside proofs; both were MEDIUM findings, 2026-08-21.

The mechanical form of the exclusion is `.claude/agents/integrator.md` step 1, check (c). Judge it by
EMPTY OUTPUT, never by exit status — `grep` exits 1 when it matches nothing. Empty output only counts
once check (0) has confirmed the pathspec matches something — see §8.3.

### 8.3 Three ways the carve-out's own checks read as a pass without checking

Measured 2026-08-22 against the staged carve-out at HEAD `231b769`, before it was committed. Each
made all of checks (a), (b) and (c) print nothing, which the integrator reads as "carve-out applies,
security may be SKIPPED".

**Renames were invisible.** `git status --porcelain` prints a rename as ONE line, `R  old -> new`, and
the `sed 's/^...//'` leaves `old -> new`. Check (a) tests only the line END, so any rename whose
TARGET ends `.md` or `_test.go` was suppressed whatever the SOURCE was; check (c) is `^`-anchored, so
it never saw the target at all. All three of these produced three empty checks:

```
R  CLAUDE.md -> DOCS.md
R  docs/policy.md -> .claude/agents/newreviewer.md
R  client/pin.go -> client/pin_test.go
```

The second lands a new agent definition under `.claude/` with no security verdict, reachable from any
docs reorganisation done with `git mv`. Fixed by `--no-renames` on every `git status --porcelain` in
the checks, which prints the rename as a separate `D old` and `A new` so both halves get classified.

**The checks failed OPEN on a pathspec that matched nothing.** `git status --porcelain -- <pathspec
matching nothing>` prints nothing and exits 0 — unlike `git commit`, which errors. A typo, the wrong
case, the wrong cwd, or an unquoted pathspec containing a space (`P=(dir with space/x.md)` splits
into three elements) therefore emptied all three checks. Measured: a staged unreviewed edit to
`client/pin.go`, checked as `client/Pin.go`, gave SECURITY SKIPPED. Fixed by check (0), which
REFUSES when any element of the pathspec matches nothing: **empty output now means two different
things, and only one of them is a pass.**

**"Guard file" was enforced by filename only.** `CLAUDE.md` defines a guard as "an AST guard, any
`*guard*_test.go`, any test whose removal disables an invariant check", but check (b) grepped PATHS
for `guard`. Measured over `git ls-files`: 16 tracked code files carry a `go/ast`/`go/parser` walk
and only 5 have `guard` in the path. Missed: `cmd/agent-bus/tlslisten_test.go` (the
no-plaintext-listener AST guard, invariant 11) and `client/pinrotate_test.go`'s
`TestPinIsNeverLearnedFromAHandshake` (no TOFU, invariant 11) — `client/guard_test.go` was protected
while two siblings enforcing the same invariant were not. Check (d) now greps the touched
`_test.go` files' CONTENT, on both the HEAD and the index side, for
`go/ast|go/parser|InsecureSkipVerify|VerifyPeerCertificate`.

**(b) and (d) together are still a FLOOR.** `internal/httpapi/authmw_test.go`'s
`TestEveryRouteRequiresAuth` pins invariant 3's unauthenticated-route allow-list and matches NEITHER
pattern, so it remains deletable under the carve-out as written. The definition governs; the regexes
are a convenience.

**Enumerate with `git ls-files`, never a filesystem walk.** Nested checkouts under
`.claude/worktrees/` and `.worktrees/` inflate a naive `grep -r`/`find` by a large and VARYING
factor, which makes the coverage gap look closed when it is not. **Record the METHOD, never an
absolute walk count** — the count changes as worktrees are created and removed, so anything written
down here is stale by the next read. Snapshot only, 2026-08-22: the two counts above were recorded as
148 files / 61 guard-named earlier in the day and measured 114 / 45 later the same day, while
`git ls-files` gave 16 / 5 both times. Treat those figures as an illustration of the spread, not as
current; re-measure with `git ls-files` when you need a number. (Same trap as the CLAUDE.md byte
count in `docs/doc-budgets.tsv`: "do not trust that number: run the check".)
