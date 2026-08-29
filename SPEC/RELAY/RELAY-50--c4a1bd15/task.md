# RELAY-50: a PARTIALLY-routed relayed message is not individually logged -- one destination silently never hears about it, and nothing names the message

| Field | Value |
| --- | --- |
| Public id | `c4a1bd15-f993-40bf-90e9-13d48a8ab2c6` |
| Key | RELAY-50 |
| Epic | [RELAY](../epic.md) |
| Status | todo |
| Priority | P2 |
| Component | relay |
| Section | backlog |
| Tags | relay, observability, invariant-6, from-review, relay-47-followup |
| Created | 2026-08-15T12:55:08.052491+00:00 |
| Updated | 2026-08-15T13:13:30.384881+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestPartiallyRoutedRelayLogsEachUnroutableRecipient ./internal/relay
```

## Description

Filed 2026-08-15 by spec-keeper on behalf of the RELAY-47 feature-runner. Found by the RELAY-47 review/security gates; the sound fix needs the forwarder's resolved target set, so it is OUT OF BOUNDS for RELAY-47's wiring site.

== THE GAP ==

Recipients on TWO foreign buses, one routable and one not:

- `relay.Forwarder.targets` (internal/relay/forward.go:1044-1050) counts the no-route recipient into a counter that has **NO log line at all**;
- `queued` is 1 (the routable one), so RELAY-47's `onwardRelay.Enqueue` stays silent;
- `warnIfCarriedNoFurther` early-returns, because SOMETHING was carried.

Net: the message is accepted, acked **200**, and one destination NEVER hears about it, **with nothing in the log naming that message**. Invariant 6 requires every discard to be logged loudly and SPECIFICALLY; a silent partial discard is exactly the defect it names. The all-or-nothing cases are both already covered -- this is the mixed case that falls between them.

== DO NOT USE THE OBVIOUS DETECTOR ==

`queued < len(foreign)` is **NOT SOUND** and must not be shipped: the EGRESS SPLIT HORIZON legitimately drops a destination bus that is already on the traversed path -- a message relayed A->B naming recipients on both A and C counts TWO foreign buses at B and correctly queues ONE copy (nothing is lost, A already has it) -- and `relay.Forwarder.targets` also drops a destination with no route, without a log line. Counted-destinations and queued-copies therefore differ routinely on entirely correct traffic (RELAY-47's own headline ring test exercises exactly this), which is exactly why the comparison cannot be used as a delivery-failure detector.

CORRECTION (2026-08-15, from the RELAY-47 reviewer gate): an earlier version of this section justified the same conclusion with a DIFFERENT, FALSE claim -- that a `-route-for` topology "legitimately collapses several destination bus halves onto ONE outbox job." That is retracted: cmd/agent-bus/peer.go:940 writes a SEPARATE relay.PeerConfig{BusID: <destination>, BaseURL: <next hop's URL>} per -route-for destination, internal/relay/registry.go:495-509 Route returns the DESTINATION bus id, and internal/relay/forward.go:1044-1060 dedupes on that value -- so two destinations behind one next hop are TWO jobs and TWO POSTs to one address, not one job. The conclusion (detector forbidden) stands; the reason above (split horizon + no-route drop) is the correct one.

The sound version needs the set of targets the forwarder ACTUALLY RESOLVED, compared against the set of foreign recipient bus halves in the envelope -- i.e. the log line must be emitted from inside `relay.Forwarder.targets`, where both sets exist. Fix is entirely within `internal/relay`.

The log line must name: the origin bus, the origin message id, the specific unroutable recipient(s), the local bus, and the remedy (add a peer record or a `-route-for` route), matching the specificity of the existing carried-no-further warning.

== PROOF ==

    go test -race -run TestPartiallyRoutedRelayLogsEachUnroutableRecipient ./internal/relay

Assert BOTH directions: (a) mixed routable/unroutable recipients -> exactly one WARN naming the unroutable recipient and the origin message id; (b) a topology where queued copies are legitimately reduced below the foreign-bus count by the egress split horizon (a recipient bus already on the traversed path) and/or a no-route drop -> NO warning. Keep the `-route-for` topology fed-smoke.sh exercises as part of this false-positive arm, but its stated mechanism must be the split horizon / no-route drop present in that topology, NOT job collapsing -- `-route-for` destinations do not collapse onto one outbox job (see CORRECTION above), so job collapsing is never the reason a false positive is avoided.

RELATES: RELAY-47, RELAY-24-BLOCKER-EGRESS-FU-NOROUTEWORDING (the sibling wording gap on the all-unroutable path).

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up of** [RELAY-47](../RELAY-47--dd69c4d3/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24-BLOCKER-EGRESS-FU-NOROUTEWORDING](../RELAY-24-BLOCKER-EGRESS-FU-NOROUTEWORDING--5c465133/task.md) — RELAY-24-BLOCKER-EGRESS-FU-NOROUTEWORDING: no-route drop log line does not spell out inva… (todo)
- [RELAY-47](../RELAY-47--dd69c4d3/task.md) — RELAY-47: ONWARD RELAY -- WIRE an intermediate bus to forward a relayed message to a THIR… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
