#!/usr/bin/env bash
# Regenerate SPEC.md — the mirror of the Spec Server backlog.
#
# This is the ONLY supported way to write SPEC.md. It replaces the bare
#     bash scripts/spec-cloud.sh -s $B/projects/agent-bus/export > SPEC.md
# which mirrored EVERY task including closed ones.
#
# WHY FILTER. The mirror exists so an agent (or a human without Spec Server
# credentials) can see what is left to do. Closed tasks do not serve that
# purpose, and they were 39% of the tasks and 38% of the description bytes --
# roughly 250 KB of a 658 KB file. Nothing is lost: the Spec Server remains the
# source of truth and still holds every closed task with its full history, and
# `--all` below reproduces the old unfiltered output on demand.
#
# WHAT IS KEPT: todo, in_progress, blocked, deferred -- i.e. everything a reader
# could still act on. Dropped: done, cancelled, superseded.
#
# An epic whose tasks are ALL closed is dropped too, heading included; an epic
# heading with nothing under it is noise that reads like an omission.
#
# Usage:
#   bash scripts/gen-spec-mirror.sh            # write SPEC.md (filtered)
#   bash scripts/gen-spec-mirror.sh --all      # write SPEC.md unfiltered (old behaviour)
#   bash scripts/gen-spec-mirror.sh --stdout   # print, do not write
set -euo pipefail

cd "$(dirname "$0")/.."

KEEP_ALL=0
TO_STDOUT=0
for arg in "$@"; do
  case "$arg" in
    --all) KEEP_ALL=1 ;;
    --stdout) TO_STDOUT=1 ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "gen-spec-mirror: unknown option: $arg" >&2; exit 2 ;;
  esac
done

raw="$(mktemp)"
trap 'rm -f "$raw" "$raw.out"' EXIT

# Fail loudly rather than truncating SPEC.md to an error page. A mirror that is
# silently replaced by "401 Unauthorized" is worse than a stale one, because it
# looks like an empty backlog.
if ! bash scripts/spec-cloud.sh -sf "/api/v1/projects/agent-bus/export" > "$raw"; then
  echo "gen-spec-mirror: export failed; SPEC.md NOT modified" >&2
  exit 1
fi
if [ ! -s "$raw" ]; then
  echo "gen-spec-mirror: export was empty; SPEC.md NOT modified" >&2
  exit 1
fi
if ! head -1 "$raw" | grep -q '^# '; then
  echo "gen-spec-mirror: export does not look like the markdown mirror (no leading '# '); SPEC.md NOT modified" >&2
  head -3 "$raw" >&2
  exit 1
fi

if [ "$KEEP_ALL" = 1 ]; then
  cp "$raw" "$raw.out"
else
  python3 - "$raw" "$raw.out" <<'PY'
import re, sys

src, dst = sys.argv[1], sys.argv[2]
lines = open(src, encoding="utf-8").read().splitlines(keepends=True)

# A task starts with "- [<mark>] " at column 0. Its continuation is every
# following line until the next task, the next heading, or EOF. Closed marks are
# "x" (done) and "-" (superseded/cancelled); "~" is in progress and is KEPT.
TASK = re.compile(r"^- \[(.)\] ")
HEAD = re.compile(r"^#{1,6} ")
CLOSED = {"x", "-"}

blocks = []          # (kind, mark, [lines])  kind in {"other","task","epic"}
cur = ["other", None, []]
for ln in lines:
    m = TASK.match(ln)
    if m:
        blocks.append(cur)
        cur = ["task", m.group(1), [ln]]
    elif HEAD.match(ln):
        blocks.append(cur)
        kind = "epic" if ln.startswith("### EPIC ") else "other"
        cur = [kind, None, [ln]]
    else:
        cur[2].append(ln)
blocks.append(cur)

kept, dropped = 0, 0
out, pending_epic = [], None
for kind, mark, buf in blocks:
    if kind == "task":
        if mark in CLOSED:
            dropped += 1
            continue
        kept += 1
        if pending_epic is not None:
            out.extend(pending_epic)
            pending_epic = None
        out.extend(buf)
    elif kind == "epic":
        # Hold the heading until a live task appears under it.
        pending_epic = buf
    else:
        if pending_epic is not None:
            pending_epic = None      # epic ended with no live task: drop it
        out.extend(buf)

