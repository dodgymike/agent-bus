# AUTH-10-WIRING-DOCS: six published sites still say the operator plane is unwired - correct them with or before the AUTH-10-WIRING commit

| Field | Value |
| --- | --- |
| Public id | `82724d91-4e6b-4c78-b264-967966fb449d` |
| Key | AUTH-10-WIRING-DOCS |
| Epic | [AUTH](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T10:04:51.096094+00:00 |
| Updated | 2026-08-22T23:05:26.869297+00:00 |
| Completed | 2026-08-22T23:05:26.869282+00:00 |

## Proof command

```sh
bash scripts/doc-check.sh section CONTRACTS-CLI.md "#### Wiring status — BOTH GAPS ARE CLOSED (`AUTH-10-WIRING`, 2026-08-21)" "BOTH GAPS ARE CLOSED" && bash scripts/doc-check.sh section AGENT_PROTOCOL.md "## The OPERATOR PRINCIPAL exists, and it is not you — `agent-bus operator` is an OPERATOR command" "REACHABLE FROM `argv` since `AUTH-10-WIRING`" && bash scripts/doc-check.sh section CONTRACTS-CLI.md "### `agent-bus operator keygen|add|list|revoke` — the OPERATOR/ADMIN principal (`AUTH-10`, 2026-08-16)" "REACHABLE FROM `argv` since `AUTH-10-WIRING`"
```

## Description

AUTH-10-WIRING makes `agent-bus operator …` reachable from argv and registers `auth.OperatorRecordKind` in the server's WAL applier map. Six sites across four files assert the OPPOSITE, in the present tense, and become false the moment that commit lands. CLAUDE.md carries a dated incident for exactly this failure mode (the invite-gate paragraph that claimed the opposite for hours AFTER the gate shipped): "a stale 'not yet implemented' note is more dangerous than no note, because it reads as freshly checked". Every one of these points the UNDERSTATING way - a reader trusts them and believes the operator plane is inert server-side when it is live and replaying.

The six sites, all verified present:
1. CONTRACTS-CLI.md:1039-1047 - the caveat over the `agent-bus operator` EXIT-CODE TABLE: "NOT REACHABLE FROM `argv` TODAY (2026-08-16)" and "they are not yet the codes a command run at a prompt returns, because the command does not run." THE MOST URGENT of the six: it publishes an exit-code table as CONTRACT and steers an operator away from a working revocation mechanism.
2. CONTRACTS-CLI.md:1093-1112 - the whole "#### Wiring status - READ THIS BEFORE ASSUMING ANY OF THE ABOVE RUNS" section, both numbered gaps.
3. CONTRACTS-ONDISK.md:2787-2795 - "### WIRING STATUS - a server replay currently passes these records over IN SILENCE".
4. AGENT_PROTOCOL.md:1371-1379 - "NOT YET REACHABLE FROM `argv`, as of 2026-08-16".
5. cmd/agent-bus/operator.go:44-45 (the applier-map claim) AND :78-89 (the `operatorCommandName` doc: "!! THE DISPATCH IN main.go IS NOT YET WIRED …").
6. internal/auth/operator.go:32-45 (the banner "!! THE SERVER DOES NOT YET REGISTER THIS APPLIER - READ THIS BEFORE ASSUMING IT DOES !!"), :513-514 ("which is EXACTLY the state cmd/agent-bus/main.go is in today"), AND :114-115 - `Attach`'s own doc says "a registry with no log would acknowledge an operator that never reached disk (invariant 4)", which is INVERTED: `Add`/`Revoke` refuse with ErrNotAttached (:392-396, :454-456), so unattached is FAIL-CLOSED. The reviewer found main.go's corrected comment is now more accurate than the library's own doc.

MUST BE PRESERVED, still true after the change: cmd/agent-bus/operator.go:41 - "Nothing on the wire consumes an OperatorPrincipal yet" (AUTH-7 / INVMINT / CONV-AUTHZ-ADMIN are the consumers; the security gate verified NewOperatorService has zero non-test callers).

WARNING - CONTRACTS-CLI.md and AGENT_PROTOCOL.md are currently STAGED by a concurrent task. Diff `git diff HEAD -- <file>` and ADD to that work; do not overwrite it. Never hand-edit SPEC.md or SPEC/.

proof_cmd must PIN THE SPECIFIC LINE and be observed RED before the fix - a `grep` proof that passes on an incidental match elsewhere in the file is the documented hazard here.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-10-WIRING](../AUTH-10-WIRING--b11ef24c/task.md) — AUTH-10-WIRING: wire the operator principal into cmd/agent-bus/main.go — until this lands… (done)
- [AUTH-7](../AUTH-7--4ba67a7b/task.md) — AUTH-7: Operator/admin can clear one agent's active sessions without restarting the bus (todo)
- [CONV-AUTHZ-ADMIN](../../CONV/CONV-AUTHZ-ADMIN--70dd573a/task.md) — CONV-AUTHZ-ADMIN: the ADMIN arm of membership change -- BLOCKED, there is no admin princi… (blocked)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-10-WIRING](../AUTH-10-WIRING--b11ef24c/task.md) — AUTH-10-WIRING: wire the operator principal into cmd/agent-bus/main.go — until this lands… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
