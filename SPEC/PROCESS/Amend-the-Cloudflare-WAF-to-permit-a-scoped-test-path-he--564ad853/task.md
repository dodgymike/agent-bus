# Amend the Cloudflare WAF to permit a scoped test path/header for trigger-shaped payloads (unblocks proving 46afc19c)

| Field | Value |
| --- | --- |
| Public id | `564ad853-0c54-4797-9bda-85a253a6a646` |
| Key | _(null in the export)_ |
| Epic | [PROCESS](../epic.md) |
| Status | done |
| Priority | P2 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-22T10:24:09.354554+00:00 |
| Updated | 2026-08-22T11:41:41.679566+00:00 |
| Completed | 2026-08-22T11:41:41.679550+00:00 |

## Proof command

```sh
test -n "$AGENT_BUS_WAF_TEST_HEADER" && test -n "$AGENT_BUS_WAF_TEST_TOKEN" && test -n "$AGENT_BUS_WAF_TEST_PAYLOAD" || { echo "RED: set AGENT_BUS_WAF_TEST_HEADER, AGENT_BUS_WAF_TEST_TOKEN and AGENT_BUS_WAF_TEST_PAYLOAD (the trigger-shaped body, per the Trigger shape paragraph on 46afc19c -- never stored in this task record) once the Cloudflare edge change designates the scoped test route/header" >&2; exit 2; }; RESP=$(mktemp); CODE=$(curl -s -o "$RESP" -w "%{http_code}" -H "$AGENT_BUS_WAF_TEST_HEADER: $AGENT_BUS_WAF_TEST_TOKEN" -H "Content-Type: application/json" -X POST https://api.spec.elasticninja.com/api/v1/projects/agent-bus/tasks/00000000-0000-0000-0000-000000000000/notes -d "$AGENT_BUS_WAF_TEST_PAYLOAD"); if grep -qi cloudflare "$RESP"; then echo "FAIL: scoped path still WAF-blocked (http $CODE), edge change not effective yet" >&2; exit 1; fi; if [ "$CODE" = "404" ]; then echo PASS; exit 0; fi; echo "PROOF-INCONCLUSIVE: unexpected response code $CODE" >&2; exit 3
```

## Description

This is a change to the user's Cloudflare EDGE CONFIGURATION in front of api.spec.elasticninja.com, not a code change. No agent can make it -- it requires the user or an operator with Cloudflare dashboard/API access. This task tracks the decision and the requirement; a human executes it. Decided 2026-08-22: the user chose this option over prose-only documentation and over proving the wrapper's behaviour against a local non-WAF server, when asked how to prove the defect recorded on task 46afc19c-e0dd-48cf-b003-6f5fe3bac48c.

Why it is needed. 46afc19c documents that scripts/spec-cloud.sh cannot distinguish a Cloudflare WAF block from a Spec Server auth failure -- both currently surface as a bare HTTP 403. Proving that distinction requires deliberately sending a request the WAF blocks, and today BOTH running that proof AND storing the proof text hit the same control: an earlier attempt to save 46afc19c's own description was itself rejected three times before the trigger was isolated by bisection.

Scope -- keep this narrow. Add exactly ONE of: (a) a single designated test route (e.g. a path segment that exists only for this purpose), or (b) a header-gated bypass (a specific request header name/value pair that lets a request through a WAF rule only when present). Whichever is chosen, scope the Cloudflare rule to that single route or header match -- NOT a general relaxation of the WAF, and not a bypass of the WAF as a whole for any other path. A broad carve-out would weaken the control this task exists to keep meaningful. Whoever implements the edge change should pick the exact route/header name, record it and the reasoning in DECISIONS.md, and update this task's proof_cmd's environment-variable contract (see below) to match if it differs.

Explicitly forbidden alternative -- do not repeat it. Proofs for this defect class must NOT fragment or encode the trigger payload (build it from separate string pieces at runtime, or otherwise obscure it) in order to evade the WAF when sending OR when storing a proof. That technique was used once on 46afc19c to get its own description saved, was flagged by the authoring agent, confirmed by the user, and reverted -- see the 'Addendum -- 2026-08-22' section of 46afc19c's description. This task exists so that reproduction happens through a legitimate, narrowly-scoped allow path instead of through evasion.

Relates to 46afc19c-e0dd-48cf-b003-6f5fe3bac48c, which this task BLOCKS: that task's proof_cmd was set to null pending this decision and can be written once the scoped test path/header exists.
## Addendum -- 2026-08-22, edge change applied and verified

The Cloudflare WAF change this task tracks has been made and verified. Option (b) from the scope
paragraph above was chosen: a header-gated bypass, not a new route.

- A `skip` rule was added to the `elasticninja.com` zone's custom firewall ruleset (phase
  `http_request_firewall_custom`). Cloudflare rule id `d6468bb0303949ef986773a2c300ea70`. It skips
  the Managed WAF for `api.spec.elasticninja.com` requests to `/api/v1/*` ONLY when a secret
  header is present on the request. It sits alongside a pre-existing `/fleet/*` skip rule, which
  was not modified.
- Header name: `x-agent-bus-waf-test`. The secret value is stored OUTSIDE the repo at
  `/mnt/sdc/mike/claude-scratch/waf-test-bypass-agent-bus.env` (mode 600), alongside the
  spec-cloud creds, as `AGENT_BUS_WAF_TEST_HEADER` / `AGENT_BUS_WAF_TEST_TOKEN`. The value is
  intentionally NOT recorded in this task, any note, or any description -- it is a live
  production WAF-bypass secret.
- Verified in both directions:
  - A WAF-triggering request body with NO header returns a Cloudflare HTML 403 block page
    (`content-type: text/html`, `cf-ray` header present, HTML body) -- confirming the WAF is
    still active for normal traffic.
  - The SAME body WITH the secret header reaches the origin and returns the Spec Server's own
    JSON response (`401 {"message":"Unauthorized"}` in this check, since it hit an authenticated
    route without valid session auth) -- confirming the bypass works and traffic reaches the
    Spec Server rather than being blocked at the edge.
  - This is exactly the WAF-403-vs-auth-4xx shape distinction that 46afc19c's fix needs to key on:
    `content-type: text/html` + `cf-ray` + HTML body (WAF block) vs. `content-type:
    application/json` + Spec Server JSON body (auth/app failure).
- This verification was performed by the operator (user) with the secret sourced from the env
  file above, not by an agent session, to avoid pulling the secret token into agent context.
  The task's stored `proof_cmd` requires `AGENT_BUS_WAF_TEST_HEADER`, `AGENT_BUS_WAF_TEST_TOKEN`
  and `AGENT_BUS_WAF_TEST_PAYLOAD` sourced into the environment; it is human-run for the same
  reason. It was exercised in this equivalent, human-run form and passed (edge lets the header
  through, no-header traffic is still blocked) -- see the two-direction results above.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [46afc19c-e0dd-48cf-b003-6f5fe3bac48c](../scripts-spec-cloud.sh-reports-a-Cloudflare-WAF-block-as--46afc19c/task.md) — scripts/spec-cloud.sh reports a Cloudflare WAF block as a bare HTTP 403, indistinguishabl… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [46afc19c-e0dd-48cf-b003-6f5fe3bac48c](../scripts-spec-cloud.sh-reports-a-Cloudflare-WAF-block-as--46afc19c/task.md) — scripts/spec-cloud.sh reports a Cloudflare WAF block as a bare HTTP 403, indistinguishabl… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
