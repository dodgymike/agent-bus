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
   validated, never an identity to be trusted. **Ids are never reused, including across restarts —
   and this was reaffirmed WITHOUT narrowing on 2026-08-02.** Recovery may not reissue an index it
   has already handed out, even for a record it discards: a salvage path that reuses the index of a
   damaged tail record is a DEFECT to fix, not a licence to narrow this invariant. When recovery
   discards a record, the sequence advances past the hole; it never rewinds. Contrast invariant 4,
   which WAS deliberately narrowed — this one was not.
2. **Every agent id is fully qualified: `<bus-id>.<agent-id>`.** That namespacing is what makes
   cross-bus routing and agent-list exchange unambiguous. Buses have ids for the same reason.
3. **Enrolment is INVITE-ONLY (2026-08-02), and the CLIENT signs a SERVER-PROVIDED session
   token.** No agent may enrol without redeeming an operator-minted invite. This closes the root
   cause of a whole family of pre-auth attacks rather than patching them one at a time: an
   unauthenticated enrolment route let an attacker mint its own agents, and from there exhaust the
   session table, lock out a named agent, or enumerate the roster. Invites must be single-use,
   expiring, and revocable, and redeeming one is the ONLY way onto the bus — including for peer
   buses.

   On the credential itself: Note the direction — an earlier wording had
   this backwards ("the server signs the agent's key"), which is neither the decision nor the code.
   At enrolment the agent presents its Ed25519 **public** key and the server records it. To get a
   credential the agent asks for a session, the **server** provides the token value, the agent
   **signs that value** with its private key, and the server verifies against the recorded public
   key. The client never chooses the bytes it signs — a client-chosen challenge permits
   pre-computation and proves far less. Sessions last **at most one hour**; the client refreshes at
   75% of lifetime. Tokens are **opaque server-side handles, not signed claims**, which is precisely
   what makes immediate revocation possible — stateless claims cannot be revoked before they expire.
   Sessions do NOT survive a restart. Every route authenticates EXCEPT the three that necessarily
   cannot: enrolment, session-begin and session-complete (they are how a credential is obtained),
   plus `/healthz` and `/v1/info`.
4. **Nothing is acknowledged before it is durable.** A send returns success only after the message
   is committed via the two-phase (prepare → commit) write path and fsynced. Never trade that for
   latency. **Narrowing (2026-08-02):** this guarantees we never lose acknowledged data through our
   own write path. It does NOT promise acknowledged data survives damaged media — see invariant 6,
   where availability wins and the discard is logged.
5. **Memory is the serving copy; disk is the truth.** State is held in memory for speed and rebuilt
   by replaying the durable store on start. A crash at any point must recover to a state that is a
   prefix of the accepted history — no torn records, no acknowledged-but-lost messages.
6. **Every message is also written to an append-only log — METADATA AND ROUTING INFO ONLY.** The log
   is the audit trail: message id, sequence, sender, recipient(s), bus path traversed, timestamp,
   size, and content hash. It does **not** record message bodies. That is a deliberate decision
   (2026-08-02) taken so the audit trail stays compatible with end-to-end encrypted, forward-secret
   payloads — a log holding plaintext would be unwritable the moment PFS lands, and a log holding
   ciphertext it can never decrypt would be dead weight. The log is append-only in the strict sense: no in-place edits.
   **Recovery ALWAYS reaches a running server (2026-08-02): damaged records are discarded and the
   bus starts.** It must never refuse to boot over corruption — a bus held hostage by one bad sector
   is worse than a bus that has lost a message and said so. The absolute requirement is that every
   discard is LOGGED, loudly and specifically: silent discard is the actual defect (it was rated P0),
   not discard itself. Integrity is protected by a keyed MAC (`crypto/hmac` + `crypto/sha256`), never
   a CRC — a CRC is unkeyed and linear, and a remote client was shown able to forge one.
7. **Nobody hand-writes HTTP — the compiled Go CLI is THE client.** Every capability ships with a CLI
   subcommand and an `AGENT_PROTOCOL.md` entry **in the same task**. A feature without its subcommand
   is not done. The CLI **replaces** the `scripts/bus-*.sh` wrappers (decided 2026-08-02); shell
   wrappers are no longer the delivery vehicle, and the ones that exist are to be retired as their
   subcommands land. It does all the heavy lifting: key generation and storage, session-token refresh,
   long-polling with cursor management, reconnect/backoff, and verification of inbound messages.

   It has **three audiences, and all three are requirements, not aspirations**:
   - **A human**, interactively: readable default output, sane defaults, `--help` that answers the
     common question, and errors that name the remedy rather than the stack.
   - **An agent**, shelling out: `--json` on every command, stable documented exit codes, never an
     interactive prompt, and credentials from config/env rather than a TTY. The long-poll command
     streams newline-delimited JSON so it can be piped and consumed incrementally.
   - **An agent, embedding it**: the CLI is a thin shell over a reusable Go client package. That
     package therefore CANNOT live under `internal/` — an importable client is the whole point of
     "embed", and putting it in `internal/` silently forecloses it.
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

