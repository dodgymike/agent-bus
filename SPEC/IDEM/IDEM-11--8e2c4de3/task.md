# IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention window

| Field | Value |
| --- | --- |
| Public id | `8e2c4de3-5752-4d4c-a321-778cb6daa6e1` |
| Key | IDEM-11 |
| Epic | [IDEM](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | core |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:10:34.566117+00:00 |
| Updated | 2026-08-21T09:35:54.152389+00:00 |
| Completed | 2026-08-21T09:35:54.152373+00:00 |

## Proof command

```sh
go test -race -run "TestIdemCrash|TestRetentionWindowDerivation|TestMemoryBoundDerivation|TestAppliedKeyRecoveryFromPreIdemLog" ./internal/idem/ ./internal/hub/
```

## Status note

RESET in_progress -> todo, 2026-08-14 (spec-keeper, on the feature-runner's audit). WHY THE RESET MATTERS MORE THAN THE STATUS: this task was sitting in_progress with owner=None, which makes it INVISIBLE to claim-next -- the exact RELAY-6 failure mode that hid a blocker for six days. Nobody was working it and nobody could be handed it. It is now claimable again.

STATE OF THE WORK. CODE IS COMPLETE AND PROVEN: the feature-runner re-ran the stored proof in a clean `git archive HEAD` overlay at HEAD 208dacd -- `go test -race -run TestIdemCrash ./internal/hub/` -> proof-check verdict=PASS class=test exit=0 tests_run=4 top_level=4 skipped=1 failed=0.

THE SOLE REMAINING WORK IS THE DOCUMENTATION PAPER TRAIL, and it is a REAL blocker, not bookkeeping: the reviewer ruled explicitly on 2026-08-03 that IDEM-11 must NOT be flipped to done until it lands. DECISIONS.md:706-708 still says applied keys "fail closed" with "retention 1 day or 1 GB" -- the OPPOSITE of the shipped bounded-window / 50h10m22s / 65536-entry behaviour -- and CONTRACTS-HTTP.md:164 still documents a coupling that was DELETED. Completing this task today would put a false claim in the backlog on top of docs that already contradict shipped code.

OVERLAP, DELIBERATE: that same work is separately tracked as IDEM-11-FU-PAPERTRAIL (c416a458-0bde-4a9b-8818-5dfe11efb48e, P1, todo). The two are not independent. WHOEVER CLAIMS EITHER ONE SHOULD CLOSE BOTH TOGETHER -- do not fix the docs under the follow-up and leave this task open, and do not close this task while the follow-up still describes live contradictions. Of the three originally-named follow-ups, IDEM-11-FU-FAIRSHARE (5abec835) is DONE and that debt is resolved.

## Description

PRIORITY P0 (escalated from the epic default of P1): every other IDEM task's correctness depends on this store actually being durable, and per invariant 5/10 a store that LOOKS idempotent under normal operation but silently reverts to double-applying after a restart is the exact failure mode invariant 10 exists to prevent -- the same reasoning that makes DUR-1/DUR-2 P0 rather than P1. The store answering 'have I already applied this (agent, key) pair, and if so what was the result' MUST be durable, NOT an in-memory-only cache: a restart must not turn a duplicate into a second effect (invariant 10 explicitly, plus invariant 5 -- memory is the serving copy, disk is the truth). GATED on DUR-1 (WAL record framing), DUR-2 (two-phase prepare->commit write path) and DUR-3 (replay/recovery on start, currently in_progress -- do NOT touch DUR-3 itself, this task only depends on its contract). Applied-key records are written through the SAME prepare->commit path as every other durable write (invariant 4) and rebuilt by replaying the WAL on start, exactly like message history and the roster; the write-path half of this task can be developed against DUR-3's documented contract in parallel, but the recovery half cannot land until DUR-3 does. RETENTION is the sharp edge and MUST be decided, not left vague: keys cannot be kept forever (unbounded growth on an append-only durable store is DUR-7's snapshot/compaction problem, multiplied by one record per mutating call ever made). Choose ONE concrete bounded window -- by wall-clock time (e.g. a fixed 24h TTL), by count (e.g. the last N keys per agent), or by sequence range (e.g. keys older than current-sequence-minus-W) -- and record the choice plus its rationale in DECISIONS.md; a configurable-with-no-default is not an acceptable substitute for picking one. Explicitly specify and implement the behaviour for a retry that arrives AFTER its key's window has expired: it MUST FAIL CLOSED -- rejected as unrecognized/expired with a distinct, documented error -- never silently re-applied as if it were a fresh operation and never silently treated as already-seen when it in fact was not. Depends on IDEM-10 for the key shape being stored. BLOCKS IDEM-12 through IDEM-15.

--- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-2 and IDEM-3, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) SAME-TRANSACTION IS THE LOAD-BEARING REQUIREMENT: the applied-key record MUST commit in the SAME two-phase (prepare -> commit -> fsync) transaction as the effect it records. Not a second write, not ordered 'after' the effect. If the message commits and the key record does not, a crash in that window plus a client retry produces exactly the duplicate this epic exists to prevent -- and it stays invisible in ordinary testing because the window is small. (b) STORE THE RESULT, NOT JUST THE KEY: the record holds the scope tuple, IDEM-10's payload fingerprint, the MINTED RESULT (message id, sequence, timestamp) and the commit time. A key with no stored result cannot satisfy IDEM-12's 'return the original result verbatim'. (c) RESERVE THE ON-DISK RECORD-TYPE NUMBER via POST /api/v1/projects/agent-bus/reservations {"namespace":"record-type"} -- never hand-pick it; that is the classic parallel-agent collision, and DUR-1's framing already has neighbours. Bump the on-disk format version the same way if the framing changes. (d) RECOVERY MUST BE PREFIX-CONSISTENT: a key whose effect was NOT committed must not appear as applied after replay (invariant 5). (e) DERIVE THE RETENTION WINDOW, DO NOT PICK A ROUND NUMBER: it must EXCEED the maximum client retry horizon or the guarantee is a lie in exactly the case that matters. The realistic worst cases to derive it from are a peer reconnecting after an outage (RELAY-4's backoff ceiling) and a long-poll client resuming after a network partition. (f) EVICTION MUST BE CONSISTENT ACROSS MEMORY AND DISK: evicting in memory while the record survives on disk (or the reverse) makes behaviour depend on whether a restart happened since -- the worst kind of intermittent bug. State how eviction interacts with DUR-7 snapshot/compaction: a snapshot must neither silently reinstate evicted keys nor drop live ones. (g) MAKE THE BOUND OBSERVABLE: expose the applied-key count and the oldest-key age wherever CORE-5's inspect/metrics endpoint lands, so the bound is verified in production rather than assumed.

--- CONTRADICTION RAISED BY THE MERGE (2026-08-02), MUST BE RESOLVED BY WHOEVER IMPLEMENTS THIS TASK: the paragraph above says a retry arriving after its key's window expired MUST FAIL CLOSED (rejected as unrecognized/expired), while withdrawn IDEM-3 and the surviving IDEM-18 doc task both state the honest guarantee as 'duplicates are suppressed within the retention window' -- i.e. a retry arriving after eviction IS applied as a NEW operation and produces a second effect. Both cannot ship. THE MECHANISM PROBLEM THAT DECIDES IT: keys are opaque client-supplied strings (IDEM-10), so a server that has evicted a key CANNOT distinguish it from a key it has never seen -- and every legitimate first attempt is a key it has never seen. Fail-closed is therefore only implementable if this task ALSO specifies a mechanism that makes expiry detectable (e.g. a retained eviction watermark plus a verifiable mint-time carried with the key); designing that mechanism is in scope here, assuming it is not. So: either (i) specify that mechanism and keep fail-closed, or (ii) adopt the bounded-window statement and document the boundary honestly. Record the choice and its rationale in DECISIONS.md, and make IDEM-18's PROTOCOL.md wording and IDEM-16's past-the-window test match it -- both of those currently assume (ii).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [IDEM-11-FU-PAPERTRAIL](../../DOCS/IDEM-11-FU-PAPERTRAIL--c416a458/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CORE-5](../../CORE/CORE-5--06c5b1f5/task.md) — CORE-5: Observability: metrics/inspect endpoint (follow-up) (superseded)
- [DUR-1](../../DUR/DUR-1--c51e1959/task.md) — DUR-1: WAL record framing + writer (done)
- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-3](../../DUR/DUR-3--d8a991ea/task.md) — DUR-3: Replay/recovery on start (done)
- [DUR-7](../../DUR/DUR-7--ba6739e6/task.md) — DUR-7: Snapshot/compaction follow-up (bounds WAL replay time) (todo)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11-FU-FAIRSHARE](../IDEM-11-FU-FAIRSHARE--5abec835/task.md) — IDEM-11-FU-FAIRSHARE: applied-key capacity is bus-wide fail-closed with no per-agent shar… (done)
- [IDEM-11-FU-PAPERTRAIL](../../DOCS/IDEM-11-FU-PAPERTRAIL--c416a458/task.md) — IDEM-11-FU-PAPERTRAIL: DECISIONS.md and CONTRACTS-HTTP.md state the OPPOSITE of what IDEM… (todo)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (in_progress)
- [IDEM-15](../IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)
- [IDEM-18](../IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (in_progress)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-3](../IDEM-3--e34f9c31/task.md) — IDEM-3: Bounded dedupe window -- retention policy, eviction, and the honest statement of… (superseded)
- [RELAY-4](../../RELAY/RELAY-4--5ac738b4/task.md) — RELAY-4: Peer-down retry/backoff (done)
- [RELAY-6](../../RELAY/RELAY-6--0f7275b9/task.md) — RELAY-6: Record the FEDERATION deployment assumptions (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-4](../../CLI/CLI-4--137465b9/task.md) — CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadc… (done)
- [CLI-5](../../CLI/CLI-5--86dea094/task.md) — CLI-5: agents -- roster listing (replaces bus-agents.sh) (done)
- [HUB-FU-RECOVER-IDEM-RELAY-ARM](../../RELAY/HUB-FU-RECOVER-IDEM-RELAY-ARM--5e74485a/task.md) — HUB-FU-RECOVER-IDEM-RELAY-ARM: recoverIdemRecord has no relay arm, and a lost relay appli… (todo)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11-FU-DOWNGRADE](../../DUR/IDEM-11-FU-DOWNGRADE--84f5ad57/task.md) — IDEM-11-FU-DOWNGRADE: an old binary SILENTLY DISCARDS acknowledged writes after IDEM-11 -… (todo)
- [IDEM-11-FU-FAIRSHARE](../IDEM-11-FU-FAIRSHARE--5abec835/task.md) — IDEM-11-FU-FAIRSHARE: applied-key capacity is bus-wide fail-closed with no per-agent shar… (done)
- [IDEM-11-FU-HUBAPPLY](../IDEM-11-FU-HUBAPPLY--a9f827b9/task.md) — IDEM-11-FU-HUBAPPLY: hub.Apply returns early for non-message Entry.Kind, so IDEM-13/14/15… (todo)
- [IDEM-11-FU-PAPERTRAIL](../../DOCS/IDEM-11-FU-PAPERTRAIL--c416a458/task.md) — IDEM-11-FU-PAPERTRAIL: DECISIONS.md and CONTRACTS-HTTP.md state the OPPOSITE of what IDEM… (todo)
- [IDEM-11-FU-THROUGHPUT](../IDEM-11-FU-THROUGHPUT--4b67701c/task.md) — IDEM-11-FU-THROUGHPUT: sustained ceiling roughly halves to ~0.36 ops/s and nothing surfac… (todo)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (in_progress)
- [IDEM-13](../IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-15](../IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)
- [IDEM-17](../IDEM-17--8b1e85fd/task.md) — IDEM-17: Crash-injection test -- restart mid-retry-window still yields exactly one effect (done)
- [IDEM-18](../IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (in_progress)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-3](../IDEM-3--e34f9c31/task.md) — IDEM-3: Bounded dedupe window -- retention policy, eviction, and the honest statement of… (superseded)
- [MSG-5](../../MSG/MSG-5--9d125bc6/task.md) — MSG-5: Messaging durability integration test (done)
- [RELAY-52-FU-HUBDISCARDS](../../RELAY/RELAY-52-FU-HUBDISCARDS--d2cad9e7/task.md) — RELAY-52-FU-HUBDISCARDS: remaining untested hub/mint/roster discard-and-recovery log lines (done)
- [RELAY-52-FU-HUBDISCARDS-FU-IDEMDISCARD-UNCAPPED](../../RELAY/RELAY-52-FU-HUBDISCARDS-FU-IDEMDISCARD-UNCAPPED--d858bf19/task.md) — RELAY-52-FU-HUBDISCARDS-FU-IDEMDISCARD-UNCAPPED: the two applied-key discard lines are un… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
