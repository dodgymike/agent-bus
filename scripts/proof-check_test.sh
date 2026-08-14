#!/usr/bin/env bash
# scripts/proof-check_test.sh — focused regressions for proof-check.sh.
#
# Root-cause under test: proof-check.sh used to resolve relative executables
# and run the proof command's `bash -c` in REPO_ROOT/SCRIPT_DIR (where the
# script physically lives), rather than the caller's cwd. That meant an
# absolute-path invocation of proof-check.sh from a *different* tree would
# silently execute the proof against THIS repo instead of the caller's tree.
#
# This test proves the fix by running the exact same relative-path proof
# command (`test -f ./ISO_MARKER.txt`) from two different callers, both
# invoking scripts/proof-check.sh by absolute path:
#   1. The live repo tree (no ISO_MARKER.txt present)   -> must be FAIL/vacuous-ok, NOT PASS
#   2. An isolated caller tree with ISO_MARKER.txt added -> must be PASS
#
# If proof-check.sh still resolved cwd via its own SCRIPT_DIR, both cases
# would run against the live tree and neither would see the overlay's marker
# file, so case 2 would also come back FAIL — proving the bug. This test
# fails loudly if that regresses.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd)"
PROOF_CHECK="${SCRIPT_DIR}/proof-check.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# --- Case 1: live repo tree, no ISO_MARKER.txt --------------------------
LIVE_VERDICT="$(cd "$REPO_ROOT" && bash "$PROOF_CHECK" "test -f ./ISO_MARKER.txt" 2>&1 | grep -o 'verdict=[A-Z]*')"
echo "live tree verdict: ${LIVE_VERDICT:-<none>}"
if [[ "$LIVE_VERDICT" == "verdict=PASS" ]]; then
  fail "live tree (no ISO_MARKER.txt) unexpectedly PASSed — cwd resolution is broken"
fi

# --- Case 2: isolated caller tree, with ISO_MARKER.txt added ------------
T="$(mktemp -d)"
cleanup() { rm -rf "$T"; }
trap cleanup EXIT

CALLER="$T/caller"
mkdir -p "$CALLER"
echo only-in-overlay > "$CALLER/ISO_MARKER.txt"

OVERLAY_VERDICT="$(cd "$CALLER" && bash "$PROOF_CHECK" "test -f ./ISO_MARKER.txt" 2>&1 | grep -o 'verdict=[A-Z]*')"
echo "overlay tree verdict: ${OVERLAY_VERDICT:-<none>}"

if [[ "$OVERLAY_VERDICT" != "verdict=PASS" ]]; then
  fail "overlay tree (with ISO_MARKER.txt) did not PASS (got '${OVERLAY_VERDICT:-<none>}') — proof-check is not respecting caller cwd"
fi

echo "PASS: proof-check.sh respects caller cwd (live=${LIVE_VERDICT:-<none>}, overlay=${OVERLAY_VERDICT})"

# --- Case 3: parent PASS cannot launder all-skipped child tests ---------
# Keep this fixture outside the repository so it cannot consume or depend on
# application tests (in particular, SIGN-3 and TestEnrolmentEpoch).
FIXTURE="$T/leaf-results"
mkdir -p "$FIXTURE"
printf 'module proofcheckfixture\n\ngo 1.19\n' > "$FIXTURE/go.mod"
printf '%s\n' \
  'package proofcheckfixture' \
  'import "testing"' \
  'func TestParent(t *testing.T) {' \
  '  t.Run("one", func(t *testing.T) { t.Skip("blocked") })' \
  '  t.Run("two", func(t *testing.T) { t.Skip("blocked") })' \
  '}' \
  'func TestMixed(t *testing.T) {' \
  '  t.Run("pass", func(t *testing.T) {})' \
  '  t.Run("skip", func(t *testing.T) { t.Skip("blocked") })' \
  '}' > "$FIXTURE/fixture_test.go"

# If this regression script is itself run through proof-check.sh, bypass that
# outer invocation's Go shim. Otherwise the nested checker would select the
# shim as its "real" Go binary and recurse (tracked separately).
FIXTURE_PATH="$PATH"
if [[ -n "${PROOF_CHECK_REAL_GO:-}" ]]; then
  FIXTURE_PATH="$(dirname "$PROOF_CHECK_REAL_GO"):/usr/bin:/bin"
fi

