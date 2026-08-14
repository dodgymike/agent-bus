# Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 + DUR-4-FU-* + ID-2-WIRING + PROOF-CHECK-FU-RECURSION), and require proof_cmd at completion time

| Field | Value |
| --- | --- |
| Public id | `fc8cd234-d275-43a1-9cb0-d10bca4a4086` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T16:09:06.139375+00:00 |
| Updated | 2026-08-08T10:29:58.355506+00:00 |
| Completed | — |

## Proof command

```sh
test "$(bash scripts/spec-cloud.sh -s '/api/v1/projects/agent-bus/tasks?limit=500' | jq '[.[] | select(.proof_cmd == null and (.status != "cancelled" and .status != "superseded"))] | length')" = "0"
```

## Description

Verified via the Spec Server export this session: 20 of the 137 tasks in the agent-bus project have `proof_cmd == null` (the count given in the brief was correct). Of those 20, 6 are in a terminal state that will never be completed (5 RATCHET-* tasks: d86aaa65, be658b02, 58fd8bc3, e376433d, 9a404c64 -- all `superseded`; and ZZ-LOCKTEST e091e451 -- `cancelled`), so they arguably do not need a backfilled proof_cmd at all, only a decision that they are exempt. The remaining 14 are live/actionable and genuinely need one:

  CLI-1 (0495d133), CLI-2 (39318208), CLI-3 (6e70abe5), CLI-4 (137465b9), CLI-5 (86dea094),
  CLI-6 (47001cb4), CLI-7 (e600bde6), CLI-8 (ae4caacc), CLI-9 (93973755),
  ID-2-WIRING (838677e6, currently in_progress -- an owning agent should backfill this one directly rather than have it done for them),
  DUR-4-FU-DOCS (0b6d5c11), DUR-4-FU-DECISIONS (180f11f8), DUR-4-FU-TOOLING (26c2ce16),
  PROOF-CHECK-FU-RECURSION (69eb6f56).

DONE means: every one of the 14 actionable tasks above gets a real, non-vacuous proof_cmd (validated with `bash scripts/proof-check.sh '<cmd>'` before it is saved, exactly as this pass did for its own new tasks) -- for the CLI-* tasks that is naturally deferred until each CLI-N's shape is decided (a `scripts/bus-*.sh`-style invocation or a `go build ./cmd/agent-bus-cli && ...` smoke test, per whichever the implementer lands), and for the terminal 6 either a proof_cmd of `true` with a status_note explaining why, or spec-keeper leaves them proof-less on record as an accepted exemption for non-actionable tasks -- either is fine as long as it is a DECISION, not an omission.

POLICY RECOMMENDATION (the actual point of filing this): a missing proof_cmd should block flipping a task to `done` at LEAST as hard as a VACUOUS one does. Today scripts/proof-check.sh classifies and grades whatever proof_cmd IS supplied, but nothing stops `complete` from succeeding when proof_cmd was never set in the first place -- which is a strictly WORSE version of the vacuous-pass problem this project already fixed once (task 84b76d5e, "a `-run` pattern that matches no test must FAIL, not pass vacuously"): at least a vacuous `-run` pattern names something checkable in principle; a null proof_cmd names nothing at all. Recommend: (1) completing a task should require running `bash scripts/proof-check.sh '<cmd>'` and quoting its verdict in test_summary, not just asserting things worked; (2) the Spec Server's `complete` endpoint (or a spec-keeper-side check ahead of calling it) should refuse a task with proof_cmd unset UNLESS an explicit skip reason is recorded (mirroring how AGENT_LOG.md already carries explicit skip justifications for the reviewer/security chain).

