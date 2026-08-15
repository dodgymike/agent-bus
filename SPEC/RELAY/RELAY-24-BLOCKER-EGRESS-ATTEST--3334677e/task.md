# RELAY-24-BLOCKER-EGRESS-ATTEST: no bus can ISSUE an origin attestation for its own agents, so every relayed message it sends would be refused by the receiving bus

| Field | Value |
| --- | --- |
| Public id | `3334677e-b0d1-4e2f-addf-04ca28cd16f0` |
| Key | _(null in the export)_ |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | relay |
| Section | backlog |
| Tags | blocker, invariant-9 |
| Created | 2026-08-15T07:52:46.221512+00:00 |
| Updated | 2026-08-15T13:47:53.377955+00:00 |
| Completed | 2026-08-15T13:47:53.377939+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh 'go test -race -run "TestLocalMessageForPeerRecipientReachesForwarder" ./cmd/agent-bus/...'
```

## Status note

DELIVERED-PENDING-COMMIT by RELAY-24-BLOCKER-EGRESS (public_id 85ae8b32-3a46-4e85-bdfe-ea29730670fb). Reviewer ruled both named sub-problems done: (i) the roster public key is reached without widening hub.RosterSource, by injecting the roster at the composition root; (ii) KeyEpoch = uint64(entry.Epoch.UnixMilli()) clamped at 0 for a negative/zero epoch, recorded as a dated decision in DECISIONS.md (2026-08-xx, Decision 2, section ~line 5686) rather than a silent cast. No residual found. attest.Sign is now called in cmd/agent-bus/relayegress.go (zero production callers before this work) with notAfter = issuedAt + relay.RetryHorizonCeiling (idem.PeerOutageBudget, 24h) and the verifier applying its own attest.ClockSkewAllowance (5min). Do NOT complete this task until the delivering commit lands in HEAD -- as of now cmd/agent-bus/relayegress.go and its test are UNCOMMITTED (git status: untracked/staged, absent from HEAD).

## Description

P0 BLOCKER on RELAY-24-BLOCKER-EGRESS (85ae8b32-3a46-4e85-bdfe-ea29730670fb), discovered while implementing that task and SPLIT OUT deliberately -- NOT in its stated scope (a)-(d), exactly as RELAY-24-FU-STOREMSGLOOKUP was split from RELAY-24 itself. Evidence verified at HEAD 4dd5b67.

attest.Sign (internal/attest/attest.go:142) has ZERO production callers -- verified by `grep -rn "attest\." --include=*.go . | grep -v _test.go | grep -v internal/attest/`, whose only non-test hits are the relay package's VERIFY side plus two struct fields. relay.VerifyRelayed (internal/relay/signed.go:492) calls validateOriginAttestation and refuses a zero attestation with ErrMissingAttestation, and that check is documented as deliberately fail-closed for a directly-constructed RelayedMessage. So an egress path that builds a RelayedMessage from a locally-published store.Message has nothing to put in OriginAttestation -- every message this bus tries to relay to a peer would be refused on arrival.

TWO SUB-PROBLEMS that make this its own task rather than a clause of the handshake work:
(i) attest.Sign needs the agent's MESSAGING public key, which internal/auth.Entry.MessagingPublicKey holds but hub.Agent (internal/hub/roster.go:19) does NOT expose -- hub.RosterSource has to widen, or a second lookup must be injected at the composition root.
(ii) attest.Attestation.KeyEpoch is a uint64 while auth.Entry.Epoch is a time.Time, and choosing that mapping is a key-rotation design decision, not a cast -- needs a DECISIONS.md entry, not a silent choice inside the implementation.

Also note attest.Sign's own doc: notAfter MUST be derived from the maximum relay retention/retry window (an intermediate forwards verbatim and cannot re-mint), which ties this to internal/idem.PeerOutageBudget / relay.RetryHorizonCeiling -- pick that value deliberately, do not hardcode an arbitrary duration.

INVARIANT 9 APPLIES: use internal/attest AS-IS. Write no new crypto -- no new signing scheme, no adapted primitive, nothing that even looks like a bespoke construction out of otherwise-good primitives.

Blocks RELAY-24-BLOCKER-EGRESS (85ae8b32-3a46-4e85-bdfe-ea29730670fb). Relates to RELAY-25 (10491a01-30ae-4699-b5f1-a1993e026dd8), whose smoke test is the eventual end-to-end proof once this and RELAY-24-BLOCKER-EGRESS-HANDSHAKE both land.

PROOF: names a Go test to ADD (it does not exist today, since the production seam it would exercise does not exist yet) -- prefer TestEgressPathAttachesValidOriginAttestation, asserting a RelayedMessage built from a locally-published store.Message by the (not-yet-written) egress path carries an OriginAttestation that relay.VerifyRelayed accepts, using the SAME internal/attest.Sign/Verify pair (no parallel crypto). Currently unimplementable-as-a-test because neither the egress path nor the roster-widening seam exists; MUST be observed RED first (test added, fails because there is no caller/seam yet, or fails to compile against the not-yet-existing seam) and GREEN once (i) and (ii) above are resolved and the egress path signs outgoing messages.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md)
- **relates to** [RELAY-25](../RELAY-25--10491a01/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (done)
- [RELAY-24-BLOCKER-EGRESS-HANDSHAKE](../RELAY-24-BLOCKER-EGRESS-HANDSHAKE--0ab31d26/task.md) — RELAY-24-BLOCKER-EGRESS-HANDSHAKE: this bus never DIALS a peer, so its relay Registry nev… (todo)
- [RELAY-24-FU-STOREMSGLOOKUP](../RELAY-24-FU-STOREMSGLOOKUP--c6530638/task.md) — RELAY-24-FU-STOREMSGLOOKUP: internal/store needs a lookup-by-message-id and an OriginMess… (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (in_progress)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [0f8c5332-1236-4e22-a249-72119401003f](../../PROCESS/Spec-Server-API-gap-no-relation-delete-endpoint-wrong-bl--0f8c5332/task.md) — Spec Server API gap: no relation-delete endpoint -- wrong blocks/relates/supersedes/follo… (todo)
- [RELAY-48](../RELAY-48--9887b0eb/task.md) — RELAY-48: onward relay is NOT crash-safe -- a pending onward hop is durably ABANDONED at… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
