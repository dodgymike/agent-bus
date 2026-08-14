# INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by WAL replay, with a crash-injection test

| Field | Value |
| --- | --- |
| Public id | `a9ef92de-865f-4ee7-8b12-544409ff0263` |
| Key | INVITE-STORE |
| Epic | [INVITE](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:47.914908+00:00 |
| Updated | 2026-08-07T18:56:40.669126+00:00 |
| Completed | 2026-08-07T18:56:40.669108+00:00 |

## Proof command

```sh
go test -race -run 'TestInviteStoreRecovery|TestInviteSingleUseSurvivesCrash|TestInviteExpiredIsNotRedeemable' ./internal/invite && grep -qi 'invite record' CONTRACTS-ONDISK.md
```

## Status note

A feature agent is LIVE on internal/invite RIGHT NOW (2026-08-07): 9 uncommitted files (doc.go, errors.go, id.go, record.go, retention.go, secret.go, store.go, store_test.go, crash_test.go), plus an implementer kind=report and a security PASS verdict already posted to this tasks notes. NONE of it is committed (git status confirms internal/invite/* is entirely untracked/staged-nothing) and NOTHING is shipped -- do not claim-next this task, do not treat the security PASS as gating a merge yet. Flipped todo -> in_progress by spec-keeper to prevent a second agent colliding on internal/invite via claim-next. Complete only once committed, with go test -race ./internal/invite green and the crash-injection test (TestInviteSingleUseSurvivesCrash) proven non-vacuous.

## Description

EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: ENROL-SHAPE | BLOCKS: INVITE-MINT, INVITE-GATE, INVITE-REVOKE

New internal/invite package behind an injected interface, following the existing auth.Roster pattern (internal/auth/roster.go:39-67). Durability is REQUIRED, not optional: if single-use state is in memory only, a restart makes every spent invite redeemable again. Uses the existing two-phase path -- wal.Log.Begin/Txn.Commit (internal/wal/log.go:367, :436) with Entry.Kind = "invite". Entry.Kind is a free-form application discriminator (internal/wal/log.go:78-79), NOT a numbered frame type, so NO record-type reservation is needed and internal/wal/format.go's Type enum is not touched. Record must carry the client-cert fingerprint field DEFINED BUT UNUSED from day one, per ENROL-SHAPE, so MTLS-BIND adds a check rather than a schema change. Per CLAUDE.md, durability code requires a crash-injection test. Note DUR-12 (cbc9ab0c, in flight) changes WAL record framing to an HMAC MAC -- that is below Entry.Kind and does not conflict; do not touch DUR-12.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [DUR-12](../../DUR/DUR-12--cbc9ab0c/task.md) — DUR-12: Replace CRC32C with an HMAC-SHA256 keyed MAC (ON-DISK FORMAT CHANGE, reserved ond… (done)
- [ENROL-SHAPE](../ENROL-SHAPE--8942c8c8/task.md) — ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… (done)
- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [INVITE-MINT](../INVITE-MINT--1d0d0e60/task.md) — INVITE-MINT: an operator mints a single-use, expiring invite -- the server is authoritati… (done)
- [INVITE-REVOKE](../INVITE-REVOKE--d9def083/task.md) — INVITE-REVOKE: durably revoke an un-redeemed invite, and state what revocation does to an… (todo)
- [MTLS-BIND](../../MTLS/MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [ENROL-SHAPE](../ENROL-SHAPE--8942c8c8/task.md) — ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… (done)
- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [INVITE-MINT](../INVITE-MINT--1d0d0e60/task.md) — INVITE-MINT: an operator mints a single-use, expiring invite -- the server is authoritati… (done)
- [INVITE-REVOKE](../INVITE-REVOKE--d9def083/task.md) — INVITE-REVOKE: durably revoke an un-redeemed invite, and state what revocation does to an… (todo)
- [MTLS-BIND](../../MTLS/MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (in_progress)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
