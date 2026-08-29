# RELAY-24-BLOCKER-PEERCERTFLAG: agent-bus peer add has no flag to bind a peer's inbound client-certificate fingerprint -- bindablePeerCount is 0 for every operator-reachable configuration

| Field | Value |
| --- | --- |
| Public id | `0e6b5a49-74be-432e-ab1e-485847a84fd0` |
| Key | RELAY-24-BLOCKER-PEERCERTFLAG |
| Epic | [RELAY](../epic.md) |
| Status | done |
| Priority | P0 |
| Component | cli |
| Section | backlog |
| Tags | — |
| Created | 2026-08-14T22:00:22.363350+00:00 |
| Updated | 2026-08-15T13:47:14.440401+00:00 |
| Completed | 2026-08-15T13:47:14.440382+00:00 |

## Proof command

```sh
bash scripts/proof-check.sh 'go test -race -run TestPeerAddBindsInboundClientCertFingerprint ./cmd/agent-bus'
```

## Status note

CODE-COMPLETE, NOT YET COMMITTED. Reviewer PASS (2 LOW prose findings fixed and re-verified), security PASS (no P0/P1, 2 P2 follow-ups filed). Proof in a clean git-archive-HEAD overlay with only cmd/agent-bus/peer.go + cmd/agent-bus/peerclientcert_test.go: "proof-check: verdict=PASS class=test exit=0 tests_run=1 top_level=1 skipped=0 failed=0 empty_pkgs=0". Awaiting coordinated commit; complete with commit_sha then.

## Description

HARD BLOCKER on the federation deliverable, found while verifying RELAY-24 code-complete. cmd/agent-bus/peer.go was outside RELAY-24's file boundary. Verified directly against HEAD, 2026-08-14.

THE GAP: `agent-bus peer add` (cmd/agent-bus/peer.go:549-573) sets only `-bus-id`, `-url`, `-tls-fingerprint` and `-signing-key`. The `-tls-fingerprint` flag's own help text says it pins "the TLS certificate of the bus AT -url (the NEXT HOP)" -- confirmed at peer.go:810/831, it writes `PeerConfig.NextHopTLSCertFingerprint`, the OUTBOUND direction (this bus dialing the peer). No flag writes `relay.BusTrustRecord.PeerClientTLSCertFingerprint` (peerstore.go:815), the INBOUND field that binds the certificate the PEER presents when it connects TO this bus. `applyPeerAdd`'s trust-record write (peer.go:795) is `store.PutTrust(relay.BusTrust{BusID: req.busID, SigningKeys: req.keys})` -- only those two fields, ever.

THE CONSEQUENCE, traced through to the mount guard: `bindablePeerCount` (cmd/agent-bus/relaywiring.go:1100-1123) counts `TrustedBuses()` records with a non-zero `PeerClientTLSCertFingerprint`. Since no shipped command ever sets that field, this count is 0 for EVERY operator-reachable configuration -- not a corner case, the only case. relaywiring.go's own comment at :1105-1113 explains why this matters: httpapi's mount refuses to register a peer surface that would answer 403 to everyone, because a registered-and-refusing route advertises federation while serving nobody. So the /v1/peer/* routes never mount, and `fed-smoke.sh` cannot pass no matter what else lands in RELAY-24 or RELAY-25 -- there is no way for an operator to ever produce a bindable peer without this flag.

FIX: `agent-bus peer add` needs a new flag (naming is the implementer's call, e.g. `-bind-client-cert` or `-inbound-fingerprint`) that sets `BusTrust.PeerClientTLSCertFingerprint` -- same 64-lowercase-hex-character shape as `-tls-fingerprint`, validated the same way, but written to the TRUST record's inbound field, not the route record's outbound field. Ship the AGENT_PROTOCOL.md / CONTRACTS-CLI.md update for the new flag in the same task per invariant 7.

TESTS: a positive test that after `peer add` with the new flag, `bindablePeerCount` returns >0 for that peer; a negative test that `peer add` without it leaves the count at 0 (today's behaviour, so this is the RED-before-fix case); exercised through the compiled `agent-bus` binary per invariant 7, not a hand-written store manipulation.

Relation: blocks RELAY-25 (10491a01) directly -- RELAY-24 itself is code-complete and this fix sits in a file RELAY-24 never touched, so it is a sibling blocker of the three-bus deliverable, not a reopening of RELAY-24.

## Relations (authoritative)

> Authoritative, from the Spec Server's relations resource. `blocks` is inert
> metadata — it never changes a task's status, so the status shown is always the
> task's own field.


- **blocks** [RELAY-25](../RELAY-25--10491a01/task.md)

## Referenced in description (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-25](../RELAY-25--10491a01/task.md) — RELAY-25: fed-smoke.sh: the epic's deliverable -- three-bus loopback federation smoke test (done)

## Referenced by other tasks (derived, not authoritative)

> Derived by matching task keys, title prefixes and public-id fragments in free text.
> The export has NO dependency field, so this is best-effort and NOT authoritative;
> a real `depends_on` field is tracked by CONTEXT-SPEC-DEPS.


- [RELAY-24](../RELAY-24--e303c624/task.md) — RELAY-24: Composition root: wire federation into cmd/agent-bus/main.go (done)
- [RELAY-24-BLOCKER-EGRESS](../RELAY-24-BLOCKER-EGRESS--85ae8b32/task.md) — RELAY-24-BLOCKER-EGRESS: a bus SENDING a relayed message has no wiring at all -- relay.Ne… (done)
- [RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE](../RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE--3e96dae2/task.md) — RELAY-24-BLOCKER-EGRESS-FU-FEDSMOKE: three-bus federation smoke test (fed-smoke.sh, both… (done)
- [RELAY-24-BLOCKER-PEERCERTFLAG-FU-BINDNOROUTE](../RELAY-24-BLOCKER-PEERCERTFLAG-FU-BINDNOROUTE--002f4875/task.md) — RELAY-24-BLOCKER-PEERCERTFLAG-FU-BINDNOROUTE: a binding with no route satisfies the mount… (todo)
- [RELAY-24-BLOCKER-PEERCERTFLAG-FU-CROSSNS](../RELAY-24-BLOCKER-PEERCERTFLAG-FU-CROSSNS--b64e2675/task.md) — RELAY-24-BLOCKER-PEERCERTFLAG-FU-CROSSNS: no cross-namespace uniqueness between agent cli… (todo)
- [RELAY-25-FU-INBOUNDBIND](../RELAY-25-FU-INBOUNDBIND--336c3b76/task.md) — RELAY-25-FU-INBOUNDBIND: fed-smoke.sh never binds each peer's INBOUND client-certificate… (done)

---

_Generated by `scripts/gen-spec-mirror.sh` from the Spec Server. Never hand-edit; the server is the source of truth._
