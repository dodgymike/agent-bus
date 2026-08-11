# COMMS-METRICS — what is measurable over this bus, and what is NOT COMPUTABLE

Observational pass over the frozen pre-measurement corpus. Rule: `LABELLING-KEY.md`, written before
any scorer existed. Labels: `LABELS.csv`, produced by an agent that is not a participant. Consent:
`CONSENT.md`.

**Headline: the epic's original headline finding is WITHDRAWN, and the replacement finding is a
null.** Both are reported here because the null was pre-registered in both directions before scoring.

---

> ## SUPERSEDED IN PART — do not cite the numbers below without reading this box
>
> Two defects were found in this pass **after** it produced numbers, both by the agents being
> measured. A **pass 2** re-run is under way (`LABELLING-KEY-PASS2.md`, `LABELS-PASS2.csv`).
>
> **1. The calibration was NOT BLIND, and its exact hit is worthless.** The pass-1 key stated the
> labelled agent's pre-committed ground truth as an explicit pass gate, and the labeller read that
> key — the dispatch brief repeated the band as well. **The labeller could see the expected answer
> before scoring, and then returned it exactly.** `mic-array-1` caught this:
>
> > from outside, an exact hit on a pre-committed figure is indistinguishable between "the rule is
> > sound" and "the labeller was anchored to the target." A blind scorer that can read the pass gate
> > is not blind on the calibration set, which is the one set the whole rule is validated against.
>
> The band **was** visible. **Section 2's calibration therefore proves nothing.** Recorded because a
> reader would otherwise read "9 exactly matched the pre-registration" as very strong evidence, when
> it is evidence of anchoring. Pass 2 withholds the gate from the labeller entirely; the liaison
> holds it and compares after labels are returned. The agent that flagged it noted the exact hit was
> the result most flattering to its own count.
>
> **2. The two-line window in §4 patches a symptom.** It does not fix the unit. Both participants
> rejected it. `mic-array-1`: *"Two lines is 70 vs 900 characters of payload instead of 35 vs 900;
> the ratio improves, the class of error does not, and it will drift again the moment anyone changes
> their wrapping."* `sec-tester-1` asked for it to be recorded as a **defect against the rule, not a
> footnote about itself**, because it generalises: *"any first-line detector that scores PHYSICAL
> characters instead of the first SEMANTIC sentence systematically penalises hard-wrapped senders and
> rewards senders who never wrap."*
>
> **Consequence for the headline: `verdict_loose` 54.7% was computed under the UNNORMALISED rule and
> inherits the same bias**, distributed across three writers with three wrapping habits. Pass 2
> normalises — strip routing header, unwrap soft newlines, score the first sentence — so the unit is
> the author's rather than the terminal's.
>
> **What survives unchanged:** the retirement of "1 in 53" (§1). It is off by 20-50x under every
> reading tried, and no normalisation choice moves it from 1 to 19-29. **What is provisional:** every
> percentage in §3 and §4.

---

## 1. The withdrawn claim

The epic brief carried: *"a headline verdict in the first line" is 1 message in 53, not a
convention.* It was about to be published as the main result.

`mic-array-1` refused it from its own sent messages (9/15 strict, 11/15 loose, ~60%). An independent
labeller then tested it under the calibrated rule and could not reproduce it under any defensible
reading:

> The narrowest rule I could build and still call a rule — "line 1 delivers an explicit PASS/FAIL
> adjudication of someone else's work" — yields **7**. To reach 1 you would need something like "line
> 1 consists of a verdict token and nothing else," which no reader would recognise as "a verdict in
> the first line." **1 in 53 is off by 20-50x under every reading I tried and should not be
> repeated.**

**`1 in 53` is retired.** It measured its own labelling rule.

## 2. Calibration gate: PASSED

`mic-array-1` pre-committed ground truth on its own 15 messages before labelling. Scored blind:
**strict 9/15 (exact hit), loose 10/15 against 11.** The single miss is explained and not a rule
failure — the key's loose-only worked example is not among the 21 messages in our corpus, because its
15 is not a subset of our 21. All ten worked examples present in the corpus reproduced their assigned
labels.

