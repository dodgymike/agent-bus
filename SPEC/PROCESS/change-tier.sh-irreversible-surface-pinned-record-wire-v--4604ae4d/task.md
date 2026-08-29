# change-tier.sh: irreversible surface -- pinned record/wire-version constants and go.mod dependency changes to T4

| Field | Value |
| --- | --- |
| Public id | `4604ae4d-a8b3-4272-9226-67557de66de3` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T08:40:03.783587+00:00 |
| Updated | 2026-08-22T09:13:37.094564+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'bash scripts/change-tier_test.sh'
```

## Description

Two parts.

(a) Pinned constant lines. Reconnaissance produced the exact inventory; watch these file+identifier pairs and floor T4 on any change: internal/wal/format.go (FormatVersion, formatVersionV1, magicWAL, magicAudit), internal/wal/indexfloor.go (indexFloorMagic, indexFloorFileVersion), internal/store/message.go (RecordKind, RecordVersion), internal/auth/record.go, internal/auth/inviteenrol.go, internal/auth/operatorrecord.go, internal/invite/record.go, internal/ack/record.go, internal/hub/mint.go (SeqFloorRecordKind), internal/hub/seqfloorfile.go, internal/hub/cursor.go, internal/ids/suffixstore.go, internal/relay/peerstore.go, internal/relay/outbox.go, internal/relay/ackframe.go (AckWireVersion), internal/signing/canonical.go (FormatVersion), internal/attest/canonical.go (AttestationFormatVersion), client/canonical.go (MessageSigningFormatVersion), client/ack.go (AckSigningFormatVersion, ackWireProtocolVersion), client/cursorstore.go (cursorFormatVersion). T4 also mandates a Spec Server reservation for any new number.

(b) go.mod / go.sum / vendor changes fire NO signal in the design as proposed. Adding a third-party dependency is invariant 8 (requires a DECISIONS.md justification) and is a supply-chain security event. Floor it at T4 and mandate the DECISIONS.md entry. Invariant 8 is absent from the signal list entirely; this closes it.

Note for whoever implements: internal/signing.FormatVersion and client/canonical.go's MessageSigningFormatVersion are a cross-tree pair that must move together (likewise internal/ack/vocabulary.go <-> client/ack.go <-> internal/signing's frozen AckClass* alphabet). Flagging EITHER side raises the tier, which is all this signal owes; the one-side-only drift hazard is already covered by internal/signing/ackvocab_external_test.go and is NOT in scope here.

Proof detail: fixtures RED first.

BLOCKED BY T-03.

---

## INHERITS F1 AND F2 FROM T-03 (2026-08-22, security gate)

**This signal classifies by PATH, and therefore inherits findings F1 and F2 recorded on T-03
(b2567ffd-190d-4aff-8cc2-f6a2eb2d613e).** Both are measured, not theorised:

- **F1 (renames):** `git status --porcelain` prints a rename as ONE line, `R  old -> new`, so a
  check anchored at `^` never sees the target and a check testing the line end never sees the
  source. **This signal must consume the `git status --porcelain --no-renames` file set**, in which
  the rename is split into `D old` + `A new` and both halves are classified.
- **F2 (fails open):** `git status --porcelain -- <pathspec matching nothing>` prints nothing and
  exits **0**. **This signal must NOT treat an EMPTY file set as low-risk** -- "measured T0" and
  "could not measure" are different outcomes, and the second is an error exit, not a result.

**This signal needs a RENAME FIXTURE in its own right** -- not merely coverage in T-03's tests --
shown RED before the signal is implemented, with the RED output quoted in the task's `kind=report`.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md)
- **blocks** [461ebf5b-5d0e-4c3d-b95e-a1437e15b31f](../Acceptance-gate-all-four-low-measuring-cases-sort-correc--461ebf5b/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [461ebf5b-5d0e-4c3d-b95e-a1437e15b31f](../Acceptance-gate-all-four-low-measuring-cases-sort-correc--461ebf5b/task.md) — Acceptance gate: all four low-measuring cases sort correctly (todo)
- [b2567ffd-190d-4aff-8cc2-f6a2eb2d613e](../scripts-change-tier.sh-diff-basis-contract-output-format--b2567ffd/task.md) — scripts/change-tier.sh: diff-basis contract, output format, exit codes, and signal 1 (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
