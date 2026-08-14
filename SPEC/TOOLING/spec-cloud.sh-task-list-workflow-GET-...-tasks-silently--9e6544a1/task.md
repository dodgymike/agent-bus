# spec-cloud.sh / task-list workflow: GET .../tasks silently truncates to the oldest 200 of 550 tasks unless limit/offset or q= is used explicitly

| Field | Value |
| --- | --- |
| Public id | `9e6544a1-d606-4e65-8c43-0764ac3f0aa4` |
| Key | _(null in the export)_ |
| Epic | [TOOLING](../epic.md) |
| Status | superseded |
| Priority | P1 |
| Component | tooling |
| Section | backlog |
| Tags | security-adjacent, false-negative, spec-server, pagination |
| Created | 2026-08-14T15:28:59.906791+00:00 |
| Updated | 2026-08-14T15:41:07.228082+00:00 |
| Completed | — |

## Proof command

```sh
grep -qi 'limit.*200\|default limit\|truncat' ~/source/spec-keeper/AGENTS_API.md
```

## Status note

SUPERSEDED 2026-08-14 by SPEC-API-LIST-SILENT-TRUNCATION (82f35b73-db89-474d-b814-59df5c24f0df), filed independently minutes later with corrected, far more thorough facts -- ironic given this task's own subject matter is unreliable duplicate detection. My original facts here were WRONG in two places, both corrected by the survivor: (1) I said only /export?format=json is unbounded -- 82f35b73 measured that ?offset= IS honoured and pages correctly (200+200+150=550 disjoint union); (2) I said the ?limit=~460+ 500 was unresolved/possibly load-related -- 82f35b73 root-caused it precisely as a response-SIZE overflow against the 6,291,456-byte Lambda/API-Gateway ceiling (measured limit=450 -> 6,101,390 B), not a count cap or transient contention. Also: 82f35b73 additionally discovered ?q= does not index the key field at all (0 results for any key, taken or free), which is the more serious finding and explains today's duplicate pairs mechanistically rather than as a race. Do not work this task; work the survivor.

## Description

Filed 2026-08-14, discovered when a reviewer reported "queried all 200 tasks, none exists" for a task that had in fact been filed -- a false negative from a completeness query that silently returns a subset.

CONFIRMED LIVE AGAINST THE SERVER, 2026-08-14:
- GET /api/v1/projects/agent-bus/tasks with NO params returns exactly 200 rows (of 550 total). The openapi.json spec DOES document a `limit` param (default 200, min 1, max 1000) and an `offset` param (default 0) -- so this is a documented default, not an undocumented bug -- but nothing in AGENTS_API.md's task-listing examples mentions it, so every `GET .../tasks` call this session that omitted `limit` was silently capped at 200 and nobody reading the recipe book would know to ask.
- `?q=<term>` is ALSO bounded at 200 -- confirmed empirically (q=e, a near-universal single-letter match, still returned exactly 200 rows against 550 tasks). DO NOT assume the free-text-search path is unbounded just because it takes a different code path from the bare list -- it shares the same default limit.
- WORKAROUNDS, both verified to return the true total (550) just now: (1) `GET .../export?format=json` returns EVERY task, unpaginated, in one call -- this is what every accurate count in this session's own duplicate/closure work actually used, and is the safest default for a completeness query. (2) `GET .../tasks?limit=200&offset=0`, then `offset=200`, then `offset=400`, paginated, sums exactly to 550.
- ONE MORE FINDING, NOT FULLY ROOT-CAUSED, WORTH A LINE SO NOBODY RE-DISCOVERS IT FROM SCRATCH: `limit` values above roughly 450-460 intermittently returned HTTP 500 rather than a larger page (tested 400 PASS, 450 PASS, then 460/470/475/480/500/600/1000 all 500, inconsistently near that boundary across repeated calls). Could be a genuine server-side bug at high limit, or resource contention from the heavy concurrent load on the server today (many agents active in parallel) -- not conclusively distinguished. Recommend whoever picks this up re-test in a quieter window before deciding whether it is a second, distinct defect.

SAME DEFECT CLASS AS: proof-check.sh certifying a vacuous run, and gofmt -l reporting by printing rather than exit status -- a completeness query that silently returns a subset rather than erroring or signalling truncation. GET .../tasks returns a bare JSON array with no total/next-page/truncated field, so a caller has no way to tell '200 of 550' from '200 of 200' without already knowing the total.

CONSEQUENCE FOR THIS PROJECT SPECIFICALLY: every duplicate-check this session that listed tasks via a bare/q=-only call (rather than /export or paginated /tasks) was checking against an ARBITRARY 200-task subset (the oldest 200, since the endpoint appears unordered-by-recency / default DB order), not the full backlog -- a recently-filed task is invisible BY CONSTRUCTION under that call shape. See the paired follow-up: re-running today's duplicate checks with the correct workaround.

SCOPE FOR THE FIX: this may be a documentation-only fix (state the default limit and the workaround prominently in AGENTS_API.md's task-listing section, the way this task's own filing does) rather than a server-code change, since the underlying pagination mechanism already exists and is documented in openapi.json -- it is just not where an agent following the recipe book would find it before being bitten. Decide during triage whether AGENTS_API.md's omission is the whole fix or whether the endpoint should also return a total/truncated indicator so a caller can detect this class of bug without already knowing to ask.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **superseded by** [SPEC-API-LIST-SILENT-TRUNCATION](../../UNASSIGNED/SPEC-API-LIST-SILENT-TRUNCATION--82f35b73/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [SPEC-API-LIST-SILENT-TRUNCATION](../../UNASSIGNED/SPEC-API-LIST-SILENT-TRUNCATION--82f35b73/task.md) — Task-list API silently truncates at 200 with no total, no next and no working pagination… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
