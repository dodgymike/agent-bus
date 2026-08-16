# DUR-4-FU-DOCS: state invariants 4/6 as explicit NARROWINGS + document the WAL recovery API surface (RepairTail/TailRepair) in PROTOCOL.md/CONTRACTS-ONDISK.md -- at-least-once clause already satisfied by DOCS-2

| Field | Value |
| --- | --- |
| Public id | `0b6d5c11-e8fd-4d6b-8874-33c4038a8c6a` |
| Key | _(null in the export)_ |
| Epic | [DUR](../epic.md) |
| Status | todo |
| Priority | P0 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T14:59:50.346479+00:00 |
| Updated | 2026-08-07T20:33:59.864478+00:00 |
| Completed | — |

## Proof command

```sh
test -f PROTOCOL.md && grep -qi "invariant 4.*narrow\|narrow.*invariant 4" PROTOCOL.md && grep -qi "invariant 6.*narrow\|narrow.*invariant 6" PROTOCOL.md && grep -q "RepairTail" CONTRACTS-ONDISK.md
```

## Description

NARROWED 2026-08-07 (spec-keeper triage). Verified against HEAD: `7ddf757` (DOCS-2) added the literal "at-least-once" phrasing to PROTOCOL.md (line ~891, "Delivery is at-least-once, never exactly-once"), which satisfies clause (2) of the original description below. Do NOT re-add that obligation.

STILL OUTSTANDING -- verified RED at HEAD 2026-08-07, both by direct grep:

(1) THE NARROWED INVARIANTS, EXPLICITLY STATED AS NARROWINGS. PROTOCOL.md §6 substantively documents the always-restart/damage-never-fatal policy in detail (the "DAMAGE IS NEVER FATAL" callout and its table), but nowhere ties that back to invariant 4 or invariant 6 by name as a NARROWING -- `grep -in "invariant 4" PROTOCOL.md` finds only the unrelated normal-write-path mention at line ~492, and `grep -in "narrow" PROTOCOL.md` finds three hits, none about invariants 4/6. The 2026-08-02 decision text says this "must be stated in PROTOCOL.md, not left implicit" -- content-adjacent is not the same as stated. Add an explicit passage: invariant 4 is narrowed (acknowledged data is not lost through our OWN write path, but is not guaranteed to survive damaged media -- see invariant 6 discard); invariant 6 is narrowed (truncation is no longer restricted to a verified-corrupt TAIL; any damaged record may be discarded, each one logged).

(3) THE WAL RECOVERY API SURFACE. `grep -n "RepairTail\|TailRepair" CONTRACTS-ONDISK.md` returns NOTHING. CONTRACTS-ONDISK.md still lacks entries for `RepairTail(path, kind, logger)`, the `TailRepair{Path,Truncated,At,Removed,NextIndex,Reason}` struct, and `Recovered.Repaired`. This is unchanged from the original filing.

RELATED, DO NOT DUPLICATE: 804fa84c (P1) covers unknown-record-type startup behaviour; bd3cc650 (P2) covers the stale CONTRACTS.md:55 record-type list; DOCS-2 (7ddf757, landed) created PROTOCOL.md and added the at-least-once phrasing satisfying clause (2) above.

ORIGINAL FILING (2026-08-02), preserved for context -- clause (2) is now satisfied, clauses (1) and (3) are the current scope:

GROWN 2026-08-02 BY THE USER DECISIONS. This task now carries THREE documentation obligations the
2026-08-02 decisions create, not just the original RepairTail API surface. Two of them the decision
text says explicitly must be documented -- so they are not optional.

NOTE FIRST: **PROTOCOL.md DOES NOT EXIST.** Verified 2026-08-02 -- CLAUDE.md's repository layout lists
it as a tracked contract document ("PROTOCOL.md — the wire protocol + on-disk format") and there is no
such file in the repo. Three separate tasks (this one, 804fa84c, and DOCS-2) are written as though it
does. DOCS-2 owns CREATING it; this task owns the RECOVERY section within it. If DOCS-2 has not landed
when this is picked up, create the file with only the sections this task owns and let DOCS-2 fill the
rest -- do not block, and do not write the wire protocol here.

(1) THE NARROWED INVARIANTS -- REQUIRED BY THE DECISION, NOT OPTIONAL.
    Invariant 4 ("nothing is acknowledged before it is durable") is NARROWED: acknowledged data may
    now be DISCARDED when it is found corrupt. The decision says in terms: "The narrowing is
    deliberate and must be stated in PROTOCOL.md, not left implicit." Say it honestly -- we do not
    lose acknowledged data through our OWN write path, but we will not hold the bus hostage to
    damaged media.
    Invariant 6 is NARROWED: truncation is no longer restricted to a verified-corrupt TAIL; damaged
    records anywhere may be discarded, with a log entry each.
    Document the operator-facing consequence: the bus ALWAYS restarts on damage, and every discard is
    logged loudly and specifically. Non-damage errors (permission denied, I/O failure, dirlock held)
    still refuse to start.

