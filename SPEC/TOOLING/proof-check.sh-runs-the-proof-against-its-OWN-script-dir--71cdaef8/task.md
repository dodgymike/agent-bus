# proof-check.sh runs the proof against its OWN script directory repo root, not the callers cwd -- silently defeats git-archive-overlay isolation testing

| Field | Value |
| --- | --- |
| Public id | `71cdaef8-c757-4ba9-a693-a8f744070d08` |
| Key | _(null in the export)_ |
| Epic | [TOOLING](../epic.md) |
| Status | in_progress |
| Priority | P1 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T19:54:25.487857+00:00 |
| Updated | 2026-08-14T09:32:40.480127+00:00 |
| Completed | — |

## Proof command

```sh
REPO=$(pwd); T=$(mktemp -d); git archive HEAD | tar -x -C "$T"; echo only-in-overlay > "$T/ISO_MARKER.txt"; V=$(cd "$T" && bash "$REPO/scripts/proof-check.sh" "test -f ./ISO_MARKER.txt" 2>&1 | grep -o "verdict=[A-Z]*"); rm -rf "$T"; test "$V" = "verdict=PASS"
```

## Description

scripts/proof-check.sh:156-157 computes REPO_ROOT from SCRIPT_DIR (dirname of the script itself) and then, at the actual execution site (lines 508/510), does `( cd "$REPO_ROOT" && bash -c "$CMD" )` unconditionally -- it NEVER runs in the caller's own working directory. It also prints `running (cwd ${REPO_ROOT})...` which looks like a statement of fact but is really an announcement that the caller's cwd was silently discarded.

CONSEQUENCE: the standard isolation technique this project uses to prove a change consumes nothing from other agents' uncommitted work is `git archive HEAD | tar -x -C <tmpdir>` into a clean overlay, then invoking proof-check.sh BY ABSOLUTE PATH against that overlay. Because the script always resolves back to its own repo (the live working tree), that invocation silently computes the verdict against the LIVE tree instead -- including every other agent's uncommitted changes -- and there is NO signal that this happened. An integrator committing RELAY-27 caught this by accident (had to copy the script into the overlay and re-run scoped correctly) -- same result that time, but only because they noticed. Most invocations would not notice.

CONFIRMED RED (not VACUOUS), reproduced live 2026-08-08: created an overlay via `git archive HEAD | tar -x -C $OVERLAY`, wrote a marker file that exists ONLY in the overlay ($OVERLAY/MARKER.txt), cd'd into the overlay, then ran `bash /abs/path/to/scripts/proof-check.sh 'test -f ./MARKER.txt'`. Expected (if isolation held): PASS, since the file genuinely exists relative to the caller's cwd. Actual: `proof-check: running (cwd /mnt/sdb4/mike/mike/source/agent-bus)...` followed by `verdict=FAIL class=file-assertion exit=1` -- it silently substituted the live repo root for the overlay and the assertion failed there instead. The verdict is wrong AND there is no warning that the substitution occurred.

RELATED, not duplicate: this is the SECOND defect found in this tool, after cea09b96 (subtest SKIP/PASS lines invisible to the plain-text counter, so a parent-PASS/all-children-SKIP certifies PASS instead of VACUOUS). Both defects share the same failure shape: the tool CLAUDE.md mandates specifically to make evidence trustworthy has produced a confidently wrong verdict, silently. Filed as a sibling task under the same TOOLING epic rather than a new umbrella parent -- this backlog already tracks proof-check.sh defects as discrete atomic tasks (PROOF-CHECK-FU-RECURSION, the zero-probe-guard-convention task, and cea09b96), so a new parent would mean retrofitting three live tasks rather than following the established pattern. A `relates` link to cea09b96 records the kinship without merging scope.

DEFINITION OF DONE: (1) proof-check.sh either runs the proof in the CALLER'S cwd (the natural fix -- drop the `cd "$REPO_ROOT"` at the execution site, or make it conditional on being invoked with a relative path / no override), OR refuses loudly (distinct exit code / UNVERIFIABLE-class message) when it detects it is being invoked against a different repo root than the caller's cwd -- either is acceptable, but silent substitution is not. (2) A guard test demonstrates the isolation case concretely: same proof command, two trees (a real overlay via git archive, not a mock), two DIFFERENT verdicts -- i.e. a command that is genuinely true relative to the overlay and genuinely false (or absent) relative to the live repo, or vice versa, and the script's reported verdict tracks the overlay once fixed. (3) The `running (cwd ...)` line, once fixed, must report the directory the command ACTUALLY ran in, not a resolved repo root that may differ from where the caller invoked it.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [cea09b96-72db-40f1-84b4-c2e227eae1cf](../proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [PROOF-CHECK-FU-RECURSION](../PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) — PROOF-CHECK-FU-RECURSION: bash scripts/proof-check.sh hangs / spawns runaway processes wh… (todo)
- [RELAY-27](../../RELAY/RELAY-27--f417c6a0/task.md) — RELAY-27: fix internal/relay/signed.go:306 to wrap attest.Verify errors with %w, not %v (cancelled)
- [RELAY-27](../../RELAY/RELAY-27--c2486740/task.md) — RELAY-27: relay error taxonomy collapses ALL FIVE attest sentinels to ErrNoSignerKey/bad_… (done)
- [cea09b96-72db-40f1-84b4-c2e227eae1cf](../proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) — proof-check.sh: subtest SKIP/PASS lines invisible to plain-text counter -- parent-PASS/al… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [315899be-dd43-4462-baf4-eae2fd94364b](../../PROCESS/scripts-backlog-drift.sh-read-only-detector-listing-in_p--315899be/task.md) — scripts/backlog-drift.sh: read-only detector listing in_progress/todo tasks whose stored… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
