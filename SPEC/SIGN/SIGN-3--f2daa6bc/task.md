# SIGN-3: Broadcast signature covers the recipient set (prevents split-content broadcasts)

| Field | Value |
| --- | --- |
| Public id | `f2daa6bc-53ee-4788-935c-ab73693c5e75` |
| Key | SIGN-3 |
| Epic | [SIGN](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | crypto |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:59:07.181342+00:00 |
| Updated | 2026-08-02T13:11:41.586881+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestBroadcastDigestSignature ./internal/...
```

## Description

GATED on SIGN-1/SIGN-2. Replaces the encryption-specific scope of superseded CRYPTO-8 (broadcast fan-out under authenticated encryption / Sender Keys) and superseded RATCHET-4 (broadcast fan-out under pairwise ratchets) -- neither ratchets nor per-recipient encryption apply anymore, but the underlying risk they both flagged is REAL and still applies to a signature-only design: MSG-2 broadcasts to N agents as N separate deliveries, and without an extra check a malicious SENDER could put DIFFERENT content in each recipient's copy under the same broadcast id, and no individual recipient could tell (each copy's own per-message signature verifies fine in isolation). Fix: the sender additionally signs (invariant 9 -- crypto/ed25519.Sign, no custom construction) a digest over (broadcast_id, hash-of-body, the SORTED set of recipient fully-qualified ids), included in every recipient's envelope alongside the per-message signature from SIGN-2. A recipient who wants the 'everyone got the same broadcast' guarantee can compare this digest against other recipients' copies (e.g. via bus-trace tooling or by agents comparing out of band); document that comparison, don't just produce the digest and leave it unused. Use a standard, audited hash for hash-of-body (crypto/sha256, stdlib) -- not a bespoke construction. Tests: every recipient's digest for one broadcast is identical; a tampered per-recipient body still fails SIGN-2's per-message signature; a forged/mismatched digest is rejected. ADDED 2026-08-02 (invariant 7, epic-completion pass): the broadcast wrapper ships IN THIS TASK -- scripts/bus-broadcast.sh (AGENTIF-4) must produce both signatures via the `agent-bus sign` subcommand SIGN-2 adds, and AGENT_PROTOCOL.md must document the recipient-set digest and how two recipients compare it. A digest that no wrapper emits and no agent can check is not a defence. Verify through scripts/bus-broadcast.sh against a running throwaway bus, not hand-written curl.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AGENTIF-4](../../AGENTIF/AGENTIF-4--715fc1b8/task.md) — AGENTIF-4: scripts/bus-broadcast.sh + AGENT_PROTOCOL.md entry (superseded)
- [CRYPTO-8](../../CRYPTO/CRYPTO-8--2b1068eb/task.md) — CRYPTO-8: Broadcast to N agents -- authenticated encryption for the fan-out path (deferred)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [RATCHET-4](../../RATCHET/RATCHET-4--58fd8bc3/task.md) — RATCHET-4: Broadcast fan-out under pairwise ratchets (superseded)
- [SIGN-1](../SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-2](../SIGN-2--1c183f10/task.md) — SIGN-2: Sign on the send path (Ed25519 detached signature travels with the message) (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [COMMS-MULTI-DESIGN](../../COMMS/COMMS-MULTI-DESIGN--8e56075b/task.md) — Design: widen /v1/send to true multi-recipient (Finding A) without touching SIGN-3 (todo)
- [COMMS-THREAD-FIELD](../../COMMS/COMMS-THREAD-FIELD--35db4a7b/task.md) — Add a wire-level thread/reply field -- ONLY if COMMS-THREAD-TRIAL shows convention is ins… (superseded)
- [CRYPTO-8](../../CRYPTO/CRYPTO-8--2b1068eb/task.md) — CRYPTO-8: Broadcast to N agents -- authenticated encryption for the fan-out path (deferred)
- [RATCHET-4](../../RATCHET/RATCHET-4--58fd8bc3/task.md) — RATCHET-4: Broadcast fan-out under pairwise ratchets (superseded)
- [RELAY-24-BLOCKER-EGRESS-HANDSHAKE](../../RELAY/RELAY-24-BLOCKER-EGRESS-HANDSHAKE--0ab31d26/task.md) — RELAY-24-BLOCKER-EGRESS-HANDSHAKE: this bus never DIALS a peer, so its relay Registry nev… (todo)
- [RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC](../../RELAY/RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC--7126f08b/task.md) — RELAY-24-BLOCKER-HUBINGEST-FU-AUDITHASH-DOC: Record the relayed audit content-hash decisi… (done)
- [c55bccbb-0c80-4c82-84fb-bcb3437d8f73](../../DUR/CONTRACTS-ONDISK.md-document-the-bus.audit-on-disk-file--c55bccbb/task.md) — CONTRACTS-ONDISK.md: document the bus.audit on-disk file (DUR-5 landed, wave 217a3c0, doc… (todo)
- [cea09b96-72db-40f1-84b4-c2e227eae1cf](../../TOOLING/proof-check.sh-subtest-SKIP-PASS-lines-invisible-to-plai--cea09b96/task.md) — proof-check.sh: subtest SKIP/PASS lines invisible to plain-text counter -- parent-PASS/al… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
