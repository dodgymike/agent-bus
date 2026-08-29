# AUTH-4: POST /v1/leave -- leave / revocation

| Field | Value |
| --- | --- |
| Public id | `a853261d-2829-4101-906d-31a8a81eb59f` |
| Key | AUTH-4 |
| Epic | [AUTH](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | auth |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T09:05:49.142231+00:00 |
| Updated | 2026-08-23T00:05:01.100935+00:00 |
| Completed | 2026-08-23T00:05:01.100918+00:00 |

## Proof command

```sh
go test -race -run TestLeaveRevocation ./internal/auth
```

## Description

Lets an enrolled agent durably remove itself from the roster; its token is rejected by the auth middleware on every call afterward, including after a restart (the revocation itself goes through the two-phase write path).

ACCEPTANCE CRITERION ADDED (spec-keeper, 2026-08-02, from ID-3 reviewer F2 + security LOW finding): internal/ids/agentmint.go point 8 delegates bounding distinct-name growth to admission control, but AUTH-1 (now done) carried no such obligation in its description. Today growth is contained only because the roster never shrinks (no leave existed yet) and admission caps roster.Len(). Once this task lets leave shrink the roster while suffix counters must NOT be reclaimed (ids are never reused), an enrol/leave loop over distinct 64-byte names can grow suffix-counter memory without bound. This task must explicitly state, and test, how it bounds suffix-counter growth under a repeated enrol/leave loop (e.g. a cap on distinct names ever seen, eviction policy, or an explicit accepted-and-documented unbounded-but-slow-growth argument) -- do not ship leave without addressing this.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [AUTH-2-FU-POLLEXPIRY](../AUTH-2-FU-POLLEXPIRY--03d7ca66/task.md)
- **blocked by** [f505fb57-25ab-46e1-a7a1-2ca5787529ab](../Any-roster-reclamation-path-must-ship-a-bound-on-distinc--f505fb57/task.md)
- **blocks** [AUTH-5-FU-REVOCATION](../AUTH-5-FU-REVOCATION--fa579717/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [ID-3](../../ID/ID-3--745cce13/task.md) — ID-3: Agent id minting \`&lt;bus-id&gt;.&lt;name&gt;-&lt;n&gt;\` (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa](../SEC-unauthenticated-enrol-permanently-bricks-the-roster--1c4d3dea/task.md) — SEC: unauthenticated enrol permanently bricks the roster -- 4096-cap fails closed forever… (done)
- [ADMIN-1](../../ADMIN/ADMIN-1--db334b3c/task.md) — ADMIN-1: record the operator-console trust/transport/control rulings D1-D7 in DECISIONS.m… (blocked)
- [ADMIN-11](../../ADMIN/ADMIN-11--07926508/task.md) — ADMIN-11: remove an agent from the console (BLOCKED on AUTH-4) (blocked)
- [AUTH-1](../AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [AUTH-2-FU-POLLEXPIRY](../AUTH-2-FU-POLLEXPIRY--03d7ca66/task.md) — AUTH-2-FU-POLLEXPIRY: A long-poll can outlive its session, quietly contradicting immediat… (todo)
- [AUTH-5-FU-REVOCATION](../AUTH-5-FU-REVOCATION--fa579717/task.md) — AUTH-5-FU-REVOCATION: agent-level revocation-recovery crash-injection test, blocked on AU… (todo)
- [AUTH-7](../AUTH-7--4ba67a7b/task.md) — AUTH-7: Operator/admin can clear one agent's active sessions without restarting the bus (todo)
- [AUTH-ROSTER-RECLAIM](../AUTH-ROSTER-RECLAIM--b418638c/task.md) — AUTH-ROSTER-RECLAIM: operator-side "agent-bus roster remove &lt;id&gt;" escape hatch -- filesys… (todo)
- [CLI-2](../../CLI/CLI-2--39318208/task.md) — CLI-2: identity -- enrol, whoami, use, logout (ABSORBS AGENTIF-2; there is no bus-enrol.s… (done)
- [CLI-2-FU-LEAVE](../../CLI/CLI-2-FU-LEAVE--df79f84f/task.md) — CLI-2-FU-LEAVE: Add /v1/leave and make busctl logout actually revoke (todo)
- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [CRYPTO-8](../../CRYPTO/CRYPTO-8--2b1068eb/task.md) — CRYPTO-8: Broadcast to N agents -- authenticated encryption for the fan-out path (deferred)
- [IDEM-13](../../IDEM/IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-14](../../IDEM/IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-5](../../IDEM/IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [IDEM-6](../../IDEM/IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)
- [INVITE-REVOKE](../../INVITE/INVITE-REVOKE--d9def083/task.md) — INVITE-REVOKE: durably revoke an un-redeemed invite, and state what revocation does to an… (todo)
- [MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT](../../ID/MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT--6b0e561e/task.md) — MSG-FU-SUFFIXFLOOR-FU-ROSTERASSERT: assert a freshly-minted agent-suffix is not already i… (todo)
- [SIGN-8](../../SIGN/SIGN-8--71ef73d5/task.md) — SIGN-8: Agent-side messaging key material -- \`agent-bus keygen\`, key file location/permis… (todo)
- [ac4f9c2b-5460-4e83-997d-0e433194752f](../Enrol-REJECTS-a-duplicate-enrolment-public-key-409-one-k--ac4f9c2b/task.md) — Enrol REJECTS a duplicate enrolment public key (409) -- one keypair can no longer hold tw… (done)
- [f505fb57-25ab-46e1-a7a1-2ca5787529ab](../Any-roster-reclamation-path-must-ship-a-bound-on-distinc--f505fb57/task.md) — Any roster-reclamation path must ship a bound on distinct agent names in the SAME change… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