10. **Duplicate detection and idempotency, everywhere.** Every mutating operation — enrol, send,
    broadcast, leave, peer-enrol, relay — carries a client-supplied idempotency key and is safe to
    retry. The server durably remembers which keys it has already applied, and that memory survives
    restart (it is part of the recovered state, not an in-memory cache). No operation may be applied
    twice.

    **The distinction that makes this correct, and must not be collapsed:**
    - **Same key + same payload = a legitimate retry.** The ack was probably lost in flight. Return
      the ORIGINAL result, do not re-apply, do not error, and do NOT disconnect. This is the whole
      point of idempotency: it exists so a well-behaved client can safely retry, and punishing that
      would break exactly the clients doing the right thing.
    - **Same key + DIFFERENT payload = a protocol violation.** The client is reusing a key for new
      content, which is either a serious bug or an attack. Reject it, log it, and **disconnect the
      offending client.**
    - **Replay of an already-accepted signed message** (by a peer, a relay, or a third party) is
      rejected outright and disconnects the sender. A signature does not stop replay — a valid signed
      message can be resent verbatim — so freshness comes from the server-minted monotonic sequence
      plus recipient-side cursor, not from the signature.

    Relay is where this earns its keep: a cyclic peer topology plus at-least-once delivery means
    duplicates are not an edge case but the normal steady state, and loop-prevention via the traversed
    bus path is a *complement* to idempotency, never a substitute for it.

11. **TLS is the required transport. There is no plaintext listener.** Decided 2026-08-02. Every
    HTTP surface — client and bus-to-bus relay — is served over TLS, and the server refuses to
    start rather than fall back to plaintext. This is not defence in depth layered on something
    already safe: without it the session token, which is a **bearer credential**, crosses the wire
    in clear, and an on-path observer can read it or kill a pending challenge. The loopback default
    (invariant: `-listen 127.0.0.1:8080`) stays — it bounds exposure, it does not replace TLS, and a
    bus deliberately exposed on a real interface needs both.

    Consequences that must be designed, not assumed:
    - **Certificates are SELF-SIGNED and TLS is MUTUAL (decided 2026-08-02).** Both ends present a
      certificate and both verify. There is no CA, and **there is no trust-on-first-use either**:
      the **invite blob carries the bus's certificate fingerprint** alongside the bus id, address and
      invite secret, so the client knows what to expect BEFORE its first connection. The agent's
      client-certificate fingerprint is bound to its server-minted agent id at enrolment. A bus runs
      on a laptop with no certificate authority anywhere in the picture.

      **Consequence: the invite blob is now the trust anchor, so the integrity of the channel it
      travels over is load-bearing.** Whoever can substitute an invite can point an agent at a bus of
      their choosing. That is a real requirement on invite distribution, not a footnote — and it is
      the price of eliminating the TOFU window, which is the right trade.
    - **Certificate rotation serves TWO certificates during rollover** so clients can re-pin without
      downtime. Rotation must never require every client to re-enrol.
    - **mTLS and the session token are BOTH required, and they do different jobs.** mTLS proves which
      key holder is on the connection; the session token is the revocable, time-bounded application
      credential. Do not let one silently replace the other — but DO cross-check them: a session
      token presented over a connection whose client certificate belongs to a different agent must
      be rejected, which is a stronger property than either mechanism gives alone.
    - **The CLI must make the trusted path the easy path.** Whatever the scheme, `bus enrol` against
      a fresh bus has to work without the user hand-editing a trust store.
    - Never disable certificate verification to make something work, and never ship a flag that
      does it silently — we read that as forbidding such a flag AT ALL, since a documented hole is
      not better than a hidden one, it is a hole with a manual. Per invariant 9 the TLS stack is
      stdlib `crypto/tls` — configured, never reimplemented.
    - **`InsecureSkipVerify: true` is permitted in EXACTLY ONE FILE — `client/pin.go` — and only
      paired with `VerifyPeerCertificate` (narrowed 2026-08-07, MTLS-PIN).** The earlier absolute
      ban could not survive contact with this invariant's own requirements: self-signed, **no CA**,
      **no TOFU**. Go's default chain verification cannot succeed and cannot be configured to — there
      is no root to chain to, and the client holds a 32-byte fingerprint rather than the certificate,
      so it cannot build an `x509.CertPool` either. `crypto/tls` supports exactly one way to
      substitute a verification policy: disable the default chain check and supply
      `VerifyPeerCertificate`. A ban with no exception would not have prevented the exception — it
      would have pushed it into a package the guard does not scan, which is strictly worse than one
      loud, reviewed occurrence.

      **Read this before "fixing" that line: deleting it, or deleting the callback beside it, does
      not harden anything — it silently disables pinning.** A `tls.Config` with the callback removed
      still compiles, still completes handshakes, still returns working connections, and verifies
      nothing. Every positive test passes either way.

      What replaces the ban is stricter and mechanical, in `client/guard_test.go`: the literal must
      appear in exactly one file **exactly once** (counted, so naming it in prose there fails too);
      an **AST** walk — not a grep — requires any composite literal setting it `true` to set
      `VerifyPeerCertificate` non-nil **in the same literal**, bans setting it by assignment (an
      assignment can be conditional and far from the literal), and requires **at least one** such
      paired literal to exist, so the guard cannot pass on a tree where pinning was deleted.

      What is given up, exactly: CA chain building (there is none, by design) and **hostname
      verification** — for which the pin substitutes and is strictly stronger, since a name check
      asks "does this certificate claim this address" and the pin asks "is this the exact certificate
      the invite named". **Certificate expiry is NOT checked** — a real gap owned by `MTLS-VERIFY`.
      Full reasoning in `DECISIONS.md` (2026-08-07, MTLS-PIN §2), which supersedes the absolute
      wording at `DECISIONS.md:1290` and `:2461` in place.

