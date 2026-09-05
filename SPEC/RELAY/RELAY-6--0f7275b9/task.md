# RELAY-6: Record the FEDERATION deployment assumptions

| Field | Value |
| --- | --- |
| Public id | `0f7275b9-c45e-41b8-9f6e-3c7b1ec6ec00` |
| Key | RELAY-6 |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | docs |
| Section | backlog |
| Tags | — |
| Created | 2026-08-08T15:56:37.368139+00:00 |
| Updated | 2026-08-14T16:48:54.744187+00:00 |
| Completed | 2026-08-14T16:48:54.744171+00:00 |

## Proof command

```sh
grep -qF '(c-AMENDED)' DECISIONS.md && grep -qF '(b-CLARIFIED)' DECISIONS.md && grep -qF 'THIS RULING NARROWS INVARIANT 3' DECISIONS.md && grep -qF 'option (a), not (b)' DECISIONS.md && grep -qF 'observable PRE-AUTH' DECISIONS.md && grep -qF 'ONE FACTOR authorises on the peer surface' DECISIONS.md && grep -qF 'satisfiable but WRONG' DECISIONS.md && grep -qF 'RETIRED AND REPLACED, not deleted' DECISIONS.md
```

## Status note

RESET FROM in_progress TO todo 2026-08-14 (spec-keeper, per coordinator ownership sweep): status was in_progress with owner=None and lease_expires_at=None -- not held by anyone, not visible to claim-next either (todo-only pool), a dead zone. The same-day AMENDMENT REQUESTED note (this task, 09:35:06) asking whoever holds DECISIONS.md this wave to amend FEDERATION ruling (c) -- replacing the INVITE-PEERGUARD/MTLS-RELAYGUARD gate with MTLS-CLIENTAUTH+RELAY-41 (both now done) -- was never actioned; grepped DECISIONS.md, no amendment text exists. This blocks RELAY-20 (701dc54d), which cites the stale ruling as its second blocker. Genuinely open work, not a landed-but-unclosed case. Now todo and claimable.

## Description

FEDERATION phase, wave 1 (F1). Owns DECISIONS.md EXCLUSIVELY this wave.

Target topology: laptop(A) <-> internet(B) <-> this machine(C), B is a RELAY HOP. All links
are SSH tunnels; no bus ever listens publicly; the user is sole operator.

New dated "## 2026-08-08 -- FEDERATION" section in DECISIONS.md. Each ruling needs *what is
given up* and *what would reverse it*:
(a) Every bus-to-bus link is an SSH tunnel; no bus listens publicly; operator runs all machines.
(b) INVITE-GATE (05a5216d) does not block this epic -- with no reachable /v1/enroll the pre-auth
    attacker it exists to stop does not exist. Peer enrolment is operator-driven now; invite
    redemption is later hardening. Reversal trigger, stated mechanically: any bus bound to a
    non-loopback interface, or a tunnel endpoint shared with a non-operator. Given up: single-use
    /expiring/revocable peer admission, redemption audit.
(c) Peer routes still authenticate a PEER principal -- that is FUNCTIONALITY: roster updates must
    be bound to the connection (internal/relay/doc.go:154-158), and the last bus-path hop must be
    checkable against the sender (doc.go:172-175).
(d) Local-attacker scenarios are out of scope by operator ruling.
(e) Peer configuration is an offline `agent-bus peer` subcommand under the dirlock, following the
    `invite mint` / D6 precedent. No new online admin route, no new privilege tier. Given up:
    online re-peering -- a topology change needs a restart.
(f) Static next-hop routing, not a routing protocol. Given up: topology discovery; a fourth bus
    needs an operator route entry. Right trade for a fixed three-node line.

Standing rules for the whole epic (apply to every RELAY-6..26 task): ownership inside
internal/relay is per-FILE (new tests in a NEW test file named for the task, never appended to
relay_test.go/registry_test.go); do not edit DECISIONS.md/AGENT_LOG.md unless the task says so
this wave; a proof naming a not-yet-written test is VACUOUS not FAIL; judge gofmt by EMPTY OUTPUT
only; run the mandated reviewer AND security gates; invariant 9 is absolute (crypto/ed25519
Sign/Verify only -- stop and escalate on anything more).

Verified RED: `grep -c 'SSH tunnel\|ssh-tunnel' DECISIONS.md` -> 0.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-18](../RELAY-18--fa5d1b0d/task.md)
- **relates to** [a9a433dd-4ae0-4a0d-a6f3-6c804105fbcd](../../TOOLING/Conjunction-masking-vacuous-proof-family-filtered-clause--a9a433dd/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [INVITE-GATE](../../INVITE/INVITE-GATE--05a5216d/task.md) — INVITE-GATE: POST /v1/enroll REQUIRES a valid invite and fails closed; invite consumption… (done)
- [INVITE-PEERGUARD](../../INVITE/INVITE-PEERGUARD--f5d91dbe/task.md) — INVITE-PEERGUARD: no ungated peer/federation enrolment path may ever exist -- enumerate t… (todo)
- [MTLS-CLIENTAUTH](../../MTLS/MTLS-CLIENTAUTH--cc9558a8/task.md) — MTLS-CLIENTAUTH: request a client certificate on every connection WITHOUT a CA -- tls.Req… (done)
- [MTLS-RELAYGUARD](../../MTLS/MTLS-RELAYGUARD--8192c3c7/task.md) — MTLS-RELAYGUARD: bus-to-bus relay links are mutually authenticated too -- acceptance crit… (todo)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [2ca053dd-1b63-42b5-a485-f57b623722ac](../internal-relay-guards_test.go-912-says-the-RELAY-6-subst--2ca053dd/task.md) — internal/relay/guards_test.go:912 says the RELAY-6 substitution 'IS NOT RECORDED IN DECIS… (done)
- [IDEM-11](../../IDEM/IDEM-11--8e2c4de3/task.md) — IDEM-11: Durable applied-key store, recovered via WAL replay, with a bounded retention wi… (done)
- [RELAY-18](../RELAY-18--fa5d1b0d/task.md) — RELAY-18: Retire the relay import guard deliberately, replaced by a narrower one (done)
- [RELAY-20](../RELAY-20--701dc54d/task.md) — RELAY-20: Mount /v1/peer/{enroll,relay,roster} behind a PEER principal (done)
- [RELAY-22](../RELAY-22--b4e45cda/task.md) — RELAY-22: Choose and wire the multi-principal relay abuse-control primitive (todo)
- [RELAY-25-FU-REALHOST](../RELAY-25-FU-REALHOST--8708f7c9/task.md) — RELAY-25-FU-REALHOST: Real three-host SSH-tunnel federation run -- loopback smoke does no… (todo)
- [RELAY-26](../RELAY-26--d72a1e04/task.md) — RELAY-26: Startup refusal: non-loopback -listen with peer records and invite-gating off (done)
- [RELAY-41](../RELAY-41--05253c80/task.md) — RELAY-41: Per-NEXT-HOP TLS certificate fingerprint on PeerRecord, plumbed through \`agent-… (done)
- [f0a4eaee-8428-4b6c-8485-cf44dd9df779](../internal-httpapi-authmw.go-315-316-states-a-premise-the--f0a4eaee/task.md) — internal/httpapi/authmw.go:315-316 states a premise the security gate refuted: a peer bus… (done)
- [fbb16f9b-1b81-4fd0-a60f-5b2a76806bff](../internal-httpapi-peermount.go-pre-auth-prober-does-not-e--fbb16f9b/task.md) — internal/httpapi/peermount.go: 'pre-auth prober does not exist' overstates ruling (h), an… (todo)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
