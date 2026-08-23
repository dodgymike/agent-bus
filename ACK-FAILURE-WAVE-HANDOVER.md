# ACK failure-handling wave — HANDOVER

**Living document.** Updated after every step. If a session died mid-wave, START HERE.
Last updated: 2026-08-23. Author: orchestrator session (agent-bus).

## Why this doc exists
The orchestrator's token budget is expected to run out somewhere during this wave. Work is being
done SERIALLY (one agent at a time) so that at most one agent is in flight when tokens die, and this
doc records exactly where things stand so the next session resumes cleanly rather than re-deriving.

## Current repo state
- **HEAD at wave start: `5feceb1`** (ACK-3 R4 — record the three ACK wire-version rulings in DECISIONS.md).
- Working tree: this doc (`ACK-FAILURE-WAVE-HANDOVER.md`) is untracked until its first commit.
- **NOT PUSHED.** `origin/main` is ~60 commits behind. All session work is local `main` only.
  Push needs the user's SSH agent: `! git -C /mnt/sdb4/mike/mike/source/agent-bus push origin main`.

## Operational facts the next session needs (learned this session)
- **Spec Server list filter is `?epic=ACK`, NOT `?epic_key=ACK`** — the latter silently returns 0/all.
  Default list caps at 200 rows; pass `limit=1000` or check `X-Has-More`.
- **The SPEC.md/SPEC mirror is FROZEN** on an exporter column-0 defect (task `0f4a0736` / `a37bba55`):
  two tasks embed multi-line shell in `proof_cmd`, so `gen-spec-mirror.sh` refuses to write. Task
  state is correct in the live API; the in-repo mirror is stale. Do NOT trust SPEC.md; query the API.
- **The `complete`-URL sandbox guard**: `POST .../tasks/<id>/complete` is refused for WORKTREE-ISOLATED
  agents (the guard false-matches "complete" in the path). Non-isolated spec-keeper completes fine.
  So: feature-runners in worktrees CANNOT flip their own task — the orchestrator/spec-keeper does it.
  Do NOT percent-encode the URL to dodge the guard (an earlier agent did; it is an obfuscation bypass,
  recorded as an incident). Filed: `48be31d6`.