proof_cmd validated via scripts/proof-check.sh: verdict=FAIL (exit 1) today -- 14 actionable tasks currently have proof_cmd unset; the count will read 0 once every one of them is backfilled or explicitly exempted. (Scoped to non-terminal tasks: cancelled/superseded tasks are excluded from the count on purpose, per the exemption discussion above.)

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [84b76d5e-fe02-4651-9828-caba3d82606b](../../TOOLING/Proof-command-guard-a-run-pattern-that-matches-no-test-m--84b76d5e/task.md) — Proof-command guard: a \`-run\` pattern that matches no test must FAIL, not pass vacuously (done)
- [CLI-1](../../CLI/CLI-1--0495d133/task.md) — CLI-1: client package (NOT under internal/) + CLI subcommand skeleton -- the single clien… (done)
- [CLI-2](../../CLI/CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [CLI-3](../../CLI/CLI-3--6e70abe5/task.md) — CLI-3: watch -- long-poll tail, human-readable for a person and NDJSON for a pipe (replac… (done)
- [CLI-4](../../CLI/CLI-4--137465b9/task.md) — CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadc… (done)
- [CLI-5](../../CLI/CLI-5--86dea094/task.md) — CLI-5: agents -- roster listing (replaces bus-agents.sh) (done)
- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (done)
- [CLI-7](../../CLI/CLI-7--e600bde6/task.md) — CLI-7: peers -- relay topology and health (replaces bus-peer.sh) (todo)
- [CLI-8](../../CLI/CLI-8--ae4caacc/task.md) — CLI-8: doctor -- diagnose a broken setup with a specific remedy per failure (todo)
- [CLI-9](../../CLI/CLI-9--93973755/task.md) — CLI-9: shell completion + man/usage polish (todo)
- [DUR-4-FU-DECISIONS](../../DUR/DUR-4-FU-DECISIONS--180f11f8/task.md) — DUR-4-FU-DECISIONS: record the SHIPPED damage-class taxonomy in DECISIONS.md -- which cla… (todo)
- [DUR-4-FU-DOCS](../../DUR/DUR-4-FU-DOCS--0b6d5c11/task.md) — DUR-4-FU-DOCS: state invariants 4/6 as explicit NARROWINGS + document the WAL recovery AP… (todo)
- [DUR-4-FU-TOOLING](../../DUR/DUR-4-FU-TOOLING--26c2ce16/task.md) — DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start (superseded)
- [ID-2-WIRING](../../ID/ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (superseded)
- [PROOF-CHECK-FU-RECURSION](../../TOOLING/PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) — PROOF-CHECK-FU-RECURSION: bash scripts/proof-check.sh hangs / spawns runaway processes wh… (todo)
- [RATCHET-1](../../RATCHET/RATCHET-1--d86aaa65/task.md) — RATCHET-1: DEEP DIVE -- how to get a double ratchet WITHOUT writing our own crypto (superseded)
- [RATCHET-3](../../RATCHET/RATCHET-3--be658b02/task.md) — RATCHET-3: Do we need full Signal semantics? -- the cheaper-alternative check (superseded)
- [RATCHET-4](../../RATCHET/RATCHET-4--58fd8bc3/task.md) — RATCHET-4: Broadcast fan-out under pairwise ratchets (superseded)
- [RATCHET-5](../../RATCHET/RATCHET-5--e376433d/task.md) — RATCHET-5: Ratchet state durability vs invariants 4/5 -- the key-reuse trap (superseded)
- [RATCHET-8](../../RATCHET/RATCHET-8--9a404c64/task.md) — RATCHET-8: Record the decision, then gate the CRYPTO epic on it (superseded)
- [ZZ-LOCKTEST](../../UNASSIGNED/ZZ-LOCKTEST--e091e451/task.md) — ZZ-LOCKTEST: verify If-Match CAS (cancelled)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [932fe938-0e42-42d8-802d-ff018cb6c955](../Audit-stored-proof_cmds-for-the-subtest-skip-vacuous-sha--932fe938/task.md) — Audit stored proof_cmds for the subtest-skip vacuous shape (parent-PASS/hidden-child-SKIP… (todo)
- [CORE-1](../../CORE/CORE-1--eea035e4/task.md) — CORE-1: Repo skeleton: go.mod, internal/ package layout, .gitignore (done)
- [HANDOVER-BACKLOG-RECONCILE](../../HANDOVER/HANDOVER-BACKLOG-RECONCILE--43d14776/task.md) — HANDOVER-BACKLOG-RECONCILE: the inherited backlog stops lying about what is in flight (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
