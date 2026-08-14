# MTLS-EXPIRY: the client never checks the pinned bus certificate validity period -- the 365-day lifetime is unenforced

| Field | Value |
| --- | --- |
| Public id | `3604af80-35a0-4007-818e-ef309fdeaf0c` |
| Key | _(null in the export)_ |
| Epic | [MTLS](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-07T18:59:38.863939+00:00 |
| Updated | 2026-08-07T20:51:22.668341+00:00 |
| Completed | 2026-08-07T20:51:22.668324+00:00 |

## Proof command

```sh
go test -race -run 'TestExpiredBusCertificateIsRejectedDespiteMatchingPin|TestNotYetValidBusCertificateIsRejected' ./client/...
```

## Description

EPIC: a1b628fb-8cbf-47e8-9682-034fda8636c7

client/pin.go verifies sha256-of-DER only (verifyPinnedBusCertificate, pinVerifier, pinnedTLSConfig which sets InsecureSkipVerify: true), and disabling the default chain check disables NotBefore/NotAfter along with it. The MTLS-PIN security gate demonstrated empirically that a certificate whose NotAfter is a day in the past is pinned, accepted, and enrolled against. DECISIONS.md chose the 365-day bus certificate lifetime explicitly as a leak-containment bound, so today that bound is pure decoration -- nothing on the client enforces it.

This was folded into MTLS-VERIFY (9dab7303) only as a status_note, but MTLS-VERIFY currently DEPENDS ON MTLS-LISTENER while MTLS-LISTENER is GATED ON MTLS-VERIFY -- a genuine circular dependency (see MTLS-LISTENER status_note). Splitting the pure client-side expiry check out of MTLS-VERIFY breaks that cycle, because the expiry check needs no running TLS bus -- it is a unit-testable property of client/pin.go alone (construct an expired/not-yet-valid self-signed cert in-memory, matching pin, assert rejection).

FILE CONFLICT, sequencing requirement: this lands in client/pin.go, which MTLS-ROTATE owns this pass (dispatched 2026-08-07). Must be sequenced AFTER MTLS-ROTATE, not run in parallel with it.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (in_progress)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)
- [MTLS-ROTATE](../MTLS-ROTATE--c2e8df5b/task.md) — MTLS-ROTATE: a client accepts a SET of pinned bus certificates so a rotation does not for… (done)
- [MTLS-VERIFY](../MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is… (in_progress)
- [a1b628fb-8cbf-47e8-9682-034fda8636c7](../EPIC-mutual-TLS-with-self-signed-certs-no-CA-required-tr--a1b628fb/task.md) — EPIC: mutual TLS with self-signed certs, no CA -- required transport, no plaintext listen… (superseded)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [083c468e-7dbd-4d1f-93fc-53617e28421f](../CONTRACTS-CLI.md-client-export-table-is-missing-the-thre--083c468e/task.md) — CONTRACTS-CLI.md client export table is missing the three symbols MTLS-EXPIRY added (todo)
- [10e93262-8e34-4738-b435-bfe23d880057](../Derive-the-bus-fingerprint-from-the-certificate-not-the--10e93262/task.md) — Derive the bus fingerprint from the certificate, not the log; correct the CONTRACTS-CLI e… (in_progress)
- [3cb182dc-9bd2-489b-8a91-0d8529f77200](../No-behavioural-test-asserts-that-a-resumed-TLS-handshake--3cb182dc/task.md) — No behavioural test asserts that a resumed TLS handshake still re-verifies the pinned bus… (todo)
- [51710f76-ea92-42fd-bbc3-b86415fbc8e1](../../CLI/Latent-data-race-in-cmd-agent-busctl-enrol_test.go-serve--51710f76/task.md) — Latent data race in cmd/agent-busctl/enrol_test.go: server stderr buffer is read while os… (done)
- [CONTEXT-CLI-SECTIONS](../../CONTEXT/CONTEXT-CLI-SECTIONS--3b4bd434/task.md) — CONTEXT-CLI-SECTIONS: CONTRACTS-CLI.md's 857-line mega-section becomes real, range-readab… (todo)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (in_progress)
- [MTLS-LISTENER-FU-CLIENTHTTP](../MTLS-LISTENER-FU-CLIENTHTTP--8d906b8b/task.md) — MTLS-LISTENER-FU-CLIENTHTTP: client/config.go still allows unpinned http:// to loopback,… (todo)
- [TRIAGE-LOCK](../../PROCESS/TRIAGE-LOCK--25f0eac6/task.md) — TRIAGE-LOCK: backlog-triage mutex (done)
- [efc7facd-ac16-4a17-a3af-c0c3b69c72ae](../Config.HTTPClient-lets-an-embedder-bypass-certificate-pi--efc7facd/task.md) — Config.HTTPClient lets an embedder bypass certificate pinning entirely (todo)
- [ff5ca3f9-c100-4116-ab25-a62f74c0d066](../client-doc.go-package-documentation-does-not-mention-tha--ff5ca3f9/task.md) — client/doc.go package documentation does not mention that the pinned certificate's validi… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
