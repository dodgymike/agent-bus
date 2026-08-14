#!/usr/bin/env bash
# Regenerate the Spec Server backlog mirror — SPEC.md plus the SPEC/ tree.
#
# This is the ONLY supported way to write SPEC.md or SPEC/. It replaces the bare
#     bash scripts/spec-cloud.sh -s $B/projects/agent-bus/export > SPEC.md
# which mirrored the whole backlog into one file.
#
# WHAT IT EMITS (CONTEXT-SPEC-TREE, 2026-08-14):
#
#   SPEC.md                        epic index ONLY: one row per epic with a
#                                  one-line summary, open/total counts and a
#                                  pointer. No task descriptions. Target < 40 KB.
#   SPEC/<EPIC>/epic.md            that epic's tasks: key, title, status,
#                                  priority, pointer to the task file, and
#                                  DERIVED references. Open tasks first, closed
#                                  tasks in a clearly separated section.
#   SPEC/<EPIC>/<task>/task.md     the full record, description UNTRUNCATED.
#
# WHY. The old single-file mirror was 642,570 B, and 92% of those bytes were task
# descriptions. Nothing reads it for task content — agents get descriptions from
# `claim-next`; the mirror exists for navigation and as the server-down fallback.
# A tree makes navigation cheap WITHOUT truncating anything, which is the one
# thing the fallback case cannot tolerate.
#
# CLOSED TASKS ARE INCLUDED. The old mirror dropped them (39% of tasks) to save
# bytes. In a tree they cost nothing until a file is opened, and their absence
# was the main reason the server-down fallback was worse than the server itself.
# `--all` is therefore a no-op, kept only so old invocations do not fail.
#
# THE TREE IS BUILT FROM THE JSON EXPORT, not by parsing rendered markdown. That
# alone removes the whole class of silent corruptions the guards below were
# written for (see GUARDS). The markdown export is still fetched, because it is
# what the guards cross-check against.
#
# TWO KINDS OF TASK-TO-TASK LINK, NEVER BLURRED:
#   REAL RELATIONS  — fetched per task from `/tasks/<id>/relations`
#                     ({kind: blocks|supersedes|relates|follow_up, direction, task}).
#                     Authoritative. There is no bulk endpoint (checked 2026-08-14:
#                     `/relations` 404s, `?include=relations` is ignored on both `/export`
#                     and `/tasks`), so it costs ONE REQUEST PER TASK against an API that
#                     rate-limits: measured, 8-way parallel lost 40 of 517 requests to HTTP
#                     429, and even a serial loop at ~5.7 req/s lost 13 of 40. Hence two
#                     workers and exponential backoff — unhurried on purpose.
#                     ON by default: measured on the live backlog, 237 of 518 tasks carry
#                     at least one real edge (706 edges: blocks, supersedes, relates,
#                     follow_up), so this is the most valuable content in the tree, not a
#                     rare extra. Paced at 2 workers it completes in ~70s. `--no-relations`
#                     skips it for a fast regen, and the tree then says so IN EVERY FILE
#                     ("NOT FETCHED — unknown, not absent"), because an unstated gap is the
#                     thing this repo keeps having to delete.
#   DERIVED REFS    — task keys, title prefixes and public-id fragments matched out of
#                     free-text descriptions. Best-effort, NOT authoritative, and labelled
#                     as such everywhere. They stay because they catch the ~280 tasks that
#                     name a sibling in prose without anyone filing an edge for it.
# Nothing here TRAVERSES the graph — only direct edges are rendered — because the server
# accepts cycles (A blocks B and B blocks A both return 201, verified 2026-08-14). Any
# future traversal added here MUST carry a visited set; the graph is not a DAG.
#
# Usage:
#   bash scripts/gen-spec-mirror.sh                  # write SPEC.md + SPEC/ (~70s, with relations)
#   bash scripts/gen-spec-mirror.sh --no-relations   # fast regen; the tree says edges are UNKNOWN
#   bash scripts/gen-spec-mirror.sh --stdout         # print the SPEC.md index, write nothing
#   bash scripts/gen-spec-mirror.sh --all            # accepted, no-op (the tree is always complete)
set -euo pipefail

cd "$(dirname "$0")/.."

TO_STDOUT=0
FETCH_RELATIONS=1
for arg in "$@"; do
  case "$arg" in
    --all) echo "gen-spec-mirror: --all is a no-op; the tree always contains every task" >&2 ;;
    --relations) FETCH_RELATIONS=1 ;;
    --no-relations) FETCH_RELATIONS=0 ;;
    --stdout) TO_STDOUT=1 ;;
    -h|--help) sed -n '2,63p' "$0"; exit 0 ;;   # 63 = last Usage line; keep in step
    *) echo "gen-spec-mirror: unknown option: $arg" >&2; exit 2 ;;
  esac
done

