# agent-bus — Development Protocol

**agent-bus** is a small, very durable inter-agent message bus written in Go. Claude Code agents
enrol with it, wait on an HTTP long-poll, and broadcast or DM each other. Multiple buses relay to
each other. Agents drive it entirely through shell wrappers — **an agent should never have to
construct an HTTP call.**

Always follow the backlog. Task state is the **Spec Server** (source of truth, project slug
`agent-bus`); `SPEC.md` is its generated mirror — see the "Spec Server" section below.
Always use your agents when changing code: planner → spec-keeper → implementer → test-engineer →
reviewer → security → documentation.

## What this project is (the standing design contract)

These are the load-bearing invariants. Every change is measured against them; a change that weakens
one needs an explicit decision recorded in `DECISIONS.md`.

1. **The server is AUTHORITATIVE on every id.** Bus ids, agent ids, message ids, and sequence
   numbers are minted by the server and never by a client. A client-supplied id is input to be
   validated, never an identity to be trusted. Ids are never reused, including across restarts.
2. **Every agent id is fully qualified: `<bus-id>.<agent-id>`.** That namespacing is what makes
   cross-bus routing and agent-list exchange unambiguous. Buses have ids for the same reason.
3. **Enrolment issues a signed credential.** The agent presents a key at enrolment; the server signs
   it and returns a token the agent authenticates with on every subsequent call. Every route except
   enrolment authenticates.
4. **Nothing is acknowledged before it is durable.** A send returns success only after the message
   is committed via the two-phase (prepare → commit) write path and fsynced. Never trade that for
   latency.
5. **Memory is the serving copy; disk is the truth.** State is held in memory for speed and rebuilt
   by replaying the durable store on start. A crash at any point must recover to a state that is a
   prefix of the accepted history — no torn records, no acknowledged-but-lost messages.
6. **Every message is also written to an append-only log — METADATA AND ROUTING INFO ONLY.** The log
   is the audit trail: message id, sequence, sender, recipient(s), bus path traversed, timestamp,
   size, and content hash. It does **not** record message bodies. That is a deliberate decision
   (2026-08-02) taken so the audit trail stays compatible with end-to-end encrypted, forward-secret
   payloads — a log holding plaintext would be unwritable the moment PFS lands, and a log holding
   ciphertext it can never decrypt would be dead weight. The log is append-only in the strict sense:
   no in-place edits, no truncation except a verified-corrupt tail during recovery.
7. **Agents never hand-write HTTP.** Every capability ships with a `scripts/bus-*.sh` wrapper and an
   `AGENT_PROTOCOL.md` entry **in the same task**. A feature without its wrapper is not done.
8. **Simple beats clever.** Go stdlib first. A third-party dependency needs a justification in
   `DECISIONS.md`.
9. **NEVER write your own crypto.** This is absolute and overrides every other preference in this
   file, including invariant 8's stdlib-first bias and any argument from simplicity, elegance,
   dependency count, or performance. Always use a well-known, standard, audited crypto library, and
   pick the one that **wraps as much of the problem as possible** — prefer a high-level,
   misuse-resistant API (`crypto_sign`-style sign/verify, sealed boxes) over assembling primitives
   yourself. Specifically forbidden without explicit user consent recorded in `DECISIONS.md`:
   implementing or "adapting" a cipher, hash, MAC, KDF, signature scheme, key exchange, or ratchet;
   hand-rolling a padding, nonce, or IV scheme; inventing a bespoke construction out of otherwise-
   good primitives. The reason this outranks everything else is that broken crypto **fails
   silently** — it still encrypts, it still verifies, it simply provides none of the protection it
   appears to. No ordinary test suite detects it, so "our tests pass" is not evidence. When no
   suitable library exists, the answer is to change the requirement or stop and ask — never to
   write it yourself.

## Repository layout

```
cmd/agent-bus/        main — the server binary
internal/…            server packages (ids, store, wal, hub, http, relay, auth)
scripts/bus-*.sh      the agent-facing wrappers — the ONLY interface agents use
scripts/spec-cloud.sh authed curl shim for the Spec Server (task state)
AGENT_PROTOCOL.md     agent-facing instructions: enrol, list, wait, send, relay
PROTOCOL.md           the wire protocol + on-disk format (human/maintainer facing)
CONTRACTS.md          every route, flag, env var, record type — updated with each change
DECISIONS.md          design decisions and their rationale (append-only, dated)
AGENT_LOG.md          per-task work log (append-only, dated)
SPEC.md               GENERATED mirror of the Spec Server backlog — never hand-edit
```

## Runtime target: Docker Compose

**agent-bus ships as a container and runs under Docker Compose.** The deployment target — not this
workstation — defines the toolchain. The box's ambient `go` is go1.19.4, but that is an accident of
the dev machine, not a constraint on the product: the Go version is whatever the builder image
pins, and it is chosen to satisfy the E2E-crypto requirements (the Signal-style ratchet needs
`crypto/ecdh`, which is go1.20+, and a current libsignal-compatible stack wants newer still).

