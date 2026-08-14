# IDEM-10: Idempotency key -- format, client-supplied untrusted validation, scoped per-agent

| Field | Value |
| --- | --- |
| Public id | `b28e5153-e433-4dd8-9f5a-342ad978d322` |
| Key | IDEM-10 |
| Epic | [IDEM](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | core |
| Section | backlog |
| Tags | — |
| Created | 2026-08-02T13:10:34.326361+00:00 |
| Updated | 2026-08-02T23:43:49.519635+00:00 |
| Completed | 2026-08-02T23:43:49.519617+00:00 |

## Proof command

```sh
go test -race -run TestIdempotencyKey ./internal/... && test -z "$("$(go env GOROOT)/bin/gofmt" -l .)"
```

## Status note

DISPATCHED by triage-20260802-b1f-breadth (breadth-first feature pass, user instruction). Boundary is a SINGLE package, disjoint from the in-flight MSG/POLL (hub,store,httpapi) and CLI (client,cmd/busctl) waves.

## Description

Define the idempotency key carried on every mutating request per invariant 10 (CLAUDE.md, 2026-08-02): enrol, send, broadcast, leave, peer-enrol, relay. The key is CLIENT-SUPPLIED and therefore UNTRUSTED input per invariant 1 -- validate it, never trust it. Pick and document an EXACT byte length cap (e.g. <=128 bytes) and an EXACT charset restriction (e.g. printable ASCII or a documented allow-list), and reject any request whose key field would trigger unbounded allocation (over-cap keys are rejected before the rest of the body is read, the same fail-fast discipline AUTH-6 established for the mux). Keys MUST be scoped PER-AGENT: the applied-key lookup this task feeds (IDEM-11) is keyed by (agent id, idempotency key), NEVER by key alone. State explicitly why this matters: without per-agent scoping, one agent could either collide with another agent's key space (corrupting its retry bookkeeping) or PROBE another agent's key space -- 'does key X already exist for some agent?' becomes an oracle leaking information about another agent's traffic, the same class of cross-agent leak invariant 2's <bus-id>.<agent-id> namespacing exists to prevent elsewhere. Deliverable: a written spec (CONTRACTS.md and/or PROTOCOL.md) naming the wire field name, the length cap, the charset, and the per-agent scoping rule, PLUS validation code shared by every mutating handler so the rule cannot be implemented inconsistently route-by-route (a single validateIdempotencyKey(agentID, key) helper, not five copies). BLOCKS IDEM-11 through IDEM-15, which all consume this key shape. No dependency on DUR-3.

--- FOLDED IN FROM THE WITHDRAWN DUPLICATE EPIC (IDEM-1, superseded 2026-08-02), reconciliation pass 2. This content was unique to the withdrawn set and is now in THIS task's scope, not merely offered as a note: (a) TRANSPORT -- pick ONE canonical carrier for the key and use it everywhere; an `Idempotency-Key` request HEADER is the conventional choice, and if it goes in the body instead, say why. One place, never two: a key that can arrive by two routes is a key that will eventually disagree with itself. (b) A MISSING KEY ON A MUTATING ROUTE IS AN ERROR (4xx) and the server MUST NOT mint a substitute per attempt -- silently generating one makes every retry look like a new operation and defeats this entire epic while every server-side test keeps passing. (c) READ-ONLY ROUTES DO NOT TAKE ONE -- name them (/v1/agents, /v1/wait, /v1/messages, /healthz, /v1/info) so the rule is exhaustive in both directions rather than only listing what does require a key. (d) INVARIANT 1, STATED EXPLICITLY: the key is client-supplied input to VALIDATE, and it must NEVER become, seed, or be derivable into a message id, an agent id, or a sequence number -- all of those stay server-minted. (e) THE SCOPE TUPLE SHOULD ALSO CARRY THE OPERATION: the withdrawn task scoped dedupe by (fully-qualified <bus-id>.<agent-id>, OPERATION, key) rather than (agent, key). Decide which and record why -- without the operation component, one agent reusing a key across two different routes collides with itself. (f) ENROLMENT IS THE AWKWARD CASE AND IS SETTLED HERE, not deferred to IDEM-13: enrol has no authenticated caller yet, so its dedupe scope must be something else (the presented enrolment public key, or bus-wide). Decide it in this task, hand IDEM-13 the answer, and make sure the chosen scope cannot be used by an UNAUTHENTICATED caller to squat or probe keys. (g) DEFINE THE PAYLOAD FINGERPRINT HERE: the canonical hash (crypto/sha256, stdlib) that IDEM-11 stores next to the key, pinning EXACTLY which bytes are hashed, the same way SIGN-1 pins its signed bytes. IDEM-12's legitimate-retry path and IDEM-14's violation path both turn on same-payload-vs-different-payload, so an ambiguous fingerprint makes that distinction unreliable in BOTH directions -- it belongs in this contract task, not re-invented per route. Documentation of all of the above lands via IDEM-18 (the agent-facing wrapper + docs task filed by the merge).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [IDEM-18](../IDEM-18--61f80a28/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [AUTH-6](../../AUTH/AUTH-6--1640e0b4/task.md) — AUTH-6: Auth FAIL-OPEN risk -- wrap the mux with auth + an explicit unauthenticated allow… (superseded)
- [DUR-3](../../DUR/DUR-3--d8a991ea/task.md) — DUR-3: Replay/recovery on start (done)
- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (in_progress)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (todo)
- [IDEM-13](../IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-15](../IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [IDEM-18](../IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (in_progress)
- [SIGN-1](../../SIGN/SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-1](../IDEM-1--3cac3349/task.md) — IDEM-1: The idempotency-key contract -- format, transport, scope, and which operations re… (superseded)
- [IDEM-11](../IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (in_progress)
- [IDEM-11-FU-PAPERTRAIL](../../DOCS/IDEM-11-FU-PAPERTRAIL--c416a458/task.md) — IDEM-11-FU-PAPERTRAIL: DECISIONS.md and CONTRACTS-HTTP.md state the OPPOSITE of what IDEM… (todo)
- [IDEM-12](../IDEM-12--26dd5625/task.md) — IDEM-12: Idempotent send/broadcast -- retries return the original result, no new sequence… (todo)
- [IDEM-13](../IDEM-13--a869264d/task.md) — IDEM-13: Idempotent enrol / leave / peer-enrol (todo)
- [IDEM-14](../IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [IDEM-15](../IDEM-15--ab3f48b0/task.md) — IDEM-15: Relay duplicate suppression via idempotency keys (todo)
- [IDEM-18](../IDEM-18--61f80a28/task.md) — IDEM-18: Wrappers generate the idempotency key ONCE and reuse it across retries, + AGENT_… (in_progress)
- [IDEM-2](../IDEM-2--1c6a5ef1/task.md) — IDEM-2: Durable applied-key store -- committed in the SAME two-phase transaction as the e… (superseded)
- [IDEM-3](../IDEM-3--e34f9c31/task.md) — IDEM-3: Bounded dedupe window -- retention policy, eviction, and the honest statement of… (superseded)
- [IDEM-4](../IDEM-4--d9c00d0d/task.md) — IDEM-4: Idempotent send and broadcast -- a legitimate retry returns the ORIGINAL result a… (superseded)
- [IDEM-5](../IDEM-5--9631dfcb/task.md) — IDEM-5: Same key + DIFFERENT payload is a protocol violation -- reject, log, and disconne… (superseded)
- [IDEM-6](../IDEM-6--208c4fb5/task.md) — IDEM-6: Idempotent enrol, leave, and peer-enrol (superseded)
- [IDEM-7](../IDEM-7--1c490a08/task.md) — IDEM-7: Exactly-once application on the relay path -- dedupe on the ORIGIN's identity, co… (superseded)
- [IDEM-8](../IDEM-8--d1ecfc75/task.md) — IDEM-8: Proof suite -- a retried send produces exactly one message, including across a cr… (superseded)
- [IDEM-9](../IDEM-9--b0dc4a12/task.md) — IDEM-9: Wrappers generate the key ONCE and reuse it on retry, + AGENT_PROTOCOL.md / PROTO… (superseded)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