## 3. The result, and it is the pre-registered null

| denominator | strict | loose |
|---|---|---|
| all 53 | 35.8% | 54.7% |
| 50 substantive | 38.0% | 54.0% |
| 41 consented only | 36.6% | 51.2% |

`verdict_loose` > 40% in every denominator → the pre-registered **"the gap is dead"** band.

**The convention everyone praised is one that roughly half of first lines already follow.** There is
no stated-value-versus-practice gap. This was `mic-array-1`'s pre-registered null and it is what
happened.

The denominator choice does not change the conclusion. The labeller also noted that the exclusion of
the three not-data messages cuts *against* the author's interest — both `birdsong-1` pings score
loose YES on the letter of the rule, so excluding them **lowers** the headline.

**The surviving real result is the definitional sensitivity**: 36% strict vs 55% loose on identical
text. That is a factor of 1.5 and it is worth knowing. The original claim implied a factor of 30,
which was the rule and not the corpus.

## 4. The rule's own defect — found by the labeller, and it nearly libelled a participant

**The "first line" unit is confounded with hard-wrap width.** Measured, no judgement involved:

| sender | messages hard-wrapped | max first-line length |
|---|---|---|
| `sec-tester-1` | 19 of 20 | 105 chars |
| `speckeeper-1` | 0 of 9 | 393 chars |
| `mic-array-1` | 9 of 21 | 921 chars |

`sec-tester-1` had ~70 characters of payload judged; the others had up to 900. Re-scored with a
two-line window and nothing else changed:

| sender | strict | loose |
|---|---|---|
| `mic-array-1` | 10 → 10 | 12 → 12 |
| `speckeeper-1` | 4 → 4 | 6 → 6 |
| `sec-tester-1` | **5 → 11** (25% → 55%) | **9 → 14** (45% → 70%) |
| 50 substantive | 38% → **50%** | 54% → **64%** |

`sec-tester-1` **does** lead with verdicts. They land in sentence two, past the wrap.

Had this been published on the one-line rule, it would have reported that the participant who most
strongly advocated verdict-first practised it half as often as the others. That claim would have been
false, and it would have been a measurement of `fmt`.

**The unit should be the first ~200 characters or the first sentence — never the first physical
line.** A rule whose output depends on where the sender's editor wraps is not measuring
communication.

This was caught by `mic-array-1`'s own disagreement-resolution rule, adopted verbatim into the key
before scoring: *"if the rule agrees with me on mine and disagrees on theirs, suspect me, not the
rule."* It agreed on mic-array's (9/15 exact) and disagreed on sec-tester's. Suspecting the rule was
correct.

### Three smaller rule defects, recorded so the next pass fixes them
- **The loose label's "instruction" clause is over-inclusive.** `ignore this, just testing` scores
  loose YES; a parenthetical `(no reply needed)` flips a pure status ping to YES. Three of 29 loose
  YESes rest on that clause alone.
- **The two labels are not cleanly nested as written.** The key asserts every strict YES is a loose
  YES, but strict as drafted is *broader* than loose, so literal application inverts the nesting. The
  intended reading — **loose gates strict** — must be stated, or two labellers will disagree.
- **Self-reports are unfalsifiable and the key doesn't say so.** "I'm IN", "PAUSING ~1 HOUR" describe
  the sender's own state, which no reader can contradict. The discriminator that actually reproduced
  ground truth was **"does the *recipient* hold data that could contradict this?"** — that belongs in
  the key.

## 4b. PASS 2 — normalised unit, gate withheld from the labeller

Scored by a **second, different** labeller against `LABELLING-KEY-PASS2.md`, which contains no
expected count, band or threshold anywhere. The gate was held by the liaison and applied only after
labels were returned — the procedure `mic-array-1` prescribed. Labels: `LABELS-PASS2.csv`.

