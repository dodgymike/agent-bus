# TUI-1: DECIDE whether the terminal interface REPLACES ADMIN's browser console (D1) or complements it — blocks the epic

| Field | Value |
| --- | --- |
| Public id | `3ea68265-fb1c-4458-adce-49d0e3b7a970` |
| Key | TUI-1 |
| Epic | [TUI](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | PROCESS |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:00:48.748720+00:00 |
| Updated | 2026-08-15T08:00:48.748720+00:00 |
| Completed | — |

## Proof command

```sh
grep -q '^## 2026-.*TUI: terminal interface vs the browser console' DECISIONS.md && grep -q 'ADMIN ruling D1' DECISIONS.md && grep -qE 'supersedes D1|reaffirms D1' DECISIONS.md && echo TUI1_DECISION_RECORDED
```

## Description

Put to the operator, and record, whether the requested TERMINAL interface REPLACES the ADMIN epic's browser
console or sits beside it. BLOCKS TUI-3, TUI-4, TUI-5, TUI-6.

## Why this is a decision and not a detail

ADMIN has **14 OPEN tasks** and its console is unbuilt. Its ruling **D1** was explicit and operator-made:
"plaintext HTTP on loopback + a per-process capability token, strict Origin / Sec-Fetch-Site checks, plus a
0600 unix socket for non-browser access. Ruled EXPLICITLY, not defaulted: this is a console surface, not a bus
surface, and TLS here would reintroduce the browser trust-store problem the architecture exists to eliminate."
ADMIN-3 ships `agent-busadm serve` with an embedded UI.

The operator has now asked for a TERMINAL interface to "administrate, monitor" -- which is ADMIN's core.
Building a browser console AND a TUI over the same data duplicates effort across 14 open tasks. A new operator
request may or may not be intended to supersede an old operator ruling, and SPEC-KEEPER MUST NOT GUESS. The
same shape as INVMINT-1: name the prior ruling, and say what happens to it.

## The three legitimate outcomes -- the proof is neutral between them

1. **REPLACE.** The TUI is the console. D1 and ADMIN-3 are superseded; ADMIN's still-valid tasks (ADMIN-2
   client.Info/Health, ADMIN-6 streaming audit reader, ADMIN-8 `/v1/status`, ADMIN-C1..C3 the control schema
   and reporter) are RE-HOMED or kept as dependencies, since most are data-layer work a TUI needs just as much.
   Note this outcome is CHEAPER, not more expensive -- it deletes a browser trust surface entirely.
2. **COMPLEMENT.** Both ship over one `client/` package. Say who maintains two renderers and why that is worth
   it.
3. **THIN VIEW.** ADMIN stays the console; the TUI is a narrow terminal view of the same data.

## Also decide here (they follow from the above and are cheap to settle together)

- **Binary identity.** A new binary, an `agent-busadm` mode, or an `agent-busctl` mode? This interacts with
  TUI-3's non-interactive guarantee -- `agent-busctl` may never prompt.
- **Whether the human is an ENROLLED PRINCIPAL on the bus.** ADMIN-9 already has the console enrolling by
  redeeming an invite blob. If the TUI is a bus participant (TUI-5/TUI-6 assume it is), say so here.
- **Scope of "other people".** ADMIN's charter explicitly excludes "multi-user auth". Multiple HUMANS each
  holding their OWN enrolled bus identity is NOT the same thing as multi-user auth on one console, and the
  distinction should be recorded so the exclusion is not read as blocking TUI-6.

## Deliverable

A dated `## 2026-XX-XX — TUI: terminal interface vs the browser console` section in `DECISIONS.md` naming
ADMIN ruling D1 and recording whether it is superseded or reaffirmed, plus the binary-identity and
enrolled-principal answers, and whether operator consent was obtained.

## Proof

Deliberately OUTCOME-NEUTRAL: matches `supersedes D1` OR `reaffirms D1`, so it cannot presuppose the verdict.
RED baseline observed by spec-keeper at filing (2026-08-15): all three greps return 0 in `DECISIONS.md`.
Re-confirm RED before editing -- a doc proof never observed failing is not evidence.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ADMIN-2](../../ADMIN/ADMIN-2--786e0de1/task.md) — ADMIN-2: client.Info/Health/Discovery + \`agent-busctl status \[--json\]\`, shipped together… (todo)
- [ADMIN-3](../../ADMIN/ADMIN-3--76bfce36/task.md) — ADMIN-3: \`agent-busadm serve\` -- loopback-only console with a capability token and an emb… (todo)
- [ADMIN-6](../../ADMIN/ADMIN-6--f92aa33f/task.md) — ADMIN-6: bounded, tail-tolerant STREAMING audit reader in internal/wal (no dir lock, torn… (todo)
- [ADMIN-8](../../ADMIN/ADMIN-8--7f550309/task.md) — ADMIN-8: GET /v1/status -- authenticated, in-process counters, exhaustive field-set pin,… (todo)
- [ADMIN-9](../../ADMIN/ADMIN-9--8bb10db2/task.md) — ADMIN-9: the console enrols by redeeming an invite blob (BLOCKED on INVITE-GATE) (blocked)
- [ADMIN-C1](../../ADMIN/ADMIN-C1--9074f7f2/task.md) — ADMIN-C1: versioned control/telemetry schema in a new internal/adminctl -- unknown kinds… (todo)
- [INVMINT-1](../../INVMINT/INVMINT-1--1bed65a8/task.md) — INVMINT-1: decide the invite-minting AUTHORITY MODEL and reconcile it with E4, FEDERATION… (todo)
- [TUI-3](../TUI-3--140aadf7/task.md) — TUI-3: GUARD — the TUI sits on the client package and cannot bypass it, and agent-busctl'… (todo)
- [TUI-4](../TUI-4--11898d9b/task.md) — TUI-4: the read-only MONITOR view — renders LIVE/ADMIN data, re-specifies none of it (todo)
- [TUI-5](../TUI-5--b2a44ce9/task.md) — TUI-5: the human as a bus PARTICIPANT — read and send messages as a person (message bodie… (todo)
- [TUI-6](../TUI-6--cb4e3fd7/task.md) — TUI-6: "and other people" — multiple humans as distinct enrolled identities, DMs between… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [TUI-2](../TUI-2--4b669f76/task.md) — TUI-2: DECIDE the TUI rendering dependency — this would be the project's FIRST third-part… (todo)
- [TUI-3](../TUI-3--140aadf7/task.md) — TUI-3: GUARD — the TUI sits on the client package and cannot bypass it, and agent-busctl'… (todo)
- [TUI-4](../TUI-4--11898d9b/task.md) — TUI-4: the read-only MONITOR view — renders LIVE/ADMIN data, re-specifies none of it (todo)
- [TUI-5](../TUI-5--b2a44ce9/task.md) — TUI-5: the human as a bus PARTICIPANT — read and send messages as a person (message bodie… (todo)
- [TUI-6](../TUI-6--cb4e3fd7/task.md) — TUI-6: "and other people" — multiple humans as distinct enrolled identities, DMs between… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
