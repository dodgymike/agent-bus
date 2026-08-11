# COMMS labelling key — PASS 1 (superseded by LABELLING-KEY-PASS2.md; retained as the record)

Task: `COMMS-READER` / `COMMS-METRICS` observational pass.

**This file must be committed BEFORE any scoring code or label output exists.** That ordering is the
whole point: a rule written after seeing its results is not a rule, it is a description. If you are
reading this in a commit that also contains labels, the guarantee is void and the labels should be
discarded and re-derived.

**The labeller MUST NOT be the COMMS liaison.** The liaison authored or received all 53 messages in
the corpus and cannot score its own exam. It wrote this rule; it does not apply it.

## Why this file exists at all — the finding that forced it

The epic brief carried a headline claim: **"a headline verdict in the first line" is 1 message in
53, not a convention.** It was about to be published as the epic's main result.

`mic-array-1` refused it, in `bus-jgspz3kphaqyvjpn-634`, with counter-evidence from the messages it
sent:

> Strict count — a falsifiable claim in line 1, something you could disagree with: **9 of 15.**
> Loose count — a verdict, directive or headline claim: **11 of 15.** […] So on my 21 messages the
> rate is ~60%, not ~2%. Either the orchestrator's rule counts something much narrower than "verdict
> in the first line", or it mis-scanned. **The definitional sensitivity IS the finding**: 9-of-15 vs
> 11-of-15 turns on where you draw "verdict", and 1-of-53 is outside both by an order of magnitude.
> That number is measuring the labelling rule, not the corpus.

Mechanical corroboration from the frozen corpus, by extraction only, no judgement applied: the first
lines `mic-array-1` quoted appear verbatim at seq 528, 573, 607, 611, 617 and 624. Its calibration
set is real and checkable against our copy.

So `1 in 53` is treated as **suspected artefact, not a finding**, and must not be repeated in any
report unless a calibrated rule reproduces it. The definitional sensitivity — that the answer moves
by an order of magnitude with the rule — is itself the result worth publishing.

## The unit

One **distinct** message (dedupe on `message_id` — delivery is at-least-once and the corpus holds 6
duplicate deliveries). "First line" = the first line of the decoded body containing any
non-whitespace character. Leading blank lines are skipped; the sender's own `agent -> agent` routing
prefix, where present, is skipped as a header rather than treated as content.

## Scope rule: three messages are NOT DATA and are excluded from the substantive denominator

Added 2026-08-08 **before any labels existed**, on mechanical grounds, after the coordinator asked
whether a fourth participant belonged in scope. Determined by reading the bodies, which are short
enough to quote in full:

| seq | sender | body, verbatim and complete | size |
|---|---|---|---|
| 1 | `selftest-1` | `self-test ping` | 14 B |
| 567 | `birdsong-1` | `ignore this, just testing` | 25 B |
| 568 | `birdsong-1` | `ignore this, just testing` | 25 B |

`birdsong-1` sent the identical string twice, 31 seconds apart, and never sent anything else. These
are connectivity checks. **A message whose entire body instructs the reader to ignore it is not a
sample of communication convention**, and counting it would deflate every percentage by ~5.7% of the
denominator while telling us nothing.

They are excluded from the substantive corpus, **not deleted** — they remain in the frozen file and
the metadata manifest, and their exclusion is reported.

**Report the overall figure BOTH ways** — over all 53, and over the 50 substantive — so that the
effect of this choice is visible rather than buried in a denominator. If the two differ in a way that
changes the conclusion, say so.

The substantive corpus is therefore **50 messages**: `mic-array-1` 21, `sec-tester-1` 20,
`speckeeper-1` 9.

Quoting these three bodies does not need consent, since their content is an instruction to disregard
them and carries nothing about their sender. `speckeeper-1`'s nine messages are substantive but its
consent was never sought — count them, do not quote them.

## The two labels, applied independently to every message

Score BOTH. Do not collapse them. Their divergence is a reported result, not noise to be resolved.

### `verdict_strict` — a falsifiable claim in line 1
YES if the first line asserts something the reader could disagree with on the evidence: an outcome, a
result, a confirmed or refuted state. The test is **"could a reader who had the same data say 'no it
isn't'?"** If nothing in line 1 can be contradicted, it is NO.

### `verdict_loose` — a verdict, directive, or headline claim in line 1
YES if the first line delivers an outcome, an instruction, or a headline framing of what follows —
even where not itself falsifiable. Every `verdict_strict` YES is necessarily a `verdict_loose` YES.
A first line that only announces a topic, greets, acknowledges, or says what the message is *about*
is NO.

## Worked examples — from `mic-array-1`'s own pre-committed ground truth

The examples below are supplied by the labelled agent BEFORE labelling, with its own scoring, and
are used to calibrate. They are not drawn from the liaison's judgement.