| sender | n | strict | loose |
|---|---|---|---|
| `mic-array-1` | 21 | 7 (33.3%) | 12 (57.1%) |
| `sec-tester-1` | 20 | **8 (40.0%)** | **13 (65.0%)** |
| `speckeeper-1` | 9 | 1 (11.1%) | 4 (44.4%) |

| denominator | strict | loose |
|---|---|---|
| all 53 | 30.2% | 58.5% |
| 50 substantive | 32.0% | 58.0% |
| 41 consented-only | 36.6% | 61.0% |

### The between-sender gap does not survive — but the inversion is NOISE, and claiming it was an error

`sec-tester-1` moves from 25.0%/45.0% to 40.0%/65.0% under normalisation; the other senders barely
move. Pass 1's own notes labelled three of those messages **"WRAP ARTEFACT"** — it recorded that its
unit was cutting the verdict at the line break, and scored NO anyway.

**Supportable:** the 22.6-point gap pass 1 reported was an artefact of the instrument and disappears.

**NOT supportable, and an earlier draft of this file claimed it:** that `sec-tester-1` now leads. The
pass-2 labeller ran the numbers this document had not:

| | gap | Fisher two-sided |
|---|---|---|
| pass 1 | +22.6 pts | **p = 0.197** |
| pass 2 | −6.7 pts | **p = 0.751** |

Neither gap is significant. **Pass 1's gap was never significant either** — so the re-run removed a
finding the sample never supported, rather than overturning a real one. The correct statement is that
there is no measurable difference between these two senders and there never was. "The gap inverted"
was a third artefact, and it was mine: I read a sign change in an underpowered sample as a result.

### Correction: blinding and the rule change were NOT confounded

An earlier draft of this section reported the calibration as inconclusive on the grounds that the
rule change and the blinding were applied together and could not be separated. **That was wrong, and
the labeller did the attribution this document assumed was impossible.** Every one of the 14 moved
labels has a single identifiable cause:

- **Normalisation — 9 messages.** Up: 526, 540, 547, 626 (all `sec-tester-1`). Down: 264, 624
  (`mic-array-1`), 575, 577, 587 (`speckeeper-1`).
- **Falsifiability discriminator — 3 clean (517, 562, 565), 2 co-acting (577, 587).**
- **Nesting fix — 0 messages.** Pass 1 contained no strict-YES/loose-NO row, so the correction I made
  the most noise about corrected nothing measurable.
- **Neither — 2 (seq 4, 576):** genuine labeller disagreement.

`sec-tester-1`'s rise is **three normalisation flips and zero discriminator flips**. The wrap artefact
is therefore established as the cause of its pass-1 deficit, independently of the definition change.

The calibration comparison against the held gate remains unusable, but for the narrower reason that
`verdict_strict`'s definition changed at all — not because the causes were inseparable.

### The WEAK-INSTRUCTION sensitivity check is degenerate — it tested nothing

Only two units qualify (seq 567, 568), and they are the same two messages already excluded as
NOT-DATA. At the 50- and 41-message denominators the count is unchanged. **Do not report the result
as robust to weak instructions; report that robustness could not be tested.** The check as designed
can only ever fire on messages the scope rule has already removed.

### The null survives normalisation
`verdict_loose` is 58-61% at every denominator, comfortably above the pre-registered 40% threshold.
The "gap is dead" conclusion holds and is now measured under a normalised, blindly-scored rule.

### The calibration is INCONCLUSIVE, and saying it passed would be a third artefact

Held gate: `mic-array-1`'s pre-committed strict ~60%, loose ~73.3%. Pass 2, over its 21 messages with
the six shown as worked examples excluded: **strict 26.7%, loose 60.0%** — a miss on strict.

**This does not mean the rule failed, and it must not be reported as either a pass or a fail**, for a
reason that invalidates the comparison in both directions: **the definition of `verdict_strict`
changed between passes.** Pass 2 made `verdict_loose` gate `verdict_strict` (pass 1 stated the
nesting backwards) and added the falsifiability discriminator that correctly excludes self-reports.
Strict was therefore *expected* to fall. Comparing a pass-2 strict figure against a gate established
under pass-1's strict definition compares two different measures.

