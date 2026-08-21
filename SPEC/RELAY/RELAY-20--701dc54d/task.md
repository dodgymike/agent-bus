# RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal

| Field | Value |
| --- | --- |
| Public id | `701dc54d-f7a7-446b-9166-dfe205b0eb67` |
| Key | RELAY-20 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | httpapi |
| Section | backlog |
| Tags | vacuous-today, critical-path |
| Created | 2026-08-08T15:56:45.897307+00:00 |
| Updated | 2026-08-14T16:49:22.486921+00:00 |
| Completed | 2026-08-14T16:49:22.486906+00:00 |

## Proof command

```sh
go test -race -run TestPeerRoutesRegisterOnlyWithRegistryAndTrust ./internal/httpapi
```

## Status note

CODE-LANDED at ed77bba (part of the three-task wave with INVITE-GATE and RELAY-45), reviewer PASS + security PASS re-verified -- but CANNOT COMPLETE YET. Reviewer requires a dated DECISIONS.md entry covering the amended gate, the ca356fde expiry disposition, the 403-vs-401 pre-auth disclosure, and the one-factor decision. RELAY-6 owns that text and it is WRITTEN but still sitting in scratchpad, unapplied. DECISIONS.md is now committed and clean (as of e7a3c49), so RELAY-6's section can finally be applied -- that is what unblocks this task.

## Description

FEDERATION phase, wave 3. Deps: RELAY-17 (CrossBusTrust), RELAY-18 (import guard replaced).

Do NOT add peer paths to unauthenticatedRoutes -- that would create the ungated federation path
the guard forbids. Routes register only when registry AND trust are both non-nil (nil => 404,
NEVER a registered-503).

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [RELAY-17](../RELAY-17--817649ce/task.md) — RELAY-17: CrossBusTrust implementation + attestation travels in the relay envelope (done)
- [RELAY-18](../RELAY-18--fa5d1b0d/task.md) — RELAY-18: Retire the relay import guard deliberately, replaced by a narrower one (done)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-6](../RELAY-6--0f7275b9/task.md) — RELAY-6: Record the FEDERATION deployment assumptions (done)
- [ca356fde-0613-42cb-ac85-a629609d9c78](../../MTLS/Client-certificate-expiry-is-not-enforced-anywhere-Requi--ca356fde/task.md) — Client-certificate expiry is not enforced anywhere: RequireAnyClientCert does no chain ve… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [06ac5885-5df4-4fab-8b51-45b37c7a38c2](../CONTRACTS-ONDISK.md-document-the-bus_path-len-1-is-recor--06ac5885/task.md) — CONTRACTS-ONDISK.md: document the bus_path\[len-1\]-is-recording-bus on-disk invariant, and… (todo)
- [48223968-0f96-4ac2-8d7e-710a1a4026b8](../Choose-the-abuse-control-primitive-for-a-MULTI-PRINCIPAL--48223968/task.md) — Choose the abuse-control primitive for a MULTI-PRINCIPAL relay link (todo)
- [CLI-6](../../CLI/CLI-6--47001cb4/task.md) — CLI-6: log -- read the append-only audit log (metadata only; also absorbs the WAL-dumper… (done)
- [MTLS-BIND-FU-DOCS](../../MTLS/MTLS-BIND-FU-DOCS--8c40ea26/task.md) — MTLS-BIND-FU-DOCS: document the enrolment certificate binding -- CONTRACTS-HTTP.md 409, D… (todo)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-CROSSCHECK](../../MTLS/MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (in_progress)
- [MTLS-CROSSCHECK-FU-CERTEXPIRY](../../MTLS/MTLS-CROSSCHECK-FU-CERTEXPIRY--b5d86daa/task.md) — A bound agent whose client certificate expires is locked out permanently, including from… (todo)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-21-FU-DOCGAP4](../RELAY-21-FU-DOCGAP4--9972d0ed/task.md) — RELAY-21-FU-DOCGAP4: internal/relay/doc.go known-gaps item 4 falsely claims forward-only-… (done)
- [RELAY-22](../RELAY-22--b4e45cda/task.md) — RELAY-22: Choose and wire the multi-principal relay abuse-control primitive (todo)
- [RELAY-23](../RELAY-23--220d36f4/task.md) — RELAY-23: Relay wire protocol version (todo)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-44](../RELAY-44--cec27a90/task.md) — RELAY-44: Inbound peer-certificate binding record -- bind a presented CLIENT certificate… (superseded)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-45-FU-CLI](../RELAY-45-FU-CLI--b9d645be/task.md) — RELAY-45-FU-CLI: operator CLI surface for the inbound peer client-certificate binding (done)
- [RELAY-45-FU-ROTATION](../RELAY-45-FU-ROTATION--ec1c1d7c/task.md) — RELAY-45-FU-ROTATION: inbound peer client-certificate binding has no rollover overlap win… (todo)
- [RELAY-46](../RELAY-46--eb5c3312/task.md) — RELAY-46: NextHopTLSCertFingerprint should be a bounded list, not a scalar, for peer-cert… (todo)
- [RELAY-6](../RELAY-6--0f7275b9/task.md) — RELAY-6: Record the FEDERATION deployment assumptions (done)
- [RELAY-9-FU-CODEGUARD](../RELAY-9-FU-CODEGUARD--1e9b54d2/task.md) — RELAY-9-FU-CODEGUARD: AST guard asserting every peer error code constant has a handler ca… (todo)
- [RELAY-FU-PEERBUSID-CROSSCHECK](../RELAY-FU-PEERBUSID-CROSSCHECK--b2c28232/task.md) — RELAY-FU-PEERBUSID-CROSSCHECK: invariant 11's PEER cross-check is documented but unimplem… (done)
- [RELAY-FU-PEERENROLL-BUSID-BIND](../RELAY-FU-PEERENROLL-BUSID-BIND--12f39697/task.md) — internal/relay/doc.go gap 5 (inbound twin): peer B presenting its own valid certificate a… (done)
- [RELAY-FU-ROSTER-VERSION-BOUND](../RELAY-FU-ROSTER-VERSION-BOUND--b361d9e2/task.md) — internal/relay/doc.go gap 3: RosterUpdate.BusID is not bound to the authenticated connect… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
