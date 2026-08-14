# SIGN-1-FU-OUTOFORDER-POISON: Reserve-then-send lets mints be spent out of order, which permanently POISONS the bus

| Field | Value |
| --- | --- |
| Public id | `bbd81523-6993-4672-a4a0-bbc9d729304f` |
| Key | SIGN-1-FU-OUTOFORDER-POISON |
| Epic | [SIGN](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | store |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T15:11:45.942477+00:00 |
| Updated | 2026-08-14T17:17:13.392132+00:00 |
| Completed | 2026-08-14T17:17:13.392116+00:00 |

## Proof command

```sh
go test -race -run 'TestOutOfOrderMintSpendDoesNotPoison' ./internal/store ./internal/hub
```

## Description

Filed 2026-08-14 from the RELAY-24-BLOCKER-HUBINGEST reviewer + security gates. P0, BLOCKS RELAY-24 AND RELAY-25.

THE DEFECT: store.Append requires strictly increasing sequences, but SIGN-1's reserve-then-send design lets reservations be spent in ANY order (MintTTL is an hour). Reproduced LIVE, twice, in a clean overlay of HEAD:
  (i) two purely LOCAL agents, NO relay involved -- alice mints 1, bob mints 2, bob sends, alice sends -> ErrPoisoned; the hub then refuses ALL further writes until restart.
  (ii) ONE local agent holding an outstanding mint while ANY relayed message arrives -> identical poison.
Error text: `store: message sequence is not strictly increasing: appending sequence 1 behind head 2`.

WHY IT IS WORSE THAN AN IN-MEMORY STALL: the record commits to disk BEFORE the serving copy rejects it, so recovery then discards it loudly on every restart, for ever (the discard is correct per invariant 6 -- the orphaned record is the problem).

SECURITY SEVERITY: HIGH -- any enrolled agent can stop the bus at will with two mints and two sends. The reviewer's assessment: "I'd put a near-certain probability on fed-smoke poisoning a bus on first run."

SECURITY'S BINDING RULING, RECORDED HERE SO IT IS NOT LOST: hub.IngestRelayed MUST NOT be wired into a served relay.NewAcceptor until this is fixed. Today that is held ONLY by the accident that RELAY-17 ships no CrossBusTrust implementation (so every relayed message is ErrUnpeeredBus by construction). This task records the dependency rather than relying on that accident.

BOUNDARY: the root fix is in internal/store (Append's ordering rule) and/or the hub's apply step. It was explicitly OUT OF BOUNDARY for the ingest task, which is why it is filed separately rather than patched there.

PROOF NOTE: the stored proof_cmd names a regression test this task MUST WRITE (both shapes: two-local-agent out-of-order spend, and one-local-mint-plus-relayed-ingest). Run it through scripts/proof-check.sh and require verdict=PASS -- a VACUOUS verdict means the test was never written, which is exactly the failure this task's parent was caught on.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-24](../../RELAY/RELAY-24--e303c624/task.md)
- **blocks** [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-17](../../RELAY/RELAY-17--817649ce/task.md) — RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope (done)
- [RELAY-24](../../RELAY/RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-24-BLOCKER-HUBINGEST](../../RELAY/RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) — RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sen… (done)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [SIGN-1](../SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [4c97d561-e81f-40a6-a1fe-3c9976d790f1](../../DOCS/INVARIANTS.md-invariant-1-s-entry-has-no-pointer-to-the--4c97d561/task.md) — INVARIANTS.md invariant 1's entry has no pointer to the 2026-08-14 SIGN-1-FU-OUTOFORDER-P… (todo)
- [CONTEXT-KEY-IDENTITY](../../CONTEXT/CONTEXT-KEY-IDENTITY--73dec684/task.md) — CONTEXT-KEY-IDENTITY: Standardize task identity (public_id vs key) before SPEC/&lt;epic&gt;/&lt;ta… (todo)
- [RELAY-24-BLOCKER-HUBINGEST](../../RELAY/RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) — RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sen… (done)
- [SIGN-1-FU-REORDER-WATERMARK](../SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)
- [SIGN-1-FU-STORE-LOGGER](../SIGN-1-FU-STORE-LOGGER--50081b3c/task.md) — SIGN-1-FU-STORE-LOGGER: pass the hub's configured logger into store.New so the invariant-… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
