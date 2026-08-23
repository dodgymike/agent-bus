# DONE-NOT-FLIPPED — why the backlog systematically shows shipped work as open

Investigation date: 2026-08-22/23. HEAD: `7c79c15` (`7c79c1577dcd91818424f99966688a51f1efea10`).
Author: deep-diver. Read-only: no product code changed, mirror not regenerated, no git commit.

## 1. Symptom

Tasks marked `in_progress` in the Spec Server turn out to be already done at HEAD — code written,
committed, gated (reviewer + security PASS in the journal), and the stored `proof_cmd` passes — but
the task was never flipped to `done`. The backlog therefore overstates open engineering work, and
shows P0s as open when they are effectively finished.

This is not one stuck task. Across the current 23 `in_progress` tasks the pattern is the norm, and it
recurs across epics (WAL, IDS, MTLS, IDEM, RELAY, ACK).

Population at HEAD (`GET /api/v1/projects/agent-bus/tasks?limit=1000`, 847 tasks):
`done` 209 · `todo` 537 · `superseded` 47 · `in_progress` 23 · `cancelled` 18 · `deferred` 8 ·
`blocked` 5. All 23 `in_progress` carry a stored `proof_cmd` (9 are P0, 11 P1, 3 P2).

## 2. Evidence

### 2a. The code is at HEAD for effectively all of them

Cheap checks (chosen deliberately over full `-race` runs, which are minutes each):

- **Named test symbols exist at HEAD.** Of 20 test functions named in the `in_progress` proofs, 19
  are present verbatim (`git grep -l "func <Name>" HEAD -- '*_test.go'`). The one miss,
  `TestWALRepairDoesNotReissueDiscardedIndex` (task `e120153b`, WAL P0), was renamed: the fix shipped
  as the durable index-floor design and its tests are `TestWALIndexFloorCrashNeverReissuesAnIndex`
  et al. in `internal/wal/indexfloor_crash_test.go`. So the stored `proof_cmd` names a test that no
  longer exists — the proof is stale, not the work.

- **Journal-referenced commits are all in HEAD ancestry** (`git merge-base --is-ancestor <sha> HEAD`):
  `f56c723` (WAL floor, `e120153b`), `6985d2c` (durable suffix allocator, `94159d93`), `9418a48`
  (client certs, `MTLS-CLIENTCERT`), `d34f73c` (bus-serve fingerprint, `10e93262`), `f91a819`
  (`GET /v1/discovery`, `2d7ce37b`), `e1bd842` (relay retry/doc, `3e542d14`), `1c6c540` (invariant-10
  disconnect narrowing) — all IN-HEAD.

- **Three stored proofs run PASS at HEAD** (sampled to anchor the claim empirically; go1.19.4 on this
  box):
  - `go test -race -run TestWALIndexFloorCrashNeverReissuesAnIndex ./internal/wal` → `ok … 1.114s`
    (task `e120153b`, **P0**).
  - `go test -race -run TestPackageDocDoesNotReviveTheWithdrawnDisconnect ./internal/relay` →
    `ok … 0.036s` (task `3e542d14`).
  - `go test -race -run TestMessageRecordThatCannotBeAppliedIsDiscardedLoudly ./internal/hub` →
    `ok … 0.265s` (task `d2cad9e7`).

  Not covered: the script-shaped proofs (`bash scripts/fed-smoke.sh`, `proof-check.sh` wrappers for
  ACK/RELAY) were not run here — they need a live multi-bus federation and are expensive; for those I
  relied on the journal verdicts, and I flag `RELAY-25` below where the journal itself says the full
  proof was RED.

### 2b. The journal shows the flip was consciously skipped, not lost

The decisive evidence is the spec-keeper notes. On 2026-08-08 a spec-keeper ran an explicit
`IN-PROGRESS AUDIT` and, for task after task, bucketed the work `SHIPPED` yet **left it in_progress
on purpose**:

- `e120153b` (WAL P0): *"IN-PROGRESS AUDIT 2026-08-08. Bucket: SHIPPED, left in_progress. The title's
  'BLOCKED on DUR-12' is stale (DUR-12 is done). The fix landed at commit f56c723 …"*
- `94159d93` (suffix-floor P0): *"Bucket: SHIPPED, left in_progress."*
- `10e93262` (P1): *"Bucket: SHIPPED (both halves), left in_progress. … P1-1 landed at d34f73c …"*
- `IDEM-18` (P1): *"Bucket: SHIPPED, left in_progress. The status_note … has been overtaken: the
  CURRENT proof_cmd runs and is non-vacuous at HEAD."*
