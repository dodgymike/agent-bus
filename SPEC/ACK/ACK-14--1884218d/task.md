# ACK-14: retry exhaustion must BOUNCE to the sender -- today the horizon expires and nobody is told

| Field | Value |
| --- | --- |
| Public id | `1884218d-41bb-422e-bd83-eee81cb50cad` |
| Key | ACK-14 |
| Epic | [ACK](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-16T12:50:03.379906+00:00 |
| Updated | 2026-08-21T21:44:51.114492+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestRetryHorizonExpiryBouncesToSender ./internal/relay ./internal/hub
```

## Description

OPERATOR-REQUESTED 2026-08-16, verbatim: "A sender bus that doesn't get an ack in a reasonable
time should retry a few times, and in the end bounce the message to the sender."

# What already exists -- do NOT rebuild it

The RETRY half is largely built at the transport layer and must be reused, not duplicated:
  internal/relay/forward.go:72   RetryHorizonCeiling = idem.PeerOutageBudget (24h)
  forward.go:77                  DefaultRetryHorizon = ceiling - DefaultForwardTimeout
  forward.go:84/91/97/106        backoff base 1s, min 10ms, cap
  forward.go:268/273             RetryHorizon, RetryBackoffBase are configurable
  ACK-CONTRACT.md:227            NACK class `horizon_expired` -- "the retry horizon ran out; the
                                 outbox settled abandoned" (internal/relay/outbox.go:286-291)

# What DOES NOT exist -- this is the whole task

NOTHING TELLS THE SENDER. Searched: no `bounce`, no `return_to_sender`, no sender-notification path
anywhere in internal/. Today the horizon expires, the job settles `abandoned`, and the sender is
never informed. That silent loss is exactly what the ACK epic exists to end, and it is currently the
DEFAULT outcome of a peer outage longer than 24h.

# The design question this task must ANSWER, not assume

"Bounce the message to the sender" has two readings:
  (a) NOTIFY the sender that delivery failed, identifying the message.
  (b) RETURN the message body to the sender, like an email bounce.

RECOMMENDED: (a), carrying the correlation key and the terminal NACK class, so the sender can
resolve its OWN copy. Reasons: the sender already HAS the body -- returning it stores a second copy
of something already durable on the origin bus, and invariant 6 keeps bodies out of the audit trail,
so a bounce carrying a body would be a new at-rest copy on a new path. It also makes a bounce cheap
enough to always send, which matters because the failure case is a peer outage -- exactly when the
system is under stress.

If the operator wants (b), that is a bigger change and needs its own decision: where the returned
body lives, its retention, and whether a bounce can itself bounce.

DO NOT decide this silently. Record the ruling in DECISIONS.md.

# Constraints

  - The bounce MUST NOT itself be retried into an infinite loop. A bounce that cannot be delivered
    is logged loudly (invariant 6: discards are logged loudly and specifically) and dropped. State
    that rule explicitly; do not let a bounce generate a bounce.
  - `horizon_expired` already exists as a NACK class -- REUSE it, do not mint a synonym. The closed
    12-class enum (ACK-CONTRACT.md section 6) must stay closed.
  - Terminal is ABSORBING (ACK-CONTRACT.md section 8). A bounce is the sender-visible face of a
    terminal negative, not a new state.
  - Invariant 10: a duplicate bounce for one message is a retry, not a violation. Do not disconnect.
  - The retry COUNT and horizon are already configurable; if the operator wants "a few times" to
    mean something other than the 24h horizon, that is a config decision to record, not a new
    mechanism to build.

# Layering -- the operator wants BOTH, and one builds on the other

  TRANSPORT layer  (this task, plus ACK-3/5/7): bus-to-bus, retry, terminal outcome, bounce.
  APPLICATION layer (ACK-6 + ACK-9): the recipient's agent-busctl telling the sender's agent-busctl
                    that the message actually reached the application.

The transport ack says "another bus took responsibility". The application notification says "a human
or agent actually got it". They are different facts and must not be collapsed -- ACK-CONTRACT.md
section 8.2 already rules that a hop ACK does NOT advance the sender-visible state, and that ruling
is what keeps these two layers honest.

# Acceptance
  - a peer outage past the retry horizon produces a bounce the SENDER can observe
  - the bounce names the correlation key and the terminal class
  - a bounce that cannot be delivered is logged loudly and dropped, never retried into a loop
  - a test drives a real horizon expiry (shortened horizon, not a 24h wait) and asserts the bounce
  - the ruling on (a) vs (b) is recorded in DECISIONS.md


--- CLAIM NARROWED 2026-08-21 (spec-keeper, at the coordinator's request, following filing of ACK-RETRY-ENGINE). The paragraph
above is the ORIGINAL text and is kept verbatim; this section records a correction to one claim in it.

THE CLAIM ABOVE ("The RETRY half is largely built at the transport layer and must be reused, not
duplicated", citing internal/relay/forward.go's RetryHorizonCeiling/backoff and outbox.go's
horizon_expired NACK class) is ACCURATE about transport retry and MISLEADING about ack retry. Read
literally it invites whoever picks up this task to conclude the retry half needs no new work --
that is false, and it is exactly the kind of stale-but-freshly-cited claim CLAUDE.md warns costs
this project the most, because it reads as freshly verified and names real code.

Verified by reading the code (read-only, 2026-08-21): outbox.OutboxDelivered ("the peer accepted...
or settled as a final, non-retriable outcome") settles a job the moment the NEXT HOP's HTTP
response accepts custody of the bytes -- internal/relay/forward.go:1347-1353, comment "OutboxDelivered
means the peer answered finally, and there is acceptance". The retry loop cited above retries
getting ONE HOP to take custody. It stops the instant that hop accepts, and it never retries because
an END-TO-END ACK/NACK failed to arrive afterwards -- that is a separate, later event on the
ACK-3/ACK-5 correlation plane, not something forward.go's loop observes at all.

So: TRANSPORT retry (getting a peer to accept the bytes) exists and should indeed be reused, not
duplicated -- the original claim is right about that. ACK retry (retrying because the terminal
ack/nack for an ALREADY-ACCEPTED message never came back) is UNBUILT. That gap is now owned by
ACK-RETRY-ENGINE (81ce7331-e66e-46ee-92af-7571439e971c), filed 2026-08-21, which this task should
consume rather than reimplement: ACK-RETRY-ENGINE's exhaustion signal is what this task's bounce
must fire on, not forward.go's horizon_expired in isolation. Whoever implements this task's bounce
must not assume the sentence above already covers ack-timeout retry -- it does not.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **relates to** [ACK-RETRY-ENGINE](../ACK-RETRY-ENGINE--81ce7331/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-3](../ACK-3--263c47fe/task.md) — ACK-3: Authenticated peer-hop ACK/NACK wire semantics and correlation (done)
- [ACK-5](../ACK-5--5991ee1a/task.md) — ACK-5: Multi-hop relay ACK/NACK propagation and correlation (done)
- [ACK-6](../ACK-6--d3c50d33/task.md) — ACK-6: Recipient delivery acknowledgement boundary (done)
- [ACK-9](../ACK-9--08f9987f/task.md) — ACK-9: Sender CLI/API acknowledgement status and observability (done)
- [ACK-RETRY-ENGINE](../ACK-RETRY-ENGINE--81ce7331/task.md) — ACK-RETRY-ENGINE: sender-side retry of an unacknowledged relayed message, with a defined… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-7](../ACK-7--b7bf9631/task.md) — ACK-7: ACK/NACK retry, idempotency and exactly-once terminal handling (done)
- [ACK-RETRY-ENGINE](../ACK-RETRY-ENGINE--81ce7331/task.md) — ACK-RETRY-ENGINE: sender-side retry of an unacknowledged relayed message, with a defined… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
