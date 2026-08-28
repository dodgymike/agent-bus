# MTLS-ROTATE: a client accepts a SET of pinned bus certificates so a rotation does not force every agent to re-enrol

| Field | Value |
| --- | --- |
| Public id | `c2e8df5b-cafa-4a38-8384-a99e7f66f968` |
| Key | MTLS-ROTATE |
| Epic | [MTLS](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-07T18:14:16.660637+00:00 |
| Updated | 2026-08-07T19:55:24.516868+00:00 |
| Completed | 2026-08-07T19:55:24.516850+00:00 |

## Proof command

```sh
go test -race -run 'TestClientAcceptsEitherPinnedCertificateDuringRotation|TestPinIsNeverLearnedFromAHandshake' ./client/...
```

## Description

Raised by the security gate on MTLS-PIN (2026-08-07), MED-2. GATES MTLS-LISTENER: do not ship the TLS listener before this.

MTLS-PIN stores ONE bus certificate fingerprint per identity (client.Identity.BusFingerprint), and the only recovery from a changed certificate is `agent-busctl logout` + re-enrol. That directly contradicts recorded decision E3 in DECISIONS.md: the bus serves TWO certificates during rollover 'so no client is ever forced to re-enrol on routine rotation'.

It is harmless today because no bus serves TLS and rotation has no implementation. It stops being harmless the moment MTLS-LISTENER ships: the first routine rotation wedges every enrolled agent at once, and a wedged fleet is precisely the pressure under which somebody argues for letting --bus-fingerprint override the stored pin. MTLS-PIN deliberately refuses that override (DECISIONS.md 2026-08-07 §4) because it converts a DETECTED certificate substitution into an accepted one. So the pressure must be removed by making rotation work, not by weakening the check.

Shape of the fix: the stored identity holds a SET of accepted fingerprints, not one. A pin enters the set ONLY by explicit deliberate operator action (a new invite, or an explicit re-pin command) -- NEVER learned from a handshake, which would be trust-on-first-use by another name. Retiring an old pin must also be explicit. Consider what `whoami` shows when more than one is pinned.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-PIN](../MTLS-PIN--8c46dc93/task.md) — MTLS-PIN: the client PINS the bus's certificate fingerprint and hard-fails on a change --… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0ba2372a-09f7-4f05-bd33-98a5f80e0e6f](../../DOCS/Journal-catch-up-DECISIONS.md-AGENT_LOG.md-entries-owed--0ba2372a/task.md) — Journal catch-up: DECISIONS.md + AGENT_LOG.md entries owed by INVITE-MINT and MTLS-ROTATE (todo)
- [2cf20abf-b209-4829-bac1-bda07ddd9ed5](../../CLI/client.canonicalHost-drops-IPv6-brackets-when-removing-a--2cf20abf/task.md) — client.canonicalHost drops IPv6 brackets when removing a default port (done)
- [6b44ee89-612a-4d3d-9c39-1302c07d3c39](../../DOCS/AGENT_PROTOCOL.md-error-block-label-says-remedy-but-the--6b44ee89/task.md) — AGENT_PROTOCOL.md error-block label says remedy: but the CLI prints try: (todo)
- [767f0cd9-beaa-4d5f-a260-44e7681891fc](../CONTRACTS-ONDISK.md-document-the-client-side-identities--767f0cd9/task.md) — CONTRACTS-ONDISK.md: document the client-side identities.json format and the bus_fingerpr… (todo)
- [CONTEXT-LOG-RETIRE](../../CONTEXT/CONTEXT-LOG-RETIRE--116179c8/task.md) — CONTEXT-LOG-RETIRE: AGENT_LOG.md freezes its narrative and moves to one line per task (todo)
- [MTLS-EXPIRY](../MTLS-EXPIRY--3604af80/task.md) — MTLS-EXPIRY: the client never checks the pinned bus certificate validity period -- the 36… (done)
- [MTLS-LISTENER](../MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (done)
- [MTLS-MIGRATE](../MTLS-MIGRATE--59883178/task.md) — MTLS-MIGRATE: pin add cannot migrate a pre-TLS (http-enrolled) identity onto TLS; its own… (done)
- [MTLS-ROTATE-FU-SERVERSIDE](../MTLS-ROTATE-FU-SERVERSIDE--b624915b/task.md) — MTLS-ROTATE-FU-SERVERSIDE: the bus serves ONE certificate, so DECISIONS.md E3's two-certi… (todo)
- [bd662bae-4c6c-426d-a736-7830d2d21037](../parseBusURL-does-not-canonicalise-redundant-path-slashes--bd662bae/task.md) — parseBusURL does not canonicalise redundant path slashes/segments, so a differently-spell… (done)
- [cbfb7d88-1bb0-4ade-b1d1-f287b4c0c179](../../PROCESS/Triage-dispatched-two-concurrent-agents-with-overlapping--cbfb7d88/task.md) — Triage dispatched two concurrent agents with overlapping ownership of CONTRACTS-CLI.md (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
