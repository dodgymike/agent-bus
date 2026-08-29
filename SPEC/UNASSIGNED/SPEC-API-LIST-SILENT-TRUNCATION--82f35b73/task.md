# Task-list API silently truncates at 200 with no total, no next and no working pagination -- every list-based duplicate check is unreliable

| Field | Value |
| --- | --- |
| Public id | `82f35b73-db89-474d-b814-59df5c24f0df` |
| Key | SPEC-API-LIST-SILENT-TRUNCATION |
| Epic | [UNASSIGNED](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | tooling |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T15:34:36.231306+00:00 |
| Updated | 2026-08-22T09:23:40.808811+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/spec-cloud.sh -s -o /tmp/sk-plain.json "/api/v1/projects/agent-bus/tasks" && bash scripts/spec-cloud.sh -s -o /tmp/sk-tail.json "/api/v1/projects/agent-bus/tasks?offset=400" && python3 -c "import json,sys; p=json.load(open('/tmp/sk-plain.json')); t=json.load(open('/tmp/sk-tail.json')); total=400+len(t); env=isinstance(p,dict) and any(k in p for k in ('total','count','has_more','next','truncated')); full=isinstance(p,list) and len(p)>=total; ok=env or full; msg=('PASS: default list carries a total/next/truncated marker' if env else ('PASS: default list returned all %d tasks, no silent truncation'%total if full else 'FAIL: default list is a bare array of %d items with no total/next/truncated marker, but the project has at least %d tasks -- silent truncation'%(len(p),total))); print(msg); sys.exit(0 if ok else 1)"
```

## Description

TOOLING/PROCESS defect in the SPEC SERVER API itself -- NOT in agent-bus code. No agent-bus source file is implicated.

The default task-list query silently returns only the first page and gives the caller no way to know it was truncated. Every duplicate-check done by listing tasks is therefore unsound: it can report "no such task exists" while the task does exist. This is a FALSE-NEGATIVE GENERATOR in the exact procedure this project uses to avoid duplicate work.

== MEASURED FACTS (spec-keeper, 2026-08-14, via `bash scripts/spec-cloud.sh` against the cloud server; project holds ~551 tasks) ==

1. CORE DEFECT -- silent truncation, no total.
   `GET /api/v1/projects/agent-bus/tasks` returns EXACTLY 200 items as a BARE JSON ARRAY.
   No envelope, no `total`, no `count`, no `next`, no `has_more`, no `truncated`.
   No pagination response headers either (checked: only `x-content-type-options` is present; no `Link`, no `Content-Range`, no `X-Total-Count`).
   `GET /api/v1/projects/agent-bus` (project detail) does NOT carry a task count either -- there is NO endpoint that tells you the true total.
   A caller CANNOT distinguish "these are all the tasks" from "this is the first 200 of 551". That is the defect: silent truncation of a query whose whole purpose is completeness.

2. `?q=` IS SUBJECT TO THE SAME 200 CAP -- it is NOT a fix.
   `?q=RELAY` -> 182 (complete ONLY because the filter narrows below the cap).
   `?q=a` -> 200. `?q=e` -> 200. `?q=truncat` -> 40. `?q=TRUNCATION` -> 20.
   So `?q=` helps ONLY when the filter happens to match fewer than 200. A broad query truncates identically and just as silently. "Use ?q=" MUST NOT be documented as a fix.
   A `?q=` that returns EXACTLY 200 means TRUNCATED, not "200 matches".

3. Results are oldest-first, so a RECENTLY FILED task is invisible BY CONSTRUCTION to a default list check -- which is precisely the task a duplicate check is looking for. Confirmed: plain list first=CORE-1 (oldest), and the newest tasks appear only on the last offset page.

== TWO CORRECTIONS TO THE ORIGINAL REPORT (re-measured; file what is true, not what was assumed) ==

The brief that prompted this task stated `?offset=200` is "silently ignored" and that `?limit=1000` 500s as a flat count limit. Neither survived re-measurement. Recording the truth, because the workaround and the fix both depend on it:

4. `?offset=` IS HONOURED and DOES page correctly. Measured:
   plain -> 200 items, first=CORE-1, last=DUR-12-FU-KEYMODE
   `?offset=0`   -> 200 (identical to plain)
   `?offset=200` -> 200, first=DUR-12-VERIFY, last=RELAY-11-FU-INGEST-LOOPGUARD
   `?offset=400` -> 150
   `?offset=540` -> 10
   The three pages are DISJOINT (pairwise overlap = 0) and their union = 550 distinct tasks, i.e. the true total.
   `?q=a&offset=200` -> 200, so offset pages a search too.
   ==> A WORKING ESCAPE HATCH EXISTS TODAY: page with `?offset=N` (step 200) until a SHORT page (<200) is returned; that short page is the terminator. This is the interim procedure agents should use for any exhaustive check.

5. `?limit=` IS HONOURED, and the 500 is a RESPONSE-SIZE blowout, NOT a count cap. Measured:
   limit=5 -> 5; limit=50 -> 50; limit=200 -> 200; limit=201 -> 201; limit=250 -> 250; limit=300 -> 300; limit=350 -> 350; limit=400 -> 400; limit=450 -> 450 (OK)
   limit=460, 470, 475, 490, 499, 500, 501, 549, 550, 1000 -> HTTP 500 {"message":"Internal Server Error"}
   Response bytes: limit=200 -> 3,390,526 B; limit=450 -> 6,101,390 B; limit=460 -> error.
   6,101,390 B sits just under the AWS Lambda/API Gateway 6 MiB (6,291,456 B) payload ceiling. So the failure threshold is DATA-DEPENDENT, not a fixed number: the list returns every task's FULL `description`, so as descriptions grow the largest working `limit` SHRINKS. A `limit` that works today will start 500ing later with no code change. This is the more dangerous half of the bug.
   Invalid values are handled correctly: `?limit=-1`, `?limit=0`, `?limit=abc` -> HTTP 422.
   `?per_page=1000` and `?page=2` ARE silently ignored (both still return the default 200, first=CORE-1) -- only `limit`/`offset` are real.
   No field projection: `?fields=key,title` is ignored; the response still carries description/notes/commits (6,101,390 B for 450 items, byte-identical to the unprojected call).

== WHY IT MATTERS -- the class of bug this belongs to ==

This is the same failure class as `proof-check.sh` certifying a vacuous run, and as `gofmt -l` reporting by PRINTING rather than by EXIT STATUS: a check that reports SUCCESS while having verified NOTHING. The caller receives a well-formed 200 OK containing a plausible answer, and there is no signal anywhere in the response that the answer is partial.

CLAUDE.md mandates "never scan-and-pick" for CLAIMING a task (claim-next is atomic and collision-proof). But duplicate-checking before FILING is still list-based, and that path is unsound. The hardened rule covers the wrong half of the workflow.

== CONCRETE EVIDENCE THIS ALREADY BIT US ==

During RELAY-24-BLOCKER-HUBINGEST the reviewer queried the backlog for a follow-up task, concluded from a list query that none existed, and raised it as a MUST-FIX finding against shipped code comments that referenced it. A task with that key had in fact been created earlier the same session. The comments were correct; the check was not. Reviewer time was spent, and shipped-correct code was challenged, purely on a truncated list.

The orchestrator additionally attributes two duplicate pairs filed on 2026-08-14 -- RELAY-44/45 and RELAY-43/46 -- to concurrent filing, but they may in fact be this defect: a pre-filing duplicate check that could not see a task filed minutes earlier, because new tasks sort LAST and the list stops at 200.

== ASKS ==

1. API FIX. The list response should carry a `total`/`count` AND an explicit truncation marker (`has_more` / `next` cursor), or the bare array should be replaced by an envelope. `?offset=` already works, so the missing piece is METADATA, not paging: without a total the caller still cannot tell a full last page from a truncated one except by making an extra request. Additionally:
   - The 500 on large `limit` is its own bug worth naming: it is an unhandled response-size overflow surfacing as Internal Server Error rather than a 413/422 or an automatic cap. It should either cap `limit` server-side (and say so via the truncation marker) or return a structured error naming the payload ceiling.
   - Strongly consider a SUMMARY projection (`?fields=` or a `/tasks?view=summary` returning key/title/status/epic only). The full-description payload is what puts a 550-task project within 3% of the 6 MiB ceiling at just 450 rows; a key+title projection would fit the whole backlog in one response and would make exhaustive duplicate checks cheap and reliable.

2. DOCUMENT THE INTERIM RULE in CLAUDE.md's "Spec Server" section (and mirror to the spec-keeper agent brief):
   - Default list results are CAPPED AT 200 and truncation is SILENT.
   - A `?q=` result of EXACTLY 200 means TRUNCATED, not "200 matches".
   - An exhaustive check MUST page with `?offset=N` until a page shorter than the page size is returned -- OR use a `?q=` narrow enough that its result count is verifiably UNDER 200.
   - Do NOT document "use ?q=" as a fix; it only masks the cap.
   - Do NOT raise `limit` above ~400 on this project; it 500s on payload size, and the safe ceiling falls as descriptions grow.

3. SERVER-SIDE KEY UNIQUENESS. Consider a reservation-style or server-side UNIQUE constraint on task `key`, so a duplicate key is REFUSED by the server (409) rather than prevented by a client-side check that, as shown above, cannot be made reliable. This is the same argument that made numbered-resource RESERVATIONS mandatory instead of eyeballed: correctness that depends on a client reading a complete list is correctness that fails the moment the list is capped.

== PROOF ==

proof_cmd asserts the default list is either complete or self-describing. It is RED TODAY -- verified before filing:
  FAIL: default list is a bare array of 200 items with no total/next/truncated marker, but the project has at least 551 tasks -- silent truncation
It passes once the response carries `total`/`count`/`has_more`/`next`/`truncated`, or once the default list stops truncating. It cannot pass vacuously: it derives the true total from a live `?offset=400` page rather than a hardcoded number.

== ADDENDUM (measured immediately after filing -- these make the defect WORSE than described above) ==

6. `?q=` DOES NOT SEARCH THE `key` FIELD AT ALL. Measured against THIS task, whose key is literally SPEC-API-LIST-SILENT-TRUNCATION:
   `?q=SPEC-API-LIST-SILENT-TRUNCATION` -> 0 results. `?q=SPEC-API` -> 0 results.
   Yet `GET /api/v1/projects/agent-bus/tasks/SPEC-API-LIST-SILENT-TRUNCATION` -> HTTP 200. The task plainly exists.
   It is NOT a hyphen/tokeniser problem: `?q=list-based` -> 1 hit and `?q=Task-list` -> 2 hits, both matching this task's TITLE. `q` matches title/description TEXT only; the key field is simply not indexed.
   CONSEQUENCE, and it is severe: "confirm the key is free by SEARCHING" is UNSOUND. `?q=<key>` returns 0 for EVERY key -- taken or free -- so it looks like confirmation and confirms nothing. It is the same report-success-having-verified-nothing shape as the truncation itself, and it was the procedure recommended in-session before it was measured.

7. THE SOUND KEY CHECK IS A DIRECT GET, NOT A SEARCH: `GET /api/v1/projects/agent-bus/tasks/<KEY>` -> 200 = taken, 404 = free. Verified: SPEC-API-LIST-SILENT-TRUNCATION -> 200, NO-SUCH-KEY-XYZZY-12345 -> 404. This is O(1), immune to the 200 cap, and is what the docs should mandate.

8. BUT THE DIRECT GET HAS A HOLE, AND IT IS EXACTLY THE HOLE THAT PRODUCED THE RELAY-44/45 PAIR: many tasks carry `key: null` and wear their identity ONLY in the title text. Measured: RELAY-44 (public_id cec27a90-...) and RELAY-46 (public_id eb5c3312-...) both have `key = null`; their display_id is the bare UUID and the string "RELAY-44" exists only inside the title. So `GET /tasks/RELAY-44` -> 404 while RELAY-44 unmistakably EXISTS. A key-existence check reports "free" for a name that is already taken, because the name was never stored anywhere the server can index. Any server-side uniqueness constraint (ask 3) is therefore only as good as the discipline of actually SETTING `key` at create time -- the constraint and that discipline must ship together, or the constraint silently covers a subset.

9. THE CAP CAUGHT IN THE ACT. Seconds after this task was created, `?q=SPEC` returned EXACTLY 200 results and this task was NOT among them, while the narrower `?q=silently` returned 83 and DID include it. A just-created task, invisible to a broad search that returned a full-looking 200 OK. That is the false negative in this task's title, reproduced live against this task itself.

== RELATED (not a duplicate) ==

Task 73b29060-f595-4f4d-90a9-3f13d231b909 ("Spec Server: warn on likely-duplicate task titles at create/claim-next time", P3) attacks the SYMPTOM from the other end: fuzzy title-similarity warnings at create time, motivated by the same RELAY-44/45 and RELAY-43/46 incidents. It assumes those pairs came from a filing RACE. Findings 6-9 above show a second, independent cause that a similarity warner does not address: the pre-filing check itself cannot see the thing it is checking for. The two tasks are complements -- that one detects near-duplicate TITLES, this one makes the underlying LOOKUP sound. Finding 8 in particular explains why RELAY-44 was invisible to any key-based check: it has no key.
== CORRECTION 2026-08-22 (spec-keeper) -- point 1 of the "MEASURED FACTS" section above is now FALSE ==

Re-measured against the live cloud API, GET /api/v1/projects/agent-bus/tasks?limit=5:

  x-has-more: true
  x-total-count: 831
  link: </api/v1/projects/agent-bus/tasks?offset=5&limit=5>; rel="next"

The API now DOES carry a total (X-Total-Count), an explicit truncation marker (X-Has-More) and a
Link: rel="next" header -- exactly the fix ASK 1 in this task requested. The 2026-08-14 statement
"No pagination response headers either (checked: only x-content-type-options is present; no Link,
no Content-Range, no X-Total-Count)" no longer holds; do not rely on it, and do not re-file the
"no total/no next" defect as new -- it is fixed.

NOT closing this task: it is left open because the wider discoverability asks may still stand and
have not been re-measured here -- specifically ask 2 (documenting the interim ?offset= paging rule
in CLAUDE.md's Spec Server section), the ?q= field-coverage gap (finding 6: key field not indexed),
and the large-?limit? 500 (finding 5). Whoever picks this up next should re-measure ALL of the
original numbered findings against the current API before deciding what remains, not assume only
finding 1 changed.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [73b29060-f595-4f4d-90a9-3f13d231b909](../../CONTEXT/Spec-Server-warn-on-likely-duplicate-task-titles-at-crea--73b29060/task.md)
- **relates to** [CONTEXT-KEY-IDENTITY](../../CONTEXT/CONTEXT-KEY-IDENTITY--73dec684/task.md)
- **supersedes** [9e6544a1-d606-4e65-8c43-0764ac3f0aa4](../../TOOLING/spec-cloud.sh-task-list-workflow-GET-...-tasks-silently--9e6544a1/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [73b29060-f595-4f4d-90a9-3f13d231b909](../../CONTEXT/Spec-Server-warn-on-likely-duplicate-task-titles-at-crea--73b29060/task.md) — Spec Server: warn on likely-duplicate task titles at create/claim-next time (todo)
- [CORE-1](../../CORE/CORE-1--eea035e4/task.md) — CORE-1: Repo skeleton: go.mod, internal/ package layout, .gitignore (done)
- [DUR-12-FU-KEYMODE](../../DUR/DUR-12-FU-KEYMODE--f8bae169/task.md) — DUR-12-FU-KEYMODE: loadMACKey never checks the key file's permission mode (todo)
- [DUR-12-VERIFY](../../DUR/DUR-12-VERIFY--f602c92e/task.md) — DUR-12-VERIFY: verify the WAL MAC upgrade against a real running bus (paired not-yet-live… (todo)
- [RELAY-11-FU-INGEST-LOOPGUARD](../../RELAY/RELAY-11-FU-INGEST-LOOPGUARD--a41c273c/task.md) — Relay ingest MUST route through relay.CheckIncomingPath before hub.publish, or a 64-hop l… (todo)
- [RELAY-24-BLOCKER-HUBINGEST](../../RELAY/RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) — RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sen… (done)
- [RELAY-44](../../RELAY/RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-46](../../RELAY/RELAY-46--eb5c3312/task.md) — RELAY-46: NextHopTLSCertFingerprint should be a bounded list, not a scalar, for peer-cert… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [9e6544a1-d606-4e65-8c43-0764ac3f0aa4](../../TOOLING/spec-cloud.sh-task-list-workflow-GET-...-tasks-silently--9e6544a1/task.md) — spec-cloud.sh / task-list workflow: GET .../tasks silently truncates to the oldest 200 of… (superseded)
- [CONTEXT-KEY-IDENTITY](../../CONTEXT/CONTEXT-KEY-IDENTITY--73dec684/task.md) — CONTEXT-KEY-IDENTITY: Standardize task identity (public_id vs key) before SPEC/&lt;epic&gt;/&lt;ta… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