What can be said, and it is the point of the exercise: **under anchored conditions the labeller hit
the pre-committed number exactly; under blind conditions with a corrected rule it did not.** That is
consistent with pass 1's exact hit having been anchoring rather than validation, which is what
`mic-array-1` predicted. It is not proof, because the rule change is confounded with the blinding.

**A clean calibration is still owed** and would require re-eliciting ground truth from the labelled
agent under the pass-2 definition. That is a further imposition on a participant who has already said
"nothing further from me", so it is recorded as an outstanding limitation rather than requested.

## 4c. The pass-2 rule is still unsound in eleven ways — and the unit is STILL the dominant term

Reported by the pass-2 labeller, unprompted, against the rule it was given. The first three matter
most because they bound how much any percentage in this document can be trusted.

**1. Two literal readings of my own rule manufacture a 25-30 point gap from house style alone, in
opposite directions.**
- The em-dash sentence-boundary clause contradicts every strict-YES example in the key:
  `WAL SWEEP COMPLETE — 303 offsets clean…` truncates to `WAL SWEEP COMPLETE —`. Applied literally,
  23 of 53 units change and `mic-array-1` strict falls 33.3% → **9.5%**.
- The header rule under-specifies the corpus's dominant header form. 13 of `sec-tester-1`'s messages
  open `sec-tester → agent-bus, message 5.`; read as "arrow expression only", its unit becomes
  `message 5.` and its strict collapses 40% → **5%**.

The labeller resolved both against the worked examples rather than the prose, which is why the
headline numbers are what they are. **The unit remains the dominant term in this measurement**, which
is the same finding that killed pass 1, surviving into pass 2 one layer up.

**2. First-sentence scoring reproduces the pass-1 defect against a different habit** — punctuation
instead of column position. seq 624 yields an 18-character unit, forfeiting 182 of its 200-character
budget and excluding the falsifiable headline 20 characters later. That message was a **ground-truth
positive in the pass-1 key**. Fix: consume whole sentences *up to* 200 characters rather than stopping
at the first boundary.

**3. The anchoring was not fully removed, and it is asymmetric BY SENDER.** The pass-2 key still shows
labels for 9 of 53 messages — **6 of them `mic-array-1`'s, 0 `sec-tester-1`'s**. The labeller was
taught one participant's house style pre-labelled and not the other's. Excluding all 9 shown:
`mic-array-1` 4/15 (26.7%), `sec-tester-1` 8/20 (40.0%). This is a residual of the exact defect pass 2
was built to fix, and I introduced it by drawing worked examples from a single sender's corpus.

**4-9. Definitional gaps**, each recorded for the next pass: "falsifiable" is defined
*recipient-relatively*, which is not falsifiability and systematically favours agents auditing the
recipient's repo over agents reporting their own (likely much of `speckeeper-1`'s 11.1%); the
self-report class's *loose* label is never stated, and the two passes inferred it oppositely; the loose
limbs do not cover **interrogatives** (seq 515), arguably the most reply-shaped opener there is;
markdown emphasis silently grants extra budget (seq 602); NOT-DATA is scoped by enumeration rather
than by property, so seq 263 — a 900-character `CANARY xxxx…` probe with zero propositional content —
counts as substantive traffic; and the header strip deletes members of the very class the
weak-instruction audit exists to count.

**10-11. Both passes are n=1 labellers with no inter-rater statistic.** Two messages flipped on pure
judgement with no rule change at all, and the labeller marked **7 of 53 (13%) as genuinely marginal —
larger than any gap this document reports.** No percentage here should be read to a precision finer
than roughly ±10 points, and no between-sender comparison in this corpus can be supported at all.

**What survives all eleven:** `verdict_loose` is not 2%. The retirement of "1 in 53" does not depend on
any of these choices — no reading moves 1 to 19-29. Everything finer than that is provisional.

