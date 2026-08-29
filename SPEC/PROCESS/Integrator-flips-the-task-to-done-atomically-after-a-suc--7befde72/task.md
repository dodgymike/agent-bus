# Integrator flips the task to done atomically after a successful commit -- close the commit-&gt;complete hand-off gap

| Field | Value |
| --- | --- |
| Public id | `7befde72-488e-4cf4-a05b-b16e2c2ffd15` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | backlog-integrity, done-not-flipped |
| Created | 2026-08-23T09:47:24.952456+00:00 |
| Updated | 2026-08-23T09:47:24.952456+00:00 |
| Completed | — |

## Proof command

```sh
grep -qiE "complete|flip the task" .claude/agents/integrator.md && grep -q "integrator" DECISIONS.md  # placeholder: real proof asserts integrator.md carries the post-commit flip step scoped to fully-done reports, and DECISIONS.md records the only-spec-keeper narrowing
```

## Description

ROOT CAUSE (see DONE-NOT-FLIPPED_DEEPDIVE.md). The commit and the done-flip are owned by two DIFFERENT agents dispatched at two DIFFERENT times, and nothing binds them. feature-runner is code-only and explicitly must NOT complete (.claude/agents/feature-runner.md:35, DoD line 101 says flip is via spec-keeper). integrator is the ONLY committer but is FORBIDDEN to touch task state (.claude/agents/integrator.md:58-60 / the What-you-never-do list). So after the integrator commits, the flip falls to a SEPARATELY-dispatched spec-keeper pass that is frequently never made, or that runs a bookkeeping/audit pass and DELIBERATELY declines to flip. Measured 2026-08-22 at HEAD 7c79c15: of 23 in_progress tasks, ~11 are fully done with only the complete call missing, and 7 of 9 in_progress P0s are effectively done (evidence: spec-keeper IN-PROGRESS AUDIT notes 2026-08-08 that bucket e120153b, 94159d93, 10e93262, IDEM-18, MTLS-VERIFY, MTLS-CLIENTCERT as SHIPPED/left in_progress).

FIX: give the integrator ONE mutation -- immediately after a successful pathspec commit AND the HEAD-compiles check, POST tasks/<id>/complete with the sha it just minted and the owning agent report quoted proof-check verdict, for every task that report lists as FULLY done (docs/CLI included). This requires a narrowing of the only-spec-keeper-mutates rule, recorded in DECISIONS.md, scoped to the single complete call for the task(s) the integrator just committed.

LANDMINES to honour: (1) BLOCKED-ON 48be31d6 -- the sandbox guard false-matches complete in the URL, so an isolated integrator cannot POST it; that guard fix must land first or the integrator runs non-isolated. (2) ONE commit can carry N tasks (dc04a95, 6985d2c); the integrator must complete ALL tasks that commit closes, not one. (3) committed != running: do NOT auto-flip a task whose report says code-only/awaiting-docs (invariant 7 CLI+AGENT_PROTOCOL in same task) or awaiting-deploy -- leave in_progress with a status_note. Auto-flip ONLY on a report that states fully-done.

Builds on 48be31d6 (guard) and complements HANDOVER-BACKLOG-RECONCILE 43d14776 (manual sweep for the existing backlog).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [10e93262-8e34-4738-b435-bfe23d880057](../../MTLS/Derive-the-bus-fingerprint-from-the-certificate-not-the--10e93262/task.md) — Derive the bus fingerprint from the certificate, not the log; correct the CONTRACTS-CLI e… (done)
- [48be31d6-7642-42ab-a5d4-fe2f2aa5d54a](../../UNASSIGNED/Worktree-isolation-sandbox-bash-guard-false-matches-the--48be31d6/task.md) — Worktree-isolation sandbox bash guard false-matches the substring "complete" in the Spec… (todo)
- [HANDOVER-BACKLOG-RECONCILE](../../HANDOVER/HANDOVER-BACKLOG-RECONCILE--43d14776/task.md) — HANDOVER-BACKLOG-RECONCILE: the inherited backlog stops lying about what is in flight (todo)
- [IDEM-18](../../IDEM/IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (in_progress)
- [MSG-FU-SUFFIXFLOOR](../../ID/MSG-FU-SUFFIXFLOOR--94159d93/task.md) — MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now… (in_progress)
- [MTLS-CLIENTCERT](../../MTLS/MTLS-CLIENTCERT--0bc7a2eb/task.md) — MTLS-CLIENTCERT: the client generates and stores its own TLS keypair + self-signed certif… (done)
- [MTLS-VERIFY](../../MTLS/MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: prove a RUNNING bus is TLS-only and enforces the current RequestClientCert p… (done)
- [e120153b-9d8a-4b6a-bd4e-89431954496b](../../DUR/Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) — Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a na… (in_progress)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
