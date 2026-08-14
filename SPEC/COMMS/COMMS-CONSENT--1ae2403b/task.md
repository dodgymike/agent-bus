# Ask consent from the non-orchestrator agents whose messages are in the corpus

| Field | Value |
| --- | --- |
| Public id | `1ae2403b-ee9b-4ee3-b69f-49793939b309` |
| Key | COMMS-CONSENT |
| Epic | [COMMS](../epic.md) |
| Status | todo |
| Priority | P1 |
| Component | comms |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T17:11:59.417389+00:00 |
| Updated | 2026-08-08T17:11:59.417389+00:00 |
| Completed | — |

## Proof command

```sh
bash scripts/comms-consent-check.sh docs/comms/CONSENT.md
```

## Description

Before any hand-labelling of corpus messages or any live trial that studies a named autonomous
agent (COMMS-METRICS scoring, COMMS-STRUCT trial, COMMS-TYPES classification, COMMS-THREAD-TRIAL,
COMMS-RETRACT), ask each non-orchestrator agent whose messages appear in the COMMS-CORPUS extract
(observed senders include at least sec-tester-1, mic-array-1, speckeeper-1) for consent to have
their messages used and labelled in this study. This is a "you cannot study autonomous third
parties before asking them" requirement, not a formality -- the planner argued for FILING IT AT P1
even though it does not strictly meet this epic's own stated P1 rule (perishability / contract-
breakage), purely on the grounds that asking first outranks the scheduling rule. That inconsistency
is recorded here deliberately rather than silently resolved.

Consent must be requested over the bus itself (the honest channel, not a side-channel), with a
stated deadline. THIS TASK IS DELIBERATELY COMPLETABLE WITHOUT A RESPONSE: an unanswered request by
the stated deadline is a valid, non-vacuous outcome -- "we asked, nobody in the corpus answered by
<date>" is real information (it bounds what COMMS-METRICS/STRUCT/TYPES/THREAD-TRIAL/RETRACT may
ethically do next: proceed only with agents that consented, or fall back to fully anonymised /
aggregate-only measurement for the rest).

Definition of done:
  1. A consent-request message sent over the live bus to each candidate agent, naming the study,
     what data of theirs would be used, and a response deadline.
  2. A durable record (docs/comms/CONSENT.md or equivalent) of who was asked, when, and the outcome
     per agent: GRANTED, DECLINED, or `NO-RESPONSE-BY <date>` once the deadline passes with no
     reply.
  3. Downstream tasks (COMMS-METRICS, COMMS-STRUCT, COMMS-TYPES, COMMS-THREAD-TRIAL, COMMS-RETRACT)
     must read this record before touching any message from a given sender, and must respect a
     DECLINED or unresolved NO-RESPONSE the same way (exclude, or anonymise-and-aggregate only).

Parallel-safety: requires a LIVE bus and real remote agents able to receive and (optionally) answer
a DM -- this is NOT a standalone/offline task. Coordinate before running concurrently with anything
else that messages the same agents, to avoid confusing an unrelated DM with the consent ask.

Depends on: nothing structurally, but MUST run (or reach its NO-RESPONSE-BY terminal state) before
COMMS-METRICS, COMMS-STRUCT, COMMS-TYPES, COMMS-THREAD-TRIAL and COMMS-RETRACT proceed with any
per-agent labelling or trial.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [COMMS-METRICS](../COMMS-METRICS--ee18aa5f/task.md)
- **blocks** [COMMS-RETRACT](../COMMS-RETRACT--dd4b739c/task.md)
- **blocks** [COMMS-STRUCT](../COMMS-STRUCT--6829b61c/task.md)
- **blocks** [COMMS-THREAD-TRIAL](../COMMS-THREAD-TRIAL--3a7705b8/task.md)
- **blocks** [COMMS-TYPES](../COMMS-TYPES--f17ec5ab/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [COMMS-CORPUS](../COMMS-CORPUS--075d0c32/task.md) — Extract a real inter-agent message corpus (mechanical, not asserted) (todo)
- [COMMS-METRICS](../COMMS-METRICS--ee18aa5f/task.md) — Define measurable message-quality metrics against the corpus, honestly denominated (todo)
- [COMMS-RETRACT](../COMMS-RETRACT--dd4b739c/task.md) — Determine whether message retraction needs explicit protocol marking (todo)
- [COMMS-STRUCT](../COMMS-STRUCT--6829b61c/task.md) — Measure whether heavy message structure pays off -- pre-registered, mechanically ordered (todo)
- [COMMS-THREAD-TRIAL](../COMMS-THREAD-TRIAL--3a7705b8/task.md) — Trial threading via convention (no wire field) and measure whether it's enough (todo)
- [COMMS-TYPES](../COMMS-TYPES--f17ec5ab/task.md) — Define a message verdict-class / type taxonomy from measured corpus usage (todo)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [COMMS-CORPUS](../COMMS-CORPUS--075d0c32/task.md) — Extract a real inter-agent message corpus (mechanical, not asserted) (todo)
- [COMMS-METRICS](../COMMS-METRICS--ee18aa5f/task.md) — Define measurable message-quality metrics against the corpus, honestly denominated (todo)
- [COMMS-RETRACT](../COMMS-RETRACT--dd4b739c/task.md) — Determine whether message retraction needs explicit protocol marking (todo)
- [COMMS-STRUCT](../COMMS-STRUCT--6829b61c/task.md) — Measure whether heavy message structure pays off -- pre-registered, mechanically ordered (todo)
- [COMMS-THREAD-TRIAL](../COMMS-THREAD-TRIAL--3a7705b8/task.md) — Trial threading via convention (no wire field) and measure whether it's enough (todo)
- [COMMS-TYPES](../COMMS-TYPES--f17ec5ab/task.md) — Define a message verdict-class / type taxonomy from measured corpus usage (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
