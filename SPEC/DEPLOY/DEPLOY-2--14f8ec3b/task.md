# DEPLOY-2: docker-compose.yml -- single bus, named volume, healthcheck

| Field | Value |
| --- | --- |
| Public id | `14f8ec3b-23e1-47b9-bfd0-05bd3331b500` |
| Key | DEPLOY-2 |
| Epic | [DEPLOY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | deploy |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T10:15:41.062046+00:00 |
| Updated | 2026-08-07T12:09:31.476059+00:00 |
| Completed | 2026-08-07T12:09:31.476044+00:00 |

## Proof command

```sh
DOCKER_HOST=unix:///run/docker.sock; DOCKER=/snap/docker/current/bin/docker; ! grep -q container_name docker-compose.yml && $DOCKER compose -p agentbus-proof up -d --build && sleep 8 && $DOCKER compose -p agentbus-proof ps --format json | grep -q "\"Health\":\"healthy\"" && $DOCKER compose -p agentbus-proof exec -T agent-bus wget -q -O - http://127.0.0.1:8080/healthz && $DOCKER compose -p agentbus-proof down -v
```

## Status note

COMMITTED at 260d23d (corrected sha; ad45078 does not exist). Reviewer blocking item RESOLVED: README.md:57 has a "## Container / Docker Compose" section (up -d --build, logs -f, ps, down, down -v, volume durability, loopback-only posture), landed in 260d23d. Security re-confirmed PASS. The patched project-isolated proof_cmd (docker compose -p agentbus-proof ...) is in place, replacing the original host-loopback-curl form that could not succeed by design. PROOF BLOCKED ON DEPLOY-2-FU-CONTAINERNAME (e9dd20b4-6ecc-40da-9cdf-cc31ee3cab64): docker-compose.yml:80 hardcodes container_name: agent-bus, which is GLOBAL in Docker (not project-namespaced), so even the project-isolated proof_cmd collides with the LIVE agentbus deployments own container of the same name and cannot pass while that deployment is running. This is a real product defect, not a bookkeeping artifact. Completion of DEPLOY-2 is gated ONLY on that container_name fix (plus running the proof at/after 78412ff, where the unrelated internal/wal build break is fixed). Do not complete until then.

## Description

docker-compose.yml running a single agent-bus service built from the Dockerfile (DEPLOY-1): a named volume mounted at the container's data-dir (so `compose down` without `-v` preserves durable state per invariants 4/5), a healthcheck wired to the existing `/healthz` route (interval/timeout/retries tuned so `docker compose ps` and `depends_on: condition: service_healthy` are meaningful), and configuration passed through the EXISTING flags/env the binary already accepts -- no new config surface invented here. Document the compose invocation (`docker compose up -d`, `docker compose logs -f`, `docker compose down`) in a short README section. Depends on DEPLOY-1 (Dockerfile).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DEPLOY-1](../DEPLOY-1--fa0c5a4e/task.md) — DEPLOY-1: Dockerfile -- multi-stage build, pinned Go builder, minimal runtime image (done)
- [DEPLOY-2-FU-CONTAINERNAME](../DEPLOY-2-FU-CONTAINERNAME--e9dd20b4/task.md) — DEPLOY-2-FU-CONTAINERNAME: docker-compose.yml hardcodes container_name, collides with any… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [637fca2f-0fa6-439a-b6eb-361b681cdf80](../../TOOLING/ENV-docker-CLI-needs-an-explicit-socket-binary-shim-for--637fca2f/task.md) — ENV: docker CLI needs an explicit socket+binary shim for agent shells (workaround known,… (todo)
- [CLI-BUSCTL-IMAGE](../../CLI/CLI-BUSCTL-IMAGE--9be2105d/task.md) — CLI-BUSCTL-IMAGE: Ship the busctl binary in the container image (todo)
- [CORE-12](../../CORE/CORE-12--ae000d92/task.md) — CORE-12: defaultListen=":8080" binds all interfaces -- prefer 127.0.0.1:8080 (superseded)
- [DEPLOY-3](../DEPLOY-3--9eaf2d19/task.md) — DEPLOY-3: multi-bus Compose profile (2+ peered buses) for RELAY end-to-end testing (todo)
- [DEPLOY-5](../DEPLOY-5--259a6a55/task.md) — DEPLOY-5: container build/test check (CI or make/script target) (todo)
- [MTLS-VERIFY](../../MTLS/MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: prove a RUNNING bus is TLS-only and enforces the current RequestClientCert p… (done)
- [ORCH-5](../../ORCH/ORCH-5--c4634621/task.md) — ORCH-5: the sidecar and Kubernetes manifests themselves (todo)
- [ORCH-6](../../ORCH/ORCH-6--6cfe7288/task.md) — ORCH-6: STANDALONE — verify and document what DEPLOY already ships; do NOT rebuild it (todo)
- [ZZB-firsthal](../../UNASSIGNED/ZZB-firsthal--74cb9c06/task.md) — probe (cancelled)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../../MTLS/EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
