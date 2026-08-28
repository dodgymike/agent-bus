# Correct 0f8c5332: the relation-delete endpoint EXISTS

| Field | Value |
| --- | --- |
| Public id | `ed3537a8-9c5f-489e-8aa8-8d3f61514d5f` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T09:04:47.772062+00:00 |
| Updated | 2026-08-22T09:37:38.975469+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/proof-check.sh 'P=/api/v1/projects/agent-bus/tasks && D=/tmp/spec-reldelete-probe.$$ && mkdir -p $D && printf %s "{\"title\":\"scratch relation-delete probe A (auto-deleted by proof)\",\"epic_key\":\"PROCESS\",\"priority\":\"P3\",\"component\":\"process\"}" > $D/a.json && printf %s "{\"title\":\"scratch relation-delete probe B (auto-deleted by proof)\",\"epic_key\":\"PROCESS\",\"priority\":\"P3\",\"component\":\"process\"}" > $D/b.json && A=$(bash scripts/spec-cloud.sh -s -X POST $P -H "Content-Type: application/json" -d @$D/a.json | python3 -c "import sys,json;print(json.load(sys.stdin)[\"public_id\"])") && B=$(bash scripts/spec-cloud.sh -s -X POST $P -H "Content-Type: application/json" -d @$D/b.json | python3 -c "import sys,json;print(json.load(sys.stdin)[\"public_id\"])") && printf %s "{\"target\":\"$B\",\"kind\":\"relates\"}" > $D/rel.json && bash scripts/spec-cloud.sh -s -X POST $P/$A/relations -H "Content-Type: application/json" -d @$D/rel.json > /dev/null && BEFORE=$(bash scripts/spec-cloud.sh -s $P/$A/relations | python3 -c "import sys,json;print(sum(1 for r in json.load(sys.stdin) if r[\"kind\"]==\"relates\" and r[\"task\"]==\"$B\"))") && DELBODY=$(bash scripts/spec-cloud.sh -s -X DELETE "$P/$A/relations?target=$B&kind=relates") && AFTER=$(bash scripts/spec-cloud.sh -s $P/$A/relations | python3 -c "import sys,json;print(sum(1 for r in json.load(sys.stdin) if r[\"kind\"]==\"relates\" and r[\"task\"]==\"$B\"))"); bash scripts/spec-cloud.sh -s -X DELETE $P/$A > /dev/null 2>&1; bash scripts/spec-cloud.sh -s -X DELETE $P/$B > /dev/null 2>&1; rm -rf $D; echo "relation-delete probe: matching edges before=$BEFORE after=$AFTER delete_body_len=${#DELBODY} (empty body = 204)"; test "$BEFORE" = 1 && test "$AFTER" = 0 && test -z "$DELBODY"'
```

## Description

Open task `0f8c5332-1236-4e22-a249-72119401003f` asserts that the Spec Server has **no**
relation-delete endpoint, and that a wrong `blocks` / `relates` / `supersedes` / `follow_up` edge
therefore cannot be removed.

**The premise is false. Measured 2026-08-22:**
`DELETE /api/v1/projects/agent-bus/tasks/<id>/relations?target=<id>&kind=<kind>` returned
**HTTP 204** and the edge was gone. `~/source/spec-keeper/AGENTS_API.md` documents it under
"Relations": `target` and `kind` are REQUIRED QUERY PARAMETERS (not a JSON body), it needs the
`write` permission, it returns 204 whether or not a matching edge existed (so a retry is safe), 404
if either task id fails to resolve, and 422 on an invalid/missing `target`/`kind`. The edge is keyed
`(source, target, kind)`, so deleting `kind=blocks` leaves a parallel `relates` intact.

Agents are working around a limitation that does not exist.

Scope:
1. Verify the DELETE once more against throwaway tasks (do not experiment on real PROCESS tasks).
2. Then either CLOSE `0f8c5332` as not-a-defect, with the evidence recorded in a `kind=response`
   note on that task, or NARROW it to whatever gap actually remains -- the one real residual is that
   deleting a `supersedes` edge deliberately leaves the target's status at `superseded`, which must
   be moved back explicitly with `POST /tasks/<id>/status {"status":"todo"}`.
3. Record the working invocation (exact query-parameter form) in `.claude/agents/spec-keeper.md`.

Proof (CONCRETE AND RUNNABLE -- the placeholder that stood here until 2026-08-22 was not a
command, and is recorded below as the defect it was).

**THIS PROOF PASSES TODAY, AND THAT IS THE POINT. It is NOT vacuous.** Read the direction before
judging it: this task exists to correct a FALSE PREMISE in `0f8c5332`, which claims the
relation-delete endpoint does not exist. The proof demonstrates that it DOES. A proof of an
existing capability is green on the day it is written; that is what distinguishes "correct a wrong
record" from "build a missing thing". Verified 2026-08-22 by spec-keeper with
`scripts/proof-check.sh`: **verdict=PASS class=wrapper,file-assertion exit=0**, printing
`relation-delete probe: matching edges before=1 after=0 delete_body_len=0 (empty body = 204)`.

The stored command creates two throwaway tasks, POSTs a `relates` edge, counts it, DELETEs it by
query parameter, counts again, and DELETES BOTH THROWAWAY TASKS on the way out (the cleanup runs
after the assertion chain via `;`, so it also runs when the assertion fails). It asserts three
things, none of them tautological: the edge count is 1 before, 0 after, and the DELETE response
body is EMPTY -- which is what a 204 looks like through `scripts/spec-cloud.sh`, since the wrapper
emits the body and exits non-zero on any non-2xx. Do NOT add `-w` to read the status code: a
caller-supplied `-w` breaks the wrapper's own status detection (task
`c65a5051-678c-487c-bdae-37183e01f049`).

If this proof ever goes RED, the endpoint has regressed or been removed -- which would retroactively
make `0f8c5332` correct. Do not "fix" it by deleting the assertion.

RELATES to 0f8c5332-1236-4e22-a249-72119401003f.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0f8c5332-1236-4e22-a249-72119401003f](../Spec-Server-API-gap-no-relation-delete-endpoint-wrong-bl--0f8c5332/task.md) — Spec Server API gap: no relation-delete endpoint -- wrong blocks/relates/supersedes/follo… (todo)
- [c65a5051-678c-487c-bdae-37183e01f049](../scripts-spec-cloud.sh-a-caller-supplied-w-breaks-status--c65a5051/task.md) — scripts/spec-cloud.sh: a caller-supplied -w breaks status detection and makes a 200 exit 5 (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
