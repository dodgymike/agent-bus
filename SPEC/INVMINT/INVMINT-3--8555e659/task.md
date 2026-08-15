# INVMINT-3: the invite-mint HTTP route on the running bus (NOT /v1/mint, which is message-id minting)

| Field | Value |
| --- | --- |
| Public id | `8555e659-597e-46b1-8571-032626c41271` |
| Key | INVMINT-3 |
| Epic | [INVMINT](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T07:46:05.883274+00:00 |
| Updated | 2026-08-15T07:46:05.883274+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestInviteMintRoute' ./internal/httpapi ./internal/invite
```

## Description

Add the HTTP route that lets the RUNNING bus mint an invite, authenticated by the operator principal from
INVMINT-2. BLOCKED ON INVMINT-1 and INVMINT-2.

The running bus is the only process that CAN do this without a stop: it already holds the data directory's
exclusive dirlock and owns the WAL. Minting appends an invite record through `internal/wal`'s two-phase
path; two processes appending to one log destroys it, and there is no version of that worth the
convenience.

## NAME COLLISION -- THE MOST LIKELY WAY THIS TASK GOES WRONG

`/v1/mint` ALREADY EXISTS (`internal/httpapi/messages.go:38` `RouteMint`, `client/messages.go:27`) and it
is MESSAGE-ID MINTING -- an agent reserving a message id/sequence before `/v1/send`, with its own
idempotency semantics (`internal/hub/mint.go:180` `MintRequest{Sender, Op, IdempotencyKey}`). It has
NOTHING to do with invites. DO NOT reuse that path. DO NOT extend that handler. DO NOT add an invite branch
to it. Choose a clearly distinct path and say in the code comment why the name is not `/v1/mint`, so the
next reader does not have to rediscover this.

## INVARIANT 1 MUST STAY STRUCTURAL -- THIS IS THE REVIEW CONDITION THAT MATTERS

Today `agent-bus invite mint` has NO `-invite-id` and NO `-invite-secret` flag, AND
`invite.MintRequest` (`internal/invite/store.go:273`) has NO FIELD for either -- it carries only `Label`
and `TTL`. That ABSENCE is what makes a client-supplied id IMPOSSIBLE rather than merely forbidden.
`TestInviteMintRejectsClientSuppliedSecret` (`internal/invite/mint_test.go:161`) pins it, and
`cmd/agent-bus/invite.go:212-218` records the reasoning: a supplied value is predictable to whoever wrote
the command line -- in a shell history, in a CI log, in a process list.

A REQUEST BODY FOR A NETWORK MINT ROUTE IS EXACTLY WHERE THAT PROPERTY IS LOST BY ACCIDENT. A decoded
struct with one extra field, or a passthrough into `invite.MintRequest`, silently converts a structural
guarantee into a validation rule that some later refactor drops. Requirements:
- the route's request struct carries NO id and NO secret field, and no map/`json.RawMessage`/`any`
  passthrough that could smuggle one;
- unknown fields are REJECTED, not ignored (`DisallowUnknownFields` or equivalent), so a client cannot
  probe for one;
- a test pins the request struct's field set the way `TestInviteMintRejectsClientSuppliedSecret` pins
  `MintRequest`'s. Structural, not a comment.

## The secret is a BEARER CREDENTIAL in a response body

Today its protection is a file mode the CLI enforces. Over the network it lands in a response body and from
there into a shell variable, a pipe, a CI log, a terminal scrollback. It must never be logged, never appear
in an error message, and never be echoed back on a retry path. The invite's ID is a name safe to log; the
SECRET is not. Existing code already draws that line (`agent-busctl` `--json` gains `invite_id`, not the
secret) -- follow it.

## Also required

- Read `INVARIANTS.md` invariants 1, 3, 4, 5, 6, 10 and 11 IN FULL; name them in your `kind=report` note.
- Invariant 4/5: the mint must be durable BEFORE the response is written. Never acknowledge an invite the
  bus could lose -- the operator would hand out a credential the bus does not know about.
- Invariant 10: a retried mint with the same idempotency key and the SAME payload returns the ORIGINAL
  result and does NOT mint a second invite; same key + DIFFERENT payload is rejected and logged; neither
  disconnects.
- Rate-limit or otherwise bound minting. An unbounded mint route recreates the ~4096-anonymous-enrolments
  roster-bricking failure that INVITE-GATE-ENFORCE (3cedcb7) closed, only authenticated.
- `CONTRACTS-HTTP.md` gains the route, its authentication, its request/response shape and its status codes.

## Scope

Route only. The CLI subcommand and `AGENT_PROTOCOL.md` entry are INVMINT-4 -- BUT see invariant 7: a
capability with no subcommand is the missing half. If -3 and -4 are worked separately, -4 must land before
the capability is announced as usable anywhere.

PROOF STATUS -- READ THIS BEFORE COMPLETING. The test named in `proof_cmd` DOES NOT EXIST YET; writing it is
part of this task's deliverable, not a pre-existing artefact. `scripts/proof-check.sh` will report VACUOUS
until it is written, and THAT VACUOUS IS THE RED OBSERVATION -- record it before you start. If the design
lands under a different test or command name, have spec-keeper UPDATE this task's `proof_cmd` to the real
name; DO NOT complete the task behind a proof naming a test nobody wrote. That mechanism has produced 88
broken proofs in this backlog and closed 2 tasks on targets that never existed.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [INVMINT-1](../INVMINT-1--1bed65a8/task.md)
- **blocked by** [INVMINT-2](../INVMINT-2--ef18b37a/task.md)
- **blocks** [INVMINT-4](../INVMINT-4--ea948fb0/task.md)
- **blocks** [INVMINT-5](../INVMINT-5--18f15aa9/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVITE-GATE-ENFORCE](../../INVITE/INVITE-GATE-ENFORCE--8297d7e2/task.md) — INVITE-GATE-ENFORCE: enforce invite-only enrolment (P0: anonymous roster exhaustion) (in_progress)
- [INVMINT-1](../INVMINT-1--1bed65a8/task.md) — INVMINT-1: decide the invite-minting AUTHORITY MODEL and reconcile it with E4, FEDERATION… (todo)
- [INVMINT-2](../INVMINT-2--ef18b37a/task.md) — INVMINT-2: introduce an OPERATOR PRINCIPAL — a bus-scoped, non-agent identity that can au… (todo)
- [INVMINT-4](../INVMINT-4--ea948fb0/task.md) — INVMINT-4: the CLI subcommand for online invite minting + its AGENT_PROTOCOL.md entry (in… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-1](../INVMINT-1--1bed65a8/task.md) — INVMINT-1: decide the invite-minting AUTHORITY MODEL and reconcile it with E4, FEDERATION… (todo)
- [INVMINT-4](../INVMINT-4--ea948fb0/task.md) — INVMINT-4: the CLI subcommand for online invite minting + its AGENT_PROTOCOL.md entry (in… (todo)
- [INVMINT-5](../INVMINT-5--18f15aa9/task.md) — INVMINT-5: invite REVOCATION and LISTING over the same online operator surface (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
