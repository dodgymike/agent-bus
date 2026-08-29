# Invariant 3's unauthenticated-route enumeration is stale in three docs -- six entries in code, five in prose (RouteDiscovery missing)

| Field | Value |
| --- | --- |
| Public id | `9a02d65a-e96b-4fbe-93cf-846d8b5c2034` |
| Key | _(null in the export)_ |
| Epic | [DOCS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | docs |
| Section | backlog |
| Tags | invariant-3, stale-doc, unauthenticated-routes, discovery |
| Created | 2026-08-21T11:08:35.958857+00:00 |
| Updated | 2026-08-21T11:08:35.958857+00:00 |
| Completed | — |

## Proof command

```sh
bash -c 'fail=0; grep -qF "plus \`/healthz\` and \`/v1/info\`." INVARIANTS.md && fail=1; grep -qF "except enrolment, session begin/complete, \`/healthz\` and \`/v1/info\`." CLAUDE.md && fail=1; grep -qF "Every route except \`GET /healthz\`, \`GET /v1/info\`, \`POST /v1/enroll\`, \`POST /v1/session/begin\` and" AGENT_PROTOCOL.md && fail=1; if [ "$fail" -eq 0 ]; then echo ALL_FIXED; exit 0; else echo STALE_ENUMERATION_FOUND; exit 1; fi'
```

## Description

Operator-authorised filing. Invariant 3's enumeration of unauthenticated routes is factually stale, and the stale five-route list is repeated verbatim in THREE authoritative documents, while a FOURTH file (DECISIONS.md) already contains a correction that was never propagated back to any of them.

# Verified facts (file:line, checked at HEAD 665971c)

1. `internal/httpapi/authmw.go:76-83` -- `var unauthenticatedRoutes = map[string]struct{}{}` has SIX entries: `"/healthz"`, `"/v1/info"`, `RouteDiscovery`, `RouteEnroll`, `RouteSessionBegin`, `RouteSessionComplete`. Code is correct and fail-closed; only the prose describing it is wrong.
2. The sixth, `RouteDiscovery` (`/v1/discovery`), was added by the 2026-08-07 DISCOVERY-DOC decision, recorded at `DECISIONS.md:2896` ("## 2026-08-07 -- DISCOVERY-DOC: a separate `/v1/discovery` document, not a bigger `/v1/info`"). `INVARIANTS.md` was never amended when that route was added.
3. `INVARIANTS.md:74` (closing line of the invariant 3 paragraph, which runs ~52-74): "Sessions do NOT survive a restart. Every route authenticates EXCEPT the three that necessarily cannot: enrolment, session-begin and session-complete (they are how a credential is obtained), plus `/healthz` and `/v1/info`." -- five named exemptions, `/v1/discovery` absent.
4. `CLAUDE.md:52`: "3. **Enrolment is INVITE-ONLY ... Every route authenticates except enrolment, session begin/complete, `/healthz` and `/v1/info`." -- same stale five.
5. `AGENT_PROTOCOL.md:657` (not :655 as the finding that prompted this task said -- confirmed 657 at HEAD): "Every route except `GET /healthz`, `GET /v1/info`, `POST /v1/enroll`, `POST /v1/session/begin` and `POST /v1/session/complete` requires `Authorization: Bearer <token>`" -- same stale five.
6. `DECISIONS.md` ALREADY records the correct count in two places -- `DECISIONS.md:790-792` ("> CORRECTED 2026-08-16: the map has SIX entries, not five...") and `DECISIONS.md:6263-6267` ("The unauthenticated route list is six, not five...") -- but neither correction was carried back into INVARIANTS.md, CLAUDE.md or AGENT_PROTOCOL.md, which is exactly the "corrected in the append-only log but the authoritative doc is still wrong" gap this task exists to close.

# Why it matters
This is a wrong authoritative claim about the authentication boundary -- the highest-consequence place in the project to have one. CLAUDE.md's own preamble warns that a stale note is more dangerous than no note, because it reads as freshly checked. It is also live: `RELAY-55` (0a571a02-2f1f-41b7-8137-1a085c30f5e1) is filed and turns on precisely this enumeration -- an *authenticated* new route (e.g. a `/v1/readyz` rollout gate) owes no amendment here; an *unauthenticated* one would, and a reader trusting the stale five would not know to check.

# Scope / ownership
`INVARIANTS.md` and `CLAUDE.md` are OPERATOR-OWNED -- the operator writes the wording himself; no agent edits either file for this task. `AGENT_PROTOCOL.md` is agent-facing and is legitimately editable by an agent, but at filing time it is contended: `git status` shows it staged (`M `) by another live task, with three other agents concurrently running in docs/, internal/hub/ and internal/relay/. Whoever picks this up must confirm `AGENT_PROTOCOL.md` is free of concurrent edits before touching it, and must prepare evidence + proposed replacement wording for INVARIANTS.md/CLAUDE.md and ROUTE it to the operator rather than editing those two files directly.

# Proof
`proof_cmd` asserts NONE of the three exact stale phrases are present any more (i.e. it currently FAILS, and should flip to a pass once each file is corrected -- by the operator for INVARIANTS.md/CLAUDE.md, by an agent for AGENT_PROTOCOL.md). Verified via `bash scripts/proof-check.sh '<cmd>'` against HEAD 665971c:
verdict=FAIL class=other exit=1 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0
(printed `STALE_ENUMERATION_FOUND` before the FAIL line -- confirmed RED, not VACUOUS.)

# Duplicate check performed
Searched all ~739 tasks (paginated) for "invariant 3", "unauthenticated", "discovery", "RouteDiscovery". No exact duplicate. `DOCS-13` (8ce01598-11df-47f3-aacb-43f627c00377, todo) is a related but DISTINCT INVARIANTS.md truth-pass scoped to lines #24,25,40,41,42,43,44,59 covering five different false narratives (invite-gate, relay mount, signature verification, client-cert request, memory-roster injection) -- it does not touch line 74 or the route-count claim. `HANDOVER-MAP-DOC` (a52d4a99-9679-4fec-84e2-f615c7762b14, todo) is a broader per-invariant status audit, also not this specific claim. Neither is a duplicate; this task is narrower and citable independently. Not linked as a formal blocks/relates edge to avoid asserting a relation this filing pass did not verify both ends of -- a future pass may relate them.

`task-key-DOCS` reservation namespace is out of sync with the actual DOCS-N sequence (reservation returned 5, but DOCS-5..DOCS-30 already exist under descriptive titles, not all reserved through this namespace) -- so per CLAUDE.md's own guidance this is filed with a descriptive title and no numeric key, letting the server's public_id be the identity, rather than colliding on a stale counter.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DISCOVERY-DOC](../../CORE/DISCOVERY-DOC--2d7ce37b/task.md) — DISCOVERY-DOC: self-describing unauthenticated discovery document so an agent with only a… (in_progress)
- [DOCS-13](../DOCS-13--8ce01598/task.md) — DOCS-13: \`INVARIANTS.md\` truth pass — 8 false factual claims in the file agents must read… (todo)
- [DOCS-30](../DOCS-30--a311a067/task.md) — DOCS-30: clientcert help says the bus ignores the client certificate; the bus refuses 409… (todo)
- [DOCS-5](../DOCS-5--051a9829/task.md) — DOCS-5: \`/v1/discovery\` limitation 5 is false on the wire: cross-bus relay IS served (todo)
- [HANDOVER-MAP-DOC](../../HANDOVER/HANDOVER-MAP-DOC--a52d4a99/task.md) — HANDOVER-MAP-DOC: INVARIANTS.md -- each of the 11 invariants, its real status at HEAD, an… (todo)
- [RELAY-55](../../RELAY/RELAY-55--0a571a02/task.md) — RELAY-55: a bus can be healthy and silently deaf to the entire federation -- /healthz is… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
