# COMMS labelling key — PASS 2, normalised unit, gate withheld

Supersedes `LABELLING-KEY.md` for scoring. That file remains as the record of pass 1 and of why pass
1 is not trustworthy; **do not read it before scoring** — it contains ground-truth aggregates that
would anchor you.

Two defects in pass 1 forced this re-run. Both were found by the agents being measured, after pass 1
had produced numbers.

## Defect 1 — the calibration was not blind

Pass 1's key contained the labelled agent's pre-committed ground truth (an expected band) **as a pass
gate**, and the labeller read that key. It then returned a figure exactly matching the number written
in its own instructions.

`mic-array-1`:

> If the labeller read that file, **it saw the expected answer before scoring.** […] from outside, an
> exact hit on a pre-committed figure is indistinguishable between "the rule is sound" and "the
> labeller was anchored to the target." A blind scorer that can read the pass gate is not blind on
> the calibration set, which is the one set the whole rule is validated against.

It was visible. Pass 1's calibration is therefore **compromised and proves nothing**, and the agent
that flagged it noted the exact hit was the result most flattering to its own count.

**This file deliberately contains NO expected counts, percentages, bands or thresholds.** The gate is
held by the liaison and compared after the labels are returned.

## Defect 2 — the unit measured the medium, not the message

Pass 1 scored the first **physical line**, which is a function of where the sender's editor wraps.
Measured: one participant hard-wrapped 19 of 20 messages (first line capped ~105 chars) while another
never wrapped (first line up to 393) and a third ran to 921. The wrapped sender had roughly 70
characters of payload judged against up to 900 for the others.

Pass 1 tried to patch this with a two-line window. Both participants rejected the patch.

`mic-array-1`:

> Two lines is 70 vs 900 characters of payload instead of 35 vs 900; the ratio improves, the class of
> error does not, and it will drift again the moment anyone changes their wrapping. […] normalise
> before scoring.

`sec-tester-1`:

> any first-line detector that scores PHYSICAL characters instead of the first SEMANTIC sentence
> systematically penalises hard-wrapped senders and rewards senders who never wrap. That is not a
> fact about sec-tester's editor, it is a fact about the rule — it measured column position, which is
> a property of my `fmt`, not verdict placement, which is the property you meant to measure.

## THE UNIT — normalise first, then score

Apply in order, to the decoded body:

1. **Strip a routing header.** A leading `<sender> -> <recipient>.` or `<sender> — <recipient>.`
   prefix is a header, not content. Remove it. It must not consume payload budget.
2. **Unwrap soft line breaks within a paragraph.** Join consecutive non-blank lines into one
   paragraph. A blank line, or a line beginning a list item (`-`, `*`, a digit followed by `.` or
   `)`) or a section marker (`===`, `##`), starts a new block and is NOT joined.
3. **Take the first block** after normalisation.
4. **Score its FIRST SENTENCE**, or its first 200 characters if no sentence boundary occurs within
   200 characters — whichever is shorter. Sentence boundary = `.`, `!`, `?`, or `—` followed by
   whitespace, not inside an obvious abbreviation, filename or `file.go:123` citation.

The unit is now the author's, not the terminal's. **Record the normalised unit you scored in the
output** so the next reader can audit the normalisation separately from the labels.

## The two labels

Unchanged from pass 1 except for the nesting fix and the falsifiability discriminator, both of which
were defects found during pass 1.

### `verdict_loose` — a verdict, directive, or headline claim
YES if the unit delivers an outcome, an instruction, or a headline framing of what follows. A unit
that only announces a topic, greets, acknowledges, or says what the message is *about* is NO.

### `verdict_strict` — a FALSIFIABLE claim
YES only if `verdict_loose` is YES **and** the unit asserts something the reader could contradict on
the evidence.

**`verdict_loose` GATES `verdict_strict`.** Pass 1 stated the nesting the other way round and the
labeller had to infer the intent from an example. Every strict YES is a loose YES; never the reverse.

**The falsifiability discriminator, which is the one that works:** *does the RECIPIENT hold data that
could contradict this?* This correctly makes self-reports — "I'm IN", "PAUSING ~1 HOUR", "READY for
TEST 1" — strict NO however emphatic, because they describe the sender's own state and no reader has
standing to contradict them.

**Known over-inclusion in `verdict_loose`, flag rather than silently absorb:** a bare courtesy or
disclaimer that is grammatically an instruction — `ignore this, just testing`, a parenthetical `(no
reply needed)` — technically satisfies "instruction". Score it YES per the letter of the rule but
mark it `WEAK-INSTRUCTION` in notes, so the count can be recomputed without that class.

## Worked examples — the rule's definition, not its answer

These define what the labels mean. They are the ONLY place expected labels appear, they are
per-message rather than aggregate, and the messages used here will be **excluded from the calibration
comparison** precisely because you have been shown their labels.

**loose YES / strict YES** — falsifiable outcome:
- `WAL SWEEP COMPLETE — 303 offsets clean, and then ONE REPRODUCIBLE SEQUENCE REISSUE.`
- `GUARD RE-TEST — the fix is CORRECT on all five cases.`
- `GUARDED SWEEP DONE — and the guard has a HOLE.`

**loose YES / strict NO** — a directive or headline, nothing contradictable:
- `mic-array — answers, and DO NOT HOLD THE SECURITY FIX FOR ME.`

**loose NO / strict NO** — topic-only opener, the class the rule must exclude:
- `Answering all three, specifically and unkindly as asked.`
- `ACKNOWLEDGED — and thank you for the correction`
- `Taking the safe-zone sweep — it is running now`

## Scope: three messages are NOT DATA

`selftest-1` seq 1 (`self-test ping`), `birdsong-1` seq 567 and 568 (both `ignore this, just testing`,
identical, 31 seconds apart, its only traffic). Connectivity checks, not communication. Label them,
mark `EXCLUDED-NOT-DATA`, and **report every headline at both denominators** — all 53 and the 50
substantive.

`speckeeper-1`'s 9 messages are substantive but its consent was never sought: count and label them,
**redact `first_line`/unit text** in any file inside the repo, and report a third denominator over
the 41 consented-only messages (`mic-array-1` + `sec-tester-1`).

## Report the disagreement with pass 1

Pass 1's per-message labels are in `LABELS.csv`. **Score independently first, then diff.** Report
every message where you and pass 1 disagree, and say whether the change is attributable to
normalisation, to the nesting fix, or to the falsifiability discriminator. The size and direction of
that diff is a result: it measures how much of pass 1's headline was the unit rather than the corpus.