assert_vacuous() {
  local mode="$1"
  local verdict rc
  set +e
  verdict="$(cd "$FIXTURE" && PATH="$FIXTURE_PATH" bash "$PROOF_CHECK" "go test ${mode} -run ^TestParent$ ." 2>/dev/null)"
  rc=$?
  set -e
  [[ $rc -eq 4 ]] || fail "${mode:-plain-text} all-skipped fixture exited ${rc}, want 4: ${verdict}"
  [[ "$verdict" == *"verdict=VACUOUS"* ]] || fail "${mode:-plain-text} all-skipped fixture was not VACUOUS: ${verdict}"
  [[ "$verdict" == *"skipped=2"* ]] || fail "${mode:-plain-text} did not count both skipped leaves: ${verdict}"
}

assert_vacuous ""
assert_vacuous "-json"
echo "PASS: parent PASS with all child leaves skipped is VACUOUS (plain text and JSON)"

assert_mixed_passes() {
  local mode="$1"
  local verdict
  verdict="$(cd "$FIXTURE" && PATH="$FIXTURE_PATH" bash "$PROOF_CHECK" "go test ${mode} -run ^TestMixed$ ." 2>/dev/null)" ||
    fail "${mode:-plain-text} mixed leaf fixture did not PASS: ${verdict}"
  [[ "$verdict" == *"verdict=PASS"* ]] || fail "${mode:-plain-text} mixed leaf fixture was not PASS: ${verdict}"
  [[ "$verdict" == *"skipped=1"* ]] || fail "${mode:-plain-text} mixed fixture did not count its skipped leaf: ${verdict}"
}

assert_mixed_passes ""
assert_mixed_passes "-json"
echo "PASS: one passing leaf preserves PASS despite a skipped sibling (plain text and JSON)"

# --- Case 4: package identity prevents same-name result collisions ------
mkdir -p "$FIXTURE/a" "$FIXTURE/z"
printf '%s\n' \
  'package a' 'import "testing"' \
  'func TestSame(t *testing.T) { t.Run("leaf", func(t *testing.T) {}) }' \
  > "$FIXTURE/a/same_test.go"
printf '%s\n' \
  'package z' 'import "testing"' \
  'func TestSame(t *testing.T) { t.Run("leaf", func(t *testing.T) { t.Skip("blocked") }) }' \
  > "$FIXTURE/z/same_test.go"

assert_collision_passes() {
  local mode="$1"
  local verdict
  verdict="$(cd "$FIXTURE" && PATH="$FIXTURE_PATH" bash "$PROOF_CHECK" "go test ${mode} -run ^TestSame$ ./a ./z" 2>/dev/null)" ||
    fail "${mode:-plain-text} same-name multi-package fixture did not PASS: ${verdict}"
  [[ "$verdict" == *"verdict=PASS"* ]] || fail "${mode:-plain-text} same-name fixture was not PASS: ${verdict}"
  [[ "$verdict" == *"skipped=1"* ]] || fail "${mode:-plain-text} same-name fixture merged package results: ${verdict}"
}

assert_collision_passes ""
assert_collision_passes "-json"
echo "PASS: identical test names remain package-scoped (plain text and JSON)"

# --- Case 5: TestMain cannot forge evidence for an empty package --------
mkdir -p "$FIXTURE/forged"
printf '%s\n' \
  'package forged' \
  'import ("fmt"; "os"; "testing")' \
  'func TestMain(m *testing.M) {' \
  '  fmt.Println("=== RUN   TestForged")' \
  '  fmt.Println("--- PASS: TestForged (0.00s)")' \
  '  os.Exit(m.Run())' \
  '}' \
  'func TestReal(t *testing.T) {}' > "$FIXTURE/forged/forged_test.go"

assert_forgery_vacuous() {
  local mode="$1"
  local verdict rc
  set +e
  verdict="$(cd "$FIXTURE" && PATH="$FIXTURE_PATH" bash "$PROOF_CHECK" "go test ${mode} -run ^TestAbsent$ ./forged" 2>/dev/null)"
  rc=$?
  set -e
  [[ $rc -eq 4 ]] || fail "${mode:-plain-text} forged empty fixture exited ${rc}, want 4: ${verdict}"
  [[ "$verdict" == *"verdict=VACUOUS"* ]] || fail "${mode:-plain-text} forged empty fixture was not VACUOUS: ${verdict}"
  [[ "$verdict" == *"tests_run=0"* ]] || fail "${mode:-plain-text} forged events counted as tests: ${verdict}"
  [[ "$verdict" == *"empty_pkgs=1"* ]] || fail "${mode:-plain-text} empty package summary was lost: ${verdict}"
}

assert_forgery_vacuous ""
assert_forgery_vacuous "-json"
echo "PASS: empty-package summaries override forged TestMain markers (plain text and JSON)"
