# change-tier.sh: package to invariant-plane partition, with DEFAULT-DENY for unmapped paths

| Field | Value |
| --- | --- |
| Public id | `255bdc5a-f36e-4cfb-a484-199fbd6d16ab` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T08:40:02.201889+00:00 |
| Updated | 2026-08-22T09:13:35.012960+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/change-tier_test.sh'
```

## Description

The crux signal. The proposed mapping is an ALLOW-LIST OF DANGER, which means anything unmapped silently measures LOW. Reconnaissance confirmed this is not theoretical -- these packages are unmapped in the proposal and every one carries invariant load:

| path | invariants | evidence |
|---|---|---|
| internal/hub | 1, 2, 4, 5, 6, 10 -- the single biggest unmapped carrier | internal/hub/doc.go:6-42 names 1, 4, 5, 10, 2 one by one; internal/hub/audit.go is the invariant-6 metadata-only trail |
| internal/invite | 3 (primary), 1, 4, 5, 10, 11 | internal/invite/doc.go:29-49 -- this package IS invariant 3's single-use/expiring/revocable invites; the invite blob is the invariant-11 trust anchor |
| internal/ack | 4, 5, 6, 10 | internal/ack/doc.go:15-19, :44-58 |
| internal/attest | 9, 2, 11 | internal/attest/doc.go:44-60 is a literal invariant-9 accounting section |
| internal/buscert | 11 primary, 9, 5 | internal/buscert/doc.go:1-30; buscert.go:65-72 is the rotation gap cited in INVARIANTS.md |
| internal/signing | 9 (by exclusion), 1, 10, 4 | internal/signing/doc.go:1-6 "must never grow a construction of its own" |
| internal/dirlock | 5, 4 | internal/dirlock/dirlock.go:1-14 -- two servers on one data dir destroy the WAL |
| internal/logging | 6 | the "every discard is logged loudly" requirement has no other implementation; logging.go:11-24 is log-injection resistance |
| cmd/agent-bus/main.go | 3 and 11 | :47-67 enrolmentInviteRequired = true; :1797 tls.NewListener; :1828-1843 MinVersion/ClientAuth summary |
| cmd/agent-busctl/** | 7 | every subcommand is invariant 7 |

So: make the mapping a TOTAL PARTITION with three outcomes, not an allow-list --
1. mapped to a plane -> that plane's floor (T3+);
2. explicitly listed as low-risk -> T2;
3. unmapped non-test path -> T3, plus a loud "UNMAPPED PATH -- map it or justify it" warning naming the path.

Default-deny is what stops the table rotting the way prose counts rot in this repo. A new package added next month otherwise defaults to the cheap lane forever.

Also in scope: merge the proposed signal 8 ("change on an attacker-controlled input path") into this table. "Attacker-controlled" is a judgment a script cannot compute; the PATH SET is mechanical and is already covered by mapping internal/relay, internal/invite, internal/auth, internal/httpapi. Do not ship a signal that pretends to compute intent.

Also add the plane-to-mandated-artefact table: crash-injection test for 4/5/6; AST guard for 11; the three-case table for 10; and -- missing from the original design -- invariant 7's artefact: a cmd/agent-busctl subcommand plus an AGENT_PROTOCOL.md entry, in the same task. Invariant 7 has a mandated artefact exactly like 4/5/6 and 11 do, and it was omitted.

Proof detail: fixture cases for a mapped plane path, an explicitly-safe path, and an unmapped path that must floor at T3 with the warning. Show each RED first.

BLOCKED BY T-03.

---

## AMENDMENT C (2026-08-22, planner via orchestrator): control plane is a FOURTH outcome in the partition

Per the control-plane principle recorded on T-01 (4d990ef4-23ee-4971-ab00-84eb5ec137ae, Amendment A),
the total partition gains a fourth outcome alongside the three above:

4. **control plane -> T3 floor, never-auto-lower.**

Restating the full partition so it stays total:
1. mapped to a plane -> that plane's floor (T3+);
2. explicitly listed as low-risk -> T2;
3. unmapped non-test path -> T3, plus the loud "UNMAPPED PATH" warning;
4. **control plane -> T3 floor, never-auto-lower** (and T4 if the change deletes or narrows a check).

The control-plane test is the PRINCIPLE from T-01, not a path list: *a file that determines what is
checked, or that performs the check.* Outcome 4 is an OVERRIDE and therefore applies in the
second phase of the evaluation order (overrides may only raise) -- it must not be folded into the
first-phase path floor.

**Required fixture:** a `.md`-only change to `.claude/agents/security.md` must floor at **T3, NOT
T0**. This fixture is the regression test for the live near-miss described on T-01 -- the interim
docs-and-tests-only security carve-out (task 97a315af) whose own commit was all-`.md` and therefore
qualified for its own carve-out. **It must be shown RED first** and the RED output quoted in the
task's `kind=report`.

---

## INHERITS F1 AND F2 FROM T-03 (2026-08-22, security gate)

**This signal classifies by PATH, and therefore inherits findings F1 and F2 recorded on T-03
(b2567ffd-190d-4aff-8cc2-f6a2eb2d613e).** Both are measured, not theorised:

- **F1 (renames):** `git status --porcelain` prints a rename as ONE line, `R  old -> new`, so a
  check anchored at `^` never sees the target and a check testing the line end never sees the
  source. **This signal must consume the `git status --porcelain --no-renames` file set**, in which
  the rename is split into `D old` + `A new` and both halves are classified.
- **F2 (fails open):** `git status --porcelain -- <pathspec matching nothing>` prints nothing and
  exits **0**. **This signal must NOT treat an EMPTY file set as low-risk** -- "measured T0" and
  "could not measure" are different outcomes, and the second is an error exit, not a result.

**This signal needs a RENAME FIXTURE in its own right** -- not merely coverage in T-03's tests --
shown RED before the signal is implemented, with the RED output quoted in the task's `kind=report`.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4d990ef4-23ee-4971-ab00-84eb5ec137ae](../Write-docs-CHANGE-TIERS.md-the-normative-tier-and-signal--4d990ef4/task.md) — Write docs/CHANGE-TIERS.md, the normative tier and signal specification (todo)
- [97a315af-70b3-4a64-8456-92335d8c9631](../Make-security-skip-the-default-for-docs-and-tests-only-c--97a315af/task.md) — Make security skip the default for docs-and-tests-only changes, with a guard-file carve-o… (done)
- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [461ebf5b-5d0e-4c3d-b95e-a1437e15b31f](../Acceptance-gate-all-four-low-measuring-cases-sort-correc--461ebf5b/task.md) — Acceptance gate: all four low-measuring cases sort correctly (todo)
- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