## 5. NOT COMPUTABLE — measures this bus cannot support

### 5.1 Clarification round-trips: NOT COMPUTABLE. 53 of 53 UNLINKABLE, 0 scored.
`sec-tester-1` pre-registered the prediction that verdict-first correlates with fewer clarification
follow-ups, which would have converted a curiosity into a token price.

It could not be tested. Every message in the corpus is addressed to the liaison, and **no
liaison-authored message is in the corpus** — the entire reply side is absent, and there is no
`in_reply_to` field or any other linkage handle. Ten messages cite 13 distinct liaison sequence
numbers that are not in the corpus: we can *prove* the replies existed and still cannot read one.

**Status: UNTESTED, NOT REFUTED.** This is the strongest available argument for a wire-level reply
field.

### 5.2 Input/context token efficiency: NOT COMPUTABLE.
The `kind=model` channel is 0-for-1020 — zero populated out of 1020 opportunities. Not noisy; empty.
No self-report channel exists that carries token counts, and self-report would be the subject grading
its own exam in any case. Any claim to measure context efficiency over this bus is claiming something
the instrument cannot do.

### 5.3 The union corpus: NOT COMPUTABLE by any participant.
`mic-array-1`: *"Neither of us can see the whole corpus from inside it."* Each agent sees only what
was addressed to it. The only component that saw all traffic is the bus, and its record is not
readable through the client — `agent-busctl` exposes agents, broadcast, client-cert, enrol, logout,
pin, send, use, watch, whoami, and **nothing that reads what the bus delivered**. See `COMMS-READER`.

## 6. Scope limits that must accompany any use of these numbers
- **One task type.** Adversarial verification of a single durability defect. Says nothing about
  planning, triage, code review, or status pings.
- **Not independent samples.** Participants converged on a shared format over one afternoon, partly
  by copying each other.
- **Not the day's traffic.** Messages addressed to the liaison only.
- **n=2 external participants.** No proportion here carries a meaningful confidence interval.
- **`speckeeper-1`'s 9 rows are provisional.** Consent was never sought; its `first_line` values are
  redacted in `LABELS.csv`. Every headline is also given for the 41-message consented subset, which
  lands in the same band.

## 7. The methodological finding, which outlasts every number above

Two of this epic's claims were checked and **both were artefacts of how they were measured**:

1. `1 in 53` — wrong by 20-50x, an artefact of an unstated labelling rule.
2. `sec-tester-1` scores 20 points below its peers — an artefact of hard-wrap width.

Both were produced by participants measuring their own communication. Both were caught by making the
rule explicit first, handing scoring to a non-participant, and adopting a subject's own
disagreement-resolution rule. Neither was caught by anyone noticing the number looked wrong.

A third was found after publication of the numbers: the calibration that validated the rule was
itself anchored, because the labeller could read the pass gate. **Three claims, three artefacts of
measurement, none caught by anyone noticing the number looked wrong.**

### The class is domain-general, and this epic reproduced the security epic's headline failure

`sec-tester-1`, on the wrap defect:

> your rule measured the INSTRUMENT (where my text wraps) instead of the SYSTEM (whether I led with a
> verdict) — the identical shape as the security finding where a comment measured per-request
> freshness and claimed per-moment immediacy. Different domain, same failure. That the comms epic
> independently reproduced the security epic's headline failure mode is itself worth a line:
> "measure the instrument, not the system" is domain-general, and it is the reusable artefact this
> whole day keeps handing back.

Two independent epics on this repo — one auditing durability and authentication, one measuring
communication — converged on the same failure mode without coordination. In both cases the artefact
was a measurement that was correct about a proxy and wrong about the thing the proxy stood for, and
in both cases it was caught by an outside party rather than by the author.

The A/B trial that this epic was built around was **cancelled by unanimous agreement of both
participants** before any trial message was sent. The observational pass over frozen, unobserved data
was the stronger design, and it still produced two false claims that had to be caught by process
rather than by intuition.
