# RELAY-FU-IDEM-METER-BY-PEER: Meter the applied-key table by the AUTHENTICATED PEER, not the peer-asserted origin sender

| Field | Value |
| --- | --- |
| Public id | `8774f265-230d-49c9-90e4-bd96c866fd8d` |
| Key | RELAY-FU-IDEM-METER-BY-PEER |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | relay |
| Section | in_progress |
| Tags | — |
| Created | 2026-08-14T15:11:47.939604+00:00 |
| Updated | 2026-08-15T12:35:56.384179+00:00 |
| Completed | 2026-08-15T12:35:56.384161+00:00 |

## Proof command

```sh
go test -race -count=1 -run '^(TestPeerBucket32766InventedLabelsCountAsOnePeer|TestPeerBucketRecoveryParityAt32768Boundary|TestRelayIngestCrashRecoversCanonicalPeerBucket)$' ./internal/idem ./internal/hub
```

## Status note

PRIORITY RAISED P1 -> P0, 2026-08-14 (coordinator judgement call, recorded not silently applied): this task was filed P1 despite security calling it P0 #1 of four remaining on the RELAY-24 critical path. An attacker-controllable applied-key-table exhaustion (a peer asserting M distinct agent names converges the table toward 65536/(M+1+L) keys, refusing EVERY LOCAL AGENTs next send bus-wide once past the pressure line) is not a P1 by this project's own severity conventions -- it is exactly the shape of finding filed P0 elsewhere this session (the out-of-order mint poison, the invite-gate enforcement gap). Raised to match. SEPARATE JUDGEMENT CALL, ALSO RECORDED: this task's own description says its fix belongs at the wiring site specifically, because internal/hub never sees the authenticated peer identity that internal/httpapi resolves. That makes it a REQUIREMENT of RELAY-24s own composition wiring, not a separable prerequisite a different agent could close first and hand off. See RELAY-24s own status_note for the full read across all items on this path, not just this one.