(2) AT-LEAST-ONCE DELIVERY -- ALSO REQUIRED BY THE DECISION.
    "Delivery is AT-LEAST-ONCE. Duplicates are the normal steady state, which is what invariant 10's
    idempotency exists to absorb. Must be stated in PROTOCOL.md and AGENT_PROTOCOL.md." So state it in
    BOTH, and state the consequence for an agent author: your handler must be idempotent, and the
    server-minted monotonic sequence plus your cursor -- not the signature -- is what gives freshness.

(3) THE ORIGINAL SCOPE -- the WAL recovery API surface.
    CONTRACTS.md entries for RepairTail(path, kind, logger), the TailRepair{Path,Truncated,At,Removed,
    NextIndex,Reason} struct, and Recovered.Repaired. A PROTOCOL.md section describing WHEN records
    are discarded and what the operator sees.
    WARNING: the ORIGINAL wording of this task said the policy is "a single, provably-incomplete frame
    at EOF -- never more than one cut per start" and that anything else "is a REFUSAL TO START". THAT
    IS THE OLD, REVERSED POLICY. Do not write it. Confirm the FINAL shape against the code after
    DUR-11 lands -- the API has already been rewritten twice (laterRecordInTail -> inspectTail) and
    DUR-11 is rewriting the failure modes right now.

RELATED, DO NOT DUPLICATE: 804fa84c (P1) covers the unknown-record-type startup behaviour, itself
re-scoped to always-restart; bd3cc650 (P2) covers the stale CONTRACTS.md:55 record-type list; DOCS-2
owns creating PROTOCOL.md. Read all three first.

SEQUENCING: after DUR-11 lands. Raised to P0 because two of the three obligations are explicit
"must be stated in PROTOCOL.md" instructions from the user's own decision, and because the currently
shipped documentation describes a policy the code no longer follows.

PROOF. `test -f PROTOCOL.md && grep -q 'at-least-once' PROTOCOL.md && grep -q 'always restart' PROTOCOL.md && grep -q 'RepairTail' CONTRACTS.md`
-- FAILS TODAY at the first clause (PROTOCOL.md does not exist), which is correct and non-vacuous.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [804fa84c-e97b-4737-8866-801f87468da4](../Document-what-the-bus-does-with-an-UNKNOWN-WAL-record-ty--804fa84c/task.md) — Document what the bus does with an UNKNOWN WAL record type -- the answer is now discard-a… (todo)
- [DOCS-2](../../DOCS/DOCS-2--41c52cfa/task.md) — DOCS-2: PROTOCOL.md -- wire protocol + on-disk format (todo)
- [DUR-11](../DUR-11--884d3da4/task.md) — DUR-11: Close security's two OPEN HIGH truncation holes -- anchor the tail veto on record… (done)
- [bd3cc650-da3f-483d-a48d-321ab2a8d1dd](../CONTRACTS.md-55-is-stale-says-no-WAL-record-types-wire-v--bd3cc650/task.md) — CONTRACTS.md:55 is stale -- says no WAL record types/wire version exist yet, false as of… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [804fa84c-e97b-4737-8866-801f87468da4](../Document-what-the-bus-does-with-an-UNKNOWN-WAL-record-ty--804fa84c/task.md) — Document what the bus does with an UNKNOWN WAL record type -- the answer is now discard-a… (todo)
- [CONTEXT-PROTOCOL-WALFLOOR-DEDUP](../../CONTEXT/CONTEXT-PROTOCOL-WALFLOOR-DEDUP--1e9cec15/task.md) — CONTEXT-PROTOCOL-WALFLOOR-DEDUP: one file owns the WAL-index-floor bytes, not two that ca… (todo)
- [DOCS-2](../../DOCS/DOCS-2--41c52cfa/task.md) — DOCS-2: PROTOCOL.md -- wire protocol + on-disk format (todo)
- [DUR-4](../DUR-4--59c36769/task.md) — DUR-4: Corrupt-tail detection & truncation (superseded)
- [fc8cd234-d275-43a1-9cb0-d10bca4a4086](../../PROCESS/Backfill-non-vacuous-proof_cmd-across-the-14-actionable--fc8cd234/task.md) — Backfill non-vacuous proof_cmd across the 14 actionable tasks that have none (CLI-1..9 +… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
