# RELAY-25-FU-CORRELATION: fed-smoke.sh asserts the SAME message_id string in A's, B's and C's audits -- unsatisfiable by construction (invariant 1), and it is the ONLY reason the script exits 1

| Field | Value |
| --- | --- |
| Public id | `3f009222-e31e-404a-9c77-3e7966741b82` |
| Key | RELAY-25-FU-CORRELATION |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P1 |
| Component | tooling |
| Section | backlog |
| Tags | relay, fed-smoke, test-harness, invariant-1, relay-47-followup |
| Created | 2026-08-15T12:56:32.608122+00:00 |
| Updated | 2026-08-23T10:06:35.999616+00:00 |
| Completed | 2026-08-23T10:06:35.999600+00:00 |

## Proof command

```sh
bash scripts/fed-smoke.sh
```

## Description

Filed 2026-08-15 by spec-keeper on behalf of the RELAY-47 feature-runner. **THIS BELONGS TO RELAY-25** (public_id 10491a01-30ae-4699-b5f1-a1993e026dd8, in_progress, owner codex-1): `scripts/fed-smoke.sh` is RELAY-25's file, and RELAY-47 did not touch it. The same text has been posted as a `kind=report` note on RELAY-25 so its owner sees it.

== READ THIS BEFORE CONCLUDING ONWARD RELAY IS BROKEN ==

MEASURED at HEAD 9701611 plus RELAY-47's change: **the three-hop delivery genuinely WORKS.**

- C's audit holds exactly ONE record with the COMPLETE `bus_path=[A,B,C]`;
- C's recipient agent ACTUALLY RECEIVED the message through `agent-busctl watch` (text `relay-25:a-to-c:v1`);
- A, B and C each hold exactly ONE record -- no duplication;
- the idempotent retry created NO second copy.

The script still exits 1, and this task is the ONLY reason.

== THE UNSATISFIABLE ASSERTION ==

`scripts/fed-smoke.sh` fails at :465 because it asserts the **SAME `message_id` STRING** appears in A's, B's and C's audits (assertion sites :450, :461, :474-480, :495-497). **Each bus MINTS ITS OWN id and never adopts a peer's -- invariant 1.** The observed ids for ONE logical message were:

    A: bus-A-11     B: bus-B-11     C: bus-C-9

So the assertion is **unsatisfiable by construction, not merely unmet**. No amount of relay work makes it pass; it is asserting a violation of invariant 1.

Worse, there is nothing to correlate on today: `wal.AuditRecord.MessageID` is `m.ID` -- the LOCAL id. `store.Message.OriginMessageID` is the DESIGNED correlation key but **has no production writer** (see RELAY-48, which is blocked on the same missing plumbing).

== FIX -- CHOOSE ONE ==

(a) **Correlate per bus** (smaller, harness-only): assert A by its OWN minted id; assert B and C by `sender` + `recipient` + `content_sha256` + the expected `bus_path` PREFIX. Stays inside `scripts/fed-smoke.sh`.

(b) **Add an origin-correlation field to the audit surface** and correlate on it. Bigger: this is on-disk surface, needs a DECISIONS.md entry and a RESERVED record/field number (`POST /reservations` -- never eyeballed), and overlaps RELAY-48's durable-correlation work. If (b) is chosen, sequence it WITH RELAY-48 rather than beside it.

== CONSEQUENCE FOR RELAY-47 ==

**RELAY-47's `proof_cmd` is `bash scripts/fed-smoke.sh`, and it therefore CANNOT go green until this task is fixed.** State plainly wherever that matters: **this is NOT evidence against RELAY-47.** The delivery it wired was observed working end to end, including the agent-side receive; the red exit is a harness assertion that contradicts invariant 1.

== PROOF ==

    bash scripts/fed-smoke.sh

exits 0, with the three-hop delivery still ASSERTED (not weakened into a no-op). Whichever correlation is chosen must still fail if C's `bus_path` is short, if C's agent never receives the text, or if any bus holds two records.

RELATES: RELAY-25 (owner), RELAY-47, RELAY-48, RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [RELAY-25-FU-CORRELATION-FU-AGENTDOCS](../RELAY-25-FU-CORRELATION-FU-AGENTDOCS--6a4f6f47/task.md)
- **follow-up of** [RELAY-25](../RELAY-25--10491a01/task.md)
- **relates to** [RELAY-25-FU-CORRELATION-FU-WATCHLINK](../RELAY-25-FU-CORRELATION-FU-WATCHLINK--037a9860/task.md)
- **relates to** [RELAY-25-FU-CORRELATION-FU-DAMAGEDIAG](../RELAY-25-FU-CORRELATION-FU-DAMAGEDIAG--273b43fe/task.md)
- **relates to** [RELAY-25-FU-CORRELATION-FU-GUARDFIELD](../RELAY-25-FU-CORRELATION-FU-GUARDFIELD--44601098/task.md)
- **relates to** [RELAY-25-FU-CORRELATION-FU-AGENTDOCS](../RELAY-25-FU-CORRELATION-FU-AGENTDOCS--6a4f6f47/task.md)
- **relates to** [RELAY-47](../RELAY-47--dd69c4d3/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE](../RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE--3e96dae2/task.md) — RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE: three-bus federation smoke test (fed-smoke.sh, both… (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (done)
- [RELAY-47](../RELAY-47--dd69c4d3/task.md) — RELAY-47: ONWARD RELAY -- WIRE an intermediate bus to forward a relayed message to a THIR… (done)
- [RELAY-48](../RELAY-48--9887b0eb/task.md) — RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-25-FU-CORRELATION-FU-AGENTDOCS](../RELAY-25-FU-CORRELATION-FU-AGENTDOCS--6a4f6f47/task.md) — RELAY-25-FU-CORRELATION-FU-AGENTDOCS: CONTRACTS-AGENT.md still says fed-smoke.sh is 'expe… (todo)
- [RELAY-25-FU-CORRELATION-FU-DAMAGEDIAG](../RELAY-25-FU-CORRELATION-FU-DAMAGEDIAG--273b43fe/task.md) — RELAY-25-FU-CORRELATION-FU-DAMAGEDIAG: fed-smoke reports a DAMAGED audit as 'relay not es… (todo)
- [RELAY-25-FU-CORRELATION-FU-GUARDFIELD](../RELAY-25-FU-CORRELATION-FU-GUARDFIELD--44601098/task.md) — RELAY-25-FU-CORRELATION-FU-GUARDFIELD: the 'no message_id on non-record lines' guard name… (todo)
- [RELAY-25-FU-CORRELATION-FU-WATCHLINK](../RELAY-25-FU-CORRELATION-FU-WATCHLINK--037a9860/task.md) — RELAY-25-FU-CORRELATION-FU-WATCHLINK: link the recipient's watch record to C's audit reco… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
