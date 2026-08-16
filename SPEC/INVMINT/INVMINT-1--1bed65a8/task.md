# INVMINT-1: decide the invite-minting AUTHORITY MODEL and reconcile it with E4, FEDERATION (e) and ADMIN D6 (DECISIONS.md)

| Field | Value |
| --- | --- |
| Public id | `1bed65a8-46a3-4d3c-9ed5-32ace3805ee8` |
| Key | INVMINT-1 |
| Epic | [INVMINT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | PROCESS |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T07:46:05.417690+00:00 |
| Updated | 2026-08-15T07:46:05.417690+00:00 |
| Completed | — |

## Proof command

```sh
grep -q '^## 2026-.*INVMINT: invite-minting authority' DECISIONS.md && grep -q 'ADMIN ruling D6' DECISIONS.md && grep -qE 'supersedes E4|reaffirms E4' DECISIONS.md && echo INVMINT1_DECISION_RECORDED
```

## Description

DECIDE, IN WRITING AND BEFORE ANY CODE, whether the authority to mint an invite may move from FILESYSTEM
ACCESS to the data directory onto the NETWORK. This is the gate on the whole INVMINT spine: it BLOCKS
INVMINT-2, -3, -4 and -5. It does NOT block INVMINT-6 or INVMINT-7.

## Why this is a decision and not a design detail

`cmd/agent-bus/invite.go:7-8` states the minting authority IS filesystem access to the data directory --
the same model used for `wal-mac.key` and the bus's private keys -- and gives the reason: "Nothing new is
exposed on the wire, so bootstrapping invite-only enrolment introduces no new network-reachable privilege
-- which is the whole point, given that invariant 3 exists because an unauthenticated enrolment route let
an attacker mint its own agents." An invite is the credential that CREATES AGENTS. A route that mints them
is, by construction, the highest-value route on the bus. Changing who may reach it is an authority-model
change, and CLAUDE.md requires a dated `DECISIONS.md` entry for anything that weakens or narrows an
invariant.

## THREE prior rulings must be reconciled -- name each one and say what happens to it

1. `DECISIONS.md` E4 (2026-08-02) -- "The first invite is minted server-side."
2. `DECISIONS.md` 2026-08-08 FEDERATION (e) -- peer configuration is offline under the dirlock, "following
   the `invite mint` / E4 precedent", explicitly accepting "online re-peering is given up; a topology
   change needs a restart" (`CONTRACTS-CLI.md:321-325`).
3. The ADMIN epic's operator ruling D6 -- "NO ONLINE INVITE MINT. `agent-bus invite mint` takes the
   exclusive dirlock and needs the bus stopped; the console links to the command instead."

E4 IS NOT A ONE-OFF -- it is a pattern applied at least three times. So this decision must state its SCOPE:
invite-specific, or generalising to peer configuration and every future operator surface? Reversing E4 for
invites while leaving FEDERATION (e) standing leaves two contradictory rulings about one question, which is
worse than either answer on its own.

D6 is an OPERATOR ruling, and this epic contradicts it. D6 was a SCOPING decision for the console (the
console links to the command instead of driving it), not a finding that an online mint is unsafe -- but it
is live, and shipping a route would make it false. Say explicitly which supersedes which.

## Deliverable

A dated `## 2026-XX-XX — INVMINT: invite-minting authority` section in `DECISIONS.md` that:
- states the decision (move minting onto the network, or KEEP it filesystem-only and close the spine);
- names E4, FEDERATION (e) and ADMIN D6 and records for each whether it is superseded, narrowed or
  reaffirmed;
- states the scope (invites only, or the general operator-surface pattern);
- records whether OPERATOR CONSENT was obtained, and from whom;
- if the answer is "keep it filesystem-only", says so and this epic's INVMINT-2..-5 are closed as
  `superseded` -- THAT IS A LEGITIMATE AND CHEAP OUTCOME. The pre-minted pool works. Do not treat this task
  as a formality on the way to a foregone conclusion.

## Constraints to carry into the decision text

- Invariant 3: every route authenticates except enrolment, session begin/complete, `/healthz` and
  `/v1/info` -- and all of that is AGENT authentication. There is no operator principal (see INVMINT-2).
- Invariant 1 must remain STRUCTURAL, not a rule (see INVMINT-3).
- The invite secret is a bearer credential; over the network it lands in a response body.

## Proof

The proof is deliberately OUTCOME-NEUTRAL: it requires the section to EXIST and to ADDRESS D6 and E4,
matching either `supersedes E4` or `reaffirms E4`. It does not presuppose which way the decision goes.

Baseline observed RED by spec-keeper at filing (2026-08-15): `grep -c 'INVMINT: invite-minting authority'
DECISIONS.md` = 0, `grep -c 'ADMIN ruling D6' DECISIONS.md` = 0, `grep -cE 'supersedes E4|reaffirms E4'
DECISIONS.md` = 0. Re-confirm RED before editing -- a doc proof never observed failing is not evidence.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-2](../INVMINT-2--ef18b37a/task.md) — INVMINT-2: introduce an OPERATOR PRINCIPAL — a bus-scoped, non-agent identity that can au… (todo)
- [INVMINT-3](../INVMINT-3--8555e659/task.md) — INVMINT-3: the invite-mint HTTP route on the running bus (NOT /v1/mint, which is message-… (todo)
- [INVMINT-6](../INVMINT-6--cedb8d6f/task.md) — INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… (todo)
- [INVMINT-7](../INVMINT-7--174c7ba9/task.md) — INVMINT-7: document the pre-minted-invite pool recipe, with the 0600 mode built in (quick… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-AUTHZ-ADMIN](../../CONV/CONV-AUTHZ-ADMIN--70dd573a/task.md) — CONV-AUTHZ-ADMIN: the ADMIN arm of membership change -- BLOCKED, there is no admin princi… (blocked)
- [CONV-SUCCESSION](../../CONV/CONV-SUCCESSION--422be55b/task.md) — CONV-SUCCESSION: creator-only mutation freezes a conversation when the creator's agent id… (todo)
- [INVMINT-2](../INVMINT-2--ef18b37a/task.md) — INVMINT-2: introduce an OPERATOR PRINCIPAL — a bus-scoped, non-agent identity that can au… (todo)
- [INVMINT-3](../INVMINT-3--8555e659/task.md) — INVMINT-3: the invite-mint HTTP route on the running bus (NOT /v1/mint, which is message-… (todo)
- [INVMINT-5](../INVMINT-5--18f15aa9/task.md) — INVMINT-5: invite REVOCATION and LISTING over the same online operator surface (todo)
- [INVMINT-6](../INVMINT-6--cedb8d6f/task.md) — INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… (todo)
- [TUI-1](../../TUI/TUI-1--3ea68265/task.md) — TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or com… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