Consequences:
- `go.mod` pins the version the CONTAINER builds with. If you need a newer language or stdlib
  feature for a real requirement, bump it — and record the bump in `DECISIONS.md`.
- A local `go build` may therefore fail on this box while CI/the container is green. That is
  expected. When it happens, build in the container rather than downgrading the code.
- This does NOT license casual dependency growth. Invariant 8 still holds: stdlib first, and a
  third-party dependency still needs a justification in `DECISIONS.md`. The relaxation is about the
  Go VERSION, not about pulling in libraries.

## Go conventions

- `gofmt -l .` must be empty, `go vet ./...` clean, `go build ./...` green before any commit.
- Tests run with `-race`. Concurrency here is the product; a data race is a P0.
- Durability and recovery code must have **crash-injection tests** — a test that writes, kills at a
  chosen point in the write path, and asserts what recovery yields. "The code looks right" is not
  evidence for a durability claim.
- Prefer table-driven tests. Keep the narrowest check runnable in seconds.

## Verify — and tell the truth

Run the NARROWEST relevant check: `go test -race -run <Name> ./internal/<pkg>`, `go build ./...`,
`go vet ./...`, `gofmt -l .`. For anything agent-facing, ALSO exercise it the way an agent would:
through `scripts/bus-*.sh` against a running server, not through a hand-written `curl`. If the
wrapper doesn't work, the feature doesn't work.

If a test fails, you are NOT done. Diagnose whether YOUR change caused it or it is pre-existing,
name the exact failing test, and report the verdict. NEVER hand-wave "pre-existing failures" to
declare success.

## Model selection — ALWAYS pass a `model` when spawning a sub-agent

Do NOT let sub-agents silently inherit the session model — choose per task and pass `model`
explicitly:
- **`sonnet` (exact id `claude-sonnet-5`)** — mechanical, well-scoped, pattern-driven, or
  writing-heavy work: doc writing, test authoring, single-file implementations, shell wrappers,
  SPEC/status bookkeeping (spec-keeper). **Default to Sonnet when a task is routine.**
- **`opus` (exact id `claude-opus-5`)** — judgment, design, investigation, or correctness-critical
  work: the durability/2PC design, recovery semantics, the relay/federation protocol, id authority,
  auth, the security and reviewer gates, and anything where a wrong call is expensive.

`feature-runner` is the volume driver and is single-model (opus) — OVERRIDE per task: pass
`model: "sonnet"` for a mechanical feature, `model: "opus"` only for a design-/correctness-heavy one.

## Spec Server — task management (ALWAYS use spec-keeper)

Task state lives in the **Spec Server**, project slug **`agent-bus`**. The PRIMARY store is the
CLOUD (`https://api.spec.elasticninja.com`). Every spec API call MUST go through the authed wrapper
**`bash scripts/spec-cloud.sh <curl-opts> /api/v1/…`** (a drop-in for `curl`): it finds the `/path`
arg, prepends the cloud host, and injects a fresh Cognito bearer (cached ~40 min, auto-refreshed on
401). Creds live OUTSIDE the repo, never committed
(`/mnt/sdc/mike/claude-scratch/spec-cloud-creds.env`).

Health check: `bash scripts/spec-cloud.sh -sf /readyz`. If it fails, fall back to the local server
(`cd ~/source/spec-keeper && docker compose up -d`, then `curl -s localhost:8080/api/v1/…`) and
re-sync to cloud later. Set `B=/api/v1` for brevity.

**spec-keeper is the ONLY agent that mutates task state.** Drive each atomic increment through the
API, never by hand-editing a file:
- **Pick the next task** → `POST $B/projects/agent-bus/tasks/claim-next {"agent":"<you>"}` — never
  scan-and-pick (two agents would collide); claim is atomic. 204 = backlog empty.
- **Mark a task done** → `POST $B/projects/agent-bus/tasks/<id>/complete
  {"commit_sha":"…","test_summary":"…","proof_cmd":"…"}`.
- **Reserve numbered resources** (on-disk record-type numbers, wire protocol versions, epic task
  keys) → `POST $B/projects/agent-bus/reservations {"namespace":"<ns>","reserved_by":"<you>"}`.
  **Never pick a number by eyeballing the list** — that is the classic parallel-agent collision.
- **Your own tasks** → `GET $B/projects/agent-bus/tasks?owner=<you>`.
- **Refresh the mirror** after mutations → `bash scripts/spec-cloud.sh -s
  $B/projects/agent-bus/export > SPEC.md`. That is the ONLY write anyone makes to `SPEC.md`.

## Spec Server task notes are the work JOURNAL

Every task accumulates an append-only note journal. Four `kind=` types (every body is prefixed
`kind=<type>;` — machine-parseable):
- `kind=request` — the ask. The ORCHESTRATOR (`author=main`) posts the user's request that spawned
  the task, and the brief handed to each agent.
- `kind=report` — what was done. EVERY agent posts one on completion: approach, files changed,
  findings/evidence — concise.
- `kind=response` — the verdict. Reviewers / security / the review panel post PASS/FAIL/CHANGES +
  key points. The orchestrator (`main`) posts decisions made and what was reported to the user.
