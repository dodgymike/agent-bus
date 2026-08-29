# Make security skip the default for docs-and-tests-only changes, with a guard-file carve-out

| Field | Value |
| --- | --- |
| Public id | `97a315af-70b3-4a64-8456-92335d8c9631` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | done |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T08:22:02.300663+00:00 |
| Updated | 2026-08-22T10:30:46.576334+00:00 |
| Completed | 2026-08-22T10:30:46.576317+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/doc-check.sh section CLAUDE.md "## Agent roster (\`.claude/agents/\`)" "ONLY docs and tests" "no GUARD file" "no CONTROL-PLANE file" && bash scripts/doc-check.sh section AGENTS.md "## Agent roster (\`.claude/agents/\`)" "ONLY docs and tests" "no GUARD file" "no CONTROL-PLANE file" && cmp CLAUDE.md AGENTS.md'
```

## Status note

NOT absorbed -- correction 2026-08-22. This task is being implemented by a live documentation agent; its text is in the worktree at CLAUDE.md:390-395. It should be COMPLETED on its own terms. The tiered-chain task e4e31233 later SUPERSEDES this wording, replacing it with the tiered rule; that is succession, not absorption.

## Description

CLAUDE.md already permits skipping a chain step with a one-line AGENT_LOG.md justification, but in practice security runs on nearly everything including docs-only work. Invert it: for a change touching only documentation and test files, skipping security is the DEFAULT and running it is what needs a reason.

Careful with the boundary. A test file can carry security-relevant content: a test that weakens or deletes a guard is exactly the change that must not skip review. INVARIANTS.md invariant 11 and client/guard_test.go are the standing example -- deleting the InsecureSkipVerify line or its paired VerifyPeerCertificate callback silently disables pinning while every positive test still passes. So the rule needs to be "docs and tests only AND no guard/AST-guard/security-test file touched", not "docs and tests only".

Definition of done: the rule is written in CLAUDE.md with that carve-out explicit, AGENTS.md is re-synced (the two have drifted before -- PITFALLS.md section 5), and the enumeration of what counts as a guard file is concrete enough to apply without judgment.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [4d990ef4-23ee-4971-ab00-84eb5ec137ae](../Write-docs-CHANGE-TIERS.md-the-normative-tier-and-signal--4d990ef4/task.md)
- **relates to** [748f6366-1a46-462e-b452-f024f607976b](../claude-agents-security.md-scope-the-security-gate-by-tie--748f6366/task.md)
- **relates to** [aeae5c7d-33f0-4ba1-a420-873bec8203d1](../claude-agents-integrator.md-the-commit-gate-must-consult--aeae5c7d/task.md)
- **superseded by** [e4e31233-cabe-4af4-986b-f28c84347214](../CLAUDE.md-replace-the-flat-mandated-chain-rule-with-the--e4e31233/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [e4e31233-cabe-4af4-986b-f28c84347214](../CLAUDE.md-replace-the-flat-mandated-chain-rule-with-the--e4e31233/task.md) — CLAUDE.md: replace the flat mandated-chain rule with the tiered one-liner, byte-neutral,… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [255bdc5a-f36e-4cfb-a484-199fbd6d16ab](../change-tier.sh-package-to-invariant-plane-partition-with--255bdc5a/task.md) — change-tier.sh: package to invariant-plane partition, with DEFAULT-DENY for unmapped paths (todo)
- [4d990ef4-23ee-4971-ab00-84eb5ec137ae](../Write-docs-CHANGE-TIERS.md-the-normative-tier-and-signal--4d990ef4/task.md) — Write docs/CHANGE-TIERS.md, the normative tier and signal specification (todo)
- [4faa6782-6b49-4507-9a23-bb2cf42e7d02](../PITFALLS.md-a-proof-that-stays-RED-against-a-tree-where--4faa6782/task.md) — PITFALLS.md: a proof that stays RED against a tree where the work IS done (todo)
- [748f6366-1a46-462e-b452-f024f607976b](../claude-agents-security.md-scope-the-security-gate-by-tie--748f6366/task.md) — .claude/agents/security.md: scope the security gate by tier (todo)
- [ac674a01-a471-4231-892b-caaff5d4146e](../97a315af-FU-COSMETIC-NITS-clean-up-three-post-landing-do--ac674a01/task.md) — 97a315af-FU-COSMETIC-NITS: clean up three post-landing doc nits in the docs-and-tests sec… (todo)
- [aeae5c7d-33f0-4ba1-a420-873bec8203d1](../claude-agents-integrator.md-the-commit-gate-must-consult--aeae5c7d/task.md) — .claude/agents/integrator.md: the commit gate must consult scripts/change-tier.sh, not it… (todo)
- [c9e89d5a-6f6f-475e-8c8e-24f663a060bc](../Explicit-manifest-of-security-bearing-test-files-as-a-th--c9e89d5a/task.md) — Explicit manifest of security-bearing test files, as a third guard check alongside the tw… (cancelled)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
