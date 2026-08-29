# Define measurable message-quality metrics against the corpus, honestly denominated

| Field | Value |
| --- | --- |
| Public id | `ee18aa5f-d16a-48b8-812d-2068a4982f54` |
| Key | COMMS-METRICS |
| Epic | [COMMS](../epic.md) |
| Status | cancelled |
| Priority | P2 |
| Component | comms |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T17:11:59.998122+00:00 |
| Updated | 2026-08-29T14:26:34.376528+00:00 |
| Completed | — |

## Proof command

```sh
test -f docs/comms/METRICS.md && [ "$(grep -c 'NOT COMPUTABLE' docs/comms/METRICS.md)" -ge 1 ] && echo METRICS_OK
```

## Status note

operator decision 2026-08-29: COMMS epic dropped in favour of CONV golden path

## Description

Produce docs/comms/METRICS.md defining the metrics this epic will use to judge message quality
(e.g. verdict-class clarity, section-header usage rate, time-to-verdict, convergence-on-first-line
rate, structure-use rate) against the COMMS-CORPUS extract. Each metric must state its exact
denominator (per the corpus corrections recorded on this epic -- 53 texted messages, not 60 or 54,
unless a metric is explicitly scoped otherwise) and its formula.

CRITICAL REQUIREMENT, non-negotiable: at least one metric MUST be marked NOT COMPUTABLE against the
current corpus, with the specific reason (missing field, insufficient sample, requires data this
bus does not record e.g. per invariant 6's metadata-only audit log). A metrics document in which
everything is conveniently computable has not been honest about what the corpus actually contains
-- this mirrors the epic's own founding-claim corrections (COMMS-CORPUS) and must not repeat the
mistake it exists to fix.

Practices to ADOPT BY DECISION rather than measure (per the planner's recommendation -- record each
via a dated DECISIONS.md entry with a stated falsifier, not by running an experiment to "prove" it):
reporting negatives (a metric that finds nothing is reported, not omitted), honest denominators
(state N explicitly, every time), verdict-class precision (define PASS/FAIL/CHANGES/etc. exactly,
do not eyeball), provenance marking (every corpus row keeps its source), and naming the confound
whenever a metric's result could be explained by something other than the thing being measured.

Hand-labelling for this task must be done by an agent that is NOT claude-code-agent-bus-1 (the
orchestrator is a subject in every measured exchange, so it cannot also be the measurer), and the
labelling key/rubric must be committed to git BEFORE the scorer script is written -- this is one of
the epic's named measuring-the-instrument threats.

Definition of done:
  1. docs/comms/METRICS.md: each metric with formula, denominator, and COMPUTABLE / NOT COMPUTABLE
     status (>=1 NOT COMPUTABLE, with reason).
  2. A committed labelling key/rubric predating any scorer code (git log timestamps must show the
     rubric commit before the scorer commit).
  3. The five decide-vs-measure practices above adopted as dated DECISIONS.md entries with
     falsifiers.

Parallel-safety: needs COMMS-CORPUS's output and COMMS-CONSENT's resolved per-agent record before
labelling any specific agent's messages. Otherwise standalone (no live bus calls).

Depends on: COMMS-CORPUS, COMMS-CONSENT.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocked by** [COMMS-CONSENT](../COMMS-CONSENT--1ae2403b/task.md)
- **blocked by** [COMMS-CORPUS](../COMMS-CORPUS--075d0c32/task.md)
- **blocks** [COMMS-DOC](../COMMS-DOC--d899d622/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [COMMS-CONSENT](../COMMS-CONSENT--1ae2403b/task.md) — Ask consent from the non-orchestrator agents whose messages are in the corpus (cancelled)
- [COMMS-CORPUS](../COMMS-CORPUS--075d0c32/task.md) — Extract a real inter-agent message corpus (mechanical, not asserted) (cancelled)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [COMMS-CONSENT](../COMMS-CONSENT--1ae2403b/task.md) — Ask consent from the non-orchestrator agents whose messages are in the corpus (cancelled)
- [COMMS-CORPUS](../COMMS-CORPUS--075d0c32/task.md) — Extract a real inter-agent message corpus (mechanical, not asserted) (cancelled)
- [COMMS-DOC](../COMMS-DOC--d899d622/task.md) — Write up the COMMS epic findings and recommendations (cancelled)
- [COMMS-READER](../COMMS-READER--07a4aa0c/task.md) — Build a corpus reader tool for message-exchange review (cancelled)
- [COMMS-STRUCT](../COMMS-STRUCT--6829b61c/task.md) — Measure whether heavy message structure pays off -- pre-registered, mechanically ordered (cancelled)
- [COMMS-TYPES](../COMMS-TYPES--f17ec5ab/task.md) — Define a message verdict-class / type taxonomy from measured corpus usage (cancelled)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
