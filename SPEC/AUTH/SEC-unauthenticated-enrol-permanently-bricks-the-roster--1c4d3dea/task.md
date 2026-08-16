# SEC: unauthenticated enrol permanently bricks the roster -- 4096-cap fails closed forever, no eviction, survives restart

| Field | Value |
| --- | --- |
| Public id | `1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa` |
| Key | _(null in the export)_ |
| Epic | [AUTH](../epic.md) |
| Status | in_progress |
| Priority | P0 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-07T21:25:59.907505+00:00 |
| Updated | 2026-08-14T20:59:08.840552+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestRosterDoS ./internal/auth
```

## Status note

EVIDENCE LANDED, HOLE OPEN -- do not read this task as fixed. internal/auth/rosterdos_test.go (new, code-only, not yet committed) reproduces the finding at HEAD and PASSES BY DESIGN: it characterises the live vulnerability and is written to go RED when the vulnerability is fixed. Confirmed at HEAD by an independent security gate (PASS): POST /v1/enroll is reachable with no invite, no session and no client certificate; there is no rate limit; DefaultMaxRosterEntries=4096 fails closed with ErrCapacity; there is NO removal path (no /v1/leave, no Remove/Delete/Evict on auth.Roster, MemoryRoster or WALRoster, no TTL); and the roster is durable so restart REPLAYS the attacker entries. The root fix is invariant 3 (invite-only enrolment) and is ESCALATED TO THE OPERATOR, not taken unilaterally: it is a security-posture change with nine agents live on the bus, and it is NOT a one-line flip -- internal/httpapi/discovery.go:304's InviteRequired:false is only ADVERTISING, and the enforcement code does not exist (Service.Enrol documents that req.Invite==nil is STILL ACCEPTED). Remediation is tracked by INVITE-GATE's unwritten enforcement half (05a5216d, closed having landed only atomic redemption) and AUTH-ROSTER-RECLAIM (b418638c, P0, lands in cmd/).

## Description

Found independently twice within the same hour (an external security-testing agent reading the route table, and the AUTH-3 security-gate review) -- corroboration is why this is filed as established rather than a claim.

Compose three individually-reasonable facts: (1) POST /v1/enroll is unauthenticated (invite gate not yet landed -- INVITE-GATE). (2) DefaultMaxRosterEntries = 4096 (internal/auth/service.go:30) and enrolment fails CLOSED at the cap. (3) The roster is durable across restart (internal/auth roster WAL replay, AUTH-3) and there is NO route or method that removes a roster entry -- verified: no /v1/leave, no Remove/Delete/Evict/Leave on the roster. AGENT_PROTOCOL.md confirms bus logout is local-only (server_notified: false).

Consequence: 4096 unauthenticated POSTs -- no key material, no invite, no session, no client cert -- and the roster is full FOREVER. Not until a TTL; not until a restart, because restart REPLAYS the durable roster and restores the attacker entries. The only remedy today is an operator deleting the whole data directory, destroying every legitimate agent id and the message history along with it.

The security gate's independent wording: durable enrolment turned a TRANSIENT DoS into a PERMANENT one -- no /v1/leave, no delete on WALRoster, first-write-wins, WAL never compacts.

Why this is filable rather than a restatement of "INVITE-GATE is unfinished": internal/auth/session.go (~line 244-260) carries a careful availability analysis of the session-table flood, concluding it is untargeted, unamplified and SELF-HEALING. That write-up is the canonical analysis of what an unauthenticated caller can cost the bus -- and it stops one resource short. The roster version is untargeted and unamplified too, but it is NOT self-healing, not TTL-bounded, and survives reboot. A permanent cost sitting undocumented beside a careful analysis of a transient one is worse than an undocumented hole, because a reader reasonably concludes the analysis is complete.

SEVERITY: the reporter rated this P0 and explicitly declined to inflate it, noting the listener is loopback-only (127.0.0.1:18080 by default) so the reachable attacker set today is a local process. spec-keeper judgement recorded here: rated P0 anyway, on impact rather than current reachability -- the damage is irreversible (destroys the data directory to recover, which also destroys legitimate history) and invariant 11's own text anticipates a bus deliberately exposed on a real interface as a real, intended deployment target, not a hypothetical; a permanent-DoS primitive should not wait for that exposure to be reprioritised. Track reachability separately: if it becomes reachable from a non-loopback interface before this and INVITE-GATE both ship, that is an immediate re-escalation trigger, not a new finding.

This is a TRACKING/UMBRELLA task for the finding. Three concrete follow-ups are filed separately: extending session.go's availability analysis (doc), an operator-side roster-reclamation escape hatch independent of INVITE-GATE, and a priority note on INVITE-GATE (already P0) plus AUTH-4 (leave/revocation, P1) as the in-protocol remedy once auth exists.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-3](../AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [AUTH-4](../AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (todo)
- [AUTH-ROSTER-RECLAIM](../AUTH-ROSTER-RECLAIM--b418638c/task.md) — AUTH-ROSTER-RECLAIM: operator-side "agent-bus roster remove &lt;id&gt;" escape hatch -- filesys… (todo)
- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-3-FU-ROSTERDOS-DOCS](../AUTH-3-FU-ROSTERDOS-DOCS--d5197abb/task.md) — AUTH-3-FU-ROSTERDOS-DOCS: extend session.go availability analysis (untargeted/unamplified… (todo)
- [AUTH-ROSTER-RECLAIM](../AUTH-ROSTER-RECLAIM--b418638c/task.md) — AUTH-ROSTER-RECLAIM: operator-side "agent-bus roster remove &lt;id&gt;" escape hatch -- filesys… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
