# RELAY-47: ONWARD RELAY -- WIRE an intermediate bus to forward a relayed message to a THIRD bus (A-&gt;B-&gt;C), consuming the loop guards and hop limit that already exist

| Field | Value |
| --- | --- |
| Public id | `dd69c4d3-b129-450c-aa3b-0457a1e299f2` |
| Key | RELAY-47 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T11:46:14.971065+00:00 |
| Updated | 2026-08-15T13:48:36.097282+00:00 |
| Completed | 2026-08-15T13:48:36.097265+00:00 |

## Proof command

```sh
go test -race -count=1 -run TestOnwardRelay ./cmd/agent-bus
```

## Description

Filed 2026-08-15 by spec-keeper, FILE-ONLY. Measured first-hand by `main` at HEAD 9701611 and INDEPENDENTLY REPRODUCED by spec-keeper in a clean `git archive HEAD` overlay before filing (both runs identical, different bus ids). Every code claim below was RE-VERIFIED against 9701611 by spec-keeper rather than taken on trust.

WIRE ONWARD FORWARDING SO AN INTERMEDIATE BUS CARRIES A MESSAGE ADDRESSED TO A THIRD BUS. Last blocker to the three-bus smoke test and the RELAY epic's deliverable.

THIS IS TWO WIRING CHANGES PLUS TESTS. IT IS NOT A FEATURE BUILD. Every primitive it needs -- the forwarder, the loop guards, the hop limit -- ALREADY EXISTS AND IS ALREADY TESTED. Scope is: wiring + guard-relaxation + tests. If you find yourself writing a loop detector or a hop counter, STOP: you are duplicating a careful existing design (see "PRIMITIVES THAT ALREADY EXIST" below).

== WHAT HAPPENS TODAY ==

`bash scripts/fed-smoke.sh` at 9701611 achieves a REAL cross-bus delivery for the first time -- A->B is `POST /v1/peer/relay status=200`, `relayed message accepted`, `origin_message_id` preserved on B. It then fails, exit 1, at the SECOND hop:

    fed-smoke: ERROR: relay not established: C audit lacks bus-4efwrdiscwleoywv-11 with
    complete bus_path=[bus-4efwrdiscwleoywv,bus-zbjgiu4es4xe7fgp,bus-cm7botlevr5gu5qr]

Bus B states the cause itself. This remains the clearest available description of the gap and is quoted VERBATIM (invariant 6 working exactly as designed -- the reason diagnosis took thirty seconds rather than a day):

    level=warn msg="a relayed message was ACCEPTED AND DURABLY RECORDED but is being carried NO FURTHER: it names recipients on another bus, and this build wires no onward relay. The sending peer has been told 200 and will not retry, so those recipients will never receive it" local_bus=bus-zbjgiu4es4xe7fgp origin_bus=bus-4efwrdiscwleoywv origin_message_id=bus-4efwrdiscwleoywv-11 foreign_recipients=1 remedy="this build accepts relayed mail for its OWN agents only; onward relay needs the egress forwarder, which is not wired yet"

Startup carries `onward_relay=false`. That flag going true is the outcome of this task.

== THE 200 IS CORRECT AND IS NOT IN SCOPE -- OPERATOR RULING, 2026-08-15 ==

READ THIS BEFORE SCOPING. The warning above notes that the sending peer "has been told 200 and will not retry". THAT ACKNOWLEDGEMENT IS CORRECT AND MUST NOT BE CHANGED BY THIS TASK. The operator ruled on it directly on 2026-08-15: the 200 is fine, the successor is asynchronous message delivery notifications, and the instruction was to move forward and focus on the intermediate forwarding.

This is a DESIGNED BOUNDARY, not an oversight and not a deferral:

  * `POST /v1/peer/relay` returns 200 on DURABLE ACCEPTANCE. That is what it promises and it promises it honestly -- invariant 4 is satisfied, the message genuinely IS durable before the ack.
  * DURABLE-ACCEPTANCE and DELIVERED-TO-RECIPIENT ARE DIFFERENT FACTS. This system reports the first synchronously and will report the second ASYNCHRONOUSLY, when the delivery-notification capability exists.
  * Delivery confirmation is therefore deliberately OUT OF SCOPE here.

Do NOT change the acknowledgement semantics, do NOT add a refuse-at-ingest path on the grounds that the ack overpromises, and do NOT file a decision task about it. Someone reading this in six months should understand the boundary was DECIDED, not overlooked.

