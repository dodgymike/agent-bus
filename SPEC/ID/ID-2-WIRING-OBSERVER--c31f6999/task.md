# ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an observer during the existing replay pass

| Field | Value |
| --- | --- |
| Public id | `c31f6999-da4e-400d-ab55-178b82e2a42e` |
| Key | ID-2-WIRING-OBSERVER |
| Epic | [ID](../epic.md) |
| Status | todo |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T17:54:49.875258+00:00 |
| Updated | 2026-08-07T20:40:21.644041+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestWALReplayObservesEveryPrepare ./internal/wal && go test -race ./internal/wal
```

## Description

SPLIT OUT OF ID-2-WIRING (838677e6). See ID2_WIRING_DEEPDIVE.md sec 5/T3 (committed 2f89fc1).

BLOCKED ON ID-2-WIRING-SCHEMA choosing Option A'. If SCHEMA chooses Option B instead, this task is SUPERSEDED and replaced by ID-2-WIRING-HEADER (add Entry.Seq + preparePayload.Seq, expose Recovered.HighestSequence, RESERVE a fresh ondisk-format-version value -- never pick it -- bump FormatVersion, fix replay_test.go:1109's unknown-field fixture, ship a downgrade note; proof `go test -race -run 'TestWALRecoveredHighestSequence|TestWALFormatVersionRefusal' ./internal/wal`).

REQUIRED (Option A' shape). Add wal.ReplayWithPrepares(path, fn, onPrepare); Replay delegates with a nil observer so no existing caller changes. onPrepare fires for EVERY prepare in file order -- committed, aborted and dangling -- BEFORE resolution. The wal package still does not interpret Body; it hands the bytes up. Update CONTRACTS.md and PROTOCOL.md.

THE ASSERTION THAT MATTERS: the observer must see the DANGLING prepare's entry. That is the whole point -- assert a floor of 100 from a log whose only seq-100 record never committed. A test that only observes committed prepares proves nothing.

PROOF. `go test -race -run TestWALReplayObservesEveryPrepare ./internal/wal && go test -race ./internal/wal`. VACUOUS TODAY BY CONSTRUCTION (the test does not exist). The deep-diver's scratch equivalent (TestFloorFromPrepareObserverInOnePass) is proven PASS, so the command is executable once written. DO NOT COMPLETE ON A VACUOUS VERDICT.

---

## RE-SCOPED 2026-08-07 (spec-keeper), following the correction of ID-2-WIRING's (838677e6) supersession reason

STILL OPEN, STILL P0 -- but the JUSTIFICATION above is now partly obsolete and must not be read as-is. The steady-state need this task originally served has been closed a DIFFERENT way; a real, narrower need remains.

WHAT CHANGED. ID-2-WIRING-SCHEMA (80b54ee4, done) did choose Option A' in DECISIONS.md, so the "BLOCKED ON SCHEMA" gate above is cleared. But the consumer this task was built for -- ID-2-WIRING's T4, deriving `ids.Resume(floor)` in main.go by folding an observer over every WAL prepare body -- never landed, because ID-2-WIRING itself is now SUPERSEDED for a corrected reason: `internal/hub/seqfloorfile.go` (the `message-seq-floor` file, on-disk format version 5, landed under commit aad611c) persists the message-sequence floor OUTSIDE the log, written AHEAD of any sequence `/v1/mint` hands out. Since SIGN-2/SIGN-6, a sequence can be durably claimed in a batch of `hub.MintBatchSize=256` BEFORE any message record -- committed, aborted or dangling -- exists at all. There is therefore nothing in a message's WAL prepare body left for `ReplayWithPrepares`/`onPrepare` to derive the AUTHORITATIVE message-sequence floor FROM any more; scanning prepare bodies cannot see a number that was never written into one.

SO: is a prepare-body observer still needed? YES, but NOT for the reason this task was filed. Two OTHER open gaps still need exactly this capability, both already on record as migration-window residuals that explicitly name this task as their closure:

1. **MSG-FU-SEQHIGHWATER's residual** (6ebe51be, todo) -- a data directory that predates `message-seq-floor` has no floor file (`existedAtOpen() == false`) and must back-fill one on first start. Today that back-fill can only see COMMITTED history (hub.go's own source (3), `wal.Replay`'s ordinary committed-only callback), so a legacy directory's back-filled floor can still miss a sequence burned by a dangling, uncommitted mint/message record from before the upgrade. An observer closes that specific window.
2. **MSG-FU-SUFFIXFLOOR's residual, via AUTH-3** (d53e3b21, in_progress) -- `ids.DurableNameSuffixes` (internal/ids/suffixstore.go) has the identical shape and the identical gap for agent-id suffixes: DECISIONS.md's 2026-08-07 "MSG-FU-SUFFIXFLOOR" entry says explicitly, in so many words, that a legacy directory's back-fill "still cannot see a suffix burned by a dangling prepare, because the prepare-observer work named above [[this task]] is not implemented," and that the gap "closes for good ... once ID-2-WIRING-OBSERVER lands." AUTH-3 cross-references this task for exactly that reason.

CORRECTED SCOPE. Same code shape as originally specced (`wal.ReplayWithPrepares(path, fn, onPrepare)`, `Replay` delegates with `nil`, fires for every prepare -- committed, aborted, dangling -- in file order, before resolution; `wal` still does not interpret `Body`). The DIFFERENCE is what it is FOR: this is no longer the authoritative message-sequence derivation (that is `seqfloorfile.go`, unconditionally, on every start where the file exists) -- it is the ONE-TIME LEGACY-DIRECTORY BACK-FILL helper for BOTH `internal/hub`'s message-seq-floor back-fill and `internal/ids.DurableNameSuffixes`'s suffix-floor back-fill, invoked only on the `!existedAtOpen()` migration path each already documents. Keep the proof and assertion as specified below (a dangling prepare's entry must be observed) -- that requirement is unchanged; only the caller and the stakes are narrower than "P0, blocks every message ever sent" and are now "closes a bounded, already-logged, already-acknowledged migration-window gap for restarts of directories written before 2026-08-07."

NOT reopening ID-2-WIRING (838677e6) for this -- that task's own scope (steady-state `ids.Resume` wiring in `main.go`) is genuinely superseded and stays closed; this task's remaining justification lives entirely in the two residuals above.

Priority kept at P0 because AUTH-3 is P0/in_progress and blocked on an honest suffix-floor back-fill; whoever picks this up should coordinate with AUTH-3's owner rather than duplicate the WAL-side change.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [ID-2-WIRING-SCHEMA](../ID-2-WIRING-SCHEMA--80b54ee4/task.md)
- **blocks** [ID-2-WIRING](../ID-2-WIRING--838677e6/task.md)
- **relates to** [MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT](../MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT--6b0e561e/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (in_progress)
- [ID-2-WIRING](../ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (superseded)
- [ID-2-WIRING-SCHEMA](../ID-2-WIRING-SCHEMA--80b54ee4/task.md) — ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… (done)
- [MSG-FU-SEQHIGHWATER](../../DUR/MSG-FU-SEQHIGHWATER--6ebe51be/task.md) — MSG-FU-SEQHIGHWATER: persist the message-sequence high-water mark so deep log damage cann… (done)
- [MSG-FU-SUFFIXFLOOR](../MSG-FU-SUFFIXFLOOR--94159d93/task.md) — MSG-FU-SUFFIXFLOOR: resume per-name agent-id suffix counters from disk (agent ids are now… (in_progress)
- [SIGN-2](../../SIGN/SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (in_progress)
- [CONTRACTS-SPLIT](../../DOCS/CONTRACTS-SPLIT--360a2679/task.md) — CONTRACTS-SPLIT: split CONTRACTS.md into per-plane files (pure move) + retarget every pro… (done)
- [ID-2-WIRING](../ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (superseded)
- [ID-2-WIRING-SCHEMA](../ID-2-WIRING-SCHEMA--80b54ee4/task.md) — ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… (done)
- [MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT](../MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT--6b0e561e/task.md) — MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT: assert a freshly-minted agent-suffix is not already i… (todo)
- [a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd](../../TOOLING/Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) — Conjunction-masking vacuous-proof family: filtered-clause proof_cmds hidden by an unfilte… (todo)
- [db350e39-3dde-4166-b241-b21fa4635359](../../DUR/Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) — Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable ind… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
