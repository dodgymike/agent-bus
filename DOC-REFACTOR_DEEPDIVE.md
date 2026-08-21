# DOC-REFACTOR — why `CLAUDE.md` grows, why `AGENTS.md` drifts, and what was changed

Spec task `f4bd3c9f-3af8-4438-bcb0-18203b857255` (epic PROCESS, P2), agent `deep-diver`,
2026-08-21. Investigated and refactored against HEAD `85ed77f`, re-verified against HEAD `2ed05c2`
after an integrator commit landed mid-task.

This document records both halves the task asked for: the audit, and the refactor that was actually
made. Every claim below is attributed to a file, a line, a commit or a command's literal output.
Where a claim is a hypothesis it is labelled one.

---

## 1. Symptom

Two failures, one shared cause pattern.

**1.1 `CLAUDE.md` is over its byte ceiling and the check fails.** Literal output in the WORKING TREE
at task start, before any change by this task:

```
$ bash scripts/doc-check.sh budget
doc-check: FAIL: CLAUDE.md is 31023 B, over its 28781 B ceiling by 2242 B
doc-check: FAIL: budget — 1 failure(s) over 3 sized file(s) and 5 preserved phrase(s)
EXIT=1
```

**That 31023 B is the WORKING TREE, not any commit — and getting this wrong invalidated three of my
own findings until a review gate caught it.** `git show 85ed77f:CLAUDE.md | wc -c` is **30063 B**, as
is `b95d22d` and HEAD `2ed05c2`. The 960 B difference is the coordinator's uncommitted `## How to
write` section (§6, "Foreign uncommitted text"). `scripts/doc-check.sh` was itself UNTRACKED at
`85ed77f` — it landed at `e2c9cd0`, during this task — so this output could only ever have come from
the worktree. Against the committed file the same check reports **over by 1282 B**, not 2242 B.

Consequences, all corrected below: `docs/doc-budgets.tsv`'s "30063 B at `85ed77f`" is **CORRECT**,
not stale, and so is `CONTRACTS-AGENT.md`'s copy of it. The task brief's own 31023 B figure is a
worktree measurement. Every byte comparison in this document is therefore stated worktree-to-worktree,
which is like-for-like because the same 960 B of foreign text is present at both ends.

`docs/doc-budgets.tsv` sets that ceiling AT the file's size when the ratchet landed (`0a9a674`), so
the red is the ratchet working, not a misconfiguration. `CLAUDE.md` is injected into every sub-agent
spawn, so its bytes are multiplied by every dispatch.

**1.2 `AGENTS.md` is a second copy of the same protocol, 127 diff lines behind.**

```
$ diff AGENTS.md CLAUDE.md | grep -c '^[<>]'
127
```

Both files' most recent commit was the same sha (`b95d22d`), but `CLAUDE.md` carried two earlier
commits — `401f112`, `2828dcf` — that never propagated. The drift mechanism was live, not historical.

---

## 2. Evidence

### 2.1 Where `CLAUDE.md`'s bytes were

Per-section byte histogram of the WORKTREE file at task start (`awk` over the whole file, not
sampled; totals 31023 B, i.e. the committed 30063 B plus the 960 B foreign section):

| Bytes | Lines | Section |
|---|---|---|
| 8525 | 100 | `## What this project is (the standing design contract)` |
| 6155 | 76 | `## Work in atomic increments` |
| 4116 | 57 | `## Verify — and tell the truth` |
| 2613 | 35 | `## Spec Server — task management` |
| 1833 | 27 | `## Repository layout` |
| 1574 | 25 | `## Go conventions` |
| 1214 | 22 | `## Spec Server task notes are the work JOURNAL` |
| 1130 | 17 | `## Runtime target: Docker Compose` |
| 1127 | 19 | `## Agent roster` |
| 958 | 20 | `## How to write` |
| 767 | 13 | title + preamble |
| 761 | 11 | `## Parallel-agent coordination` |

Within those, the *incident narrative* — the dated story behind a rule, as opposed to the rule —
measured:

| Bytes | What |
|---|---|
| 2530 | the five commit traps under step 9 (dates, shas `518e71b`/`2451b4a`/`f56c723`, the `DECISIONS.md` MTLS-PIN episode, the `client/client.go` ` M` episode) |
| 3050 | the four proof traps in `## Verify` (vacuous shapes, the overlay recipe and fence, the README `localhost:8080` grep episode) |
| 1355 | the two `gofmt` traps (the 127 false pass, the `GOFMT_CLEAN`/`client/messages_test.go` episode) |
| 1332 | the enforcement-status blockquote (forge evidence: 220 refused enrolments, 0 bytes, suffix 1 not 21) |
| **8267** | **total, 27% of the file** |

### 2.2 `AGENTS.md` is not a stale copy — it is a token-substituted fork, and the substitution
corrupted real paths

The 127 diff lines are not all missed updates. Four of them are a systematic `Claude`→`Codex`
substitution, and two of those rename paths that exist on disk:

| `AGENTS.md` said | Reality |
|---|---|
| `**agent-bus** … Codex agents enrol with it` | cosmetic |
| `## Agent roster (`.Codex/agents/`)` | `ls -d .Codex` → *No such file or directory*. `.claude/agents/` exists and holds 14 agent definitions. |
| `Codex-sonnet-5`, `Codex-opus-5` | fabricated model ids. Spec task `e718e0c0` (DOCS-15) is filed on exactly this: *"AGENTS.md writes fabricated model ids into the cost audit trail"*. |
| `(/mnt/sdc/mike/Codex-scratch/spec-cloud-creds.env)` | that directory exists but holds no creds file. |

An agent following `AGENTS.md` to locate credentials reaches a file that is not there.

**The 127 lines are three categories, not two.** Measured by line-set comparison of `HEAD:AGENTS.md`
against `HEAD:CLAUDE.md`: 48 lines existed only in `AGENTS.md`, 3853 B across 11 runs. Four runs are
3 lines or longer:

| Site | Bytes | What | Disposition |
|---|---|---|---|
| `AGENTS.md:52-56` | 527 | the pre-`b95d22d` invariant-3 allow-list wording | superseded; correctly dropped |
| `AGENTS.md:212-219` | 661 | a `## Model selection` section body | relocated, see below |
| `AGENTS.md:365-383` | 1652 | the 14-entry agent roster with one-line descriptions | relocated, see below |
| `AGENTS.md:385-388` | 381 | the `**Review panel (full-system review):**` paragraph | relocated, see below |

