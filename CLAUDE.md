# agent-bus — Development Protocol

**agent-bus** is a small, very durable inter-agent message bus written in Go. Claude Code agents
enrol with it, wait on an HTTP long-poll, and broadcast or DM each other. Multiple buses relay to
each other. Agents drive it entirely through the compiled Go CLI (`cmd/agent-busctl`) — **an agent
should never have to construct an HTTP call.** The `scripts/bus-*.sh` wrappers are RETIRED; do not
add one (invariant 7).

Always follow the backlog. Task state is the **Spec Server** (source of truth, project slug
`agent-bus`); `SPEC.md` is its generated mirror — see the "Spec Server" section below.
Always use your agents when changing code: planner → spec-keeper → implementer → test-engineer →
reviewer → security → documentation.

## What this project is (the standing design contract)

These are the load-bearing invariants, stated as rules. Every change is measured against them; a
change that weakens one needs an explicit decision recorded in `DECISIONS.md`.

> **These state what MUST be true, not what IS true today.** Several are only partly enforced in
> code: the server REQUESTS but never REQUIRES a client certificate
> (`ClientAuth: tls.RequestClientCert`, `a97f854`), recipients CANNOT verify message signatures,
> and enrol idempotency is in-memory only. Enrolment IS invite-gated as of `3cedcb7` —
> `enrolmentInviteRequired = true`, `cmd/agent-bus/main.go:67`. Do not build on a guarantee without
> checking it holds — `INVARIANTS.md` "Enforcement status" carries the per-item evidence.

**The REASONING lives in `INVARIANTS.md`, and you must read the relevant entry IN FULL before working
on that plane.** The lines below are reminders, not specifications — each one is a summary of several
paragraphs that exist because an agent already violated the short version. If a task touches ids,
auth, durability, the log, the CLI surface, crypto, idempotency or TLS, open `INVARIANTS.md` first.

1. **The server is AUTHORITATIVE on every id.** Bus, agent and message ids and sequence numbers are
   minted by the server, never by a client. **Ids are never reused, including across restarts.** When
   recovery discards a record the sequence advances past the hole; it never rewinds.
   **REAFFIRMED WITHOUT NARROWING (2026-08-02) — contrast invariant 4, which WAS deliberately
   narrowed.** A salvage path that reuses the index of a damaged tail record is a **DEFECT to fix,
   not a licence to narrow this invariant.** If closing a reissue gap seems to require narrowing
   invariant 1, you have the wrong fix.
2. **Every agent id is fully qualified: `<bus-id>.<agent-id>`.** Never shorten it — that namespacing
   is what makes cross-bus routing unambiguous.
3. **Enrolment is INVITE-ONLY, and the CLIENT signs a SERVER-PROVIDED session token** (not the
   reverse). Invites are single-use, expiring, revocable, and are the ONLY way onto the bus. Sessions
   last at most one hour, are opaque server-side handles rather than signed claims (which is what
   makes immediate revocation possible), and do not survive a restart. Every route authenticates
   except the routes on the explicit allow-list in `internal/httpapi/authmw.go`
   (`unauthenticatedRoutes`, exported as `httpapi.UnauthenticatedRoutes()`): enrolment, session
   begin/complete, `/healthz`, `/v1/info`, `/v1/discovery`. **Trust the allow-list, never the
   prose** — it is the security boundary, the middleware is default-deny, and a count written here
   can go out of date while the list cannot. `INVARIANTS.md` invariant 3 justifies each entry.
4. **Nothing is acknowledged before it is durable** — two-phase prepare→commit, fsynced. Never trade
   this for latency. **NARROWED (2026-08-02):** this guarantees we never lose acknowledged data
   through OUR OWN WRITE PATH. It does NOT promise acknowledged data survives damaged media — see
   invariant 6, where availability wins and the discard is logged. **Invariants 4 and 6 are not in
   conflict; if you think you have found a contradiction, you have found this narrowing.**
5. **Memory is the serving copy; disk is the truth.** A crash must recover to a prefix of the
   accepted history: no torn records, no acknowledged-but-lost messages.
6. **The append-only log records METADATA AND ROUTING ONLY — never message bodies.** Recovery ALWAYS
   reaches a running server: damaged records are discarded and the bus starts, but **every discard
   must be logged loudly and specifically** — silent discard is the defect. Integrity is a keyed MAC
   (`crypto/hmac` + `crypto/sha256`), never a CRC.
