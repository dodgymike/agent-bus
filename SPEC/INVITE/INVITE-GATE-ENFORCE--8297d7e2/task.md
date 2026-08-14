# INVITE-GATE-ENFORCE: enforce invite-only enrolment (P0: anonymous roster exhaustion)

| Field | Value |
| --- | --- |
| Public id | `8297d7e2-be64-4a52-a910-314b4be880cf` |
| Key | INVITE-GATE-ENFORCE |
| Epic | [INVITE](../epic.md) |
| Status | in_progress |
| Priority | P0 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T21:14:37.905096+00:00 |
| Updated | 2026-08-14T22:01:57.642525+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run "TestInviteGate" ./internal/auth/ ./internal/httpapi/ ./cmd/agent-bus/
```

## Status note

Code complete in the working tree, NOT committed. Gates (reviewer, security) dispatched. Two things the orchestrator must resolve before commit: (a) cmd/agent-bus/main.go simultaneously carries another agent's uncommitted RELAY-24 peer-store wiring, so this task's main.go edits cannot be committed by pathspec without sweeping that work in; (b) cmd/agent-busctl/enrol_test.go (TestCLIEnrolEndToEnd) now fails because it enrols anonymously against the real server binary -- that file is OUTSIDE this task's boundary and needs either a boundary widening or a paired task.

## Description

P0 LIVE VULNERABILITY. Task 05a5216d (INVITE-GATE, closed/done) landed ONLY invite *redemption* -- its own test_summary admits: "Invite redemption works and is atomic; the gate is NOT enforced ... still accepts enrolment with no invite (invite_required=false)". No task in the backlog owns the ENFORCEMENT half until now.

THE DoS: ~4096 anonymous `POST /v1/enroll` requests PERMANENTLY brick the bus. `DefaultMaxRosterEntries = 4096` (internal/auth/service.go:31), nothing ever frees a slot (internal/auth/errors.go:45 -- comment states roster "FAILS CLOSED, never evicts"), there is no `/v1/leave`, and ids are never reused (invariant 1) so a slot can never be reclaimed by reusing an id. This is NOT an OOM -- memory stays bounded, which is exactly why it is easy to miss in monitoring. It is silent, permanent capacity exhaustion: once the roster fills with anonymous junk, no legitimate agent can ever enrol again for the lifetime of that data directory.

NOT A FLAG FLIP: `InviteRequired: false` at internal/httpapi/discovery.go:304 is only ADVERTISING (a /v1/info field), not enforcement. `auth.Service.Enrol` currently says, in terms, that `req.Invite == nil` is an UN-INVITED enrolment and is STILL ACCEPTED. The enforcement code path -- rejecting an enrolment that carries no valid invite -- does not exist yet and must be written from scratch in internal/auth/service.go and internal/httpapi/auth.go.

INVARIANTS TO READ IN FULL BEFORE CODING (INVARIANTS.md):
- Invariant 3: enrolment is invite-only; invites are single-use, expiring, revocable, and are the ONLY way onto the bus.
- Invariant 1: the server is authoritative on every id; ids are never reused, including across restarts -- so a roster slot can never be reclaimed by reusing an id, which is precisely why unenforced anonymous enrolment is unrecoverable exhaustion rather than a transient condition.
- Invariant 10: duplicate detection/idempotency -- re-presenting an already-spent invite is NOT automatically a disconnect. Same idempotency key + same payload is a legitimate retry (return the original result, do not re-apply, do not disconnect); same key + different payload is a protocol violation (reject + log, do not disconnect). Do not conflate "invite missing/invalid" with a disconnect-worthy replay.

ACCEPTANCE CRITERIA:
- A RED-first test that reproduces the DoS: fill the roster via anonymous (no-invite) enrolments, assert further enrolment is refused once the gate lands, and assert no slot is ever reclaimable (ids/slots are never reused per invariant 1). Confirm the test is RED against current code before the fix, then GREEN after.
- The anonymous (no-invite) enrolment path is made unreachable -- `req.Invite == nil` must be rejected, not silently accepted.
- Every guard is mutation-tested to fail (go red) ALONE -- i.e. each individual check that enforces the gate, if reverted/removed on its own, must cause a specific test to fail. No guard should be redundant-looking such that removing it alone leaves everything green.
- Exercised through the COMPILED binaries end-to-end (never curl, never a scripts/bus-*.sh wrapper -- those are retired): mint an invite via `agent-bus invite mint` (bus stopped, see operational note below), start the bus, enrol WITH the invite via the CLI and confirm success, then attempt to enrol WITHOUT an invite via the CLI and confirm it is refused. Run this against a throwaway `mktemp -d` data directory -- NEVER the tracked `data/` dir and NEVER the live bus on 127.0.0.1:8080.

proof_cmd: TBD by the implementer -- a task with no proof_cmd may not be completed; this placeholder must be replaced with a real, previously-RED, verified command before completion.

CURRENT BLOCKER (needs orchestrator sequencing): implementation requires editing internal/httpapi/auth.go, which is CONCURRENTLY HELD -- staged/uncommitted -- by the live MTLS-CROSSCHECK work. Do not start until MTLS-CROSSCHECK's changes to that file have landed (or the two are explicitly sequenced/coordinated by the orchestrator) to avoid a lost-update collision on the same file.

OUT-OF-BOUNDARY FOLLOW-ON EDITS REQUIRED IN THE SAME INCREMENT (do not lose these):
(a) cmd/agent-bus/main.go:645-651 logs at startup "ENROLMENT IS NOT GATED ... enrolment_invite_required=false" -- this becomes FALSE the moment the gate lands and the log line must be corrected in the same change, or it actively lies to operators.
(b) cmd/agent-bus/main.go:856-859 has comments claiming Invites "does NOT require one" -- these must be corrected to match enforced behaviour.

OPERATIONAL CONSEQUENCE TO DOCUMENT (CONTRACTS-CLI.md / AGENT_PROTOCOL.md as appropriate): `agent-bus invite mint` requires the bus to be STOPPED -- it takes the data directory's exclusive dirlock, which a running bus holds (cmd/agent-bus/invite.go:17-21). So once the gate is ON, admitting a NEW agent requires a stop -> mint -> start cycle. Already-enrolled agents are unaffected by this (their sessions/roster entries persist across the cycle).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVITE-GATE](../INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [MTLS-CROSSCHECK](../../MTLS/MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (in_progress)
- [RELAY-24](../../RELAY/RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
