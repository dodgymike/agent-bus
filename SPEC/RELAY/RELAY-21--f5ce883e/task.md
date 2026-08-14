# RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeNew

| Field | Value |
| --- | --- |
| Public id | `f5ce883e-80dd-4ce4-82e6-b52c470fba4f` |
| Key | RELAY-21 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | vacuous-today, critical-path |
| Created | 2026-08-08T15:56:46.477814+00:00 |
| Updated | 2026-08-14T18:04:21.565026+00:00 |
| Completed | 2026-08-14T18:04:21.565011+00:00 |

## Proof command

```sh
go test -race -run TestAcceptRelay ./internal/relay
```

## Status note

DOC GATE CLOSED at 0d31d2f -- the 404 unknown_recipient row is now documented in both PROTOCOL.md:1059 and CONTRACTS-HTTP.md:820. ONLY THE REVIEWER GATE REMAINS -- do not complete until it runs; landing the text and completing the task are separate things. TWO CORRECTIONS RECORDED IN THOSE DOCS, both errors repeated into briefs, now fixed: (1) the 404 is a NEW CLASSIFICATION, never a wire transition -- 14eafd9 ADDED the callback (accept.go is a new file) and no shipped binary has ever served internal/relay, so no build emitted a previous 503 for this to replace; CONTRACTS-HTTP.md:820 states this explicitly. (2) the row is NOT REACHABLE on main because the routes are NOT COMPOSED, not because they are not mounted -- ed77bba mounts them; the only cmd/ reference to PeerSurface/PeerPrincipals is a test file (confirmed by grep); RELAY-24 is the composition root that is still missing. CONTRACTS-HTTP.md:809 now says exactly this.

## Description

FEDERATION phase, wave 3. Deps: RELAY-20 (peer routes mounted).

The AcceptRelay callback: roster-check local recipients BEFORE the durable write, then re-forward
ONLY on OutcomeNew. Consumes cca64afd (do not duplicate, relate): "RELAY precondition: roster-check
LOCAL recipients before the durable write, or a peer can permanently exhaust an agent name."​

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [RELAY-20](../RELAY-20--701dc54d/task.md)
- **blocked by** [RELAY-22](../RELAY-22--b4e45cda/task.md)
- **blocks** [RELAY-24](../RELAY-24--e303c624/task.md)
- **relates to** [cca64afd-f75d-46e4-91ca-ebc502151253](../RELAY-precondition-roster-check-LOCAL-recipients-before--cca64afd/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [cca64afd-f75d-46e4-91ca-ebc502151253](../RELAY-precondition-roster-check-LOCAL-recipients-before--cca64afd/task.md) — RELAY precondition: roster-check LOCAL recipients before the durable write, or a peer can… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [06ac5885-5df4-4fab-8b51-45b37c7a38c2](../CONTRACTS-ONDISK.md-document-the-bus_path-len-1-is-recor--06ac5885/task.md) — CONTRACTS-ONDISK.md: document the bus_path\[len-1\]-is-recording-bus on-disk invariant, and… (todo)
- [2ca053dd-1b63-42b5-a485-f57b623722ac](../internal-relay-guards_test.go-912-says-the-RELAY-6-subst--2ca053dd/task.md) — internal/relay/guards_test.go:912 says the RELAY-6 substitution 'IS NOT RECORDED IN DECIS… (todo)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [RELAY-11-FU-INGEST-LOOPGUARD](../RELAY-11-FU-INGEST-LOOPGUARD--a41c273c/task.md) — Relay ingest MUST route through relay.CheckIncomingPath before hub.publish, or a 64-hop l… (todo)
- [RELAY-21-FU-DOCGAP4](../RELAY-21-FU-DOCGAP4--9972d0ed/task.md) — RELAY-21-FU-DOCGAP4: internal/relay/doc.go known-gaps item 4 falsely claims forward-only-… (todo)
- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-24-BLOCKER-HUBINGEST](../RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) — RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sen… (done)
- [RELAY-24-FU-STOREMSGLOOKUP](../RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) — RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMess… (todo)
- [RELAY-45](../RELAY-45--4be32336/task.md) — RELAY-45: Bind inbound peer TLS certificate to the adjacent bus principal (done)
- [RELAY-45-FU-ROTATION](../RELAY-45-FU-ROTATION--ec1c1d7c/task.md) — RELAY-45-FU-ROTATION: inbound peer client-certificate binding has no rollover overlap win… (todo)
- [RELAY-46](../RELAY-46--eb5c3312/task.md) — RELAY-46: NextHopTLSCertFingerprint should be a bounded list, not a scalar, for peer-cert… (todo)
- [RELAY-FU-DOCGO-CROSSBUSTRUST-STALE](../RELAY-FU-DOCGO-CROSSBUSTRUST-STALE--4988156c/task.md) — internal/relay/doc.go asserts relay ingest is structurally blocked (no CrossBusTrust impl… (todo)
- [c716f8e7-ad9c-4af9-9fac-1bdb75c8f900](../../DOCS/PROTOCOL.md-1002-says-internal-relay-is-imported-by-noth--c716f8e7/task.md) — PROTOCOL.md:1002 says internal/relay is 'imported by nothing' -- false since ed77bba (int… (todo)
- [fbb16f9b-1b81-4fd0-a60f-5b2a76806bff](../internal-httpapi-peermount.go-pre-auth-prober-does-not-e--fbb16f9b/task.md) — internal/httpapi/peermount.go: 'pre-auth prober does not exist' overstates ruling (h), an… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
