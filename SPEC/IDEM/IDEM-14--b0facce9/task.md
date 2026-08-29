# IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs as security, and disconnects

| Field | Value |
| --- | --- |
| Public id | `b0facce9-ddc0-40f2-86f3-cc2c2bb76a79` |
| Key | IDEM-14 |
| Epic | [IDEM](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | core |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:10:35.246183+00:00 |
| Updated | 2026-08-14T20:18:42.257232+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestIdempotencyViolation ./internal/...
```

## Description

GATED on IDEM-10, IDEM-11, and at least one of IDEM-12/IDEM-13 landing first (the happy path must exist before the violation path can be distinguished from it). Implements invariant 10's violation clause: when a client reuses an (agent, key) pair the applied-key store (IDEM-11) already has a record for, but the NEW request's payload does NOT match the original, the server must (1) REJECT the request, (2) log it as a SECURITY event -- not a routine 4xx; same severity class as an auth failure, discoverable the way the security agent expects to find things -- and (3) DISCONNECT the offending client. THE CARVE-OUT THAT MUST NOT BE COLLAPSED (restate it here explicitly, do not assume the reader has IDEM-12's copy in front of them): this path fires ONLY for same-key-DIFFERENT-payload. Same-key-SAME-payload is IDEM-12/IDEM-13's legitimate-retry path and must NEVER reach this code -- an implementation that disconnects on every duplicate key regardless of payload is WRONG and will disconnect well-behaved retrying clients, precisely the bug invariant 10's text calls out by name. TWO DECISIONS THIS TASK MUST PIN DOWN and record in DECISIONS.md, because CLAUDE.md's invariant 10 text leaves them open: (a) the EXACT HTTP status code returned for the rejected request (409 Conflict is the natural fit for 'conflicts with a prior request under this key' -- pick and justify one, don't reuse a generic 400); (b) whether 'disconnect' means merely dropping the current connection/long-poll (the agent can reconnect and retry with a fresh key) or FULL CREDENTIAL REVOCATION requiring re-enrolment (the agent's AUTH-1 token is invalidated, same blast radius as AUTH-4's leave path) -- these have very different consequences and the choice must be deliberate, not whichever was easiest to wire up. Also applies conceptually to 'replay of an already-accepted signed message' per invariant 10's third bullet -- SIGN-4/SIGN-5 own the signature-replay detection mechanics; this task's reject/log/disconnect plumbing is the natural place that behaviour hooks into, so cross-reference SIGN-4 rather than building a second, divergent disconnect path.

--- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-5, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) DEFINE 'DISCONNECT' CONCRETELY -- on an HTTP server it is not self-evident: at minimum, close the connection without keep-alive reuse. That is the MECHANICS, a separate axis from the blast-radius decision this task already carries (drop the connection vs revoke the credential); both must be written down. (b) THE ERROR MUST BE GREPPABLE: a distinct code, not the generic validation error, plus a log line an operator actually sees carrying the caller identity, the operation, the key, and BOTH payload fingerprints (the stored one and the offending one). (c) DO NOT CREATE A SELF-INFLICTED RECONNECT STORM: a disconnected long-poll client (POLL-1) reconnects immediately, so the rejection must be either sticky enough to stop the loop or cheap enough not to matter -- say which. (d) KEEP THE SIGNED-REPLAY PATH DISTINCT: replay of an already-accepted SIGNED message also rejects and disconnects. AMENDED 2026-08-14 by SIGN-1-FU-REORDER-WATERMARK: that freshness check is enforced SERVER-SIDE AT INGEST by SIGN-4 -- the bus refuses an already-accepted signed message before it is ever served. It is NOT a recipient-side sequence+cursor check (SIGN-4's prior wording specified exactly that defect and has been corrected); do not build a recipient-side rejection-by-sequence path here or anywhere else. This is also distinct from the payload fingerprint used above. Reuse this task's reject/log/disconnect plumbing, but do not merge the two detectors into one path -- cross-reference them instead. (e) BOTH DIRECTIONS GET THEIR OWN NAMED TEST: it fires on same-key-different-payload, and it provably does NOT fire on same-key-same-payload. Getting that backwards turns a correctness feature into an outage for exactly the well-behaved clients that retry correctly.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [AUTH-4](../../AUTH/AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (done)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (in_progress)
- [IDEM-13](../IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-5](../IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [POLL-1](../../POLL/POLL-1--1b0635b9/task.md) — POLL-1: GET /v1/wait -- long-poll endpoint (done)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)
- [SIGN-4](../../SIGN/SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side seque… (todo)
- [SIGN-5](../../SIGN/SIGN-5--5cedc580/task.md) — SIGN-5: MANDATORY negative-test suite -- prove the verifier rejects everything it must (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11-FU-HUBAPPLY](../IDEM-11-FU-HUBAPPLY--a9f827b9/task.md) — IDEM-11-FU-HUBAPPLY: hub.Apply returns early for non-message Entry.Kind, so IDEM-13/14/15… (todo)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (in_progress)
- [IDEM-13](../IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-14-FU-CLIENTTEXT](../IDEM-14-FU-CLIENTTEXT--30a9e4f6/task.md) — IDEM-14-FU-CLIENTTEXT: client remedy text (messages.go:1175) asserts a server disconnect… (done)
- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)
- [IDEM-18](../IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (in_progress)
- [IDEM-5](../IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [RELAY-47-FU-IDEMFINGERPRINT](../../RELAY/RELAY-47-FU-IDEMFINGERPRINT--b666cd5a/task.md) — RELAY-47-FU-IDEMFINGERPRINT: the ENFORCED idempotency fingerprint is not the one internal… (todo)
- [SWEEP-TWO-PASS-DISCIPLINE](../../PROCESS/SWEEP-TWO-PASS-DISCIPLINE--268a0c73/task.md) — SWEEP-TWO-PASS-DISCIPLINE: a contract change needs a MECHANISM sweep and a PROSE sweep, n… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