raw="$(mktemp)"
rawjson="$(mktemp)"
# The build directory MUST be a sibling of SPEC/ on the same filesystem, because
# the swap at the end is a rename. It is gitignored (/.spec-tree.*).
build="$(mktemp -d ./.spec-tree.XXXXXX)"
# $rescue holds the PREVIOUS SPEC/ during the swap. It is in the trap because a crash
# between the two renames would otherwise leave the only copy of the old tree in a
# gitignored directory nobody knows to look in.
rescue=""
trap 'rm -f "$raw" "$rawjson"; rm -rf "$build"
      if [ -n "$rescue" ]; then
        [ -e SPEC ] || mv "$rescue/SPEC" SPEC     # swap died mid-rename: put it back
        rm -rf "$rescue"
      fi' EXIT

# fetch_exports — pull the markdown and JSON exports into $raw / $rawjson and
# leave the four counts in the md_*/js_* globals. Fail loudly rather than
# truncating the mirror to an error page: a mirror silently replaced by
# "401 Unauthorized" is worse than a stale one, because it looks like an empty
# backlog.
md_open=0; md_closed=0; js_open=0; js_closed=0
fetch_exports() {
  if ! bash scripts/spec-cloud.sh -sf "/api/v1/projects/agent-bus/export" > "$raw"; then
    echo "gen-spec-mirror: markdown export failed; mirror NOT modified" >&2
    return 1
  fi
  if [ ! -s "$raw" ]; then
    echo "gen-spec-mirror: markdown export was empty; mirror NOT modified" >&2
    return 1
  fi
  if ! head -1 "$raw" | grep -q '^# '; then
    echo "gen-spec-mirror: markdown export does not look like the mirror (no leading '# '); mirror NOT modified" >&2
    head -3 "$raw" >&2
    return 1
  fi
  if ! bash scripts/spec-cloud.sh -sf "/api/v1/projects/agent-bus/export?format=json" > "$rawjson"; then
    echo "gen-spec-mirror: JSON export failed; mirror NOT modified" >&2
    return 1
  fi
  local counts
  counts="$(python3 -c '
import json,sys
try: ts = json.load(open(sys.argv[1], encoding="utf-8"))["tasks"]
except Exception: sys.exit(3)
closed = {"done","cancelled","superseded"}
o = sum(1 for t in ts if t.get("status") not in closed)
print(o, len(ts) - o)
' "$rawjson")" || {
    echo "gen-spec-mirror: JSON export did not parse; mirror NOT modified" >&2
    return 1
  }
  js_open="${counts% *}"; js_closed="${counts#* }"
  md_open="$(grep -c '^- \[ \]\|^- \[~\]' "$raw" || true)"
  md_closed="$(grep -c '^- \[x\]\|^- \[-\]' "$raw" || true)"
}

fetch_exports || exit 1

# ---------------------------------------------------------------------------
# GUARDS. Three of them, and none subsumes another.
#
# HISTORY, because the reason they exist is not obvious from the code. The old
# mirror was produced by FILTERING RENDERED MARKDOWN, and a task DESCRIPTION is
# free-form text written by agents — so a description containing a column-0
# `- [ ]` checklist, or an unindented fence whose content starts with `# `, was
# indistinguishable from real document structure. A reviewer reproduced three
# failure shapes on synthetic input:
#
#   (a) a col-0 `- [x]` inside a LIVE task silently TRUNCATED that task,
#       dropping the rest of its description including its `_Proof:_` line;
#   (b) a col-0 `# ` inside a CLOSED task dropped the FOLLOWING epic heading
#       even though live tasks sat under it, and leaked the closed task's body;
#   (c) a col-0 `- [ ]` inside a CLOSED task invented a phantom live task.
#
# The tree is built from the JSON export, so (a), (b) and (c) can no longer
# corrupt the OUTPUT — that is a real reduction in exposure, not a reason to
# drop the guards. They are KEPT because what they actually detect is an
# unhealthy EXPORT, and the JSON path is not immune to that:
#
#   1. STRUCTURAL CHECK on the raw markdown — catches (b), which no tally can
#      see, because a stray `# ` changes no task-mark count. It also catches
#      what the `head -1` check cannot: `curl -sf` exits 0 on a cleanly
#      terminated but SHORT chunked response, so a truncated-yet-well-formed
#      export would otherwise sail through. THAT CLAIM IS WRONG and is left here
#      only to be corrected: a reviewer truncated the live 993 KB export to 25,
#      50, 75 and 90% and guard 1 passed all four times. TRUNCATION IS CAUGHT BY
#      GUARD 2, the count cross-check, not by this one.
#      BE PRECISE ABOUT WHAT ELSE IT DOES NOT CATCH — this comment claimed more than
#      the awk delivers and a reviewer measured it (2026-08-14). It fires on: a
#      `# ` count other than exactly 1, a `### ` that is not `### EPIC `, and any
#      other non-blank column-0 line that is not a heading, task mark or quote.
#      A column-0 `## `, `#### ` or `###### ` smuggled out of a description
#      passes SILENTLY, because the `other` rule excludes `^#{2,6} ` — it must,
#      since the export's own `## In Progress` / `## Backlog` headings live at
#      column 0. So it is a partial canary for the exporter dropping description
#      indentation, not a complete one.
#   2. COUNT CROSS-CHECK — catches (a) and (c), and now does more than before:
#      it compares the two INDEPENDENT representations of the same backlog
#      (markdown marks vs JSON statuses), so a short read of EITHER is loud.
#   3. NO-TASK-LOST CHECK (new, and required by the tree) — every task must get
#      exactly one directory. The tree adds a second route to a silently dropped
#      task that no markdown-era guard could see: two tasks whose names slug to
#      the same string, one overwriting the other. So the builder asserts
#      emitted-file count == input-task count, and open/closed tallies match the
#      server's, and refuses to swap anything in on a mismatch.
# ---------------------------------------------------------------------------

