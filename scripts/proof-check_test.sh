#!/usr/bin/env bash
# scripts/proof-check_test.sh — guard test for proof-check.sh caller-cwd fix.
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
#   2. A `git archive` overlay tree with ISO_MARKER.txt added -> must be PASS
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

# --- Case 2: git-archive overlay tree, with ISO_MARKER.txt added --------
T="$(mktemp -d)"
cleanup() { rm -rf "$T"; }
trap cleanup EXIT

( cd "$REPO_ROOT" && git archive HEAD ) | tar -x -C "$T"
echo only-in-overlay > "$T/ISO_MARKER.txt"

OVERLAY_VERDICT="$(cd "$T" && bash "$PROOF_CHECK" "test -f ./ISO_MARKER.txt" 2>&1 | grep -o 'verdict=[A-Z]*')"
echo "overlay tree verdict: ${OVERLAY_VERDICT:-<none>}"

if [[ "$OVERLAY_VERDICT" != "verdict=PASS" ]]; then
  fail "overlay tree (with ISO_MARKER.txt) did not PASS (got '${OVERLAY_VERDICT:-<none>}') — proof-check is not respecting caller cwd"
fi

echo "PASS: proof-check.sh respects caller cwd (live=${LIVE_VERDICT:-<none>}, overlay=${OVERLAY_VERDICT})"
