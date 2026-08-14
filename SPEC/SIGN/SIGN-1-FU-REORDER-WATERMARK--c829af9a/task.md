# SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reader whose cursor has already passed it

| Field | Value |
| --- | --- |
| Public id | `c829af9a-4418-437a-a0f8-34ef2f5d15d0` |
| Key | _(null in the export)_ |
| Epic | [SIGN](../epic.md) |
| Status | superseded |
| Priority | P0 |
| Component | hub |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T16:04:24.541359+00:00 |
| Updated | 2026-08-14T17:15:27.689535+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run 'TestReorderWatermark' ./internal/hub ./internal/store
```

## Status note

SUPERSEDED 2026-08-14 by SIGN-1-FU-REORDER-WATERMARK (86c7d368-9733-434e-848d-05dd12fecf3a), a straight recreation with identical content SOLELY to fix key=null on a task quoted four times in committed comments and once in a shipped production WARN log line (commit 800fe25) -- an operational defect, not a duplicate-filing incident. PATCH .../tasks/<id> rejects key as an unknown field (422 Unknown field, confirmed live) -- key is create-only server-side, no in-place fix exists. Recreate-and-supersede was the remedy; GET /tasks/SIGN-1-FU-REORDER-WATERMARK now resolves to the new task, closing the operator-facing gap. This task carried zero relations to re-wire (verified before recreating). Resolve by the new public_id going forward.

## Description

Filed 2026-08-14 by the SIGN-1-FU-OUTOFORDER-POISON reviewer gate. This is the SECOND HALF of that defect. SIGN-1-FU-OUTOFORDER-POISON fixed the HALT (store.Append refused a late lower sequence after the record was already committed and fsynced, which poisoned the hub permanently). It deliberately did NOT close this, and internal/store/store.go's "KNOWN GAP" section points here.

THE GAP: a cursor is a SEQUENCE. store.Since binary-searches for the first retained message strictly AFTER the cursor. Since SIGN-1 the sequence is minted before the client signs and sends, so a message can land BELOW a cursor that has already advanced past that position -- and that reader never receives it. The message is durable, it is in the audit trail, and it is served to every cursor still below it. It is simply never delivered to the readers who polled in between.

WORSE THAN "readers who polled in between", per the reviewer probe: an actively LONG-POLLING reader parked at the head cursor misses it permanently AND ITS LONG POLL NEVER WAKES -- internal/hub/wait.go reads through store.Since with the cursor at every wake point. A parked watcher is the NORMAL state of an agent on this bus, so this is the common case, not a rare race.

THIS IS NOT A REGRESSION INTRODUCED BY THE FIX. It is a pre-existing consequence of SIGN-1's reserve-then-send that the poison was MASKING: before the fix the bus stopped dead instead of skipping a message, so the gap could not be observed. Trading a whole-bus halt any enrolled agent could trigger for a missed delivery is the right way round, but it is not the end state.

SECURITY ANGLE TO SETTLE IN THIS TASK: assess whether this is a targeted message-SUPPRESSION primitive. An agent that mints a low sequence, holds it, and times its send can plausibly choose a message that a chosen recipient never receives. Quantify reliability and what the attacker must observe. hub.MintTTL (15 min) sets the window size.

THE FIX, sketched: a REORDER WATERMARK W -- "no sequence <= W can still arrive" -- derivable ONLY from the hub's outstanding-mint table (a reservation is outstanding until spent or TTL-expired), so W = min(seq of outstanding mints) - 1, or the store head when none are outstanding. internal/store cannot compute it and must not be given a back channel; the hub must push it down. Candidate delivery rule: keep serving everything, but CLAMP the returned cursor to max(W, after) so a reader re-reads the reorder window and picks up a late arrival. Delivery is documented AT-LEAST-ONCE (AGENT_PROTOCOL.md), so the resulting redelivery is within contract, and the redelivery is bounded by the outstanding-mint set. Consider and reject or accept the alternative of head-of-line blocking above W (a long-held mint would stall delivery for up to MintTTL).

ALSO IN SCOPE: AGENT_PROTOCOL.md around lines 716-726 tells agents the read cursor "never skips". That is not currently true in the reorder window. Either close the gap or qualify that sentence -- do not leave it as written.

BOUNDARY NOTE: shortening hub.MintTTL narrows the window and closes nothing. It is mitigation, not a fix.

---
APPENDED 2026-08-14 — SECURITY GATE RULING (from the SIGN-1-FU-OUTOFORDER-POISON re-audit).

Severity HIGH, and reproduced live in a clean overlay with one enrolled agent, two mints and two sends, no timing skill required:
  carol cursor now 2 / bus ACKED the suppressed message: id=testbus-1 seq=1 / SUPPRESSED: carol receives 0 further messages, testbus-1 is NEVER delivered to her / hub log says nothing about it at all.

WHY IT IS WORSE THAN "a reader who happened to poll in the window": hub.notify skips any waiter with m.Seq <= w.after (internal/hub/wait.go:81-96), so a recipient LONG-POLLING AT THE HEAD CURSOR -- the normal mode of every agent on this bus -- is never woken and its next Since never returns the message. Reliability is 100% against any actively-polling recipient. The bus returns success WITH A MESSAGE ID and the record is durable and in the audit trail, so this is a REPUDIATION / FALSE-ACK primitive, not merely a lost message. Third-party censorship of ANOTHER agent's send is opportunistic: it needs the victim to hold an outstanding mint while the attacker's higher sequence is consumed, which is realistic against clients that pipeline mints or retry after a transient refusal.

THIS BLOCKS RELAY INGEST (RELAY-24 AND RELAY-25). hub.IngestRelayed has landed in main (e7a3c49) and is deliberately UNWIRED. Wiring it adds a store.Append caller whose sequence is NOT drawn from the local mint table, which is the property every no-duplicate argument currently rests on. Do not wire a served relay.NewAcceptor until this task is closed and security has re-ruled.

MintTTL is NOT a fix here. It does nothing against the deterministic self-suppression case (the attacker controls a millisecond-scale window); it narrows only accidental reordering and the prune race below.

ALSO IN SCOPE (security MEDIUM/LOW, same root cause) -- THE PRUNE RACE: pushing roughly 1 GiB / 16384 max-size sends past a victim's outstanding mint makes the victim's send accepted, ACKED and retained by NOBODY. Reproduced with MaxBytes=1: carol saw 0 messages from cursor 0. store.Append's at-or-below-prunedHead branch is where it lands. This IS the one place shortening hub.MintTTL is real mitigation.

WHAT SHIPPED IN SIGN-1-FU-OUTOFORDER-POISON, so this task does not redo it: the HALT is closed; store.Append now inserts late arrivals in order, refuses true duplicates with store.ErrDuplicateSequence, never rewinds head, and LOGS at WARN both on the ordinary late insert (m.Seq < head) and on the at-or-below-prunedHead non-retention. The logging is the invariant-6 half only -- it makes the suppression observable to an OPERATOR. It does nothing for the RECIPIENT, which is what this task must fix.
---


---
BLOCKING: THIS TASK BLOCKS RELAY-24 AND RELAY-25. hub.IngestRelayed landed in e7a3c49 and is deliberately UNWIRED; it must not be wired into a served relay.NewAcceptor until this task closes. Verified live 2026-08-14 by the security gate in a clean overlay of d71d5f5: IngestRelayed allocates its LOCAL sequence inside publish via mintRelayedSeqLocked -> h.seq.Next() (internal/hub/relayingest.go:600), not from the mint table, and always above the store head -- so a relayed message never itself becomes a late insert and never re-opens the halt. What it DOES do is let a PEER BUS advance the local head at will with no reservation of its own, which extends the suppression primitive this task describes from "any enrolled LOCAL agent" to "any peered bus". Probe: alice held mint seq=1, one IngestRelayed took seq=2, alice's later send was ACKED and durable, and carol polling from cursor=2 never received it; a peer then advanced the head 5 more times with no local mint.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **superseded by** SIGN-1-FU-REORDER-WATERMARK (unresolved)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../../RELAY/RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (todo)
- [RELAY-25](../../RELAY/RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)
- [SIGN-1](../SIGN-1--43fd21ae/task.md) — SIGN-1: Canonical signing format for messages (Ed25519 detached signatures) (done)
- [SIGN-1-FU-OUTOFORDER-POISON](../SIGN-1-FU-OUTOFORDER-POISON--bbd81523/task.md) — SIGN-1-FU-OUTOFORDER-POISON: Reserve-then-send lets mints be spent out of order, which pe… (done)
- [SIGN-1-FU-REORDER-WATERMARK](../SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-14](../../IDEM/IDEM-14--b0facce9/task.md) — IDEM-14: Idempotency violation path -- key reuse with a different payload rejects, logs a… (todo)
- [RATCHET-2](../../RATCHET/RATCHET-2--ade31a62/task.md) — RATCHET-2: Threat model -- what Ed25519 signing defends against, and explicitly what it d… (todo)
- [SIGN-1-FU-REORDER-WATERMARK](../SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)
- [SIGN-4](../SIGN-4--33fa35d8/task.md) — SIGN-4: Replay/freshness -- enforced SERVER-SIDE at ingest, never by recipient-side seque… (todo)
- [SWEEP-TWO-PASS-DISCIPLINE](../../PROCESS/SWEEP-TWO-PASS-DISCIPLINE--268a0c73/task.md) — SWEEP-TWO-PASS-DISCIPLINE: a contract change needs a MECHANISM sweep and a PROSE sweep, n… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
