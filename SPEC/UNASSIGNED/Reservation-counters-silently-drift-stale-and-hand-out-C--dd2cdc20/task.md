# Reservation counters silently drift stale and hand out COLLIDING task keys (RELAY, DOCS, ACK, AUTH)

| Field | Value |
| --- | --- |
| Public id | `dd2cdc20-8920-4e5b-bf0a-668f439cc3a6` |
| Key | _(null in the export)_ |
| Epic | [UNASSIGNED](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T11:23:45.990471+00:00 |
| Updated | 2026-08-21T14:22:45.544984+00:00 |
| Completed | — |

## Proof command

```sh
set -euo pipefail; export TMPD=$(mktemp -d); trap 'rm -rf "$TMPD"' EXIT; bash scripts/spec-cloud.sh -s /api/v1/projects/agent-bus/reservations > "$TMPD/resv.json"; echo '[]' > "$TMPD/tasks.json"; off=0; while :; do bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/tasks?offset=$off" > "$TMPD/page.json"; export n=$(jq 'length' "$TMPD/page.json"); if [ "$n" -eq 0 ]; then break; fi; jq -s '.[0] + .[1]' "$TMPD/tasks.json" "$TMPD/page.json" > "$TMPD/tasks2.json"; mv "$TMPD/tasks2.json" "$TMPD/tasks.json"; off=$((off+n)); if [ "$off" -gt 5000 ]; then echo "SAFETY BREAK" >&2; break; fi; done; jq -r '.[].title' "$TMPD/tasks.json" | grep -oP '^[A-Z][A-Z0-9]*-\d+(?=:)' | sed -E 's/-([0-9]+)$/ \1/' | awk '{ if ($2+0 > max[$1]+0) max[$1]=$2 } END { for (e in max) print e, max[e] }' > "$TMPD/true_max.txt"; jq -r '.[] | select(.namespace | test("^(epic-)?task-key-[A-Za-z0-9]+$")) | .namespace' "$TMPD/resv.json" | sort -u > "$TMPD/namespaces.txt"; fail=0; while read -r ns; do export epic=$(echo "$ns" | sed -E 's/^(epic-)?task-key-//'); export truen=$(awk -v e="$epic" '$1==e {print $2}' "$TMPD/true_max.txt"); if [ -z "$truen" ]; then continue; fi; export resvn=$(jq -r --arg ns "$ns" '[.[] | select(.namespace==$ns) | .value] | max' "$TMPD/resv.json"); nextalloc=$((resvn+1)); if [ "$resvn" -lt "$truen" ]; then echo "FAIL: namespace $ns max=$resvn next_alloc=$nextalloc is BELOW true max key ${epic}-${truen} -- collides with an existing task key" >&2; fail=1; fi; done < "$TMPD/namespaces.txt"; if [ "$fail" -ne 0 ]; then echo "RESULT: FAIL -- reservation-namespace drift detected"; exit 1; fi; echo "RESULT: PASS -- all reservation counters at or above true max"; exit 0
```

## Description

WARNING -- until this is fixed, an agent reserving a key in the RELAY, DOCS, ACK or AUTH
epics MUST verify the returned reservation value against the LIVE task list (GET
/api/v1/projects/agent-bus/tasks, paginated with ?offset=) before using it. Trusting the
reservation blindly for these four epics is currently MORE dangerous than eyeballing the backlog,
because it returns a colliding value with the authority of the mechanism CLAUDE.md documents as
the fix for exactly that collision.

THE DEFECT
CLAUDE.md says reservations "allocate a unique monotonic value so two agents never collide." That
guarantee is FALSE today for at least four epics' task-key-<EPIC> reservation namespaces: the
counter is stale, sitting below the highest task key that already exists in the live backlog, so
the NEXT value handed out collides with a real, existing task.

Re-measured independently this session (2026-08-21), method below -- NOT copied from the filer's
numbers, and they have moved since that report was written (ACK and RELAY have each grown four
more real tasks in the meantime, all filed WITHOUT any reservation call at all -- see "root cause"
below):

| namespace              | max reserved | reserved_by / when        | true max task key | next alloc | collides with |
|-------------------------|-------------:|----------------------------|--------------------|-----------:|----------------|
| task-key-RELAY          | 51           | main, 2026-08-16 11:46     | RELAY-55           | 52         | RELAY-52 (done)|
| task-key-DOCS           | 5            | spec-keeper, 2026-08-21    | DOCS-30            | 6          | DOCS-6         |
| task-key-ACK            | 14           | main, 2026-08-16 12:50     | ACK-18             | 15         | ACK-15         |
| epic-task-key-ACK       | 1            | main, 2026-08-16 11:35     | ACK-18             | 2          | ACK-2 (done)   |
| task-key-AUTH           | 8            | feature-runner, 2026-08-16 | AUTH-10            | 9          | AUTH-9         |

The other ~20 task-key-<EPIC> namespaces are currently consistent (reservation max >= true max) --
checked exhaustively, not sampled; see proof_cmd, which checks every task-key-*/epic-task-key-*
namespace, not just these four.

METHOD (read-only, no reservation POSTed): GET /api/v1/projects/agent-bus/reservations (returns
all 348 rows unpaginated -- confirmed by requesting offset=340 and getting the same 348 back, so
there is no truncation to worry about there). GET /api/v1/projects/agent-bus/tasks paginated with
?offset= in steps of the page's own returned length until a page comes back empty (727 tasks
total, matching the four in-flight agents' claims not being included since they're mid-work, not
new keys). True max per epic computed from titles matching ^EPIC-N: at the start (colon required,
immediately after the number, so derived/follow-up keys like ACK-17-FU or RELAY-45-FU-CLI are
correctly excluded from the epic's *numbered* max -- they don't consume the reservation counter and
never should).

