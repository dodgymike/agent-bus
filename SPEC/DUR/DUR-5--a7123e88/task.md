# DUR-5: Append-only message audit log

| Field | Value |
| --- | --- |
| Public id | `a7123e88-4997-4a5e-ac67-2c0c174e2b43` |
| Key | DUR-5 |
| Epic | [DUR](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | durability |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:05:46.625895+00:00 |
| Updated | 2026-08-08T13:28:29.605240+00:00 |
| Completed | 2026-08-08T13:28:29.605222+00:00 |

## Proof command

```sh
go test -race -run TestAuditLog ./internal/wal
```

## Status note

BLOCKED (P0, confirmed independently 2026-08-07 by a third agent): internal/hub/hub.go passes Audit: &wal.AuditRecord{} (empty). wal.Begin fail-closed validation rejects it, so EVERY POST /v1/send and /v1/broadcast fails on a pristine data dir with wal: audit record is invalid: message_id is empty -- reproduced fresh-enrol-then-first-send. The mint succeeds one line earlier, so the message id is populated but the audit record is not -- that is what misdirects debugging to the wrong layer. Reviewer/security both already gate on this (see notes): DUR-5 is CODE-COMPLETE in internal/wal but NOT LIVE and must not be marked done until the hub wiring lands in the SAME commit and go test ./internal/hub -run TestSend is green. This is the THIRD instance of the same ordering-mistake pattern in this project (signatures required before the client could sign; client-auth before the client certificate existed; now audit-required before the producer populates it) -- record the pattern, not just the instance. Missing-test gap: no round-trip test asserts a plain send SUCCEEDS on a pristine dir with the audit path compiled in; todays audit tests exercise internal/wal in isolation only, so an empty-record producer passes a green go test ./internal/wal.

## Description

A second, separate append-only file (distinct from the WAL) that every message (broadcast + DM) is written to as part of the same commit, independent of the WAL's own record-keeping -- the audit trail invariant 6 calls out explicitly. The audit record is METADATA AND ROUTING INFO ONLY -- message id, sequence, sender, recipient(s), bus path traversed, timestamp, size, and a content hash of the body -- and never the message body itself. The WAL is NOT affected by this change: it still carries whatever it needs to reconstruct state on replay, including bodies if replay requires them; only this separate audit log is metadata-only. Rationale: agent-bus is getting Signal-style end-to-end encryption with forward secrecy (CRYPTO epic); an audit log holding plaintext becomes unwritable the moment PFS lands, and one holding ciphertext the bus can never decrypt is dead weight -- so the audit trail is deliberately a routing/provenance record, not a content archive, and the content hash preserves the ability to prove WHAT was sent without retaining it. Never edited or truncated except by the verified-corrupt-tail rule. Forward-compatibility requirement: the record must be shaped so the CRYPTO epic can add an encrypted-envelope descriptor field later WITHOUT an on-disk format break (e.g. reserve/permit additional optional fields in the JSON payload).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [259b7033-2191-423f-bb7b-cff8c6b59dc1](../Bound-the-wal-index-floor-reserved-value-the-same-way-as--259b7033/task.md) — Bound the wal-index-floor reserved value the same way as the message-seq floor (todo)
- [9fd58deb-6fb8-4d4e-8bf1-6df01329c3b2](../Expose-on-wal.Recovered-the-highest-index-a-record-actua--9fd58deb/task.md) — Expose on wal.Recovered the highest index a record actually CONSUMED (todo)
- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (in_progress)
- [CRYPTO-1](../../CRYPTO/CRYPTO-1--30570fb9/task.md) — CRYPTO-1: DESIGN SPIKE -- Signal-style E2E crypto for agent-bus (CRYPTO_DEEPDIVE.md + DEC… (in_progress)
- [CRYPTO-11](../../CRYPTO/CRYPTO-11--0047e5b7/task.md) — CRYPTO-11: Audit-log content hash for signed (cleartext) messages -- implements the invar… (todo)
- [CRYPTO-2](../../CRYPTO/CRYPTO-2--0ad37da2/task.md) — CRYPTO-2: Adopt the crypto primitive layer chosen by the spike (internal/cryptobox + go.m… (superseded)
- [DUR-12-FU-AUDITUPGRADE](../DUR-12-FU-AUDITUPGRADE--1a04063a/task.md) — DUR-12-FU-AUDITUPGRADE: version 1 audit logs have no upgrade path -- must land before the… (todo)
- [DUR-9](../DUR-9--8234db61/task.md) — DUR-9: Wire the WAL into server startup (open, replay, hold for process lifetime, expose… (done)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [c55bccbb-0c80-4c82-84fb-bcb3437d8f73](../CONTRACTS-ONDISK.md-document-the-bus.audit-on-disk-file--c55bccbb/task.md) — CONTRACTS-ONDISK.md: document the bus.audit on-disk file (DUR-5 landed, wave 217a3c0, doc… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
