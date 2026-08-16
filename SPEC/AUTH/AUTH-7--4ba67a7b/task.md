# AUTH-7: Operator/admin can clear one agent's active sessions without restarting the bus

| Field | Value |
| --- | --- |
| Public id | `4ba67a7b-2253-4dfe-a99e-1b32e3b76bcc` |
| Key | AUTH-7 |
| Epic | [AUTH](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T07:31:27.492916+00:00 |
| Updated | 2026-08-16T07:31:27.492916+00:00 |
| Completed | — |

## Description

OPERATOR-REQUESTED 2026-08-16.

An operator/admin MUST be able to clear the active sessions held by one agent,
without restarting the bus.

# Why this is needed — observed in production, not hypothetical

2026-08-15, live bus bus-matv6xu7ronvdq7o: agent `elastic-agent-1` accumulated
32 active sessions and was then refused on every subsequent handshake:

  auth request refused at a capacity limit
    op="session complete"
    err="agent \"bus-matv6xu7ronvdq7o.elastic-agent-1\" holds 32 active
         sessions, at the per-agent limit of 32; one of its OWN sessions must
         expire before another can be established, and none is evicted"

Measured: 12 x HTTP 200 then 32 x HTTP 503 on /v1/session/complete. The agent was
locked out of its own identity and could NOT recover by any action of its own.

The ONLY remedies available today are:
  1. wait up to SessionLifetime (1h) for its own sessions to age out
  2. restart the bus, which destroys EVERY agent's sessions

Remedy 2 was used four times on 2026-08-15. It is a blunt instrument: it
punishes all six agents on the bus to unstick one, and it masks the underlying
client defect by resetting the count.

# Scope

Add an operator-only capability to clear the sessions of ONE named agent:
  - a server-side route
  - a CLI subcommand (invariant 7 — a capability without a subcommand is the
    missing half of the task)
  - an AGENT_PROTOCOL.md / CONTRACTS-*.md entry in the SAME task

# The hard design constraint — read internal/auth/service.go before designing

`internal/auth/service.go:40-61` documents WHY nothing evicts today, and it is
NOT an oversight:

  "Nor does it help against a COMPROMISED private key: whoever holds it can
   occupy all maxActiveSessionsPerAgent slots and, because nothing evicts, deny
   the legitimate holder a NEW session for up to SessionLifetime. That is still
   a win ... and the remedy is revocation (AUTH-4), not eviction here: evicting
   on a full bucket would let the thief destroy the victim's live sessions on
   demand."

So AUTOMATIC eviction is REFUSED by an existing recorded decision. This task must
NOT reintroduce it. What is wanted is an OPERATOR-TRIGGERED clear — a human or
admin principal deciding, not the bus deciding under pressure. Preserve that
distinction explicitly in the design and in DECISIONS.md.

# BLOCKER — there is no operator principal yet

agent-bus currently has NO admin/operator identity. Every authenticated caller is
an enrolled agent. An "operator-only" route therefore has nothing to authorise
against, and MUST NOT be built as "any authenticated agent may clear any other
agent's sessions" — that is a trivial cross-agent denial of service and strictly
worse than the problem it solves.

This is the SAME blocker that holds the admin arm of CONV and INVMINT-2. Either
this task waits on an operator-principal task, or it ships in the one form that
needs no principal: an offline `agent-bus sessions clear <agent-id>` subcommand
taking the data directory's exclusive lock, exactly like `invite mint`. Note that
the offline form requires stopping the bus, which is only marginally better than
the restart it replaces — so its value is mostly in being SCOPED to one agent.
Decide, and record the decision.

# Acceptance
  - an operator can clear ONE agent's sessions without restarting the bus, OR
    the offline-only form ships with its limitation documented
  - no path lets agent A clear agent B's sessions
  - clearing is LOUD in the log: who cleared, whose sessions, how many
  - a test proves a cleared agent can immediately establish a new session
  - a test proves a non-operator caller is REFUSED

# Related
  - AUTH-8 (deep dive: usability vs abuse-protection balance) — that task should
    inform the numbers here; do not tune the cap in THIS task
  - the client-side half shipped 2026-08-16: --persist-session collapses a
    shell-out agent from one session per command to one session, which REDUCES
    the frequency of this failure but does not remove the need for the remedy

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-4](../AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (todo)
- [AUTH-8](../AUTH-8--b65948b7/task.md) — AUTH-8: DEEP DIVE — the balance between usability and security / abuse protection (todo)
- [INVMINT-2](../../INVMINT/INVMINT-2--ef18b37a/task.md) — INVMINT-2: introduce an OPERATOR PRINCIPAL — a bus-scoped, non-agent identity that can au… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [6fd8c8c5-b653-4d35-af83-8c9d1b82dedd](../../PROCESS/Correct-stale-wave-label-AUTH-7-to-its-real-task-identit--6fd8c8c5/task.md) — Correct stale wave label AUTH-7 to its real task identity across code and docs (todo)
- [AUTH-10](../AUTH-10--37993b49/task.md) — AUTH-10: An operator/admin principal -- the missing noun blocking AUTH-7, INVMINT and CON… (todo)
- [AUTH-9](../AUTH-9--483ee09b/task.md) — AUTH-9: Opt-in session persistence (--persist-session) + agent-busctl session logout (done)
- [DOCS-10](../../DOCS/DOCS-10--d6c84ff8/task.md) — DOCS-10: \`client\` package documents fail-closed verification while shipping fail-open (todo)
- [DOCS-11](../../DOCS/DOCS-11--a434830e/task.md) — DOCS-11: Invite revocation is documented in three places and implemented in none (todo)
- [DOCS-12](../../DOCS/DOCS-12--7b363ccf/task.md) — DOCS-12: 8 error remedies name \`agent-busctl keygen\` / \`trust\`, which do not exist (todo)
- [DOCS-13](../../DOCS/DOCS-13--8ce01598/task.md) — DOCS-13: \`INVARIANTS.md\` truth pass — 8 false factual claims in the file agents must read… (todo)
- [DOCS-14](../../DOCS/DOCS-14--86741a89/task.md) — DOCS-14: \`CLAUDE.md\`/\`AGENTS.md\`: delete the false \`crypto/ecdh\` toolchain rationale; fix… (todo)
- [DOCS-15](../../DOCS/DOCS-15--e718e0c0/task.md) — DOCS-15: \`AGENTS.md\` writes fabricated model ids (\`Codex-opus-5\`) into the cost audit tra… (todo)
- [DOCS-16](../../DOCS/DOCS-16--57933ce7/task.md) — DOCS-16: \`PROTOCOL.md\`'s on-disk version registry omits versions 5, 6 and 7 — a live rese… (todo)
- [DOCS-17](../../DOCS/DOCS-17--a35d1ec1/task.md) — DOCS-17: Session per-agent cap (32, no eviction) is documented as not existing — caused a… (todo)
- [DOCS-18](../../DOCS/DOCS-18--5b3f4886/task.md) — DOCS-18: Retire two standing directives that outlived their premise and now FORBID the fix (todo)
- [DOCS-19](../../DOCS/DOCS-19--9d8ff93b/task.md) — DOCS-19: Durability inverted: \`internal/auth/service.go:502\` says main injects the MEMORY… (todo)
- [DOCS-20](../../DOCS/DOCS-20--55d5bac2/task.md) — DOCS-20: Mechanical stale-claim detector — likely to MERGE with the in-flight \`scripts/do… (todo)
- [DOCS-21](../../DOCS/DOCS-21--cdf8660c/task.md) — DOCS-21: \`CONTRACTS-CLI.md\` claims a "mechanically enforced" import guard that nothing ru… (todo)
- [DOCS-22](../../DOCS/DOCS-22--2f8ae959/task.md) — DOCS-22: The four agent ENTRY POINTS the invite gate missed — \`README\` Quickstart, \`agent… (todo)
- [DOCS-23](../../DOCS/DOCS-23--c9a51528/task.md) — DOCS-23: \`agent-busctl broadcast --help\` never says the route is refused (501) (todo)
- [DOCS-24](../../DOCS/DOCS-24--4aaf2803/task.md) — DOCS-24: \`client/transport.go:429-430\`: the 403 remedy tells an agent to retry a refusal… (todo)
- [DOCS-25](../../DOCS/DOCS-25--9c894053/task.md) — DOCS-25: \`CONTRACTS-AGENT.md\` documents the log-scrape that \`bus-serve.sh\` deliberately r… (todo)
- [DOCS-26](../../DOCS/DOCS-26--fb39c79d/task.md) — DOCS-26: \`docs/THREE-BUS-DOCKER.md\` tells the operator to ignore \`fed-smoke.sh\`, mint an… (todo)
- [DOCS-27](../../DOCS/DOCS-27--ec19df4e/task.md) — DOCS-27: \`AGENT_PROTOCOL.md\`: \`client-cert\` undocumented (invariant-7 gap), TOC lists 10… (todo)
- [DOCS-28](../../DOCS/DOCS-28--7f1030b7/task.md) — DOCS-28: \`docs/comms\` self-audit numbers disagree with the CSVs, and \`LABELLING-KEY.md\`'s… (todo)
- [DOCS-29](../../DOCS/DOCS-29--7b0e66e8/task.md) — DOCS-29: Investigate \`TestWALRepairDoesNotReissueDiscardedIndex\` — a P0's recorded \`proof… (todo)
- [DOCS-5](../../DOCS/DOCS-5--051a9829/task.md) — DOCS-5: \`/v1/discovery\` limitation 5 is false on the wire: cross-bus relay IS served (todo)
- [DOCS-6](../../DOCS/DOCS-6--76879ad1/task.md) — DOCS-6: README is unusable: quickstart 403s, "what works today" curls a TLS port in plain… (todo)
- [DOCS-7](../../DOCS/DOCS-7--a98ffca6/task.md) — DOCS-7: Doc-truth sweep: enrolment is invite-gated (11 passages, 5 files) (todo)
- [DOCS-8](../../DOCS/DOCS-8--1f955f09/task.md) — DOCS-8: Doc-truth sweep: relay is mounted, live and imported (17 passages) — incl. 3 \`MUS… (todo)
- [DOCS-9](../../DOCS/DOCS-9--873417cb/task.md) — DOCS-9: P0-adjacent: reserve a relay wire-protocol version — the envelope is on the wire… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
