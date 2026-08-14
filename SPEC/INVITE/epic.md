# EPIC INVITE — Invite-only enrolment

[← all epics](../../SPEC.md)

**6 open / 10 total.** Full records live in `SPEC/INVITE/<task>/task.md`.

_Relations (real)_ are authoritative edges from the Spec Server. _Referenced (derived)_ are guesses parsed out of description free text — useful, but not a dependency list.

## Open tasks (6)

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| INVITE-GATE | INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… | todo | P0 | [task.md](INVITE-GATE--05a5216d/task.md) | blocks [INVITE-CLIENT](INVITE-CLIENT--4123e25d/task.md) | [ENROL-SHAPE](ENROL-SHAPE--8942c8c8/task.md) [INVITE-STORE](INVITE-STORE--a9ef92de/task.md) [INVITE-MINT](INVITE-MINT--1d0d0e60/task.md) [INVITE-HARDEN](INVITE-HARDEN--d250d0dd/task.md) [INVITE-REVOKE](INVITE-REVOKE--d9def083/task.md) [INVITE-CLIENT](INVITE-CLIENT--4123e25d/task.md) +4 more |
| INVITE-CLIENT | INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… | todo | P1 | [task.md](INVITE-CLIENT--4123e25d/task.md) | blocked by [INVITE-GATE](INVITE-GATE--05a5216d/task.md)<br>blocks [RELAY-25](../RELAY/RELAY-25--10491a01/task.md) | [INVITE-GATE](INVITE-GATE--05a5216d/task.md) [CLI-1](../CLI/CLI-1--0495d133/task.md) [CLI-2](../CLI/CLI-2--39318208/task.md) [AGENTIF-2](../AGENTIF/AGENTIF-2--15e4509c/task.md) [MTLS-LISTENER](../MTLS/MTLS-LISTENER--17e70a7e/task.md) [RELAY-25](../RELAY/RELAY-25--10491a01/task.md) +1 more |
| INVITE-HARDEN | INVITE-HARDEN: constant-time invite-secret comparison and ONE indistinguishable failure r… | todo | P1 | [task.md](INVITE-HARDEN--d250d0dd/task.md) | — | [INVITE-GATE](INVITE-GATE--05a5216d/task.md) [0b43393e-556b-409a-938a-846be2fb4a75](EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) |
| INVITE-PEERGUARD | INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate t… | todo | P1 | [task.md](INVITE-PEERGUARD--f5d91dbe/task.md) | — | [INVITE-GATE](INVITE-GATE--05a5216d/task.md) [RELAY-1](../RELAY/RELAY-1--9bc9d6c4/task.md) [MTLS-RELAYGUARD](../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) [0b43393e-556b-409a-938a-846be2fb4a75](EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) |
| INVITE-REVOKE | INVITE-REVOKE: durably revoke an un-redeemed invite, and state what revocation does to an… | todo | P1 | [task.md](INVITE-REVOKE--d9def083/task.md) | — | [INVITE-STORE](INVITE-STORE--a9ef92de/task.md) [INVITE-GATE](INVITE-GATE--05a5216d/task.md) [AUTH-4](../AUTH/AUTH-4--a853261d/task.md) [0b43393e-556b-409a-938a-846be2fb4a75](EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) |
| d7a0e7c4-6ea8-4fa7-8db5-c8044dce3a8d | TestInviteNotDurableIsRefused is a time-bomb: hardcoded 2026-08-07 fixture date now falls… | todo | P1 | [task.md](TestInviteNotDurableIsRefused-is-a-time-bomb-hardcoded-2--d7a0e7c4/task.md) | — | [RELAY-19](../RELAY/RELAY-19--24e0bd11/task.md) |

## Closed tasks (4) — done, cancelled, superseded

| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |
| --- | --- | --- | --- | --- | --- | --- |
| 0b43393e-556b-409a-938a-846be2fb4a75 | EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… | superseded | P0 | [task.md](EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) | — | [AUTH-1-FU-ACTIVECAP](../AUTH/AUTH-1-FU-ACTIVECAP--2d92b699/task.md) [AUTH-1-FU-PENDINGCAP](../AUTH/AUTH-1-FU-PENDINGCAP--687ad8c9/task.md) [ENROL-SHAPE](ENROL-SHAPE--8942c8c8/task.md) [INVITE-STORE](INVITE-STORE--a9ef92de/task.md) [INVITE-MINT](INVITE-MINT--1d0d0e60/task.md) [INVITE-GATE](INVITE-GATE--05a5216d/task.md) +4 more |
| ENROL-SHAPE | ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… | done | P0 | [task.md](ENROL-SHAPE--8942c8c8/task.md) | — | [INVITE-STORE](INVITE-STORE--a9ef92de/task.md) [INVITE-GATE](INVITE-GATE--05a5216d/task.md) [MTLS-BIND](../MTLS/MTLS-BIND--b6378bda/task.md) [AUTH-3](../AUTH/AUTH-3--d53e3b21/task.md) [AUTH-1-FU-POPKEY](../AUTH/AUTH-1-FU-POPKEY--6e3083b0/task.md) [0b43393e-556b-409a-938a-846be2fb4a75](EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) |
| INVITE-MINT | INVITE-MINT: an operator mints a single-use, expiring invite -- the server is authoritati… | done | P0 | [task.md](INVITE-MINT--1d0d0e60/task.md) | — | [INVITE-STORE](INVITE-STORE--a9ef92de/task.md) [INVITE-GATE](INVITE-GATE--05a5216d/task.md) [0b43393e-556b-409a-938a-846be2fb4a75](EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) |
| INVITE-STORE | INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by… | done | P0 | [task.md](INVITE-STORE--a9ef92de/task.md) | — | [ENROL-SHAPE](ENROL-SHAPE--8942c8c8/task.md) [INVITE-MINT](INVITE-MINT--1d0d0e60/task.md) [INVITE-GATE](INVITE-GATE--05a5216d/task.md) [INVITE-REVOKE](INVITE-REVOKE--d9def083/task.md) [MTLS-BIND](../MTLS/MTLS-BIND--b6378bda/task.md) [DUR-12](../DUR/DUR-12--cbc9ab0c/task.md) +1 more |

## Epic description

Invites are single-use, expiring and revocable, minted by an operator; redeeming one is the ONLY route onto the bus, including for peer buses (CLAUDE.md invariant 3, amended 2026-08-02). Supersedes placeholder epic task 0b43393e-556b-409a-938a-846be2fb4a75, whose planner breakdown is these tasks (ENROL-SHAPE, INVITE-STORE, INVITE-MINT, INVITE-GATE, INVITE-HARDEN, INVITE-REVOKE, INVITE-CLIENT, INVITE-PEERGUARD).

---

_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._
