# ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE, before invite, mTLS or proof-of-possession break it three times

| Field | Value |
| --- | --- |
| Public id | `8942c8c8-b8ea-4ec5-8689-64f25eedd648` |
| Key | ENROL-SHAPE |
| Epic | [INVITE](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:47.594959+00:00 |
| Updated | 2026-08-08T10:29:29.281912+00:00 |
| Completed | 2026-08-07T12:10:36.447459+00:00 |

## Proof command

```sh
grep -qF '## 2026-08-07 — ENROL-SHAPE: the final `/v1/enroll` shape and `auth.RosterEntry` field set' DECISIONS.md && grep -q AuthPublicKey DECISIONS.md && grep -q MessagingPublicKey DECISIONS.md && grep -q InviteID DECISIONS.md && grep -q CertBindings DECISIONS.md && grep -q Epoch DECISIONS.md
```

## Description

EPIC: 0b43393e-556b-409a-938a-846be2fb4a75 | DEPENDS ON: none | BLOCKS: INVITE-STORE, INVITE-GATE, MTLS-BIND, AUTH-3 (d53e3b21), AUTH-1-FU-POPKEY (6e3083b0-c113-4b26-9dd6-025825671ceb)

BLOCKED ON USER DECISION -- do not implement until the escalated questions are answered (bootstrap/who mints the first invite; how a client learns the bus cert fingerprint; migration for already-enrolled agents; rotation/expiry). Three separately-filed changes each break POST /v1/enroll's request body: the invite field (INVITE-GATE), the client-cert fingerprint binding (MTLS-BIND), and the proof-of-possession signature already filed as AUTH-1-FU-POPKEY (6e3083b0-c113-4b26-9dd6-025825671ceb, which explicitly says "this CHANGES THE ENROL WIRE SHAPE ... do not land it unilaterally"). Landing them independently revises the same contract three times. This task records ONE target shape in DECISIONS.md covering: the enrol request/response fields, the final auth.RosterEntry field set (internal/auth/roster.go:16-37 -- today AgentID/Name/PublicKey/EnrolledAt; it needs a client-cert fingerprint field), and the ordering rule that AUTH-3 (d53e3b21, durable roster) must encode that final field set so the durable record is written once, not migrated. Deliverable is a DECISIONS.md entry ONLY -- do NOT update CONTRACTS-HTTP.md, which documents SHIPPED behaviour, and none of this has shipped. Escalation context: today the roster, sessions and idempotency table are ALL in-memory (internal/auth/roster.go MemoryRoster, internal/auth/service.go:161), so there is currently NOTHING persisted to migrate -- that window closes the moment AUTH-3 lands.

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
- [AUTH-1-FU-POPKEY](../../AUTH/AUTH-1-FU-POPKEY--6e3083b0/task.md) — AUTH-1-FU-POPKEY: enrolment does not prove possession of the enrolling private key (todo)
- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [INVITE-STORE](../INVITE-STORE--a9ef92de/task.md) — INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by… (done)
- [MTLS-BIND](../../MTLS/MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [AUTH-8-FU-MSGKEY-POP](../../AUTH/AUTH-8-FU-MSGKEY-POP--576a794d/task.md) — AUTH-8-FU-MSGKEY-POP: enrolment does not prove possession of the MESSAGING private key (A… (todo)
- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [INVITE-STORE](../INVITE-STORE--a9ef92de/task.md) — INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by… (done)
- [MTLS-BIND](../../MTLS/MTLS-BIND--b6378bda/task.md) — MTLS-BIND: enrolment binds the presenting client-cert fingerprint to the SERVER-MINTED ag… (done)
- [MTLS-BIND-FU-DOCS](../../MTLS/MTLS-BIND-FU-DOCS--8c40ea26/task.md) — MTLS-BIND-FU-DOCS: document the enrolment certificate binding -- CONTRACTS-HTTP.md 409, D… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
