# INVMINT-2: introduce an OPERATOR PRINCIPAL — a bus-scoped, non-agent identity that can authenticate to the running bus

| Field | Value |
| --- | --- |
| Public id | `ef18b37a-72b5-4b00-865f-edac288a0659` |
| Key | INVMINT-2 |
| Epic | [INVMINT](../epic.md) |
| Status | superseded |
| Priority | P3 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T07:46:05.674354+00:00 |
| Updated | 2026-08-16T10:08:03.185880+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestOperatorPrincipal' ./internal/httpapi ./internal/auth
```

## Description

Give the running bus a principal that is NOT an agent, so an admin surface has something to authenticate.
BLOCKED ON INVMINT-1 -- do not start until the authority decision is recorded and says yes.

## The gap

Every route authenticates except enrolment, session begin/complete, `/healthz` and `/v1/info` -- but ALL of
that authentication is AGENT authentication. If an admin route reused it, an AGENT credential would
authorise minting the credentials that CREATE AGENTS: any enrolled agent could mint itself an unlimited
supply of new identities, which collapses invariant 3 completely. So the principal must be distinct in
kind, not merely in permission.

## The natural place is invariant 11's mTLS

An operator client certificate, bound at init the way the bus's own identity is. Read `INVARIANTS.md`
invariant 11 IN FULL first, and invariant 3.

## THE PEER PRECEDENT: cite it accurately or not at all

The peer surface authenticates by CLIENT CERTIFICATE ALONE, and security recorded that as a DELIBERATE
NARROWING of invariants 3 and 11 at `internal/httpapi/authmw.go:339-351`. That is a real precedent for a
non-agent principal authenticated by certificate, and it is the right thing to reference.

BUT READ THE WHOLE COMMENT BEFORE LEANING ON IT. The same block says, verbatim, "DO NOT EXTEND IT BY ADDING
A PEER PATH TO unauthenticatedRoutes"; warns "Do not read this arm as 'invariant 11 is fully honoured on
peer routes'. It is narrowed here, deliberately"; records the cost ("a peer's authority has no online
revocation... and nothing caps a peer certificate's NotAfter"); and states the narrowing REVERSES the
moment a BUS-SCOPED bearer credential exists -- "one naming the peer bus rather than an agent -- at which
point the pair becomes constructible and the clause applies unnarrowed."

That last clause lands directly on this task. If the operator principal ends up holding a token AND a
certificate, invariant 11's cross-check APPLIES UNNARROWED: a token presented over a connection whose
client certificate belongs to a DIFFERENT principal must be REJECTED. Do not copy the peer arm's
certificate-only shape and inherit its narrowing by accident; it was accepted there for a reason that may
not hold here.

## Requirements

- Fail-closed, in the shape of `RequirePeerPrincipal`: no resolver, no TLS, no certificate, an out-of-date
  leaf, an unknown fingerprint, a withdrawn binding or an ambiguous one ALL refuse.
- An AGENT credential must NEVER satisfy the operator principal, and vice versa. Pin that with a test --
  it is the single most important assertion in this task.
- Revocation: say what it is and what it costs. The peer arm has none online; if this one has none either,
  that must be a stated, recorded cost rather than an omission.
- Scope: ONE operator principal. No role tier, no permission matrix, no multi-user admin -- those are
  explicitly out of scope for the epic.
- Do NOT add anything to `unauthenticatedRoutes`.

## Review conditions

reviewer AND security are mandatory. Security must specifically answer: can an enrolled agent reach this
principal by any path, and does a stolen operator certificate have a bounded blast radius?

PROOF STATUS -- READ THIS BEFORE COMPLETING. The test named in `proof_cmd` DOES NOT EXIST YET; writing it is
part of this task's deliverable, not a pre-existing artefact. `scripts/proof-check.sh` will report VACUOUS
until it is written, and THAT VACUOUS IS THE RED OBSERVATION -- record it before you start. If the design
lands under a different test or command name, have spec-keeper UPDATE this task's `proof_cmd` to the real
name; DO NOT complete the task behind a proof naming a test nobody wrote. That mechanism has produced 88
broken proofs in this backlog and closed 2 tasks on targets that never existed.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-1](../INVMINT-1--1bed65a8/task.md) — INVMINT-1: decide the invite-minting AUTHORITY MODEL and reconcile it with E4, FEDERATION… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-10](../../AUTH/AUTH-10--37993b49/task.md) — AUTH-10: An operator/admin principal -- the missing noun blocking AUTH-7, INVMINT and CON… (done)
- [AUTH-7](../../AUTH/AUTH-7--4ba67a7b/task.md) — AUTH-7: Operator/admin can clear one agent's active sessions without restarting the bus (todo)
- [CONV-AUTHZ-ADMIN](../../CONV/CONV-AUTHZ-ADMIN--70dd573a/task.md) — CONV-AUTHZ-ADMIN: the ADMIN arm of membership change -- BLOCKED, there is no admin princi… (blocked)
- [CONV-SUCCESSION](../../CONV/CONV-SUCCESSION--422be55b/task.md) — CONV-SUCCESSION: creator-only mutation freezes a conversation when the creator's agent id… (todo)
- [INVMINT-1](../INVMINT-1--1bed65a8/task.md) — INVMINT-1: decide the invite-minting AUTHORITY MODEL and reconcile it with E4, FEDERATION… (todo)
- [INVMINT-3](../INVMINT-3--8555e659/task.md) — INVMINT-3: the invite-mint HTTP route on the running bus (NOT /v1/mint, which is message-… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