ROOT CAUSE -- CONFIRMED for two epics, REFUTED as the sole explanation for the other two
Two competing reservation-namespace conventions exist simultaneously: task-key-<EPIC> (24
namespaces, the form CLAUDE.md documents) and epic-task-key-<EPIC> (created only for ACK, AUTH,
DOCS). Timestamps confirm exactly how this happened, and it is NOT one uniform story:

CONFIRMED for AUTH and DOCS: agent "main" bulk-seeded epic-task-key-AUTH (1..10, all within 13
seconds on 2026-08-16 07:30) and epic-task-key-DOCS (1..29, all within 6 seconds on 2026-08-16
08:36) to match each epic's true max AT THAT MOMENT, then continued forward correctly in the NEW
namespace (AUTH-10, DOCS-30 both trace to it) -- but the OLD task-key-<EPIC> namespace, the one
CLAUDE.md actually documents, was left exactly where it stood before the seed and other agents
(spec-keeper, feature-runner) kept reserving from IT, unaware a second counter now existed. That is
the two-namespace hypothesis, and it is directly confirmed by the timestamp/value match.

REFUTED as the explanation for ACK-15..18 and RELAY-52..55: these eight task records were created
on 2026-08-16 (RELAY-52, RELAY-53) and 2026-08-21 (ACK-15..18, RELAY-54, RELAY-55) with NO
corresponding reservation call in EITHER namespace at or near their creation time -- task-key-ACK
and epic-task-key-ACK both stall at 2026-08-16 12:50 and 11:35 respectively, task-key-RELAY stalls
at 2026-08-16 11:46, yet ACK-15 was created 2026-08-21 09:45 and RELAY-54/55 on 2026-08-21 10:09,
each with a numbered title typed directly with no reservation event within hours either side. These
were filed by eyeballing the existing max and incrementing -- the EXACT anti-pattern CLAUDE.md
reserves to prevent -- not by the two-namespace confusion. So: two independent defects are
compounding on ACK and RELAY (both a stale counter AND agents bypassing reservation outright);
DOCS and AUTH show only the first.

A third, smaller instance of the same underlying bug: task-key-TOOLING (hyphen, correct form) vs
task-key:TOOLING (colon typo, by codex-1 2026-08-14) are two separate counters for one epic.
TOOLING-1 was legitimately reserved via the colon-typo'd namespace (value 1, 9s before task
creation); when spec-keeper later reserved via the CORRECT hyphenated namespace it got value 1
back -- collided with the already-existing TOOLING-1 -- and had to burn a second reservation (value
2) to get an unused number for TOOLING-2. This is the SAME class of bug (a phantom duplicate
counter) caught and self-corrected once, in miniature, and it is direct proof this defect class
already cost a real reservation.

EMPIRICAL CONFIRMATION (from the filer, reproduced in spirit above): a spec-keeper filing a
follow-up this session POSTed to task-key-DOCS and got back 5, saw DOCS-5 already existed, refused
to use it, and filed with a descriptive title instead -- a real allocation that would have
collided, caught only by a human/agent double-check the mechanism exists specifically to make
unnecessary.

