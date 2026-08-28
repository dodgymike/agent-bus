# CONTEXT-DOCCHECK-FU-ENVIRON: the environment can turn any doc-check.sh verdict green -- document it, do NOT unset -f

| Field | Value |
| --- | --- |
| Public id | `a9bf1905-266b-4df6-87d2-e616c70a12b6` |
| Key | CONTEXT-DOCCHECK-FU-ENVIRON |
| Epic | [CONTEXT](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T21:33:06.343284+00:00 |
| Updated | 2026-08-21T21:33:06.343284+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'set -uo pipefail; REPO=$(pwd); T=$(mktemp -d); printf "## Real\n\nreal-needle-here\n" > "$T/doc.md"; ( cd "$T" && bash -c "awk() { echo 1 3; }; export -f awk; bash \"$REPO/scripts/doc-check.sh\" section doc.md TotallyBogusHeadingThatDoesNotExist real-needle-here" ); rc=$?; rm -rf "$T"; echo "EXPLOIT_DEMO exit=$rc (an exported awk turns a nonexistent heading into a PASS; informational, not gated)"; n=$(awk "/^# Three MORE blind spots/{f=1} f && /^#   \* /{c++} /^# \`section\` fixes that by/{f=0} END{print c+0}" scripts/doc-check.sh); if [ "$n" -lt 4 ]; then echo "BLIND_SPOT_LIST_STILL_${n}: header blind-spot list has not gained the ENVIRON-subversion bullet" >&2; exit 1; fi; echo "BLIND_SPOT_LIST_DOCUMENTED: ${n} bullets"'
```

## Description

Follow-up to CONTEXT-DOCCHECK (b3b28f45-54b3-4d0e-bde7-933c9c3923b2). Filed from the round-4
security re-gate's kind=response (2026-08-21T21:15:02), LOW 1.

FINDING: the environment scripts/doc-check.sh runs in can subvert every verdict it produces, and it
cannot be fixed inside the file. Three vectors, each turning a red document green at exit 0 against
HEAD: an exported shell function shadowing grep, sed, awk or wc (bash imports exported functions at
startup); a shim directory earlier on PATH; and the BASH_ENV startup-file variable. Security's own
reproduction: an exported `awk` function that always reports a match at a fixed line range makes
`doc-check.sh section doc.md TotallyBogusHeadingThatDoesNotExist real-needle-here` print
`doc-check: PASS: doc.md -- 1/1 needles inside "TotallyBogusHeadingThatDoesNotExist" (lines 1-3)`,
exit 0 -- a PASS naming a heading that does not exist in the file. Reproduced again here (see RED
verification below), same shape.

SEVERITY / WHY THIS STAYS LOW, NOT A BLOCKER: whoever sets the environment already writes the proof
command and could simply make it exit zero directly, so this adds no NEW capability to a hostile
party who already controls the environment -- it is a documentation gap about the instrument's trust
boundary, not a new hole. Recommended by security: fix forward, do not block CONTEXT-DOCCHECK on it
(and it did not -- CONTEXT-DOCCHECK is closed).

*** DO NOT CLOSE THIS BY ADDING `unset -f grep sed awk wc` (or similar) AT THE TOP OF THE SCRIPT. ***
Security tested that exact mutation. It DOES block the hijack for those names, but it ALSO breaks
the selftest's OWN `wc` and `mktemp` stubs -- the guards that prove security gate MEDIUM 2 (an
unmeasurable file silently counted as within its byte ceiling) and LOW 8 (mktemp failing silently
under container-root, littering `/`) are still caught. Security calls this the invariant-11 shape
exactly: a deletion that reads as hardening while disabling a guard, where every POSITIVE test still
passes -- so a naive fix looks green in `--selftest` while quietly removing real coverage. Whoever
picks this up must read this paragraph before reaching for `unset -f`.

Recommended minimal fix (security's own suggestion): add ONE bullet to the header's existing "Three
MORE blind spots" list (scripts/doc-check.sh, currently 3 items: SETEXT headings, indented ATX
headings, unbalanced fences) naming the ENVIRON-subversion vector by name (exported function /
PATH shim / BASH_ENV) as a fourth documented, accepted limitation. This is a DOCUMENTATION-ONLY fix;
no code path needs to change, and the proof_cmd below gates on exactly that -- it counts the bullets
in that specific header list (via anchor text, not a line number, so it survives unrelated edits
elsewhere in the file) and requires the count to grow from 3 to 4 or more.

NOT covered by CONTEXT-DOCCHECK-FU-COUNTPIN (5f3a3cd6): security read that task and confirmed it is
solely about pinning the outer selftest assertion count; its PATH and grep mentions belong to a
proposed count-matching proof command, not to this environment-subversion vector.

Chain: touches scripts/doc-check.sh (a comment-only change) -- reviewer, not security-blocking (the
fix adds no code path), matching CONTEXT-DOCCHECK's own chain note for doc-only edits. A quick
security sanity check that the added bullet does not itself invite a new mutation is still cheap
insurance and is not discouraged.

RED VERIFICATION (spec-keeper filing, 2026-08-21): ran the proof_cmd below against current HEAD.
It demonstrates the exploit (exported awk turns a nonexistent heading into
`PASS: doc.md -- 1/1 needles inside "TotallyBogusHeadingThatDoesNotExist" (lines 1-3)`, exit 0 --
informational only, not gated, since this vector is accepted as a documentation gap not a code
defect) and then counts the header's "Three MORE blind spots" bullets: 3 today. Output observed:
  doc-check: PASS: doc.md -- 1/1 needles inside "TotallyBogusHeadingThatDoesNotExist" (lines 1-3)
  EXPLOIT_DEMO exit=0 (an exported awk turns a nonexistent heading into a PASS; informational, not gated)
  BLIND_SPOT_LIST_STILL_3: header blind-spot list has not gained the ENVIRON-subversion bullet
proof-check.sh verdict: verdict=FAIL class=other exit=1 -- confirmed RED, non-vacuous (the exploit
demo and the bullet count are both real, reproducible measurements, not a name-not-found stub).

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONTEXT-DOCCHECK](../CONTEXT-DOCCHECK--b3b28f45/task.md) — CONTEXT-DOCCHECK: doc-check.sh -- the instrument every other proof in this epic depends on (done)
- [CONTEXT-DOCCHECK-FU-COUNTPIN](../CONTEXT-DOCCHECK-FU-COUNTPIN--5f3a3cd6/task.md) — CONTEXT-DOCCHECK-FU-COUNTPIN: the outer doc-check.sh --selftest run's assertion count is… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