bad_struct="$(awk '
  /^# /      { h1++ }
  /^### /    { if ($0 !~ /^### EPIC /) nonepic++ }
  /^[^ \t]/  { if ($0 !~ /^# / && $0 !~ /^#{2,6} / && $0 !~ /^- \[.\] / && $0 !~ /^> /) other++ }
  END { if (h1 != 1) printf "  top-level `# ` headings: %d (expected exactly 1)\n", h1
        if (nonepic > 0) printf "  `### ` headings that are not `### EPIC `: %d\n", nonepic
        if (other > 0)   printf "  unexpected non-blank column-0 lines: %d\n", other }
' "$raw")"
if [ -n "$bad_struct" ]; then
  echo "gen-spec-mirror: REFUSING TO WRITE — the markdown export is not the shape this script expects." >&2
  printf '%s\n' "$bad_struct" >&2
  echo "  Most likely the export is truncated, or a task DESCRIPTION contains a column-0" >&2
  echo "  markdown heading or fence and the exporter stopped indenting descriptions." >&2
  echo "  SPEC.md and SPEC/ are unchanged." >&2
  exit 1
fi

# The two exports are NOT fetched simultaneously. Concurrent agents file tasks
# constantly (observed: 345 -> 346 between two fetches), so a single mismatch is
# ambiguous — corruption, or merely churn. Re-fetch BOTH once; two disagreements
# in a row is not churn.
if [ "$md_open" != "$js_open" ] || [ "$md_closed" != "$js_closed" ]; then
  first="md=$md_open/$md_closed json=$js_open/$js_closed"
  sleep 2
  fetch_exports || exit 1
  if [ "$md_open" != "$js_open" ] || [ "$md_closed" != "$js_closed" ]; then
    echo "gen-spec-mirror: REFUSING TO WRITE — the two exports disagree, twice." >&2
    echo "  first attempt:  $first" >&2
    echo "  second attempt: md=$md_open/$md_closed json=$js_open/$js_closed  (open/closed)" >&2
    echo "  Either the export is truncated, or a task DESCRIPTION contains a column-0" >&2
    echo "  '- [ ]' checklist that the markdown renderer counts as a task." >&2
    echo "  SPEC.md and SPEC/ are unchanged." >&2
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
# REAL RELATIONS (--relations). One request per task against a rate-limited API,
# so: two workers, and each worker backs off 1s, 3s, 9s, 27s between attempts.
# That is deliberately unhurried — the Spec Server is shared with every other
# agent in this repo, and a mirror regen is not worth starving them of quota.
#
# A task whose relations cannot be fetched is a REFUSAL, not an empty list:
# rendering "no relations" for a task that has them is exactly the silent lie
# the guards below exist to prevent.
#
# The ids are re-validated as UUIDs before they reach xargs. They come from the
# server, and they are about to be interpolated into a URL and a filename inside
# a shell command — an id containing a quote or a slash must never get that far.
# ---------------------------------------------------------------------------
reldir="$build/relations"
mkdir -p "$reldir"
relations_note="fetched"
if [ "$FETCH_RELATIONS" = 1 ]; then
  ids="$build/ids.txt"
  python3 -c '
import json,re,sys
ok = re.compile(r"\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\Z")
ts = json.load(open(sys.argv[1], encoding="utf-8"))["tasks"]
bad = [t.get("public_id") for t in ts if not ok.match(str(t.get("public_id") or ""))]
if bad:
    sys.stderr.write("gen-spec-mirror: non-UUID public_id from the server: %r\n" % bad[:3])
    sys.exit(1)
print("\n".join(t["public_id"] for t in ts))
' "$rawjson" > "$ids"
  cat > "$build/fetch-rel.sh" <<'SH'
#!/usr/bin/env bash
# $REL_OUT = output dir (environment, never argv), $1 = task public_id
# (UUID-validated by the caller). Backs off on failure: the server answers 429
# well before 500-odd requests are done.
set -u
out="$REL_OUT/$1.json"
delay=1
for attempt in 1 2 3 4 5; do
  if bash scripts/spec-cloud.sh -sf "/api/v1/projects/agent-bus/tasks/$1/relations" > "$out" \
     && [ -s "$out" ] && [ "$(head -c1 "$out")" = "[" ]; then
    exit 0
  fi
  sleep "$delay"
  delay=$((delay * 3))
done
echo "$1" >> "$REL_OUT/FAILED"
exit 0
SH
  echo "gen-spec-mirror: fetching relations for $(wc -l < "$ids") tasks (rate-limited, ~70s)…" >&2
  REL_OUT="$reldir" xargs -a "$ids" -P 2 -n 1 bash "$build/fetch-rel.sh" 2> "$build/relfetch.log"
  if [ -s "$reldir/FAILED" ]; then
    echo "gen-spec-mirror: REFUSING TO WRITE — relations fetch failed for $(wc -l < "$reldir/FAILED") task(s):" >&2
    head -5 "$reldir/FAILED" >&2
    tail -3 "$build/relfetch.log" >&2
    echo "  Rendering them as 'no relations' would assert something unverified." >&2
    echo "  Re-run, or drop --relations to publish a tree that says so explicitly." >&2
    exit 1
  fi
else
  relations_note="skipped"
  echo "gen-spec-mirror: --no-relations: real edges will be rendered UNKNOWN, not absent" >&2
fi

# ---------------------------------------------------------------------------
# BUILD. Writes $build/SPEC.md and $build/SPEC/... — never touches the live
# tree. Guard 3 lives inside the builder, which exits non-zero on any breach.
# ---------------------------------------------------------------------------
if ! python3 - "$rawjson" "$build" "$js_open" "$js_closed" "$reldir" "$relations_note" <<'PY'
import hashlib, json, os, re, sys, unicodedata

src, build, want_open, want_closed = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
rel_dir, rel_state = sys.argv[5], sys.argv[6]

data = json.load(open(src, encoding="utf-8"))
tasks = data["tasks"]
epics = {e["key"]: e for e in data.get("epics", []) if e.get("key")}

CLOSED = {"done", "cancelled", "superseded"}
NO_EPIC = "UNASSIGNED"           # 6 tasks carry no epic_key; they get a bucket, not the bin
# A key-looking prefix in the title, e.g. "CONTEXT-SPEC-TREE: Split ...". 185 of
# 511 tasks have key = null and carry their identity here instead. spec-keeper
# owns fixing that; this script's job is to be correct in its presence.
TITLE_TOK = re.compile(r"^([A-Z][A-Z0-9]*(?:-[A-Z0-9]+)+)\s*[:—-]")
# public_id is the identity every path is built from, so it is VALIDATED, not
# trusted: a non-UUID here would be a server-side surprise reaching a filename.
# \A..\Z, not ^..$: Python's `$` also matches before a trailing newline, which
# would put a line break inside the code span this id is rendered in.
UUID = re.compile(r"\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\Z")
# Windows-reserved device names. This tree is checked into git and will be
# cloned on Windows; a directory named `CON` or `NUL` is not creatable there.
WINRES = {"CON", "PRN", "AUX", "NUL", "COM0", "LPT0"} | {
    f"{p}{i}" for p in ("COM", "LPT") for i in range(1, 10)
}


def slug(text, maxlen=56):
    """Path-safe component. Slugs are NOT trusted -- see the containment
    assertion in place() -- but they are made boring first: no separators, no
    traversal, no leading dash, no NUL, no Windows device name."""
    text = unicodedata.normalize("NFKD", text or "")
    text = "".join(c for c in text if c.isprintable())
    text = re.sub(r"[^A-Za-z0-9._-]+", "-", text)
    text = re.sub(r"-{2,}", "-", text).strip("-._")[:maxlen].strip("-._")
    if text.split(".")[0].upper() in WINRES:
        text = "_" + text
    return text


def esc(text, limit=None):
    """One-line, INERT rendering of server-supplied text.

    Everything this touches — titles, keys, tags, relation peer names — is
    written by agents into the Spec Server and lands inside markdown link text
    and table cells here. Escaping only `|` was not enough: a security re-check
    (2026-08-14) demonstrated a peer key of `X](https://evil/steal)[y` closing
    the link early and rendering an ATTACKER-CHOSEN URL, and an asymmetric
    backtick run escaping a code span to render a live image and link. These are
    files agents read as authoritative navigation, so that is a phishing and
    prompt-injection surface in trusted committed docs.

    Truncation happens BEFORE escaping, so a cut can never land inside an escape
    sequence and leave a dangling backslash.

    NOT covered, deliberately: a task DESCRIPTION and its STATUS NOTE in
    task.md, and the EPIC DESCRIPTION in epic.md — all copied verbatim because
    reproducing the record exactly is the whole point of the tree — and they were equally verbatim in the old single-file
    mirror, so this is not new exposure. The proof_cmd is fenced with a fence
    longer than any backtick run inside it, so it cannot break out.
    """
    text = re.sub(r"\s+", " ", (text or "")).strip()
    if limit and len(text) > limit:
        text = text[: limit - 1].rstrip() + "…"
    text = (text.replace("\\", "\\\\")
                .replace("|", "\\|").replace("`", "\\`")
                .replace("[", "\\[").replace("]", "\\]")
                .replace("<", "&lt;").replace(">", "&gt;"))
    if text.startswith("!"):          # `![...]` is an image; a leading ! is enough
        text = "\\" + text
    return text


# ---- pass 1: identity, paths, reference index --------------------------------
by_pid = {}
tok_index = {}          # key-ish token -> [task, ...]
pid8_index = {}         # 8-hex public_id prefix -> [task, ...]

for t in tasks:
    pid = t["public_id"]
    if not UUID.match(str(pid)):
        raise SystemExit(f"gen-spec-mirror: task public_id is not a UUID: {pid!r}")
    tok = t.get("key") or (TITLE_TOK.match(t.get("title") or "") or [None, None])[1]
    t["_tok"] = tok
    t["_label"] = tok or pid
    t["_epic"] = t.get("epic_key") or NO_EPIC
    t["_closed"] = t.get("status") in CLOSED
    by_pid[pid] = t
    for k in {t.get("key"), tok} - {None}:
        tok_index.setdefault(k, []).append(t)
    pid8_index.setdefault(pid[:8], []).append(t)

spec_root = os.path.join(build, "SPEC")
os.makedirs(spec_root)
real_root = os.path.realpath(spec_root)
used = set()

# One directory per epic, allocated up front so a slug collision between two
# epic keys cannot silently merge two epics into one epic.md.
epic_dirs = {}
for ekey in sorted({t["_epic"] for t in tasks} | set(epics)):
    d = slug(ekey) or "epic"
    if d in set(epic_dirs.values()):
        d = f"{d}-{hashlib.sha1(ekey.encode()).hexdigest()[:8]}"
    epic_dirs[ekey] = d


def place(t):
    """Assign each task a unique directory. IDENTITY IS public_id -- the
    readable part is decoration, because `key` is null for 36% of the backlog
    and titles collide. The public_id fragment is always appended so two tasks
    can never land on the same path, and the result is asserted to resolve
    INSIDE SPEC/ rather than trusted."""
    pid = t["public_id"]
    epic_dir = epic_dirs[t["_epic"]]
    readable = slug(t["_tok"] or t.get("title") or "")
    name = f"{readable}--{pid[:8]}" if readable else pid
    if (epic_dir, name) in used:           # 8-hex prefixes are unique today; do not assume it
        name = f"{readable}--{pid}" if readable else pid
    if (epic_dir, name) in used:
        raise SystemExit(f"gen-spec-mirror: duplicate task directory {epic_dir}/{name}")
    used.add((epic_dir, name))
    rel = os.path.join(epic_dir, name)
    resolved = os.path.realpath(os.path.join(spec_root, rel))
    if not resolved.startswith(real_root + os.sep):
        raise SystemExit(f"gen-spec-mirror: task {pid} escapes SPEC/: {rel!r}")
    t["_dir"] = rel
    t["_epicdir"] = epic_dir


for t in tasks:
    place(t)

# Derived references: token mentions and public_id (or 8-hex prefix) mentions
# found in the FREE TEXT of a description. There is no dependency field in the
# export -- CONTEXT-SPEC-DEPS is filed to add one -- so these are best-effort
# and are labelled as such everywhere they are rendered.
TOKEN_RE = re.compile(r"\b[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)+\b")
HEX_RE = re.compile(r"\b[0-9a-f]{8}(?:-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})?\b")
refs_out = {t["public_id"]: [] for t in tasks}
refs_in = {t["public_id"]: [] for t in tasks}

for t in tasks:
    text = " ".join(filter(None, (t.get("description"), t.get("status_note"))))
    hits = []
    for m in TOKEN_RE.findall(text):
        hits.extend(tok_index.get(m, ()))
    for m in HEX_RE.findall(text):
        hits.extend([by_pid[m]] if m in by_pid else pid8_index.get(m[:8], ()))
    seen = set()
    for o in hits:
        p = o["public_id"]
        if p == t["public_id"] or p in seen:
            continue
        seen.add(p)
        refs_out[t["public_id"]].append(o)
        refs_in[p].append(t)

# ---- real relations (authoritative) -----------------------------------------
# Shape, from the server: {"kind": "blocks"|"supersedes"|"relates"|"follow_up",
# "direction": "outgoing"|"incoming", "task": "<peer KEY>", "created_at": ...}.
# The peer is named by KEY, which is null for a third of the backlog, so
# resolution falls back to the title-prefix token and then to public_id — and
# an unresolvable peer is RENDERED RAW rather than dropped.
#
# `blocks` is inert metadata: it does not touch the target's status. A task can
# hold an incoming block and still be done. Status is always the task's own field.
# Only DIRECT edges are rendered — no traversal, so a cycle (which the server
# accepts) cannot hang this script.
DIRECTION = {
    ("blocks", "outgoing"): "blocks",
    ("blocks", "incoming"): "blocked by",
    ("supersedes", "outgoing"): "supersedes",
    ("supersedes", "incoming"): "superseded by",
    ("follow_up", "outgoing"): "follow-up",
    ("follow_up", "incoming"): "follow-up of",
    ("relates", "outgoing"): "relates to",
    ("relates", "incoming"): "relates to",
}
relations = {t["public_id"]: [] for t in tasks}
if rel_state == "fetched":
    for t in tasks:
        pid = t["public_id"]
        path = os.path.join(rel_dir, pid + ".json")
        if not os.path.exists(path):
            raise SystemExit(f"gen-spec-mirror: no relations file for task {pid}")
        for r in json.load(open(path, encoding="utf-8")):
            kind, direction = r.get("kind", "?"), r.get("direction", "?")
            peer_name = r.get("task")
            peers = tok_index.get(peer_name) or ([by_pid[peer_name]] if peer_name in by_pid else [])
            relations[pid].append({
                "verb": DIRECTION.get((kind, direction),
                                     f"{esc(kind)} ({esc(direction)})"),
                "name": peer_name,
                "peer": peers[0] if len(peers) == 1 else None,
            })
        relations[pid].sort(key=lambda r: (r["verb"], str(r["name"])))


def rel_items(t, frm_dir):
    out = []
    for r in relations[t["public_id"]]:
        target = (f"[{esc(r['peer']['_label'])}]({link(frm_dir, r['peer'])})"
                  if r["peer"] else f"{esc(r['name'])} (unresolved)")
        out.append((r["verb"], target))
    return out


REL_NOTE = {
    "fetched": "> Authoritative, from the Spec Server's relations resource. `blocks` is inert\n"
               "> metadata — it never changes a task's status, so the status shown is always the\n"
               "> task's own field.\n",
    "skipped": "> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built\n"
               "> with `--no-relations`, which skips one rate-limited request per task. Re-run\n"
               "> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.\n",
}[rel_state]

# ---- ordering ---------------------------------------------------------------
def sort_key(t):
    prio = t.get("priority") or "P9"
    try:
        pos = float(t.get("position") or 0)
    except (TypeError, ValueError):
        pos = 0.0
    return (prio, pos, t["_label"])


groups = {k: [] for k in epic_dirs}      # every epic gets a page, even an empty one
for t in tasks:
    groups[t["_epic"]].append(t)
for v in groups.values():
    v.sort(key=sort_key)


def link(frm_dir, to_task):
    """Relative link from a directory inside SPEC/ to a task file."""
    target = os.path.join(spec_root, to_task["_dir"], "task.md")
    return os.path.relpath(target, os.path.join(spec_root, frm_dir)).replace(os.sep, "/")


def ref_cell(t, frm_dir):
    out = refs_out[t["public_id"]]
    if not out:
        return "—"
    shown = [f"[{esc(o['_label'])}]({link(frm_dir, o)})" for o in out[:6]]
    if len(out) > 6:
        shown.append(f"+{len(out) - 6} more")
    return " ".join(shown)


DERIVED_NOTE = (
    "> Derived by matching task keys, title prefixes and public-id fragments in free text.\n"
    "> The export has NO dependency field, so this is best-effort and NOT authoritative;\n"
    "> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.\n"
)

# ---- task.md ----------------------------------------------------------------
written = 0
for t in tasks:
    d = os.path.join(spec_root, t["_dir"])
    os.makedirs(d, exist_ok=True)
    pid = t["public_id"]
    rows = [
        ("Public id", f"`{pid}`"),   # UUID-validated above, so the span cannot be escaped
        ("Key", esc(t["key"]) if t.get("key") else "_(null in the export)_"),
        ("Epic", f"[{esc(t['_epic'])}](../epic.md)"),
        ("Status", esc(t.get("status")) or "?"),
        ("Priority", esc(t.get("priority")) or "—"),
        ("Component", esc(t.get("component")) or "—"),
        ("Section", esc(t.get("section")) or "—"),
        ("Tags", ", ".join(esc(x) for x in (t.get("tags") or [])) or "—"),
        ("Created", esc(t.get("created_at")) or "—"),
        ("Updated", esc(t.get("updated_at")) or "—"),
        ("Completed", esc(t.get("completed_at")) or "—"),
    ]
    body = [f"# {esc(t.get('title'))}\n", "| Field | Value |", "| --- | --- |"]
    body += [f"| {k} | {v} |" for k, v in rows]
    body.append("")
    if t.get("proof_cmd"):
        proof = (t["proof_cmd"] or "").rstrip()
        fence = "`" * max(3, max((len(m) for m in re.findall(r"`+", proof)), default=0) + 1)
        body.append("## Proof command\n")
        body.append(f"{fence}sh\n{proof}\n{fence}\n")
    if t.get("status_note"):
        body.append("## Status note\n")
        body.append(t["status_note"].rstrip() + "\n")
    body.append("## Description\n")
    body.append((t.get("description") or "_(none)_").rstrip() + "\n")
    body.append("## Relations (authoritative)\n")
    body.append(REL_NOTE)
    body.append("")
    items = rel_items(t, t["_dir"])
    body += [f"- **{verb}** {target}" for verb, target in items] or \
            ["_None recorded._" if rel_state == "fetched" else "_Unknown._"]
    body.append("")
    for heading, rel in (
        ("Referenced in description (derived, not authoritative)", refs_out[pid]),
        ("Referenced by other tasks (derived, not authoritative)", refs_in[pid]),
    ):
        if rel:
            body.append(f"## {heading}\n")
            body.append(DERIVED_NOTE)
            body.append("")
            for o in sorted(rel, key=lambda x: x["_label"]):
                body.append(
                    f"- [{esc(o['_label'])}]({link(t['_dir'], o)}) — "
                    f"{esc(o.get('title'), 90)} ({esc(o.get('status'))})"
                )
            body.append("")
    body.append("---\n")
    body.append("_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. "
                "Never hand-edit; the server is the source of truth._\n")
    with open(os.path.join(d, "task.md"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(body))
    written += 1

# ---- epic.md ----------------------------------------------------------------
def task_table(rows, epic_dir):
    out = ["| Key | Title | Status | Prio | Task | Relations (real) | Referenced (derived) |",
           "| --- | --- | --- | --- | --- | --- | --- |"]
    for t in rows:
        items = rel_items(t, epic_dir)
        shown = [f"{verb} {target}" for verb, target in items[:6]]
        if len(items) > 6:
            shown.append(f"+{len(items) - 6} more (see task.md)")
        cell = "<br>".join(shown) or ("—" if rel_state == "fetched" else "_not fetched_")
        out.append(
            f"| {esc(t['_label'])} | {esc(t.get('title'), 90)} | {esc(t.get('status'))} | "
            f"{esc(t.get('priority')) or '—'} | [task.md]({link(epic_dir, t)}) | {cell} | "
            f"{ref_cell(t, epic_dir)} |"
        )
    return out


index_rows = []
for ekey in sorted(groups):
    rows = groups[ekey]
    epic_dir = epic_dirs[ekey]
    os.makedirs(os.path.join(spec_root, epic_dir), exist_ok=True)
    meta = epics.get(ekey, {})
    op = [t for t in rows if not t["_closed"]]
    cl = [t for t in rows if t["_closed"]]
    title = esc(meta.get("title")) or esc(ekey)
    lines = [f"# EPIC {esc(ekey)} — {title}\n",
             "[← all epics](../../SPEC.md)\n",
             f"**{len(op)} open / {len(rows)} total.** Full records live in "
             f"`SPEC/{epic_dir}/<task>/task.md`.\n",
             "_Relations (real)_ are authoritative edges from the Spec Server. "
             "_Referenced (derived)_ are guesses parsed out of description free text — "
             "useful, but not a dependency list.\n"]
    lines.append(f"## Open tasks ({len(op)})\n")
    lines += task_table(op, epic_dir) if op else ["_None._"]
    lines.append("")
    lines.append(f"## Closed tasks ({len(cl)}) — done, cancelled, superseded\n")
    lines += task_table(cl, epic_dir) if cl else ["_None._"]
    lines.append("")
    if meta.get("description"):
        lines.append("## Epic description\n")
        lines.append(meta["description"].rstrip() + "\n")
    lines.append("---\n")
    lines.append("_Generated by `scripts/gen-spec-mirror.sh`; never hand-edit._\n")
    with open(os.path.join(spec_root, epic_dir, "epic.md"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines))
    written += 1
    summary = esc(meta.get("description", "").split("\n")[0] if meta.get("description") else "", 110)
    index_rows.append((ekey, len(op), len(rows), summary,
                       f"[SPEC/{epic_dir}/epic.md](SPEC/{epic_dir}/epic.md)"))

# ---- SPEC.md ----------------------------------------------------------------
total_open = sum(1 for t in tasks if not t["_closed"])
idx = [
    f"# {esc(data.get('project', {}).get('name')) or 'Backlog'} — backlog index\n",
    "> GENERATED MIRROR of the Spec Server (project slug `agent-bus`) — **never hand-edit**.",
    "> Regenerate with `bash scripts/gen-spec-mirror.sh`. The server is the source of truth.",
    ">",
    "> This file lists EPICS ONLY. Task records live in the tree, one file each, with",
    "> descriptions complete and untruncated:",
    ">",
    "> - `SPEC/<EPIC>/epic.md` — every task in the epic, open first, then closed",
    "> - `SPEC/<EPIC>/<task>/task.md` — the full record for one task\n",
    f"**{len(tasks)} tasks in {len(index_rows)} epics — {total_open} open, "
    f"{len(tasks) - total_open} closed.**\n",
    "| Epic | Open | Total | Summary | Tasks |",
    "| --- | ---: | ---: | --- | --- |",
]
for ekey, o, tot, summary, ptr in index_rows:
    idx.append(f"| {esc(ekey)} | {o} | {tot} | {summary or '—'} | {ptr} |")
idx += [
    "",
    "Directory names are `<key-or-title-slug>--<public-id-prefix>`; the public-id fragment is",
    "what makes them unique, since `key` is null for a third of the backlog. Use the FULL",
    "`public_id` (recorded in every `task.md`) for any Spec Server lookup — prefix resolution",
    "does not exist server-side.",
    "",
    "The tree shows task-to-task links in two clearly separated forms, and they are not the",
    "same thing: **relations** are authoritative edges from the Spec Server "
    f"(`blocks`, `supersedes`, `relates`, `follow_up` — {rel_state} for this run), while",
    "**referenced (derived)** links are merely key-shaped strings matched in description free",
    "text. Derived links are best-effort and must not be read as a dependency list.",
    "",
]
with open(os.path.join(build, "SPEC.md"), "w", encoding="utf-8") as fh:
    fh.write("\n".join(idx))

# ---- GUARD 3: no task lost, none invented -----------------------------------
task_files = sum(1 for _, _, fs in os.walk(spec_root) for f in fs if f == "task.md")
got_closed = len(tasks) - total_open
problems = []
found = sum(1 for _, _, fs in os.walk(spec_root) for f in fs if f.endswith(".md"))
if found != written:
    problems.append(f"  wrote {written} files but {found} are on disk")
if task_files != len(tasks):
    problems.append(f"  emitted {task_files} task.md files for {len(tasks)} tasks")
if len(used) != len(tasks):
    problems.append(f"  {len(used)} unique directories for {len(tasks)} tasks")
if total_open != want_open or got_closed != want_closed:
    problems.append(f"  tallies open/closed: tree {total_open}/{got_closed}, "
                    f"server {want_open}/{want_closed}")
if problems:
    sys.stderr.write("gen-spec-mirror: REFUSING TO WRITE — the tree does not account for "
                     "every task.\n" + "\n".join(problems) + "\n")
    sys.exit(1)
sys.stderr.write(f"gen-spec-mirror: built {task_files} task files across "
                 f"{len(index_rows)} epics\n")
PY
then
  echo "gen-spec-mirror: builder failed; SPEC.md and SPEC/ are unchanged" >&2
  exit 1
fi

if [ "$TO_STDOUT" = 1 ]; then
  cat "$build/SPEC.md"
  exit 0
fi

# ---------------------------------------------------------------------------
# SWAP. Only now, with a fully built and validated tree in hand.
#
# STALE FILES ARE LIES: a task that moves epic, is renamed or is deleted leaves
# behind an orphan file asserting current state. So the replacement is whole —
# never a partial overwrite of SPEC/, and never an `rm -rf SPEC/` before the
# replacement exists.
# ---------------------------------------------------------------------------
before=0
[ -f SPEC.md ] && before=$(wc -c < SPEC.md)

# TREE FIRST, INDEX SECOND. Two artefacts cannot be swapped in one atomic step,
# so the order is chosen for which half-done state is least misleading: a fresh
# tree under a stale index still resolves, whereas a fresh index over a missing
# tree is a page of dead links (and on a first run, of links to nothing at all).
#
# The old tree is moved ASIDE, not deleted, and only removed once the new one is
# in place — and the rescue directory is in the EXIT trap, so a crash mid-swap
# cannot leave it behind masquerading as content.
# `-e`, not `-d`: if SPEC exists as a FILE, `mv <dir> SPEC` fails and every future run
# fails identically. Move whatever is there aside and carry on.
if [ -e SPEC ]; then
  rescue="$(mktemp -d ./.spec-tree.XXXXXX)"        # its own statement, so a failed
  mv SPEC "$rescue/SPEC"                           # mktemp can never make this "/"
fi
mv "$build/SPEC" SPEC
mv -f "$build/SPEC.md" SPEC.md
if [ -n "$rescue" ]; then rm -rf "$rescue"; rescue=""; fi

after=$(wc -c < SPEC.md)
files=$(find SPEC -type f | wc -l)
echo "gen-spec-mirror: SPEC.md ${before} -> ${after} bytes; SPEC/ holds ${files} files (${js_open} open, ${js_closed} closed tasks)"

# A GENERATED FILE THAT GIT IGNORES IS A SILENTLY DROPPED TASK, and it drops it
# in the one place the guards above cannot see: they count files in the build
# directory, before git has an opinion. `.gitignore` carries several unanchored
# credential patterns (`id_rsa*`, `id_ed25519*`, `*credentials*`…), git refuses
# to descend into an excluded DIRECTORY, and an ignored file leaves
# `git status --porcelain` perfectly clean. So ask git directly, every run.
if git rev-parse --git-dir >/dev/null 2>&1; then
  ignored="$(git ls-files --others --ignored --exclude-standard -- SPEC | head -20)"
  if [ -n "$ignored" ]; then
    echo "gen-spec-mirror: WARNING — git IGNORES generated mirror files. They exist on disk," >&2
    echo "  but they will never be committed and nothing else will complain:" >&2
    printf '%s\n' "$ignored" | sed 's/^/    /' >&2
    echo "  A .gitignore pattern (most likely one of the unanchored credential patterns)" >&2
    echo "  matches a task directory name. Add a directory negation for it, e.g. !id_rsa*/." >&2
    exit 3
  fi
fi
