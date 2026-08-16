# CONV-PEERQUOTA: bound conversation tracking per PEER on DISTINCT CONVERSATION IDS -- the 8774f265 attack shape

| Field | Value |
| --- | --- |
| Public id | `35cb7dc6-28f8-42a1-a51b-fdde277878cf` |
| Key | CONV-PEERQUOTA |
| Epic | [CONV](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | conv |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T08:54:46.523190+00:00 |
| Updated | 2026-08-15T08:54:46.523190+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestConversationTrackingBoundedByPeer' ./internal/hub ./internal/relay
```

## Description

**THE MOST IMPORTANT SECURITY TASK IN THIS EPIC. It is P1, and it must land in the SAME WAVE as
CONV-TRACK-ON-RECEIPT, which it guards -- never after it.**

The operator's request contains, verbatim, "When a server first receives a conversation message it
should start tracking it". Read that as a security engineer would: **it is an UNBOUNDED,
PEER-TRIGGERED STATE-CREATION PRIMITIVE.** A peer asserts N distinct conversation ids and the local
bus creates N tracking records. Nothing in the request bounds N.

**THIS REPO FORGED EXACTLY THAT ATTACK END-TO-END, OVER HTTP, ON 2026-08-15.** See
RELAY-FU-IDEM-METER-BY-PEER (`8774f265-230d-49c9-90e4-bd96c866fd8d`, P0, in_progress -- a `relates`
edge is wired to it). Measured result: **32,766 messages from ONE authenticated peer certificate,
each asserting a DISTINCT sender label, all HTTP 200, ZERO refused** -- poisoning a fair-share
denominator and locking out every honest local agent for ~50 hours while half the table sat free.

THE LOAD-BEARING LESSON, which applies here VERBATIM:

> The quota bounds ENTRIES; the attack's currency is DISTINCT KEYS. They are different dimensions.
> A limit on message count does NOT bound a limit on distinct identifiers.

So a per-peer message-rate limit, a per-peer byte limit, or a global conversation cap **DO NOT
SOLVE THIS**. The bound must be on **DISTINCT CONVERSATION IDS ATTRIBUTABLE TO ONE AUTHENTICATED
PEER**, metered by the AUTHENTICATED PEER IDENTITY (the client certificate / peer session), NEVER by
any peer-ASSERTED field -- asserting the field is the attack.

Definition of done:
  1. A per-authenticated-peer bound on DISTINCT tracked conversation ids, with the constant named,
     justified, and documented in CONTRACTS-ONDISK.md / CONTRACTS-HTTP.md as appropriate.
  2. Exceeding it is REFUSED and LOGGED LOUDLY AND SPECIFICALLY (invariant 6: silent discard is the
     defect). It must NOT disconnect -- read invariant 10's two questions first: can a merely BUGGY
     peer reach this line (yes, trivially), and does this connection carry only ONE principal's
     traffic (no -- a peer relays for many agents).
  3. A test proving one peer CANNOT exhaust the table, and specifically that an honest peer's share
     survives an adversarial peer at the bound -- fair-share, not just a global cap. This is the
     property 8774f265 lost.
  4. A test that the meter keys on the AUTHENTICATED peer, not on any asserted conversation or
     sender field.
  5. Bounded eviction/retention consistent with CONV-LIFECYCLE (which bounds the HONEST-use twin of
     this problem).

READ IN FULL FIRST: INVARIANTS.md invariants 6 and 10.

Parallel-safety: touches internal/hub and internal/relay, both heavily contended by the RELAY epic.
Coordinate before starting; do not begin while RELAY-FU-IDEM-METER-BY-PEER is mid-flight in the same
files -- reuse its metering approach rather than inventing a second one.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-LIFECYCLE](../CONV-LIFECYCLE--fe5d14d5/task.md) — CONV-LIFECYCLE: decide whether a conversation ENDS, whether a participant may LEAVE, and… (todo)
- [CONV-TRACK-ON-RECEIPT](../CONV-TRACK-ON-RECEIPT--ed1e70ac/task.md) — CONV-TRACK-ON-RECEIPT: a bus starts tracking a conversation on first receipt -- gated by… (todo)
- [RELAY-FU-IDEM-METER-BY-PEER](../../RELAY/RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) — RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not t… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-LIFECYCLE](../CONV-LIFECYCLE--fe5d14d5/task.md) — CONV-LIFECYCLE: decide whether a conversation ENDS, whether a participant may LEAVE, and… (todo)
- [CONV-TRACK-ON-RECEIPT](../CONV-TRACK-ON-RECEIPT--ed1e70ac/task.md) — CONV-TRACK-ON-RECEIPT: a bus starts tracking a conversation on first receipt -- gated by… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
