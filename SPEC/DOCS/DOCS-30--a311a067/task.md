# DOCS-30: clientcert help says the bus ignores the client certificate; the bus refuses 409 on a reused one

| Field | Value |
| --- | --- |
| Public id | `a311a067-4714-4fee-b7ba-181a89c139b0` |
| Key | DOCS-30 |
| Epic | [DOCS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T13:18:17.385270+00:00 |
| Updated | 2026-08-16T13:18:17.385270+00:00 |
| Completed | — |

## Proof command

```sh
! grep -n 'does not require one yet' cmd/agent-busctl/clientcert.go
```

## Description

FILED 2026-08-16 by main. Found by the DOCS-22 agent while fixing the invite-gate entry points,
confirmed independently by main before filing.

# The falsehood, in agent-facing help text

cmd/agent-busctl/clientcert.go:39-41 tells the agent:

  "The bus does not require one yet, so a fresh [certificate is] ... presented on the
   connection and, for now, ignored."

# What the bus actually does

internal/httpapi/auth.go:829 REFUSES enrolment with 409 Conflict when the presented client
certificate is already bound to another agent:

  "this client certificate is already bound to an agent; enrol with a fresh client keypair"

and logs: "one certificate must never name two agents (invariant 11)".

So the certificate is not ignored. It is load-bearing enough that reusing one BLOCKS a second
enrolment outright.

# Why this is worth a task and not a note

This is the same class DOCS-22 just fixed and it has a concrete, reproducible cost. The DOCS-22 agent
hit it live: while running the README quickstart end to end against a real bus, two agents sharing
one --identity credential store could not both enrol, because one mTLS client certificate can never
bind two agent names. The README now uses separate --identity dirs BECAUSE of that discovery.

An agent reading clientcert.go's help would conclude the opposite -- that certificates are
inconsequential -- and would design exactly the sharing arrangement the server refuses. The help text
is the agent's contract (invariant 7).

# Scope
  - correct cmd/agent-busctl/clientcert.go:39-41 to state what is enforced: mutual TLS is required
    (invariant 11), the certificate is presented and IS consequential, and one certificate binds to
    exactly one agent
  - sweep the rest of that file's help for the same "not yet / ignored" framing
  - check whether AGENT_PROTOCOL.md and CONTRACTS-CLI.md repeat it

# Standard of evidence
Pin the SPECIFIC line and confirm the proof is RED before the fix. A grep proof passing on an
incidental match elsewhere in the file has already green-lit a bad close in this repo. Note also that
`! grep -q ...` under `set -e` does NOT fail a script -- bash does not apply errexit to a negated
command, which has produced a false pass here.

Better still, do what DOCS-22 did and prove it live: try to enrol two agents against one --identity
store on a throwaway bus and record the 409.

# Related
  DOCS-22 (2f8ae959) fixed the four invite-gate entry points and found this while doing it.
  DOCS-11 -- `enrol --help` claims invites are "revocable" while `agent-bus invite revoke` does not
  exist. Same family: agent-facing help describing a capability the binary lacks.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DOCS-11](../DOCS-11--a434830e/task.md) — DOCS-11: Invite revocation is documented in three places and implemented in none (todo)
- [DOCS-22](../DOCS-22--2f8ae959/task.md) — DOCS-22: The four agent ENTRY POINTS the invite gate missed — \`README\` Quickstart, \`agent… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [9a02d65a-e96b-4fbe-93cf-846d8b5c2034](../Invariant-3-s-unauthenticated-route-enumeration-is-stale--9a02d65a/task.md) — Invariant 3's unauthenticated-route enumeration is stale in three docs -- six entries in… (todo)
- [dd2cdc20-8920-4e5b-bf0a-668f439cc3a6](../../UNASSIGNED/Reservation-counters-silently-drift-stale-and-hand-out-C--dd2cdc20/task.md) — Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS,… (todo)
- [f4bd3c9f-3af8-4438-bcb0-18203b857255](../../PROCESS/Deep-dive-audit-and-refactor-the-repo-s-tracked-.md-file--f4bd3c9f/task.md) — Deep-dive: audit and refactor the repo's tracked .md files, CLAUDE.md primary, fix AGENTS… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
