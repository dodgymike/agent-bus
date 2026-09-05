# Certificate-expiry enforcement is now present on the pinned-bus, agent, peer, and bus-startup planes; residual rebind/lockout follow-ups remain

| Field | Value |
| --- | --- |
| Public id | `ca356fde-0613-42cb-ac85-a629609d9c78` |
| Key | _(null in the export)_ |
| Epic | [MTLS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-07T13:05:07.445368+00:00 |
| Updated | 2026-08-23T21:23:55.219910+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run "TestExpiredBusCertificateIsRejectedDespiteMatchingPin|TestNotYetValidBusCertificateIsRejected|TestPinnedTLSConfigUsesALiveClock" ./client && go test -race -run "TestExpiredClientCertificateIsIgnored|TestNotYetValidClientCertificateIsIgnored|TestCrossCheckAnExpiredCertificateIsAbsenceNotPresence|TestExpiredPeerCertificateIsRefusedBeforeTheBindingIsConsulted|TestCheckClientCertValidityRefusesWhatItCannotJudge" ./internal/httpapi && go test -race -run "TestBusCertRefusesAnOutOfWindowCertificate" ./internal/buscert
```

## Status note

2026-08-23 spec-keeper reconciliation: stale RequireAnyClientCert framing corrected and non-vacuous clean-overlay proof recorded. Do not close yet without task-local reviewer PASS, security PASS, documentation PASS, and attached commit evidence for this reconciled scope; the landed implementation is spread across multiple earlier commits.

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-DESIGN (39dcdcff) | RELATED: MTLS-CROSSCHECK (2b2af075), RELAY-20 (701dc54d), 7a197025, b5d86daa

RECONCILED 2026-08-23. The original RequireAnyClientCert framing is stale. The running design uses tls.RequestClientCert at the listener and enforces certificate validity at the surfaces that AUTHORISE on a presented certificate, plus at the client pin and bus-startup planes. This task now records the landed expiry-enforcement evidence and the residual work that remains.

LANDED EVIDENCE IN HEAD:
1. PINNED BUS CERTIFICATE: client/pin.go rejects an otherwise-matching pinned bus certificate outside its own validity window (landed in 9f2878a, later touched in 9701611). The verifier checks the certificate AFTER the pin match and returns ErrBusCertificateExpired for both expired and not-yet-valid leaves.
2. AGENT PLANE: internal/httpapi/clientcert.go refuses to publish a fingerprint for an out-of-window presented client certificate, and MTLS-CROSSCHECK then treats that agent as presenting NO certificate for the route-level binding check (2ea7dfb on the cross-check path; the publication path remains in main). The listener still only REQUESTS a certificate; the validity decision is now at application admission, where the authorisation decision actually happens.
3. PEER / BUS PLANE: internal/httpapi/RequirePeerPrincipal checks the presented peer certificate's validity window BEFORE consulting the durable binding, so a stale peer certificate cannot resolve to an adjacent-bus principal even if the fingerprint remains bound (peer route work landed through ed77bba and later touched in 9701611).
4. BUS STARTUP / RECOVERY: internal/buscert.LoadOrCreate refuses to start on expired or not-yet-valid bus certificate material rather than silently regenerating or starting anyway (16f54c9 and later mainline touches).

RESIDUAL WORK PRESERVED, NOT CLOSED BY THIS TASK:
- 7a197025-93f9-470b-a69b-bad494eeae94 owns the USER-RATIFIED re-bind route for a still-valid identity to rotate its own client certificate without spending an invite.
- b5d86daa-a6d3-4fa3-945d-f933ad894274 owns the current lockout consequence for a bound agent whose client certificate expires before such a re-bind path exists.

This task should therefore be read as "expiry enforcement exists on the relevant present-certificate planes", not as "certificate lifecycle is complete".

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-20](../../RELAY/RELAY-20--701dc54d/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [7a197025-93f9-470b-a69b-bad494eeae94](../MTLS-re-bind-route-an-agent-renews-its-client-certificat--7a197025/task.md) — MTLS re-bind route: an agent renews its client certificate against its EXISTING agent id,… (todo)
- [MTLS-CROSSCHECK](../MTLS-CROSSCHECK--2b2af075/task.md) — MTLS-CROSSCHECK: reject a session token presented over a connection whose client certific… (done)
- [MTLS-CROSSCHECK-FU-CERTEXPIRY](../MTLS-CROSSCHECK-FU-CERTEXPIRY--b5d86daa/task.md) — A bound agent whose client certificate expires is locked out permanently, including from… (todo)
- [MTLS-DESIGN](../MTLS-DESIGN--39dcdcff/task.md) — MTLS-DESIGN: record the decided certificate lifecycle in DECISIONS.md -- key locations, h… (done)
- [RELAY-20](../../RELAY/RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-20](../../RELAY/RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