FORWARD REFERENCE ONLY -- NOTHING BLOCKS ON IT: the asynchronous delivery-notification capability the operator named already has a home in this backlog, EPIC ACK ("End-to-end authenticated delivery ACK/NACK", 12 open tasks), in particular ACK-6 (Recipient delivery acknowledgement boundary), ACK-5 (Multi-hop relay ACK/NACK propagation and correlation) and ACK-1 (the contract and terminal state machine). Recorded as a pointer for the reader. RELAY-47 does NOT depend on epic ACK, is NOT blocked by it, and must not grow an edge to it.

== PRIMITIVES THAT ALREADY EXIST -- CONSUME THESE, DO NOT REBUILD THEM ==

All verified at 9701611. internal/relay/path.go carries a two-part loop-prevention design WITH ITS REASONING WRITTEN DOWN (see its own doc at path.go:204, "The division of labour, which matters because neither half is sufficient"):

  * `AppendHop` (path.go:179) -- refuses via `PathContains` with `ErrRelayLoop`: "bus %q is already on the path, so appending it would fabricate a second visit" (path.go:189-191). It ALWAYS RETURNS A FRESH SLICE (path.go:195-198, `out := make([]string, 0, len(path)+1)`), so one outbound forward cannot silently rewrite the path another is about to read.
  * `NextHopAllowed` (path.go:220) -- the EGRESS split horizon, stopping cycle traffic before a byte leaves the process.
  * `CheckIncomingPath` (path.go:155) -- the INGRESS backstop, because A PEER'S PATH IS UNTRUSTED INPUT (PROTOCOL.md 8.5). A peer can strip itself or us out of the path it forwards, so the receiving side re-derives the decision from its OWN identity -- the one thing on the path it does know (path.go:160, ErrRelayLoop: "bus %q appears on the %d-hop path this message arrived with, so it has been here before").

THE HOP LIMIT ALREADY EXISTS TOO. `AppendHop` refuses when `len(path)+1 > store.MaxBusPath` (64) with `ErrBusPathTooLong` (path.go:192-194), and `MaxReceivedBusPath` is HARD-LINKED to `store.MaxBusPath` rather than re-declared as a literal, deliberately and with the reasoning recorded (path.go:40-46: the received cap is derived as one less than the durable cap, because a path we accept is a path we go on to persist after appending ourselves). DO NOT BUILD A HOP LIMIT. DO NOT FILE ONE.

== WHAT IS ACTUALLY MISSING -- TWO WIRING CHANGES ==

1. `relay.AcceptOptions.Onward` IS NIL. The interface `relay.OnwardForwarder` exists (accept.go:99); `AcceptOptions.Onward` exists and is documented as OPTIONAL with nil a legitimate LEAF configuration (accept.go:122-128); the `Acceptor` already stores and uses it (accept.go:163, :188); and `relay.Forwarder` ALREADY SATISFIES THE INTERFACE -- there is a compile-time assertion saying exactly that at accept.go:107, `var _ OnwardForwarder = (*Forwarder)(nil)`. RELAY-21 (done, 14eafd9) already implemented re-forward-ONLY-on-`idem.OutcomeNew` in `Acceptor.Accept`. NOTHING IN cmd/agent-bus/relaywiring.go SUPPLIES IT -- relaywiring.go:727 passes `Onward: nil`, documented at :20-21 and :723.

2. cmd/agent-bus/relayegress.go:259 DECLINES ANYTHING NOT LOCALLY-ORIGINATED -- `if !strings.EqualFold(m.BusPath[0], e.busID) { return }`. That single line is what makes this bus a leaf. The design intent is stated plainly at main.go:1269-1280: the egress adapter "deliberately forwards only messages ORIGINATED HERE".

== THE SUBTLETY THIS TASK MUST NAME -- THIS IS WHERE IT GETS BROKEN ==

THAT `BusPath[0]` CHECK IS CURRENTLY DOING DOUBLE DUTY. It distinguishes "originated here" from "relay-ingested", AND it acts as an IMPLICIT LOOP GUARD by refusing everything that arrived from elsewhere. RELAXING IT FOR ONWARD RELAY REMOVES BOTH.

Therefore: the EXPLICIT guards (`AppendHop` / `NextHopAllowed` / `CheckIncomingPath`) must be DEMONSTRABLY CARRYING THE LOOP-PREVENTION LOAD BEFORE that check is loosened -- proven by A TEST THAT FAILS IF THEY ARE NOT. DO NOT LET A REVIEWER ACCEPT "AppendHop is called somewhere" AS EVIDENCE. The test must be one that goes RED if the explicit guard is removed while the implicit one is already gone.

