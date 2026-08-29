# change-tier.sh: guard-file and verification-infrastructure signal

| Field | Value |
| --- | --- |
| Public id | `212e695b-c11c-485b-aaa4-730d2f0ebd13` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T08:40:02.781768+00:00 |
| Updated | 2026-08-22T09:51:05.519419+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/change-tier_test.sh'
```

## Description

Two corrections to the proposed signal 3 (*guard*_test.go), both found by reconnaissance:

(a) The filename glob misses most real guards. Only five files match *guard*_test.go (client/guard_test.go, internal/invite/guard_test.go, internal/relay/guards_test.go, internal/store/guard_relay24fu_test.go, internal/ids/freshsuffixguard_test.go) but roughly thirty more are genuine source-parsing guards without "guard" in the name -- including cmd/agent-bus/invitegate_enforce_test.go (asserts enrolmentInviteRequired is true, i.e. the constant-value guard for invariant 3, :25-26), internal/httpapi/crosscheck_mtlscrosscheck_test.go, internal/signing/ackvocab_external_test.go, cmd/agent-bus/tlslisten_test.go, client/pinrotate_test.go. Use the mechanical rule that actually characterises them: any _test.go that imports go/ast or go/parser, or walks .go sources via filepath.Walk/filepath.WalkDir -- plus the filename glob as a union, not a replacement.

(b) The classifier and the verification scripts must be in the guard set, including the classifier itself: scripts/change-tier.sh, scripts/doc-check.sh, scripts/proof-check.sh, scripts/proof-cmd-audit.py and their test files. As originally specified the mechanism that computes the floor is a shell script, so editing it is "no .go change" and measures T0 -- the tiering scheme's own enforcement mechanism would sit in its lowest tier, and a change removing a signal would lower every future measurement while skipping review. This is a hole in the design, not an edge case.

Detection must fire on DELETE and RENAME of a guard path, not only on modification.

Proof detail: fixtures for a guard-named file, an AST-importing test file with no "guard" in its name, a DELETED guard file, and an edit to scripts/change-tier.sh itself. RED first.

BLOCKED BY T-03.

2026-08-22 CORRECTION (planner; every number below RE-MEASURED by spec-keeper against the tracked
tree, not copied from the brief). Measurement method matters and is part of the requirement: scope
the classifier to `git ls-files`, NOT a filesystem walk. A naive `find` / `grep -r` from the repo
root also descends `.claude/worktrees/*` and `.worktrees/*` (seven and one nested checkouts
respectively at the time of writing), which turned the glob count from 5 into 45 and the union from
16 into 159. A classifier that miscounts by 10x because of sibling worktrees is not a floor.

Measured 2026-08-22 over `git ls-files '*_test.go'` (235 tracked test files):
  * `*guard*_test.go` matches 5 files: client/guard_test.go, internal/ids/freshsuffixguard_test.go,
    internal/invite/guard_test.go, internal/relay/guards_test.go,
    internal/store/guard_relay24fu_test.go.
  * The union {glob} U {_test.go importing "go/ast" or "go/parser"} U {_test.go calling
    filepath.Walk/WalkDir} is 16 files (component sizes 5 / 16 / 4). So the filename glob alone
    MISSES 11 real guards: client/pinrotate_test.go, cmd/agent-busctl/cli_test.go,
    cmd/agent-bus/servertimeouts_test.go, cmd/agent-bus/suffixfloors_test.go,
    cmd/agent-bus/tlslisten_test.go, internal/ack/vocabulary_test.go,
    internal/httpapi/crosscheck_mtlscrosscheck_test.go, internal/httpapi/peermount_relay20_test.go,
    internal/ids/atomicfile_test.go, internal/relay/ackretry_ack7_test.go,
    internal/relay/ack_test.go.

BUT EVEN THE UNION MISSES GENUINE GUARDS -- four confirmed, which is the finding that decides the
design. cmd/agent-bus/invitegate_enforce_test.go is the invariant-3 constant-value guard:
TestInviteGateShippedServerRequiresAnInvite asserts `enrolmentInviteRequired` against the LITERAL
`true` (deliberately, so flipping the constant goes RED), and its failure message states that
flipping it reopens a permanent roster-exhaustion DoS -- ~4096 unauthenticated POSTs to /v1/enroll
brick the bus forever, because the roster caps at 4096, nothing frees a slot, there is no leave
route, and ids are never reused (invariant 1). It parses nothing and walks nothing: it imports only
"testing". It has NO mechanical signature at all. The same holds for
internal/auth/invitegate_enforce_test.go and internal/httpapi/invitegate_enforce_test.go. Fourth,
and a direct correction to paragraph (a) above: internal/signing/ackvocab_external_test.go is cited
there as an example the union rule catches -- it does NOT; it is in neither the glob nor the
ast/parser nor the walk set. Treat that citation as an example of a MISS, not a hit. (Paragraph
(a)'s "roughly thirty more" is also wrong as an estimate; the measured figure is 11.)

CONSEQUENCE FOR THIS TASK -- pattern-matching alone CANNOT enumerate the guard set, and no amount
of additional patterns fixes it, because a guard can be an ordinary assertion against a literal.
The signal must therefore be PATTERNS UNIONED WITH AN EXPLICIT CHECKED-IN MANIFEST of guard paths.
A TSV alongside docs/doc-budgets.tsv is the established shape here; follow it. The manifest is
additive to the patterns, never a replacement -- a path matching either source is a guard.

AND THE MANIFEST HAS ITS OWN ROT PROBLEM: deleting a line from it silently disables that guard,
which is the same class of defect as paragraph (b)'s "editing the classifier measures T0". So the
MANIFEST FILE ITSELF MUST BE IN THE NEVER-AUTO-LOWER SET, exactly like scripts/change-tier.sh and
the verification scripts -- any modification, deletion or rename of the manifest raises the floor
and requires recorded reviewer sign-off, and a REMOVAL from the manifest must be detected as such,
not merely as "the manifest changed". Add fixtures for: a manifest-listed guard with no matching
pattern (proves the manifest is consulted), and a DELETED line from the manifest (proves removal
raises rather than lowers the floor). RED first.

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

---

## CLASSIFY GUARDS BY CONTENT, NOT BY FILENAME (2026-08-22, security gate)

This STRENGTHENS the manifest requirement above with the security gate's NAMED consequences. It does
not replace it: the rule stays **patterns UNIONED WITH the checked-in manifest**.

**The measurement, re-confirmed 2026-08-22 over `git ls-files` (never a filesystem walk):**
`grep -Ei 'guard'` over PATHS matches **5** files, while **16** tracked files carry a
`go/ast` / `go/parser` / `filepath.Walk` source walk. Filename matching is not a floor; it is a
sample.

**Three specifically missed guards, each with a real invariant behind it:**

- `cmd/agent-bus/tlslisten_test.go` -- the **no-plaintext-listener AST guard, invariant 11**.
  Verified: the string "Guard" appears ONLY inside function names
  (`TestCmdHasNoPlaintextListenerGuardIsRedCapable`, :359/:367/:527) and NEVER in the path, so no
  path-based pattern can see it.
- `client/pinrotate_test.go:357` -- the **no-trust-on-first-use guard, invariant 11**
  (`TestPinIsNeverLearnedFromAHandshake`: across a refused and a successful handshake the persisted
  accept-set is byte-for-byte unchanged).
- `internal/httpapi/authmw_test.go:549` -- **`TestEveryRouteRequiresAuth`**, which pins invariant
  3's `unauthenticatedRoutes` allow-list by walking the server's REAL registered surface. It was
  **measured DELETABLE** under the path-based rule: nothing in its path says "guard".

**REQUIREMENT: match on the CONTENT of every touched `_test.go` file**, at minimum these tokens --
`go/ast`, `go/parser`, `InsecureSkipVerify`, `VerifyPeerCertificate` -- **and keep the explicit
manifest**. State the token list in the script and in `docs/CHANGE-TIERS.md` as a **FLOOR, NOT AN
ENUMERATION**: adding a token is always permitted, and the list never claims completeness.

**The manifest is NOT made redundant by content matching, and the reason is already measured above:**
`cmd/agent-bus/invitegate_enforce_test.go` (with `internal/auth/` and `internal/httpapi/`
counterparts) is a constant-value guard on `enrolmentInviteRequired` that imports only `testing` --
it parses nothing, walks nothing, and has **NO mechanical signature at all**. Content patterns miss
it exactly as filename patterns do. That finding stands unchanged and is precisely why the manifest
exists.

---

## MERGED IN FROM c9e89d5a (2026-08-22, spec-keeper) — THIS TASK IS THE SINGLE OWNER OF THE MANIFEST

`c9e89d5a-6f6f-475e-8c8e-24f663a060bc` ("Explicit manifest of security-bearing test files, as a third
guard check alongside the two carve-out patterns") was filed ~40 minutes after this task by a
concurrent run that did not see it, and substantially duplicated it: this task already mandated
"patterns UNIONED WITH the checked-in manifest" and already named all four of that task's files,
including `internal/httpapi/authmw_test.go:549`. It has been CLOSED as `cancelled` naming this task
as the owner, AFTER the three contributions below were folded in here.

**THE SINGLE-OWNER RULE, which is the reason for the merge and is now part of this task's
definition of done: ONE owner for the manifest. Otherwise the classifier (`scripts/change-tier.sh`)
and the TSV become TWO definitions of the guard set, free to disagree — and a guard set that
disagrees with itself FAILS OPEN.** That is the same failure shape as the two classifiers T-18
exists to collapse. Whoever implements this owns both halves in one change: the manifest file and
the classifier's consumption of it. Do not split them across tasks, and do not let a second task
acquire the right to define manifest membership.

### Addition 1 — CORRECTED SCALE: the manifest is ~60 entries, not 4

Re-measured 2026-08-22 over `git ls-files '*_test.go'` (235 tracked test files; `git ls-files`, never
a filesystem walk — see the MEASUREMENT-METHOD WARNING implications in the section above). The two
existing carve-out patterns — path `grep -Ei 'guard'` and content
`grep -E 'go/ast|go/parser|InsecureSkipVerify|VerifyPeerCertificate'` — cover only **22 of 235**.
The content pattern alone already matches those same 22 (at this snapshot it is a SUPERSET of the 5
path-pattern matches), so the union is 22, not 27.

**Size the manifest for roughly 60 entries, not 4.** The four files originally seeded plus the ~15
enumerated below are a FLOOR, not the set; reaching the real total requires a full pass across all
235 tracked `_test.go` files against INVARIANTS.md 1–11. Each entry records which invariant/check it
enforces.

Uncovered files verified present via `git ls-files --error-unmatch`, and verified 2026-08-22 to have
0 content-pattern matches and no "guard" in the path:
- Invariant 11 (mTLS / pinning / no-TOFU): `internal/auth/crosscheck_mtlscrosscheck_test.go`,
  `internal/httpapi/clientcert_mtlsbind_test.go`
- Invariant 3 (invite-gated enrolment / allow-list): `internal/httpapi/invitegate_enforce_test.go`,
  `internal/auth/invitegate_crash_test.go`, `internal/auth/invitegate_enforce_test.go`,
  `internal/auth/invitegate_service_test.go`, `internal/auth/invitegate_test.go`
- Invariants 4/5/6 (durability / recovery / crash-injection): `internal/wal/crash_injection_test.go`,
  `internal/wal/replay_crash_test.go`, `internal/wal/indexfloor_crash_test.go`,
  `internal/wal/indexfloor_auth_test.go`
- Invariant 10 (idempotency / duplicate detection): `internal/idem/crashinjection_test.go`
- Invariant 1 (server-authoritative ids / sequence never rewinds):
  `cmd/agent-bus/seqfloorforge_test.go`, `cmd/agent-bus/seqfloormissing_test.go`,
  `cmd/agent-bus/seqfloorrestart_test.go`

COST CONSEQUENCE: seeding ~60 entries with a per-entry justification is materially more work than
seeding 4. If that changes this task's priority relative to the rest of the PROCESS epic, decide and
record it explicitly via spec-keeper rather than silently absorbing it into the existing P1.

### Addition 2 — OMISSION DRIFT IS **UNSOLVED**, and must not be recorded as solved

A ~60-entry hand-maintained manifest will drift BY OMISSION, and nothing in this task's current
design detects it. The two directions are DIFFERENT and only one is covered:

- **Entry DELETED or RENAMED** — the manifest names a file that no longer exists. Already caught by
  this task's existing requirement (the never-auto-lower set plus the "every manifest entry exists"
  check, which must detect a REMOVAL as such, not merely as "the manifest changed").
- **Entry SILENTLY ABSENT** — a NEW security-bearing test that nobody adds to the manifest.
  **NOTHING DETECTS THIS.** An existence check cannot: the manifest that omits the file is still
  internally consistent, so it looks complete while quietly rotting as new tests land uncatalogued.

**Do NOT pick a fix for this inside this task, and do NOT let the existence check be presented as
covering it.** Record it as OPEN. Options on the table for whoever picks it up:
- a periodic audit that diffs the current crash-injection-test and AST-guard sets (or a broader
  per-invariant sweep) against the manifest and flags anything present in the sweep but absent from
  the manifest;
- requiring any NEW `_test.go` under an invariant-bearing package (`internal/auth`, `internal/wal`,
  `internal/idem`, `internal/httpapi`, `cmd/agent-bus`, `client`, and similar) to be either added to
  the manifest or to carry an explicit not-a-guard annotation, enforced at review time.

### Addition 3 — THE SAME-BASENAME PAIR: match by EXACT PATH, never by basename

`internal/auth/crosscheck_mtlscrosscheck_test.go` and
`internal/httpapi/crosscheck_mtlscrosscheck_test.go` are DIFFERENT files in DIFFERENT packages that
share a basename. Confirmed 2026-08-22: the `internal/httpapi/` one IS already covered by the content
pattern; the `internal/auth/` one is NOT. **The two land on opposite sides of the existing patterns
while being indistinguishable by basename.**

**Therefore the manifest matches by EXACT REPO-RELATIVE PATH, and any keying, deduplication, lookup
or "already covered?" test that reduces an entry to its basename is a DEFECT** — it would treat the
covered `internal/httpapi/` file as evidence that the uncovered `internal/auth/` file is protected,
which is a fail-open collision. Add a fixture for this exact pair, shown RED against a
basename-keyed implementation.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md)
- **relates to** [c9e89d5a-6f6f-475e-8c8e-24f663a060bc](../Explicit-manifest-of-security-bearing-test-files-as-a-th--c9e89d5a/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)
- [c9e89d5a-6f6f-475e-8c8e-24f663a060bc](../Explicit-manifest-of-security-bearing-test-files-as-a-th--c9e89d5a/task.md) — Explicit manifest of security-bearing test files, as a third guard check alongside the tw… (cancelled)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)
- [c9e89d5a-6f6f-475e-8c8e-24f663a060bc](../Explicit-manifest-of-security-bearing-test-files-as-a-th--c9e89d5a/task.md) — Explicit manifest of security-bearing test files, as a third guard check alongside the tw… (cancelled)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
