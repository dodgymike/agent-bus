# CONTEXT-DOCCHECK-FU-DASHFILE: a file argument of a lone '-' is accepted by containment, contradicting the 'a file is a file' guarantee

| Field | Value |
| --- | --- |
| Public id | `1b997e84-abaf-461e-8dee-9229403161d9` |
| Key | CONTEXT-DOCCHECK-FU-DASHFILE |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T21:33:12.264346+00:00 |
| Updated | 2026-08-21T21:33:12.264346+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'set -uo pipefail; REPO=$(pwd); T=$(mktemp -d); ( cd "$T" && printf "## Real\n\nreal-dash-needle\n" > ./- && OUT=$(bash "$REPO/scripts/doc-check.sh" section - Real real-dash-needle < /dev/null 2>&1); RC=$?; case "$OUT" in *"escapes the repo"*) echo "DASH_REJECTED_BY_CONTAINMENT: $OUT"; exit 0 ;; *) echo "DASH_NOT_REJECTED (rc=$RC): $OUT" >&2; exit 1 ;; esac ); rc=$?; rm -rf "$T"; exit $rc'
```

## Description

Follow-up to CONTEXT-DOCCHECK (b3b28f45-54b3-4d0e-bde7-933c9c3923b2). Filed from the round-4
security re-gate's kind=response (2026-08-21T21:15:02), LOW 2.

FINDING: a `<file>` argument to `doc-check.sh section` of a lone `-` is a documented-guarantee gap.
Both `path_is_contained` and the `[ -f "$file" ]` existence check ACCEPT a real file literally named
`-` (confirmed here: `[ -f "-" ]` against a real such file returns true), and GNU sed treats `-` as
standard input EVEN AFTER the `--` operand terminator that the rest of this file's containment
comments rely on (security verified this directly). Security could NOT construct a false PASS from
it: `section_range`'s awk call also receives the SAME `-` argument and ALSO reads it as stdin,
consuming it first -- so with a seekable stdin, awk and sed share the same file offset, awk drains
it looking for the heading, and sed inherits an already-exhausted descriptor, so the section body
comes back empty and the needle is correctly reported ABSENT. It fails SAFE, but only by accident of
composition (two independent programs happening to both treat `-` as stdin, in the right order).

Reproduced here on a real file named `-` in cwd, containing a real heading and needle: with stdin
closed (`< /dev/null`), `doc-check.sh section - Real real-dash-needle` reports
`doc-check: FAIL: heading not found in -: Real` -- the real `-` file, which DOES contain that heading,
is never read at all. That contradicts CONTRACTS-AGENT.md's stated guarantee ("a file is a file") and
the code comment at cmd_section, which enumerates only the four sed-option-shaped names (`-n`, `-s`,
`-z`, `--debug` and similar) as the thing containment defends against -- `-` itself is not one of
them, and is a real risk of a different shape (stdin-swallowing, not option-swallowing).

MINIMAL FIX (security's preferred remedy, over narrowing the doc claim): add `-` (bare dash) as an
explicit reject case in `path_is_contained`'s case arms in scripts/doc-check.sh, so `section -` fails
the SAME "path escapes the repo" containment check that absolute and `..` paths already fail, rather
than silently reading nothing. This makes the stated guarantee ("a file is a file") actually true,
instead of narrowing what is claimed. (Alternative, not preferred: add one clause to the
CONTRACTS-AGENT.md / code-comment guarantee text carving out this one filename -- weaker, since it
documents a hole instead of closing it.)

Chain: touches scripts/doc-check.sh (path_is_contained, ~10 lines) -- reviewer + security, per
CONTEXT-DOCCHECK's own chain note for code changes to this file (containment logic is exactly what
security gated hardest on across all four rounds).

RED VERIFICATION (spec-keeper filing, 2026-08-21): ran the proof_cmd below against current HEAD. It
creates a real file named `-` in a throwaway directory containing a real heading and needle, then
calls `doc-check.sh section - Real real-dash-needle` with stdin closed, and checks whether the output
names a containment rejection ("escapes the repo"). Observed:
  DASH_NOT_REJECTED (rc=1): doc-check: FAIL: heading not found in -: Real
proof-check.sh verdict: verdict=FAIL class=other exit=1 -- confirmed RED: today the dash is silently
treated as an unreadable stdin swap, not rejected by containment. After the fix the same proof_cmd
must print `DASH_REJECTED_BY_CONTAINMENT: doc-check: FAIL: path escapes the repo (absolute or ..): -`
and exit 0.

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
