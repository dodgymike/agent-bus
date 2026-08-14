# Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable index floor outside the log (invariant 1, second instance)

| Field | Value |
| --- | --- |
| Public id | `db350e39-3dde-4166-b241-b21fa4635359` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T20:45:40.395665+00:00 |
| Updated | 2026-08-08T10:29:42.625093+00:00 |
| Completed | 2026-08-07T19:24:09.436704+00:00 |

## Proof command

```sh
go test -race -count=1 -run 'TestWALIndexFloorAcceptsTheBodyShippedInMain|TestWALIndexFloorLegacyDigestNeverCarriesASeal|TestWALIndexFloorForgedSealIsNotBelieved|TestWALIndexFloorTamperedUnderItsOwnKeyIsFatal|TestWALIndexFloorRemedyDoesNotUnderstateItsCost|TestWALIndexFloorSurvivesALostMACKey|TestWALIndexFloorReapIsNotAGlobPattern|TestWALIndexFloorCorruptFileIsFatalAndNamesTheRemedy|TestWALIndexFloorP0BTruncationSweepNeverReissues|TestWALSevereDiscardIsNeverCrowdedOutOfTheLog' ./internal/wal
```

## Status note

REOPENED 2026-08-07 (un-superseded): the prior collapse into 6ebe51be is reversed -- the actual fix for this task's specific defect (whole-log quarantine losing the sequence floor) shipped under this task and sibling e120153b via the durable wal-index-floor mechanism (internal/wal/indexfloor.go, ondisk-format-version=4). DUR-12 block also CLEARED (DUR-12/cbc9ab0c is done). Title and description updated to drop the superseded refuse-to-start premise; see the appended SUPERSEDED PREMISE paragraph. Reviewer and security gates running under feature-runner.

## Description

THE DEFECT: internal/wal/recover.go:252-262 -- when the entire WAL log is quarantined as corrupt, recovery starts a FRESH log at index 1. No PREPARE record survives anywhere in the log. The message-sequence high-water mark derived at startup is therefore 0, and a bus that then mints sequences from 1 REISSUES EVERY SEQUENCE NUMBER IT HAS EVER USED -- not one index at a damaged tail, but the bus's entire history. Nothing downstream can detect this; it is silent.

This is INVARIANT 1 (CLAUDE.md: 'ids are never reused ... including across restarts') and the user's ruling stands WITHOUT narrowing (commit 4110946; DECISIONS.md 'Five decisions...' section 3; amended invariant 1 in CLAUDE.md). That ruling was made about the tail-salvage reissue tracked on e120153b-9d8a-4b6a-bd4e-89431954496b. THIS TASK IS A SEPARATE, STRICTLY WORSE INSTANCE OF THE SAME VIOLATION and is explicitly NOT covered by e120153b and NOT a duplicate of it: e120153b is about reissuing one index at a damaged tail; this is about the WHOLE log vanishing and the floor silently becoming 0.

REQUIRED BEHAVIOUR (fail-closed, proposed): startup must distinguish 'legitimately empty -- a brand-new bus that has never minted anything' from 'quarantined -- the high-water mark is UNKNOWN because the log that would prove it was discarded as corrupt'. In the second case startup must REFUSE TO START rather than silently resuming from 1. This is a deliberate, narrow exception to the always-restart rule -- in the same family as the user's decision that a missing/wrong MAC key is FATAL. Always-restart exists to stop media damage holding the bus hostage; it does not exist to license silently reissuing every id the bus ever minted.

*** CONSENT-SENSITIVE -- CONFIRM WITH THE USER BEFORE IMPLEMENTATION ***. The user reverted a broader refuse-to-start policy once already. The fail-closed DIRECTION follows from invariant 1 as ruled, but the specific mechanism (what marks 'legitimately empty' vs 'quarantined-unknown', where that marker is durably recorded, and the exact refuse-to-start condition) needs explicit sign-off before code is written, not just inferred from the earlier ruling on the tail-salvage case.

