# Expose on wal.Recovered the highest index a record actually CONSUMED

| Field | Value |
| --- | --- |
| Public id | `9fd58deb-6fb8-4d4e-8bf1-6df01329c3b2` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T13:14:19.410362+00:00 |
| Updated | 2026-08-08T13:14:19.410362+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'go test -race -run TestWALRecoveredExposesHighestConsumedIndex ./internal/wal'
```

## Description

SHARED BLOCKER, reached from three directions independently: (1) be447589 (data-directory permissions + message-seq-floor guard) -- the shipped fix removed both durable predicate arms that tried to approximate this (NextIndex accounting, then MissingRecords) after each one PERMANENTLY BRICKED a healthy directory on an ordinary unclean shutdown; feature-runners closing note on that task says explicitly: closing the gap needs internal/wal to expose the highest CONSUMED index -- BLOCKER, outside boundary. (2) e120153b (WAL recovery reissuing a discarded tail record index) -- reviewer found the P0 symptom remains reachable by a non-quarantine route: floor.written is only raised at begin() and at a CLEAN seal(); a crashed run leaves ONLY reserved as evidence, and reserved is consulted only when the log ITSELF proves damage -- so a truncation that looks clean (no torn frame, LostUnidentified=false) can still reissue. (3) An independent measurement (reported by an agent in another repo, see notes on e120153b and be447589) swept 553 byte-exact truncation offsets against a purpose-built specimen (7 delivered messages, seqs 1,2,3 and 257-260, floor written=22 after a restart, 8900-byte WAL) and found a reissuing band (offset 1004-4439, derived floor 256) that is INDISTINGUISHABLE from a healthy directory one restart younger -- valid digest, nothing wrong with the file -- so no plausibility bound, human inspection, or MAC/integrity check can ever see it. Only knowing what the log itself proves was CONSUMED (survived + discarded + quarantined, as opposed to merely reserved-but-never-written) can close this.

REQUIRED: add a field to wal.Recovered (or an equivalent accessor) reporting the highest record index the replay/repair pass actually CONSUMED this run -- i.e. observed in the file, whether it survived, was discarded as damaged, or was quarantined -- distinct from NextIndex (which is floor-raised) and distinct from the durable floors reserved/written (which persist across runs and can go stale). wal already computes and LOGS this internally (log.go: indices_skipped / fileNext at the WARN line noted by reviewer), it is just not on the public Recovered struct.

This unblocks: a narrower be447589 guard predicate (refuse only when the floor is absent AND this-run-consumed exceeds what the log alone can prove, not on every unclean shutdown); a real fix for e120153bs non-quarantine reissue route; and any future low-floor plausibility check (the class of check the independent measurement showed a high-value bound cannot substitute for).

SCOPE: internal/wal only. Coordate with whichever agent has DUR-5 (append-only audit log) live in the package to avoid two agents rewriting replay.go/recover.go concurrently.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [18eac796-d1fd-4619-94cb-1164bf989634](../seq-floor-guard-predicate-keys-on-discard-not-on-account--18eac796/task.md)
- **blocks** [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../../CORE/Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md)
- **blocks** [e120153b-9d8a-4b6a-bd4e-89431954496b](../Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md)
- **relates to** [259b7033-2191-423f-bb7b-cff8c6b59dc1](../Bound-the-wal-index-floor-reserved-value-the-same-way-as--259b7033/task.md)
- **relates to** [2a38cdec-528f-47ef-8f38-7f83465b0213](../CONTRACTS-ONDISK.md-and-four-sibling-Go-comments-oversta--2a38cdec/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DUR-5](../DUR-5--a7123e88/task.md) — DUR-5: Append-only message audit log (done)
- [be447589-6583-4d5c-a9d4-ec9d9fef0f1c](../../CORE/Enforce-data-directory-permissions-at-startup-and-bound--be447589/task.md) — Enforce data-directory permissions at startup, and bound the message-seq floor (done)
- [e120153b-9d8a-4b6a-bd4e-89431954496b](../Fix-WAL-recovery-reissuing-a-discarded-tail-record-index--e120153b/task.md) — Fix WAL recovery reissuing a discarded tail record index (invariant 1 violation, NOT a na… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [18eac796-d1fd-4619-94cb-1164bf989634](../seq-floor-guard-predicate-keys-on-discard-not-on-account--18eac796/task.md) — seq-floor guard predicate keys on discard, not on accounting -- boundary-exact truncation… (todo)
- [2a38cdec-528f-47ef-8f38-7f83465b0213](../CONTRACTS-ONDISK.md-and-four-sibling-Go-comments-oversta--2a38cdec/task.md) — CONTRACTS-ONDISK.md and four sibling Go comments overstate the seq-floor migration guard:… (todo)
- [4ae04e3b-4a24-45fe-8521-c548c930c1db](../Rewrite-the-seq-floor-migration-WARN-and-its-comment-Log--4ae04e3b/task.md) — Rewrite the seq-floor migration WARN (and its comment/LogRepaired doc) so it claims only… (done)
- [HANDOVER-REGISTER](../../HANDOVER/HANDOVER-REGISTER--7fddae9d/task.md) — HANDOVER-REGISTER: KNOWN_ISSUES.md, the known-defect register (todo)
- [b7ac3580-d4ff-44f0-9d10-a734ef4a6043](../internal-hub-hub.go-590-s-no-floor-file-quarantine-ERROR--b7ac3580/task.md) — internal/hub/hub.go:590's no-floor-file quarantine ERROR promises the file will be writte… (todo)
- [d9cfaa61-d643-44eb-b38f-22dbd29e6692](../Close-the-two-coverage-gaps-the-security-gates-declared--d9cfaa61/task.md) — Close the two coverage gaps the security gates declared UNVERIFIED on the seq-floor/data-… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
