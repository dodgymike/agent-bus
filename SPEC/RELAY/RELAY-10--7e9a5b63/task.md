# RELAY-10: Durable peer records that survive restart

| Field | Value |
| --- | --- |
| Public id | `7e9a5b63-90ec-4ae5-ae6c-2e53f00d4de5` |
| Key | RELAY-10 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | relay |
| Section | backlog |
| Tags | vacuous-today |
| Created | 2026-08-08T15:56:40.657578+00:00 |
| Updated | 2026-08-08T20:01:10.706356+00:00 |
| Completed | 2026-08-08T20:01:10.706340+00:00 |

## Proof command

```sh
go test -race -run TestPeerStoreSurvivesReplay ./internal/relay
```

## Status note

Gates CLEARED at round 4 (2026-08-08T17:42): security re-verified independently (11 mutations applied, all 11 caught, none survived) and posted PASS. All three prior blockers (P0 sequence-rewind ordering, C1-C4 stale-doc-claims, F1 CONTRACTS false claim) are fixed and mutation-verified. REMAINING BEFORE done: code is CODE-COMPLETE and gate-cleared but NOT YET COMMITTED (peerstore.go/peerstore_test.go still untracked/staged in the working tree) and NOT WIRED (PeerStore is constructed nowhere outside internal/relay -- confirmed by grep). Do not complete until an integrator lands a commit sha. On completion, test_summary must say CODE-COMPLETE/UNWIRED, not that peers durably survive restart in production -- wiring is RELAY-24, now hard-blocked by RELAY-34 (revocation fails open on WAL discard, disclosed not fixed).

## Description

FEDERATION phase, wave 1 (F5).
Owns internal/relay/peerstore.go (new), peerstore_test.go (new), CONTRACTS-ONDISK.md (EXCLUSIVE
this wave).

relay.Registry persists nothing (registry.go:180-189) -- peers vanish on restart, so the operator
would re-peer three machines every time.

CORRECTED 2026-08-08 (spec-keeper, from RELAY-7 trust deep-dive + gate findings) -- description below
supersedes the original single-record text, which no longer matches what shipped:

Ship TWO wal.Entry.Kind values, not one record:
  - Kind="peer": PeerRecord {bus_id, config_seq, state, base_url} -- NEVER carries key material.
  - Kind="bustrust": BusTrustRecord {bus_id, config_seq, state, bus_signing_keys[]} -- NEVER carries
    a base_url/transport field.
Reason for the split: laptop(A) <-> internet(B) <-> here(C) -- C must be able to PIN A's bus
signing key while having NO peering/routing relationship with A at all. A single coupled record
cannot express trust-without-routing; that is the case the whole federation design turns on.
bus_signing_keys is a LIST bounded at MaxPinnedBusSigningKeys=2, derived (not chosen) from
signed.go:178-182: more than one key is carried ONLY during a rollover window (outgoing + incoming),
never as a general-purpose list. Both record bodies additionally carry a "rec" field repeating the
Kind: without it the two kinds' TOMBSTONE bodies are byte-identical, and a Kind mix-up during future
wiring would silently un-pin a bus (land a route tombstone in the trust table or vice versa) with no
decode error.

Monotonic upsert on replay is keyed on config_seq, a BUS-WIDE, server-minted counter -- explicitly
NOT per-peer, NOT state, and NOT a timestamp. Rejected alternatives, kept for the reason each was
rejected: not `state` (a peer lifecycle is not terminal; re-peering after removal must work); not a
timestamp (clocks step back); not per-peer (P0, found by the gates -- a per-peer counter derives from
an entry that can legitimately leave the table on tombstone sweep or a capacity discard, so the next
write restarts at 1 over a log holding 1..N and a superseded generation wins on replay). Safe because
the mark is raised from every decoded record BEFORE any discard decision and never lowered.

Modelled on internal/invite's pending->done shape (invite/record.go:27-34, invite/store.go:965-1000).
No record-type reservation is needed -- Entry.Kind is a free-form application discriminator,
precedent CONTRACTS-ONDISK.md:703-710. State that explicitly in the contract update.

KNOWN P1 NOT YET CLOSED (see RELAY-10-FU-REVOCATION-FAILOPEN): a discard is fail-closed for a route
but fail-OPEN for a revocation -- losing a withdrawal (tombstone) record reinstates the previous
generation, so a revoked pinned bus signing key can come back. Reproduced via an 8-byte tail
truncation of bus.wal, which invariant 6 requires recovery to survive by discarding and starting
anyway -- so this is reachable through a supported path. RELAY-17 builds its cross-bus trust anchor
on this record and must know this before consuming it.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-12](../RELAY-12--069f0607/task.md)
- **blocks** [RELAY-17](../RELAY-17--817649ce/task.md)
- **follow-up** [RELAY-34](../RELAY-34--03fd8897/task.md)
- **follow-up** [RELAY-35](../RELAY-35--2bafb2a5/task.md)
- **follow-up** [RELAY-36](../RELAY-36--1961682b/task.md)
- **follow-up** RELAY-37 (unresolved)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-17](../RELAY-17--817649ce/task.md) — RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-34](../RELAY-34--03fd8897/task.md) — RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can… (done)
- [RELAY-7](../RELAY-7--756655f3/task.md) — RELAY-7: Cross-bus trust deep-dive (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-12](../RELAY-12--069f0607/task.md) — RELAY-12: agent-bus peer add\|list\|remove (done)
- [RELAY-17](../RELAY-17--817649ce/task.md) — RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-34](../RELAY-34--03fd8897/task.md) — RELAY-34: Revocation fails OPEN on a WAL discard -- a revoked pinned bus signing key can… (done)
- [RELAY-35](../RELAY-35--2bafb2a5/task.md) — RELAY-35: PeerStore composition-root precondition -- replay MUST run before the first wri… (todo)
- [RELAY-36](../RELAY-36--1961682b/task.md) — RELAY-36: internal/relay/client.go peerURL accepts a path -- tighten to bare-origin, touc… (todo)
- [RELAY-37](../RELAY-37--a613ddc8/task.md) — RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64)… (cancelled)
- [RELAY-37](../RELAY-37--7a7e6e8b/task.md) — RELAY-37: peerstore.go:690 unparseable-URL error breaks the file's own elidePeerText(64)… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