CROSS-REFERENCES:
- e120153b-9d8a-4b6a-bd4e-89431954496b -- the tail-salvage reissue defect at the same invariant, different site (damaged tail, not whole-log quarantine). This task is NOT a duplicate; do not merge them.
- 8c9b6489-abb1-444e-9eeb-3ff87646f632 (ID-2-WIRING-SEAL, landing now) -- provides the machinery this needs: Seal()/ErrFloorUnproven, where a sequence allocator is born UNSEALED and Next() refuses to issue until the floor is proven. Its in-tree doc comment already states the requirement verbatim: the floor must be derived 'from the highest sequence number EVER WRITTEN TO DISK -- every prepare, committed, aborted and dangling alike'. The whole-log-quarantine case is PRECISELY the case where that derivation is impossible -- so the seal must never be taken, and Next() must keep refusing (ErrFloorUnproven), which is the mechanism this task should most likely use to implement the refuse-to-start behaviour rather than inventing a new one.
- c31f6999-da4e-400d-ab55-178b82e2a42e (ID-2-WIRING-OBSERVER) and 838677e6-d424-45ed-8580-924cb2da28a6 (ID-2-WIRING) -- the floor-derivation machinery this interacts with. The ID-2-WIRING-SCHEMA agent (80b54ee4-55d5-44b8-a479-c0a13343d15a) recorded this whole-log-quarantine case as a required fail-closed behaviour of what it called 'ID-2-WIRING-STARTUP' while choosing Option A'.

ORDERING: lives in internal/wal, which DUR-12 (cbc9ab0c-3b34-48d0-acd8-5eabd4dc4a02, in_progress -- CRC32C to HMAC-SHA256 MAC swap, on-disk format v2) owns this loop right now. Do NOT dispatch/implement until DUR-12 lands -- same ordering constraint that applies to e120153b.

PROOF_CMD VERDICT (recorded now, pre-implementation): `bash scripts/proof-check.sh 'go test -race -run TestRecover_WholeLogQuarantine_RefusesStartOnUnprovenSequenceFloor ./internal/wal'` returns verdict=VACUOUS (exit 4, 'no tests to run', empty_pkgs=1) because the test does not exist yet -- this is expected and is recorded here explicitly so nobody mistakes an unwritten-test 0-exit for a pass. DO NOT complete this task on a VACUOUS proof; the test must be written (quarantine a log holding sequences up to N, restart, assert the bus REFUSES TO START rather than minting from 1) and proof-check must report PASS before this is marked done.

--- SUPERSEDED PREMISE (appended 2026-08-07 by spec-keeper, original text above left intact) ---

This task's TITLE and description originally asserted, as the REQUIRED BEHAVIOUR: "startup must
REFUSE TO START rather than silently resuming from 1" on a whole-log quarantine, and the task was
marked *** CONSENT-SENSITIVE -- CONFIRM WITH THE USER BEFORE IMPLEMENTATION ***.