- `kind=model` — `model=<exact-id>; tokens_in=<N>; tokens_out=<N>; tokens_total=<N>`. Every agent
  posts one; the git footer is a fixed string, so these notes are the auditable cost signal.

```
bash scripts/spec-cloud.sh -s -X POST /api/v1/projects/agent-bus/tasks/<task-id>/notes \
  -H 'Content-Type: application/json' \
  -d '{"body":"kind=report; <text>","author":"<agent-slug>"}'
```

`author` = your agent slug, or `main` for the orchestrator. Do NOT flip a task to `done` until each
agent that touched it has posted at minimum `kind=report` + `kind=model`.

## Work in atomic increments

1. Read the backlog (via the API) before changing code.
2. Claim exactly one task — `POST .../tasks/claim-next {"agent":"<you>"}`.
3. Restate the task in one sentence.
4. Make the smallest code change that completes only that task.
5. Run the narrowest relevant check (see "Verify" above). For durability/recovery work, that
   includes the crash-injection test.
6. Commit with a descriptive message + short tldr, on branch `main`.
7. Mark the task done via `complete` (with `commit_sha`, `test_summary`, `proof_cmd`), add any
   discovered follow-ups, refresh the `SPEC.md` mirror, and post the journal notes.
8. Record decisions in `DECISIONS.md`; append to `AGENT_LOG.md`.
9. Update `CONTRACTS.md` (every new/changed route, flag, env var, record type) and — if the
   agent-facing surface moved — `AGENT_PROTOCOL.md` plus the `scripts/bus-*.sh` wrappers.
10. **Tidy-up & git hygiene — a task is NOT complete until ALL of these hold:**
    - `git status --porcelain` is EMPTY. Every file you created or changed, including outside the
      Edit tool (gofmt, chmod, generators, renames), is committed or gitignored. New files MUST be
      `git add`ed.
    - No scratch in the repo: temp goes under `/tmp`, never a tracked path.
    - The task is flipped to `done` in the Spec Server by spec-keeper.
    - One logical commit for the task.
    - The mandated chain actually ran (reviewer AND security AND documentation for code changes), or
      it is explicitly recorded in `AGENT_LOG.md` WHY one was skipped.
11. Stop and report: files changed · test result · `git status` clean · next recommended task.

Do not batch unrelated tasks. Do not refactor unless the task explicitly asks for it. If the spec is
wrong or incomplete, fix the spec first (via spec-keeper), then continue. A task is not complete
until documentation is updated.

For tasks that require permission multiple times, write a script and ask permission once.

## Parallel-agent coordination

- **Numbers are reserved, not chosen** — record-type numbers, protocol versions, epic task keys. Use
  `POST .../reservations`; it allocates a unique monotonic value so two agents never collide.
- **Task state is coordinated by the Spec Server, not by file locks.** `claim-next` hands each agent
  a distinct task; `SPEC.md` is a GENERATED MIRROR — never hand-edit it concurrently.
- For the remaining shared files (`DECISIONS.md`, `AGENT_LOG.md`, `CONTRACTS.md`), only ONE agent at
  a time; prefer adding a new dated section over editing existing lines.
- Never run two agents against the same bus **data directory**. Each parallel run gets its own
  throwaway dir under `/tmp`; the tracked `data/` dir is not a test fixture.

## Agent roster (`.claude/agents/`)

- **planner** — breaks large requests into an atomic, ordered implementation plan.
- **spec-keeper** — owns task state (drives the Spec Server API). The only agent that mutates it.
- **implementer** — writes the code for exactly one task.
- **test-engineer** — writes/improves automated tests and runs the narrowest check.
- **reviewer** — correctness, style, maintainability, scope.
- **security** — vulnerabilities, leaked secrets, authn/authz gaps, id spoofing.
- **documentation** — README, `AGENT_PROTOCOL.md`, `PROTOCOL.md`, `CONTRACTS.md`, changelog.
- **deep-diver** — root-cause investigation, writes `<TOPIC>_DEEPDIVE.md`.
- **architecture-reviewer** — component boundaries, data flow, the durability and relay planes.
- **performance-reviewer** — latency, throughput, lock contention, fsync cost, long-poll scale.
- **reliability-reviewer** — crash-consistency, recovery, delivery guarantees, relay partial failure.
- **backlog-triage** — decides what deserves doing now and dispatches sub-agents. Never edits code.
- **feature-runner** — runs ONE task end-to-end through the mandated chain, code-only, parallel-safe.

**Review panel (full-system review):** before a large change or as a periodic audit, convene
architecture-reviewer + reliability-reviewer + performance-reviewer + security + test-engineer
(+ reviewer for code-level). Run them READ-ONLY in parallel, each emitting findings to its own doc,
then synthesize into a single prioritized P0/P1/P2 backlog. None of the reviewers edit code.

For ANY code change the chain spec-keeper → implementer → reviewer → security is MANDATORY; skipping
a step requires an explicit one-line justification in `AGENT_LOG.md`.
