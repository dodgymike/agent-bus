# ID-2-WIRING-SCHEMA: DECIDE and record where the message sequence high-water mark lives on disk (blocks the floor derivation)

| Field | Value |
| --- | --- |
| Public id | `80b54ee4-55d5-44b8-a479-c0a13343d15a` |
| Key | ID-2-WIRING-SCHEMA |
| Epic | [ID](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T17:54:49.588447+00:00 |
| Updated | 2026-08-07T12:05:01.996243+00:00 |
| Completed | 2026-08-07T12:05:01.996224+00:00 |

## Proof command

```sh
grep -q '^## .* The message sequence high-water mark lives in the WAL message body, read via a replay-time PREPARE observer (ID-2-WIRING-SCHEMA)$' DECISIONS.md
```

## Status note

dispatched by triage-20260802-r4 to a feature-runner

## Description

SPLIT OUT OF ID-2-WIRING (838677e6). This is a DECISION task -- docs only, no code -- and it is the thing actually blocking the floor derivation. See ID2_WIRING_DEEPDIVE.md sec 3.5, 4.2 and 4.4 (committed 2f89fc1) for the ranked options and the disproof test.

THE PROBLEM. ids.Resume(floor) needs the highest sequence EVER WRITTEN TO DISK -- committed, aborted AND dangling. Today the sequence lives inside the caller-written PREPARE body (wal.Entry.Body), the WAL deliberately does not interpret Body, wal.Replay hands its callback COMMITTED entries only, and Recovered exposes no message-sequence high-water mark (Recovered.NextIndex is the WAL RECORD index, a different counter). So there is no way to derive the floor without first deciding WHERE the number lives.

THE DECISION (record it in DECISIONS.md, dated, appended -- the file is contended, add a new section rather than editing lines):
  Option A' -- the WAL offers every PREPARE to an observer during the EXISTING replay pass; the sequence stays in the caller's body and the ids/msg layer decodes it. No on-disk format change; also removes the third startup scan before it is ever added (see task 2a961fcc).
  Option B  -- promote the sequence to a WAL-level field (Entry.Seq / preparePayload.Seq, Recovered.HighestSequence). This IS an on-disk format change and therefore REQUIRES a reservation from the `ondisk-format-version` namespace (NEVER pick the number) plus a downgrade note.
Record the chosen option, the rejected ones, and the sec-4.4 disproof test.

ORDERING WARNING: the CRC32C -> HMAC-SHA256 MAC task is ALSO an on-disk format change and has ALREADY reserved ondisk-format-version=2. If this task chooses Option B it must reserve its OWN value; format changes are ORDERED and two agents must never share one version number.

BLOCKS: ID-2-WIRING (838677e6) and ID-2-WIRING-OBSERVER.

PROOF. `grep -q 'message sequence high-water mark' DECISIONS.md` -- verdict=FAIL class=file-assertion exit=1 TODAY, which is correct and non-vacuous: it fails precisely because the decision is unrecorded, and flips to PASS when it is written. The chosen wording must therefore contain that exact phrase.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [ID-2-WIRING](../ID-2-WIRING--838677e6/task.md)
- **blocks** [ID-2-WIRING-OBSERVER](../ID-2-WIRING-OBSERVER--c31f6999/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2a961fcc-426d-4c98-bc63-eb236367fd85](../../DUR/Startup-scans-the-WAL-twice-soon-three-times-bound-the-c--2a961fcc/task.md) — Startup scans the WAL twice (soon three times) -- bound the cost (todo)
- [ID-2-WIRING](../ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (done)
- [ID-2-WIRING-OBSERVER](../ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-12](../../DUR/DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (in_progress)
- [ID-2-WIRING](../ID-2-WIRING--838677e6/task.md) — ID-2-WIRING: Derive the sequence resume floor from ALL prepares, never from committed his… (done)
- [ID-2-WIRING-OBSERVER](../ID-2-WIRING-OBSERVER--c31f6999/task.md) — ID-2-WIRING-OBSERVER: wal offers EVERY prepare (committed, aborted AND dangling) to an ob… (todo)
- [ID-2-WIRING-SEAL](../ID-2-WIRING-SEAL--8c9b6489/task.md) — ID-2-WIRING-SEAL: Sequence refuses to issue from an UNSEALED floor (the only half impleme… (done)
- [ID-2-WIRING-SEAL-FU-CONTRACTS](../ID-2-WIRING-SEAL-FU-CONTRACTS--9c183c8e/task.md) — ID-2-WIRING-SEAL-FU-CONTRACTS: land the Sequence seal contract rows that the file-boundar… (todo)
- [db350e39-3dde-4166-b241-b21fa4635359](../../DUR/Whole-log-quarantine-reissued-EVERY-sequence-number-ever--db350e39/task.md) — Whole-log quarantine reissued EVERY sequence number ever minted -- fixed by a durable ind… (done)
- [e120153b-9d8a-4b6a-bd4e-89431954496b](../../DUR/Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) — Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a na… (in_progress)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
