# EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner pass)

| Field | Value |
| --- | --- |
| Public id | `0b43393e-556b-409a-938a-846be2fb4a75` |
| Key | _(null in the export)_ |
| Epic | [INVITE](../epic.md) |
| Status | superseded |
| Priority | P0 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T20:48:47.714259+00:00 |
| Updated | 2026-08-08T10:29:29.665118+00:00 |
| Completed | — |

## Status note

SUPERSEDED 2026-08-07 by the real INVITE epic (public_id 1339f9e2-0a37-4d94-8fc6-24285a0c9801, key=INVITE), created via POST /epics now that the Spec Server API supports first-class epics. This placeholder task existed to hold the epic description until a planner pass broke it into atomic tasks -- that pass already happened (2026-08-02, planner, recorded in this tasks own notes): ENROL-SHAPE (done), INVITE-STORE, INVITE-MINT, INVITE-GATE (all now epic_key=INVITE), plus INVITE-HARDEN/INVITE-REVOKE/INVITE-CLIENT/INVITE-PEERGUARD (P1, not in my mutation scope this pass, still need epic_key=INVITE set by whoever owns them next). The breakdown IS complete; there is nothing left for this placeholder to gate. Superseded rather than done because this task itself never delivered anything -- it stood in for an epic that now exists as a first-class object.

## Description

USER DECISION, 2026-08-02 (DECISIONS.md "Five decisions" #2; CLAUDE.md invariant 3, amended). Root fix for the entire pre-auth attack family. Enrolment is currently UNAUTHENTICATED, so an attacker can mint its own agents and, from there, exhaust the session table (AUTH-1-FU-ACTIVECAP, raised to P0 as defence-in-depth behind this gate), lock out a named agent (AUTH-1-FU-PENDINGCAP, already fixed), or enumerate the roster. Capping table sizes patches the symptoms one at a time; the invite removes the capability that makes all of them possible.

REQUIREMENTS (from the user, verbatim in substance):
- Invites are SINGLE-USE, EXPIRING and REVOCABLE, minted by an operator.
- Redeeming an invite is the ONLY route onto the bus -- including for PEER buses (bus-to-bus enrolment/federation must also go through invite redemption, not a separate unauthenticated path).
- Composes with mTLS (see the paired mTLS epic): the invite is what AUTHORISES binding a new client certificate to a new agent id -- invite redemption and certificate binding happen together, not as two independent gates either of which alone would suffice.
- CLAUDE.md invariant 3 now covers this directly: "Enrolment is INVITE-ONLY... No agent may enrol without redeeming an operator-minted invite... Invites must be single-use, expiring, and revocable, and redeeming one is the ONLY way onto the bus -- including for peer buses." Read that invariant in full before design.

CONSENT-SENSITIVE: this changes AUTHN DEFAULTS (enrolment moves from open to gated) -- per this project operating rules that is a consent-gated action even though the user has already decided the shape; the atomic tasks under this epic should still each be explicit about what changes for an operator standing up a fresh bus (an invite must now be minted before the FIRST agent can enrol, including whatever bootstraps the operators own tooling).

NEEDS A PLANNER PASS before implementation: this is an epic, not an atomic task. A planner should break it into atomic tasks covering at minimum: invite data model + storage (durable, survives restart -- consider how this interacts with the WAL/store), invite minting (operator-facing, likely a CLI/admin route), invite redemption at enrolment (single-use enforcement, expiry check, replaces or gates todays open enrol route), revocation, peer-bus enrolment redemption (federation path), CONTRACTS-HTTP.md + AGENT_PROTOCOL.md updates, and the mTLS cert-binding integration point once the mTLS epic lands enough to bind to.

Does not yet have atomic sub-tasks; do not claim-next this epic directly -- claim-next the atomic tasks a planner files under it once that pass runs.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1-FU-ACTIVECAP](../../AUTH/AUTH-1-FU-ACTIVECAP--2d92b699/task.md) — AUTH-1-FU-ACTIVECAP: cap ACTIVE sessions per agent -- the one place an agent-id-keyed cap… (done)
- [AUTH-1-FU-PENDINGCAP](../../AUTH/AUTH-1-FU-PENDINGCAP--687ad8c9/task.md) — AUTH-1-FU-PENDINGCAP: MaxPendingPerAgent is a lockout primitive, not a defence -- rekey o… (done)
- [ENROL-SHAPE](../ENROL-SHAPE--8942c8c8/task.md) — ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… (done)
- [INVITE-CLIENT](../INVITE-CLIENT--4123e25d/task.md) — INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… (done)
- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [INVITE-HARDEN](../INVITE-HARDEN--d250d0dd/task.md) — INVITE-HARDEN: constant-time invite-secret comparison and ONE indistinguishable failure r… (in_progress)
- [INVITE-MINT](../INVITE-MINT--1d0d0e60/task.md) — INVITE-MINT: an operator mints a single-use, expiring invite -- the server is authoritati… (done)
- [INVITE-PEERGUARD](../INVITE-PEERGUARD--f5d91dbe/task.md) — INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate t… (todo)
- [INVITE-REVOKE](../INVITE-REVOKE--d9def083/task.md) — INVITE-REVOKE: durably revoke an un-redeemed invite, and state what revocation does to an… (todo)
- [INVITE-STORE](../INVITE-STORE--a9ef92de/task.md) — INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1-FU-ACTIVECAP-DOCS](../../AUTH/AUTH-1-FU-ACTIVECAP-DOCS--27a811c9/task.md) — AUTH-1-FU-ACTIVECAP-DOCS: document the per-agent ACTIVE-session cap in CONTRACTS-HTTP.md… (todo)
- [ENROL-SHAPE](../ENROL-SHAPE--8942c8c8/task.md) — ENROL-SHAPE: settle the FINAL /v1/enroll wire shape and auth.RosterEntry field set ONCE,… (done)
- [INVITE-CLIENT](../INVITE-CLIENT--4123e25d/task.md) — INVITE-CLIENT: the Go client/CLI redeems an invite at enrol (+ AGENT_PROTOCOL.md entry) -… (done)
- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [INVITE-HARDEN](../INVITE-HARDEN--d250d0dd/task.md) — INVITE-HARDEN: constant-time invite-secret comparison and ONE indistinguishable failure r… (in_progress)
- [INVITE-MINT](../INVITE-MINT--1d0d0e60/task.md) — INVITE-MINT: an operator mints a single-use, expiring invite -- the server is authoritati… (done)
- [INVITE-PEERGUARD](../INVITE-PEERGUARD--f5d91dbe/task.md) — INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate t… (todo)
- [INVITE-REVOKE](../INVITE-REVOKE--d9def083/task.md) — INVITE-REVOKE: durably revoke an un-redeemed invite, and state what revocation does to an… (todo)
- [INVITE-STORE](../INVITE-STORE--a9ef92de/task.md) — INVITE-STORE: durable single-use invite record (mint/lookup/consume/expire), recovered by… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