7. **Nobody hand-writes HTTP — the compiled Go CLI is THE client.** Every capability ships with a CLI
   subcommand and an `AGENT_PROTOCOL.md` entry **in the same task**. Three audiences, all
   requirements: a human interactively; an agent shelling out (`--json` everywhere, stable documented
   exit codes, never an interactive prompt); and an agent embedding it — which is why the client
   package **cannot live under `internal/`**.
8. **Simple beats clever.** Go stdlib first. A third-party dependency needs a `DECISIONS.md`
   justification.
9. **NEVER write your own crypto.** Absolute, and it overrides every other preference here including
   invariant 8. Always use a well-known, audited library, and prefer the one that wraps as much of
   the problem as possible. **Specifically forbidden without explicit user consent in `DECISIONS.md`:
   implementing or "adapting" a cipher, hash, MAC, KDF, signature scheme, key exchange or ratchet;
   hand-rolling a padding, nonce or IV scheme; inventing a bespoke construction out of otherwise-good
   primitives** — that last one does not FEEL like writing your own crypto, which is exactly why it is
   enumerated. Broken crypto **fails silently** — it still encrypts, still verifies, and provides none
   of the protection it appears to, so "our tests pass" is not evidence. When no suitable library
   exists, change the requirement or stop and ask.
10. **Duplicate detection and idempotency, everywhere**, durable across restart. Three cases that must
    not be collapsed: same key + **same** payload is a legitimate retry — return the original result,
    do not re-apply, **do not disconnect**; same key + **different** payload is a protocol violation —
    reject and log it, but **do not disconnect**; only **replay of an already-accepted signed
    message** disconnects, and only when the sender claim inside the signed bytes is a well-formed
    fully-qualified id — an absent, unqualified or whitespace-padded claim names nobody, **is still
    REFUSED**, and must not disconnect. **Before adding ANY disconnect, ask two questions: can a
    merely BUGGY client reach this line, and does this connection carry only ONE principal's
    traffic?** One ambiguity is deliberately left un-disconnected: `409 no-matching-reservation` is
    byte-identical for a third party spending someone else's mint and for an agent re-presenting its
    OWN spent reservation, and **a test asserts that indistinguishability** — it goes RED the day it
    becomes resolvable, so do not "fix" it. Loop prevention via the traversed bus path is a
    *complement* to idempotency, never a substitute.
11. **TLS is the required transport. There is no plaintext listener** — the server refuses to start
    rather than fall back. Certificates are self-signed, TLS is **mutual**, and there is no CA **and
    no trust-on-first-use**: the invite blob carries the bus's certificate fingerprint. mTLS and the
    session token are BOTH required and do different jobs — and **cross-check them: a session token
    presented over a connection whose client certificate belongs to a DIFFERENT agent must be
    rejected**, which is stronger than either mechanism alone. The loopback default
    (`-listen 127.0.0.1:8080`) stays. Rotation serves TWO certificates during rollover and must never
    require re-enrolment. Never disable certificate verification — and never ship a flag that does,
    not even a documented one.
    `InsecureSkipVerify: true` is permitted in **exactly one file — `client/pin.go` — exactly once,
    paired with `VerifyPeerCertificate` in the same composite literal**, enforced by an AST guard in
    `client/guard_test.go`. **Read `INVARIANTS.md` "Invariant 11" before touching it: deleting that line, or the
    callback beside it, does not harden anything — it silently disables pinning, and every positive
    test passes either way.**

## Repository layout