That premise is SUPERSEDED by a newer, explicit, always-restart decision: DECISIONS.md 2026-08-02
"Availability over retention" plus CLAUDE.md invariant 6 ("recovery ALWAYS reaches a running
server"), reaffirmed and reconciled with invariant 1 in commit 888f6c6 (2026-08-07, "Resolve a
self-contradiction in DECISIONS.md about id reuse and refuse-to-start"). The refuse-to-start
mechanism was NOT implemented. Reasons: (1) a refusal would directly contradict the newer,
explicit always-restart decision -- invariant 6 is not narrowed by this task, any more than
invariant 1 is narrowed by e120153b; (2) it is unnecessary, because a durable index floor
(internal/wal/indexfloor.go, <data-dir>/wal-index-floor, on-disk format version 4, reserved
2026-08-07 by feature-runner) makes the high-water mark KNOWABLE even after a whole-log quarantine
-- the floor is persisted OUTSIDE the log, written ahead of the index it authorises, so recovery
never has to choose between "refuse" and "reissue"; it just reads the floor. The SHIPPED fix
therefore does neither horn of the false dilemma the original framing posed: the bus ALWAYS boots
(invariant 6 holds), and it resumes strictly ABOVE every index/sequence ever minted, proven by the
floor file rather than by anything that survived in the (possibly quarantined) log itself
(invariant 1 holds, unnarrowed).

The task's recorded proof_cmd named a test, TestRecover_WholeLogQuarantine_RefusesStartOnUnprovenSequenceFloor,
that was deliberately NEVER WRITTEN, precisely because its name enshrines the superseded
refuse-to-start policy; writing it would have committed the test suite to asserting a mechanism
the project explicitly rejected. The crash-injection coverage that actually proves the shipped
behaviour lives instead in internal/wal/indexfloor_crash_test.go
(TestWALIndexFloorCrashNeverReissuesAnIndex and TestWALIndexFloorCrashedRunThatJumpedIsRemembered),
which kill -9 a real writer mid-run, quarantine the resulting log, and assert the next index is
strictly greater than every index the killed process was handed -- see test-engineer's report on
sibling task e120153b for the full run-down.

This task is REOPENED (un-superseded) to track that shipped fix through review/security, rather
than staying folded into 6ebe51be-2486-4ab9-a25d-675b627675f6, because the actual implementation and
test work for the whole-log-quarantine defect described above was done under this task (and its
sibling e120153b) by the currently in-flight feature-runner pass, not under 6ebe51be. 6ebe51be
remains cross-referenced as largely subsumed by this same mechanism, with one residual gap noted on
it (data directories that predate the floor file, quarantined on their first start under the new
binary) -- see the response note posted there today.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-12](../DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (done)
- [ID-2-WIRING](../../ID/ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (superseded)
- [ID-2-WIRING-OBSERVER](../../ID/ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [ID-2-WIRING-SCHEMA](../../ID/ID-2-WIRING-SCHEMA--80b54ee4/task.md) — ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… (done)
- [ID-2-WIRING-SEAL](../../ID/ID-2-WIRING-SEAL--8c9b6489/task.md) — ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half impleme… (done)
- [MSG-FU-SEQHIGHWATER](../MSG-FU-SEQHIGHWATER--6ebe51be/task.md) — MSG-FU-SEQHIGHWATER: persist the message-sequence high-water mark so deep log damage cann… (done)
- [e120153b-9d8a-4b6a-bd4e-89431954496b](../Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) — Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a na… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [18eac796-d1fd-4619-94cb-1164bf989634](../seq-floor-guard-predicate-keys-on-discard-not-on-account--18eac796/task.md) — seq-floor guard predicate keys on discard, not on accounting -- boundary-exact truncation… (todo)
- [68c1788f-6043-4c2d-b409-887f71507d69](../Invert-stale-internal-hub-hub.go-quarantine-reissue-perm--68c1788f/task.md) — Invert stale internal/hub/hub.go quarantine reissue-permitted assertions once MSG-FU-SEQH… (todo)
- [DUR-11-FU-CONTRACTS](../../DOCS/DUR-11-FU-CONTRACTS--5b178dde/task.md) — DUR-11-FU-CONTRACTS: CONTRACTS.md still documents the reverted refuse-to-start WAL policy… (todo)
- [MSG-FU-SEQHIGHWATER](../MSG-FU-SEQHIGHWATER--6ebe51be/task.md) — MSG-FU-SEQHIGHWATER: persist the message-sequence high-water mark so deep log damage cann… (done)
- [a695f85f-0c69-42a8-a653-deed4960a610](../../DOCS/PROTOCOL.md-8-cites-Spec-Server-task-id-INVITE-PEERGUARD--a695f85f/task.md) — PROTOCOL.md §8 cites Spec Server task id INVITE-PEERGUARD (f5d91dbe) as if it were a comm… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
