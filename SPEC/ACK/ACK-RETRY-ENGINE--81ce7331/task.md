# ACK-RETRY-ENGINE: sender-side retry of an unacknowledged relayed message, with a defined handoff to ACK-14's bounce

| Field | Value |
| --- | --- |
| Public id | `81ce7331-e66e-46ee-92af-7571439e971c` |
| Key | _(null in the export)_ |
| Epic | [ACK](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | relay/ack |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T21:42:58.752618+00:00 |
| Updated | 2026-08-21T21:42:58.752618+00:00 |
| Completed | — |

## Proof command

```sh
grep -q 'ACK-RETRY-ENGINE' DECISIONS.md && grep -rq -E 'AckRetryEngine|SenderAckRetry' --include=*.go internal/relay internal/hub internal/ack
```

## Description

OPERATOR REQUIREMENT (verbatim, 2026-08-16, quoted again on ACK-14): "A sender bus that doesn't get an ack in a reasonable time should retry a few times, and in the end bounce the message to the sender." ACK-14 (1884218d-41bb-422e-bd83-eee81cb50cad, P1, todo) owns the BOUNCE half. This task owns the RETRY half, which is genuinely unbuilt at the ACK/NACK layer -- do not assume ACK-14's own description settles it; see the disambiguation section below.

WHY THIS IS NOT ALREADY COVERED

ACK-7 (b7bf9631, done) was titled 'ACK/NACK retry, idempotency and exactly-once terminal handling' and was narrowed by reviewer ruling on 2026-08-21 to two DECISIONS.md rulings plus one concurrency test (TestAckTerminalExactlyOnceUnderRetry). It shipped ZERO production code for retry/backoff EMISSION -- see its SCOPE NARROWED section, verbatim: 'NOT DONE, AND DELIBERATELY: retry/backoff for ACK/NACK frame EMISSION. There is nothing to retry... Implementing retry here would have collided head-on with in-flight P0 work [ACK-5].'

ACK-5 (5991ee1a, in_progress) built relay.BackPropagator.Propagate, the terminal-outcome emitter. Its propagation is SYNCHRONOUS and has no retry queue, DELIBERATELY -- synchrony is what carries invariant 4 end to end instead of a local write standing in for one.

DISAMBIGUATION -- read this before assuming ACK-14 already covers retry: ACK-14's description says the retry half is 'largely built at the transport layer' and cites internal/relay/forward.go's RetryHorizonCeiling/backoff and outbox.go's horizon_expired NACK class. That is TRUE but answers a DIFFERENT question. Verified by reading the code (2026-08-21, read-only): outbox.OutboxDelivered ('the peer accepted... or settled as a final, non-retriable outcome') settles the job the moment the NEXT HOP's HTTP response accepts custody -- forward.go:1347-1353, comment 'OutboxDelivered means the peer answered finally, and there is acceptance'. That retry loop is about getting ONE HOP to take custody of the bytes; once it does, the outbox job is terminal and the retry loop stops, regardless of whether an end-to-end ACK/NACK for that message ever later arrives back through ACK-3/ACK-5's separate correlation path. So the existing retry mechanism does not retry ANYTHING once a hop has accepted -- it cannot be what closes the operator's ask, which is about the SENDER not hearing back an ack at all, not about a peer refusing to take the bytes. horizon_expired is real but it is the TRANSPORT-hop failure-to-accept case, not the ACK-timeout case.

SCOPE

Design and implement bounded sender-side retry of a message whose terminal ACK/NACK has not arrived within a reasonable time, with a defined handoff to ACK-14 when attempts are exhausted. This is a NEW mechanism at the ACK/NACK correlation layer (ACK-3/ACK-5/ACK-8's plane), not a reuse of the transport-hop outbox retry, though it may choose to piggyback on the SAME durable outbox record (internal/relay/outbox.go, `agent-bus outbox` operator command landed RELAY-54, 7c96f2b) rather than invent a second durable structure.

OPEN DESIGN QUESTIONS THIS TASK MUST ANSWER AND RECORD IN DECISIONS.md, not silently assume:

1. WHERE does retry live -- a new field/state on the existing outbox.OutboxRecord (extending pending to distinguish 'hop-accepted, still awaiting terminal ack' from today's binary pending/delivered/abandoned), or a wholly separate durable structure? Point 4 of the brief that produced this task: pose it as a design question, do not implement an answer speculatively.

2. WHAT does 'retry' mean here, precisely, given the hop is already accepted -- re-emitting the original relay POST (which risks the recipient bus double-processing an already-accepted message, mitigated only by idempotency keys), or something else (e.g. a status-probe / re-poll of the correlation key)? State the chosen mechanism and why re-POSTing an already-accepted message is or is not safe under invariant 10.

3. INVARIANT 4 (read INVARIANTS.md in full before designing). ACK-5 chose synchronous propagation specifically so nothing is acknowledged before durable, end to end. A retry queue at this layer reintroduces the question of what is durable and when a retry is safe to fire. Answer it explicitly.

4. INVARIANT 10 (read INVARIANTS.md in full). A retry engine is an idempotency machine. The three cases must not be collapsed: same key + same payload is a legitimate retry -- return the original result, do not re-apply, do NOT disconnect; same key + different payload is a protocol violation -- reject and log, still without disconnecting; only replay of an already-accepted SIGNED message disconnects, under the narrow conditions the invariant states.

5. PERMANENT VS TRANSIENT. ACK-5-FU-TRANSIT-503-IS-PERMANENT (ce287d71-1937-41de-9ac9-c163a237e2eb, P1, todo) records that a permanent origin 409 currently reaches the recipient as a retriable 503 on the agent-facing ack surface. A retry engine built before that is resolved will retry something that can never succeed. CITE it in the design; state explicitly whether this task BLOCKS on ce287d71 or can proceed with a documented interim workaround (e.g. bounded attempt count catches the permanent case eventually, just wastefully). Do not silently retry-forever a 409-shaped permanent failure.

6. SATURATION. errAckTransitSaturated answers 503 with Retry-After: 1 on both the peer and agent surfaces today, and nothing currently acts on that signal. Decide and record whether this retry engine should treat a saturation 503 differently from an ordinary unacked-timeout (e.g. honour Retry-After rather than the engine's own backoff schedule) -- this is in scope to DECIDE, not to leave unaddressed.

7. 'REASONABLE TIME' must be DEFINED, not left implicit. ACK-14's title refers to a horizon expiring but that horizon (RetryHorizonCeiling / DefaultRetryHorizon in forward.go, bound to idem.PeerOutageBudget = 24h) is the TRANSPORT-hop horizon disambiguated away above -- it is not necessarily the right horizon for 'no ack received'. Either define a new horizon for this layer (name it, state where it is stored, and whether it survives restart) or state explicitly that it deliberately reuses the transport horizon and why that is correct despite the two being conceptually different questions.

8. RECIPIENT FIELD COLLISION. ACK-14 will add the recipient to the outbox record, and ACK-4-FU-RECIPIENT-BINDING (ec4a1ac8-d490-4723-9f00-d34ca64c44f6) needs the same field. Whichever of ACK-14 / ACK-4-FU-RECIPIENT-BINDING / this task lands first OWNS that field's shape; the other two must consume it, not add a second differently-spelled one. State in this task's report which field name was landed or reused.

HANDOFF TO ACK-14: on exhausting the bounded attempts (however 'attempts' is ultimately defined per point 1/7 above), this task must produce EXACTLY the signal ACK-14 already expects to consume -- the horizon_expired-shaped terminal outcome ACK-14 acts on to bounce. Coordinate the exact interface (a function call, a settled outbox state, a NACK class) with whichever task lands second; do not let both tasks invent independent notions of 'exhausted'.

ACCEPTANCE
- A message whose end-to-end ACK/NACK does not arrive within the defined 'reasonable time' is retried a bounded number of times with backoff, not retried forever and not retried exactly once.
- The retry mechanism does not violate invariant 10 (three cases above) under a concurrency/-race test.
- On exhaustion, the mechanism hands off to whatever ACK-14 consumes, verified by an integration test that drives a real (shortened) horizon expiry, not a 24h wait.
- Every open design question above (1-8) is answered and the answer is recorded in DECISIONS.md before the corresponding code lands.
- The DECISIONS.md entries explicitly state whether this task BLOCKS on ACK-5-FU-TRANSIT-503-IS-PERMANENT and, if not blocking, what the interim behaviour is against a permanent failure.

Invariants to read in full before designing: 1, 4, 5, 6, 10. Read INVARIANTS.md, not just the CLAUDE.md one-liners.

Related, not duplicate: ACK-7 (b7bf9631) is the task this scope was cut FROM (reviewer-narrowed 2026-08-21, zero production code shipped for retry emission). ACK-14 (1884218d) is the bounce this task hands off to. ACK-5-FU-TRANSIT-503-IS-PERMANENT (ce287d71) is the permanent-vs-transient classification this task's retry loop must not blindly retry through.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-14](../ACK-14--1884218d/task.md) — ACK-14: retry exhaustion must BOUNCE to the sender -- today the horizon expires and nobod… (todo)
- [ACK-3](../ACK-3--263c47fe/task.md) — ACK-3: Authenticated peer-hop ACK/NACK wire semantics and correlation (done)
- [ACK-4-FU-RECIPIENT-BINDING](../ACK-4-FU-RECIPIENT-BINDING--ec4a1ac8/task.md) — ACK-4-FU-RECIPIENT-BINDING: the obligation binding does not bind the RECIPIENT to the ack… (done)
- [ACK-5](../ACK-5--5991ee1a/task.md) — ACK-5: Multi-hop relay ACK/NACK propagation and correlation (done)
- [ACK-5-FU-TRANSIT-503-IS-PERMANENT](../ACK-5-FU-TRANSIT-503-IS-PERMANENT--ce287d71/task.md) — ACK-5-FU-TRANSIT-503-IS-PERMANENT: a permanent origin 409 reaches the recipient as a retr… (todo)
- [ACK-7](../ACK-7--b7bf9631/task.md) — ACK-7: ACK/NACK retry, idempotency and exactly-once terminal handling (done)
- [ACK-8](../ACK-8--bc12541b/task.md) — ACK-8: ACK/NACK restart, replay and crash-consistency recovery (done)
- [RELAY-54](../../RELAY/RELAY-54--911841af/task.md) — RELAY-54: an abandoned outbox job is invisible to every subcommand -- the drain a rollout… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-14](../ACK-14--1884218d/task.md) — ACK-14: retry exhaustion must BOUNCE to the sender -- today the horizon expires and nobod… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
