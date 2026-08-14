# EPIC TOOLING — The repo's own build & verification tooling (scripts/, .gitignore, dev env)

[← all epics](../../SPEC.md)

**7 open / 9 total.** Full records live in `SPEC/TOOLING/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (7)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| 521d68b5-4181-4df6-b3c2-ef660ff5461d | proof-check.sh cannot tell "executed" from "asserted" -- adopt a zero-probe guard convent… | todo | P1 | [task.md](proof-check.sh-cannot-tell-executed-from-asserted-adopt--521d68b5/task.md) | blocks [HANDOVER-BACKLOG-RECONCILE](../HANDOVER/HANDOVER-BACKLOG-RECONCILE--43d14776/task.md) | [AUTH-2](../AUTH/AUTH-2--4b45a6d8/task.md) [AUTH-1-FU-LISTENADDR](../AUTH/AUTH-1-FU-LISTENADDR--c27f9439/task.md) |
| 71cdaef8-c757-4ba9-a693-a8f744070d08 | proof-check.sh runs the proof against its OWN script directory repo root, not the callers… | in_progress | P1 | [task.md](proof-check.sh-runs-the-proof-against-its-OWN-script-dir--71cdaef8/task.md) | relates to [cea09b96-72db-40f1-84b4-c2e227eae1cf](proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) | [RELAY-27](../RELAY/RELAY-27--f417c6a0/task.md) [RELAY-27](../RELAY/RELAY-27--c2486740/task.md) [PROOF-CHECK-FU-RECURSION](PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) [cea09b96-72db-40f1-84b4-c2e227eae1cf](proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) |
| a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd | Conjunction-masking vacuous-proof family: filtered-clause proof_cmds hidden by an unfilte… | todo | P1 | [task.md](Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) | blocks [HANDOVER-BACKLOG-RECONCILE](../HANDOVER/HANDOVER-BACKLOG-RECONCILE--43d14776/task.md) | [ID-2-WIRING-SEAL](../ID/ID-2-WIRING-SEAL--8c9b6489/task.md) [DUR-12](../DUR/DUR-12--cbc9ab0c/task.md) [ID-2-WIRING-OBSERVER](../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) [DUR-3](../DUR/DUR-3--d8a991ea/task.md) |
| cea09b96-72db-40f1-84b4-c2e227eae1cf | proof-check.sh: subtest SKIP/PASS lines invisible to plain-text counter -- parent-PASS/al… | todo | P1 | [task.md](proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) | blocks [HANDOVER-CHECK](../HANDOVER/HANDOVER-CHECK--0f909b6c/task.md)<br>relates to [71cdaef8-c757-4ba9-a693-a8f744070d08](proof-check.sh-runs-the-proof-against-its-OWN-script-dir--71cdaef8/task.md) | [SIGN-3](../SIGN/SIGN-3--f2daa6bc/task.md) [84b76d5e-fe02-4651-9828-caba3d82606b](Proof-command-guard-a-run-pattern-that-matches-no-test-m--84b76d5e/task.md) [a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd](Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) |
| DISCOVERY-DOC-FU-GITIGNORE | DISCOVERY-DOC-FU-GITIGNORE: stale untracked busctl binary at repo root is not gitignored | todo | P2 | [task.md](DISCOVERY-DOC-FU-GITIGNORE--9047f6a7/task.md) | — | [DISCOVERY-DOC](../CORE/DISCOVERY-DOC--2d7ce37b/task.md) |
| PROOF-CHECK-FU-RECURSION | PROOF-CHECK-FU-RECURSION: bash scripts/proof-check.sh hangs / spawns runaway processes wh… | todo | P2 | [task.md](PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) | — | [84b76d5e-fe02-4651-9828-caba3d82606b](Proof-command-guard-a-run-pattern-that-matches-no-test-m--84b76d5e/task.md) |
| 637fca2f-0fa6-439a-b6eb-361b681cdf80 | ENV: docker CLI needs an explicit socket+binary shim for agent shells (workaround known,… | todo | P3 | [task.md](ENV-docker-CLI-needs-an-explicit-socket-binary-shim-for--637fca2f/task.md) | — | [DEPLOY-2-FU-CONTAINERNAME](../DEPLOY/DEPLOY-2-FU-CONTAINERNAME--e9dd20b4/task.md) [DEPLOY-2](../DEPLOY/DEPLOY-2--14f8ec3b/task.md) [DEPLOY-3](../DEPLOY/DEPLOY-3--9eaf2d19/task.md) |

## Closed tasks (2) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| 84b76d5e-fe02-4651-9828-caba3d82606b | Proof-command guard: a \`-run\` pattern that matches no test must FAIL, not pass vacuously | done | P0 | [task.md](Proof-command-guard-a-run-pattern-that-matches-no-test-m--84b76d5e/task.md) | — | [DUR-4](../DUR/DUR-4--59c36769/task.md) [DUR-6](../DUR/DUR-6--d56a997d/task.md) |
| cf886bb9-1921-4b17-826b-1ce2f8ef987f | scripts/spec-cloud.sh leaks SPEC_CLOUD_PASSWORD on the \`aws\` argv (readable via /proc/*/c… | done | P2 | [task.md](scripts-spec-cloud.sh-leaks-SPEC_CLOUD_PASSWORD-on-the-a--cf886bb9/task.md) | — | [CORE-1](../CORE/CORE-1--eea035e4/task.md) |

## Epic description

Epic key reserved 2026-08-08 (reservation namespace epic-key, value 2). The tooling an agent in THIS repo can actually change: scripts/proof-check.sh (the vacuous-proof guard and its known families), scripts/spec-cloud.sh (the authed Spec Server shim), .gitignore and repo-root hygiene, and the dev-environment shims agents need to run anything (docker). Deliberately scoped to a single ownership boundary -- scripts/** plus repo-root dotfiles -- so the whole epic is dispatchable to ONE agent, per the rule that an epic sharing an ownership boundary is worth more than one sharing a theme. Explicitly EXCLUDES scripts/bus-*.sh, which are the agent-facing product surface and belong to AGENTIF (invariant 7). Excludes upstream Spec Server defects, which are in PROCESS because they are not fixable from here.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
