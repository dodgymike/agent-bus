# TRIAGE-LOCK: backlog-triage mutex

| Field | Value |
| --- | --- |
| Public id | `25f0eac6-9522-433e-96b5-4217226599c3` |
| Key | TRIAGE-LOCK |
| Epic | [PROCESS](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | process |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T12:58:23.855518+00:00 |
| Updated | 2026-08-08T10:29:58.019003+00:00 |
| Completed | 2026-08-02T14:06:09.392131+00:00 |

## Proof command

```sh
n/a - process lock
```

## Status note

released by triage-20260807T1815-tls-next (FINAL). Pass complete, tree quiet, HEAD 51a99ba. SHIPPED: d0dadc0 INVITE-MINT (P0, proof PASS 36 tests) + 29cdafc MTLS-ROTATE code + 51a99ba AGENT_PROTOCOL.md; both tasks now DONE with real commit_sha. Integrator REFUSED the doc commit once over two real doc-vs-code drifts (spliced pinError example that no code path emits; never-null guarantee wrongly attached to enrol/whoami/use which are omitempty) -- both fixed before landing. Filed this pass: 3604af80 MTLS-EXPIRY (P1, breaks the MTLS-VERIFY<->MTLS-LISTENER dependency cycle), bd662bae parseBusURL canonicalisation (P1, raised from the security gates P2 on the invariant-10 idempotency-scope-key angle), cbfb7d88 (P2, triage error: CONTRACTS-CLI.md granted to two agents), 2582f548 (P2, /export drops commits[]), e109c867 (P2, PATCH rejects key with 422), 767f0cd9 (P2, CONTRACTS-ONDISK lacks identities.json), 0ba2372a (P2, journal catch-up), 6b44ee89 (P3, remedy: vs try:). NEXT PASS: dispatch MTLS-EXPIRY 3604af80 FIRST -- client/pin.go is now free and it needs no consent. MTLS-LISTENER 17e70a7e remains the head of the TLS chain and is STILL BLOCKED ON USER CONSENT (breaking change, asked three times, unanswered). Do NOT dispatch it without an answer. Only SPEC.md remains uncommitted (spec-keeper mirror).

## Description

Reusable mutex. in_progress = held, done = free. Whoever holds it is the only agent allowed to dispatch from the backlog. Acquire = compare-and-set on status via If-Match: "v<version>"; on 412 you lost the race, STOP, do not retry. Release = PATCH {status:done}. Do NOT use /complete on it. NEVER delete this task. Holder identity lives in status_note, never here.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0ba2372a-09f7-4f05-bd33-98a5f80e0e6f](../../DOCS/Journal-catch-up-DECISIONS.md-AGENT_LOG.md-entries-owed--0ba2372a/task.md) — Journal catch-up: DECISIONS.md + AGENT_LOG.md entries owed by INVITE-MINT and MTLS-ROTATE (todo)
- [2582f548-6493-439c-ba71-7f5cf73650fc](../Spec-Server-export-both-format-markdown-and-format-json--2582f548/task.md) — Spec Server /export (both format=markdown and format=json) silently drops the commits\[\] a… (todo)
- [6b44ee89-612a-4d3d-9c39-1302c07d3c39](../../DOCS/AGENT_PROTOCOL.md-error-block-label-says-remedy-but-the--6b44ee89/task.md) — AGENT_PROTOCOL.md error-block label says remedy: but the CLI prints try: (todo)
- [767f0cd9-beaa-4d5f-a260-44e7681891fc](../../MTLS/CONTRACTS-ONDISK.md-document-the-client-side-identities--767f0cd9/task.md) — CONTRACTS-ONDISK.md: document the client-side identities.json format and the bus_fingerpr… (todo)
- [INVITE-MINT](../../INVITE/INVITE-MINT--1d0d0e60/task.md) — INVITE-MINT: an operator mints a single-use, expiring invite -- the server is authoritati… (done)
- [MTLS-EXPIRY](../../MTLS/MTLS-EXPIRY--3604af80/task.md) — MTLS-EXPIRY: the client never checks the pinned bus certificate validity period -- the 36… (done)
- [MTLS-LISTENER](../../MTLS/MTLS-LISTENER--17e70a7e/task.md) — MTLS-LISTENER: serve TLS ONLY and REFUSE TO START without a usable cert/key -- there is n… (in_progress)
- [MTLS-ROTATE](../../MTLS/MTLS-ROTATE--c2e8df5b/task.md) — MTLS-ROTATE: a client accepts a SET of pinned bus certificates so a rotation does not for… (done)
- [MTLS-VERIFY](../../MTLS/MTLS-VERIFY--9dab7303/task.md) — MTLS-VERIFY: fix scripts/bus-serve.sh's plaintext health probe AND prove a RUNNING bus is… (in_progress)
- [bd662bae-4c6c-426d-a736-7830d2d21037](../../MTLS/parseBusURL-does-not-canonicalise-redundant-path-slashes--bd662bae/task.md) — parseBusURL does not canonicalise redundant path slashes/segments, so a differently-spell… (todo)
- [cbfb7d88-1bb0-4ade-b1d1-f287b4c0c179](../Triage-dispatched-two-concurrent-agents-with-overlapping--cbfb7d88/task.md) — Triage dispatched two concurrent agents with overlapping ownership of CONTRACTS-CLI.md (todo)
- [e109c867-fcd2-4ddc-bc4d-55779dc5f5e1](../Spec-Server-PATCH-tasks-id-rejects-the-key-field-outrigh--e109c867/task.md) — Spec Server: PATCH /tasks/{id} rejects the key field outright (422 Unknown field) -- a ke… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