```
cmd/agent-bus/        main — the server binary
internal/…            server packages (ids, store, wal, hub, http, relay, auth)
cmd/agent-busctl/     the CLI — THE client, and the only interface agents use (invariant 7)
scripts/bus-*.sh      RETIRED wrappers; only bus-serve.sh remains (server lifecycle). Do not add one
scripts/spec-cloud.sh authed curl shim for the Spec Server (task state)
scripts/gen-spec-mirror.sh regenerates SPEC.md AND SPEC/ — the ONLY supported way to write either
INVARIANTS.md         the 11 invariants WITH their reasoning — read the relevant one IN FULL
                      before working on that plane; CLAUDE.md carries only the one-line rules
PITFALLS.md           the verification and commit traps WITH the incident behind each — same
                      split: the one-line rule here, the dates/shas/output there
AGENTS.md             this same protocol for runtimes that read AGENTS.md, not CLAUDE.md. Edit
                      CLAUDE.md and re-sync; the two have drifted before (PITFALLS.md §5)
.claude/ORCHESTRATION.md  which sub-agent to pick, which model to pass, the review panel — read
                      before spawning anything; deliberately NOT injected per spawn
AGENT_PROTOCOL.md     agent-facing instructions: enrol, list, wait, send, relay
PROTOCOL.md           the wire protocol + on-disk format (human/maintainer facing)
CONTRACTS.md          INDEX only (split 2026-08-02) — see CONTRACTS-*.md for the actual surface:
CONTRACTS-CLI.md        server/CLI flags + env vars
CONTRACTS-HTTP.md       HTTP routes, headers, enrolment/sessions, authentication
CONTRACTS-ONDISK.md     record types, wire protocol versions, on-disk files, WAL at startup
CONTRACTS-AGENT.md      agent-facing wrappers + repo tooling scripts
DECISIONS.md          design decisions and their rationale (append-only, dated)
AGENT_LOG.md          per-task work log (append-only, dated)
SPEC.md               GENERATED epic INDEX of the Spec Server backlog — never hand-edit
SPEC/<EPIC>/epic.md   GENERATED — that epic's tasks, open first then closed
SPEC/<EPIC>/<task>/task.md  GENERATED — one full task record, description untruncated
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

## How to write (agent output, commit messages, docs, notes)

Speak plainly and directly. Prioritize useful information over commentary or rhetorical flourish.

Avoid:
- metaphors, figurative language, and colorful phrasing
- praise or validation such as "that's the right instinct"
- editorial commentary about what a point "teaches" or "reveals"
- dramatic, clever, or literary wording
- restating the user's observation before answering it

Use simple, literal language. State the relevant fact, implication, or next action directly.
Prefer "The count can become outdated" over "A count restated in prose is precisely the thing
that rots."

This applies to everything written here: replies to the user, briefs to sub-agents, commit
messages, `DECISIONS.md` and `AGENT_LOG.md` entries, task notes, and code comments. It does not
license dropping detail — the evidence, file:line citations, exact verdicts and caveats stay. Cut
the framing, not the facts.

**Where a new warning goes.** This file is injected into EVERY sub-agent spawn, so a paragraph added
here is paid on every dispatch and it has a byte ceiling in `docs/doc-budgets.tsv`. A newly learned
trap gets its ONE-LINE rule here and its incident — date, sha, exact output — in `PITFALLS.md`;
a design rule gets its one-liner here and its reasoning in `INVARIANTS.md`. Never delete a warning
to make room: relocate it and leave the pointer.

## Go conventions

- Formatting must be clean, `go vet ./...` clean, `go build ./...` green before any commit.
  **Do NOT call bare `gofmt` — it is NOT on PATH on this box**, and `test -z "$(gofmt -l .)"`
  PASSES when it exits 127. **And `gofmt -l` EXITS 0 EVEN WHEN IT LISTS FILES**, so
  `gofmt -l . && echo CLEAN` prints CLEAN over a list. Judge by empty OUTPUT, never exit status:
  ```
  go fmt ./...                                     # reformats in place; prints what it changed
  test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"   # correct: tests the output
  ```
  Both false passes, with the chain that echoed `GOFMT_CLEAN`, are in `PITFALLS.md` §1.
- Tests run with `-race`. Concurrency here is the product; a data race is a P0.
- Durability and recovery code must have **crash-injection tests** — a test that writes, kills at a
  chosen point in the write path, and asserts what recovery yields. "The code looks right" is not
  evidence for a durability claim.
- Prefer table-driven tests. Keep the narrowest check runnable in seconds.

## Verify — and tell the truth

Run the NARROWEST relevant check: `go test -race -run <Name> ./internal/<pkg>`, `go build ./...`,
`go vet ./...`, `go fmt ./...` (NOT bare `gofmt` — see the formatting note above).

**A check that runs nothing is not a pass.** `go test -run TestThatDoesNotExist ./pkg` prints
`ok ... [no tests to run]` and EXITS 0. Run every proof through `bash scripts/proof-check.sh '<cmd>'`,
which reports PASS / FAIL / VACUOUS / UNVERIFIABLE, and quote its verdict rather than a bare exit
code. A task must never be completed on a VACUOUS proof, and **a task with NO `proof_cmd` may not be
completed at all** — a missing proof leaves no record of what would even count as evidence.
Completing a task requires RUNNING `proof-check.sh` and quoting its verdict, not storing a command
nobody executed.

More shapes that look like a pass and are not. Each rule stands alone; the incident behind it is in
`PITFALLS.md` §2:
- **A passing parent test does not rescue skipped children** — a parent whose leaves all called
  `t.Skip` reports PASS; `proof-check.sh` reports VACUOUS and judges leaf results.
- **An unquoted `-run` regex is re-parsed by the inner shell**, so the command that runs is not the
  one you stored: verdict UNVERIFIABLE, exit 3. Double-quote the `-run` argument.
- **`grep`-based doc proofs are the MORE dangerous family** — they pass on an INCIDENTAL match
  elsewhere in the file. Pin the specific line (the table row, the field name, the artefact name),
  and confirm the proof is **RED before the fix**; a proof never observed failing is not evidence
  that it can fail. `scripts/doc-check.sh section` scopes a doc assertion to one heading.
- **Quote the proof's own number**, never a wider suite figure in its place.
- **A guard can be disabled by a change that reads as hardening** — `unset -f`, a "tidier" flag, a
  deleted line. And an assertion checking only an EXIT CODE can pass for the wrong reason once its
  guard is gone: assert on WHY a fixture failed, not just that it did. `PITFALLS.md` §6, and
  invariant 11's `client/pin.go`.

**Verify in a clean overlay of HEAD, not in your working tree — and run the OVERLAY's
`proof-check.sh`, not the live one.** A working tree that builds proves nothing about what is
COMMITTED: a consumer can land before its definition and break `main`, and that has happened here.
Extract HEAD, copy in ONLY the files you own, `cd` in, and run from there — calling `proof-check.sh`
and every path in the proof by a RELATIVE path, never an absolute one into the live worktree.
`PITFALLS.md` §3 has the recipe and why copying the live script over the overlay's defeats it.

For anything agent-facing, ALSO exercise it the way an agent would: through the compiled CLI
(`cmd/agent-busctl`) against a running server, **never** through a hand-written `curl` and **never**
through a `scripts/bus-*.sh` wrapper — those are retired and only `bus-serve.sh` (server lifecycle)
survives. If the subcommand doesn't work, the feature doesn't work; if the capability has no
subcommand yet, that is the missing half of the task, not a reason to reach for `curl`.

If a test fails, you are NOT done. Diagnose whether YOUR change caused it or it is pre-existing,
name the exact failing test, and report the verdict. NEVER hand-wave "pre-existing failures" to
declare success.

## Spec Server — task management (ALWAYS use spec-keeper)

Task state lives in the **Spec Server**, project slug **`agent-bus`**. The PRIMARY store is the
CLOUD (`https://api.spec.elasticninja.com`). Every spec API call MUST go through the authed wrapper
**`bash scripts/spec-cloud.sh <curl-opts> /api/v1/…`** (a drop-in for `curl`): it finds the `/path`
arg, prepends the cloud host, and injects a fresh Cognito bearer (cached ~40 min, auto-refreshed on
401). Creds live OUTSIDE the repo, never committed; the path is `SPEC_CLOUD_CREDS`, whose default
is set at `scripts/spec-cloud.sh:20`. Read it there; a path copied into prose here can go out of date.

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
- **Refresh the mirror** after mutations → `bash scripts/gen-spec-mirror.sh`. That is the ONLY
  write anyone makes to `SPEC.md` **or** `SPEC/`, and it rewrites BOTH: `SPEC.md` is an epic INDEX,
  the records live one file per task in `SPEC/<EPIC>/<task>/task.md`, description untruncated,
  closed tasks included (`--all` is now a NO-OP). The default run also fetches the authoritative
  `blocks`/`supersedes`/`relates`/`follow_up` edges (~70 s); `--no-relations` is the fast path and
  then every file says "NOT FETCHED — unknown, not absent". **Do NOT regenerate with a bare
  `spec-cloud.sh … export > SPEC.md`** — it bypasses the generator's guards and silently writes an
  error page over the index if the fetch fails.

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
4. **Before writing code, open `INVARIANTS.md` and read IN FULL every invariant your task touches** —
   ids/sequence (1, 2), auth/sessions (3), durability/recovery (4, 5, 6), CLI surface (7), crypto (9),
   idempotency/disconnects (10), TLS (11), relay/federation (2, 3, 6, 10, 11). The one-liners above
   are reminders, not specifications. **Name the invariants you read in your `kind=report` note** —
   that is what makes this step verifiable rather than aspirational. Then make the smallest code
   change that completes only that task.
