# RELAY-52-FU-HUBDISCARDS: remaining untested hub/mint/roster discard-and-recovery log lines

| Field | Value |
| --- | --- |
| Public id | `d2cad9e7-cbf2-4cab-b0cd-6ddb1ed5433a` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P2 |
| Component | BE |
| Section | backlog |
| Tags | — |
| Created | 2026-08-21T10:29:10.547619+00:00 |
| Updated | 2026-08-23T09:50:27.217287+00:00 |
| Completed | 2026-08-23T09:50:27.217270+00:00 |

## Proof command

```sh
go test -race -run 'TestMessageRecordThatCannotBeAppliedIsDiscardedLoudlyAndTheBusStarts|TestAppliedKeyRecordThatCannotBeDecodedIsDiscardedLoudlyAndTheMessageSurvives|TestPreIdem11AppliedKeyThatCannotBeRememberedIsDiscardedLoudly|TestMessageRecordDiscardLinesAreNotInterchangeable' ./internal/hub
```

## Status note

mint.go:559 and :567 (sequence-floor discards) remain UNTESTED by any test in the tree -- grep -rn 'DISCARDING a sequence-floor record' --include=*_test.go . returns nothing. proof_cmd replaced 2026-08-21 (spec-keeper): was a bare grep (verdict=PASS class=file-assertion tests_run=0, proves existence not testedness); now go test -race -run naming the four tests in internal/hub/discard_relay52fu_test.go covering hub.go:1126/1187/1207 (1240 documented unreachable-by-construction). Task stays open until mint.go:559/:567 are covered.

## Description

FILED 2026-08-21 by spec-keeper, split out of RELAY-52 (67c6248d-c611-4a3b-85ad-97cdd7c4cb20) while
correcting that task's premise. RELAY-52 itself covers only the internal/hub/hub.go:1104
undecodable-message-record discard and its cap-reached line at hub.go:1110.

This follow-up tracks the REST of the untested discard/recovery-summary log lines found in the same
grep audit (every DISCARDING/recovery-summary line in internal/hub and internal/relay's mint/roster
plane, checked against every *_test.go in the repo, at HEAD 14ed009):

  - internal/hub/hub.go:1126 -- apply-failure discard. The string is pinned as const
    discardOnApplyLine at internal/hub/outoforder_poison_sign1fu_test.go:56, but its ONLY use
    (line 208) asserts the line is ABSENT in that scenario. There is no test that this line is ever
    actually EMITTED, nor that it names the offending record specifically enough to act on --
    weaker than "tested", since a test that only proves absence in one scenario cannot catch the
    line going silent or vague in the scenario where it SHOULD fire.
  - internal/hub/hub.go:1187 -- applied-key could not be remembered (discard). No test.
  - internal/hub/hub.go:1207 -- applied-key could not be decoded (discard). No test.
  - internal/hub/hub.go:1240 -- applied-key rebuilt from pre-IDEM-11 record (compat/discard path).
    No test.
  - internal/hub/mint.go:559 -- sequence-floor record discard. No test.
  - internal/hub/mint.go:567 -- sequence-floor record discard (second branch). No test.
  - internal/hub/roster.go:351 -- incomplete-input recovery summary line. No test.

By contrast the equivalent discard lines in internal/auth and internal/invite ARE tested, so this
gap is specific to the hub/mint/roster plane.

# Why it matters

Same invariant-6 rationale as the parent: recovery always reaches a running server, but every
discard must be logged loudly and specifically, and the log is the ONLY evidence a record was
dropped. An untested discard line can go silent, go vague, or stop firing and nothing catches it.

# Scope

For each line above: a positive test that drives the code down that exact branch, asserts the bus
(or recovery pass) still reaches a running/completed state, and asserts the log line fires and
names the record/summary specifically enough to act on. Standard of evidence per CLAUDE.md: each
test must be observed RED first (comment out or mutate the log line, confirm the test fails) before
it is trusted -- log-line assertions are unusually prone to matching something incidental.

This is deliberately left as ONE follow-up rather than seven, since triage may want to size/split it
further; whoever picks it up should feel free to file per-line sub-tasks if that is a better shape.

# Related
  RELAY-52 (67c6248d-c611-4a3b-85ad-97cdd7c4cb20) -- parent task; corrected premise covers only
  hub.go:1104/1110.
  RELAY-48 (9887b0eb) -- the durability fix whose gate re-verification originally surfaced this
  whole family of gaps.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **follow-up** [RELAY-52](../RELAY-52--67c6248d/task.md)
- **follow-up of** [RELAY-52-FU-HUBDISCARDS-FU-APPLIEDSEQ-XCHECK](../RELAY-52-FU-HUBDISCARDS-FU-APPLIEDSEQ-XCHECK--2c7da802/task.md)
- **follow-up of** [RELAY-52-FU-HUBDISCARDS-FU-APPLYDISCARD-UNCOUNTED](../RELAY-52-FU-HUBDISCARDS-FU-APPLYDISCARD-UNCOUNTED--52196a49/task.md)
- **follow-up of** [RELAY-52-FU-HUBDISCARDS-FU-IDEMDISCARD-UNCAPPED](../RELAY-52-FU-HUBDISCARDS-FU-IDEMDISCARD-UNCAPPED--d858bf19/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [IDEM-11](../../IDEM/IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [RELAY-48](../RELAY-48--9887b0eb/task.md) — RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at… (done)
- [RELAY-52](../RELAY-52--67c6248d/task.md) — RELAY-52: invariant 6's loud-discard line at hub.go:1104 has no test anywhere in the repo (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-52](../RELAY-52--67c6248d/task.md) — RELAY-52: invariant 6's loud-discard line at hub.go:1104 has no test anywhere in the repo (done)
- [RELAY-52-FU-HUBDISCARDS-FU-APPLIEDSEQ-XCHECK](../RELAY-52-FU-HUBDISCARDS-FU-APPLIEDSEQ-XCHECK--2c7da802/task.md) — RELAY-52-FU-HUBDISCARDS-FU-APPLIEDSEQ-XCHECK: cross-check test fixture appliedSeq against… (todo)
- [RELAY-52-FU-HUBDISCARDS-FU-APPLYDISCARD-UNCOUNTED](../RELAY-52-FU-HUBDISCARDS-FU-APPLYDISCARD-UNCOUNTED--52196a49/task.md) — RELAY-52-FU-HUBDISCARDS-FU-APPLYDISCARD-UNCOUNTED: apply-branch discard increments no cou… (done)
- [RELAY-52-FU-HUBDISCARDS-FU-IDEMDISCARD-UNCAPPED](../RELAY-52-FU-HUBDISCARDS-FU-IDEMDISCARD-UNCAPPED--d858bf19/task.md) — RELAY-52-FU-HUBDISCARDS-FU-IDEMDISCARD-UNCAPPED: the two applied-key discard lines are un… (todo)
- [TRIAGE-LOCK](../../PROCESS/TRIAGE-LOCK--25f0eac6/task.md) — TRIAGE-LOCK: backlog-triage mutex (in_progress)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
