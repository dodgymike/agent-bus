# ORCH-6: STANDALONE — verify and document what DEPLOY already ships; do NOT rebuild it

| Field | Value |
| --- | --- |
| Public id | `6cfe7288-ca1e-45ea-84c1-194834bcc8d1` |
| Key | ORCH-6 |
| Epic | [ORCH](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | DEPLOY |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:04:03.129149+00:00 |
| Updated | 2026-08-15T08:04:03.129149+00:00 |
| Completed | — |

## Proof command

```sh
grep -q -i 'standalone' README.md && grep -q 'agent-bus healthcheck' README.md && echo ORCH6_STANDALONE_DOC_OK
```

## Description

The third of the operator's ask that is LARGELY ALREADY DONE. **NOT BLOCKED ON ORCH-1** -- the standalone
shape needs no posture change (it is the shipped loopback default), so this can be done at any time and is the
cheapest item in the epic.

## What already exists — verify, do not duplicate

- **DEPLOY-1 is DONE**: multi-stage Dockerfile, pinned Go builder, minimal runtime image.
- **DEPLOY-2 is DONE**: `docker-compose.yml`, single bus, named volume, healthcheck (now invoking
  `agent-bus healthcheck`, not `wget`, because busybox `wget` cannot trust one self-signed certificate without
  `--no-check-certificate`, which invariant 11 forbids).
- **DEPLOY-2-FU-CONTAINERNAME is DONE**.
- **DEPLOY-REDEPLOY (open)** already carries the strongest acceptance in the area: two distinct agents enrol
  against a freshly recreated bus and exchange a message, "VERIFICATION BY EXECUTION, not container health".

**So the standalone container ALREADY WORKS. This task is verification and documentation ONLY.** If you find
yourself editing the Dockerfile or `docker-compose.yml`, STOP -- that is DEPLOY's work and you have the wrong
task. `relates` DEPLOY-REDEPLOY; if the verification you need is exactly its acceptance criterion, depend on
it rather than repeating it.

## Deliverable

A `README.md` section for running the bus standalone that states honestly:
- how to start it and how to check it is healthy (`agent-bus healthcheck` -- takes no lock, safe against a
  running bus);
- **that as shipped it binds loopback INSIDE its own container and is therefore unreachable from outside it**
  -- "That is the price of the constraint, not an oversight", in `docker-compose.yml`'s own words. A reader
  who does not learn this immediately will conclude the container is broken. This is the single most valuable
  sentence in the task;
- the bootstrap sequence: prime, stop, pre-mint a pool (invites `0600` or the CLI refuses them), start, then
  enrol against the running bus. Point at `INVMINT` for the no-stop fix rather than restating it;
- that `agent-busctl` is not in the image today (CLI-BUSCTL-IMAGE, open), so the client must be supplied.

## Proof

Pins two specific strings in `README.md`. RED BASELINE VERIFIED BY SPEC-KEEPER AT FILING (2026-08-15):
the standalone and healthcheck greps both return 0 today, so neither matches incidentally. This check
mattered -- `README.md` already carries container and healthcheck material, and a pre-existing healthz probe
line in this very file once green-lit closing a task that two reviewers had blocked. Re-confirm RED before
the fix; if a reword makes either string match early, CHANGE THE PINNED STRINGS rather than accepting a proof
that passes without the fix.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-BUSCTL-IMAGE](../../CLI/CLI-BUSCTL-IMAGE--9be2105d/task.md) — CLI-BUSCTL-IMAGE: Ship the busctl binary in the container image (todo)
- [DEPLOY-1](../../DEPLOY/DEPLOY-1--fa0c5a4e/task.md) — DEPLOY-1: Dockerfile -- multi-stage build, pinned Go builder, minimal runtime image (done)
- [DEPLOY-2](../../DEPLOY/DEPLOY-2--14f8ec3b/task.md) — DEPLOY-2: docker-compose.yml -- single bus, named volume, healthcheck (done)
- [DEPLOY-2-FU-CONTAINERNAME](../../DEPLOY/DEPLOY-2-FU-CONTAINERNAME--e9dd20b4/task.md) — DEPLOY-2-FU-CONTAINERNAME: docker-compose.yml hardcodes container_name, collides with any… (done)
- [DEPLOY-REDEPLOY](../../DEPLOY/DEPLOY-REDEPLOY--f801d128/task.md) — DEPLOY-REDEPLOY: recreate the Compose bus fresh (volume included) and prove two agents ex… (todo)
- [ORCH-1](../ORCH-1--e22449ec/task.md) — ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose ratio… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [DEPLOY-6](../../DEPLOY/DEPLOY-6--e12b75cd/task.md) — DEPLOY-6: host-reachable Dockerfile CMD + THREE-BUS-DOCKER.md federated runbook (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