note = (
    "\n> This mirror lists OPEN tasks only (todo, in progress, blocked, deferred).\n"
    f"> {dropped} closed tasks are omitted; the Spec Server holds them in full.\n"
    "> Regenerate with `bash scripts/gen-spec-mirror.sh` (`--all` to include closed).\n"
)
# Insert after the title line so it is the first thing a reader sees.
for i, ln in enumerate(out):
    if ln.startswith("# "):
        out.insert(i + 1, note)
        break

open(dst, "w", encoding="utf-8").write("".join(out))
sys.stderr.write(f"gen-spec-mirror: kept {kept} open, dropped {dropped} closed\n")
PY
fi

# CROSS-CHECK AGAINST THE AUTHORITATIVE STATUS COUNTS, and refuse to write on a mismatch.
#
# This is not belt-and-braces. The filter parses RENDERED MARKDOWN, and a task
# DESCRIPTION is free-form text written by agents -- so a description containing
# a nested checklist (`- [ ]` at column 0), or an unindented fenced block whose
# content starts with `# `, is indistinguishable from real structure. A reviewer
# reproduced all three failure shapes on synthetic input:
#
#   - a col-0 `- [x]` inside a LIVE task silently TRUNCATES that task, dropping
#     the rest of its description INCLUDING its `_Proof:_` line;
#   - a col-0 `# ` inside a closed task drops the FOLLOWING epic heading even
#     though live tasks sit under it, and leaks the closed task's content;
#   - a col-0 `- [ ]` inside a CLOSED task invents a phantom live task.
#
# Every one of those is a SILENT corruption of a file people read to decide what
# to work on, and invariant 6's doctrine is that silent failure is the defect.
#
# TWO GUARDS, BECAUSE ONE CANNOT COVER ALL THREE. This distinction is the whole
# point and an earlier version of this comment got it wrong -- it claimed the
# counts caught all three, which is exactly the kind of false all-clear this
# repo keeps having to delete from its own code:
#
#   - The COUNT cross-check below catches (a) and (c). Both change the number of
#     task marks, so comparing the parser's tally against the server's `status`
#     field makes them loud.
#   - It CANNOT catch (b). A stray `# ` inside a closed task changes NO task-mark
#     count, so no tally can see it. That shape needs the STRUCTURAL assertion on
#     the raw export instead: exactly one `^# `, every `^### ` is an `### EPIC `,
#     and no other unexpected non-blank column-0 line.
#
# The structural check is not belt-and-braces either. Four task descriptions in
# the backlog TODAY contain lines starting `## ` or `### ` -- they are saved only
# by the exporter indenting descriptions two spaces, an upstream property nothing
# here controls and nothing asserted until now. One renderer change and (b) fires
# on live data.
#
# It also closes a hole the header check above does NOT: `curl -sf` exits 0 on a
# cleanly-terminated but SHORT chunked response, so a truncated-yet-well-formed
# mirror would otherwise pass `head -1 | grep '^# '` and overwrite SPEC.md.
if [ "$KEEP_ALL" = 0 ]; then
  # STRUCTURAL CHECK — catches shape (b), which the counts cannot. Runs on the
  # RAW export, so it is race-free: no second fetch, nothing to churn.
  bad_struct="$(awk '
    /^# /      { h1++ }
    /^### /    { if ($0 !~ /^### EPIC /) nonepic++ }
    /^[^ \t]/  { if ($0 !~ /^# / && $0 !~ /^#{2,6} / && $0 !~ /^- \[.\] / && $0 !~ /^> /) other++ }
    END { if (h1 != 1) printf "  top-level `# ` headings: %d (expected exactly 1)\n", h1
          if (nonepic > 0) printf "  `### ` headings that are not `### EPIC `: %d\n", nonepic
          if (other > 0)   printf "  unexpected non-blank column-0 lines: %d\n", other }
  ' "$raw")"
  if [ -n "$bad_struct" ]; then
    echo "gen-spec-mirror: REFUSING TO WRITE — the export is not the shape this filter parses." >&2
    printf '%s\n' "$bad_struct" >&2
    echo "  Most likely a task DESCRIPTION contains a column-0 markdown heading or fence," >&2
    echo "  which this filter would mistake for document structure and silently drop an" >&2
    echo "  epic heading (taking its live tasks with it). Indent it, or fix the exporter." >&2
    echo "  SPEC.md is unchanged." >&2
    exit 1
  fi

  counts="$(bash scripts/spec-cloud.sh -sf "/api/v1/projects/agent-bus/export?format=json" 2>/dev/null | python3 -c '
import json,sys
try: ts = json.load(sys.stdin)["tasks"]
except Exception: sys.exit(3)
closed = {"done","cancelled","superseded"}
o = sum(1 for t in ts if t.get("status") not in closed)
print(o, len(ts) - o)
' 2>/dev/null)" || counts=""
  if [ -z "$counts" ]; then
    echo "gen-spec-mirror: WARNING could not fetch JSON status counts; wrote nothing" >&2
    exit 1
  fi
  want_open="${counts% *}"; want_closed="${counts#* }"
  got_open="$(grep -c '^- \[ \]\|^- \[~\]' "$raw.out" || true)"
  got_closed="$(grep -c '^- \[x\]\|^- \[-\]' "$raw" || true)"
  # The two fetches are NOT simultaneous -- the markdown was fetched above, the
  # JSON just now, with the filter in between. Concurrent agents file tasks
  # constantly (observed: 345 -> 346 between two fetches), so a mismatch is
  # ambiguous: it may be corruption, or merely churn. Re-fetch BOTH once and
  # retry before failing, because the failure message blames a malformed
  # description and would otherwise send someone hunting for one that does not
  # exist. Two disagreements in a row is not churn.
  if [ "$got_open" != "$want_open" ] || [ "$got_closed" != "$want_closed" ]; then
    sleep 2
    if bash scripts/spec-cloud.sh -sf "/api/v1/projects/agent-bus/export" > "$raw.retry" 2>/dev/null; then
      recounts="$(bash scripts/spec-cloud.sh -sf "/api/v1/projects/agent-bus/export?format=json" 2>/dev/null | python3 -c '
import json,sys
try: ts = json.load(sys.stdin)["tasks"]
except Exception: sys.exit(3)
closed = {"done","cancelled","superseded"}
o = sum(1 for t in ts if t.get("status") not in closed)
print(o, len(ts) - o)
' 2>/dev/null)" || recounts=""
      if [ -n "$recounts" ]; then
        want_open="${recounts% *}"; want_closed="${recounts#* }"
        got_closed="$(grep -c '^- \[x\]\|^- \[-\]' "$raw.retry" || true)"
        if [ "$got_closed" = "$want_closed" ]; then
          echo "gen-spec-mirror: counts moved between fetches (concurrent filing); re-run to pick up the change" >&2
          rm -f "$raw.retry"
          exit 1
        fi
      fi
      rm -f "$raw.retry"
    fi
    echo "gen-spec-mirror: REFUSING TO WRITE — parser/server disagree, twice." >&2
    echo "  open:   parser kept $got_open, server says $want_open" >&2
    echo "  closed: parser saw  $got_closed, server says $want_closed" >&2
    echo "  A task DESCRIPTION most likely contains a column-0 '- [ ]' checklist or an" >&2
    echo "  unindented fenced block. Find it, indent it, and re-run. SPEC.md is unchanged." >&2
    exit 1
  fi
  # Any closed task surviving the filter is a parse failure by definition.
  if grep -q '^- \[x\]\|^- \[-\]' "$raw.out"; then
    echo "gen-spec-mirror: REFUSING TO WRITE — closed tasks survived the filter" >&2
    exit 1
  fi
fi

if [ "$TO_STDOUT" = 1 ]; then
  cat "$raw.out"
  exit 0
fi

before=0
[ -f SPEC.md ] && before=$(wc -c < SPEC.md)
cp "$raw.out" SPEC.md
after=$(wc -c < SPEC.md)
echo "gen-spec-mirror: SPEC.md ${before} -> ${after} bytes"