Note also, at relayegress.go:261-265, the `OriginMessageID` "belt" immediately below that check, honestly labelled as one: "Nothing sets this field today, so this line cannot fire ... It is NOT the control -- the bus path above is." Do not mistake the belt for the braces.

== THE TWO BUSPATH CONVENTIONS -- CARRIED VERBATIM, SAME CODE PATH THIS TASK TOUCHES ==

From relayegress.go:394-407:

    // BusPath IS DELIBERATELY EMPTY, AND THIS IS THE TRAP ON THIS PATH.
    //
    // RelayedMessage.BusPath is the path AS RECEIVED, NOT including this
    // bus: Forward(localBusID) appends our hop via AppendHop, whose doc
    // names an empty input path as the ONE legal empty case, meaning "this
    // bus is the ORIGIN".
    //
    // store.Message.BusPath on a locally-originated message is
    // store.LocalBusPath(busID) — that is, [busID], OUR OWN HOP ALREADY IN
    // IT. Copying it here would hand AppendHop a path it is already on, so
    // every single forward would come back ErrRelayLoop and be dropped, on a
    // bus whose logs would say "loop" about a message that had never left.
    // Leaving it nil is not an omission; it is the value.

TWO DIFFERENT CONVENTIONS ON SIMILARLY-NAMED FIELDS, on exactly the code path this task touches. Getting it wrong produces a bus that logs "loop" about messages that never left.

== THE OTHER SEAM IS NOT A GATE TO "FIX" ==

Do NOT route onward relay through `hub.Egress` by simply deleting the relayegress.go:259 check and rebuilding an envelope there. That seam's own doc (relayegress.go:203-240) explains why forwarding a relay-ingested message THERE would be ACTIVELY WRONG rather than merely redundant: it would claim OUR bus as the origin, mint an attestation for an agent in someone else's namespace (which `attest.Sign` refuses outright, invariant 2), and hand `AppendHop` an empty path, ERASING the loop-prevention history. The correct seam for an ingested message is `AcceptOptions.Onward`, which carries a different envelope. Whatever relaxation happens at :259 must respect that.

cmd/agent-bus/relaywiring.go:1064 (`warnIfCarriedNoFurther`) emits the warning quoted at the top; it is the one place that knows the difference between "leaf by design" and "egress not wired yet". KEEP IT after this lands -- there will still be cases where it must fire (peer down, no route to the recipient, hop limit reached), and it is the model for how a limitation should be reported.

== STILL REQUIRED ==

SECURITY -- BOUND PEER-TRIGGERED ONWARD WORK PER PEER. An onward hop means a bus does OUTBOUND work triggered by a PEER's message. That is exactly the shape of the idem-denominator P0 recorded on RELAY-FU-IDEM-METER-BY-PEER (8774f265) and of the SSRF class this repo has ALREADY SHIPPED ONCE. The existing per-peer admission reserve at relaywiring.go:1034 (`f.admission.reserve(peerBusID)`) is the precedent to follow: metered by the AUTHENTICATED PEER, never by the sender label inside the envelope, which a peer chooses.

READ INVARIANT 10 IN FULL before touching this: the traversed bus path is a COMPLEMENT to idempotency, NEVER A SUBSTITUTE.

KNOWN COST, ALREADY FILED -- DO NOT RE-FILE: `forwardOnward` runs with `writeMu` HELD, so a broadcast can cost up to 128 serial fsyncs under the global send lock. Onward relay puts MORE work under that lock. NOTE THE INTERACTION in your report; do not open a duplicate performance task.

MVP ONLY. Deliberately EXCLUDED, to be separate lower-priority tasks or left unfiled: retry-policy sophistication, delivery receipts (epic ACK, above), multi-hop beyond three buses, and onward relay across more than one intermediate bus.

== PROOF ==

`bash scripts/fed-smoke.sh` -- the epic's own three-bus deliverable, which asserts the complete ordered three-hop `bus_path` (scripts/fed-smoke.sh:465 and :480). Deliberately contains NO shell pipe (an unquoted `|` inside `-run` makes proof-check split the proof as a pipeline and report UNVERIFIABLE for any implementation -- that defect has been found repeatedly this session).

OBSERVED RED BEFORE THE FIX, by spec-keeper, 2026-08-15, in a clean overlay of HEAD 9701611:
`proof-check: verdict=FAIL class=wrapper exit=1 tests_run=0 top_level=0 skipped=0 failed=0 empty_pkgs=0`
failing on exactly the line above (`C audit lacks ... complete bus_path=[A,B,C]`), with B's warning confirmed present in /tmp/fed-smoke-b/run/agent-bus.log, foreign_recipients=1. This proof is CONFIRMED RED, not merely stored.

