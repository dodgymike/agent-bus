# ACK-7: ACK/NACK retry, idempotency and exactly-once terminal handling

| Field | Value |
| --- | --- |
| Public id | `b7bf9631-59e2-4baf-805a-24968c5675db` |
| Key | ACK-7 |
| Epic | [ACK](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | ack |
| Section | backlog |
| Tags | — |
| Created | 2026-08-09T08:25:38.493317+00:00 |
| Updated | 2026-08-21T21:06:09.388143+00:00 |
| Completed | 2026-08-21T21:06:09.388125+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh 'go test -race -run ^TestAckTerminalExactlyOnceUnderRetry$ ./internal/relay'
```

## Description

Implement retry/backoff and durable idempotency for lost/duplicated ACK/NACK frames. A terminal state is absorbing; retry must neither redeliver a completed message nor emit contradictory terminal outcomes. Explicitly distinguish same-payload retry, conflicting correlation, and signed replay. Invariants: 1,4,5,6,10. Prospective files: internal/relay/, internal/hub/, internal/wal/, CONTRACTS-ONDISK.md.

--- SCOPE NARROWED 2026-08-21 (feature-runner, confirmed by the reviewer gate). The paragraph
above is the ORIGINAL intent and is kept verbatim; this section records what the task actually
delivers and why.

NOT DONE, AND DELIBERATELY: retry/backoff for ACK/NACK frame EMISSION. There is nothing to retry.
relay.Client.PeerAck has zero non-test callers on main, because ACK-5 owns emission and is
BUILDING IT RIGHT NOW (internal/relay/ackback.go in a live worktree, calling p.sender.PeerAck).
Implementing retry here would have collided head-on with in-flight P0 work.

ALREADY COVERED ELSEWHERE, so not duplicated: durable restart/crash idempotency is ACK-8
(d454ef7, internal/ack). Retry-exhaustion -> bounce is ACK-14. "Retry must not redeliver a
completed message" already holds structurally: Forwarder.Resume re-offers outbox.Pending(), which
is Jobs(OutboxPending), so a settled job is never re-offered.

WHAT THIS TASK DELIVERS: (a) TestAckTerminalExactlyOnceUnderRetry in internal/relay, closing a
real hole -- the relay ACK plane had ZERO concurrency coverage (no go func / sync.WaitGroup
anywhere in ack_test.go or ackhttp_test.go) for a P0 exactly-once property; (b) the
ACK-CONTRACT.md section 16 Q2 ruling, which the contract assigns to ACK-7 BY NAME: a terminal
negative does NOT cancel outstanding hops; (c) the ruling that a concurrent byte-identical retry
is answered 503 and must NOT be "fixed" to 200 duplicate:true, because the reservation cannot
distinguish pre- from post-fsync and answering duplicate:true would acknowledge before durability
(invariant 4 narrowing invariant 10). Both rulings are recorded in DECISIONS.md 2026-08-21.

FILES: internal/relay/ackretry_ack7_test.go (new), DECISIONS.md, AGENT_LOG.md. ZERO production
code changed. internal/hub, internal/wal and CONTRACTS-ONDISK.md are NOT touched -- no record
type, wire version, route, env var or CLI surface moved, so there is nothing to document there.
Invariants read in full: 4 and 10 (1, 5, 6 consulted).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [ACK-1](../ACK-1--e0ac42e1/task.md)
- **blocked by** [ACK-2](../ACK-2--9564f953/task.md)
- **blocked by** [ACK-3](../ACK-3--263c47fe/task.md)
- **blocks** [ACK-12](../ACK-12--17406b3a/task.md)
- **blocks** [ACK-8](../ACK-8--bc12541b/task.md)
- **follow-up of** [ACK-RETRY-ENGINE](../ACK-RETRY-ENGINE--81ce7331/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-14](../ACK-14--1884218d/task.md) — ACK-14: retry exhaustion must BOUNCE to the sender -- today the horizon expires and nobod… (todo)
- [ACK-5](../ACK-5--5991ee1a/task.md) — ACK-5: Multi-hop relay ACK/NACK propagation and correlation (done)
- [ACK-8](../ACK-8--bc12541b/task.md) — ACK-8: ACK/NACK restart, replay and crash-consistency recovery (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-7-FU-MIRROR-REPOINT](../ACK-7-FU-MIRROR-REPOINT--ee253a27/task.md) — ACK-7-FU-MIRROR-REPOINT: a7AssertMirrorMatchesProduction must follow the settle path, not… (done)
- [ACK-7-FU-SETTLEPATH-GUARD](../ACK-7-FU-SETTLEPATH-GUARD--e3218b15/task.md) — ACK-7-FU-SETTLEPATH-GUARD: a7SettlePath's default-arm assertion accepts the wrong switch,… (todo)
- [ACK-RETRY-ENGINE](../ACK-RETRY-ENGINE--81ce7331/task.md) — ACK-RETRY-ENGINE: sender-side retry of an unacknowledged relayed message, with a defined… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
