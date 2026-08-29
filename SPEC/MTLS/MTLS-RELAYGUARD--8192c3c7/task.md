# MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance criterion plus a guard test

| Field | Value |
| --- | --- |
| Public id | `8192c3c7-78cb-44a4-a841-54b72be8fc2a` |
| Key | MTLS-RELAYGUARD |
| Epic | [MTLS](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T21:12:51.694883+00:00 |
| Updated | 2026-08-14T11:42:39.011142+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestRelayDialerRequiresMutualTLS|TestRelayListenerRequiresClientCert' ./internal/relay
```

## Status note

SPURIOUS EDGE FLAGGED 2026-08-14 (spec-keeper, per coordinator's priority-inversion check during RELAY-25 closure computation): a real `blocks` relation exists with MTLS-RELAYGUARD as source and RELAY-45 as target (created 2026-08-14T11:26:04 by codex-1). This produces a P2-blocks-P0 priority inversion feeding the P0 epic deliverable RELAY-25, which is a real smell -- but on inspection THE EDGE ITSELF IS WRONG, not the P2 priority. Evidence, from both tasks' own descriptions, neither of which I authored: (1) MTLS-RELAYGUARD's own description states "BLOCKS: RELAY-1 (9bc9d6c4), RELAY-2 (654140d7)" -- not RELAY-45 -- and explicitly disclaims the overlap: "This task covers the outbound client credential and transport guard. It does NOT establish the inbound fingerprint -> adjacent bus principal mapping; that distinct record/lookup... is owned by RELAY-45 (4be32336), which blocks RELAY-20." (2) RELAY-45's own DEPENDENCIES section lists only MTLS-CLIENTAUTH and RELAY-41 as what it depends on -- not MTLS-RELAYGUARD. Both tasks' own prose describe them as adjacent/parallel work in the same certificate area, not a blocking dependency. CONCLUSION: the P2 priority on MTLS-RELAYGUARD is correct for its actual scope (RELAY-1/RELAY-2); the edge to RELAY-45 is the defect. There is no DELETE on the relations API, so the edge is permanent -- do not treat it as load-bearing. Anyone computing RELAY-25's critical path should disregard the MTLS-RELAYGUARD->RELAY-45 edge specifically; RELAY-45's real blockers are MTLS-CLIENTAUTH (done) and RELAY-41 (done).

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7 | DEPENDS ON: MTLS-CLIENTAUTH, INVITE-PEERGUARD | BLOCKS: RELAY-1 (9bc9d6c4), RELAY-2 (654140d7)

Every relay hop is both a certificate-verifying TLS client and a TLS server, and invariant 2's <bus-id>.<agent-id> addressing plus traversed-bus-path loop prevention must keep working over it. internal/relay/ is a stub (internal/relay/doc.go:8), so the landable increment now is the guard and the acceptance criterion; RELAY-1 (9bc9d6c4) and RELAY-2 (654140d7) must satisfy it (the planner was not permitted to edit those tasks). Pairs with INVITE-PEERGUARD: a peer bus needs BOTH an invite and mutual TLS.

SECURITY CROSS-REFERENCE (2026-08-14): relay mutual-TLS verification is fingerprint-based over the presented leaf DER and MUST NOT build an x509.CertPool from enrolled agents, peer buses, or the bus's dual-usage certificate (see MTLS-RELAYGUARD-FU-BUSCERTPOOL). This task covers the outbound client credential and transport guard. It does NOT establish the inbound fingerprint -> adjacent bus principal mapping; that distinct record/lookup and its route-for ambiguity negatives are owned by RELAY-45 (4be32336), which blocks RELAY-20. A relay implementation must consume RELAY-45's dedicated principal binding and never reinterpret RELAY-41's duplicated `NextHopTLSCertFingerprint` route pins as an inbound identity.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-45](../../RELAY/RELAY-45--4be32336/task.md)
- **relates to** [RELAY-25-FU-CERTSHOW](../../RELAY/RELAY-25-FU-CERTSHOW--9c6813dc/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVITE-PEERGUARD](../../INVITE/INVITE-PEERGUARD--f5d91dbe/task.md) — INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate t… (todo)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-RELAYGUARD-FU-BUSCERTPOOL](../MTLS-RELAYGUARD-FU-BUSCERTPOOL--c873482f/task.md) — MTLS-RELAYGUARD-FU-BUSCERTPOOL: relay client-cert verification must not build a CertPool… (todo)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)
- [RELAY-2](../../RELAY/RELAY-2--654140d7/task.md) — RELAY-2: Message relay + ongoing roster sync across peers (done)
- [RELAY-20](../../RELAY/RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (done)
- [RELAY-41](../../RELAY/RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [RELAY-45](../../RELAY/RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2ca053dd-1b63-42b5-a485-f57b623722ac](../../RELAY/internal-relay-guards_test.go-912-says-the-RELAY-6-subst--2ca053dd/task.md) — internal/relay/guards_test.go:912 says the RELAY-6 substitution 'IS NOT RECORDED IN DECIS… (done)
- [3e542d14-81ea-4b86-8b95-a8ea6cfc4a79](../../RELAY/internal-relay-doc.go-still-specifies-per-connection-dis--3e542d14/task.md) — internal/relay/doc.go still specifies per-connection disconnect on idempotency-key-reuse-… (done)
- [48223968-0f96-4ac2-8d7e-710a1a4026b8](../../RELAY/Choose-the-abuse-control-primitive-for-a-MULTI-PRINCIPAL--48223968/task.md) — Choose the abuse-control primitive for a MULTI-PRINCIPAL relay link (todo)
- [INVITE-PEERGUARD](../../INVITE/INVITE-PEERGUARD--f5d91dbe/task.md) — INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate t… (todo)
- [MTLS-CLIENTAUTH](../MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-RELAYGUARD-FU-BUSCERTPOOL](../MTLS-RELAYGUARD-FU-BUSCERTPOOL--c873482f/task.md) — MTLS-RELAYGUARD-FU-BUSCERTPOOL: relay client-cert verification must not build a CertPool… (todo)
- [RELAY-2](../../RELAY/RELAY-2--654140d7/task.md) — RELAY-2: Message relay + ongoing roster sync across peers (done)
- [RELAY-25-FU-CERTSHOW](../../RELAY/RELAY-25-FU-CERTSHOW--9c6813dc/task.md) — RELAY-25-FU-CERTSHOW: no read-only command exposes a bus's own TLS certificate fingerprint (todo)
- [RELAY-3](../../RELAY/RELAY-3--e944edda/task.md) — RELAY-3: Loop prevention via traversed-bus path (done)
- [RELAY-4](../../RELAY/RELAY-4--5ac738b4/task.md) — RELAY-4: Peer-down retry/backoff (done)
- [RELAY-5](../../RELAY/RELAY-5--f3a31e10/task.md) — RELAY-5: Relay crash/loop integration test (done)
- [RELAY-6](../../RELAY/RELAY-6--0f7275b9/task.md) — RELAY-6: Record the FEDERATION deployment assumptions (done)
- [SIGN-7](../../SIGN/SIGN-7--aeb90793/task.md) — SIGN-7: Cross-bus relay preserves the signed envelope byte-exact -- an intermediate bus c… (done)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)
- [a695f85f-0c69-42a8-a653-deed4960a610](../../DOCS/PROTOCOL.md-8-cites-Spec-Server-task-id-INVITE-PEERGUARD--a695f85f/task.md) — PROTOCOL.md §8 cites Spec Server task id INVITE-PEERGUARD (f5d91dbe) as if it were a comm… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
