# INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win, NOT blocked)

| Field | Value |
| --- | --- |
| Public id | `cedb8d6f-c875-4094-963c-7a439705476e` |
| Key | INVMINT-6 |
| Epic | [INVMINT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | CLI |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T07:46:06.757694+00:00 |
| Updated | 2026-08-15T07:47:21.636178+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestInviteMintCount' ./cmd/agent-bus && grep -n 'agent-bus invite mint -data-dir' CONTRACTS-CLI.md | grep -q '\[-count' && echo INVMINT6_OK
```

## Description

Add a `-count N` flag to the EXISTING offline `agent-bus invite mint` so a pool of invites is minted in ONE
process start.

## NOT BLOCKED ON ANYTHING. This needs no authority decision, no route, no new principal.

It does not move minting onto the network and does not touch the authority model, so it is independent of
INVMINT-1..-5 and can be done at any time. It is one of the two best value-for-effort items in the epic.

## The problem

There is exactly ONE invite per `mint` invocation -- `cmd/agent-bus/invite.go:195-210` declares
`-data-dir`, `-bus-address`, `-ttl`, `-label`, `-json`, `-log-level` and no count. Minting a pool of 20
therefore means 20 process starts, each acquiring and releasing the data directory's exclusive dirlock.
Since the bus must be STOPPED for the whole sequence, that time is maintenance-window time. `-count N`
turns 20 lock acquisitions into one and materially shrinks the window.

The pre-minted pool is the SUPPORTED WORKAROUND for the epic's whole problem (see the epic description:
verified end to end -- mint a pool during one window, then enrol against the RUNNING bus indefinitely, zero
restarts between enrolments). This task makes that workaround cheap. It is worth doing even if INVMINT-1
decides against an online route -- ARGUABLY IT IS WORTH MORE IN THAT CASE.

## Requirements

- `-count` defaults to 1; the existing single-invite behaviour and output shape are UNCHANGED when it is
  absent or 1. Do not break the documented
  `agent-bus invite mint -json ... | agent-busctl enrol --invite-file - --name planner` pipeline
  (`AGENT_PROTOCOL.md:417-418`), which depends on `-json` emitting ONE JSON object on stdout.
- Decide and DOCUMENT the `-json` shape for N>1 -- a JSON array, or JSON Lines. Whichever you pick, the
  N==1 output must stay byte-compatible with today's, or the pipeline above breaks silently.
- Bound N and reject a bad value rather than clamping it, following the existing `-ttl` convention: an
  over-max TTL "is REJECTED rather than silently clamped, because quietly issuing a shorter-lived
  credential than the operator asked for is how an invite mysteriously stops working"
  (`internal/invite/store.go:278-284`). Same reasoning applies to count. Zero and negative are usage
  errors (exit 2).
- PARTIAL FAILURE IS THE INTERESTING CASE. If invite k of N fails to append, the k-1 already-durable
  invites EXIST and are redeemable. The command must not imply otherwise: report exactly which were minted,
  and do not emit an all-or-nothing success. Invariant 1 -- ids are never reused, so a failed mint does not
  free its id. A crash-injection or fault-injection test on this path is the strongest part of this task.
- Invariant 1 stays structural: `-count` adds NO id and NO secret input, and `invite.MintRequest` gains no
  field for either. `TestInviteMintRejectsClientSuppliedSecret` must still pass.
- `CONTRACTS-CLI.md` synopsis (line ~124) and flag table updated; `AGENT_PROTOCOL.md` if the operator
  recipe changes (it should -- see INVMINT-7, which is the natural companion).

## Proof

Note the doc half pins the SYNOPSIS LINE specifically, not a bare `-count`: `grep -c -- '-count'
CONTRACTS-CLI.md` is already 3 today (matching `double-count` at line 797 and `agent-busctl watch --count`
at lines 1224/1264), so a bare grep would PASS WITHOUT THE FIX -- exactly the incidental-match trap. The
stored proof anchors on `agent-bus invite mint -data-dir <dir> -bus-address <url>` followed by `[-count`.
Baseline observed RED by spec-keeper at filing (2026-08-15). Confirm RED before the fix.

PROOF STATUS -- READ THIS BEFORE COMPLETING. The test named in `proof_cmd` DOES NOT EXIST YET; writing it is
part of this task's deliverable, not a pre-existing artefact. `scripts/proof-check.sh` will report VACUOUS
until it is written, and THAT VACUOUS IS THE RED OBSERVATION -- record it before you start. If the design
lands under a different test or command name, have spec-keeper UPDATE this task's `proof_cmd` to the real
name; DO NOT complete the task behind a proof naming a test nobody wrote. That mechanism has produced 88
broken proofs in this backlog and closed 2 tasks on targets that never existed.

## PROOF PIN — WHY IT IS SHAPED THIS WAY (spec-keeper, 2026-08-15)

The first version of this proof spelled the synopsis out literally, including `<dir>` and `<url>`.
`scripts/proof-check.sh` classified it **UNVERIFIABLE — contains an unfilled <placeholder>, the proof is a
template, not a command** and REFUSED TO RUN IT. It would have been impossible to complete this task behind
that proof. The stored proof now anchors with `grep -n 'agent-bus invite mint -data-dir' CONTRACTS-CLI.md |
grep -q '\[-count'`, which pins the SAME synopsis line without any angle brackets. Verified by
spec-keeper at filing: verdict=FAIL (RED, as required) rather than UNVERIFIABLE.

GENERAL LESSON FOR THIS EPIC: do not put `<...>` in a `proof_cmd`. proof-check.sh reads it as an unfilled
template and refuses.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [INVMINT-7](../INVMINT-7--174c7ba9/task.md)
- **relates to** [ORCH-3](../../ORCH/ORCH-3--d75a3b68/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-1](../INVMINT-1--1bed65a8/task.md) — INVMINT-1: decide the invite-minting AUTHORITY MODEL and reconcile it with E4, FEDERATION… (todo)
- [INVMINT-7](../INVMINT-7--174c7ba9/task.md) — INVMINT-7: document the pre-minted-invite pool recipe, with the 0600 mode built in (quick… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-1](../INVMINT-1--1bed65a8/task.md) — INVMINT-1: decide the invite-minting AUTHORITY MODEL and reconcile it with E4, FEDERATION… (todo)
- [INVMINT-7](../INVMINT-7--174c7ba9/task.md) — INVMINT-7: document the pre-minted-invite pool recipe, with the 0600 mode built in (quick… (todo)
- [ORCH-2](../../ORCH/ORCH-2--5ffeb926/task.md) — ORCH-2: certificate and invite PROVISIONING under an orchestrator — no CA, no TOFU, 0600,… (todo)
- [ORCH-3](../../ORCH/ORCH-3--d75a3b68/task.md) — ORCH-3: first-boot ordering — a bus's first-ever boot can enrol NOBODY, and an orchestrat… (todo)
- [ORCH-4](../../ORCH/ORCH-4--282a2e9c/task.md) — ORCH-4: restart semantics under an orchestrator — sessions are IN-MEMORY, so every pod re… (todo)
- [TUI-3](../../TUI/TUI-3--140aadf7/task.md) — TUI-3: GUARD — the TUI sits on the client package and cannot bypass it, and agent-busctl'… (todo)
- [TUI-4](../../TUI/TUI-4--11898d9b/task.md) — TUI-4: the read-only MONITOR view — renders LIVE/ADMIN data, re-specifies none of it (todo)
- [TUI-5](../../TUI/TUI-5--b2a44ce9/task.md) — TUI-5: the human as a bus PARTICIPANT — read and send messages as a person (message bodie… (todo)
- [TUI-6](../../TUI/TUI-6--cb4e3fd7/task.md) — TUI-6: "and other people" — multiple humans as distinct enrolled identities, DMs between… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
