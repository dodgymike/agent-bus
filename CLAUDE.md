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
> (`ClientAuth: tls.RequestClientCert`, `a97f854` — one that IS presented authenticates nobody by
> itself), recipients CANNOT verify message signatures, and enrol idempotency is in-memory only.
> Do not build on a guarantee without checking it holds.
>
> **Enrolment IS invite-gated as of `3cedcb7` (2026-08-15)** — `enrolmentInviteRequired = true`,
> `cmd/agent-bus/main.go:66`. This paragraph claimed the opposite for several hours AFTER the gate
> shipped, which is the failure it exists to warn about: a stale "not yet implemented" note is more
> dangerous than no note, because it reads as freshly checked. Verified by forge, not by code
> reading — 220 refused enrolments (20 via CLI, 200 raw) grew the data dir by **0 bytes**, and a
> name refused 20 times then enrolled legitimately received suffix **1, not 21**, so the gate sits
> ABOVE the id mint and invariant 1's never-reuse rule is never engaged.
> **Known still-stale twin: `client/enrol.go:64` repeats the old claim.**

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
   except the **six** on the explicit allow-list in `internal/httpapi/authmw.go` (`unauthenticatedRoutes`, exported as `httpapi.UnauthenticatedRoutes()`): enrolment,
   session begin/complete, `/healthz`, `/v1/info` and **`/v1/discovery`** — that last one was
   missing from this line until 2026-08-21, when three counts that had been live at once (the
   code's, this line's, and one in an `internal/httpapi` test's failure message) were reconciled
   against `httpapi.UnauthenticatedRoutes()` — all three had read as freshly checked, which is why
   they lasted. Trust the allow-list, never the prose: it is the security boundary and the
   middleware is default-deny.
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

## Go conventions

- Formatting must be clean, `go vet ./...` clean, `go build ./...` green before any commit.
  **Do NOT call bare `gofmt` — it is NOT on PATH on this box** (only `$(go env GOROOT)/bin/gofmt` is).
  This matters because the idiomatic check is silently self-defeating: `test -z "$(gofmt -l .)"`
  **passes** when `gofmt` exits 127, because a command that fails to launch prints nothing to stdout.
  Every "gofmt clean" recorded from a bare call is a false pass. Use one of:
  ```
  go fmt ./...                      # reformats in place; prints the files it changed
  "$(go env GOROOT)/bin/gofmt" -l . # lists unformatted files; empty output = clean
  ```
  **And `gofmt -l` EXITS 0 EVEN WHEN IT LISTS FILES.** It reports by printing, not by status. So
  `gofmt -l . && echo CLEAN` prints CLEAN over a list of unformatted files — a second false pass, in
  a form the 127 case above does not cover. Observed 2026-08-07: a chain echoed `GOFMT_CLEAN` while
  `gofmt -l` had just named `client/messages_test.go`. Judge it by whether the OUTPUT is empty, never
  by its exit status:
  ```
  test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"   # correct: tests the output
  ```
- Tests run with `-race`. Concurrency here is the product; a data race is a P0.
- Durability and recovery code must have **crash-injection tests** — a test that writes, kills at a
  chosen point in the write path, and asserts what recovery yields. "The code looks right" is not
  evidence for a durability claim.
- Prefer table-driven tests. Keep the narrowest check runnable in seconds.

## Verify — and tell the truth

Run the NARROWEST relevant check: `go test -race -run <Name> ./internal/<pkg>`, `go build ./...`,
`go vet ./...`, `go fmt ./...` (NOT bare `gofmt` — see the formatting note above).

**A check that runs nothing is not a pass.** `go test -run TestThatDoesNotExist ./pkg` prints
`ok ... [no tests to run]` and EXITS 0, so a proof command naming a test that was never written
looks identical to a passing one. Run proof commands through `bash scripts/proof-check.sh '<cmd>'`,
which reports PASS / FAIL / VACUOUS / UNVERIFIABLE, and quote its verdict rather than a bare exit
code. A task must never be completed on a VACUOUS proof, and **a task with NO `proof_cmd` may not be
completed at all** — a missing proof is worse than a vacuous one, since it leaves no record of what
would even count as evidence. Completing a task requires RUNNING `proof-check.sh` and quoting its
verdict, not storing a command nobody executed. For anything agent-facing, ALSO exercise it the way an agent would:
through the compiled CLI (`cmd/agent-busctl`) against a running server, **never** through a
hand-written `curl` and **never** through a `scripts/bus-*.sh` wrapper — those are retired and only
`bus-serve.sh` (server lifecycle) survives. If the subcommand doesn't work, the feature doesn't work;
if the capability has no subcommand yet, that is the missing half of the task, not a reason to reach
for `curl`.

**A passing parent test does not rescue skipped children.** Go reports a parent as PASS when every
leaf subtest called `t.Skip`; that shape exercised no assertion and `proof-check.sh` therefore reports
VACUOUS. Its plain-text and JSON parsers judge leaf results so an indented child `--- SKIP:` cannot be
hidden by the unindented parent `--- PASS:` line. Results remain scoped to their package, and a
package's `[no tests to run]` summary overrides marker-shaped output printed by `TestMain`.

**Verify in a clean overlay of HEAD, not in your working tree — and run the OVERLAY's `proof-check.sh`,
not the live one.** A working tree that builds proves nothing about what is COMMITTED: a definition
you consume may be sitting uncommitted beside you, so a consumer can land before its definition and
break `main`. That has happened here. Extract HEAD, copy in ONLY the files you own, `cd` in, and run
the check from there:

```
T=$(mktemp -d); git archive HEAD | tar -x -C "$T"
cp <the paths you own> "$T"/<same paths>     # ONLY your files — nothing else uncommitted
(cd "$T" && go build ./... && bash scripts/proof-check.sh '<cmd>')
```

**Call `proof-check.sh` — and every path in your proof command — by a RELATIVE path from inside `$T`,
never by an absolute path into the live worktree.** `git archive` already places `scripts/proof-check.sh`
in the overlay, so there is nothing to copy: **do NOT `cp` the live script over it**, or the one file
deciding PASS/FAIL becomes the only uncommitted code in the overlay. The point is that the verifier's
logic comes from HEAD too. (Its *cwd* handling is no longer the hazard — `535876c` made it run proofs
in the caller's cwd — but an absolute path still reaches a script that MAY be uncommitted, and any
other absolute path in the proof reaches uncommitted files.)

**`grep`-based proofs are the MORE dangerous family, and CLAUDE.md previously warned only about
tests.** A doc proof like `grep -n '8080' README.md CONTRACTS.md | grep -qi localhost && echo DOCS_OK`
passes on an INCIDENTAL match somewhere else in the file — in the real case, a pre-existing
`curl -s localhost:8080/healthz` line in README — and would have green-lit closing a task over the
exact file two reviewers had blocked on. A doc proof must pin the specific line it claims to prove
(the table row, the field name, the artefact name), and you must confirm it is **RED before the fix**.
A proof that was never observed failing is not evidence that it can fail.

If a test fails, you are NOT done. Diagnose whether YOUR change caused it or it is pre-existing,
name the exact failing test, and report the verdict. NEVER hand-wave "pre-existing failures" to
declare success.

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
- **Refresh the mirror** after mutations → `bash scripts/gen-spec-mirror.sh`.
  That is the ONLY write anyone makes to `SPEC.md` **or** `SPEC/`, and it rewrites BOTH. `SPEC.md`
  is an epic INDEX; the records live in `SPEC/<EPIC>/epic.md` and `SPEC/<EPIC>/<task>/task.md`, one
  file per task, description untruncated. **Closed tasks ARE included** — in a tree they cost
  nothing until a file is opened — so **`--all` is now a NO-OP**, kept only so old invocations do
  not fail. The default run also fetches the authoritative `blocks`/`supersedes`/`relates`/
  `follow_up` edges, one request per task against a rate-limited API (~70 s per the script header);
  `--no-relations` is the fast path and then every file says "NOT FETCHED — unknown, not absent".
  Do NOT regenerate with a bare `spec-cloud.sh … export > SPEC.md`: that puts the old 640 KB
  single-file mirror back over the index, bypasses the generator's guards, and silently overwrites
  it with an error page if the fetch fails.

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
9. Update the relevant `CONTRACTS-*.md` plane file for what changed — `CONTRACTS-CLI.md` (flags, env
   vars), `CONTRACTS-HTTP.md` (routes, headers, enrolment/sessions, auth), `CONTRACTS-ONDISK.md`
   (record types, wire protocol versions, on-disk files, WAL), `CONTRACTS-AGENT.md` (agent-facing
   wrappers, repo tooling scripts) — see `CONTRACTS.md` for the full index if unsure which one. And
   — if the agent-facing surface moved — `AGENT_PROTOCOL.md` plus the `cmd/agent-busctl` subcommand
   that delivers it. **Not** a `scripts/bus-*.sh` wrapper: those are retired (invariant 7), and a
   capability without its subcommand is the missing half of the task.
    - **ALWAYS commit with an explicit pathspec: `git commit -m '…' -- <paths>`.** `git add <paths>`
      does NOT scope a later commit — a bare `git commit` takes the WHOLE index, including anything a
      concurrently-running agent has staged. This has produced four mis-titled commits in this repo,
      one of which left `main` un-compilable for several commits because half of a change was swept
      into an unrelated docs commit while the other half stayed in the working tree. The working tree
      looked green throughout, which is why nobody noticed. Never `git add` then bare-`git commit`
      while any other agent is running.
    - **A pathspec commit takes the WORKTREE, not the index — so `git add` does not protect you
      either.** This is the other half of the trap above and it bites in the opposite direction.
      `git commit -- <path>` commits that path's WORKING-TREE content, silently discarding whatever
      you staged for it. So on a file showing `MM` in `git status --porcelain` — index clean, worktree
      dirty — carefully staging only your own text and then committing by pathspec ships the OTHER
      agent's unstaged edits under YOUR commit title. Caught 2026-08-07 by the integrator on
      `DECISIONS.md`: the index held only the DISCOVERY-DOC section, while the worktree had gained a
      full `## 2026-08-07 — MTLS-PIN` section from a concurrent agent — text asserting that
      `client/pin.go` had landed, when that file was untracked and its test was red. It refused the
      commit rather than putting a false dated claim in `main`. **Before any pathspec commit, check
      `git status --porcelain -- <paths>` for an `MM`, and diff the worktree (`git diff HEAD -- …`),
      never just the index (`git diff --cached -- …`).** This applies hardest to the shared
      append-only files — `DECISIONS.md`, `AGENT_LOG.md`, `CONTRACTS*.md` — which several agents
      append to at once by design.
    - **`MM` catches only ONE direction; a clean ` M` hides the other.** Index clean over a
      contaminated worktree trips no status check, and the pathspec commit still takes the lot: on
      2026-08-14 `client/client.go` sat at ` M` carrying one in-scope doc comment plus `endpointWith`
      and `resolvePinsWith` from another agent's live, ungated task. Status is never sufficient —
      read `git diff HEAD -- <path>` and confirm every hunk is yours.
    - **Do NOT commit work no agent has reported.** A package appearing in the tree and passing its
      tests is not a signal that it is finished — it may be mid-review, or mid-edit. Wait for the
      owning agent's report with gates COMPLETED. Committing on "it is green and it is there" has
      shipped ungated code three times (`518e71b`, `2451b4a`, `f56c723`), each time discovered only
      when the agent later reported findings against code already in `main`.
    - **A green tree is not a GATED tree.** Do not commit an agent's work until it reports its
      reviewer AND security gates as COMPLETED, not merely dispatched. Committing mid-review has
      shipped two real security holes here (a relay SSRF and an unbounded input), both caught by
      gates that were still running when the commit landed.
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
