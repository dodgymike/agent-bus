# ORCHESTRATION — choosing, sizing and convening sub-agents

Read this **before spawning any sub-agent**. It is deliberately NOT in `CLAUDE.md`: `CLAUDE.md` is
injected into every agent on every spawn, and only four of the fourteen agent types (`backlog-triage`,
`feature-runner`, `deep-diver`, and the orchestrator itself) ever spawn anything. The other ten paid
for this text on every spawn and never used it.

`CLAUDE.md` keeps the *rules* — the bare roster, "always pass `model`", and the mandatory chain. This
file keeps the *rationale and the detail*: what each agent is for, and how to pick a model.

---

## Model selection — ALWAYS pass a `model` when spawning a sub-agent

Do NOT let sub-agents silently inherit the session model — choose per task and pass `model`
explicitly:

- **`sonnet` (exact id `claude-sonnet-5`)** — mechanical, well-scoped, pattern-driven, or
  writing-heavy work: doc writing, test authoring, single-file implementations, CLI subcommands,
  SPEC/status bookkeeping (spec-keeper). **Default to Sonnet when a task is routine.**
- **`opus` (exact id `claude-opus-5`)** — judgment, design, investigation, or correctness-critical
  work: the durability/2PC design, recovery semantics, the relay/federation protocol, id authority,
  auth, the security and reviewer gates, and anything where a wrong call is expensive.

`feature-runner` is the volume driver and is single-model (opus) — **OVERRIDE per task**: pass
`model: "sonnet"` for a mechanical feature, `model: "opus"` only for a design-/correctness-heavy one.

The exact model id matters beyond the spawn: every agent posts a `kind=model` note to the Spec Server
task, and the git footer is a fixed string, so those notes are the only auditable cost signal.

## Agent roster (`.claude/agents/`)

- **planner** — breaks large requests into an atomic, ordered implementation plan.
- **spec-keeper** — owns task state (drives the Spec Server API). The only agent that mutates it.
- **implementer** — writes the code for exactly one task. Narrow and cheap; no `Agent` tool, so it
  cannot fan out or run the review chain — that work belongs to feature-runner.
- **test-engineer** — writes/improves automated tests and runs the narrowest check.
- **reviewer** — correctness, style, maintainability, scope.
- **security** — vulnerabilities, leaked secrets, authn/authz gaps, id spoofing.
- **documentation** — README, `AGENT_PROTOCOL.md`, `PROTOCOL.md`, `CONTRACTS-*.md`, changelog.
- **deep-diver** — root-cause investigation, writes `<TOPIC>_DEEPDIVE.md`.
- **architecture-reviewer** — component boundaries, data flow, the durability and relay planes.
- **performance-reviewer** — latency, throughput, lock contention, fsync cost, long-poll scale.
- **reliability-reviewer** — crash-consistency, recovery, delivery guarantees, relay partial failure.
- **backlog-triage** — decides what deserves doing now and dispatches sub-agents. Never edits code.
  Takes the `TRIAGE-LOCK` task first; two triage agents dispatching at once produce two sub-agents
  editing the same file.
- **feature-runner** — runs ONE task end-to-end through the mandated chain, code-only, parallel-safe.
  Use it INSTEAD OF `general-purpose` for any change that touches app code (Go server, the
  `cmd/agent-busctl` CLI, protocol docs). Give it the task **and its file-ownership boundary**.
- **integrator** — the ONLY agent permitted to `git commit`. Verifies gates COMPLETED, that the
  commit is pathspec-scoped, that HEAD compiles afterwards, and that the message matches the
  evidence — then commits, or REFUSES with a reason. Added 2026-08-07 because every commit-time
  failure in this repo was mechanical and repeated: ungated code shipped three times, four
  index-sweeping mis-titled commits, and one `main` left un-compilable because a package was verified
  against the working tree rather than HEAD.

## Review panel (full-system review)

Before a large change, or as a periodic audit, convene architecture-reviewer + reliability-reviewer +
performance-reviewer + security + test-engineer (+ reviewer for code-level). Run them **READ-ONLY in
parallel**, each emitting findings to its own doc, then synthesize into a single prioritized
P0/P1/P2 backlog. None of the reviewers edit code.

## Dispatching well

- Send independent spawns in ONE message so they run concurrently.
- Give every agent its **file-ownership boundary**. Boundaries have collided in this repo precisely
  because agents quietly reached one file further; widening a boundary in one message is far cheaper
  than untangling two agents' edits to the same file.