5. Run the narrowest relevant check (see "Verify" above). For durability/recovery work, that
   includes the crash-injection test.
6. Commit with a descriptive message + short tldr, on branch `main`.
7. Mark the task done via `complete` (with `commit_sha`, `test_summary`, `proof_cmd`), add any
   discovered follow-ups, refresh the `SPEC.md` mirror, and post the journal notes.
8. Record decisions in `DECISIONS.md`; append to `AGENT_LOG.md`.
9. Update the relevant `CONTRACTS-*.md` plane file for what changed — CLI flags and env vars, HTTP
   routes and auth, on-disk records and the WAL, or agent-facing tooling. `CONTRACTS.md` is the
   index; use it rather than guessing. And — if the agent-facing surface moved — `AGENT_PROTOCOL.md`
   plus the `cmd/agent-busctl` subcommand that delivers it. **Not** a `scripts/bus-*.sh` wrapper:
   those are retired (invariant 7), and a capability without its subcommand is the missing half of
   the task.
    - **ALWAYS commit with an explicit pathspec: `git commit -m '…' -- <paths>`.** `git add <paths>`
      does NOT scope a later commit — a bare `git commit` takes the WHOLE index, including anything
      a concurrently-running agent has staged. Never `git add` then bare-`git commit` while any
      other agent is running.
    - **A pathspec commit takes the WORKTREE, not the index — so `git add` does not protect you
      either.** It commits that path's working-tree content and silently discards what you staged,
      so it can ship another agent's edits under YOUR commit title. Before any pathspec commit,
      check `git status --porcelain -- <paths>` AND diff the worktree (`git diff HEAD -- …`), never
      just the index (`git diff --cached -- …`).
    - **`MM` catches only ONE direction; a clean ` M` hides the other.** Status is never sufficient
      — read `git diff HEAD -- <path>` and confirm every hunk is yours. This applies hardest to the
      shared append-only files — `DECISIONS.md`, `AGENT_LOG.md`, `CONTRACTS*.md` — which several
      agents append to at once by design.
    - **Do NOT commit work no agent has reported.** A package that is present and green may be
      mid-review or mid-edit. Wait for the owning agent's report with gates COMPLETED.
    - **A green tree is not a GATED tree.** Do not commit an agent's work until it reports its
      reviewer AND security gates as COMPLETED, not merely dispatched.
    - Every one of those five has already happened here — four mis-titled commits, one
      un-compilable `main`, ungated code shipped three times, two security holes committed
      mid-review. `PITFALLS.md` §4 carries each incident with its date, sha and file.
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
- For the shared append-only files (`DECISIONS.md`, `AGENT_LOG.md`), only ONE agent at a time;
  prefer adding a new dated section over editing existing lines. The four `CONTRACTS-*.md` PLANE
  files are the opposite case — the 2026-08-02 split (commit `0439836`) exists so they can be written
  concurrently — but treat each plane file as single-owner FOR ONE PASS. Concurrency is about who may
  OWN a file; it does not relax the pathspec rule in step 9, which applies to any file two agents can
  touch at once, plane files included.
- Never run two agents against the same bus **data directory**. Each parallel run gets its own
  throwaway dir under `/tmp`; the tracked `data/` dir is not a test fixture.

## Agent roster (`.claude/agents/`)

planner · spec-keeper · implementer · test-engineer · reviewer · security · documentation ·
deep-diver · architecture-reviewer · performance-reviewer · reliability-reviewer · backlog-triage ·
feature-runner · integrator.

**`integrator` is the ONLY agent permitted to `git commit`.** Everyone else writes source and stops;
it verifies the gates are COMPLETED, the commit is pathspec-scoped and HEAD still compiles, then
commits or REFUSES. This is the rule that keeps ungated code out of `main` — do not commit around it.

**Before spawning ANY sub-agent, read `.claude/ORCHESTRATION.md`** — what each agent is for, how to
pick a model, the review panel, and how to write a brief. It is not injected per-spawn; read it on
demand.

**ALWAYS pass `model` explicitly** — never let a sub-agent inherit the session model.
`sonnet` = mechanical/well-scoped/writing-heavy; `opus` = judgment, design, or correctness-critical.

For ANY code change the chain spec-keeper → implementer → reviewer → security is MANDATORY; skipping
a step requires an explicit one-line justification in `AGENT_LOG.md`.