- `MTLS-VERIFY` / `MTLS-CLIENTCERT` (P1): *"Bucket: PARTIALLY SHIPPED — kept in_progress."*

These are not agents that died before flipping. A spec-keeper looked at each, confirmed it shipped,
and did **not** call `complete`. That is the mechanism to explain: why the agent whose job is the
flip declined to make it, and why nothing else picks it up.

The reasons the notes give for holding fall into a few families:

- **Invariant-7 same-task half owed** (a capability must ship its CLI subcommand + `AGENT_PROTOCOL.md`
  entry in the SAME task): `2d7ce37b` DISCOVERY-DOC — *"left in_progress per invariant 7 — server half
  committed at f91a819, CLI half (DISCOVERY-DOC-FU-CLI) confirmed unstarted"*; `MTLS-CROSSCHECK` —
  *"NOT COMPLETED, deliberately: the documentation half is owed"*; `836c9ff8` ACK-6-FU-CLI —
  *"PART (a) ONLY … the CLI subcommand (part b) is owed"*.
- **"committed != running"** hold: `INVITE-HARDEN` — *"deliberately NOT completed, since the guard is
  in an untracked file and committed != running"*; `MTLS-CROSSCHECK` — sweep must be observed RED
  first.
- **Held pending reviewer re-verification** of a CHANGES-REQUESTED whose findings were
  record-keeping, not code: `ACK-8` — *"remains in_progress … awaiting reviewer re-verification"*;
  `MTLS-BIND` — reviewer CHANGES-REQUIRED narrowed to blocker 2.
- **Owner set but not completed** — a spec-keeper PATCHed `owner`/`status_note` but the flip is a
  separate call it did not make: `71cdaef8` — *"since PATCH-owner alone does not flip status … task
  not completed"*.
- **Code-only agent rule**: `IDEM-12` — *"complete/gates-PASS/awaiting-coordinated-commit per the
  code-only-agent rule — not completed here since no real commit_sha exists yet."*

### 2c. The stale mirror amplifies it

`SPEC.md` / `SPEC/` are a generated mirror, and the generator is frozen: notes across the session say
*"Mirror NOT regenerated (generator frozen)"*, and existing task `0f4a0736` records
*"gen-spec-mirror.sh REFUSES TO WRITE ('unexpected non-blank column-0 lines')"*. Anyone reading the
in-repo mirror instead of the live API sees an even older state than the (already stale) server.

### 2d. Rework actually happened this session

Agents redispatched onto done work discovered it mid-task:
- `3e542d14`: *"Verified (did not re-implement): internal/relay/doc.go was ALREADY correctly
  reconciled … committed at HEAD (commit e1bd842, prior feature-runner run)."*
- `MTLS-CROSSCHECK`: an agent opened with *"NOT STARTED — BLOCKED on MTLS-BIND … No code written"*
  and only then found the wiring already in the shipped binary
  (`internal/httpapi/peerprincipal.go:242-303`, `tlslisten.go:152`).
- `e120153b`: documentation and test-engineer passes both re-verified an already-shipped fix rather
  than building it.

## 3. Root cause(s)

### CONFIRMED PRIMARY — the commit and the flip are owned by two different agents, dispatched at two different times, with nothing binding them

The intended sequence and where it breaks:

1. **feature-runner / implementer writes code — and MUST NOT flip.** `.claude/agents/feature-runner.md:35`
   ("NEVER `git commit` …"), its parallel-safety rule *"Task state is NOT a file you edit: it lives
   in the Spec Server … mutated only by spec-keeper"* (`:58-60`), and its Definition-of-Done which
   says the flip happens *"via spec-keeper"* (`:101`). Code-only by contract.
2. **integrator commits — and MUST NOT flip.** It is *"The ONLY agent permitted to git commit"*
   (`.claude/agents/integrator.md:3`), but its "What you never do" list and role bound it to
   COMMITTED/REFUSED; it never touches task state. Its report ends at the sha.