Those last three were **not unique content and were not destroyed by the sync.** Commit `5a4f885`
("Trim per-spawn context: move orchestration detail out of `CLAUDE.md`") moved them to
`.claude/ORCHESTRATION.md`, where they still are, with the correct model ids rather than
`AGENTS.md`'s fabricated `Codex-sonnet-5`. That commit touched `CLAUDE.md` and `.claude/agents/*`
and **not `AGENTS.md`**, so `AGENTS.md` never received the relocation. The `cp` propagated a move
that had already happened.

**One open question handed to `6a5ece85`, raised by the reviewer gate and not resolved here:** the
destination is inside `.claude/`. If `AGENTS.md` exists for a runtime that does not read `.claude/`,
that runtime can no longer reach the roster, the model-selection guidance or the review-panel
definition at all. Restoring the text into `AGENTS.md` was NOT done — it would re-fork the file and
undo the sync. Whether the answer is a symlink, a generator, or moving `ORCHESTRATION.md` out of
`.claude/`, it is a mechanism question and `6a5ece85` owns it.

### 2.3 …and `CLAUDE.md`'s copy of that path was wrong too

This was found while checking 2.2, and is a separate defect neither file's history had caught.
`scripts/spec-cloud.sh:20`:

```
CREDS="${SPEC_CLOUD_CREDS:-/mnt/sdc/mike/claude-scratch/spec-cloud-creds-agent-bus.env}"
```

`CLAUDE.md` named `/mnt/sdc/mike/claude-scratch/spec-cloud-creds.env` — **without the `-agent-bus`
suffix**. Both files exist on disk (`spec-cloud-creds.env` 289 B, Jul 23; `spec-cloud-creds-agent-bus.env`
2311 B, Aug 2), so the wrong one is readable and stale rather than absent, which is why the error
survived. Only two doc sites named a creds path; both were wrong, in different ways.

### 2.4 The unauthenticated-route count kept going wrong for a mechanical reason nobody recorded

`CLAUDE.md` invariant 3 read *"except the **six** on the explicit allow-list … enrolment, session
begin/complete, `/healthz`, `/v1/info` and `/v1/discovery`"*. That enumeration presents as **five
bullets** while denoting **six routes**, because `session begin/complete` is two routes written as
one phrase. Any reader recounting the list from the prose gets five.

This is, I believe, the mechanical explanation for a defect the repo has now paid for three times
(`401f112`, `2828dcf`, `b95d22d`; `DOC-TRUTH_DEEPDIVE.md` row 25; open task `9a02d65a`). Labelled a
**hypothesis about the cause** — the miscounts themselves are confirmed and documented, the
"why five" explanation is my inference from the text shape.

### 2.5 The injected copy of `CLAUDE.md` can lag the file on disk

During this session the harness-injected project instructions carried invariant 3's PRE-`401f112`
wording (*"except enrolment, session begin/complete, `/healthz` and `/v1/info`"*) while the file on
disk carried the six-route wording. Recorded because it affects how the budget should be read: the
per-spawn cost is real, and so is the possibility that a sub-agent is reasoning from a snapshot
older than HEAD. **Hypothesis, not confirmed** — this is harness caching behaviour, outside this
repo, and I did not instrument it. Disproof test: spawn two sub-agents either side of a `CLAUDE.md`
edit and have each echo a phrase unique to the new text.

---

## 3. Root causes

### 3.1 CONFIRMED — `CLAUDE.md` had no destination for incident narrative

`INVARIANTS.md` already established the correct pattern and says so at `INVARIANTS.md:6-8`:
*"This file holds the RULE and the REASONING. `CLAUDE.md` holds the one-line rules only."* That
companion covers exactly one plane — the eleven design invariants. **Nothing covered process and
verification traps.** So when an agent learned that `gofmt -l` exits 0 while listing files, the only
place to put it was the file injected into every spawn.

Evidence that this is the growth mechanism rather than a theory about it: the commits that grew the
file each append a full paragraph of incident to `CLAUDE.md` and touch no companion —
`3d9955a` (vacuous skipped subtests), `d71d5f5` (*"status alone never clears a pathspec commit"*),
`aade191` (+678 B, the overrun that produced task `721b51ef`). 27% of the file was narrative with
nowhere else to live (2.1).

**Disproof test:** if a companion had existed, the traps in `CLAUDE.md` would be one-liners with
pointers, as the invariants are. They were 8267 B of prose. Disproved-by-inspection.

### 3.2 CONFIRMED — reasoning relocated into `INVARIANTS.md` was never removed from `CLAUDE.md`

Invariant 3's *"three counts live at once … all three had read as freshly checked"* narrative existed
in `CLAUDE.md:52-58` **and** in `INVARIANTS.md:96-111`, in near-identical wording. The relocation
half of the split was done; the deletion half was not. This is the failure mode the fix in §4 must
avoid repeating: a pointer without a cut saves nothing.

### 3.3 CONFIRMED — the ratchet was unenforced until today

`docs/doc-budgets.tsv`, `docs/doc-preserve.tsv` and `scripts/doc-check.sh` were UNTRACKED at
`85ed77f`. They became tracked at `e2c9cd0` during this task. Nothing ran the budget in any commit
gate, which is why `aade191`'s +678 B landed invisibly. This is a *contributing* cause — it removed
the signal — not the growth mechanism itself. `CONTEXT-BUDGET-WIRE` (`be76c7e2`) owns wiring it into
`scripts/check.sh`; that work is not done and this task did not do it.

### 3.4 CONFIRMED — `AGENTS.md` is maintained by hand re-copy plus a hand substitution

`401f112` and `2828dcf` touched `CLAUDE.md` only. `b95d22d` touched both, but only the single
paragraph its title names. The substitution (2.2) is applied by hand and has no rule about what is
*safe* to substitute, so it renamed a real directory and a real credentials file.

**Ranked candidate, NOT confirmed:** that `AGENTS.md` diverges *deliberately* because a non-Claude
runtime needs different content. Evidence against: every divergence found is either a missed update
or a substitution that made a true statement false; `.claude/agents/` and `claude-scratch` are the
real paths regardless of which runtime reads the file. Evidence that would confirm it: an operator
statement, or a runtime that genuinely cannot use `.claude/`. **I did not find one, and I did not
decide the question** — see §5.

---

## 4. The fix

### 4.1 New companion: `PITFALLS.md` (21539 B, new file)

The smallest correct change is to give the traps the home `INVARIANTS.md` already models. `PITFALLS.md`
holds the RULE and the INCIDENT; `CLAUDE.md` holds the one-line rule and a `§`-numbered pointer.
Sections: 1 formatting checks · 2 proofs that prove nothing · 3 clean-overlay verification ·
4 commits that ship someone else's work · 5 documents that read as freshly checked · 6 hardening
that disables a guard.

