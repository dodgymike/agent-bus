# RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can come back

| Field | Value |
| --- | --- |
| Public id | `03fd8897-731d-4e4b-95a0-fe4b9b47a354` |
| Key | RELAY-34 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | relay |
| Section | backlog |
| Tags | security, durability, blocks-relay-17 |
| Created | 2026-08-08T17:39:09.306492+00:00 |
| Updated | 2026-08-08T22:20:33.148108+00:00 |
| Completed | 2026-08-08T22:20:33.148090+00:00 |

## Proof command

```sh
go test -race -run TestPeerStoreTrustSurvivesATornWALTail ./internal/relay
```

## Description

Found by the security gate during RELAY-10 review (round-3 addendum, finding F2). PeerStore.BusTrustRecord carries the COMPLETE post-transition state on every entry (not a delta), so a discard of the withdrawal (tombstone) record silently REINSTATES the previous generation -- and for the trust table, the previous generation is a pinned bus signing key the operator deliberately REVOKED.

Reproduced end to end against a real wal.Log: PutTrust(bus,key) then RemoveTrust(bus), both acknowledged, PinnedKeys correctly nil. Truncate 8 bytes off the tail of bus.wal -- a torn tail -- reopen: PinnedKeys returns the REVOKED key, active, 1 key pinned. This is reachable through a SUPPORTED path, not an exotic one: invariant 6 (CLAUDE.md) requires recovery to survive exactly this kind of tail damage by discarding and starting anyway, never refusing to boot. Realistic triggers: bit-rot, a torn write, a VM/filesystem snapshot rollback (which un-revokes every pin revoked since the snapshot).

Not reachable today -- PeerStore is constructed nowhere outside internal/relay (RELAY-10 shipped code-complete, unwired). It becomes live the moment RELAY-17 (CrossBusTrust) or RELAY-24 (composition root) wires PeerStore in, since RELAY-17 builds its cross-bus trust anchor directly on this record.

Closing it needs a mechanism the current record design does not have -- this is not a small fix. Candidates raised by the security gate (none applied, read-only gate): (a) refuse to boot -- or at minimum ERROR loudly -- at startup when wal Recovered.DiscardCount > 0 AND the trust table holds any active pins, so an operator is told to re-verify revocations by hand; (b) a durable per-bus REVOCATION FLOOR, independent of the tombstone record itself, that a lost tombstone cannot roll back (structurally the same fix class as RELAY-10s sequence-rewind and swept-tombstone-resurrection defects, but for the specific case where the record that must survive loss is a revocation).

Also: the shipped text (peerstore.go and the matching CONTRACTS-ONDISK.md bullet) claims a discard is fail-closed in the direction that matters and can never install a key this bus did not already hold -- true of Apply() in isolation, false of the system: a discard cannot INSTALL a key, but it CAN fail to REMOVE one, which for a revocation mechanism is exactly the direction that matters. That sentence needs correcting alongside the fix (or immediately, as a documentation-only change, if the mechanism fix lands later).

RELATE TO RELAY-17: the keystone builds its cross-bus trust anchor on this record: its implementer must know which half is sound (routing) and which is not yet (revocation) before consuming PinnedKeys(). A security re-verification of RELAY-10 is running as of 2026-08-08T17:2x and will say precisely what that half is -- post its conclusion as a note on RELAY-17 once it lands, and do not treat RELAY-10 as safe to build on for revocation until this task closes.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-17](../RELAY-17--817649ce/task.md) — RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-SPEC-DEPS](../../CONTEXT/CONTEXT-SPEC-DEPS--8280358d/task.md) — CONTEXT-SPEC-DEPS: Adopt and document the blocks-relation convention for task dependencies (todo)
- [RELAY-10](../RELAY-10--7e9a5b63/task.md) — RELAY-10: Durable peer records that survive restart (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-34-FU-ATOMICWRITER](../RELAY-34-FU-ATOMICWRITER--7497f762/task.md) — RELAY-34-FU-ATOMICWRITER: four copies of the atomic temp+fsync+rename writer must move to… (todo)
- [RELAY-34-FU-CONFUSABLEORDER](../RELAY-34-FU-CONFUSABLEORDER--bbe577ae/task.md) — RELAY-34-FU-CONFUSABLEORDER: reconcile's ASCII-case-confusable guard is order-dependent (… (todo)
- [RELAY-34-FU-DIRWIRING](../RELAY-34-FU-DIRWIRING--4b302011/task.md) — RELAY-34-FU-DIRWIRING: cmd/agent-bus/peer.go must pass PeerStoreOptions.Dir or every revo… (todo)
- [RELAY-34-FU-ERRNIT](../RELAY-34-FU-ERRNIT--8e68c6cb/task.md) — RELAY-34-FU-ERRNIT: a comment names ErrPeerBusIDCollision where the refusal may now be Er… (todo)
- [RELAY-34-FU-REPAIRCOST](../RELAY-34-FU-REPAIRCOST--8bd25efd/task.md) — RELAY-34-FU-REPAIRCOST: floor repair from the log is linear, one rewrite + fsync per with… (todo)
- [RELAY-34-FU-TEMPREAPER](../RELAY-34-FU-TEMPREAPER--07777f7b/task.md) — RELAY-34-FU-TEMPREAPER: no stale-temp reaper for .peer-withdrawal-floor-* left by a crash… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