3. **spec-keeper flips — but is a SEPARATE dispatch.** `.claude/agents/spec-keeper.md:46` ("When a
   task is reported complete, FLIP it") and CLAUDE.md step 7. This dispatch is the orchestrator's to
   make, after the commit, in a later turn.

So the `complete` call is nobody's *default* action at the moment the work finishes. The last agent
holding the task (integrator) is forbidden to make it; the agent allowed to make it (spec-keeper) has
to be summoned again. Under parallel load that summons is often not made, or the spec-keeper that IS
summoned runs an audit pass and — correctly, by the rules it is given — declines to flip because a
docs half is owed or a re-verification is pending (§2b). The result is a monotonic accumulation of
shipped-but-open tasks. The 2026-08-08 `IN-PROGRESS AUDIT` notes are the direct proof: the
responsible agent saw "SHIPPED" and still did not flip.

This is a hand-off gap, structurally identical in shape to the migration-number and task-key
collisions the repo already documents: an action that must happen is not atomic with the action that
makes it necessary.

### RANKED CONTRIBUTING CAUSES

**C1 — the invariant-7 / "documentation is part of done" hold (STRONG contributor, and partly
legitimate).** Many tasks shipped their code half but genuinely owed a CLI subcommand or an
`AGENT_PROTOCOL.md` / `CONTRACTS-*.md` entry in the same task. Holding those is *correct*. The defect
is that the owed half is then filed as a follow-up (or never dispatched) and the parent sits
`in_progress` indefinitely — so tasks that should have been completed-as-code-only + a paired FU (the
pattern `.claude/agents/*.md` already prescribe) instead linger as apparently-open full tasks.
Affects at least `2d7ce37b`, `MTLS-CROSSCHECK`, `MTLS-VERIFY`, `MTLS-CLIENTCERT`, `836c9ff8`,
`RELAY-25-FU-CORRELATION`, `MTLS-BIND`.
*Disproof test:* if this were the whole story, tasks with no owed half would flip promptly. They do
not — the pure-shipped bucket (`e120153b`, `94159d93`, `10e93262`, `IDEM-18`, `3e542d14`) also sits
open, which is why the hand-off gap (above), not this, is primary.

**C2 — multiple tasks in one commit; siblings left open.** A single commit closes one task's proof
while sibling task records are not flipped. Evidence: `94159d93` note — *"commit 6985d2c shipped the
code WITHOUT DECISIONS.md … entangled with other agents' SIGN/MTLS entries in the index, so git
status cannot go clean for this task"*; the AUTH `dc04a95` commit named in the brief carried several
AUTH tasks. Whoever flips must map commit→tasks and complete all of them; nothing does.
*Disproof test:* single-task commits would still flip if the hand-off existed — they don't, so C2 is
additive, not primary.

**C3 — the complete-URL sandbox guard (task `48be31d6`) — REAL but NOT the primary explanation for
these 23.** A worktree-isolated agent cannot `POST …/tasks/<id>/complete` because a sandbox bash
guard false-matches the bare substring "complete" and treats the HTTPS POST as an unverifiable git
op; an earlier agent even percent-encoded `complete`→`com%70lete` to evade it (a permission-control
bypass, recorded in `48be31d6`). This structurally blocks an isolated feature-runner from flipping
its own task. **But:** I found no note in any of the 23 journals showing a `complete` call
attempted-and-blocked for these tasks; the spec-keeper passes reached the API fine (they PATCHed
status and posted notes), and chose not to flip. So the guard is a genuine hazard that becomes
load-bearing the moment we try to make the *integrator* flip (the fix below runs into it directly),
but it did not cause this specific backlog drift.
*Evidence that would raise C3 to primary:* journal notes of the form "complete refused by guard" or
`com%70lete` usage against these task ids. Absent here.

**C4 — agents dying after commit, before flip (session limits).** Plausible given the many
killed/limit-hit agents this session, and it likely explains any task that never even received a
bookkeeping pass. But for the audited set the evidence points the other way: a spec-keeper DID run
and DID decline (§2b), so death-before-flip is not the driver for these 23.
*Disproof test:* a death-driven miss leaves no "SHIPPED, left in_progress" note; these tasks have
exactly that note, so they were seen and held, not dropped.

**C5 — the frozen mirror (task `0f4a0736`).** Not itself a cause of the missing flip; it amplifies
the cost by feeding stale state to anyone who reads `SPEC.md`/`SPEC/` instead of the API (§2c, §2d).

### Classification of the 23 in_progress tasks (method: §2a + full journal read)

| Bucket | Meaning | Tasks (P0 in bold) |
|---|---|---|
| 1 — fully done, only the `complete` flip missing | code + gates done, no real remainder | **e120153b**, **94159d93**, **INVITE-GATE-ENFORCE**, **ACK-3**, `10e93262`, `71cdaef8`, `IDEM-18`, `IDEM-12`, `ACK-17`, `3e542d14`, `52930611` |
| 2 — code+gates at HEAD, a real sub-deliverable owed (invariant-7 CLI/docs), often filed as a FU | overstates open effort; should be code-done + paired FU | **MTLS-CROSSCHECK**, **MTLS-BIND**, `2d7ce37b`, `MTLS-CLIENTCERT`, `MTLS-VERIFY`, `836c9ff8`, `RELAY-25-FU-CORRELATION`, `INVITE-HARDEN`, `d2cad9e7` (partial: 3 of 7 lines) |
| 3 — held pending reviewer re-verification | findings are record-keeping | **ACK-8** (overlaps bucket 2) |
| 4 — genuinely open / not a code task | | **TRIAGE-LOCK** (process lock, `proof_cmd = n/a`), **RELAY-25** (journal says full `fed-smoke.sh` proof was RED on 2026-08-14/15; a 2026-08-16 sweep reclassifies it "functionally done, held on a doc contradiction" — I did not run the script myself) |

**Rate, stated honestly:** sample = **all 23** `in_progress` tasks (100% of the population, 0% of the
537 `todo`). At least **11/23 (~48%)** are unambiguously done-but-not-flipped — everything complete,
only the `complete` call missing (bucket 1). **~21/23 (~91%)** have their substantive code at HEAD
with gates passed (buckets 1+2+3); only 2 are genuinely open. **Of the 9 in_progress P0s, 7 are
effectively done** (all but `TRIAGE-LOCK` and the borderline `RELAY-25`). Method limits: I ran 3 of
~23 proofs (chosen cheap, incl. 1 P0) and verified 20/20 code-present via symbol/ancestry checks plus
the full journals; I did not run the federation/script proofs.

## 4. The fix

**Smallest correct change (highest leverage): make the flip atomic with the commit.** The integrator
already commits and already holds the task; give it ONE additional mutation — immediately after a
successful pathspec commit AND the HEAD-compiles check, `POST tasks/<id>/complete` with the sha it
just minted and the owning agent's quoted `proof-check.sh` verdict — but ONLY for the tasks the
owning report marks FULLY done. This deletes the hand-off: the action that makes the flip necessary
now performs it. Filed as **`7befde72`** (P1, epic PROCESS).

This needs a deliberate, recorded narrowing of "only spec-keeper mutates task state" (CLAUDE.md;
`.claude/agents/integrator.md`, `spec-keeper.md`), scoped to the single `complete` call for the
task(s) the integrator just committed — recorded in `DECISIONS.md`, per the CLAUDE.md rule that a
change weakening an invariant needs an explicit decision.

**Latent landmines this fix must honour (found during this investigation):**

1. **`48be31d6` blocks it.** The complete-URL sandbox guard would refuse the integrator's own
   `POST …/complete` if the integrator runs worktree-isolated. **`48be31d6` must land first**, or the
   integrator must not be isolated. Do not paper over it with `com%70lete` — that is the bypass the
   task exists to kill. `7befde72` is filed BLOCKED-ON `48be31d6`.
2. **One commit can carry N tasks** (`6985d2c`, `dc04a95`). The integrator must complete ALL tasks the
   commit closes, not one; it needs the report's task→commit mapping.
3. **committed ≠ running / invariant-7 half owed.** Do NOT auto-flip a task whose report says
   code-only, awaiting-docs, or awaiting-deploy. Auto-flip only on a report that states fully-done;
   otherwise leave `in_progress` with a `status_note` and rely on the paired FU. This preserves the
   legitimate holds in bucket 2/3 (§3 C1).

**Complementary, lower-risk fixes:**

- **Read-only drift detector — `scripts/backlog-drift.sh`.** Filed as **`315899be`** (P2, PROCESS).
  Lists `in_progress`/`todo` tasks whose stored `proof_cmd` passes at HEAD (cheap symbol/ancestry
  checks first; proof execution opt-in), mutating nothing. Gives spec-keeper a flip worklist and
  gives triage a way to avoid dispatching a feature-runner onto done work. Safe to run any time
  because it does not compete for spec-keeper or mutate state.
- **Unblock the existing manual reconcile.** `HANDOVER-BACKLOG-RECONCILE` (`43d14776`, P2, epic
  HANDOVER) already specifies the one-off pass that flips or resets each `in_progress` task; it was
  filed off the critical path in 2026-08-08 pending tooling fixes (`521d68b5`, `a9a433dd`) and cited
  "15 in_progress" (now 23). Recommend spec-keeper re-scope and run it once `315899be` gives it a
  trustworthy instrument. (Not re-filed — building on the existing task, not duplicating it.)
- **Fix the guard `48be31d6`** (prerequisite for `7befde72`).
- **Unfreeze the mirror `0f4a0736`** so `SPEC.md`/`SPEC/` stop showing an even-staler state.

## 5. SPEC-ready task breakdown

Filed this session (do not re-file):

- **`7befde72`** — *Integrator flips the task to done atomically after a successful commit — close the
  commit→complete hand-off gap.* P1, epic PROCESS. The primary fix. BLOCKED-ON `48be31d6`; must honour
  landmines 2 and 3 above; needs a `DECISIONS.md` narrowing of the only-spec-keeper-mutates rule.
  Proof sketch: `integrator.md` carries the post-commit flip step scoped to fully-done reports, and
  `DECISIONS.md` records the narrowing.
- **`315899be`** — *`scripts/backlog-drift.sh`: read-only detector listing in_progress/todo tasks
  whose stored proof_cmd passes at HEAD.* P2, epic PROCESS. Read-only complement to `43d14776`.

Referenced, already in the backlog (owner/spec-keeper action, not new tasks):

- `48be31d6` — narrow the sandbox guard so `…/tasks/<id>/complete` is not treated as a git op
  (prerequisite for `7befde72`).
- `43d14776` HANDOVER-BACKLOG-RECONCILE — the manual flip/reset sweep; re-scope from "15" to the
  current set and run once the instrument is trustworthy.
- `0f4a0736` — `gen-spec-mirror.sh` refuses to write; unfreeze the mirror.

## 6. Cost / risk / rollback

**What this failure moves cost onto:**
- **Re-work of finished work.** Agents were dispatched onto done tasks and spent a full context
  discovering they were already shipped (`3e542d14`, `MTLS-CROSSCHECK`, `e120153b` — §2d). Each such
  wave is a wasted feature-runner + review panel.
- **Misdirected prioritisation.** 7 of 9 `in_progress` P0s are effectively done but read as open, so
  a triage agent picking "next P0" burns a wave rediscovering completed work instead of advancing the
  2 real P0s (`TRIAGE-LOCK`, `RELAY-25`).
- **Release uncertainty.** With the backlog overstating open work and the mirror frozen, no reader
  can tell from task state what is actually shipped at HEAD.

**Risk of the fix (`7befde72`):**
- It grants the integrator a task-state mutation, widening a deliberately narrow boundary. Contained
  by scoping it to the single `complete` for the just-committed task and requiring the fully-done
  signal in the report — a wrong auto-flip is the failure mode, mitigated by landmine 3.
- Depends on `48be31d6`; shipping `7befde72` first would make the integrator's flip fail silently
  under isolation.

**Rollback:** both new tasks are additive. `7befde72` is a contract/agent-definition change plus a
`DECISIONS.md` entry — revert the `integrator.md`/`DECISIONS.md` edit to restore the current hand-off.
`315899be` is a new read-only script — delete it. Neither touches product code, the WAL, or on-disk
state, so there is no data-migration or wire-format risk.

## Residual unknowns

- I did not run the script/federation proofs (`fed-smoke.sh`, some `proof-check.sh` wrappers). For
  those I relied on the journals; `RELAY-25`'s own notes disagree across dates (RED on 08-14/15,
  "functionally done" on 08-16), so its true state is the one item I cannot assert from evidence I
  reproduced.
- I sampled 3 of ~23 proofs directly; the other 17 code-present conclusions rest on symbol existence,
  commit ancestry, and gate verdicts in the journal, not on a re-run.
- The 537 `todo` tasks were not classified; the same drift may exist there (some `todo` tasks may also
  be done), but that was out of scope for this investigation, which targeted the `in_progress` set.
- Whether any of the missed flips were caused by an agent dying mid-turn (C4) cannot be distinguished
  from a deliberate hold for tasks that received no spec-keeper note at all — none of the 23 clearly
  fits that case, but the session's killed-agent count means it is possible for tasks outside this set.
