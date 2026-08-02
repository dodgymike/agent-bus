#!/usr/bin/env bash
# commit-on-stop: one commit per turn (replaces the old per-file PostToolUse auto-commit).
#
# Fires on Stop (main agent end-of-turn) and SubagentStop (a Task subagent finishing),
# so each turn/task produces ONE commit instead of one commit per Write/Edit. Stages all
# changes (respecting .gitignore) and commits them together.
#
# Guard: refuses to auto-commit when an untracked, NON-ignored file larger than 25 MB
# would be staged — that almost always means a build artifact, model weight, or dataset
# leaked into the tree. In that case it commits nothing and tells you to .gitignore it
# or move it to /tmp, so a giant blob never lands in history by accident.
set -u

cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
git rev-parse --git-dir >/dev/null 2>&1 || exit 0

# --- HANDOVER-P1-5: opt-in gate --------------------------------------------------
# Auto-commit is OFF by default so a human who clones this repo gets NORMAL git
# behaviour (no surprise "Session update" commits). It is a LOCAL opt-in for the
# agent-heavy workflow. Enable it any ONE of these ways (all local / gitignored):
#   * export AGENTBUS_AUTOCOMMIT=1  (or true) in the environment, or
#   * add it to the gitignored .claude/settings.local.json "env" block, or
#   * create a .claude/.autocommit-enabled marker file.
# When enabled, ALL existing behaviour below (the >25 MB guard, the CFG-2b quality
# gate, the single per-turn commit) runs UNCHANGED. See CONTRIBUTING.md.
_autocommit_on() {
  case "${AGENTBUS_AUTOCOMMIT:-}" in 1 | true | TRUE | True | yes | on) return 0 ;; esac
  [ -f ".claude/.autocommit-enabled" ] && return 0
  [ -f ".claude/settings.local.json" ] &&
    grep -Eq '"AGENTBUS_AUTOCOMMIT"[[:space:]]*:[[:space:]]*"?(1|true)"?' \
      ".claude/settings.local.json" 2>/dev/null && return 0
  return 1
}
if ! _autocommit_on; then
  stamp="${TMPDIR:-/tmp}/.agentbus-autocommit-optin-noted"
  if [ ! -e "$stamp" ]; then
    printf 'commit-on-stop: auto-commit is opt-in and OFF (human-clean default). Set AGENTBUS_AUTOCOMMIT=1 (or add it to .claude/settings.local.json "env", or create .claude/.autocommit-enabled) to enable it. See CONTRIBUTING.md.\n' >&2
    : >"$stamp" 2>/dev/null || true
  fi
  exit 0
fi

# --- >25 MB untracked-file guard -------------------------------------------------
limit=$((25 * 1024 * 1024))
big=""
while IFS= read -r f; do
  [ -n "$f" ] && [ -f "$f" ] || continue
  sz=$(stat -f%z "$f" 2>/dev/null || stat -c%s "$f" 2>/dev/null || echo 0)
  if [ "$sz" -gt "$limit" ]; then
    big="${big}  ${f} ($((sz / 1024 / 1024)) MB)\n"
  fi
done <<EOF
$(git ls-files --others --exclude-standard)
EOF

if [ -n "$big" ]; then
  printf 'commit-on-stop: refusing auto-commit — large untracked file(s) present:\n%b' "$big" >&2
  printf 'Add them to .gitignore or move to /tmp, then commit manually.\n' >&2
  exit 0
fi

# --- one commit for everything staged this turn ----------------------------------
git add -A 2>/dev/null || exit 0
git diff --cached --quiet 2>/dev/null && exit 0  # nothing to commit

# --- CFG-2b: quality gate — never commit conflict markers or syntax-broken code ---
# A stray `git stash pop` once committed <<<<<<< markers into analyze_audio.py (the
# audio lambda), which then would not even parse — and a deploy nearly shipped it.
# Refuse to auto-commit when a staged file carries merge-conflict markers or fails a
# cheap syntax check. On refusal we UNSTAGE and commit nothing this turn, so the work
# stays in the working tree to be fixed (never lost) rather than landing broken in
# history. Best-effort: language checks run only when the interpreter is on PATH.
bad=""
while IFS= read -r f; do
  [ -n "$f" ] && [ -f "$f" ] || continue
  if grep -Iq -E '^(<<<<<<< |>>>>>>> )' "$f" 2>/dev/null; then
    bad="${bad}  ${f}: merge-conflict markers\n"; continue
  fi
  case "$f" in
    *.py)
      command -v python3 >/dev/null 2>&1 &&
        ! python3 -c "import ast,sys; ast.parse(open(sys.argv[1]).read())" "$f" 2>/dev/null &&
        bad="${bad}  ${f}: python syntax error\n" ;;
    *.js|*.mjs)
      command -v node >/dev/null 2>&1 &&
        ! node --check "$f" 2>/dev/null &&
        bad="${bad}  ${f}: javascript syntax error\n" ;;
  esac
done <<EOF
$(git diff --cached --name-only --diff-filter=ACM)
EOF

if [ -n "$bad" ]; then
  git reset -q 2>/dev/null   # unstage; leave the work in the tree, uncommitted
  printf 'CFG-2b: refusing auto-commit — broken file(s) would be committed:\n%b' "$bad" >&2
  printf 'Fix them (changes remain in your working tree); they will commit next turn.\n' >&2
  exit 0
fi

n=$(git diff --cached --name-only | wc -l | tr -d ' ')
git commit -q \
  -m "Session update: ${n} file(s)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>" 2>/dev/null
exit 0
