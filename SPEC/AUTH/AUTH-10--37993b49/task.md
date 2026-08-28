# AUTH-10: An operator/admin principal -- the missing noun blocking AUTH-7, INVMINT and CONV-AUTHZ-ADMIN

| Field | Value |
| --- | --- |
| Public id | `37993b49-e317-4dde-bcf5-abd22c97648d` |
| Key | AUTH-10 |
| Epic | [AUTH](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T09:09:20.872778+00:00 |
| Updated | 2026-08-22T21:59:59.700515+00:00 |
| Completed | 2026-08-22T21:59:59.700498+00:00 |

## Proof command

```sh
go test -race -count=1 -run 'TestOperatorPrincipal' ./internal/auth
```

## Status note

CODE-COMPLETE, NOT LIVE — 2026-08-16, feature-runner. The operator principal, its durable registry, its session table and the authorization check are implemented, gated and proven; reviewer and security both re-verified (security PASS; reviewer PASS on everything except the out-of-boundary wiring). It is NOT runnable: cmd/agent-bus/main.go was held by RELAY-48 for the whole run and is outside this agent's file ownership, so (a) `agent-bus operator ...` is not dispatched from argv — including `operator revoke`, the design's only revocation mechanism — and (b) auth.OperatorRecordKind is not in the server's applier map, so a server replay passes operator records over in silence (an invariant-6 shape, present by omission). Both are carried by AUTH-10-WIRING (P0, b11ef24c-3791-456f-a45f-1223cce5b50b) and are disclosed in CONTRACTS-CLI.md, CONTRACTS-ONDISK.md and AGENT_PROTOCOL.md. AUTH-10 may be completed only once the five design answers are recorded in DECISIONS.md (proposed text handed to the orchestrator; DECISIONS.md is contended and was outside this agent's boundary) — and it should be completed as CODE-ONLY, with AUTH-10-WIRING carrying the observable behaviour.

## Description

OPERATOR-REQUESTED 2026-08-16: "Yes, that's what I want to tackle next."

Introduce an OPERATOR/ADMIN PRINCIPAL: an authenticated identity that is not an enrolled agent, that
operator-only capabilities can be authorised against.

# Why this is the critical path

agent-bus today has NO admin identity. Every authenticated caller is an enrolled agent. So every
"an operator can do X" feature has been filed, designed, and then stalled at the same missing noun.
This ONE gap blocks FOUR pieces of work:

  - AUTH-7 -- an operator clearing one agent's active sessions without restarting the bus. Filed off
    a live incident where an agent locked itself out for an hour and the only remedy was restarting
    the bus, punishing all six agents to unstick one.
  - INVMINT -- online invite mint. The operator overruled ADMIN D6 on 2026-08-16 to allow it. It
    CANNOT ship without this: "any authenticated agent may mint an invite" is a self-service
    enrolment hole, strictly worse than the bus-stop it replaces.
  - INVMINT-2 -- as already recorded.
  - CONV-AUTHZ-ADMIN -- "only the channel creator may change the recipient list, OR AN ADMIN". The
    operator answered that question; the admin arm cannot be built.

It is not four problems. It is one.

# The hard constraints -- read INVARIANTS.md 3 and 11 IN FULL first

INVARIANT 3: enrolment is INVITE-ONLY and the CLIENT signs a SERVER-PROVIDED session token. Sessions
are opaque server-side handles, not signed claims -- which is precisely what makes immediate
revocation possible. An operator principal must NOT become a signed claim, or revoking one stops
being possible.

INVARIANT 11: mTLS is required, there is no CA and NO TRUST-ON-FIRST-USE. Whatever identifies an
operator must be pinned the way everything else here is. And the session/certificate cross-check
applies: a session token presented over a connection whose client certificate belongs to a DIFFERENT
principal must be rejected. That is stronger than either mechanism alone and must hold for operators
too.

INVARIANT 7: every capability ships with a CLI subcommand and an AGENT_PROTOCOL.md entry in the SAME
task. Note the audience split -- operator commands belong in `agent-bus` (the server binary, which
already hosts `invite mint`, `peer`, `log`), NOT in `agent-busctl`, which is the AGENT's client. Do
not put an admin capability on the agent surface.

# Design questions this task must ANSWER, not assume

  1. Is an operator a roster entry with a flag, or a SEPARATE principal type? A flag on the roster
     means an attacker who can write the roster can grant themselves admin. A separate type means a
     second identity system to keep correct.
  2. Where does the operator credential come from? There is no CA. The bus mints its own identity at
     first start; an operator credential minted the same way, offline, taking the data directory
     lock like `invite mint` does, is the shape most consistent with what exists.
  3. Can an operator principal be REVOKED, and how fast? Invariant 3 chose opaque handles for
     exactly this. Do not regress it.
  4. Is there ONE operator or many? Many means naming, listing and per-principal audit. One means a
     shared secret, which is the thing that never gets rotated.
  5. What does the AUDIT LOG record? Invariant 6: metadata and routing only, never bodies, integrity
     by keyed MAC. An operator action MUST be loudly attributable -- who did what, to whom, when.

# Explicitly OUT of scope
  Do not implement AUTH-7, INVMINT, or CONV-AUTHZ-ADMIN here. This task delivers the PRINCIPAL and
  the authorisation check. Those three consume it and stay separate, each with its own gates.

# Acceptance
  - an operator principal exists, authenticates, and is distinguishable from every enrolled agent
  - NO enrolled agent can authenticate as an operator, proven by a test
  - an operator action is attributable in the audit log
  - revocation works and is fast; a test proves a revoked operator is refused
  - the capability ships with its `agent-bus` subcommand and its docs in the same task
  - decisions on all five questions above recorded in DECISIONS.md

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-10-WIRING](../AUTH-10-WIRING--b11ef24c/task.md) — AUTH-10-WIRING: wire the operator principal into cmd/agent-bus/main.go — until this lands… (done)
- [AUTH-7](../AUTH-7--4ba67a7b/task.md) — AUTH-7: Operator/admin can clear one agent's active sessions without restarting the bus (todo)
- [CONV-AUTHZ-ADMIN](../../CONV/CONV-AUTHZ-ADMIN--70dd573a/task.md) — CONV-AUTHZ-ADMIN: the ADMIN arm of membership change -- BLOCKED, there is no admin princi… (blocked)
- [INVMINT-2](../../INVMINT/INVMINT-2--ef18b37a/task.md) — INVMINT-2: introduce an OPERATOR PRINCIPAL — a bus-scoped, non-agent identity that can au… (superseded)
- [RELAY-48](../../RELAY/RELAY-48--9887b0eb/task.md) — RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-10-FU-CHECKPOINT](../AUTH-10-FU-CHECKPOINT--4a7289bb/task.md) — AUTH-10-FU-CHECKPOINT: OperatorRegistry is not a wal.CheckpointParticipant — operator rec… (todo)
- [AUTH-10-FU-ENROLSEAM](../AUTH-10-FU-ENROLSEAM--a83e9a13/task.md) — AUTH-10-FU-ENROLSEAM: cross-plane certificate uniqueness is one-directional — Service.Enr… (todo)
- [AUTH-10-FU-LABELAGREE](../AUTH-10-FU-LABELAGREE--7336077e/task.md) — AUTH-10-FU-LABELAGREE: no test asserts the label-differs-still-silent path that two docum… (todo)
- [AUTH-10-WIRING](../AUTH-10-WIRING--b11ef24c/task.md) — AUTH-10-WIRING: wire the operator principal into cmd/agent-bus/main.go — until this lands… (done)
- [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](../../UNASSIGNED/Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) — Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
