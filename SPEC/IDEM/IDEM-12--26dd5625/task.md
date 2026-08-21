# IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence, no second audit record

| Field | Value |
| --- | --- |
| Public id | `26dd5625-ddb2-4b68-a114-dadc1c5364b0` |
| Key | IDEM-12 |
| Epic | [IDEM](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | core |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:10:34.806392+00:00 |
| Updated | 2026-08-02T13:24:04.839176+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestIdempotentSend ./internal/hub/... ./internal/httpapi/... ; then, against a throwaway bus with its own data dir under /tmp, the same scripts/bus-send.sh call issued TWICE with one idempotency key returns the SAME message id and sequence both times
```

## Description

GATED on IDEM-10, IDEM-11, MSG-2 (POST /v1/broadcast) and MSG-3 (POST /v1/send). Wire the idempotency key into both routes: on a request whose (agent, key) already has an applied-key record, look it up (IDEM-11) BEFORE doing any normal send work. THE CARVE-OUT THAT MUST NOT BE COLLAPSED (state this in the code comments and the task's own tests, not just in this description): same key + SAME payload is a LEGITIMATE RETRY -- the ack was probably lost in flight. Return the ORIGINAL message id and sequence number verbatim, allocate NO new sequence (invariant 1: sequences are server-minted and never duplicated for one logical operation), write NO second record to the append-only audit log (invariant 6 -- a retry must not create a phantom second entry for what is, from the audit trail's point of view, one logical send), do NOT return an error, and do NOT disconnect the client. This is the entire point of idempotency: punishing a well-behaved retrying client breaks exactly the client doing the right thing. ONLY same key + DIFFERENT payload is a violation, and that path is IDEM-14's job, not this task's -- this task implements the happy path only. 'Same payload' comparison MUST be exact/content-addressed (e.g. compare a hash of the canonical request body), not fuzzily approximated. This task's own narrow test must show: a same-key-same-payload retry of both /v1/send and /v1/broadcast returns identical id+sequence on the second call, and the audit log gains no second entry for it. Broader exactly-once coverage (retry storms, concurrency) lives in IDEM-16/IDEM-17, not here.

--- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-4, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) THE CONCURRENT IN-FLIGHT CASE, which is where implementations usually break: two requests with the same key arrive concurrently because the client retried before the first ack landed, so the first operation is committed-in-progress and there is NO stored result yet. A naive check-then-act double-applies. Handle it with a single-flight reservation on the key taken inside the SAME critical section that mints the sequence: the second caller either blocks and then returns the first's result, or receives an explicitly retriable 'in progress' response -- pick one and document it. (b) MARK A REPLAYED ACK: give the caller a way to tell a replayed ack from a fresh one (a response field or header) for debugging and for the wrapper's logging -- but the rest of the body must be byte-identical to the original. (c) BROADCAST DEDUPES ON THE OPERATION, NOT ON PER-RECIPIENT DELIVERY: a retried broadcast must not fan out a second time to ANYONE, including recipients whose delivery failed on the first attempt. (d) SIGN-6 INTERACTION: a message rejected for a missing or invalid signature was NEVER applied, so its key must not be recorded as applied -- a corrected resend under the same key is a new operation, not a retry. State this explicitly, or an implementer will record keys before validation and permanently burn them.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)
- [IDEM-17](../IDEM-17--8b1e85fd/task.md) — IDEM-17: Crash-injection test -- restart mid-retry-window still yields exactly one effect (done)
- [IDEM-4](../IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [MSG-2](../../MSG/MSG-2--50995c75/task.md) — MSG-2: POST /v1/broadcast (done)
- [MSG-3](../../MSG/MSG-3--2655c6ae/task.md) — MSG-3: POST /v1/send -- direct message (done)
- [SIGN-6](../../SIGN/SIGN-6--c9e4aea1/task.md) — SIGN-6: A signature is MANDATORY on the wire -- ingest policy and fail-closed handling of… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-10](../IDEM-10--b28e5153/task.md) — IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent (done)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [IDEM-13](../IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-15](../IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [IDEM-16](../IDEM-16--b6b76aeb/task.md) — IDEM-16: Exactly-once test suite -- retry storm, concurrent race under -race, and key-reu… (todo)
- [IDEM-18](../IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (todo)
- [IDEM-4](../IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [MSG-5](../../MSG/MSG-5--9d125bc6/task.md) — MSG-5: Messaging durability integration test (done)
- [RELAY-16-FU-RETRY404](../../RELAY/RELAY-16-FU-RETRY404--7f515d76/task.md) — RELAY-16-FU-RETRY404: retry of an already-committed send can 404 if the recipient stopped… (todo)
- [RELAY-47-FU-IDEMFINGERPRINT](../../RELAY/RELAY-47-FU-IDEMFINGERPRINT--b666cd5a/task.md) — RELAY-47-FU-IDEMFINGERPRINT: the ENFORCED idempotency fingerprint is not the one internal… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
