# EPIC TOOLING — The repo's own build & verification tooling (scripts/, .gitignore, dev env)

[← all epics](../../SPEC.md)

**11 open / 16 total.** Full records live in `SPEC/TOOLING/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (11)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| 521d68b5-4181-4df6-b3c2-ef660ff5461d | proof-check.sh cannot tell "executed" from "asserted" -- adopt a zero-probe guard convent… | todo | P1 | [task.md](proof-check.sh-cannot-tell-executed-from-asserted-adopt--521d68b5/task.md) | _not fetched_ | [AUTH-2](../AUTH/AUTH-2--4b45a6d8/task.md) [AUTH-1-FU-LISTENADDR](../AUTH/AUTH-1-FU-LISTENADDR--c27f9439/task.md) |
| 71cdaef8-c757-4ba9-a693-a8f744070d08 | proof-check.sh runs the proof against its OWN script directory repo root, not the callers… | in_progress | P1 | [task.md](proof-check.sh-runs-the-proof-against-its-OWN-script-dir--71cdaef8/task.md) | _not fetched_ | [RELAY-27](../RELAY/RELAY-27--f417c6a0/task.md) [RELAY-27](../RELAY/RELAY-27--c2486740/task.md) [PROOF-CHECK-FU-RECURSION](PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) [cea09b96-72db-40f1-84b4-c2e227eae1cf](proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) |
| a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd | Conjunction-masking vacuous-proof family: filtered-clause proof_cmds hidden by an unfilte… | todo | P1 | [task.md](Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) | _not fetched_ | [ID-2-WIRING-SEAL](../ID/ID-2-WIRING-SEAL--8c9b6489/task.md) [DUR-12](../DUR/DUR-12--cbc9ab0c/task.md) [ID-2-WIRING-OBSERVER](../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) [DUR-3](../DUR/DUR-3--d8a991ea/task.md) |
| aa2dfd79-9bc5-4e0a-925a-824168b710be | scripts/spec-cloud.sh: Cognito bearer token is on curl's argv, readable via /proc/*/cmdli… | todo | P1 | [task.md](scripts-spec-cloud.sh-Cognito-bearer-token-is-on-curl-s--aa2dfd79/task.md) | _not fetched_ | [CONTEXT-SPEC-TREE](../CONTEXT/CONTEXT-SPEC-TREE--ff15e9ff/task.md) |
| DISCOVERY-DOC-FU-GITIGNORE | DISCOVERY-DOC-FU-GITIGNORE: stale untracked busctl binary at repo root is not gitignored | todo | P2 | [task.md](DISCOVERY-DOC-FU-GITIGNORE--9047f6a7/task.md) | _not fetched_ | [DISCOVERY-DOC](../CORE/DISCOVERY-DOC--2d7ce37b/task.md) |
| PROOF-CHECK-FU-RECURSION | PROOF-CHECK-FU-RECURSION: bash scripts/proof-check.sh hangs / spawns runaway processes wh… | todo | P2 | [task.md](PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) | _not fetched_ | [84b76d5e-fe02-4651-9828-caba3d82606b](Proof-command-guard-a-run-pattern-that-matches-no-test-m--84b76d5e/task.md) |
| TOOLING-2 | TOOLING-2: make docs/THREE-BUS-DOCKER.md's bash blocks a repeatable executable check | todo | P2 | [task.md](TOOLING-2--87d9e8d1/task.md) | _not fetched_ | [DEPLOY-6](../DEPLOY/DEPLOY-6--e12b75cd/task.md) |
| fe0d9030-f95f-49b9-ab3b-68c96860df8a | proof-check.sh cannot authenticate go test evidence against adversarial TestMain output | todo | P2 | [task.md](proof-check.sh-cannot-authenticate-go-test-evidence-agai--fe0d9030/task.md) | _not fetched_ | [cea09b96-72db-40f1-84b4-c2e227eae1cf](proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) |
| 3dbbd034-d699-4184-aec8-7efe59dd5c67 | Reconcile derived prose references against real blocks edges and report the ranked gap | todo | P3 | [task.md](Reconcile-derived-prose-references-against-real-blocks-e--3dbbd034/task.md) | _not fetched_ | [CONTEXT-SPEC-DEPS](../CONTEXT/CONTEXT-SPEC-DEPS--8280358d/task.md) |
| 637fca2f-0fa6-439a-b6eb-361b681cdf80 | ENV: docker CLI needs an explicit socket+binary shim for agent shells (workaround known,… | todo | P3 | [task.md](ENV-docker-CLI-needs-an-explicit-socket-binary-shim-for--637fca2f/task.md) | _not fetched_ | [DEPLOY-2-FU-CONTAINERNAME](../DEPLOY/DEPLOY-2-FU-CONTAINERNAME--e9dd20b4/task.md) [DEPLOY-2](../DEPLOY/DEPLOY-2--14f8ec3b/task.md) [DEPLOY-3](../DEPLOY/DEPLOY-3--9eaf2d19/task.md) |
| de0fc1df-a948-4b44-95a4-4b9d01cab267 | DECISIONS.md HTML-comment section fences are imbalanced (6 BEGIN / 8 END) -- introduced b… | todo | P3 | [task.md](DECISIONS.md-HTML-comment-section-fences-are-imbalanced--de0fc1df/task.md) | _not fetched_ | [MTLS-CLIENTAUTH](../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) [RELAY-24-BLOCKER-HUBINGEST](../RELAY/RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) [CLI-11](../UNASSIGNED/CLI-11--bf966c07/task.md) [INVITE-GATE](../INVITE/INVITE-GATE--05a5216d/task.md) [RELAY-45](../RELAY/RELAY-45--4be32336/task.md) |

## Closed tasks (5) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| 84b76d5e-fe02-4651-9828-caba3d82606b | Proof-command guard: a \`-run\` pattern that matches no test must FAIL, not pass vacuously | done | P0 | [task.md](Proof-command-guard-a-run-pattern-that-matches-no-test-m--84b76d5e/task.md) | _not fetched_ | [DUR-4](../DUR/DUR-4--59c36769/task.md) [DUR-6](../DUR/DUR-6--d56a997d/task.md) |
| 9e6544a1-d606-4e65-8c43-0764ac3f0aa4 | spec-cloud.sh / task-list workflow: GET .../tasks silently truncates to the oldest 200 of… | superseded | P1 | [task.md](spec-cloud.sh-task-list-workflow-GET-...-tasks-silently--9e6544a1/task.md) | _not fetched_ | [SPEC-API-LIST-SILENT-TRUNCATION](../UNASSIGNED/SPEC-API-LIST-SILENT-TRUNCATION--82f35b73/task.md) |
| TOOLING-1 | TOOLING-1: read-only linter for mechanically broken stored proof_cmd values | done | P1 | [task.md](TOOLING-1--eeb4109b/task.md) | _not fetched_ | — |
| cea09b96-72db-40f1-84b4-c2e227eae1cf | proof-check.sh: subtest SKIP/PASS lines invisible to plain-text counter -- parent-PASS/al… | done | P1 | [task.md](proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) | _not fetched_ | [SIGN-3](../SIGN/SIGN-3--f2daa6bc/task.md) [84b76d5e-fe02-4651-9828-caba3d82606b](Proof-command-guard-a-run-pattern-that-matches-no-test-m--84b76d5e/task.md) [a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd](Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) [fe0d9030-f95f-49b9-ab3b-68c96860df8a](proof-check.sh-cannot-authenticate-go-test-evidence-agai--fe0d9030/task.md) |
| cf886bb9-1921-4b17-826b-1ce2f8ef987f | scripts/spec-cloud.sh leaks SPEC_CLOUD_PASSWORD on the \`aws\` argv (readable via /proc/*/c… | done | P2 | [task.md](scripts-spec-cloud.sh-leaks-SPEC_CLOUD_PASSWORD-on-the-a--cf886bb9/task.md) | _not fetched_ | [CORE-1](../CORE/CORE-1--eea035e4/task.md) |

## Epic description

Epic key reserved 2026-08-08 (reservation namespace epic-key, value 2). The tooling an agent in THIS repo can actually change: scripts/proof-check.sh (the vacuous-proof guard and its known families), scripts/spec-cloud.sh (the authed Spec Server shim), .gitignore and repo-root hygiene, and the dev-environment shims agents need to run anything (docker). Deliberately scoped to a single ownership boundary -- scripts/** plus repo-root dotfiles -- so the whole epic is dispatchable to ONE agent, per the rule that an epic sharing an ownership boundary is worth more than one sharing a theme. Explicitly EXCLUDES scripts/bus-*.sh, which are the agent-facing product surface and belong to AGENTIF (invariant 7). Excludes upstream Spec Server defects, which are in PROCESS because they are not fixable from here.

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
