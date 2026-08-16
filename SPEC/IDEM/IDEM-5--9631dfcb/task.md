# IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconnect the offending client

| Field | Value |
| --- | --- |
| Public id | `9631dfcb-9866-44a2-9deb-1ba3c6b3966d` |
| Key | IDEM-5 |
| Epic | [IDEM](../epic.md) |
| Status | superseded |
| Priority | P1 |
| Component | security |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:14:14.887705+00:00 |
| Updated | 2026-08-02T13:17:02.587321+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestKeyReuseDifferentPayloadDisconnects ./internal/httpapi
```

## Status note

DUPLICATE-EPIC MERGE, 2026-08-02. Two spec-keeper agents were dispatched to file the IDEM epic (invariant 10) concurrently and both did: IDEM-1..9 (this set, keys reserved 13:05) and IDEM-10..17 (created 13:10). The task-key RESERVATION namespace did its job -- no two tasks share a key -- but reservations cannot prevent two agents writing the same EPIC, and 17 overlapping claimable tasks would have had two agents independently implementing the same applied-key store. Resolved by superseding IDEM-1..9 and keeping IDEM-10..17, for three reasons: (a) IDEM-10..17 gate more precisely against existing keys (DUR-1/2/3/6, AUTH-1/4, MSG-2/3, RELAY-1/2/3); (b) they escalate the durable applied-key store and its crash-injection test to P0 with an argument that matches why DUR-1/DUR-2/DUR-6 are P0; (c) the alternative -- cancelling another agent's tasks -- was outside this pass's ownership, whereas IDEM-1..9 are this pass's own to withdraw. Nothing was lost: the unique content of this task is preserved either in its counterpart or as an append-only note on it. SUPERSEDED BY: IDEM-14 (violation path: reject, log as security, disconnect).

## Description

GATED on IDEM-1 (payload fingerprint) and IDEM-2 (stored fingerprint). This is the case the user's instruction targeted: a client reusing an idempotency key for DIFFERENT content is either a serious bug or an attack -- it is trying to make the server believe new content was already-acked content, or to suppress a message by pre-burning its key. REJECT it with a distinct, unambiguous error code (not the generic validation error -- an operator must be able to grep for this), LOG it at a level a human actually sees, with the caller identity, the operation, the key and both fingerprints, and DISCONNECT the offending client. DEFINE 'DISCONNECT' CONCRETELY, because on an HTTP server it is not obvious: at minimum close the connection without keep-alive reuse. DECIDE AND JUSTIFY THE BLAST RADIUS -- does it also invalidate the token / revoke enrolment (AUTH-4), or only drop the connection? The user asked for a disconnect; the choice between 'drop the TCP connection' and 'evict the agent' is a real security/availability trade-off and belongs in DECISIONS.md, not in an implementer's head. THE LINE THAT MUST NOT BE CROSSED: this path must NEVER fire for same-key-same-payload (IDEM-4). Getting that backwards turns a correctness feature into an outage for well-behaved clients, so both directions get their own named test. INTERACTIONS: (a) a disconnected long-poll client (POLL-1) will reconnect immediately -- make sure the rejection is sticky enough not to become a self-inflicted reconnect storm, or is cheap enough not to matter; say which. (b) Replay of an already-accepted SIGNED message by a peer or third party is the related-but-distinct case in invariant 10 -- it is rejected and disconnects the sender too, but its freshness check is SIGN-4's sequence+cursor, not the fingerprint; keep the two paths distinct and cross-reference them rather than merging them.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [AUTH-4](../../AUTH/AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (todo)
- [DUR-1](../../DUR/DUR-1--c51e1959/task.md) — DUR-1: WAL record framing + writer (done)
- [DUR-2](../../DUR/DUR-2--4132b879/task.md) — DUR-2: Two-phase prepare-&gt;commit write path (done)
- [DUR-6](../../DUR/DUR-6--d56a997d/task.md) — DUR-6: Crash-injection test suite for the write path (done)
- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-4](../IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [POLL-1](../../POLL/POLL-1--1b0635b9/task.md) — POLL-1: GET /v1/wait -- long-poll endpoint (done)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)
- [SIGN-4](../../SIGN/SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side seque… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-4](../IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [IDEM-6](../IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)
- [IDEM-8](../IDEM-8--d1ecfc75/task.md) — IDEM-8: Proof suite -- a retried send produces exactly one message, including across a cr… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
