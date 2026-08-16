# ORCH-3: first-boot ordering — a bus's first-ever boot can enrol NOBODY, and an orchestrator must not paper over it

| Field | Value |
| --- | --- |
| Public id | `d75a3b68-78c6-4a77-8aed-ed31e132c686` |
| Key | ORCH-3 |
| Epic | [ORCH](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | DEPLOY |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:00:50.552510+00:00 |
| Updated | 2026-08-15T08:00:50.552510+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestOrchFirstBootOrdering' ./cmd/agent-bus/... && echo ORCH3_FIRSTBOOT_OK
```

## Description

Handle the ordering constraint that a fresh bus cannot admit anyone on its first boot. BLOCKED ON ORCH-1.

## The constraint

An invite pins the bus's certificate FINGERPRINT, and that fingerprint only exists once a start has COMPLETED
and produced the certificate. So on a brand-new data directory there is a window in which no valid invite can
exist yet -- and `agent-bus invite mint` needs the bus STOPPED anyway (exclusive dirlock). The verified
working sequence is: **prime once, stop, pre-mint a pool, start** -- and then agents enrol against the running
bus indefinitely (measured: two agents enrolled with zero restarts between them, and a spent invite correctly
refused).

Under Compose that is a manual one-off. Under an orchestrator it is a LIFECYCLE PROBLEM: something must run
the prime/mint step exactly once, before dependents start, and it must be idempotent under retry because
orchestrators restart things.

## Requirements

- **This is an ORDERING problem, not a flakiness problem. Do NOT solve it with a client retry loop.** A
  dependent that retries until enrolment succeeds turns a deterministic sequencing requirement into a race
  that appears fixed and fails under load or on a slow node. If a retry is used at all it is a supplement to
  correct ordering, never a substitute.
- Whatever runs the prime/mint step must be IDEMPOTENT: re-running it must not mint a second pool, and must
  not fail the deployment. Invariant 10's shape applies -- same input, same result, no re-application.
- **It must not run concurrently with the bus, and must not run twice concurrently with itself.** Two writers
  to one log destroys it; the dirlock is what prevents that, so a second copy must FAIL LOUDLY, not wait
  forever or silently skip.
- State what happens on an EXISTING data directory (the far more common case): the step must detect the bus is
  already primed and do nothing, rather than re-priming or erroring the rollout.
- Readiness must reflect reality: do not report ready before the bus can actually serve enrolment.
  `agent-bus healthcheck` already exists as a subcommand that takes NO lock and is safe against a running
  bus -- use it rather than inventing a probe, and note invariant 11 forbids a `--no-check-certificate`-style
  probe.

Depends on ORCH-2 for where the invites land.

## PROOF STATUS -- READ BEFORE COMPLETING

The test named in `proof_cmd` DOES NOT EXIST YET; writing it is part of this task's deliverable, not a
pre-existing artefact. `scripts/proof-check.sh` reports VACUOUS until it is written, and THAT VACUOUS IS THE
RED OBSERVATION -- record it before starting. If the design lands under a different name, have spec-keeper
UPDATE this `proof_cmd`; never complete behind a proof naming a test nobody wrote (88 broken proofs in this
backlog, 2 tasks closed on targets that never existed). Do NOT put `<angle brackets>` in a proof_cmd --
proof-check.sh classifies them as an unfilled template and REFUSES TO RUN IT (caught on INVMINT-6).

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-6](../../INVMINT/INVMINT-6--cedb8d6f/task.md) — INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… (todo)
- [ORCH-1](../ORCH-1--e22449ec/task.md) — ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose ratio… (todo)
- [ORCH-2](../ORCH-2--5ffeb926/task.md) — ORCH-2: certificate and invite PROVISIONING under an orchestrator — no CA, no TOFU, 0600,… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ORCH-1](../ORCH-1--e22449ec/task.md) — ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose ratio… (todo)
- [ORCH-5](../ORCH-5--c4634621/task.md) — ORCH-5: the sidecar and Kubernetes manifests themselves (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
