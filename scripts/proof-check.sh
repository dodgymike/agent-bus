#!/usr/bin/env bash
# scripts/proof-check.sh — run a task's `proof_cmd` and refuse to call it a pass
# unless it actually demonstrated something.
#
# ---------------------------------------------------------------------------
# WHY THIS EXISTS
# ---------------------------------------------------------------------------
# `go test -race -run TestThatDoesNotExist ./internal/wal` prints
#
#     ok  	github.com/dodgymike/agent-bus/internal/wal	0.025s [no tests to run]
#
# and EXITS 0. Almost every proof_cmd in this backlog has that shape, so a task
# could be flipped to `done` behind a proof command that proves literally
# nothing — the test it names need never have been written. This script closes
# that hole: it classifies the proof, applies the check that shape deserves,
# and reports one of PASS / FAIL / VACUOUS / UNVERIFIABLE.
#
# The boundary is the whole job. A guard that false-POSITIVES is worse than no
# guard (it launders vacuous proofs as evidence). A guard that false-NEGATIVES
# blocks every completion and gets switched off within a day. So:
#   * the ONLY thing enforced on a Go proof is "at least one test actually ran,
#     and not every one of them skipped" — the minimum that makes the proof
#     mean anything at all;
#   * non-Go proofs (`test -s FILE`, `grep -q ...`, `scripts/bus-*.sh ...`,
#     `docker compose ...`) are LEGITIMATE and are judged purely on their exit
#     status. They are never forced through a test-count check;
#   * anything this script cannot honestly evaluate is reported UNVERIFIABLE
#     (exit 3) and never silently passed.
#
# ---------------------------------------------------------------------------
# POLICY: should `complete` REQUIRE proof-check.sh rather than a bare command?
# ---------------------------------------------------------------------------
# RECOMMENDATION: yes for the test-based proofs, which are ~75% of this
# backlog — but as a *gate the completing agent must run and quote*, not as a
# rewrite of the stored proof_cmd.
#
# Concretely: keep `proof_cmd` stored as the bare command (it stays readable,
# copy-pasteable, and tool-independent — a proof that only runs inside our
# harness is a worse artifact than one anybody can paste into a shell), and
# require that spec-keeper run `scripts/proof-check.sh --task <id>` before
# POSTing `/complete`, pasting the verdict line into `test_summary`.
#
# The tradeoff, stated honestly:
#   FOR: the failure this prevents is silent and self-congratulatory. A vacuous
#     proof does not merely fail to catch a bug, it manufactures evidence that
#     none exists, and it is invisible in review because the command looks
#     exactly like a real one. Exit-0 is precisely the signal a reviewer trusts.
#   AGAINST: it puts a bash script in the completion path. If proof-check.sh is
#     itself wrong, it blocks correct work — and an agent under pressure will
#     route around a gate it does not trust. That is why the default check is
#     deliberately the weakest useful one (>=1 non-skipped test), why --strict
#     is opt-in rather than default, and why UNVERIFIABLE is a distinct exit
#     code (3) instead of a failure: "I cannot check this" must never read as
#     "this is broken", or the gate becomes noise.
#   ALSO: this cannot be enforced by the tool itself. Nothing stops an agent
#     from skipping the script, so its real value is that the verdict line is
#     quotable and auditable after the fact. Treat it as a norm with an audit
#     trail, not as a lock.
#
# NOT recommended: making proof-check.sh mandatory for file-assertion and
# wrapper proofs. It adds no signal there — it just runs the command and
# forwards the exit code — so mandating it buys nothing but ceremony.
#
# ---------------------------------------------------------------------------
# DECIDED: a -run pattern that matches tests in ONE listed package but not
# another (e.g. `go test -race -run TestEnrolMessagingKey ./internal/auth
# ./internal/httpapi`) is a **PASS**, with a warning naming the empty packages.
#
# Rationale: listing several packages is how a proof says "the behaviour is
# demonstrated somewhere across these" — and `./internal/...` (used by SIGN-1,
# IDEM-14, RATCHET-6 and others) expands to a dozen packages of which two will
# ever match. Requiring every listed package to contribute a test would fail
# almost every legitimate proof in this backlog, which is the false-negative
# mode that gets guards disabled. The trap being closed is ZERO tests anywhere,
# and that is what the default enforces. Use --strict to require every listed
# package to contribute at least one test; it is opt-in for exactly the proofs
# whose author meant "each of these packages must cover it".
#
# ---------------------------------------------------------------------------
# TRUST BOUNDARY — read before using --task
# ---------------------------------------------------------------------------
# A proof_cmd is EXECUTABLE INPUT. This script runs it verbatim, with your
# privileges, in the repo root — exactly as if you had pasted it into a shell.
# With --task the string comes from the Spec Server, so anyone who can edit
# that backlog can choose a command that runs on your machine. Only use --task
# against a project whose backlog you trust, and read the `proof:` line this
# script echoes before it executes. Use --classify to inspect a command
# statically without running anything.
#
# ---------------------------------------------------------------------------
# Usage:
#   scripts/proof-check.sh 'go test -race -run TestWALReplay ./internal/wal'
#   scripts/proof-check.sh --task DUR-3
#   scripts/proof-check.sh --classify 'test -s PROTOCOL.md'
#   scripts/proof-check.sh --strict 'go test -race -run TestX ./internal/a ./internal/b'
#
# Options:
#   --task <id>   fetch proof_cmd from the Spec Server (task key or public_id)
#                 via scripts/spec-cloud.sh, then check it. Needs jq.
#   --classify    static classification only; runs NOTHING. Always exits 0
#                 unless the command is UNVERIFIABLE (3) or usage is bad (2).
#   --strict      additionally require every package listed in a `go test`
#                 invocation to contribute at least one test.
#   --quiet       suppress the proof command's own output; print only the verdict.
#   -h, --help    this text.
#
# Output: human lines on stderr, plus one machine-readable verdict line on
# stdout:
#   proof-check: verdict=<PASS|FAIL|VACUOUS|UNVERIFIABLE> class=<...> exit=<n> tests_run=<n> top_level=<n> skipped=<n> empty_pkgs=<n>
#
# Exit codes:
#   0  PASS          — ran, exited 0, and (if it ran Go tests) >=1 test really ran
#   1  FAIL          — ran and exited non-zero
#   2  usage error
#   3  UNVERIFIABLE  — cannot be checked: n/a, unfilled <placeholder>, not valid
#                      shell, or a segment whose command does not exist. NOT a
#                      claim that the underlying work is broken.
#   4  VACUOUS       — exited 0 but proved nothing: zero tests ran (the trap
#                      above), or every test that ran was skipped
#
# Known limitation: to count tests, `go test` invocations inside the proof are
# re-run through a shim that adds `-v` and merges stderr into stdout. A proof
# that parses non-verbose `go test` output, or that redirects the two streams
# separately, will see different text than it would standalone. Nothing in this
# backlog does; if yours does, run it directly and say so in test_summary.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd)"
PROJECT_SLUG="${PROOF_CHECK_PROJECT:-agent-bus}"

