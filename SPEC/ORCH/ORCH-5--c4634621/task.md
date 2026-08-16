# ORCH-5: the sidecar and Kubernetes manifests themselves

| Field | Value |
| --- | --- |
| Public id | `c4634621-953c-4209-8d9b-68872d8736c9` |
| Key | ORCH-5 |
| Epic | [ORCH](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | DEPLOY |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:00:50.992351+00:00 |
| Updated | 2026-08-15T08:05:23.647719+00:00 |
| Completed | — |

## Proof command

```sh
test -f deploy/k8s/README.md && test -f deploy/sidecar/docker-compose.sidecar.yml && grep -q 'agent-bus' deploy/k8s/README.md && echo ORCH5_MANIFESTS_PRESENT
```

## Description

Ship the actual sidecar/k8s deployment artefacts. BLOCKED ON ORCH-1 (posture), and depends on ORCH-2
(provisioning), ORCH-3 (ordering) and ORCH-4 (restarts). It is deliberately LAST: every hard question is
answered in those tasks, and this one is assembly.

## Scope

- The in-pod SIDECAR shape (containers in one pod share a network namespace -- if ORCH-1 confirms this reaches
  `127.0.0.1:8080` with NO bind change, this is the cheapest and most defensible topology and should be the
  documented default).
- A Kubernetes manifest set for a standalone bus, honouring ORCH-1's posture ruling exactly.
- A docker-compose SIDECAR example, distinct from DEPLOY-2's single-bus file, which stays as it is.

## Hard constraints

- **DO NOT modify `docker-compose.yml`'s security constraint block or add `ports:` to it.** DEPLOY-2 owns that
  file and DEPLOY-REDEPLOY explicitly says "Do NOT widen the listener / do NOT add a ports: mapping to satisfy
  this task." New examples go in NEW files.
- Persistence: the WAL and data directory need durable storage with correct permissions (the bus ENFORCES data
  directory permissions at startup and will tighten a group-writable dir). An `emptyDir` loses the roster and
  the WAL -- invariant 5 says disk is the truth, so an ephemeral volume silently discards the property the
  whole system is built on. Say so.
- **Never more than ONE bus process per data directory.** The dirlock enforces it, but a manifest that can
  schedule two replicas against one volume is a defect even if the lock saves it. Replicas > 1 against one
  volume must be impossible by construction, not by luck.
- `agent-busctl` is NOT in the built image (CLI-BUSCTL-IMAGE, open). A sidecar whose neighbour must talk to
  the bus needs the client somewhere -- depend on that task, do not re-solve it.
- No `--no-check-certificate`-style probe, and no skip-verification flag: invariant 11. Use
  `agent-bus healthcheck` (a subcommand that takes no lock and is safe against a running bus).

## Proof — honest about its limits

The stored proof asserts the artefacts EXIST and are named; it does NOT prove a cluster converges, because
this box has no cluster and a proof that silently skips is worse than none. **If a cluster (kind/k3d) is
available, replace this proof with one that actually applies the manifests and proves an agent enrols** --
have spec-keeper update `proof_cmd`. Until then the real acceptance is an execution transcript recorded in
`AGENT_LOG.md`, in the style DEPLOY-REDEPLOY already demands: "VERIFICATION BY EXECUTION, not container
health -- the container being healthy/Up is NOT sufficient." Do not complete this task on manifests that were
never applied anywhere.

## PROOF CORRECTED AT FILING (spec-keeper, 2026-08-15)

The first version of this proof ended with `bash scripts/proof-check.sh --help`, i.e. proof-check invoking
ITSELF. That is a latent recursion landmine -- see the open TOOLING task PROOF-CHECK-FU-RECURSION, which
records a real observed incident where a proof resolved to a shim that re-invoked the checker and
"recurses/forks until killed", spawning dozens of processes. Never put `scripts/proof-check.sh` inside a
`proof_cmd`; the checker is the thing that RUNS the proof, never part of it. Replaced with a plain
artefact-existence assertion. Verified verdict=FAIL (RED) after the change.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CLI-BUSCTL-IMAGE](../../CLI/CLI-BUSCTL-IMAGE--9be2105d/task.md) — CLI-BUSCTL-IMAGE: Ship the busctl binary in the container image (todo)
- [DEPLOY-2](../../DEPLOY/DEPLOY-2--14f8ec3b/task.md) — DEPLOY-2: docker-compose.yml -- single bus, named volume, healthcheck (done)
- [DEPLOY-REDEPLOY](../../DEPLOY/DEPLOY-REDEPLOY--f801d128/task.md) — DEPLOY-REDEPLOY: recreate the Compose bus fresh (volume included) and prove two agents ex… (todo)
- [ORCH-1](../ORCH-1--e22449ec/task.md) — ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose ratio… (todo)
- [ORCH-2](../ORCH-2--5ffeb926/task.md) — ORCH-2: certificate and invite PROVISIONING under an orchestrator — no CA, no TOFU, 0600,… (todo)
- [ORCH-3](../ORCH-3--d75a3b68/task.md) — ORCH-3: first-boot ordering — a bus's first-ever boot can enrol NOBODY, and an orchestrat… (todo)
- [ORCH-4](../ORCH-4--282a2e9c/task.md) — ORCH-4: restart semantics under an orchestrator — sessions are IN-MEMORY, so every pod re… (todo)
- [PROOF-CHECK-FU-RECURSION](../../TOOLING/PROOF-CHECK-FU-RECURSION--69eb6f56/task.md) — PROOF-CHECK-FU-RECURSION: bash scripts/proof-check.sh hangs / spawns runaway processes wh… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ORCH-1](../ORCH-1--e22449ec/task.md) — ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose ratio… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
