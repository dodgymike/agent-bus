# AUTH-1-FU-ACTIVECAP: cap ACTIVE sessions per agent -- the one place an agent-id-keyed cap is SAFE

| Field | Value |
| --- | --- |
| Public id | `2d92b699-818a-4fd0-bbb7-76c06449756b` |
| Key | _(null in the export)_ |
| Epic | [AUTH](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T19:56:26.573739+00:00 |
| Updated | 2026-08-02T23:47:55.938483+00:00 |
| Completed | 2026-08-02T23:47:55.938465+00:00 |

## Proof command

```sh
go test -race -run TestSessionActiveCap ./internal/auth
```

## Status note

CODE-COMPLETE / UNCOMMITTED. internal/auth only: per-agent ACTIVE-session cap (DefaultMaxActiveSessionsPerAgent=32, Options.MaxActiveSessionsPerAgent), enforced in CompleteSession after signature verification. reviewer PASS, security PASS (no P0/P1). proof-check verdict=PASS tests_run=13 for 'go test -race -run TestSessionActiveCap ./internal/auth'; verdict=PASS tests_run=99 for the whole package. Staged, awaiting orchestrator commit. Docs deferred to AUTH-1-FU-ACTIVECAP-DOCS (CONTRACTS*.md + AGENT_PROTOCOL.md were owned by concurrent agents this loop). Orchestrator: complete with commit_sha once committed.

## Description

Discovered by the security gate on AUTH-1-FU-PENDINGCAP. Nothing caps ACTIVE sessions per agent, and enrolment is itself unauthenticated, so an attacker that enrols its own agent can complete MaxSessions handshakes and fill the session table with ACTIVE entries. Those are reclaimed only after SessionLifetime (1 hour), not after ChallengeTTL (2 minutes), so this costs roughly 9 req/s to hold rather than ~137, and the resulting denial of NEW session establishment outlives the flood by an hour. Verified empirically by the security agent: advancing the clock past ChallengeTTL reclaims nothing. This is PRE-EXISTING and NOT a regression from AUTH-1-FU-PENDINGCAP -- the cap removed there counted only SessionPending entries and never protected against this. THE LOAD-BEARING INSIGHT, and the reason this is not a repeat of the mistake AUTH-1-FU-PENDINGCAP just fixed: unlike a PENDING-challenge cap, an ACTIVE-session cap keyed on agent id is SAFE, because an active session can only be created by proving possession of that agent private key. The key is a PROVEN identity, not an attacker-supplied victim identifier, so a flooder cannot make its sessions land in a victim bucket. Must be argued explicitly in the implementation comment so the distinction is not lost, and must ship with an adversarial test in the shape of TestSessionBeginNoVictimLockout. Note this is referenced by name from internal/auth/session.go BeginSession and from CONTRACTS.md, so the key AUTH-1-FU-ACTIVECAP must not change.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1-FU-ACTIVECAP-DOCS](../AUTH-1-FU-ACTIVECAP-DOCS--27a811c9/task.md) — AUTH-1-FU-ACTIVECAP-DOCS: document the per-agent ACTIVE-session cap in CONTRACTS-HTTP.md… (todo)
- [AUTH-1-FU-PENDINGCAP](../AUTH-1-FU-PENDINGCAP--687ad8c9/task.md) — AUTH-1-FU-PENDINGCAP: MaxPendingPerAgent is a lockout primitive, not a defence -- rekey o… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0b43393e-556b-409a-938a-846be2fb4a75](../../INVITE/EPIC-invite-only-enrolment-the-root-fix-for-the-pre-auth--0b43393e/task.md) — EPIC: invite-only enrolment -- the root fix for the pre-auth attack family (needs planner… (superseded)
- [AUTH-1-FU-ACTIVECAP-DOCS](../AUTH-1-FU-ACTIVECAP-DOCS--27a811c9/task.md) — AUTH-1-FU-ACTIVECAP-DOCS: document the per-agent ACTIVE-session cap in CONTRACTS-HTTP.md… (todo)
- [AUTH-1-FU-ACTIVECAP-RETRYAFTER](../AUTH-1-FU-ACTIVECAP-RETRYAFTER--03a8512b/task.md) — AUTH-1-FU-ACTIVECAP-RETRYAFTER: a per-agent cap 503 tells the client the wrong thing and… (todo)
- [AUTH-1-FU-SESSIONSCALE](../AUTH-1-FU-SESSIONSCALE--067b80cf/task.md) — AUTH-1-FU-SESSIONSCALE: session-table O(n) scans and refuse-not-evict policy cause CPU/lo… (todo)
- [CONTRACTS-SPLIT](../../DOCS/CONTRACTS-SPLIT--360a2679/task.md) — CONTRACTS-SPLIT: split CONTRACTS.md into per-plane files (pure move) + retarget every pro… (done)
- [ac4f9c2b-5460-4e83-997d-0e433194752f](../Enrol-accepts-a-duplicate-enrolment-public-key-one-keypa--ac4f9c2b/task.md) — Enrol accepts a duplicate enrolment public key -- one keypair can hold unlimited agent ids (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