RELATED, NOT DUPLICATE: CONTEXT-RESERVE-CANON (3aea21a7-f4e1-4b3b-b7af-cdf81c3c2c7d, todo) is about
reconciling four AGENT DEFINITION FILES that disagree on the reservation-seeding INSTRUCTIONS text
(whether to "seed past the existing max," which a 2026-08-08 server change turned into a
409-burning loop). That is a documentation-consistency task about what agents are TOLD to do. This
task is about the LIVE COUNTER STATE itself already being wrong, independent of what any doc says.
Searched titles/descriptions for "reservation", "namespace", "collide", "collision", "task key",
"task-key", "epic-task-key" across all 727 tasks -- CONTEXT-RESERVE-CANON is the only adjacent
hit; nothing duplicates this task's scope.

SCOPE (decide and record -- do NOT implement in this task):
1. Decide the single canonical namespace form (task-key-<EPIC> vs epic-task-key-<EPIC>) and record
   the decision.
2. Reconcile every stale counter identified above (and any others the proof_cmd surfaces on re-run)
   to at or above its epic's true max, via legitimate reservation POSTs in the CANONICAL namespace
   only.
3. Retire or explicitly alias the losing namespace convention, INCLUDING the task-key:TOOLING colon
   typo (namespaces cannot be renamed or deleted via the read/write API as far as this
   investigation established -- record whatever the server's actual capability is here rather than
   assuming).
4. Note that filing ACK-15..18 and RELAY-52..55 without reservation is a SEPARATE, recurring
   process defect (agents bypassing reservation outright) that reconciling the counters does not
   fix by itself -- flag it as a distinct follow-up concern, not something this task's proof_cmd can
   catch going forward (the proof only detects counter/task drift that has ALREADY happened, not an
   agent choosing not to reserve at all).
5. CLAUDE.md is OPERATOR-OWNED -- prepare the corrected reservation-guidance text (which namespace
   form wins) and ROUTE it to the operator; do not edit CLAUDE.md directly.

PROOF_CMD -- proven to go RED today
Verified via `bash scripts/proof-check.sh '<proof_cmd>'` against the live server on 2026-08-21.
Verdict quoted verbatim:

    proof-check: class: wrapper,file-assertion
    proof-check: running (cwd /mnt/sdb4/mike/mike/source/agent-bus)...
    FAIL: namespace epic-task-key-ACK max=1 next_alloc=2 is BELOW true max key ACK-18 -- collides with an existing task key
    FAIL: namespace task-key-ACK max=14 next_alloc=15 is BELOW true max key ACK-18 -- collides with an existing task key
    FAIL: namespace task-key-AUTH max=8 next_alloc=9 is BELOW true max key AUTH-10 -- collides with an existing task key
    FAIL: namespace task-key-DOCS max=5 next_alloc=6 is BELOW true max key DOCS-30 -- collides with an existing task key
    FAIL: namespace task-key-RELAY max=51 next_alloc=52 is BELOW true max key RELAY-55 -- collides with an existing task key
    RESULT: FAIL -- reservation-namespace drift detected
    proof-check: FAIL -- proof command exited 1
    proof-check: verdict=FAIL class=wrapper,file-assertion exit=1 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0

This is a genuine FAIL, not VACUOUS or UNVERIFIABLE -- proof-check.sh actually ran the command
(class=wrapper,file-assertion, a real shape, judged on exit status) and the FAIL lines are the
script's own findings, not a parse error. Re-run the SAME command once every counter above is
reconciled: it must then print "RESULT: PASS -- all reservation counters at or above true max" and
exit 0, which is the task's actual definition of done. NOTE for whoever re-verifies: proof-check.sh
has an independent, PRE-EXISTING parsing limitation this proof had to route around -- its
head_token segment check re-word-splits an already-substituted variable's text without
re-recognizing embedded $() syntax, so a bare `VAR=$(cmd multi word args)` at the top level is
misparsed as an unrunnable segment (observed: `TMPD=$(mktemp -d)` alone was enough to trip it,
reported as segment '-d)'). The workaround used throughout (prefixing such assignments with
`export`, and stripping internal spaces from $((...)) arithmetic) is NOT a defect in THIS task's
logic -- it is a real, reusable proof-check.sh quirk worth knowing if anyone else's proof_cmd
mysteriously reports UNVERIFIABLE/placeholder or unrunnable on an otherwise-correct script; it did
not seem worth a separate filed task on its own (the norm document already tells agents to always
run proof-check.sh and quote its literal verdict, which is exactly what surfaced and let this be
worked around, rather than the guard being silently bypassed).

Read-only investigation: zero reservations POSTed (GET only throughout), zero files edited, zero
commits. Repo HEAD 665971c untouched.