--- APPENDED 2026-08-14 (spec-keeper, on the feature-runner's report). TASK STAYS OPEN AT P0; THE FIX IS NOT DONE. What DID land this pass is only the IN-PACKAGE RESIDUE, code-complete and NOT COMMITTED, confined to internal/idem and comment-only: store.go no longer asserts the fair-share bucket key "is therefore a PROVEN IDENTITY, not an attacker-chosen label"; scope.go no longer calls a share refusal "always SELF-INFLICTED"; errors.go's ErrAgentQuota doc no longer tells operators to "reach for the one client named in the message", which blamed an honest local agent when a peer poisons the denominator. All three statements were FALSE on the relay ingest path; all three are now narrowed. Non-comment lines are byte-identical to HEAD (proven). Gates on the residue: reviewer PASS-WITH-NITS, explicitly "task must stay OPEN"; security PASS-WITH-FINDINGS plus an adversarial addendum re-checking the cheap-fix claim. The description has been AMENDED with the security gate's four mandatory conditions on the no-format-change option -- read the amendment before picking this up, it changes the scope.

--- APPENDED 2026-08-15 (owner sequencing correction). Work proceeds NOW on the collision-free internal/idem increment: implement the valid, case-folded foreign bus-half fairness bucket; fail closed malformed sender/BusID inputs; require a non-zero valid local BusID; and add the live denominator, mixed-case, two-peer, recovery/restart, and crash regressions. Preserve and journal all five sec-tester wire cases, including every failed attempt and before/after denominator evidence. hub.Open identity injection is a REQUIRED later composition step but is explicitly DEFERRED until the held internal/hub/hub.go is released; codex-1 must not edit held hub or relay files. Do not substitute an OpRelay proxy, and do not narrow the spec or acceptance to avoid the deferred identity injection. The task remains in_progress throughout this sequencing split.

--- APPENDED 2026-08-15 (final-phase acceptance correction; task remains in_progress). The former proof named TestAppliedKeyMeteredByAuthenticatedPeer in internal/relay/internal/httpapi, but that test does not exist and proof-check measured it VACUOUS. Acceptance now requires a non-vacuous exact proof of (1) the live same-pressure boundary at Count=32768 after two keys held by the same local victim plus 32,766 asserted foreign labels, including the poisoned peer refusal and continued local/other-peer service; (2) recovery parity at that same boundary; and (3) the genuine hub re-exec/SIGKILL-after-commit recovery test rebuilding two mixed-case foreign senders as one canonical peer bucket from WAL bytes. This automated proof does NOT replace or narrow the sec-tester acceptance: final security approval still requires all five live wire cases with measured before/after denominator evidence, including failed attempts, as already specified. Do not complete until that wire evidence and final reviewer/security gates are recorded.

## Description

Filed 2026-08-14 from the RELAY-24-BLOCKER-HUBINGEST security gate. HIGH -- GATES RELAY-24 (the wiring), not the ingest commit.

THE DEFECT: relay.RelayedMessage.Scope() / hub.IngestRelayed key internal/idem on m.Sender, which is PEER-ASSERTED. internal/idem/store.go:417-421 argues its per-agent fair share is safe precisely because the bucket key is "a PROVEN IDENTITY, not an attacker-chosen label". IngestRelayed makes that statement FALSE the moment RELAY-24 wires a caller.

MEASURED IMPACT (security, not theory): the share is not enforced at all BELOW the pressure line (maxEntries/2 = 32768), and a peer asserting M distinct agent names gets M x 65536/(M+1+L) keys, converging on the whole 65536-entry table. At that point EVERY LOCAL AGENT's next send is refused with ErrCapacity, bus-wide, for up to the 50h10m retention window, evicting nothing -- by design.

WHERE THE FIX BELONGS: the wiring site. It genuinely CANNOT live in internal/hub, because the only PROVEN identity is the authenticated peer and internal/hub never sees it (httpapi.PeerBusIDFromContext does). Meter by the authenticated peer bus.

ALSO UPDATE THE DOC: internal/relay/doc.go item 2 records this gap and UNDERSTATES it -- it does not say the fair share is unenforced below the pressure line, nor that the end state is a bus-wide ErrCapacity for local agents. Correct that text in the same change.

PROOF NOTE: the stored proof_cmd names a test this task MUST WRITE. Require proof-check.sh verdict=PASS, not VACUOUS.

=== AMENDED 2026-08-14 (spec-keeper, recording the security gate's finding via the feature-runner). THIS CHANGES THE TASK'S SCOPE -- read it before estimating. ===

The "WHERE THE FIX BELONGS" paragraph above says the fix "genuinely CANNOT live in internal/hub" and belongs only at the wiring site. The security gate proved that is only HALF TRUE. Both halves matter:

(a) Metering by the AUTHENTICATED PEER indeed CANNOT live in internal/idem. The peer principal must be caller-supplied AND PERSISTED, because Recover rebuilds byAgent from decoded records; and since Record is the on-disk shape, decoded with DisallowUnknownFields, adding a field to it is an ON-DISK FORMAT CHANGE.

(b) BUT metering a FOREIGN sender by the VERIFIED BUS HALF of the id it already carries needs NO on-disk change at all. Record.Agent is ALREADY persisted and Record.Scope() rebuilds it, so the bucket key is a pure function of persisted data plus this bus's own id.

Option (b) is therefore available without a format change -- and it is NOT a one-line change. FOUR CONDITIONS, every one of them verified in code by the security gate, are mandatory:

  1. THE BUCKET KEY MUST BE CASE-FOLDED. ids.BusIDPattern is ^[A-Za-z0-9_-]{1,64}$ (uppercase allowed), so an un-folded key hands ONE pinned peer 2^n distinct buckets and reinstates the exact denominator attack this fix exists to close.
  2. A ZERO-VALUE BusID OPTION MUST BE A CONSTRUCTION ERROR. StoreOptions has no BusID today; defaulting it to "" classifies every LOCAL agent as foreign and collapses the whole roster into ONE bucket -- fairness silently off, every positive test still green.
  3. THE BOUND IS A DEPENDENCY, NOT A PROPERTY. It holds ONLY because relay refuses an unpinned origin bus and pins sender-bus == OriginBus == BusPath[0]. Relax that and the idem bucket silently stops being bounded, with NOTHING in internal/idem failing. Write the dependency down wherever the fix lands.
  4. IT NEEDS A DECISIONS.md ENTRY. Under (b) a peer bus's ENTIRE agent population shares ONE bucket, so an honest 100-agent peer gets the same share as a 1-agent peer. Record also the inherent cost of ANY meter-by-peer shape: for foreign agents a share refusal stops being self-inflicted at all -- one agent behind peer B can starve another honest agent behind peer B for the full RetentionWindow.

TEST IMPACT of option (b): a required BusID touches 15 idem.NewStore call sites, 14 of them in _test.go. quota_test.go's fair-share tests survive with BusID:"bus-a" (mechanical churn). The genuinely SEMANTIC rewrites are any test pinning Stats().Agents, and any test mixing bus halves inside a single store.

EXPOSURE IS NOT LIVE AT HEAD 208dacd: newFederation has no production caller there, verified two ways by both gates. But the wiring is IN FLIGHT in another agent's uncommitted cmd/agent-bus/main.go:882, so this goes live the moment that lands. Treat it as pre-landing, not hypothetical.

EXTRA ASK FROM THE SECURITY GATE: hub.IngestRelayed is EXPORTED and UNGUARDED. Any SECOND wiring site that calls it without relay.ValidateRelayRequest re-poisons the denominator, and nothing in internal/idem would notice. A guard test against that belongs with whichever fix lands.

PROOF_CMD IS CORRECT AS STORED: it names TestAppliedKeyMeteredByAuthenticatedPeer in ./internal/relay ./internal/httpapi, which does not exist yet. That is right for an OPEN task -- the test is to be written. Do not "fix" it, and do not complete this task on a VACUOUS proof-check verdict.

=== CORRECTED 2026-08-15 (spec-keeper, on a FORGED and MEASURED attack from an external security agent, not theorised). THIS REORDERS THE FIX PLAN -- read before picking this up. ===

FORGED, not theorised: a clean `git archive HEAD` overlay against internal/idem at HEAD, default store (MaxEntries=65536, pressure line 32768, RetentionWindow 50h10m22s). Flood: 32,766 records under DISTINCT asserted sender labels ("bus-peer.aN", OpRelay), one key each. Result: table reaches 32,768 = exactly 50% of cap, and an HONEST LOCAL AGENT holding 2 keys is then refused ErrAgentQuota -- fair share of 2, for the full ~50h retention window, while 32,768 slots sit free. Recover() reproduces it: it skips admitAgentLocked but still increments byAgent, so the POISONED DENOMINATOR SURVIVES RESTART. Nothing evicts it. A control was run (honest agent with 2 keys IS admitted absent the flood) -- not vacuous.

Repro kept and re-runnable: /tmp/claude-1000/-mnt-sdb4-mike-mike-source-agent-bus/061f69e7-e5f1-43cd-87cb-cc5640740da5/scratchpad/idemforge -- `go test -race -run TestForgeDenominator ./internal/idem`, two tests, both with controls.

THE LOAD-BEARING CORRECTION (verbatim, this is WHY the plan changes -- do not round it off):

  "The quota bounds ENTRIES; the attack's currency is DISTINCT BUCKET KEYS. These are different
  dimensions. Therefore metering by the authenticated peer at the wiring layer -- the expensive fix
  requiring the on-disk format change -- does NOT close this limb on its own, because idem's divisor
  is still the label count. Only bucketing a foreign sender by the VERIFIED BUS HALF of its id
  (case-folded, fail-closed BusID) closes it, by making the denominator grow by at most
  relay.MaxPeers instead of unboundedly. Raising relayAppliedKeyShare or charging below the pressure
  line does not fix it either."

WHAT THIS MEANS FOR THE TWO OPTIONS ABOVE: option (a) ("meter by the AUTHENTICATED PEER" at the wiring layer, requiring the on-disk format change to persist the authenticated peer identity into Record) is NOT a fix for THIS limb -- it is an attribution/charging mechanism (who pays), and it leaves internal/idem's own bucket key as m.Sender (the peer-asserted label), so the denominator is still poisoned by however many distinct labels the peer asserts. Option (b) (bucket a foreign sender's applied-key entry by the VERIFIED BUS HALF of the id it already carries, case-folded, no format change) is THE fix that closes the denominator-poisoning limb, because it bounds denominator growth to relay.MaxPeers regardless of how many sender labels one peer asserts. The task's TITLE ("Meter the applied-key table by the AUTHENTICATED PEER, not the peer-asserted origin sender") reads as endorsing option (a) as primary; it must not be read that way. Option (b), detailed in the "AMENDED 2026-08-14" section above with its four mandatory conditions (case-fold, zero-value BusID is a construction error, the bound is a dependency on relay's own bus-pinning not a property, and the DECISIONS.md entry on per-peer-not-per-agent fairness), IS the required fix for this limb. Option (a) may still be worth doing separately for attribution/charging purposes, but must not be presented or implemented as sufficient on its own, and does not gate this task's completion.

