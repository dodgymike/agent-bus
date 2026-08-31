# CONV golden path — HANDOVER

**Living doc.** If a session died mid-build, START HERE. Last updated 2026-08-31.

## Goal (operator, verbatim intent)
Minimum end-to-end conversations: a client asks the server to CREATE a conversation with recipients,
gets a server-minted uuid, and SENDS to that uuid; members receive without tracking participants.
Scope: SINGLE-BUS, FIXED membership at create. COMMS epic was CANCELLED (operator 2026-08-29).

## Committed state (all on local `main`, HEAD `fb65ad8`, NOT pushed to origin)
- `f6411146` — the three CONV rulings (DECISIONS.md 2026-08-29). Operator-approved.
- `0683f2f` — **CONV-RECORD**: durable `ConversationRecord`, `wal.Entry.Kind "conversation"`,
  `internal/store/conversation.go` + `conversationstore.go`. Crash-injection tested. DONE+closed.
- `fb65ad8` — **CONV-CREATE-CLI**: `POST /v1/conversations` + `agent-busctl conversation create`,
  `internal/httpapi/conversations.go`, `internal/store/conversationcreate.go`, `client/conversation.go`,
  `cmd/agent-busctl/conversation.go`, `internal/idem/scope.go`. DONE+closed.

## The three rulings (in DECISIONS.md, build on these)
1. **CONV-VS-THREAD** = moot (COMMS cancelled).
2. **Id shape** = `<bus-id>.<uuid-v4>` (server-minted, crypto/rand, stdlib — no uuid dep).
3. **Name** = metadata, ≤128 bytes, single-line printable UTF-8, **refuse-not-truncate**, enforced on
   BOTH encode AND `DecodeConversationRecord` (the invariant-6 crux). At-rest exposure documented.

## Golden path status — 5/6 done
| # | Task | Status |
|---|------|--------|
| 1-3 | CONV-VS-THREAD, CONV-ID-SHAPE, CONV-NAME-INV6 (rulings) | ✅ done |
| 4 | CONV-RECORD | ✅ done (`0683f2f`) |
| 5 | CONV-CREATE-CLI | ✅ done (`fb65ad8`) |
| 6 | **CONV-SEND-BY-ID** (`ce8bff7b`) | ⏳ IN FLIGHT — agent `a4529b7`, re-dispatched (a prior attempt died on session limit, worktree lost) |

## CONV-SEND-BY-ID (the last step) — design already decided in the brief
- Route: send a message addressed to a conversation id → resolve the record → deliver to CURRENT
  members via the EXISTING message-delivery path (reuse `/v1/send`/hub delivery, no parallel path).
- CLI: `agent-busctl conversation send <conv-id> --body <text>` (match `send` + `conversation create`).
- **Sender-authz ruling (default): PARTICIPANT-ONLY** — sender must be creator or a current recipient;
  non-participant → refused (404 preferred, leaks less). Record in DECISIONS.md. The agent posts this
  to the task journal EARLY for resilience.
- Body rides the existing message path (invariant 6 — NOT written into the conversation record).
- Unknown conv id → clean 404, no panic. Idempotency reuses existing message-send machinery (inv 10).
- Docs REQUIRED in-task: AGENT_PROTOCOL.md + CONTRACTS-HTTP.md + CONTRACTS-CLI.md (the CONV-CREATE-CLI
  reviewer BLOCKED on missing docs — do not repeat).

### If CONV-SEND-BY-ID work is LOST (worktree cleaned on agent death)
Re-dispatch a feature-runner (isolation:worktree, opus) with the brief above against current HEAD.
No analysis is redone — the design is fully specified here and in the task `ce8bff7b`. Check first:
`git worktree list` for a surviving `agent-a4529b7…` worktree (2 of 3 prior CONV worktrees SURVIVED
their agents' deaths — salvage its files if present). Also check `/tmp/convsend-wip/` and
`/tmp/convsend-checkpoint.patch` (the agent was asked to checkpoint there).

## Operational facts (learned this session)
- **Live-CLI proof recipe** (invariant 7): binaries `go build -o /tmp/.../agent-bus ./cmd/agent-bus`
  + `...agent-busctl ./cmd/agent-busctl`. Bus runs as `agent-bus -data-dir DIR -listen 127.0.0.1:PORT`
  (serve is default, TLS-only). MINT requires bus STOPPED + dir already initialised: start once to
  init cert, stop, `agent-bus invite mint -data-dir DIR -bus-address https://127.0.0.1:PORT -ttl 1h
  -json`, start. ENROL refuses a mode-0664 invite → `chmod 0600` it first, then `agent-busctl enrol
  --invite-file inv --name alice`. Enrolled ids carry a `-N` suffix (`alice-1`). Second agent: mint a
  2nd invite, `enrol --name bob --keep-current`. Then `conversation create --recipient bus-XXX.bob-1`.
- **Spec Server**: filter `?epic=CONV&limit=1000` (NOT `?epic_key=`, NOT default 200-cap). Complete a
  task via `POST /tasks/<full-uuid>/complete`; short 8-char ids 404 on `/notes` (use full public_id).
  Cancel via `POST /tasks/<id>/status {"status":"cancelled"}`.
- **Commits**: integrator-only, explicit pathspec (takes WORKTREE not index). AGENT_LOG/DECISIONS are
  pure-append — reconcile by appending only the new block, deletions=0. Docs-only commit under the
  security carve-out needs an AGENT_LOG skip line.
- **Known pre-existing flake**: `TestCLIEnrolEndToEnd` (cmd/agent-busctl) fails ~1/3 under `-race`,
  unrelated to conversations, green on clean HEAD. Filed P1 as `23d4e264`. Not a CONV regression.
- **Follow-up filed**: `fba16f74` (P3) — `conversation create` missing from the exit-9 version-skew list.
- ~20 stale worktrees clutter `.claude/worktrees/` (dead agents) — harmless, prunable with
  `git worktree prune` / `git worktree remove` (skip the `locked` ones which hold referenced work).
- **Unpushed**: local `main` is many commits ahead of origin. Push needs the user's SSH agent:
  `! git -C /mnt/sdb4/mike/mike/source/agent-bus push origin main`.

## Progress log
- 2026-08-31: 5/6 golden-path steps committed. CONV-SEND-BY-ID re-dispatched (`a4529b7`) after a
  session-limit death lost the prior worktree. Handover written to survive further token exhaustion.
