# IDEM-13: Idempotent enrol / leave / peer-enrol

| Field | Value |
| --- | --- |
| Public id | `a869264d-cbb3-41e1-9d6c-8771fd3f6b57` |
| Key | IDEM-13 |
| Epic | [IDEM](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | core |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:10:35.025400+00:00 |
| Updated | 2026-08-02T13:24:05.506429+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestIdempotentEnrol ./internal/auth/... ./internal/relay/...
```

## Description

GATED on IDEM-10, IDEM-11, AUTH-1 (POST /v1/enroll), AUTH-4 (POST /v1/leave) and RELAY-1 (peer enrolment). Extends the IDEM-12 discipline to the non-messaging mutating operations invariant 10 names explicitly: enrol, leave, peer-enrol. Same-key-same-request-shape returns the original result rather than erroring or re-minting -- e.g. re-presenting the same enrolment public key with the same idempotency key after a lost ack returns the SAME signed credential/token, not a second one and not an 'already enrolled' error that would force the agent down a spurious re-enrolment path. Same-key-different-content is a violation and is IDEM-14's job, not this task's. Each of the three routes has its own notion of 'same request' worth being explicit about in CONTRACTS.md: enrol's identity is the presented public key; leave's is the agent being revoked; peer-enrol's is the peer bus id plus its offered credential. Because enrol issues a signed credential (invariant 3), pay particular attention to NOT minting a second valid token for a retried enrol -- a client holding two live tokens for one identity is a small security smell worth avoiding even when neither token is individually wrong. Document each route's comparison basis in CONTRACTS.md alongside its existing route entry.

--- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-6, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) WHY A DOUBLE-APPLIED ENROL IS WORSE THAN A DUPLICATE MESSAGE: ids are never reused (invariant 1), so minting a second agent burns an id permanently and leaves a PHANTOM agent in the roster that nothing will ever collect, while the client ends up holding a credential for an identity its peers were never told about. (b) THE UNAUTHENTICATED SCOPE: enrol has no authenticated caller yet, so it uses the alternative key scope IDEM-10 settles (the presented enrolment public key, or bus-wide) -- implement exactly that, and ensure it cannot be used by an unauthenticated caller to squat or probe another party's keys. (c) RE-ENROLMENT WITH A DIFFERENT PUBLIC KEY under the same idempotency key is a different-payload VIOLATION (IDEM-14), not a retry -- important, because that is precisely how an attacker would attempt an identity takeover. (d) LEAVE MUST NOT DOUBLE-APPLY ITS SIDE EFFECTS: return success (not an error) on a second call, and do not repeat revocation side effects -- notably CRYPTO-4's key_epoch bump, where a second bump needlessly invalidates freshly-issued bundles. (e) PEER-ENROL MUST CONVERGE: two buses enrolling each other concurrently, and a peer retrying after a timeout, must end up with ONE peering, not two half-configured ones. (f) All three operations persist their applied-key records through IDEM-11's store so they survive restart, and all three must still behave after roster recovery (AUTH-3). PRIORITY NOTE: kept at P1 (the withdrawn counterpart was P2); the merge preserves the STRONGER priority of the two batches, never the weaker.

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
- [AUTH-3](../../AUTH/AUTH-3--d53e3b21/task.md) — AUTH-3: Roster persistence & recovery (done)
- [AUTH-4](../../AUTH/AUTH-4--a853261d/task.md) — AUTH-4: POST /v1/leave -- leave / revocation (todo)
- [CRYPTO-4](../../CRYPTO/CRYPTO-4--13f3947e/task.md) — CRYPTO-4: Key-distribution endpoint -- server-attested messaging key bundles (todo)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (todo)
- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-6](../IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)
- [RELAY-1](../../RELAY/RELAY-1--9bc9d6c4/task.md) — RELAY-1: Peer enrolment + initial agent-list exchange (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-1](../../AUTH/AUTH-1--54fa94c0/task.md) — AUTH-1: POST /v1/enroll -- signed credential issuance (done)
- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11-FU-HUBAPPLY](../IDEM-11-FU-HUBAPPLY--a9f827b9/task.md) — IDEM-11-FU-HUBAPPLY: hub.Apply returns early for non-message Entry.Kind, so IDEM-13/14/15… (todo)
- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)
- [IDEM-6](../IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
