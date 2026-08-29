# CONTEXT-DOCCHECK-FU-BREAKCOUNT: doc-check.sh:344's 'breaks 20 assertions' is unsourceable -- measured 19/21/23, never 20

| Field | Value |
| --- | --- |
| Public id | `a9e66edb-4e03-414f-9509-2b61c998d647` |
| Key | CONTEXT-DOCCHECK-FU-BREAKCOUNT |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T21:33:12.568686+00:00 |
| Updated | 2026-08-21T21:33:12.568686+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'set -uo pipefail; REPO=$(pwd); CLAIM=$(grep -oE "breaks [0-9]+ assertions" scripts/doc-check.sh | grep -oE "[0-9]+"); if [ -z "$CLAIM" ]; then echo "NUMBER_CLAIM_REMOVED_OR_REWORDED: treat as fixed per the alternative remedy"; exit 0; fi; T=$(mktemp -d); git archive HEAD | tar -x -C "$T"; Q="'\''"; EXPECT="  ${Q} \"\$file\""; LINE=$(sed -n "261p" "$T/scripts/doc-check.sh"); if [ "$LINE" != "$EXPECT" ]; then echo "AWK_CALL_LINE_MOVED (got: $LINE): re-derive the patch line by hand" >&2; rm -rf "$T"; exit 3; fi; NEWLINE="  ${Q} -- \"\$file\""; awk -v n=261 -v repl="$NEWLINE" "NR==n{print repl; next} {print}" "$T/scripts/doc-check.sh" > "$T/scripts/doc-check.sh.new" && mv "$T/scripts/doc-check.sh.new" "$T/scripts/doc-check.sh"; cd "$T"; FULL=$(bash scripts/doc-check.sh --selftest 2>&1 | grep -oE "[0-9]+ of [0-9]+" | tail -1 | grep -oE "^[0-9]+"); GUARDS=$(bash scripts/doc-check.sh --selftest --internal-reentry-guards 2>&1 | grep -oE "[0-9]+ of [0-9]+" | tail -1 | grep -oE "^[0-9]+"); NONE=$(bash scripts/doc-check.sh --selftest --internal-reentry-none 2>&1 | grep -oE "[0-9]+ of [0-9]+" | tail -1 | grep -oE "^[0-9]+"); cd "$REPO"; rm -rf "$T"; echo "MEASURED innermost=$NONE guards=$GUARDS full=$FULL comment_claims=$CLAIM"; case "$CLAIM" in "$NONE"|"$GUARDS"|"$FULL") echo "CLAIM_MATCHES_A_MEASURED_LEVEL: fixed"; exit 0 ;; *) echo "CLAIM_STILL_UNSOURCEABLE: $CLAIM matches none of innermost=$NONE guards=$GUARDS full=$FULL" >&2; exit 1 ;; esac'
```

## Description

Follow-up to CONTEXT-DOCCHECK (b3b28f45-54b3-4d0e-bde7-933c9c3923b2). Filed from the round-4
security re-gate's kind=response (2026-08-21T21:15:02), INFO.

FINDING: scripts/doc-check.sh:344 (comment above the `section_range` awk invocation) reads:
  "NOTE the awk call above deliberately does NOT get `--`: awk stops option parsing at the program
   text, and adding it there breaks 20 assertions."
That specific number, 20, is unsourceable -- it does not correspond to any actual measured count.
Security measured 19, 21 or 23 depending on the selftest's re-entry level (the script's own
`--internal-reentry-none` / `--internal-reentry-guards` / full/no-args levels), and NEVER 20 at any
level. Reproduced independently here by patching a `git archive HEAD` overlay to add ` -- ` to the
awk call at its unique closing line, then running all three selftest levels:
  MEASURED innermost=19 guards=21 full=23 comment_claims=20
This is explicitly the SAME CLASS of defect this file's own header already disclosed and fixed once:
the "eight broken proof commands in a single day" figure that used to open this file's header was
deleted for being unsourceable (see the header's own note about it), because an unverifiable number
inside a proof INSTRUMENT undermines trust in every number the instrument reports. This is that same
defect recurring one paragraph away, inside the same file.

MINIMAL FIX (either is acceptable, security's suggestion): name the specific level the 20-ish number
applies to (it would need to be three numbers, one per level, e.g. "19 at the innermost re-entry
level, 21 with the env-probe guards, 23 at the full top-level run"), OR drop the specific count
entirely and just say the change "takes the selftest red" without a number. Do not simply substitute
a different bare single number (e.g. change 20 to 19) without naming a level -- that repeats the same
defect one digit over, since the true count is level-dependent, not a single value.

Chain: comment-only change in scripts/doc-check.sh -- reviewer only; no security-relevant code path
changes (informational finding, no exploit, no guard being modified).

RED VERIFICATION (spec-keeper filing, 2026-08-21): ran the proof_cmd below against current HEAD. It
re-derives the measurement itself (not just checking wording): it patches a `git archive HEAD`
overlay identically to how security did, adding ` -- ` to the awk call at its known unique line, then
runs `--selftest` at all three re-entry levels and cross-checks the comment's claimed number against
the three actual measured break-counts. Observed:
  MEASURED innermost=19 guards=21 full=23 comment_claims=20
  CLAIM_STILL_UNSOURCEABLE: 20 matches none of innermost=19 guards=21 full=23
proof-check.sh verdict: verdict=FAIL class=other exit=1 -- confirmed RED. The proof is self-verifying
against the live file rather than pinned to today's exact line numbers: if the awk call's exact
closing line ever moves, the proof reports AWK_CALL_LINE_MOVED and exits 3 (re-derive by hand) rather
than silently mis-measuring; if the comment's specific numeric claim is ever removed in favour of the
"just say it takes the selftest red" remedy, the proof reports NUMBER_CLAIM_REMOVED_OR_REWORDED and
exits 0 (treated as fixed, since that is an equally acceptable remedy per the finding).

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

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
