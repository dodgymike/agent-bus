# RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal

| Field | Value |
| --- | --- |
| Public id | `701dc54d-f7a7-446b-9166-dfe205b0eb67` |
| Key | RELAY-20 |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P0 |
| Component | httpapi |
| Section | backlog |
| Tags | vacuous-today, critical-path |
| Created | 2026-08-08T15:56:45.897307+00:00 |
| Updated | 2026-08-14T11:28:52.611375+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestPeerRoutesRegisterOnlyWithRegistryAndTrust ./internal/httpapi
```

## Status note

CORRECTED AGAIN 2026-08-14 (reconciliation): the previous status_note named RELAY-44 (cec27a90) as the real dependency. RELAY-44 was filed independently of RELAY-45 (4be32336-5a48-410e-a70c-62ea154a6196), 39 seconds apart, describing the same gap -- two different agents, confirmed via the reservations event log, not a retry. RELAY-45 is the reconciled survivor (earlier filing, broader acceptance criteria); RELAY-44 is now `superseded` and its description says so. This task now unblocks when MTLS-CLIENTAUTH (cc9558a8, in_progress) and RELAY-45 (4be32336) land, not RELAY-44 and not RELAY-41 directly. Real `blocks` relations point at the survivor: RELAY-45 blocks this task (confirmed live via GET .../relations); the stale RELAY-44 edges are documented as stale on RELAY-44 itself, since relations cannot be deleted.

## Description

FEDERATION phase, wave 3. Deps: RELAY-17 (CrossBusTrust), RELAY-18 (import guard replaced).

Do NOT add peer paths to unauthenticatedRoutes -- that would create the ungated federation path
the guard forbids. Routes register only when registry AND trust are both non-nil (nil => 404,
NEVER a registered-503).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [RELAY-17](../RELAY-17--817649ce/task.md)
- **blocked by** [RELAY-18](../RELAY-18--fa5d1b0d/task.md)
- **blocked by** [RELAY-22](../RELAY-22--b4e45cda/task.md)
- **blocked by** [RELAY-41](../RELAY-41--05253c80/task.md)
- **blocked by** [RELAY-45](../RELAY-45--4be32336/task.md)
- **blocked by** [RELAY-45-FU-CLI](../RELAY-45-FU-CLI--b9d645be/task.md)
- **blocked by** [ca356fde-0613-42cb-ac85-a629609d9c78](../../MTLS/Client-certificate-expiry-is-not-enforced-anywhere-Requi--ca356fde/task.md)
- **blocked by** [RELAY-44](../RELAY-44--cec27a90/task.md)
- **blocks** [RELAY-21](../RELAY-21--f5ce883e/task.md)
- **blocks** [RELAY-23](../RELAY-23--220d36f4/task.md)
- **blocks** [RELAY-24](../RELAY-24--e303c624/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [RELAY-17](../RELAY-17--817649ce/task.md) — RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope (done)
- [RELAY-18](../RELAY-18--fa5d1b0d/task.md) — RELAY-18: Retire the relay import guard deliberately, replaced by a narrower one (done)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [06ac5885-5df4-4fab-8b51-45b37c7a38c2](../CONTRACTS-ONDISK.md-document-the-bus_path-len-1-is-recor--06ac5885/task.md) — CONTRACTS-ONDISK.md: document the bus_path\[len-1\]-is-recording-bus on-disk invariant, and… (todo)
- [48223968-0f96-4ac2-8d7e-710a1a4026b8](../Choose-the-abuse-control-primitive-for-a-MULTI-PRINCIPAL--48223968/task.md) — Choose the abuse-control primitive for a MULTI-PRINCIPAL relay link (todo)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (todo)
- [RELAY-22](../RELAY-22--b4e45cda/task.md) — RELAY-22: Choose and wire the multi-principal relay abuse-control primitive (todo)
- [RELAY-23](../RELAY-23--220d36f4/task.md) — RELAY-23: Relay wire protocol version (todo)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (in_progress)
- [RELAY-45-FU-CLI](../RELAY-45-FU-CLI--b9d645be/task.md) — RELAY-45-FU-CLI: operator CLI surface for the inbound peer client-certificate binding (todo)
- [RELAY-45-FU-ROTATION](../RELAY-45-FU-ROTATION--ec1c1d7c/task.md) — RELAY-45-FU-ROTATION: inbound peer client-certificate binding has no rollover overlap win… (todo)
- [RELAY-46](../RELAY-46--eb5c3312/task.md) — RELAY-46: NextHopTLSCertFingerprint should be a bounded list, not a scalar, for peer-cert… (todo)
- [RELAY-6](../RELAY-6--0f7275b9/task.md) — RELAY-6: Record the FEDERATION deployment assumptions (todo)
- [RELAY-9-FU-CODEGUARD](../RELAY-9-FU-CODEGUARD--1e9b54d2/task.md) — RELAY-9-FU-CODEGUARD: AST guard asserting every peer error code constant has a handler ca… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