**Nothing was deleted.** A relocation audit checked 50 load-bearing phrases plus one negative
control across `CLAUDE.md`, `PITFALLS.md` and `INVARIANTS.md` with whitespace normalisation
(`re.sub(r'\s+',' ')`, matching `doc-check.sh`'s own `norm`). Result: 50/50 present, control
correctly absent, script exit 0. Phrases checked include every one the task brief named, plus
`518e71b`/`2451b4a`/`f56c723`, `client/messages_test.go`, `endpointWith`, `resolvePinsWith`,
`535876c`, `curl -s localhost:8080/healthz`, `TestMain`, `MTLS-PIN`.

The first run of that audit reported 3 MISSING, of which 2 were **line-wrap false negatives in the
audit itself** — `grep -F` against a phrase that the file wraps across two lines. That is the same
failure class as the grep-proof trap in `PITFALLS.md` §2.4, encountered while writing the file that
documents it. The third was the deliberate negative control.

`PITFALLS.md` also gains one trap that was **never in `CLAUDE.md` at all**: an unquoted `-run` regex
is re-parsed by `proof-check.sh`'s inner `bash -c`, giving `verdict=UNVERIFIABLE`. It was recorded
only in `ACK-CONTRACT.md:786` and `AGENT_LOG.md:3904` — an append-only log nobody greps for it. Open
task `0fb4d032` is filed on four `proof_cmd`s broken this way.

### 4.2 `INVARIANTS.md` gains "Enforcement status" (23003 → 27199 B)

`CLAUDE.md`'s 1332 B enforcement blockquote moved here in full, under a new `##` heading, and the
file gained a missing `## The eleven invariants` parent so that `doc-check.sh section` can scope to
it. Verified:

```
$ bash scripts/doc-check.sh section INVARIANTS.md \
    'Enforcement status — what is actually true in code today' \
    'invite-gated' 'client/enrol.go:64' 'RequestClientCert' '0 bytes' 'suffix **1, not 21**'
doc-check: PASS: INVARIANTS.md — 5/5 needles inside "Enforcement status …" (lines 29-78)
```

Before the `##` parent was added the same check reported `(lines 29-319)` — the section ran to end of
file because every invariant below is `###` with no `##` above them. The needles passed either way,
so **the pass was real but the scoping was not**; this is worth knowing for anyone writing
`doc-check.sh section` proofs against a file whose heading levels skip.

*(The quoted range moves whenever the section is edited — it was `29-56`, then `29-68`, then `29-78`
within this one task. Re-run the command rather than trusting the number here; that is the same rule
§2.4 states for counts in prose, applied to this document.)*

**A defect I introduced and then caught.** The first draft of that section said *"A certificate that
IS presented therefore authenticates nobody on its own"* — carried over verbatim from `CLAUDE.md`.
`DOC-TRUTH_DEEPDIVE.md` row 26 had already found that claim FALSE, and the code says so directly at
`internal/httpapi/crosscheck.go:69-72`: *"On that surface the certificate alone authorises and there
is no PAIR to cross-check — recorded as a NAMED NARROWING of invariant 11 … not as compliance with
it."* `internal/httpapi/peerprincipal.go:9-24` says the same. Corrected before any gate ran; the
entry now splits the agent plane from the peer plane and cites both files.

Moving text re-asserts it. Two further entries were added while the section was open, both verified
against code rather than copied:

- certificate rotation is NOT implemented server-side — `cmd/agent-bus/tlslisten.go:134` serves
  `Certificates: []tls.Certificate{cert}`, one certificate, no `GetCertificate`;
  `internal/buscert/buscert.go:65-70` — *"there is no rotation machinery yet … this expiry is a
  SCHEDULED OUTAGE"*. (`DOC-TRUTH_DEEPDIVE.md` row 27.)

### 4.3 `CLAUDE.md`: 31023 → 28213 B (−2810 B, −9.1%)

Worktree-to-worktree. The committed file goes 30063 B → 27253 B if the coordinator's 960 B section
is committed separately, or → 28213 B if it rides along; **both are under the 28781 B ceiling**, so
the integrator's choice does not change the verdict.

**The rows below are per-edit ESTIMATES, not measurements, and they do not reconcile to the net.**
They sum to −3224 B against a measured −2810 B: a 414 B gap, arising from estimating each edit's
size before writing it rather than measuring after, compounded across fourteen rows. Only the
headline is measured (`wc -c`, before and after). The rows are kept because the SHAPE of the
reduction is the useful part — which sections gave up bytes, and to where — but do not add them up
and do not cite a row as a measurement. Stated rather than quietly reconciled, because a table whose
column silently fails to sum is the same defect as §6.2's unre-derivable count, and this document
should not commit it while describing it.

| Edit | Effect |
|---|---|
| enforcement blockquote → `INVARIANTS.md` | −840 B |
| invariant 3: drop the prose count and the narrative already in `INVARIANTS.md` | −135 B, and removes the miscount surface of 2.4 |
| `gofmt` bullet → rule + `PITFALLS.md` §1 | −745 B |
| `## Verify` → rules + `PITFALLS.md` §2, §3 | −1050 B, and ADDS two trap one-liners that were not there |
| step 9 commit bullets → rules + `PITFALLS.md` §4 | −1060 B |
| step 9 plane-file enumeration → `CONTRACTS.md` is the index | −350 B |
| `gen-spec-mirror` paragraph, tightened | −450 B |
| repository layout gains `PITFALLS.md` and `AGENTS.md` rows | +350 B |
| `## How to write` gains the growth rule (§4.5) | +330 B |
| creds path now cites `scripts/spec-cloud.sh:20` instead of copying it | +70 B |
| `CONTRACTS.md` removed from the single-writer list (see below) | +250 B |
| a one-line rule pointing at `PITFALLS.md` §6 (added after the security re-gate, §4.7) | +301 B |
| reviewer + security gate fixes: sha `0439836`, the plane-file/pathspec reconciliation, dropped numerals | +105 B |

**All eleven `##` headings are unchanged**, which was a hard constraint discovered during the audit:
`README.md:34` and `README.md:137` are anchor links (`#what-this-project-is-the-standing-design-contract`,
`#repository-layout`) and `README.md` holds no local copy of either section, so renaming one leaves
the front door pointing at nothing. Both anchors verified to resolve after the change, as did the
quoted-name references from `CONTRACTS-ONDISK.md:28` ("Parallel-agent coordination"),
`CONTRACTS-HTTP.md:1194` ("Runtime target") and `CONTRACTS.md:5` ("`CLAUDE.md` step 9").

One correctness fix landed in the same pass, taken from open task `f0ef1ed9`: the
"Parallel-agent coordination" bullet listed `CONTRACTS.md` alongside `DECISIONS.md` and
`AGENT_LOG.md` as single-writer-contended. Post-split (commit `0439836`; `360a2679` is the Spec task public_id, not a sha — see §6 landmine 8) — the split
exists so the plane files can be written concurrently. Rewritten to name the append-only files as
the contended ones and to say each plane file is single-owner **for one pass**, which is the ruling
open task `cbfb7d88` reached independently.

### 4.4 `AGENTS.md`: content sync only, mechanism deferred

`AGENTS.md` is now byte-identical to `CLAUDE.md` (28213 B each), `SYNC_OK`.

**I did NOT decide the sync mechanism, deliberately.** Task `f4bd3c9f`'s own description instructs:
*"This task must NOT independently decide the sync mechanism if `6a5ece85` is still open and
unowned."* `6a5ece85` is `status: todo, owner: null` as of this writing. So this task did the
CONTENT half and hands `6a5ece85` a stable shape to sync against, plus three findings it did not
have:

1. The divergence is not only staleness — it is a hand substitution that renamed real paths (2.2).
   That is an argument *against* any mechanism that preserves a substitution table, or for one whose
   table is explicitly forbidden from touching filesystem paths, model ids and directory names.
2. **`6a5ece85`'s stored `proof_cmd` is now insufficient.** It asserts
   `diff AGENTS.md CLAUDE.md | grep -c '^[<>]' | grep -qx 0`, which passes RIGHT NOW because of this
   task's `cp` — it measures the symptom, not the mechanism. Closing `6a5ece85` on it today would
   close it having built nothing. Its proof needs to assert the MECHANISM (that `AGENTS.md` is a
   symlink, or that a generator/guard exists and runs), not the current diff.
3. An identical copy carries no marker saying "do not edit me directly", so the next agent to edit
   `AGENTS.md` restarts the drift. A symlink removes that risk structurally; a copy does not.

### 4.5 The structural fix for the growth mechanism

`CLAUDE.md`'s `## How to write` section gained:

> **Where a new warning goes.** This file is injected into EVERY sub-agent spawn … A newly learned
> trap gets its ONE-LINE rule here and its incident — date, sha, exact output — in `PITFALLS.md`;
> a design rule gets its one-liner here and its reasoning in `INVARIANTS.md`. Never delete a warning
> to make room: relocate it and leave the pointer.

Without this, the refactor buys a fixed number of bytes and the mechanism resumes. With it, the
568 B of remaining headroom is enough — the ratchet going red on the next append is the *intended*
behaviour, because the correct response is now defined and cheap.

### 4.6 Verification

Task `f4bd3c9f`'s stored `proof_cmd`, run through the OVERLAY's `proof-check.sh` in a clean
`git archive HEAD` overlay at `2ed05c2` with only the owned files copied in, by relative path
(re-run after the §4.7 addendum):

```
proof-check: proof: bash scripts/doc-check.sh budget
proof-check: class: wrapper
proof-check: running (cwd /tmp/docrefactor.QnDQNo)...
doc-check: PASS: budget — 3 file(s) within ceiling, 5 preserved phrase(s) present
proof-check: PASS — proof command exited 0.
proof-check: verdict=PASS class=wrapper exit=0 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0
```

Baseline for the same command was `verdict=FAIL class=wrapper exit=1`. The preserved-phrase count is
**5, unchanged** — the task's explicit condition that no trap was deleted to make room. All five
rows in `docs/doc-preserve.tsv` point at `CLAUDE.md` and all five phrases are one-line RULES, so the
relocation was designed around keeping them in place; `docs/doc-preserve.tsv` was NOT edited.

`6a5ece85`'s proof in the same overlay: `SYNC_OK`, `verdict=PASS class=file-assertion exit=0`.

**What this PASS does NOT cover, stated so the verdict is not over-read.** `budget` reports "3 file(s)
within ceiling" — `CLAUDE.md`, `SPEC.md`, `README.md`. **`PITFALLS.md` is not one of them.** This
refactor moved 8267 B of incident prose out of the file the ratchet watches into a new file it does
not, so on the numbers alone the growth is RELOCATED, not BOUNDED. `CLAUDE.md` is genuinely smaller
and that saving is real and recurring, because it is the file injected per spawn; but a reader who
takes `budget` PASS as "documentation growth is now under control" would be wrong, and the file that
is now the designated destination for every future trap (§4.5) is the one nothing measures.

It is left unfixed on purpose. Choosing the number is policy, not mechanics — the existing rows are
not set by one convention (`CLAUDE.md`'s ceiling equals its size at `0a9a674`, a ratchet, while
`SPEC.md` and `README.md` were given a round 8192 B, which is headroom) — and `DOCS-4-FU-BUDGET`
(`721b51ef`) owns that decision. Editing `docs/doc-budgets.tsv` here would also mean editing the
instrument that judges this change, which is the trap in `PITFALLS.md` §3. Filed as `3d1b47d9-1395-4f61-a848-e1c06ced2ff8`;
see §7 task 7.

### 4.7 Three traps added after a concurrent security re-gate

A security re-gate on `scripts/doc-check.sh` (committed `e2c9cd0`) reported three findings of one
shape mid-task. They are recorded in `PITFALLS.md` §6. **Each was re-measured rather than restated,
and my first write-up of two of them was wrong** — both review gates caught it:

- **§6.1 `unset -f` closes one vector of three, and hides a vacuous assertion.** The weakness
  reproduces: an exported `wc()` turns `FAIL: big.md is 100 B, over its 10 B ceiling by 90 B`
  (exit 1) into `PASS: budget — 1 file(s) within ceiling` (exit 0). My first draft then claimed
  `unset -f` would break the selftest's stubs *silently*, with the count dropping to `92/92`. **That
  was fabricated.** Measured: `unset -f wc grep sed awk mktemp` makes the selftest exit 1 and print
  `6 of 96 assertions did not hold` (the four-name form without `mktemp` gives 5 — pairing that
  command with the six-count was a second unre-derivable number, caught by the integrator, which
  refused the commit over it); the denominator cannot move, because `checks=$((checks + 1))`
  at `:904`, `:909`, `:916`, `:923` is unconditional. The real finding is one level down and is the
  useful one: of the two assertions guarding that stub, `:905` checks only the EXIT CODE and still
  passes with the stub gone — the fixture is over its ceiling anyway — while `:909-913`, which
  checks the MESSAGE for `could not measure`, is the load-bearing one. An assertion on an exit code
  alone can be satisfied by an unrelated path.
- **§6.2 an unsourceable number in a proof instrument.** `scripts/doc-check.sh:343-344` says adding
  `--` to the awk call "breaks 20 assertions". **Adding `--` breaks nothing** — 96/96, 84/84, 94/94,
  identical to the control. My first write-up stated the edit ambiguously, as replacing
  `DOC_CHECK_WANT="$heading" awk '` with `awk -- '`, which also drops the env assignment; a reviewer
  measured that literal reading and got the re-gate's 19/21/23. The isolating control settles it:
  dropping `DOC_CHECK_WANT` alone, with no `--`, breaks the same 23 assertions, so `--` contributes
  zero and the heading simply has to reach awk through `ENVIRON`. The comment's "20" and the
  re-gate's 19/21/23 are both consistent with having measured a mutation that dropped the env
  assignment. Routed, not edited — I did not touch that file.
- **§6.3 a guarantee that exceeds what is delivered.** `CONTRACTS-AGENT.md:299-300` claims `<file>`
  after `--` is read as a FILE. `printf 'FROM-STDIN\n' | sed -n -e '1,3p' -- -` prints `FROM-STDIN`,
  so a lone `-` still reads stdin. With a file named `-` present, `path_is_contained` and `[ -f - ]`
  both admit it. The result is fail-closed (`FAIL … exit 1`), but by composition: `awk` runs first
  and drains stdin to EOF, so `sed` reads an exhausted stream. Sequential, not a race. The security
  gate probed `/dev/null`, pipes, redirects and a 5 MB seekable stdin and found **no fail-open path**.
  `CONTRACTS-AGENT.md` is out of this task's scope; routed.

---

## 5. Decisions this task made, and decisions it refused to make

**MADE — `PITFALLS.md` is a third companion tier.** `CLAUDE.md` = rules, injected per spawn.
`INVARIANTS.md` = design reasoning. `PITFALLS.md` = process and verification incidents. Each rule in
`CLAUDE.md` points at exactly one companion section.

**MADE — the append-only convention on `DECISIONS.md` (501669 B) and `AGENT_LOG.md` (382393 B)
still serves `DECISIONS.md`, and no longer serves `AGENT_LOG.md`.** Reasoning, stated because the
task asked for it explicitly rather than a silent reorganisation, and NOTHING was reorganised:

- `DECISIONS.md` — **keep append-only, unchanged.** It is consulted by grep and range-read, never
  injected, so its bytes are paid once by the one agent that needs the range. Its value is precisely
  that a decision and its date cannot be rewritten. Open task `ec7fc25e` (CONTEXT-STALE-INPLACE)
  identifies the one real cost — a superseded claim stays readable in place — and correctly frames it
  as a conflict between two things the repo values, not a slip. That is a wording/tombstone problem,
  not an argument for changing the shape. `docs/doc-budgets.tsv` already exempts it by design and
  says why: *"capping them would delete the reasons this project has already been caught removing
  three times."*
- `AGENT_LOG.md` — **the convention has already been ruled on and this task does not re-open it.**
  Open task `116179c8` (CONTEXT-LOG-RETIRE) records `43 entries averaging 5,963 B, all written in
  6 days, with no committed tooling that reads it`, and records a **user decision already given
  (2026-08-08): APPROVED as "freeze + one-line entries"**. `f39083ae` (CONTEXT-LOG-GUARD) adds the
  mechanical check. Both are `todo`. The correct action here is to point at them, not to file a
  fourth opinion. I agree with the ruling and have nothing to add to it.
- **Neither file was touched by this refactor** beyond appending one normal dated entry each, which
  is what the convention prescribes.

**REFUSED — the `AGENTS.md` sync mechanism.** Owned by `6a5ece85` (§4.4).

**REFUSED — the ceiling number.** Owned by `DOCS-4-FU-BUDGET` (`721b51ef`). This refactor brings
`CLAUDE.md` under the **current** 28781 B ceiling rather than arguing for a higher one, which is what
the brief asked for. **The ceiling is not wrong and should not be raised.** It should arguably be
re-ratcheted DOWN to the new size once this lands — that is `721b51ef`'s call, not mine, and §7
hands it the numbers.

**REFUSED — invariant-block trimming beyond the two edits above.** The eleven one-liners still run
~7900 B and are the largest remaining reduction in the file. Three of their phrases are NOT yet in
`INVARIANTS.md` and would have to move first: `fails silently` (invariant 9), `Rotation serves TWO`
(invariant 11), `traversed bus path` (invariant 10). Cutting the standing design contract to save
bytes without moving those first is the exact trade `docs/doc-preserve.tsv` exists to prevent.
Filed as a task in §7 rather than done here.

---

## 6. Landmines and residual risk

**Created by this change — must be fixed by whoever owns the file:**

1. **`CONTRACTS-AGENT.md:322-334` goes stale in ONE respect only — its number is right.** It states
   *"**`CLAUDE.md` is OVER its ceiling** — 30063 B against 28781 B **at `85ed77f`**"*. **The 30063 B
   figure is CORRECT for the commit it pins** (`git show 85ed77f:CLAUDE.md | wc -c` = 30063). An
   earlier draft of this document called the figure false; that was my own worktree-versus-commit
   error, described in §1.1, and the reviewer gate caught it. What this change does make false is
   only the STATUS — `CLAUDE.md` is no longer over its ceiling. Pinning the figure to a sha is what
   kept that sentence honest, and is the practice to copy. The file was staged by an integrator when
   this task began and was committed at `2ed05c2`; I did not edit it.
2. **`docs/doc-budgets.tsv`'s header comment needs the same one-word update, and is otherwise
   accurate.** It says *"`CLAUDE.md` was 30063 B at `85ed77f`, i.e. OVER by 1282 B"* — **both numbers
   check out**, and it already tells the reader not to trust them (*"do not trust that number: run
   `doc-check.sh budget`"*). I did not edit it: it is an input to my own proof, and editing the
   instrument that decides PASS/FAIL as part of the change it judges is the trap in `PITFALLS.md` §3.
   Hand to `721b51ef`, which may re-ratchet the ceiling in the same edit.
3. **`DOC-TRUTH_DEEPDIVE.md`'s line citations into `CLAUDE.md`/`AGENTS.md` are all shifted.**
   Affected: row 25 (`CLAUDE.md:51-52`, `AGENTS.md:51-52`), row 26 (`:20-22`), row 27 (`:99-100`),
   row 35 (*"`CLAUDE.md:31` flags this exact line as a known stale twin"* — that flag now lives in
   `INVARIANTS.md`'s Enforcement status, not `CLAUDE.md`), and the row-1 quote citing `CLAUDE.md:27-29`.
   Rows 26 and 27 are also now *partly discharged*: their claims are recorded, with code citations,
   in `INVARIANTS.md`'s Enforcement status.

**Pre-existing, found and not fixed:**

4. **An operator-ownership conflict.** Open task `9a02d65a` states: *"`INVARIANTS.md` and `CLAUDE.md`
   are OPERATOR-OWNED — the operator writes the wording himself; no agent edits either file for this
   task."* That constraint is scoped to `9a02d65a`, and task `f4bd3c9f` explicitly assigns
   `CLAUDE.md` to this refactor, so the two are not formally in conflict — but **I edited invariant
   3's wording**, which is the exact passage `9a02d65a` reserves. The change is strictly
   conservative: the enumeration is unchanged, only the redundant numeral "six" was removed, which is
   the outcome both tasks want (2.4). **Flagged for operator ratification rather than assumed.**
5. **`CLAUDE.md`'s `crypto/ecdh` toolchain rationale** in "Runtime target" is flagged FALSE by open
   task `86741a89` (DOCS-14, P3), whose proof is `! grep -n 'crypto/ecdh' CLAUDE.md AGENTS.md`. I did
   not act on it: I have the task's summary but not its evidence, and deleting a rationale on a
   one-line summary is how a correct paragraph gets removed. `AGENTS.md` now carries the same text as
   `CLAUDE.md`, so DOCS-14's two sites are now one edit instead of two.
6. **`PITFALLS.md` has no budget row and no `doc-preserve` rows** — see landmine 9, which states this
   fully. *(This entry originally added "it is not injected per spawn, so it needs no ceiling today".
   That reasoning is RETRACTED: being read on demand rather than per spawn bounds the READ cost, not
   the growth, and `PITFALLS.md` is the one file guaranteed to grow because `CLAUDE.md` now sends
   every future trap to it. The integrator refused the commit partly on this point. Left here as a
   tombstone rather than deleted, so the two entries cannot be read as disagreeing.)*
7. **Deep-dive location.** Open task `cea3880c` (CONTEXT-DEEPDIVE-CONVENTION) wants to *"stop the next
   75 KB deep-dive from landing at the repo root"*. This document is at the repo root because
   `f4bd3c9f` mandates `<TOPIC>_DEEPDIVE.md` there. It is deliberately smaller than its predecessors
   (`DOC-TRUTH_DEEPDIVE.md` 84 KB, `CRYPTO_DEEPDIVE.md` 76 KB). Move it when `cea3880c` lands.

**Found by the reviewer and security gates, after the audit above was written:**

8. **A Spec task public_id cited as a commit sha, by me.** `CLAUDE.md`'s "Parallel-agent
   coordination" bullet cited the CONTRACTS split as `360a2679`. `git cat-file -t 360a2679` fails —
   it is the task public_id (`CONTRACTS.md:8`) and the commit is `0439836`. Corrected in this change.
   Recorded because it is an independent instance of open task `a695f85f`'s defect class,
   *"PROTOCOL.md §8 cites Spec Server task id … as if it were a commit sha"*, which suggests the
   pattern is systemic rather than a one-off; `a695f85f` may want to widen its sweep.

9. **`PITFALLS.md` and `AGENTS.md` are in neither `docs/doc-budgets.tsv` nor
   `docs/doc-preserve.tsv` — so this refactor relocated the growth rather than bounding it.**
   Two gaps, one file. On the PRESERVE side, raised by the security gate: the reasoning those rows
   protect has moved into `PITFALLS.md`, which nothing guards, while the rows still pass because the
   one-line headline stayed in `CLAUDE.md`. On the BUDGET side, raised by the integrator: `budget`
   reports "3 file(s) within ceiling" and `PITFALLS.md` is not among them, so the 8267 B of prose
   this change moved is now unmeasured — and it is the designated destination for every future trap.
   **Deliberately not fixed here, for two independent reasons:** `f4bd3c9f`'s definition of done
   requires the preserved-phrase count to stay at **5**, so adding preserve rows would break the
   condition this change is measured by; and adding a budget row means editing the instrument that
   judges this change (`PITFALLS.md` §3). The ceiling is also a real decision rather than a
   mechanical one — the existing rows follow two different conventions (ratchet-at-size for
   `CLAUDE.md`, round 8192 B headroom for `SPEC.md` and `README.md`) — and `721b51ef`
   (DOCS-4-FU-BUDGET) owns it. §7 task 3 carries the preserve half;
   `3d1b47d9-1395-4f61-a848-e1c06ced2ff8` (§7 task 7) carries the budget half.

10. **A stale, readable credentials file, outside this change.**
    `/mnt/sdc/mike/claude-scratch/spec-cloud-creds.env` (289 B, 2026-07-23) is not the file
    `scripts/spec-cloud.sh` uses; its default is `spec-cloud-creds-agent-bus.env`. The security gate
    recommends deleting it and rotating those credentials. **Not actioned — this agent does not touch
    credential material.** Operator decision.

**Files left alone because another agent held them.** Checked with `git status --porcelain` and
`git diff HEAD -- <path>` before and after:

| File | Held by | What was left undone |
|---|---|---|
| `CONTRACTS-AGENT.md` | integrator, staged at task start; committed at `2ed05c2` | landmine 1 |
| `docs/doc-budgets.tsv`, `docs/doc-preserve.tsv` | same | landmine 2, landmine 6 |
| `AGENT_PROTOCOL.md`, `PROTOCOL.md`, `CONTRACTS-HTTP.md`, `ACK-CONTRACT.md` | worktrees `agent-a5af74373fb0b1fc3` (ACK-5) and `agent-a3b41d07f84017fc1` | the duplication clusters in §7 tasks 4 and 5 |
| `CONTRACTS-ONDISK.md` | worktree `agent-a3b41d07f84017fc1` | §7 task 5 |

**Foreign uncommitted text riding in this change — the integrator must be told.** `CLAUDE.md`'s
`## How to write (agent output, commit messages, docs, notes)` section (1410 B) appears in **no
commit**: `git log -S'How to write (agent output' --all` returns nothing. It was already in the
worktree when this task started, it belongs to the coordinator, and the task brief directed that it
be preserved and applied rather than removed. It is not this task's work. Two consequences:
`cp CLAUDE.md AGENTS.md` has duplicated it into `AGENTS.md`, and `CLAUDE.md` sat at ` M` throughout,
which is the direction `PITFALLS.md` §4.3 says a status check will not catch. **Any commit of
`CLAUDE.md` or `AGENTS.md` carries those 1410 B under this task's title.** Disclosed rather than
resolved, because removing it was explicitly forbidden by the brief.

`README.md`, `CONTRACTS.md` and `CONTRACTS-CLI.md` were uncontended throughout. They were audited
(§7) and deliberately not edited: this task's scope is `CLAUDE.md` and the sync, and the README work
is already owned by `76879ad1` (DOCS-6) and `cb4fd330` (MTLS-VERIFY-FU-DOCSCHEME, P0).

---

## 7. Which open tasks this closes or eases

Verified against a full paginated read of the DOCS (43 open), CONTEXT (25 open) and PROCESS
(13 open) epics — `x-total-count` 49/30/16 with `x-has-more: false` at `limit=200`, so complete, not
sampled.

**Closed outright by this change (spec-keeper to verify and flip — this task does not flip them):**

| Task | Why |
|---|---|
| `e718e0c0` DOCS-15 (P3) — *"AGENTS.md writes fabricated model ids (`Codex-opus-5`) into the cost audit trail … AGENTS.md:212,215,359, and the 5 missing safety rails"* | `grep -n 'Codex' AGENTS.md` now returns nothing (exit 1). The five missing safety rails were the `CLAUDE.md` paragraphs `AGENTS.md` lacked; all present after the sync. |

**Proof now passes, but should NOT be closed on it:**

| Task | Why not |
|---|---|
| `6a5ece85` — AGENTS.md sync | `SYNC_OK` passes today because of this task's `cp`. Its scope is the MECHANISM, which is not built. See §4.4; its `proof_cmd` needs strengthening first. |

**Made substantially easier:**

- `f0ef1ed9` — three stale post-split pointers. The `CLAUDE.md:332` third is **done** (§4.3);
  `README.md:88` and `AGENT_PROTOCOL.md:122` remain, the latter on a contended file.
- `202ad8d7` CONTEXT-READRULE (P1) — wants a ~900 B "Reading the documents in this repo" section in
  `CLAUDE.md`. It now **fits under the ceiling**; before this refactor it could not land at all.
- `86741a89` DOCS-14 — its two sites (`CLAUDE.md`, `AGENTS.md`) are now identical text, so it is one
  edit plus a re-sync rather than two independent edits that can disagree.
- `67b42913` CONTEXT-STALE-NOTYET — its cited site (*"`CLAUDE.md`/`AGENTS.md` line 20"*) is
  restructured; the enforcement claims now sit in one place with code citations, which is what a
  `forbid` mode would need to target.
- `9a02d65a` — invariant 3's stale enumeration. The `CLAUDE.md` half is corrected and the prose count
  removed; `INVARIANTS.md:74` and `AGENT_PROTOCOL.md` remain. See landmine 4.
- `0fb4d032` — four `UNVERIFIABLE` `proof_cmd`s. The trap is now documented where an agent will meet
  it (`PITFALLS.md` §2.3) rather than only in `ACK-CONTRACT.md` and `AGENT_LOG.md`.
- `721b51ef` DOCS-4-FU-BUDGET and `be76c7e2` CONTEXT-BUDGET-WIRE — both now have a green baseline to
  wire against instead of a red one.
- `463afaf6` CONTEXT-PLANE-TOC — `INVARIANTS.md` now has the `## The eleven invariants` parent its
  `###` headings were missing, which a TOC generator needs.
- `cbfb7d88` — its "one owner per plane file per pass" ruling is now stated in `CLAUDE.md`.

**No new overlapping DOCS tasks were filed.** The four tasks proposed below are new findings.

### SPEC-ready task breakdown

Atomic, for the orchestrator/spec-keeper to POST to `/projects/agent-bus/tasks`. Per `f4bd3c9f`'s
instruction and the `dd2cdc20` ruling, **descriptive titles only — no numbered keys reserved.**

1. **"`CONTRACTS-AGENT.md` and `docs/doc-budgets.tsv` both state `CLAUDE.md` is over its ceiling; it
   is not"** — epic CONTEXT, P2. Two pinned figures went stale the moment the doc refactor landed:
   `CONTRACTS-AGENT.md:322-334` (*"30063 B against 28781 B at `85ed77f`"*) and
   `docs/doc-budgets.tsv`'s header comment. Both were held by another agent during the refactor, so
   neither could be corrected in the same change. Coordinate with `721b51ef`, which may re-ratchet the
   number in the same edit. Proof must be observed RED first and must pin the specific line, not
   `grep` for a number that appears elsewhere.

2. **"Re-ratchet `CLAUDE.md`'s ceiling to its post-refactor size, or record why not"** — epic CONTEXT,
   P2, **assign to / fold into `721b51ef` (DOCS-4-FU-BUDGET) rather than filing separately if that
   task is still open.** `CLAUDE.md` is 28213 B against a 28781 B ceiling: 568 B of headroom, which
   is less than one appended paragraph. The ratchet's stated design is to sit AT the file's size. Input
   this task provides: the reduction was 2810 B, none of it by deletion, and `PITFALLS.md` is now the
   defined destination for the next warning.

3. **"`PITFALLS.md` needs `doc-preserve` rows and a growth policy"** — epic CONTEXT, P2, **relates to
   `be76c7e2` (CONTEXT-BUDGET-WIRE)**. Phrases relocated out of `CLAUDE.md` are no longer covered by
   the preserve check. `be76c7e2`'s own definition of done already names phrases that should be
   preserved and are now in `PITFALLS.md` rather than `CLAUDE.md` — *"gofmt -l . && echo CLEAN"*,
   *"takes the WORKTREE, not the index"*, *"no tests to run"*, *"InsecureSkipVerify"*. Add rows
   pointing at their new home.

   **The "does it need a ceiling?" half of this has been split out** as
   `3d1b47d9-1395-4f61-a848-e1c06ced2ff8`, task 7 below. An earlier draft of this entry reasoned "it is not injected per spawn, so
   probably not". That reasoning is incomplete: `PITFALLS.md` is now the designated destination for
   every future trap (§4.5), so it is the one file guaranteed to grow, and `budget` currently cannot
   see it. Whether the answer is a ratchet, headroom, or a recorded exemption in the style
   `docs/doc-budgets.tsv` already uses for `DECISIONS.md`, it is a decision someone must make rather
   than assume.

4. **"`CONTRACTS.md` is 91% plane content, and two of its standing directives now forbid the fix"** —
   epic DOCS, P2, **relates to `881dae01` (CONTEXT-CONTRACTS-PARKING) and `5b3f4886` (DOCS-18); check
   whether those two already cover it before filing.** Measured this task: 32506 B, of which the index
   is 2903 B (8%). `CONTRACTS.md:28-30` and `:31-33` warn about `CONTRACTS-CLI.md`'s `-listen`
   default and `CONTRACTS-ONDISK.md`'s `RepairTail` prose — **both were verified FIXED**, so the
   warnings guard passages that no longer exist, and `:35-36` says *"Do not 'fix while you're in
   there' on either passage"*. `CONTRACTS.md:115-117` forbids adding `/v1/peer/relay` and
   `/v1/peer/roster` to `CONTRACTS-HTTP.md` — contradicted by `internal/httpapi/peermount.go:325-328`
   and by `CONTRACTS-HTTP.md:1062-1063`, which already document them. `CONTRACTS.md` is uncontended.

5. **"One file owns the exit-code table; `AGENT_PROTOCOL.md` and `CONTRACTS-CLI.md` have already
   diverged on three rows"** — epic DOCS, P1. `AGENT_PROTOCOL.md:1390-1412` and
   `CONTRACTS-CLI.md:1692-1712` are both 10-row exit-code tables, codes 0-9, with the same preamble.
   Rows 2, 5 and 6 differ: `AGENT_PROTOCOL.md` has pinning and `watch`-mismatch clauses
   `CONTRACTS-CLI.md` lacks; `CONTRACTS-CLI.md` has a capacity-refusal clause `AGENT_PROTOCOL.md`
   lacks. The file `CONTRACTS.md:5` nominates as authoritative is the LESS complete of the two.
   Merge, then leave a pointer. **`AGENT_PROTOCOL.md` is held by worktree `agent-a5af74373fb0b1fc3`
   (ACK-5) — this task must wait for that re-gate.** Related but distinct from `1e9cec15`
   (CONTEXT-PROTOCOL-WALFLOOR-DEDUP), which covers the WAL-floor pair.

6. **"Move the remaining invariant-block reasoning out of `CLAUDE.md`, three phrases first"** — epic
   CONTEXT, P3. The eleven one-liners are ~7900 B, the largest remaining reduction. Three phrases must
   move to `INVARIANTS.md` before any trim: `fails silently` (invariant 9), `Rotation serves TWO`
   (invariant 11), `traversed bus path` (invariant 10) — none currently appear there. **Requires
   operator sign-off per `9a02d65a`'s operator-ownership note.** Low priority: the ceiling is
   satisfied without it.

7. **`PITFALLS.md` has no row in `doc-budgets.tsv`, so the prose relocated out of `CLAUDE.md` is
   unmeasured** — epic CONTEXT, P2, **FILED: `3d1b47d9-1395-4f61-a848-e1c06ced2ff8`**, `follow_up`
   from this task, `relates` to `721b51ef`. Raised by the integrator against this change. `budget`
   reports "3 file(s) within ceiling" and `PITFALLS.md` is not among them, so the 8267 B this
   refactor moved is now unmeasured — in the one file `CLAUDE.md` designates as the destination for
   every future trap. Choosing the number is a real decision, not a mechanical one: the existing rows
   follow two conventions (ratchet-at-size for `CLAUDE.md` at `0a9a674`, round 8192 B headroom for
   `SPEC.md` and `README.md`). Stored `proof_cmd`
   `bash -c 'grep -Pq "^PITFALLS\.md\t[0-9]+$" docs/doc-budgets.tsv && bash scripts/doc-check.sh budget'`
   was demonstrated in BOTH directions before filing — `verdict=FAIL class=other exit=1` against the
   live repo, and `verdict=PASS class=other exit=0` in an isolated overlay with a row added, where
   `budget` then reported "4 file(s) within ceiling, 5 preserved phrase(s) present". That two-way
   demonstration is what `PITFALLS.md` §2.4 asks for and what most proofs in this repo lack.

---

## 8. Cost, risk, rollback

**Cost.** **Seven paths, no code**: four documentation files changed or added (`CLAUDE.md`,
`AGENTS.md`, `INVARIANTS.md`, `PITFALLS.md`), this deep-dive, and one appended section each in
`DECISIONS.md` and `AGENT_LOG.md`. `CLAUDE.md` −2810 B per spawn, which is the recurring
saving; everything else is paid once per read. Net repo bytes +21637 B (`PITFALLS.md` +21539,
`INVARIANTS.md` +4196, `CLAUDE.md` −2810, `AGENTS.md` −1288). The trade is deliberate: bytes moved
from the file multiplied by every dispatch into files read on demand.

**Risk, ranked.**

1. *A relocated warning is not read at the moment it is needed.* Highest-consequence risk, since
   every relocated trap already cost an incident. Mitigations: the one-line rule stays in
   `CLAUDE.md` — a reader who never opens `PITFALLS.md` still gets the rule; every pointer names a
   specific `§`; the five `docs/doc-preserve.tsv` phrases were kept in `CLAUDE.md` by design and the
   check still passes at count 5.
2. *Relocation re-asserts stale text.* Demonstrated, not hypothetical: §4.2 records the false
   peer-plane claim I copied forward and then caught against `crosscheck.go:69-72`. Mitigation
   applied, and **it was not sufficient**: I re-checked the relocated claims against code and still
   carried a SECOND stale one across — the "known still-stale twin: `client/enrol.go:64`" warning,
   which `ad03e13` (DOCS-22) had corrected on 2026-08-16, five days earlier. The reviewer gate caught
   that one, not me. Both are now removed, and the second is recorded in `INVARIANTS.md`'s
   Enforcement status. Read this as evidence that a relocation pass needs an independent reader, not
   only a careful author. Residual risk is in the passages nobody re-verified — the commit incidents
   in `PITFALLS.md` §4, historical narrative about shas rather than claims about current behaviour.
3. *`AGENTS.md` restarts drifting the next time someone edits it directly.* Unmitigated by design —
   it is `6a5ece85`'s decision (§4.4).
4. *Stale line-number citations elsewhere.* Landmine 3. Bounded and enumerated.

**Rollback.** Fully reversible, no data or schema involved.

```
git checkout <commit>^ -- CLAUDE.md AGENTS.md INVARIANTS.md
git rm PITFALLS.md DOC-REFACTOR_DEEPDIVE.md
```

`bash scripts/doc-check.sh budget` returns to `FAIL … over its 28781 B ceiling`, which is the
pre-change state. Nothing else in the repo depends on `PITFALLS.md`: it is referenced only from
`CLAUDE.md`/`AGENTS.md`, which revert together. Partial rollback is also safe — reverting
`CLAUDE.md` alone leaves `PITFALLS.md` as an unreferenced but accurate file.

**Not applied by this task, for the coordinated commit to note:** no migration number was needed or
reserved; no record type, wire protocol version or task key was reserved; no task state was flipped
beyond this task's own claim.
