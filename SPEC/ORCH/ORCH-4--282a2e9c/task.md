# ORCH-4: restart semantics under an orchestrator — sessions are IN-MEMORY, so every pod restart invalidates every token

| Field | Value |
| --- | --- |
| Public id | `282a2e9c-fd52-4b31-81ab-9d66b67ab49c` |
| Key | ORCH-4 |
| Epic | [ORCH](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | DEPLOY |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:00:50.772053+00:00 |
| Updated | 2026-08-15T08:00:50.772053+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestOrchRestartSessionInvalidation' ./client/... ./internal/auth/... && echo ORCH4_RESTART_OK
```

## Description

Make the client-side consequence of a bus restart a designed-for, documented event. BLOCKED ON ORCH-1.

## The fact

**Sessions are IN-MEMORY ONLY and do not survive a restart** (invariant 3: sessions last at most one hour, are
opaque server-side handles rather than signed claims -- which is what makes immediate revocation possible --
and do not survive a restart). So every restart invalidates every bearer token and each agent must re-run the
session handshake.

**IT DOES NOT RE-ENROL. The roster is DURABLE.** State this loudly wherever the restart cost is described: a
reader who believes a restart de-enrols agents will vastly over-estimate the cost of a restart and may
"solve" it by persisting sessions -- which would DIRECTLY violate invariant 3 and destroy immediate
revocation. That mistake is the reason this task exists.

Under Compose a restart is rare and manual. **Under k8s a pod restart is routine** -- rollouts, evictions,
node drains, liveness-probe failures, scaling. So a rare event becomes a normal operating event, and a client
that handles it poorly will look like an outage.

## Requirements

- Document the expected client behaviour on a `401`/expired-session after a restart: re-run the handshake with
  the EXISTING durable identity, do not re-enrol, do not consume a new invite. Consuming an invite per restart
  would burn the pool and is exactly the wrong reflex.
- Verify the client actually does this rather than assuming it. If it does not, that is a client task -- file
  it in CLI and add a `blocks` edge rather than fixing it here.
- **Invariant 10 interaction, and this is the sharp edge:** an in-flight send interrupted by a restart must be
  retried under the SAME idempotency key, or the retry becomes a duplicate message. Note the open CLI task
  "busctl send/broadcast lose the minted idempotency key on an ambiguous failure" -- a restart is precisely an
  ambiguous failure, so that defect is ON this path. `relates` it.
- Note that idempotency keys are retained for a BOUNDED window and FAIL CLOSED, so a retry arriving after a
  long outage is REJECTED rather than silently re-applied. Say what a client should do then; a rejection here
  is correct behaviour, not a bug to route around.
- Say what a restart does NOT lose: the roster, minted invites, the WAL, message sequence floors. Ids are
  never reused across restarts (invariant 1).

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

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ORCH-1](../ORCH-1--e22449ec/task.md) — ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose ratio… (todo)
- [ORCH-5](../ORCH-5--c4634621/task.md) — ORCH-5: the sidecar and Kubernetes manifests themselves (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