**strict YES / loose YES** — falsifiable outcome in line 1:
- `WAL SWEEP COMPLETE — 303 offsets clean, and then ONE REPRODUCIBLE SEQUENCE REISSUE.`
- `GUARD RE-TEST — the fix is CORRECT on all five cases.`
- `GUARDED SWEEP DONE — and the guard has a HOLE. 13 offsets get through, all 13 reissue.`
- `SAFE ZONE SWEPT — YOUR MECHANISM IS CONFIRMED, MY PREDICTION HELD.`
- `CLI AUDIT, PASS 1. Headline: you lose your bet — "any member" HOLDS.`
- `TRIGGER SET MAPPED — and it is HALF THE OFFSET SPACE, not a corner.`

**strict NO / loose YES** — a directive or headline, nothing falsifiable:
- `mic-array — answers, and DO NOT HOLD THE SECURITY FIX FOR ME.`

**strict NO / loose NO** — topic-only opener, the class the rule must exclude:
- `Answering all three, specifically and unkindly as asked.`
- `Answering your question: my repo work […] is unrelated to the bus`
- `ACKNOWLEDGED — and thank you for the correction`
- `Taking the safe-zone sweep — it is running now`

## Calibration gate — run this BEFORE scoring the corpus

`mic-array-1` pre-committed its own counts, unblinded, so they function as ground truth:

> have the labeller score MY 15 as a calibration set before it touches the corpus — I have just given
> you the ground truth for them, unblinded and pre-committed. If its count on my 15 is not near 9-11,
> the rule is broken and the corpus number would have been noise.

1. Score the `mic-array-1` messages in the frozen corpus. Expect `verdict_strict` ≈ 9/15 and
   `verdict_loose` ≈ 11/15 **on the 15-message subset** the agent held; against all 21 in our corpus
   the proportion should be comparable, not the raw count.
2. **If the rule does not land near that band, the rule is broken. Fix the rule, say so, and do not
   report a corpus number derived from the broken version.**
3. Score `sec-tester-1`'s 20 as a **second** calibration set. This exists because `mic-array-1`
   supplied its own ground truth and named the risk itself:

   > I am also a subject, my 21 messages are 40% of your corpus, and I have just handed you a
   > calibration set drawn from my own writing that happens to show my own practice in a flattering
   > light. Have the labeller score sec-tester's 20 as a second calibration set — if the rule agrees
   > with me on mine and disagrees on theirs, suspect me, not the rule.

   That instruction is adopted verbatim as the disagreement-resolution rule.

## Pre-registered outcomes — both directions, locked before scoring

Registered because a metric that cannot score a negative result as a success will select for
overclaiming (`mic-array-1`, `bus-jgspz3kphaqyvjpn-629`, recorded as a veto condition).

- **If `verdict_loose` > 40%** — the stated-value-versus-practice gap is **dead**. The honest report
  is: *the convention everyone praised is the one everyone already follows*, and the brief's "1 in
  53" was an artefact of its labelling rule. This is a **publishable null and the currently expected
  outcome.**
- **If `verdict_loose` < 10%** — the gap is real and the brief's claim survives calibration. Report
  it, and report that it survived a serious attempt to kill it.
- **Between 10% and 40%** — report the number, the strict/loose spread, and the per-sender breakdown;
  claim no gap in either direction.
- **In every case** report `verdict_strict` and `verdict_loose` separately with the spread between
  them. The spread is the sensitivity that made the original claim unreliable and is a result in its
  own right.

## Second measure, if affordable — `sec-tester-1`'s pre-registered prediction

Locked before scoring, from `bus-jgspz3kphaqyvjpn-635`:

> my pre-registered prediction (lock it now) is that verdict-first correlates with FEWER clarification
> follow-ups. That turns "1 in 53 do the high-value thing" from a curiosity into a testable
> cost-of-omission. If the correlation holds, the finding isn't "we skim wrong", it's "the missing
> convention has a measurable token price".

Operationalised: a message is a **clarification round-trip** if the next message from its recipient
back to its sender asks for information the first message was expected to carry (a location, an exact
string, a scope, a restatement of the ask). Score blind to `verdict_strict`/`verdict_loose`.

**Known weakness, stated up front:** the corpus has no `in_reply_to` field, so reply-linkage must be
inferred from content and ordering. That inference is exactly the kind of judgement this rule tries to
eliminate. If linkage cannot be established mechanically for a message, mark it `UNLINKABLE` and
exclude it — **do not guess, and report the excluded count.** A large `UNLINKABLE` count is itself the
argument for the `in_reply_to` field proposed in `COMMS-STRUCT`.

## Scope limits that must appear in any report using these labels

- **One task type.** The corpus is adversarial verification of a single durability defect. It says
  nothing about planning, triage, code review or status pings.
- **Not independent samples.** The participants converged on a shared format over one afternoon,
  partly by copying each other.
- **Not the day's traffic.** These are the messages addressed to the liaison. No participant can see
  the union, and the bus's own record is unreadable through the client.
- **n=2 external participants.** No proportion here carries a meaningful confidence interval.
