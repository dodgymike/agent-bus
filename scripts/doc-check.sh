#!/usr/bin/env bash
# scripts/doc-check.sh — section-scoped documentation assertions, and a doc budget.
#
# ---------------------------------------------------------------------------
# WHY THIS EXISTS
# ---------------------------------------------------------------------------
# The failure this closes is a doc proof that passes on an incidental match in a
# DIFFERENT SECTION of the file. `grep -q 'reserved, not chosen' CLAUDE.md` is
# green whether the phrase is in the section the task changed or in an unrelated
# one — so a task claiming "I documented X under heading Y" can be proven by a
# string that was already there. That has already green-lit a wrong closure here.
#
# WHAT IT DOES NOT CLOSE, stated because the reverse would be the same defect one
# level up — a proof instrument documenting a guarantee it does not provide:
# a needle matching text inside a CODE FENCE *within the target section* still
# passes. Fences are skipped for HEADING detection (so a fenced `#` cannot be
# mistaken for structure), but fenced content is still part of the section body.
# If a section quotes its own old text in a fence, scope it with a needle that
# does not appear there.
#
# Three MORE blind spots, listed because an instrument that understates its own
# limits is the defect it exists to close. Heading detection is ATX-only and
# fence-parity-dependent, so each of these makes a section run to END OF FILE —
# which can only make a needle EASIER to find, i.e. a false PASS:
#   * SETEXT headings ("Title" underlined with === or ---) are not recognised at
#     all, so they neither start nor end a section.
#   * INDENTED ATX headings (1-3 leading spaces, valid CommonMark) are not
#     recognised either; only a `#` in column 1 counts.
#   * An UNBALANCED fence (an opening ``` with no close) inverts the fence toggle
#     for the rest of the file, hiding every heading after it.
# Measured over the 13 docs this repo checks, on 2026-08-21: all fences balanced,
# zero indented ATX outside fences, and one setext-SHAPED line — the closing `---`
# of the YAML front matter in .claude/agents/deep-diver.md, which CommonMark would
# read as an h2 titled "model: opus". Ignoring setext is what keeps that out of
# the heading table, so all three limits are LATENT here rather than live. They
# are not fixed; do not assume the parser is a CommonMark parser.
#
# `section` fixes that by computing the heading's line range and asserting the
# needle occurs INSIDE it.
#
# ---------------------------------------------------------------------------
# THIS SCRIPT IS ITSELF A PROOF INSTRUMENT, SO IT IS BUILT TO FAIL LOUDLY
# ---------------------------------------------------------------------------
# Broken proof commands are a recurring failure here, and the thing they have in
# common is that every one REPORTED SUCCESS. The shapes that did it, and what is
# done here instead:
#
# (This paragraph used to open "Eight broken proof commands ... in a single day".
# That count is not sourceable: "eight" appears nowhere in this repo except here
# and in the CONTRACTS-AGENT.md entry that cites this file, so it has been dropped
# rather than repeated — an unverifiable number in a proof instrument is the
# defect this instrument exists to catch. What IS measurable, and reproducible on
# demand: `python3 scripts/proof-cmd-audit.py` reported 114 stored proofs naming a
# Go test target that does not exist, on 2026-08-21. That is ONE failure kind, not
# a census of the list below.)
#
#   * `grep -qF ... && echo STILL_FALSE || echo FIXED` — exits 0 on BOTH
#     branches. No `&& echo … || echo …` appears in this file; every check sets
#     a failure counter and the exit status is computed from it once, at the end.
#   * a conjunction ending in `sed -n`, which exits 0 unconditionally. No
#     pipeline here ends in a formatting command; matching is done in awk/grep
#     and its result is captured, never inferred from a pipeline's tail.
#   * a proof naming a test/file that does not exist — passing vacuously.
#     `section` exits NON-ZERO when the heading is absent, and `budget` exits
#     non-zero when a listed path is missing. Absence can never be a pass.
#   * a string pinned across a line wrap, which produced a spurious result.
#     Needles and haystacks are BOTH whitespace-normalised (every run of
#     whitespace, newlines included, becomes one space) before comparison, so a
#     wrapped phrase matches exactly as an unwrapped one does.
#
# Matching is LITERAL (fixed-string), never regex: a needle containing `.` or
# `*` must not quietly match something else.
#
# It must NOT call scripts/proof-check.sh — proof-check runs doc-check, and the
# reverse would recurse (see task 69eb6f56).
#
# ---------------------------------------------------------------------------
# USAGE
# ---------------------------------------------------------------------------
#   doc-check.sh section <file> '<heading>' '<needle>' [<needle>...]
#   doc-check.sh budget
#   doc-check.sh --selftest
#
# exit: 0 pass · 1 fail · 2 usage
#
# `<heading>` is matched against markdown ATX headings, either as the full line
# ("## Go conventions") or as the heading text alone ("Go conventions"). The
# section runs from the heading to the next heading of the SAME OR SHALLOWER
# level, or end of file. Shallower counts deliberately: a parent heading ends a
# subsection, and stopping early can only make an assertion HARDER to satisfy.
# A range that ran past its parent could manufacture a pass.
#
# If the heading matches MORE THAN ONE place in the file this FAILS and names the
# matching line numbers; it never picks one. Duplicated headings are live in these
# docs (AGENT_PROTOCOL.md has both `### Exit codes` and `## Exit codes`), and
# binding to the first match produced a false FAIL in one direction and a silent
# pass against the wrong section in the other. Disambiguate by giving the full
# heading line, which is compared verbatim and so distinguishes the levels.
#
# `<file>` must be INSIDE the tree: absolute paths and `..` traversal are refused.
# A proof running in a `git archive HEAD` overlay must not be able to reach the
# live worktree and prove uncommitted text. The check is lexical, so it does not
# follow a symlink that is itself inside the tree.

set -uo pipefail

EXIT_PASS=0
EXIT_FAIL=1
EXIT_USAGE=2

BUDGETS_TSV="${DOC_CHECK_BUDGETS:-docs/doc-budgets.tsv}"
PRESERVE_TSV="${DOC_CHECK_PRESERVE:-docs/doc-preserve.tsv}"

usage() {
  cat >&2 <<'USAGE'
usage: doc-check.sh section <file> '<heading>' '<needle>' [<needle>...]
       doc-check.sh budget
       doc-check.sh --selftest

exit: 0 pass · 1 fail · 2 usage
USAGE
  exit "$EXIT_USAGE"
}

# norm — collapse every run of whitespace (newlines included) to a single space
# and trim. This is what makes a line-wrapped phrase match an unwrapped one.
norm() { tr '\n' ' ' | tr -s '[:space:]' ' ' | sed -e 's/^ //' -e 's/ $//'; }

# is_uint STR — true only for a non-empty string of ASCII digits.
#
# WHY THIS EXISTS, and it is the sharpest lesson in this file: `[ "$a" -gt "$b" ]`
# with a non-numeric $b prints "integer expression expected" to stderr and
# returns 2. An `if` treats 2 as FALSE, so the overrun branch is skipped, the
# failure counter never increments, and budget reports PASS with exit 0 — a
# SILENT FALSE PASS from a typo like `28,781` or `8192B`. proof-check.sh judges
# non-Go proofs purely by exit status, so that would propagate straight into
# task-completion gating.
#
# That is the exact defect class this whole script exists to close, found by the
# security gate INSIDE the instrument built to catch it (2026-08-15). A typed
# comparison must never be reached with an untyped operand.
is_uint() { case "${1:-}" in ''|*[!0-9]*) return 1 ;; *) return 0 ;; esac; }

# path_is_contained PATH — reject anything that escapes the repo.
#
# .tsv files are trusted checked-in content, so this is defence in depth rather
# than a boundary: it stops a typo or a bad merge silently measuring /etc/passwd
# and reporting it as a documentation file.
path_is_contained() {
  case "${1:-}" in
    /*)                      return 1 ;;
    ..|../*|*/../*|*/..)     return 1 ;;
    '')                      return 1 ;;
    *)                       return 0 ;;
  esac
}