## Repository layout

```
cmd/agent-bus/        main — the server binary
internal/…            server packages (ids, store, wal, hub, http, relay, auth)
scripts/bus-*.sh      the agent-facing wrappers — the ONLY interface agents use
scripts/spec-cloud.sh authed curl shim for the Spec Server (task state)
AGENT_PROTOCOL.md     agent-facing instructions: enrol, list, wait, send, relay
PROTOCOL.md           the wire protocol + on-disk format (human/maintainer facing)
CONTRACTS.md          INDEX only (split 2026-08-02) — see CONTRACTS-*.md for the actual surface:
CONTRACTS-CLI.md        server/CLI flags + env vars
CONTRACTS-HTTP.md       HTTP routes, headers, enrolment/sessions, authentication
CONTRACTS-ONDISK.md     record types, wire protocol versions, on-disk files, WAL at startup
CONTRACTS-AGENT.md      agent-facing wrappers + repo tooling scripts
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
through `scripts/bus-*.sh` against a running server, not through a hand-written `curl`. If the
wrapper doesn't work, the feature doesn't work.

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
9. Update the relevant `CONTRACTS-*.md` plane file for what changed — `CONTRACTS-CLI.md` (flags, env
   vars), `CONTRACTS-HTTP.md` (routes, headers, enrolment/sessions, auth), `CONTRACTS-ONDISK.md`
   (record types, wire protocol versions, on-disk files, WAL), `CONTRACTS-AGENT.md` (agent-facing
   wrappers, repo tooling scripts) — see `CONTRACTS.md` for the full index if unsure which one. And
   — if the agent-facing surface moved — `AGENT_PROTOCOL.md` plus the `scripts/bus-*.sh` wrappers.
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
- **integrator** — the ONLY agent permitted to `git commit`. Verifies gates COMPLETED, that the
  commit is pathspec-scoped, that HEAD compiles afterwards, and that the message matches the
  evidence — then commits, or REFUSES with a reason. Added 2026-08-07 because every commit-time
  failure in this repo was mechanical and repeated: ungated code shipped three times, four
  index-sweeping mis-titled commits, and one `main` left un-compilable because a package was verified
  against the working tree rather than HEAD.

**Review panel (full-system review):** before a large change or as a periodic audit, convene
architecture-reviewer + reliability-reviewer + performance-reviewer + security + test-engineer
(+ reviewer for code-level). Run them READ-ONLY in parallel, each emitting findings to its own doc,
then synthesize into a single prioritized P0/P1/P2 backlog. None of the reviewers edit code.

For ANY code change the chain spec-keeper → implementer → reviewer → security is MANDATORY; skipping
a step requires an explicit one-line justification in `AGENT_LOG.md`.
