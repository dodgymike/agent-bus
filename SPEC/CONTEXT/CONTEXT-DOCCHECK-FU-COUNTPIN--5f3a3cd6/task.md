# CONTEXT-DOCCHECK-FU-COUNTPIN: the outer doc-check.sh --selftest run's assertion count is pinned nowhere

| Field | Value |
| --- | --- |
| Public id | `5f3a3cd6-f53a-4bb4-a99f-6fe44b3791ac` |
| Key | _(null in the export)_ |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T20:24:41.139024+00:00 |
| Updated | 2026-08-21T20:24:41.139024+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'set -e
if [ ! -f scripts/doc-check.sh ]; then echo "doc-check.sh not tracked yet -- proof not runnable until CONTEXT-DOCCHECK lands" >&2; exit 3; fi
T=$(mktemp -d); git archive HEAD | tar -x -C "$T"
if [ ! -f "$T/scripts/doc-check.sh" ]; then echo "doc-check.sh not tracked at HEAD -- proof not runnable until CONTEXT-DOCCHECK lands" >&2; exit 3; fi
sed -i "s/reentry=full ;;/reentry=none ;;/" "$T/scripts/doc-check.sh"
grep -q "reentry=none ;;" "$T/scripts/doc-check.sh" || { echo "mutation pattern reentry=full ;; not found -- doc-check.sh dispatch shape changed, update this proof" >&2; exit 3; }
cp scripts/spec-cloud.sh "$T/scripts/spec-cloud.sh"
CMD=$(bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/tasks/b3b28f45-54b3-4d0e-bde7-933c9c3923b2" | python3 -c "import json,sys; print(json.load(sys.stdin)[\"proof_cmd\"])")
cd "$T"
if eval "$CMD"; then
  echo "STILL VACUOUS: CONTEXT-DOCCHECK stored proof_cmd currently scores a reduced (guards-skipped) top-level selftest run PASS" >&2
  exit 1
fi
echo "FIXED: the stored proof_cmd correctly FAILs when a top-level run silently skips its re-entrant guards"
'
```

## Description

Follow-up to CONTEXT-DOCCHECK (b3b28f45-54b3-4d0e-bde7-933c9c3923b2). Filed per the round-3 reviewer's kind=response, CONDITION 2, explicitly ruled NON-BLOCKING there -- it did not hold up landing CONTEXT-DOCCHECK and should not.

The defect: the outer (top-level, human/agent-facing) run of scripts/doc-check.sh --selftest has its assertion count pinned NOWHERE. Mutating the dispatch's default case arm -- the one that maps a bare `--selftest` invocation (no extra argv) to reentry=full -- so that it instead sets reentry=none, yields a fully GREEN result: doc-check prints "SELFTEST PASS: 71/71 assertions held ... [internal child: re-entrant guards SKIPPED]" and exits 0, silently dropping the 12 re-entrant-guard assertions (83 -> 71) that cover the TMPDIR-injection fix, the mktemp-failure guard, and the other round-2/round-3 hardening. The suffix IS printed and IS self-evidently wrong on a genuine top-level run -- a real improvement over the previous env-var design this replaced, where 56/56 (full) and 49/49 (env-suppressed) were indistinguishable by inspection. But scripts/proof-check.sh judges a wrapper-class proof by EXIT STATUS ALONE (confirmed by reading its header), so nothing mechanical catches the reduced count: the currently-stored CONTEXT-DOCCHECK proof_cmd, `bash scripts/proof-check.sh 'bash scripts/doc-check.sh --selftest'`, scores this mutant PASS.

This is the SAME defect family as the blockers already fixed on CONTEXT-DOCCHECK itself (a false PASS surviving in the instrument that exists specifically to catch false-PASS proofs), one level up: it lives in how the SELFTEST'S OWN outer invocation is verified, not in the selftest's internal assertions, which is exactly why reaching it requires mutating the script's own dispatch (the case arm at the bottom of scripts/doc-check.sh), not any of doc-check's documented section/budget/selftest behaviour.

Cheapest closes, either is acceptable (named by the round-3 reviewer): (a) pin the expected assertion count in the stored proof_cmd, e.g. append `&& bash scripts/doc-check.sh --selftest 2>&1 | grep -q 'NN/NN assertions held'` -- brittle, needs bumping whenever the selftest count changes; or (b) assert the printed scope suffix is EMPTY on a top-level run, e.g. append `&& ! bash scripts/doc-check.sh --selftest 2>&1 | grep -q 'internal child'` -- robust to the count changing, and catches ANY reentry != full on a bare invocation, not just this specific mutation. Prefer (b). Either fix is most naturally a metadata change (spec-keeper updating this task's sibling CONTEXT-DOCCHECK's stored `proof_cmd` field to the augmented form) rather than a code change to scripts/doc-check.sh itself -- the mutation targeted here changes what "bare invocation" MEANS, which is not something the script can validate about its own invocation from the inside.

PROOF DESIGN NOTE, read before objecting to the network call: because the fix is most naturally a Spec Server metadata edit (CONTEXT-DOCCHECK's stored proof_cmd) rather than a repo file, this task's proof_cmd deliberately deviates from the fully-offline clean-overlay shape used by its sibling CONTEXT-DOCCHECK-SYMLINK: it re-fetches CONTEXT-DOCCHECK's CURRENT stored proof_cmd via the sanctioned `scripts/spec-cloud.sh` (never a hand-written curl), applies the reentry=full -> reentry=none mutation to a HEAD overlay of scripts/doc-check.sh, and requires the FETCHED command to now exit non-zero against the mutant. This makes the proof self-updating: whichever fix mechanism (a) or (b) above eventually lands in the stored proof_cmd, this check picks it up automatically without needing to be rewritten. It reuses CONTEXT-DOCCHECK-SYMLINK's three-outcome shape for the not-yet-tracked case (exit 3, not a false RED) since scripts/doc-check.sh is currently staged but uncommitted -- confirmed to be about to land per the operator.

RED verified 2026-08-21 (spec-keeper filing), against a throwaway local commit of the currently-staged scripts/doc-check.sh (the real file is not at HEAD yet, so this was verified against a disposable git commit built solely to exercise the HEAD-tracked code path -- once CONTEXT-DOCCHECK actually lands this reproduces directly against real HEAD): the proof_cmd below printed
  "doc-check: SELFTEST PASS: 71/71 assertions held (3 required by CONTEXT-DOCCHECK, 68 added by gate findings) [internal child: re-entrant guards SKIPPED]"
  "proof-check: verdict=PASS class=wrapper exit=0 ..."
  "STILL VACUOUS: CONTEXT-DOCCHECK stored proof_cmd currently scores a reduced (guards-skipped) top-level selftest run PASS"
and exited 1 -- confirming the currently-stored proof_cmd does not catch the mutation. Before CONTEXT-DOCCHECK lands, running this proof_cmd against a tree with no scripts/doc-check.sh at HEAD exits 3 with "doc-check.sh not tracked at HEAD -- proof not runnable until CONTEXT-DOCCHECK lands" -- also confirmed live, an honest non-runnable signal rather than a vacuous pass.

Chain: the fix is most likely a proof_cmd metadata update (spec-keeper), not a scripts/doc-check.sh code change -- no reviewer/security gate needed for a metadata-only close. If instead implemented as a script change (e.g. baking the suffix assertion into a wrapper script), it needs reviewer + security like its CONTEXT-DOCCHECK-SYMLINK sibling.

Priority P2: non-blocking, does not gate CONTEXT-DOCCHECK's own landing, and the blast radius today is limited to the outer selftest's own self-reported number, not to any of the 11+ stored `doc-check.sh section`/`budget` proof_cmds elsewhere in the CONTEXT epic.

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
- [CONTEXT-DOCCHECK-SYMLINK](../CONTEXT-DOCCHECK-SYMLINK--d7277d6f/task.md) — CONTEXT-DOCCHECK-SYMLINK: physical (realpath) containment for cmd_section -- a symlink st… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [29ac1f66-efb9-4b05-afda-a2775e50f1c6](../gen-spec-mirror-guard-trips-on-column-0-shell-in-two-CON--29ac1f66/task.md) — gen-spec-mirror guard trips on column-0 shell in two CONTEXT-epic task descriptions, bloc… (todo)
- [5cf1edd0-a678-4072-98f9-4c1cb08c7c92](../doc-check.sh-s-re-entry-probe-reports-suppressed-asserti--5cf1edd0/task.md) — doc-check.sh's re-entry probe reports "suppressed assertions" while quoting two equal cou… (todo)
- [CONTEXT-DOCCHECK-FU-ENVIRON](../CONTEXT-DOCCHECK-FU-ENVIRON--a9bf1905/task.md) — CONTEXT-DOCCHECK-FU-ENVIRON: the environment can turn any doc-check.sh verdict green -- d… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
