# INVMINT-7: document the pre-minted-invite pool recipe, with the 0600 mode built in (quick win, NOT blocked)

| Field | Value |
| --- | --- |
| Public id | `174c7ba9-334e-40e7-9fbc-1530e37a9095` |
| Key | INVMINT-7 |
| Epic | [INVMINT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | DOCS |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T07:46:07.094347+00:00 |
| Updated | 2026-08-15T07:46:07.094347+00:00 |
| Completed | — |

## Proof command

```sh
grep -q 'pre-mint a pool of invites' AGENT_PROTOCOL.md && grep -q '0664' AGENT_PROTOCOL.md && echo INVMINT7_DOC_OK
```

## Description

Document the operator recipe that ALREADY WORKS: pre-mint a pool of invites during one maintenance window,
then enrol agents against the RUNNING bus indefinitely. Write it so the reader gets the file mode right on
the FIRST attempt.

## NOT BLOCKED ON ANYTHING. Pure documentation of existing, verified behaviour.

## The trap this task exists to close

The invite file MUST be `0600`. `client/invite.go:237` refuses any group or world bit (anything in `0o077`)
at exit 3, with the remedy "chmod 0600 <path> -- and if it was readable while an untrusted user had access
to this machine, treat the invite as spent: revoke it and mint another".

THAT CHECK IS CORRECT AND MUST NOT BE RELAXED. It is not the bug. The bug is that a shell redirect
(`agent-bus invite mint -json ... > invite.json`) creates the file at `0664` under a typical umask, so
EVERY operator scripting this hits the refusal on their first attempt -- verified: three consecutive
enrolments failed this way before the mode was corrected. The docs currently show `chmod 0600 invite.json`
as a step (`AGENT_PROTOCOL.md:408`), which is remediation AFTER the fact, and say nothing about pooling at
all.

Nothing in the repo documents the pool: `grep -rn -i 'pre-mint|premint|pool of invites|umask 077'` over all
`*.md` returns ZERO matches, while `cmd/agent-bus/invitepool_test.go` describes the same flow in a test
comment as "the operator's real flow, not a test-only shortcut". The behaviour is real, tested and
undocumented.

## Deliverable

A section in `AGENT_PROTOCOL.md` (near the existing mint-then-enrol recipe at lines ~402-420) that:
- states plainly THAT THE STOP IS REQUIRED FOR MINTING, NOT FOR ENROLLING -- this is the single most
  valuable sentence in the epic, and it is currently written down nowhere;
- gives a copy-pasteable recipe that produces `0600` files WITHOUT a follow-up `chmod` -- e.g. a subshell
  setting `umask 077` around the redirect, or `install -m 0600`. Pick one and show it working;
- explains WHY `> file` yields `0664` and why the CLI refuses it, so the reader understands the check
  rather than working around it;
- states the consequence honestly: a pre-minted invite is a bearer credential sitting on disk for as long
  as the pool lasts. Recommend a TTL matched to the window and point at revocation. Do not present pooling
  as free.
- notes that a bus RESTART costs every agent a HANDSHAKE (sessions are in-memory and do not survive a
  restart), NOT a re-enrolment -- the roster is durable. Anyone who believes a restart de-enrols agents
  will over-estimate the cost of the current workaround.

Update `CONTRACTS-AGENT.md` if the agent-facing surface description needs it. If INVMINT-6 lands first, the
recipe should use `-count`.

## Proof

The proof pins TWO specific strings: the literal recipe phrase `pre-mint a pool of invites`, and `0664`
(the mode the explanation must name). Baseline observed RED by spec-keeper at filing (2026-08-15):
`grep -c 'pre-mint a pool of invites' AGENT_PROTOCOL.md` = 0 and `grep -c '0664' AGENT_PROTOCOL.md` = 0.
A doc proof that passes on an incidental match elsewhere in the file would have green-lit closing a task
two reviewers had blocked on -- so CONFIRM THIS IS RED BEFORE THE FIX, and if you reword the section,
have spec-keeper update the pinned strings rather than loosening them.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [INVMINT-6](../INVMINT-6--cedb8d6f/task.md)
- **relates to** [ORCH-2](../../ORCH/ORCH-2--5ffeb926/task.md)
- **relates to** [TUI-6](../../TUI/TUI-6--cb4e3fd7/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-6](../INVMINT-6--cedb8d6f/task.md) — INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-1](../INVMINT-1--1bed65a8/task.md) — INVMINT-1: decide the invite-minting AUTHORITY MODEL and reconcile it with E4, FEDERATION… (todo)
- [INVMINT-4](../INVMINT-4--ea948fb0/task.md) — INVMINT-4: the CLI subcommand for online invite minting + its AGENT_PROTOCOL.md entry (in… (todo)
- [INVMINT-6](../INVMINT-6--cedb8d6f/task.md) — INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… (todo)
- [ORCH-2](../../ORCH/ORCH-2--5ffeb926/task.md) — ORCH-2: certificate and invite PROVISIONING under an orchestrator — no CA, no TOFU, 0600,… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