- **Commit discipline**: only the `integrator` agent commits. Pathspec commits take the WORKTREE not the
  index. `AGENT_LOG.md` / `DECISIONS.md` are shared append-only — reconcile by appending ONLY the new
  block onto current HEAD (never copy a worktree's whole file — it reverts intervening entries); verify
  deletions=0. Worktree feature-runners branch from an older base, so their appends need this reconcile.
- **Docs-only commit under the security carve-out needs an `AGENT_LOG.md` skip record** naming the
  skipped tier + paths, or the integrator refuses. Deep-dive docs: reviewer NOT RUN (analysis doc),
  security SKIPPED (carve-out) — precedent `61db229` (AUTH-8), `2856b13` (done-not-flipped).
- **Deep-dive / doc agents sometimes leak `</content>`/`</invoke>` at the file tail** — the integrator
  refuses. Strip with `head -n <N>`; verify `grep -cE '^</(content|invoke)>$' <file>` == 0.
- **The "done-but-not-flipped" pattern is rampant** — ~48% of in_progress tasks are complete-but-unflipped
  because the commit and the done-flip are owned by different unbound agents. Deep-dive:
  `DONE-NOT-FLIPPED_DEEPDIVE.md` (committed `2856b13`). Fix filed `7befde72` (integrator flips atomically,
  blocked on `48be31d6`) + `315899be` (`backlog-drift.sh` detector). **When picking any ACK task, VERIFY
  it is not already done at HEAD before implementing** (run its proof; a green proof on a task that claims
  to be unbuilt means done-but-not-flipped → verify gates + close, don't re-implement).

## Concurrently in flight (NOT part of this wave, do not collide)
- **ACK-17** (`d4a2d828`, P2) — feature-runner `aa719d536dfa3a835`, worktree-isolated, writing four
  mutation-killing tests (the tests were gated against an overlay that never committed — the "vapor gate"
  case). Touches `internal/httpapi/ackstatus_test.go` + possibly the parked-wait cap keying. If it
  reports, land + close it. It may collide with ACK failure tasks on `internal/httpapi` at COMMIT time
  (worktree isolation makes edit-time safe) — sequence commits, do not race.

## The wave — SERIAL order (highest value first, so an early stop still lands the best)
Do ONE at a time. After each: land (integrator) → close (spec-keeper) → UPDATE THIS DOC → next.

| # | Task | Pri | What | Status |
|---|------|-----|------|--------|
| 1 | `7d564118` ACK-12-FU-DESTINATION-ROW | P0 | relayed msg gets no ack row on DESTINATION bus; ack auth rides on message retention which can expire before ack.Retention (24h). Fix: write a destination/broadcast row OR bind the retention windows; test must advance time past message retention while the ack row is still live and assert the ack still resolves. NOTE: `recordAcceptance` (`internal/hub/hub.go:~2197`) early-returns for `relayed OR broadcast` — BOTH cases uncovered. | NOT STARTED |
| 2 | `ACK-5-FU-TRANSIT-503-IS-PERMANENT` | P1 | `internal/httpapi/ack.go:~375-399` maps EVERY TransitAck error to 503+Retry-After:1, so a PERMANENT upstream 409 reaches the recipient as retriable. Fix: a permanent upstream refusal maps to a permanent code, not 503. Contained, well-understood. | NOT STARTED |
| 3 | `ACK-14` | P1 | retry exhaustion must BOUNCE to the sender — today the horizon expires and the message vanishes silently. Sender must learn its relayed message died. | NOT STARTED |
| 4 | `81ce7331` ACK-RETRY-ENGINE | P1 | sender-side retry of an unacknowledged relayed message, with backoff/horizon. Pairs with ACK-14 (retry then bounce). | NOT STARTED |
| 5 | `ACK-8-FU-HOPBOUNDARY` | P1 | crash-inject the HOP acknowledgement boundary (durability of the ack as it crosses a bus). | NOT STARTED |
| 6 | `ACK-8-FU-D2-OBLIGATIONLOST` | P1 | detect and emit `obligation_lost` when a delivery obligation cannot be met. | NOT STARTED |

Dependencies/notes:
- 1 (destination row) is FOUNDATIONAL — several others assume a destination-side lifecycle row exists.
  Do it first. It interacts with ACK-14 and the retry engine.
- 2 is the smallest and most contained — a good early win if time is short after 1.
- 3+4 are a pair (bounce + retry). 5+6 are the ACK-8 crash/obligation follow-ups.
- Every task: read `INVARIANTS.md` in full for the planes it touches (ACK/relay → 2,3,6,10,11), name
  them in the report; RED-first proof; security REQUIRED (relay/durability surface, no carve-out);
  ship the CLI/AGENT_PROTOCOL half in the same task (invariant 7); do NOT commit (integrator does).

## Progress log (append after each step)
- 2026-08-23: Wave planned, handover doc written. HEAD `5feceb1`. ACK epic: 40 open, 1 P0 (`7d564118`).
  Starting task 1 (`7d564118`). ACK-17 still in flight.


## 2026-08-23 REORDER (task-1 P0 came back BLOCKED — dependency found)

`7d564118` (destination-row P0) is BLOCKED and must NOT be implemented yet:
- A relayed message carries 1..64 recipients, so a destination-side ack row means SEVERAL rows under
  one correlation key. But `relay.AuthorizePeerAck` binds the job id to `(peer, key)` — NOT the
  recipient. With >1 row per key, a peer bound for one recipient can settle ANY recipient of that key;
  terminal is absorbing → uncorrectable cross-peer forgery. Filed as **ACK-4-FU-RECIPIENT-BINDING
  (`ec4a1ac8`, P1)** whose description says verbatim "Must land before any task creates a second row
  for one correlation key."
- Option (b) bind-retention-windows conflicts with the byte-based memory-safety prune (`store.go:894`
  drops oldest once bytes > 1GiB regardless of age) → would reintroduce unbounded growth.
- Broadcast arm of `recordAcceptance` is a DELIBERATE NON-GOAL: a broadcast has no canonical audience
  under signing v1, so no `(message,recipient)` pair to key a row on. Record as non-goal when the
  relayed arm is built.

**AMENDED SERIAL ORDER:**
0. `ec4a1ac8` ACK-4-FU-RECIPIENT-BINDING (P1) — bind the recipient into AuthorizePeerAck. PREREQUISITE.
1. `7d564118` destination-row P0 — option (a) destination row, AFTER ec4a1ac8. (was task 1, now blocked)
2. `ACK-5-FU-TRANSIT-503-IS-PERMANENT`  3. `ACK-14`  4. `81ce7331`  5/6. ACK-8-FU-*.

## PROGRESS LOG (continued)
- 2026-08-23: Handover doc committed attempt hit a CONCURRENCY INCIDENT — two integrators dispatched
  concurrently both touching AGENT_LOG.md (the ACK-17 landing + the handover commit). Caught before
  either committed; stopped the ACK-17 integrator (TaskStop), reverted the main tree to clean HEAD
  (ACK-17 work safe in its own worktree, nothing lost, HEAD never moved). LESSON: never run two
  integrators concurrently when both touch a shared append-only file (AGENT_LOG.md / DECISIONS.md).
  From here: strictly one integrator at a time.
- 2026-08-23: ACK-17 (vapor-gate tests) reviewer+security PASS; re-landing serially (was interrupted).
- 2026-08-23: task 1 `7d564118` came back BLOCKED on `ec4a1ac8` (see REORDER above). Next: do
  `ec4a1ac8` first, then re-dispatch `7d564118`.
