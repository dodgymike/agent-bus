# doc-check.sh's re-entry probe reports "suppressed assertions" while quoting two equal counts (84 vs 84)

| Field | Value |
| --- | --- |
| Public id | `5cf1edd0-a678-4072-98f9-4c1cb08c7c92` |
| Key | _(null in the export)_ |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T22:06:54.909316+00:00 |
| Updated | 2026-08-21T22:06:54.909316+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'T=$(mktemp -d); trap "rm -rf \"$T\"" EXIT; git archive HEAD | tar -x -C "$T"; sed -i "105a unset -f mktemp" "$T/scripts/doc-check.sh"; OUT=$(cd "$T" && bash scripts/doc-check.sh --selftest 2>&1); LINE=$(printf "%s\n" "$OUT" | grep "suppressed assertions" || true); if [ -z "$LINE" ]; then echo NO_SUPPRESSION_MESSAGE_EMITTED; exit 0; fi; A=$(printf "%s" "$LINE" | sed -n "s/.*(\([0-9][0-9]*\) with them vs \([0-9][0-9]*\) for.*/\1/p"); Bn=$(printf "%s" "$LINE" | sed -n "s/.*(\([0-9][0-9]*\) with them vs \([0-9][0-9]*\) for.*/\2/p"); if [ -z "$A" ] || [ -z "$Bn" ]; then echo "COULD_NOT_PARSE_COUNTS: $LINE"; exit 2; fi; if [ "$A" = "$Bn" ]; then echo "BUG_REPRODUCED: suppressed-assertions message quotes two EQUAL counts ($A vs $Bn): $LINE"; exit 1; fi; echo "COUNTS_DIFFER_MESSAGE_HONEST: $A vs $Bn"; exit 0'
```

## Description

Follow-up to CONTEXT-DOCCHECK (b3b28f45-54b3-4d0e-bde7-933c9c3923b2). Found while
auditing PITFALLS.md §6.1 for task f4bd3c9f-3af8-4438-bcb0-18203b857255 -- explicitly NOT fixed
there, because that task must not edit the instrument that judges its own proof.

Pre-existing at HEAD 2ed05c2, NOT introduced by any current change. Not a live failure: the selftest
is green at HEAD (96/96). It surfaces only when scripts/doc-check.sh's own internal `mktemp` stub is
unavailable in the environment running --selftest -- but the misleading message text is in the code
path regardless of when it fires, and it can mislead whoever is diagnosing the failure that triggers
it.

THE DEFECT: scripts/doc-check.sh's re-entry probe (around line 1176, the
`exported env vars suppressed assertions` printf) compares an env-suppressed child's assertion count
(env_n) against a token-suppressed child's assertion count (tok_n), and prints a FAIL message
claiming env vars SUPPRESSED assertions relative to the token-suppressed baseline -- but when the
underlying cause is something else entirely (an unrelated `mktemp` unavailability affecting BOTH
children equally), the two numbers it quotes are IDENTICAL, so the printed message asserts a
difference it does not demonstrate.

Observed literal output:

    doc-check: SELFTEST FAIL: exported env vars suppressed assertions (84 with them vs 84 for a
    token-suppressed child) -- re-entry must be argv-only

84 vs 84. The comparison at doc-check.sh:1174 (`elif [ "$env_n" -le "$tok_n" ]`) correctly fires on
equality (equal counts ARE a regression relative to what the guard wants: env_n must exceed tok_n),
but the message text at :1176 is worded as though env vars actively suppressed something relative to
the token-suppressed run, which is not what an EQUAL count demonstrates. Either the comparison should
distinguish the equal case from the strictly-less-than case with separate wording, or the message
needs to stop implying a magnitude difference it has not shown.

HOW TO REPRODUCE (verified in a clean extract, twice):

    T=$(mktemp -d); git archive HEAD | tar -x -C "$T"
    # insert `unset -f mktemp` immediately after `set -uo pipefail` in $T/scripts/doc-check.sh
    (cd "$T" && bash scripts/doc-check.sh --selftest 2>&1 | grep 'suppressed assertions')

That variant exits 1 with `4 of 96 assertions did not hold`, and the message above is one of them.
The unmodified script is `SELFTEST PASS: 96/96`, so this surfaces only when the internal mktemp stub
is unavailable -- but the misleading message is in the code path regardless.

WHY THE EQUAL COUNT HAPPENS (root cause, for whoever picks this up): with `mktemp` unset, BOTH the
env-suppressed child (line ~1160/1161, `--internal-reentry-guards`) and the earlier token-suppressed
child (`inner_out`, used for `tok_n`) run under the same broken mktemp, so the mktemp-dependent
guards inside each fail identically in both children -- producing the same reduced count on both
sides. The equal-count message is therefore actually reporting an UNRELATED mktemp-availability
defect, mislabelled as an env-var-suppression defect.

NOT covered by CONTEXT-DOCCHECK-FU-COUNTPIN (5f3a3cd6-f53a-4bb4-a99f-6fe44b3791ac): that task is
about the OUTER top-level --selftest invocation's assertion count being unpinned in the stored
proof_cmd -- a different defect, about a different invocation's count not being asserted at all,
not about this message's wording being misleading when it IS asserted and DOES fire.

Chain: touches scripts/doc-check.sh (the printf message and/or the surrounding comparison, a few
lines) -- reviewer + security, matching CONTEXT-DOCCHECK's own chain note for code changes to this
file (this is exactly the kind of subtle self-referential-instrument defect that epic has gated hard
on every round).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up** [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md) — CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on (done)
- [CONTEXT-DOCCHECK-FU-COUNTPIN](../CONTEXT-DOCCHECK-FU-COUNTPIN--5f3a3cd6/task.md) — CONTEXT-DOCCHECK-FU-COUNTPIN: the outer doc-check.sh --selftest run's assertion count is… (todo)
- [f4bd3c9f-3af8-4438-bcb0-18203b857255](../../PROCESS/Deep-dive-audit-and-refactor-the-repo-s-tracked-.md-file--f4bd3c9f/task.md) — Deep-dive: audit and refactor the repo's tracked .md files, CLAUDE.md primary, fix AGENTS… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
