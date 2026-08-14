# WAL foundation: authenticated multi-applier checkpoints over shared bus.wal

| Field | Value |
| --- | --- |
| Public id | `a1cbef29-400a-4a1e-9638-cc14d38a7ebf` |
| Key | _(null in the export)_ |
| Epic | [UNASSIGNED](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | wal |
| Section | in_progress |
| Tags | — |
| Created | 2026-08-09T00:18:45.788567+00:00 |
| Updated | 2026-08-09T13:34:17.450000+00:00 |
| Completed | 2026-08-09T13:34:17.449981+00:00 |

## Proof command

```sh
go test -race -count=1 -run '^TestCheckpointV7SignalCrashAndRecoveryObservability$' ./internal/wal
```

## Status note

User accepted residual crash-path evidence deferral to non-blocking P1 follow-up 8fb219ca-1236-4058-9020-afd52a7e93f3. V7 production/test/security evidence is accepted; integration-ready pending docs/integrator/commit. This follow-up does not block 617ffe5a or RELAY-19.

## Description

Prerequisite for 617ffe5a and therefore RELAY-19. The shared bus.wal remains the one authenticated durable truth; do not replace it with a relay-only store.

Deliver the WAL-owned, authenticated multi-applier checkpoint/segment-generation substrate. At one committed shared-log high-water H, all registered participants snapshot the same state; WAL owns keying, versioning and generation publication. Recovery restores one wholly verified generation and replays only its bounded tail in global commit order, preserving all committed app kinds and never reusing WAL indices.

Blocking cutover requirements (2026-08-09 gate remediation):
1. Writer/index-floor ownership: a cutover must never seal the shared floor while the new tail is live. Transfer/retire the old writer without a clean seal, or establish and durably prove the new tail’s active ownership before any acknowledgement. Crash/fallback/tail-damage recovery must not reissue any index.
2. Bind and constrain the tail: the manifest must cryptographically bind a unique tail generation/header identity and recovery must reject substitution. Every tail commit replayed after a snapshot must be strictly greater than H and globally monotonic; records at/below H are a rejected generation, not re-applied.
3. CURRENT is a recoverable, rollback-aware publication pointer: absence/corruption/torn CURRENT when generations exist must never silently choose legacy bus.wal or brick startup. Loudly quarantine/reject the bad pointer/material and select only a complete authenticated fallback generation; do not mix snapshots/tails. Legacy generation zero is permitted only before any generation has ever been durably published. Diagnostics must report accepted-history loss if invariant-6 discard/fallback makes it unavoidable.
4. Publication ambiguity: after CURRENT rename, a parent-dir fsync failure is ambiguous; the process must poison/stop acknowledgement and recovery must resolve old-or-new complete state safely. No later write may continue on an old tail after a potentially published new CURRENT.
5. Filesystem hardening: quarantine and loudly log orphan temp/unpublished/future generation directories; retry must not collide with an orphan. Reject symlinks/non-regular files, traversal, malformed/oversized names and generation overflow before opening or allocating.
6. Crash matrix is mandatory, using deterministic fault injection/child-process recovery at: participant snapshot write/fsync, tail header/write/fsync, manifest write/fsync, generation-dir fsync/rename, CURRENT temp write/fsync/rename/parent-dir fsync, writer handoff and old-writer retirement. For each point prove recovery is exactly complete-old+tail or complete-new+tail; assert no mixed generation, no silent fallback to legacy, no acknowledged terminal loss/resurrection, bounded tail replay, and non-reused indices. Include bad manifest/snapshot/path/length/version, tail substitution, selected-tail repair, missing/torn CURRENT, orphan quarantine/retry and global cross-kind ordering.

Infrastructure scope: internal/wal and its crash/integration tests plus CONTRACTS-ONDISK.md/DECISIONS.md. It does not implement Outbox policy or RELAY-19 wiring. 617ffe5a supplies the outbox participant after this accepted API lands. Read invariants 1,2,4,5,6,10 before code. ondisk-format-version=7 is reserved for this task.

V5 escalation (2026-08-09): prior whole-generation rejection is still required, but the current tail MAC is not a unique-tail binding: generic WAL headers repeat. Bind a per-generation tail identity/provenance (fresh nonce committed into the authenticated manifest and authenticated tail header or equivalent) so substituting a later tail whose commits are all > target H rejects/quarantines the target generation before Restore/Apply; prove both older-to-newer and newer-to-older substitutions. CURRENT must not be a rollback authority: valid plaintext rollback to an older generation may not hide a newer complete authenticated generation or discard its acknowledged tail. Select verified complete candidates newest-first independently of CURRENT (or provide equally non-rollbackable authenticated authority), logging/quarantining pointer inconsistency; never regress to legacy after a published generation.

Before reading any candidate tail, Lstat and reject symlink/non-regular/FIFO/device entries, then open with no-follow and fstat/revalidate to close TOCTOU. Quarantine/log orphan temp and final generation directories AND regular files occupying generation names; retry must make progress. Complete deterministic fault/child-process cutover matrix remains mandatory, with global cross-kind order, selected-tail repair, malformed snapshot/path/length/version, legacy migration, fallback NextIndex non-reuse and orphan handling. The sole required completion proof is the exact aggregate TestCheckpointV5SecurityAndCrashAcceptance, which must contain/assert all v5 cases; it is intentionally absent until implementation so no partial-alternation proof can green-light completion.

V6 correctness evidence escalation (2026-08-09): no waiver. Invariants 4/5 and repository durability protocol require deterministic child-process crash injection, not post-operation error hooks followed by in-process Close. At every cutover boundary (snapshot write/fsync, tail header/write/fsync, manifest write/fsync, generation directory fsync/rename, CURRENT temp write/fsync/rename/parent fsync, writer handoff and old-writer retirement), kill the child at/within the operation and reopen the resulting on-disk bytes without a clean close. Prove only complete-old+tail or complete-new+tail recovery; torn files/dirty directory state/active floor must not reissue an index, lose an acknowledged record or mix generation state.

The exact V6 aggregate must additionally cover: malformed snapshot, path, declared length and version material; selected-tail torn-frame RepairLog availability semantics; an explicit bounded-tail replay measurement/assertion; a real pre-checkpoint legacy bus.wal including v1 migration path; crash/fallback NextIndex nonreuse; global cross-kind ordering; and quarantine with loud reason of every rejected generation material class that can safely be quarantined (not merely semantic boundary rejection), including authenticated/unauthenticated malformed manifest/snapshot candidates. Invalid candidates must not be silently retried forever. The sole completion proof is TestCheckpointV6ProcessCrashRecoveryAcceptance; it is intentionally absent until the complete V6 matrix exists.

V7 recovery-observability and crash-evidence escalation (2026-08-09): selected candidate verification MUST NOT silently mutate a tail. verifyGeneration must use a read-only validator; it must not call repairLog with nil logger or otherwise truncate/rewrite before normal selected-tail recovery. If the selected tail is repairable, the one normal recovery path must repair it exactly once, surface the repair in Recovered.Repaired/Discarded and emit a loud specific discard log. If candidate corruption rejects/falls back instead, it must be quarantined and loudly reported; either path must preserve the invariant-6 evidence.

Replace normal os.Exit child termination with genuine syscall.SIGKILL delivered to the subprocess and assert exec.ExitError plus syscall.WaitStatus.Signaled and Signal()==SIGKILL. Hooks must be capable of stopping inside partial write/fsync/rename operations; pair a real SIGKILL process harness with deterministic torn-byte/dirty-directory construction where platform semantics cannot force a physical tear. At every boundary assert the exact chosen old or new generation identity, no snapshot/tail mixing, recovery diagnostics, accepted-history preservation and index-floor nonreuse.

The sole V7 aggregate must call/assert the core V5 cases (both TailID substitution directions, stale CURRENT rollback, pre-open FIFO/symlink/no-follow, orphan masquerader quarantine/retry), all malformed candidate classes with a non-nil logger and exact loud reason, selected-tail repair observable through Recovered plus logs, explicit bounded replay, real v1 legacy upgrade with restored participant state and a post-checkpoint reopen, and all crash/fallback selection/NextIndex/global-order assertions. The proof TestCheckpointV7SignalCrashAndRecoveryObservability is intentionally absent until all conditions exist.

User-directed acceptance revision (2026-08-09): accept the shipped v7 checkpoint production with its non-vacuous v7 aggregate, test-engineer PASS and security PASS. The remaining reviewer crash-path evidence hardening is explicitly deferred to non-blocking follow-up 8fb219ca-1236-4058-9020-afd52a7e93f3 and does not block this task, 617ffe5a or RELAY-19. This revision does not weaken any implemented production security/recovery behavior; it changes only completion evidence priority under user authority. The task is integration-ready pending ordinary documentation/integrator verification and commit.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [617ffe5a-db42-4aeb-89bb-d9b0889f6c19](../../RELAY/Bound-retained-outbox-tombstone-resources-without-reopen--617ffe5a/task.md)
- **blocks** [ACK-2](../../ACK/ACK-2--9564f953/task.md)
- **follow-up** [8fb219ca-1236-4058-9020-afd52a7e93f3](../WAL-checkpoint-follow-up-exhaustive-in-operation-crash-p--8fb219ca/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [617ffe5a-db42-4aeb-89bb-d9b0889f6c19](../../RELAY/Bound-retained-outbox-tombstone-resources-without-reopen--617ffe5a/task.md) — Bound retained outbox tombstone resources without reopening replay resurrection (done)
- [8fb219ca-1236-4058-9020-afd52a7e93f3](../WAL-checkpoint-follow-up-exhaustive-in-operation-crash-p--8fb219ca/task.md) — WAL checkpoint follow-up: exhaustive in-operation crash-path evidence (todo)
- [RELAY-19](../../RELAY/RELAY-19--24e0bd11/task.md) — RELAY-19: Forwarder writes and settles outbox records (part 2 of 2) (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [8fb219ca-1236-4058-9020-afd52a7e93f3](../WAL-checkpoint-follow-up-exhaustive-in-operation-crash-p--8fb219ca/task.md) — WAL checkpoint follow-up: exhaustive in-operation crash-path evidence (todo)
- [ACK-2](../../ACK/ACK-2--9564f953/task.md) — ACK-2: Durable local send acceptance and ACK/NACK lifecycle record (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
