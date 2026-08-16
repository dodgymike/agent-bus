# AUTH-1-FU-ACTIVECAP-DOCS: document the per-agent ACTIVE-session cap in CONTRACTS-HTTP.md + AGENT_PROTOCOL.md

| Field | Value |
| --- | --- |
| Public id | `27a811c9-5942-4341-b5fd-67c12a2547d0` |
| Key | _(null in the export)_ |
| Epic | [AUTH](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:03:48.650620+00:00 |
| Updated | 2026-08-02T21:03:48.650620+00:00 |
| Completed | — |

## Proof command

```sh
grep -q 'a PROVEN identity, not an attacker-supplied victim identifier' CONTRACTS*.md && grep -q 'MaxActiveSessionsPerAgent' CONTRACTS*.md && grep -q '503' AGENT_PROTOCOL.md && echo DOCS_OK
```

## Description

The code shipped in AUTH-1-FU-ACTIVECAP; the docs could not, because CONTRACTS*.md and AGENT_PROTOCOL.md were owned by concurrent agents during that loop. This is tracked debt, deliberately incurred. The proof globs CONTRACTS*.md so it survives the CONTRACTS split. Four edits, all in CONTRACTS-HTTP.md unless stated:
(a) NEW row in the admission-control caps table (~line 122-124, columns Cap | Default | Behaviour at the cap):
| `MaxActiveSessionsPerAgent` | 32 | **Fails closed**: `POST /v1/session/complete` returns 503 (`ErrCapacity`, `Retry-After: 5`) when the transition from pending to active would take the agent past its cap of concurrently ACTIVE sessions. Enforced in `CompleteSession`, after the Ed25519 signature verifies and after the already-active early return, so re-completing an already-active session (invariant 10's legitimate retry) is never refused. Never evicts, and a refusal mutates nothing -- the pending challenge survives and can complete once one of the agent's OWN sessions expires (up to `SessionLifetime`, 1 hour, away). This cap is keyed on agent id and is safe to key that way *only here*: unlike `BeginSession`'s `agent_id`, which is an attacker-supplied victim identifier, the key on this route is a PROVEN identity, not an attacker-supplied victim identifier -- an entry only enters an agent's bucket behind a valid Ed25519 signature made with that agent's own enrolment private key, so a flooder can only fill its own bucket. See `MaxSessions` for the residual risk this narrows but does not close. |
(b) FIX the `MaxSessions` row (line 124). Its tail is now FALSE -- it still says "nothing caps active sessions per agent" and that the gap "is filed as AUTH-1-FU-ACTIVECAP". Replace those sentences with: MaxActiveSessionsPerAgent now bounds how much of the hour-long outage a SINGLE enrolment can hold; at the 16384/32 defaults filling the table with active entries takes ceil(16384/32) = 512 DISTINCT enrolments rather than one agent. Be honest per the security audit: that is only +1.6% attacker cost (33280 vs 32769 requests) and the sustained hold is UNCHANGED at ~9.1 req/s, because Enrol accepts duplicate public keys and names so the 512 enrolments come from ONE keypair. 512 is 12.5% of MaxRosterEntries, so the roster bound is NOT binding. The cap bounds blast radius per identity; it does not make the table unfillable. Root fix is the invite-only enrolment EPIC (0b43393e-556b-409a-938a-846be2fb4a75); partial mitigation AUTH-1-FU-RATELIMIT (42670f8b).
(c) NEW route-table row (match the format of lines 21-28):
| `POST` | `/v1/session/complete` | none | 503 | the completing agent already holds `MaxActiveSessionsPerAgent` (default 32) ACTIVE sessions; `Retry-After: 5`. Never evicts -- the refusal leaves the pending challenge and every session the agent already holds untouched |
(d) AMEND the "There is deliberately no per-agent pending-challenge cap" paragraph (~line 126) so it reads as a deliberate ASYMMETRY, not a blanket ban -- a future reader must not cite it to delete the active cap. Append: "**This is a statement about the PENDING side only.** `MaxActiveSessionsPerAgent` (AUTH-1-FU-ACTIVECAP) IS a per-agent cap, on the ACTIVE side of `CompleteSession`, and it is safe precisely because that key is a proven identity rather than an attacker-supplied one -- do not cite this paragraph to justify removing it."
(e) AGENT_PROTOCOL.md (~line 110, after the 401 paragraph): a `POST /v1/session/complete` can now return 503 where before it could not. Correct client behaviour: honour `Retry-After`, do NOT re-enrol and do NOT treat it as an auth failure. Retry the SAME pending challenge only while it is still within `ChallengeTTL` (2 minutes); after that the challenge is gone and a fresh `POST /v1/session/begin` is required. A cap of 32 genuinely exhausted usually means the agent is leaking sessions rather than refreshing at `refresh_after_seconds`.
(f) DECISIONS.md entry (dated 2026-08-02), text supplied by feature-runner: "Per-agent ACTIVE-session cap: refuse-new, never evict, default 32. An agent-id-keyed bucket is a lockout primitive on the unauthenticated BeginSession route (AUTH-1-FU-PENDINGCAP removed one) but is SAFE on CompleteSession, because an entry only enters a bucket behind a valid Ed25519 signature with that agent's enrolment private key -- a proven identity, so a flooder can only fill its own bucket and a refusal is self-inflicted. Refuse over evict, deliberately: evicting an agent's own oldest session would let a thief who compromised its key destroy the legitimate holder's LIVE sessions on demand. 32 = ~16x the compliant steady state of 2 concurrent sessions (a client refreshes at 75% of lifetime, so old and new overlap), bounding one identity to 0.2% of the session table."

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../../INVITE/EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [AUTH-1-FU-ACTIVECAP](../AUTH-1-FU-ACTIVECAP--2d92b699/task.md) — AUTH-1-FU-ACTIVECAP: cap ACTIVE sessions per agent -- the one place an agent-id-keyed cap… (done)
- [AUTH-1-FU-PENDINGCAP](../AUTH-1-FU-PENDINGCAP--687ad8c9/task.md) — AUTH-1-FU-PENDINGCAP: MaxPendingPerAgent is a lockout primitive, not a defence -- rekey o… (done)
- [AUTH-1-FU-RATELIMIT](../AUTH-1-FU-RATELIMIT--42670f8b/task.md) — AUTH-1-FU-RATELIMIT: per-source rate limiting on the three unauthenticated auth routes (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1-FU-ACTIVECAP](../AUTH-1-FU-ACTIVECAP--2d92b699/task.md) — AUTH-1-FU-ACTIVECAP: cap ACTIVE sessions per agent -- the one place an agent-id-keyed cap… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
