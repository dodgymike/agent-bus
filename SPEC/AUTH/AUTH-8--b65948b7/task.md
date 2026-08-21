# AUTH-8: DEEP DIVE — the balance between usability and security / abuse protection

| Field | Value |
| --- | --- |
| Public id | `b65948b7-cd1d-4728-9f8e-10a76a1e50a3` |
| Key | AUTH-8 |
| Epic | [AUTH](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T07:31:27.747616+00:00 |
| Updated | 2026-08-16T07:31:27.747616+00:00 |
| Completed | — |

## Description

OPERATOR-REQUESTED 2026-08-16. DEEP DIVE — analysis, not implementation.

Examine the balance between USABILITY and SECURITY / ABUSE PROTECTION across
agent-bus, and produce a written recommendation.

# The trigger

On 2026-08-15 a healthy, non-malicious agent locked ITSELF out of its own
identity for up to an hour, by working normally. The abuse protection worked
exactly as designed and the outcome was still wrong. That is the shape of
problem this deep dive is about: limits that are individually defensible and
collectively hostile to the mandated usage pattern.

The specific collision:
  - invariant 7 mandates agents drive the bus by SHELLING OUT to the CLI
  - each process is short-lived, so its in-memory session cache dies with it
  - internal/auth: SessionLifetime=1h, DefaultMaxActiveSessionsPerAgent=32,
    and NOTHING evicts
  => an agent exceeding ~1 command / 2 minutes exhausts its own cap

Note the sizing comment at internal/auth/service.go:46 — "The steady state for a
well-behaved agent is TWO concurrent sessions" — is correct for a LONG-LIVED
embedding client and wrong for the shell-out shape invariant 7 requires. The cap
was sized against an assumption the mandated usage pattern does not meet. That
mismatch, not the number 32, is the interesting finding.

# Scope — audit EVERY limit, not just sessions

Inventory each protective limit, and for each: what it defends, what it costs a
legitimate user, whether its sizing assumption still matches how the system is
actually used, and how a user who trips it RECOVERS.

At least:
  - DefaultMaxActiveSessionsPerAgent (32), SessionLifetime (1h), no eviction
  - MaxSessions (16384), MaxRosterEntries
  - idempotency store: MaxIdempotencyEntries and the per-bus bucketing landed
    at 72d6f5d (denominator 32767 -> 2)
  - invite TTL (24h default / 168h max), single-use, and the ABSENCE of revoke
  - MaxBusPath (64) and maxOnwardBusesPerMessage (8)
  - relay peerAdmission, which charges only UnderPressure
  - rate limiting: AUTH-1-FU-RATELIMIT is still OPEN — note what its absence
    shifts onto the other limits

# The specific question to answer for each

**How does a legitimate user who trips this limit get out?** The session case
had NO self-service recovery: no error the client could act on, no way to
release a session, and the only remedies were "wait an hour" or "restart the
bus and punish everyone". A limit with no recovery path is a trap, and that is
probably the single most useful lens for this whole review.

# Also examine

  - LOUDNESS. Every refusal above was logged clearly server-side and was
    INVISIBLE to the agent that caused it. Invariant 6 requires discards be
    logged "loudly and specifically" — consider whether an equivalent rule
    should apply to REFUSALS reaching the party who can act on them.
  - The 2026-08-16 --persist-session decision, which traded a bearer token at
    rest for survivable ergonomics. Was that trade right, and where ELSE does
    the same shape appear?
  - Whether any limit is currently doing NOTHING (see peerAdmission charging
    only under pressure, where reaching pressure IS the attack).

# Deliverable

A written analysis with a prioritised list of recommendations, each as a
proposed task. DO NOT change limits in this task — the point is the reasoning.
Anything acted on becomes its own task with its own gates.

Record conclusions in DECISIONS.md. Where a limit is judged correct AS IS, say
so explicitly and why — a re-affirmation is as valuable as a change and stops
the question being reopened.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1-FU-RATELIMIT](../AUTH-1-FU-RATELIMIT--42670f8b/task.md) — AUTH-1-FU-RATELIMIT: per-source rate limiting on the three unauthenticated auth routes (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-10-WIRING](../AUTH-10-WIRING--b11ef24c/task.md) — AUTH-10-WIRING: wire the operator principal into cmd/agent-bus/main.go — until this lands… (in_progress)
- [AUTH-7](../AUTH-7--4ba67a7b/task.md) — AUTH-7: Operator/admin can clear one agent's active sessions without restarting the bus (todo)
- [AUTH-9](../AUTH-9--483ee09b/task.md) — AUTH-9: Opt-in session persistence (--persist-session) + agent-busctl session logout (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
