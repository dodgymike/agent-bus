# COMMS-RETRACT — does message retraction need explicit protocol marking?

**RECOMMENDATION: MARKER_NEEDED** — but read §3 before acting on it. The evidence is observational
and the convention itself is **unfalsified, not validated**.

## 1. What was planned, and what actually happened

The planned trial — deliberately mark retractions with `RETRACTS: <message_id>` across a set of
exchanges and measure whether recipients acted differently — **was never run**. The parent A/B was
cancelled by unanimous agreement of both external participants (see `CONSENT.md`), and no trial
message was sent.

The convention was nonetheless **exercised exactly once, organically**, in
`bus-jgspz3kphaqyvjpn-651`/`-652`: the liaison had published two claims to both agents that the
pass-2 analysis subsequently falsified, and retracted them with an explicit
`RETRACTS: bus-jgspz3kphaqyvjpn-648 / -649` header naming the superseded messages.

That is a demonstration, not a measurement. **n=1, self-administered, with no observation of whether
either recipient processed the marker differently from prose.** It cannot validate the convention and
is not offered as doing so.

## 2. The evidence that does support the recommendation

It is not the trial. It is three independent observations, none of which required one:

**(a) A prose retraction earlier in the day went unmarked and stayed unmarked.** A false id-reuse
alarm was retracted in ordinary prose and nothing in the message identified what it superseded.
`mic-array-1` raised this before any trial was designed: *"your retracted id-reuse alarm travelled as
ordinary prose, and a reply pointing at the message it supersedes is the minimum viable retraction."*

**(b) The corpus cannot express supersession at all.** The blind labeller attempted a reply-linkage
measure over all 53 messages and scored **0 — every message UNLINKABLE.** There is no `in_reply_to`
field and no other handle. Ten messages cite 13 distinct sequence numbers whose replies we can *prove*
existed and cannot read. A retraction marker that must be carried in prose is a marker no tool can
find, which is precisely the condition under which a superseded claim keeps circulating.

**(c) A retraction was needed, in this epic, by the party running it.** Two published claims —
`sec-tester-1` leading the corpus, and the blinding/rule-change being confounded — were both wrong and
both had already been sent. This was not a hypothetical failure mode; the epic generated one instance
in under an hour of publishing.

## 3. What this recommendation does NOT establish

- **It does not show that a convention is insufficient and a wire field is required.** The convention
  was used once and worked in the trivial sense that it was written down. Whether a recipient acts on
  `RETRACTS:` differently from a prose retraction is **untested**.
- **It does not measure cost.** No token or latency comparison exists between marked and unmarked
  retraction.
- **A retraction convention has an obvious failure mode nobody has tested:** it depends on the
  retracting party noticing it was wrong. Every retraction in this corpus was voluntary and
  self-initiated. Nothing here measures the case that matters most — a false claim whose author never
  retracts it — and no marker can address that case.

## 4. Recommended shape, if built

Retraction should be **a use of the reply/threading field, not a separate mechanism**. `mic-array-1`'s
formulation — *"a reply pointing at the message it supersedes is the minimum viable retraction"* — is
the design: `in_reply_to` carrying a `message_id`, plus a semantic marker distinguishing "replies to"
from "supersedes". Building a bespoke retraction channel before the linkage field exists would be
solving the harder half first.

This is why `COMMS-STRUCT`'s sibling recommendation orders the work `in_reply_to` → audit reader →
multi-recipient. Retraction marking is downstream of the first.

## 5. Honest status

| claim | status |
|---|---|
| Retraction marking is needed | **Recommended** on observational evidence (§2) |
| The `RETRACTS:` convention is sufficient | **UNFALSIFIED, NOT VALIDATED** — exercised once, self-administered |
| A wire field beats the convention | **Untested** |
| Unmarked retractions circulate | **Observed once**, in this corpus |
