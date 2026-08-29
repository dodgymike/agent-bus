# Write docs/CHANGE-TIERS.md, the normative tier and signal specification

| Field | Value |
| --- | --- |
| Public id | `4d990ef4-23ee-4971-ab00-84eb5ec137ae` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T08:39:34.406298+00:00 |
| Updated | 2026-08-22T09:26:23.406247+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/doc-check.sh section docs/CHANGE-TIERS.md "## The mechanical floor" "may never lower" "reviewer sign-off"'
```

## Description

Create docs/CHANGE-TIERS.md as the single normative definition of the tiered chain. Everything else in this epic points at it. It exists as a companion file specifically because CLAUDE.md is byte-capped (docs/doc-budgets.tsv: 28781; HEAD is 28213, i.e. 568 bytes headroom, and a live documentation edit has already consumed 563 of it -- see T-15/T-16 budget note).

It must define, normatively:

- The five tiers T0..T4 and the agent chain each mandates.

- **SUPERSEDED 2026-08-22 by RATIFICATION R6 below -- do NOT transcribe this bullet. T0 is documentation CONTENT only, and ANY EXECUTABLE FILE FLOORS AT T2 MINIMUM whatever its language. Read R6 before writing the T0 section.** T0 is DOCUMENTATION FILES ONLY (*.md, docs/), NOT "no .go change". The original framing was "no .go change", which routes every scripts/*.sh change to T0. That is wrong here: shell scripts in this repo are production and verification infrastructure, and scripts/doc-check.sh has already had two real security findings against it (injectable TMPDIR, sed option-injection, both fixed at scripts/doc-check.sh:554-573 and :345). scripts/** is never T0.

- The mechanical floor, and the raise-only asymmetry: the implementer may raise with a reason, may never lower below the floor. Lowering requires reviewer sign-off, recorded. Rationale, stated in the doc: "I expected risk and found none" is indistinguishable from "I did not find the risk."

- Evaluation ORDER, which is load-bearing: compute the path/content floor first, THEN apply the never-auto-lower overrides. Overrides may only raise. This ordering is what stops a test-only change that DELETES tests from taking the T1 skip-security lane.

- The never-auto-lower overrides: guard file touched; invariant 9 (crypto) in scope; the change deletes or weakens a test.

- An honest statement of what is NOT mechanical. "Weakens a test" cannot be computed -- loosening an assertion inside a retained test function is semantic. The script can compute DELETION (removed _test.go, removed func Test..., added t.Skip); "weakens" stays a reviewer judgment. The doc must say so plainly rather than implying the floor covers it, because this is the highest-stakes override and overclaiming it is how it gets trusted when it should not be.

- The diff-basis contract. The classifier must measure THE SAME BYTES THE COMMIT WILL TAKE. This repo commits with an explicit pathspec, and a pathspec commit takes the WORKTREE, not the index (PITFALLS.md section 4). So the basis is `git diff HEAD -- <pathspec>` over the worktree, plus untracked files under the pathspec. **SUPERSEDED 2026-08-22 -- do NOT transcribe this command. The basis is `git status --porcelain --no-renames`; see the correction below in this description.** Classifying the index would measure something the commit will not contain. The script must refuse ambiguous input loudly rather than guess.

- The design rule for every signal: bias to OVER-trigger, never under-trigger -- since raising is free and lowering needs sign-off. With one stated caveat: a signal that fires on nearly everything is not conservative, it is noise, and noise trains agents to lower. Named example, which is why this caveat exists: the originally proposed "any crypto/* import -> invariant 9" rule fires on 57 non-test files, because crypto/sha256, crypto/rand and crypto/subtle are used repo-wide for hashing, id generation and constant-time compares. Invariant 9 is about AUTHORING constructions. The narrowed rule is: a crypto/* import in one of internal/signing, internal/attest, internal/buscert, internal/wal, client/canonical.go, client/pin.go, client/ack.go.

- The kind=tier note format (new fifth journal kind): `kind=tier; estimated=T<n>; measured_floor=T<n>; final=T<n>; signals=<comma-list>; raised_by=<agent>; lowered_by=<agent>; reason=<text>`. One per task. This is the record drift detection reads (T-17).

BLOCKS: T-02, T-03, T-10, T-11, T-12, T-13, T-14, T-15, T-16, T-17.

---

## AMENDMENT A (2026-08-22, planner via orchestrator): CONTROL PLANE is a risk class of its own

**This amendment corrects the T0/T1 definitions above, which are WRONG AS STATED.** A file's
EXTENSION does not determine its tier. The bullet above that reads "T0 is DOCUMENTATION FILES ONLY
(*.md, docs/)" is necessary but NOT sufficient: some `.md` files are not documentation.

**The evidence is a live near-miss, not a hypothetical.** The interim docs-and-tests-only security
carve-out (task 97a315af) was tested against its own commit. Every path in that commit was `.md`,
so **the change that weakens the security gate qualified for its own carve-out.** Caught before
commit.

**The category error.** These are NOT documentation:
`CLAUDE.md`, `AGENTS.md`, `.claude/agents/*.md`, `.claude/ORCHESTRATION.md`,
`scripts/doc-check.sh`, `scripts/proof-check.sh`, `scripts/change-tier.sh`,
`docs/doc-budgets.tsv`, `docs/doc-preserve.tsv`, the guard manifest from T-05, and
`docs/CHANGE-TIERS.md` itself. They determine what is checked, or they perform the check. Changing
one can disable a check without touching a line of product code.

`docs/CHANGE-TIERS.md` MUST therefore state control plane as its own classification, and MUST state
the PRINCIPLE, not only a path list -- a list goes stale and invites "my new file is not on it":

> **A file that determines what is checked, or that performs the check, is control plane.**
> Operational test: if changing this file ALONE could make a previously-failing check pass, it is
> control plane.

Ship BOTH: the principle as normative text, and the path list as a non-exhaustive illustration
explicitly labelled non-exhaustive.

**Planner's RECOMMENDATION on where control plane sits.** Record this as the recommendation, to be
CONFIRMED when T-01 is implemented -- it is not yet ratified:

- Control plane floors at **T3**, never lower, and is in the never-auto-lower override set.
- A control-plane change that DELETES or NARROWS a check floors at **T4** -- the same treatment as
  deleting a test, because it is the same act with more leverage.
- Rationale to write down in the doc: a control-plane change is typically small, touches no product
  code, and measures LOW on every size-based and package-based signal, while being able to switch
  off every other check. Every positive test still passes afterwards. That is the exact shape of the
  failure mode this scheme exists to catch.
- Note the recursion EXPLICITLY in the doc: `docs/CHANGE-TIERS.md` and `scripts/change-tier.sh` are
  themselves control plane, so the scheme classifies its own modification at T3+. That is intended,
  not an oversight.

**Interaction with the evaluation ORDER above.** Control plane is an override, so it is applied in
the second phase (overrides may only raise), consistent with the ordering rule already stated. It
must not be folded into the first-phase path floor, or a T0 `.md` floor computed first would be
lowered by nothing and raised by this -- which is the correct outcome and is exactly what the
ordering guarantees.

**See also Amendment B on T-03 (b2567ffd-190d-4aff-8cc2-f6a2eb2d613e): the "diff-basis contract"
bullet above says the basis is `git diff HEAD -- <pathspec>` plus untracked files. That phrasing is
SUPERSEDED.** The basis is **`git status --porcelain --no-renames`** over the exact pathspec. Write
THAT rule into `docs/CHANGE-TIERS.md`; do not transcribe the stale line.

**The REASON was itself corrected on 2026-08-22 -- use the corrected one.** An earlier version of
this pointer gave the untracked-file gap as the reason, and T-03's Amendment B additionally claimed
"a pathspec commit TAKES that file". **That claim is FALSE and withdrawn**: `git commit -m x --
brandnew.go` on an untracked path errors with `pathspec ... did not match any file(s) known to git`
and exits 1 (measured 2026-08-22). The CONCLUSION did not change; do not re-open it on the strength
of finding the old reason false.

**The PRIMARY reason is RENAMES:** `git diff HEAD --name-only` prints only the NEW path for a
rename, so `client/pin.go -> client/pin_test.go` reads as a TESTS-ONLY change while moving the one
file where `InsecureSkipVerify` is permitted (invariant 11). `--no-renames` splits a rename into
`D old` + `A new` so both halves are classified -- see finding F1 on T-03. The untracked-file gap is
the SECONDARY reason and is still real (`git diff HEAD --name-only` prints nothing, exit 0, for a
new untracked `.go` file).

**Also normative for `docs/CHANGE-TIERS.md`, from finding F2 on T-03:**
`git status --porcelain -- <pathspec matching nothing>` prints nothing and exits **0**, so the doc
must define **"MEASURED T0"** and **"COULD NOT MEASURE"** as DIFFERENT outcomes, the second being an
error exit rather than a tier.

---

## RULINGS 2026-08-22 (coordinator, via spec-keeper)

Rulings on the open questions above. Normative for `docs/CHANGE-TIERS.md` unless a later dated
section says otherwise.

**Ruling 1 -- CONTROL PLANE IS RATIFIED, not a recommendation.** Amendment A's "planner's
RECOMMENDATION ... to be CONFIRMED when T-01 is implemented" is now confirmed. Control plane floors
at **T3**, is in the never-auto-lower override set, and floors at **T4 if the change DELETES OR
NARROWS a check**.

**Do not restate the principle -- CITE it.** The principle, the near-miss and the path table are
already written at `PITFALLS.md` section 8 ("A gate rule that exempts the change which weakens it"):
8.1 carries the measured near-miss, 8.2 carries the principle, the illustrative path table and the
explicit warning that the table will go stale. (That text is staged and UNCOMMITTED in the worktree
at the time of this ruling; if it is not at HEAD when you implement, wait for it rather than
duplicating it.) `docs/CHANGE-TIERS.md` carries only the TIER ASSIGNMENT and points at
`PITFALLS.md` section 8 for the reasoning -- this repo's standing split: the rule here, the incident
there. Amendment A's "Ship BOTH: the principle as normative text, and the path list" is superseded
to that extent -- the principle and the list live in `PITFALLS.md` section 8; CHANGE-TIERS.md cites
them.

**The genuinely new part, which section 8 does NOT yet cover -- add it to the T4 arm:** *narrowing
includes WIDENING WHAT A CARVE-OUT EXEMPTS.* The failure this scheme already hit was not deleting a
check; it was making the exemption bigger. Adding a path to the control-plane list, or widening a
carve-out's predicate, is **T4** work even though it reads as a one-line edit.

**Ruling 2 -- TIER COLLAPSE: FOUR lanes, not five.** T1 is REMOVED and absorbed into T2. The bullet
above reading "The five tiers T0..T4" is superseded.

- **T0** -- see ruling 3.
- **T2** -- production code OR tests-only; no invariant plane, no trust boundary, no concurrency,
  not control plane. Chain: implementer/test-engineer -> reviewer -> security (delta-scoped).
- **T3**, **T4** -- unchanged.

Evidence to record in the doc: over the last 120 commits the split is **58 T3+, 48 T0, 12 T2, 2 T1**.
Two lanes splitting 12% of commits did not earn separate routing. **JUSTIFICATION CORRECTED
2026-08-22 -- see RATIFICATION R7 below; the ruling STANDS, its stated reason does not. Carry R7's
wording into the doc, not a claim about saved signals.**

**Ruling 3 -- T0's definition. CONFIRMED AND FURTHER TIGHTENED 2026-08-22 by RATIFICATION R6
below -- the deviation recorded here was accepted, and R6 is STRICTER than it. R6 is the normative
text; this ruling is retained as history. THIS DEVIATES FROM THE LITERAL RULING and is flagged for the
coordinator to confirm.** The ruling as spoken was "T0 = no `.go` change". Recorded deviation, as
specified here:

> **T0 = documentation files only (`*.md`, `docs/`) AND not control plane.** Everything else that is
> not `.go` -- `scripts/**`, testdata, `Dockerfile`, compose -- floors **T2**.

Reason: "no `.go` change" sends every shell script to T0. Control plane at T3 now covers
`scripts/doc-check.sh`, `scripts/proof-check.sh`, `scripts/change-tier.sh`,
`scripts/proof-cmd-audit.py` and `.claude/**` -- but NOT `scripts/spec-cloud.sh` (handles Cognito
credentials), `scripts/gen-spec-mirror.sh` (writes tracked files) or `scripts/bus-serve.sh`. None of
those performs a check, so the control-plane principle does not catch them, and "no `.go` change"
would route all three to documentation-only review. One line in the spec closes it. This agrees with
the pre-existing T0 bullet above; it is restated because T0 is now the boundary of a four-lane
scheme.

**Ruling 4 -- INVARIANT 7's MANDATED ARTEFACT joins the T3 artefact table**, alongside
crash-injection for invariants 4/5/6, the AST guard for 11 and the three-case table for 10: a
capability ships its `cmd/agent-busctl` subcommand AND its `AGENT_PROTOCOL.md` entry in the SAME
task. (Already recorded on T-04; the artefact TABLE lives in this doc, so state it here too.)

**Ruling 5 -- the TEST-REMOVAL LIMIT, stated normatively here:** the mechanical floor covers test
**DELETION** only. Semantic weakening -- loosening an assertion inside a retained test -- is not
computable and is reviewer-owned. **Nobody may read the floor as covering it.** T-06
(9921c55d-d8a0-460c-ac5f-91a6bb6adcf2) implements the deletion half.


---

## RATIFICATIONS 2026-08-22, SECOND ROUND (coordinator, via spec-keeper)

Normative. Where these conflict with anything dated earlier in this description, THESE WIN.

**R6 -- T0 IS RATIFIED, AND IT IS STRICTER THAN THE EARLIER DEVIATION.** The Ruling 3 deviation
("documentation files only (`*.md`, `docs/`) AND not control plane") is confirmed as the direction
and replaced by this tighter text. Write THIS into `docs/CHANGE-TIERS.md`:

> **T0 is documentation CONTENT only** -- `*.md` / `docs/` material that is not control plane and is
> **not executable**.
>
> **Any executable file floors at T2 MINIMUM, whatever the language.** `.sh`, `.go`, `.py`,
> `Makefile`, anything carrying a shebang or the executable bit. There is no language whose files
> are exempt.
>
> **`scripts/spec-cloud.sh` floors at T3** specifically: it handles credentials, and it is the one
> script in this repo whose compromise leaks something OFF this machine. (The general credential
> rule is T-19; this floor does not wait on it.)

**The REASONING, and it must be written into the doc, because it is the same error twice:**
"no `.go` change" uses **LANGUAGE as a proxy for risk**, which is the IDENTICAL mistake as using
**EXTENSION as a proxy for risk** -- the one that produced the self-exemption near-miss recorded at
`PITFALLS.md` section 8.1, where a gate-weakening change qualified for its own carve-out because
every path in it ended `.md`. Language is not a risk signal. **Being executable is.** A shell script
and a Go file differ in how they are run, not in whether they can do damage; a `.py` helper nobody
has written yet must not arrive exempt because the rule enumerated `.sh` and `.go`.

State the principle, then the illustrations, and label the illustrations non-exhaustive --
`PITFALLS.md` section 8.2's standing warning that path and extension tables go stale applies here
directly.

**R7 -- THE T1-COLLAPSE RULING STANDS; ITS STATED JUSTIFICATION WAS WRONG AND MUST NOT BE CARRIED.**
Ruling 2 (T1 removed, four lanes T0/T2/T3/T4) is unchanged. Replace its reason with this one, which
is the reason to write in the doc:

> **2 commits in 120 do not justify a distinct lane with its own routing, its own documentation and
> its own agent-facing rule.**

The earlier claim -- that collapsing T1 "saves a whole signal, its fixtures and its failure modes"
-- is **MEASURED FALSE and is withdrawn**. Named rather than deleted, so nobody re-derives it:

- **All eight signals survive the collapse.** None was removed.
- The actual saving is **one routing lane, plus a simplified qualifier on signal 1** (signal 1 is now
  "any `.go` file, test or not, floors T2" instead of carrying a test/non-test tier split).
- The subsequently-added **`--no-renames` requirement ADDED a rename fixture to five signals**
  (T-04, T-05, T-06, T-09, T-10 -- finding F3 on T-03). **The classifier work grew on net.**

Do not re-open the collapse on the strength of finding its old reason false; the conclusion is
independent of it. This is the same correction shape already applied to the diff-basis rationale in
Amendment B on T-03.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [016508f4-b57b-4bf1-8a7d-186e1fe82a7f](../claude-agents-implementer.md-require-a-measured-tier-rep--016508f4/task.md)
- **blocks** [24d3e2b5-0d12-484b-8430-6f421a10c275](../spec-keeper-and-planner-record-the-ESTIMATED-tier-when-a--24d3e2b5/task.md)
- **blocks** [3c9c28d9-a02e-465b-b13b-6f9d29056eb4](../Decide-and-implement-the-client-exported-API-signal-or-r--3c9c28d9/task.md)
- **blocks** [445b17af-98c6-4013-8a4c-9faff3774dd1](../Detect-and-record-estimate-vs-measure-tier-drift--445b17af/task.md)
- **blocks** [4e8af108-11e2-4a1b-9193-722309c63dda](../claude-agents-reviewer.md-the-reviewer-owns-any-lowering--4e8af108/task.md)
- **blocks** [51e0993f-76e0-40fd-b6a0-cd7d83d83548](../DECISIONS.md-record-the-tiered-review-chain-and-the-rais--51e0993f/task.md)
- **blocks** [748f6366-1a46-462e-b452-f024f607976b](../claude-agents-security.md-scope-the-security-gate-by-tie--748f6366/task.md)
- **blocks** [a94dee14-fea7-406c-9c4f-485736f434c4](../claude-ORCHESTRATION.md-the-tier-to-agent-routing-table--a94dee14/task.md)
- **blocks** [aeae5c7d-33f0-4ba1-a420-873bec8203d1](../claude-agents-integrator.md-the-commit-gate-must-consult--aeae5c7d/task.md)
- **blocks** [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md)
- **blocks** [e4e31233-cabe-4af4-986b-f28c84347214](../CLAUDE.md-replace-the-flat-mandated-chain-rule-with-the--e4e31233/task.md)
- **relates to** [727dc387-dd95-48e4-9616-9b9b1584ac90](../Security-re-gates-must-be-delta-scoped-citing-the-prior--727dc387/task.md)
- **relates to** [97a315af-70b3-4a64-8456-92335d8c9631](../Make-security-skip-the-default-for-docs-and-tests-only-c--97a315af/task.md)
- **relates to** [ed6853d4-b5de-437a-a3dc-430e1d38243f](../Establish-a-periodic-repo-wide-security-sweep-additive-t--ed6853d4/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [97a315af-70b3-4a64-8456-92335d8c9631](../Make-security-skip-the-default-for-docs-and-tests-only-c--97a315af/task.md) — Make security skip the default for docs-and-tests-only changes, with a guard-file carve-o… (done)
- [9921c55d-d8a0-460c-ac5f-91a6bb6adcf2](../change-tier.sh-test-removal-signal-the-missing-signal--9921c55d/task.md) — change-tier.sh: test-removal signal (the missing signal) (todo)
- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [016508f4-b57b-4bf1-8a7d-186e1fe82a7f](../claude-agents-implementer.md-require-a-measured-tier-rep--016508f4/task.md) — .claude/agents/implementer.md: require a measured tier report and state the raise-only ru… (todo)
- [255bdc5a-f36e-4cfb-a484-199fbd6d16ab](../change-tier.sh-package-to-invariant-plane-partition-with--255bdc5a/task.md) — change-tier.sh: package to invariant-plane partition, with DEFAULT-DENY for unmapped paths (todo)
- [4a24853a-d5f4-4099-97d7-fedb15e38e67](../PITFALLS.md-section-2-a-correction-placed-BELOW-the-text--4a24853a/task.md) — PITFALLS.md section 2: a correction placed BELOW the text it corrects (todo)
- [4e8af108-11e2-4a1b-9193-722309c63dda](../claude-agents-reviewer.md-the-reviewer-owns-any-lowering--4e8af108/task.md) — .claude/agents/reviewer.md: the reviewer owns any lowering below the mechanical floor, an… (todo)
- [748f6366-1a46-462e-b452-f024f607976b](../claude-agents-security.md-scope-the-security-gate-by-tie--748f6366/task.md) — .claude/agents/security.md: scope the security gate by tier (todo)
- [9921c55d-d8a0-460c-ac5f-91a6bb6adcf2](../change-tier.sh-test-removal-signal-the-missing-signal--9921c55d/task.md) — change-tier.sh: test-removal signal (the missing signal) (todo)
- [a94dee14-fea7-406c-9c4f-485736f434c4](../claude-ORCHESTRATION.md-the-tier-to-agent-routing-table--a94dee14/task.md) — .claude/ORCHESTRATION.md: the tier-to-agent routing table (todo)
- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)
- [e4e31233-cabe-4af4-986b-f28c84347214](../CLAUDE.md-replace-the-flat-mandated-chain-rule-with-the--e4e31233/task.md) — CLAUDE.md: replace the flat mandated-chain rule with the tiered one-liner, byte-neutral,… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
