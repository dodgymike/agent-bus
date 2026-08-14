# DUR-9: Wire the WAL into server startup (open, replay, hold for process lifetime, expose to handlers)

| Field | Value |
| --- | --- |
| Public id | `8234db61-a96f-4fd9-bb3b-e208a065630b` |
| Key | DUR-9 |
| Epic | [DUR](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T14:12:49.269655+00:00 |
| Updated | 2026-08-02T17:52:18.068650+00:00 |
| Completed | 2026-08-02T17:52:18.068632+00:00 |

## Proof command

```sh
grep -q '"github.com/dodgymike/agent-bus/internal/wal"' cmd/agent-bus/main.go && test $(go test -run TestServerOpensWALOnStart -v ./... 2>&1 | grep -c "=== RUN") -gt 0 && go test -race -run TestServerOpensWALOnStart ./... && rm -rf /tmp/agent-bus-dur9 && AGENT_BUS_RUN_DIR=/tmp/agent-bus-dur9 AGENT_BUS_LISTEN=127.0.0.1:8091 bash scripts/bus-serve.sh start && test -s /tmp/agent-bus-dur9/data/bus.wal && AGENT_BUS_RUN_DIR=/tmp/agent-bus-dur9 AGENT_BUS_LISTEN=127.0.0.1:8091 bash scripts/bus-serve.sh stop
```

## Status note

DUR-9 dispatched by backlog-triage-pass5-b828c013 to feature-runner(opus). Boundary: cmd/agent-bus/** + internal/httpapi/** ONLY; internal/wal/** is FORBIDDEN (under DUR-10 review).

## Description

THE DURABILITY PLANE IS NOT WIRED TO THE SERVER. Verified 2026-08-02: `grep -rn 'internal/wal' cmd/ internal/httpapi/` matches only a COMMENT in cmd/agent-bus/main.go:165 -- there is no import; `wal.Open` has ZERO non-test callers in the whole repo; and internal/httpapi/server.go:101-102 registers exactly two routes (/healthz, /v1/info) beside a well-tested WAL library that no request path touches. DUR-1..DUR-4 are all `done` and NONE of their behaviour is live in the binary. That is the single biggest gap between what the backlog claims and what the process does.

SCOPE (this task only wires what already exists -- do NOT add new WAL features):
1. Server startup opens the WAL for the configured -data-dir exactly once (wal.Open), holds the *wal.Log for the process lifetime, and closes it on the SIGINT/SIGTERM shutdown path already in main.go.
2. Startup REPLAYS on open and applies the recovered state before the listener starts accepting -- serving must never begin from an unreplayed store (invariant 5: memory is the serving copy, disk is the truth).
3. A failed open/replay is a FATAL startup error with a non-zero exit and a clear operator message -- never a silent start with an empty store. The one permitted exception is the verified torn tail DUR-4 already repairs.
4. The Log is exposed to handlers (a field on the httpapi Server / a narrow interface), so later epics (MSG, AUTH, IDEM, SIGN) have a durable store to commit into. No handler needs to USE it in this task.
5. Startup logs, at info, the data dir, the number of records replayed and the repair outcome.

GATED ON DUR-8 (exclusive data-dir lock, in flight 2026-08-02). Both edit cmd/agent-bus/main.go startup, and the ORDER is load-bearing: acquire the exclusive data-dir lock FIRST, then open the WAL -- opening a WAL a second process already holds is exactly the corruption DUR-8 exists to prevent. Do not start this until DUR-8 is done, or you will collide in main.go and get the ordering backwards.

NOT IN SCOPE: the audit log (DUR-5), snapshots (DUR-7), any new route, any message being written. This is wiring.

PROOF NOTES: the proof_cmd is non-vacuous BY CONSTRUCTION and FAILS TODAY (verified: proof-check.sh --quiet -> verdict=FAIL exit=1, stops at clause 1 because main.go has no wal import). Clause 2 is the DUR-3 anti-vacuity guard (assert at least one test actually RUNs before trusting the run). The last clauses are the invariant-7 end-to-end check: a real server brought up through scripts/bus-serve.sh must leave a non-empty bus.wal in its data dir -- 'the handler is written' is not the same as 'a running server does it'. Uses an isolated AGENT_BUS_RUN_DIR/port, never the tracked data/ dir.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-1](../DUR-1--c51e1959/task.md) — DUR-1: WAL record framing + writer (done)
- [DUR-10](../DUR-10--bab09b2e/task.md) — DUR-10: Review the RepairTail truncation veto -- half is already in \`main\` UNREVIEWED (la… (done)
- [DUR-3](../DUR-3--d8a991ea/task.md) — DUR-3: Replay/recovery on start (done)
- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [DUR-5](../DUR-5--a7123e88/task.md) — DUR-5: Append-only message audit log (done)
- [DUR-7](../DUR-7--ba6739e6/task.md) — DUR-7: Snapshot/compaction follow-up (bounds WAL replay time) (todo)
- [DUR-8](../DUR-8--6f099429/task.md) — DUR-8: Exclusive lock on the bus data directory (stop two servers destroying one WAL) (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [7bdf6c98-a1a5-488c-af70-4b2332b101df](../../MSG/Acceptance-criterion-for-the-first-durable-write-HTTP-ha--7bdf6c98/task.md) — Acceptance criterion for the first durable-write HTTP handler (MSG-2/MSG-3): wal.ErrClose… (todo)
- [DUR-4-FU-TOOLING](../DUR-4-FU-TOOLING--26c2ce16/task.md) — DUR-4-FU-TOOLING: Operator tooling for a WAL that refuses to start (superseded)
- [d23e9864-fad5-4d49-94b8-8c10f71663f6](../Shutdown-timeout-path-can-release-the-data-dir-lock-whil--d23e9864/task.md) — Shutdown-timeout path can release the data-dir lock while handlers are still running (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
