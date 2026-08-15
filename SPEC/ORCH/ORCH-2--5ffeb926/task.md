# ORCH-2: certificate and invite PROVISIONING under an orchestrator — no CA, no TOFU, 0600, fingerprint in the invite

| Field | Value |
| --- | --- |
| Public id | `5ffeb926-d6dd-4139-ae69-c4c96b8b62b6` |
| Key | ORCH-2 |
| Epic | [ORCH](../epic.md) |
| Status | todo |
| Priority | P3 |
| Component | DEPLOY |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:00:50.315155+00:00 |
| Updated | 2026-08-15T08:00:50.315155+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestOrchProvisioning' ./cmd/agent-bus/... && echo ORCH2_PROVISIONING_OK
```

## Description

Say where a sidecar/k8s deployment gets its certificates and invites from, and prove the answer works.
BLOCKED ON ORCH-1.

## The constraint that shapes everything

Invariant 11: certificates are SELF-SIGNED, there is NO CA and NO TRUST-ON-FIRST-USE. The invite blob carries
the bus's certificate FINGERPRINT -- that is the entire trust bootstrap. So there is no cluster CA to lean on,
no cert-manager issuer to delegate to, and no "trust whatever answered" fallback. **A k8s-shaped solution that
reaches for a cluster CA has changed the trust model and needs its own decision, not a manifest.**

Rotation must not require re-enrolment, and rotation serves TWO certificates during rollover -- so the
provisioning story must survive a rotation without re-provisioning every client (see the MTLS epic; the client
identity holds an accept-set of up to two certificates).

## What must be answered concretely

- Where the bus's key material lives (a mounted volume? a Secret?) and who may read it. Note the data
  directory's permissions are ENFORCED AT STARTUP -- the bus tightens a group-writable data dir and warns
  loudly, because replacing a file is a permission on the DIRECTORY. A k8s volume mount with a permissive
  `fsGroup` will trip this; say what the correct mount looks like.
- Where invites come from, and their file mode. **`0600` or the CLI refuses them** -- and the natural
  container idiom (a redirect, a ConfigMap, a projected volume) does NOT produce `0600`. This is the same
  first-attempt failure documented in INVMINT-7; `relates`, do not duplicate.
- Whether invites are baked into an image (they must NOT be -- an invite is a bearer credential and images are
  shared and cached), and what the supported mechanism is instead.
- The pre-minted POOL pattern, since `agent-bus invite mint` needs the bus STOPPED (exclusive dirlock). Under
  an orchestrator "stop the bus" is a lifecycle event, so pooling has to be part of the provisioning story.
  The no-stop fix belongs to `INVMINT` -- `relates`, do not duplicate.
- Secret hygiene: an invite in a k8s Secret is base64, NOT encrypted at rest by default. Say so plainly rather
  than implying it is protected.

## Out of scope

Any "skip verification" flag, in any form, documented or not -- FORBIDDEN by invariant 11.

## PROOF STATUS -- READ BEFORE COMPLETING

The test named in `proof_cmd` DOES NOT EXIST YET; writing it is part of this task's deliverable, not a
pre-existing artefact. `scripts/proof-check.sh` reports VACUOUS until it is written, and THAT VACUOUS IS THE
RED OBSERVATION -- record it before starting. If the design lands under a different name, have spec-keeper
UPDATE this `proof_cmd`; never complete behind a proof naming a test nobody wrote (88 broken proofs in this
backlog, 2 tasks closed on targets that never existed). Do NOT put `<angle brackets>` in a proof_cmd --
proof-check.sh classifies them as an unfilled template and REFUSES TO RUN IT (caught on INVMINT-6).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [ORCH-1](../ORCH-1--e22449ec/task.md)
- **blocks** [ORCH-3](../ORCH-3--d75a3b68/task.md)
- **blocks** [ORCH-5](../ORCH-5--c4634621/task.md)
- **relates to** [INVMINT-7](../../INVMINT/INVMINT-7--174c7ba9/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVMINT-6](../../INVMINT/INVMINT-6--cedb8d6f/task.md) — INVMINT-6: \`agent-bus invite mint -count N\` — mint a pool in ONE process start (quick win… (todo)
- [INVMINT-7](../../INVMINT/INVMINT-7--174c7ba9/task.md) — INVMINT-7: document the pre-minted-invite pool recipe, with the 0600 mode built in (quick… (todo)
- [ORCH-1](../ORCH-1--e22449ec/task.md) — ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose ratio… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ORCH-1](../ORCH-1--e22449ec/task.md) — ORCH-1: DECIDE the network posture for sidecar/k8s — CONSENT-GATED, and the compose ratio… (todo)
- [ORCH-3](../ORCH-3--d75a3b68/task.md) — ORCH-3: first-boot ordering — a bus's first-ever boot can enrol NOBODY, and an orchestrat… (todo)
- [ORCH-5](../ORCH-5--c4634621/task.md) — ORCH-5: the sidecar and Kubernetes manifests themselves (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