SEVERITY, IN THESE WORDS (do not shorten either direction): a peered bus can silently disable its neighbour's local agents for 50h. The principal is an AUTHENTICATED PEER BUS with a bound inbound client certificate, NOT a stranger -- it is P0 under a federation model where peering is not trust, but it is NOT remote-unauth. Both the longer phrasing and the P0 priority belong on the record together; the short phrasing alone gets this mis-prioritised in both directions (reads as either "just a peer being noisy" or "any attacker on the internet").

WIRING GAP, ALSO MEASURED: relaywiring.go's peerAdmission charges the peer ONLY when UnderPressure is true, so the entire 32,766-label flood in the repro above is UNCHARGED end to end -- by the time the meter engages, the denominator is already poisoned and, per Recover() above, retained across restart. This task is linked `relates` (and effectively blocking, see relations below) to RELAY-24-BLOCKER-EGRESS / the wiring commit, because the finder's judgement is that the wiring commit is the right place to land the bus-half bucketing.

DOC DEBT ALSO FLAGGED: internal/idem/store.go's current comment (committed at 51154ac) says "peer traffic alone can still end in global ErrCapacity" -- that names only the WEAKER limb (global capacity) and reads as complete to a future maintainer who has not seen this finding. It needs correcting to name the per-agent fair-share denominator-poisoning limb explicitly, alongside the global-capacity one. Linked `relates`.

