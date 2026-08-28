# Spec Server: creating a supersedes edge silently flips the target task to status=superseded

| Field | Value |
| --- | --- |
| Public id | `afc2fe3f-848f-4804-b031-92e4ffbb015e` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T09:04:47.495376+00:00 |
| Updated | 2026-08-22T09:38:02.839540+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'P=/api/v1/projects/agent-bus/tasks && D=/tmp/spec-supersedes-probe.$$ && mkdir -p $D && printf %s "{\"title\":\"scratch supersedes probe A (auto-deleted by proof)\",\"epic_key\":\"PROCESS\",\"priority\":\"P3\",\"component\":\"process\"}" > $D/a.json && printf %s "{\"title\":\"scratch supersedes probe B (auto-deleted by proof)\",\"epic_key\":\"PROCESS\",\"priority\":\"P3\",\"component\":\"process\"}" > $D/b.json && A=$(bash scripts/spec-cloud.sh -s -X POST $P -H "Content-Type: application/json" -d @$D/a.json | python3 -c "import sys,json;print(json.load(sys.stdin)[\"public_id\"])") && B=$(bash scripts/spec-cloud.sh -s -X POST $P -H "Content-Type: application/json" -d @$D/b.json | python3 -c "import sys,json;print(json.load(sys.stdin)[\"public_id\"])") && bash scripts/spec-cloud.sh -s -X POST $P/$B/status -H "Content-Type: application/json" -d "{\"status\":\"blocked\"}" > /dev/null && BEFORE=$(bash scripts/spec-cloud.sh -s $P/$B | python3 -c "import sys,json;print(json.load(sys.stdin)[\"status\"])") && printf %s "{\"target\":\"$B\",\"kind\":\"supersedes\"}" > $D/rel.json && bash scripts/spec-cloud.sh -s -X POST $P/$A/relations -H "Content-Type: application/json" -d @$D/rel.json > /dev/null && AFTER=$(bash scripts/spec-cloud.sh -s $P/$B | python3 -c "import sys,json;print(json.load(sys.stdin)[\"status\"])"); bash scripts/spec-cloud.sh -s -X DELETE $P/$A > /dev/null 2>&1; bash scripts/spec-cloud.sh -s -X DELETE $P/$B > /dev/null 2>&1; rm -rf $D; echo "supersedes probe: target status before=$BEFORE after=$AFTER (FIXED behaviour = unchanged)"; test -n "$AFTER" && test "$AFTER" = "$BEFORE"'
```

## Description

**Observed 2026-08-22 while reconciling the PROCESS tiered-chain tasks.**

`POST /api/v1/projects/<slug>/tasks/<id>/relations` with `{"kind":"supersedes"}` mutates the TARGET
task's `status` to `superseded` as a SIDE EFFECT. The response gives no indication that a status
transition occurred. `~/source/spec-keeper/AGENTS_API.md` mentions it in one half-sentence
("`supersedes` also sets the target's status to `superseded` and links it back"), which is easy to
read past and does not state the ordering consequence.

The consequence: any agent that writes relations and then writes status -- or writes status and then
writes relations -- will silently overwrite, or be overwritten by, that implicit transition
DEPENDING ON ORDERING. Nothing in the response or in the task's version history distinguishes an
intended status write from the implicit one.

Required outcome, either:
- document the behaviour and the required ordering (**relations BEFORE status**) prominently, or
- make the side effect explicit / opt-in (e.g. a `set_status` flag on the relation POST).

Either way, record the workaround in `.claude/agents/spec-keeper.md` so the next spec-keeper run
does not rediscover it. Note the asymmetry already documented for the reverse operation: DELETING a
`supersedes` edge clears `superseded_by` but DELIBERATELY LEAVES the status at `superseded`, so the
side effect is not self-reversing.

## PROOF -- READ THE DIRECTION BEFORE RUNNING IT (concrete and runnable as of 2026-08-22)

Until 2026-08-22 the stored `proof_cmd` here was an angle-bracket PLACEHOLDER, not a command. It is
now a real reproduction, verified by spec-keeper.

**THE PROOF ASSERTS THE FIXED BEHAVIOUR, SO IT IS RED TODAY. It is not broken; the bug is present.**
This is the one thing to get right about this task, because the naive framing runs the other way:

- A REPRODUCTION of the bug ("assert the target flipped to `superseded`") would PASS today and go
  RED the day someone fixes the server. That direction is a trap -- a green reproduction is
  indistinguishable at a glance from a green fix, and whoever completes the task reads a PASS.
- The stored proof therefore asserts the FIXED behaviour: **the target task's status is UNCHANGED by
  creating a `supersedes` edge.** It reads the status BEFORE the edge and compares it AFTER, rather
  than hard-coding a value, so it stays correct whichever status the target happens to hold.

Measured 2026-08-22 by spec-keeper via `scripts/proof-check.sh`: **verdict=FAIL exit=1**, printing
`supersedes probe: target status before=blocked after=superseded (FIXED behaviour = unchanged)`.
That is the intended RED. When the fix lands -- documented ordering is NOT enough to turn this
green; only making the transition opt-in is -- the same command goes green unmodified.

**NEW MEASUREMENT, not previously recorded: the flip is not confined to `todo` targets.** The probe
sets the target to `blocked` first, and the edge still forced it to `superseded`. The side effect
therefore overwrites a DELIBERATE, non-default status, which is strictly worse than the originally
reported behaviour and is the concrete reason ordering-only documentation does not close this.

The proof also asserts `AFTER` is NON-EMPTY. Without that, a proof whose setup failed early would
compare two empty strings and PASS -- a false green produced by the proof not running at all.

Throwaway tasks only: the command creates its own pair, sets the target to `blocked`, and DELETES
BOTH on the way out (cleanup is `;`-joined after the assertion chain, so it runs on failure too).
Never run this against real PROCESS tasks.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [c9e89d5a-6f6f-475e-8c8e-24f663a060bc](../Explicit-manifest-of-security-bearing-test-files-as-a-th--c9e89d5a/task.md) — Explicit manifest of security-bearing test files, as a third guard check alongside the tw… (cancelled)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