EXIT_PASS=0
EXIT_FAIL=1
EXIT_USAGE=2
EXIT_UNVERIFIABLE=3
EXIT_VACUOUS=4

usage() {
  sed -n '2,140p' "${BASH_SOURCE[0]}" | sed -n '/^# Usage:/,/^# Known limitation/p' >&2
}

say() { printf 'proof-check: %s\n' "$*" >&2; }

# verdict CLASS VERDICT EXIT TESTS TOP SKIPPED EMPTY — the one machine-readable
# line, on stdout so it can be captured while human output goes to stderr.
verdict_line() {
  printf 'proof-check: verdict=%s class=%s exit=%s tests_run=%s top_level=%s skipped=%s empty_pkgs=%s\n' \
    "$2" "$1" "$3" "$4" "$5" "$6" "$7"
}

# ---------------------------------------------------------------------------
# split_segments CMD — writes the command's top-level segments, one per line,
# with `&&`, `||`, `;`, `|` and newlines OUTSIDE quotes and outside $( ) / ``
# turned into separators. Quoting and command-substitution state are tracked so
# that `test -z "$(gofmt -l .)"` and `test $(go test ... | grep -c RUN) -gt 0`
# stay single segments.
# ---------------------------------------------------------------------------
split_segments() {
  printf '%s' "$1" | awk '
    BEGIN { RS = "\0"; q = ""; depth = 0 }
    {
      s = $0; n = length(s); i = 1; out = ""
      while (i <= n) {
        c = substr(s, i, 1)
        if (q != "") {
          if (q == "\"" && c == "\\") { out = out c substr(s, i+1, 1); i += 2; continue }
          if (c == q) { q = "" }
          out = out c; i++; continue
        }
        if (c == "\\")                    { out = out c substr(s, i+1, 1); i += 2; continue }
        if (c == "\047" || c == "\"")     { q = c; out = out c; i++; continue }
        if (c == "$" && substr(s, i+1, 1) == "(") { depth++; out = out "$("; i += 2; continue }
        if (c == "`")                     { depth = (depth > 0 ? depth - 1 : depth + 1); out = out c; i++; continue }
        if (c == ")" && depth > 0)        { depth--; out = out c; i++; continue }
        if (depth > 0)                    { out = out c; i++; continue }
        if (c == "&" && substr(s, i+1, 1) == "&") { out = out "\n"; i += 2; continue }
        if (c == "|" && substr(s, i+1, 1) == "|") { out = out "\n"; i += 2; continue }
        if (c == ";" || c == "|" || c == "\n")    { out = out "\n"; i++; continue }
        out = out c; i++
      }
      printf "%s\n", out
    }'
}