PROOF_CMD MOVED TO ONE LINE 2026-08-21 -- the multi-line shell script that used to live in this task's proof_cmd field was moved here, into the description, to unblock scripts/gen-spec-mirror.sh. Root cause: the Spec Server's markdown exporter renders proof_cmd as a single inline-italic `_Proof: <cmd>_` span, and an inline span carries no per-line indentation -- only the FIRST physical line of a multi-line proof_cmd stays indented, every continuation line lands at column 0, and gen-spec-mirror.sh's structural guard (correctly) refuses to write on unexpected column-0 lines. The description field does not have this problem -- the exporter indents every description line uniformly -- which is why the original script is preserved verbatim below instead.

The new one-line proof_cmd (joins the same statements with ; and && -- a bash -c-free single physical line, no embedded newlines) is semantically equivalent: same reservation-namespace vs true-max-task-key comparison, same FAIL/PASS output shape, same exit codes. Verified via `bash scripts/proof-check.sh '<new proof_cmd>'` on 2026-08-21 against the live server: verdict FAIL (class=wrapper,file-assertion, exit=1), printing the SAME five FAIL lines as the original multi-line version (namespaces epic-task-key-ACK, task-key-ACK, task-key-AUTH, task-key-DOCS, task-key-RELAY all below their true max task key), followed by "RESULT: FAIL -- reservation-namespace drift detected". Not VACUOUS, not UNVERIFIABLE -- a genuine, unweakened FAIL reflecting the real unresolved drift documented above. Re-run the same one-line proof_cmd once every counter is reconciled; it must then print "RESULT: PASS -- all reservation counters at or above true max" and exit 0, exactly as before this move.

The workaround already documented above for proof-check.sh's head_token/$() re-word-splitting quirk (prefixing top-level `VAR=$(cmd multi-word-args)` assignments with `export`, and keeping `$((...))` arithmetic free of internal spaces) is preserved unchanged in the one-line form -- every VAR=$(...) assignment with a multi-word command substitution is still `export`-prefixed. Nothing else about this task's scope, findings or SCOPE section changed.