PROOF_CMD: unchanged, still names TestAppliedKeyMeteredByAuthenticatedPeer in ./internal/relay ./internal/httpapi as the test to be WRITTEN once the fix (option b, bus-half bucketing) lands -- despite the name, the test this task must ultimately produce should assert the DENOMINATOR bound (entries-per-distinct-bus-half, not per-authenticated-peer-charging), consistent with the correction above. Do not rename the proof_cmd without updating the test plan to match; flag if the name itself should change when the fix lands.

## Relations (authoritative)

> **NOT FETCHED** — real edges are UNKNOWN here, not absent. This tree was built
> with `--no-relations`, which skips one rate-limited request per task. Re-run
> `bash scripts/gen-spec-mirror.sh` (no flag, ~70s) to render them.


_Unknown._

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (done)
- [RELAY-24-BLOCKER-HUBINGEST](../RELAY-24-BLOCKER-HUBINGEST--9ee98866/task.md) — RELAY-24-BLOCKER-HUBINGEST: internal/hub exported relay-ingest entry point -- foreign sen… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [CONV-PEERQUOTA](../../CONV/CONV-PEERQUOTA--35cb7dc6/task.md) — CONV-PEERQUOTA: bound conversation tracking per PEER on DISTINCT CONVERSATION IDS -- the… (todo)
- [CONV-TRACK-ON-RECEIPT](../../CONV/CONV-TRACK-ON-RECEIPT--ed1e70ac/task.md) — CONV-TRACK-ON-RECEIPT: a bus starts tracking a conversation on first receipt -- gated by… (todo)
- [IDEM-17-FU-CROSSAGENT](../../IDEM/IDEM-17-FU-CROSSAGENT--0cd0ce79/task.md) — Crash-injection coverage for cross-agent applied-key isolation across recovery (todo)
- [POLL-CONCURRENT-WAITERS](../../POLL/POLL-CONCURRENT-WAITERS--f6268dab/task.md) — POLL-CONCURRENT-WAITERS: two long-polls on ONE agent id can split delivery non-determinis… (todo)
- [RELAY-47](../RELAY-47--dd69c4d3/task.md) — RELAY-47: ONWARD RELAY -- WIRE an intermediate bus to forward a relayed message to a THIR… (done)
- [RELAY-FU-DOCGO-CROSSBUSTRUST-STALE](../RELAY-FU-DOCGO-CROSSBUSTRUST-STALE--4988156c/task.md) — internal/relay/doc.go asserts relay ingest is structurally blocked (no CrossBusTrust impl… (todo)
- [RELAY-FU-INGEST-RATELIMIT](../RELAY-FU-INGEST-RATELIMIT--e7c66d83/task.md) — RELAY-FU-INGEST-RATELIMIT: no rate limit, quota or concurrency cap of any kind on relayed… (superseded)
- [ff38f871-988a-4f2c-aa9a-febee4f3b15a](../../DOCS/AGENT_LOG-entry-skipped-doc-gate-justification-for-the-2--ff38f871/task.md) — AGENT_LOG entry + skipped-doc-gate justification for the 2026-08-14 internal/idem comment… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
