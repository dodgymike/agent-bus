# AUTH-1-FU-SESSIONSCALE: session-table O(n) scans and refuse-not-evict policy cause CPU/lock amplification

| Field | Value |
| --- | --- |
| Public id | `067b80cf-89a4-4485-82bd-27e8a769239e` |
| Key | _(null in the export)_ |
| Epic | [AUTH](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T18:34:14.051325+00:00 |
| Updated | 2026-08-14T22:29:25.440476+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestSweepLockedAmortizedUnderLoad|TestEnrollThenSessionGlobalCapFailsClosed|TestSessionBeginNoVictimLockout' ./internal/auth
```

## Description

Follow-up to AUTH-1 (public_id 54fa94c0-6ca3-459a-aaa2-a3ea047f97d9). Security measured roughly 1.04 ms of exclusive global-mutex time per ~180-byte request at default caps with a full table (a ~960 req/s ceiling for the WHOLE auth surface), caused by the O(n) sweepLocked / countPendingLocked / oldestPendingLocked scans over a 16384-entry table, all held under sessMu.

RIDER, ADDED 2026-08-14 (spec-keeper, coordinator-directed audit against a clean ec14bb8 overlay) -- REQUIRED READING BEFORE ANY IMPLEMENTATION. This is the rider DECISIONS.md:871 ("Constraint this places on AUTH-1-FU-SESSIONSCALE") says is recorded on this task -- it was NOT actually present in this description until now; that gap is fixed here.

THE ORIGINAL FIX BELOW ("change the full-table policy to evict-oldest-pending rather than refuse") IS FORBIDDEN. DECISIONS.md:867-871 rules on this directly: "That reintroduces cross-tenant challenge destruction -- far less severe (16384 begins inside a victim's round trip, versus 9 per round) but the same class -- and it will fail the session_test.go subtest asserting ErrCapacity. That failure is a constraint to honour, not a test to update." AUTH-1-FU-PENDINGCAP removed a PER-AGENT pending cap for exactly this reason (an unauthenticated flooder can name a victim's agent_id and evict the victim's own pending challenge); a GLOBAL evict-oldest-pending policy reintroduces the same class of attack at the whole-table level -- once the table is full, ANY new begin (however sized the flood) evicts whichever entry is globally oldest, which can be a legitimate agent's own in-flight challenge with no way for that agent to protect it.

TWO TESTS DOCUMENT THIS CONSTRAINT, verified directly against the ec14bb8 tree:
- session_test.go's "a refusal at the global cap destroys no pending challenge" subtest (inside TestEnrollThenSessionGlobalCapFailsClosed, ~:1064-1111) asserts BeginSession at the full table returns errors.Is(err, ErrCapacity) -- i.e. REFUSES -- and that the table is left UNCHANGED, and that a caller's own already-issued pending challenge still completes afterward. An evict-oldest-pending policy breaks BOTH assertions: the call would succeed (not return ErrCapacity) and the table WOULD change (an entry evicted).
- TestSessionBeginNoVictimLockout (:412-459, doc comment + setup) is the adversarial test that a PER-AGENT cap was removed to satisfy, and its own comment says plainly "this test is what stops one being reintroduced." Its "no third party can destroy an already-issued challenge" subtest explicitly documents the GLOBAL-eviction failure mode this rider is about ("Guards a future regression that keys the cap globally-but-wrongly"). PRECISION, so this rider is not overstated: as currently CONSTANTED (floodSize=32, MaxSessions=4096), this specific subtest does not reach the global cap and so would not go red with today's exact numbers -- but its documented property ("no amount of traffic naming the victim may destroy the victim's ability to authenticate") is incompatible in principle with any global evict-oldest-pending policy exercised at real scale, and an implementer who validates a real fix by actually filling the table (which the task's own motivating scenario requires testing) will trip it.

REQUIRED SCOPE FOR THIS TASK NOW: the O(n)-scan performance fix (sweepLocked / an amortized structure keyed on expiry) remains valid and wanted. The full-table policy MUST STAY refuse-with-ErrCapacity. Do not implement, propose, or land any eviction-on-full-table behaviour without a new, explicit, dated DECISIONS.md entry overriding the 2026-08-1x ruling above -- and if that is ever seriously proposed, it needs the SAME per-agent-fairness analysis that killed the per-agent cap, applied at the global scope.

NOTE, function names updated (2026-08-14): TWO of the three hot functions this task originally named -- countPendingLocked and oldestPendingLocked -- were DELETED by AUTH-1-FU-PENDINGCAP (they existed only to support the now-forbidden eviction policy). Only sweepLocked remains as a real O(n) scan target from the original set. A different, unrelated O(n) scan now lives at session.go:451-456 (BeginSession's per-agent ACTIVE-session count, added for AUTH-1-FU-ACTIVECAP) -- in scope for this task's performance goal if it becomes a hot path, but it is not what the original 1.04ms measurement was about and is a separate structure (per-agent, not global) requiring its own amortization approach if addressed here.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [AUTH-1-FU-ACTIVECAP](../AUTH-1-FU-ACTIVECAP--2d92b699/task.md) — AUTH-1-FU-ACTIVECAP: cap ACTIVE sessions per agent -- the one place an agent-id-keyed cap… (done)
- [AUTH-1-FU-PENDINGCAP](../AUTH-1-FU-PENDINGCAP--687ad8c9/task.md) — AUTH-1-FU-PENDINGCAP: MaxPendingPerAgent is a lockout primitive, not a defence -- rekey o… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
