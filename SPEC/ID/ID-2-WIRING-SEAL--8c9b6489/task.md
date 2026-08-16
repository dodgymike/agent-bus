# ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half implementable today)

| Field | Value |
| --- | --- |
| Public id | `8c9b6489-abb1-444e-9eeb-3ff87646f632` |
| Key | ID-2-WIRING-SEAL |
| Epic | [ID](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | ids |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T17:54:49.331870+00:00 |
| Updated | 2026-08-02T23:45:26.102393+00:00 |
| Completed | 2026-08-02T23:45:26.102375+00:00 |

## Proof command

```sh
go test -race -run TestSequenceRefusesToIssueFromAnUnsealedFloor ./internal/ids && go test -race ./internal/ids
```

## Status note

PLACEHOLDER commit_sha RECORDED, NOT a real sha: the completion commit above reads sha="uncommitted-at-completion-orchestrator-lands-wave-commit" -- a fabricated value the API accepted, not a git commit. Work itself is genuinely complete and green (proof-check verdict=PASS class=test exit=0 tests_run=15 top_level=1 for the named test, 203 tests / 41 top-level for the whole package; go build/vet/gofmt clean; reviewer PASS-WITH-NITS, security PASS-WITH-NOTES, both nit sets fixed in-task) -- do NOT reopen this task. DO NOT trust the commit_sha field until it is replaced with the real sha once the user commits internal/ids/sequence.go, internal/ids/sequence_test.go, internal/ids/messageid_test.go, internal/ids/doc.go. This is a bookkeeping-only placeholder-sha issue, flagged 2026-08-02 by triage.

## Description

SPLIT OUT OF ID-2-WIRING (838677e6) on the deep-diver's recommendation -- see ID2_WIRING_DEEPDIVE.md sec 4.1 and sec 5/T1, committed at 2f89fc1. This is the ONLY half of ID-2-WIRING that can start immediately: it touches internal/ids ONLY and depends on nothing.

THE DEFECT. internal/ids/sequence.go's RaiseFloor guard is INERT AT STARTUP. It only fires once something has been issued (last != 0), so in exactly the window where the floor is derived, every value -- including one far too low -- is accepted silently. Worse (deep-dive sec 3.4, verified first-hand there): `go vet` CANNOT be made to catch a bare `s.RaiseFloor(x)` that drops the error, so the mistake is invisible to the toolchain.

REQUIRED.
- Add Seal(), ErrFloorUnproven and ErrFloorSealed to internal/ids/sequence.go.
- Next() returns (0, ErrFloorUnproven) until Seal() has been called. RaiseFloor returns ErrFloorSealed after.
- BOTH constructors are born UNSEALED (New and Resume) -- a fresh bus must seal explicitly too, so 'floor 0 because the log was empty' and 'floor 0 because derivation failed' can never be confused.
- Update sequence.go's doc comment ('When it may be called') and the 5 existing tests.
- Update CONTRACTS.md.

NOT IN SCOPE: anything in cmd/agent-bus, anything in internal/wal, and the floor DERIVATION itself (that is ID-2-WIRING, which stays blocked on ID-2-WIRING-SCHEMA).

PROOF. `go test -race -run TestSequenceRefusesToIssueFromAnUnsealedFloor ./internal/ids && go test -race ./internal/ids`. VACUOUS TODAY BY CONSTRUCTION -- the named test does not exist yet, which is the point: it is the test this task must write. The deep-diver ran the equivalent test against a scratch prototype and recorded verdict=PASS class=test exit=0 tests_run=5 top_level=1, so the command is executable and non-vacuous the moment the test is written. DO NOT COMPLETE THIS TASK ON A VACUOUS VERDICT; scripts/proof-check.sh must report PASS with tests_run > 0.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ID-2-WIRING](../ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (superseded)
- [ID-2-WIRING-SCHEMA](../ID-2-WIRING-SCHEMA--80b54ee4/task.md) — ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [932fe938-0e42-42d8-802d-ff018cb6c955](../../PROCESS/Audit-stored-proof_cmds-for-the-subtest-skip-vacuous-sha--932fe938/task.md) — Audit stored proof_cmds for the subtest-skip vacuous shape (parent-PASS/hidden-child-SKIP… (todo)
- [CONTRACTS-SPLIT](../../DOCS/CONTRACTS-SPLIT--360a2679/task.md) — CONTRACTS-SPLIT: split CONTRACTS.md into per-plane files (pure move) + retarget every pro… (done)
- [ID-2-WIRING](../ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (superseded)
- [ID-2-WIRING-SEAL-FU-CONTRACTS](../ID-2-WIRING-SEAL-FU-CONTRACTS--9c183c8e/task.md) — ID-2-WIRING-SEAL-FU-CONTRACTS: land the Sequence seal contract rows that the file-boundar… (todo)
- [ID-2-WIRING-SEAL-FU-NAMESUFFIXES](../ID-2-WIRING-SEAL-FU-NAMESUFFIXES--1c207a62/task.md) — ID-2-WIRING-SEAL-FU-NAMESUFFIXES: NameSuffixes has the identical inert-floor-guard defect… (done)
- [a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd](../../TOOLING/Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md) — Conjunction-masking vacuous-proof family: filtered-clause proof_cmds hidden by an unfilte… (todo)
- [db350e39-3dde-4166-b241-b21fa4635359](../../DUR/Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) — Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable ind… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