Original multi-line proof_cmd, preserved verbatim for reference:

    set -euo pipefail
    export TMPD=$(mktemp -d)
    trap 'rm -rf "$TMPD"' EXIT
    bash scripts/spec-cloud.sh -s /api/v1/projects/agent-bus/reservations > "$TMPD/resv.json"
    echo '[]' > "$TMPD/tasks.json"
    off=0
    while :; do
      bash scripts/spec-cloud.sh -s "/api/v1/projects/agent-bus/tasks?offset=$off" > "$TMPD/page.json"
      export n=$(jq 'length' "$TMPD/page.json")
      if [ "$n" -eq 0 ]; then break; fi
      jq -s '.[0] + .[1]' "$TMPD/tasks.json" "$TMPD/page.json" > "$TMPD/tasks2.json"
      mv "$TMPD/tasks2.json" "$TMPD/tasks.json"
      off=$((off+n))
      if [ "$off" -gt 5000 ]; then echo "SAFETY BREAK" >&2; break; fi
    done
    jq -r '.[].title' "$TMPD/tasks.json" | grep -oP '^[A-Z][A-Z0-9]*-\d+(?=:)' | sed -E 's/-([0-9]+)$/ \1/' | awk '{ if ($2+0 > max[$1]+0) max[$1]=$2 } END { for (e in max) print e, max[e] }' > "$TMPD/true_max.txt"
    jq -r '.[] | select(.namespace | test("^(epic-)?task-key-[A-Za-z0-9]+$")) | .namespace' "$TMPD/resv.json" | sort -u > "$TMPD/namespaces.txt"
    fail=0
    while read -r ns; do
      export epic=$(echo "$ns" | sed -E 's/^(epic-)?task-key-//')
      export truen=$(awk -v e="$epic" '$1==e {print $2}' "$TMPD/true_max.txt")
      if [ -z "$truen" ]; then continue; fi
      export resvn=$(jq -r --arg ns "$ns" '[.[] | select(.namespace==$ns) | .value] | max' "$TMPD/resv.json")
      nextalloc=$((resvn+1))
      if [ "$resvn" -lt "$truen" ]; then
        echo "FAIL: namespace $ns max=$resvn next_alloc=$nextalloc is BELOW true max key ${epic}-${truen} -- collides with an existing task key" >&2
        fail=1
      fi
    done < "$TMPD/namespaces.txt"
    if [ "$fail" -ne 0 ]; then echo "RESULT: FAIL -- reservation-namespace drift detected"; exit 1; fi
    echo "RESULT: PASS -- all reservation counters at or above true max"
    exit 0

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-15](../../ACK/ACK-15--a63b133d/task.md) — ACK-15: POST /v1/ack has no CLI subcommand -- until it does, no row can ever reach delive… (done)
- [ACK-18](../../ACK/ACK-18--ac5f5fb2/task.md) — ACK-18: no GLOBAL ceiling on parked ack-status waits -- 32 x enrolled principals (todo)
- [ACK-2](../../ACK/ACK-2--9564f953/task.md) — ACK-2: Durable local send acceptance and ACK/NACK lifecycle record (done)
- [AUTH-10](../../AUTH/AUTH-10--37993b49/task.md) — AUTH-10: An operator/admin principal -- the missing noun blocking AUTH-7, INVMINT and CON… (done)
- [AUTH-9](../../AUTH/AUTH-9--483ee09b/task.md) — AUTH-9: Opt-in session persistence (--persist-session) + agent-busctl session logout (done)
- [CONTEXT-RESERVE-CANON](../../CONTEXT/CONTEXT-RESERVE-CANON--3aea21a7/task.md) — CONTEXT-RESERVE-CANON: the reservation guidance stops disagreeing with itself across four… (todo)
- [DOCS-30](../../DOCS/DOCS-30--a311a067/task.md) — DOCS-30: clientcert help says the bus ignores the client certificate; the bus refuses 409… (todo)
- [DOCS-5](../../DOCS/DOCS-5--051a9829/task.md) — DOCS-5: \`/v1/discovery\` limitation 5 is false on the wire: cross-bus relay IS served (todo)
- [DOCS-6](../../DOCS/DOCS-6--76879ad1/task.md) — DOCS-6: README is unusable: quickstart 403s, "what works today" curls a TLS port in plain… (todo)
- [RELAY-45-FU-CLI](../../RELAY/RELAY-45-FU-CLI--b9d645be/task.md) — RELAY-45-FU-CLI: operator CLI surface for the inbound peer client-certificate binding (done)
- [RELAY-52](../../RELAY/RELAY-52--67c6248d/task.md) — RELAY-52: invariant 6's loud-discard line at hub.go:1104 has no test anywhere in the repo (done)
- [RELAY-53](../../RELAY/RELAY-53--d5bbdec9/task.md) — RELAY-53: RELAY-23 will merge cleanly and then fail to compile -- two wire-version resolv… (todo)
- [RELAY-54](../../RELAY/RELAY-54--911841af/task.md) — RELAY-54: an abandoned outbox job is invisible to every subcommand -- the drain a rollout… (done)
- [RELAY-55](../../RELAY/RELAY-55--0a571a02/task.md) — RELAY-55: a bus can be healthy and silently deaf to the entire federation -- /healthz is… (todo)
- [TOOLING-1](../../TOOLING/TOOLING-1--eeb4109b/task.md) — TOOLING-1: read-only linter for mechanically broken stored proof_cmd values (done)
- [TOOLING-2](../../TOOLING/TOOLING-2--87d9e8d1/task.md) — TOOLING-2: make docs/THREE-BUS-DOCKER.md's bash blocks a repeatable executable check (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [017304e6-a088-40c9-b6c2-5cac4bc0fb66](../proof-check.sh-head_token-word-splits-the-LITERAL-text-o--017304e6/task.md) — proof-check.sh head_token word-splits the LITERAL text of a VAR=$(...) proof_cmd, mis-ref… (todo)
- [0f4a0736-979b-4a20-b75f-0b2950f2181c](../../PROCESS/gen-spec-mirror.sh-REFUSES-TO-WRITE-unexpected-non-blank--0f4a0736/task.md) — gen-spec-mirror.sh REFUSES TO WRITE ("unexpected non-blank column-0 lines") -- markdown e… (todo)
- [3d1b47d9-1395-4f61-a848-e1c06ced2ff8](../../CONTEXT/PITFALLS.md-has-no-row-in-doc-budgets.tsv-so-the-prose-r--3d1b47d9/task.md) — PITFALLS.md has no row in doc-budgets.tsv, so the prose relocated out of CLAUDE.md is unm… (todo)
- [f4bd3c9f-3af8-4438-bcb0-18203b857255](../../PROCESS/Deep-dive-audit-and-refactor-the-repo-s-tracked-.md-file--f4bd3c9f/task.md) — Deep-dive: audit and refactor the repo's tracked .md files, CLAUDE.md primary, fix AGENTS… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