# section_range FILE HEADING — on stdout, either:
#   "START END"                                  (1-based, inclusive), return 0
#   "AMBIGUOUS <count> <line,line,...> <kind>"    followed by one "<line>: <full
#                                                 heading>" detail line per match,
#                                                 return 0
# and return 1 if the heading matches nothing. <kind> is DISTINCT when the matched
# headings are not all spelled identically (so one CAN be pinned by passing its
# full line) or SAME-SPELLING when they are (so none can — see below).
#
# AMBIGUITY IS A FAILURE, NOT A COIN TOSS. Binding silently to the first match
# produced BOTH wrong answers on live docs — AGENT_PROTOCOL.md carries `### Exit
# codes` and, further down, the canonical `## Exit codes` table. A needle from the
# canonical table FAILED (the range stopped at the earlier section), and a needle
# from the earlier section PASSED against a section the caller never meant. Either
# way the proof asserts something the document does not say, which is the exact
# defect class this whole script exists to close, one level down. So the scan no
# longer stops at the first section end: every heading in the file is examined and
# a count > 1 is reported to the caller, which fails loudly and names the lines.
#
# A full heading line ("## Exit codes") is compared verbatim, so it pins one of a
# DISTINCT set. It does NOTHING for a SAME-SPELLING set: the comparison is against
# the same string either way, and telling the caller to "use the full heading line"
# when they already did is advice that cannot be followed. Those headings are
# unassertable until the document makes them unique.
#
# NO LIST OF LIVE DUPLICATES IS KEPT HERE. The one that used to be was dated
# 2026-08-16 and was wrong in BOTH directions by 2026-08-21 — it named four
# headings that were still singular on the date claimed, and omitted three that
# were already duplicated — inside the instrument that exists to catch exactly
# that. A hand-maintained list of a moving property is a stale claim waiting to
# happen, so what is recorded instead is how to MEASURE it (same parser rules as
# below: fence-aware, ATX in column 1):
#
#   awk '/^[ \t]*(```|~~~)/{f=!f;next} !f && /^#+[ \t]/{t=$0; sub(/^#+[ \t]+/,"",t); \
#        sub(/[ \t]+$/,"",t); print FILENAME": "t}' <docs...> | sort | uniq -d
#
# Over the 13 docs this repo checks, that reported 11 duplicated heading keys on
# 2026-08-21 (2 DISTINCT, still pinnable; 9 SAME-SPELLING, not). Re-run it; do not
# trust this sentence.
section_range() {
  local file=$1 heading=$2
  # ENVIRON, not `-v`. `awk -v want=...` INTERPRETS backslash escapes, so a
  # heading containing \n or \t never matched itself — the header states literal
  # matching as a guarantee and needles honour it, but headings did not (security
  # gate LOW, 2026-08-21; latent, no live doc has such a heading). The value is
  # assigned on this command only, so nothing ambient can supply it.
  #
  # THE PREVIOUS VERSION OF THIS COMMENT WAS WRONG, and being wrong here is worse
  # than being silent: it claimed that an awk without ENVIRON would leave `want`
  # empty "so every heading would be reported NOT FOUND — loud, and in the safe
  # direction". Measured under a simulated no-ENVIRON awk, the opposite happens —
  # an empty `want` matches a heading whose TEXT is empty (a bare "# " line),
  # scopes the WHOLE FILE, and prints
  #   doc-check: PASS: emptyhead.md — 1/1 needles inside "TotallyBogusHeading" (lines 1-7)
  # exit 0, for a heading that does not exist. A comment asserting a safety
  # property the code does not have is exactly the failure this script exists to
  # catch, so the property is now IMPLEMENTED rather than described: the BEGIN
  # block bails on an empty want. ENVIRON is POSIX-mandated and cmd_section
  # already refuses an empty heading argument, so this is the second lock on the
  # same door — kept because the first one is an argument check and this one is
  # a data check, and they fail for different reasons.
  DOC_CHECK_WANT="$heading" awk '
    BEGIN {
      want = ENVIRON["DOC_CHECK_WANT"]
      if (trim(want) == "") exit 1
    }
    function trim(s) { gsub(/^[ \t]+|[ \t]+$/, "", s); return s }
    # Fenced blocks are NOT markdown structure. Without this toggle a shell
    # comment inside ``` is read as a level-1 heading, which produced BOTH
    # failure directions on real docs (AGENT_PROTOCOL.md read 44 headings where
    # 39 exist): a false FAIL when a fenced "#" truncated a real section, and a
    # false PASS when a fenced "# X" was selected as the start and its level-1
    # range swallowed the real section plus its neighbours.
    /^[ \t]*(```|~~~)/ { fence = !fence; next }
    !fence && /^#+[ \t]/ {
      line = $0
      hashes = line; sub(/[^#].*$/, "", hashes); lvl = length(hashes)
      text = line; sub(/^#+[ \t]+/, "", text); text = trim(text)
      if (trim(line) == trim(want) || text == trim(want)) {
        n = n + 1
        hits = (n == 1) ? NR : hits "," NR
        hitline[n] = NR; spell[n] = trim(line)
        if (n == 1) { start = NR; startlvl = lvl }
        else if (spell[n] != spell[1]) { distinct = 1 }
        next
      }
      # First heading at the same or a shallower level closes the section. It is
      # recorded rather than returned, because the scan has to run to EOF to know
      # whether the heading was ambiguous.
      if (start != 0 && end == 0 && lvl <= startlvl) { end = NR - 1 }
    }
    END {
      if (n == 0) exit 1
      if (n > 1) {
        # The detail lines quote each match verbatim, so the caller is never told
        # to construct a heading string that does not exist in the file.
        print "AMBIGUOUS", n, hits, (distinct ? "DISTINCT" : "SAME-SPELLING")
        for (i = 1; i <= n; i++) print hitline[i] ": " spell[i]
        exit 0
      }
      if (end == 0) end = NR
      print start, end
    }
  ' "$file"
}

cmd_section() {
  [ "$#" -ge 3 ] || usage
  local file=$1 heading=$2; shift 2

  # An EMPTY heading is a caller bug for the same reason an empty needle is, and
  # the asymmetry of rejecting one but not the other was indefensible: '' matches
  # a heading whose text is empty ("# " on its own), which scopes the WHOLE FILE
  # and lets a needle from any section pass — the incidental-match defect this
  # tool exists to close, arriving through the front door. Whitespace-only is
  # empty after normalisation, so both spellings are caught. Usage (exit 2), not
  # FAIL, because it is a malformed invocation rather than a false document.
  if [ -z "$(printf '%s' "$heading" | norm)" ]; then
    printf 'doc-check: usage: empty heading (it would scope the whole file and prove nothing about a section)\n' >&2
    return "$EXIT_USAGE"
  fi

  # Containment matters MOST here. The .tsv rows are checked-in content, but
  # `section <file>` is the one path an untrusted stored proof_cmd controls, and
  # an escaping path defeats the clean-overlay rule directly: a proof running
  # inside a `git archive HEAD` overlay could reach ../../<live worktree>/CLAUDE.md
  # and "prove" text that is not committed — which is precisely what the overlay
  # exists to prevent. Checked BEFORE existence, as the .tsv loops do, so the
  # reported reason is the real one. NOTE the check is LEXICAL: it stops absolute
  # paths and `..` traversal, not a symlink inside the tree that points outside.
  if ! path_is_contained "$file"; then
    printf 'doc-check: FAIL: path escapes the repo (absolute or ..): %s\n' "$file" >&2
    return "$EXIT_FAIL"
  fi

  if [ ! -f "$file" ]; then
    printf 'doc-check: FAIL: file does not exist: %s\n' "$file" >&2
    return "$EXIT_FAIL"
  fi

  local range
  range=$(section_range "$file" "$heading")
  if [ -z "$range" ]; then
    # Absence is a FAILURE, never a vacuous pass. This is the whole point.
    printf 'doc-check: FAIL: heading not found in %s: %s\n' "$file" "$heading" >&2
    return "$EXIT_FAIL"
  fi
  # More than one heading matched. Do NOT guess — an ambiguous proof that
  # resolves itself is a proof about a section the caller did not choose.
  case "$range" in
    'AMBIGUOUS '*)
      local ahead acount ahits akind
      ahead=$(printf '%s\n' "$range" | head -1)
      acount=$(printf '%s' "$ahead" | cut -d' ' -f2)
      ahits=$(printf '%s' "$ahead" | cut -d' ' -f3)
      akind=$(printf '%s' "$ahead" | cut -d' ' -f4)
      printf 'doc-check: FAIL: heading is AMBIGUOUS in %s: "%s" matches %s headings (lines %s) — refusing to guess which one you meant:\n' \
        "$file" "$heading" "$acount" "$ahits" >&2
      printf '%s\n' "$range" | tail -n +2 | sed 's/^/  /' >&2
      # The remedy depends on WHY it is ambiguous, and offering the wrong one is
      # worse than offering none: the old message always said "pin it with the
      # full heading line, e.g. \"## $heading\"", which for a caller who had
      # already passed "### Status codes" rendered as `"## ### Status codes"` —
      # nonsense — and for identically-spelled matches advised a remedy that
      # cannot work, since the full-line form compares the very same string.
      if [ "$akind" = DISTINCT ]; then
        printf 'doc-check: those differ in level or spelling: pass the heading EXACTLY as it appears on one of the lines above to pin that one.\n' >&2
      else
        printf 'doc-check: those are spelled identically, so NO argument can pin one — the full-line form compares the same string. Make the heading unique in %s, or assert against a unique parent heading.\n' "$file" >&2
      fi
      return "$EXIT_FAIL"
      ;;
  esac

  local start end body
  start=${range%% *}
  end=${range##* }
  # `-e` and `--` are both load-bearing, and this is the one input an untrusted
  # stored proof_cmd fully controls. A <file> named `-n`, `-s`, `-z` or `--debug`
  # is eaten by sed as an OPTION, leaving no file operand — so sed reads STDIN
  # while awk (which is not fooled) still returns the real file's line range. The
  # result is a verdict line that looks perfect:
  #   doc-check: PASS: -n — 1/1 needles inside "Alpha" (lines 1-3)   exit 0
  # for a file that does not contain the needle, and it passes both
  # path_is_contained and [ -f ] on the way (security gate MEDIUM, 2026-08-21).
  # NOTE the awk call above deliberately does NOT get `--`: awk stops option
  # parsing at the program text, and adding it there breaks 20 assertions.
  body=$(sed -n -e "${start},${end}p" -- "$file" | norm)

  # An EMPTY needle is a caller bug, not a passing assertion: `grep -qF -- ""`
  # matches anything, so it would exit 0 against any section and prove nothing.
  # Rejected as usage (exit 2), distinct from PASS and FAIL, and checked for ALL
  # needles before any is evaluated so a later empty one cannot ride in behind an
  # earlier real match. Whitespace-only is empty after normalisation, so both are
  # caught here.
  local needle
  for needle in "$@"; do
    if [ -z "$(printf '%s' "$needle" | norm)" ]; then
      printf 'doc-check: usage: empty needle (an empty string matches everything and proves nothing)\n' >&2
      return "$EXIT_USAGE"
    fi
  done

  local missing=0 checked=0
  for needle in "$@"; do
    checked=$((checked + 1))
    local n
    n=$(printf '%s' "$needle" | norm)
    if printf '%s' "$body" | grep -qF -- "$n"; then
      :
    else
      missing=$((missing + 1))
      printf 'doc-check: FAIL: needle absent from section %s [lines %s-%s]: %s\n' \
        "$heading" "$start" "$end" "$needle" >&2
    fi
  done

  if [ "$missing" -ne 0 ]; then
    printf 'doc-check: FAIL: %s — %d of %d needles absent from "%s" (lines %s-%s)\n' \
      "$file" "$missing" "$checked" "$heading" "$start" "$end" >&2
    return "$EXIT_FAIL"
  fi
  printf 'doc-check: PASS: %s — %d/%d needles inside "%s" (lines %s-%s)\n' \
    "$file" "$checked" "$checked" "$heading" "$start" "$end"
  return "$EXIT_PASS"
}

cmd_budget() {
  # `rows`/`prows` count DATA ROWS SEEN; `sized`/`preserved` count rows actually
  # measured. They diverge exactly when a row fails, which is why the no-rows
  # check below reads the former: a file with one unmeasurable row has rows=1 and
  # sized=0, and reporting that as "no data rows" would name the wrong defect.
  local failures=0 sized=0 preserved=0 rows=0 prows=0

  # LOW 2 (security gate, 2026-08-21): every ROW inside these files is contained,
  # so the files naming them must be too — the same asymmetry the empty-heading
  # rule had. They are settable from the environment (DOC_CHECK_BUDGETS /
  # DOC_CHECK_PRESERVE), which makes them the second caller-controlled path in
  # this script. To point at a file elsewhere, cd there.
  if ! path_is_contained "$BUDGETS_TSV"; then
    printf 'doc-check: FAIL: budgets path escapes the repo (absolute or ..): %s\n' "$BUDGETS_TSV" >&2
    return "$EXIT_FAIL"
  fi
  if ! path_is_contained "$PRESERVE_TSV"; then
    printf 'doc-check: FAIL: preserve path escapes the repo (absolute or ..): %s\n' "$PRESERVE_TSV" >&2
    return "$EXIT_FAIL"
  fi
  if [ ! -f "$BUDGETS_TSV" ]; then
    printf 'doc-check: FAIL: budgets file missing: %s\n' "$BUDGETS_TSV" >&2
    return "$EXIT_FAIL"
  fi
  if [ ! -f "$PRESERVE_TSV" ]; then
    printf 'doc-check: FAIL: preserve file missing: %s\n' "$PRESERVE_TSV" >&2
    return "$EXIT_FAIL"
  fi

  local path max actual
  while IFS=$'\t' read -r path max _rest || [ -n "${path:-}" ]; do
    case "${path:-}" in ''|'#'*) continue ;; esac
    rows=$((rows + 1))
    [ -n "${max:-}" ] || { printf 'doc-check: FAIL: %s: no max_bytes for %s\n' "$BUDGETS_TSV" "$path" >&2; failures=$((failures+1)); continue; }
    # Validate BEFORE the numeric comparison — see is_uint. A non-integer here
    # used to sail through as a PASS.
    if ! is_uint "$max"; then
      printf 'doc-check: FAIL: %s: max_bytes for %s is not a plain integer (digits only): %s\n' \
        "$BUDGETS_TSV" "$path" "$max" >&2
      failures=$((failures + 1)); continue
    fi
    if ! path_is_contained "$path"; then
      printf 'doc-check: FAIL: %s: path escapes the repo (absolute or ..): %s\n' "$BUDGETS_TSV" "$path" >&2
      failures=$((failures + 1)); continue
    fi
    if [ ! -f "$path" ]; then
      printf 'doc-check: FAIL: budgeted file does not exist: %s\n' "$path" >&2
      failures=$((failures + 1)); continue
    fi
    actual=$(wc -c <"$path" 2>/dev/null | tr -d ' ')
    # THE SAME LESSON AS is_uint ABOVE, ON THE OTHER OPERAND — and it was left
    # unclosed here. If the file cannot be measured (unreadable, or a container
    # uid mismatch — the LOW-8 scenario — or it vanishes between [ -f ] and here)
    # $actual is empty, `[ "" -gt 10 ]` prints "integer expression expected" and
    # returns 2, `if` reads 2 as FALSE, and the file is counted as WITHIN its
    # ceiling: "PASS: budget — 1 file(s) within ceiling", exit 0, for a file 6 B
    # over (security gate MEDIUM, 2026-08-21). It also matters beyond its
    # severity: it quietly stops measuring a failing row WITHOUT any .tsv diff,
    # which is the deferral argument for the row-deletion hole.
    if ! is_uint "$actual"; then
      printf 'doc-check: FAIL: could not measure %s (wc -c gave "%s") — a file that cannot be measured is never "within ceiling"\n' \
        "$path" "$actual" >&2
      failures=$((failures + 1)); continue
    fi
    sized=$((sized + 1))
    if [ "$actual" -gt "$max" ]; then
      printf 'doc-check: FAIL: %s is %s B, over its %s B ceiling by %s B\n' \
        "$path" "$actual" "$max" "$((actual - max))" >&2
      failures=$((failures + 1))
    fi
  done < "$BUDGETS_TSV"

  local phrase body
  while IFS=$'\t' read -r path phrase _rest || [ -n "${path:-}" ]; do
    case "${path:-}" in ''|'#'*) continue ;; esac
    prows=$((prows + 1))
    [ -n "${phrase:-}" ] || { printf 'doc-check: FAIL: %s: no phrase for %s\n' "$PRESERVE_TSV" "$path" >&2; failures=$((failures+1)); continue; }
    if ! path_is_contained "$path"; then
      printf 'doc-check: FAIL: %s: path escapes the repo (absolute or ..): %s\n' "$PRESERVE_TSV" "$path" >&2
      failures=$((failures + 1)); continue
    fi
    if [ ! -f "$path" ]; then
      printf 'doc-check: FAIL: preserve-listed file does not exist: %s\n' "$path" >&2
      failures=$((failures + 1)); continue
    fi
    preserved=$((preserved + 1))
    body=$(norm <"$path")
    if printf '%s' "$body" | grep -qF -- "$(printf '%s' "$phrase" | norm)"; then
      :
    else
      printf 'doc-check: FAIL: %s no longer contains its preserved phrase: %s\n' "$path" "$phrase" >&2
      failures=$((failures + 1))
    fi
  done < "$PRESERVE_TSV"

  # ABSENCE IS NEVER A PASS — and that includes the absence of ROWS. An empty or
  # comment-only .tsv measured nothing and used to print
  # "PASS: budget — 0 file(s) within ceiling, 0 preserved phrase(s) present",
  # exit 0, which contradicts this file's own headline guarantee. The counters
  # were already computed and printed; nothing read them. The sharp end is not a
  # hypothetical bad edit: deleting the single failing CLAUDE.md row turns the
  # ratchet green and leaves no number for a reviewer to notice, which is a much
  # easier move than raising a ceiling (reviewer gate, 2026-08-21). Once
  # CONTEXT-BUDGET-WIRE makes this a commit gate, an empty file must not be the
  # way through it.
  if [ "$rows" -eq 0 ]; then
    printf 'doc-check: FAIL: %s has no data rows — a budget that measures nothing cannot pass\n' "$BUDGETS_TSV" >&2
    failures=$((failures + 1))
  fi
  if [ "$prows" -eq 0 ]; then
    printf 'doc-check: FAIL: %s has no data rows — a preserve check that measures nothing cannot pass\n' "$PRESERVE_TSV" >&2
    failures=$((failures + 1))
  fi

  if [ "$failures" -ne 0 ]; then
    printf 'doc-check: FAIL: budget — %d failure(s) over %d sized file(s) and %d preserved phrase(s)\n' \
      "$failures" "$sized" "$preserved" >&2
    return "$EXIT_FAIL"
  fi
  printf 'doc-check: PASS: budget — %d file(s) within ceiling, %d preserved phrase(s) present\n' \
    "$sized" "$preserved"
  return "$EXIT_PASS"
}

# ---------------------------------------------------------------------------
# --selftest — assert the assert.
#
# A checker that cannot fail is worse than no checker, so this does not merely
# run `section` and hope: it asserts the NEGATIVE cases produce a non-zero exit.
# If `section` ever stops failing on an absent heading, this selftest fails.
# ---------------------------------------------------------------------------
cmd_selftest() {
  # RE-ENTRY IS CONTROLLED BY ARGV, NEVER BY THE ENVIRONMENT.
  #
  # This used to read DOC_CHECK_SELFTEST_INNER, which meant a variable inherited
  # from the surrounding shell silently dropped SEVEN assertions — precisely the
  # TMPDIR-injection and mktemp guards that prove the two fixes under review —
  # while both runs printed an identical `proof-check: verdict=PASS ... exit=0`,
  # because proof-check judges a wrapper proof by exit status alone (measured
  # 56/56 vs 49/49, security gate 2026-08-21). That is the same shape as the
  # defect this whole script exists to close, one level up: an ambient value
  # quietly changing what a proof proves, with the verdict line unchanged.
  # An internal flag must not be settable by the environment the tool runs in, so
  # it is now a private argv token that only this function passes to itself.
  #
  #   (none)                      full  — every assertion, including re-entry
  #   --internal-reentry-guards   the temp-dir guards run; the env probe does not
  #   --internal-reentry-none     no re-entrant guards at all
  #
  # Two levels, not one, because the env probe has to spawn a run that DOES
  # perform the temp-dir guards; with a single token that child would suppress
  # the very thing being counted, and with no token it would recurse forever.
  local reentry
  case "${1:-}" in
    '')                        reentry=full ;;
    --internal-reentry-guards) reentry=guards ;;
    --internal-reentry-none)   reentry=none ;;
    *)                         usage ;;
  esac

  # NOT `local`. The EXIT trap fires after this function has already returned, so
  # a function-local $tmp would be out of scope by then: the cleanup would trip
  # the ${tmp:?} guard and the fixture dir would leak.
  #
  # `|| return` matters as much as the trap. When mktemp failed, $tmp was empty
  # and every fixture below was written to the FILESYSTEM ROOT — /doc.md, /ten.md,
  # /b.tsv, /p.tsv. That is four "Permission denied" lines for an ordinary user,
  # but this project deploys in a container where the selftest runs as root and
  # those writes SUCCEED (security gate, LOW 8, 2026-08-21).
  tmp=$(mktemp -d) || tmp=''
  # `|| return` alone was HALF the guard: it catches a non-zero exit but not a
  # mktemp that exits 0 having printed nothing. $tmp is then empty, every fixture
  # path below is absolute ("/doc.md"), and the ${tmp:?} belt CANNOT save the run
  # — an expansion error inside an EXIT trap does not change the exit status, so
  # as container root the selftest litters / and still exits 0 (security gate,
  # 2026-08-21). The value is therefore tested, not merely the status. Literal 1,
  # not "$EXIT_FAIL", for the reason given at the foot of this function.
  if [ -z "$tmp" ] || [ ! -d "$tmp" ]; then
    printf 'doc-check: FAIL: selftest could not create a temporary directory (mktemp -d failed or printed nothing)\n' >&2
    return 1
  fi
  # SINGLE quotes, deliberately: the trap body must be parsed when the trap fires,
  # not interpolated now. With "rm -rf '$tmp'" the value of TMPDIR was expanded
  # INTO a string bash re-parses at exit, so a TMPDIR containing one quote injected
  # its own commands onto a line beginning `rm -rf` — they RAN, and the selftest
  # still printed "SELFTEST PASS: 27/27" (security gate, MEDIUM 1, 2026-08-21).
  # ${tmp:?} is the second belt: an unset or empty $tmp aborts the expansion rather
  # than handing `rm -rf --` an empty argument.
  trap 'rm -rf -- "${tmp:?}"' EXIT

  # The section fixtures live one level DOWN, in $tmp/repo, which is the cwd every
  # `section` assertion runs from. That gives a `../` assertion a REAL file to
  # reach ($tmp/outside.md), so a containment test that passes is a genuine escape
  # rather than a file-not-found — a negative control has to be able to succeed.
  mkdir -p "$tmp/repo/sub"
  printf '## Outside\n\noutside-the-repo-needle lives here.\n' > "$tmp/outside.md"

  cat >"$tmp/repo/doc.md" <<'FIXTURE'
# Title

## Alpha

The alpha section contains a distinctive-alpha-needle here.
It also contains a phrase that is wrapped across
the line boundary on purpose.
Matching is literal, so some ordinary text here must stay out of reach
of a needle spelled with regex metacharacters. This section deliberately
contains none of them, or it would match its own negative controls.

## Beta

The beta section contains distinctive-beta-needle only, plus a literal
(.*) sequence that a fixed-string needle must still find as itself.

### Beta child

Child text.

## Gamma

Gamma text.

## Fenced

Before the fence.

```bash
# Rotation
echo "a shell comment that is NOT a heading"
```

distinctive-fenced-needle after the fence.

## AfterFence

Text belonging to AfterFence, not to Fenced.

## Duplicated

dup-one-needle is under the level-2 heading.

### Duplicated

dup-two-needle is under the level-3 heading of the SAME text.

## Twice

twice-first-needle is under the first of two identically spelled headings.

## Twice

twice-second-needle is under the second.
FIXTURE

  # A heading whose TEXT is empty ("# " alone). `section FILE '' <needle>` used to
  # match it, scope the whole file, and pass on a needle from any section — the
  # incidental-match defect, through the front door.
  printf '# \n\nempty-heading-body-needle\n\n## Real\n\nreal-section-needle\n' > "$tmp/repo/emptyhead.md"

  # A file whose NAME is a sed option, containing the heading but NOT the needle.
  # The exploit feeds the needle on stdin; sed with no file operand reads it and
  # the verdict line looks legitimate.
  printf '## Alpha\n\nthis file does not contain the needle at all\n' > "$tmp/repo/-n"
  # A heading carrying a literal backslash, which `awk -v` used to interpret.
  printf '## a\\nb\n\nbackslash-heading-needle\n' > "$tmp/repo/backslash.md"

  local checks=0 bad=0

  # The path to THIS script, absolute, resolved before anything changes cwd. The
  # assertions below re-invoke it as a subprocess; that is the only way to put the
  # dispatcher `case` arms at the foot of the file on the tested path.
  local self
  self=${BASH_SOURCE[0]}
  case "$self" in /*) : ;; *) self="$PWD/$self" ;; esac

  # assert NAME EXPECTED_EXIT <file> <heading> <needle>...
  #
  # Runs with cwd = $tmp/repo so the <file> argument is RELATIVE, which is the
  # only shape a stored proof_cmd may use: `section` now rejects absolute paths
  # and `..` traversal, so absolute fixture paths would fail for the wrong reason.
  assert() {
    local name=$1 want=$2; shift 2
    checks=$((checks + 1))
    local out rc
    out=$(cd "$tmp/repo" && cmd_section "$@" 2>&1); rc=$?
    if [ "$rc" -ne "$want" ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: %s — expected exit %d, got %d\n%s\n' "$name" "$want" "$rc" "$out" >&2
    fi
  }

  # The three the task names.
  assert 'heading-absent must FAIL'            "$EXIT_FAIL" 'doc.md'  'Nonexistent' 'distinctive-alpha-needle'
  assert 'needle-outside-section must FAIL'    "$EXIT_FAIL" 'doc.md'  'Alpha'       'distinctive-beta-needle'
  assert 'needle-inside-section must PASS'     "$EXIT_PASS" 'doc.md'  'Alpha'       'distinctive-alpha-needle'
  # Additional traps this repo has actually been bitten by.
  assert 'line-wrapped needle must PASS'       "$EXIT_PASS" 'doc.md'  'Alpha'       'wrapped across the line boundary'
  assert 'missing file must FAIL'              "$EXIT_FAIL" 'nope.md' 'Alpha'     'distinctive-alpha-needle'
  assert 'multiple needles, one absent, FAILs' "$EXIT_FAIL" 'doc.md'  'Alpha'       'distinctive-alpha-needle' 'distinctive-beta-needle'
  assert 'full-line heading form must PASS'    "$EXIT_PASS" 'doc.md'  '## Alpha'    'distinctive-alpha-needle'
  # A subsection must not leak its parent's siblings into range.
  assert 'section stops at same level'         "$EXIT_FAIL" 'doc.md'  'Beta'        'Gamma text'
  # ...but a subsection's own text IS inside its parent.
  assert 'parent contains child text'          "$EXIT_PASS" 'doc.md'  'Beta'        'Child text'

  # Code fences are not structure. Before the fence toggle, the `# Rotation`
  # inside ```bash was read as a level-1 heading — which BOTH truncated the real
  # section (false FAIL) and, when selected as a start, swallowed its neighbours
  # (false PASS). Reviewer finding, 2026-08-15.
  assert 'fence does not truncate a section'   "$EXIT_PASS" 'doc.md'  'Fenced' 'distinctive-fenced-needle'
  assert 'fenced # is not a heading'           "$EXIT_FAIL" 'doc.md'  'Rotation' 'distinctive-fenced-needle'
  assert 'fenced section stops at next real h' "$EXIT_FAIL" 'doc.md'  'Fenced' 'belonging to AfterFence'
  # An empty needle matches everything; it must never read as a pass.
  assert 'empty needle is USAGE not pass'      "$EXIT_USAGE" 'doc.md'  'Alpha' ''
  assert 'whitespace-only needle is USAGE'     "$EXIT_USAGE" 'doc.md'  'Alpha' '   '
  assert 'empty needle after a real one'       "$EXIT_USAGE" 'doc.md'  'Alpha' 'distinctive-alpha-needle' ''

  # -------------------------------------------------------------------------
  # MATCHING IS LITERAL — reviewer gate, 2026-08-21. Both the header and
  # CONTRACTS-AGENT.md state fixed-string matching as a GUARANTEE, and changing
  # `grep -qF` to `grep -q` at either site left the selftest fully green: needle
  # `ordi.ary` then matches "some ordinary text here", and `.*` matches any
  # section at all. That is the incidental-match defect this tool exists to close,
  # arriving as a one-character edit nothing would catch.
  # -------------------------------------------------------------------------
  assert 'regex dot must not match a letter'   "$EXIT_FAIL" 'doc.md' 'Alpha' 'ordi.ary'
  assert 'dot-star needle must not match all'  "$EXIT_FAIL" 'doc.md' 'Alpha' '.*'
  assert 'bracket class must not match'        "$EXIT_FAIL" 'doc.md' 'Alpha' 'distinctive-alpha-needl[e]'
  # ...and the guarantee cuts both ways: text that IS metacharacters must still be
  # findable as itself. It lives in Beta so it cannot satisfy the negatives above.
  assert 'a literal (.*) IS found as itself'   "$EXIT_PASS" 'doc.md' 'Beta'  '(.*)'

  # -------------------------------------------------------------------------
  # Path containment on `section <file>` — security gate MEDIUM 2, 2026-08-21.
  # All three shapes below used to PASS. $tmp/outside.md EXISTS and contains the
  # needle, so each of these is a real read that containment must refuse; if the
  # check were removed they would go green again, which is what makes them a
  # guard rather than decoration.
  # -------------------------------------------------------------------------
  assert 'absolute section path FAILs'         "$EXIT_FAIL" "$tmp/outside.md" 'Outside' 'outside-the-repo-needle'
  assert 'dot-dot section path FAILs'          "$EXIT_FAIL" '../outside.md' 'Outside' 'outside-the-repo-needle'
  assert 'dot-dot mid-path section FAILs'      "$EXIT_FAIL" 'sub/../../outside.md' 'Outside' 'outside-the-repo-needle'

  # -------------------------------------------------------------------------
  # Duplicate headings — security gate MEDIUM 3, 2026-08-21. A repeated heading
  # used to bind silently to the FIRST match, so a needle from the second section
  # falsely FAILED and a needle from the first falsely PASSED for a section the
  # caller never named. Both directions are asserted, plus the escape hatch (the
  # full heading line, which distinguishes levels) so the fix cannot be "closed"
  # by making every duplicated heading unusable.
  # -------------------------------------------------------------------------
  assert 'ambiguous heading FAILs (1st match)' "$EXIT_FAIL" 'doc.md'  'Duplicated' 'dup-one-needle'
  assert 'ambiguous heading FAILs (2nd match)' "$EXIT_FAIL" 'doc.md'  'Duplicated' 'dup-two-needle'
  assert 'full line picks the level-2 section' "$EXIT_PASS" 'doc.md'  '## Duplicated' 'dup-one-needle'
  assert 'full line picks the level-3 section' "$EXIT_PASS" 'doc.md'  '### Duplicated' 'dup-two-needle'
  # The message has to NAME the ambiguity and the count, or a reader debugging a
  # red proof cannot tell it from an ordinary missing needle.
  local amb_out
  checks=$((checks + 1))
  amb_out=$(cd "$tmp/repo" && cmd_section 'doc.md' 'Duplicated' 'dup-one-needle' 2>&1)
  case "$amb_out" in
    *AMBIGUOUS*'matches 2 headings'*) : ;;
    *) bad=$((bad + 1))
       printf 'doc-check: SELFTEST FAIL: ambiguity must be reported by name and count, got:\n%s\n' "$amb_out" >&2 ;;
  esac

  # Identically-spelled duplicates: ambiguous with NO remedy, and the message must
  # say so instead of advising the full-line form. Security/spot-check finding,
  # 2026-08-21: the old text rendered as `pin it with the full heading line, e.g.
  # "## ### Status codes"` for a caller who had already passed the full line.
  assert 'same-spelling duplicate FAILs'       "$EXIT_FAIL" 'doc.md' 'Twice'    'twice-first-needle'
  assert 'same-spelling, full line FAILs too'  "$EXIT_FAIL" 'doc.md' '## Twice' 'twice-second-needle'
  local same_out dist_out
  checks=$((checks + 1))
  same_out=$(cd "$tmp/repo" && cmd_section 'doc.md' '## Twice' 'twice-first-needle' 2>&1)
  case "$same_out" in
    *'spelled identically'*'NO argument can pin one'*) : ;;
    *) bad=$((bad + 1))
       printf 'doc-check: SELFTEST FAIL: an unpinnable duplicate must say so, got:\n%s\n' "$same_out" >&2 ;;
  esac
  checks=$((checks + 1))
  case "$same_out" in
    *'"## ## Twice"'*|*'e.g. "## '*)
       bad=$((bad + 1))
       printf 'doc-check: SELFTEST FAIL: the message fabricated a heading by prefixing hashes onto the argument:\n%s\n' "$same_out" >&2 ;;
    *) : ;;
  esac
  # ...and the DISTINCT case must quote the real spellings, since that remedy IS
  # followable.
  checks=$((checks + 1))
  dist_out=$(cd "$tmp/repo" && cmd_section 'doc.md' 'Duplicated' 'dup-one-needle' 2>&1)
  case "$dist_out" in
    *'## Duplicated'*'### Duplicated'*'differ in level or spelling'*) : ;;
    *) bad=$((bad + 1))
       printf 'doc-check: SELFTEST FAIL: a pinnable duplicate must quote both spellings verbatim, got:\n%s\n' "$dist_out" >&2 ;;
  esac

  # A <file> named like an option must be a FILE, not a flag — security gate
  # MEDIUM, 2026-08-21. Both spellings are checked: in-process, and through the
  # dispatcher, because it is a stored proof_cmd that would carry it. stdin is
  # loaded with the needle, so a sed that reads stdin PASSES; that is the whole
  # exploit, and it is why the assertion is worth its four lines.
  local opt_out opt_rc
  checks=$((checks + 1))
  opt_out=$(cd "$tmp/repo" && printf 'distinctive-alpha-needle' | cmd_section '-n' 'Alpha' 'distinctive-alpha-needle' 2>&1); opt_rc=$?
  if [ "$opt_rc" -ne "$EXIT_FAIL" ]; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: a file named -n must be read as a FILE (expected exit %d, got %d) — sed is reading stdin:\n%s\n' "$EXIT_FAIL" "$opt_rc" "$opt_out" >&2
  fi
  checks=$((checks + 1))
  opt_out=$(cd "$tmp/repo" && printf 'distinctive-alpha-needle' | bash "$self" section '-n' 'Alpha' 'distinctive-alpha-needle' 2>&1); opt_rc=$?
  if [ "$opt_rc" -ne 1 ]; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: CLI section on a file named -n — expected exit 1, got %d:\n%s\n' "$opt_rc" "$opt_out" >&2
  fi
  # ...and an honest file still reads from the file, not from stdin.
  checks=$((checks + 1))
  opt_out=$(cd "$tmp/repo" && printf 'distinctive-beta-needle' | cmd_section 'doc.md' 'Alpha' 'distinctive-alpha-needle' 2>&1); opt_rc=$?
  if [ "$opt_rc" -ne "$EXIT_PASS" ]; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: an ordinary file must still be read normally (expected exit %d, got %d):\n%s\n' "$EXIT_PASS" "$opt_rc" "$opt_out" >&2
  fi

  # The heading must reach awk byte-for-byte: `awk -v` interprets backslashes.
  assert 'backslash heading matches itself'    "$EXIT_PASS" 'backslash.md' 'a\nb' 'backslash-heading-needle'
  assert 'backslash heading, full-line form'   "$EXIT_PASS" 'backslash.md' '## a\nb' 'backslash-heading-needle'

  # An EMPTY heading is usage, exactly as an empty needle is. emptyhead.md really
  # does contain a heading with empty text, so these would PASS without the guard.
  assert 'empty heading is USAGE not pass'     "$EXIT_USAGE" 'emptyhead.md' ''    'real-section-needle'
  assert 'whitespace-only heading is USAGE'    "$EXIT_USAGE" 'emptyhead.md' '  '  'real-section-needle'

  # ...and the SECOND lock, tested where it lives. cmd_section refuses an empty
  # heading argument, so the only way to reach section_range with an empty want is
  # to call it directly — which is also what an awk lacking ENVIRON would produce.
  # Without the BEGIN bail this resolves to "1 7": the empty want matches the
  # bare "# " heading and scopes the whole file, which is how a bogus heading name
  # scored a PASS under a simulated no-ENVIRON awk (security gate LOW,
  # 2026-08-21). Asserted on OUTPUT as well as status: a range printed with a
  # non-zero exit would still be a resolved heading.
  local sr_out sr_rc
  checks=$((checks + 1))
  sr_out=$(cd "$tmp/repo" && section_range 'emptyhead.md' '' 2>&1); sr_rc=$?
  if [ "$sr_rc" -eq 0 ] || [ -n "$sr_out" ]; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: an empty want must resolve to NO range (exit %d, output "%s") — it is matching the empty-text heading\n' "$sr_rc" "$sr_out" >&2
  fi
  checks=$((checks + 1))
  sr_out=$(cd "$tmp/repo" && section_range 'emptyhead.md' '   ' 2>&1); sr_rc=$?
  if [ "$sr_rc" -eq 0 ] || [ -n "$sr_out" ]; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: a whitespace-only want must resolve to NO range (exit %d, output "%s")\n' "$sr_rc" "$sr_out" >&2
  fi

  # -------------------------------------------------------------------------
  # budget assertions. Added 2026-08-15 after the security gate found a SILENT
  # FALSE PASS here: a non-integer max_bytes made `[ -gt ]` return 2, which an
  # `if` reads as false, so an overrun was never counted and budget exited 0.
  # These cases exist so that regression cannot come back unnoticed.
  # -------------------------------------------------------------------------
  printf '0123456789' > "$tmp/ten.md"          # exactly 10 bytes
  # assert_budget NAME EXPECTED_EXIT BUDGETS_CONTENT PRESERVE_CONTENT
  #
  # Runs with cwd = the fixture dir so the .tsv rows are REPO-RELATIVE, which is
  # what real rows look like. The containment check rejects absolute paths, so a
  # fixture written as /tmp/... would fail for the wrong reason — the positive
  # controls have to exercise the same shape production does.
  assert_budget() {
    local name=$1 want=$2 bud=$3 pre=$4
    checks=$((checks + 1))
    printf '%b' "$bud" > "$tmp/b.tsv"
    printf '%b' "$pre" > "$tmp/p.tsv"
    local out rc
    out=$(cd "$tmp" && BUDGETS_TSV="b.tsv" PRESERVE_TSV="p.tsv" cmd_budget 2>&1); rc=$?
    if [ "$rc" -ne "$want" ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: %s — expected exit %d, got %d\n%s\n' "$name" "$want" "$rc" "$out" >&2
    fi
  }

  local ok_pre='ten.md\t0123456789\n'
  assert_budget 'within ceiling PASSes'            "$EXIT_PASS" 'ten.md\t100\n'          "$ok_pre"
  assert_budget 'over ceiling FAILs'               "$EXIT_FAIL" 'ten.md\t5\n'            "$ok_pre"
  assert_budget 'exact-size boundary PASSes'       "$EXIT_PASS" 'ten.md\t10\n'           "$ok_pre"
  # The regression the gate found — each of these used to exit 0.
  assert_budget 'non-numeric max_bytes FAILs'      "$EXIT_FAIL" 'ten.md\tnot_a_number\n' "$ok_pre"
  assert_budget 'thousands-separator max FAILs'    "$EXIT_FAIL" 'ten.md\t1,000\n'        "$ok_pre"
  assert_budget 'unit-suffixed max FAILs'          "$EXIT_FAIL" 'ten.md\t8192B\n'        "$ok_pre"
  assert_budget 'negative max FAILs'               "$EXIT_FAIL" 'ten.md\t-1\n'           "$ok_pre"
  # Path containment.
  assert_budget 'absolute budget path FAILs'       "$EXIT_FAIL" '/etc/passwd\t999999\n'  "$ok_pre"
  assert_budget 'dot-dot budget path FAILs'        "$EXIT_FAIL" '../outside.md\t100\n'   "$ok_pre"
  assert_budget 'absolute preserve path FAILs'     "$EXIT_FAIL" 'ten.md\t100\n'          '/etc/passwd\troot\n'
  # Preserve semantics.
  assert_budget 'missing preserved phrase FAILs'   "$EXIT_FAIL" 'ten.md\t100\n'          'ten.md\tabsent-phrase\n'
  assert_budget 'missing budgeted file FAILs'      "$EXIT_FAIL" 'nope.md\t100\n'         "$ok_pre"
  # The preserve check is the SECOND grep -qF site; it needs its own coverage or
  # half the guarantee is untested. '012.456789' matches the fixture's literal
  # '0123456789' under plain grep and must not here.
  assert_budget 'regex preserve phrase FAILs'      "$EXIT_FAIL" 'ten.md\t100\n'          'ten.md\t012.456789\n'
  assert_budget 'dot-star preserve phrase FAILs'   "$EXIT_FAIL" 'ten.md\t100\n'          'ten.md\t.*\n'
  # A .tsv with no data rows measured NOTHING; that is never a pass.
  assert_budget 'empty budgets tsv FAILs'          "$EXIT_FAIL" ''                        "$ok_pre"
  assert_budget 'comment-only budgets tsv FAILs'   "$EXIT_FAIL" '# header only\n'         "$ok_pre"
  assert_budget 'empty preserve tsv FAILs'         "$EXIT_FAIL" 'ten.md\t100\n'          ''
  assert_budget 'comment-only preserve tsv FAILs'  "$EXIT_FAIL" 'ten.md\t100\n'          '# header only\n'
  assert_budget 'blank-line-only budgets FAILs'    "$EXIT_FAIL" '\n\n'                    "$ok_pre"

  # -------------------------------------------------------------------------
  # A SIZE THAT IS NOT AN INTEGER IS NOT A MEASUREMENT — security gate MEDIUM,
  # 2026-08-21, and the other half of the is_uint lesson. `wc` is stubbed to
  # produce nothing (and then garbage) exactly as the mktemp stub is: an exported
  # function, so it works whatever the uid, where a chmod-000 fixture would be
  # vacuous as container root. The row is deliberately OVER its ceiling, so
  # without the guard the run reports "within ceiling" and exits 0.
  # -------------------------------------------------------------------------
  local wcs_out wcs_rc
  printf 'ten.md\t5\n' > "$tmp/b.tsv"; printf 'ten.md\t0123456789\n' > "$tmp/p.tsv"
  wcs_out=$(cd "$tmp" && { wc() { return 0; }; export -f wc; DOC_CHECK_BUDGETS=b.tsv DOC_CHECK_PRESERVE=p.tsv bash "$self" budget 2>&1; }); wcs_rc=$?
  checks=$((checks + 1))
  if [ "$wcs_rc" -ne 1 ]; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: an unmeasurable file must not pass its ceiling — expected exit 1, got %d:\n%s\n' "$wcs_rc" "$wcs_out" >&2
  fi
  checks=$((checks + 1))
  case "$wcs_out" in
    *'could not measure'*) : ;;
    *) bad=$((bad + 1))
       printf 'doc-check: SELFTEST FAIL: an unmeasurable file must be named as such, got:\n%s\n' "$wcs_out" >&2 ;;
  esac
  wcs_out=$(cd "$tmp" && { wc() { printf 'not-a-number\n'; }; export -f wc; DOC_CHECK_BUDGETS=b.tsv DOC_CHECK_PRESERVE=p.tsv bash "$self" budget 2>&1; }); wcs_rc=$?
  checks=$((checks + 1))
  if [ "$wcs_rc" -ne 1 ]; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: a non-numeric size must not pass its ceiling — expected exit 1, got %d:\n%s\n' "$wcs_rc" "$wcs_out" >&2
  fi
  # A row that fails to measure is NOT "no data rows" — the counters are split so
  # the message names the real defect.
  checks=$((checks + 1))
  case "$wcs_out" in
    *'has no data rows'*)
       bad=$((bad + 1))
       printf 'doc-check: SELFTEST FAIL: an unmeasurable row was reported as a missing row:\n%s\n' "$wcs_out" >&2 ;;
    *) : ;;
  esac

  # The .tsv paths are caller-controlled through the environment, so they are
  # contained like the rows inside them (security gate LOW 2, 2026-08-21).
  local envp_out envp_rc
  envp_out=$(cd "$tmp" && DOC_CHECK_BUDGETS=/etc/passwd DOC_CHECK_PRESERVE=p.tsv bash "$self" budget 2>&1); envp_rc=$?
  checks=$((checks + 1))
  if [ "$envp_rc" -ne 1 ] || case "$envp_out" in *'budgets path escapes'*) false ;; *) true ;; esac; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: an absolute DOC_CHECK_BUDGETS must be refused (exit %d):\n%s\n' "$envp_rc" "$envp_out" >&2
  fi
  envp_out=$(cd "$tmp" && DOC_CHECK_BUDGETS=b.tsv DOC_CHECK_PRESERVE=../outside.md bash "$self" budget 2>&1); envp_rc=$?
  checks=$((checks + 1))
  if [ "$envp_rc" -ne 1 ] || case "$envp_out" in *'preserve path escapes'*) false ;; *) true ;; esac; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: a dot-dot DOC_CHECK_PRESERVE must be refused (exit %d):\n%s\n' "$envp_rc" "$envp_out" >&2
  fi

  # -------------------------------------------------------------------------
  # The exit-code contract, asserted against LITERALS — reviewer BLOCKING B,
  # 2026-08-21. Every other assertion in this function compares a constant with
  # itself: `assert ... "$EXIT_FAIL"` expects whatever $EXIT_FAIL happens to be,
  # so setting EXIT_FAIL=0 kept the selftest at 27/27 while every failure exited
  # 0 — and proof-check.sh judges a shell proof PURELY by exit status, so all 16
  # stored proof_cmds that invoke this script would have scored PASS on a red
  # document (16 measured across a 783-task enumeration on 2026-08-21: every one
  # in the CONTEXT epic, every one still todo; the earlier "29" was unreachable —
  # that epic holds 30 tasks in total). A contract
  # validated against itself is not a contract, so the numbers are written out
  # here and repeated as literals in every assert_cli call below.
  # -------------------------------------------------------------------------
  checks=$((checks + 1))
  if ! { [ "$EXIT_PASS" -eq 0 ] && [ "$EXIT_FAIL" -eq 1 ] && [ "$EXIT_USAGE" -eq 2 ]; }; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: exit codes must be literally 0 pass / 1 fail / 2 usage, got pass=%s fail=%s usage=%s\n' \
      "$EXIT_PASS" "$EXIT_FAIL" "$EXIT_USAGE" >&2
  fi

  # -------------------------------------------------------------------------
  # The DISPATCHER, exercised as a real invocation — reviewer BLOCKING A,
  # 2026-08-21. Everything above calls cmd_section/cmd_budget as in-process
  # FUNCTIONS, which leaves the `case` arms at the foot of this file — the only
  # path a real caller takes — with zero coverage. Mutating `exit $?` to `exit 0`
  # there left the selftest at 27/27 while a genuinely failing check printed FAIL
  # and exited 0, and proof-check.sh scored it PASS: this script's own header
  # warning about a pipeline whose tail swallows the status, one level up. It is
  # not hypothetical — CONTEXT-STALE-NOTYET adds a `forbid` arm next, and
  # anything inserted between the command and `exit $?` reintroduces it.
  #
  # assert_cli NAME EXPECTED_EXIT <argv...> — runs `bash <this script> <argv...>`
  # in a SUBPROCESS from $tmp/repo, and takes the expected status as a literal.
  # -------------------------------------------------------------------------
  assert_cli() {
    local name=$1 want=$2; shift 2
    checks=$((checks + 1))
    local out rc
    out=$(cd "$tmp/repo" && bash "$self" "$@" 2>&1); rc=$?
    if [ "$rc" -ne "$want" ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: %s — expected exit %d from the dispatcher, got %d\n%s\n' "$name" "$want" "$rc" "$out" >&2
    fi
  }

  assert_cli 'CLI section: needle present exits 0' 0 section doc.md 'Alpha' 'distinctive-alpha-needle'
  assert_cli 'CLI section: needle absent exits 1'  1 section doc.md 'Alpha' 'distinctive-beta-needle'
  assert_cli 'CLI section: heading absent exits 1' 1 section doc.md 'Nonexistent' 'distinctive-alpha-needle'
  assert_cli 'CLI section: ambiguity exits 1'      1 section doc.md 'Duplicated' 'dup-one-needle'
  assert_cli 'CLI section: escaping path exits 1'  1 section '../outside.md' 'Outside' 'outside-the-repo-needle'
  assert_cli 'CLI section: empty needle exits 2'   2 section doc.md 'Alpha' ''
  assert_cli 'CLI section: too few args exits 2'   2 section doc.md 'Alpha'
  assert_cli 'CLI: unknown subcommand exits 2'     2 frobnicate
  assert_cli 'CLI: no subcommand exits 2'          2
  assert_cli 'CLI: --help exits 2'                 2 --help
  assert_cli 'CLI budget: extra argument exits 2'  2 budget extra
  assert_cli 'CLI section: empty heading exits 2'  2 section emptyhead.md '' 'real-section-needle'
  # The private re-entry tokens are argv, so they are part of the dispatcher's
  # contract and must be validated there: accepted after --selftest, rejected
  # anywhere else, and no OTHER argument accepted after --selftest.
  assert_cli 'CLI: --selftest junk arg exits 2'    2 --selftest --not-a-real-token
  assert_cli 'CLI: --selftest two args exits 2'    2 --selftest --internal-reentry-none extra
  assert_cli 'CLI: private token alone exits 2'    2 --internal-reentry-none

  # `budget` through the dispatcher, and through the DOC_CHECK_BUDGETS /
  # DOC_CHECK_PRESERVE env vars — the pair a real caller has to use, and which the
  # in-process assert_budget above cannot exercise because it overrides the
  # variables directly.
  local cli_bud cli_rc
  printf 'ten.md\t100\n' > "$tmp/b.tsv"; printf 'ten.md\t0123456789\n' > "$tmp/p.tsv"
  checks=$((checks + 1))
  cli_bud=$(cd "$tmp" && DOC_CHECK_BUDGETS=b.tsv DOC_CHECK_PRESERVE=p.tsv bash "$self" budget 2>&1); cli_rc=$?
  if [ "$cli_rc" -ne 0 ]; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: CLI budget within ceiling — expected exit 0 from the dispatcher, got %d\n%s\n' "$cli_rc" "$cli_bud" >&2
  fi
  printf 'ten.md\t5\n' > "$tmp/b.tsv"
  checks=$((checks + 1))
  cli_bud=$(cd "$tmp" && DOC_CHECK_BUDGETS=b.tsv DOC_CHECK_PRESERVE=p.tsv bash "$self" budget 2>&1); cli_rc=$?
  if [ "$cli_rc" -ne 1 ]; then
    bad=$((bad + 1))
    printf 'doc-check: SELFTEST FAIL: CLI budget over ceiling — expected exit 1 from the dispatcher, got %d\n%s\n' "$cli_rc" "$cli_bud" >&2
  fi

  # -------------------------------------------------------------------------
  # The temp-dir guards — security gate MEDIUM 1 and LOW 8, 2026-08-21. These
  # RE-INVOKE this script with a hostile TMPDIR, because the defect lives in the
  # selftest's own setup and cannot be observed from inside a single run. The
  # inner run is told not to re-enter, or it would recurse forever.
  #
  # What was demonstrated before the fix: TMPDIR="/tmp/z'; touch /tmp/PWNED; :'"
  # ran the injected command AND printed "SELFTEST PASS: 27/27" — the instrument
  # reported success on a run that had just executed an attacker's command.
  # -------------------------------------------------------------------------
  if [ "$reentry" != none ]; then
    local marker hostile inner_out inner_rc

    marker="$tmp/tmpdir-injection-marker"
    hostile="$tmp/inj'; touch $marker; :'"
    mkdir -p "$hostile"
    inner_out=$(TMPDIR="$hostile" bash "$self" --selftest --internal-reentry-none 2>&1); inner_rc=$?
    checks=$((checks + 1))
    if [ -e "$marker" ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: a quote-bearing TMPDIR executed an injected command (%s exists) — the EXIT trap is interpolating $tmp again\n' "$marker" >&2
    fi
    checks=$((checks + 1))
    if [ "$inner_rc" -ne "$EXIT_PASS" ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: selftest under a quote-bearing TMPDIR — expected exit %d, got %d\n%s\n' "$EXIT_PASS" "$inner_rc" "$inner_out" >&2
    fi

    # The other half of the same defect: an UNBALANCED quote in TMPDIR made the
    # interpolated trap unparseable ("unexpected EOF while looking for matching
    # \'"), so cleanup never ran and the fixture dir leaked. Emptiness of the
    # hostile TMPDIR afterwards is the assertion — it is the only observable
    # difference between a trap that ran and one that died being parsed.
    local quoted leak_out leak_rc leftover
    quoted="$tmp/q'uote"
    mkdir -p "$quoted"
    leak_out=$(TMPDIR="$quoted" bash "$self" --selftest --internal-reentry-none 2>&1); leak_rc=$?
    checks=$((checks + 1))
    if [ "$leak_rc" -ne "$EXIT_PASS" ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: selftest under an unbalanced-quote TMPDIR — expected exit %d, got %d\n%s\n' "$EXIT_PASS" "$leak_rc" "$leak_out" >&2
    fi
    checks=$((checks + 1))
    leftover=$(ls -A "$quoted" 2>/dev/null)
    if [ -n "$leftover" ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: cleanup leaked its temp dir under a quote-bearing TMPDIR: %s\n' "$leftover" >&2
    fi

    # LOW 8: mktemp failing left $tmp empty, so the fixtures were written to the
    # filesystem ROOT (/doc.md, /ten.md, /b.tsv, /p.tsv) — denied for a user, but
    # this project deploys in a container and the selftest would run there as
    # root. The exit status alone does not discriminate (the old code also ended
    # non-zero, via ten failed assertions), so the MESSAGE is asserted: the run
    # must abort at mktemp, not stumble on and try to write to /.
    local nodir_out nodir_rc
    nodir_out=$(TMPDIR="$tmp/no-such-dir-here" bash "$self" --selftest --internal-reentry-none 2>&1); nodir_rc=$?
    checks=$((checks + 1))
    if [ "$nodir_rc" -ne "$EXIT_FAIL" ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: unusable TMPDIR must exit %d, got %d\n%s\n' "$EXIT_FAIL" "$nodir_rc" "$nodir_out" >&2
    fi
    checks=$((checks + 1))
    case "$nodir_out" in
      *'could not create a temporary directory'*) : ;;
      *) bad=$((bad + 1))
         printf 'doc-check: SELFTEST FAIL: an unusable TMPDIR must abort at mktemp, not write fixtures to /; got:\n%s\n' "$nodir_out" >&2 ;;
    esac
    checks=$((checks + 1))
    case "$nodir_out" in
      *'/doc.md'*|*'/ten.md'*|*'/b.tsv'*|*'/p.tsv'*)
         bad=$((bad + 1))
         printf 'doc-check: SELFTEST FAIL: fixtures were still attempted outside a temp dir:\n%s\n' "$nodir_out" >&2 ;;
      *) : ;;
    esac

    # The OTHER half of the mktemp guard: a mktemp that exits 0 having printed
    # NOTHING. `|| return` cannot see that, $tmp is then empty, every fixture path
    # is absolute, and the ${tmp:?} belt cannot save the run because an expansion
    # error inside an EXIT trap does not change the exit status — as container
    # root that is SELFTEST PASS with / littered (security gate, 2026-08-21). A
    # stub mktemp on PATH is the only honest way to reach that branch.
    # The stub is an EXPORTED SHELL FUNCTION, not a directory on PATH. The PATH
    # form was written first and was silently vacuous: $tmp is derived from
    # TMPDIR, the hostile TMPDIR fixture above deliberately contains "; :'", and a
    # directory whose name contains a COLON cannot be represented in PATH at all —
    # the entry is split at the colon, the stub is never found, the child runs a
    # real mktemp and passes, and the probe proves nothing. Caught by re-running
    # the original injection reproduction against the finished file (2026-08-21),
    # which is the only reason it was not shipped as a decorative guard.
    #
    # So the installation is VERIFIED before it is relied on. A probe that cannot
    # arrange its own precondition must say so, not quietly succeed.
    local stub_out stub_rc stub_seen
    stub_seen=$(mktemp() { return 0; }; export -f mktemp; bash -c 'mktemp -d; printf "[rc=%s]" "$?"' 2>&1)
    checks=$((checks + 1))
    if [ "$stub_seen" != '[rc=0]' ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: could not install a silent-empty mktemp stub (child saw %s) — the probe below would prove nothing\n' "$stub_seen" >&2
    fi
    stub_out=$(mktemp() { return 0; }; export -f mktemp; bash "$self" --selftest --internal-reentry-none 2>&1); stub_rc=$?
    checks=$((checks + 1))
    if [ "$stub_rc" -eq 0 ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: a silent empty mktemp must not produce a passing run; got exit 0:\n%s\n' "$stub_out" >&2
    fi
    checks=$((checks + 1))
    case "$stub_out" in
      *'could not create a temporary directory'*) : ;;
      *) bad=$((bad + 1))
         printf 'doc-check: SELFTEST FAIL: an empty mktemp result must abort at mktemp, not write fixtures to /; got:\n%s\n' "$stub_out" >&2 ;;
    esac
  fi

  # -------------------------------------------------------------------------
  # THE ENVIRONMENT MUST HAVE NO SAY IN WHAT RUNS — security gate MEDIUM,
  # 2026-08-21. Re-entry used to be keyed on an exported DOC_CHECK_SELFTEST_INNER,
  # so a variable inherited from the surrounding shell dropped seven assertions —
  # exactly the guards proving the injection and mktemp fixes — while both runs
  # printed an identical `proof-check: verdict=PASS ... exit=0` (56/56 vs 49/49).
  # This spawns a child that DOES perform the temp-dir guards, with those names
  # exported, and asserts the child's assertion COUNT is still the larger one.
  # Comparing counts rather than the exit status is the point: the broken version
  # exited 0 too. It needs its own token because a child given the plain token
  # would suppress the very guards being counted, and a child given none would
  # recurse forever.
  # -------------------------------------------------------------------------
  if [ "$reentry" = full ]; then
    local env_out env_rc env_n tok_n
    env_out=$(DOC_CHECK_SELFTEST_INNER=1 DOC_CHECK_INNER=1 SELFTEST_INNER=1 INNER=1 \
      bash "$self" --selftest --internal-reentry-guards 2>&1); env_rc=$?
    env_n=$(printf '%s\n' "$env_out" | sed -n 's/.*SELFTEST PASS: \([0-9][0-9]*\)\/.*/\1/p' | tail -1)
    tok_n=$(printf '%s\n' "$inner_out" | sed -n 's/.*SELFTEST PASS: \([0-9][0-9]*\)\/.*/\1/p' | tail -1)
    checks=$((checks + 1))
    if [ "$env_rc" -ne 0 ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: a run with re-entry env vars exported must still pass; got exit %d\n%s\n' "$env_rc" "$env_out" >&2
    fi
    checks=$((checks + 1))
    if ! is_uint "${env_n:-}" || ! is_uint "${tok_n:-}"; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: could not read assertion counts (env=%s token-suppressed=%s) — the probe proves nothing without them\n' \
        "${env_n:-<none>}" "${tok_n:-<none>}" >&2
    elif [ "$env_n" -le "$tok_n" ]; then
      bad=$((bad + 1))
      printf 'doc-check: SELFTEST FAIL: exported env vars suppressed assertions (%s with them vs %s for a token-suppressed child) — re-entry must be argv-only\n' \
        "$env_n" "$tok_n" >&2
    fi
  fi

  if [ "$bad" -ne 0 ]; then
    printf 'doc-check: SELFTEST FAIL: %d of %d assertions did not hold\n' "$bad" "$checks" >&2
    # LITERAL 1, not "$EXIT_FAIL". When the broken thing IS the constant — the
    # reviewer's demonstration was EXIT_FAIL=0 — returning it would print
    # SELFTEST FAIL on stderr and then exit 0, and proof-check.sh judges a shell
    # proof purely by exit status. The selftest's own verdict must not be
    # expressed in the vocabulary it is testing.
    #
    # And `exit`, not `return`, for the same reason one level further out: found
    # while mutation-proving this file (2026-08-21), mutating the dispatcher's
    # `--selftest ... exit $?` arm to `exit 0` made a FAILING selftest print
    # "SELFTEST FAIL" and still exit 0. Returning hands the verdict to that arm;
    # exiting here means the failure path never passes through it at all. The PASS
    # path deliberately still returns — an arm that mishandles success is caught
    # by the assertions above, and a bug there cannot manufacture a false PASS.
    exit 1
  fi
  # Say so when re-entrant assertions were suppressed. Only this function's own
  # re-invocations can suppress them now — the token is argv, not environment —
  # but a silently smaller PASS is still exactly the shape of false confidence
  # this script exists to refuse, so the count is never printed unqualified.
  local scope=''
  case "$reentry" in
    guards) scope=' [internal child: env probe SKIPPED]' ;;
    none)   scope=' [internal child: re-entrant guards SKIPPED]' ;;
  esac
  printf 'doc-check: SELFTEST PASS: %d/%d assertions held (3 required by CONTEXT-DOCCHECK, %d added by gate findings)%s\n' \
    "$checks" "$checks" "$((checks - 3))" "$scope"
  return "$EXIT_PASS"
}

case "${1:---help}" in
  section)    shift; cmd_section "$@"; exit $? ;;
  budget)     shift; [ "$#" -eq 0 ] || usage; cmd_budget; exit $? ;;
  # The private --internal-reentry-* tokens are deliberately undocumented in
  # usage(): they are this script talking to itself, not a caller interface.
  # cmd_selftest validates them and rejects anything else with exit 2.
  --selftest) shift; [ "$#" -le 1 ] || usage; cmd_selftest "$@"; exit $? ;;
  -h|--help)  usage ;;
  *)          usage ;;
esac