NOTE FOR WHOEVER RUNS IT: fed-smoke.sh PRESERVES /tmp/fed-smoke-{a,b,c} on failure for inspection, and its preflight then REFUSES to run again until they are removed. A rerun without `rm -rf -- /tmp/fed-smoke-a /tmp/fed-smoke-b /tmp/fed-smoke-c` fails with "preflight refused to alter existing resources" -- an ENVIRONMENTAL FAIL easily mistaken for the real RED. They are different failures; check which one you have.

ALSO REQUIRED, and these are the ones that carry the real risk: the guard-relaxation test described above (RED if the explicit loop guards are not carrying the load), and a per-peer bound test. The smoke test is the end-to-end evidence, not the only evidence.

== SCOPE / RELATIONS ==

Blocks RELAY-25 (10491a01) -- the epic deliverable. This is the ENTIRE remaining critical path to the three-bus smoke test.
Do NOT touch internal/idem or internal/hub's constructor line while codex is live there (2026-08-15).
Per invariant 7, any agent-visible surface change ships with its `cmd/agent-busctl` subcommand and AGENT_PROTOCOL.md entry IN THIS TASK.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-25](../RELAY-25--10491a01/task.md)
- **follow-up** [RELAY-47-FU-DOCS](../RELAY-47-FU-DOCS--6f7281e8/task.md)
- **follow-up** [RELAY-47-FU-FANOUT](../RELAY-47-FU-FANOUT--1cbdcc37/task.md)
- **follow-up** [RELAY-47-FU-IDEMFINGERPRINT](../RELAY-47-FU-IDEMFINGERPRINT--b666cd5a/task.md)
- **follow-up** [RELAY-48](../RELAY-48--9887b0eb/task.md)
- **follow-up** [RELAY-49](../RELAY-49--efbcc6cf/task.md)
- **follow-up** [RELAY-50](../RELAY-50--c4a1bd15/task.md)
- **relates to** [RELAY-11-FU-INGEST-LOOPGUARD](../RELAY-11-FU-INGEST-LOOPGUARD--a41c273c/task.md)
- **relates to** [RELAY-21](../RELAY-21--f5ce883e/task.md)
- **relates to** [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md)
- **relates to** [RELAY-25-FU-CORRELATION](../RELAY-25-FU-CORRELATION--3f009222/task.md)
- **relates to** [RELAY-FU-IDEM-METER-BY-PEER](../RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [ACK-1](../../ACK/ACK-1--e0ac42e1/task.md) — ACK-1: Define end-to-end ACK/NACK delivery contract and terminal state machine (done)
- [ACK-5](../../ACK/ACK-5--5991ee1a/task.md) — ACK-5: Multi-hop relay ACK/NACK propagation and correlation (done)
- [ACK-6](../../ACK/ACK-6--d3c50d33/task.md) — ACK-6: Recipient delivery acknowledgement boundary (done)
- [RELAY-21](../RELAY-21--f5ce883e/task.md) — RELAY-21: AcceptRelay callback: roster-check before durable write, re-forward on OutcomeN… (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (done)
- [RELAY-FU-IDEM-METER-BY-PEER](../RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) — RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not t… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-25-FU-CORRELATION](../RELAY-25-FU-CORRELATION--3f009222/task.md) — RELAY-25-FU-CORRELATION: fed-smoke.sh asserts the SAME message_id string in A's, B's and… (done)
- [RELAY-47-FU-DOCS](../RELAY-47-FU-DOCS--6f7281e8/task.md) — RELAY-47-FU-DOCS: three shipped docs still tell agents multi-hop relay does not work, aft… (done)
- [RELAY-47-FU-FANOUT](../RELAY-47-FU-FANOUT--1cbdcc37/task.md) — RELAY-47-FU-FANOUT: refine the onward fan-out bound -- maxOnwardBusesPerMessage counts DE… (todo)
- [RELAY-47-FU-IDEMFINGERPRINT](../RELAY-47-FU-IDEMFINGERPRINT--b666cd5a/task.md) — RELAY-47-FU-IDEMFINGERPRINT: the ENFORCED idempotency fingerprint is not the one internal… (todo)
- [RELAY-48](../RELAY-48--9887b0eb/task.md) — RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at… (done)
- [RELAY-49](../RELAY-49--efbcc6cf/task.md) — RELAY-49: the egress split horizon is applied to the DESTINATION bus, not to the NEXT HOP… (todo)
- [RELAY-50](../RELAY-50--c4a1bd15/task.md) — RELAY-50: a PARTIALLY-routed relayed message is not individually logged -- one destinatio… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
