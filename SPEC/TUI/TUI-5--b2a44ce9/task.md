# TUI-5: the human as a bus PARTICIPANT — read and send messages as a person (message bodies, which ADMIN excludes)

| Field | Value |
| --- | --- |
| Public id | `b2a44ce9-a678-4919-a4a2-23a07b750776` |
| Key | TUI-5 |
| Epic | [TUI](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | CLI |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:00:49.652362+00:00 |
| Updated | 2026-08-15T08:00:49.652362+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestTUIParticipantSendReceive' ./client/... ./cmd/... && echo TUI5_PARTICIPANT_OK
```

## Description

The "instruct / communicate with agents" half of the request: a person reading their inbox and sending
messages from the terminal. BLOCKED ON TUI-1 and TUI-3.

## This is the part that is genuinely NOT covered anywhere

ADMIN's charter EXPLICITLY EXCLUDES "message bodies". `grep -rliE 'human participant|humans on the bus|person
on the bus|chat' SPEC/` over all 621 task files returns ZERO. Nothing in the backlog makes a human a
conversational participant.

## Invariant 6 is NOT violated by this, and the reasoning must be in the code

Invariant 6 constrains THE APPEND-ONLY LOG: it records metadata and routing only, never message bodies. It does
NOT say a client may not see bodies. Every agent reads its own inbox bodies over `/v1/wait` today; a human
principal doing the same thing is ORDINARY. The violation would be sourcing OTHER principals' bodies from the
log or the audit view. Write that distinction down where a future reader will hit it, because "the TUI shows
message bodies" reads like an invariant-6 breach until you know which source it came from.

## Requirements

- Goes through `client/` only (TUI-3's guard enforces it) -- `client.Send`/`Broadcast`/`Wait`, not a second
  implementation.
- **Invariant 10 is this task's hard requirement, exactly as it was CLI-4's.** The idempotency key is
  generated ONCE per logical send and REUSED ON EVERY RETRY. "Generating a fresh key per attempt turns the
  retry that idempotency exists to make safe into a duplicate message." A TUI adds a NEW way to get this
  wrong that the CLI does not have: a human edits a draft and re-sends. Same key + DIFFERENT payload is a
  PROTOCOL VIOLATION -- an edited draft is a NEW message and must take a NEW key. Pin that with a test; it is
  the sharpest edge in this task.
- Fully-qualified ids everywhere (invariant 2): `<bus-id>.<agent-id>`, never shortened, including in
  autocomplete and in any "reply" affordance.
- Untrusted text: message bodies and agent ids are attacker-influenced and are being written to a TERMINAL.
  Reuse the existing terminal-safe renderer rather than writing a second one -- see CLI-3-FU-SAFETEXT ("export
  the terminal-safe renderer from client/ and delete busctl's copy"). A TUI is MORE exposed than line output,
  not less: escape sequences can move the cursor and rewrite panes.
- A send that fails ambiguously must say so honestly; see the open CLI tasks on post-200 validation and on
  losing the minted idempotency key on ambiguous failure, and do not reintroduce either.

## Provability

Model and client behaviour are unit-testable; the rendering is not. Same split as TUI-4 -- the proof targets
send/receive behaviour and idempotency-key reuse, NOT the visuals.

## PROOF STATUS -- READ BEFORE COMPLETING

The test named in `proof_cmd` DOES NOT EXIST YET; writing it is part of this task's deliverable, not a
pre-existing artefact. `scripts/proof-check.sh` reports VACUOUS until it is written, and THAT VACUOUS IS THE
RED OBSERVATION -- record it before starting. If the design lands under a different name, have spec-keeper
UPDATE this `proof_cmd`; never complete behind a proof naming a test nobody wrote (88 broken proofs in this
backlog, 2 tasks closed on targets that never existed). Do NOT put `<angle brackets>` in a proof_cmd --
proof-check.sh classifies them as an unfilled template and REFUSES TO RUN IT (caught on INVMINT-6).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [TUI-1](../TUI-1--3ea68265/task.md)
- **blocked by** [TUI-3](../TUI-3--140aadf7/task.md)
- **relates to** [CLI-3-FU-SAFETEXT](../../CLI/CLI-3-FU-SAFETEXT--e4baf8c5/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-3-FU-SAFETEXT](../../CLI/CLI-3-FU-SAFETEXT--e4baf8c5/task.md) — CLI-3-FU-SAFETEXT: export the terminal-safe renderer from client/ and delete busctl's copy (done)
- [CLI-4](../../CLI/CLI-4--137465b9/task.md) — CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadc… (done)
- [INVMINT-6](../../INVMINT/INVMINT-6--cedb8d6f/task.md) — INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… (todo)
- [TUI-1](../TUI-1--3ea68265/task.md) — TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or com… (todo)
- [TUI-3](../TUI-3--140aadf7/task.md) — TUI-3: GUARD — the TUI sits on the client package and cannot bypass it, and agent-busctl'… (todo)
- [TUI-4](../TUI-4--11898d9b/task.md) — TUI-4: the read-only MONITOR view — renders LIVE/ADMIN data, re-specifies none of it (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [TUI-1](../TUI-1--3ea68265/task.md) — TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or com… (todo)
- [TUI-4](../TUI-4--11898d9b/task.md) — TUI-4: the read-only MONITOR view — renders LIVE/ADMIN data, re-specifies none of it (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
