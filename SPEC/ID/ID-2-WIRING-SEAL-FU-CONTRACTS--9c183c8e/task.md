# ID-2-WIRING-SEAL-FU-CONTRACTS: land the Sequence seal contract rows that the file-boundary deferred

| Field | Value |
| --- | --- |
| Public id | `9c183c8e-ca4f-4b5a-9d74-30c9c2d6f812` |
| Key | ID-2-WIRING-SEAL-FU-CONTRACTS |
| Epic | [ID](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T20:55:44.789011+00:00 |
| Updated | 2026-08-02T20:55:44.789011+00:00 |
| Completed | — |

## Proof command

```sh
grep -q 'ErrFloorUnproven' CONTRACTS*.md
```

## Description

Deliberately incurred, tracked debt. `ID-2-WIRING-SEAL` (public_id 8c9b6489-abb1-444e-9eeb-3ff87646f632) shipped `Seal()`, `ErrFloorUnproven` and `ErrFloorSealed` on `internal/ids.Sequence`, and its own description said "Update CONTRACTS.md" -- but CONTRACTS.md was being split into per-plane files by a concurrent agent in the same loop (`CONTRACTS-SPLIT`, 360a2679-b5dc-4b17-863f-fb4462764e6d) and admits ONE writer per loop, so the feature-runner was explicitly barred from touching it. No contract row was written. This task lands them.

Note there is currently NO section anywhere in CONTRACTS*.md documenting `internal/ids.Sequence` at all (grep confirms zero matches for `Sequence`, `RaiseFloor`, `NewSequence`). Suggested home: `CONTRACTS-ONDISK.md`, since the sequence number is the durable half of a message id and its floor is derived from the WAL -- but the owner of this task decides, and may reasonably argue for a new internal-package section instead.

Rows to add:

### Message-sequence allocator (`internal/ids.Sequence`) -- added 2026-08-02 (ID-2-WIRING-SEAL)

The allocator has two states and moves between them once, in one direction: UNSEALED -> SEALED.

| Symbol | Contract |
| --- | --- |
| `ids.NewSequence() *Sequence` | Allocator for a FRESH bus: floor 0, first `Next` returns 1. Born **UNSEALED**. |
| `ids.Resume(highestOnDisk uint64) *Sequence` | Allocator resuming strictly above `highestOnDisk` -- the highest sequence number EVER WRITTEN TO DISK: every prepare, committed, aborted and dangling alike; NOT a record count, NOT the highest COMMITTED sequence. Born **UNSEALED**. |
| `(*Sequence).RaiseFloor(atLeast uint64) error` | Legal **only while UNSEALED**. Raises the floor to `atLeast`; never lowers it; may be called repeatedly from several sources in any order (it takes a maximum). Returns an error wrapping `ErrFloorSealed` after `Seal()` and changes nothing. |
| `(*Sequence).Seal() error` | Ends floor assembly. One-way, exactly once: `nil` the first time; wraps `ErrFloorSealed` on every subsequent call and changes nothing. Concurrency-safe. `Seal()` is the caller ASSERTING the floor is >= every sequence ever written; the allocator holds no durable state and cannot verify that assertion. |
| `(*Sequence).Next() (uint64, error)` | Returns `(0, ErrFloorUnproven)` while UNSEALED and allocates NOTHING (floor and last untouched). After `Seal()` issues floor+1, strictly monotonic, concurrency-safe; `(0, ErrSequenceExhausted)` at `math.MaxUint64`, never a wrap. The unsealed check runs BEFORE the exhaustion check. |
| `(*Sequence).Last() uint64` | Highest number ISSUED by this allocator, 0 if none. Unchanged by ID-2-WIRING-SEAL. |
| `ids.ErrFloorUnproven` | Sentinel returned by `Next` on an unsealed allocator. The fail-closed half of invariant 1: an allocator with no proven floor refuses to mint rather than minting from a guess. |
| `ids.ErrFloorSealed` | Sentinel returned by `RaiseFloor` after `Seal`, and by a second `Seal`. |
| `ids.ErrFloorBelowIssued` | Sentinel. **Unreachable on `Sequence`** under the seal gate (`last != 0` requires a successful `Next`, which requires `sealed`, and the sealed check returns first); the branch is kept as defence-in-depth. Still LIVE per-name on `NameSuffixes.RaiseFloor`, which has no seal. |

**Startup contract.** A bus MUST derive the floor, `RaiseFloor` it from every source it has, and then call `Seal()` exactly ONCE before serving. Until it does, every `Next` is refused. Both constructors are born unsealed on purpose: "floor 0 because the log was empty" and "floor 0 because the recovery scan failed" are the same value, so neither is allowed to issue until somebody says out loud that the floor is proven.

**What this does NOT defend.** `Seal()` proves a CLAIM was made, not that the claim is TRUE. `Sequence` holds no durable state, so a floor computed off a record count or off committed history seals just as cleanly as a correct one. Proving the floor remains the caller's obligation; deriving it is ID-2-WIRING (838677e6), blocked on ID-2-WIRING-SCHEMA.

proof_cmd is RED today: verified zero matches for `ErrFloorUnproven` anywhere in the repo's CONTRACTS files. The glob `CONTRACTS*.md` is deliberate so the proof survives the CONTRACTS split into per-plane files.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTRACTS-SPLIT](../../DOCS/CONTRACTS-SPLIT--360a2679/task.md) — CONTRACTS-SPLIT: split CONTRACTS.md into per-plane files (pure move) + retarget every pro… (done)
- [ID-2-WIRING](../ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (done)
- [ID-2-WIRING-SCHEMA](../ID-2-WIRING-SCHEMA--80b54ee4/task.md) — ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… (done)
- [ID-2-WIRING-SEAL](../ID-2-WIRING-SEAL--8c9b6489/task.md) — ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half impleme… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ID-2-WIRING-SEAL-FU-NAMESUFFIXES](../ID-2-WIRING-SEAL-FU-NAMESUFFIXES--1c207a62/task.md) — ID-2-WIRING-SEAL-FU-NAMESUFFIXES: NameSuffixes has the identical inert-floor-guard defect… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