# head_token SEGMENT — first word, with `NAME=value` env prefixes, a leading
# `!`, and surrounding whitespace stripped. Empty if the segment is blank or a
# pure comment.
head_token() {
  local seg="$1" tok
  # shellcheck disable=SC2086
  set -- $seg
  while (( $# > 0 )); do
    tok="$1"
    case "$tok" in
      '!'|'(') shift; continue ;;
      *=*) [[ "${tok%%=*}" =~ ^[A-Za-z_][A-Za-z_0-9]*$ ]] && { shift; continue; } ;;
    esac
    break
  done
  tok="${1:-}"
  [[ "$tok" == '#'* ]] && tok=''
  printf '%s' "$tok"
}

# resolvable TOKEN — true if the token names something we could actually run:
# a builtin, keyword, function, PATH executable, or an existing executable path.
resolvable() {
  local tok="$1"
  [[ -z "$tok" ]] && return 0
  if [[ "$tok" == */* ]]; then
    [[ -x "${REPO_ROOT}/${tok}" || -x "$tok" ]]
    return
  fi
  type -t "$tok" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# classify CMD — sets CLASS (a comma-joined shape tag list) and, if the command
# cannot be checked, UNVERIFIABLE_REASON.
# ---------------------------------------------------------------------------
CLASS=""
UNVERIFIABLE_REASON=""
HAS_GO_TEST=0

classify() {
  local cmd="$1"
  CLASS=""; UNVERIFIABLE_REASON=""; HAS_GO_TEST=0

  local trimmed="${cmd#"${cmd%%[![:space:]]*}"}"
  trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"

  # --- shape tags (purely lexical; independent of runnability) --------------
  local tags=()
  [[ "$trimmed" =~ (^|[^[:alnum:]_])go[[:space:]]+test([^[:alnum:]_]|$) ]] && { tags+=("test"); HAS_GO_TEST=1; }
  [[ "$trimmed" == *"scripts/"*".sh"* ]] && tags+=("wrapper")
  [[ "$trimmed" =~ (^|[^[:alnum:]_])(test[[:space:]]+-[sfdez]|grep[[:space:]]|git[[:space:]]+check-ignore) ]] && tags+=("file-assertion")
  [[ "$trimmed" =~ (^|[^[:alnum:]_])(docker|make)([^[:alnum:]_]|$) ]] && tags+=("build")
  [[ "$trimmed" =~ (^|[^[:alnum:]_])go[[:space:]]+(build|vet|list)([^[:alnum:]_]|$) ]] && tags+=("toolchain")
  [[ "$trimmed" == *"gofmt"* ]] && tags+=("toolchain")

  # --- unverifiable checks, cheapest and most decisive first ----------------
  if [[ -z "$trimmed" ]]; then
    CLASS="empty"; UNVERIFIABLE_REASON="proof command is empty (no proof_cmd recorded)"; return
  fi
  if [[ "$trimmed" =~ ^[Nn]/[Aa]([^[:alnum:]]|$) ]]; then
    CLASS="declared-n/a"
    UNVERIFIABLE_REASON="proof declares itself n/a: ${trimmed}"
    return
  fi
  # An unfilled placeholder: <word...> with no space before the closing '>'.
  if [[ "$trimmed" =~ \<[A-Za-z][^\<\>]*[^[:space:]\<\>]\> ]]; then
    CLASS="${CLASS:-}placeholder"
    UNVERIFIABLE_REASON="contains an unfilled <placeholder> — the proof is a template, not a command"
    return
  fi
  if ! bash -n -c "$trimmed" 2>/dev/null; then
    CLASS="prose"
    UNVERIFIABLE_REASON="not valid shell syntax — this is prose describing a proof, not a runnable one"
    return
  fi

  # Every top-level segment must begin with something runnable. This is what
  # catches prose spliced onto a real command with `;` or `&&` — e.g.
  # "...bus-send.sh forced to retry ...; grep -q 'idempotency' FILE", where the
  # prose half is silently discarded and grep's exit code passes the whole
  # proof. It also, correctly, refuses proofs naming a wrapper that does not
  # exist yet.
  local seg tok idx=0
  while IFS= read -r seg; do
    idx=$((idx + 1))
    tok="$(head_token "$seg")"
    [[ -z "$tok" ]] && continue
    if ! resolvable "$tok"; then
      CLASS="${CLASS:-unrunnable}"
      UNVERIFIABLE_REASON="segment ${idx} starts with '${tok}', which is not an executable command here (prose, or a wrapper that does not exist yet)"
      break
    fi
  done < <(split_segments "$trimmed")

  local joined=""
  local t
  for t in "${tags[@]+"${tags[@]}"}"; do
    [[ ",${joined}," == *",${t},"* ]] && continue
    joined="${joined:+${joined},}${t}"
  done
  if [[ -n "$UNVERIFIABLE_REASON" ]]; then
    CLASS="${joined:+${joined}+}${CLASS}"
  else
    CLASS="${joined:-other}"
  fi
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
CLASSIFY_ONLY=0
STRICT=0
QUIET=0
TASK_ID=""
CMD=""

while (( $# > 0 )); do
  case "$1" in
    -h|--help) usage; exit "$EXIT_USAGE" ;;
    --classify) CLASSIFY_ONLY=1; shift ;;
    --strict) STRICT=1; shift ;;
    --quiet) QUIET=1; shift ;;
    --task) TASK_ID="${2:-}"; [[ -z "$TASK_ID" ]] && { say "--task needs a task id"; exit "$EXIT_USAGE"; }; shift 2 ;;
    --) shift; CMD="$*"; break ;;
    -*) say "unknown option: $1"; usage; exit "$EXIT_USAGE" ;;
    *) CMD="$*"; break ;;
  esac
done

if [[ -n "$TASK_ID" ]]; then
  if [[ -n "$CMD" ]]; then say "give --task OR a command, not both"; exit "$EXIT_USAGE"; fi
  if ! command -v jq >/dev/null 2>&1; then say "--task needs jq installed"; exit "$EXIT_USAGE"; fi
  task_json="$(bash "${SCRIPT_DIR}/spec-cloud.sh" -s "/api/v1/projects/${PROJECT_SLUG}/tasks/${TASK_ID}" 2>/dev/null)"
  if [[ -z "$task_json" ]] || ! printf '%s' "$task_json" | jq -e . >/dev/null 2>&1; then
    say "could not fetch task ${TASK_ID} from the Spec Server"
    exit "$EXIT_UNVERIFIABLE"
  fi
  CMD="$(printf '%s' "$task_json" | jq -r '.proof_cmd // ""')"
  say "task ${TASK_ID} status=$(printf '%s' "$task_json" | jq -r '.status // "?"')"
fi

if [[ -z "$CMD" && -z "$TASK_ID" ]]; then usage; exit "$EXIT_USAGE"; fi

say "proof: ${CMD:-<empty>}"
classify "$CMD"
say "class: ${CLASS}"

if [[ -n "$UNVERIFIABLE_REASON" ]]; then
  say "UNVERIFIABLE — ${UNVERIFIABLE_REASON}"
  say "refusing to run it; this is NOT a claim that the work is broken."
  verdict_line "$CLASS" UNVERIFIABLE - 0 0 0 0
  exit "$EXIT_UNVERIFIABLE"
fi

if (( CLASSIFY_ONLY )); then
  verdict_line "$CLASS" CLASSIFIED - 0 0 0 0
  exit "$EXIT_PASS"
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/proof-check.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT INT TERM

GOTEST_LOG="${WORK}/gotest.log"
: > "$GOTEST_LOG"

# A `go` shim ahead of the real one on PATH. It leaves every subcommand but
# `test` untouched (`go build`, `go vet`, `go list -m all` must behave
# normally), and for `test` it injects -v and tees the output so we can count
# what actually ran — no matter how deeply the invocation is buried in the
# proof's shell operators. Letting the real shell run the proof verbatim is
# what keeps `&&` short-circuiting, `$(...)`, and quoting exactly right; we do
# not reimplement shell semantics.
if (( HAS_GO_TEST )); then
  REAL_GO="$(command -v go 2>/dev/null || true)"
  if [[ -z "$REAL_GO" ]]; then
    say "UNVERIFIABLE — proof runs 'go test' but no go toolchain is on PATH"
    verdict_line "$CLASS" UNVERIFIABLE - 0 0 0 0
    exit "$EXIT_UNVERIFIABLE"
  fi
  mkdir -p "${WORK}/bin"
  cat > "${WORK}/bin/go" <<'SHIM'
#!/usr/bin/env bash
set -uo pipefail
real="${PROOF_CHECK_REAL_GO:?}"
log="${PROOF_CHECK_GOTEST_LOG:?}"
if [[ "${1:-}" != "test" ]]; then exec "$real" "$@"; fi
shift
inject=(-v)
for a in "$@"; do
  case "$a" in
    -v|--v|-v=true|-test.v|-test.v=true|-json|--json) inject=() ;;
  esac
done
"$real" test "${inject[@]+"${inject[@]}"}" "$@" 2>&1 | tee -a "$log"
exit "${PIPESTATUS[0]}"
SHIM
  chmod 0755 "${WORK}/bin/go"
  export PROOF_CHECK_REAL_GO="$REAL_GO"
  export PROOF_CHECK_GOTEST_LOG="$GOTEST_LOG"
  export PATH="${WORK}/bin:${PATH}"
fi

say "running (cwd ${REPO_ROOT})..."
if (( QUIET )); then
  ( cd "$REPO_ROOT" && bash -c "$CMD" ) >/dev/null 2>&1
else
  ( cd "$REPO_ROOT" && bash -c "$CMD" )
fi
RC=$?

TESTS_RUN=0; TOP_LEVEL=0; SKIPPED=0; PASSED=0; FAILED=0; EMPTY_PKGS=0
EMPTY_PKG_NAMES=""

if (( HAS_GO_TEST )) && [[ -s "$GOTEST_LOG" ]]; then
  if grep -q '"Action":"run"' "$GOTEST_LOG" 2>/dev/null; then
    TESTS_RUN="$(grep -c '"Action":"run"' "$GOTEST_LOG")"
    TOP_LEVEL="$TESTS_RUN"
  else
    TESTS_RUN="$(grep -c '^=== RUN' "$GOTEST_LOG" || true)"
    TOP_LEVEL="$(grep -cE '^=== RUN[[:space:]]+[^/]+$' "$GOTEST_LOG" || true)"
  fi
  PASSED="$(grep -cE '^--- PASS:' "$GOTEST_LOG" || true)"
  FAILED="$(grep -cE '^--- FAIL:' "$GOTEST_LOG" || true)"
  SKIPPED="$(grep -cE '^--- SKIP:' "$GOTEST_LOG" || true)"
  # A test binary that produced result lines but no RUN lines (unusual, but
  # possible if output interleaved badly) still counts as having run tests.
  if (( TESTS_RUN == 0 )); then TESTS_RUN=$(( PASSED + FAILED + SKIPPED )); fi
  # Packages that compiled and reported success while running nothing. Covers
  # both `[no tests to run]` (pattern matched nothing) and `[no test files]`
  # (the package has no tests at all) — including the `(cached)` variants.
  EMPTY_PKG_NAMES="$(grep -oE '^(ok|\?)[[:space:]]+[^[:space:]]+[[:space:]].*\[no test(s to run| files)\]' "$GOTEST_LOG" \
    | awk '{print $2}' | sort -u | tr '\n' ' ')"
  EMPTY_PKGS="$(printf '%s' "$EMPTY_PKG_NAMES" | wc -w | tr -d ' ')"
fi

if (( RC != 0 )); then
  say "FAIL — proof command exited ${RC}"
  (( HAS_GO_TEST )) && say "  (tests run: ${TESTS_RUN}, passed ${PASSED}, failed ${FAILED}, skipped ${SKIPPED})"
  verdict_line "$CLASS" FAIL "$RC" "$TESTS_RUN" "$TOP_LEVEL" "$SKIPPED" "$EMPTY_PKGS"
  exit "$EXIT_FAIL"
fi

if (( HAS_GO_TEST )); then
  if (( TESTS_RUN == 0 )); then
    say "VACUOUS — the proof exited 0 but ZERO tests ran."
    say "  The -run pattern matched nothing, so this command proves nothing."
    [[ -n "$EMPTY_PKG_NAMES" ]] && say "  empty packages: ${EMPTY_PKG_NAMES}"
    verdict_line "$CLASS" VACUOUS "$RC" 0 0 0 "$EMPTY_PKGS"
    exit "$EXIT_VACUOUS"
  fi
  if (( PASSED == 0 && FAILED == 0 && SKIPPED > 0 )); then
    say "VACUOUS — ${SKIPPED} test(s) ran and EVERY ONE of them skipped."
    say "  Exit 0 here means 'not exercised', not 'verified'."
    verdict_line "$CLASS" VACUOUS "$RC" "$TESTS_RUN" "$TOP_LEVEL" "$SKIPPED" "$EMPTY_PKGS"
    exit "$EXIT_VACUOUS"
  fi
  if (( EMPTY_PKGS > 0 )); then
    if (( STRICT )); then
      say "FAIL (--strict) — these listed packages contributed no test: ${EMPTY_PKG_NAMES}"
      verdict_line "$CLASS" VACUOUS "$RC" "$TESTS_RUN" "$TOP_LEVEL" "$SKIPPED" "$EMPTY_PKGS"
      exit "$EXIT_VACUOUS"
    fi
    say "warning: ${EMPTY_PKGS} listed package(s) ran no test: ${EMPTY_PKG_NAMES}"
    say "  (allowed by default — see the -run/multi-package note at the top of this script)"
  fi
  say "PASS — ${TESTS_RUN} test(s) ran (${TOP_LEVEL} top-level), ${PASSED} passed, ${SKIPPED} skipped."
else
  say "PASS — proof command exited 0."
  say "  Not a Go test proof (class=${CLASS}); its exit status IS the whole check."
fi

verdict_line "$CLASS" PASS "$RC" "$TESTS_RUN" "$TOP_LEVEL" "$SKIPPED" "$EMPTY_PKGS"
exit "$EXIT_PASS"
