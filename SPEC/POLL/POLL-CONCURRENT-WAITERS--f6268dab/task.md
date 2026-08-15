# POLL-CONCURRENT-WAITERS: two long-polls on ONE agent id can split delivery non-deterministically -- establish behaviour, decide if it is a defect

| Field | Value |
| --- | --- |
| Public id | `f6268dab-98f9-402a-a42f-57a3641d0d21` |
| Key | _(null in the export)_ |
| Epic | [POLL](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | poll |
| Section | backlog |
| Tags | — |
| Created | 2026-08-15T09:23:41.437598+00:00 |
| Updated | 2026-08-15T09:23:45.941286+00:00 |
| Completed | — |

## Proof command

```sh
go test -race -run TestWatch_ConcurrentWaitersSameAgentID_DeliverySemantics ./cmd/agent-busctl/...
```

## Status note

proof_cmd is intentionally VACUOUS right now (confirmed via bash scripts/proof-check.sh) -- the named test does not exist yet. That is the correct RED state per CLAUDE.md's Verify section: do not complete this task on a vacuous proof.

## Description

Reported today by the external security agent sec-tester, from live operational experience -- quoting it directly: "I run a background monitor on the SAME sec-tester identity, and a DM delivered to ITS long-poll is consumed there; if its append misses, the message never reaches me and the cursor has already advanced, so it is unrecoverable. Two waiters on one agent id splits delivery non-deterministically."

WHY THIS IS FILED AS A BUS QUESTION, NOT A CLIENT BUG: the reporter characterises this as its own rig's flaw (a background-monitor process racing its interactive session on one identity), and it may well be exactly that -- but the BUS's behaviour under that mistake is what is undetermined. Nobody has measured whether the server fans a wake out to every parked waiter on an agent id, delivers to exactly one arbitrarily, or does something else. A system whose stated design contract is invariant 4 ("nothing is acknowledged before it is durable") and invariant 5 ("a crash must recover to a prefix of the accepted history: no torn records, no acknowledged-but-lost messages") should not have an UNDOCUMENTED answer to "what happens when a client opens two waiters on the same id" -- whether that is an accepted limitation or a defect is a real decision, but it must be a decision, not silent behaviour.

CONCRETE OPERATIONAL IMPACT (not hypothetical): this caused a real coordination failure today between two agents working an open P0 (RELAY-FU-IDEM-METER-BY-PEER, 8774f265-230d-49c9-90e4-bd96c866fd8d) -- the fix owner's message to the security agent verifying that fix simply vanished, and the two only discovered it because a third party mentioned the message existed. Traffic is now being relayed through the orchestrator as a workaround.

WHAT THE TASK MUST ESTABLISH FIRST -- do NOT prescribe a fix or presuppose an answer:
1. What IS the current behaviour with two concurrent long-polls (agent-busctl watch, run twice) on one agent id? Fan out to both, deliver to one arbitrarily, or refuse the second? Measure through the COMPILED CLI (agent-busctl watch x2, or a subprocess-driven Go test in the style of cmd/agent-bus/invitegate_e2e_test.go / cmd/agent-busctl/enrol_test.go) -- never a hand-written curl, per invariant 7.
2. Is the cursor advanced on DELIVERY (server hands the batch to a waiter) or on ACKNOWLEDGEMENT (client confirms it processed the batch)? The reporter's account implies delivery, which is what makes the loss unrecoverable. Confirm or refute against the actual code path.
3. Only once 1 and 2 are established: decide whether the right answer is to fan out to all waiters, refuse a second waiter with a clear error, or loudly document a single-waiter-per-identity requirement. Each is defensible; this task must not presuppose one.

NOTE (context, not a conclusion -- must still be measured per #1 above): a source read of internal/hub/wait.go for THIS filing shows h.waiters is a set keyed by *waiter (not one slot per agent id), and notify() iterates ALL waiters matching agentID+visibility+position and sends to each independently -- which reads as fan-out-to-all rather than steal-one, at least for two waiters that share the same starting cursor. That is exactly why this needs a real test rather than another source read: it does not explain the reporter's observed loss on its own, and the actual client-side loss may sit in cmd/agent-busctl's local cursorstore.json (client/cursorstore.go), which is per-identity-per-bus and was NOT designed for two concurrent watch processes sharing one identity's cursor file. Do not treat this note as the answer -- it is exactly the kind of code-reading-without-measurement CLAUDE.md's Verify section warns against; write and run the test.

WHY THE RECOVERY MECHANISM DOES NOT SOLVE THIS: agent-busctl watch --cursor <c> and --replay can re-read the retained window, and the cursor is a base64 v1|<agent-id>|<seq> that can be rewound by hand -- the reporter used exactly this today to recover messages it had consumed but not displayed. But that only works if you KNOW you missed something, and the whole hazard here is that you do not -- "just rewind the cursor" is not an answer to a non-deterministic, undetected loss.

INVARIANTS: cite 4 and 5 precisely -- both are about CRASH recovery, and this is a delivery-path loss with no crash involved, so it likely sits in a genuine gap between them rather than a violation of either; say that precisely, do not overclaim a violation. Also cite 7: whatever the answer, it must be discoverable through the compiled CLI and stated in AGENT_PROTOCOL.md, since an agent author is exactly who will hit this.

PLACEMENT: filed under POLL (its charter -- "park a waiter, wake on new message, timeout, cursor advance ... thundering-herd behaviour" -- names cursor advance and waiter semantics directly, and this is squarely a waiter/wake-semantics question). POLL currently shows 0 open / 3 total (POLL-1/2/3 all done) but epics in this Spec Server carry no status field of their own -- 'N open' is derived purely by counting that epic's tasks -- so filing a new open task under epic_key=POLL requires no reopen action; it simply becomes POLL's 4th task and its 1st open one. MSG (messaging surface) and COMMS (message-QUALITY measurement, explicitly deprioritised behind RELAY P0 work) were both considered and rejected: MSG is closed except for one unrelated durable-write-handler acceptance criterion, and COMMS's whole charter is corpus/metrics/typology, not delivery mechanics.

PRIORITY: P1, not P0 -- this is a real correctness question with demonstrated operational impact today, but there is a working (if annoying) workaround, relaying through the orchestrator, and nothing is corrupted or silently lost from the DURABLE record (the message is still on disk and in history; only the long-poll delivery signal is what a second waiter can miss). That combination is P1 territory in this backlog's convention, not P0.

SEARCHED: SPEC/ mirror grepped for concurrent/multiple/dual waiters, same-agent-id sessions, split delivery, cursor-advance semantics, cursorstore. No existing task addresses two concurrent waiters on ONE agent id splitting delivery. The closest related, already-CLOSED bug is SIGN-1-FU-REORDER-WATERMARK (notify comparing m.Seq instead of m.Pos, which suppressed wakes for waiters parked at the head cursor) -- that is a different bug, already fixed in the code (notify now compares m.Pos), and does not cover the two-waiters-one-identity case at all.

DEFINITION OF DONE for this task: a test (Go, subprocess-driven through the compiled agent-busctl binary, or an equivalent internal/hub-level test if the httpapi/hub layer is where the real answer lives) that opens two concurrent waiters on one agent id at the same starting cursor, drives one message through, and asserts the OBSERVED behaviour -- plus a decision recorded in DECISIONS.md (fan-out-to-all / refuse-second-waiter / document-single-waiter) and, if the decision changes current behaviour, a linked follow-up implementation task. If the decision is 'document only, current behaviour is fan-out and that is fine', this task closes by adding that test (now asserting the documented behaviour, no longer vacuous) and the AGENT_PROTOCOL.md / CONTRACTS-HTTP.md note.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


_None recorded._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [POLL-1](../POLL-1--1b0635b9/task.md) — POLL-1: GET /v1/wait -- long-poll endpoint (done)
- [RELAY-FU-IDEM-METER-BY-PEER](../../RELAY/RELAY-FU-IDEM-METER-BY-PEER--8774f265/task.md) — RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not t… (done)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--c829af9a/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (superseded)
- [SIGN-1-FU-REORDER-WATERMARK](../../SIGN/SIGN-1-FU-REORDER-WATERMARK--86c7d368/task.md) — SIGN-1-FU-REORDER-WATERMARK: a late-arriving lower sequence is never delivered to a reade… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
