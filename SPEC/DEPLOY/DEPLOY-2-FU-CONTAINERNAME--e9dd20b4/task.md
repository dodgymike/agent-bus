# DEPLOY-2-FU-CONTAINERNAME: docker-compose.yml hardcodes container_name, collides with any other compose project using the same name

| Field | Value |
| --- | --- |
| Public id | `e9dd20b4-6ecc-40da-9cdf-cc31ee3cab64` |
| Key | _(null in the export)_ |
| Epic | [DEPLOY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | deploy |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T22:53:52.484792+00:00 |
| Updated | 2026-08-07T11:30:09.949174+00:00 |
| Completed | 2026-08-07T11:30:09.949158+00:00 |

## Proof command

```sh
DOCKER_HOST=unix:///run/docker.sock; DOCKER=/snap/docker/current/bin/docker; ! grep -q container_name docker-compose.yml && $DOCKER compose -p agentbus-proof up -d --build && sleep 8 && $DOCKER compose -p agentbus-proof ps --format json | grep -q '"Health":"healthy"' && $DOCKER compose -p agentbus-proof exec -T agent-bus wget -q -O - http://127.0.0.1:8080/healthz && $DOCKER ps --format '{{.Names}}' | grep -qx agent-bus && $DOCKER compose -p agentbus-proof down -v   # requires the real snap binary + explicit socket, see DECISIONS.md 2026-08-07 'the working docker invocation for agent shells on this box'
```

## Status note

Code LANDED at 518e71b (container_name line removed from docker-compose.yml). Triage verified the line is absent and 518e71b is green. NOT COMPLETABLE YET: the proof verdict is UNVERIFIABLE, not PASS -- the Docker CLI is non-functional for every agent on this box (snap confinement cannot resolve /home/mike, a symlink to /mnt/sdb4/mike/mike; even 'docker ps' fails). Independently reproduced by triage, not merely claimed. Tracked as 637fca2f. Nobody has yet watched this compose file come up twice on one host, which is the entire point of the change. Complete this ONLY after a real 'docker compose -p agentbus-proof up -d --build' is observed. DEPLOY-3 (end-to-end RELAY verification) is now structurally unblocked but remains unprovable until 637fca2f is fixed.

## Description

docker-compose.yml:80 sets `container_name: agent-bus` unconditionally. Docker container names are global (not project-namespaced), so `docker compose -p <any-other-project> up` for this same file collides with any OTHER running container also named agent-bus -- including, on this box, the live production `agentbus` compose project (its service container is also named agent-bus). Reproduced 2026-08-02: `docker compose -p agentbus-proof up -d --build` from a clean commit failed cleanly with `Conflict. The container name "/agent-bus" is already in use` while the live agentbus project was running -- Docker refused rather than clobbering anything, so no data was harmed, but it means a project-name-isolated proof_cmd (see DEPLOY-2s patched proof_cmd) still cannot succeed while the live deployment is up. Fix: drop the `container_name:` line (or template it off `${COMPOSE_PROJECT_NAME}`) so compose derives a project-scoped name the normal way.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [637fca2f-0fa6-439a-b6eb-361b681cdf80](../../TOOLING/ENV-docker-CLI-needs-an-explicit-socket-binary-shim-for--637fca2f/task.md) — ENV: docker CLI needs an explicit socket+binary shim for agent shells (workaround known,… (todo)
- [DEPLOY-3](../DEPLOY-3--9eaf2d19/task.md) — DEPLOY-3: multi-bus Compose profile (2+ peered buses) for RELAY end-to-end testing (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [637fca2f-0fa6-439a-b6eb-361b681cdf80](../../TOOLING/ENV-docker-CLI-needs-an-explicit-socket-binary-shim-for--637fca2f/task.md) — ENV: docker CLI needs an explicit socket+binary shim for agent shells (workaround known,… (todo)
- [CLI-3](../../CLI/CLI-3--6e70abe5/task.md) — CLI-3: watch -- long-poll tail, human-readable for a person and NDJSON for a pipe (replac… (done)
- [CLI-4](../../CLI/CLI-4--137465b9/task.md) — CLI-4: send + broadcast, incl. stdin and interactive (replaces bus-send.sh and bus-broadc… (done)
- [CLI-5](../../CLI/CLI-5--86dea094/task.md) — CLI-5: agents -- roster listing (replaces bus-agents.sh) (done)
- [DEPLOY-2](../DEPLOY-2--14f8ec3b/task.md) — DEPLOY-2: docker-compose.yml -- single bus, named volume, healthcheck (done)
- [ORCH-6](../../ORCH/ORCH-6--6cfe7288/task.md) — ORCH-6: STANDALONE — verify and document what DEPLOY already ships; do NOT rebuild it (todo)
- [ZZB-firsthal](../../UNASSIGNED/ZZB-firsthal--74cb9c06/task.md) — probe (cancelled)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
