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
| Updated | 2026-08-16T12:50:03.379906+00:00 |
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

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-3](../ACK-3--263c47fe/task.md) — ACK-3: Authenticated peer-hop ACK/NACK wire semantics and correlation (in_progress)
- [ACK-6](../ACK-6--d3c50d33/task.md) — ACK-6: Recipient delivery acknowledgement boundary (done)
- [ACK-9](../ACK-9--08f9987f/task.md) — ACK-9: Sender CLI/API acknowledgement status and observability (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
